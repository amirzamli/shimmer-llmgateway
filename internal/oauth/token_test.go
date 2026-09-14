package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// makeJWT builds an unsigned JWT with the given claims payload.
func makeJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc(header) + "." + enc(body) + ".sig"
}

// tokenBody builds a token endpoint response body.
func tokenBody(overrides map[string]any) string {
	body := map[string]any{
		"access_token":  "at-" + strings.Repeat("secret", 4),
		"refresh_token": "rt-secret",
		"id_token":      makeJWT(map[string]any{"chatgpt_account_id": "acc_123"}),
		"expires_in":    900,
	}
	for k, v := range overrides {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func TestExchangeSuccess(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != TokenPath {
			t.Errorf("path = %s, want %s", r.URL.Path, TokenPath)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", ct)
		}
		if ua := r.Header.Get("User-Agent"); ua != "shimmer-llmgateway/oauth" {
			t.Errorf("User-Agent = %q", ua)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		gotForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(tokenBody(nil)))
	}))
	defer srv.Close()

	verifier, err := GenerateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Issuer: srv.URL}
	tok, err := cfg.Exchange(context.Background(), srv.Client(), "the-auth-code", verifier)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	wantForm := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-auth-code",
		"redirect_uri":  srv.URL + DeviceRedirectPath,
		"client_id":     ClientID,
		"code_verifier": verifier,
	}
	for k, v := range wantForm {
		if got := gotForm.Get(k); got != v {
			t.Errorf("form %s = %q, want %q", k, got, v)
		}
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.IDToken == "" {
		t.Errorf("token fields empty: %+v", tok)
	}
	if tok.AccountID != "acc_123" {
		t.Errorf("AccountID = %q, want acc_123", tok.AccountID)
	}
	if tok.Expired(time.Now().Add(30*time.Second), 0) {
		t.Error("fresh token (expires_in=900) reports expired at +30s")
	}
	if !tok.Expired(time.Now().Add(20*time.Minute), 0) {
		t.Error("token does not report expiry after expires_in")
	}
}

func TestExchangeUsesDefaultClientWhenNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tokenBody(nil)))
	}))
	defer srv.Close()

	verifier, _ := GenerateVerifier()
	// Default client must reach the test server (it does not verify TLS for
	// plain HTTP), so this exercises the nil-client fallback path.
	tok, err := (Config{Issuer: srv.URL}).Exchange(context.Background(), nil, "code", verifier)
	if err != nil {
		t.Fatalf("Exchange with nil client: %v", err)
	}
	if tok.AccessToken == "" {
		t.Error("nil-client exchange returned no access token")
	}
}

func TestExchangeValidatesVerifier(t *testing.T) {
	cfg := Config{Issuer: "https://unused.test"}
	_, err := cfg.Exchange(context.Background(), nil, "code", "not-a-verifier")
	if err == nil {
		t.Fatal("Exchange with invalid verifier: want error")
	}
	if !strings.Contains(err.Error(), "verifier") {
		t.Errorf("error does not mention verifier: %v", err)
	}
	_, err = cfg.Exchange(context.Background(), nil, "", "x")
	if err == nil {
		t.Fatal("Exchange with empty code: want error")
	}
}

func TestExchangeHTTPErrorIsRedacted(t *testing.T) {
	const code = "auth-code-XYZ-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"sensitive material that must never surface"}`))
	}))
	defer srv.Close()

	verifier, _ := GenerateVerifier()
	_, err := (Config{Issuer: srv.URL}).Exchange(context.Background(), srv.Client(), code, verifier)
	if err == nil {
		t.Fatal("Exchange over 400 response: want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "400") {
		t.Errorf("error lacks HTTP status: %v", err)
	}
	if !strings.Contains(msg, "invalid_grant") {
		t.Errorf("error lacks provider error code: %v", err)
	}
	if strings.Contains(msg, "sensitive") {
		t.Errorf("error leaked error_description: %v", err)
	}
	if strings.Contains(msg, code) {
		t.Errorf("error leaked the authorization code: %v", err)
	}
}

func TestExchangeUnknownProviderErrorCodeIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"access-token-was-embedded-here","error_description":"ignored"}`))
	}))
	defer srv.Close()

	verifier, _ := GenerateVerifier()
	_, err := (Config{Issuer: srv.URL}).Exchange(context.Background(), srv.Client(), "code", verifier)
	if err == nil {
		t.Fatal("Exchange over 400 response: want error")
	}
	if strings.Contains(err.Error(), "access-token-was-embedded-here") {
		t.Errorf("error leaked unknown provider error code: %v", err)
	}
}

func TestExchangeNonJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error with possibly sensitive stack trace"))
	}))
	defer srv.Close()

	verifier, _ := GenerateVerifier()
	_, err := (Config{Issuer: srv.URL}).Exchange(context.Background(), srv.Client(), "code", verifier)
	if err == nil {
		t.Fatal("Exchange over 500 response: want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "stack trace") || strings.Contains(msg, "internal server error") {
		t.Errorf("error leaked response body: %v", err)
	}
	if !strings.Contains(msg, "500") {
		t.Errorf("error lacks HTTP status: %v", err)
	}
}

func TestExchangeMalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":`)) // truncated JSON
	}))
	defer srv.Close()

	verifier, _ := GenerateVerifier()
	_, err := (Config{Issuer: srv.URL}).Exchange(context.Background(), srv.Client(), "code", verifier)
	if err == nil {
		t.Fatal("Exchange with malformed body: want error")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExchangeMissingAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"refresh_token":"rt"}`))
	}))
	defer srv.Close()

	verifier, _ := GenerateVerifier()
	_, err := (Config{Issuer: srv.URL}).Exchange(context.Background(), srv.Client(), "code", verifier)
	if err == nil {
		t.Fatal("Exchange without access_token: want error")
	}
	if !strings.Contains(err.Error(), "access token") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseTokenResponseDefaults(t *testing.T) {
	// expires_in omitted -> DefaultExpiresIn; no id_token -> account id from
	// access token; no refresh_token tolerated.
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	body := tokenBody(map[string]any{
		"expires_in":    nil,
		"id_token":      nil,
		"refresh_token": nil,
		"access_token": makeJWT(map[string]any{
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc_from_at"},
		}),
	})
	tok, err := parseTokenResponse([]byte(body), now)
	if err != nil {
		t.Fatalf("parseTokenResponse: %v", err)
	}
	if !tok.ExpiresAt.Equal(now.Add(DefaultExpiresIn * time.Second)) {
		t.Errorf("ExpiresAt = %v, want now+%ds", tok.ExpiresAt, DefaultExpiresIn)
	}
	if tok.AccountID != "acc_from_at" {
		t.Errorf("AccountID = %q, want acc_from_at (from access token)", tok.AccountID)
	}
	if tok.RefreshToken != "" {
		t.Errorf("RefreshToken = %q, want empty", tok.RefreshToken)
	}
}

func TestParseTokenResponseExpiresInZero(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	tok, err := parseTokenResponse([]byte(tokenBody(map[string]any{"expires_in": 0})), now)
	if err != nil {
		t.Fatalf("parseTokenResponse: %v", err)
	}
	// expires_in=0 means "expires now" (verified OpenCode honors the raw
	// value): the token is expired one instant later.
	if !tok.ExpiresAt.Equal(now) {
		t.Errorf("ExpiresAt = %v, want now", tok.ExpiresAt)
	}
	if !tok.Expired(now.Add(time.Second), 0) {
		t.Error("expires_in=0 token must be expired immediately")
	}
}

func TestTokenExpiredSkew(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	tok, err := parseTokenResponse([]byte(tokenBody(map[string]any{"expires_in": 60})), now)
	if err != nil {
		t.Fatal(err)
	}
	// 30s before expiry: not expired without skew, expired with 31s skew.
	at := now.Add(30 * time.Second)
	if tok.Expired(at, 0) {
		t.Error("expired 30s before expiry without skew")
	}
	if !tok.Expired(at, 31*time.Second) {
		t.Error("not expired with 31s skew 30s before expiry")
	}
}

func TestRefreshSuccess(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TokenPath {
			t.Errorf("path = %s, want %s", r.URL.Path, TokenPath)
		}
		_ = r.ParseForm()
		gotForm = r.Form
		w.Write([]byte(tokenBody(nil)))
	}))
	defer srv.Close()

	tok, err := (Config{Issuer: srv.URL}).Refresh(context.Background(), srv.Client(), "rt-rotate-me")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotForm.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", gotForm.Get("grant_type"))
	}
	if gotForm.Get("refresh_token") != "rt-rotate-me" {
		t.Errorf("refresh_token = %q", gotForm.Get("refresh_token"))
	}
	if gotForm.Get("client_id") != ClientID {
		t.Errorf("client_id = %q", gotForm.Get("client_id"))
	}
	if got := gotForm.Get("redirect_uri"); got != "" {
		t.Errorf("refresh must not send redirect_uri, got %q", got)
	}
	if tok.AccessToken == "" {
		t.Error("Refresh returned no access token")
	}
}

func TestRefreshEmptyToken(t *testing.T) {
	_, err := (Config{}).Refresh(context.Background(), nil, "")
	if err == nil {
		t.Fatal("Refresh with empty refresh token: want error")
	}
}

func TestRefreshHTTPErrorRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_token","error_description":"refresh token was revoked: abc123"}`))
	}))
	defer srv.Close()

	_, err := (Config{Issuer: srv.URL}).Refresh(context.Background(), srv.Client(), "rt-secret")
	if err == nil {
		t.Fatal("Refresh over 401: want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "abc123") || strings.Contains(msg, "revoked") {
		t.Errorf("error leaked error_description: %v", err)
	}
	if !strings.Contains(msg, "invalid_token") || !strings.Contains(msg, "401") {
		t.Errorf("error lacks status/code: %v", err)
	}
}
