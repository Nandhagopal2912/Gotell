package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"time"
)

// analyticsSchema holds one summary row per target per UTC hour
// (blueprint §10.4, §11). Expected-run columns come in a later step.
const analyticsSchema = `
CREATE TABLE IF NOT EXISTS historical_metrics (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id      INTEGER NOT NULL REFERENCES targets(id),
    hourly_bucket  TEXT    NOT NULL,
    observed_runs  INTEGER NOT NULL,
    success_runs   INTEGER NOT NULL,
    recovered_runs INTEGER NOT NULL,
    failed_runs    INTEGER NOT NULL,
    cancelled_runs INTEGER NOT NULL,
    uptime_pct     REAL,
    computed_at    TEXT    NOT NULL,
    UNIQUE (target_id, hourly_bucket)
);
`

// addedColumns were added to historical_metrics after Phase 7 step 1.
// Databases created earlier already have the table without them, so
// migrate adds whichever are missing.
var addedColumns = []struct{ name, def string }{
	{"latency_samples", "INTEGER NOT NULL DEFAULT 0"},
	{"no_response_attempts", "INTEGER NOT NULL DEFAULT 0"},
	{"avg_ttfb_ms", "REAL"},
	{"p50_ttfb_ms", "REAL"},
	{"p95_ttfb_ms", "REAL"},
	{"p99_ttfb_ms", "REAL"},
	{"peak_ttfb_ms", "REAL"},
	// Step 3: expected vs observed (blueprint §11.2).
	{"expected_runs", "INTEGER NOT NULL DEFAULT 0"},
	{"missing_runs", "INTEGER NOT NULL DEFAULT 0"},
	{"watched_seconds", "REAL NOT NULL DEFAULT 0"},
	{"paused_seconds", "REAL NOT NULL DEFAULT 0"},
	{"gap_seconds", "REAL NOT NULL DEFAULT 0"},
}

// migrate adds columns that newer versions of Gotell need to tables that
// older versions created. Existing rows are kept.
func migrate(db *sql.DB) error {
	existing := make(map[string]bool)
	rows, err := db.Query(`SELECT name FROM pragma_table_info('historical_metrics')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, c := range addedColumns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE historical_metrics ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
			return fmt.Errorf("adding column %s: %w", c.name, err)
		}
	}
	return nil
}

// HourSummary is one target's results for one UTC hour.
type HourSummary struct {
	TargetID      int
	Hour          time.Time // start of the hour, UTC
	ObservedRuns  int
	SuccessRuns   int
	RecoveredRuns int
	FailedRuns    int
	CancelledRuns int
	UptimePct     *float64 // nil when no run in the hour could say up or down

	// Latency of attempts that got a response (all attempts, including
	// retries and possible cold starts, blueprint §7.2). All nil when
	// LatencySamples is 0.
	LatencySamples     int // attempts with a measured TTFB
	NoResponseAttempts int // attempts with no TTFB, not counting ones we cancelled
	AvgTTFBms          *float64
	P50TTFBms          *float64
	P95TTFBms          *float64
	P99TTFBms          *float64
	PeakTTFBms         *float64

	// Coverage (blueprint §11.2). Seconds of this hour (up to now, and
	// after the target was added) split three ways: Gotell was watching,
	// the target was paused, or Gotell was not running (a monitoring gap,
	// not the target's fault).
	ExpectedRuns   int // watched seconds / interval
	MissingRuns    int // expected runs with no recorded result; never below 0
	WatchedSeconds float64
	PausedSeconds  float64
	GapSeconds     float64
}

// bucketExpr puts a run in the UTC hour it was scheduled in (started_at if
// it was not scheduled): "2026-11-08T22:41:07.123Z" -> "2026-11-08T22:00:00.000Z".
const bucketExpr = `substr(COALESCE(r.scheduled_at, r.started_at), 1, 13) || ':00:00.000Z'`

// AggregateHours (re)computes the summaries for every UTC hour that
// overlaps [from, to), in one transaction. Running it again replaces the
// rows instead of adding duplicates, so recent hours can safely be
// recomputed when late runs arrive (blueprint §11.1).
//
// The rules (Phase 7 design):
//   - A run belongs to the hour it was scheduled in, not the hour lag
//     pushed it into.
//   - success and recovered count as up; failed counts as down.
//   - cancelled runs were stopped by us: counted, but neither up nor down.
//   - An hour with no up/down runs has uptime NULL, never 100% or 0%.
//   - Latency uses every attempt that got a response; attempts without
//     one are counted separately, never given an invented value.
//   - Expected runs only count time Gotell was running and the target was
//     not paused; missing runs are shown as missing, never as success.
func (s *SQLite) AggregateHours(ctx context.Context, from, to time.Time) error {
	return s.aggregateHours(ctx, from, to, time.Now())
}

// aggregateHours is AggregateHours with "now" passed in, so tests can
// control which part of the current hour has happened.
func (s *SQLite) aggregateHours(ctx context.Context, from, to, now time.Time) (err error) {
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC().Truncate(time.Hour)
	if !to.After(from) {
		to = from.Add(time.Hour)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if err := upsertUptime(ctx, tx, from, to); err != nil {
		return err
	}
	if err := updateLatency(ctx, tx, from, to); err != nil {
		return err
	}
	if err := updateCoverage(ctx, tx, from, to, now.UTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// upsertUptime writes the run counts and uptime for each target-hour.
func upsertUptime(ctx context.Context, tx *sql.Tx, from, to time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO historical_metrics (target_id, hourly_bucket, observed_runs,
			success_runs, recovered_runs, failed_runs, cancelled_runs,
			uptime_pct, computed_at)
		SELECT
			target_id,
			bucket,
			COUNT(*),
			SUM(outcome = 'success'),
			SUM(outcome = 'recovered'),
			SUM(outcome = 'failed'),
			SUM(outcome = 'cancelled'),
			CASE
				WHEN SUM(outcome IN ('success', 'recovered', 'failed')) = 0 THEN NULL
				ELSE 100.0 * SUM(outcome IN ('success', 'recovered'))
				           / SUM(outcome IN ('success', 'recovered', 'failed'))
			END,
			?
		FROM (
			SELECT r.target_id, r.outcome, `+bucketExpr+` AS bucket
			FROM probe_runs r
			WHERE COALESCE(r.scheduled_at, r.started_at) >= ?
			  AND COALESCE(r.scheduled_at, r.started_at) < ?
		)
		WHERE true
		GROUP BY target_id, bucket
		ON CONFLICT (target_id, hourly_bucket) DO UPDATE SET
			observed_runs  = excluded.observed_runs,
			success_runs   = excluded.success_runs,
			recovered_runs = excluded.recovered_runs,
			failed_runs    = excluded.failed_runs,
			cancelled_runs = excluded.cancelled_runs,
			uptime_pct     = excluded.uptime_pct,
			computed_at    = excluded.computed_at`,
		ts(time.Now()), ts(from), ts(to))
	if err != nil {
		return fmt.Errorf("aggregating uptime %s to %s: %w", ts(from), ts(to), err)
	}
	return nil
}

// hourKey identifies one target in one hour.
type hourKey struct {
	targetID int
	bucket   string
}

// latencyStats collects one target-hour's attempts.
type latencyStats struct {
	ttfbs      []float64 // ms, attempts that got a response
	noResponse int
}

// updateLatency reads every attempt in the range, computes the latency
// figures in Go (SQLite has no percentile function), and writes them to
// the rows upsertUptime created.
func updateLatency(ctx context.Context, tx *sql.Tx, from, to time.Time) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT r.target_id, `+bucketExpr+`, a.ttfb_ms, a.failure_class
		FROM probe_attempts a
		JOIN probe_runs r ON r.id = a.run_id
		WHERE COALESCE(r.scheduled_at, r.started_at) >= ?
		  AND COALESCE(r.scheduled_at, r.started_at) < ?`,
		ts(from), ts(to))
	if err != nil {
		return fmt.Errorf("reading attempts: %w", err)
	}

	stats := make(map[hourKey]*latencyStats)
	for rows.Next() {
		var k hourKey
		var ttfb sql.NullFloat64
		var class string
		if err := rows.Scan(&k.targetID, &k.bucket, &ttfb, &class); err != nil {
			rows.Close()
			return err
		}
		st := stats[k]
		if st == nil {
			st = &latencyStats{}
			stats[k] = st
		}
		switch {
		case ttfb.Valid:
			st.ttfbs = append(st.ttfbs, ttfb.Float64)
		case class != "cancelled": // stopped by us is not the target's fault
			st.noResponse++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for k, st := range stats {
		var avg, p50, p95, p99, peak any // NULL unless there are samples
		if n := len(st.ttfbs); n > 0 {
			slices.Sort(st.ttfbs)
			sum := 0.0
			for _, v := range st.ttfbs {
				sum += v
			}
			avg = sum / float64(n)
			p50 = percentile(st.ttfbs, 50)
			p95 = percentile(st.ttfbs, 95)
			p99 = percentile(st.ttfbs, 99)
			peak = st.ttfbs[n-1]
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE historical_metrics SET
				latency_samples = ?, no_response_attempts = ?,
				avg_ttfb_ms = ?, p50_ttfb_ms = ?, p95_ttfb_ms = ?,
				p99_ttfb_ms = ?, peak_ttfb_ms = ?
			WHERE target_id = ? AND hourly_bucket = ?`,
			len(st.ttfbs), st.noResponse, avg, p50, p95, p99, peak, k.targetID, k.bucket)
		if err != nil {
			return fmt.Errorf("saving latency for target %d at %s: %w", k.targetID, k.bucket, err)
		}
	}
	return nil
}

// percentile returns the nearest-rank p-th percentile of sorted, which
// must be sorted ascending and not empty. The result is always a value
// that actually occurred.
func percentile(sorted []float64, p float64) float64 {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	rank = max(rank, 1)
	return sorted[rank-1]
}

// HourlySummaries returns the stored summaries for one target, for the
// UTC hours in [from, to), oldest first.
func (s *SQLite) HourlySummaries(ctx context.Context, targetID int, from, to time.Time) ([]HourSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT hourly_bucket, observed_runs, success_runs, recovered_runs,
		       failed_runs, cancelled_runs, uptime_pct,
		       latency_samples, no_response_attempts,
		       avg_ttfb_ms, p50_ttfb_ms, p95_ttfb_ms, p99_ttfb_ms, peak_ttfb_ms,
		       expected_runs, missing_runs, watched_seconds, paused_seconds, gap_seconds
		FROM historical_metrics
		WHERE target_id = ? AND hourly_bucket >= ? AND hourly_bucket < ?
		ORDER BY hourly_bucket`,
		targetID, ts(from.UTC().Truncate(time.Hour)), ts(to.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HourSummary
	for rows.Next() {
		h := HourSummary{TargetID: targetID}
		var bucket string
		var uptime, avg, p50, p95, p99, peak sql.NullFloat64
		if err := rows.Scan(&bucket, &h.ObservedRuns, &h.SuccessRuns, &h.RecoveredRuns,
			&h.FailedRuns, &h.CancelledRuns, &uptime,
			&h.LatencySamples, &h.NoResponseAttempts,
			&avg, &p50, &p95, &p99, &peak,
			&h.ExpectedRuns, &h.MissingRuns, &h.WatchedSeconds, &h.PausedSeconds, &h.GapSeconds); err != nil {
			return nil, err
		}
		if h.Hour, err = time.Parse(timeLayout, bucket); err != nil {
			return nil, fmt.Errorf("bad hourly_bucket %q: %w", bucket, err)
		}
		h.UptimePct = nullable(uptime)
		h.AvgTTFBms = nullable(avg)
		h.P50TTFBms = nullable(p50)
		h.P95TTFBms = nullable(p95)
		h.P99TTFBms = nullable(p99)
		h.PeakTTFBms = nullable(peak)
		out = append(out, h)
	}
	return out, rows.Err()
}

// nullable turns a SQL NULL into a nil pointer.
func nullable(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}
