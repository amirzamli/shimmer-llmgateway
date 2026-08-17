package api

import "net/http"

// handleQuota serves the per-instance quota/balance results for the UI
// (internal/quota). ?refresh=1 bypasses the server-side TTL cache.
func (a *API) handleQuota(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	writeJSON(w, http.StatusOK, map[string]any{"quota": a.quota.List(r.Context(), refresh)})
}
