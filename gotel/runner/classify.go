package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"

	"gotel/probe"
)

// Classify maps what one attempt observed to a FailureClass (blueprint §7.1).
// It only interprets; it never changes the Result.
//
// The order of the checks matters: the most specific cause wins.
func Classify(r probe.Result, sp probe.StatusPolicy) FailureClass {
	err := r.Err
	if err == nil {
		return classifyStatus(r.StatusCode, sp)
	}

	switch {
	// Stopped by us: never the target's fault, so check it first.
	case errors.Is(err, context.Canceled):
		return ClassCancelled

	// A status line arrived, so the failure happened while reading the body.
	case r.StatusCode != 0:
		return ClassBodyRead

	case isDNS(err):
		return ClassDNS
	case isTLS(err):
		return ClassTLS
	case isConnect(err):
		return ClassConnect
	case isTimeout(err):
		return ClassTimeout
	}
	return ClassUnknown
}

// classifyStatus handles a completed HTTP exchange.
func classifyStatus(code int, sp probe.StatusPolicy) FailureClass {
	switch {
	case sp.Allows(code):
		return ClassNone
	case code >= 400 && code <= 499:
		return ClassHTTP4xx
	case code >= 500 && code <= 599:
		return ClassHTTP5xx
	default:
		return ClassHTTPOther
	}
}

func isDNS(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

func isTLS(err error) bool {
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError // the server did not speak TLS
	return errors.As(err, &certErr) ||
		errors.As(err, &unknownCA) ||
		errors.As(err, &hostErr) ||
		errors.As(err, &invalidErr) ||
		errors.As(err, &recordErr)
}

func isConnect(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Retryable reports whether an attempt with this class is worth trying
// again (blueprint §6.2). Retrying never hides the failure: every attempt
// is kept, and a run that needed a retry is OutcomeRecovered, not success.
func Retryable(c FailureClass) bool {
	switch c {
	case ClassDNS, // resolver hiccups are often brief
		ClassConnect,  // the server may be restarting
		ClassTimeout,  // e.g. a cold start
		ClassHTTP5xx,  // 502/503 while a host wakes up or deploys
		ClassBodyRead: // the connection dropped mid-response
		return true
	default:
		// None, 4xx, other statuses, TLS, cancelled, unknown.
		return false
	}
}
