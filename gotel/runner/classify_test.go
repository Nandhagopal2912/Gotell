package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gotel/netguard"
	"gotel/probe"
)

// TestClassifyTable checks the rules with hand-made results. Errors are
// wrapped the way Probe wraps them, so errors.Is/As must look through.
func TestClassifyTable(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("sending request: %w", err) }

	tests := []struct {
		name string
		r    probe.Result
		want FailureClass
	}{
		{"200 ok", probe.Result{StatusCode: 200}, ClassNone},
		{"204 ok", probe.Result{StatusCode: 204}, ClassNone},
		{"404", probe.Result{StatusCode: 404}, ClassHTTP4xx},
		{"503", probe.Result{StatusCode: 503}, ClassHTTP5xx},
		{"302 redirect", probe.Result{StatusCode: 302}, ClassHTTPOther},
		{"cancelled", probe.Result{Err: wrap(context.Canceled)}, ClassCancelled},
		{"deadline", probe.Result{Err: wrap(context.DeadlineExceeded)}, ClassTimeout},
		{"dns", probe.Result{Err: wrap(&net.DNSError{Name: "x.invalid", IsNotFound: true})}, ClassDNS},
		{"connect refused", probe.Result{Err: wrap(&net.OpError{Op: "dial", Err: errors.New("refused")})}, ClassConnect},
		{"unknown CA", probe.Result{Err: wrap(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}})}, ClassTLS},
		{"body read", probe.Result{StatusCode: 200, Err: fmt.Errorf("reading body: %w", errors.New("unexpected EOF"))}, ClassBodyRead},
		{"cancelled while reading body", probe.Result{StatusCode: 200, Err: fmt.Errorf("reading body: %w", context.Canceled)}, ClassCancelled},
		{"blocked by the SSRF guard", probe.Result{Err: wrap(&net.OpError{Op: "dial", Err: fmt.Errorf("%w (127.0.0.1)", netguard.ErrBlocked)})}, ClassBlocked},
		{"something else", probe.Result{Err: errors.New("weird")}, ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.r, probe.Default2xx); got != tt.want {
				t.Errorf("Classify = %q, want %q (err: %v)", got, tt.want, tt.r.Err)
			}
		})
	}
}

// TestClassifyRealProbes runs the real Probe against local servers, so
// the rules are checked against the error shapes Go actually produces.
func TestClassifyRealProbes(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer untrusted.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedURL := "http://" + ln.Addr().String() + "/"
	ln.Close()

	tests := []struct {
		name    string
		url     string
		timeout time.Duration
		want    FailureClass
	}{
		{"healthy", ok.URL, 2 * time.Second, ClassNone},
		{"404", notFound.URL, 2 * time.Second, ClassHTTP4xx},
		{"503", broken.URL, 2 * time.Second, ClassHTTP5xx},
		{"untrusted certificate", untrusted.URL, 2 * time.Second, ClassTLS},
		{"connection refused", refusedURL, 2 * time.Second, ClassConnect},
		{"dns failure", "http://gotell-test.invalid/", 5 * time.Second, ClassDNS},
		{"timeout", slow.URL, 50 * time.Millisecond, ClassTimeout},
	}
	cfg := probe.DefaultConfig()
	cfg.AllowPrivate = true // these tests use local servers
	p := probe.New(cfg)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()
			r := p.Probe(ctx, tt.url)
			if got := Classify(r, probe.Default2xx); got != tt.want {
				t.Errorf("Classify = %q, want %q (status %d, err: %v)", got, tt.want, r.StatusCode, r.Err)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		class FailureClass
		want  bool
	}{
		{ClassNone, false},
		{ClassDNS, true},
		{ClassConnect, true},
		{ClassTimeout, true},
		{ClassHTTP5xx, true},
		{ClassBodyRead, true},
		{ClassTLS, false},
		{ClassHTTP4xx, false},
		{ClassHTTPOther, false},
		{ClassCancelled, false},
		{ClassBlocked, false},
		{ClassUnknown, false},
	}
	for _, tt := range tests {
		if got := Retryable(tt.class); got != tt.want {
			t.Errorf("Retryable(%q) = %v, want %v", tt.class, got, tt.want)
		}
	}
}
