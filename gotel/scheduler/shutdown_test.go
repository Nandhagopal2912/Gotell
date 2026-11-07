package scheduler

import (
	"context"
	"testing"
	"time"

	"gotel/probe"
	"gotel/worker"
)

// hangingProber waits until its context is cancelled.
type hangingProber struct{}

func (hangingProber) Probe(ctx context.Context, url string) probe.Result {
	<-ctx.Done()
	return probe.Result{URL: url, Err: ctx.Err()}
}

func TestSchedulerShutdownIsPromptAndClosesResults(t *testing.T) {
	pool := worker.NewPool(hangingProber{}, 2, 10)
	targets := []Target{
		{ID: 1, URL: "http://a.test/", Interval: 10 * time.Millisecond, Timeout: time.Minute},
		{ID: 2, URL: "http://b.test/", Interval: 10 * time.Millisecond, Timeout: time.Minute},
	}
	s := New(pool, 5*time.Millisecond, 0, targets)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(runDone)
	}()

	time.Sleep(50 * time.Millisecond) // both probes are now hanging
	start := time.Now()
	cancel()

	got := 0
	for range s.Results() { // ends only when Run closes Results
		got++
	}
	took := time.Since(start)
	<-runDone

	if took > time.Second {
		t.Errorf("shutdown took %v, want the hanging probes cancelled quickly", took)
	}
	if got != 2 {
		t.Errorf("got %d results during shutdown, want 2 (the cancelled probes)", got)
	}
}
