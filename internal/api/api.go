// Package api implements the §6.2 REST surface: templates and instances
// (add/edit/disable/delete), settings, sessions (read-only), status, and the
// one-time generated master-key surface (expose + acknowledge).
//
// Config mutations persist by write-back to gateway.toml (atomic temp-file +
// rename, resolved review decision #5) and hot-swap through the ConfigManager,
// so the next request observes the new config with no restart. Trace data is
// read-only; the only writes are config mutations and the UI-managed secrets
// file (<store>.secrets.json, mode 0600). Keys are never written to
// gateway.toml and are returned masked.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"shimmer-llmgateway/internal/config"
	"shimmer-llmgateway/internal/logging"
	"shimmer-llmgateway/internal/quota"
	"shimmer-llmgateway/internal/secrets"
	"shimmer-llmgateway/internal/store"
)

// apiError is a handler error with its HTTP mapping.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

// badRequest builds a 400 INVALID_ARGUMENT error for rejected input.
func badRequest(format string, args ...any) error {
	return &apiError{status: http.StatusBadRequest, code: "INVALID_ARGUMENT", message: fmt.Sprintf(format, args...)}
}

// API is the §6.2 REST handler set.
type API struct {
	mgr    *config.ConfigManager
	path   string // gateway.toml path for write-back
	store  *store.Store
	sec    *secrets.Store
	logger *logging.Logger
	// updateMu serializes read-clone-modify-swap so concurrent PATCH/POST
	// mutations cannot silently drop each other's changes (each update clones
	// the live config; without serialization a faster writer could clone a
	// stale base and its Swap would clobber the other's result).
	updateMu  sync.Mutex
	startedAt time.Time
	// modelsMu guards modelsCache, the in-memory provider model-fetch cache
	// keyed by instance alias (TTL 5 min; invalidated on PATCH/DELETE), and
	// modelsFlight, the in-flight fetches that dedupe concurrent misses.
	modelsMu     sync.Mutex
	modelsCache  map[string]modelsCacheEntry
	modelsFlight map[string]*modelsFlightCall
	// quota fetches per-provider account quota/balance for the UI with its
	// own getter-based TTL cache (internal/quota; invalidated on PATCH/DELETE).
	quota *quota.Fetcher
}

// New builds the API handler set over the live config manager, the capture
// store, and the secrets store. configPath is the gateway.toml path written
// back on config mutations.
func New(mgr *config.ConfigManager, configPath string, st *store.Store, sec *secrets.Store, logger *logging.Logger) *API {
	return &API{mgr: mgr, path: configPath, store: st, sec: sec, logger: logger, startedAt: time.Now(), modelsCache: map[string]modelsCacheEntry{}, modelsFlight: map[string]*modelsFlightCall{}, quota: quota.New(mgr.Get, sec)}
}

// Handler returns the §6.2 router.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/templates", a.handleTemplatesList)
	mux.HandleFunc("POST /api/templates", a.handleTemplatesCreate)
	mux.HandleFunc("GET /api/instances", a.handleInstancesList)
	mux.HandleFunc("POST /api/instances", a.handleInstancesCreate)
	mux.HandleFunc("PATCH /api/instances/{alias}", a.handleInstancePatch)
	mux.HandleFunc("DELETE /api/instances/{alias}", a.handleInstanceDelete)
	mux.HandleFunc("GET /api/instances/{alias}/models", a.handleInstanceModels)
	mux.HandleFunc("GET /api/quota", a.handleQuota)
	mux.HandleFunc("GET /api/settings", a.handleSettingsGet)
	mux.HandleFunc("PATCH /api/settings", a.handleSettingsPatch)
	mux.HandleFunc("GET /api/sessions", a.handleSessionsList)
	mux.HandleFunc("GET /api/sessions/{id}", a.handleSessionGet)
	mux.HandleFunc("GET /api/sessions/{id}/export", a.handleSessionExport)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/secrets/master-key", a.handleMasterKeyGet)
	mux.HandleFunc("POST /api/secrets/master-key/ack", a.handleMasterKeyAck)
	// Any other /api/* path or method is a 404 JSON error, never an HTML
	// "page not found".
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": "NOT_FOUND", "message": "unknown api endpoint: " + r.URL.Path})
	})
	return mux
}

// update clones the live config, applies fn to the clone, persists the clone
// to gateway.toml (atomic temp+rename), and hot-swaps it in. Mutations are
// serialized so concurrent writes observe each other's results (no lost
// updates).
func (a *API) update(fn func(*config.Config) error) error {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	c := a.mgr.Get().Clone()
	if err := fn(c); err != nil {
		return err
	}
	return a.mgr.Update(a.path, c)
}

// writeUpdateError maps an update failure to the {code, message} shape:
// validation errors and explicit apiErrors keep their status, everything else
// is a 500.
func (a *API) writeUpdateError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.status, map[string]any{"code": ae.code, "message": ae.message})
		return
	}
	if config.IsValidationError(err) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "INVALID_ARGUMENT", "message": err.Error()})
		return
	}
	a.logger.Error("config_write_failed", map[string]any{"path": a.path, "error": err.Error()})
	writeJSON(w, http.StatusInternalServerError, map[string]any{"code": "INTERNAL", "message": err.Error()})
}

// writeError writes a JSON error with the {code, message} convention.
func (a *API) writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"code": code, "message": message})
	a.logger.Warn("api_request_rejected", map[string]any{"status": status, "code": code, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
