// Package telemetry is a bounded buffer between the probe engine and the
// storage. When it is full, senders wait (backpressure)
// instead of records being dropped , and every wait is measured
package telemetry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gotel/worker"
)

// Stats is the snapshot of the channel's health
type Stats struct {
	Capacity  int           // size of the buffer
	Depth     int           // records the waiting in the buffer right now
	Sent      int64         // records put into the buffer
	Dropped   int64         // records that could not be sent , bust be zero
	TotalWait time.Duration //time senders spent waiting for space
	MaxWait   time.Duration // longest single wait
}

// Telementry is a bounded, measured channel of run results
type Telementry struct {
	ch        chan worker.JobResult
	mu        sync.Mutex // guards the counters below
	sent      int64
	dropped   int64
	totalWait time.Duration
	maxWait   time.Duration
}

// New creates a telementry channle that holds most capacity records.
func New(capacity int) *Telementry {
	return &Telementry{ch: make(chan worker.JobResult, capacity)}
}

// Send puts jr into the buffer , If the buffer is full it waits for space
// (backpressure). If ctx ends first . the record is not sent : Send counts it
// as dropped and returns as error , so the loss is never silent.
func (c *Telementry) Send(ctx context.Context, jr worker.JobResult) error {
	start := time.Now()

	// Fetch path : if there is space, always take it, even if ctx is done.
	select {
	case c.ch <- jr:
		c.record(0)
		return nil
	default:
	}

	// Full : wait for space or for ctx to end
	select {
	case c.ch <- jr:
		c.record(time.Since(start))
		return nil

	case <-ctx.Done():
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
		return fmt.Errorf("telemetry: record for target %d not sent: %w", jr.Job.TargetID, ctx.Err())
	}
}

// record updates the counters after a successful send
func (c *Telementry) record(wait time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent++
	c.totalWait += wait
	if wait > c.maxWait {
		c.maxWait = wait
	}
}

// Record returns the channel the reader (the storage writer) ranges over.
// It is closed by Close, after the last record
func (c *Telementry) Records() <-chan worker.JobResult {
	return c.ch
}

// Close tells the reader no more records will come. only the sending side
// may call it, once afer its last Send.
func (c *Telementry) Close() {
	close(c.ch)
}

// Stats returns a consistent snapshot of the counters.
func (c *Telementry) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Capacity:  cap(c.ch),
		Depth:     len(c.ch),
		Sent:      c.sent,
		Dropped:   c.dropped,
		TotalWait: c.totalWait,
		MaxWait:   c.maxWait,
	}
}

// Forward sends everyting from in into c, the n closes c when when in is
// closed. It is the link in the close cascade between the scheduler and
// Storage: run it in its own go routine.
func Forward(ctx context.Context, in <-chan worker.JobResult, c *Telementry) {
	defer c.Close()
	for jr := range in {
		_ = c.Send(ctx, jr) // a failure is aldready counted in Dropped
	}
}
