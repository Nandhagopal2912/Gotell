# Gotell

A lightweight, self-hosted Go engine for black-box HTTP/HTTPS availability and latency monitoring, built for independent developers, small applications and hobby-hosted services.

Gotell checks your endpoints **from the outside**, the way a user would. It records whether they answered, how long each stage took (DNS, TCP connect, TLS handshake, time to first byte, total), keeps every retry, opens incidents when a target is really down, and shows an honest history on a local dashboard.

> **Status:** v1. All 10 phases of the design are complete. Gotell runs as **one process and one SQLite file on your own machine** (or in Docker). Capacity numbers below come from repeatable load tests, see [BENCHMARKS.md](BENCHMARKS.md); anything not measured there is not claimed.

<!-- Add a screenshot: save it as assets/dashboard.png and uncomment the next line. -->
<!-- ![Gotell dashboard](assets/dashboard.png) -->

---

## Who it's for

Indie developers and small teams running apps on **free, hobby or serverless hosting** (Render, Railway, Fly.io and similar), where the common question is:

> *Is my app down, slow, or just waking up from a cold start?*

Most uptime tools answer only "up" or "down". Gotell shows **what happened on every attempt** and **where the time went**.

## Why Gotell

A URL pinger is easy to write. A monitor whose numbers you can trust is not. Gotell focuses on the details that make monitoring data honest:

- **Honest uptime.** Missing checks are never counted as success. Time when Gotell was off, or a target was paused, is shown separately from the target's own availability.
- **Where the time went.** Each check is split into DNS, TCP connect, TLS handshake, time to first byte and total.
- **"Not measured" is never 0 ms.** Stages that did not happen (a reused connection, an IP address with no DNS) are stored as missing.
- **Every attempt kept.** "Failed once, recovered on retry" is recorded differently from "healthy the whole time", so flapping and slow wake-ups stay visible.
- **No silent data loss.** If a result cannot be saved, it is retried, quarantined or counted as lost, and reported.
- **Safe by default.** User-supplied URLs cannot reach your private network (SSRF protection), changes need an admin token, and every queue and limit is bounded.

---

## Quick start

### With Docker (recommended)

Requires Docker with Compose.

```bash
git clone https://github.com/Nandhagopal2912/Gotell.git
cd Gotell/gotel
docker compose up -d --build
docker compose logs
```

Open **http://127.0.0.1:8080**. The logs show the **admin token**: paste it into the dashboard once to add, pause or delete targets (the browser remembers it). Reading the dashboard needs no token.

- Data (targets, history, the token) lives in the Docker volume **`gotell-data`**, so it survives rebuilds and `docker compose down`. `docker compose down -v` **deletes it**.
- The port is published on `127.0.0.1` only, so other machines on your network cannot reach the dashboard.
- To choose your own token: `GOTELL_TOKEN=yoursecret docker compose up -d` (PowerShell: `$env:GOTELL_TOKEN="yoursecret"; docker compose up -d`).
- The image is about 14 MB: a static Go binary on distroless, running as a non-root user, with a built-in health check (`docker compose ps` shows `healthy`).

### With Go

Requires **Go 1.27+**.

```bash
cd Gotell/gotel
go run .
```

Then open http://127.0.0.1:8080 and add a target. Stop with Ctrl+C; Gotell shuts down cleanly and prints a summary.

### Options

| Flag | Default | Meaning |
|---|---|---|
| `-db` | `gotell.db` | SQLite database file |
| `-listen` | `127.0.0.1:8080` | dashboard and API address (localhost only by default) |
| `-token` | `$GOTELL_TOKEN`, else generated and saved in the database | admin token for changes |
| `-workers` | `16` | probes that may run at the same time (see [BENCHMARKS.md](BENCHMARKS.md) to size it) |
| `-max-targets` | `200` | most targets Gotell will watch |
| `-max-targets-per-host` | `20` | most targets on one website, so one site is not overloaded |
| `-allow-private` | off | allow targets on private/internal addresses (localhost, 192.168.x.x, ...) |
| `-duration` | `0` (forever) | stop after this long |

### Backup

The whole state is one SQLite file. With Docker, copy it out of the volume while Gotell is stopped:

```bash
docker compose stop
docker run --rm -v gotell-data:/data -v "${PWD}:/backup" busybox cp /data/gotell.db /backup/gotell-backup.db
docker compose start
```

Without Docker, stop Gotell and copy `gotell.db` (if `gotell.db-wal` exists, copy it too).

---

## What Gotell does

**Probing** (Phases 1–2)
- One shared HTTP client with separate timeouts: connect 5 s, TLS handshake 5 s, response headers 10 s, and a 30 s deadline for a whole check including retries.
- Response bodies are read up to 1 MiB and always closed. Redirects are **not followed**; the 3xx is the result.
- `httptrace` timings for DNS, TCP connect, TLS, time to first byte, total, and whether the connection was reused.

**Scheduling, retries and classification** (Phases 3–4)
- One scheduler goroutine and a fixed pool of workers with a bounded job queue. A target never has two checks running at once. Random jitter spreads checks out; scheduler lag is measured.
- Each check is a **run** of one or more **attempts**. Failures are classified: `dns`, `connect`, `tls`, `timeout`, `http_4xx`, `http_5xx`, `http_other`, `body_read`, `blocked`, `cancelled`, `unknown`.
- Retries with doubling waits for `dns`, `connect`, `timeout`, `http_5xx` and `body_read` only. A retry is never started if it could not finish before the deadline.
- Outcomes: `success`, `recovered` (failed, then healthy), `failed`, `cancelled`.

**Telemetry and storage** (Phases 5–6)
- A bounded channel between probing and storage. When it is full, senders wait (backpressure); nothing is dropped silently.
- A single writer saves runs and attempts to SQLite (WAL mode, busy timeout, foreign keys) in batches. Temporary errors are retried, a bad record is quarantined alone, fatal errors stop Gotell cleanly; anything not saved is counted and logged.

**Analytics and retention** (Phase 7)
- Hourly summaries per target (UTC hours): run counts, uptime, time-to-first-byte p50/p95/p99 and peak.
- Recomputing an hour replaces it (idempotent), and hours missed while Gotell was off are caught up at start-up.
- Raw runs are kept for 14 days; hourly summaries are kept.

**Incidents and notifications** (Phase 8)
- A target is **DOWN** after 3 failed runs in a row and **RECOVERED** at the first healthy run. Cancelled runs are ignored. Open incidents are reloaded after a restart, so nothing alerts twice.
- Quiet hours 23:00–07:00 (local time) hold notifications, unless a target stays down for 30 minutes. Held notifications are recorded and summarised in the morning, never dropped.
- Notifications are printed to the console and recorded in the database.

**API and dashboard** (Phase 9)
- A JSON API and an embedded dashboard (no external scripts; strict Content-Security-Policy).
- The dashboard shows each target's status, uptime, latency by stage, hour-by-hour bars, recent runs with every attempt, incidents, and plain-language explanations.
- Add, pause, resume and delete targets from the dashboard (admin token required).

**Hardening** (Phase 10)
- **SSRF protection:** targets on loopback, private, link-local, cloud-metadata and other internal addresses are refused when added **and** checked again on the real IP at connect time, which also covers DNS rebinding. Opt out with `-allow-private` for your own LAN.
- **Limits:** 200 targets in total and 20 per website by default; check intervals from 10 s to 24 h; request size and header limits; server read/write/idle timeouts.
- **Container:** multi-stage Docker build, distroless non-root image, health check, graceful stop on `SIGTERM`, Compose file with a named volume.
- **Self-monitoring:** `GET /api/stats` reports workers busy, queue depths, queue-full skips, memory, goroutines and data-loss counters.
- **Load tests:** a repeatable harness (`go run ./loadtest`) and the results in [BENCHMARKS.md](BENCHMARKS.md). On a laptop, 16 workers kept up with 1,000 targets checked every 60 s (fake site, ~0.35 s per check) using about 1% of one CPU core and 23 MiB of memory, with no data lost. With 4 workers the same load fell behind, but still lost nothing, so the default is now 16.

### How uptime is calculated

For each target and UTC hour:

```
uptime % = (success + recovered runs) / (success + recovered + failed runs) × 100
```

- **Cancelled** runs (stopped by Gotell's own shutdown) count as neither up nor down.
- An hour with no up/down runs has **no** uptime (shown as `n/a`), never 100% or 0%.
- A run belongs to the hour it was **scheduled** in.
- **Maintenance:** while a target is paused, no checks run and that time is excluded. Time when Gotell itself was not running is shown separately as a coverage gap, never as downtime or uptime.

### API

| Method and path | Token | Purpose |
|---|---|---|
| `GET /api/health` | | Gotell is running |
| `GET /api/stats` | | Gotell's own load and data-loss counters |
| `GET /api/targets` | | all targets with status and recent hours |
| `GET /api/targets/{id}` | | one target with its hourly history |
| `GET /api/targets/{id}/runs` | | recent runs with every attempt |
| `GET /api/incidents` | | recent incidents |
| `POST /api/targets` | ✓ | add `{"url": "https://...", "interval_seconds": 60}` |
| `DELETE /api/targets/{id}` | ✓ | delete a target (its history is kept) |
| `POST /api/targets/{id}/pause` | ✓ | maintenance mode on |
| `POST /api/targets/{id}/resume` | ✓ | maintenance mode off |

Changes need the header `Authorization: Bearer <token>`.

---

## Architecture

```
Dashboard / JSON API ──(token)──> target store (SQLite) ──> SSRF + limit checks
                                        │
                                        v
Central scheduler ──> bounded job queue ──> fixed worker pool ──> HTTP probe (httptrace)
                                                  │                 SSRF check at connect time
                                                  v
                                   retries + failure classification
                                                  │
                                                  v
                             bounded telemetry channel (backpressure)
                                                  │
                                  incident detector ──> notification dispatcher (quiet hours)
                                                  │
                                                  v
                           single SQLite batch writer (WAL) ──> runs + attempts
                                                                    │
                                                    hourly aggregator + 14-day retention
```

Everything runs in one process. One goroutine owns each piece of shared state (scheduler, writer), and every queue between them is bounded.

## Project layout

```
.
├── README.md
├── BENCHMARKS.md        # load-test results
├── RELEASE.md           # release checklist (blueprint §19)
└── gotel/
    ├── main.go          # wires the engine, API and dashboard together
    ├── Dockerfile, compose.yml, .dockerignore
    ├── probe/           # one HTTP attempt: timeouts, tracing, status policy
    ├── runner/          # one check: attempts, retries, classification
    ├── worker/          # bounded worker pool
    ├── scheduler/       # decides which targets are due; pause/resume/add/remove
    ├── telemetry/       # bounded channel between probing and storage
    ├── store/           # SQLite: schema, writer, analytics, retention, incidents, targets
    ├── incident/        # incident detection and notification dispatch
    ├── netguard/        # which addresses are internal (SSRF)
    ├── api/             # JSON API, token check, validation, limits
    ├── web/             # embedded dashboard (HTML, CSS, JS)
    └── loadtest/        # load-test harness
```

## Development

```bash
go test -race ./...
gofmt -l .
go vet ./...
```

The race detector needs cgo and a 64-bit C compiler. On Windows, one way is MSYS2's UCRT GCC: install `mingw-w64-ucrt-x86_64-gcc`, add `C:\msys64\ucrt64\bin` to your `PATH`, and run `go env -w CC=C:\msys64\ucrt64\bin\gcc.exe`.

Load test (see [BENCHMARKS.md](BENCHMARKS.md)):

```bash
go build -o gotell.exe .
go run ./loadtest -gotell ./gotell.exe -targets 100 -duration 10m
```

## Not in v1

Kept out on purpose, so v1 stays small and honest:

- Multi-region or distributed probing; Gotell checks from one machine.
- Following redirects (needs per-hop destination checks first).
- Automatic "suspected cold start" labels. Slow first attempts and timeout-then-recovered runs are recorded with all their timings, but not yet labelled.
- Email, Slack or webhook notifications (notifications are printed and recorded).
- Multiple users; there is one admin token.
- Hosting Gotell publicly. It is designed to run on your own machine; keep the dashboard on localhost.

## About this project

Gotell is a learning project, built step by step in Go from a written architecture blueprint (v4.1) and its 10-phase roadmap, with tests at every phase.
