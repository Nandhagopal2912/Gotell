# Gotell

A lightweight Go engine for black-box HTTP/HTTPS availability and latency monitoring, built for independent developers, small applications and self-hosted services.

Gotell probes your endpoints **from the outside**, the way a user would. It records whether they answered, how long each stage took (DNS, TCP connect, TLS handshake, time to first byte, total), and keeps an honest history of availability and latency.

> **Status:** early development. Phases 1–4 of 10 are complete: Gotell can probe, trace, schedule, retry and classify, but does not store results yet. It is not production-ready, and no capacity or performance claims are made until they are backed by benchmarks.

---

## Who it's for

Indie developers and small teams running apps on **free, hobby or serverless hosting** (Render, Railway, Fly.io, serverless functions and similar), where the most common question is:

> *Is my app down, slow, or just waking up from a cold start?*

Most uptime tools answer only "up" or "down". Gotell aims to tell you **which one**, and **where the time went**.

## Why Gotell

A basic URL pinger is easy to write. A monitor whose numbers you can trust is not. Gotell focuses on the details that make monitoring data honest:

- **Honest uptime.** Missing measurements are never counted as success, and gaps in monitoring coverage are shown separately from the target's own availability. A "99.9%" from Gotell means 99.9% of expected checks actually succeeded.
- **Where the time went.** Each probe is split into DNS lookup, TCP connect, TLS handshake, time to first byte and total time, so a slow response points to a stage, not just a number.
- **Suspected cold starts.** Slow responses after idle periods are flagged as *suspected* cold starts, with the evidence kept, instead of being reported as outages or silently hidden.
- **Every attempt kept.** "Failed once, recovered on retry" is recorded differently from "healthy the whole time", so flapping services are visible.
- **"Not measured" is never 0 ms.** Stages that didn't happen (for example on a reused connection) are stored as missing, not as zero.
- **Simple to self-host.** One Go binary and one SQLite file. Your monitoring data stays with you.

---

## Goals

- Monitor user-configured **HTTP and HTTPS** endpoints from outside the target application.
- Measure availability and request timing: DNS, TCP connect, TLS handshake, TTFB and total duration, where observable.
- Probe many targets with **bounded, controlled concurrency** and configurable intervals.
- **Keep transient failures and every retry attempt**, not just the final outcome.
- Persist results safely with **SQLite (WAL mode)** and batched transactions.
- Produce hourly availability and latency summaries with **explicit missing-data semantics**.
- Support maintenance mode, adaptive scheduling, time-window-aware notifications and graceful shutdown.
- Protect the monitoring service itself from unsafe user-supplied destinations (**SSRF protection**).

## Non-goals (first release)

- A globally distributed, multi-region probe network.
- Full application performance monitoring (APM) or distributed tracing.
- Attributing slow responses to specific components inside the target server.
- Unlimited targets or unbounded request concurrency.
- Any claim of production-grade availability before load, failure-injection and recovery testing.

## Design principles

| Principle | What it means in Gotell |
|---|---|
| Facts before interpretation | The probe records what happened; a separate policy/classifier decides what it means. |
| Bounded resource use | Workers, queues, timeouts, response sizes and request rates all have limits. |
| No silent data loss | Database errors are surfaced; failed batches are retried, never quietly discarded. |
| Explicit measurement semantics | "Not measured" is never stored as 0 ms. Missing data is never counted as success. |
| Secure by default | User-supplied URLs are treated as untrusted network destinations. |
| Measure before optimizing | Concurrency and capacity numbers are hypotheses until benchmarked. |

## Architecture

`[built]` marks the parts that exist today; the rest is planned.

```
Target configuration
        |
        v
Target registry + URL / network policy validation
        |
        v
Central scheduler ---> bounded due-job queue                      [built]
        |
        v
Bounded worker pool ---> HTTP probe executor (net/http + httptrace) [built]
        |
        v
Retries + classification (failure types)                           [built]
(suspected cold starts: planned)
        |
        v
Bounded telemetry channel (backpressure, no silent drops)
        |
        v
Single SQLite batch writer (WAL)
        |
        +--> Raw probe runs and attempts
        +--> Hourly aggregates
                    |
                    v
        Incident state ---> Notification dispatcher
```

Gotell runs as a **single instance** first, designed so that distributed probe agents remain possible later.

## Roadmap

| Phase | Deliverable | Status |
|---|---|---|
| 1. Probe foundation | Single-target HTTP probe with bounded timeouts and a basic outcome | ✅ Done |
| 2. Trace instrumentation | DNS, TCP, TLS, TTFB, total time, connection reuse | ✅ Done |
| 3. Scheduler and worker pool | Central scheduler, bounded queue, fixed worker capacity | ✅ Done |
| 4. Retry and classification | Run/attempt model, configurable retry policy | ✅ Done |
| 5. Telemetry channel | Bounded channel with backpressure metrics | ⏳ Next |
| 6. SQLite writer | WAL setup and transactional batch writer | |
| 7. Analytics and retention | Idempotent hourly rollups, raw-data pruning | |
| 8. Incidents and notifications | Incident state, deduplication, timezone-aware windows | |
| 9. API and dashboard | Target management and historical views | |
| 10. Production hardening | Security controls, resource limits, load tests | |

### What works today

**Phase 1: probe foundation**
- `Prober` with one shared `http.Client`, so connections can be reused.
- Separate timeouts for DNS + TCP connect, TLS handshake and response headers, plus an overall deadline via `context`.
- Response bodies are always closed and reads are capped at 1 MiB.
- Redirects are **not followed** until redirect destinations can be validated; the 3xx response is returned as the result.
- A status policy (default: any `2xx` is healthy), kept separate from the probe itself.

**Phase 2: trace instrumentation**
- `httptrace` timings for DNS lookup, TCP connect, TLS handshake and time to first byte, plus total time and whether the connection was reused.
- Stages that did not happen (an IP-literal URL, a reused connection, plain `http://`) are `nil` ("not measured"), never 0 ms.
- DNS failures can be told apart from connection failures.

**Phase 3: scheduler and worker pool**
- A fixed number of worker goroutines and a bounded job queue; a full queue is refused and counted, not hidden.
- A single-goroutine scheduler: no overlapping runs of the same target, pause/resume (maintenance mode), random jitter, and scheduler-lag measurement.
- Graceful shutdown: running probes are cancelled, queued jobs are skipped (not recorded as failures), and the results channel closes once everything has stopped.
- Race-free: trace hooks are guarded by a mutex, and every test passes under `go test -race`.

**Phase 4: retries and classification**
- Each scheduled check is a **run** of one or more **attempts**; every attempt is kept.
- Failures are classified as `dns`, `connect`, `tls`, `timeout`, `http_4xx`, `http_5xx`, `http_other`, `body_read`, `cancelled` or `unknown`.
- Retries with doubling waits for `dns`, `connect`, `timeout`, `http_5xx` and `body_read`; no retries for `tls`, 4xx, cancellations or unknown errors. A retry is never started if it could not finish before the run's deadline.
- Run outcomes: `success`, `recovered` (failed, then healthy), `failed`, `cancelled`.

Example output of `go run .`, catching a real cold start on a free-tier host:

```
recovered https://startly-api.onrender.com/health  (2 attempt(s), 13.659s)
  #1 timeout    status=0 ttfb=- dns=39ms tcp=12ms tls=75ms total=10.001s
  #2 none       status=200 ttfb=3.156s dns=62ms tcp=10ms tls=16ms total=3.157s
```

Results are printed, not yet stored: persistence arrives with the telemetry channel (Phase 5) and the SQLite writer (Phase 6).

## Project layout

```
.
├── README.md
└── gotel/
    ├── go.mod
    ├── main.go              # small CLI: runs a few URLs and prints every attempt
    ├── probe/               # one HTTP attempt: timeouts, tracing, status policy
    │   ├── probe.go         # Prober, Config, Result
    │   ├── policy.go        # StatusPolicy, Default2xx
    │   └── *_test.go
    ├── runner/              # one scheduled check: attempts, retries, classification
    │   ├── types.go         # Run, Attempt, FailureClass, Outcome, RetryPolicy
    │   ├── classify.go      # Classify, Retryable
    │   ├── runner.go        # Do: the retry loop
    │   └── *_test.go
    ├── worker/              # bounded worker pool
    │   ├── pool.go          # Pool, Job, JobResult, Shutdown
    │   └── *_test.go
    └── scheduler/           # decides which targets are due
        ├── scheduler.go     # Scheduler, Target, pause/resume, jitter, lag
        └── *_test.go
```

## Getting started

Requires **Go 1.27+**.

```bash
git clone https://github.com/Nandhagopal2912/Gotell.git
cd Gotell/gotel
```

Run the demo CLI (it exits with code 1 if any run is neither `success` nor `recovered`):

```bash
go run .
```

Run the tests with Go's race detector (Gotell is concurrent, so always use `-race`):

```bash
go test -race ./...
```

The race detector needs cgo and a 64-bit C compiler. On Windows, one way is MSYS2's UCRT GCC: install `mingw-w64-ucrt-x86_64-gcc`, add `C:\msys64\ucrt64\bin` to your `PATH`, and point Go at it with `go env -w CC=C:\msys64\ucrt64\bin\gcc.exe`.

Before committing, check formatting and common mistakes:

```bash
gofmt -l .
go vet ./...
```

## Security note

A monitoring service that fetches user-supplied URLs can be abused to reach internal systems (SSRF). Full destination validation (blocking private, loopback, link-local and cloud-metadata addresses, revalidating redirects, and guarding against DNS rebinding) is a **release-blocking requirement** and is planned before Gotell is exposed publicly. Until then, run it only against endpoints you trust.

## About this project

Gotell is also a learning project: it is being built step by step in Go, following a written architecture blueprint (v4.1) and its phased roadmap, with tests required at each phase.
