package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// stateSchema stores small pieces of analytics bookkeeping by name.
const stateSchema = `
CREATE TABLE IF NOT EXISTS analytics_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// watermarkKey names the hour before which every summary is final.
const watermarkKey = "aggregated_through"

// openHours is how many recent hours are always recomputed, because runs
// for them can still arrive late (a retry that crosses into the next
// hour, a batch saved a few seconds after the hour ends).
const openHours = 2

// CatchUp brings the hourly summaries up to date (blueprint §11.1). It
// recomputes every hour from the watermark to now, then moves the
// watermark forward, keeping the most recent openHours still open. After
// a crash or a long stop, the next CatchUp fills in everything missed.
func (s *SQLite) CatchUp(ctx context.Context, now time.Time) error {
	now = now.UTC()
	from, ok, err := s.watermark(ctx)
	if err != nil {
		return err
	}
	if !ok {
		// First time: start from the earliest run, or from now.
		if from, err = s.earliestRun(ctx, now); err != nil {
			return err
		}
	}

	to := now.Truncate(time.Hour).Add(time.Hour) // include the current hour
	if err := s.aggregateHours(ctx, from, to, now); err != nil {
		return err
	}

	next := now.Truncate(time.Hour).Add(-(openHours - 1) * time.Hour)
	if next.After(from) {
		return s.setWatermark(ctx, next)
	}
	return nil
}

// RunAggregator calls CatchUp at once, then every interval, until ctx
// ends. Errors are logged and retried on the next tick: a failed summary
// can always be recomputed, because the raw runs are kept.
func (s *SQLite) RunAggregator(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.CatchUp(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("analytics: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// watermark returns the stored watermark, and false if there is none yet.
func (s *SQLite) watermark(ctx context.Context) (time.Time, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM analytics_state WHERE key = ?`, watermarkKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading watermark: %w", err)
	}
	t, err := time.Parse(timeLayout, value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("bad watermark %q: %w", value, err)
	}
	return t, true, nil
}

// setWatermark stores t as the new watermark.
func (s *SQLite) setWatermark(ctx context.Context, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO analytics_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		watermarkKey, ts(t))
	if err != nil {
		return fmt.Errorf("saving watermark: %w", err)
	}
	return nil
}

// earliestRun returns the hour of the oldest stored run, or now's hour if
// there are no runs yet.
func (s *SQLite) earliestRun(ctx context.Context, now time.Time) (time.Time, error) {
	var first sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(COALESCE(scheduled_at, started_at)) FROM probe_runs`).Scan(&first)
	if err != nil {
		return time.Time{}, fmt.Errorf("finding earliest run: %w", err)
	}
	if !first.Valid {
		return now.Truncate(time.Hour), nil
	}
	t, err := time.Parse(timeLayout, first.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad run time %q: %w", first.String, err)
	}
	return t.Truncate(time.Hour), nil
}
