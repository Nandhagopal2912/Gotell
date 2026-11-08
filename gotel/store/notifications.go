package store

import (
	"context"

	"gotel/incident"
)

// notificationSchema records every notification decision, including
// alerts that were held or never delivered (blueprint §12.3).
const notificationSchema = `
CREATE TABLE IF NOT EXISTS notifications (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    incident_id INTEGER NOT NULL REFERENCES incidents(id),
    target_id   INTEGER NOT NULL REFERENCES targets(id),
    kind        TEXT    NOT NULL,  -- opened, closed or summary
    status      TEXT    NOT NULL,  -- sent, held, sent_override, ...
    reason      TEXT,
    decided_at  TEXT    NOT NULL
);
`

// RecordNotification saves one notification decision.
func (s *SQLite) RecordNotification(ctx context.Context, r incident.Record) error {
	var reason any // NULL when there is nothing to explain
	if r.Reason != "" {
		reason = r.Reason
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notifications (incident_id, target_id, kind, status, reason, decided_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.Event.IncidentID, r.Event.TargetID, string(r.Event.Kind), r.Status, reason, ts(r.At))
	return err
}

// NotificationCounts returns how many notifications have each status.
func (s *SQLite) NotificationCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM notifications GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[status] = n
	}
	return counts, rows.Err()
}
