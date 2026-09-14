// Package oauth implements the ChatGPT Plus OAuth protocol used by OpenCode
// (the "add ChatGPT Plus account" browser flow) as an isolated protocol
// component: typed authorization state with S256 PKCE, authorization URL
// construction, authorization-code exchange and refresh requests, token
// response parsing with expiry, ChatGPT account-ID extraction from the
// OpenAI/Codex JWT claims, and redacted errors that never render tokens,
// codes, verifiers, or state values.
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
	"net/url"
	"strings"
	"time"
)

// Verified OpenCode v1.18.30 ChatGPT Plus OAuth constants
// (packages/core/src/plugin/provider/openai.ts in opencode 1.18.30).
const (
	// Issuer is the OAuth issuer / auth host.
	Issuer = "https://auth.openai.com"
	// ClientID is the public client identifier registered by OpenCode.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// RedirectURI is the loopback callback the browser flow redirects to.
	RedirectURI = "http://localhost:1455/auth/callback"
	// Scope is the space-separated scope set requested in the flow.
	Scope = "openid profile email offline_access"
	// AuthorizePath is the issuer-relative authorization endpoint.
	AuthorizePath = "/oauth/authorize"
	// TokenPath is the issuer-relative token endpoint.
	TokenPath = "/oauth/token"
	// CodeChallengeMethod is the PKCE transform used by the flow.
	CodeChallengeMethod = "S256"
	// DefaultExpiresIn is the token lifetime in seconds assumed when the
	// token response omits expires_in (matches OpenCode's ?? 3600).
	DefaultExpiresIn = 3600
	// originator is sent in the authorize request exactly as OpenCode sends
	// it; the value is part of the verified request contract.
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
	Issuer      string // default: Issuer
	ClientID    string // default: ClientID
	RedirectURI string // default: RedirectURI
	Scope       string // default: Scope
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
	if c.RedirectURI == "" {
		c.RedirectURI = RedirectURI
	}
	if c.Scope == "" {
		c.Scope = Scope
	}
	if c.UserAgent == "" {
		c.UserAgent = "shimmer-llmgateway/oauth"
	}
	return c
}

// State is a typed authorization transaction: the opaque state value sent to
// the provider, the PKCE verifier kept server-side, and the derived S256
// challenge. It binds the transaction to a gateway instance and is
// short-lived and single-use by design (enforced by StateStore).
type State struct {
	// Value is the random opaque state parameter sent to the provider.
	Value string
	// Verifier is the PKCE code verifier; it must never leave the server.
	Verifier string
	// Challenge is the S256 code challenge derived from Verifier, sent in
	// the authorization URL.
	Challenge string
	// InstanceID is the gateway instance this transaction is bound to.
	InstanceID string
	// Generation is the lifecycle generation active when the state was minted.
	// It is server-side metadata and is never sent to the provider.
	Generation uint64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	// Used marks a consumed single-use state.
	Used bool
}

// NewState generates a fresh state (random value and PKCE verifier/challenge)
// valid for ttl. ttl must be positive.
func NewState(instanceID string, ttl time.Duration) (*State, error) {
	return newStateAt(instanceID, ttl, time.Now())
}

func newStateAt(instanceID string, ttl time.Duration, now time.Time) (*State, error) {
	if ttl <= 0 {
		return nil, errf("state.generate", ErrInvalidState, "state ttl must be positive")
	}
	value, err := randomBase64URL(32)
	if err != nil {
		return nil, errf("state.generate", ErrInvalidState, "failed to generate state value")
	}
	verifier, err := GenerateVerifier()
	if err != nil {
		return nil, errf("state.generate", ErrInvalidState, "failed to generate PKCE verifier")
	}
	challenge, err := S256Challenge(verifier)
	if err != nil {
		return nil, errf("state.generate", ErrInvalidState, "failed to derive PKCE challenge")
	}
	return &State{
		Value:      value,
		Verifier:   verifier,
		Challenge:  challenge,
		InstanceID: instanceID,
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
	}, nil
}

// Expired reports whether the state has expired at now.
func (s *State) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
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

// randomBase64URL returns n cryptographically random bytes as an unpadded
// base64url string.
func randomBase64URL(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthorizeURL builds the provider authorization URL for state, replicating
// the verified OpenCode v1.18.30 request parameter for parameter
// (id_token_add_organizations, codex_cli_simplified_flow, and originator are
// part of the verified contract). state must be non-nil with Value and
// Challenge set.
func (c Config) AuthorizeURL(state *State) (string, error) {
	c = c.WithDefaults()
	if state == nil || state.Value == "" || state.Challenge == "" {
		return "", errf("authorize", ErrInvalidState, "state value and challenge are required")
	}
	// url.Values.Encode percent-encodes every value, matching OpenCode's
	// URLSearchParams serialization (notably the redirect URI).
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURI)
	q.Set("scope", c.Scope)
	q.Set("code_challenge", state.Challenge)
	q.Set("code_challenge_method", CodeChallengeMethod)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state.Value)
	q.Set("originator", Originator)
	return c.Issuer + AuthorizePath + "?" + q.Encode(), nil
}
