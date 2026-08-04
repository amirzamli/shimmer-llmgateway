package api

import (
	"net/http"
)

// handleMasterKeyGet reports the one-time exposure of a gateway-generated
// secrets master key (SHIMMER_MASTER_KEY unset on first run). A user-provided
// key is never exposed: the endpoint returns 404 unless the gateway generated
// a key that has not yet been acknowledged. The no-store headers keep browsers
// and proxies from caching the key.
func (a *API) handleMasterKeyGet(w http.ResponseWriter, r *http.Request) {
	// The key must never be cached by a browser or proxy.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	key, ok := a.sec.GeneratedMasterKey()
	if !ok {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", "no unacknowledged generated master key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"generated": true, "key": key})
}

// handleMasterKeyAck acknowledges a generated master key, clearing its
// one-time exposure. When a legacy plaintext migration is pending, the secrets
// file is encrypted and persisted first; on persist failure the exposure copy
// is left intact (the caller can retry) and a 500 is returned instead of 204.
func (a *API) handleMasterKeyAck(w http.ResponseWriter, r *http.Request) {
	if err := a.sec.AckMasterKey(); err != nil {
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "persist secrets file: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
