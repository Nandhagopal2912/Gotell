package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gotel/runner"
	"gotel/worker"
)

// failure is an injected error that says its own kind (see kinded).
type failure struct{ kind ErrorKind }

func (f failure) Error() string          { return "injected " + f.kind.String() + " failure" }
func (f failure) StorageKind() ErrorKind { return f.kind }

// fakeStore records saved runs. Before saving, it asks fail (if set)
// whether this batch should fail instead.
type fakeStore struct {
	fail func(call int, runs []runner.Run) error

	mu      sync.Mutex
	calls   int
	batches [][]runner.Run
}

func (f *fakeStore) WriteBatch(ctx context.Context, runs []runner.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		if err := f.fail(f.calls, runs); err != nil {
			return err
		}
	}
	f.batches = append(f.batches, append([]runner.Run(nil), runs...))
	return nil
}

func (f *fakeStore) saved() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []int
	for _, b := range f.batches {
		for _, r := range b {
			ids = append(ids, r.TargetID)
		}
	}
	return ids
}

// fastConfig keeps test waits short.
func fastConfig() WriterConfig {
	return WriterConfig{
		BatchSize:        3,
		FlushInterval:    time.Hour, // only size or shutdown flushes, unless a test says otherwise
		RetryBase:        time.Millisecond,
		RetryMax:         5 * time.Millisecond,
		ShutdownAttempts: 3,
	}
}

// feed sends one result per target ID, then closes the channel.
func feed(ids ...int) <-chan worker.JobResult {
	ch := make(chan worker.JobResult, len(ids))
	for _, id := range ids {
		ch <- worker.JobResult{Run: runner.Run{TargetID: id}}
	}
	close(ch)
	return ch
}

func TestWriterBatchesBySizeAndFlushesOnClose(t *testing.T) {
	store := &fakeStore{}
	w := NewWriter(store, fastConfig())

	if err := w.Run(feed(1, 2, 3, 4, 5)); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	sizes := []int{len(store.batches[0]), len(store.batches[1])}
	store.mu.Unlock()
	if sizes[0] != 3 || sizes[1] != 2 {
		t.Errorf("batch sizes = %v, want [3 2] (one full batch, then the final flush)", sizes)
	}
	if st := w.Stats(); st.Written != 5 || st.Lost != 0 || st.Pending != 0 {
		t.Errorf("Stats = %+v, want Written 5, Lost 0, Pending 0", st)
	}
}

func TestWriterFlushesOnTimer(t *testing.T) {
	store := &fakeStore{}
	cfg := fastConfig()
	cfg.FlushInterval = 20 * time.Millisecond
	w := NewWriter(store, cfg)

	in := make(chan worker.JobResult)
	done := make(chan error)
	go func() { done <- w.Run(in) }()

	in <- worker.JobResult{Run: runner.Run{TargetID: 1}} // far below BatchSize
	time.Sleep(100 * time.Millisecond)
	if got := store.saved(); len(got) != 1 {
		t.Errorf("saved %v before shutdown, want [1]: the timer should have flushed it", got)
	}
	close(in)
	<-done
}

func TestWriterRetriesTransientFailuresWithoutLosingData(t *testing.T) {
	store := &fakeStore{fail: func(call int, _ []runner.Run) error {
		if call <= 3 {
			return failure{Transient} // e.g. "database is locked", three times
		}
		return nil
	}}
	w := NewWriter(store, fastConfig())

	// Three runs fill a batch (BatchSize 3), so the failing flush happens
	// while the engine is running: it must retry until it succeeds.
	if err := w.Run(feed(1, 2, 3)); err != nil {
		t.Fatal(err)
	}
	st := w.Stats()
	if st.Written != 3 || st.WriteErrors != 3 || st.Lost != 0 {
		t.Errorf("Stats = %+v, want Written 3, WriteErrors 3, Lost 0", st)
	}
}

func TestWriterQuarantinesOnlyTheBadRecord(t *testing.T) {
	// Target 99 breaks a schema rule: any batch containing it fails.
	store := &fakeStore{fail: func(_ int, runs []runner.Run) error {
		for _, r := range runs {
			if r.TargetID == 99 {
				return failure{BadRecord}
			}
		}
		return nil
	}}
	w := NewWriter(store, fastConfig())

	if err := w.Run(feed(1, 99, 2)); err != nil {
		t.Fatal(err)
	}
	got := store.saved()
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("saved %v, want [1 2]: the good runs must not be blocked", got)
	}
	if st := w.Stats(); st.Quarantined != 1 || st.Lost != 0 {
		t.Errorf("Stats = %+v, want Quarantined 1, Lost 0", st)
	}
}

func TestWriterStopsOnFatalError(t *testing.T) {
	store := &fakeStore{fail: func(int, []runner.Run) error { return failure{Fatal} }}
	w := NewWriter(store, fastConfig())

	err := w.Run(feed(1, 2, 3))
	var f failure
	if !errors.As(err, &f) || f.kind != Fatal {
		t.Fatalf("err = %v, want the fatal failure", err)
	}
	if st := w.Stats(); st.Lost != 3 {
		t.Errorf("Lost = %d, want 3 (counted, not silent)", st.Lost)
	}
}

func TestWriterGivesUpAtShutdownAndCountsLoss(t *testing.T) {
	// The database stays locked; the input is already closed (shutdown).
	store := &fakeStore{fail: func(int, []runner.Run) error { return failure{Transient} }}
	w := NewWriter(store, fastConfig())

	start := time.Now()
	err := w.Run(feed(1, 2))
	if err == nil {
		t.Fatal("expected an error reporting unsaved runs, got nil")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %v, want a bounded number of final tries", took)
	}
	if st := w.Stats(); st.Lost != 2 || st.WriteErrors != 3 {
		t.Errorf("Stats = %+v, want Lost 2 after 3 tries", st)
	}
}
