// Package scheduler decides which targets are due and hands them to the
// worker pool (blueprint §4.1: central scheduler).
package scheduler

import (
	"context"
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
}

// entry is the scheduler's private bookkeeping for one target.
type entry struct {
	target   Target
	nextDue  time.Time
	inFlight bool // a probe for this target is queued or running
	paused   bool // maintenance mode: do not dispatch (blueprint §12.2)
}

// command asks the Run goroutine to pause or resume one target.
type command struct {
	targetID int
	pause    bool
}

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
		commands: make(chan command, 16),
	}
	now := time.Now()
	for _, t := range targets {
		// Spread the first runs out too, instead of all at start-up.
		s.entries[t.ID] = &entry{target: t, nextDue: nextDueAfter(now, 0, jitter)}
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
// is already running finishes normally. It takes effect on the scheduler's
// next loop, so Run must be running.
func (s *Scheduler) Pause(targetID int) {
	s.commands <- command{targetID: targetID, pause: true}
}

// Resume makes a paused target due again immediately.
func (s *Scheduler) Resume(targetID int) {
	s.commands <- command{targetID: targetID, pause: false}
}

// Results returns completed probes, forwarded from the pool.
func (s *Scheduler) Results() <-chan worker.JobResult {
	return s.results
}

// Run is the scheduler loop. It returns when ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case now := <-ticker.C:
			s.dispatchDue(now)

		case c := <-s.commands:
			if e, ok := s.entries[c.targetID]; ok {
				e.paused = c.pause
				if !c.pause {
					e.nextDue = time.Now() // resumed: due right away
				}
			}

		case jr := <-s.pool.Results():
			// The target's probe has finished; it may be scheduled again.
			if e, ok := s.entries[jr.Job.TargetID]; ok {
				e.inFlight = false
			}
			select {
			case s.results <- jr:
			case <-ctx.Done():
				return
			}
		}
	}
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
