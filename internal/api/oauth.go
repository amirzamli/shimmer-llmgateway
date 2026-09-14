package api

// ChatGPT OAuth account surface (plan phase 2b): start either the browser
// redirect or device-code sign-in, inspect the connection state, and
// disconnect. All OAuth operations require an OAuth-marked template instance;
// API-key instances keep their unchanged paths. Pending state and PKCE/device
// values stay server-side, short-lived, and bound to the instance that started
// the flow; only the browser callback or the device status poll can persist the
// credential under that instance. Token material never appears in responses,
// logs, or the callback page.

import (
	"fmt"
	"html"
	"net/http"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
)

// oauthStateTTL bounds a pending authorization transaction: short-lived per
// the plan, so an abandoned browser tab cannot complete a stale sign-in
// later. A gateway restart discards all pending transactions anyway.
const oauthStateTTL = 10 * time.Minute
const oauthDeviceTTL = oauthStateTTL

// requireOAuthInstance resolves alias to an instance served by an
// OAuth-marked template (the ChatGPT browser/device flows). Unknown instances
// are a 404; instances whose template is not OAuth-marked are rejected with
// INVALID_ARGUMENT so the OAuth surface can never touch an API-key instance.
func (a *API) requireOAuthInstance(alias string) (*config.Instance, error) {
	cfg := a.mgr.Get()
	inst, ok := cfg.Instance(alias)
	if !ok {
		return nil, &apiError{status: http.StatusNotFound, code: "NOT_FOUND", message: fmt.Sprintf("instance %q not found", alias)}
	}
	tpl, ok := cfg.Templates[inst.Template]
	if !ok || !tpl.OAuth {
		return nil, badRequest("instance %q is not a ChatGPT OAuth instance", alias)
	}
	return inst, nil
}

// handleOAuthStart begins the ChatGPT Plus browser sign-in for an OAuth
// instance: it mints a short-lived, single-use, instance-bound state/PKCE
// transaction and returns the verified provider authorization URL (with its
// expiry) for the caller to open. A new start supersedes any older pending
// flow for the same instance, so a stale browser tab can never complete a
// superseded sign-in.
func (a *API) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !a.requireOAuthSourceMessage(w, r, "oauth endpoint requires loopback or a configured listener") {
		return
	}
	alias := r.PathValue("alias")
	a.updateMu.Lock()
	if _, err := a.requireOAuthInstance(alias); err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}
	generation := a.oauthLife.Begin(alias)
	a.oauthStates.PurgeInstance(alias)
	a.oauthDevices.PurgeInstance(alias)
	st, err := a.oauthStates.GenerateWithGeneration(alias, oauthStateTTL, generation)
	if err != nil {
		a.updateMu.Unlock()
		a.logger.Error("oauth_state_generate_failed", map[string]any{"alias": alias, "error": err.Error()})
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to start the sign-in flow")
		return
	}
	authURL, err := a.oauthCfg.AuthorizeURL(st)
	if err != nil {
		a.updateMu.Unlock()
		a.logger.Error("oauth_authorize_url_failed", map[string]any{"alias": alias, "error": err.Error()})
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to build the authorization URL")
		return
	}
	a.updateMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"authorization_url": authURL,
		"expires_in":        int(oauthStateTTL / time.Second),
	})
}

// handleOAuthDeviceStart begins OpenCode's headless/device sign-in. The
// provider user code is returned to the dashboard, while the provider device
// id remains in the gateway's in-memory transaction store. No localhost
// callback is involved; the dashboard polls handleOAuthDeviceStatus while the
// user enters the code at the provider page.
func (a *API) handleOAuthDeviceStart(w http.ResponseWriter, r *http.Request) {
	if !a.requireOAuthSourceMessage(w, r, "oauth endpoint requires loopback or a configured listener") {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	alias := r.PathValue("alias")
	a.updateMu.Lock()
	if _, err := a.requireOAuthInstance(alias); err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}
	generation := a.oauthLife.Begin(alias)
	a.oauthStates.PurgeInstance(alias)
	a.oauthDevices.PurgeInstance(alias)
	a.updateMu.Unlock()

	device, err := a.oauthCfg.RequestDeviceCode(r.Context(), a.oauthClient)
	if err != nil {
		a.logger.Warn("oauth_device_start_failed", map[string]any{"alias": alias, "error": oauth.Redact(err).Error()})
		a.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "the provider could not start device sign-in")
		return
	}
	if device.Interval <= 0 {
		device.Interval = 5 * time.Second
	}
	a.updateMu.Lock()
	if !a.oauthLife.Current(alias, generation) {
		a.updateMu.Unlock()
		a.writeError(w, http.StatusConflict, "CONFLICT", "the sign-in was superseded by a newer account change")
		return
	}
	if _, err := a.requireOAuthInstance(alias); err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}
	a.oauthDevices.Put(oauth.DeviceTransaction{
		InstanceID:   alias,
		Generation:   generation,
		DeviceAuthID: device.DeviceAuthID,
		UserCode:     device.UserCode,
		Interval:     device.Interval,
		ExpiresAt:    time.Now().Add(oauthDeviceTTL),
	})
	a.updateMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"authorization_url": a.oauthCfg.DeviceURL(),
		"user_code":         device.UserCode,
		"interval":          int(device.Interval / time.Second),
		"expires_in":        int(oauthDeviceTTL / time.Second),
	})
}

// handleOAuthStatus reports the connection state of an OAuth instance:
// whether a credential is stored and, when it is, the masked account
// identifier and the access-token expiry. Token material is never returned.
func (a *API) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireOAuthSourceMessage(w, r, "oauth endpoint requires loopback or a configured listener") {
		return
	}
	alias := r.PathValue("alias")
	a.updateMu.Lock()
	if _, err := a.requireOAuthInstance(alias); err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}
	a.updateMu.Unlock()
	payload, err := a.oauthStatusPayload(alias)
	if err != nil {
		a.logger.Error("oauth_record_corrupted", map[string]any{"alias": alias, "error": err.Error()})
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "the stored sign-in credential is corrupted")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleOAuthDeviceStatus advances a pending device transaction by one
// provider poll and persists the credential when the user has completed the
// browser step. A dashboard can safely poll this route at the provider's
// returned interval; pending provider responses never become gateway errors.
func (a *API) handleOAuthDeviceStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireOAuthSourceMessage(w, r, "oauth endpoint requires loopback or a configured listener") {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	alias := r.PathValue("alias")
	a.updateMu.Lock()
	if _, err := a.requireOAuthInstance(alias); err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}
	transaction, pending := a.oauthDevices.Get(alias)
	a.updateMu.Unlock()
	if !pending || !a.oauthLife.Current(alias, transaction.Generation) {
		if pending {
			a.oauthDevices.PurgeInstance(alias)
		}
		a.writeOAuthStatus(w, alias)
		return
	}

	a.oauthDevicePollMu.Lock()
	defer a.oauthDevicePollMu.Unlock()
	authorization, waiting, err := a.oauthCfg.PollDeviceAuthorization(r.Context(), a.oauthClient, oauth.DeviceCode{
		DeviceAuthID: transaction.DeviceAuthID,
		UserCode:     transaction.UserCode,
		Interval:     transaction.Interval,
	})
	if err != nil {
		a.oauthDevices.PurgeInstance(alias)
		a.logger.Warn("oauth_device_poll_failed", map[string]any{"alias": alias, "error": oauth.Redact(err).Error()})
		a.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "the provider rejected device sign-in")
		return
	}
	if waiting {
		seconds := int(time.Until(transaction.ExpiresAt).Seconds())
		if seconds < 1 {
			a.oauthDevices.PurgeInstance(alias)
			a.writeOAuthStatus(w, alias)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"connected": false, "pending": true, "expires_in": seconds})
		return
	}

	tok, err := a.oauthCfg.ExchangeWithRedirect(r.Context(), a.oauthClient, authorization.AuthorizationCode, authorization.CodeVerifier, a.oauthCfg.DeviceRedirectURI())
	a.oauthDevices.PurgeInstance(alias)
	if err != nil {
		a.logger.Warn("oauth_device_exchange_failed", map[string]any{"alias": alias, "error": oauth.Redact(err).Error()})
		a.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "the provider rejected device sign-in")
		return
	}
	if !a.oauthLife.Current(alias, transaction.Generation) {
		a.logger.Warn("oauth_device_superseded", map[string]any{"alias": alias})
		a.writeError(w, http.StatusConflict, "CONFLICT", "the sign-in was superseded by a newer account change")
		return
	}
	if tok.AccountID == "" {
		a.logger.Warn("oauth_device_no_account_id", map[string]any{"alias": alias})
		a.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "the provider did not return an account identifier")
		return
	}
	cred := secrets.OAuthCredential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		AccountID:    tok.AccountID,
	}
	current, err := a.oauthLife.IfCurrent(alias, transaction.Generation, func() error {
		return a.sec.SetOAuth(alias, cred)
	})
	if err != nil {
		a.logger.Error("oauth_device_persist_failed", map[string]any{"alias": alias, "error": err.Error()})
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "the gateway could not store the sign-in credential")
		return
	}
	if !current {
		a.logger.Warn("oauth_device_superseded", map[string]any{"alias": alias})
		a.writeError(w, http.StatusConflict, "CONFLICT", "the sign-in was superseded by a newer account change")
		return
	}
	a.logger.Info("oauth_connected", map[string]any{"alias": alias, "method": "device"})
	payload := map[string]any{
		"connected":  true,
		"account_id": maskAccountID(cred.AccountID),
		"expires_at": cred.ExpiresAt,
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *API) oauthStatusPayload(alias string) (map[string]any, error) {
	cred, ok, err := a.sec.GetOAuth(alias)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]any{"connected": false}, nil
	}
	return map[string]any{
		"connected":  true,
		"account_id": maskAccountID(cred.AccountID),
		"expires_at": cred.ExpiresAt,
	}, nil
}

func (a *API) writeOAuthStatus(w http.ResponseWriter, alias string) {
	payload, err := a.oauthStatusPayload(alias)
	if err != nil {
		a.logger.Error("oauth_record_corrupted", map[string]any{"alias": alias, "error": err.Error()})
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "the stored sign-in credential is corrupted")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleOAuthDisconnect removes the stored OAuth credential of an OAuth
// instance and retires any pending sign-in flow for it, so a browser tab
// still on the provider page cannot reconnect a disconnected instance. The
// instance itself stays configured; only the credential is dropped.
// Idempotent: disconnecting an already-disconnected instance is a 204.
func (a *API) handleOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	if !a.requireOAuthSourceMessage(w, r, "oauth endpoint requires loopback or a configured listener") {
		return
	}
	alias := r.PathValue("alias")
	a.updateMu.Lock()
	if _, err := a.requireOAuthInstance(alias); err != nil {
		a.updateMu.Unlock()
		a.writeUpdateError(w, err)
		return
	}
	err := a.oauthLife.WithTransition([]string{alias}, func() (bool, error) {
		if err := a.sec.Delete(alias); err != nil {
			return false, err
		}
		a.oauthStates.PurgeInstance(alias)
		a.oauthDevices.PurgeInstance(alias)
		return true, nil
	})
	a.updateMu.Unlock()
	if err != nil {
		a.logger.Error("secrets_write_failed", map[string]any{"alias": alias, "error": err.Error()})
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to disconnect the account")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleOAuthCallback completes the ChatGPT browser sign-in. It is served on
// the fixed loopback callback path /auth/callback (the verified
// http://localhost:1455/auth/callback contract, mounted only by the gateway's
// dedicated callback handler), which the provider redirects the browser to. It validates the
// callback against the pending state (single-use; unknown, expired, and
// replayed states are all rejected), exchanges the authorization code with
// the server-side PKCE verifier, and persists the encrypted credential under
// the state's bound instance. The response is a minimal HTML page — never
// JSON — and the code, state, verifier, and tokens never appear in the page,
// the logs, or any error.
func (a *API) HandleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	stateValue := q.Get("state")
	var st *oauth.State
	var stateErr error
	if stateValue != "" {
		// Consume before examining provider errors or code presence. Every
		// callback carrying a real state is single-use, including cancellation
		// and malformed provider callbacks.
		st, stateErr = a.oauthStates.Consume(stateValue, "")
	}
	if providerErr := q.Get("error"); providerErr != "" {
		// The provider rejected or cancelled the authorization (e.g.
		// access_denied). The error and description are untrusted callback
		// input, so neither is logged or echoed.
		if stateValue == "" || stateErr != nil {
			a.logger.Warn("oauth_callback_provider_state_rejected", nil)
			a.writeOAuthCallbackPage(w, http.StatusBadRequest, "The sign-in link is invalid, expired, or was already used. You can close this window and start the sign-in again.")
			return
		}
		a.logger.Warn("oauth_callback_provider_rejected", nil)
		a.writeOAuthCallbackPage(w, http.StatusOK, "ChatGPT sign-in was cancelled or rejected by the provider. You can close this window and try again.")
		return
	}
	if stateValue == "" || stateErr != nil || q.Get("code") == "" {
		if stateErr != nil {
			a.logger.Warn("oauth_callback_state_rejected", map[string]any{"error": oauth.Redact(stateErr).Error()})
		}
		a.writeOAuthCallbackPage(w, http.StatusBadRequest, "The sign-in callback is missing required parameters. You can close this window and start the sign-in again.")
		return
	}
	alias := st.InstanceID
	if !a.oauthLife.Current(alias, st.Generation) {
		a.logger.Warn("oauth_callback_superseded", map[string]any{"alias": alias})
		a.writeOAuthCallbackPage(w, http.StatusBadRequest, "This sign-in was superseded by a newer account change. You can close this window and start the sign-in again.")
		return
	}
	if _, err := a.requireOAuthInstance(alias); err != nil {
		// The instance was deleted or its template changed while the browser
		// was away; the consumed state is not retryable, which is correct.
		a.logger.Warn("oauth_callback_instance_rejected", map[string]any{"alias": alias})
		a.writeOAuthCallbackPage(w, http.StatusBadRequest, "The gateway instance for this sign-in no longer exists. You can close this window.")
		return
	}
	tok, err := a.oauthCfg.Exchange(r.Context(), a.oauthClient, q.Get("code"), st.Verifier)
	if err != nil {
		// oauth errors are already redacted; Redact also wraps any other
		// error defensively so no token or code material can reach the log.
		a.logger.Warn("oauth_callback_exchange_failed", map[string]any{"error": oauth.Redact(err).Error()})
		a.writeOAuthCallbackPage(w, http.StatusBadGateway, "The provider rejected the sign-in. You can close this window and try again.")
		return
	}
	if !a.oauthLife.Current(alias, st.Generation) {
		a.logger.Warn("oauth_callback_superseded", map[string]any{"alias": alias})
		a.writeOAuthCallbackPage(w, http.StatusBadRequest, "This sign-in was superseded by a newer account change. You can close this window and start the sign-in again.")
		return
	}
	if tok.AccountID == "" {
		// The verified account identity is part of the credential model and
		// the upstream identity headers; a token response with no account
		// claim is rejected rather than stored without an identity.
		a.logger.Warn("oauth_callback_no_account_id", map[string]any{"alias": alias})
		a.writeOAuthCallbackPage(w, http.StatusBadGateway, "The provider did not return an account identifier. You can close this window and try again.")
		return
	}
	cred := secrets.OAuthCredential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		AccountID:    tok.AccountID,
	}
	current, err := a.oauthLife.IfCurrent(alias, st.Generation, func() error {
		return a.sec.SetOAuth(alias, cred)
	})
	if err != nil {
		a.logger.Error("oauth_callback_persist_failed", map[string]any{"alias": alias, "error": err.Error()})
		a.writeOAuthCallbackPage(w, http.StatusInternalServerError, "The gateway could not store the sign-in credential. You can close this window and try again.")
		return
	}
	if !current {
		a.logger.Warn("oauth_callback_superseded", map[string]any{"alias": alias})
		a.writeOAuthCallbackPage(w, http.StatusBadRequest, "This sign-in was superseded by a newer account change. You can close this window and start the sign-in again.")
		return
	}
	a.logger.Info("oauth_connected", map[string]any{"alias": alias})
	a.writeOAuthCallbackPage(w, http.StatusOK, "ChatGPT sign-in successful. You can close this window and return to the gateway.")
}

// writeOAuthCallbackPage renders the minimal browser page for the fixed
// loopback callback. message is always a fixed string — the page never
// carries tokens, codes, or state values — and html.EscapeString defends
// against any future message with dynamic content.
func (a *API) writeOAuthCallbackPage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>ChatGPT sign-in</title></head><body><p>%s</p></body></html>\n", html.EscapeString(message))
}

// maskAccountID renders a ChatGPT account identifier for display, keeping the
// first and last four characters. Very short identifiers are masked entirely
// so the display never reveals the value.
func maskAccountID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return "••••••"
	}
	return id[:4] + "…" + id[len(id)-4:]
}
