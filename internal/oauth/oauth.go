// Package oauth implements the ChatGPT Plus device OAuth protocol used by
// OpenCode as an isolated protocol component: PKCE verifier validation,
// authorization-code exchange and refresh requests, token response parsing
// with expiry, ChatGPT account-ID extraction from the OpenAI/Codex JWT claims,
// and redacted errors that never render token material.
//
// The protocol constants below are pinned to the installed OpenCode v1.18.30
// implementation (packages/core/src/plugin/provider/openai.ts). The package
// is deliberately free of persistence, HTTP routing, and configuration
// integration; callers inject their own *http.Client.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
)

// Verified OpenCode v1.18.30 ChatGPT Plus OAuth constants
// (packages/core/src/plugin/provider/openai.ts in opencode 1.18.30).
const (
	// Issuer is the OAuth issuer / auth host.
	Issuer = "https://auth.openai.com"
	// ClientID is the public client identifier registered by OpenCode.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// TokenPath is the issuer-relative token endpoint.
	TokenPath = "/oauth/token"
	// DefaultExpiresIn is the token lifetime in seconds assumed when the
	// token response omits expires_in (matches OpenCode's ?? 3600).
	DefaultExpiresIn = 3600
	// Originator is the fixed upstream identity header used by the codex
	// endpoint.
	Originator = "opencode"
)

// PKCE verifier alphabet (RFC 7636 unreserved characters) and length.
// OpenCode generates a 43-character verifier; RFC 7636 requires 43-128.
const (
	verifierChars  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	verifierLength = 43
)

// Config pins the protocol endpoints and identifiers for one OAuth flow. The
// zero value is usable: every field falls back to the verified OpenCode
// constant via WithDefaults.
type Config struct {
	Issuer   string // default: Issuer
	ClientID string // default: ClientID
	// UserAgent is sent as the User-Agent header on token requests. OpenCode
	// sends "opencode/<version>"; this gateway sends its own identifier
	// instead of impersonating OpenCode.
	UserAgent string // default: "shimmer-llmgateway/oauth"
}

// WithDefaults returns c with every empty field replaced by the verified
// OpenCode constant. It never mutates c.
func (c Config) WithDefaults() Config {
	if c.Issuer == "" {
		c.Issuer = Issuer
	}
	if c.ClientID == "" {
		c.ClientID = ClientID
	}
	if c.UserAgent == "" {
		c.UserAgent = "shimmer-llmgateway/oauth"
	}
	return c
}

// GenerateVerifier returns a cryptographically random RFC 7636 code verifier:
// 43 characters drawn uniformly from the unreserved alphabet
// [A-Za-z0-9-._~] (rejection sampling, no modulo bias).
func GenerateVerifier() (string, error) {
	out := make([]byte, verifierLength)
	if err := randomAlphabet(out, verifierChars); err != nil {
		return "", err
	}
	return string(out), nil
}

// ValidateVerifier reports whether v is a syntactically valid RFC 7636 code
// verifier: 43-128 unreserved characters.
func ValidateVerifier(v string) error {
	if len(v) < 43 || len(v) > 128 {
		return errf("pkce", ErrInvalidVerifier, "verifier must be 43-128 characters")
	}
	for i := 0; i < len(v); i++ {
		if !strings.ContainsRune(verifierChars, rune(v[i])) {
			return errf("pkce", ErrInvalidVerifier, "verifier contains a character outside the RFC 7636 alphabet")
		}
	}
	return nil
}

// S256Challenge returns the S256 PKCE code challenge for verifier: the
// base64url-encoded (unpadded) SHA-256 digest. It rejects syntactically
// invalid verifiers.
func S256Challenge(verifier string) (string, error) {
	if err := ValidateVerifier(verifier); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// PKCEMatches reports whether challenge equals the S256 challenge of
// verifier, compared in constant time. A syntactically invalid verifier or
// challenge never matches.
func PKCEMatches(verifier, challenge string) bool {
	if ValidateVerifier(verifier) != nil || challenge == "" {
		return false
	}
	computed, err := S256Challenge(verifier)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// randomAlphabet fills out with n uniformly random characters from alphabet,
// using rejection sampling on crypto/rand bytes.
func randomAlphabet(out []byte, alphabet string) error {
	if len(alphabet) == 0 || len(alphabet) > 256 {
		return errors.New("invalid alphabet")
	}
	// Largest multiple of len(alphabet) that fits in a byte; bytes at or
	// above it are rejected to keep the sampling uniform.
	max := 256 - (256 % len(alphabet))
	buf := make([]byte, len(out))
	for {
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		ok := true
		for i, b := range buf {
			if int(b) >= max {
				ok = false
				break
			}
			out[i] = alphabet[int(b)%len(alphabet)]
		}
		if ok {
			return nil
		}
	}
}
