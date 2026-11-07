package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gotel/probe"
)

// blockingProber never finishes on its own: each probe waits until its
// context is cancelled (or a long safety timeout passes).
type blockingProber struct {
	started chan string // receives the URL of each probe as it starts

	mu    sync.Mutex
	calls int
}

func newBlockingProber() *blockingProber {
	return &blockingProber{started: make(chan string, 10)}
}

func (b *blockingProber) Probe(ctx context.Context, url string) probe.Result {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.started <- url

	select {
	case <-ctx.Done():
		return probe.Result{URL: url, Err: ctx.Err()}
	case <-time.After(5 * time.Second):
		return probe.Result{URL: url, StatusCode: 200}
	}
}

// shutdownAndCollect calls Shutdown, reads every remaining result, and
// reports how long the whole thing took.
func shutdownAndCollect(pool *Pool) ([]JobResult, time.Duration) {
	start := time.Now()
	go pool.Shutdown()
	var got []JobResult
	for jr := range pool.Results() {
		got = append(got, jr)
	}
	return got, time.Since(start)
}

func TestPoolShutdownCancelsInFlightProbes(t *testing.T) {
	fake := newBlockingProber()
	pool := NewPool(fake, 2, 10)

	pool.Submit(Job{TargetID: 1, URL: "http://a.test/", Timeout: 10 * time.Second})
	pool.Submit(Job{TargetID: 2, URL: "http://b.test/", Timeout: 10 * time.Second})
	<-fake.started // wait until both probes are really running
	<-fake.started

	got, took := shutdownAndCollect(pool)

	if took > time.Second {
		t.Errorf("Shutdown took %v, want it to cancel running probes quickly", took)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (one per running probe)", len(got))
	}
	for _, jr := range got {
		if !errors.Is(jr.Result.Err, context.Canceled) {
			t.Errorf("target %d: Err = %v, want context.Canceled", jr.Job.TargetID, jr.Result.Err)
		}
	}
}

func TestPoolShutdownSkipsQueuedJobs(t *testing.T) {
	fake := newBlockingProber()
	pool := NewPool(fake, 1, 10) // one worker: the other jobs must wait

	for i := 1; i <= 3; i++ {
		pool.Submit(Job{TargetID: i, URL: "http://a.test/", Timeout: 10 * time.Second})
	}
	<-fake.started // job 1 is running; jobs 2 and 3 are queued

	got, _ := shutdownAndCollect(pool)

	if len(got) != 1 {
		t.Errorf("got %d results, want 1 (only the probe that had started)", len(got))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls != 1 {
		t.Errorf("prober called %d times, want 1 (queued jobs must not run)", fake.calls)
	}
}
