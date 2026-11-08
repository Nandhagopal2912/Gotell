package incident

import (
	"fmt"
	"io"
	"time"
)

// Printer is a Notifier that writes events as text, in the given time
// zone. It is the delivery channel until the dashboard exists.
type Printer struct {
	Out      io.Writer
	Location *time.Location
}

// Notify prints one event.
func (p Printer) Notify(e Event) {
	loc := p.Location
	if loc == nil {
		loc = time.Local
	}
	switch e.Kind {
	case Opened:
		fmt.Fprintf(p.Out, "[DOWN]      %s is down since %s (%d failed runs, cause %s)\n",
			e.URL, e.Since.In(loc).Format("15:04 MST"), e.FailedRuns, e.Cause)
	case Closed:
		fmt.Fprintf(p.Out, "[RECOVERED] %s recovered at %s, down for %s (%d failed runs, last cause %s)\n",
			e.URL, e.At.In(loc).Format("15:04 MST"), e.Duration().Round(time.Second), e.FailedRuns, e.Cause)
	case Summary:
		fmt.Fprintf(p.Out, "[OVERNIGHT] %s was down from %s to %s (%s, %d failed runs, last cause %s)\n",
			e.URL, e.Since.In(loc).Format("15:04"), e.At.In(loc).Format("15:04 MST"),
			e.Duration().Round(time.Second), e.FailedRuns, e.Cause)
	}
}
