package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"gotel/runner"
)

func TestPercentileNearestRank(t *testing.T) {
	oneToTen := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	coldStart := []float64{69, 70, 70, 71, 72, 72, 73, 74, 75, 4188} // sorted

	tests := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"p50 of 1..10", oneToTen, 50, 5},
		{"p95 of 1..10", oneToTen, 95, 10},
		{"p99 of 1..10", oneToTen, 99, 10},
		{"p50 hides one cold start", coldStart, 50, 72},
		{"p95 shows the cold start", coldStart, 95, 4188},
		{"single value", []float64{42}, 99, 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile(tt.sorted, tt.p); got != tt.want {
				t.Errorf("percentile(%v) = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
}

// attempt builds an attempt; ttfbMs < 0 means "no response" (nil TTFB).
func attempt(n int, class runner.FailureClass, ttfbMs float64) runner.Attempt {
	a := runner.Attempt{Number: n, StartedAt: hour22, Class: class}
	if ttfbMs >= 0 {
		a.Result.TTFB = dur(time.Duration(ttfbMs * float64(time.Millisecond)))
		a.Result.StatusCode = 200
	}
	return a
}

// runWith is a run in the 22:00 hour with the given attempts.
func runWith(minute int, outcome runner.Outcome, attempts ...runner.Attempt) runner.Run {
	r := runAt(hour22.Add(time.Duration(minute)*time.Minute), outcome)
	r.Attempts = attempts
	return r
}

func TestAggregateLatencyUsesEveryAttemptWithAResponse(t *testing.T) {
	s := openTemp(t)
	save(t, s,
		runWith(1, runner.OutcomeSuccess, attempt(1, runner.ClassNone, 70)),
		runWith(2, runner.OutcomeSuccess, attempt(1, runner.ClassNone, 72)),
		// A cold start: timed out (no response), then answered slowly.
		runWith(3, runner.OutcomeRecovered,
			attempt(1, runner.ClassTimeout, -1),
			attempt(2, runner.ClassNone, 4188)),
		// Stopped by us: not the target's fault, not "no response".
		runWith(4, runner.OutcomeCancelled, attempt(1, runner.ClassCancelled, -1)),
	)

	h := summaries(t, s)[0]
	if h.LatencySamples != 3 || h.NoResponseAttempts != 1 {
		t.Errorf("samples = %d, no response = %d, want 3 and 1", h.LatencySamples, h.NoResponseAttempts)
	}
	want := map[string]struct {
		got  *float64
		want float64
	}{
		"avg":  {h.AvgTTFBms, (70 + 72 + 4188) / 3.0},
		"p50":  {h.P50TTFBms, 72},
		"p95":  {h.P95TTFBms, 4188},
		"p99":  {h.P99TTFBms, 4188},
		"peak": {h.PeakTTFBms, 4188},
	}
	for name, w := range want {
		if w.got == nil || *w.got != w.want {
			t.Errorf("%s = %v, want %v", name, w.got, w.want)
		}
	}
}

func TestAggregateLatencyIsNullWithoutResponses(t *testing.T) {
	s := openTemp(t)
	save(t, s, runWith(1, runner.OutcomeFailed,
		attempt(1, runner.ClassTimeout, -1),
		attempt(2, runner.ClassTimeout, -1)))

	h := summaries(t, s)[0]
	if h.LatencySamples != 0 || h.NoResponseAttempts != 2 {
		t.Errorf("samples = %d, no response = %d, want 0 and 2", h.LatencySamples, h.NoResponseAttempts)
	}
	if h.P50TTFBms != nil || h.AvgTTFBms != nil || h.PeakTTFBms != nil {
		t.Errorf("latency = %v / %v / %v, want all nil: nothing was measured",
			h.AvgTTFBms, h.P50TTFBms, h.PeakTTFBms)
	}
}

// A database created by Phase 7 step 1 has historical_metrics without the
// latency columns. Opening it must add them and keep the existing rows.
func TestOpenMigratesAStep1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(schema + `
		CREATE TABLE historical_metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			target_id INTEGER NOT NULL REFERENCES targets(id),
			hourly_bucket TEXT NOT NULL,
			observed_runs INTEGER NOT NULL, success_runs INTEGER NOT NULL,
			recovered_runs INTEGER NOT NULL, failed_runs INTEGER NOT NULL,
			cancelled_runs INTEGER NOT NULL, uptime_pct REAL,
			computed_at TEXT NOT NULL,
			UNIQUE (target_id, hourly_bucket));
		INSERT INTO targets VALUES (1, 'http://a.test/', 60, 'x', 'x');
		INSERT INTO historical_metrics (target_id, hourly_bucket, observed_runs,
			success_runs, recovered_runs, failed_runs, cancelled_runs, uptime_pct, computed_at)
		VALUES (1, '2026-11-08T22:00:00.000Z', 4, 4, 0, 0, 0, 100, 'x');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a step 1 database: %v", err)
	}
	defer s.Close()

	got, err := s.HourlySummaries(context.Background(), 1, hour22, hour22.Add(time.Hour))
	if err != nil {
		t.Fatalf("reading after migration: %v", err)
	}
	if len(got) != 1 || got[0].ObservedRuns != 4 || got[0].LatencySamples != 0 || got[0].P95TTFBms != nil {
		t.Errorf("after migration: %+v, want the old row kept, with empty latency", got)
	}
}
