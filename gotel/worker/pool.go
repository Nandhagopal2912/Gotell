// Package worker runs probe jobs on a fixed number of goroutines
// (blueprint §4.1: bounded worker pool).
package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"gotel/probe"
	"gotel/runner"
)

// Prober is anything that can probe a URL. *probe.Prober satisfies it,
// and tests can pass a fake.
type Prober interface {
	Probe(ctx context.Context, url string) probe.Result
}

// Job is one scheduled check to run.
type Job struct {
	TargetID int
	URL      string
	Timeout  time.Duration // deadline for the whole run, retries included

	// ScheduledAt is when the scheduler wanted this check to start.
	// Zero means "not scheduled" (e.g. submitted directly).
	ScheduledAt time.Time
}

// JobResult pairs a Job with the run it produced: every attempt, and the
// final outcome (blueprint §6.1).
type JobResult struct {
	Job Job
	Run runner.Run
}

// Pool runs jobs on a fixed number of worker goroutines.
type Pool struct {
	prober  Prober
	retry   runner.RetryPolicy
	status  probe.StatusPolicy
	jobs    chan Job
	results chan JobResult
	wg      sync.WaitGroup
	workers int
	busy    atomic.Int64 // workers running a job right now

	// ctx is the parent of every probe's context. Cancelling it (Shutdown)
	// stops in-flight probes and makes workers skip queued jobs.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewPool creates a pool with the given number of workers and a job queue
// that holds at most queueSize waiting jobs. It uses the default retry
// policy and accepts any 2xx status as healthy.
func NewPool(p Prober, workers, queueSize int) *Pool {
	return NewPoolWithPolicies(p, workers, queueSize, runner.DefaultRetryPolicy(), probe.Default2xx)
}

// NewPoolWithPolicies is NewPool with an explicit retry and status policy.
func NewPoolWithPolicies(p Prober, workers, queueSize int, rp runner.RetryPolicy, sp probe.StatusPolicy) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	pl := &Pool{
		prober:  p,
		retry:   rp,
		status:  sp,
		jobs:    make(chan Job, queueSize),
		results: make(chan JobResult, queueSize),
		workers: workers,
		ctx:     ctx,
		cancel:  cancel,
	}

	for i := 0; i < workers; i++ {
		pl.wg.Add(1)
		go pl.work()
	}
	return pl
}

// work is the loop each worker goroutine runs: take a job, run it (with
// retries), send the result. It ends when the jobs channel is closed and
// empty. While a run waits between retries, this worker stays busy, so
// the target cannot get an overlapping run.
func (pl *Pool) work() {
	defer pl.wg.Done()
	for job := range pl.jobs {
		if pl.ctx.Err() != nil {
			// Shutting down: a queued job that never started is skipped,
			// not run with an already-cancelled context.
			continue
		}

		pl.busy.Add(1)
		ctx, cancel := context.WithTimeout(pl.ctx, job.Timeout)
		run := runner.Do(ctx, pl.prober, job.URL, pl.retry, pl.status)
		cancel()
		pl.busy.Add(-1)

		// runner.Do knows the attempts; the scheduling facts come from the job.
		run.TargetID = job.TargetID
		run.ScheduledAt = job.ScheduledAt
		if !job.ScheduledAt.IsZero() {
			run.SchedulerDelay = run.StartedAt.Sub(job.ScheduledAt)
		}

		pl.results <- JobResult{Job: job, Run: run}
	}
}

// PoolStats is a snapshot of how loaded the pool is (blueprint §13:
// worker utilization and job queue pressure).
type PoolStats struct {
	Workers       int   `json:"workers"`
	Busy          int64 `json:"busy"`
	QueueDepth    int   `json:"queue_depth"`
	QueueCapacity int   `json:"queue_capacity"`
}

// Stats returns the pool's current load. Safe to call from any goroutine.
func (pl *Pool) Stats() PoolStats {
	return PoolStats{
		Workers:       pl.workers,
		Busy:          pl.busy.Load(),
		QueueDepth:    len(pl.jobs),
		QueueCapacity: cap(pl.jobs),
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
// then closes the results channel. Cancelled runs still send a result,
// with Outcome runner.OutcomeCancelled. Someone must keep reading
// Results until it is closed. Call Stop or Shutdown once, not both.
func (pl *Pool) Shutdown() {
	pl.cancel()
	close(pl.jobs)
	pl.wg.Wait()
	close(pl.results)
}
