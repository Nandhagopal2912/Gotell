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
}

// JobResult pairs a Job with what the probe observed.
type JobResult struct {
	Job    Job
	Result probe.Result
}

// Pool runs jobs on a fixed number of worker goroutines.
type Pool struct {
	prober  Prober
	jobs    chan Job
	results chan JobResult
	wg      sync.WaitGroup
}

// NewPool creates a pool with the given number of workers and a job queue
// that holds at most queueSize waiting jobs.
func NewPool(p Prober, workers, queueSize int) *Pool {
	pl := &Pool{
		prober:  p,
		jobs:    make(chan Job, queueSize),
		results: make(chan JobResult, queueSize),
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
		ctx, cancel := context.WithTimeout(context.Background(), job.Timeout)
		r := pl.prober.Probe(ctx, job.URL)
		cancel()
		pl.results <- JobResult{Job: job, Result: r}
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
// then closes the results channel.
func (pl *Pool) Stop() {
	close(pl.jobs)
	pl.wg.Wait()
	close(pl.results)
}
