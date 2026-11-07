// Package runner turns one scheduled check into a run of one or more
// attempts, with a retry policy and failure classification
// (blueprint §6: probe runs and attempts, §7.1: failure classification).
package runner

import (
	"time"

	"gotel/probe"
)

// FailureClass says what kind of failure an attempt had (blueprint §7.1).
type FailureClass string

const (
	ClassNone      FailureClass = "none"       // the attempt was healthy
	ClassDNS       FailureClass = "dns"        // name lookup failed
	ClassConnect   FailureClass = "connect"    // TCP connection failed (e.g. refused)
	ClassTLS       FailureClass = "tls"        // handshake or certificate failed
	ClassTimeout   FailureClass = "timeout"    // a deadline passed before an answer
	ClassHTTP4xx   FailureClass = "http_4xx"   // server answered 4xx not allowed by policy
	ClassHTTP5xx   FailureClass = "http_5xx"   // server answered 5xx not allowed by policy
	ClassHTTPOther FailureClass = "http_other" // other status not allowed by policy (e.g. 3xx)
	ClassBodyRead  FailureClass = "body_read"  // connection broke while reading the body
	ClassCancelled FailureClass = "cancelled"  // stopped by us (shutdown), not the target's fault
	ClassUnknown   FailureClass = "unknown"    // an error we could not classify
)

// Outcome is the final verdict of a whole run (blueprint §6.1).
type Outcome string

const (
	OutcomeSuccess   Outcome = "success"   // healthy on the first attempt
	OutcomeRecovered Outcome = "recovered" // failed at least once, then healthy
	OutcomeFailed    Outcome = "failed"    // no attempt was healthy
	OutcomeCancelled Outcome = "cancelled" // stopped by us before a verdict
)

// Attempt is one HTTP try inside a run. Every attempt is kept, including
// failed ones that a later retry recovered from (blueprint §6.1).
type Attempt struct {
	Number int          // 1 for the first try, 2 for the first retry, ...
	Result probe.Result // everything the probe observed during this try
	Class  FailureClass // ClassNone if the attempt was healthy
}

// Run is one scheduled monitoring check of one target.
type Run struct {
	TargetID int
	URL      string

	ScheduledAt    time.Time     // when the scheduler wanted it to start
	StartedAt      time.Time     // when the first attempt started
	CompletedAt    time.Time     // when the last attempt finished
	SchedulerDelay time.Duration // StartedAt - ScheduledAt (blueprint §4.1)

	Attempts []Attempt // in order; never empty for a run that started
	Outcome  Outcome
}

// RetryPolicy says how many times to try and how long to wait between
// tries. The values are starting points, to be tuned (blueprint §6.2).
type RetryPolicy struct {
	MaxAttempts int           // total tries, including the first (3 = 1 try + 2 retries)
	BaseDelay   time.Duration // wait before the first retry
	MaxDelay    time.Duration // the doubling wait never goes above this

	// AttemptTimeout limits each single try. The whole run is limited by
	// the deadline on the ctx passed to Do. Zero means "no per-try limit".
	AttemptTimeout time.Duration
}

// DefaultRetryPolicy returns 3 attempts with waits of 2s then 4s, each
// attempt limited to 10s.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    3,
		BaseDelay:      2 * time.Second,
		MaxDelay:       15 * time.Second,
		AttemptTimeout: 10 * time.Second,
	}
}
