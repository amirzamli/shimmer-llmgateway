// Package gateway implements the §4 HTTP surface: /healthz, /v1/models, and
// the /v1/chat/completions capture surface (stream + non-stream) with §4.4
// streaming correctness (line-by-line forward with per-chunk flush,
// index-stable tool-call delta reassembly, truncation on disconnect), §4.3
// session correlation, one capture transaction per request (resolved review
// decision #6), the §8 append JSONL log, and a one-line JSON access log for
// every HTTP request (method, path, status, duration).
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/api"
	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/oauth"
	"github.com/amirzamli/shimmer-llmgateway/internal/plugins"
	"github.com/amirzamli/shimmer-llmgateway/internal/pricing"
	"github.com/amirzamli/shimmer-llmgateway/internal/secrets"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
	"github.com/amirzamli/shimmer-llmgateway/web"
)

// endpoint is the gateway-facing capture surface path recorded on every row.
const endpoint = "/v1/chat/completions"

// noRedirect is the CheckRedirect policy for the outbound provider client:
// return the 3xx response as-is instead of following it, so a redirecting
// base_url can never redirect a request to an internal endpoint.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// providerTransport bounds the outbound provider connections. ResponseHeaderTimeout is
// the key limit: it only bounds the wait for the first response headers, after
// which a long-running chat stream runs to completion. A total
// http.Client.Timeout is deliberately NOT set — it would impose a hard
// deadline on the whole exchange and cut off legitimate long-running streams
// (and their SSE chunk flow).
var providerTransport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	IdleConnTimeout:       90 * time.Second,
}

var errOAuthEndpoint = errors.New("oauth template must use the fixed ChatGPT Codex endpoint")

// Server is the capture-only gateway. It serves all configured instances;
// config swaps are atomic via the ConfigManager.
type Server struct {
	cfg    *config.ConfigManager
	store  *store.Store
	logger *logging.Logger
	client *http.Client
	append *appendLog
	// secrets holds the UI-managed API keys (<store>.secrets.json, mode 0600);
	// key resolution is env-var-then-file.
	secrets *secrets.Store
	// oauth resolves and refreshes the encrypted OAuth credentials used by
	// OAuth-marked templates (the ChatGPT Plus device flow).
	oauth *OAuthResolver
	// api is the §6.2 REST handler set mounted at /api/.
	api *api.API

	// emitterFactory builds the SSE emit path for a response writer (resolved
	// review decision #1). newStreamEmitter returns the live line-by-line
	// emitter when no response plugins are configured, and the buffered
	// emitter (reassemble → run response filters → emit one SSE block) when
	// they are.
	emitterFactory func(w http.ResponseWriter, source EmitterSource, filter EmitterFilter) Emitter
	// pricing holds the per-model token rates loaded from disk at startup
	// (pricing.DefaultPath) used to estimate the cost of captured traffic
	// (nil when the table failed to load, which disables cost estimation).
	pricing *pricing.Table
	// listenAddrs are the addresses the gateway is being served on; they
	// seed the admin-surface Host allowlist (see guard).
	listenAddrs []string
}

// New builds a gateway server over cfg/st, opening the §8 append log at
// <store>.jsonl, the §6.2 secrets file at <store>.secrets.json (0600), and
// the REST API wired to write config mutations back to configPath. masterKey
// is the decoded AES-256 master key for the secrets file, or nil to have the
// gateway generate one. listenAddrs are the validated listen addresses the
// server will be served on (used by the admin-surface host guard; nil falls
// back to the loopback/CGNAT/ULA address-family policy alone).
func New(cfg *config.ConfigManager, st *store.Store, logger *logging.Logger, configPath string, masterKey []byte, listenAddrs []string) (*Server, error) {
	ap, err := openAppendLog(st.Path() + ".jsonl")
	if err != nil {
		return nil, fmt.Errorf("gateway: open append log %s.jsonl: %w", st.Path(), err)
	}
	// Tie the §8 append-log trim into the store's retention purge so the JSONL
	// never retains data past retention_days (W1). The callback runs under the
	// append log's own lock, so it cannot race with capture appends.
	st.SetJSONLPurger(ap.trim)
	sec, err := secrets.Open(st.Path()+".secrets.json", masterKey)
	if err != nil {
		ap.Close()
		return nil, fmt.Errorf("gateway: open secrets %s.secrets.json: %w", st.Path(), err)
	}
	pt, perr := pricing.Load(pricing.DefaultPath)
	if perr != nil {
		logger.Error("pricing_load_failed", map[string]any{"error": perr.Error()})
	}
	// Refuse to follow upstream redirects: a redirecting or malicious
	// base_url must not bounce the request to an internal endpoint. The
	// shared transport bounds connection/header timeouts without imposing
	// a total request deadline (see providerTransport).
	client := &http.Client{Transport: providerTransport, CheckRedirect: noRedirect}
	life := oauth.NewLifecycle()
	oauthResolver := NewOAuthResolver(sec, client, oauth.Config{}, life)
	apiHandler := api.New(cfg, configPath, st, sec, logger, client, oauthResolver)
	apiHandler.SetOAuthLifecycle(life)
	apiHandler.SetOAuthListenAddrs(listenAddrs)
	return &Server{
		cfg:     cfg,
		store:   st,
		logger:  logger,
		client:  client,
		append:  ap,
		secrets: sec,
		// The OAuth resolver shares the outbound client (redirect policy
		// included) and persists rotated tokens through the secrets store.
		oauth: oauthResolver,
		// The API uses the same no-redirect client for the device sign-in code
		// exchange (a redirecting token endpoint must not bounce the code
		// and PKCE verifier elsewhere).
		api:            apiHandler,
		emitterFactory: newStreamEmitter,
		pricing:        pt,
		listenAddrs:    listenAddrs,
	}, nil
}

// Close releases the gateway's file handles: the §8 append log (the secrets
// file and pricing table carry no persistent handles). Called by main on
// shutdown after the HTTP servers have drained; the store is closed separately
// so its close-time WAL checkpoint runs last.
func (s *Server) Close() error {
	return s.append.Close()
}

// Handler returns the HTTP router. Any /v1/* path other than the capture
// surface and /v1/models returns 404; /healthz is the repo-convention health
// probe; /api/* is the §6.2 REST surface; / serves the embedded HTML UI. The
// router is wrapped in the access-log middleware, so every request is logged.
// The browser-facing surfaces (/api/* and the UI) are additionally wrapped in
// the host/origin guard; the capture surface and /healthz are not — non-browser
// clients address them with whatever Host their base_url carries.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	// Responses API shim (ahead of the /v1/ catch-all): stateless-only,
	// translated to chat and run through the shared pipeline. Retrieval is
	// always a 404 — the gateway stores captures, not Responses state.
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("GET /v1/responses/{id}", s.handleResponsesGet)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.Handle("/api/", s.guard(s.api.Handler()))
	mux.Handle("GET /{$}", s.guard(http.HandlerFunc(s.handleUI)))
	return s.accessLog(mux)
}

// statusRecorder wraps an http.ResponseWriter to capture the response status
// code written (defaulting to 200 when the handler never writes a header) and
// the number of response bytes written. Flush forwards to the underlying
// writer so the streaming path's http.Flusher assertion keeps working — the
// recorder must implement Flush itself because http.ResponseWriter does not
// declare it — and Unwrap exposes the underlying writer's optional interfaces.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// accessLog wraps next and emits one one-line JSON access record per request
// after the handler completes: event "request" with fields {method, path,
// status, bytes, duration_ms, remote_addr}, plus session_id when the request
// carries an X-Session-Id header or the handler echoed a normalized
// X-Gateway-Session-Id response header (chat completions). This is separate
// from — and complementary to — the opt-in redacted request_payloads logging,
// which only fires for the capture surface when settings.log_payloads is on.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		fields := map[string]any{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      rec.status,
			"bytes":       rec.bytes,
			"duration_ms": durationMS(start),
			"remote_addr": r.RemoteAddr,
		}
		if sid := rec.Header().Get("X-Gateway-Session-Id"); sid != "" {
			fields["session_id"] = sid
		} else if sid := r.Header.Get("X-Session-Id"); sid != "" {
			fields["session_id"] = sid
		}
		s.logger.Info("request", fields)
	})
}

// sessionIDFromRequest resolves the §4.3 client-side session id from headers:
// an explicit X-Session-Id always wins; opencode-style clients that send
// their native x-opencode-session header are honored next. (Header lookup is
// case-insensitive, so the x-session-id session-affinity compat spelling is
// matched by the first branch.) An empty result means the caller falls back
// to its own (fresh-UUID) default. Values are NOT normalized here —
// NormalizeSessionID runs at the call site and degrades unsafe or over-length
// values to a fresh UUID.
func sessionIDFromRequest(r *http.Request) string {
	if sid := r.Header.Get("X-Session-Id"); sid != "" {
		return sid
	}
	if sid := r.Header.Get("x-opencode-session"); sid != "" {
		return sid
	}
	return ""
}

// uiDiskPath is checked before the embedded asset: a web/index.html in the
// working directory shadows the copy embedded at build time, so UI edits show
// up on browser refresh without rebuilding or restarting the gateway.
const uiDiskPath = "web/index.html"

// indexHTML caches the embedded single-page UI; it is read from embed.FS once
// (first fallback request) instead of on every request. indexHTML is nil only
// when the embedded asset is missing.
var (
	indexHTMLOnce sync.Once
	indexHTML     []byte
)

// handleUI serves the single-page HTML UI (§6.1): web/index.html from the
// working directory when present (re-read per request, so edits are visible
// after a browser refresh), otherwise the copy embedded at build time.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if data, err := os.ReadFile(uiDiskPath); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
		return
	}
	indexHTMLOnce.Do(func() {
		data, err := web.FS.ReadFile("index.html")
		if err != nil {
			indexHTML = nil
			return
		}
		indexHTML = data
	})
	if indexHTML == nil {
		http.Error(w, "embedded UI missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// modelObject is the OpenAI /v1/models data shape.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleModels returns an OpenAI-shaped model list: every enabled instance's
// effective models as alias/model ids, plus unprefixed ids for the first
// instance listing each model (deduped, per the plan's assumption 6), and the
// instance's model_alias keys as unprefixed and alias/key ids (deduped with the
// same maps, so an alias key that collides with a literal model listed by an
// earlier instance keeps that literal entry). Disabled instances are excluded —
// they are not routable.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	data := []modelObject{}
	seenUnprefixed := map[string]bool{}
	seenAliased := map[string]bool{}
	for _, inst := range cfg.Instances {
		if inst.Disabled {
			continue
		}
		for _, m := range inst.EffectiveModels(cfg) {
			if !seenUnprefixed[m] {
				seenUnprefixed[m] = true
				data = append(data, modelObject{ID: m, Object: "model", OwnedBy: inst.Template})
			}
			id := inst.Alias + "/" + m
			if !seenAliased[id] {
				seenAliased[id] = true
				data = append(data, modelObject{ID: id, Object: "model", OwnedBy: inst.Template})
			}
		}
		for _, key := range sortedMapKeys(inst.ModelAliases) {
			if !seenUnprefixed[key] {
				seenUnprefixed[key] = true
				data = append(data, modelObject{ID: key, Object: "model", OwnedBy: inst.Template})
			}
			id := inst.Alias + "/" + key
			if !seenAliased[id] {
				seenAliased[id] = true
				data = append(data, modelObject{ID: id, Object: "model", OwnedBy: inst.Template})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// sortedMapKeys returns m's keys in sorted order for a deterministic listing.
func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// chatRequest is the subset of the client body the gateway routes on.
type chatRequest struct {
	Model  string `json:"model"`
	Stream *bool  `json:"stream"`
}

// route is the resolved instance/model for one request.
type route struct {
	inst      *config.Instance
	template  *config.Template
	model     string
	alias     string
	provider  string
	reasoning string
}

// clientSurface is the per-request seam between the shared completion
// pipeline and the client-facing API surface a request arrived on. The chat
// surface uses identity (byte-for-byte unchanged behavior); the Responses
// surface records its own endpoint and as-received request bytes for capture,
// and converts 2xx chat bodies to Responses objects for the client only —
// capture keeps the chat-normalized shapes.
type clientSurface struct {
	// endpoint is the capture record's endpoint value.
	endpoint string
	// requestJSON is the as-received client body recorded as request_json.
	requestJSON []byte
	// shapeClient converts a 2xx chat completion body (post response
	// plugins, which always see the chat shape) into the body returned to
	// the client. Error bodies are never shaped.
	shapeClient func([]byte) []byte
	// newEmitter builds the SSE emit path for a streaming request. nil on
	// the chat surface: s.emitterFactory is used unchanged. The Responses
	// surface binds a typed-event translator to the request's resp_ id.
	newEmitter func(w http.ResponseWriter, source EmitterSource, filter EmitterFilter) Emitter
}

// identityBody returns b unchanged — the chat surface's client shaper.
func identityBody(b []byte) []byte { return b }

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// One atomic config snapshot per request: Resolve, the template lookup,
	// and key resolution must all observe the same config (Phase 5 hot-reload
	// swaps the manager atomically, so repeated Get() calls could straddle a
	// swap and resolve an instance against a newer template set).
	cfg := s.cfg.Get()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "read body: "+err.Error())
		return
	}
	var parsed chatRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body: "+err.Error())
		return
	}

	inst, model, err := cfg.Resolve(parsed.Model)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}
	aliasKey := parsed.Model
	if i := strings.IndexByte(parsed.Model, '/'); i >= 0 {
		aliasKey = parsed.Model[i+1:]
	}
	reasoning := inst.EffectiveReasoning(aliasKey)

	tmpl := cfg.Templates[inst.Template]
	rt := &route{
		inst:      inst,
		template:  tmpl,
		model:     model,
		alias:     inst.Alias,
		provider:  tmpl.Name,
		reasoning: reasoning,
	}

	// §4.3 session correlation: echo the effective session id on every
	// response, stream and non-stream. The id is normalized against the safe
	// charset so a malicious X-Session-Id is replaced with a UUID before it is
	// echoed or persisted (defense in depth against stored XSS via the UI).
	sessionID := store.NormalizeSessionID(sessionIDFromRequest(r))
	w.Header().Set("X-Gateway-Session-Id", sessionID)

	// Pre-generate the request id so log lines and the final capture share one
	// id (Capture falls back to generating one when empty).
	requestID := store.NewID()

	streaming := parsed.Stream != nil && *parsed.Stream
	if streaming {
		s.handleStream(w, r, cfg, rt, body, sessionID, requestID, start, clientSurface{
			endpoint:    endpoint,
			requestJSON: body,
		})
		return
	}
	s.handleNonStream(w, r, cfg, rt, body, sessionID, requestID, start, clientSurface{
		endpoint:    endpoint,
		requestJSON: body,
		shapeClient: identityBody,
	})
}

// writeResolveError maps routing failures to the §4.2 error convention:
// unknown alias (and unknown model) → 400 {"code":"INVALID_ARGUMENT",
// "message": ...}, with the available aliases listed for unknown aliases.
func (s *Server) writeResolveError(w http.ResponseWriter, err error) {
	var uae *config.UnknownAliasError
	if errors.As(err, &uae) {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", uae.Error())
		return
	}
	s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
}

// writeBuildUpstreamError maps a buildUpstream failure. Non-OAuth failures
// (body rewrite/translation) keep the existing 500 CONFIG_ERROR mapping. An
// OAuth instance with no stored credential is also a configuration error (the
// instance is not usable, mirroring the missing-API-key behavior). OAuth
// refresh failures surface as sanitized 502 UPSTREAM_ERROR: the oauth package
// errors render only fixed messages and never carry token material, verifiers,
// or provider response bodies, so echoing them is safe.
func (s *Server) writeBuildUpstreamError(w http.ResponseWriter, err error) {
	if errors.Is(err, errOAuthNotConfigured) {
		s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
		return
	}
	if errors.Is(err, oauth.ErrOperationStale) {
		s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "oauth operation was superseded")
		return
	}
	var oe *oauth.Error
	if errors.As(err, &oe) {
		s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed: "+err.Error())
		return
	}
	s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
}

// handleResponses serves POST /v1/responses: the stateless OpenAI Responses
// shim over the shared chat pipeline. Guards run before any upstream work
// (the stateless contract); unknown tool types are skipped with a
// warn log (Codex CLI sends newer tool groupings alongside function tools).
// The validated request is translated to a Chat Completions body and
// dispatched into the existing stream/non-stream branches. Capture records
// endpoint = "/v1/responses" with the as-received Responses bytes and the
// chat-normalized response; the client receives a Responses object or typed
// SSE event sequence shaped by the surface seam.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// One atomic config snapshot per request (see handleChatCompletions).
	cfg := s.cfg.Get()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "", "read body: "+err.Error())
		return
	}
	req, err := parseResponsesRequest(body)
	if err != nil {
		s.writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "", "invalid request body: "+err.Error())
		return
	}
	if param, msg := validateResponsesRequest(req); param != "" {
		s.writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", param, msg)
		return
	}

	inst, model, err := cfg.Resolve(req.Model)
	if err != nil {
		s.writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "model", err.Error())
		return
	}

	aliasKey := req.Model
	if i := strings.IndexByte(req.Model, '/'); i >= 0 {
		aliasKey = req.Model[i+1:]
	}
	tmpl := cfg.Templates[inst.Template]
	rt := &route{
		inst:      inst,
		template:  tmpl,
		model:     model,
		alias:     inst.Alias,
		provider:  tmpl.Name,
		reasoning: inst.EffectiveReasoning(aliasKey),
	}

	// §4.3 session correlation, identical to the chat surface, with one
	// Responses-specific extension: Codex CLI sends no X-Session-Id but does
	// send a stable per-conversation prompt_cache_key in the body, so without
	// grouping each turn captured as its own 1-request session. Resolution
	// order: an explicit X-Session-Id header always wins (header lookup is
	// case-insensitive, so the x-session-id session-affinity compat spelling
	// lands in the same branch); else the x-opencode-session header opencode-
	// style clients send natively; else the body's
	// prompt_cache_key derives the session id, prefixed to avoid cross-surface
	// collisions with chat-surface ids; else a fresh UUID per request (the
	// unchanged no-header fallback). The derived id passes through the same
	// normalization as a header id, so an unsafe or over-length key degrades
	// to the fresh-UUID fallback rather than being persisted.
	sessionID := sessionIDFromRequest(r)
	if sessionID == "" && req.PromptCacheKey != "" {
		sessionID = "responses-" + req.PromptCacheKey
	}
	sessionID = store.NormalizeSessionID(sessionID)
	w.Header().Set("X-Gateway-Session-Id", sessionID)

	// Pre-generate the capture id (shared with the record) and the resp_ id
	// once per request — both surfaces reuse it, and the stream path's
	// response.created/response.completed events carry the same resp_ id.
	requestID := store.NewID()
	respID := "resp_" + store.NewID()

	// Non-function tool types are skipped, not rejected: Codex CLI 0.153
	// sends newer tool groupings (e.g. "namespace") alongside function tools,
	// and a 400 aborted every turn. Log what was dropped once per request.
	var skippedTools []string
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			skippedTools = append(skippedTools, t.Type)
		}
	}
	if len(skippedTools) > 0 {
		s.logger.Warn("responses_unsupported_tool_skipped", map[string]any{
			"request_id": requestID,
			"types":      skippedTools,
		})
	}

	// Unsupported content parts (file-referenced images, unknown part types)
	// are skipped during translation, not rejected — same tolerance as
	// unknown tool types. Log what was dropped once per request.
	var skippedParts []string
	if len(req.Input) > 0 && string(req.Input) != "null" {
		var items []responsesItem
		if err := json.Unmarshal(req.Input, &items); err == nil {
			for _, item := range items {
				switch item.Type {
				case "", "message":
					skippedParts = append(skippedParts, responsesSkippedPartTypes(item.Content)...)
				case "function_call_output":
					skippedParts = append(skippedParts, responsesSkippedPartTypes(item.Output)...)
				}
			}
		}
	}
	if len(skippedParts) > 0 {
		s.logger.Warn("responses_unsupported_part_skipped", map[string]any{
			"request_id": requestID,
			"types":      skippedParts,
		})
	}

	chatBody, err := translateResponsesToChat(req)
	if err != nil {
		s.writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "", err.Error())
		return
	}

	if req.Stream != nil && *req.Stream {
		s.handleStream(w, r, cfg, rt, chatBody, sessionID, requestID, start, clientSurface{
			endpoint:    responsesEndpoint,
			requestJSON: body,
			newEmitter:  newResponsesStreamEmitter(respID, model),
		})
		return
	}
	s.handleNonStream(w, r, cfg, rt, chatBody, sessionID, requestID, start, clientSurface{
		endpoint:    responsesEndpoint,
		requestJSON: body,
		shapeClient: func(b []byte) []byte { return chatCompletionToResponse(respID, b) },
	})
}

// handleResponsesGet rejects response retrieval: the shim is stateless-only,
// so no response id is ever retrievable.
func (s *Server) handleResponsesGet(w http.ResponseWriter, r *http.Request) {
	s.writeResponsesError(w, http.StatusNotFound, "invalid_request_error", "", "response retrieval is not supported: this gateway is stateless")
}

// effectiveStyle resolves the upstream protocol for one resolved model: a
// per-model model_styles override wins when set, otherwise the template-level
// Style applies ("" → the openai chat default).
func effectiveStyle(tmpl *config.Template, model string) string {
	if s, ok := tmpl.ModelStyles[model]; ok {
		return s
	}
	return tmpl.Style
}

// buildUpstream builds the provider request: Authorization injected from the
// resolved instance's api_key_env (never from the client — keys are not
// stored or logged), body forwarded verbatim except the model field rewritten
// to the resolved model name. Templates that declare a SessionHeader (e.g.
// opencode_go → x-opencode-session) get it set from the request's effective
// session id, so the upstream sees one stable session per conversation. The
// outbound request always carries an explicit User-Agent — the inbound
// client's UA when present, else the shared config.UserAgent constant — and
// any template-declared IdentityHeaders (e.g. opencode_go's
// X-Opencode-Client / X-Opencode-Project), with a client-supplied value for
// the same header name winning over the declared default. inbound is the
// client request the gateway is serving (its UA and headers are the only
// client headers ever forwarded). cfg is the request's config snapshot. The
// returned sentBody is the exact bytes forwarded to the provider (post-model
// rewrite), used as request_filtered_json when request plugins ran.
//
// Anthropic-style models talk the Messages API instead: the OpenAI body is
// translated (translateOpenAIToAnthropic), the endpoint is <base_url>/messages,
// the key rides the x-api-key header, and the anthropic-version header is set
// (plus the extended-thinking beta header when the request enables thinking).
// Responses-style models talk the Responses API instead: the OpenAI body is
// translated (translateChatToResponses), the endpoint is <base_url>/responses,
// and the key rides the Authorization header like the openai style.
func (s *Server) buildUpstream(ctx context.Context, cfg *config.Config, rt *route, body []byte, sessionID string, inbound *http.Request) (*http.Request, []byte, error) {
	upstreamBody := body
	sent, ok := modelField(body)
	if !ok || sent != rt.model || rt.reasoning != "" {
		rewritten, err := rewriteModelAndReasoning(body, rt.model, rt.reasoning)
		if err != nil {
			return nil, nil, err
		}
		upstreamBody = rewritten
	}

	style := effectiveStyle(rt.template, rt.model)
	if rt.template.OAuth && (rt.template.BaseURL != config.ChatGPTCodexBaseURL || style != config.StyleResponses || rt.template.SessionHeader != config.ChatGPTCodexSessionHeader) {
		return nil, nil, errOAuthEndpoint
	}
	anthropic := style == config.StyleAnthropic
	responses := style == config.StyleResponses
	if anthropic {
		translated, err := translateOpenAIToAnthropic(upstreamBody, s.logger)
		if err != nil {
			return nil, nil, err
		}
		upstreamBody = translated
	} else if responses {
		translated, err := translateChatToResponses(upstreamBody, sessionID)
		if err != nil {
			return nil, nil, err
		}
		upstreamBody = translated
	}

	url := strings.TrimRight(rt.template.BaseURL, "/")
	if anthropic {
		url += "/messages"
	} else if responses {
		url += "/responses"
	} else {
		url += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Authentication: OAuth templates resolve the instance's encrypted OAuth
	// credential (refreshing it when expired) instead of the API-key path;
	// every other template keeps the unchanged env-var-then-secrets-file key
	// resolution. The OAuth headers themselves are applied after the
	// identity-header loop below so the stored credential identity always
	// wins.
	var oauthCred *secrets.OAuthCredential
	if rt.template.OAuth {
		cred, err := s.oauth.Credential(ctx, rt.inst.Alias)
		if err != nil {
			return nil, nil, err
		}
		oauthCred = &cred
	} else {
		key, err := s.resolvedKey(cfg, rt.inst)
		if err != nil {
			return nil, nil, err
		}
		if key != "" {
			if anthropic {
				req.Header.Set("x-api-key", key)
			} else {
				req.Header.Set("Authorization", "Bearer "+key)
			}
		}
	}
	if anthropic {
		req.Header.Set("anthropic-version", anthropicVersionHeader)
		if bodyHasThinking(upstreamBody) {
			req.Header.Set("anthropic-beta", anthropicThinkingBetaHeader)
		}
	}
	// Providers that require a session-context header (opencode_go's
	// x-opencode-session) get the request's effective session id — the same
	// id echoed as X-Gateway-Session-Id — so upstream sees one stable session
	// per conversation and never rejects with MissingSessionID.
	if h := rt.template.SessionHeader; h != "" && sessionID != "" {
		req.Header.Set(h, sessionID)
	}
	// The gateway identifies itself on every upstream call: the inbound
	// client's User-Agent is forwarded faithfully when present (the real
	// client's UA is what the upstream expects to see), else a constant own
	// product name replaces Go's "Go-http-client/1.1" default.
	ua := config.UserAgent
	if v := inbound.UserAgent(); v != "" {
		ua = v
	}
	req.Header.Set("User-Agent", ua)
	// Template-declared identity headers (opencode_go's X-Opencode-Client /
	// X-Opencode-Project) ride upstream on every request so the account is
	// not fingerprinted as non-agent traffic. A client-supplied value for the
	// same header name (lookup is case-insensitive) wins — the client's own
	// identity is forwarded faithfully; otherwise the declared default is sent.
	names := sortedMapKeys(rt.template.IdentityHeaders)
	for _, h := range names {
		v := rt.template.IdentityHeaders[h]
		if cv := inbound.Header.Get(h); cv != "" {
			v = cv
		}
		req.Header.Set(h, v)
	}
	// The stored OAuth credential identity is authoritative: a client-supplied
	// Authorization or ChatGPT account header (which the identity-header loop
	// above could otherwise forward) can never override it. The headers mirror
	// the verified OpenCode v1.18.30 upstream contract: the bearer token, the
	// ChatGPT-Account-Id account identifier, and the x-openai-internal-codex-
	// residency header when the access token carries a compute-residency
	// claim. Token material never reaches logs: it rides request headers only.
	if oauthCred != nil {
		req.Header.Del("Authorization")
		req.Header.Del("ChatGPT-Account-Id")
		req.Header.Del("x-openai-internal-codex-residency")
		req.Header.Del("Originator")
		req.Header.Set("Authorization", "Bearer "+oauthCred.AccessToken)
		if oauthCred.AccountID != "" {
			req.Header.Set("ChatGPT-Account-Id", oauthCred.AccountID)
		}
		if residency := oauth.ComputeResidency(oauthCred.AccessToken); residency != "" {
			req.Header.Set("x-openai-internal-codex-residency", residency)
		}
		req.Header.Set("Originator", oauth.Originator)
	}
	return req, upstreamBody, nil
}

// buildChain resolves the §4.5 plugin chain for one instance: a per-instance
// plugins list wins (empty list = no plugins), otherwise the global settings
// defaults apply; each name is built from the built-in registry with its
// [plugins.<name>] config. There is no template-default runtime fallback.
// Names are validated at config load; Build still defends against an unknown
// name defensively. Request-only plugins are skipped on the response side so
// they never switch streaming into buffer mode (their FilterResponse is a
// no-op; see plugins.RequestOnly).
func (s *Server) buildChain(cfg *config.Config, inst *config.Instance) (*plugins.Chain, error) {
	reqNames, respNames := inst.EffectivePlugins(cfg)
	chain := plugins.NewChain()
	for _, name := range reqNames {
		p, err := plugins.Build(name, plugins.Options{Config: cfg.PluginConfig(name)})
		if err != nil {
			return nil, err
		}
		chain.AddRequest(p)
	}
	for _, name := range respNames {
		p, err := plugins.Build(name, plugins.Options{Config: cfg.PluginConfig(name)})
		if err != nil {
			return nil, err
		}
		if plugins.IsRequestOnly(p) {
			continue
		}
		chain.AddResponse(p)
	}
	return chain, nil
}

// filterRequestBody runs the request plugin chain over the client body,
// returning the body to forward (filtered when plugins ran, verbatim
// otherwise).
func (s *Server) filterRequestBody(ctx context.Context, chain *plugins.Chain, body []byte) ([]byte, error) {
	if !chain.HasRequest() {
		return body, nil
	}
	req := &plugins.Request{Body: body}
	if err := chain.FilterRequest(ctx, req); err != nil {
		return nil, err
	}
	return req.Body, nil
}

// resolvedKey returns the provider key for an instance with §6.2 precedence:
// the api_key_env environment variable first, then the UI-managed secrets
// file. An empty key is legitimate for keyless templates (e.g. ollama); a
// configured env var that is unset and no stored key is a misconfiguration
// reported to the caller. cfg is the request's config snapshot.
func (s *Server) resolvedKey(cfg *config.Config, inst *config.Instance) (string, error) {
	env := inst.EffectiveAPIKeyEnv(cfg)
	key, ok := s.secrets.ResolveKey(env, inst.Alias)
	if ok {
		return key, nil
	}
	if env != "" {
		return "", fmt.Errorf("api key not set for instance %q (expected env var %s or a stored key)", inst.Alias, env)
	}
	return "", nil
}

// translateUpstreamBody maps a non-openai-style provider response into the
// OpenAI shape the gateway and its clients speak: anthropic error bodies get
// the OpenAI error shape and 2xx completions become chat.completion objects;
// responses 2xx completions become chat.completion objects (responses error
// bodies already carry the OpenAI error envelope, so they and any unparseable
// body pass through unchanged). Non-anthropic/non-responses styles and
// unparseable bodies pass through unchanged.
func translateUpstreamBody(style string, status int, body []byte) []byte {
	switch style {
	case config.StyleAnthropic:
		if status >= 400 {
			return translateAnthropicError(body)
		}
		translated, err := translateAnthropicToOpenAI(body)
		if err != nil {
			return body
		}
		return translated
	case config.StyleResponses:
		if status >= 400 {
			return body
		}
		return translateResponsesToChatCompletion(body)
	default:
		return body
	}
}

// handleNonStream forwards the request, records the provider response, runs
// the response plugins on the provider body, and passes the (filtered)
// provider status/body through — error bodies included. sf is the client
// surface seam: capture records its endpoint and as-received request bytes,
// and 2xx bodies are shaped for the client after the plugins ran. Transport
// errors and non-2xx responses are passed through exactly as received.
func (s *Server) handleNonStream(w http.ResponseWriter, r *http.Request, cfg *config.Config, rt *route, body []byte, sessionID, requestID string, start time.Time, sf clientSurface) {
	chain, err := s.buildChain(cfg, rt.inst)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
		return
	}
	forwardedBody, err := s.filterRequestBody(r.Context(), chain, body)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "PLUGIN_ERROR", "request plugin failed: "+err.Error())
		return
	}

	req, sentBody, err := s.buildUpstream(r.Context(), cfg, rt, forwardedBody, sessionID, r)
	if err != nil {
		s.writeBuildUpstreamError(w, err)
		return
	}
	rsp, err := s.client.Do(req)
	if err != nil {
		s.capture(upstreamErrorRecord(rt, sf.requestJSON, requestFilteredBody(chain, sentBody), sessionID, requestID, start, err, chain.Applied()))
		s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed: "+err.Error())
		return
	}
	respBody, readErr := io.ReadAll(rsp.Body)
	rsp.Body.Close()
	if readErr != nil {
		s.capture(upstreamErrorRecord(rt, sf.requestJSON, requestFilteredBody(chain, sentBody), sessionID, requestID, start, readErr, chain.Applied()))
		s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream read failed: "+readErr.Error())
		return
	}
	// Anthropic responses are translated to the OpenAI shape before plugins
	// and capture, so the whole pipeline (and the client) sees one schema.
	respBody = translateUpstreamBody(effectiveStyle(rt.template, rt.model), rsp.StatusCode, respBody)

	rec := &store.CaptureRecord{
		ID:              requestID,
		SessionID:       sessionID,
		CreatedAt:       start,
		Alias:           rt.alias,
		Provider:        rt.provider,
		ProviderBaseURL: rt.template.BaseURL,
		Model:           rt.model,
		Endpoint:        sf.endpoint,
		DurationMS:      durationMS(start),
		StatusCode:      rsp.StatusCode,
		RequestJSON:     sf.requestJSON,
		PluginsApplied:  chain.Applied(),
	}
	rec.RequestFilteredJSON = requestFilteredBody(chain, sentBody)

	clientBody := respBody
	if rsp.StatusCode >= 400 {
		rec.Error = providerError(rsp.StatusCode, respBody)
	} else {
		rec.FinishReason, rec.Usage = parseCompletionMeta(respBody)
		rec.ResponseJSON = respBody
		if chain.HasResponse() {
			respDomain := &plugins.Response{Body: respBody}
			if err := chain.FilterResponse(r.Context(), respDomain); err != nil {
				// The provider's data is never dropped; log and pass through
				// the unfiltered body.
				s.logger.Error("response_plugin_failed", map[string]any{"session_id": sessionID, "error": err.Error()})
			} else {
				rec.ResponseFilteredJSON = respDomain.Body
				clientBody = respDomain.Body
			}
		}
		// Shape the client body last: the capture fields above keep the chat
		// bytes, the surface decides what the client sees (identity on the
		// chat surface).
		clientBody = sf.shapeClient(clientBody)
	}
	s.passthrough(w, rsp, clientBody)
	s.capture(rec)
}

// requestFilteredBody returns the body forwarded to the provider when request
// plugins ran, else nil (→ NULL request_filtered_json).
func requestFilteredBody(chain *plugins.Chain, sentBody []byte) []byte {
	if !chain.HasRequest() {
		return nil
	}
	return sentBody
}

// handleStream is the §4.4 streaming path: SSE is forwarded line-by-line with
// a flush after each chunk while a concurrent assembler folds deltas into the
// completion captured at request end. With response plugins configured the
// path switches to buffer mode (resolved review decision #1): the stream is
// reassembled, response plugins run post-reassembly, and the filtered result
// is emitted as one SSE block. sf is the client surface seam (capture endpoint
// and as-received request bytes; a surface-specific emitter builder). On
// client disconnect the upstream is canceled and the partially reassembled
// data is persisted truncated.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, cfg *config.Config, rt *route, body []byte, sessionID, requestID string, start time.Time, sf clientSurface) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	chain, err := s.buildChain(cfg, rt.inst)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
		return
	}
	forwardedBody, err := s.filterRequestBody(ctx, chain, body)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "PLUGIN_ERROR", "request plugin failed: "+err.Error())
		return
	}

	req, sentBody, err := s.buildUpstream(ctx, cfg, rt, forwardedBody, sessionID, r)
	if err != nil {
		s.writeBuildUpstreamError(w, err)
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.capture(upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, requestID, start, err, chain.Applied()))
		s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			respBody = nil
		}
		respBody = translateUpstreamBody(effectiveStyle(rt.template, rt.model), resp.StatusCode, respBody)
		rec := &store.CaptureRecord{
			ID:              requestID,
			SessionID:       sessionID,
			CreatedAt:       start,
			Alias:           rt.alias,
			Provider:        rt.provider,
			ProviderBaseURL: rt.template.BaseURL,
			Model:           rt.model,
			Endpoint:        sf.endpoint,
			DurationMS:      durationMS(start),
			StatusCode:      resp.StatusCode,
			RequestJSON:     sf.requestJSON,
			PluginsApplied:  chain.Applied(),
			Error:           providerError(resp.StatusCode, respBody),
		}
		rec.RequestFilteredJSON = requestFilteredBody(chain, sentBody)
		s.passthrough(w, resp, respBody)
		s.capture(rec)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "INTERNAL", "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	asm := newAssembler()
	source := func() json.RawMessage { b, _, _ := asm.result(); return b }
	var filter EmitterFilter
	if chain.HasResponse() {
		filter = func(body json.RawMessage) (json.RawMessage, error) {
			resp := &plugins.Response{Body: body}
			if err := chain.FilterResponse(context.Background(), resp); err != nil {
				return nil, err
			}
			return resp.Body, nil
		}
	}
	// The surface seam swaps the emitter (the Responses surface translates
	// chat chunks into typed events); the chat surface keeps the factory
	// output unchanged.
	newEmitter := s.emitterFactory
	if sf.newEmitter != nil {
		newEmitter = sf.newEmitter
	}
	emitter := newEmitter(w, source, filter)

	var out streamOutcome
	style := effectiveStyle(rt.template, rt.model)
	if style == config.StyleAnthropic {
		out = readAnthropicStream(ctx, resp, asm, emitter.Write)
	} else if style == config.StyleResponses {
		out = readResponsesStream(ctx, resp, asm, emitter.Write)
	} else {
		out = readStream(ctx, resp, asm, emitter.Write)
	}
	// Finalization (item .done + response.completed) runs after readStream
	// returns, so asm.result() (finish_reason, usage) is complete — exactly
	// like bufferedEmitter — except on a truncated stream: the disconnect
	// behavior is inherited unchanged (capture truncated, never a fabricated
	// completed turn). The chat surface's Done stays unconditional (live
	// no-op; buffered partial block).
	if sf.newEmitter == nil || !out.truncated {
		_ = emitter.Done()
	}

	rec := &store.CaptureRecord{
		ID:              requestID,
		SessionID:       sessionID,
		CreatedAt:       start,
		Alias:           rt.alias,
		Provider:        rt.provider,
		ProviderBaseURL: rt.template.BaseURL,
		Model:           rt.model,
		Endpoint:        sf.endpoint,
		DurationMS:      durationMS(start),
		StatusCode:      resp.StatusCode,
		FinishReason:    out.finish,
		Usage:           out.usage,
		RequestJSON:     sf.requestJSON,
		ResponseJSON:    out.reassembled,
		Truncated:       out.truncated,
		PluginsApplied:  chain.Applied(),
		Error:           out.streamErr,
		ChunkCount:      out.chunks,
	}
	rec.RequestFilteredJSON = requestFilteredBody(chain, sentBody)
	if chain.HasResponse() {
		if be, ok := emitter.(*bufferedEmitter); ok {
			// Byte-exact copy of what the client received.
			rec.ResponseFilteredJSON = be.emittedBody()
		} else if re, ok := emitter.(*responsesEmitter); ok && len(re.emittedBody()) > 0 {
			// Byte-exact copy of the filtered CHAT body the Responses burst
			// was built from (capture keeps chat shapes on this surface too).
			rec.ResponseFilteredJSON = re.emittedBody()
		} else {
			// Custom emitter override: filter now for the record.
			respDomain := &plugins.Response{Body: out.reassembled}
			if err := chain.FilterResponse(context.Background(), respDomain); err == nil {
				rec.ResponseFilteredJSON = respDomain.Body
			}
		}
	}
	s.capture(rec)
}

// capture persists one request in a single store transaction and appends the
// §8 log lines. A fresh context (not the canceled client context) is used so
// a disconnect never drops the record. The estimated cost split is derived
// from the record's usage object here, the single chokepoint every capture
// path shares.
func (s *Server) capture(rec *store.CaptureRecord) {
	s.estimateCost(rec)
	ctx := context.Background()
	if err := s.store.Capture(ctx, rec); err != nil {
		s.logger.Error("capture_failed", map[string]any{"session_id": rec.SessionID, "error": err.Error()})
		return
	}
	if err := s.append.write(rec); err != nil {
		s.logger.Error("append_log_failed", map[string]any{"session_id": rec.SessionID, "error": err.Error()})
	}
	s.logPayloads(rec)
}

// logPayloads writes one opt-in console record with the redacted request and
// response payloads when settings.log_payloads is enabled. The payloads are
// redacted before logging so prompt/tool-call content never reaches stderr.
func (s *Server) logPayloads(rec *store.CaptureRecord) {
	if !s.cfg.Get().Settings.LogPayloads {
		return
	}
	fields := map[string]any{
		"session_id":  rec.SessionID,
		"alias":       rec.Alias,
		"model":       rec.Model,
		"status_code": rec.StatusCode,
		"duration_ms": rec.DurationMS,
	}
	if len(rec.RequestJSON) > 0 {
		fields["request"] = string(redactPayload(rec.RequestJSON))
	}
	if len(rec.ResponseJSON) > 0 {
		fields["response"] = string(redactPayload(rec.ResponseJSON))
	}
	s.logger.Info("request_payloads", fields)
}

// passthrough copies a provider response (status, content type, body) to the
// client. The X-Gateway-Session-Id header was set by the caller beforehand.
func (s *Server) passthrough(w http.ResponseWriter, resp *http.Response, body []byte) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"code": code, "message": message})
	s.logger.Warn("request_rejected", map[string]any{"status": status, "code": code, "message": message})
}

// parseCompletionMeta extracts finish_reason (first choice) and usage from a
// non-stream completion body.
func parseCompletionMeta(body []byte) (string, json.RawMessage) {
	var c struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return "", nil
	}
	finish := ""
	if len(c.Choices) > 0 {
		finish = c.Choices[0].FinishReason
	}
	return finish, c.Usage
}

// providerError extracts {code, message} from an OpenAI-shaped non-2xx body,
// falling back to a generic code and a snippet of the body.
func providerError(status int, body []byte) *store.ErrorInfo {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	msg, code := "", ""
	if err := json.Unmarshal(body, &e); err == nil {
		msg = e.Error.Message
		code = stringOf(e.Error.Code)
		if code == "" {
			code = e.Error.Type
		}
	}
	if msg == "" {
		msg = snippet(string(body), 512)
	}
	if code == "" {
		code = "UPSTREAM_ERROR"
	}
	return &store.ErrorInfo{Code: code, Message: msg}
}

// upstreamErrorRecord builds the capture record for a transport failure (no
// provider response received); the data is necessarily incomplete. filteredBody
// is the post-request-plugin body when request plugins ran, else nil. requestID
// is the request's pre-generated id, shared with any retry log lines.
func upstreamErrorRecord(rt *route, body, filteredBody []byte, sessionID, requestID string, start time.Time, cause error, applied []string) *store.CaptureRecord {
	rec := &store.CaptureRecord{
		ID:              requestID,
		SessionID:       sessionID,
		CreatedAt:       start,
		Alias:           rt.alias,
		Provider:        rt.provider,
		ProviderBaseURL: rt.template.BaseURL,
		Model:           rt.model,
		Endpoint:        endpoint,
		DurationMS:      durationMS(start),
		StatusCode:      502,
		RequestJSON:     body,
		Truncated:       true,
		Error:           &store.ErrorInfo{Code: "UPSTREAM_ERROR", Message: "upstream error: " + cause.Error()},
	}
	if len(filteredBody) > 0 {
		rec.RequestFilteredJSON = filteredBody
	}
	if len(applied) > 0 {
		rec.PluginsApplied = applied
	}
	return rec
}

// ssePayload extracts the payload of a data: line, or nil for any other line
// (keepalive comments, event lines, blank separators are passed through
// verbatim and never reassembled).
func ssePayload(line string) []byte {
	const prefix = "data:"
	if !strings.HasPrefix(line, prefix) {
		return nil
	}
	payload := strings.TrimSpace(line[len(prefix):])
	if payload == "" {
		return nil
	}
	return []byte(payload)
}

// modelField extracts the body's model field.
func modelField(body []byte) (string, bool) {
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", false
	}
	return m.Model, true
}

// rewriteModel re-serializes the body with the model field set to model, so
// the provider sees the plain resolved model name (e.g. "gpt-4o") rather than
// the client's "openai-2/gpt-4o" routing key.
func rewriteModel(body []byte, model string) ([]byte, error) {
	return rewriteModelAndReasoning(body, model, "")
}

// rewriteModelAndReasoning re-serializes the body with the model field set to model
// and optionally reasoning_effort set to reasoning (when non-empty).
func rewriteModelAndReasoning(body []byte, model string, reasoning string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	b, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = b
	if reasoning != "" {
		rb, err := json.Marshal(reasoning)
		if err != nil {
			return nil, err
		}
		m["reasoning_effort"] = rb
	}
	return json.Marshal(m)
}

func durationMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

// snippet truncates s to at most n code points without splitting a UTF-8 rune.
func snippet(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
