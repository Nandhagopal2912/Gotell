package runner

import (
	"context"
	"errors"
	"time"

	"gotel/probe"
)

// Prober is anything that can make one probe attempt. *probe.Prober
// satisfies it, and tests can pass a fake.
type Prober interface {
	Probe(ctx context.Context, url string) probe.Result
}

// Do runs one scheduled check of url: it makes attempts until one is
// healthy, a failure is not worth retrying, the policy's attempts run
// out, or the run's deadline (on ctx) would be passed (blueprint §6).
//
// Every attempt is kept in the returned Run, in order. Do fills URL,
// StartedAt, CompletedAt, Attempts and Outcome; the caller fills the
// scheduling fields (TargetID, ScheduledAt, SchedulerDelay).
func Do(ctx context.Context, p Prober, url string, rp RetryPolicy, sp probe.StatusPolicy) Run {
	run := Run{URL: url, StartedAt: time.Now()}
	delay := rp.BaseDelay
	stoppedByUs := false

	for n := 1; n <= rp.MaxAttempts; n++ {
		started := time.Now()
		r := attempt(ctx, p, url, rp.AttemptTimeout)
		class := Classify(r, sp)
		run.Attempts = append(run.Attempts, Attempt{Number: n, StartedAt: started, Result: r, Class: class})

		if class == ClassNone || !Retryable(class) || n == rp.MaxAttempts {
			break
		}

		// Do not start a wait that would end after the run's deadline:
		// the next attempt could not finish in time (blueprint §6.2).
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(delay).After(deadline) {
			break
		}

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			stoppedByUs = errors.Is(ctx.Err(), context.Canceled)
		}
		if ctx.Err() != nil {
			break
		}
		delay = min(delay*2, rp.MaxDelay)
	}

	run.CompletedAt = time.Now()
	run.Outcome = outcome(run.Attempts, stoppedByUs)
	return run
}

// attempt makes one try, limited by timeout if it is set.
func attempt(ctx context.Context, p Prober, url string, timeout time.Duration) probe.Result {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return p.Probe(ctx, url)
}

// outcome gives the run's verdict from its attempts.
func outcome(attempts []Attempt, stoppedByUs bool) Outcome {
	last := attempts[len(attempts)-1]
	switch {
	case last.Class == ClassNone && len(attempts) == 1:
		return OutcomeSuccess
	case last.Class == ClassNone:
		return OutcomeRecovered // healthy, but only after failing first
	case last.Class == ClassCancelled || stoppedByUs:
		return OutcomeCancelled
	default:
		return OutcomeFailed
	}
}
