package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"gotel/incident"
	"gotel/runner"
)

func TestAddAndDeleteTargets(t *testing.T) {
	s := openTemp(t) // already has target 1
	ctx := context.Background()

	id, err := s.AddTarget(ctx, "https://new.test/health", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTarget(ctx, "https://new.test/health", time.Minute); !errors.Is(err, ErrDuplicate) {
		t.Errorf("adding the same URL again: err = %v, want ErrDuplicate", err)
	}

	// An open incident is closed when its target is deleted.
	if _, err := s.OpenIncident(ctx, incident.OpenIncident{TargetID: id, Since: time.Now(), Cause: runner.ClassTimeout, FailedRuns: 3}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTarget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTarget(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice: err = %v, want ErrNotFound", err)
	}

	list, err := s.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != 1 {
		t.Errorf("targets = %+v, want only target 1 (the deleted one is archived)", list)
	}
	if open, _ := s.OpenIncidents(ctx); len(open) != 0 {
		t.Errorf("open incidents = %+v, want the deleted target's incident closed", open)
	}
	// Its history is kept: the row still exists, archived.
	var archived int
	s.db.QueryRow(`SELECT COUNT(*) FROM targets WHERE id = ? AND deleted_at IS NOT NULL`, id).Scan(&archived)
	if archived != 1 {
		t.Error("deleted target row was removed, want it archived")
	}

	// The URL can be added again after deletion.
	if _, err := s.AddTarget(ctx, "https://new.test/health", time.Minute); err != nil {
		t.Errorf("re-adding a deleted URL: %v", err)
	}
}

func TestActiveTargetsRemembersPaused(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.PauseTarget(ctx, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActiveTargets(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Paused || got[0].Timeout != 30*time.Second || got[0].Interval != time.Minute {
		t.Errorf("active targets = %+v, want target 1 paused, 1m interval, 30s timeout", got)
	}
}

func TestAdminTokenIsCreatedOnceAndKept(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	first, err := s.AdminToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AdminToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || first != second {
		t.Errorf("tokens %q and %q, want the same 32-character token twice", first, second)
	}
}
