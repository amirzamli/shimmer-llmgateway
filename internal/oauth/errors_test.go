package oauth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorRendersOnlyOpAndMsg(t *testing.T) {
	secret := "super-secret-refresh-token"
	err := errf("token.refresh", ErrTokenEndpoint, "token endpoint returned HTTP 500")
	msg := err.Error()
	if !strings.Contains(msg, "token.refresh") || !strings.Contains(msg, "HTTP 500") {
		t.Errorf("Error() = %q, want op and msg", msg)
	}
	if strings.Contains(msg, secret) {
		t.Errorf("Error() leaked secret: %q", msg)
	}
}

func TestErrorUnwrapSentinel(t *testing.T) {
	err := errf("token.exchange", ErrInvalidTokenResponse, "token response lacks an access token")
	if !errors.Is(err, ErrInvalidTokenResponse) {
		t.Errorf("errors.Is(err, ErrInvalidTokenResponse) = false")
	}
	if errors.Is(err, ErrTokenEndpoint) {
		t.Error("errors.Is against unrelated sentinel = true")
	}
	var re *Error
	if !errors.As(err, &re) || re.Op != "token.exchange" {
		t.Errorf("errors.As did not recover *Error: %+v", re)
	}
}

func TestRedact(t *testing.T) {
	if Redact(nil) != nil {
		t.Error("Redact(nil) != nil")
	}
	safe := errf("state.consume", ErrStateUsed, "state was already consumed")
	if Redact(safe) != safe {
		t.Error("Redact returned a different error for an already-safe *Error")
	}
	// An arbitrary error may carry secrets in its text; Redact must hide it.
	leaky := fmt.Errorf("request body contained refresh token %q", "rt-leak-me")
	red := Redact(leaky)
	msg := red.Error()
	if strings.Contains(msg, "rt-leak-me") || strings.Contains(msg, "request body") {
		t.Errorf("Redact leaked underlying message: %q", msg)
	}
	if !strings.Contains(msg, "oauth") {
		t.Errorf("Redact message lacks package context: %q", msg)
	}
	// The cause stays reachable for classification without rendering.
	if !errors.Is(red, leaky) {
		t.Error("errors.Is(red, leaky) = false, want the cause reachable")
	}
}

func TestTokenEndpointError(t *testing.T) {
	err := tokenEndpointError(401, "invalid_token")
	msg := err.Error()
	if !strings.Contains(msg, "401") || !strings.Contains(msg, "invalid_token") {
		t.Errorf("Error() = %q, want status and code", msg)
	}
	if !errors.Is(err, ErrTokenEndpoint) {
		t.Error("tokenEndpointError not errors.Is ErrTokenEndpoint")
	}
	err = tokenEndpointError(500, "")
	if strings.Contains(err.Error(), "HTTP 500:") {
		t.Errorf("Error() = %q, want no trailing code after status", err.Error())
	}
}
