package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
)

// Model-fetch tuning: the provider /models list is cached in-memory for
// modelsCacheTTL (no persistence, no timers beyond the TTL), the client has a
// bounded timeout, and the response body is capped so an abusive provider
// cannot exhaust memory.
const (
	modelsCacheTTL      = 5 * time.Minute
	modelsFetchTimeout  = 10 * time.Second
	modelsResponseLimit = 1 << 20 // 1 MiB
	// modelsGenericError is the only error surfaced to the UI on a failed
	// fetch; the /api/ surface is unauthenticated, so base_url/network detail
	// goes to the gateway log only.
	modelsGenericError   = "provider models unavailable"
	modelsSourceProvider = "provider"
	modelsSourceConfig   = "config"
)

// modelsClient fetches provider /models endpoints with a bounded timeout and
// redirect-following disabled: a redirecting or malicious provider base_url
// must not bounce the /models fetch to an internal endpoint.
var modelsClient = &http.Client{
	Timeout: modelsFetchTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// instanceModels is the GET /api/instances/{alias}/models response. Source is
// "provider" when the list came from a live fetch and "config" when the fetch
// failed (or was skipped) and the configured models were returned. Error is
// the generic string above, present only on an actual fetch failure.
type instanceModels struct {
	Alias     string    `json:"alias"`
	Models    []string  `json:"models"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}

// modelsCacheEntry is a cached fetch result keyed by instance alias. The
// generic error is cached too, so a failing provider is not re-hammered on
// every request within the TTL.
type modelsCacheEntry struct {
	models    []string
	fetchedAt time.Time
	err       string
}

// modelsFlightCall is one in-flight /models fetch for an alias. Concurrent
// callers that arrive while it runs wait on done and reuse its entry instead of
// duplicating the upstream call. The entry is set before done is closed.
type modelsFlightCall struct {
	done  chan struct{}
	entry modelsCacheEntry
}

// handleInstanceModels serves the provider model list for an instance, from the
// TTL cache when fresh unless ?refresh=1 bypasses it. Disabled instances are
// served too (the UI needs the list to re-enable them); an unknown alias is a
// 404 NOT_FOUND.
func (a *API) handleInstanceModels(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	cfg := a.mgr.Get()
	inst, ok := cfg.Instance(alias)
	if !ok {
		a.writeError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("instance %q not found", alias))
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	writeJSON(w, http.StatusOK, a.fetchInstanceModels(cfg, inst, refresh))
}

// fetchInstanceModels returns the model list for an instance, serving the TTL
// cache when fresh (unless refresh) and otherwise fetching <base_url>/models.
// Anthropic-style templates have no OpenAI /models endpoint, so they always
// fall back to the configured models (source "config"). Any fetch failure
// falls back to the configured models with source "config" and the generic
// error string; the underlying detail is logged, never surfaced.
func (a *API) fetchInstanceModels(cfg *config.Config, inst *config.Instance, refresh bool) instanceModels {
	if tmpl, ok := cfg.Templates[inst.Template]; ok && tmpl.Style == config.StyleAnthropic {
		return instanceModels{Alias: inst.Alias, Models: inst.EffectiveModels(cfg), Source: modelsSourceConfig, FetchedAt: time.Now()}
	}
	if !refresh {
		a.modelsMu.Lock()
		if e, ok := a.modelsCache[inst.Alias]; ok && time.Since(e.fetchedAt) < modelsCacheTTL {
			a.modelsMu.Unlock()
			return instanceModels{Alias: inst.Alias, Models: e.models, Source: modelsSourceOf(e.err), FetchedAt: e.fetchedAt, Error: e.err}
		}
		a.modelsMu.Unlock()
	}

	entry := a.flightModelsFetch(cfg, inst)

	a.modelsMu.Lock()
	a.modelsCache[inst.Alias] = entry
	a.modelsMu.Unlock()

	return instanceModels{Alias: inst.Alias, Models: entry.models, Source: modelsSourceOf(entry.err), FetchedAt: entry.fetchedAt, Error: entry.err}
}

// flightModelsFetch is a small singleflight for provider /models fetches:
// the first caller for an alias runs the fetch, later callers wait on the
// shared done channel and reuse its result, so concurrent misses never
// duplicate the upstream call. golang.org/x/sync/singleflight is deliberately
// not imported — a mutex + in-flight map is ~20 lines and avoids a new
// dependency for this one call site.
func (a *API) flightModelsFetch(cfg *config.Config, inst *config.Instance) modelsCacheEntry {
	a.modelsMu.Lock()
	if c, ok := a.modelsFlight[inst.Alias]; ok {
		a.modelsMu.Unlock()
		<-c.done
		return c.entry
	}
	c := &modelsFlightCall{done: make(chan struct{})}
	a.modelsFlight[inst.Alias] = c
	a.modelsMu.Unlock()

	models, fetchedAt, err := a.fetchProviderModels(cfg, inst)
	entry := modelsCacheEntry{models: models, fetchedAt: fetchedAt, err: err}
	if err != "" {
		entry.models = inst.EffectiveModels(cfg)
	}
	c.entry = entry
	close(c.done)

	a.modelsMu.Lock()
	delete(a.modelsFlight, inst.Alias)
	a.modelsMu.Unlock()
	return entry
}

// invalidateModels drops the cached fetch result for an alias (called after a
// successful PATCH or DELETE so a recycled or reconfigured alias never serves a
// stale provider list).
func (a *API) invalidateModels(alias string) {
	a.modelsMu.Lock()
	delete(a.modelsCache, alias)
	a.modelsMu.Unlock()
}

// fetchProviderModels GETs <base_url>/models and parses the OpenAI list shape
// {"data":[{"id":"..."}]}, trimming each id. It returns the fetched ids, the
// fetch time, and the generic error string ("" on success). All failure detail
// is logged here and replaced with the generic string for the caller.
func (a *API) fetchProviderModels(cfg *config.Config, inst *config.Instance) ([]string, time.Time, string) {
	tmpl, ok := cfg.Templates[inst.Template]
	if !ok {
		a.logger.Error("models_fetch_no_template", map[string]any{"alias": inst.Alias, "template": inst.Template})
		return nil, time.Now(), modelsGenericError
	}
	// Mirror the gateway's buildUpstream URL construction.
	url := strings.TrimRight(tmpl.BaseURL, "/") + "/models"

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		a.logger.Error("models_fetch_build_request", map[string]any{"alias": inst.Alias, "url": url, "error": err.Error()})
		return nil, time.Now(), modelsGenericError
	}
	// Key precedence mirrors the gateway's resolvedKey: the api_key_env env var
	// first, then the secrets file. An empty key is legitimate for keyless
	// providers (ollama/vllm) — the fetch proceeds WITHOUT an Authorization
	// header, and a keyless-only skip is not surfaced as an error.
	if key := a.fetchKey(cfg, inst); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// No inbound client exists on this path, so the probe always identifies
	// itself with the gateway's constant UA instead of Go's "Go-http-client/1.1".
	req.Header.Set("User-Agent", config.UserAgent)

	resp, err := modelsClient.Do(req)
	if err != nil {
		a.logger.Error("models_fetch_request_failed", map[string]any{"alias": inst.Alias, "url": url, "error": err.Error()})
		return nil, time.Now(), modelsGenericError
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		a.logger.Error("models_fetch_non_200", map[string]any{"alias": inst.Alias, "url": url, "status": resp.StatusCode})
		return nil, time.Now(), modelsGenericError
	}

	// Cap the response body at ~1 MiB, reading one extra byte to detect
	// truncation before decoding.
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsResponseLimit+1))
	if err != nil {
		a.logger.Error("models_fetch_read_failed", map[string]any{"alias": inst.Alias, "url": url, "error": err.Error()})
		return nil, time.Now(), modelsGenericError
	}
	if len(body) > modelsResponseLimit {
		a.logger.Error("models_fetch_body_too_large", map[string]any{"alias": inst.Alias, "url": url})
		return nil, time.Now(), modelsGenericError
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		a.logger.Error("models_fetch_parse_failed", map[string]any{"alias": inst.Alias, "url": url, "error": err.Error()})
		return nil, time.Now(), modelsGenericError
	}
	ids := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		if id := strings.TrimSpace(d.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, time.Now(), ""
}

// fetchKey returns the provider key for an instance with §6.2 precedence: the
// api_key_env environment variable first, then the UI-managed secrets file. An
// empty result means the provider is keyless and the fetch proceeds without an
// Authorization header.
func (a *API) fetchKey(cfg *config.Config, inst *config.Instance) string {
	key, _ := a.sec.ResolveKey(inst.EffectiveAPIKeyEnv(cfg), inst.Alias)
	return key
}

// modelsSourceOf maps a cached error to the response source: a successful
// fetch is "provider", anything else is the "config" fallback.
func modelsSourceOf(err string) string {
	if err == "" {
		return modelsSourceProvider
	}
	return modelsSourceConfig
}
