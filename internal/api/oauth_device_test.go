package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
)

func TestOAuthDeviceLifecycle(t *testing.T) {
	verifier, err := oauth.GenerateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	polls := 0
	var exchangeForm url.Values
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case oauth.DeviceUserCodePath:
			if r.Method != http.MethodPost {
				t.Errorf("device start method = %s", r.Method)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("device start body: %v", err)
			}
			if body["client_id"] != oauth.ClientID {
				t.Errorf("device start client_id = %q", body["client_id"])
			}
			fmt.Fprint(w, `{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":"1"}`)
		case oauth.DeviceTokenPath:
			mu.Lock()
			polls++
			current := polls
			mu.Unlock()
			if current == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprintf(w, `{"authorization_code":"device-auth","code_verifier":%q}`, verifier)
		case oauth.TokenPath:
			if err := r.ParseForm(); err != nil {
				t.Errorf("token form: %v", err)
			}
			exchangeForm = r.Form
			fmt.Fprint(w, tokenBody(map[string]any{"chatgpt_account_id": testAccountID}, "device-refresh"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()

	a, gs, _, sec, _ := newOAuthAPI(t)
	a.SetOAuth(oauth.Config{Issuer: issuer.URL}, issuer.Client())

	status, out := doJSON(t, gs, http.MethodPost, "/api/instances/chatgpt/oauth/device/start", "")
	if status != http.StatusOK {
		t.Fatalf("device start status = %d/%v", status, out)
	}
	if out["user_code"] != "ABCD-EFGH" || out["authorization_url"] != issuer.URL+oauth.DeviceURLPath {
		t.Fatalf("device start response = %v", out)
	}

	status, out = doJSON(t, gs, http.MethodGet, "/api/instances/chatgpt/oauth/device/status", "")
	if status != http.StatusOK || out["pending"] != true || out["connected"] != false {
		t.Fatalf("pending device status = %d/%v", status, out)
	}
	status, out = doJSON(t, gs, http.MethodGet, "/api/instances/chatgpt/oauth/device/status", "")
	if status != http.StatusOK || out["connected"] != true || out["pending"] != nil {
		t.Fatalf("connected device status = %d/%v", status, out)
	}
	if got := exchangeForm.Get("redirect_uri"); got != issuer.URL+oauth.DeviceRedirectPath {
		t.Errorf("device redirect_uri = %q", got)
	}
	if got := exchangeForm.Get("code"); got != "device-auth" {
		t.Errorf("device code = %q", got)
	}
	if got, ok, err := sec.GetOAuth("chatgpt"); err != nil || !ok || got.AccountID != testAccountID || got.RefreshToken != "device-refresh" {
		t.Fatalf("stored device credential = (%+v, %v, %v)", got, ok, err)
	}
}

func TestOAuthDeviceDisconnectPurgesPendingTransaction(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == oauth.DeviceUserCodePath {
			fmt.Fprint(w, `{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":"1"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer issuer.Close()

	a, gs, _, _, _ := newOAuthAPI(t)
	a.SetOAuth(oauth.Config{Issuer: issuer.URL}, issuer.Client())
	if status, out := doJSON(t, gs, http.MethodPost, "/api/instances/chatgpt/oauth/device/start", ""); status != http.StatusOK {
		t.Fatalf("device start status = %d/%v", status, out)
	}
	if status, out := doJSON(t, gs, http.MethodDelete, "/api/instances/chatgpt/oauth", ""); status != http.StatusNoContent {
		t.Fatalf("disconnect status = %d/%v", status, out)
	}
	status, out := doJSON(t, gs, http.MethodGet, "/api/instances/chatgpt/oauth/device/status", "")
	if status != http.StatusOK || out["connected"] != false || out["pending"] != nil {
		t.Fatalf("device status after disconnect = %d/%v", status, out)
	}
}

func TestOAuthDeviceErrorsDoNotExposeProviderBody(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == oauth.DeviceUserCodePath {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`sensitive-device-body`))
			return
		}
		http.NotFound(w, r)
	}))
	defer issuer.Close()

	a, gs, _, _, _ := newOAuthAPI(t)
	a.SetOAuth(oauth.Config{Issuer: issuer.URL}, issuer.Client())
	status, out := doJSON(t, gs, http.MethodPost, "/api/instances/chatgpt/oauth/device/start", "")
	if status != http.StatusBadGateway {
		t.Fatalf("device start status = %d/%v", status, out)
	}
	if strings.Contains(fmt.Sprint(out), "sensitive-device-body") {
		t.Fatalf("device error exposed provider body: %v", out)
	}
}

func TestOAuthDeviceStatusRequiresConfiguredRemoteListener(t *testing.T) {
	a, _, _, _, _ := newOAuthAPI(t)
	a.SetOAuthListenAddrs([]string{"100.92.90.99:8787"})
	req := httptest.NewRequest(http.MethodGet, "/api/instances/chatgpt/oauth/device/status", nil)
	req.RemoteAddr = "100.92.90.99:4321"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{
		IP:   net.ParseIP("100.92.90.99"),
		Port: 8787,
	}))
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configured listener device status = %d/%s", rec.Code, rec.Body.String())
	}
}
