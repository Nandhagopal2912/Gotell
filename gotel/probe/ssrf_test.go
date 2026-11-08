package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gotel/netguard"
)

// With the default config, a probe must never reach a local server, even
// when it is reached by a name ("localhost") rather than an IP address:
// the check runs on the resolved IP at connect time.
func TestDefaultConfigBlocksInternalAddresses(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer srv.Close()

	byName := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	for _, url := range []string{srv.URL, byName} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		r := New(DefaultConfig()).Probe(ctx, url)
		cancel()

		if !errors.Is(r.Err, netguard.ErrBlocked) {
			t.Errorf("%s: Err = %v, want ErrBlocked", url, r.Err)
		}
		if r.StatusCode != 0 || r.TCPConnect != nil {
			t.Errorf("%s: status %d, tcp %v; want no connection at all", url, r.StatusCode, r.TCPConnect)
		}
	}
	if reached {
		t.Error("the local server received a request; the guard let it through")
	}
}

func TestAllowPrivateReachesLocalServers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cfg := DefaultConfig()
	cfg.AllowPrivate = true
	if r := New(cfg).Probe(ctx, srv.URL); r.Err != nil || r.StatusCode != 200 {
		t.Errorf("with AllowPrivate: status %d, err %v; want 200", r.StatusCode, r.Err)
	}
}
