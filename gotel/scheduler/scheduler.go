// Package scheduler decides which targets are due and hands them to the
// worker pool (blueprint §4.1: central scheduler).
package scheduler

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"gotel/worker"
)

// Target is one endpoint to monitor.
type Target struct {
	ID       int
	URL      string
	Interval time.Duration // how often to probe
	Timeout  time.Duration // overall deadline for each probe
	Paused   bool          // start in maintenance mode (e.g. paused before a restart)
}

// entry is the scheduler's private bookkeeping for one target.
type entry struct {
	target   Target
	nextDue  time.Time
	inFlight bool // a probe for this target is queued or running
	paused   bool // maintenance mode: do not dispatch (blueprint §12.2)
}

// commandKind says what a command asks the Run goroutine to do.
type commandKind int

const (
	cmdPause commandKind = iota
	cmdResume
	cmdAdd
	cmdRemove
)

// command asks the Run goroutine to change its targets.
type command struct {
	kind     commandKind
	targetID int
	target   Target // for cmdAdd
}

// ErrBusy means the scheduler could not take a command in time (it is
// shutting down, or very overloaded).
var ErrBusy = errors.New("scheduler busy")

// Scheduler dispatches due targets to a worker pool.
// All of its state is owned by the Run goroutine, so it needs no mutex.
type Scheduler struct {
	pool     *worker.Pool
	tick     time.Duration
	jitter   time.Duration // max random delay added to each next run
	entries  map[int]*entry
	results  chan worker.JobResult
	commands chan command
	skipped  int // dispatches refused because the job queue was full
}

// New creates a Scheduler that checks for due targets every tick.
// Each next run is delayed by a random amount in [0, jitter) so targets
// with the same interval do not all fire at once (blueprint §4.2).
func New(pool *worker.Pool, tick, jitter time.Duration, targets []Target) *Scheduler {
	s := &Scheduler{
		pool:     pool,
		tick:     tick,
		jitter:   jitter,
		entries:  make(map[int]*entry),
		results:  make(chan worker.JobResult),
		commands: make(chan command, 64),
	}
	now := time.Now()
	for _, t := range targets {
		// Spread the first runs out too, instead of all at start-up.
		s.entries[t.ID] = &entry{target: t, paused: t.Paused, nextDue: nextDueAfter(now, 0, jitter)}
	}
	return s
}

// nextDueAfter returns now + interval + a random jitter in [0, jitter).
func nextDueAfter(now time.Time, interval, jitter time.Duration) time.Time {
	next := now.Add(interval)
	if jitter > 0 {
		next = next.Add(rand.N(jitter))
	}
	return next
}

// Pause stops new probes of a target until Resume is called. A probe that
// is already running finishes normally. Like every command, it takes
// effect on the scheduler's next loop.
func (s *Scheduler) Pause(targetID int) error {
	return s.send(command{kind: cmdPause, targetID: targetID})
}

// Resume makes a paused target due again immediately.
func (s *Scheduler) Resume(targetID int) error {
	return s.send(command{kind: cmdResume, targetID: targetID})
}

// Add starts monitoring a new target while the scheduler runs. Its first
// probe is due right away.
func (s *Scheduler) Add(t Target) error {
	return s.send(command{kind: cmdAdd, targetID: t.ID, target: t})
}

// Remove stops monitoring a target. A probe already running finishes, and
// its result is still delivered.
func (s *Scheduler) Remove(targetID int) error {
	return s.send(command{kind: cmdRemove, targetID: targetID})
}

// send queues a command, giving up after a short wait so that a caller
// (such as an HTTP request) never hangs while the scheduler shuts down.
func (s *Scheduler) send(c command) error {
	select {
	case s.commands <- c:
		return nil
	case <-time.After(2 * time.Second):
		return ErrBusy
	}
}

// Results returns completed probes, forwarded from the pool. Callers must
// keep reading it until it is closed; Run closes it after shutting down.
func (s *Scheduler) Results() <-chan worker.JobResult {
	return s.results
}

// Run is the scheduler loop. When ctx is cancelled it shuts down in the
// order of blueprint §15: stop scheduling, cancel running probes, forward
// the last results, then close Results.
func (s *Scheduler) Run(ctx context.Context) {
	defer close(s.results)

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.drain()
			return

		case now := <-ticker.C:
			s.dispatchDue(now)

		case c := <-s.commands:
			s.apply(c)

		case jr := <-s.pool.Results():
			s.finish(jr)
		}
	}
}

// apply carries out one command. Only the Run goroutine calls it.
func (s *Scheduler) apply(c command) {
	switch c.kind {
	case cmdAdd:
		if _, exists := s.entries[c.targetID]; !exists {
			s.entries[c.targetID] = &entry{target: c.target, paused: c.target.Paused, nextDue: time.Now()}
		}
	case cmdRemove:
		delete(s.entries, c.targetID)
	case cmdPause, cmdResume:
		if e, ok := s.entries[c.targetID]; ok {
			e.paused = c.kind == cmdPause
			if !e.paused {
				e.nextDue = time.Now() // resumed: due right away
			}
		}
	}
}

// drain stops the pool and forwards every result it still produces.
// pool.Shutdown closes pool.Results when the workers have exited, which
// ends the range loop.
func (s *Scheduler) drain() {
	go s.pool.Shutdown()
	for jr := range s.pool.Results() {
		s.finish(jr)
	}
}

// finish marks a target's probe as done and forwards its result.
func (s *Scheduler) finish(jr worker.JobResult) {
	if e, ok := s.entries[jr.Job.TargetID]; ok {
		e.inFlight = false // it may be scheduled again
	}
	s.results <- jr
}

// dispatchDue submits every target whose next run is due, that is not
// paused, and that has no probe in flight (blueprint §4.1: no overlapping
// runs per target).
func (s *Scheduler) dispatchDue(now time.Time) {
	for _, e := range s.entries {
		if e.paused || e.inFlight || now.Before(e.nextDue) {
			continue
		}
		job := worker.Job{
			TargetID:    e.target.ID,
			URL:         e.target.URL,
			Timeout:     e.target.Timeout,
			ScheduledAt: e.nextDue,
		}
		if !s.pool.TrySubmit(job) {
			s.skipped++ // queue full: try again on the next tick
			continue
		}
		e.inFlight = true
		e.nextDue = nextDueAfter(now, e.target.Interval, s.jitter)
	}
}
