package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"gotel/netguard"
	"gotel/store"
)

// Limits for a new target. The interval floor keeps one Gotell from
// hammering a site; SSRF checks on the destination come in Phase 10.
const (
	minInterval = 10 * time.Second
	maxInterval = 24 * time.Hour
	maxURLLen   = 2048
	maxBodySize = 4 << 10 // 4 KiB is plenty for {"url": ..., "interval_seconds": ...}
)

// requireToken lets a request through only with "Authorization: Bearer
// <token>". It compares in constant time, so response timing cannot leak
// the token one character at a time.
func (s *Server) requireToken(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ctl == nil || s.token == "" {
			writeError(w, http.StatusForbidden, "target management is disabled")
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gotell"`)
			writeError(w, http.StatusUnauthorized, "a valid admin token is required to change targets")
			return
		}
		next(w, r)
	})
}

// newTargetRequest is the body of POST /api/targets.
type newTargetRequest struct {
	URL             string `json:"url"`
	IntervalSeconds int    `json:"interval_seconds"`
}

// validateTarget checks a new target and returns its cleaned URL.
func validateTarget(req newTargetRequest) (string, time.Duration, error) {
	raw := strings.TrimSpace(req.URL)
	if raw == "" {
		return "", 0, errors.New("url is required")
	}
	if len(raw) > maxURLLen {
		return "", 0, errors.New("url is too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, errors.New("url is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", 0, errors.New("url must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return "", 0, errors.New("url must include a host name")
	}
	if u.User != nil {
		// Passwords in URLs end up in logs and the database.
		return "", 0, errors.New("url must not contain a username or password")
	}
	interval := time.Duration(req.IntervalSeconds) * time.Second
	if interval < minInterval || interval > maxInterval {
		return "", 0, errors.New("interval_seconds must be from 10 to 86400")
	}
	return u.String(), interval, nil
}

func (s *Server) addTarget(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req newTargetRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, `body must be JSON like {"url": "https://...", "interval_seconds": 60}`)
		return
	}
	cleanURL, interval, err := validateTarget(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.checkAddress(r.Context(), cleanURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id, err := s.store.AddTarget(r.Context(), cleanURL, interval)
	if errors.Is(err, store.ErrDuplicate) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if err := s.ctl.Add(id, cleanURL, interval); err != nil {
		log.Printf("api: target %d saved, but the scheduler did not take it: %v", id, err)
		writeError(w, http.StatusServiceUnavailable, "saved, but it will only be checked after a restart")
		return
	}
	s.respondWithTarget(w, r, id, http.StatusCreated)
}

func (s *Server) deleteTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.findTarget(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTarget(r.Context(), t.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		internalError(w, err)
		return
	}
	if err := s.ctl.Remove(t.ID); err != nil {
		log.Printf("api: target %d deleted, but the scheduler did not take it: %v", t.ID, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pauseTarget(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, true)
}

func (s *Server) resumeTarget(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, false)
}

// setPaused records a maintenance window (blueprint §12.2) and tells the
// scheduler. Pausing twice would open two overlapping windows, so it is
// refused.
func (s *Server) setPaused(w http.ResponseWriter, r *http.Request, pause bool) {
	t, ok := s.findTarget(w, r)
	if !ok {
		return
	}
	if t.Paused == pause {
		msg := "target is already paused"
		if !pause {
			msg = "target is not paused"
		}
		writeError(w, http.StatusConflict, msg)
		return
	}

	now := s.now()
	var err error
	if pause {
		err = s.store.PauseTarget(r.Context(), t.ID, now)
	} else {
		err = s.store.ResumeTarget(r.Context(), t.ID, now)
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if pause {
		err = s.ctl.Pause(t.ID)
	} else {
		err = s.ctl.Resume(t.ID)
	}
	if err != nil {
		log.Printf("api: target %d saved as paused=%v, but the scheduler did not take it: %v", t.ID, pause, err)
		writeError(w, http.StatusServiceUnavailable, "saved, but it only takes effect after a restart")
		return
	}
	s.respondWithTarget(w, r, t.ID, http.StatusOK)
}

// respondWithTarget answers with the target's current summary.
func (s *Server) respondWithTarget(w http.ResponseWriter, r *http.Request, id, code int) {
	rows, err := s.store.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	for _, t := range rows {
		if t.ID == id {
			tj, _, err := s.targetWithHours(r.Context(), t, 24)
			if err != nil {
				internalError(w, err)
				return
			}
			writeJSON(w, code, tj)
			return
		}
	}
	writeError(w, http.StatusNotFound, "target not found")
}

// checkAddress refuses a target whose host is, or resolves to, an internal
// address, with a clear message. It is a convenience: the probe checks
// every connection's real IP again (netguard), so a host that changes its
// DNS answer later is still caught. If the name cannot be looked up now,
// the target is accepted, and its checks will show the DNS failure.
func (s *Server) checkAddress(ctx context.Context, rawURL string) error {
	if s.allowPrivate {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("url is not valid")
	}
	host := u.Hostname()

	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if addrs, err = s.resolver.LookupNetIP(ctx, "ip", host); err != nil {
			return nil
		}
	}
	for _, ip := range addrs {
		if netguard.IsBlocked(ip) {
			return fmt.Errorf("%s points to a private or internal address (%s). Gotell refuses these by default, "+
				"so a target can't be used to reach internal systems. To monitor your own network, start Gotell with -allow-private", host, ip.Unmap())
		}
	}
	return nil
}
