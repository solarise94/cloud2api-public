package loginsdk

import (
	"errors"
	"fmt"
)

// Error codes. The HTTP layer maps them onto OpenList response codes and the
// page keys its hints off them.
const (
	CodeInvalidInput  = "invalid_input"  // 400: malformed or missing input
	CodeNotFound      = "not_found"      // 404: unknown platform / session / storage
	CodeAppMissing    = "app_missing"    // 400: the platform needs an app profile first
	CodeAppInvalid    = "app_invalid"    // 400: the app profile is incomplete for its mode
	CodeUnsupported   = "unsupported"    // 400: the adapter cannot do this (e.g. reauth of a SharePoint storage)
	CodeConflict      = "conflict"       // 409: concurrent change, already used state, mount taken…
	CodeStateMismatch = "state_mismatch" // 400: OAuth state unknown, used or for another session
	CodeExpired       = "expired"        // 410: session or QR code expired
	CodeCancelled     = "cancelled"      // 410: session cancelled
	CodeLimit         = "limit"          // 429: too many sessions / too frequent
	CodeProvider      = "provider"       // 502: the provider rejected the request
	CodeNetwork       = "network"        // 502: the provider could not be reached
	CodeInternal      = "internal"       // 500
)

// Error is the SDK's typed error. Message is Chinese and safe to show: it has
// been scrubbed of secrets by whoever built it.
type Error struct {
	Code    string
	Message string
	Err     error // optional cause (already scrubbed or secret-free)
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an *Error with a formatted message.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds an *Error around a cause.
func Wrap(code, message string, err error) *Error {
	return &Error{Code: code, Message: message, Err: err}
}

// CodeOf returns the code of the first *Error in err's chain, or CodeInternal.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}
