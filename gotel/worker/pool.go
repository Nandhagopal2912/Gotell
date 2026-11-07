// Package worker runs probe jobs on a fixed number of goroutines
// (blueprint §4.1: bounded worker pool).
package worker

import (
	"context"
	"sync"
	"time"

	"gotel/probe"
)

// Prober is anything that can probe a URL. *probe.Prober satisfies it,
// and tests can pass a fake.
type Prober interface {
	Probe(ctx context.Context, url string) probe.Result
}

// Job is one probe to run.
type Job struct {
	TargetID int
	URL      string
	Timeout  time.Duration // overall deadline for this probe

	// ScheduledAt is when the scheduler wanted this probe to start.
	// Zero means "not scheduled" (e.g. submitted directly).
	ScheduledAt time.Time
}

// JobResult pairs a Job with what the probe observed.
type JobResult struct {
	Job    Job
	Result probe.Result

	StartedAt time.Time // when a worker actually started the probe
	// SchedulerDelay is StartedAt - Job.ScheduledAt (blueprint §4.1:
	// scheduler lag). Zero if the job had no ScheduledAt.
	SchedulerDelay time.Duration
}

// Pool runs jobs on a fixed number of worker goroutines.
type Pool struct {
	prober  Prober
	jobs    chan Job
	results chan JobResult
	wg      sync.WaitGroup

	// ctx is the parent of every probe's context. Cancelling it (Shutdown)
	// stops in-flight probes and makes workers skip queued jobs.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewPool creates a pool with the given number of workers and a job queue
// that holds at most queueSize waiting jobs.
func NewPool(p Prober, workers, queueSize int) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	pl := &Pool{
		prober:  p,
		jobs:    make(chan Job, queueSize),
		results: make(chan JobResult, queueSize),
		ctx:     ctx,
		cancel:  cancel,
	}

	for i := 0; i < workers; i++ {
		pl.wg.Add(1)
		go pl.work()
	}
	return pl
}

// work is the loop each worker goroutine runs: take a job, probe, send the
// result. It ends when the jobs channel is closed and empty.
func (pl *Pool) work() {
	defer pl.wg.Done()
	for job := range pl.jobs {
		if pl.ctx.Err() != nil {
			// Shutting down: a queued job that never started is skipped,
			// not run with an already-cancelled context.
			continue
		}

		jr := JobResult{Job: job, StartedAt: time.Now()}
		if !job.ScheduledAt.IsZero() {
			jr.SchedulerDelay = jr.StartedAt.Sub(job.ScheduledAt)
		}

		ctx, cancel := context.WithTimeout(pl.ctx, job.Timeout)
		jr.Result = pl.prober.Probe(ctx, job.URL)
		cancel()

		pl.results <- jr
	}
}

// Submit queues a job. It blocks while the queue is full.
func (pl *Pool) Submit(job Job) {
	pl.jobs <- job
}

// TrySubmit queues a job without waiting. It returns false if the queue
// is full, so the caller can see the overload (blueprint §4.2).
func (pl *Pool) TrySubmit(job Job) bool {
	select {
	case pl.jobs <- job:
		return true
	default:
		return false
	}
}

// Results returns the channel that completed jobs are sent on.
func (pl *Pool) Results() <-chan JobResult {
	return pl.results
}

// Stop stops accepting jobs, waits for queued and running jobs to finish,
// then closes the results channel. Someone must keep reading Results
// until it is closed. Call Stop or Shutdown once, not both.
func (pl *Pool) Stop() {
	close(pl.jobs)
	pl.wg.Wait()
	pl.cancel() // release the context's resources
	close(pl.results)
}

// Shutdown is the fast stop (blueprint §15): it cancels probes that are
// running, skips jobs still in the queue, waits for the workers to exit,
// then closes the results channel. Cancelled probes still send a result,
// with an error that wraps context.Canceled. Someone must keep reading
// Results until it is closed. Call Stop or Shutdown once, not both.
func (pl *Pool) Shutdown() {
	pl.cancel()
	close(pl.jobs)
	pl.wg.Wait()
	close(pl.results)
}
