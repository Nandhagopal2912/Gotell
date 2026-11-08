package store

import (
	"context"
	"fmt"
	"time"
)

// DefaultRetention is how long raw runs and attempts are kept (blueprint
// §10.4: 14 days is the starting proposal). Hourly summaries are kept.
const DefaultRetention = 14 * 24 * time.Hour

// Prune deletes raw runs (and their attempts) from hours that are both
// older than retention and already final, that is, before the analytics
// watermark. A run is never deleted before its hour's summary has been
// committed, so nothing is lost from the summaries (blueprint §11.1).
// It returns how many runs were deleted.
//
// Summaries for pruned hours must never be recomputed: there would be no
// raw runs left to count. CatchUp only recomputes from the watermark
// onwards, and the watermark is always newer than anything pruned.
func (s *SQLite) Prune(ctx context.Context, now time.Time, retention time.Duration) (n int64, err error) {
	watermark, ok, err := s.watermark(ctx)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil // nothing is final yet
	}

	cutoff := now.UTC().Add(-retention).Truncate(time.Hour)
	if watermark.Before(cutoff) {
		cutoff = watermark // never delete what is not final yet
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Attempts first: they reference their runs (foreign key).
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM probe_attempts WHERE run_id IN (
			SELECT id FROM probe_runs WHERE COALESCE(scheduled_at, started_at) < ?)`,
		ts(cutoff)); err != nil {
		return 0, fmt.Errorf("deleting old attempts: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM probe_runs WHERE COALESCE(scheduled_at, started_at) < ?`, ts(cutoff))
	if err != nil {
		return 0, fmt.Errorf("deleting old runs: %w", err)
	}
	if n, err = res.RowsAffected(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return n, nil
}
