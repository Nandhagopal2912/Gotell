package store

import (
	"context"
	"testing"
	"time"

	"gotel/runner"
)

// count returns the number of rows in a table.
func count(t *testing.T, s *SQLite, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// oldAndNew saves one run (with an attempt) 20 days before hour22 and one
// in hour22, then catches up as of hour22+3h.
func oldAndNew(t *testing.T, s *SQLite) {
	t.Helper()
	old := runWith(0, runner.OutcomeSuccess, attempt(1, runner.ClassNone, 70))
	old.ScheduledAt = hour22.Add(-20 * 24 * time.Hour)
	old.StartedAt = old.ScheduledAt
	save(t, s, old, runWith(5, runner.OutcomeSuccess, attempt(1, runner.ClassNone, 80)))
	if err := s.CatchUp(context.Background(), hour22.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestPruneDeletesOldRawRunsButKeepsSummaries(t *testing.T) {
	s := openTemp(t)
	oldAndNew(t, s)
	ctx := context.Background()

	n, err := s.Prune(ctx, hour22.Add(3*time.Hour), DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || count(t, s, "probe_runs") != 1 || count(t, s, "probe_attempts") != 1 {
		t.Errorf("deleted %d; runs left %d, attempts left %d; want 1 deleted, 1 and 1 left",
			n, count(t, s, "probe_runs"), count(t, s, "probe_attempts"))
	}

	// The old hour's summary survives the raw data.
	oldHour := hour22.Add(-20 * 24 * time.Hour)
	got, err := s.HourlySummaries(ctx, 1, oldHour, oldHour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ObservedRuns != 1 {
		t.Errorf("old summary = %+v, want it kept with 1 run", got)
	}
}

func TestPruneDoesNothingWithoutAWatermark(t *testing.T) {
	s := openTemp(t)
	old := runAt(hour22.Add(-30*24*time.Hour), runner.OutcomeSuccess)
	save(t, s, old)

	n, err := s.Prune(context.Background(), hour22, DefaultRetention)
	if err != nil || n != 0 || count(t, s, "probe_runs") != 1 {
		t.Errorf("deleted %d (err %v); want nothing deleted before any summary is final", n, err)
	}
}

func TestPruneNeverDeletesHoursThatAreNotFinal(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	// The watermark is stuck 30 days ago (say aggregation kept failing).
	if err := s.setWatermark(ctx, hour22.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	save(t, s, runAt(hour22.Add(-20*24*time.Hour), runner.OutcomeSuccess))

	n, err := s.Prune(ctx, hour22, DefaultRetention)
	if err != nil || n != 0 {
		t.Errorf("deleted %d (err %v); want 0: that run's hour has no final summary yet", n, err)
	}
}
