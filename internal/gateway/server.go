// Package gateway implements the §4 HTTP surface: /healthz, /v1/models, and
// the /v1/chat/completions capture surface (stream + non-stream) with §4.4
// streaming correctness (line-by-line forward with per-chunk flush,
// index-stable tool-call delta reassembly, truncation on disconnect), §4.3
// session correlation, one capture transaction per request (resolved review
// decision #6), and the §8 append JSONL log.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"shimmer-llmgateway/internal/api"
	"shimmer-llmgateway/internal/config"
	"shimmer-llmgateway/internal/logging"
	"shimmer-llmgateway/internal/plugins"
	"shimmer-llmgateway/internal/secrets"
	"shimmer-llmgateway/internal/store"
	"shimmer-llmgateway/web"
)

// endpoint is the gateway-facing capture surface path recorded on every row.
const endpoint = "/v1/chat/completions"

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
}

// New builds a gateway server over cfg/st, opening the §8 append log at
// <store>.jsonl, the §6.2 secrets file at <store>.secrets.json (0600), and
// the REST API wired to write config mutations back to configPath. masterKey
// is the decoded AES-256 master key for the secrets file, or nil to have the
// gateway generate one.
func New(cfg *config.ConfigManager, st *store.Store, logger *logging.Logger, configPath string, masterKey []byte) (*Server, error) {
	ap, err := openAppendLog(st.Path() + ".jsonl")
	if err != nil {
		return nil, fmt.Errorf("gateway: open append log %s.jsonl: %w", st.Path(), err)
	}
	sec, err := secrets.Open(st.Path()+".secrets.json", masterKey)
	if err != nil {
		ap.Close()
		return nil, fmt.Errorf("gateway: open secrets %s.secrets.json: %w", st.Path(), err)
	}
	return &Server{
		cfg:            cfg,
		store:          st,
		logger:         logger,
		client:         &http.Client{},
		append:         ap,
		secrets:        sec,
		api:            api.New(cfg, configPath, st, sec, logger),
		emitterFactory: newStreamEmitter,
	}, nil
}

// Handler returns the HTTP router. Any /v1/* path other than the capture
// surface and /v1/models returns 404; /healthz is the repo-convention health
// probe; /api/* is the §6.2 REST surface; / serves the embedded HTML UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.Handle("/api/", s.api.Handler())
	mux.HandleFunc("GET /{$}", s.handleUI)
	return mux
}

// handleUI serves the embedded single-page HTML UI (§6.1).
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	data, err := web.FS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "embedded UI missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
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
// instance listing each model (deduped, per the plan's assumption 6). Disabled
// instances are excluded — they are not routable.
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
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// chatRequest is the subset of the client body the gateway routes on.
type chatRequest struct {
	Model  string `json:"model"`
	Stream *bool  `json:"stream"`
}

// route is the resolved instance/model for one request.
type route struct {
	inst     *config.Instance
	template *config.Template
	model    string
	alias    string
	provider string
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
	tmpl := cfg.Templates[inst.Template]
	rt := &route{
		inst:     inst,
		template: tmpl,
		model:    model,
		alias:    inst.Alias,
		provider: tmpl.Name,
	}

	// §4.3 session correlation: echo the effective session id on every
	// response, stream and non-stream. The id is normalized against the safe
	// charset so a malicious X-Session-Id is replaced with a UUID before it is
	// echoed or persisted (defense in depth against stored XSS via the UI).
	sessionID := store.NormalizeSessionID(r.Header.Get("X-Session-Id"))
	w.Header().Set("X-Gateway-Session-Id", sessionID)

	streaming := parsed.Stream != nil && *parsed.Stream
	if streaming {
		s.handleStream(w, r, cfg, rt, body, sessionID, start)
		return
	}
	s.handleNonStream(w, r, cfg, rt, body, sessionID, start)
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
func (s *Server) buildUpstream(ctx context.Context, cfg *config.Config, rt *route, body []byte) (*http.Request, []byte, error) {
	upstreamBody := body
	if sent, ok := modelField(body); !ok || sent != rt.model {
		rewritten, err := rewriteModel(body, rt.model)
		if err != nil {
			return nil, nil, err
		}
		upstreamBody = rewritten
	}

	url := strings.TrimRight(rt.template.BaseURL, "/") + "/chat/completions"
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
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, upstreamBody, nil
}

// buildChain resolves the §4.5 plugin chain for one instance: per-instance
// plugins override the global settings defaults (empty list = no plugins),
// each name is built from the built-in registry with its [plugins.<name>]
// config. Names are validated at config load; Build still defends against an
// unknown name defensively.
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
	if env != "" {
		if key := os.Getenv(env); key != "" {
			return key, nil
		}
	}
	if key, ok := s.secrets.Get(inst.Alias); ok {
		return key, nil
	}
	if env != "" {
		return "", fmt.Errorf("api key not set for instance %q (expected env var %s or a stored key)", inst.Alias, env)
	}
	return "", nil
}

// handleNonStream forwards the request, records the provider response, runs
// the response plugins on the provider body, and passes the (filtered)
// provider status/body through — error bodies included.
func (s *Server) handleNonStream(w http.ResponseWriter, r *http.Request, cfg *config.Config, rt *route, body []byte, sessionID string, start time.Time) {
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

	req, sentBody, err := s.buildUpstream(r.Context(), cfg, rt, forwardedBody)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.capture(upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, start, err, chain.Applied()))
		s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.capture(upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, start, err, chain.Applied()))
		s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream read failed: "+err.Error())
		return
	}

	rec := &store.CaptureRecord{
		SessionID:      sessionID,
		CreatedAt:      start,
		Alias:          rt.alias,
		Provider:       rt.provider,
		Model:          rt.model,
		Endpoint:       endpoint,
		DurationMS:     durationMS(start),
		StatusCode:     resp.StatusCode,
		RequestJSON:    body,
		PluginsApplied: chain.Applied(),
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
// and the partially reassembled data is persisted truncated.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, cfg *config.Config, rt *route, body []byte, sessionID string, start time.Time) {
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

	req, sentBody, err := s.buildUpstream(ctx, cfg, rt, forwardedBody)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "CONFIG_ERROR", err.Error())
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.capture(upstreamErrorRecord(rt, body, requestFilteredBody(chain, sentBody), sessionID, start, err, chain.Applied()))
		s.writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			respBody = nil
		}
		rec := &store.CaptureRecord{
			SessionID:      sessionID,
			CreatedAt:      start,
			Alias:          rt.alias,
			Provider:       rt.provider,
			Model:          rt.model,
			Endpoint:       endpoint,
			DurationMS:     durationMS(start),
			StatusCode:     resp.StatusCode,
			RequestJSON:    body,
			PluginsApplied: chain.Applied(),
			Error:          providerError(resp.StatusCode, respBody),
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

	lines := make(chan []byte, 64)
	asmDone := make(chan struct{})
	go func() {
		defer close(asmDone)
		for payload := range lines {
			asm.add(payload)
		}
	}()

	// cleanEnd marks a fully delivered stream: the terminating [DONE] event
	// was forwarded or the upstream reached io.EOF. Truncation is decided on
	// loop exit, NOT in the disconnect branches, so a client that closes right
	// after receiving [DONE] does not produce a false truncated:true record
	// (the request context can be canceled before the loop observes EOF).
	truncated := false
	cleanEnd := false
	reader := bufio.NewReader(resp.Body)
	for {
		select {
		case <-r.Context().Done():
			// Client disconnected; cancel upstream so the read unblocks. The
			// clean-end flag still decides truncation.
			cancel()
		default:
		}
		line, err := reader.ReadString('\n')
		if line != "" {
			if werr := emitter.Write([]byte(line)); werr != nil {
				// Client is gone mid-write; cancel upstream. Whether the
				// stream was truncated is decided by cleanEnd on exit.
				cancel()
				break
			}
			if payload := ssePayload(line); payload != nil {
				lines <- payload
				if string(payload) == "[DONE]" {
					cleanEnd = true
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				cleanEnd = true
			} else {
				// Upstream stream did not terminate cleanly.
				cancel()
			}
			break
		}
	}
	close(lines)
	<-asmDone
	_ = emitter.Done()

	if !cleanEnd {
		// The loop exited without a complete stream (client disconnect or
		// upstream read failure before [DONE]/EOF): persist what was
		// reassembled as truncated.
		truncated = true
	}

	reassembled, finish, usage := asm.result()
	rec := &store.CaptureRecord{
		SessionID:      sessionID,
		CreatedAt:      start,
		Alias:          rt.alias,
		Provider:       rt.provider,
		Model:          rt.model,
		Endpoint:       endpoint,
		DurationMS:     durationMS(start),
		StatusCode:     resp.StatusCode,
		FinishReason:   finish,
		Usage:          usage,
		RequestJSON:    body,
		ResponseJSON:   reassembled,
		Truncated:      truncated,
		PluginsApplied: chain.Applied(),
		Error:          asm.streamError(),
		ChunkCount:     asm.chunks(),
	}
	rec.RequestFilteredJSON = requestFilteredBody(chain, sentBody)
	if chain.HasResponse() {
		if be, ok := emitter.(*bufferedEmitter); ok {
			// Byte-exact copy of what the client received.
			rec.ResponseFilteredJSON = be.emittedBody()
		} else {
			// Custom emitter override: filter now for the record.
			respDomain := &plugins.Response{Body: reassembled}
			if err := chain.FilterResponse(context.Background(), respDomain); err == nil {
				rec.ResponseFilteredJSON = respDomain.Body
			}
		}
	}
	s.capture(rec)
}

// capture persists one request in a single store transaction and appends the
// §8 log lines. A fresh context (not the canceled client context) is used so
// a disconnect never drops the record.
func (s *Server) capture(rec *store.CaptureRecord) {
	ctx := context.Background()
	if err := s.store.Capture(ctx, rec); err != nil {
		s.logger.Error("capture_failed", map[string]any{"session_id": rec.SessionID, "error": err.Error()})
		return
	}
	if err := s.append.write(rec); err != nil {
		s.logger.Error("append_log_failed", map[string]any{"session_id": rec.SessionID, "error": err.Error()})
	}
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
// is the post-request-plugin body when request plugins ran, else nil.
func upstreamErrorRecord(rt *route, body, filteredBody []byte, sessionID string, start time.Time, cause error, applied []string) *store.CaptureRecord {
	rec := &store.CaptureRecord{
		SessionID:   sessionID,
		CreatedAt:   start,
		Alias:       rt.alias,
		Provider:    rt.provider,
		Model:       rt.model,
		Endpoint:    endpoint,
		DurationMS:  durationMS(start),
		StatusCode:  502,
		RequestJSON: body,
		Truncated:   true,
		Error:       &store.ErrorInfo{Code: "UPSTREAM_ERROR", Message: "upstream error: " + cause.Error()},
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
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	b, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = b
	return json.Marshal(m)
}

func durationMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

// snippet truncates s to n bytes.
func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
