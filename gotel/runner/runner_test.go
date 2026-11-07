package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"gotel/probe"
)

// scriptedProber returns its results in order (repeating the last one)
// and records when each attempt started.
type scriptedProber struct {
	results []probe.Result

	mu    sync.Mutex
	calls []time.Time
}

func (s *scriptedProber) Probe(ctx context.Context, url string) probe.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(len(s.calls), len(s.results)-1)
	s.calls = append(s.calls, time.Now())
	return s.results[i]
}

var (
	ok200  = probe.Result{StatusCode: 200}
	err503 = probe.Result{StatusCode: 503}
	err404 = probe.Result{StatusCode: 404}
)

// fastPolicy keeps test waits short.
func fastPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, BaseDelay: 20 * time.Millisecond, MaxDelay: time.Second}
}

func classes(run Run) []FailureClass {
	var out []FailureClass
	for _, a := range run.Attempts {
		out = append(out, a.Class)
	}
	return out
}

func TestDoSuccessFirstTry(t *testing.T) {
	p := &scriptedProber{results: []probe.Result{ok200}}
	run := Do(context.Background(), p, "http://a.test/", fastPolicy(), probe.Default2xx)

	if run.Outcome != OutcomeSuccess || len(run.Attempts) != 1 {
		t.Errorf("outcome = %q with %d attempts, want success with 1", run.Outcome, len(run.Attempts))
	}
}

// Phase 4 exit criterion: transient failures remain visible.
func TestDoRecoveredKeepsEveryAttempt(t *testing.T) {
	p := &scriptedProber{results: []probe.Result{err503, err503, ok200}}
	run := Do(context.Background(), p, "http://a.test/", fastPolicy(), probe.Default2xx)

	if run.Outcome != OutcomeRecovered {
		t.Errorf("outcome = %q, want %q", run.Outcome, OutcomeRecovered)
	}
	want := []FailureClass{ClassHTTP5xx, ClassHTTP5xx, ClassNone}
	got := classes(run)
	if len(got) != len(want) {
		t.Fatalf("attempt classes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] || run.Attempts[i].Number != i+1 {
			t.Errorf("attempt %d: class %q number %d, want %q number %d",
				i, got[i], run.Attempts[i].Number, want[i], i+1)
		}
	}
}

func TestDoFailedAfterAllAttempts(t *testing.T) {
	p := &scriptedProber{results: []probe.Result{err503}}
	run := Do(context.Background(), p, "http://a.test/", fastPolicy(), probe.Default2xx)

	if run.Outcome != OutcomeFailed || len(run.Attempts) != 3 {
		t.Errorf("outcome = %q with %d attempts, want failed with 3", run.Outcome, len(run.Attempts))
	}
}

func TestDoDoesNotRetryNonRetryable(t *testing.T) {
	p := &scriptedProber{results: []probe.Result{err404, ok200}}
	run := Do(context.Background(), p, "http://a.test/", fastPolicy(), probe.Default2xx)

	if run.Outcome != OutcomeFailed || len(run.Attempts) != 1 {
		t.Errorf("outcome = %q with %d attempts, want failed with 1 (404 is not retried)",
			run.Outcome, len(run.Attempts))
	}
}

func TestDoBacksOffExponentially(t *testing.T) {
	p := &scriptedProber{results: []probe.Result{err503}}
	Do(context.Background(), p, "http://a.test/", fastPolicy(), probe.Default2xx)

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 3 {
		t.Fatalf("got %d attempts, want 3", len(p.calls))
	}
	first, second := p.calls[1].Sub(p.calls[0]), p.calls[2].Sub(p.calls[1])
	if first < 20*time.Millisecond {
		t.Errorf("first wait = %v, want >= 20ms", first)
	}
	if second < 40*time.Millisecond {
		t.Errorf("second wait = %v, want >= 40ms (doubled)", second)
	}
}

func TestDoStopsBeforeRunDeadline(t *testing.T) {
	// The run has 50ms; the first retry would only start after 100ms.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rp := fastPolicy()
	rp.BaseDelay = 100 * time.Millisecond

	p := &scriptedProber{results: []probe.Result{err503}}
	start := time.Now()
	run := Do(ctx, p, "http://a.test/", rp, probe.Default2xx)

	if len(run.Attempts) != 1 || run.Outcome != OutcomeFailed {
		t.Errorf("outcome = %q with %d attempts, want failed with 1", run.Outcome, len(run.Attempts))
	}
	if took := time.Since(start); took > 40*time.Millisecond {
		t.Errorf("Do took %v, want it to stop at once instead of waiting", took)
	}
}

func TestDoCancelledDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rp := fastPolicy()
	rp.BaseDelay = time.Second // a long wait that cancel interrupts

	p := &scriptedProber{results: []probe.Result{err503}}
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	run := Do(ctx, p, "http://a.test/", rp, probe.Default2xx)

	if run.Outcome != OutcomeCancelled {
		t.Errorf("outcome = %q, want %q", run.Outcome, OutcomeCancelled)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("Do took %v, want it to stop promptly on cancel", took)
	}
}
