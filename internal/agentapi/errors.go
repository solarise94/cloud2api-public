// Package agentapi implements the v0.4 agent data plane (UPGRADE §4): the
// /v1 endpoints with apikey-header authentication, logical paths inside
// per-key chroots, typed responses without OpenList fields, upload integrity
// checksums, idempotent retries, the trash and limited-time links.
package agentapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Error codes of UPGRADE §4.3 plus the documented Phase 1a extensions
// (invalid_request for malformed parameters, idempotency_mismatch, and
// link_expired for /l/{token} 410s).
const (
	CodeUnauthorized      = "unauthorized"
	CodeForbidden         = "forbidden"
	CodeDriveNotFound     = "drive_not_found"
	CodeDriveUnavailable  = "drive_unavailable"
	CodePathInvalid       = "path_invalid"
	CodePathNotFound      = "path_not_found"
	CodeAlreadyExists     = "already_exists"
	CodeTooLarge          = "too_large"
	CodeRateLimited       = "rate_limited"
	CodeChecksumMismatch  = "checksum_mismatch"
	CodeUpstreamError     = "upstream_error"
	CodeInvalidRequest    = "invalid_request"
	CodeIdempMismatch     = "idempotency_mismatch"
	CodeLinkExpired       = "link_expired"
	CodeTicketInvalid     = "ticket_invalid"
	CodeNotFound          = "not_found"
	CodeMethodNotAllowed  = "method_not_allowed"
	CodeLengthRequired    = "length_required"
	CodeHopExceeded       = "hop_limit_exceeded"
	CodeIdempotencyReplay = "" // marker only, not an error
)

// APIError is the typed error envelope of every /v1 response.
type APIError struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	Retryable   bool     `json:"retryable"`
	RetryAfter  int      `json:"retry_after,omitempty"` // seconds
	Hint        string   `json:"hint,omitempty"`
	Docs        string   `json:"docs,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`

	status int
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// WithStatus attaches the HTTP status (internal).
func newAPIError(status int, code, message string) *APIError {
	return &APIError{status: status, Code: code, Message: message}
}

// Status returns the HTTP status code of the error.
func (e *APIError) Status() int { return e.status }

// hint adds a hint (chainable).
func (e *APIError) hint(h string) *APIError { e.Hint = h; return e }

// docs fills the docs URL from the request base URL when written.
func (e *APIError) withDocs(base string) *APIError {
	if base != "" {
		e.Docs = base + "/custom/agent-instructions.txt#errors"
	}
	return e
}

func errUnauthorized(msg string) *APIError {
	if msg == "" {
		msg = "missing or invalid API key"
	}
	e := newAPIError(http.StatusUnauthorized, CodeUnauthorized, msg)
	e.Retryable = false
	return e.hint("send the key in the `apikey` request header (Authorization: Bearer also works); never put it in the URL")
}

func errForbidden(msg string) *APIError {
	return newAPIError(http.StatusForbidden, CodeForbidden, msg)
}

func errDriveNotFound(drive string, available []string) *APIError {
	e := newAPIError(http.StatusNotFound, CodeDriveNotFound,
		fmt.Sprintf("drive %q not found", drive))
	if len(available) > 0 {
		e.Hint = "available drives for this key: " + joinQuoted(available)
	}
	return e
}

func errDriveUnavailable(drive, why string) *APIError {
	e := newAPIError(http.StatusServiceUnavailable, CodeDriveUnavailable,
		fmt.Sprintf("drive %q is currently unavailable", drive))
	e.Retryable = true
	e.Hint = why
	return e
}

// driveUnavailableErr wraps errDriveUnavailable for storage-guard failures.
func driveUnavailableErr(drive, why string) *APIError {
	return errDriveUnavailable(drive, why)
}

func errPathInvalid(p string) *APIError {
	e := newAPIError(http.StatusBadRequest, CodePathInvalid,
		fmt.Sprintf("path %q is invalid or escapes the key root", p))
	e.Hint = "use an absolute logical path inside your key's root; '..' segments, backslashes and encoded traversal are rejected"
	return e
}

func errPathNotFound(p string) *APIError {
	e := newAPIError(http.StatusNotFound, CodePathNotFound,
		fmt.Sprintf("%s does not exist", p))
	e.Hint = "list the parent directory to see what exists"
	return e
}

func errAlreadyExists(p string) *APIError {
	e := newAPIError(http.StatusConflict, CodeAlreadyExists,
		fmt.Sprintf("%s already exists", p))
	e.Hint = "pass on_conflict=overwrite to replace it, or on_conflict=rename to keep both"
	return e
}

func errUpstream(msg string) *APIError {
	e := newAPIError(http.StatusBadGateway, CodeUpstreamError, "upstream storage error: "+msg)
	e.Retryable = true
	return e
}

func errInvalidRequest(msg string) *APIError {
	return newAPIError(http.StatusBadRequest, CodeInvalidRequest, msg)
}

func errNotFoundRoute() *APIError {
	return newAPIError(http.StatusNotFound, CodeNotFound, "endpoint not found; see /apidocumentation/openapi.json")
}

func errRateLimited(retryAfter time.Duration) *APIError {
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	e := newAPIError(http.StatusTooManyRequests, CodeRateLimited,
		fmt.Sprintf("rate limit exceeded; retry after %ds", secs))
	e.Retryable = true
	e.RetryAfter = secs
	return e
}

func joinQuoted(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ", "
		}
		out += "\"" + s + "\""
	}
	return out
}

// writeJSON writes a typed JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
