package store

import (
	"context"
	"testing"
	"time"

	"gotel/runner"
)

func TestCatchUpComputesEveryHourAndMovesTheWatermark(t *testing.T) {
	s := openTemp(t)
	save(t, s,
		runAt(hour22.Add(5*time.Minute), runner.OutcomeSuccess),
		runAt(hour22.Add(65*time.Minute), runner.OutcomeFailed), // 23:05
	)
	ctx := context.Background()
	now := hour22.Add(3*time.Hour + 10*time.Minute) // 01:10 the next day

	if err := s.CatchUp(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.HourlySummaries(ctx, 1, hour22, hour22.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ObservedRuns != 1 || got[1].ObservedRuns != 1 {
		t.Errorf("summaries = %+v, want 22:00 and 23:00 with one run each", got)
	}

	// Hours before 00:00 are final; 00:00 and 01:00 stay open.
	w, ok, err := s.watermark(ctx)
	if err != nil || !ok || !w.Equal(hour22.Add(2*time.Hour)) {
		t.Errorf("watermark = %v (ok %v, err %v), want 00:00", w, ok, err)
	}
}

func TestCatchUpRecomputesOpenHoursForLateRuns(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := hour22.Add(30 * time.Minute)

	save(t, s, runAt(hour22.Add(5*time.Minute), runner.OutcomeSuccess))
	if err := s.CatchUp(ctx, now); err != nil {
		t.Fatal(err)
	}
	// A run for the same, still open, hour arrives late.
	save(t, s, runAt(hour22.Add(10*time.Minute), runner.OutcomeFailed))
	if err := s.CatchUp(ctx, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := s.HourlySummaries(ctx, 1, hour22, hour22.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ObservedRuns != 2 {
		t.Errorf("summaries = %+v, want the late run included", got)
	}
}

func TestCatchUpAfterALongStopFillsTheMissedHours(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	// Gotell last caught up at 22:00, then was off for three days.
	if err := s.setWatermark(ctx, hour22); err != nil {
		t.Fatal(err)
	}
	save(t, s, runAt(hour22.Add(20*time.Minute), runner.OutcomeSuccess))
	if err := s.CatchUp(ctx, hour22.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := s.HourlySummaries(ctx, 1, hour22, hour22.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ObservedRuns != 1 {
		t.Errorf("summaries = %+v, want the 22:00 hour recovered", got)
	}
}

func TestRunAggregatorStopsWithItsContext(t *testing.T) {
	s := openTemp(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunAggregator(ctx, time.Hour, DefaultRetention)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunAggregator did not stop after its context was cancelled")
	}
}
