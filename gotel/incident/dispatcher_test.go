package incident

import (
	"context"
	"testing"
	"time"
)

var ist = time.FixedZone("IST", 5*3600+30*60)

// at is a time on 8 Nov 2026 in IST, e.g. at(23, 30).
func at(hour, minute int) time.Time {
	return time.Date(2026, 11, 8, hour, minute, 0, 0, ist)
}

// memLog remembers recorded statuses, in order.
type memLog struct{ statuses []string }

func (m *memLog) RecordNotification(_ context.Context, r Record) error {
	m.statuses = append(m.statuses, r.Status)
	return nil
}

// newTestDispatcher uses quiet hours 23:00-07:00 IST, a 30 minute
// override, and a clock the test moves by hand.
func newTestDispatcher(clock *time.Time) (*Dispatcher, *recorder, *memLog) {
	out, lg := &recorder{}, &memLog{}
	d := NewDispatcher(out, lg, QuietHours{Start: 23 * 60, End: 7 * 60, Location: ist}, 30*time.Minute)
	d.now = func() time.Time { return *clock }
	return d, out, lg
}

func opened(since time.Time) Event {
	return Event{Kind: Opened, IncidentID: 1, URL: "https://startly-api.onrender.com/health", Since: since, At: since}
}

func closed(since, at time.Time) Event {
	return Event{Kind: Closed, IncidentID: 1, URL: "https://startly-api.onrender.com/health", Since: since, At: at}
}

func TestQuietHoursContains(t *testing.T) {
	q := QuietHours{Start: 23 * 60, End: 7 * 60, Location: ist}
	for _, tt := range []struct {
		t    time.Time
		want bool
	}{
		{at(22, 59), false}, {at(23, 0), true}, {at(2, 0), true},
		{at(6, 59), true}, {at(7, 0), false}, {at(14, 30), false},
	} {
		if got := q.Contains(tt.t); got != tt.want {
			t.Errorf("Contains(%s) = %v, want %v", tt.t.Format("15:04"), got, tt.want)
		}
	}
	if (QuietHours{}).Contains(at(2, 0)) {
		t.Error("zero QuietHours should never be quiet")
	}
}

func TestDaytimeAlertsAreDeliveredAtOnce(t *testing.T) {
	clock := at(14, 30)
	d, out, lg := newTestDispatcher(&clock)

	d.Notify(opened(clock))
	clock = at(17, 3)
	d.Notify(closed(at(14, 30), clock))

	if len(out.events) != 2 || lg.statuses[0] != StatusSent || lg.statuses[1] != StatusSent {
		t.Errorf("delivered %v, statuses %v; want both sent at once", kinds(out), lg.statuses)
	}
}

func TestShortNightOutageBecomesOneMorningSummary(t *testing.T) {
	clock := at(3, 10)
	d, out, lg := newTestDispatcher(&clock)

	d.Notify(opened(clock)) // down at 03:10
	clock = at(3, 25)
	d.Notify(closed(at(3, 10), clock)) // back at 03:25, before the override
	clock = at(5, 0)
	d.Tick()
	if len(out.events) != 0 {
		t.Fatalf("delivered %v during quiet hours, want nothing", kinds(out))
	}

	clock = at(7, 0) // quiet hours end
	d.Tick()
	if got := kinds(out); len(got) != 1 || got[0] != Summary {
		t.Fatalf("delivered %v at 07:00, want one Summary", got)
	}
	if out.events[0].Duration() != 15*time.Minute {
		t.Errorf("summary duration = %v, want 15m", out.events[0].Duration())
	}
	// Both held decisions were recorded, so nothing was invisible.
	want := []string{StatusHeld, StatusHeld, StatusSentAfterQuiet}
	if len(lg.statuses) != 3 || lg.statuses[0] != want[0] || lg.statuses[1] != want[1] || lg.statuses[2] != want[2] {
		t.Errorf("statuses = %v, want %v", lg.statuses, want)
	}
}

func TestLongNightOutageOverridesQuietHours(t *testing.T) {
	clock := at(2, 0)
	d, out, lg := newTestDispatcher(&clock)

	d.Notify(opened(clock))
	clock = at(2, 29)
	d.Tick()
	if len(out.events) != 0 {
		t.Fatalf("delivered %v after 29 min, want nothing yet", kinds(out))
	}
	clock = at(2, 30)
	d.Tick()
	if got := kinds(out); len(got) != 1 || got[0] != Opened {
		t.Fatalf("delivered %v after 30 min, want the DOWN alert", got)
	}
	if lg.statuses[len(lg.statuses)-1] != StatusSentOverride {
		t.Errorf("last status = %q, want %q", lg.statuses[len(lg.statuses)-1], StatusSentOverride)
	}

	// The DOWN alert went out, so the recovery is sent at once, even at night.
	clock = at(3, 0)
	d.Notify(closed(at(2, 0), clock))
	if got := kinds(out); len(got) != 2 || got[1] != Closed {
		t.Errorf("delivered %v, want the RECOVERED alert right away", got)
	}
}

func TestStillDownWhenQuietHoursEndIsSentThen(t *testing.T) {
	clock := at(6, 50)
	d, out, lg := newTestDispatcher(&clock)

	d.Notify(opened(clock)) // down at 06:50, only 10 min before 07:00
	clock = at(7, 0)
	d.Tick()
	if got := kinds(out); len(got) != 1 || got[0] != Opened {
		t.Fatalf("delivered %v at 07:00, want the DOWN alert", got)
	}
	if lg.statuses[len(lg.statuses)-1] != StatusSentAfterQuiet {
		t.Errorf("last status = %q, want %q", lg.statuses[len(lg.statuses)-1], StatusSentAfterQuiet)
	}
}

func TestStoppingRecordsHeldAlertsAsUndelivered(t *testing.T) {
	clock := at(1, 0)
	d, out, lg := newTestDispatcher(&clock)
	d.Notify(opened(clock))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Run(ctx, time.Hour) // returns at once, since ctx is already done

	if len(out.events) != 0 || lg.statuses[len(lg.statuses)-1] != StatusUndelivered {
		t.Errorf("delivered %v, statuses %v; want nothing delivered and %q recorded",
			kinds(out), lg.statuses, StatusUndelivered)
	}
}
