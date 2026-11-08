package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gotel/scheduler"
)

var (
	// ErrNotFound means no active target has that id.
	ErrNotFound = errors.New("target not found")
	// ErrDuplicate means an active target already has that URL.
	ErrDuplicate = errors.New("a target with this URL already exists")
)

// AddTarget saves a new target and returns its id.
func (s *SQLite) AddTarget(ctx context.Context, url string, interval time.Duration) (int, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM targets WHERE url = ? AND deleted_at IS NULL)`, url).Scan(&exists); err != nil {
		return 0, err
	}
	if exists {
		return 0, ErrDuplicate
	}
	now := ts(time.Now())
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO targets (url, interval_seconds, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		url, int(interval.Seconds()), now, now)
	if err != nil {
		return 0, fmt.Errorf("adding target: %w", err)
	}
	id, err := res.LastInsertId()
	return int(id), err
}

// DeleteTarget archives a target: it stops appearing and being expected,
// but its runs, summaries and incidents are kept. An open incident is
// closed, since nobody is watching the target any more.
func (s *SQLite) DeleteTarget(ctx context.Context, id int) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	now := ts(time.Now())
	res, err := tx.ExecContext(ctx,
		`UPDATE targets SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE incidents SET closed_at = ? WHERE target_id = ? AND closed_at IS NULL`, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE target_pauses SET resumed_at = ? WHERE target_id = ? AND resumed_at IS NULL`, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ActiveTargets returns the targets to schedule at start-up, including
// whether each one was paused when Gotell last stopped.
func (s *SQLite) ActiveTargets(ctx context.Context, timeout time.Duration) ([]scheduler.Target, error) {
	rows, err := s.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.Target, 0, len(rows))
	for _, t := range rows {
		out = append(out, scheduler.Target{
			ID: t.ID, URL: t.URL, Interval: time.Duration(t.IntervalSeconds) * time.Second,
			Timeout: timeout, Paused: t.Paused,
		})
	}
	return out, nil
}

// adminTokenKey stores the generated admin token in analytics_state.
const adminTokenKey = "admin_token"

// AdminToken returns the token that protects changes made through the
// API, creating a random one the first time. Keeping it in the database
// means it stays the same across restarts (and container upgrades).
func (s *SQLite) AdminToken(ctx context.Context) (string, error) {
	var token string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM analytics_state WHERE key = ?`, adminTokenKey).Scan(&token)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token = hex.EncodeToString(b)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO analytics_state (key, value) VALUES (?, ?)`, adminTokenKey, token); err != nil {
		return "", err
	}
	return token, nil
}
