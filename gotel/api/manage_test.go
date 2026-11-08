package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

const testToken = "s3cret-token"

// fakeControl records what the API asked the scheduler to do.
type fakeControl struct{ calls []string }

func (f *fakeControl) Add(id int, url string, _ time.Duration) error {
	f.calls = append(f.calls, "add "+url)
	return nil
}
func (f *fakeControl) Remove(id int) error { f.calls = append(f.calls, "remove"); return nil }
func (f *fakeControl) Pause(id int) error  { f.calls = append(f.calls, "pause"); return nil }
func (f *fakeControl) Resume(id int) error { f.calls = append(f.calls, "resume"); return nil }

// fakeResolver answers DNS lookups from a fixed table; unknown names fail.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := f[host]; ok {
		return addrs, nil
	}
	return nil, errors.New("no such host")
}

var testDNS = fakeResolver{
	"localhost":        {netip.MustParseAddr("127.0.0.1")},
	"public.test":      {netip.MustParseAddr("93.184.216.34")},
	"sneaky.test":      {netip.MustParseAddr("192.168.1.10")},
	"metadata.test":    {netip.MustParseAddr("169.254.169.254")},
	"half-sneaky.test": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")},
}

// managedServer builds a server with a fake scheduler, fake DNS and the
// test token.
func managedServer(t *testing.T) (*Server, *fakeControl) {
	t.Helper()
	db, _ := fixture(t)
	ctl := &fakeControl{}
	s := New(db, ctl, testToken)
	s.now = func() time.Time { return now }
	s.resolver = testDNS
	return s, ctl
}

// managed is managedServer's handler.
func managed(t *testing.T) (http.Handler, *fakeControl) {
	t.Helper()
	s, ctl := managedServer(t)
	return s.Handler(), ctl
}

// send performs a request with an optional token and JSON body.
func send(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChangesNeedTheToken(t *testing.T) {
	h, ctl := managed(t)
	for _, token := range []string{"", "wrong"} {
		for _, tt := range []struct{ method, path, body string }{
			{http.MethodPost, "/api/targets", `{"url":"https://x.test/","interval_seconds":60}`},
			{http.MethodDelete, "/api/targets/1", ""},
			{http.MethodPost, "/api/targets/1/pause", ""},
		} {
			if rec := send(h, tt.method, tt.path, token, tt.body); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: status %d, want 401", tt.method, tt.path, token, rec.Code)
			}
		}
	}
	if len(ctl.calls) != 0 {
		t.Errorf("scheduler calls = %v, want none without a valid token", ctl.calls)
	}
}

func TestManagementDisabledWithoutToken(t *testing.T) {
	db, _ := fixture(t)
	h := New(db, nil, "").Handler()
	if rec := send(h, http.MethodPost, "/api/targets", "anything", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 when management is disabled", rec.Code)
	}
}

func TestAddTargetValidates(t *testing.T) {
	h, ctl := managed(t)
	for _, tt := range []struct{ body, why string }{
		{`not json`, "not JSON"},
		{`{"url":"https://x.test/","interval_seconds":60,"extra":1}`, "unknown field"},
		{`{"url":"","interval_seconds":60}`, "empty url"},
		{`{"url":"ftp://x.test/","interval_seconds":60}`, "not http(s)"},
		{`{"url":"file:///etc/passwd","interval_seconds":60}`, "file scheme"},
		{`{"url":"https:///path","interval_seconds":60}`, "no host"},
		{`{"url":"https://user:pw@x.test/","interval_seconds":60}`, "password in URL"},
		{`{"url":"https://x.test/","interval_seconds":5}`, "interval too short"},
		{`{"url":"https://x.test/","interval_seconds":999999}`, "interval too long"},
	} {
		if rec := send(h, http.MethodPost, "/api/targets", testToken, tt.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (body %s)", tt.why, rec.Code, rec.Body)
		}
	}
	if len(ctl.calls) != 0 {
		t.Errorf("scheduler calls = %v, want none for invalid targets", ctl.calls)
	}
}

func TestAddPauseResumeDelete(t *testing.T) {
	h, ctl := managed(t)

	rec := send(h, http.MethodPost, "/api/targets", testToken, `{"url":"https://new.test/health","interval_seconds":60}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"status":"unknown"`) {
		t.Fatalf("add: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := send(h, http.MethodPost, "/api/targets", testToken, `{"url":"https://new.test/health","interval_seconds":60}`); rec.Code != http.StatusConflict {
		t.Errorf("adding a duplicate: status %d, want 409", rec.Code)
	}

	if rec := send(h, http.MethodPost, "/api/targets/3/pause", testToken, ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"status":"paused"`) {
		t.Fatalf("pause: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := send(h, http.MethodPost, "/api/targets/3/pause", testToken, ""); rec.Code != http.StatusConflict {
		t.Errorf("pausing twice: status %d, want 409", rec.Code)
	}
	if rec := send(h, http.MethodPost, "/api/targets/3/resume", testToken, ""); rec.Code != http.StatusOK {
		t.Errorf("resume: status %d, body %s", rec.Code, rec.Body)
	}

	if rec := send(h, http.MethodDelete, "/api/targets/3", testToken, ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete: status %d, want 204", rec.Code)
	}
	if rec := send(h, http.MethodGet, "/api/targets/3", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("after delete: status %d, want 404", rec.Code)
	}
	if rec := send(h, http.MethodDelete, "/api/targets/3", testToken, ""); rec.Code != http.StatusNotFound {
		t.Errorf("deleting twice: status %d, want 404", rec.Code)
	}

	want := []string{"add https://new.test/health", "pause", "resume", "remove"}
	if strings.Join(ctl.calls, ",") != strings.Join(want, ",") {
		t.Errorf("scheduler calls = %v, want %v", ctl.calls, want)
	}
}

func TestAddTargetRefusesInternalAddresses(t *testing.T) {
	h, ctl := managed(t)
	for _, url := range []string{
		"http://localhost:8080/",
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://10.0.0.5/health",
		"https://sneaky.test/",      // a public-looking name that resolves to 192.168.1.10
		"http://metadata.test/",     // resolves to the cloud metadata service
		"https://half-sneaky.test/", // one public and one private answer
	} {
		rec := send(h, http.MethodPost, "/api/targets", testToken, `{"url":"`+url+`","interval_seconds":60}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "-allow-private") {
			t.Errorf("%s: status %d, body %s; want 400 mentioning -allow-private", url, rec.Code, rec.Body)
		}
	}
	if len(ctl.calls) != 0 {
		t.Errorf("scheduler calls = %v, want none", ctl.calls)
	}

	// Public addresses, and names that cannot be looked up yet, are fine.
	for _, url := range []string{"https://public.test/", "https://not-in-dns.test/"} {
		if rec := send(h, http.MethodPost, "/api/targets", testToken, `{"url":"`+url+`","interval_seconds":60}`); rec.Code != http.StatusCreated {
			t.Errorf("%s: status %d, body %s; want 201", url, rec.Code, rec.Body)
		}
	}
}

func TestAllowPrivateAcceptsInternalAddresses(t *testing.T) {
	s, _ := managedServer(t)
	s.SetAllowPrivate(true)
	rec := send(s.Handler(), http.MethodPost, "/api/targets", testToken, `{"url":"http://localhost:3000/health","interval_seconds":60}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("status %d, body %s; want 201 with -allow-private", rec.Code, rec.Body)
	}
}

func TestTargetLimits(t *testing.T) {
	s, ctl := managedServer(t) // the fixture already has 2 targets
	s.SetLimits(4, 1)
	h := s.Handler()
	add := func(url string) *httptest.ResponseRecorder {
		return send(h, http.MethodPost, "/api/targets", testToken, `{"url":"`+url+`","interval_seconds":60}`)
	}

	if rec := add("https://public.test/a"); rec.Code != http.StatusCreated {
		t.Fatalf("first target on public.test: status %d, want 201", rec.Code)
	}
	// Same host (any case), different path: the per-host limit (1) applies.
	if rec := add("https://PUBLIC.test/b"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "-max-targets-per-host") {
		t.Errorf("second target on the same host: status %d, body %s; want 409 naming the flag", rec.Code, rec.Body)
	}

	if rec := add("https://not-in-dns.test/"); rec.Code != http.StatusCreated {
		t.Fatalf("4th target: status %d, want 201", rec.Code)
	}
	// Now 4 targets: the total limit applies.
	if rec := add("https://other.test/"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "-max-targets") {
		t.Errorf("5th target: status %d, body %s; want 409 naming -max-targets", rec.Code, rec.Body)
	}

	// Deleting one frees a place again.
	if rec := send(h, http.MethodDelete, "/api/targets/1", testToken, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", rec.Code)
	}
	if rec := add("https://other.test/"); rec.Code != http.StatusCreated {
		t.Errorf("after deleting one: status %d, want 201", rec.Code)
	}
	if n := len(ctl.calls); n != 4 { // 3 adds and 1 remove reached the scheduler
		t.Errorf("scheduler calls = %v", ctl.calls)
	}
}

func TestDefaultLimits(t *testing.T) {
	s, _ := managedServer(t)
	if s.maxTargets != 200 || s.maxPerHostTgt != 20 {
		t.Errorf("defaults = %d total, %d per host; want 200 and 20", s.maxTargets, s.maxPerHostTgt)
	}
	s.SetLimits(0, -1) // ignored
	if s.maxTargets != 200 || s.maxPerHostTgt != 20 {
		t.Errorf("after SetLimits(0, -1): %d and %d; want the defaults kept", s.maxTargets, s.maxPerHostTgt)
	}
}
