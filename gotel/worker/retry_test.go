package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"gotel/probe"
	"gotel/runner"
)

// flakyProber fails with 503 the first time, then answers 200.
type flakyProber struct {
	mu    sync.Mutex
	calls int
}

func (f *flakyProber) Probe(ctx context.Context, url string) probe.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == 1 {
		return probe.Result{URL: url, StatusCode: 503}
	}
	return probe.Result{URL: url, StatusCode: 200}
}

func TestPoolRunsRetriesAndFillsSchedulingFields(t *testing.T) {
	rp := runner.RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: time.Second}
	pool := NewPoolWithPolicies(&flakyProber{}, 1, 1, rp, probe.Default2xx)

	scheduled := time.Now()
	pool.Submit(Job{TargetID: 7, URL: "http://a.test/", Timeout: time.Second, ScheduledAt: scheduled})
	pool.Stop()

	var got []JobResult
	for jr := range pool.Results() {
		got = append(got, jr)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1 run", len(got))
	}
	run := got[0].Run

	if run.Outcome != runner.OutcomeRecovered {
		t.Errorf("Outcome = %q, want %q", run.Outcome, runner.OutcomeRecovered)
	}
	if len(run.Attempts) != 2 || run.Attempts[0].Class != runner.ClassHTTP5xx {
		t.Errorf("attempts = %+v, want a 5xx attempt followed by a healthy one", run.Attempts)
	}
	if run.TargetID != 7 || run.URL != "http://a.test/" {
		t.Errorf("TargetID = %d, URL = %q, want 7 and http://a.test/", run.TargetID, run.URL)
	}
	if !run.ScheduledAt.Equal(scheduled) || run.SchedulerDelay < 0 {
		t.Errorf("ScheduledAt = %v, SchedulerDelay = %v, want the job's time and a delay >= 0",
			run.ScheduledAt, run.SchedulerDelay)
	}
}
