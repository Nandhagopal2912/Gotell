package probe

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestProbeDNSFailure(t *testing.T) {
	// The .invalid top-level domain is reserved (RFC 6761) and never
	// resolves, so this lookup always fails, with or without internet.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := New(DefaultConfig()).Probe(ctx, "http://gotell-test.invalid/")

	if r.Err == nil {
		t.Fatal("expected an error, got nil")
	}

	var dnsErr *net.DNSError
	if !errors.As(r.Err, &dnsErr) {
		t.Fatalf("Err = %v, want it to wrap a *net.DNSError", r.Err)
	}
	if r.DNS == nil {
		t.Error("DNS = nil, want measured (the lookup ran, then failed)")
	}
	if r.TCPConnect != nil {
		t.Error("TCPConnect measured, want nil (no address to connect to)")
	}
	if r.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0", r.StatusCode)
	}
	t.Logf("dnsErr: name=%q notFound=%v timeout=%v", dnsErr.Name, dnsErr.IsNotFound, dnsErr.IsTimeout)
}

func TestProbeConnectFailureIsNotDNS(t *testing.T) {
	// Connection refused on 127.0.0.1: no lookup at all, so it must
	// not be mistaken for a DNS failure.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	r := New(DefaultConfig()).Probe(ctx, "http://"+addr+"/")

	if r.Err == nil {
		t.Fatal("expected an error, got nil")
	}
	var dnsErr *net.DNSError
	if errors.As(r.Err, &dnsErr) {
		t.Errorf("Err = %v, want a connection error, not a DNS error", r.Err)
	}
	var opErr *net.OpError
	if !errors.As(r.Err, &opErr) || opErr.Op != "dial" {
		t.Errorf("Err = %v, want a *net.OpError with Op \"dial\"", r.Err)
	}
}
