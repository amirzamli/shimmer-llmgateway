package api

// ChatGPT OAuth account surface: start device-code sign-in, inspect the
// connection state, and disconnect. All OAuth operations require an
// OAuth-marked template instance; API-key instances keep their unchanged
// paths. Pending device values stay server-side, short-lived, and bound to the
// instance that started the flow. Token material never appears in responses
// or logs.

import (
	"fmt"
	"net/http"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
)

// oauthDeviceTTL bounds a pending device authorization transaction. A gateway
// restart discards all pending transactions.
const oauthDeviceTTL = 10 * time.Minute

// requireOAuthInstance resolves alias to an instance served by an
// OAuth-marked template (the ChatGPT device flow). Unknown instances
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
// provider approval step. A dashboard can safely poll this route at the provider's
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

	tok, err := a.oauthCfg.Exchange(r.Context(), a.oauthClient, authorization.AuthorizationCode, authorization.CodeVerifier)
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
// instance and retires any pending sign-in flow for it, so a provider page
// still open cannot reconnect a disconnected instance. The
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
