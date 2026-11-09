// Command loadtest measures how a Gotell build behaves with many targets
// (blueprint §16.2: "100, 1,000 ... configured targets: memory, goroutine
// count, scheduler lag, worker utilization, queue pressure").
//
// It starts a fake website on localhost whose pages answer slowly and
// sometimes fail, saves N targets on it into a fresh database, runs the
// real Gotell binary against them for a while, samples GET /api/stats, and
// finally reads the saved runs back from the database. Nothing touches the
// internet, so results are repeatable.
//
//	go build -o gotell.exe .
//	go run ./loadtest -gotell ./gotell.exe -targets 100 -duration 10m
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"gotel/store"

	_ "modernc.org/sqlite"
)

func main() {
	gotell := flag.String("gotell", "", "path to the Gotell binary to test (required)")
	targets := flag.Int("targets", 100, "number of targets to watch")
	interval := flag.Duration("interval", time.Minute, "check interval of every target")
	duration := flag.Duration("duration", 10*time.Minute, "how long Gotell runs")
	workers := flag.Int("workers", 16, "Gotell's -workers")
	minLat := flag.Duration("min-latency", 100*time.Millisecond, "fastest fake page")
	maxLat := flag.Duration("max-latency", 600*time.Millisecond, "slowest fake page")
	failRate := flag.Float64("fail-rate", 0.01, "share of fake responses that are 503")
	flag.Parse()
	if *gotell == "" {
		log.Fatal("-gotell is required: build it first with: go build -o gotell.exe .")
	}

	// 1. The fake website.
	var served atomic.Int64
	site, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go http.Serve(site, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		time.Sleep(*minLat + rand.N(*maxLat-*minLat+1))
		if rand.Float64() < *failRate {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	}))

	// 2. A fresh database with N targets on it.
	dir, err := os.MkdirTemp("", "gotell-load-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "load.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	for i := 0; i < *targets; i++ {
		url := fmt.Sprintf("http://%s/page/%d", site.Addr(), i)
		if _, err := db.AddTarget(context.Background(), url, *interval); err != nil {
			log.Fatal(err)
		}
	}
	db.Close()

	// 3. Run Gotell on a free port.
	apiLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	apiAddr := apiLn.Addr().String()
	apiLn.Close()

	logFile, err := os.Create(filepath.Join(dir, "gotell.log"))
	if err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command(*gotell,
		"-db", dbPath, "-listen", apiAddr, "-token", "loadtest", "-allow-private",
		"-max-targets", fmt.Sprint(*targets), "-max-targets-per-host", fmt.Sprint(*targets),
		"-workers", fmt.Sprint(*workers), "-duration", duration.String())
	cmd.Stdout, cmd.Stderr = logFile, logFile
	fmt.Printf("load test: %d targets every %v, %d workers, %v, fake latency %v-%v, %.0f%% failing\n",
		*targets, *interval, *workers, *duration, *minLat, *maxLat, *failRate*100)
	started := time.Now()
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	// 4. Sample Gotell's own numbers until it exits.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var peak peaks
	var last stats
	ticker := time.NewTicker(5 * time.Second)
wait:
	for {
		select {
		case err := <-done:
			if err != nil {
				log.Printf("gotell exited with %v (log: see below)", err)
			}
			break wait
		case <-ticker.C:
			if st, err := sample(apiAddr); err == nil {
				peak.update(st)
				last = st
			}
		}
	}
	wall := time.Since(started)
	logFile.Close()

	// 5. What was saved?
	res, err := analyse(dbPath, started, *interval)
	if err != nil {
		log.Fatal(err)
	}
	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	expected := float64(*targets) * duration.Seconds() / interval.Seconds()
	final := finalLine(filepath.Join(dir, "gotell.log"))

	fmt.Printf("\nresults (%v wall)\n", wall.Round(time.Second))
	fmt.Printf("  runs saved           %d (about %.0f expected; %d requests reached the fake site)\n", res.runs, expected, served.Load())
	fmt.Printf("  outcomes             %v\n", res.outcomes)
	fmt.Printf("  scheduler lag, all   p50 %s  p95 %s  p99 %s  max %s\n", ms(res.all, 50), ms(res.all, 95), ms(res.all, 99), ms(res.all, 100))
	fmt.Printf("  lag after 1st round  p50 %s  p95 %s  p99 %s  max %s\n", ms(res.steady, 50), ms(res.steady, 95), ms(res.steady, 99), ms(res.steady, 100))
	fmt.Printf("  CPU time             %v (%.1f%% of one core on average)\n", cpu.Round(10*time.Millisecond), 100*cpu.Seconds()/wall.Seconds())
	fmt.Printf("  peak memory          heap %.1f MiB, from the OS %.1f MiB\n", mib(peak.heap), mib(peak.sys))
	fmt.Printf("  peak goroutines      %d\n", peak.goroutines)
	fmt.Printf("  peak workers busy    %d of %d; job queue peak %d of %d; queue-full skips %d\n",
		peak.busy, last.Workers.Workers, peak.queue, last.Workers.QueueCapacity, last.Skipped)
	fmt.Printf("  telemetry            peak depth %d of %d, longest wait %.0f ms, dropped %d\n",
		peak.telDepth, last.TelCapacity, last.TelMaxWait, last.TelDropped)
	fmt.Printf("  database             %.1f MiB (+ WAL %.1f MiB)\n", fileMiB(dbPath), fileMiB(dbPath+"-wal"))
	fmt.Printf("  Gotell's own summary %s\n", final)
}

// stats is the part of GET /api/stats the load test reads.
type stats struct {
	Goroutines int    `json:"goroutines"`
	Heap       uint64 `json:"heap_alloc_bytes"`
	Sys        uint64 `json:"sys_bytes"`
	Workers    struct {
		Workers       int `json:"workers"`
		Busy          int `json:"busy"`
		QueueDepth    int `json:"queue_depth"`
		QueueCapacity int `json:"queue_capacity"`
	} `json:"workers"`
	Skipped     int64   `json:"scheduler_skipped"`
	TelDepth    int     `json:"telemetry_depth"`
	TelCapacity int     `json:"telemetry_capacity"`
	TelDropped  int64   `json:"telemetry_dropped"`
	TelMaxWait  float64 `json:"telemetry_max_wait_ms"`
}

func sample(addr string) (stats, error) {
	var st stats
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://" + addr + "/api/stats")
	if err != nil {
		return st, err
	}
	defer res.Body.Close()
	return st, json.NewDecoder(res.Body).Decode(&st)
}

type peaks struct {
	heap, sys                         uint64
	goroutines, busy, queue, telDepth int
}

func (p *peaks) update(s stats) {
	p.heap = max(p.heap, s.Heap)
	p.sys = max(p.sys, s.Sys)
	p.goroutines = max(p.goroutines, s.Goroutines)
	p.busy = max(p.busy, s.Workers.Busy)
	p.queue = max(p.queue, s.Workers.QueueDepth)
	p.telDepth = max(p.telDepth, s.TelDepth)
}

type result struct {
	runs        int
	outcomes    map[string]int
	all, steady []float64 // scheduler lag in ms, sorted
}

// analyse reads the saved runs. "steady" leaves out the first round, when
// every target starts within the first seconds.
func analyse(dbPath string, started time.Time, interval time.Duration) (result, error) {
	r := result{outcomes: map[string]int{}}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return r, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT outcome, started_at, scheduler_delay_ms FROM probe_runs`)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	steadyFrom := started.Add(interval)
	for rows.Next() {
		var outcome, at string
		var lag sql.NullFloat64
		if err := rows.Scan(&outcome, &at, &lag); err != nil {
			return r, err
		}
		r.runs++
		r.outcomes[outcome]++
		if !lag.Valid {
			continue
		}
		r.all = append(r.all, lag.Float64)
		if t, err := time.Parse("2006-01-02T15:04:05.000Z", at); err == nil && t.After(steadyFrom) {
			r.steady = append(r.steady, lag.Float64)
		}
	}
	slices.Sort(r.all)
	slices.Sort(r.steady)
	return r, rows.Err()
}

// ms returns the p-th percentile (nearest rank) of sorted values.
func ms(sorted []float64, p int) string {
	if len(sorted) == 0 {
		return "-"
	}
	i := (p*len(sorted)+99)/100 - 1
	i = min(max(i, 0), len(sorted)-1)
	v := sorted[i]
	if v >= 1000 {
		return fmt.Sprintf("%.1fs", v/1000)
	}
	return fmt.Sprintf("%.0fms", v)
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

func fileMiB(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return mib(uint64(fi.Size()))
}

// finalLine returns Gotell's "stopped. telemetry: ..." summary line.
func finalLine(logPath string) string {
	b, err := os.ReadFile(logPath)
	if err != nil {
		return "(no log)"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "stopped.") {
			return strings.TrimSpace(line)
		}
	}
	tail := string(b)
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	return "(no summary; end of log: " + tail + ")"
}
