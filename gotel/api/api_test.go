package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"gotel/incident"
	"gotel/runner"
	"gotel/scheduler"
	"gotel/store"
)

// now is the fake "current time" for every test: 22:30 UTC.
var now = time.Date(2026, 11, 8, 22, 30, 0, 0, time.UTC)

// fixture opens a real database with two targets:
//   - 1: three runs at 22:0x (success, recovered, failed) and an open incident
//   - 2: no runs at all
func fixture(t *testing.T) (*store.SQLite, http.Handler) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	if err := db.UpsertTargets(ctx, []scheduler.Target{
		{ID: 1, URL: "https://a.test/health", Interval: 20 * time.Second},
		{ID: 2, URL: "https://b.test/", Interval: time.Minute},
	}); err != nil {
		t.Fatal(err)
	}

	hour := now.Truncate(time.Hour)
	ttfb := 120 * time.Millisecond
	mk := func(min int, outcome runner.Outcome, class runner.FailureClass, withTTFB bool) runner.Run {
		at := hour.Add(time.Duration(min) * time.Minute)
		a := runner.Attempt{Number: 1, StartedAt: at, Class: class}
		if withTTFB {
			a.Result.StatusCode = 200
			a.Result.TTFB = &ttfb
		}
		return runner.Run{TargetID: 1, URL: "https://a.test/health", ScheduledAt: at, StartedAt: at,
			CompletedAt: at.Add(time.Second), Outcome: outcome, Attempts: []runner.Attempt{a}}
	}
	if err := db.WriteBatch(ctx, []runner.Run{
		mk(1, runner.OutcomeSuccess, runner.ClassNone, true),
		mk(2, runner.OutcomeRecovered, runner.ClassNone, true),
		mk(3, runner.OutcomeFailed, runner.ClassTimeout, false),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenIncident(ctx, incident.OpenIncident{
		TargetID: 1, Since: hour.Add(3 * time.Minute), Cause: runner.ClassTimeout, FailedRuns: 3,
	}, hour.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.AggregateHours(ctx, hour, hour.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	s := New(db, nil, "")
	s.now = func() time.Time { return now }
	return db, s.Handler()
}

// get performs a request and decodes the JSON body into out.
func get(t *testing.T, h http.Handler, path string, wantCode int, out any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != wantCode {
		t.Fatalf("GET %s: status %d, want %d (body %s)", path, rec.Code, wantCode, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s: Content-Type %q, want application/json", path, ct)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: bad JSON %q: %v", path, rec.Body, err)
		}
	}
}

func TestHealth(t *testing.T) {
	_, h := fixture(t)
	var body map[string]any
	get(t, h, "/api/health", http.StatusOK, &body)
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestListTargetsShowsStatusAndUptime(t *testing.T) {
	_, h := fixture(t)
	var targets []targetJSON
	get(t, h, "/api/targets", http.StatusOK, &targets)

	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	a, b := targets[0], targets[1]
	// 2 up (success + recovered) of 3 countable = 66.7%.
	if a.Status != "down" || a.Uptime24h == nil || *a.Uptime24h < 66.6 || *a.Uptime24h > 66.7 {
		t.Errorf("target 1 = %+v, want status down and uptime ~66.7%%", a)
	}
	if a.P95TTFB24hMs == nil || *a.P95TTFB24hMs != 120 {
		t.Errorf("target 1 p95 = %v, want 120", a.P95TTFB24hMs)
	}
	// No runs: status unknown, uptime null (not 100%, not 0%).
	if b.Status != "unknown" || b.Uptime24h != nil {
		t.Errorf("target 2 = %+v, want status unknown and uptime null", b)
	}
}

func TestGetTargetIncludesHours(t *testing.T) {
	_, h := fixture(t)
	var got targetDetailJSON
	get(t, h, "/api/targets/1?hours=6", http.StatusOK, &got)
	if got.ID != 1 || len(got.Hours) != 1 || got.Hours[0].ObservedRuns != 3 || got.Hours[0].NoResponse != 1 {
		t.Errorf("target detail = %+v, want 1 hour with 3 runs and 1 no-response", got)
	}
}

func TestListRunsIncludesAttemptsAndNulls(t *testing.T) {
	_, h := fixture(t)
	var runs []runJSON
	get(t, h, "/api/targets/1/runs?limit=2", http.StatusOK, &runs)

	if len(runs) != 2 || runs[0].Outcome != "failed" {
		t.Fatalf("runs = %+v, want 2 runs, newest (failed) first", runs)
	}
	a := runs[0].Attempts[0]
	if a.FailureClass != "timeout" || a.TTFBms != nil || a.StatusCode != nil {
		t.Errorf("attempt = %+v, want timeout with null ttfb and null status", a)
	}
}

func TestListIncidents(t *testing.T) {
	_, h := fixture(t)
	var incidents []incidentJSON
	get(t, h, "/api/incidents", http.StatusOK, &incidents)
	if len(incidents) != 1 || !incidents[0].Open || incidents[0].URL != "https://a.test/health" {
		t.Errorf("incidents = %+v, want one open incident for a.test", incidents)
	}
	// Open since 22:03, "now" is 22:30: 27 minutes so far.
	if incidents[0].DurationSec != 27*60 {
		t.Errorf("duration = %v s, want %v", incidents[0].DurationSec, 27*60)
	}
}

func TestBadRequestsAreRejected(t *testing.T) {
	_, h := fixture(t)
	for _, tt := range []struct {
		path string
		code int
	}{
		{"/api/targets/abc", http.StatusBadRequest},
		{"/api/targets/0", http.StatusBadRequest},
		{"/api/targets/99", http.StatusNotFound},
		{"/api/targets/1/runs?limit=0", http.StatusBadRequest},
		{"/api/targets/1/runs?limit=9999", http.StatusBadRequest},
		{"/api/targets/1?hours=x", http.StatusBadRequest},
		{"/api/incidents?limit=-1", http.StatusBadRequest},
	} {
		var body map[string]string
		get(t, h, tt.path, tt.code, &body)
		if body["error"] == "" {
			t.Errorf("GET %s: no error message in %v", tt.path, body)
		}
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	_, h := fixture(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/targets", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/targets: status %d, want 405", rec.Code)
	}
}

func TestStatusIgnoresCancelledRuns(t *testing.T) {
	db, h := fixture(t)
	// Target 2 fails, then a run is cancelled by a shutdown: it is still failing.
	at := now.Truncate(time.Hour)
	run := func(min int, o runner.Outcome) runner.Run {
		ts := at.Add(time.Duration(min) * time.Minute)
		return runner.Run{TargetID: 2, ScheduledAt: ts, StartedAt: ts, CompletedAt: ts, Outcome: o}
	}
	if err := db.WriteBatch(context.Background(), []runner.Run{
		run(10, runner.OutcomeFailed), run(11, runner.OutcomeCancelled),
	}); err != nil {
		t.Fatal(err)
	}
	var targets []targetJSON
	get(t, h, "/api/targets", http.StatusOK, &targets)
	if targets[1].Status != "failing" {
		t.Errorf("target 2 status = %q, want failing (the cancelled run says nothing)", targets[1].Status)
	}
}
