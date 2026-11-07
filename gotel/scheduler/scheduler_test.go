package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"gotel/probe"
	"gotel/worker"
)

// countingProber records how many probes ran per URL, and the highest
// number running at once for any single URL.
type countingProber struct {
	delay time.Duration

	mu      sync.Mutex
	running map[string]int
	maxSeen map[string]int
	calls   map[string]int
}

func newCountingProber(delay time.Duration) *countingProber {
	return &countingProber{
		delay:   delay,
		running: make(map[string]int),
		maxSeen: make(map[string]int),
		calls:   make(map[string]int),
	}
}

func (c *countingProber) Probe(ctx context.Context, url string) probe.Result {
	c.mu.Lock()
	c.calls[url]++
	c.running[url]++
	if c.running[url] > c.maxSeen[url] {
		c.maxSeen[url] = c.running[url]
	}
	c.mu.Unlock()

	time.Sleep(c.delay)

	c.mu.Lock()
	c.running[url]--
	c.mu.Unlock()
	return probe.Result{URL: url, StatusCode: 200}
}

// runFor runs the scheduler for d and returns how many results arrived
// per target. Run shuts the pool down and closes Results by itself.
func runFor(t *testing.T, s *Scheduler, d time.Duration) map[int]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	go s.Run(ctx)

	got := make(map[int]int)
	for jr := range s.Results() {
		got[jr.Job.TargetID]++
	}
	return got
}

func TestSchedulerProbesEachTargetRepeatedly(t *testing.T) {
	fake := newCountingProber(time.Millisecond)
	pool := worker.NewPool(fake, 2, 10)
	targets := []Target{
		{ID: 1, URL: "http://a.test/", Interval: 40 * time.Millisecond, Timeout: time.Second},
		{ID: 2, URL: "http://b.test/", Interval: 40 * time.Millisecond, Timeout: time.Second},
	}
	s := New(pool, 5*time.Millisecond, 0, targets)

	got := runFor(t, s, 210*time.Millisecond)

	// About 210/40 ≈ 5 runs each; allow slack for timer granularity.
	for _, tg := range targets {
		if n := got[tg.ID]; n < 3 || n > 7 {
			t.Errorf("target %d: %d results, want about 5", tg.ID, n)
		}
	}
}

func TestSchedulerNeverOverlapsOneTarget(t *testing.T) {
	// Each probe takes longer than the interval, so a naive scheduler
	// would start a second probe of the same target before the first ends.
	fake := newCountingProber(50 * time.Millisecond)
	pool := worker.NewPool(fake, 4, 10)
	targets := []Target{
		{ID: 1, URL: "http://slow.test/", Interval: 10 * time.Millisecond, Timeout: time.Second},
	}
	s := New(pool, 5*time.Millisecond, 0, targets)

	runFor(t, s, 200*time.Millisecond)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if m := fake.maxSeen["http://slow.test/"]; m != 1 {
		t.Errorf("max concurrent probes of one target = %d, want 1", m)
	}
	if c := fake.calls["http://slow.test/"]; c < 2 {
		t.Errorf("probes = %d, want at least 2 (one after another)", c)
	}
}
