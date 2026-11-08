// Package store saves runs and attempts to SQLite (blueprint §9, §10).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql

	"gotel/runner"
	"gotel/scheduler"
)

// classifierVersion is stored with each run so results can be re-read
// correctly if the classification rules change later (blueprint §7.2).
const classifierVersion = "1"

// schema creates the tables if they do not exist yet (blueprint §10,
// trimmed to what Gotell records today).
const schema = `
CREATE TABLE IF NOT EXISTS targets (
    id               INTEGER PRIMARY KEY,
    url              TEXT    NOT NULL,
    interval_seconds INTEGER NOT NULL,
    created_at       TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS probe_runs (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id          INTEGER NOT NULL REFERENCES targets(id),
    scheduled_at       TEXT,
    started_at         TEXT    NOT NULL,
    completed_at       TEXT    NOT NULL,
    outcome            TEXT    NOT NULL,
    attempt_count      INTEGER NOT NULL,
    scheduler_delay_ms REAL,
    classifier_version TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_probe_runs_target_time
    ON probe_runs(target_id, started_at DESC);

CREATE TABLE IF NOT EXISTS probe_attempts (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id            INTEGER NOT NULL REFERENCES probe_runs(id),
    attempt_number    INTEGER NOT NULL,
    started_at        TEXT    NOT NULL,
    status_code       INTEGER,
    failure_class     TEXT    NOT NULL,
    dns_ms            REAL,
    tcp_connect_ms    REAL,
    tls_handshake_ms  REAL,
    ttfb_ms           REAL,
    total_time_ms     REAL    NOT NULL,
    connection_reused INTEGER NOT NULL,
    body_bytes        INTEGER NOT NULL,
    error_message     TEXT,
    UNIQUE (run_id, attempt_number)
);
`

// SQLite is Gotell's database. Only the Writer goroutine writes to it.
type SQLite struct {
	db *sql.DB
}

// Open opens (or creates) the database file at path, applies the
// connection settings to every connection, and creates the tables.
func Open(path string) (*SQLite, error) {
	// Each _pragma runs on every new connection (blueprint §9.1): WAL so
	// readers do not block the writer, a busy timeout instead of failing
	// at once on a lock, and foreign keys (off by default in SQLite).
	dsn := path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection: SQLite has one writer at a time anyway, and this
	// keeps the write path serialized (blueprint §9.1).
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema + analyticsSchema + coverageSchema + stateSchema + incidentSchema + notificationSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema in %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return &SQLite{db: db}, nil

}

// Close closes the database.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// UpsertTargets inserts the configured targets, or updates them if they
// already exist, so restarting Gotell never creates duplicates.
func (s *SQLite) UpsertTargets(ctx context.Context, targets []scheduler.Target) error {
	now := ts(time.Now())
	for _, t := range targets {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO targets (id, url, interval_seconds, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				url = excluded.url,
				interval_seconds = excluded.interval_seconds,
				updated_at = excluded.updated_at`,
			t.ID, t.URL, int(t.Interval.Seconds()), now, now)
		if err != nil {
			return fmt.Errorf("saving target %d: %w", t.ID, err)
		}
	}
	return nil
}

// WriteBatch saves every run and its attempts in one transaction: all of
// them are saved, or none are (blueprint §9.2).
func (s *SQLite) WriteBatch(ctx context.Context, runs []runner.Run) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// If anything below fails, undo the whole batch. After a successful
	// Commit, Rollback does nothing.
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	runStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO probe_runs (target_id, scheduled_at, started_at, completed_at,
			outcome, attempt_count, scheduler_delay_ms, classifier_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare run insert: %w", err)
	}
	defer runStmt.Close()

	attemptStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO probe_attempts (run_id, attempt_number, started_at, status_code,
			failure_class, dns_ms, tcp_connect_ms, tls_handshake_ms, ttfb_ms,
			total_time_ms, connection_reused, body_bytes, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare attempt insert: %w", err)
	}
	defer attemptStmt.Close()

	for _, run := range runs {
		var delay any // NULL when the run was not scheduled
		if !run.ScheduledAt.IsZero() {
			delay = ms(run.SchedulerDelay)
		}
		res, err := runStmt.ExecContext(ctx,
			run.TargetID, ts(run.ScheduledAt), ts(run.StartedAt), ts(run.CompletedAt),
			string(run.Outcome), len(run.Attempts), delay, classifierVersion)
		if err != nil {
			return fmt.Errorf("insert run for target %d: %w", run.TargetID, err)
		}
		runID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("run id for target %d: %w", run.TargetID, err)
		}

		for _, a := range run.Attempts {
			r := a.Result
			var status, errMsg any // NULL when there was no response / no error
			if r.StatusCode != 0 {
				status = r.StatusCode
			}
			if r.Err != nil {
				errMsg = r.Err.Error()
			}
			_, err := attemptStmt.ExecContext(ctx,
				runID, a.Number, ts(a.StartedAt), status, string(a.Class),
				msPtr(r.DNS), msPtr(r.TCPConnect), msPtr(r.TLSHandshake), msPtr(r.TTFB),
				ms(r.Total), r.ConnReused, r.BodyBytes, errMsg)
			if err != nil {
				return fmt.Errorf("insert attempt %d for target %d: %w", a.Number, run.TargetID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// OutcomeCounts returns how many saved runs have each outcome.
func (s *SQLite) OutcomeCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT outcome, COUNT(*) FROM probe_runs GROUP BY outcome`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var outcome string
		var n int
		if err := rows.Scan(&outcome, &n); err != nil {
			return nil, err
		}
		counts[outcome] = n
	}
	return counts, rows.Err()
}

// timeLayout is fixed-width UTC with milliseconds, so text timestamps sort
// in time order.
const timeLayout = "2006-01-02T15:04:05.000Z"

// ts formats t for storage, or returns nil (SQL NULL) for the zero time.
func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

// ms converts a duration to milliseconds.
func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// msPtr is ms for an optional duration: nil (not measured) becomes NULL,
// never 0 (blueprint §5.2).
func msPtr(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return ms(*d)
}
