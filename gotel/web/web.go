// Package web serves the dashboard: static files embedded in the Gotell
// binary, so there is still only one file to run (no build step, no
// frontend framework).
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Handler serves the dashboard files with security headers.
func Handler() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embedded folder is part of the binary; this cannot fail
	}
	fileServer := http.FileServerFS(static)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Only our own scripts and styles may run, and the page may only
		// talk to Gotell itself. Target URLs are shown as text, never run.
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		fileServer.ServeHTTP(w, r)
	})
}
