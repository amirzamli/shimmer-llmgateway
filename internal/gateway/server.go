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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/api"
	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
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
	return &Server{
		cfg:    cfg,
		store:  st,
		logger: logger,
		// Refuse to follow upstream redirects: a redirecting or malicious
		// base_url must not bounce the request to an internal endpoint. The
		// shared transport bounds connection/header timeouts without imposing
		// a total request deadline (see providerTransport).
		client:         &http.Client{Transport: providerTransport, CheckRedirect: noRedirect},
		append:         ap,
		secrets:        sec,
		api:            api.New(cfg, configPath, st, sec, logger),
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

// indexHTML caches the embedded single-page UI; it is read from embed.FS once
// (first request) instead of on every request. indexHTML is nil only when the
// embedded asset is missing.
var (
	indexHTMLOnce sync.Once
	indexHTML     []byte
)

// handleUI serves the embedded single-page HTML UI (§6.1).
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
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
	sessionID := store.NormalizeSessionID(r.Header.Get("X-Session-Id"))
	w.Header().Set("X-Gateway-Session-Id", sessionID)

	// Pre-generate the request id so retry_empty retry log lines and the final
	// capture share one id (Capture falls back to generating one when empty).
	requestID := store.NewID()

	streaming := parsed.Stream != nil && *parsed.Stream
	if streaming {
		s.handleStream(w, r, cfg, rt, body, sessionID, requestID, start)
		return
	}
	s.handleNonStream(w, r, cfg, rt, body, sessionID, requestID, start)
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

// buildUpstream builds the provider request: Authorization injected from the
// resolved instance's api_key_env (never from the client — keys are not
// stored or logged), body forwarded verbatim except the model field rewritten
// to the resolved model name. cfg is the request's config snapshot. The
// returned sentBody is the exact bytes forwarded to the provider (post-model
// rewrite), used as request_filtered_json when request plugins ran.
//
// Anthropic-style templates talk the Messages API instead: the OpenAI body is
// translated (translateOpenAIToAnthropic), the endpoint is <base_url>/messages,
// the key rides the x-api-key header, and the anthropic-version header is set
// (plus the extended-thinking beta header when the request enables thinking).
func (s *Server) buildUpstream(ctx context.Context, cfg *config.Config, rt *route, body []byte) (*http.Request, []byte, error) {
	upstreamBody := body
	sent, ok := modelField(body)
	if !ok || sent != rt.model || rt.reasoning != "" {
		rewritten, err := rewriteModelAndReasoning(body, rt.model, rt.reasoning)
		if err != nil {
			return nil, nil, err
		}
		upstreamBody = rewritten
	}

	anthropic := rt.template.Style == config.StyleAnthropic
	if anthropic {
		translated, err := translateOpenAIToAnthropic(upstreamBody)
		if err != nil {
			return nil, nil, err
		}
		upstreamBody = translated
	}

	url := strings.TrimRight(rt.template.BaseURL, "/")
	if anthropic {
		url += "/messages"
	} else {
		url += "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

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
	if anthropic {
		req.Header.Set("anthropic-version", anthropicVersionHeader)
		if bodyHasThinking(upstreamBody) {
			req.Header.Set("anthropic-beta", anthropicThinkingBetaHeader)
		}
	}
	return req, upstreamBody, nil
}

// buildChain resolves the §4.5 plugin chain for one instance: a per-instance
// plugins list wins (empty list = no plugins), otherwise the global settings
// defaults apply; each name is built from the built-in registry with its
// [plugins.<name>] config. There is no template-default runtime fallback.
// Control plugin names (plugins.IsControl) are skipped — they are valid config
// values read directly by the proxy path, never transforms. Names are
// validated at config load; Build still defends against an unknown name
// defensively.
func (s *Server) buildChain(cfg *config.Config, inst *config.Instance) (*plugins.Chain, error) {
	reqNames, respNames := inst.EffectivePlugins(cfg)
	chain := plugins.NewChain()
	for _, name := range reqNames {
		if plugins.IsControl(name) {
			continue
		}
		p, err := plugins.Build(name, plugins.Options{Config: cfg.PluginConfig(name)})
		if err != nil {
			return nil, err
		}
		chain.AddRequest(p)
	}
	for _, name := range respNames {
		if plugins.IsControl(name) {
			continue
		}
		p, err := plugins.Build(name, plugins.Options{Config: cfg.PluginConfig(name)})
		if err != nil {
			return nil, err
		}
		chain.AddResponse(p)
	}
	return chain, nil
}

// retryEmptyActive reports whether the retry_empty control plugin is active
// for the instance (listed on either side of the effective plugin chain).
func retryEmptyActive(cfg *config.Config, inst *config.Instance) bool {
	req, resp := inst.EffectivePlugins(cfg)
	for _, side := range [2][]string{req, resp} {
		for _, name := range side {
			if name == "retry_empty" {
				return true
			}
		}
	}
	return false
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

// maxRetryAttempts is the bound on upstream re-issues when retry_empty is
// active: a premature-empty 2xx result is retried up to this many attempts
// total (constant, per the plan).
const maxRetryAttempts = 3

// translateUpstreamBody maps an anthropic-style provider response into the
// OpenAI shape the gateway and its clients speak: error bodies get the OpenAI
// error shape, 2xx completions become chat.completion objects. Non-anthropic
// styles and unparseable bodies pass through unchanged.
func translateUpstreamBody(style string, status int, body []byte) []byte {
	if style != config.StyleAnthropic {
		return body
	}
	if status >= 400 {
		return translateAnthropicError(body)
	}
	translated, err := translateAnthropicToOpenAI(body)
	if err != nil {
		return body
	}
	return translated
}

// handleNonStream forwards the request, records the provider response, runs
// the response plugins on the provider body, and passes the (filtered)
// provider status/body through — error bodies included. When retry_empty is
// active for the instance, a 2xx completion that is premature-empty (see
// isEmptyCompletion) is re-issued upstream up to maxRetryAttempts; the client
// and the capture observe only the final attempt, sharing requestID. Transport
// errors and non-2xx responses are passed through exactly as before (never
// retried).
func (s *Server) handleNonStream(w http.ResponseWriter, r *http.Request, cfg *config.Config, rt *route, body []byte, sessionID, requestID string, start time.Time) {
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

	retryActive := retryEmptyActive(cfg, rt.inst)
	var (
		resp     *http.Response
		respBody []byte
		sentBody []byte
	)
	for attempt := 1; ; attempt++ {
		req, sb, err := s.buildUpstream(r.Context(), cfg, rt, forwardedBody)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
			return
		}
		sentBody = sb
		rsp, err := s.client.Do(req)
		if err != nil {
			s.capture(upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, requestID, start, err, chain.Applied()))
			s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed: "+err.Error())
			return
		}
		rb, readErr := io.ReadAll(rsp.Body)
		rsp.Body.Close()
		if readErr != nil {
			s.capture(upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, requestID, start, readErr, chain.Applied()))
			s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream read failed: "+readErr.Error())
			return
		}
		resp, respBody = rsp, rb
		// Anthropic responses are translated to the OpenAI shape before the
		// empty-completion check, plugins, and capture, so the whole pipeline
		// (and the client) sees one schema.
		respBody = translateUpstreamBody(rt.template.Style, rsp.StatusCode, respBody)
		if retryActive && attempt < maxRetryAttempts && rsp.StatusCode >= 200 && rsp.StatusCode < 300 && isEmptyCompletion(respBody) {
			s.logger.Warn("retry_empty", map[string]any{
				"request_id": requestID,
				"session_id": sessionID,
				"alias":      rt.alias,
				"attempt":    attempt,
			})
			continue
		}
		break
	}

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
		StatusCode:      resp.StatusCode,
		RequestJSON:     body,
		PluginsApplied:  chain.Applied(),
	}
	rec.RequestFilteredJSON = requestFilteredBody(chain, sentBody)

	clientBody := respBody
	if resp.StatusCode >= 400 {
		rec.Error = providerError(resp.StatusCode, respBody)
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
	}
	s.passthrough(w, resp, clientBody)
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
// is emitted as one SSE block. On client disconnect the upstream is canceled
// and the partially reassembled data is persisted truncated. With retry_empty
// active the path switches to held mode: each attempt is reassembled without
// forwarding content, premature-empty attempts are re-issued upstream,
// reasoning/thinking deltas are forwarded live so the client never waits in
// silence, and the final attempt is emitted once as one SSE block.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, cfg *config.Config, rt *route, body []byte, sessionID, requestID string, start time.Time) {
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

	// The retry path issues its first upstream attempt before committing the
	// SSE headers, so transport errors (502) and non-2xx responses (including
	// 3xx, surfaced by the no-redirect policy) reach the client with their real
	// status. Only after a 2xx are the held-mode SSE headers written and the
	// read/retry loop started. The non-retry path issues once up front and
	// keeps today's ordering (4xx and transport errors before headers).
	if retryEmptyActive(cfg, rt.inst) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", "streaming not supported")
			return
		}
		req, sentBody, err := s.buildUpstream(ctx, cfg, rt, forwardedBody)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
			return
		}
		resp, err := s.client.Do(req)
		if err != nil {
			s.capture(upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, requestID, start, err, chain.Applied()))
			s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed: "+err.Error())
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// The provider's real status (including 3xx) is passed through and
			// captured; the headers are still uncommitted, so the client sees
			// it directly.
			errBody, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr != nil {
				errBody = nil
			}
			errBody = translateUpstreamBody(rt.template.Style, resp.StatusCode, errBody)
			s.capture(streamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, requestID, start, resp, errBody, chain.Applied()))
			s.passthrough(w, resp, errBody)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		s.handleStreamRetry(w, ctx, cfg, chain, rt, forwardedBody, body, sessionID, requestID, start, flusher, resp, sentBody)
		return
	}

	req, sentBody, err := s.buildUpstream(ctx, cfg, rt, forwardedBody)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
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
		respBody = translateUpstreamBody(rt.template.Style, resp.StatusCode, respBody)
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
			StatusCode:      resp.StatusCode,
			RequestJSON:     body,
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
	emitter := s.emitterFactory(w, source, filter)

	var out streamOutcome
	if rt.template.Style == config.StyleAnthropic {
		out = readAnthropicStream(ctx, resp, asm, emitter.Write)
	} else {
		out = readStream(ctx, resp, asm, emitter.Write)
	}
	_ = emitter.Done()

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
		StatusCode:      resp.StatusCode,
		FinishReason:    out.finish,
		Usage:           out.usage,
		RequestJSON:     body,
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

// handleStreamRetry is the retry_empty held-mode streaming path. The first
// upstream attempt (2xx) was already issued by handleStream before the 200+SSE
// headers were committed; here each attempt is read to completion without
// forwarding a byte until an attempt is non-empty (or the attempt budget is
// exhausted). Subsequent attempts are issued in this loop; on a transport error
// or non-2xx response (headers already committed) the stream terminates cleanly
// with an SSE error event plus [DONE] — never a writeError/passthrough over the
// committed status — and the failure is captured. The final reassembly is
// emitted as one SSE block plus [DONE] (response plugins applied once,
// mirroring bufferedEmitter.Done), then captured with the request's
// pre-generated id. A client disconnect during a held attempt is captured
// truncated; the capture is never dropped by a failed write.
func (s *Server) handleStreamRetry(w http.ResponseWriter, ctx context.Context, cfg *config.Config, chain *plugins.Chain, rt *route, forwardedBody, body []byte, sessionID, requestID string, start time.Time, flusher http.Flusher, firstResp *http.Response, firstSentBody []byte) {
	var (
		out      streamOutcome
		sentBody = firstSentBody
	)
	attempt := 1
	rsp := firstResp
	// readAttempt reads one upstream SSE attempt in held mode, translating
	// anthropic-style streams to the OpenAI shape.
	readAttempt := func(r *http.Response) streamOutcome {
		if rt.template.Style == config.StyleAnthropic {
			return readAnthropicStream(ctx, r, newAssembler(), s.holdStreamForward(w, flusher))
		}
		return readStream(ctx, r, newAssembler(), s.holdStreamForward(w, flusher))
	}
	for {
		attemptOut := readAttempt(rsp)
		rsp.Body.Close()
		if !(isEmptyCompletion(attemptOut.reassembled) && attempt < maxRetryAttempts) {
			out = attemptOut
			break
		}
		if ctx.Err() != nil {
			// The client is gone: keep what this attempt produced (captured
			// truncated) instead of re-issuing against a canceled context.
			out = attemptOut
			break
		}
		s.logger.Warn("retry_empty", map[string]any{
			"request_id": requestID,
			"session_id": sessionID,
			"alias":      rt.alias,
			"attempt":    attempt,
		})
		attempt++
		req, sb, err := s.buildUpstream(ctx, cfg, rt, forwardedBody)
		if err != nil {
			s.retryAttemptError(w, flusher, upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, requestID, start, err, chain.Applied()), attempt, "CONFIG_ERROR", err.Error())
			return
		}
		sentBody = sb
		rsp, err = s.client.Do(req)
		if err != nil {
			s.retryAttemptError(w, flusher, upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, requestID, start, err, chain.Applied()), attempt, "UPSTREAM_ERROR", "upstream request failed: "+err.Error())
			return
		}
		if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
			// Non-2xx (including 3xx) is never retried. The SSE headers are
			// already committed, so the error is captured and the stream ends
			// with an SSE error event instead of a status-line swap.
			errBody, rerr := io.ReadAll(rsp.Body)
			rsp.Body.Close()
			if rerr != nil {
				errBody = nil
			}
			errBody = translateUpstreamBody(rt.template.Style, rsp.StatusCode, errBody)
			s.retryAttemptError(w, flusher, streamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, requestID, start, rsp, errBody, chain.Applied()), attempt, "UPSTREAM_ERROR", fmt.Sprintf("upstream returned non-2xx status %d", rsp.StatusCode))
			return
		}
	}

	// Emit the final reassembly as one SSE block plus [DONE], applying the
	// response plugin chain once (mirroring bufferedEmitter.Done). A write
	// error here means the client disconnected; the capture below must still
	// run (mirrors the non-retry path's unconditional capture).
	clientBody := out.reassembled
	if chain.HasResponse() {
		respDomain := &plugins.Response{Body: out.reassembled}
		if err := chain.FilterResponse(context.Background(), respDomain); err != nil {
			// The provider's data is never dropped; log and pass through the
			// unfiltered body.
			s.logger.Error("response_plugin_failed", map[string]any{"session_id": sessionID, "error": err.Error()})
		} else {
			clientBody = respDomain.Body
		}
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", clientBody)
	flusher.Flush()
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

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
		StatusCode:      out.statusCode,
		FinishReason:    out.finish,
		Usage:           out.usage,
		RequestJSON:     body,
		ResponseJSON:    out.reassembled,
		Truncated:       out.truncated,
		PluginsApplied:  chain.Applied(),
		Error:           out.streamErr,
		ChunkCount:      out.chunks,
	}
	rec.RequestFilteredJSON = requestFilteredBody(chain, sentBody)
	if chain.HasResponse() {
		rec.ResponseFilteredJSON = clientBody
	}
	s.capture(rec)
}

// retryAttemptError handles a failed subsequent retry attempt once the SSE
// headers are committed (writeError/passthrough would no-op on the committed
// 200): it captures the error record, logs via the retry_empty warn channel,
// and terminates the held stream cleanly with an SSE error event plus [DONE].
func (s *Server) retryAttemptError(w io.Writer, flusher http.Flusher, rec *store.CaptureRecord, attempt int, code, message string) {
	s.capture(rec)
	s.logger.Warn("retry_empty", map[string]any{
		"request_id": rec.ID,
		"session_id": rec.SessionID,
		"alias":      rec.Alias,
		"attempt":    attempt,
		"error":      message,
	})
	writeStreamErrorEvent(w, flusher, code, message)
}

// writeStreamErrorEvent terminates a held retry stream after a failed
// subsequent attempt: an SSE error event followed by [DONE], so the client
// sees a clean end instead of a dangling 200 stream.
func writeStreamErrorEvent(w io.Writer, flusher http.Flusher, code, message string) {
	errJSON, _ := json.Marshal(map[string]any{"code": code, "message": message})
	fmt.Fprintf(w, "data: {\"error\": %s}\n\n", errJSON)
	flusher.Flush()
	io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// streamErrorRecord builds the capture record for a non-2xx upstream response
// on the stream retry path: the provider's real status and error body are
// captured verbatim.
func streamErrorRecord(rt *route, body, filteredBody []byte, sessionID, requestID string, start time.Time, rsp *http.Response, errBody []byte, applied []string) *store.CaptureRecord {
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
		StatusCode:      rsp.StatusCode,
		RequestJSON:     body,
		PluginsApplied:  applied,
		Error:           providerError(rsp.StatusCode, errBody),
	}
	rec.RequestFilteredJSON = filteredBody
	return rec
}

// holdStreamForward is the retry-empty held-mode forward: reasoning/thinking
// deltas (delta.reasoning / delta.reasoning_content) and keepalive comment
// lines are written to the client immediately, so the connection stays
// visibly alive across a held attempt — idle/first-token timeouts never fire
// and the agent shows thinking progress instead of appearing hung — while
// content, tool_calls, finish_reason, and [DONE] stay held until the attempt
// is known non-empty. The reassembly is unaffected: the assembler never folds
// reasoning into content.
func (s *Server) holdStreamForward(w io.Writer, flusher http.Flusher) func([]byte) error {
	forward := func(line []byte) error {
		if _, err := w.Write(line); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	return func(line []byte) error {
		payload := ssePayload(string(line))
		if payload == nil {
			// Keepalive comments and other non-data lines pass through.
			return forward(line)
		}
		if string(payload) != "[DONE]" && carriesReasoning(payload) {
			return forward(line)
		}
		return nil
	}
}

// carriesReasoning reports whether an SSE data payload is a chunk whose delta
// carries non-empty reasoning/thinking content. Such chunks are forwarded
// live by the held retry path so the client sees progress during an attempt.
func carriesReasoning(payload []byte) bool {
	var ev struct {
		Choices []struct {
			Delta struct {
				Reasoning        *string `json:"reasoning"`
				ReasoningContent *string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return false
	}
	for _, c := range ev.Choices {
		if c.Delta.Reasoning != nil && strings.TrimSpace(*c.Delta.Reasoning) != "" {
			return true
		}
		if c.Delta.ReasoningContent != nil && strings.TrimSpace(*c.Delta.ReasoningContent) != "" {
			return true
		}
	}
	return false
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

// isEmptyCompletion reports whether a 2xx chat-completion body is
// premature-empty: the first choice has no content (absent/nil/whitespace
// string, or an empty array) and no tool_calls. An empty choices list counts
// as empty; reasoning volume is never present in reassembled content, so it
// does not count. The finish_reason is deliberately not consulted: providers
// that reason and then terminate (e.g. openrouter stealth/ox-alpha) routinely
// stop with a populated finish_reason ("stop"/"length") and an empty message,
// and an empty assistant turn is never useful to the client, so it is retried
// like any other premature-empty result. A body that is not JSON (or
// unparseable) is conservatively NOT empty — the retry loop only fires on a
// provable premature-empty result.
func isEmptyCompletion(body []byte) bool {
	var c struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage   `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return false
	}
	if len(c.Choices) == 0 {
		return true
	}
	ch := c.Choices[0]
	if len(ch.Message.ToolCalls) > 0 {
		return false
	}
	contentEmpty := true
	if len(ch.Message.Content) > 0 {
		var s string
		if err := json.Unmarshal(ch.Message.Content, &s); err == nil {
			contentEmpty = strings.TrimSpace(s) == ""
		} else {
			// Non-string content (e.g. an array of content parts): empty only
			// when it is an empty array.
			var arr []json.RawMessage
			contentEmpty = json.Unmarshal(ch.Message.Content, &arr) == nil && len(arr) == 0
		}
	}
	return contentEmpty
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
