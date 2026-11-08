// Package api serves Gotell's data as JSON for the dashboard, and lets
// the dashboard manage targets (blueprint Phase 9). This file is the
// read-only part; manage.go holds the changes.
package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"gotel/store"
)

// Store is what the API reads and changes. *store.SQLite satisfies it.
type Store interface {
	ListTargets(ctx context.Context) ([]store.TargetRow, error)
	HourlySummaries(ctx context.Context, targetID int, from, to time.Time) ([]store.HourSummary, error)
	RecentRuns(ctx context.Context, targetID, limit int) ([]store.RunRow, error)
	RecentIncidents(ctx context.Context, limit int) ([]store.IncidentRow, error)

	AddTarget(ctx context.Context, url string, interval time.Duration) (int, error)
	DeleteTarget(ctx context.Context, id int) error
	PauseTarget(ctx context.Context, targetID int, at time.Time) error
	ResumeTarget(ctx context.Context, targetID int, at time.Time) error
}

// Control applies target changes to the running scheduler, so they take
// effect at once, without a restart.
type Control interface {
	Add(id int, url string, interval time.Duration) error
	Remove(id int) error
	Pause(id int) error
	Resume(id int) error
}

// Server holds what the handlers need.
type Server struct {
	store Store
	ctl   Control          // nil: management is disabled
	token string           // required for every change; empty: management is disabled
	now   func() time.Time // replaced in tests
}

// New creates a Server reading from s. Changes go to ctl and require
// token; with a nil ctl or an empty token, the API is read-only.
func New(s Store, ctl Control, token string) *Server {
	return &Server{store: s, ctl: ctl, token: token, now: time.Now}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/targets", s.listTargets)
	mux.HandleFunc("GET /api/targets/{id}", s.getTarget)
	mux.HandleFunc("GET /api/targets/{id}/runs", s.listRuns)
	mux.HandleFunc("GET /api/incidents", s.listIncidents)

	mux.Handle("POST /api/targets", s.requireToken(s.addTarget))
	mux.Handle("DELETE /api/targets/{id}", s.requireToken(s.deleteTarget))
	mux.Handle("POST /api/targets/{id}/pause", s.requireToken(s.pauseTarget))
	mux.Handle("POST /api/targets/{id}/resume", s.requireToken(s.resumeTarget))
	return mux
}

// --- response shapes -------------------------------------------------------
//
// Pointers marshal as null: a value that was not measured is null in the
// JSON, never 0 (blueprint §5.2).

type targetJSON struct {
	ID              int       `json:"id"`
	URL             string    `json:"url"`
	IntervalSeconds int       `json:"interval_seconds"`
	CreatedAt       time.Time `json:"created_at"`
	// Status is up, failing (failed, not yet an incident), down (open
	// incident), paused, or unknown (no runs yet).
	Status       string   `json:"status"`
	Uptime24h    *float64 `json:"uptime_24h_pct"`     // nil when nothing in 24h said up or down
	P95TTFB24hMs *float64 `json:"p95_ttfb_24h_ms"`    // worst hourly p95 in 24h
	Missing24h   int      `json:"missing_runs_24h"`   // expected but not recorded
	GapMinutes   float64  `json:"gotell_off_24h_min"` // monitoring gaps, not the target's fault
}

type hourJSON struct {
	Hour           time.Time `json:"hour"`
	ObservedRuns   int       `json:"observed_runs"`
	SuccessRuns    int       `json:"success_runs"`
	RecoveredRuns  int       `json:"recovered_runs"`
	FailedRuns     int       `json:"failed_runs"`
	CancelledRuns  int       `json:"cancelled_runs"`
	UptimePct      *float64  `json:"uptime_pct"`
	LatencySamples int       `json:"latency_samples"`
	NoResponse     int       `json:"no_response_attempts"`
	P50TTFBms      *float64  `json:"p50_ttfb_ms"`
	P95TTFBms      *float64  `json:"p95_ttfb_ms"`
	P99TTFBms      *float64  `json:"p99_ttfb_ms"`
	ExpectedRuns   int       `json:"expected_runs"`
	MissingRuns    int       `json:"missing_runs"`
	WatchedMinutes float64   `json:"watched_min"`
	PausedMinutes  float64   `json:"paused_min"`
	GapMinutes     float64   `json:"gotell_off_min"`
}

type targetDetailJSON struct {
	targetJSON
	Hours []hourJSON `json:"hours"`
}

type attemptJSON struct {
	Number         int       `json:"number"`
	StartedAt      time.Time `json:"started_at"`
	StatusCode     *int      `json:"status_code"`
	FailureClass   string    `json:"failure_class"`
	DNSms          *float64  `json:"dns_ms"`
	TCPConnectms   *float64  `json:"tcp_connect_ms"`
	TLSHandshakems *float64  `json:"tls_handshake_ms"`
	TTFBms         *float64  `json:"ttfb_ms"`
	Totalms        float64   `json:"total_ms"`
	ConnReused     bool      `json:"connection_reused"`
	Error          *string   `json:"error"`
}

type runJSON struct {
	ID               int64         `json:"id"`
	ScheduledAt      *time.Time    `json:"scheduled_at"`
	StartedAt        time.Time     `json:"started_at"`
	CompletedAt      time.Time     `json:"completed_at"`
	Outcome          string        `json:"outcome"`
	SchedulerDelayms *float64      `json:"scheduler_delay_ms"`
	Attempts         []attemptJSON `json:"attempts"`
}

type incidentJSON struct {
	ID          int64      `json:"id"`
	TargetID    int        `json:"target_id"`
	URL         string     `json:"url"`
	OpenedAt    time.Time  `json:"opened_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	Open        bool       `json:"open"`
	DurationSec float64    `json:"duration_sec"` // so far, if still open
	Cause       string     `json:"cause"`
	FailedRuns  int        `json:"failed_runs"`
}

// --- handlers ---------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": s.now().UTC()})
}

func (s *Server) listTargets(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]targetJSON, 0, len(rows))
	for _, t := range rows {
		tj, _, err := s.targetWithHours(r.Context(), t, 24)
		if err != nil {
			internalError(w, err)
			return
		}
		out = append(out, tj)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.findTarget(w, r)
	if !ok {
		return
	}
	hours, ok := intParam(w, r, "hours", 24, 1, 24*14)
	if !ok {
		return
	}
	tj, hs, err := s.targetWithHours(r.Context(), t, hours)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, targetDetailJSON{targetJSON: tj, Hours: hs})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	t, ok := s.findTarget(w, r)
	if !ok {
		return
	}
	limit, ok := intParam(w, r, "limit", 20, 1, 200)
	if !ok {
		return
	}
	runs, err := s.store.RecentRuns(r.Context(), t.ID, limit)
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]runJSON, 0, len(runs))
	for _, run := range runs {
		rj := runJSON{
			ID: run.ID, ScheduledAt: run.ScheduledAt, StartedAt: run.StartedAt,
			CompletedAt: run.CompletedAt, Outcome: run.Outcome,
			SchedulerDelayms: run.SchedulerDelayms, Attempts: []attemptJSON{},
		}
		for _, a := range run.Attempts {
			rj.Attempts = append(rj.Attempts, attemptJSON{
				Number: a.Number, StartedAt: a.StartedAt, StatusCode: a.StatusCode,
				FailureClass: a.FailureClass, DNSms: a.DNSms, TCPConnectms: a.TCPConnectms,
				TLSHandshakems: a.TLSHandshakems, TTFBms: a.TTFBms, Totalms: a.Totalms,
				ConnReused: a.ConnReused, Error: a.Error,
			})
		}
		out = append(out, rj)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request) {
	limit, ok := intParam(w, r, "limit", 50, 1, 500)
	if !ok {
		return
	}
	rows, err := s.store.RecentIncidents(r.Context(), limit)
	if err != nil {
		internalError(w, err)
		return
	}
	now := s.now()
	out := make([]incidentJSON, 0, len(rows))
	for _, in := range rows {
		end := now
		if in.ClosedAt != nil {
			end = *in.ClosedAt
		}
		out = append(out, incidentJSON{
			ID: in.ID, TargetID: in.TargetID, URL: in.URL, OpenedAt: in.OpenedAt,
			ClosedAt: in.ClosedAt, Open: in.ClosedAt == nil,
			DurationSec: end.Sub(in.OpenedAt).Seconds(), Cause: in.Cause, FailedRuns: in.FailedRuns,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers ----------------------------------------------------------------

// findTarget reads {id} from the path and looks the target up, writing a
// 400 or 404 response itself when it cannot.
func (s *Server) findTarget(w http.ResponseWriter, r *http.Request) (store.TargetRow, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "target id must be a positive number")
		return store.TargetRow{}, false
	}
	rows, err := s.store.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return store.TargetRow{}, false
	}
	for _, t := range rows {
		if t.ID == id {
			return t, true
		}
	}
	writeError(w, http.StatusNotFound, "no target with id "+strconv.Itoa(id))
	return store.TargetRow{}, false
}

// targetWithHours builds a target's summary from its last n hours.
func (s *Server) targetWithHours(ctx context.Context, t store.TargetRow, n int) (targetJSON, []hourJSON, error) {
	to := s.now().UTC().Truncate(time.Hour).Add(time.Hour) // include this hour
	from := to.Add(-time.Duration(n) * time.Hour)
	hours, err := s.store.HourlySummaries(ctx, t.ID, from, to)
	if err != nil {
		return targetJSON{}, nil, err
	}

	tj := targetJSON{
		ID: t.ID, URL: t.URL, IntervalSeconds: t.IntervalSeconds, CreatedAt: t.CreatedAt,
		Status: status(t),
	}
	out := make([]hourJSON, 0, len(hours))
	up, countable := 0, 0
	for _, h := range hours {
		up += h.SuccessRuns + h.RecoveredRuns
		countable += h.SuccessRuns + h.RecoveredRuns + h.FailedRuns
		tj.Missing24h += h.MissingRuns
		tj.GapMinutes += h.GapSeconds / 60
		if h.P95TTFBms != nil && (tj.P95TTFB24hMs == nil || *h.P95TTFBms > *tj.P95TTFB24hMs) {
			v := *h.P95TTFBms
			tj.P95TTFB24hMs = &v
		}
		out = append(out, hourJSON{
			Hour: h.Hour, ObservedRuns: h.ObservedRuns, SuccessRuns: h.SuccessRuns,
			RecoveredRuns: h.RecoveredRuns, FailedRuns: h.FailedRuns, CancelledRuns: h.CancelledRuns,
			UptimePct: h.UptimePct, LatencySamples: h.LatencySamples, NoResponse: h.NoResponseAttempts,
			P50TTFBms: h.P50TTFBms, P95TTFBms: h.P95TTFBms, P99TTFBms: h.P99TTFBms,
			ExpectedRuns: h.ExpectedRuns, MissingRuns: h.MissingRuns,
			WatchedMinutes: h.WatchedSeconds / 60, PausedMinutes: h.PausedSeconds / 60,
			GapMinutes: h.GapSeconds / 60,
		})
	}
	if countable > 0 { // otherwise null: nothing said up or down
		pct := 100 * float64(up) / float64(countable)
		tj.Uptime24h = &pct
	}
	return tj, out, nil
}

// status summarises a target's current state in one word.
func status(t store.TargetRow) string {
	switch {
	case t.Paused:
		return "paused"
	case t.OpenIncident:
		return "down"
	case t.LastOutcome == nil:
		return "unknown"
	case *t.LastOutcome == "failed":
		return "failing"
	default:
		return "up"
	}
}

// intParam reads an optional integer query parameter within [min, max],
// writing a 400 response itself if it is invalid.
func intParam(w http.ResponseWriter, r *http.Request, name string, def, min, max int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		writeError(w, http.StatusBadRequest,
			name+" must be a number from "+strconv.Itoa(min)+" to "+strconv.Itoa(max))
		return 0, false
	}
	return n, true
}

// writeJSON sends v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("api: writing response: %v", err)
	}
}

// writeError sends {"error": msg}.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// internalError logs the real error and sends a generic 500, so internal
// details (paths, SQL) never reach the client.
func internalError(w http.ResponseWriter, err error) {
	log.Printf("api: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
