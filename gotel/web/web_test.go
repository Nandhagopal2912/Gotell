package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestServesTheDashboard(t *testing.T) {
	for _, tt := range []struct{ path, contains, contentType string }{
		{"/", "<title>Gotell</title>", "text/html"},
		{"/app.js", "textContent", "javascript"},
		{"/style.css", "--good", "text/css"},
	} {
		rec := serve(t, tt.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", tt.path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), tt.contains) {
			t.Errorf("GET %s: body does not contain %q", tt.path, tt.contains)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tt.contentType) {
			t.Errorf("GET %s: Content-Type %q, want %s", tt.path, ct, tt.contentType)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := serve(t, "/")
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP = %q, want only same-origin scripts", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}

func TestUnknownFileIs404(t *testing.T) {
	if rec := serve(t, "/nope.js"); rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}
