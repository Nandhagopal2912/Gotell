// Package scheduler decides which targets are due and hands them to the
// worker pool (blueprint §4.1: central scheduler).
package scheduler

import (
	"context"
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
}

// Scheduler dispatches due targets to a worker pool.
// All of its state is owned by the Run goroutine, so it needs no mutex.
type Scheduler struct {
	pool    *worker.Pool
	tick    time.Duration
	entries map[int]*entry
	results chan worker.JobResult
	skipped int // dispatches refused because the job queue was full
}

// New creates a Scheduler that checks for due targets every tick.
func New(pool *worker.Pool, tick time.Duration, targets []Target) *Scheduler {
	s := &Scheduler{
		pool:    pool,
		tick:    tick,
		entries: make(map[int]*entry),
		results: make(chan worker.JobResult),
	}
	for _, t := range targets {
		s.entries[t.ID] = &entry{target: t} // zero nextDue: due immediately
	}
	return s
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

// dispatchDue submits every target whose next run is due and that has no
// probe in flight (blueprint §4.1: no overlapping runs per target).
func (s *Scheduler) dispatchDue(now time.Time) {
	for _, e := range s.entries {
		if e.inFlight || now.Before(e.nextDue) {
			continue
		}
		job := worker.Job{TargetID: e.target.ID, URL: e.target.URL, Timeout: e.target.Timeout}
		if !s.pool.TrySubmit(job) {
			s.skipped++ // queue full: try again on the next tick
			continue
		}
		e.inFlight = true
		e.nextDue = now.Add(e.target.Interval)
	}
}
