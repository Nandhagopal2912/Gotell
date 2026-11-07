package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"gotel/probe"
	"gotel/runner"
)

func main() {
	urls := []string{
		"https://startly-api.onrender.com/health",
		"https://does-not-exist.invalid/",
		"https://httpbin.org/status/500",
	}
	p := probe.New(probe.DefaultConfig())

	// Shorter waits than the default so the demo finishes quickly.
	rp := runner.DefaultRetryPolicy()
	rp.BaseDelay = 500 * time.Millisecond

	failed := false
	for _, url := range urls {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		run := runner.Do(ctx, p, url, rp, probe.Default2xx)
		cancel()

		if run.Outcome != runner.OutcomeSuccess && run.Outcome != runner.OutcomeRecovered {
			failed = true
		}
		printRun(run)
	}

	if failed {
		os.Exit(1)
	}
}

// printRun shows a run's outcome and every attempt, failed ones included.
func printRun(run runner.Run) {
	fmt.Printf("%-9s %s  (%d attempt(s), %v)\n",
		run.Outcome, run.URL, len(run.Attempts), run.CompletedAt.Sub(run.StartedAt).Round(time.Millisecond))
	for _, a := range run.Attempts {
		r := a.Result
		fmt.Printf("  #%d %-10s status=%d ttfb=%s dns=%s tcp=%s tls=%s total=%v\n",
			a.Number, a.Class, r.StatusCode, show(r.TTFB), show(r.DNS),
			show(r.TCPConnect), show(r.TLSHandshake), r.Total.Round(time.Millisecond))
		if r.Err != nil {
			fmt.Printf("     error: %v\n", r.Err)
		}
	}
}

// show formats an optional duration: "-" when it was not measured.
func show(d *time.Duration) string {
	if d == nil {
		return "-"
	}
	return d.Round(time.Millisecond).String()
}
