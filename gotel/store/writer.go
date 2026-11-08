package store

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"gotel/runner"
	"gotel/worker"
)

// BatchStore is anything that can save a batch of runs in one go.
// *SQLite satisfies it, and tests can pass a fake that fails on purpose.
type BatchStore interface {
	WriteBatch(ctx context.Context, runs []runner.Run) error
}

// WriterConfig controls batching and retries. The values are starting
// points, to be tuned by measurement (blueprint §9.2).
type WriterConfig struct {
	BatchSize        int           // flush when this many runs are waiting
	FlushInterval    time.Duration // ...or when this much time has passed
	RetryBase        time.Duration // first wait after a transient failure
	RetryMax         time.Duration // waits double up to this cap
	ShutdownAttempts int           // tries per batch once input has closed
}

// DefaultWriterConfig returns 100 runs or 3s per batch (the blueprint's v3
// values), retries from 100ms up to 5s, and 5 tries at shutdown.
func DefaultWriterConfig() WriterConfig {
	return WriterConfig{
		BatchSize:        100,
		FlushInterval:    3 * time.Second,
		RetryBase:        100 * time.Millisecond,
		RetryMax:         5 * time.Second,
		ShutdownAttempts: 5,
	}
}

// WriterStats is a snapshot of the writer's health (blueprint §14).
type WriterStats struct {
	Batches     int64 // successful transactions
	Written     int64 // runs saved
	WriteErrors int64 // failed write attempts, retried or not
	Quarantined int64 // runs refused by the schema, logged and not saved
	Lost        int64 // runs that could not be saved at all
	Pending     int   // runs waiting in memory, not yet saved
}

// Writer is the single goroutine that saves runs (blueprint §9.2).
type Writer struct {
	store BatchStore
	cfg   WriterConfig

	mu    sync.Mutex // guards stats
	stats WriterStats
}

// NewWriter creates a Writer that saves to store.
func NewWriter(store BatchStore, cfg WriterConfig) *Writer {
	return &Writer{store: store, cfg: cfg}
}

// Stats returns a consistent snapshot of the counters.
func (w *Writer) Stats() WriterStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// Run reads results until in is closed, saving them in batches. It
// returns nil after the final flush, or an error if storage failed in a
// way that retrying cannot fix. Whatever the result, every run it could
// not save has been counted in Lost and logged.
func (w *Writer) Run(in <-chan worker.JobResult) error {
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	var batch []runner.Run
	for {
		select {
		case jr, ok := <-in:
			if !ok {
				// Input closed: the engine has stopped. Final flush.
				return w.flush(batch, true)
			}
			batch = append(batch, jr.Run)
			w.setPending(len(batch))
			if len(batch) >= w.cfg.BatchSize {
				if err := w.flush(batch, false); err != nil {
					return err
				}
				batch = nil
			}

		case <-ticker.C:
			if len(batch) > 0 {
				if err := w.flush(batch, false); err != nil {
					return err
				}
				batch = nil
			}
		}
	}
}

// flush saves batch, retrying transient failures. It never discards the
// batch: it either saves it, quarantines the bad records in it, or counts
// it as lost and returns an error.
func (w *Writer) flush(batch []runner.Run, final bool) error {
	if len(batch) == 0 {
		return nil
	}
	delay := w.cfg.RetryBase
	for attempt := 1; ; attempt++ {
		err := w.store.WriteBatch(context.Background(), batch)
		if err == nil {
			w.mu.Lock()
			w.stats.Batches++
			w.stats.Written += int64(len(batch))
			w.stats.Pending = 0
			w.mu.Unlock()
			return nil
		}

		w.mu.Lock()
		w.stats.WriteErrors++
		w.mu.Unlock()

		switch kind := ClassifyError(err); kind {
		case BadRecord:
			return w.quarantine(batch, final, err)

		case Fatal:
			w.lose(len(batch))
			return fmt.Errorf("storage failed, %d runs not saved: %w", len(batch), err)

		default: // Transient
			if final && attempt >= w.cfg.ShutdownAttempts {
				w.lose(len(batch))
				return fmt.Errorf("shutting down: %d runs not saved after %d tries: %w",
					len(batch), attempt, err)
			}
			log.Printf("store: write of %d runs failed (try %d, %v), retrying in %v: %v",
				len(batch), attempt, kind, delay, err)
			time.Sleep(delay)
			delay = min(delay*2, w.cfg.RetryMax)
		}
	}
}

// quarantine handles a batch that a schema rule rejected. One bad run
// makes the whole transaction fail, so each run is retried on its own:
// the good ones are saved, and only the bad ones are left out.
func (w *Writer) quarantine(batch []runner.Run, final bool, cause error) error {
	if len(batch) == 1 {
		run := batch[0]
		log.Printf("store: QUARANTINED run for target %d (outcome %s, started %s): %v",
			run.TargetID, run.Outcome, run.StartedAt.Format(time.RFC3339), cause)
		w.mu.Lock()
		w.stats.Quarantined++
		w.mu.Unlock()
		return nil
	}
	for _, run := range batch {
		if err := w.flush([]runner.Run{run}, final); err != nil {
			return err
		}
	}
	return nil
}

// lose counts runs that could not be saved and says so loudly.
func (w *Writer) lose(n int) {
	log.Printf("store: ERROR %d runs could not be saved", n)
	w.mu.Lock()
	w.stats.Lost += int64(n)
	w.stats.Pending = 0
	w.mu.Unlock()
}

func (w *Writer) setPending(n int) {
	w.mu.Lock()
	w.stats.Pending = n
	w.mu.Unlock()
}
