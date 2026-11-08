package incident

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"gotel/runner"
)

// memStore is an in-memory Store.
type memStore struct {
	nextID int64
	open   map[int64]OpenIncident
	closed map[int64]time.Time
}

func newMemStore() *memStore {
	return &memStore{open: map[int64]OpenIncident{}, closed: map[int64]time.Time{}}
}

func (m *memStore) OpenIncidents(context.Context) ([]OpenIncident, error) {
	var out []OpenIncident
	for _, in := range m.open {
		out = append(out, in)
	}
	return out, nil
}

func (m *memStore) OpenIncident(_ context.Context, in OpenIncident, _ time.Time) (int64, error) {
	m.nextID++
	in.ID = m.nextID
	m.open[in.ID] = in
	return in.ID, nil
}

func (m *memStore) CloseIncident(_ context.Context, id int64, at time.Time, _ int) error {
	delete(m.open, id)
	m.closed[id] = at
	return nil
}

// recorder is a Notifier that remembers events.
type recorder struct{ events []Event }

func (r *recorder) Notify(e Event) { r.events = append(r.events, e) }

var t0 = time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC) // 14:30 IST

// run is the n-th run of target 1, due every 20s from t0.
func run(n int, outcome runner.Outcome) runner.Run {
	due := t0.Add(time.Duration(n) * 20 * time.Second)
	class := runner.ClassNone
	if outcome == runner.OutcomeFailed {
		class = runner.ClassTimeout
	}
	return runner.Run{
		TargetID: 1, URL: "https://startly-api.onrender.com/health",
		ScheduledAt: due, StartedAt: due, CompletedAt: due.Add(10 * time.Second),
		Outcome:  outcome,
		Attempts: []runner.Attempt{{Number: 1, Class: class}},
	}
}

// feed creates a detector (threshold 3) and observes the given outcomes.
func feed(t *testing.T, store *memStore, outcomes ...runner.Outcome) *recorder {
	t.Helper()
	rec := &recorder{}
	d, err := NewDetector(context.Background(), store, rec, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range outcomes {
		if err := d.Observe(context.Background(), run(i, o)); err != nil {
			t.Fatal(err)
		}
	}
	return rec
}

const (
	ok    = runner.OutcomeSuccess
	recov = runner.OutcomeRecovered
	fail  = runner.OutcomeFailed
	canc  = runner.OutcomeCancelled
)

func kinds(r *recorder) []Kind {
	var out []Kind
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func TestOpensOnlyAfterThresholdFailuresInARow(t *testing.T) {
	r := feed(t, newMemStore(), fail, fail)
	if len(r.events) != 0 {
		t.Fatalf("after 2 failures: %v, want no event yet", kinds(r))
	}

	r = feed(t, newMemStore(), ok, fail, fail, fail)
	if len(r.events) != 1 || r.events[0].Kind != Opened {
		t.Fatalf("after 3 failures: %v, want one Opened", kinds(r))
	}
	e := r.events[0]
	if !e.Since.Equal(run(1, fail).ScheduledAt) || e.FailedRuns != 3 || e.Cause != runner.ClassTimeout {
		t.Errorf("Opened = %+v; want since the first failed run, 3 failed runs, cause timeout", e)
	}
}

func TestABlipDoesNotOpenAnIncident(t *testing.T) {
	r := feed(t, newMemStore(), fail, fail, ok, fail, fail, ok)
	if len(r.events) != 0 {
		t.Errorf("events = %v, want none: never 3 failures in a row", kinds(r))
	}
}

func TestOneOutageIsOneIncident(t *testing.T) {
	r := feed(t, newMemStore(), fail, fail, fail, fail, fail, fail, fail, fail)
	if len(r.events) != 1 {
		t.Errorf("events = %v, want exactly one Opened for 8 failures", kinds(r))
	}
}

func TestFirstHealthyRunClosesWithTheDuration(t *testing.T) {
	r := feed(t, newMemStore(), fail, fail, fail, fail, recov)
	if got := kinds(r); len(got) != 2 || got[1] != Closed {
		t.Fatalf("events = %v, want Opened then Closed", got)
	}
	closed := r.events[1]
	// Down from the run due at t0 to the recovered run finishing at t0+90s.
	if closed.Duration() != 90*time.Second || closed.FailedRuns != 4 {
		t.Errorf("Closed: duration %v, failed runs %d; want 1m30s and 4", closed.Duration(), closed.FailedRuns)
	}
}

func TestCancelledRunsAreIgnored(t *testing.T) {
	// Cancelled runs neither break a failure streak nor close an incident.
	r := feed(t, newMemStore(), fail, canc, fail, canc, fail, canc)
	if got := kinds(r); len(got) != 1 || got[0] != Opened {
		t.Errorf("events = %v, want one Opened and no Closed", got)
	}
}

func TestRestartDoesNotOpenTwice(t *testing.T) {
	store := newMemStore()
	feed(t, store, fail, fail, fail) // opens; then Gotell restarts

	// A new detector reloads the open incident: more failures add no new
	// Opened, and the next healthy run closes the original incident.
	r := feed(t, store, fail, fail, fail, fail, ok)
	if got := kinds(r); len(got) != 1 || got[0] != Closed {
		t.Errorf("after restart: %v, want only Closed", got)
	}
	if len(store.closed) != 1 || len(store.open) != 0 {
		t.Errorf("store: %d closed, %d open; want 1 and 0", len(store.closed), len(store.open))
	}
}

func TestTargetsAreIndependent(t *testing.T) {
	rec := &recorder{}
	d, err := NewDetector(context.Background(), newMemStore(), rec, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a := run(i, fail)
		b := run(i, ok)
		b.TargetID = 2
		d.Observe(context.Background(), a)
		d.Observe(context.Background(), b) // target 2 is healthy
	}
	if len(rec.events) != 1 || rec.events[0].TargetID != 1 {
		t.Errorf("events = %+v, want one Opened for target 1 only", rec.events)
	}
}

func TestPrinterUsesTheGivenTimeZone(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+30*60)
	var buf bytes.Buffer
	Printer{Out: &buf, Location: ist}.Notify(Event{
		Kind: Opened, URL: "https://startly-api.onrender.com/health",
		Since: t0, FailedRuns: 3, Cause: runner.ClassTimeout,
	})
	if got := buf.String(); !strings.Contains(got, "since 14:30 IST") || !strings.Contains(got, "[DOWN]") {
		t.Errorf("printed %q, want [DOWN] ... since 14:30 IST", got)
	}
}
