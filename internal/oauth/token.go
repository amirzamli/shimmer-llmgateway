package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Token is a parsed token response from the OpenAI token endpoint. None of
// the fields are ever rendered by the package's error paths; Token does not
// implement fmt.Stringer on purpose.
type Token struct {
	// AccessToken is the bearer credential used to call the model API.
	AccessToken string
	// RefreshToken rotates the access token; may be empty if the provider
	// did not issue one despite offline_access.
	RefreshToken string
	// IDToken is the OpenID Connect identity token, when present.
	IDToken string
	// TokenType is the provider's token_type field, when present (typically
	// "Bearer").
	TokenType string
	// Scope is the provider's echoed scope field, when present.
	Scope string
	// ExpiresAt is the access token expiry computed from expires_in
	// (defaulting to DefaultExpiresIn seconds, matching OpenCode).
	ExpiresAt time.Time
	// AccountID is the ChatGPT account identifier extracted from the
	// id_token, falling back to the access token, using the verified
	// OpenCode claim order. Empty when no claim is present.
	AccountID string
}

// Expired reports whether the access token has expired at now, applying
// skew seconds of leeway (positive skew expires the token earlier).
func (t *Token) Expired(now time.Time, skew time.Duration) bool {
	return !t.ExpiresAt.IsZero() && now.Add(skew).After(t.ExpiresAt)
}

// tokenResponse mirrors the verified OpenCode v1.18.30 TokenResponse fields
// (id_token, access_token, refresh_token, expires_in) plus the optional
// token_type/scope passthrough fields.
type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    *int64 `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// providerError mirrors the token endpoint error envelope. Only the fixed
// error code is surfaced; error_description is never rendered.
type providerError struct {
	Code string `json:"error"`
}

// Exchange performs the authorization-code exchange at the token endpoint
// (grant_type=authorization_code) using the verified OpenCode v1.18.30
// request shape: form-encoded code, redirect_uri, client_id, and the PKCE
// code_verifier. client may be nil to use http.DefaultClient.
func (c Config) Exchange(ctx context.Context, client *http.Client, code, verifier string) (*Token, error) {
	c = c.WithDefaults()
	if code == "" {
		return nil, errf("token.exchange", ErrInvalidTokenResponse, "authorization code is required")
	}
	if err := ValidateVerifier(verifier); err != nil {
		return nil, errf("token.exchange", ErrInvalidVerifier, "valid PKCE verifier is required")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.RedirectURI)
	form.Set("client_id", c.ClientID)
	form.Set("code_verifier", verifier)
	return c.tokenRequest(ctx, client, form, "token.exchange")
}

// Refresh rotates a refresh token at the token endpoint
// (grant_type=refresh_token) using the verified OpenCode v1.18.30 request
// shape: form-encoded refresh_token and client_id. client may be nil to use
// http.DefaultClient.
func (c Config) Refresh(ctx context.Context, client *http.Client, refreshToken string) (*Token, error) {
	c = c.WithDefaults()
	if refreshToken == "" {
		return nil, errf("token.refresh", ErrInvalidTokenResponse, "refresh token is required")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", c.ClientID)
	return c.tokenRequest(ctx, client, form, "token.refresh")
}

// tokenRequest posts form to the token endpoint and parses the response.
// The response body never reaches any error message: HTTP failures surface
// only the status (and a fixed provider error code when present), and parse
// failures surface a fixed message.
func (c Config) tokenRequest(ctx context.Context, client *http.Client, form url.Values, op string) (*Token, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Issuer+TokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errf(op, ErrTokenEndpoint, "failed to build token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, &Error{Op: op, Msg: "token endpoint request failed", Err: ErrTokenEndpoint}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		code := providerErrorCode(resp.Body)
		return nil, tokenEndpointError(resp.StatusCode, code)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errf(op, ErrInvalidTokenResponse, "failed to read token response")
	}
	return parseTokenResponse(body, time.Now())
}

// providerErrorCode extracts the fixed error code from a provider error
// body, ignoring error_description entirely. Malformed bodies yield "".
func providerErrorCode(r io.Reader) string {
	var pe providerError
	if err := json.NewDecoder(r).Decode(&pe); err != nil {
		return ""
	}
	return pe.Code
}

// parseTokenResponse parses a token endpoint body. now fixes the clock so
// ExpiresAt is deterministic (tests inject it; production callers use
// time.Now()). ExpiresAt = now + expires_in, defaulting a missing expires_in
// to DefaultExpiresIn and otherwise honoring the raw value exactly (verified
// OpenCode behavior: expires_in ?? 3600 — so 0 means "expires now"). A
// response without an access token is an error; a missing refresh_token is
// tolerated (the provider may rotate or omit it).
func parseTokenResponse(body []byte, now time.Time) (*Token, error) {
	var raw tokenResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errf("token.parse", ErrInvalidTokenResponse, "token response is not valid JSON")
	}
	if raw.AccessToken == "" {
		return nil, errf("token.parse", ErrInvalidTokenResponse, "token response lacks an access token")
	}
	expiresIn := int64(DefaultExpiresIn)
	if raw.ExpiresIn != nil {
		expiresIn = *raw.ExpiresIn
	}
	t := &Token{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		IDToken:      raw.IDToken,
		TokenType:    raw.TokenType,
		Scope:        raw.Scope,
		ExpiresAt:    now.Add(time.Duration(expiresIn) * time.Second),
	}
	t.AccountID = extractAccountID(raw)
	return t, nil
}

// extractAccountID applies the verified OpenCode v1.18.30 order: id_token
// first, then access_token. Returns "" when neither JWT carries a claim.
func extractAccountID(raw tokenResponse) string {
	if raw.IDToken != "" {
		if id, err := AccountIDFromJWT(raw.IDToken); err == nil {
			return id
		}
	}
	if raw.AccessToken != "" {
		if id, err := AccountIDFromJWT(raw.AccessToken); err == nil {
			return id
		}
	}
	return ""
}
