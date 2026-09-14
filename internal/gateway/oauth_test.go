package gateway

// Phase 2a tests: the OAuth credential resolver (refresh, singleflight,
// redaction, persistence) and the ChatGPT OAuth upstream routing (verified
// OpenCode-compatible URL/headers/account identity, client override
// rejection, API-key routing untouched).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
)

// testJWT builds an unsigned JWT with the given claims (same shape the oauth
// package tests use; no signature is needed for claim extraction).
func testJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc(header) + "." + enc(body) + ".sig"
}

// openTestSecrets opens a throwaway secrets store in a temp dir.
func openTestSecrets(t *testing.T) *secrets.Store {
	t.Helper()
	s, err := secrets.Open(t.TempDir()+"/gateway.db.secrets.json", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	return s
}

// freshOAuthCred returns a credential with a JWT access token carrying the
// verified account and residency claims.
func freshOAuthCred() secrets.OAuthCredential {
	return secrets.OAuthCredential{
		AccessToken:  testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc_123", "chatgpt_compute_residency": "us-east-1"}}),
		RefreshToken: "rt-original",
		ExpiresAt:    time.Now().Add(time.Hour),
		AccountID:    "acc_123",
	}
}

// tokenEndpoint is a fake issuer token endpoint recording requests.
type tokenEndpoint struct {
	srv *httptest.Server
	// status/body are the canned response; calls counts refresh invocations.
	status int
	body   string
	calls  atomic.Int64
	mu     sync.Mutex
	forms  []string
}

func newTokenEndpoint(t *testing.T, status int, body string) *tokenEndpoint {
	te := &tokenEndpoint{status: status, body: body}
	te.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		te.calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		te.mu.Lock()
		te.forms = append(te.forms, string(raw))
		te.mu.Unlock()
		w.WriteHeader(te.status)
		fmt.Fprint(w, te.body)
	}))
	t.Cleanup(te.srv.Close)
	return te
}

func (te *tokenEndpoint) formCount() int {
	te.mu.Lock()
	defer te.mu.Unlock()
	return len(te.forms)
}

func (te *tokenEndpoint) lastForm() string {
	te.mu.Lock()
	defer te.mu.Unlock()
	if len(te.forms) == 0 {
		return ""
	}
	return te.forms[len(te.forms)-1]
}

// newTestResolver builds a resolver pointed at the fake token endpoint.
func newTestResolver(t *testing.T, sec *secrets.Store, te *tokenEndpoint) *OAuthResolver {
	return NewOAuthResolver(sec, te.srv.Client(), oauth.Config{Issuer: te.srv.URL})
}

// ---- resolver unit tests ----

func TestOAuthResolverFreshCredentialNoRefresh(t *testing.T) {
	sec := openTestSecrets(t)
	if err := sec.SetOAuth("chatgpt", freshOAuthCred()); err != nil {
		t.Fatal(err)
	}
	te := newTokenEndpoint(t, http.StatusOK, `{}`)
	r := newTestResolver(t, sec, te)

	cred, err := r.Credential(context.Background(), "chatgpt")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.AccessToken != freshOAuthCred().AccessToken {
		t.Error("fresh credential was not returned unchanged")
	}
	if te.calls.Load() != 0 {
		t.Errorf("token endpoint called %d times for a fresh credential, want 0", te.calls.Load())
	}
}

func TestOAuthResolverMissingCredential(t *testing.T) {
	sec := openTestSecrets(t)
	te := newTokenEndpoint(t, http.StatusOK, `{}`)
	r := newTestResolver(t, sec, te)
	if _, err := r.Credential(context.Background(), "chatgpt"); !errors.Is(err, errOAuthNotConfigured) {
		t.Errorf("Credential = %v, want errOAuthNotConfigured", err)
	}
}

func TestOAuthResolverCorruptedRecord(t *testing.T) {
	sec := openTestSecrets(t)
	if err := sec.Set("chatgpt", "oauth:v1:{not json"); err != nil {
		t.Fatal(err)
	}
	r := NewOAuthResolver(sec, http.DefaultClient, oauth.Config{})
	if _, err := r.Credential(context.Background(), "chatgpt"); err == nil {
		t.Error("Credential of corrupted record = nil error, want error")
	}
}

func TestOAuthResolverRefreshPersistsRotatedTokens(t *testing.T) {
	sec := openTestSecrets(t)
	old := freshOAuthCred()
	old.ExpiresAt = time.Now().Add(-time.Minute) // expired
	if err := sec.SetOAuth("chatgpt", old); err != nil {
		t.Fatal(err)
	}
	newAccess := testJWT(map[string]any{"chatgpt_account_id": "acc_123"})
	te := newTokenEndpoint(t, http.StatusOK, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-rotated","expires_in":1800}`, newAccess))
	r := newTestResolver(t, sec, te)

	cred, err := r.Credential(context.Background(), "chatgpt")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.AccessToken != newAccess {
		t.Errorf("access token = %q, want the refreshed one", cred.AccessToken)
	}
	if cred.RefreshToken != "rt-rotated" {
		t.Errorf("refresh token = %q, want rt-rotated", cred.RefreshToken)
	}
	if cred.AccountID != "acc_123" {
		t.Errorf("account id = %q, want acc_123 preserved from the previous credential", cred.AccountID)
	}
	wantExpiry := time.Now().Add(1800 * time.Second)
	if diff := cred.ExpiresAt.Sub(wantExpiry); diff < -time.Second || diff > time.Second {
		t.Errorf("expires_at = %v, want ~%v", cred.ExpiresAt, wantExpiry)
	}
	// The rotated record was persisted atomically: a reload sees it.
	got, ok, err := sec.GetOAuth("chatgpt")
	if err != nil || !ok {
		t.Fatalf("reloaded GetOAuth = (%v, %v)", ok, err)
	}
	if got.AccessToken != newAccess || got.RefreshToken != "rt-rotated" {
		t.Errorf("reloaded record = %+v, want the rotated tokens", got)
	}
	// The refresh request used the verified form shape (grant_type +
	// refresh_token + client_id).
	form := te.lastForm()
	if !strings.Contains(form, "grant_type=refresh_token") || !strings.Contains(form, "refresh_token=rt-original") || !strings.Contains(form, "client_id=") {
		t.Errorf("refresh form = %q, want the verified grant shape", form)
	}
}

func TestOAuthResolverRefreshKeepsOldRefreshTokenWhenOmitted(t *testing.T) {
	sec := openTestSecrets(t)
	old := freshOAuthCred()
	old.ExpiresAt = time.Now().Add(-time.Minute)
	if err := sec.SetOAuth("chatgpt", old); err != nil {
		t.Fatal(err)
	}
	te := newTokenEndpoint(t, http.StatusOK, fmt.Sprintf(`{"access_token":%q,"expires_in":3600}`, testJWT(map[string]any{"sub": "x"})))
	r := newTestResolver(t, sec, te)

	cred, err := r.Credential(context.Background(), "chatgpt")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if cred.RefreshToken != "rt-original" {
		t.Errorf("refresh token = %q, want the stored one kept when the provider omits it", cred.RefreshToken)
	}
	if cred.AccountID != "acc_123" {
		t.Errorf("account id = %q, want the stored one kept when the refreshed tokens carry no claims", cred.AccountID)
	}
}

func TestOAuthResolverConcurrentRefreshSingleflight(t *testing.T) {
	sec := openTestSecrets(t)
	old := freshOAuthCred()
	old.ExpiresAt = time.Now().Add(-time.Minute)
	if err := sec.SetOAuth("chatgpt", old); err != nil {
		t.Fatal(err)
	}
	te := newTokenEndpoint(t, http.StatusOK, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-rotated","expires_in":3600}`, testJWT(map[string]any{"chatgpt_account_id": "acc_123"})))
	r := newTestResolver(t, sec, te)

	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.Credential(context.Background(), "chatgpt")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if te.calls.Load() != 1 {
		t.Errorf("token endpoint called %d times for 12 concurrent callers, want 1", te.calls.Load())
	}
}

func TestOAuthResolverRefreshCannotResurrectAfterDisconnect(t *testing.T) {
	sec := openTestSecrets(t)
	old := freshOAuthCred()
	old.ExpiresAt = time.Now().Add(-time.Minute)
	if err := sec.SetOAuth("chatgpt", old); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-stale","expires_in":3600}`, testJWT(map[string]any{"chatgpt_account_id": "acc_123"})))
	}))
	t.Cleanup(tokenSrv.Close)
	life := oauth.NewLifecycle()
	r := NewOAuthResolver(sec, tokenSrv.Client(), oauth.Config{Issuer: tokenSrv.URL}, life)
	result := make(chan error, 1)
	go func() {
		_, err := r.Credential(context.Background(), "chatgpt")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach the token endpoint")
	}
	life.Begin("chatgpt")
	if err := sec.Delete("chatgpt"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, oauth.ErrOperationStale) {
		t.Fatalf("refresh error = %v, want oauth.ErrOperationStale", err)
	}
	if _, ok, err := sec.GetOAuth("chatgpt"); err != nil || ok {
		t.Errorf("credential after stale refresh = (%v, %v), want absent", ok, err)
	}
}

func TestOAuthResolverRefreshFailureRedactedAndUnchanged(t *testing.T) {
	sec := openTestSecrets(t)
	old := freshOAuthCred()
	old.ExpiresAt = time.Now().Add(-time.Minute)
	if err := sec.SetOAuth("chatgpt", old); err != nil {
		t.Fatal(err)
	}
	// The provider error_description must never reach the caller.
	te := newTokenEndpoint(t, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"super-secret-body"}`)
	r := newTestResolver(t, sec, te)

	_, err := r.Credential(context.Background(), "chatgpt")
	if err == nil {
		t.Fatal("Credential = nil error, want refresh failure")
	}
	if !errors.Is(err, oauth.ErrTokenEndpoint) {
		t.Errorf("error = %v, want oauth.ErrTokenEndpoint", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "super-secret-body") {
		t.Errorf("refresh error leaked the provider body: %q", msg)
	}
	if strings.Contains(msg, old.RefreshToken) || strings.Contains(msg, old.AccessToken) {
		t.Errorf("refresh error leaked token material: %q", msg)
	}
	// The stored credential was not touched by the failed refresh.
	got, ok, err := sec.GetOAuth("chatgpt")
	if err != nil || !ok {
		t.Fatalf("GetOAuth after failed refresh = (%v, %v)", ok, err)
	}
	if got.AccessToken != old.AccessToken || got.RefreshToken != old.RefreshToken {
		t.Errorf("failed refresh mutated the stored credential: %+v", got)
	}
}

// ---- buildUpstream routing ----

// chatgptOAuthTOML builds a gateway.toml with a chatgpt instance. The
// [providers.chatgpt] table overrides the built-in template wholesale (config
// merge semantics), so the OAuth-relevant fields are redeclared exactly as the
// built-in defines them, including the fixed endpoint.
func chatgptOAuthTOML(_ string) string {
	return fmt.Sprintf(`
[providers.chatgpt]
base_url = %q
style = "responses"
oauth = true
session_header = "session-id"
models = ["gpt-5.3-codex"]

[[instances]]
alias = "chatgpt"
template = "chatgpt"
`, config.ChatGPTCodexBaseURL)
}

func TestOAuthBuildUpstreamHeaders(t *testing.T) {
	provider := newFakeProvider(t, nil)
	srv, _ := newGatewayServer(t, provider, chatgptOAuthTOML(provider.url()), nil, io.Discard)
	if err := srv.secrets.SetOAuth("chatgpt", freshOAuthCred()); err != nil {
		t.Fatal(err)
	}

	cfg := srv.cfg.Get()
	inst, model, err := cfg.Resolve("chatgpt/gpt-5.3-codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	rt := &route{inst: inst, template: cfg.Templates[inst.Template], model: model, alias: inst.Alias, provider: inst.Template}
	body := []byte(`{"model":"chatgpt/gpt-5.3-codex","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	inbound := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	// A hostile client tries to inject its own identity; it must never win.
	inbound.Header.Set("Authorization", "Bearer client-evil")
	inbound.Header.Set("ChatGPT-Account-Id", "client-evil-account")
	inbound.Header.Set("x-openai-internal-codex-residency", "client-evil-region")
	inbound.Header.Set("Originator", "client-evil-originator")
	inbound.Header.Set("User-Agent", "client-ua/1.0")

	req, sentBody, err := srv.buildUpstream(context.Background(), cfg, rt, body, "sess-oauth-1", inbound)
	if err != nil {
		t.Fatalf("buildUpstream: %v", err)
	}
	// The verified OpenCode upstream shape: the configured base URL plus the
	// responses-style endpoint (/responses), i.e.
	// https://chatgpt.com/backend-api/codex/responses in production (pinned
	// in the config test).
	if want := "/codex/responses"; !strings.HasSuffix(req.URL.String(), want) {
		t.Errorf("upstream URL = %q, want %q suffix", req.URL.String(), want)
	}
	// The stored credential identity, never the client's.
	if got := req.Header.Get("Authorization"); got != "Bearer "+freshOAuthCred().AccessToken {
		t.Errorf("Authorization = %q, want the stored bearer token", got)
	}
	if got := req.Header.Get("ChatGPT-Account-Id"); got != "acc_123" {
		t.Errorf("ChatGPT-Account-Id = %q, want acc_123 from the credential", got)
	}
	if got := req.Header.Get("x-openai-internal-codex-residency"); got != "us-east-1" {
		t.Errorf("x-openai-internal-codex-residency = %q, want us-east-1 from the token claims", got)
	}
	if got := req.Header.Get("originator"); got != oauth.Originator {
		t.Errorf("originator = %q, want %q", got, oauth.Originator)
	}
	// The per-conversation session header rides upstream (verified OpenCode
	// chat.headers contract).
	if got := req.Header.Get("session-id"); got != "sess-oauth-1" {
		t.Errorf("session-id = %q, want sess-oauth-1", got)
	}
	// The client UA is still forwarded faithfully (unchanged gateway behavior).
	if got := req.Header.Get("User-Agent"); got != "client-ua/1.0" {
		t.Errorf("User-Agent = %q, want client-ua/1.0", got)
	}
	// The Responses request carries the resolved model name.
	var up struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(sentBody, &up); err != nil {
		t.Fatalf("sent body: %v", err)
	}
	if up.Model != "gpt-5.3-codex" {
		t.Errorf("upstream model = %q, want gpt-5.3-codex", up.Model)
	}
}

func TestOAuthMissingCredentialIsConfigError(t *testing.T) {
	provider := newFakeProvider(t, nil)
	gs, _ := newGatewayTest(t, provider, chatgptOAuthTOML(provider.url()), nil)

	resp := postChat(t, gs, `{"model":"chatgpt/gpt-5.3-codex","messages":[{"role":"user","content":"hi"}]}`, nil)
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "oauth credential not set") {
		t.Errorf("body = %q, want the missing-credential message", body)
	}
}

// TestOAuthChatEndToEnd covers the full non-stream path: expired credential
// refresh through the fake token endpoint, then the upstream call with the
// verified URL/headers/body, translated back to the chat shape for the client.
func TestOAuthChatEndToEnd(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/codex/responses") {
			provider.t.Errorf("upstream path = %q, want /codex/responses suffix", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.3-codex","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"oauth-hi","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`))
	})
	srv, st := newGatewayServer(t, provider, chatgptOAuthTOML(provider.url()), nil, io.Discard)
	old := freshOAuthCred()
	old.ExpiresAt = time.Now().Add(-time.Minute)
	if err := srv.secrets.SetOAuth("chatgpt", old); err != nil {
		t.Fatal(err)
	}
	te := newTokenEndpoint(t, http.StatusOK, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-rotated","expires_in":3600}`, testJWT(map[string]any{"chatgpt_account_id": "acc_123"})))
	srv.oauth = newTestResolver(t, srv.secrets, te)
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	resp := postChat(t, gs, `{"model":"chatgpt/gpt-5.3-codex","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
		"X-Session-Id":       "sess-oauth-e2e",
		"Authorization":      "Bearer client-evil",
		"ChatGPT-Account-Id": "client-evil",
	})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"oauth-hi"`) {
		t.Errorf("client body missing the translated completion: %q", body)
	}
	if te.calls.Load() != 1 {
		t.Errorf("token endpoint called %d times, want 1", te.calls.Load())
	}
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	pr := seen[0]
	// The stored credential identity won over the client's attempted override.
	if pr.Authorization == "Bearer client-evil" || pr.Authorization == "" {
		t.Errorf("upstream Authorization = %q, want the stored bearer token", pr.Authorization)
	}
	if pr.Originator != oauth.Originator {
		t.Errorf("upstream originator = %q, want %q", pr.Originator, oauth.Originator)
	}
	var up struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(pr.Body, &up); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if up.Model != "gpt-5.3-codex" {
		t.Errorf("upstream model = %q, want gpt-5.3-codex", up.Model)
	}
	// Capture stays chat-shaped and records the request.
	req := waitForRequest(t, st, "sess-oauth-e2e", 5*time.Second)
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("capture response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
}

func TestOAuthStreamEndToEnd(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, line := range []string{
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1722600000,"status":"in_progress","model":"gpt-5.3-codex"}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`,
			`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"stream-hi"}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1722600000,"status":"completed","model":"gpt-5.3-codex","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
		} {
			fmt.Fprintf(w, "%s\n\n", line)
			fl.Flush()
		}
	})
	srv, st := newGatewayServer(t, provider, chatgptOAuthTOML(provider.url()), nil, io.Discard)
	if err := srv.secrets.SetOAuth("chatgpt", freshOAuthCred()); err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	resp := postChat(t, gs, `{"model":"chatgpt/gpt-5.3-codex","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-oauth-stream"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "stream-hi") || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Errorf("client stream = %q, want the translated chunk and [DONE]", body)
	}
	req := waitForRequest(t, st, "sess-oauth-stream", 5*time.Second)
	if req.FinishReason != "stop" || req.Error != nil {
		t.Errorf("capture finish/error = %q/%+v, want stop/nil", req.FinishReason, req.Error)
	}
}

func TestOAuthRefreshFailureViaHTTPIsSanitized(t *testing.T) {
	provider := newFakeProvider(t, nil)
	srv, _ := newGatewayServer(t, provider, chatgptOAuthTOML(provider.url()), nil, io.Discard)
	old := freshOAuthCred()
	old.ExpiresAt = time.Now().Add(-time.Minute)
	if err := srv.secrets.SetOAuth("chatgpt", old); err != nil {
		t.Fatal(err)
	}
	te := newTokenEndpoint(t, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"super-secret-body"}`)
	srv.oauth = newTestResolver(t, srv.secrets, te)
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	resp := postChat(t, gs, `{"model":"chatgpt/gpt-5.3-codex","messages":[{"role":"user","content":"hi"}]}`, nil)
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", resp.StatusCode, body)
	}
	text := string(body)
	if strings.Contains(text, "super-secret-body") || strings.Contains(text, old.AccessToken) || strings.Contains(text, old.RefreshToken) {
		t.Errorf("client-visible error leaked provider/token material: %q", text)
	}
	if !strings.Contains(text, "invalid_grant") {
		t.Errorf("body = %q, want the fixed provider error code", text)
	}
	// The failed refresh left the stored credential untouched.
	got, ok, err := srv.secrets.GetOAuth("chatgpt")
	if err != nil || !ok {
		t.Fatalf("GetOAuth after failed refresh = (%v, %v)", ok, err)
	}
	if got.RefreshToken != old.RefreshToken {
		t.Errorf("failed refresh rotated the stored credential: %+v", got)
	}
}

// TestOAuthAPIKeyRoutingUnchanged asserts an OAuth-marked template never
// touches the API-key path while ordinary keyed instances keep the exact
// previous behavior (env-var precedence and stored-key fallback).
func TestOAuthAPIKeyRoutingUnchanged(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"key-hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	srv, _ := newGatewayServer(t, provider, `
[providers.chatgpt]
base_url = "https://chatgpt.com/backend-api/codex"
style = "responses"
oauth = true
session_header = "session-id"
models = ["gpt-5.3-codex"]

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"

[[instances]]
alias = "chatgpt"
template = "chatgpt"
`, map[string]string{"TEST_KEY_1": "sk-account-1"}, io.Discard)
	// The chatgpt instance carries a stored credential AND the openai instance
	// a stored key; each must use its own auth path.
	if err := srv.secrets.SetOAuth("chatgpt", freshOAuthCred()); err != nil {
		t.Fatal(err)
	}
	if err := srv.secrets.Set("openai", "sk-stored-fallback"); err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	// Keyed instance: env var wins (unchanged §6.2 precedence).
	resp := postChat(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	if body := drainClose(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("openai status = %d (%s)", resp.StatusCode, body)
	}
	seen := provider.requests()
	if len(seen) != 1 || seen[0].Authorization != "Bearer sk-account-1" {
		t.Fatalf("openai upstream auth = %q, want Bearer sk-account-1", seen[0].Authorization)
	}
	// The chatgpt instance's stored credential went through the OAuth branch,
	// not the key path.
	resp = postChat(t, gs, `{"model":"chatgpt/gpt-5.3-codex","messages":[{"role":"user","content":"hi"}]}`, nil)
	if body := drainClose(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("chatgpt status = %d (%s)", resp.StatusCode, body)
	}
	seen = provider.requests()
	if len(seen) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(seen))
	}
	if seen[1].Authorization == "Bearer sk-account-1" {
		t.Error("chatgpt upstream reused the API-key auth path")
	}

	// The instance list never masks an OAuth record as an API key: the
	// chatgpt instance shows no key_masked while the keyed instance does.
	_, out := apiDo(t, gs, "GET", "/api/instances", "")
	insts, ok := out["instances"].([]any)
	if !ok || len(insts) != 2 {
		t.Fatalf("instances = %v, want 2 entries", out["instances"])
	}
	byAlias := map[string]map[string]any{}
	for _, raw := range insts {
		m := raw.(map[string]any)
		byAlias[m["alias"].(string)] = m
	}
	if km, ok := byAlias["chatgpt"]["key_masked"]; ok {
		t.Errorf("chatgpt key_masked = %v, want absent (OAuth records are not keys)", km)
	}
	if km := byAlias["openai"]["key_masked"]; km != "sk-…back" {
		t.Errorf("openai key_masked = %v, want sk-…back (stored key still masked)", km)
	}
}
