package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// analyticsSchema holds one summary row per target per UTC hour
// (blueprint §10.4, §11). Latency and expected-run columns come later.
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
}

// AggregateHours (re)computes the summaries for every UTC hour that
// overlaps [from, to). Running it again replaces the rows instead of
// adding duplicates, so recent hours can safely be recomputed when late
// runs arrive (blueprint §11.1).
//
// The rules (Phase 7 design):
//   - A run belongs to the hour it was scheduled in (started_at if it was
//     not scheduled), not the hour lag pushed it into.
//   - success and recovered count as up; failed counts as down.
//   - cancelled runs were stopped by us: counted, but neither up nor down.
//   - An hour with no up/down runs has uptime NULL, never 100% or 0%.
func (s *SQLite) AggregateHours(ctx context.Context, from, to time.Time) error {
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC().Truncate(time.Hour)
	if !to.After(from) {
		to = from.Add(time.Hour)
	}

	_, err := s.db.ExecContext(ctx, `
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
			SELECT target_id, outcome,
			       -- "2026-11-08T22:41:07.123Z" -> "2026-11-08T22:00:00.000Z"
			       substr(COALESCE(scheduled_at, started_at), 1, 13) || ':00:00.000Z' AS bucket
			FROM probe_runs
			WHERE COALESCE(scheduled_at, started_at) >= ?
			  AND COALESCE(scheduled_at, started_at) < ?
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
		return fmt.Errorf("aggregating %s to %s: %w", ts(from), ts(to), err)
	}
	return nil
}

// HourlySummaries returns the stored summaries for one target, for the
// UTC hours in [from, to), oldest first.
func (s *SQLite) HourlySummaries(ctx context.Context, targetID int, from, to time.Time) ([]HourSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT hourly_bucket, observed_runs, success_runs, recovered_runs,
		       failed_runs, cancelled_runs, uptime_pct
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
		var uptime sql.NullFloat64
		if err := rows.Scan(&bucket, &h.ObservedRuns, &h.SuccessRuns, &h.RecoveredRuns,
			&h.FailedRuns, &h.CancelledRuns, &uptime); err != nil {
			return nil, err
		}
		if h.Hour, err = time.Parse(timeLayout, bucket); err != nil {
			return nil, fmt.Errorf("bad hourly_bucket %q: %w", bucket, err)
		}
		if uptime.Valid {
			h.UptimePct = &uptime.Float64
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
