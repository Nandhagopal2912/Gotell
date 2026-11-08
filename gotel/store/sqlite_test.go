package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gotel/probe"
	"gotel/runner"
	"gotel/scheduler"
)

// openTemp opens a fresh database in a temporary folder that the test
// framework deletes afterwards.
func openTemp(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.UpsertTargets(context.Background(), []scheduler.Target{
		{ID: 1, URL: "http://a.test/", Interval: time.Minute},
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func dur(d time.Duration) *time.Duration { return &d }

// sampleRun is a recovered run: a timeout, then a healthy attempt on a
// reused connection (so its DNS/TCP/TLS were not measured).
func sampleRun(targetID int) runner.Run {
	start := time.Date(2026, 11, 7, 22, 58, 18, 0, time.UTC)
	return runner.Run{
		TargetID:       targetID,
		URL:            "http://a.test/",
		ScheduledAt:    start.Add(-50 * time.Millisecond),
		StartedAt:      start,
		CompletedAt:    start.Add(14 * time.Second),
		SchedulerDelay: 50 * time.Millisecond,
		Outcome:        runner.OutcomeRecovered,
		Attempts: []runner.Attempt{
			{Number: 1, StartedAt: start, Class: runner.ClassTimeout, Result: probe.Result{
				Total: 10 * time.Second, DNS: dur(40 * time.Millisecond),
				Err: errors.New("context deadline exceeded"),
			}},
			{Number: 2, StartedAt: start.Add(10 * time.Second), Class: runner.ClassNone, Result: probe.Result{
				StatusCode: 200, Total: 3 * time.Second, TTFB: dur(3 * time.Second),
				ConnReused: true, BodyBytes: 15,
			}},
		},
	}
}

func TestOpenEnablesWALAndForeignKeys(t *testing.T) {
	s := openTemp(t)

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q (err %v), want wal", mode, err)
	}
	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d (err %v), want 1", fk, err)
	}
}

func TestWriteBatchSavesRunsAndAttempts(t *testing.T) {
	s := openTemp(t)
	if err := s.WriteBatch(context.Background(), []runner.Run{sampleRun(1)}); err != nil {
		t.Fatal(err)
	}

	var outcome, startedAt string
	var attempts int
	err := s.db.QueryRow(`SELECT outcome, started_at, attempt_count FROM probe_runs`).
		Scan(&outcome, &startedAt, &attempts)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "recovered" || attempts != 2 || startedAt != "2026-11-07T22:58:18.000Z" {
		t.Errorf("run = %q, %d attempts, started %q", outcome, attempts, startedAt)
	}

	// Attempt 2 reused its connection: DNS was not measured, so it must
	// be NULL, never 0. Its status and TTFB were measured.
	var dns, ttfb sql.NullFloat64
	var status sql.NullInt64
	var class string
	err = s.db.QueryRow(`SELECT dns_ms, ttfb_ms, status_code, failure_class
		FROM probe_attempts WHERE attempt_number = 2`).Scan(&dns, &ttfb, &status, &class)
	if err != nil {
		t.Fatal(err)
	}
	if dns.Valid {
		t.Errorf("dns_ms = %v, want NULL (not measured)", dns.Float64)
	}
	if !ttfb.Valid || ttfb.Float64 != 3000 || status.Int64 != 200 || class != "none" {
		t.Errorf("ttfb_ms = %v, status = %v, class = %q", ttfb, status, class)
	}

	// Attempt 1 had no response: status NULL, error message kept.
	var errMsg sql.NullString
	err = s.db.QueryRow(`SELECT status_code, error_message FROM probe_attempts
		WHERE attempt_number = 1`).Scan(&status, &errMsg)
	if err != nil {
		t.Fatal(err)
	}
	if status.Valid || !errMsg.Valid {
		t.Errorf("attempt 1: status = %v (want NULL), error = %v (want kept)", status, errMsg)
	}
}

func TestWriteBatchIsAllOrNothing(t *testing.T) {
	s := openTemp(t)

	// The second run points at a target that does not exist, which breaks
	// the foreign key. The first, valid run must be rolled back too.
	err := s.WriteBatch(context.Background(), []runner.Run{sampleRun(1), sampleRun(99)})
	if err == nil {
		t.Fatal("expected a foreign key error, got nil")
	}
	if kind := ClassifyError(err); kind != BadRecord {
		t.Errorf("ClassifyError = %v, want %v (err: %v)", kind, BadRecord, err)
	}

	var runs int
	s.db.QueryRow(`SELECT COUNT(*) FROM probe_runs`).Scan(&runs)
	if runs != 0 {
		t.Errorf("%d runs saved, want 0: the transaction must be rolled back", runs)
	}
}

func TestUpsertTargetsDoesNotDuplicate(t *testing.T) {
	s := openTemp(t) // already saved target 1 once
	err := s.UpsertTargets(context.Background(), []scheduler.Target{
		{ID: 1, URL: "http://new.test/", Interval: 30 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}

	var count int
	var url string
	s.db.QueryRow(`SELECT COUNT(*), url FROM targets`).Scan(&count, &url)
	if count != 1 || url != "http://new.test/" {
		t.Errorf("targets: %d rows, url %q; want 1 row with the new url", count, url)
	}
}
