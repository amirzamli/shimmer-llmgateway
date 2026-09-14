package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DeviceUserCodePath is the OpenCode headless device-code request endpoint.
	DeviceUserCodePath = "/api/accounts/deviceauth/usercode"
	// DeviceTokenPath is the OpenCode headless device-code polling endpoint.
	DeviceTokenPath = "/api/accounts/deviceauth/token"
	// DeviceURLPath is the browser page where the user enters UserCode.
	DeviceURLPath = "/codex/device"
	// DeviceRedirectPath is the redirect URI used for the authorization-code
	// exchange returned by the device-token endpoint.
	DeviceRedirectPath = "/deviceauth/callback"
)

// DeviceCode is the short-lived provider transaction returned by the device
// authorization endpoint. The credentials are never part of this value.
type DeviceCode struct {
	DeviceAuthID string
	UserCode     string
	Interval     time.Duration
}

// DeviceAuthorization is the one-time authorization code and PKCE verifier
// returned after the user approves the device code in the browser.
type DeviceAuthorization struct {
	AuthorizationCode string
	CodeVerifier      string
}

// DeviceTransaction is the server-side state needed to poll one device login.
// It binds provider state to an instance and OAuth lifecycle generation.
type DeviceTransaction struct {
	InstanceID   string
	Generation   uint64
	DeviceAuthID string
	UserCode     string
	Interval     time.Duration
	ExpiresAt    time.Time
}

// Expired reports whether the device transaction is no longer usable.
func (t DeviceTransaction) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// DeviceStore keeps pending device transactions in memory. A gateway restart
// discards them, matching the browser state store's restart behavior.
type DeviceStore struct {
	mu     sync.Mutex
	values map[string]DeviceTransaction
	now    func() time.Time
}

// NewDeviceStore returns an empty device transaction store.
func NewDeviceStore() *DeviceStore {
	return &DeviceStore{values: make(map[string]DeviceTransaction), now: time.Now}
}

// Put replaces the pending transaction for an instance.
func (s *DeviceStore) Put(transaction DeviceTransaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]DeviceTransaction)
	}
	s.values[transaction.InstanceID] = transaction
}

// Get returns the pending transaction for an instance. Expired transactions
// are removed and reported as absent.
func (s *DeviceStore) Get(instanceID string) (DeviceTransaction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	transaction, ok := s.values[instanceID]
	if !ok {
		return DeviceTransaction{}, false
	}
	if transaction.Expired(s.now()) {
		delete(s.values, instanceID)
		return DeviceTransaction{}, false
	}
	return transaction, true
}

// PurgeInstance removes any pending device transaction for instanceID.
func (s *DeviceStore) PurgeInstance(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, instanceID)
}

type deviceCodeResponse struct {
	DeviceAuthID string          `json:"device_auth_id"`
	UserCode     string          `json:"user_code"`
	Interval     json.RawMessage `json:"interval"`
}

type deviceAuthorizationResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

// DeviceURL returns the provider page used to enter a device code.
func (c Config) DeviceURL() string {
	c = c.WithDefaults()
	return c.Issuer + DeviceURLPath
}

// DeviceRedirectURI returns the redirect URI used by the provider's device
// authorization exchange. It is not a browser callback served by the gateway.
func (c Config) DeviceRedirectURI() string {
	c = c.WithDefaults()
	return c.Issuer + DeviceRedirectPath
}

// RequestDeviceCode starts the OpenCode-compatible headless authorization
// transaction. The response contains only the user-facing code and opaque
// server-side provider transaction id.
func (c Config) RequestDeviceCode(ctx context.Context, client *http.Client) (*DeviceCode, error) {
	c = c.WithDefaults()
	body, err := json.Marshal(map[string]string{"client_id": c.ClientID})
	if err != nil {
		return nil, errf("device.start", ErrInvalidTokenResponse, "failed to build device authorization request")
	}
	resp, err := c.deviceRequest(ctx, client, http.MethodPost, c.Issuer+DeviceUserCodePath, body)
	if err != nil {
		return nil, err
	}
	var raw deviceCodeResponse
	if err := json.Unmarshal(resp, &raw); err != nil || raw.DeviceAuthID == "" || raw.UserCode == "" {
		return nil, errf("device.start", ErrInvalidTokenResponse, "device authorization response is invalid")
	}
	interval := parseDeviceInterval(raw.Interval)
	return &DeviceCode{DeviceAuthID: raw.DeviceAuthID, UserCode: raw.UserCode, Interval: interval}, nil
}

// PollDeviceAuthorization checks whether the user has completed the device
// authorization. pending is true for the provider's documented 403/404
// pending responses; callers should wait for DeviceCode.Interval and retry.
func (c Config) PollDeviceAuthorization(ctx context.Context, client *http.Client, device DeviceCode) (*DeviceAuthorization, bool, error) {
	c = c.WithDefaults()
	if device.DeviceAuthID == "" || device.UserCode == "" {
		return nil, false, errf("device.poll", ErrInvalidTokenResponse, "device authorization state is invalid")
	}
	body, err := json.Marshal(map[string]string{
		"device_auth_id": device.DeviceAuthID,
		"user_code":      device.UserCode,
	})
	if err != nil {
		return nil, false, errf("device.poll", ErrInvalidTokenResponse, "failed to build device authorization request")
	}
	response, status, err := c.rawDeviceRequest(ctx, client, http.MethodPost, c.Issuer+DeviceTokenPath, body)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		return nil, true, nil
	}
	if status < 200 || status > 299 {
		return nil, false, deviceEndpointError(status)
	}
	var raw deviceAuthorizationResponse
	if err := json.Unmarshal(response, &raw); err != nil || raw.AuthorizationCode == "" {
		return nil, false, errf("device.poll", ErrInvalidTokenResponse, "device authorization response is invalid")
	}
	if err := ValidateVerifier(raw.CodeVerifier); err != nil {
		return nil, false, errf("device.poll", ErrInvalidVerifier, "device authorization returned an invalid PKCE verifier")
	}
	return &DeviceAuthorization{AuthorizationCode: raw.AuthorizationCode, CodeVerifier: raw.CodeVerifier}, false, nil
}

func (c Config) deviceRequest(ctx context.Context, client *http.Client, method, endpoint string, body []byte) ([]byte, error) {
	response, status, err := c.rawDeviceRequest(ctx, client, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, deviceEndpointError(status)
	}
	return response, nil
}

func (c Config) rawDeviceRequest(ctx context.Context, client *http.Client, method, endpoint string, body []byte) ([]byte, int, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, errf("device.request", ErrTokenEndpoint, "failed to build device authorization request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, &Error{Op: "device.request", Msg: "device authorization request failed", Err: ErrTokenEndpoint}
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, errf("device.request", ErrInvalidTokenResponse, "failed to read device authorization response")
	}
	return response, resp.StatusCode, nil
}

func parseDeviceInterval(raw json.RawMessage) time.Duration {
	if len(raw) == 0 {
		return 5 * time.Second
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if seconds, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	var seconds int
	if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 5 * time.Second
}

func deviceEndpointError(status int) error {
	return &Error{Op: "device.endpoint", Msg: fmt.Sprintf("device endpoint returned HTTP %d", status), Err: ErrTokenEndpoint}
}
