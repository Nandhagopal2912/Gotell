package store

import (
	"context"
	"fmt"
	"time"

	"gotel/incident"
	"gotel/runner"
)

// incidentSchema stores incidents so an outage survives a restart and can
// be shown later (blueprint §12.3).
const incidentSchema = `
CREATE TABLE IF NOT EXISTS incidents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id   INTEGER NOT NULL REFERENCES targets(id),
    opened_at   TEXT    NOT NULL,  -- when the first failed run was due
    detected_at TEXT    NOT NULL,  -- when the threshold was reached
    closed_at   TEXT,              -- NULL while the incident is open
    cause       TEXT    NOT NULL,  -- failure class of the latest failed attempt
    failed_runs INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_incidents_open ON incidents(target_id) WHERE closed_at IS NULL;
`

// OpenIncidents returns every incident that has not been closed.
func (s *SQLite) OpenIncidents(ctx context.Context) ([]incident.OpenIncident, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, target_id, opened_at, cause, failed_runs
		FROM incidents WHERE closed_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("reading open incidents: %w", err)
	}
	defer rows.Close()

	var out []incident.OpenIncident
	for rows.Next() {
		var in incident.OpenIncident
		var opened, cause string
		if err := rows.Scan(&in.ID, &in.TargetID, &opened, &cause, &in.FailedRuns); err != nil {
			return nil, err
		}
		if in.Since, err = time.Parse(timeLayout, opened); err != nil {
			return nil, fmt.Errorf("incident %d: bad opened_at %q: %w", in.ID, opened, err)
		}
		in.Cause = runner.FailureClass(cause)
		out = append(out, in)
	}
	return out, rows.Err()
}

// OpenIncident saves a new open incident and returns its id.
func (s *SQLite) OpenIncident(ctx context.Context, in incident.OpenIncident, detectedAt time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO incidents (target_id, opened_at, detected_at, cause, failed_runs)
		VALUES (?, ?, ?, ?, ?)`,
		in.TargetID, ts(in.Since), ts(detectedAt), string(in.Cause), in.FailedRuns)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CloseIncident marks an incident as resolved.
func (s *SQLite) CloseIncident(ctx context.Context, id int64, closedAt time.Time, failedRuns int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET closed_at = ?, failed_runs = ? WHERE id = ?`,
		ts(closedAt), failedRuns, id)
	return err
}
