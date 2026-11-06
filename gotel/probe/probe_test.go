package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Phase 1 exit criteria (blueprint §17): deterministic tests for
// success, HTTP error, timeout, and DNS/connect failure.

func TestProbeSuccess(t *testing.T) {
	// A fake server that always returns 200 with a 5-byte body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(DefaultConfig()).Probe(ctx, srv.URL)

	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", r.StatusCode, http.StatusOK)
	}
	if r.BodyBytes != 5 {
		t.Errorf("BodyBytes = %d, want 5", r.BodyBytes)
	}
}

func TestProbeHTTPError(t *testing.T) {
	// The request itself succeeds; the server just answers 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(DefaultConfig()).Probe(ctx, srv.URL)

	// A 500 is a completed HTTP exchange, not a Go error.
	// Deciding that 500 is "unhealthy" is the status policy (exercise 1.4).
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", r.StatusCode, http.StatusInternalServerError)
	}
}

func TestProbeTimeout(t *testing.T) {
	// release lets the handler stop sleeping once the test is over,
	// so srv.Close() doesn't have to wait for it.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)

	const timeout = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	r := New(DefaultConfig()).Probe(ctx, srv.URL)

	if r.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	// Works only because Probe wraps errors with %w.
	if !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want it to wrap context.DeadlineExceeded", r.Err)
	}
	if r.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 (no response arrived)", r.StatusCode)
	}
	// A failed probe must still record how long it took to fail.
	// The context's clock starts a moment before Probe's, and the Windows
	// clock is coarse, so allow some slack below the exact timeout.
	if r.Total < timeout/2 {
		t.Errorf("Total = %v, want close to %v", r.Total, timeout)
	}
}

func TestProbeConnectFailure(t *testing.T) {
	// Start a server only to get a free local address, then shut it down.
	// Nothing is listening there any more, so the connection is refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(DefaultConfig()).Probe(ctx, url)

	if r.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	if r.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 (no response arrived)", r.StatusCode)
	}
}

func TestProbeResponseHeaderTimeout(t *testing.T) {
	// Server accepts the request but never sends headers (until released).
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)

	// Short header timeout, generous overall deadline.
	cfg := DefaultConfig()
	cfg.ResponseHeaderTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(cfg).Probe(ctx, srv.URL)

	if r.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	// The context still has time left, so the transport timeout must have fired.
	if ctx.Err() != nil {
		t.Errorf("context expired (%v); want the transport timeout to fire first", ctx.Err())
	}
	if r.Total >= time.Second {
		t.Errorf("Total = %v, want well under the 2s context deadline", r.Total)
	}
}

func TestProbeDoesNotFollowRedirect(t *testing.T) {
	// followed records whether the redirect target was ever requested.
	followed := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		followed = true
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(DefaultConfig()).Probe(ctx, srv.URL)

	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.StatusCode != http.StatusFound {
		t.Errorf("StatusCode = %d, want %d", r.StatusCode, http.StatusFound)
	}
	if followed {
		t.Error("redirect was followed; want it returned as the result")
	}
}
