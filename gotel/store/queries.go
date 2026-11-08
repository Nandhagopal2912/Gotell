package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// TargetRow is a target as the dashboard sees it.
type TargetRow struct {
	ID              int
	URL             string
	IntervalSeconds int
	CreatedAt       time.Time
	Paused          bool    // an open maintenance window
	OpenIncident    bool    // currently down
	LastOutcome     *string // outcome of the latest run that was not cancelled; nil if none
}

// ListTargets returns every target with its current state.
func (s *SQLite) ListTargets(ctx context.Context) ([]TargetRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.url, t.interval_seconds, t.created_at,
		       EXISTS (SELECT 1 FROM target_pauses p
		               WHERE p.target_id = t.id AND p.resumed_at IS NULL),
		       EXISTS (SELECT 1 FROM incidents i
		               WHERE i.target_id = t.id AND i.closed_at IS NULL),
		       -- Cancelled runs (stopped by us) say nothing about the target.
		       (SELECT r.outcome FROM probe_runs r
		        WHERE r.target_id = t.id AND r.outcome != 'cancelled'
		        ORDER BY r.started_at DESC LIMIT 1)
		FROM targets t
		ORDER BY t.id`)
	if err != nil {
		return nil, fmt.Errorf("listing targets: %w", err)
	}
	defer rows.Close()

	var out []TargetRow
	for rows.Next() {
		var t TargetRow
		var created string
		var last sql.NullString
		if err := rows.Scan(&t.ID, &t.URL, &t.IntervalSeconds, &created,
			&t.Paused, &t.OpenIncident, &last); err != nil {
			return nil, err
		}
		if t.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
			return nil, fmt.Errorf("target %d: bad created_at %q: %w", t.ID, created, err)
		}
		if last.Valid {
			t.LastOutcome = &last.String
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AttemptRow is one stored attempt. Pointer fields are nil when the value
// was not measured (stored as NULL).
type AttemptRow struct {
	Number         int
	StartedAt      time.Time
	StatusCode     *int
	FailureClass   string
	DNSms          *float64
	TCPConnectms   *float64
	TLSHandshakems *float64
	TTFBms         *float64
	Totalms        float64
	ConnReused     bool
	BodyBytes      int64
	Error          *string
}

// RunRow is one stored run with its attempts.
type RunRow struct {
	ID               int64
	TargetID         int
	ScheduledAt      *time.Time
	StartedAt        time.Time
	CompletedAt      time.Time
	Outcome          string
	SchedulerDelayms *float64
	Attempts         []AttemptRow
}

// RecentRuns returns a target's latest runs, newest first, with attempts.
func (s *SQLite) RecentRuns(ctx context.Context, targetID, limit int) ([]RunRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, target_id, scheduled_at, started_at, completed_at, outcome, scheduler_delay_ms
		FROM probe_runs WHERE target_id = ?
		ORDER BY started_at DESC LIMIT ?`, targetID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading runs: %w", err)
	}
	var runs []RunRow
	index := make(map[int64]int) // run id -> position in runs
	for rows.Next() {
		var r RunRow
		var scheduled sql.NullString
		var delay sql.NullFloat64
		var started, completed string
		if err := rows.Scan(&r.ID, &r.TargetID, &scheduled, &started, &completed, &r.Outcome, &delay); err != nil {
			rows.Close()
			return nil, err
		}
		if r.StartedAt, err = time.Parse(timeLayout, started); err != nil {
			rows.Close()
			return nil, err
		}
		if r.CompletedAt, err = time.Parse(timeLayout, completed); err != nil {
			rows.Close()
			return nil, err
		}
		if scheduled.Valid {
			t, err := time.Parse(timeLayout, scheduled.String)
			if err != nil {
				rows.Close()
				return nil, err
			}
			r.ScheduledAt = &t
		}
		r.SchedulerDelayms = nullable(delay)
		index[r.ID] = len(runs)
		runs = append(runs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(runs) == 0 {
		return runs, err
	}

	// One query for all their attempts.
	ids := make([]any, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	arows, err := s.db.QueryContext(ctx, `
		SELECT run_id, attempt_number, started_at, status_code, failure_class,
		       dns_ms, tcp_connect_ms, tls_handshake_ms, ttfb_ms, total_time_ms,
		       connection_reused, body_bytes, error_message
		FROM probe_attempts WHERE run_id IN (`+placeholders+`)
		ORDER BY run_id, attempt_number`, ids...)
	if err != nil {
		return nil, fmt.Errorf("reading attempts: %w", err)
	}
	defer arows.Close()
	for arows.Next() {
		var a AttemptRow
		var runID int64
		var started string
		var status sql.NullInt64
		var dns, tcp, tlsMs, ttfb sql.NullFloat64
		var errMsg sql.NullString
		if err := arows.Scan(&runID, &a.Number, &started, &status, &a.FailureClass,
			&dns, &tcp, &tlsMs, &ttfb, &a.Totalms, &a.ConnReused, &a.BodyBytes, &errMsg); err != nil {
			return nil, err
		}
		if a.StartedAt, err = time.Parse(timeLayout, started); err != nil {
			return nil, err
		}
		if status.Valid {
			code := int(status.Int64)
			a.StatusCode = &code
		}
		a.DNSms, a.TCPConnectms, a.TLSHandshakems, a.TTFBms = nullable(dns), nullable(tcp), nullable(tlsMs), nullable(ttfb)
		if errMsg.Valid {
			a.Error = &errMsg.String
		}
		i := index[runID]
		runs[i].Attempts = append(runs[i].Attempts, a)
	}
	return runs, arows.Err()
}

// IncidentRow is a stored incident with its target's URL.
type IncidentRow struct {
	ID         int64
	TargetID   int
	URL        string
	OpenedAt   time.Time
	DetectedAt time.Time
	ClosedAt   *time.Time // nil while still open
	Cause      string
	FailedRuns int
}

// RecentIncidents returns the latest incidents, newest first.
func (s *SQLite) RecentIncidents(ctx context.Context, limit int) ([]IncidentRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.target_id, t.url, i.opened_at, i.detected_at, i.closed_at,
		       i.cause, i.failed_runs
		FROM incidents i JOIN targets t ON t.id = i.target_id
		ORDER BY i.opened_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading incidents: %w", err)
	}
	defer rows.Close()

	var out []IncidentRow
	for rows.Next() {
		var in IncidentRow
		var opened, detected string
		var closed sql.NullString
		if err := rows.Scan(&in.ID, &in.TargetID, &in.URL, &opened, &detected, &closed,
			&in.Cause, &in.FailedRuns); err != nil {
			return nil, err
		}
		if in.OpenedAt, err = time.Parse(timeLayout, opened); err != nil {
			return nil, err
		}
		if in.DetectedAt, err = time.Parse(timeLayout, detected); err != nil {
			return nil, err
		}
		if closed.Valid {
			t, err := time.Parse(timeLayout, closed.String)
			if err != nil {
				return nil, err
			}
			in.ClosedAt = &t
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
