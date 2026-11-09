package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"gotel/probe"
)

// slowProber pretends to probe: it sleeps, and records the highest number
// of probes that were running at the same time.
type slowProber struct {
	delay time.Duration

	mu      sync.Mutex
	running int
	maxSeen int
}

func (s *slowProber) Probe(ctx context.Context, url string) probe.Result {
	s.mu.Lock()
	s.running++
	if s.running > s.maxSeen {
		s.maxSeen = s.running
	}
	s.mu.Unlock()

	time.Sleep(s.delay)

	s.mu.Lock()
	s.running--
	s.mu.Unlock()

	return probe.Result{URL: url, StatusCode: 200}
}

// max returns the highest concurrency seen so far.
func (s *slowProber) max() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxSeen
}

func TestPoolNeverExceedsWorkerCount(t *testing.T) {
	const workers = 3
	const jobs = 20

	fake := &slowProber{delay: 20 * time.Millisecond}
	// The queue and results buffers hold all jobs, so Submit and Stop never
	// wait on a reader here. Real callers read Results while jobs run.
	pool := NewPool(fake, workers, jobs)

	for i := 0; i < jobs; i++ {
		pool.Submit(Job{TargetID: i, URL: "http://example.test/", Timeout: time.Second})
	}
	pool.Stop()

	got := 0
	for range pool.Results() {
		got++
	}

	if got != jobs {
		t.Errorf("got %d results, want %d", got, jobs)
	}
	if m := fake.max(); m > workers {
		t.Errorf("max concurrent probes = %d, want <= %d", m, workers)
	}
	if m := fake.max(); m < 2 {
		t.Errorf("max concurrent probes = %d, want workers running in parallel", m)
	}
}

func TestPoolEveryJobGetsItsResult(t *testing.T) {
	fake := &slowProber{delay: time.Millisecond}
	pool := NewPool(fake, 4, 10)

	for i := 0; i < 10; i++ {
		pool.Submit(Job{TargetID: i, URL: "http://example.test/", Timeout: time.Second})
	}
	pool.Stop()

	seen := make(map[int]bool)
	for jr := range pool.Results() {
		if seen[jr.Job.TargetID] {
			t.Errorf("target %d reported twice", jr.Job.TargetID)
		}
		seen[jr.Job.TargetID] = true
	}
	for i := 0; i < 10; i++ {
		if !seen[i] {
			t.Errorf("target %d has no result", i)
		}
	}
}

func TestPoolStatsShowsLoad(t *testing.T) {
	fake := &slowProber{delay: 100 * time.Millisecond}
	pool := NewPool(fake, 2, 8)
	if st := pool.Stats(); st != (PoolStats{Workers: 2, QueueCapacity: 8}) {
		t.Errorf("idle pool stats = %+v", st)
	}
	for i := 0; i < 5; i++ {
		pool.Submit(Job{TargetID: i, URL: "http://example.test/", Timeout: time.Second})
	}
	time.Sleep(50 * time.Millisecond) // both workers are inside a 100 ms probe
	st := pool.Stats()
	if st.Busy != 2 || st.QueueDepth != 3 {
		t.Errorf("loaded pool stats = %+v, want 2 busy and 3 queued", st)
	}
	pool.Stop()
	for range pool.Results() {
	}
	if st := pool.Stats(); st.Busy != 0 || st.QueueDepth != 0 {
		t.Errorf("after Stop: %+v, want nothing busy or queued", st)
	}
}
