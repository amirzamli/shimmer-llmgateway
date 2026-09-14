package api

// Tests for the ChatGPT device-code OAuth API surface: instance gating,
// trusted-source enforcement, status masking, disconnect, and credential-safe
// instance mutations.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// chatgptOAuthTOML seeds one OAuth-marked chatgpt instance plus an ordinary
// keyed openai instance, mirroring the provider table shape used by gateway
// tests.
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

type oauthTokenEndpoint struct {
	srv    *httptest.Server
	status int
	body   string
}

func newOAuthTokenEndpoint(t *testing.T, status int, body string) *oauthTokenEndpoint {
	t.Helper()
	te := &oauthTokenEndpoint{status: status, body: body}
	te.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(te.status)
		_, _ = fmt.Fprint(w, te.body)
	}))
	t.Cleanup(te.srv.Close)
	return te
}

// testJWT builds an unsigned JWT with the given claims. The OAuth parser only
// needs the claims payload; signature verification is outside this protocol
// component.
func testJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc(header) + "." + enc(body) + ".sig"
}

// testAccountID is a UUID-shaped account id whose masked form is "1234…9012".
const testAccountID = "12345678-1234-1234-1234-123456789012"

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
	t.Cleanup(func() { _ = st.Close() })
	sec, err := secrets.Open(st.Path()+".secrets.json", testMasterKey)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	a := New(mgr, cfgPath, st, sec, logging.New(io.Discard), nil, nil)
	a.SetOAuth(oauth.Config{Issuer: te.srv.URL}, te.srv.Client())
	gs := httptest.NewServer(a.Handler())
	t.Cleanup(gs.Close)
	return a, gs, mgr, sec, te
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
		{http.MethodPost, "/api/instances/chatgpt/oauth/device/start"},
		{http.MethodGet, "/api/instances/chatgpt/oauth/device/status"},
		{http.MethodGet, "/api/instances/chatgpt/oauth/status"},
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

func TestOAuthStatusLifecycle(t *testing.T) {
	_, gs, _, _, _ := newOAuthAPI(t)
	status, out := doJSON(t, gs, "GET", "/api/instances/chatgpt/oauth/status", "")
	if status != http.StatusOK || out["connected"] != false {
		t.Errorf("status before connect = %d/%v, want 200 connected:false", status, out)
	}
	status, _ = doJSON(t, gs, "GET", "/api/instances/nope/oauth/status", "")
	if status != http.StatusNotFound {
		t.Errorf("unknown instance status = %d, want 404", status)
	}
	status, _ = doJSON(t, gs, "GET", "/api/instances/openai/oauth/status", "")
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
