package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// coverageSchema records when Gotell was running and when targets were
// paused, so analytics can tell "the target was down" apart from "nobody
// was watching" (blueprint §11.2).
const coverageSchema = `
CREATE TABLE IF NOT EXISTS monitor_sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,  -- heartbeat: the end time if Gotell crashed
    stopped_at   TEXT            -- NULL while running, or after a crash
);

CREATE TABLE IF NOT EXISTS target_pauses (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id  INTEGER NOT NULL REFERENCES targets(id),
    paused_at  TEXT NOT NULL,
    resumed_at TEXT             -- NULL while still paused
);
`

// StartSession records that Gotell has started monitoring, and returns
// the session's id for Heartbeat and EndSession.
func (s *SQLite) StartSession(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO monitor_sessions (started_at, last_seen_at) VALUES (?, ?)`, ts(now), ts(now))
	if err != nil {
		return 0, fmt.Errorf("starting session: %w", err)
	}
	return res.LastInsertId()
}

// Heartbeat records that the session is still running. If Gotell crashes,
// the last heartbeat is taken as the end of the session.
func (s *SQLite) Heartbeat(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE monitor_sessions SET last_seen_at = ? WHERE id = ?`, ts(now), id)
	return err
}

// EndSession records a clean stop.
func (s *SQLite) EndSession(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE monitor_sessions SET last_seen_at = ?, stopped_at = ? WHERE id = ?`, ts(now), ts(now), id)
	return err
}

// PauseTarget records the start of a maintenance window for a target.
func (s *SQLite) PauseTarget(ctx context.Context, targetID int, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO target_pauses (target_id, paused_at) VALUES (?, ?)`, targetID, ts(at))
	return err
}

// ResumeTarget ends a target's open maintenance window.
func (s *SQLite) ResumeTarget(ctx context.Context, targetID int, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE target_pauses SET resumed_at = ? WHERE target_id = ? AND resumed_at IS NULL`,
		ts(at), targetID)
	return err
}

// span is a stretch of time [start, end).
type span struct{ start, end time.Time }

// overlap returns how long two spans overlap (0 if they do not).
func overlap(a, b span) time.Duration {
	start, end := a.start, a.end
	if b.start.After(start) {
		start = b.start
	}
	if b.end.Before(end) {
		end = b.end
	}
	return max(end.Sub(start), 0)
}

// intersect returns the part of a that is inside b, and false if none.
func intersect(a, b span) (span, bool) {
	if overlap(a, b) <= 0 {
		return span{}, false
	}
	out := a
	if b.start.After(out.start) {
		out.start = b.start
	}
	if b.end.Before(out.end) {
		out.end = b.end
	}
	return out, true
}

// totalOverlap is how much of window is covered by spans. The spans must
// not overlap each other (one Gotell at a time; one pause per target at a
// time).
func totalOverlap(window span, spans []span) time.Duration {
	var total time.Duration
	for _, sp := range spans {
		total += overlap(window, sp)
	}
	return total
}

// targetInfo is what coverage needs to know about a target.
type targetInfo struct {
	id       int
	interval time.Duration
	created  time.Time
}

// updateCoverage writes expected, missing, watched, paused and gap figures
// for every target-hour in [from, to), creating rows for hours that had
// coverage but no runs at all (every expected run is then missing).
func updateCoverage(ctx context.Context, tx *sql.Tx, from, to, now time.Time) error {
	targets, err := loadTargets(ctx, tx)
	if err != nil {
		return err
	}
	sessions, err := loadSpans(ctx, tx, `
		SELECT started_at, COALESCE(stopped_at, last_seen_at)
		FROM monitor_sessions
		WHERE started_at < ? AND COALESCE(stopped_at, last_seen_at) > ?`,
		ts(to), ts(from))
	if err != nil {
		return fmt.Errorf("reading sessions: %w", err)
	}

	for _, t := range targets {
		pauses, err := loadSpans(ctx, tx, `
			SELECT paused_at, COALESCE(resumed_at, ?)
			FROM target_pauses
			WHERE target_id = ? AND paused_at < ? AND COALESCE(resumed_at, ?) > ?`,
			ts(now), t.id, ts(to), ts(now), ts(from))
		if err != nil {
			return fmt.Errorf("reading pauses for target %d: %w", t.id, err)
		}

		for h := from; h.Before(to); h = h.Add(time.Hour) {
			// The part of this hour that has happened, since the target existed.
			window := span{start: h, end: h.Add(time.Hour)}
			if t.created.After(window.start) {
				window.start = t.created
			}
			if now.Before(window.end) {
				window.end = now
			}
			if !window.end.After(window.start) {
				continue
			}

			paused := totalOverlap(window, pauses)
			watched := totalOverlap(window, sessions)
			// Time that was both watched and paused counts as paused only.
			for _, sess := range sessions {
				if part, ok := intersect(window, sess); ok {
					watched -= totalOverlap(part, pauses)
				}
			}
			gap := max(window.end.Sub(window.start)-watched-paused, 0)
			expected := int(watched / t.interval)

			_, err := tx.ExecContext(ctx, `
				INSERT INTO historical_metrics (target_id, hourly_bucket, observed_runs,
					success_runs, recovered_runs, failed_runs, cancelled_runs, computed_at,
					expected_runs, watched_seconds, paused_seconds, gap_seconds)
				VALUES (?, ?, 0, 0, 0, 0, 0, ?, ?, ?, ?, ?)
				ON CONFLICT (target_id, hourly_bucket) DO UPDATE SET
					expected_runs   = excluded.expected_runs,
					watched_seconds = excluded.watched_seconds,
					paused_seconds  = excluded.paused_seconds,
					gap_seconds     = excluded.gap_seconds,
					computed_at     = excluded.computed_at`,
				t.id, ts(h), ts(now), expected,
				watched.Seconds(), paused.Seconds(), gap.Seconds())
			if err != nil {
				return fmt.Errorf("saving coverage for target %d at %s: %w", t.id, ts(h), err)
			}
		}
	}

	// Missing = expected but not recorded. Runs can slightly exceed the
	// expected count (the first run starts at once), so never below 0.
	_, err = tx.ExecContext(ctx, `
		UPDATE historical_metrics
		SET missing_runs = MAX(expected_runs - observed_runs, 0)
		WHERE hourly_bucket >= ? AND hourly_bucket < ?`,
		ts(from), ts(to))
	if err != nil {
		return fmt.Errorf("computing missing runs: %w", err)
	}
	return nil
}

// loadTargets reads every target's interval and creation time.
func loadTargets(ctx context.Context, tx *sql.Tx) ([]targetInfo, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, interval_seconds, created_at FROM targets`)
	if err != nil {
		return nil, fmt.Errorf("reading targets: %w", err)
	}
	defer rows.Close()

	var out []targetInfo
	for rows.Next() {
		var t targetInfo
		var seconds int
		var created string
		if err := rows.Scan(&t.id, &seconds, &created); err != nil {
			return nil, err
		}
		if seconds <= 0 {
			continue // no sensible expectation without an interval
		}
		t.interval = time.Duration(seconds) * time.Second
		if t.created, err = time.Parse(timeLayout, created); err != nil {
			return nil, fmt.Errorf("target %d: bad created_at %q: %w", t.id, created, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// loadSpans runs a query that returns (start, end) text timestamps.
func loadSpans(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]span, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []span
	for rows.Next() {
		var start, end string
		if err := rows.Scan(&start, &end); err != nil {
			return nil, err
		}
		var sp span
		if sp.start, err = time.Parse(timeLayout, start); err != nil {
			return nil, err
		}
		if sp.end, err = time.Parse(timeLayout, end); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}
