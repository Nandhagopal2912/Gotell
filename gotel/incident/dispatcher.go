package incident

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Summary is a Kind for an outage that started and ended during quiet
// hours: one message the next morning instead of two at night.
const Summary Kind = "summary"

// QuietHours is a daily window, in a time zone, when non-urgent alerts are
// held instead of delivered (blueprint §12.3). Start and End are minutes
// after midnight; the window may cross midnight (23:00 to 07:00). Equal
// Start and End means "no quiet hours".
type QuietHours struct {
	Start, End int
	Location   *time.Location
}

// Contains reports whether t falls inside the quiet window.
func (q QuietHours) Contains(t time.Time) bool {
	if q.Start == q.End {
		return false
	}
	loc := q.Location
	if loc == nil {
		loc = time.Local
	}
	local := t.In(loc)
	m := local.Hour()*60 + local.Minute()
	if q.Start < q.End {
		return m >= q.Start && m < q.End
	}
	return m >= q.Start || m < q.End // crosses midnight
}

// Delivery statuses, recorded for every notification decision so that a
// held alert is never invisible (blueprint §12.3).
const (
	StatusSent           = "sent"             // delivered right away
	StatusHeld           = "held"             // quiet hours: kept for later
	StatusSentOverride   = "sent_override"    // still down after the override time
	StatusSentAfterQuiet = "sent_after_quiet" // delivered when quiet hours ended
	StatusUndelivered    = "undelivered"      // Gotell stopped while it was held
)

// Record is one notification decision.
type Record struct {
	Event  Event
	Status string
	Reason string
	At     time.Time
}

// Log keeps every notification decision.
type Log interface {
	RecordNotification(ctx context.Context, r Record) error
}

// held is an incident whose alerts are waiting for quiet hours to end.
type held struct {
	opened Event
	closed *Event // set if it recovered while still held
}

// Dispatcher decides when events are delivered. Outside quiet hours it
// delivers at once. Inside, it holds them, records that, and delivers
// later: at once if the target is still down after the override time,
// or when quiet hours end (as one summary if it has already recovered).
// Monitoring never pauses; only delivery waits.
type Dispatcher struct {
	out      Notifier
	log      Log
	quiet    QuietHours
	override time.Duration
	now      func() time.Time // replaced in tests

	mu      sync.Mutex // Notify and Tick run on different goroutines
	pending map[int64]*held
}

// NewDispatcher creates a Dispatcher delivering to out and recording to log.
func NewDispatcher(out Notifier, log Log, quiet QuietHours, override time.Duration) *Dispatcher {
	return &Dispatcher{
		out: out, log: log, quiet: quiet, override: override,
		now:     time.Now,
		pending: make(map[int64]*held),
	}
}

// Notify receives an event from the Detector.
func (d *Dispatcher) Notify(e Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()

	switch e.Kind {
	case Opened:
		if !d.quiet.Contains(now) {
			d.deliver(e, StatusSent, "", now)
			return
		}
		d.pending[e.IncidentID] = &held{opened: e}
		d.record(e, StatusHeld, "quiet hours", now)

	case Closed:
		if h, ok := d.pending[e.IncidentID]; ok {
			// Its DOWN alert was never sent: keep both for one summary.
			h.closed = &e
			d.record(e, StatusHeld, "quiet hours", now)
			return
		}
		// The DOWN alert was delivered, so tell them it is over, even at night.
		d.deliver(e, StatusSent, "", now)
	}
}

// Tick delivers held alerts whose time has come. Call it regularly.
func (d *Dispatcher) Tick() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	quiet := d.quiet.Contains(now)

	for id, h := range d.pending {
		switch {
		case h.closed == nil && now.Sub(h.opened.Since) >= d.override:
			// Critical override: down too long to wait until morning.
			d.deliver(h.opened, StatusSentOverride,
				fmt.Sprintf("still down after %v", d.override), now)
			delete(d.pending, id)

		case !quiet && h.closed == nil:
			d.deliver(h.opened, StatusSentAfterQuiet, "quiet hours ended", now)
			delete(d.pending, id)

		case !quiet:
			s := *h.closed
			s.Kind = Summary
			d.deliver(s, StatusSentAfterQuiet, "recovered during quiet hours", now)
			delete(d.pending, id)
		}
	}
}

// Run calls Tick every interval until ctx ends, then records any alert
// still held as undelivered, so nothing is silently forgotten.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.close()
			return
		case <-ticker.C:
			d.Tick()
		}
	}
}

// close records every still-held alert as undelivered.
func (d *Dispatcher) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for id, h := range d.pending {
		d.record(h.opened, StatusUndelivered, "Gotell stopped during quiet hours", now)
		delete(d.pending, id)
	}
}

// deliver hands e to the output and records it. The caller holds mu.
func (d *Dispatcher) deliver(e Event, status, reason string, now time.Time) {
	d.out.Notify(e)
	d.record(e, status, reason, now)
}

// record saves a decision. A failure is logged, not fatal: the alert
// itself has already been delivered or held. The caller holds mu.
func (d *Dispatcher) record(e Event, status, reason string, now time.Time) {
	r := Record{Event: e, Status: status, Reason: reason, At: now}
	if err := d.log.RecordNotification(context.Background(), r); err != nil {
		log.Printf("notifications: recording %s for incident %d: %v", status, e.IncidentID, err)
	}
}
