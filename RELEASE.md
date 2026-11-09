# Gotell v1 release checklist

The blueprint's release-readiness checklist (§19), checked against the code. ✅ done, ⚠️ done with a stated limit.

| # | Item | Status | Evidence |
|---|---|---|---|
| 1 | All probe timeouts and response limits are explicit | ✅ | `probe.DefaultConfig`: connect 5 s, TLS 5 s, response headers 10 s; 30 s per check incl. retries (`main.go` `probeTimeout`); body read capped at 1 MiB; API body 4 KiB, headers 16 KiB, server read/write/idle timeouts. |
| 2 | Connection reuse is recorded and measurement modes are separated | ✅ | `probe.Result.ConnReused`; stages that did not happen on a reused connection are `nil`/`NULL`, never 0 (trace tests). |
| 3 | Retry attempts are persisted independently from final run outcomes | ✅ | Tables `probe_runs` and `probe_attempts`; outcome `recovered` vs `success`. |
| 4 | Worker count, queue sizes and request rates are bounded | ✅ | `-workers` (1–256, default 16), job queue 4 × workers, telemetry buffer 64, intervals 10 s–24 h, at most 200 targets and 20 per website by default. |
| 5 | Backpressure and dropped-record behavior are observable | ✅ | Telemetry stats (depth, waits, dropped), scheduler `queue-full skips`, writer `lost`; all in `GET /api/stats` and the shutdown summary. Load tests: 0 dropped, 0 lost even when overloaded. |
| 6 | Database errors are checked; failed batches are not silently discarded | ✅ | `store.Writer`: transient errors retried with backoff, bad records quarantined alone, fatal errors stop Gotell; anything unsaved is counted and logged (writer tests). |
| 7 | Shutdown drains and flushes the telemetry pipeline | ✅ | Ctrl+C / `SIGTERM` (`docker stop`): scheduler drains, channels close in order, writer does a final flush (shutdown tests). |
| 8 | Hourly aggregation is idempotent and handles missing data | ✅ | Upsert per (target, hour); recompute replaces; empty hours are `NULL`, not 100%; missed hours caught up at start-up (aggregator tests). |
| 9 | Uptime denominator and maintenance exclusions are documented | ✅ | README, "How uptime is calculated". |
| 10 | SSRF protections include redirect and DNS-rebinding considerations | ✅ | Internal addresses refused when a target is added and again on the real IP at connect time (`netguard.Control`), which covers DNS rebinding. Redirects are not followed at all. `-allow-private` is an explicit opt-out. |
| 11 | Authentication, authorization, per-user limits, secret-safe logging | ⚠️ | One admin token for all changes, compared in constant time; reads need none. Limits are per instance (Gotell has one user). The token is printed once at start-up so the owner can copy it; it is never logged per request. The dashboard listens on localhost only. |
| 12 | Load, soak, failure-injection and recovery tests have been run | ⚠️ | Load tests: 100 and 1,000 targets ([BENCHMARKS.md](BENCHMARKS.md)). Failure injection and recovery: unit tests (writer errors, quarantine, shutdown, incident reload after restart). Not yet: multi-day soak, 10,000 targets, failure injection under load. |
| 13 | Performance claims are backed by reproducible benchmark results | ✅ | [BENCHMARKS.md](BENCHMARKS.md) and `go run ./loadtest`. The README claims only what was measured. |

Before tagging a release, run:

```bash
cd gotel
gofmt -l .
go vet ./...
go test -race ./...
docker compose up -d --build
docker compose ps        # STATUS shows (healthy)
```
