package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"gotel/probe"
	"gotel/runner"
	"gotel/scheduler"
	"gotel/store"
	"gotel/telemetry"
	"gotel/worker"
)

func main() {
	duration := flag.Duration("duration", 0, "stop after this long (0 = run until Ctrl+C)")
	dbPath := flag.String("db", "gotell.db", "SQLite database file")
	flag.Parse()

	targets := []scheduler.Target{
		{ID: 1, URL: "https://startly-api.onrender.com/health", Interval: 20 * time.Second, Timeout: 30 * time.Second},
		{ID: 2, URL: "https://does-not-exist.invalid/", Interval: 20 * time.Second, Timeout: 30 * time.Second},
		{ID: 3, URL: "https://httpbin.org/status/500", Interval: 20 * time.Second, Timeout: 30 * time.Second},
	}

	// Open the database first: if it cannot be written, do not start probing.
	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertTargets(context.Background(), targets); err != nil {
		log.Fatal(err)
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
	writer := store.NewWriter(db, store.DefaultWriterConfig())

	// The pipeline: scheduler -> telemetry -> print -> writer -> SQLite.
	go sched.Run(ctx)
	go telemetry.Forward(context.Background(), sched.Results(), tel)

	// A small "tee" stage: print each run, then pass it on to the writer.
	printed := make(chan worker.JobResult)
	go func() {
		defer close(printed)
		for jr := range tel.Records() {
			printRun(jr.Run)
			printed <- jr
		}
	}()

	fmt.Printf("Gotell running, saving to %s. Press Ctrl+C to stop.\n", *dbPath)
	if err := writer.Run(printed); err != nil {
		log.Printf("storage error: %v", err)
		stop() // stop probing: results could not be saved anyway
		lost := 0
		for range printed { // keep draining so the engine can shut down
			lost++
		}
		if lost > 0 {
			log.Printf("ERROR %d more runs were not saved during shutdown", lost)
		}
	}

	st, ws := tel.Stats(), writer.Stats()
	fmt.Printf("\nstopped. telemetry: sent=%d dropped=%d | writer: batches=%d written=%d errors=%d quarantined=%d lost=%d\n",
		st.Sent, st.Dropped, ws.Batches, ws.Written, ws.WriteErrors, ws.Quarantined, ws.Lost)
	if counts, err := db.OutcomeCounts(context.Background()); err == nil {
		fmt.Printf("all runs in %s: %v\n", *dbPath, counts)
	}
	printUptime(db, targets)
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

// printUptime summarises the last 24 hours, hour by hour, for each target.
func printUptime(db *store.SQLite, targets []scheduler.Target) {
	ctx := context.Background()
	to := time.Now().Add(time.Hour) // include the current, unfinished hour
	from := to.Add(-25 * time.Hour)
	if err := db.AggregateHours(ctx, from, to); err != nil {
		log.Printf("analytics: %v", err)
		return
	}

	fmt.Println("\nhourly uptime, last 24h (times shown in UTC and local):")
	for _, t := range targets {
		hours, err := db.HourlySummaries(ctx, t.ID, from, to)
		if err != nil {
			log.Printf("analytics: %v", err)
			return
		}
		fmt.Println(" ", t.URL)
		for _, h := range hours {
			uptime := "  n/a" // no run in this hour could say up or down
			if h.UptimePct != nil {
				uptime = fmt.Sprintf("%5.1f%%", *h.UptimePct)
			}
			fmt.Printf("    %s UTC (%s local)  %s  runs=%d ok=%d recovered=%d failed=%d cancelled=%d\n",
				h.Hour.Format("2006-01-02 15:04"), h.Hour.Local().Format("15:04"), uptime,
				h.ObservedRuns, h.SuccessRuns, h.RecoveredRuns, h.FailedRuns, h.CancelledRuns)
			fmt.Printf("      ttfb p50=%s p95=%s p99=%s peak=%s  (%d samples, %d no response)\n",
				showMs(h.P50TTFBms), showMs(h.P95TTFBms), showMs(h.P99TTFBms), showMs(h.PeakTTFBms),
				h.LatencySamples, h.NoResponseAttempts)
		}
	}
}

// showMs formats an optional millisecond value: "-" when it was not measured.
func showMs(ms *float64) string {
	if ms == nil {
		return "-"
	}
	return fmt.Sprintf("%.0fms", *ms)
}
