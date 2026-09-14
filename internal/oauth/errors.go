package oauth

import (
	"errors"
	"fmt"
)

// Sentinel errors. All are safe to expose: they contain no token material,
// codes, verifiers, state values, or response bodies. Use errors.Is against
// these when branching on failure kind.
var (
	// ErrInvalidVerifier reports a PKCE verifier outside the RFC 7636
	// alphabet or length range.
	ErrInvalidVerifier = errors.New("invalid PKCE verifier")
	// ErrTokenEndpoint reports a non-2xx token endpoint response. The error
	// message carries only the HTTP status; response bodies are never
	// rendered.
	ErrTokenEndpoint = errors.New("token endpoint request failed")
	// ErrInvalidTokenResponse reports a token response that could not be
	// parsed or lacks an access token.
	ErrInvalidTokenResponse = errors.New("invalid token response")
	// ErrInvalidToken reports a JWT that is not structurally decodable.
	ErrInvalidToken = errors.New("invalid token JWT")
	// ErrAccountIDNotFound reports a JWT with none of the verified account
	// identifier claims.
	ErrAccountIDNotFound = errors.New("account id not found in token claims")
)

// Error is a redacted OAuth error. Error() renders only Op and Msg — fixed,
// pre-sanitized strings that never contain tokens, codes, verifiers, state
// values, or provider response bodies. The wrapped cause (Err) is never
// rendered; it exists so errors.Is and errors.As can still classify the
// failure for logging and branching.
type Error struct {
	// Op is a short operation label, e.g. "token.exchange".
	Op string
	// Msg is a fixed, safe description.
	Msg string
	// Err is the wrapped cause, never rendered.
	Err error
}

func (e *Error) Error() string {
	if e.Op == "" {
		return e.Msg
	}
	return e.Op + ": " + e.Msg
}

// Unwrap exposes the wrapped cause for errors.Is/As without ever rendering it.
func (e *Error) Unwrap() error { return e.Err }

// errf builds a redacted *Error wrapping a sentinel, with a fixed safe
// message. op is a constant operation label.
func errf(op string, sentinel error, msg string) error {
	return &Error{Op: op, Msg: msg, Err: sentinel}
}

// Redact returns err unchanged when it is already a safe *Error, and
// otherwise wraps it in a generic redacted error so that no underlying
// message (which may embed token material) can escape. It returns nil for
// nil input.
func Redact(err error) error {
	if err == nil {
		return nil
	}
	var re *Error
	if errors.As(err, &re) {
		return err
	}
	return &Error{Op: "oauth", Msg: "operation failed", Err: err}
}

// tokenEndpointError builds a redacted error for a non-2xx token endpoint
// response. status is safe to render; the response body is never read into
// any error message. When the body carries a provider error code
// ({"error": "invalid_grant", ...}), the code is echoed as the message — it
// is a fixed enum value, not free-form content. The error_description field
// is deliberately discarded: it can echo sensitive request material.
func tokenEndpointError(status int, code string) error {
	switch code {
	case "invalid_request", "invalid_client", "invalid_grant", "invalid_token", "unauthorized_client", "unsupported_grant_type", "invalid_scope":
		// OAuth 2.0 token endpoint error codes are a closed vocabulary. Do not
		// echo an arbitrary provider string that could contain request material.
	default:
		code = ""
	}
	if code != "" {
		return &Error{
			Op:  "token.endpoint",
			Msg: fmt.Sprintf("token endpoint returned HTTP %d: %s", status, code),
			Err: ErrTokenEndpoint,
		}
	}
	return &Error{
		Op:  "token.endpoint",
		Msg: fmt.Sprintf("token endpoint returned HTTP %d", status),
		Err: ErrTokenEndpoint,
	}
}
