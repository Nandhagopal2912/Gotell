package scheduler

import (
	"context"
	"testing"
	"time"

	"gotel/worker"
)

func TestAddAndRemoveTargetsWhileRunning(t *testing.T) {
	fake := newCountingProber(time.Millisecond)
	pool := worker.NewPool(fake, 2, 10)
	s := New(pool, 5*time.Millisecond, 0, nil) // starts with no targets

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	defer stopAll(cancel, s)

	if err := s.Add(Target{ID: 7, URL: "http://new.test/", Interval: 10 * time.Millisecond, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if n := collect(s, 100*time.Millisecond); n == 0 {
		t.Fatal("after Add: no results, want the new target probed")
	}

	if err := s.Remove(7); err != nil {
		t.Fatal(err)
	}
	collect(s, 30*time.Millisecond) // a probe already running may still finish
	if n := collect(s, 100*time.Millisecond); n != 0 {
		t.Errorf("after Remove: %d results, want 0", n)
	}
}

func TestTargetCanStartPaused(t *testing.T) {
	fake := newCountingProber(time.Millisecond)
	pool := worker.NewPool(fake, 2, 10)
	s := New(pool, 5*time.Millisecond, 0, []Target{
		{ID: 1, URL: "http://a.test/", Interval: 10 * time.Millisecond, Timeout: time.Second, Paused: true},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	defer stopAll(cancel, s)

	if n := collect(s, 80*time.Millisecond); n != 0 {
		t.Errorf("paused at start: %d results, want 0", n)
	}
	if err := s.Resume(1); err != nil {
		t.Fatal(err)
	}
	if n := collect(s, 80*time.Millisecond); n == 0 {
		t.Error("after Resume: no results")
	}
}
