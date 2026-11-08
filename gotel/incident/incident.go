// Package incident turns a stream of runs into incidents: a target is
// DOWN after several failed runs in a row, and RECOVERED at the first
// healthy run (blueprint §12.3: detection is separate from delivery).
package incident

import (
	"context"
	"fmt"
	"time"

	"gotel/runner"
)

// Kind says what happened to an incident.
type Kind string

const (
	Opened Kind = "opened" // the target is down
	Closed Kind = "closed" // the target has recovered
)

// Event is one change in a target's incident state.
type Event struct {
	Kind       Kind
	IncidentID int64
	TargetID   int
	URL        string
	Since      time.Time           // when the first failed run of the outage was due
	At         time.Time           // when this event was detected
	Cause      runner.FailureClass // failure class of the latest failed attempt
	FailedRuns int                 // failed runs in this outage so far
}

// Duration is how long the outage lasted (for Closed) or has lasted so far.
func (e Event) Duration() time.Duration {
	return e.At.Sub(e.Since)
}

// Notifier delivers events. Today it prints; a dashboard or webhook can
// implement it later without changing detection.
type Notifier interface {
	Notify(Event)
}

// OpenIncident is an incident that has not been closed yet.
type OpenIncident struct {
	ID         int64
	TargetID   int
	Since      time.Time
	Cause      runner.FailureClass
	FailedRuns int
}

// Store keeps incidents, so an outage survives a restart of Gotell.
type Store interface {
	OpenIncidents(ctx context.Context) ([]OpenIncident, error)
	OpenIncident(ctx context.Context, in OpenIncident, detectedAt time.Time) (int64, error)
	CloseIncident(ctx context.Context, id int64, closedAt time.Time, failedRuns int) error
}

// state is the detector's memory for one target.
type state struct {
	streak      int       // failed runs in a row
	streakStart time.Time // when the first of them was due
	open        *OpenIncident
}

// Detector watches runs and opens or closes incidents. It is used by one
// goroutine (the pipeline stage that feeds it), so it needs no mutex.
type Detector struct {
	store     Store
	notifier  Notifier
	threshold int
	targets   map[int]*state
}

// NewDetector creates a Detector that opens an incident after threshold
// failed runs in a row. Incidents still open from an earlier run of
// Gotell are reloaded, so a restart never opens (or alerts) twice.
func NewDetector(ctx context.Context, store Store, notifier Notifier, threshold int) (*Detector, error) {
	d := &Detector{
		store:     store,
		notifier:  notifier,
		threshold: max(threshold, 1),
		targets:   make(map[int]*state),
	}
	open, err := store.OpenIncidents(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading open incidents: %w", err)
	}
	for _, in := range open {
		in := in
		d.target(in.TargetID).open = &in
	}
	return d, nil
}

// target returns the state for a target, creating it if needed.
func (d *Detector) target(id int) *state {
	st := d.targets[id]
	if st == nil {
		st = &state{}
		d.targets[id] = st
	}
	return st
}

// Observe feeds one completed run to the detector.
//
// The rules (Phase 8 design):
//   - failed: counts towards an incident; threshold failures in a row open one.
//   - success or recovered: the target answered, so any open incident closes.
//   - cancelled: stopped by us, says nothing about the target; ignored.
func (d *Detector) Observe(ctx context.Context, run runner.Run) error {
	st := d.target(run.TargetID)

	switch run.Outcome {
	case runner.OutcomeCancelled:
		return nil

	case runner.OutcomeSuccess, runner.OutcomeRecovered:
		st.streak = 0
		if st.open == nil {
			return nil
		}
		in := st.open
		if err := d.store.CloseIncident(ctx, in.ID, run.CompletedAt, in.FailedRuns); err != nil {
			return fmt.Errorf("closing incident %d: %w", in.ID, err)
		}
		st.open = nil
		d.notifier.Notify(Event{
			Kind: Closed, IncidentID: in.ID, TargetID: run.TargetID, URL: run.URL,
			Since: in.Since, At: run.CompletedAt, Cause: in.Cause, FailedRuns: in.FailedRuns,
		})
		return nil

	default: // failed
		if st.streak == 0 {
			st.streakStart = dueTime(run)
		}
		st.streak++
		cause := lastClass(run)

		if st.open != nil { // already down: just count it
			st.open.FailedRuns++
			st.open.Cause = cause
			return nil
		}
		if st.streak < d.threshold {
			return nil // could still be a blip
		}

		in := OpenIncident{TargetID: run.TargetID, Since: st.streakStart, Cause: cause, FailedRuns: st.streak}
		id, err := d.store.OpenIncident(ctx, in, run.CompletedAt)
		if err != nil {
			return fmt.Errorf("opening incident for target %d: %w", run.TargetID, err)
		}
		in.ID = id
		st.open = &in
		d.notifier.Notify(Event{
			Kind: Opened, IncidentID: id, TargetID: run.TargetID, URL: run.URL,
			Since: in.Since, At: run.CompletedAt, Cause: cause, FailedRuns: in.FailedRuns,
		})
		return nil
	}
}

// dueTime is when a run was scheduled (or started, if it was not).
func dueTime(run runner.Run) time.Time {
	if !run.ScheduledAt.IsZero() {
		return run.ScheduledAt
	}
	return run.StartedAt
}

// lastClass is the failure class of a run's last attempt.
func lastClass(run runner.Run) runner.FailureClass {
	if len(run.Attempts) == 0 {
		return runner.ClassUnknown
	}
	return run.Attempts[len(run.Attempts)-1].Class
}
