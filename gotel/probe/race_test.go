package probe

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestProbeLateTraceHookIsRaceFree(t *testing.T) {
	// A server that accepts the TCP connection but never answers the TLS
	// handshake, so the handshake is still running when ctx expires.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		<-done // hold the connection open until the test ends
		conn.Close()
	}()

	cfg := DefaultConfig()
	cfg.TLSHandshakeTimeout = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	r := New(cfg).Probe(ctx, "https://"+ln.Addr().String()+"/")
	if r.Err == nil {
		t.Fatal("expected an error, got nil")
	}

	// Give the abandoned handshake time to time out and fire its hooks.
	// Run with -race: an unprotected hook write shows up as a DATA RACE.
	time.Sleep(400 * time.Millisecond)
}
