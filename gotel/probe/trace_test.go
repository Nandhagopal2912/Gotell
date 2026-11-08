package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeDNSNotMeasuredForIPLiteral(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// srv.URL is like http://127.0.0.1:54321, so there is no name to look up.
	r := New(testConfig()).Probe(ctx, srv.URL)

	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.DNS != nil {
		t.Errorf("DNS = %v, want nil (no lookup for an IP address)", *r.DNS)
	}
}

func TestProbeDNSMeasuredForHostname(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Same server, but reached by name, so a DNS lookup happens.
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	r := New(testConfig()).Probe(ctx, url)

	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.DNS == nil {
		t.Fatal("DNS = nil, want a measured lookup")
	}
}

func TestProbeConnectionReuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p := New(testConfig())
	first := p.Probe(ctx, srv.URL)
	second := p.Probe(ctx, srv.URL)

	if first.Err != nil || second.Err != nil {
		t.Fatalf("unexpected errors: %v, %v", first.Err, second.Err)
	}
	if first.ConnReused {
		t.Error("first probe: ConnReused = true, want false (pool was empty)")
	}
	if !second.ConnReused {
		t.Error("second probe: ConnReused = false, want true (same Prober, same server)")
	}
}

func TestProbeTCPMeasuredTLSNotForHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(testConfig()).Probe(ctx, srv.URL)

	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.TCPConnect == nil {
		t.Error("TCPConnect = nil, want measured (new connection)")
	}
	if r.TLSHandshake != nil {
		t.Errorf("TLSHandshake = %v, want nil (plain http://)", *r.TLSHandshake)
	}
}

func TestProbeTLSMeasuredEvenWhenCertRejected(t *testing.T) {
	// NewTLSServer uses a self-signed certificate that our Prober does not
	// trust, so the handshake runs but verification fails.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(testConfig()).Probe(ctx, srv.URL)

	if r.Err == nil {
		t.Fatal("expected a certificate error, got nil")
	}
	if r.TCPConnect == nil {
		t.Error("TCPConnect = nil, want measured")
	}
	if r.TLSHandshake == nil {
		t.Error("TLSHandshake = nil, want measured (handshake ran, then failed)")
	}
	if r.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 (no HTTP response)", r.StatusCode)
	}
}

func TestProbeReusedConnectionSkipsSetupTimings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p := New(testConfig())
	p.Probe(ctx, srv.URL) // opens the connection
	r := p.Probe(ctx, srv.URL)

	if !r.ConnReused {
		t.Fatal("ConnReused = false, want true")
	}
	if r.TCPConnect != nil {
		t.Errorf("TCPConnect = %v, want nil (no new connection)", *r.TCPConnect)
	}
}

func TestProbeTTFBMeasured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(testConfig()).Probe(ctx, srv.URL)

	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.TTFB == nil {
		t.Fatal("TTFB = nil, want measured")
	}
	if *r.TTFB > r.Total {
		t.Errorf("TTFB = %v is after Total = %v", *r.TTFB, r.Total)
	}
}

func TestProbeTTFBNotMeasuredWithoutResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening: no response byte can ever arrive

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	r := New(testConfig()).Probe(ctx, url)

	if r.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	if r.TTFB != nil {
		t.Errorf("TTFB = %v, want nil (no response)", *r.TTFB)
	}
}
