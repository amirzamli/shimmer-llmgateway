package api

// Phase 2b tests: the ChatGPT OAuth API surface — start (state/PKCE minting,
// instance gating, superseding), callback (state validation, replay/expiry
// rejection, code exchange, account identity, credential persistence,
// redaction), status (masking), and disconnect (lifecycle cleanup).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// chatgptOAuthTOML seeds one OAuth-marked chatgpt instance plus an ordinary
// keyed openai instance, mirroring the provider table shape the gateway tests
// use (a [providers.*] table overrides the built-in template wholesale).
const chatgptOAuthTOML = `
listen = "127.0.0.1:8787"
store = "gateway.db"
retention_days = 30

[providers.chatgpt]
base_url = "https://chatgpt.com/backend-api/codex"
style = "responses"
oauth = true
session_header = "session-id"
models = ["gpt-5.3-codex"]

[providers.openai]
base_url = "https://api.openai.com/v1"
api_key_env = "TEST_KEY_1"
models = ["gpt-4o"]

[[instances]]
alias = "chatgpt"
template = "chatgpt"

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`

// oauthTokenEndpoint is a fake issuer token endpoint with a settable canned
// response; it records every request form so tests can assert the verified
// exchange shape.
type oauthTokenEndpoint struct {
	srv    *httptest.Server
	status int
	body   string

	mu    sync.Mutex
	forms []string
	calls int
}

func newOAuthTokenEndpoint(t *testing.T, status int, body string) *oauthTokenEndpoint {
	te := &oauthTokenEndpoint{status: status, body: body}
	te.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		te.mu.Lock()
		te.forms = append(te.forms, string(raw))
		te.calls++
		te.mu.Unlock()
		w.WriteHeader(te.status)
		fmt.Fprint(w, te.body)
	}))
	t.Cleanup(te.srv.Close)
	return te
}

func (te *oauthTokenEndpoint) formCount() int {
	te.mu.Lock()
	defer te.mu.Unlock()
	return len(te.forms)
}

func (te *oauthTokenEndpoint) lastForm() string {
	te.mu.Lock()
	defer te.mu.Unlock()
	if len(te.forms) == 0 {
		return ""
	}
	return te.forms[len(te.forms)-1]
}

// testJWT builds an unsigned JWT with the given claims (same shape the
// gateway oauth tests use; no signature is needed for claim extraction).
func testJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc(header) + "." + enc(body) + ".sig"
}

// testAccountID is a UUID-shaped account id whose masked form is "1234…9012".
const testAccountID = "12345678-1234-1234-1234-123456789012"

// newOAuthAPI builds an API server over the chatgpt/openai test config with
// the OAuth issuer pinned at the mock token endpoint. It returns the API
// (for direct handler calls), the HTTP server, the config manager, the
// secrets store, and the token endpoint.
func newOAuthAPI(t *testing.T) (*API, *httptest.Server, *config.ConfigManager, *secrets.Store, *oauthTokenEndpoint) {
	t.Helper()
	te := newOAuthTokenEndpoint(t, http.StatusOK, "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gateway.toml")
	if err := os.WriteFile(cfgPath, []byte(chatgptOAuthTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	mgr := config.New(cfg)
	st, err := store.Open(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sec, err := secrets.Open(st.Path()+".secrets.json", testMasterKey)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	a := New(mgr, cfgPath, st, sec, logging.New(io.Discard), nil)
	a.SetOAuth(oauth.Config{Issuer: te.srv.URL}, te.srv.Client())
	gs := httptest.NewServer(a.Handler())
	t.Cleanup(gs.Close)
	return a, gs, mgr, sec, te
}

// startOAuth calls the start route and returns the parsed authorization URL
// plus the raw JSON body.
func startOAuth(t *testing.T, gs *httptest.Server, alias string) (*url.URL, map[string]any) {
	t.Helper()
	status, out := doJSON(t, gs, "POST", "/api/instances/"+alias+"/oauth/start", "")
	if status != http.StatusOK {
		t.Fatalf("start status = %d, want 200 (%v)", status, out)
	}
	raw, ok := out["authorization_url"].(string)
	if !ok || raw == "" {
		t.Fatalf("start response missing authorization_url: %v", out)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("authorization_url is not a URL: %v", err)
	}
	return u, out
}

// oauthCallback performs a callback request directly against the handler and
// returns the recorder.
func oauthCallback(t *testing.T, a *API, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/callback"+query, nil)
	rec := httptest.NewRecorder()
	a.HandleOAuthCallback(rec, req)
	return rec
}

// tokenBody builds a token endpoint response body from a claims map (or nil
// for the fallback shape) with an optional refresh token.
func tokenBody(claims map[string]any, refresh string) string {
	access := testJWT(map[string]any{"sub": "u-1"})
	if claims != nil {
		access = testJWT(claims)
	}
	rt := ""
	if refresh != "" {
		rt = fmt.Sprintf(`,"refresh_token":%q`, refresh)
	}
	return fmt.Sprintf(`{"access_token":%q%s,"expires_in":3600}`, access, rt)
}

func TestOAuthStartRejectsUnknownAndNonOAuthInstances(t *testing.T) {
	_, gs, _, _, _ := newOAuthAPI(t)

	status, out := doJSON(t, gs, "POST", "/api/instances/nope/oauth/start", "")
	if status != http.StatusNotFound || out["code"] != "NOT_FOUND" {
		t.Errorf("unknown instance: status/code = %d/%v, want 404 NOT_FOUND", status, out["code"])
	}
	status, out = doJSON(t, gs, "POST", "/api/instances/openai/oauth/start", "")
	if status != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Errorf("non-OAuth instance: status/code = %d/%v, want 400 INVALID_ARGUMENT", status, out["code"])
	}
	if msg, _ := out["message"].(string); !strings.Contains(msg, "not a ChatGPT OAuth instance") {
		t.Errorf("non-OAuth message = %q, want the OAuth-only rejection", msg)
	}
}

func TestOAuthInstanceCreateAndPatchRejectAPIKeys(t *testing.T) {
	_, gs, _, _, _ := newOAuthAPI(t)
	status, _ := doJSON(t, gs, "POST", "/api/instances", `{"alias":"oauth-key","template":"chatgpt","key":"sk-secret"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("OAuth create with key status = %d, want 400", status)
	}
	status, _ = doJSON(t, gs, "POST", "/api/instances", `{"alias":"oauth-env","template":"chatgpt","api_key_env":"CHATGPT_KEY"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("OAuth create with api_key_env status = %d, want 400", status)
	}
	status, _ = doJSON(t, gs, "PATCH", "/api/instances/chatgpt", `{"key":"sk-secret"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("OAuth patch with key status = %d, want 400", status)
	}
}

func TestOAuthLifecycleRequiresLoopbackSource(t *testing.T) {
	a, _, _, sec, _ := newOAuthAPI(t)
	if err := sec.SetOAuth("chatgpt", secrets.OAuthCredential{AccessToken: "access-token", AccountID: testAccountID}); err != nil {
		t.Fatal(err)
	}
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/instances/chatgpt/oauth/start"},
		{http.MethodPost, "/api/instances/chatgpt/oauth/device/start"},
		{http.MethodGet, "/api/instances/chatgpt/oauth/status"},
		{http.MethodGet, "/api/instances/chatgpt/oauth/device/status"},
		{http.MethodDelete, "/api/instances/chatgpt/oauth"},
	}
	for _, remote := range []string{
		"100.64.0.1:4321", "fd00::1:4321", "[fd00::1]:4321", "10.0.0.9:4321", "203.0.113.5:4321", "", "not-an-address",
	} {
		for _, route := range routes {
			t.Run(route.method+"/"+remote, func(t *testing.T) {
				req := httptest.NewRequest(route.method, route.path, nil)
				req.RemoteAddr = remote
				rec := httptest.NewRecorder()
				a.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s from %q status = %d, want 403 (%s)", route.method, remote, rec.Code, rec.Body.String())
				}
			})
		}
	}
	if _, ok, err := sec.GetOAuth("chatgpt"); err != nil || !ok {
		t.Fatalf("remote lifecycle requests changed the credential: (%v, %v)", ok, err)
	}
}

func TestOAuthLifecycleAllowsConfiguredListener(t *testing.T) {
	a, _, _, sec, _ := newOAuthAPI(t)
	a.SetOAuthListenAddrs([]string{"100.92.90.99:8787"})
	if err := sec.SetOAuth("chatgpt", secrets.OAuthCredential{AccessToken: "access-token", AccountID: testAccountID}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/instances/chatgpt/oauth/status", http.StatusOK},
		{http.MethodPost, "/api/instances/chatgpt/oauth/start", http.StatusOK},
		{http.MethodDelete, "/api/instances/chatgpt/oauth", http.StatusNoContent},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		req.RemoteAddr = "100.64.0.7:4321"
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{
			IP:   net.ParseIP("100.92.90.99"),
			Port: 8787,
		}))
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		if rec.Code != route.want {
			t.Errorf("%s %s status = %d, want %d (%s)", route.method, route.path, rec.Code, route.want, rec.Body.String())
		}
	}
}

func TestOAuthStartReturnsVerifiedAuthorizationURL(t *testing.T) {
	_, gs, _, _, _ := newOAuthAPI(t)

	u, out := startOAuth(t, gs, "chatgpt")
	if expires, _ := out["expires_in"].(float64); expires != 600 {
		t.Errorf("expires_in = %v, want 600", out["expires_in"])
	}
	if !strings.HasSuffix(u.Path, oauth.AuthorizePath) {
		t.Errorf("authorize path = %q, want %q suffix", u.Path, oauth.AuthorizePath)
	}
	q := u.Query()
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	if q.Get("client_id") != oauth.ClientID {
		t.Errorf("client_id = %q, want %q", q.Get("client_id"), oauth.ClientID)
	}
	if q.Get("redirect_uri") != oauth.RedirectURI {
		t.Errorf("redirect_uri = %q, want the fixed loopback %q", q.Get("redirect_uri"), oauth.RedirectURI)
	}
	if q.Get("scope") != oauth.Scope {
		t.Errorf("scope = %q, want %q", q.Get("scope"), oauth.Scope)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Errorf("code_challenge(_method) = %q/%q, want S256 and a value", q.Get("code_challenge_method"), q.Get("code_challenge"))
	}
	if q.Get("state") == "" {
		t.Error("state is empty")
	}
}

func TestOAuthStatusLifecycle(t *testing.T) {
	_, gs, _, _, _ := newOAuthAPI(t)

	status, out := doJSON(t, gs, "GET", "/api/instances/chatgpt/oauth/status", "")
	if status != http.StatusOK || out["connected"] != false {
		t.Errorf("status before connect = %d/%v, want 200 connected:false", status, out)
	}
	status, out = doJSON(t, gs, "GET", "/api/instances/nope/oauth/status", "")
	if status != http.StatusNotFound {
		t.Errorf("unknown instance status = %d, want 404", status)
	}
	status, out = doJSON(t, gs, "GET", "/api/instances/openai/oauth/status", "")
	if status != http.StatusBadRequest {
		t.Errorf("non-OAuth instance status = %d, want 400", status)
	}
}

func TestOAuthStatusMasksAccountID(t *testing.T) {
	_, gs, _, sec, _ := newOAuthAPI(t)
	if err := sec.SetOAuth("chatgpt", secrets.OAuthCredential{
		AccessToken: "tok-1",
		AccountID:   testAccountID,
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	status, out := doJSON(t, gs, "GET", "/api/instances/chatgpt/oauth/status", "")
	if status != http.StatusOK || out["connected"] != true {
		t.Fatalf("status = %d/%v, want 200 connected:true", status, out)
	}
	if got := out["account_id"]; got != "1234…9012" {
		t.Errorf("account_id = %v, want the masked form", got)
	}
	body, _ := json.Marshal(out)
	if strings.Contains(string(body), testAccountID) {
		t.Errorf("status body leaked the full account id: %s", body)
	}
	if _, ok := out["access_token"]; ok {
		t.Error("status body contains access_token")
	}
	if _, ok := out["refresh_token"]; ok {
		t.Error("status body contains refresh_token")
	}
}

func TestOAuthStatusCorruptedRecord(t *testing.T) {
	_, gs, _, sec, _ := newOAuthAPI(t)
	if err := sec.Set("chatgpt", "oauth:v1:{not json"); err != nil {
		t.Fatal(err)
	}
	status, out := doJSON(t, gs, "GET", "/api/instances/chatgpt/oauth/status", "")
	if status != http.StatusInternalServerError {
		t.Errorf("corrupted record status = %d, want 500 (%v)", status, out)
	}
}

func TestOAuthCallbackSuccessFlow(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	te.body = tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "rt-1")

	u, _ := startOAuth(t, gs, "chatgpt")
	state := u.Query().Get("state")
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(state)+"&code=the-auth-code")
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "successful") {
		t.Errorf("callback page = %q, want the success message", rec.Body.String())
	}
	// The callback page never carries tokens, the code, or the state.
	for _, secret := range []string{"the-auth-code", state, "rt-1", "tok", testAccountID} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("callback page leaked %q", secret)
		}
	}

	// The encrypted credential was persisted under the bound instance.
	cred, ok, err := sec.GetOAuth("chatgpt")
	if err != nil || !ok {
		t.Fatalf("GetOAuth after callback = (%v, %v), want the stored credential", ok, err)
	}
	if cred.AccountID != testAccountID || cred.RefreshToken != "rt-1" {
		t.Errorf("stored credential = %+v, want account %s / refresh rt-1", cred, testAccountID)
	}
	if cred.AccessToken == "" {
		t.Error("stored credential has no access token")
	}

	// The exchange used the verified request shape and the server-side
	// verifier matching the URL's S256 challenge.
	form := te.lastForm()
	for key, want := range map[string]string{
		"grant_type":   "authorization_code",
		"code":         "the-auth-code",
		"redirect_uri": oauth.RedirectURI,
		"client_id":    oauth.ClientID,
	} {
		if got := formValue(form, key); got != want {
			t.Errorf("exchange form %s = %q, want %q (form %q)", key, got, want, form)
		}
	}
	verifier := formValue(form, "code_verifier")
	if verifier == "" || !oauth.PKCEMatches(verifier, u.Query().Get("code_challenge")) {
		t.Errorf("code_verifier %q does not match the URL challenge", verifier)
	}

	// Status now reports connected with the masked account.
	status, out := doJSON(t, gs, "GET", "/api/instances/chatgpt/oauth/status", "")
	if status != http.StatusOK || out["connected"] != true || out["account_id"] != "1234…9012" {
		t.Errorf("status after connect = %d/%v, want connected with masked id", status, out)
	}
}

func TestOAuthCallbackReplayRejected(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	te.body = tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "")

	u, _ := startOAuth(t, gs, "chatgpt")
	state := u.Query().Get("state")
	query := "?state=" + url.QueryEscape(state) + "&code=the-auth-code"
	if rec := oauthCallback(t, a, query); rec.Code != http.StatusOK {
		t.Fatalf("first callback status = %d, want 200", rec.Code)
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); !ok {
		t.Fatal("first callback did not persist the credential")
	}
	// The replayed state is rejected (single-use) and never reaches the
	// token endpoint again.
	rec := oauthCallback(t, a, query)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("replayed callback status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "the-auth-code") || strings.Contains(rec.Body.String(), state) {
		t.Errorf("replay page leaked the code or state: %q", rec.Body.String())
	}
	if te.formCount() != 1 {
		t.Errorf("token endpoint called %d times, want 1 (replay must not exchange)", te.formCount())
	}
}

func TestOAuthCallbackUnknownStateAndMissingParams(t *testing.T) {
	a, _, _, sec, te := newOAuthAPI(t)

	for _, query := range []string{
		"?state=never-generated&code=abc",
		"?state=&code=abc",
		"?state=xyz",
		"",
	} {
		rec := oauthCallback(t, a, query)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("callback %q status = %d, want 400", query, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "abc") || strings.Contains(rec.Body.String(), "never-generated") {
			t.Errorf("callback %q page echoed input: %q", query, rec.Body.String())
		}
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); ok {
		t.Error("a rejected callback persisted a credential")
	}
	if te.formCount() != 0 {
		t.Errorf("token endpoint called %d times, want 0", te.formCount())
	}
}

func TestOAuthCallbackProviderError(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)

	u, _ := startOAuth(t, gs, "chatgpt")
	state := u.Query().Get("state")
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(state)+"&error=access_denied&error_description=super-secret-description")
	if rec.Code != http.StatusOK {
		t.Errorf("provider-error callback status = %d, want 200 (informational page)", rec.Code)
	}
	// error_description is never echoed; the fixed error code may appear.
	if strings.Contains(rec.Body.String(), "super-secret-description") {
		t.Errorf("callback page leaked error_description: %q", rec.Body.String())
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); ok {
		t.Error("provider-error callback persisted a credential")
	}
	if te.formCount() != 0 {
		t.Errorf("token endpoint called %d times, want 0", te.formCount())
	}
	// A provider-declined callback burns its state just like a successful
	// callback, so the same browser result cannot be replayed as a login.
	rec = oauthCallback(t, a, "?state="+url.QueryEscape(state)+"&code=late-code")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("callback after provider error status = %d, want 400", rec.Code)
	}
}

func TestOAuthCallbackExchangeFailureIsRedacted(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	te.status = http.StatusBadRequest
	te.body = `{"error":"invalid_grant","error_description":"super-secret-body"}`

	u, _ := startOAuth(t, gs, "chatgpt")
	state := u.Query().Get("state")
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(state)+"&code=the-auth-code")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("exchange-failure callback status = %d, want 502", rec.Code)
	}
	page := rec.Body.String()
	if strings.Contains(page, "super-secret-body") || strings.Contains(page, "the-auth-code") || strings.Contains(page, state) {
		t.Errorf("callback page leaked provider/token material: %q", page)
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); ok {
		t.Error("failed exchange persisted a credential")
	}
}

func TestOAuthCallbackCannotPersistAfterDisconnect(t *testing.T) {
	a, gs, _, sec, _ := newOAuthAPI(t)
	started := make(chan struct{})
	release := make(chan struct{})
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "rt-stale"))
	}))
	t.Cleanup(tokenSrv.Close)
	a.SetOAuth(oauth.Config{Issuer: tokenSrv.URL}, tokenSrv.Client())

	u, _ := startOAuth(t, gs, "chatgpt")
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- oauthCallback(t, a, "?state="+url.QueryEscape(u.Query().Get("state"))+"&code=the-auth-code")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not reach the token endpoint")
	}
	status, _ := doJSON(t, gs, "DELETE", "/api/instances/chatgpt/oauth", "")
	if status != http.StatusNoContent {
		t.Fatalf("disconnect status = %d, want 204", status)
	}
	close(release)
	rec := <-result
	if rec.Code != http.StatusBadRequest {
		t.Errorf("stale callback status = %d, want 400", rec.Code)
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); ok {
		t.Error("stale callback resurrected the credential after disconnect")
	}
}

func TestOAuthCallbackNoAccountIDRejected(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	te.body = tokenBody(nil, "rt-1") // access token without account claims

	u, _ := startOAuth(t, gs, "chatgpt")
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(u.Query().Get("state"))+"&code=the-auth-code")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("no-account-id callback status = %d, want 502", rec.Code)
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); ok {
		t.Error("a token response without an account id was persisted")
	}
}

// TestOAuthCallbackInstanceParamIgnored pins the cross-instance binding: the
// credential lands under the state's minting instance, never under an
// attacker-supplied instance parameter in the callback URL.
func TestOAuthCallbackInstanceParamIgnored(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	te.body = tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "")

	u, _ := startOAuth(t, gs, "chatgpt")
	state := u.Query().Get("state")
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(state)+"&code=the-auth-code&instance=openai")
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", rec.Code)
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); !ok {
		t.Error("credential not stored under the minting instance chatgpt")
	}
	if _, ok, _ := sec.GetOAuth("openai"); ok {
		t.Error("credential stored under the attacker-supplied instance openai")
	}
}

func TestOAuthCallbackInstanceDeletedMidFlow(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	te.body = tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "")

	u, _ := startOAuth(t, gs, "chatgpt")
	status, _ := doJSON(t, gs, "DELETE", "/api/instances/chatgpt", "")
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", status)
	}
	status, _ = doJSON(t, gs, "POST", "/api/instances", `{"alias":"chatgpt","template":"chatgpt"}`)
	if status != http.StatusCreated {
		t.Fatalf("recreate status = %d, want 201", status)
	}
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(u.Query().Get("state"))+"&code=the-auth-code")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("callback for a deleted instance status = %d, want 400", rec.Code)
	}
	if te.formCount() != 0 {
		t.Errorf("token endpoint called %d times for a deleted instance, want 0", te.formCount())
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); ok {
		t.Error("stale callback resurrected a credential after delete/recreate")
	}
}

func TestOAuthRenamePurgesPendingFlow(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	u, _ := startOAuth(t, gs, "chatgpt")
	status, _ := doJSON(t, gs, "PATCH", "/api/instances/chatgpt", `{"alias":"chatgpt-renamed"}`)
	if status != http.StatusOK {
		t.Fatalf("rename status = %d, want 200", status)
	}
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(u.Query().Get("state"))+"&code=stale-code")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("renamed flow callback status = %d, want 400", rec.Code)
	}
	if te.formCount() != 0 {
		t.Errorf("token endpoint called %d times after rename, want 0", te.formCount())
	}
	if _, ok, _ := sec.GetOAuth("chatgpt-renamed"); ok {
		t.Error("stale callback persisted a credential under the renamed instance")
	}
}

func TestOAuthRenameMovesCredential(t *testing.T) {
	_, gs, _, sec, _ := newOAuthAPI(t)
	cred := secrets.OAuthCredential{AccessToken: "access-token", RefreshToken: "refresh-token", AccountID: testAccountID}
	if err := sec.SetOAuth("chatgpt", cred); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, gs, "PATCH", "/api/instances/chatgpt", `{"alias":"chatgpt-renamed"}`)
	if status != http.StatusOK {
		t.Fatalf("rename status = %d, want 200", status)
	}
	if _, ok, err := sec.GetOAuth("chatgpt"); err != nil || ok {
		t.Errorf("old OAuth credential = (%v, %v), want absent", ok, err)
	}
	got, ok, err := sec.GetOAuth("chatgpt-renamed")
	if err != nil || !ok {
		t.Fatalf("renamed OAuth credential = (%+v, %v, %v), want record", got, ok, err)
	}
	if got.AccessToken != cred.AccessToken || got.RefreshToken != cred.RefreshToken || got.AccountID != cred.AccountID {
		t.Errorf("renamed OAuth credential = %+v, want %+v", got, cred)
	}
}

func forceSecretWriteFailure(t *testing.T, sec *secrets.Store) {
	t.Helper()
	if err := os.Remove(sec.Path()); err != nil {
		t.Fatalf("remove secrets file: %v", err)
	}
	if err := os.Mkdir(sec.Path(), 0o700); err != nil {
		t.Fatalf("replace secrets file with directory: %v", err)
	}
}

func TestInstanceCredentialFailureRollsBackConfig(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		_, gs, mgr, sec, _ := newOAuthAPI(t)
		forceSecretWriteFailure(t, sec)
		status, _ := doJSON(t, gs, "POST", "/api/instances", `{"alias":"new","template":"openai","key":"sk-create-secret"}`)
		if status != http.StatusInternalServerError {
			t.Fatalf("create status = %d, want 500", status)
		}
		if _, ok := mgr.Get().Instance("new"); ok {
			t.Error("failed create remained in config")
		}
		if _, ok := sec.Get("new"); ok {
			t.Error("failed create left a credential in memory")
		}
	})

	t.Run("rename", func(t *testing.T) {
		_, gs, mgr, sec, _ := newOAuthAPI(t)
		if err := sec.SetOAuth("chatgpt", secrets.OAuthCredential{AccessToken: "access-token", AccountID: testAccountID}); err != nil {
			t.Fatal(err)
		}
		forceSecretWriteFailure(t, sec)
		status, _ := doJSON(t, gs, "PATCH", "/api/instances/chatgpt", `{"alias":"renamed"}`)
		if status != http.StatusInternalServerError {
			t.Fatalf("rename status = %d, want 500", status)
		}
		if _, ok := mgr.Get().Instance("renamed"); ok {
			t.Error("failed rename remained under the new alias")
		}
		if _, ok := mgr.Get().Instance("chatgpt"); !ok {
			t.Error("failed rename removed the old config alias")
		}
		if _, ok, err := sec.GetOAuth("chatgpt"); err != nil || !ok {
			t.Errorf("failed rename credential = (%v, %v), want old record", ok, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		_, gs, mgr, sec, _ := newOAuthAPI(t)
		if err := sec.SetOAuth("chatgpt", secrets.OAuthCredential{AccessToken: "access-token", AccountID: testAccountID}); err != nil {
			t.Fatal(err)
		}
		forceSecretWriteFailure(t, sec)
		status, _ := doJSON(t, gs, "DELETE", "/api/instances/chatgpt", "")
		if status != http.StatusInternalServerError {
			t.Fatalf("delete status = %d, want 500", status)
		}
		if _, ok := mgr.Get().Instance("chatgpt"); !ok {
			t.Error("failed delete removed the config instance")
		}
		if _, ok, err := sec.GetOAuth("chatgpt"); err != nil || !ok {
			t.Errorf("failed delete credential = (%v, %v), want record", ok, err)
		}
	})
}

func TestOAuthDisconnectFailurePreservesLifecycle(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	if err := sec.SetOAuth("chatgpt", secrets.OAuthCredential{AccessToken: "access-token", AccountID: testAccountID}); err != nil {
		t.Fatal(err)
	}
	u, _ := startOAuth(t, gs, "chatgpt")
	generation := a.oauthLife.Generation("chatgpt")
	forceSecretWriteFailure(t, sec)
	status, _ := doJSON(t, gs, "DELETE", "/api/instances/chatgpt/oauth", "")
	if status != http.StatusInternalServerError {
		t.Fatalf("disconnect status = %d, want 500", status)
	}
	if got := a.oauthLife.Generation("chatgpt"); got != generation {
		t.Fatalf("failed disconnect generation = %d, want %d", got, generation)
	}

	// Restore a writable path without purging the pending state. Its callback
	// must remain usable because the failed disconnect did not commit.
	if err := os.RemoveAll(sec.Path()); err != nil {
		t.Fatal(err)
	}
	te.body = tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "")
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(u.Query().Get("state"))+"&code=retry-code")
	if rec.Code != http.StatusOK {
		t.Fatalf("callback after failed disconnect status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestOAuthRenameValidationDoesNotInvalidateTargetFlow(t *testing.T) {
	a, gs, mgr, sec, te := newOAuthAPI(t)
	clone := mgr.Get().Clone()
	clone.Instances = append(clone.Instances, &config.Instance{Alias: "target", Template: "chatgpt"})
	if err := mgr.Swap(clone); err != nil {
		t.Fatalf("add target instance: %v", err)
	}
	te.body = tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "")
	u, _ := startOAuth(t, gs, "target")
	status, _ := doJSON(t, gs, "PATCH", "/api/instances/chatgpt", `{"alias":"target"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("colliding rename status = %d, want 400", status)
	}
	if rec := oauthCallback(t, a, "?state="+url.QueryEscape(u.Query().Get("state"))+"&code=target-code"); rec.Code != http.StatusOK {
		t.Fatalf("target callback status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if _, ok, err := sec.GetOAuth("target"); err != nil || !ok {
		t.Fatalf("target credential after failed rename = (%v, %v), want record", ok, err)
	}
}

// TestOAuthStartSupersedesPriorFlow pins the single-active-flow invariant: a
// new start retires the previous pending transaction, so the stale browser
// tab cannot complete and clobber the newer sign-in.
func TestOAuthStartSupersedesPriorFlow(t *testing.T) {
	a, gs, _, _, te := newOAuthAPI(t)
	te.body = tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "")

	first, _ := startOAuth(t, gs, "chatgpt")
	second, _ := startOAuth(t, gs, "chatgpt")
	if first.Query().Get("state") == second.Query().Get("state") {
		t.Fatal("two starts returned the same state")
	}
	// The first flow's callback is rejected (its state was purged)…
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(first.Query().Get("state"))+"&code=first-code")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("superseded flow callback status = %d, want 400", rec.Code)
	}
	// …while the newest flow completes.
	rec = oauthCallback(t, a, "?state="+url.QueryEscape(second.Query().Get("state"))+"&code=second-code")
	if rec.Code != http.StatusOK {
		t.Errorf("newest flow callback status = %d, want 200", rec.Code)
	}
	if te.formCount() != 1 {
		t.Errorf("token endpoint called %d times, want 1 (only the newest flow)", te.formCount())
	}
}

func TestOAuthDisconnectAndPendingFlowPurge(t *testing.T) {
	a, gs, _, sec, te := newOAuthAPI(t)
	te.body = tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "")

	// Connect first.
	u, _ := startOAuth(t, gs, "chatgpt")
	if rec := oauthCallback(t, a, "?state="+url.QueryEscape(u.Query().Get("state"))+"&code=the-auth-code"); rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", rec.Code)
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); !ok {
		t.Fatal("credential not stored before disconnect")
	}

	// Start a fresh flow, then disconnect: the pending flow must be retired
	// so the still-open browser tab cannot reconnect the instance.
	u2, _ := startOAuth(t, gs, "chatgpt")
	status, _ := doJSON(t, gs, "DELETE", "/api/instances/chatgpt/oauth", "")
	if status != http.StatusNoContent {
		t.Fatalf("disconnect status = %d, want 204", status)
	}
	if _, ok, _ := sec.GetOAuth("chatgpt"); ok {
		t.Error("credential still stored after disconnect")
	}
	status, out := doJSON(t, gs, "GET", "/api/instances/chatgpt/oauth/status", "")
	if status != http.StatusOK || out["connected"] != false {
		t.Errorf("status after disconnect = %d/%v, want connected:false", status, out)
	}
	rec := oauthCallback(t, a, "?state="+url.QueryEscape(u2.Query().Get("state"))+"&code=the-auth-code")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("callback after disconnect status = %d, want 400", rec.Code)
	}

	// Disconnect is idempotent; unknown/non-OAuth instances are rejected.
	status, _ = doJSON(t, gs, "DELETE", "/api/instances/chatgpt/oauth", "")
	if status != http.StatusNoContent {
		t.Errorf("second disconnect status = %d, want 204", status)
	}
	status, out = doJSON(t, gs, "DELETE", "/api/instances/openai/oauth", "")
	if status != http.StatusBadRequest || out["code"] != "INVALID_ARGUMENT" {
		t.Errorf("disconnect non-OAuth status = %d/%v, want 400 INVALID_ARGUMENT", status, out["code"])
	}
	status, _ = doJSON(t, gs, "DELETE", "/api/instances/nope/oauth", "")
	if status != http.StatusNotFound {
		t.Errorf("disconnect unknown instance status = %d, want 404", status)
	}
}

// formValue extracts one field from a url.Values-encoded form string.
func formValue(form, key string) string {
	vals, err := url.ParseQuery(form)
	if err != nil {
		return ""
	}
	return vals.Get(key)
}
