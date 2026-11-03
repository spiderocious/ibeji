// Package api is the daemon's local HTTP surface.
//
// It is the single programmable surface of the product: the Electron window,
// the `ibeji` CLI and the MCP server are all clients of it. Nothing is reachable
// from the UI that is not reachable from the CLI and from an agent, because
// there is only one implementation of each capability.
//
// It listens on loopback only and authenticates with a token from the keychain.
// Loopback keeps the network out; the token keeps *other local processes* out,
// which is the likelier problem of the two — a web page in a browser on this
// machine can POST to localhost, and must not be able to run commands on the
// other one.
package api

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, documented failure mode.
//
// These strings are a contract with every client, including agents that branch
// on them: an agent that sees "declined" should tell the user and stop, one
// that sees "peer_offline" should retry later, and one that sees "timed_out"
// should say nobody answered. Prose cannot carry that distinction reliably, so
// the code is the thing clients are expected to read and the message is for
// humans. They are never renamed; a new failure mode gets a new code.
type Code string

const (
	// CodeBadRequest means the request was malformed or missing a field.
	CodeBadRequest Code = "bad_request"

	// CodeUnauthorized means the API token was missing or wrong.
	CodeUnauthorized Code = "unauthorized"

	// CodeNotFound means the addressed thing does not exist — a device id, a
	// space, a process, a path.
	CodeNotFound Code = "not_found"

	// CodeNotPaired means the operation needs a paired device and none is.
	CodeNotPaired Code = "not_paired"

	// CodeAlreadyPaired means a second device tried to pair. v1 is exactly two.
	CodeAlreadyPaired Code = "already_paired"

	// CodeKeyMismatch means the peer presented a key that is not the pinned
	// one. The client must raise a re-pair prompt and must never auto-trust.
	CodeKeyMismatch Code = "key_mismatch"

	// CodePeerOffline means the paired device is not reachable right now. On a
	// LAN this is normal — the other machine is asleep — so clients render it
	// as unreachable rather than as an error.
	CodePeerOffline Code = "peer_offline"

	// CodeDeclined means the user on the receiving machine said no.
	CodeDeclined Code = "declined"

	// CodeTimedOut means a permission prompt expired unanswered. Distinct from
	// declined on purpose: nobody said no, nobody was there.
	CodeTimedOut Code = "timed_out"

	// CodeExecTimeout means the command itself ran longer than its timeout.
	// Distinct from timed_out, which is about the approval.
	CodeExecTimeout Code = "exec_timeout"

	// CodeSyncConflict means a space cannot converge because a conflict is
	// unresolved, so a barrier cannot proceed.
	CodeSyncConflict Code = "sync_conflict"

	// CodePairingExpired means a pairing attempt was confirmed too late.
	CodePairingExpired Code = "pairing_expired"

	// CodeCodeMismatch means the confirmed pairing code did not match.
	CodeCodeMismatch Code = "code_mismatch"

	// CodeInternal is the catch-all. Its presence in a trace is a bug report.
	CodeInternal Code = "internal"
)

// Status maps a code to the HTTP status that carries it.
func (c Code) Status() int {
	switch c {
	case CodeBadRequest, CodeCodeMismatch:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeDeclined:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeNotPaired, CodeAlreadyPaired, CodeSyncConflict, CodePairingExpired:
		return http.StatusConflict
	case CodeTimedOut, CodeExecTimeout:
		return http.StatusGatewayTimeout
	case CodePeerOffline:
		return http.StatusServiceUnavailable
	case CodeKeyMismatch:
		// Not 401: the request was authorised, the *peer* is wrong. A client
		// that retries with better credentials is fixing the wrong thing.
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// Error is a failure with a stable code, a human sentence, and optionally a
// hint naming the likely cause.
//
// The hint is the part that saves an agent a round of guessing: "the prompt was
// never seen because Desktop's window was closed" is actionable in a way that
// "timed out" is not.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`

	// wrapped carries the underlying cause for errors.Is/As. It is never
	// serialised: internal detail does not cross the API boundary.
	wrapped error
}

func (e *Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.wrapped }

// Is lets callers match on the code alone: errors.Is(err, &Error{Code: ...}).
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// Errf builds an Error with a formatted message.
func Errf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithHint returns a copy carrying a hint about the likely cause.
func (e *Error) WithHint(format string, args ...any) *Error {
	cp := *e
	cp.Hint = fmt.Sprintf(format, args...)
	return &cp
}

// Wrapping returns a copy carrying an underlying cause for errors.Is/As.
func (e *Error) Wrapping(err error) *Error {
	cp := *e
	cp.wrapped = err
	return &cp
}

// AsError converts any error into an *Error, mapping the domain errors the
// daemon's packages return onto their stable codes.
//
// Centralising the mapping here is what stops a handler inventing its own
// string for a failure that already has a code — the symptom of which is an
// agent that cannot branch reliably because the same condition reports
// differently depending on which endpoint it hit.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	for _, m := range domainMappings {
		if errors.Is(err, m.target) {
			return (&Error{Code: m.code, Message: m.message}).Wrapping(err)
		}
	}
	return (&Error{
		Code:    CodeInternal,
		Message: err.Error(),
	}).Wrapping(err)
}

// domainMappings is populated by register() in mappings.go, so this file does
// not import every package in the daemon.
type domainMapping struct {
	target  error
	code    Code
	message string
}

var domainMappings []domainMapping

// RegisterMapping teaches AsError about a domain error. Called from init in
// mappings.go.
func RegisterMapping(target error, code Code, message string) {
	domainMappings = append(domainMappings, domainMapping{target, code, message})
}
