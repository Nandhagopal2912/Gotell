# Gotell benchmarks

Gotell makes no capacity claim that is not in this file (blueprint §16.2: *"Do not claim that the system supports a target count solely because it can create that many target records"*).

## How the test works

`gotel/loadtest` is a repeatable harness. It never touches the internet:

1. It starts a **fake website on 127.0.0.1**. Every page waits a random **100–600 ms** (about 350 ms on average), and **1%** of answers are `503`, so retries happen too.
2. It creates a **fresh temporary database** with N targets on that site, all checked **every 60 s**.
3. It runs the **real Gotell binary** against it (`-allow-private`, since the fake site is on localhost) and samples `GET /api/stats` every 5 s.
4. When Gotell stops, it reads every saved run back from the database: count, outcomes and **scheduler lag** (how late each check started compared with when it was due).

```bash
cd gotel
go build -o gotell.exe .
go run ./loadtest -gotell ./gotell.exe -targets 1000 -workers 16 -duration 10m
```

**Machine:** AMD Ryzen 7 5800HS (8 cores / 16 threads), 15.4 GB RAM, Windows 11, Go 1.27.0, run natively (not in Docker). Date: 2026-11-09.

## Results (10 minutes each, 60 s interval)

| | A: 100 targets, 4 workers | B: 1,000 targets, 4 workers | C: 1,000 targets, 16 workers |
|---|---|---|---|
| Runs saved / expected | **1,000 / 1,000** | **6,735 / 10,000 (67%)** | **10,000 / 10,000** |
| Outcomes | 996 success, 4 recovered | 6,674 success, 57 recovered, 4 cancelled | 9,899 success, 101 recovered |
| Lag p50 / p95 / p99 / max, after the first round | 0.9 s / 1.8 s / 2.2 s / 3.0 s | **21 s / 93 s / 132 s / 229 s** | 1.0 s / 1.7 s / 2.0 s / 3.5 s |
| Lag p95 / max, all runs (incl. start-up) | 4.1 s / 9.2 s | 90 s / 229 s | 10.9 s / 22.9 s |
| Workers busy (peak) | 4 of 4 | 4 of 4 | 16 of 16 |
| Job queue (peak) | 7 of 16 | 9 of 16 | 23 of 64 |
| Queue-full skips | 302 | 204,389 | 9,742 |
| Telemetry buffer (peak) / dropped | 0 of 64 / **0** | 0 of 64 / **0** | 0 of 64 / **0** |
| Writer: written / errors / lost | 1,000 / 0 / **0** | 6,735 / 0 / **0** | 10,000 / 0 / **0** |
| CPU (average) | 0.2% of one core | 1.1% of one core | 1.3% of one core |
| Memory: Go heap / from the OS (peak) | 3.0 MiB / 16.7 MiB | 3.2 MiB / 21.0 MiB | 3.1 MiB / 22.8 MiB |
| Goroutines (peak) | 25 | 25 | 63 |
| Database after 10 min | 0.3 MiB | 1.8 MiB | 2.7 MiB |

## What the numbers say

**1. The limit is worker time, not CPU, memory or the database.** In B, 4 workers saved 11.2 runs per second (6,735 runs in 600 s), so one check took about 0.36 s of a worker's time, matching the fake site's latency. 1,000 targets every 60 s need 16.7 runs per second, so the workers fell further behind every minute. CPU stayed near 1%, memory near 21 MiB, and the database writer was never the bottleneck (telemetry buffer always empty).

A useful rule of thumb from this:

```
targets one Gotell can keep up with ≈ workers × interval ÷ average check time
```

With 4 workers, 60 s and 0.36 s: about 670 targets. Real websites are slower than the fake one (0.5–2 s per check was typical in earlier runs, and a timeout can hold a worker for up to 30 s), so leave a wide margin.

**2. Overload is visible and loses nothing.** When B fell behind, Gotell did not drop or invent results. Checks simply started late, `queue-full skips` and lag grew (both visible in `/api/stats` and on every saved run), and every check that ran was saved. That is the reliability-first behavior the blueprint asks for (§8).

**3. 16 workers keep up with 1,000 targets.** C did every expected check, with lag after the first round the same as with 100 targets. The cost of 12 more workers was about 40 goroutines and 2 MiB.

**4. Lag of about 1 s is normal.** The scheduler looks for due targets once per second, so a check starts 0–1 s after it is due, plus up to 2 s of jitter that spreads checks out. Lag above a few seconds means the workers cannot keep up.

**5. Start-up is a burst.** All targets are first checked within the first 2 seconds after start-up, so the first round queues up (C: p95 10.9 s, max 22.9 s, then back to normal). It settles by itself after one interval.

**6. Queue-full skips count retries, not lost checks.** A due check that finds the queue full is tried again on the next 1-second tick, and each try is counted. The number shows pressure; it is not lost data.

## Decision

The default **`-workers` is now 16** (was 4). Run B showed 4 is too few for the 1,000-target planning baseline, and run C showed 16 keeps up for a negligible cost. The default **`-max-targets` stays 200**: it leaves room for real sites that are much slower than the fake one. A confirmation run with the new default (100 targets, 16 workers, 3 minutes) saved 300 of 300 runs, with lag p95 1.5 s and max 1.9 s even including start-up, no queue-full skips, at most 5 workers busy, 21 MiB memory and 0 lost.

To watch more, raise both, using the rule above, and check `GET /api/stats` (`workers.busy` close to `workers.workers` all the time, or growing `scheduler_skipped`, means add workers).

## Storage estimate (extrapolated, not measured over days)

Run C stored about **280 bytes per run** (with its attempts and indexes). Raw runs are kept for 14 days:

| Targets every 60 s | Runs per day | Per day | At 14-day retention |
|---|---|---|---|
| 100 | 144,000 | ~39 MiB | ~0.5 GiB |
| 1,000 | 1,440,000 | ~390 MiB | ~5.4 GiB |

## Not tested yet

To be honest about the gaps, from the blueprint's list (§16.2):

- **10,000 targets.**
- **Long soak** (days): memory growth, WAL growth, retention running for real. The longest run here is 10 minutes.
- **HTTPS under load.** The fake site is plain HTTP, so TLS handshake cost is not included.
- **Failure injection under load:** slow or unavailable database, network instability, killing Gotell during writes. These paths are covered by unit tests (writer retries, quarantine, fatal errors, shutdown drain), not by load tests.
- **Docker.** These runs were native; the container should behave the same but was not measured.
