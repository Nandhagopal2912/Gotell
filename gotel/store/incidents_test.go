package store

import (
	"context"
	"testing"
	"time"

	"gotel/incident"
	"gotel/runner"
)

// The SQLite store must satisfy the detector's Store interface.
var _ incident.Store = (*SQLite)(nil)

func TestIncidentsOpenAndCloseInSQLite(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	id, err := s.OpenIncident(ctx, incident.OpenIncident{
		TargetID: 1, Since: hour22, Cause: runner.ClassTimeout, FailedRuns: 3,
	}, hour22.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	open, err := s.OpenIncidents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != id || !open[0].Since.Equal(hour22) ||
		open[0].Cause != runner.ClassTimeout || open[0].FailedRuns != 3 {
		t.Fatalf("open incidents = %+v", open)
	}

	if err := s.CloseIncident(ctx, id, hour22.Add(2*time.Hour), 40); err != nil {
		t.Fatal(err)
	}
	if open, err = s.OpenIncidents(ctx); err != nil || len(open) != 0 {
		t.Errorf("after close: %+v (err %v), want none open", open, err)
	}
}

// The SQLite store must also satisfy the dispatcher's Log interface.
var _ incident.Log = (*SQLite)(nil)

func TestNotificationsAreRecorded(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	id, err := s.OpenIncident(ctx, incident.OpenIncident{TargetID: 1, Since: hour22, Cause: runner.ClassTimeout, FailedRuns: 3}, hour22)
	if err != nil {
		t.Fatal(err)
	}
	e := incident.Event{Kind: incident.Opened, IncidentID: id, TargetID: 1}
	for _, st := range []string{incident.StatusHeld, incident.StatusSentOverride} {
		if err := s.RecordNotification(ctx, incident.Record{Event: e, Status: st, At: hour22}); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := s.NotificationCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[incident.StatusHeld] != 1 || counts[incident.StatusSentOverride] != 1 {
		t.Errorf("counts = %v, want one held and one sent_override", counts)
	}
}
