package scheduler

import (
	"context"
	"testing"
	"time"

	"gotel/worker"
)

// collect counts results that arrive within d.
func collect(s *Scheduler, d time.Duration) int {
	n := 0
	timeout := time.After(d)
	for {
		select {
		case <-s.Results():
			n++
		case <-timeout:
			return n
		}
	}
}

// stopAll cancels the scheduler and reads Results until Run closes it.
func stopAll(cancel context.CancelFunc, s *Scheduler) {
	cancel()
	for range s.Results() {
	}
}

func TestSchedulerPauseAndResume(t *testing.T) {
	fake := newCountingProber(time.Millisecond)
	pool := worker.NewPool(fake, 2, 10)
	targets := []Target{
		{ID: 1, URL: "http://a.test/", Interval: 10 * time.Millisecond, Timeout: time.Second},
	}
	s := New(pool, 5*time.Millisecond, 0, targets)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	defer stopAll(cancel, s)

	if n := collect(s, 100*time.Millisecond); n == 0 {
		t.Fatal("before pause: got no results, want some")
	}

	s.Pause(1)
	collect(s, 30*time.Millisecond) // let a probe already in flight finish
	if n := collect(s, 100*time.Millisecond); n != 0 {
		t.Errorf("while paused: got %d results, want 0", n)
	}

	s.Resume(1)
	if n := collect(s, 100*time.Millisecond); n == 0 {
		t.Error("after resume: got no results, want some")
	}
}

func TestSchedulerMeasuresLag(t *testing.T) {
	// One worker, three targets, and probes slower than the interval:
	// jobs must wait in the queue, so they start later than scheduled.
	fake := newCountingProber(30 * time.Millisecond)
	pool := worker.NewPool(fake, 1, 10)
	targets := []Target{
		{ID: 1, URL: "http://a.test/", Interval: 10 * time.Millisecond, Timeout: time.Second},
		{ID: 2, URL: "http://b.test/", Interval: 10 * time.Millisecond, Timeout: time.Second},
		{ID: 3, URL: "http://c.test/", Interval: 10 * time.Millisecond, Timeout: time.Second},
	}
	s := New(pool, 5*time.Millisecond, 0, targets)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	defer stopAll(cancel, s)

	var maxLag time.Duration
	deadline := time.After(300 * time.Millisecond)
	count := 0
	for done := false; !done; {
		select {
		case jr := <-s.Results():
			count++
			if jr.SchedulerDelay < 0 {
				t.Errorf("target %d: SchedulerDelay = %v, want >= 0", jr.Job.TargetID, jr.SchedulerDelay)
			}
			if jr.SchedulerDelay > maxLag {
				maxLag = jr.SchedulerDelay
			}
		case <-deadline:
			done = true
		}
	}

	if count == 0 {
		t.Fatal("got no results")
	}
	// The third job in line waits for two 30ms probes ahead of it.
	if maxLag < 30*time.Millisecond {
		t.Errorf("max scheduler lag = %v, want >= 30ms under overload", maxLag)
	}
}

func TestNextDueAfterJitterBounds(t *testing.T) {
	now := time.Now()
	const interval = time.Minute
	const jitter = 5 * time.Second
	for i := 0; i < 1000; i++ {
		next := nextDueAfter(now, interval, jitter)
		if next.Before(now.Add(interval)) || !next.Before(now.Add(interval+jitter)) {
			t.Fatalf("next = %v after now, want in [%v, %v)", next.Sub(now), interval, interval+jitter)
		}
	}
	if got := nextDueAfter(now, interval, 0); !got.Equal(now.Add(interval)) {
		t.Errorf("with no jitter: next = %v after now, want exactly %v", got.Sub(now), interval)
	}
}
