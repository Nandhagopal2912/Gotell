package store

import (
	"context"
	"testing"
	"time"

	"gotel/runner"
)

func TestOverlap(t *testing.T) {
	at := func(min int) time.Time { return hour22.Add(time.Duration(min) * time.Minute) }
	hour := span{at(0), at(60)}

	tests := []struct {
		name string
		b    span
		want time.Duration
	}{
		{"inside", span{at(10), at(20)}, 10 * time.Minute},
		{"covers it", span{at(-30), at(90)}, time.Hour},
		{"starts before", span{at(-30), at(15)}, 15 * time.Minute},
		{"ends after", span{at(45), at(90)}, 15 * time.Minute},
		{"before", span{at(-30), at(-10)}, 0},
		{"touching", span{at(60), at(70)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := overlap(hour, tt.b); got != tt.want {
				t.Errorf("overlap = %v, want %v", got, tt.want)
			}
		})
	}
}

// coverageDB opens a database whose target 1 (20s interval) has existed
// since well before the 22:00 hour.
func coverageDB(t *testing.T) *SQLite {
	t.Helper()
	s := openTemp(t)
	_, err := s.db.Exec(`UPDATE targets SET interval_seconds = 20, created_at = ? WHERE id = 1`,
		ts(hour22.Add(-24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// session records a clean Gotell run from start to end.
func session(t *testing.T, s *SQLite, start, end time.Time) {
	t.Helper()
	ctx := context.Background()
	id, err := s.StartSession(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(ctx, id, end); err != nil {
		t.Fatal(err)
	}
}

// coverage aggregates the 22:00 hour as if it were now, and returns it.
func coverage(t *testing.T, s *SQLite, now time.Time) HourSummary {
	t.Helper()
	ctx := context.Background()
	if err := s.aggregateHours(ctx, hour22, hour22.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	got, err := s.HourlySummaries(ctx, 1, hour22, hour22.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d summaries, want 1", len(got))
	}
	return got[0]
}

// manyRuns returns n successful runs spread over the 22:00 hour.
func manyRuns(n int) []runner.Run {
	runs := make([]runner.Run, n)
	for i := range runs {
		runs[i] = runAt(hour22.Add(time.Duration(i)*20*time.Second), runner.OutcomeSuccess)
	}
	return runs
}

func TestCoverageFullHourCountsMissingRuns(t *testing.T) {
	s := coverageDB(t)
	session(t, s, hour22.Add(-time.Hour), hour22.Add(2*time.Hour))
	save(t, s, manyRuns(170)...)

	h := coverage(t, s, hour22.Add(3*time.Hour))
	if h.ExpectedRuns != 180 || h.MissingRuns != 10 || h.GapSeconds != 0 {
		t.Errorf("expected = %d, missing = %d, gap = %vs; want 180, 10, 0",
			h.ExpectedRuns, h.MissingRuns, h.GapSeconds)
	}
	// Missing runs are not counted as success or failure.
	if h.UptimePct == nil || *h.UptimePct != 100 {
		t.Errorf("UptimePct = %v, want 100 (from the 170 recorded runs)", h.UptimePct)
	}
}

func TestCoverageGotellOffIsAGapNotMissing(t *testing.T) {
	s := coverageDB(t)
	// Gotell only ran for the second half of the hour.
	session(t, s, hour22.Add(30*time.Minute), hour22.Add(2*time.Hour))
	save(t, s, manyRuns(0)...)

	h := coverage(t, s, hour22.Add(3*time.Hour))
	if h.WatchedSeconds != 1800 || h.GapSeconds != 1800 || h.ExpectedRuns != 90 {
		t.Errorf("watched = %v, gap = %v, expected = %d; want 1800, 1800, 90",
			h.WatchedSeconds, h.GapSeconds, h.ExpectedRuns)
	}
}

func TestCoveragePausedTimeIsNotExpected(t *testing.T) {
	s := coverageDB(t)
	session(t, s, hour22.Add(-time.Hour), hour22.Add(2*time.Hour))
	ctx := context.Background()
	if err := s.PauseTarget(ctx, 1, hour22.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeTarget(ctx, 1, hour22.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	h := coverage(t, s, hour22.Add(3*time.Hour))
	// 60 min - 20 min paused = 40 min watched = 120 runs at 20s.
	if h.PausedSeconds != 1200 || h.WatchedSeconds != 2400 || h.ExpectedRuns != 120 || h.GapSeconds != 0 {
		t.Errorf("paused = %v, watched = %v, expected = %d, gap = %v; want 1200, 2400, 120, 0",
			h.PausedSeconds, h.WatchedSeconds, h.ExpectedRuns, h.GapSeconds)
	}
}

func TestCoverageHourWithNoRunsStillGetsARow(t *testing.T) {
	s := coverageDB(t)
	session(t, s, hour22.Add(-time.Hour), hour22.Add(2*time.Hour))
	// Gotell was running, but not one run was recorded for this hour.

	h := coverage(t, s, hour22.Add(3*time.Hour))
	if h.ObservedRuns != 0 || h.ExpectedRuns != 180 || h.MissingRuns != 180 || h.UptimePct != nil {
		t.Errorf("%+v; want 0 observed, 180 expected, 180 missing, uptime nil", h)
	}
}

func TestCoverageCurrentHourOnlyCountsTheTimeSoFar(t *testing.T) {
	s := coverageDB(t)
	id, err := s.StartSession(context.Background(), hour22.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	now := hour22.Add(15 * time.Minute) // still running, it is 22:15
	if err := s.Heartbeat(context.Background(), id, now); err != nil {
		t.Fatal(err)
	}

	h := coverage(t, s, now)
	if h.WatchedSeconds != 900 || h.ExpectedRuns != 45 || h.GapSeconds != 0 {
		t.Errorf("watched = %v, expected = %d, gap = %v; want 900, 45, 0",
			h.WatchedSeconds, h.ExpectedRuns, h.GapSeconds)
	}
}

func TestCoverageStartsWhenTheTargetWasAdded(t *testing.T) {
	s := coverageDB(t)
	_, err := s.db.Exec(`UPDATE targets SET created_at = ? WHERE id = 1`, ts(hour22.Add(20*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	session(t, s, hour22.Add(-time.Hour), hour22.Add(2*time.Hour))

	h := coverage(t, s, hour22.Add(3*time.Hour))
	if h.WatchedSeconds != 2400 || h.ExpectedRuns != 120 {
		t.Errorf("watched = %v, expected = %d; want 2400 and 120 (from 22:20)", h.WatchedSeconds, h.ExpectedRuns)
	}
}

func TestCoverageCrashedSessionEndsAtLastHeartbeat(t *testing.T) {
	s := coverageDB(t)
	ctx := context.Background()
	id, err := s.StartSession(ctx, hour22)
	if err != nil {
		t.Fatal(err)
	}
	// Last heartbeat at 22:40, then Gotell crashed: no EndSession.
	if err := s.Heartbeat(ctx, id, hour22.Add(40*time.Minute)); err != nil {
		t.Fatal(err)
	}

	h := coverage(t, s, hour22.Add(3*time.Hour))
	if h.WatchedSeconds != 2400 || h.GapSeconds != 1200 {
		t.Errorf("watched = %v, gap = %v; want 2400 and 1200", h.WatchedSeconds, h.GapSeconds)
	}
}
