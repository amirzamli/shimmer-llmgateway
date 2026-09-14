package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestDeviceCode(t *testing.T) {
	var request map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != DeviceUserCodePath {
			t.Fatalf("request = %s %s, want POST %s", r.Method, r.URL.Path, DeviceUserCodePath)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":"7"}`)
	}))
	defer srv.Close()

	device, err := (Config{Issuer: srv.URL}).RequestDeviceCode(context.Background(), srv.Client())
	if err != nil {
		t.Fatalf("RequestDeviceCode: %v", err)
	}
	if request["client_id"] != ClientID {
		t.Errorf("client_id = %q, want %q", request["client_id"], ClientID)
	}
	if device.DeviceAuthID != "device-1" || device.UserCode != "ABCD-EFGH" || device.Interval != 7*time.Second {
		t.Errorf("device = %+v", device)
	}
	if got := (Config{Issuer: srv.URL}).DeviceURL(); got != srv.URL+DeviceURLPath {
		t.Errorf("DeviceURL = %q", got)
	}
	if got := (Config{Issuer: srv.URL}).DeviceRedirectURI(); got != srv.URL+DeviceRedirectPath {
		t.Errorf("DeviceRedirectURI = %q", got)
	}
}

func TestPollDeviceAuthorizationPendingThenComplete(t *testing.T) {
	verifier, err := GenerateVerifier()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DeviceTokenPath {
			t.Fatalf("path = %s, want %s", r.URL.Path, DeviceTokenPath)
		}
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request["device_auth_id"] != "device-1" || request["user_code"] != "ABCD-EFGH" {
			t.Errorf("request = %v", request)
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `{"authorization_code":"auth-1","code_verifier":"`+verifier+`"}`)
	}))
	defer srv.Close()

	cfg := Config{Issuer: srv.URL}
	device := DeviceCode{DeviceAuthID: "device-1", UserCode: "ABCD-EFGH"}
	auth, pending, err := cfg.PollDeviceAuthorization(context.Background(), srv.Client(), device)
	if err != nil || !pending || auth != nil {
		t.Fatalf("pending poll = (%+v, %v, %v)", auth, pending, err)
	}
	auth, pending, err = cfg.PollDeviceAuthorization(context.Background(), srv.Client(), device)
	if err != nil || pending || auth == nil {
		t.Fatalf("complete poll = (%+v, %v, %v)", auth, pending, err)
	}
	if auth.AuthorizationCode != "auth-1" || auth.CodeVerifier != verifier {
		t.Errorf("authorization = %+v", auth)
	}
}

func TestPollDeviceAuthorizationRejectsInvalidVerifier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"authorization_code":"auth-1","code_verifier":"bad"}`)
	}))
	defer srv.Close()

	_, pending, err := (Config{Issuer: srv.URL}).PollDeviceAuthorization(context.Background(), srv.Client(), DeviceCode{DeviceAuthID: "device", UserCode: "code"})
	if pending || err == nil || !strings.Contains(err.Error(), "verifier") {
		t.Fatalf("poll = (pending=%v, err=%v), want invalid verifier", pending, err)
	}
}

func TestDeviceStoreExpiresTransactions(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	store := NewDeviceStore()
	store.now = func() time.Time { return now }
	store.Put(DeviceTransaction{InstanceID: "chatgpt", ExpiresAt: now.Add(time.Minute)})
	if _, ok := store.Get("chatgpt"); !ok {
		t.Fatal("fresh device transaction missing")
	}
	now = now.Add(time.Minute)
	if _, ok := store.Get("chatgpt"); ok {
		t.Fatal("expired device transaction returned")
	}
}
