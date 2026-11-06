package probe

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// maxBodyBytes caps how much of a response body is read (blueprint §5.1).
const maxBodyBytes = 1 << 20 // 1 MiB

// Result is the outcome of one probe of one URL.
// A failed probe is still a Result: Err is set and the fields
// observed before the failure are kept.
type Result struct {
	URL        string
	StatusCode int
	Headers    time.Duration // request start -> status and headers received
	Total      time.Duration // request start -> body read, or -> failure
	BodyBytes  int64         // bytes read, capped at maxBodyBytes
	Err        error         // nil if the HTTP exchange completed
}

// Config holds the separate per-stage timeouts (blueprint §5.1).
// The overall timeout is the deadline on the ctx passed to Probe.
type Config struct {
	DialTimeout           time.Duration // DNS lookup + TCP connect
	TLSHandshakeTimeout   time.Duration // HTTPS handshake
	ResponseHeaderTimeout time.Duration // request sent -> headers received
}

// DefaultConfig returns starting timeout values. They are not benchmarked yet.
func DefaultConfig() Config {
	return Config{
		DialTimeout:           5 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
}

// Prober sends probes using one shared HTTP client,
// so connections can be reused between probes.
type Prober struct {
	client *http.Client
}

// New creates a Prober whose client enforces the timeouts in cfg
// and does not follow redirects.
func New(cfg Config) *Prober {
	dialer := &net.Dialer{
		Timeout: cfg.DialTimeout,
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
	}
	return &Prober{
		client: &http.Client{
			Transport: transport,
			// Do not follow redirects until destinations can be validated
			// (blueprint §5.1, §13). The 3xx response is returned instead.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Probe sends one GET request to url and measures it.
// It never prints; the caller controls the deadline through ctx.
func (p *Prober) Probe(ctx context.Context, url string) Result {
	r := Result{URL: url}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		r.Err = fmt.Errorf("creating request: %w", err)
		return r
	}

	start := time.Now()

	res, err := p.client.Do(req)
	if err != nil {
		r.Total = time.Since(start)
		r.Err = fmt.Errorf("sending request: %w", err)
		return r
	}
	defer res.Body.Close()

	r.Headers = time.Since(start)
	r.StatusCode = res.StatusCode

	n, err := io.Copy(io.Discard, io.LimitReader(res.Body, maxBodyBytes))
	r.BodyBytes = n
	r.Total = time.Since(start)
	if err != nil {
		r.Err = fmt.Errorf("reading body: %w", err)
	}
	return r
}
