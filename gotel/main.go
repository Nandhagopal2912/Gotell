package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"gotel/probe"
	"gotel/runner"
	"gotel/scheduler"
	"gotel/telemetry"
	"gotel/worker"
)

func main() {
	duration := flag.Duration("duration", 0, "stop after this long (0 = run until Ctrl+C)")
	flag.Parse()

	targets := []scheduler.Target{
		{ID: 1, URL: "https://startly-api.onrender.com/health", Interval: 20 * time.Second, Timeout: 30 * time.Second},
		{ID: 2, URL: "https://does-not-exist.invalid/", Interval: 20 * time.Second, Timeout: 30 * time.Second},
		{ID: 3, URL: "https://httpbin.org/status/500", Interval: 20 * time.Second, Timeout: 30 * time.Second},
	}

	// Stop on Ctrl+C, or after -duration if it was given.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	rp := runner.DefaultRetryPolicy()
	rp.BaseDelay = 500 * time.Millisecond // short waits for the demo
	pool := worker.NewPoolWithPolicies(probe.New(probe.DefaultConfig()), 4, 16, rp, probe.Default2xx)
	sched := scheduler.New(pool, time.Second, 2*time.Second, targets)
	tel := telemetry.New(64)

	// The pipeline: scheduler -> telemetry -> this loop (storage, in Phase 6).
	go sched.Run(ctx)
	go telemetry.Forward(context.Background(), sched.Results(), tel)

	fmt.Println("Gotell running. Press Ctrl+C to stop.")
	for jr := range tel.Records() { // ends after shutdown has finished
		printRun(jr.Run)
	}

	st := tel.Stats()
	fmt.Printf("\nstopped cleanly. telemetry: sent=%d dropped=%d max_wait=%v capacity=%d\n",
		st.Sent, st.Dropped, st.MaxWait, st.Capacity)
}

// printRun shows a run's outcome and every attempt, failed ones included.
func printRun(run runner.Run) {
	fmt.Printf("%s %-9s %s  (%d attempt(s), lag %v)\n",
		run.StartedAt.Format("15:04:05"), run.Outcome, run.URL, len(run.Attempts),
		run.SchedulerDelay.Round(time.Millisecond))
	for _, a := range run.Attempts {
		r := a.Result
		fmt.Printf("  #%d %-10s status=%d ttfb=%s dns=%s tcp=%s tls=%s total=%v\n",
			a.Number, a.Class, r.StatusCode, show(r.TTFB), show(r.DNS),
			show(r.TCPConnect), show(r.TLSHandshake), r.Total.Round(time.Millisecond))
	}
}

// show formats an optional duration: "-" when it was not measured.
func show(d *time.Duration) string {
	if d == nil {
		return "-"
	}
	return d.Round(time.Millisecond).String()
}
