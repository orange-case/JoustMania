package flagsmith

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// Reasons (services.md §7.3 offline_reason, telemetry.md §10.4 push reasons).
const (
	ReasonTimeout                   = "timeout"
	ReasonConnection                = "connection"
	ReasonTLS                       = "tls"
	ReasonUnauthorized              = "unauthorized"
	ReasonNotFound                  = "not_found"
	ReasonBadValue                  = "bad_value" // the value cannot be encoded; no request is sent
	ReasonRateLimited               = "rate_limited"
	ReasonChangeRequestRequired     = "change_request_required"
	ReasonIdentityOverridesDisabled = "identity_overrides_disabled"
	ReasonProjectMismatch           = "project_mismatch"
	ReasonEdgeRequired              = "edge_required" // link-down: the project has no edge identities
	ReasonResponseTooLarge          = "response_too_large"
	ReasonStopped                   = "stopped"
	ReasonOther                     = "error"
)

// ReasonError is an error that is its reason (for example
// ReasonError{Reason: ReasonIdentityOverridesDisabled} from a start check).
// Use the value, not a pointer.
type ReasonError struct{ Reason string }

func (e ReasonError) Error() string { return "flagsmith: " + e.Reason }

// HTTPError is a non-2xx answer from Flagsmith.
type HTTPError struct {
	Status int
	// RetryAfter is the Retry-After of a 429 or 503 (0 when absent).
	RetryAfter time.Duration
	// Body is the start of the response body (at most MaxErrorBody bytes);
	// it is Flagsmith's answer and never holds a key of ours.
	Body []byte
	// JSONDetail: the body is a JSON object with a "detail" key.
	JSONDetail bool
}

// MaxErrorBody bounds HTTPError.Body.
const MaxErrorBody = 1 << 10

func (e *HTTPError) Error() string { return "flagsmith: HTTP " + strconv.Itoa(e.Status) }

// TransportError is a request that got no answer. The URL is never part of it
// (it may carry a token: the webhook URL, a query string).
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string { return "flagsmith: " + e.Op + ": " + e.Err.Error() }

// Unwrap returns the cause.
func (e *TransportError) Unwrap() error { return e.Err }

// Reason classifies err: "" for nil, a ReasonError's reason, unauthorized
// (401, 403), not_found, rate_limited, change_request_required, http_<code>
// for another status, timeout, tls, connection, else error.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	var re ReasonError
	if errors.As(err, &re) {
		return re.Reason
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch {
		case (he.Status == 400 || he.Status == 403) && strings.Contains(strings.ToLower(string(he.Body)), "change request"):
			return ReasonChangeRequestRequired
		case he.Status == 401 || he.Status == 403:
			return ReasonUnauthorized
		case he.Status == 404:
			return ReasonNotFound
		case he.Status == 429:
			return ReasonRateLimited
		}
		return "http_" + strconv.Itoa(he.Status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ReasonTimeout
	}
	if isTLS(err) {
		return ReasonTLS
	}
	var te *TransportError
	if errors.As(err, &te) {
		return ReasonConnection
	}
	return ReasonOther
}

func isTLS(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	return errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &verify) || errors.As(err, &record) || errors.As(err, &alert)
}

// IsLinkDown reports whether err takes the whole link offline (no answer, TLS,
// bad credentials, a 5xx, a start check such as identity overrides disabled)
// rather than refusing one key (404, 400, change request required, 429,
// handled by Retry-After), which marks that key refused and keeps the link up.
func IsLinkDown(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		if he.Status >= 500 {
			return true
		}
		return Reason(err) == ReasonUnauthorized
	}
	switch Reason(err) {
	case ReasonNotFound, ReasonRateLimited, ReasonChangeRequestRequired:
		return false
	}
	return !errors.Is(err, context.Canceled)
}
