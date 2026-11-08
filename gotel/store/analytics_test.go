package store

import (
	"context"
	"testing"
	"time"

	"gotel/runner"
	"gotel/scheduler"
)

// hour22 is the UTC hour most tests use: 2026-11-08 22:00–23:00.
var hour22 = time.Date(2026, 11, 8, 22, 0, 0, 0, time.UTC)

// runAt is a stored run for target 1, scheduled at the given time.
func runAt(scheduled time.Time, outcome runner.Outcome) runner.Run {
	return runner.Run{
		TargetID:    1,
		ScheduledAt: scheduled,
		StartedAt:   scheduled,
		CompletedAt: scheduled.Add(time.Second),
		Outcome:     outcome,
	}
}

// save writes runs and fails the test on error.
func save(t *testing.T, s *SQLite, runs ...runner.Run) {
	t.Helper()
	if err := s.WriteBatch(context.Background(), runs); err != nil {
		t.Fatal(err)
	}
}

// summaries aggregates the 22:00 hour and returns target 1's rows.
func summaries(t *testing.T, s *SQLite) []HourSummary {
	t.Helper()
	ctx := context.Background()
	if err := s.AggregateHours(ctx, hour22, hour22.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.HourlySummaries(ctx, 1, hour22, hour22.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAggregateCountsOutcomesAndUptime(t *testing.T) {
	s := openTemp(t)
	save(t, s,
		runAt(hour22.Add(1*time.Minute), runner.OutcomeSuccess),
		runAt(hour22.Add(2*time.Minute), runner.OutcomeSuccess),
		runAt(hour22.Add(3*time.Minute), runner.OutcomeRecovered),
		runAt(hour22.Add(4*time.Minute), runner.OutcomeFailed),
		runAt(hour22.Add(5*time.Minute), runner.OutcomeCancelled),
	)

	got := summaries(t, s)
	if len(got) != 1 {
		t.Fatalf("got %d summaries, want 1", len(got))
	}
	h := got[0]
	if h.ObservedRuns != 5 || h.SuccessRuns != 2 || h.RecoveredRuns != 1 ||
		h.FailedRuns != 1 || h.CancelledRuns != 1 {
		t.Errorf("counts = %+v", h)
	}
	// Up = success + recovered = 3; countable = 3 + 1 failed = 4; the
	// cancelled run is not counted either way. 3/4 = 75%.
	if h.UptimePct == nil || *h.UptimePct != 75 {
		t.Errorf("UptimePct = %v, want 75", h.UptimePct)
	}
	if !h.Hour.Equal(hour22) {
		t.Errorf("Hour = %v, want %v", h.Hour, hour22)
	}
}

func TestAggregateUsesTheScheduledHour(t *testing.T) {
	s := openTemp(t)
	// Due at 22:59:59.900 but started late, at 23:00:01 (scheduler lag).
	late := runAt(hour22.Add(time.Hour-100*time.Millisecond), runner.OutcomeSuccess)
	late.StartedAt = hour22.Add(time.Hour + time.Second)
	save(t, s, late)

	got := summaries(t, s)
	if len(got) != 1 || got[0].ObservedRuns != 1 {
		t.Errorf("summaries = %+v, want the run counted in the 22:00 hour", got)
	}
}

func TestAggregateHourWithOnlyCancelledRunsHasNoUptime(t *testing.T) {
	s := openTemp(t)
	save(t, s, runAt(hour22.Add(time.Minute), runner.OutcomeCancelled))

	got := summaries(t, s)
	if len(got) != 1 {
		t.Fatalf("got %d summaries, want 1", len(got))
	}
	if got[0].UptimePct != nil {
		t.Errorf("UptimePct = %v, want nil: nothing said up or down", *got[0].UptimePct)
	}
}

func TestAggregateIsIdempotentAndPicksUpLateRuns(t *testing.T) {
	s := openTemp(t)
	save(t, s, runAt(hour22.Add(time.Minute), runner.OutcomeSuccess))
	summaries(t, s) // first pass
	summaries(t, s) // running it again must not duplicate

	// A run for the same hour is saved late, then the hour is recomputed.
	save(t, s, runAt(hour22.Add(30*time.Minute), runner.OutcomeFailed))
	got := summaries(t, s)

	if len(got) != 1 {
		t.Fatalf("got %d summaries, want exactly 1 row for the hour", len(got))
	}
	if got[0].ObservedRuns != 2 || got[0].UptimePct == nil || *got[0].UptimePct != 50 {
		t.Errorf("after recompute: %+v, want 2 runs and 50%% uptime", got[0])
	}
}

func TestAggregateSeparatesTargetsAndHours(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertTargets(context.Background(), []scheduler.Target{
		{ID: 2, URL: "http://b.test/", Interval: time.Minute},
	}); err != nil {
		t.Fatal(err)
	}
	other := runAt(hour22.Add(time.Minute), runner.OutcomeFailed)
	other.TargetID = 2
	save(t, s,
		runAt(hour22.Add(time.Minute), runner.OutcomeSuccess),
		runAt(hour22.Add(time.Hour+time.Minute), runner.OutcomeFailed), // 23:01, next hour
		other,
	)

	ctx := context.Background()
	if err := s.AggregateHours(ctx, hour22, hour22.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.HourlySummaries(ctx, 1, hour22, hour22.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || *got[0].UptimePct != 100 || *got[1].UptimePct != 0 {
		t.Errorf("target 1 = %+v, want 22:00 at 100%% and 23:00 at 0%%", got)
	}
}
