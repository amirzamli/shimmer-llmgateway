package gateway

// Phase 2b gateway tests: the fixed loopback OAuth callback surface — route
// mounting under the admin guard, Host policy, and access-log redaction of
// the state/code query string.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/amirzamli/shimmer-llmgateway/internal/netutil"
	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
)

// TestOAuthCallbackRouteMounted runs the sign-in flow end to end through the
// gateway HTTP surface: start (guarded /api route), the callback on the
// fixed loopback path with a localhost Host header (the provider's browser
// redirect), and the status check.
func TestOAuthCallbackRouteMounted(t *testing.T) {
	te := newTokenEndpoint(t, http.StatusOK, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-1","expires_in":3600}`, testJWT(map[string]any{"chatgpt_account_id": "acc-1234-9012"})))
	var logBuf bytes.Buffer
	srv, _ := newGatewayServer(t, newFakeProvider(t, nil), chatgptOAuthTOML("https://chatgpt.example.com"), nil, &logBuf)
	srv.api.SetOAuth(oauth.Config{Issuer: te.srv.URL}, te.srv.Client())
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	resp, out := apiDo(t, gs, "POST", "/api/instances/chatgpt/oauth/start", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d (%v)", resp.StatusCode, out)
	}
	u, err := url.Parse(out["authorization_url"].(string))
	if err != nil {
		t.Fatalf("authorization_url: %v", err)
	}
	state := u.Query().Get("state")

	// The provider's redirect arrives at the dedicated callback handler with a
	// localhost Host header and a loopback TCP source.
	req := httptest.NewRequest(http.MethodGet, gs.URL+"/auth/callback?state="+url.QueryEscape(state)+"&code=cb-code", nil)
	req.Host = "localhost:1455"
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	srv.OAuthCallbackHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "successful") {
		t.Errorf("callback page = %q, want the success message", rec.Body.String())
	}

	resp, out = apiDo(t, gs, "GET", "/api/instances/chatgpt/oauth/status", "")
	if resp.StatusCode != http.StatusOK || out["connected"] != true {
		t.Fatalf("status = %d/%v, want connected", resp.StatusCode, out)
	}
	// The access log records only the callback path, never the state/code
	// query string.
	logs := logBuf.String()
	if strings.Contains(logs, "cb-code") || strings.Contains(logs, state) {
		t.Errorf("access log leaked callback query material:\n%s", logs)
	}
}

// TestOAuthCallbackGuardRejectsForeignHost pins the admin-surface guard on
// the callback: a request whose Host is not a served address, loopback host,
// or literal loopback/CGNAT/ULA address is refused before the handler runs.
func TestOAuthCallbackGuardRejectsForeignHost(t *testing.T) {
	srv, _ := newGatewayServer(t, newFakeProvider(t, nil), chatgptOAuthTOML("https://chatgpt.example.com"), nil, io.Discard)
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	req := httptest.NewRequest(http.MethodGet, gs.URL+"/auth/callback?state=x&code=y", nil)
	req.Host = "evil.example.com"
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	srv.OAuthCallbackHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("callback with foreign Host status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "FORBIDDEN") {
		t.Errorf("guard body = %q, want the FORBIDDEN error", rec.Body.String())
	}
}

func TestOAuthCallbackGuardRejectsRemoteSourceAndConfiguredHost(t *testing.T) {
	srv, _ := newGatewayServer(t, newFakeProvider(t, nil), chatgptOAuthTOML("https://chatgpt.example.com"), nil, io.Discard)
	h := srv.OAuthCallbackHandler()
	cases := []struct {
		name, host, remote string
	}{
		{"configured remote host", "100.64.0.10:1455", "127.0.0.1:54321"},
		{"remote source", "localhost:1455", "100.64.0.10:54321"},
		{"public source", "localhost:1455", "8.8.8.8:54321"},
		{"missing source", "localhost:1455", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=x&code=y", nil)
			req.Host = tc.host
			req.RemoteAddr = tc.remote
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("callback status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestOAuthCallbackNotMountedOnConfiguredHandler(t *testing.T) {
	srv, _ := newGatewayServer(t, newFakeProvider(t, nil), chatgptOAuthTOML("https://chatgpt.example.com"), nil, io.Discard)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=x&code=y", nil)
	req.Host = "localhost:8787"
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("configured handler callback status = %d, want 404", rec.Code)
	}
}

// TestOAuthCallbackAddrLoopback pins the fixed callback listener to the
// loopback family: the browser redirect contract must never be served on a
// non-loopback interface.
func TestOAuthCallbackAddrLoopback(t *testing.T) {
	host := hostOnly(OAuthCallbackAddr)
	if !netutil.IsLoopbackHost(host) {
		t.Errorf("OAuthCallbackAddr %q resolves to host %q, want a loopback host", OAuthCallbackAddr, host)
	}
}
