package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// ---- fake OpenAI-compatible provider ----

type providerRequest struct {
	Authorization    string
	XAPIKey          string
	XOpencode        string
	UserAgent        string
	XOpencodeClient  string
	XOpencodeProject string
	Body             []byte
}

// fakeProvider records what the gateway sends it and runs a per-test handler.
// It is fully self-contained (no external servers).
type fakeProvider struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	seen []providerRequest

	writeMu sync.Mutex
	writes  []string

	h http.HandlerFunc
}

func newFakeProvider(t *testing.T, h http.HandlerFunc) *fakeProvider {
	fp := &fakeProvider{t: t, h: h}
	fp.srv = httptest.NewServer(http.HandlerFunc(fp.handle))
	t.Cleanup(fp.srv.Close)
	return fp
}

func (fp *fakeProvider) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fp.t.Errorf("provider read body: %v", err)
	}
	fp.mu.Lock()
	fp.seen = append(fp.seen, providerRequest{
		Authorization:    r.Header.Get("Authorization"),
		XAPIKey:          r.Header.Get("x-api-key"),
		XOpencode:        r.Header.Get("x-opencode-session"),
		UserAgent:        r.Header.Get("User-Agent"),
		XOpencodeClient:  r.Header.Get("X-Opencode-Client"),
		XOpencodeProject: r.Header.Get("X-Opencode-Project"),
		Body:             body,
	})
	fp.mu.Unlock()
	if fp.h != nil {
		fp.h(w, r)
		return
	}
	http.NotFound(w, r)
}

func (fp *fakeProvider) addWrite(s string) {
	fp.writeMu.Lock()
	fp.writes = append(fp.writes, s)
	fp.writeMu.Unlock()
}

func (fp *fakeProvider) totalWrites() string {
	fp.writeMu.Lock()
	defer fp.writeMu.Unlock()
	return strings.Join(fp.writes, "")
}

func (fp *fakeProvider) requests() []providerRequest {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return append([]providerRequest(nil), fp.seen...)
}

func (fp *fakeProvider) url() string { return fp.srv.URL }

// ---- harness ----

// twoInstanceTOML routes openai/gpt-4o vs openai-2/gpt-4o to two distinct
// accounts, the §9.3 multi-account scenario.
const twoInstanceTOML = `
[settings]
default_alias = "openai"

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"

[[instances]]
alias = "openai-2"
template = "openai"
api_key_env = "TEST_KEY_2"
`

var defaultEnv = map[string]string{"TEST_KEY_1": "sk-account-1", "TEST_KEY_2": "sk-account-2"}

// newGatewayTest starts a gateway routed at the fake provider. The store is
// opened in a temp dir so the append log lands at <store>.jsonl.
func newGatewayTest(t *testing.T, provider *fakeProvider, instances string, env map[string]string) (*httptest.Server, *store.Store) {
	t.Helper()
	srv, st := newGatewayServer(t, provider, instances, env, io.Discard)
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)
	return gs, st
}

// newGatewayServer builds the gateway Server (not yet served) with console
// logs written to logw, so tests can observe the JSON log stream.
func newGatewayServer(t *testing.T, provider *fakeProvider, instances string, env map[string]string, logw io.Writer) (*Server, *store.Store) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	storePath := filepath.Join(dir, "gateway.db")

	base := fmt.Sprintf("[providers.openai]\nbase_url = %q\napi_key_env = \"GATEWAY_TEST_KEY\"\nmodels = [\"gpt-4o\", \"gpt-4o-mini\"]\n\n", provider.url()+"/v1")
	cfg, err := config.Parse([]byte(base + instances))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	mgr := config.New(cfg)
	st, err := store.Open(storePath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := New(mgr, st, logging.New(logw), filepath.Join(dir, "gateway.toml"), nil, nil)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return srv, st
}

func postChat(t *testing.T, gs *httptest.Server, body string, headers map[string]string) *http.Response {
	t.Helper()
	return postChatWithUA(t, gs, body, headers, true)
}

// postChatNoUA is postChat with the User-Agent header explicitly suppressed:
// Go's http client would otherwise inject "Go-http-client/1.1", which the
// gateway would faithfully forward. Suppressing it lets the test observe the
// no-client-UA fallback path (the gateway's own constant UA upstream).
func postChatNoUA(t *testing.T, gs *httptest.Server, body string, headers map[string]string) *http.Response {
	t.Helper()
	return postChatWithUA(t, gs, body, headers, false)
}

func postChatWithUA(t *testing.T, gs *httptest.Server, body string, headers map[string]string, sendUA bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gs.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if !sendUA {
		// Explicit empty UA suppresses the transport's auto-injected
		// Go-http-client/1.1 while still satisfying the http.Client.
		req.Header["User-Agent"] = nil
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	return resp
}

func drainClose(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return b
}

// waitForRequest polls the store until the request is captured (the gateway
// captures after the stream completes or the client disconnects).
func waitForRequest(t *testing.T, st *store.Store, sessionID string, timeout time.Duration) *store.Request {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess, err := st.GetSession(context.Background(), sessionID)
		if err == nil && len(sess.Requests) > 0 {
			return sess.Requests[len(sess.Requests)-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("request for session %q not captured within %v", sessionID, timeout)
	return nil
}

// readUntilContent reads streamed lines until the accumulated text contains
// needle, returning the accumulated text.
func readUntilContent(t *testing.T, br *bufio.Reader, d time.Duration, needle string) string {
	t.Helper()
	deadline := time.Now().Add(d)
	var buf strings.Builder
	for time.Now().Before(deadline) {
		done := make(chan struct{})
		var line string
		var err error
		go func() {
			line, err = br.ReadString('\n')
			close(done)
		}()
		select {
		case <-done:
			buf.WriteString(line)
			if strings.Contains(buf.String(), needle) {
				return buf.String()
			}
			if err != nil {
				t.Fatalf("stream read: %v", err)
			}
		case <-time.After(1 * time.Second):
		}
	}
	t.Fatalf("timed out waiting for %q in the stream", needle)
	return ""
}

func readAppendLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read append log: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// ---- tests ----

func TestHealthzModelsAndV1NotFound(t *testing.T) {
	provider := newFakeProvider(t, nil)
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// /healthz
	resp, err := http.Get(gs.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}
	if strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Errorf("healthz body = %q", body)
	}

	// /v1/models: OpenAI-shaped list, unprefixed ids deduped to first instance.
	resp, err = http.Get(gs.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body = drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200", resp.StatusCode)
	}
	var models struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &models); err != nil {
		t.Fatalf("models body: %v", err)
	}
	if models.Object != "list" {
		t.Errorf("models object = %q, want list", models.Object)
	}
	got := map[string]bool{}
	unprefixed := map[string]int{}
	for _, m := range models.Data {
		got[m.ID] = true
		if m.Object != "model" {
			t.Errorf("model %q object = %q, want model", m.ID, m.Object)
		}
		if !strings.Contains(m.ID, "/") {
			unprefixed[m.ID]++
		}
	}
	for _, want := range []string{
		"gpt-4o", "gpt-4o-mini",
		"openai/gpt-4o", "openai/gpt-4o-mini",
		"openai-2/gpt-4o", "openai-2/gpt-4o-mini",
	} {
		if !got[want] {
			t.Errorf("models list missing %q", want)
		}
	}
	if unprefixed["gpt-4o"] != 1 || unprefixed["gpt-4o-mini"] != 1 {
		t.Errorf("unprefixed ids deduped wrong: %v", unprefixed)
	}

	// Any other /v1/* path is 404 (including a GET on the capture surface).
	for _, path := range []string{"/v1/whatever", "/v1/chat/completions", "/v1/models/extra"} {
		resp, err = http.Get(gs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		drainClose(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestUnknownAliasReturns400WithAliases(t *testing.T) {
	provider := newFakeProvider(t, nil)
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"nope/gpt-4o","messages":[{"role":"user","content":"hi"}]}`, nil)
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if e.Code != "INVALID_ARGUMENT" {
		t.Errorf("code = %q, want INVALID_ARGUMENT", e.Code)
	}
	for _, want := range []string{"nope", "openai", "openai-2"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q missing %q", e.Message, want)
		}
	}

	// Unknown unprefixed model is the same 400 shape.
	resp = postChat(t, gs, `{"model":"no-such-model"}`, nil)
	body = drainClose(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown model status = %d, want 400", resp.StatusCode)
	}
	var e2 struct {
		Code string `json:"code"`
	}
	json.Unmarshal(body, &e2)
	if e2.Code != "INVALID_ARGUMENT" {
		t.Errorf("unknown model code = %q, want INVALID_ARGUMENT", e2.Code)
	}
	// The provider must never have seen the rejected request.
	if n := len(provider.requests()); n != 0 {
		t.Errorf("provider saw %d requests for rejected input, want 0", n)
	}
}

func TestMissingKeyReturns500ConfigError(t *testing.T) {
	provider := newFakeProvider(t, nil)
	gs, _ := newGatewayTest(t, provider, `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "NEVER_SET_ENV"
`, nil)

	resp := postChat(t, gs, `{"model":"gpt-4o"}`, nil)
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	json.Unmarshal(body, &e)
	if e.Code != "CONFIG_ERROR" {
		t.Errorf("code = %q, want CONFIG_ERROR", e.Code)
	}
	if !strings.Contains(e.Message, "NEVER_SET_ENV") {
		t.Errorf("message %q should name the missing env var", e.Message)
	}
}

func TestSessionCorrelationEcho(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// Non-stream: client-provided session id is echoed.
	resp := postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": "my-sess"})
	drainClose(t, resp)
	if got := resp.Header.Get("X-Gateway-Session-Id"); got != "my-sess" {
		t.Errorf("non-stream X-Gateway-Session-Id = %q, want my-sess", got)
	}

	// Non-stream: absent header gets a generated id.
	resp = postChat(t, gs, `{"model":"gpt-4o"}`, nil)
	drainClose(t, resp)
	if got := resp.Header.Get("X-Gateway-Session-Id"); got == "" {
		t.Error("non-stream X-Gateway-Session-Id missing for generated session")
	}

	// Stream path echoes too (resolved review decision #3).
	resp = postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "stream-sess"})
	drainClose(t, resp)
	if got := resp.Header.Get("X-Gateway-Session-Id"); got != "stream-sess" {
		t.Errorf("stream X-Gateway-Session-Id = %q, want stream-sess", got)
	}
}

func TestNonStreamForwardAndCapture(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-ns"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "chatcmpl-x") {
		t.Errorf("client did not receive the provider body: %q", body)
	}

	req := waitForRequest(t, st, "sess-ns", 5*time.Second)
	if req.Alias != "openai" || req.Provider != "openai" || req.Model != "gpt-4o" {
		t.Errorf("routing = %s/%s/%s, want openai/openai/gpt-4o", req.Alias, req.Provider, req.Model)
	}
	if req.Endpoint != "/v1/chat/completions" {
		t.Errorf("endpoint = %q", req.Endpoint)
	}
	if req.StatusCode != http.StatusOK || req.FinishReason != "stop" {
		t.Errorf("status/finish = %d/%q, want 200/stop", req.StatusCode, req.FinishReason)
	}
	if req.DurationMS < 0 {
		t.Errorf("duration_ms = %d, want >= 0", req.DurationMS)
	}
	var usage struct {
		TotalTokens int `json:"total_tokens"`
	}
	if err := json.Unmarshal(req.Usage, &usage); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.TotalTokens != 8 {
		t.Errorf("usage total_tokens = %d, want 8", usage.TotalTokens)
	}
	var sent struct {
		Model string `json:"model"`
	}
	json.Unmarshal(req.RequestJSON, &sent)
	if sent.Model != "gpt-4o" {
		t.Errorf("request_json model = %q, want gpt-4o verbatim", sent.Model)
	}
	if len(req.ResponseJSON) == 0 {
		t.Error("response_json empty")
	}
	if req.Truncated {
		t.Error("truncated = true, want false")
	}
	sess, err := st.GetSession(context.Background(), "sess-ns")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestCount != 1 || sess.FailureCount != 0 {
		t.Errorf("session counters = %d/%d, want 1/0", sess.RequestCount, sess.FailureCount)
	}
}

// Chat image input is verbatim passthrough: the provider sees the image_url
// content part exactly as received, and capture stores the as-received bytes.
// Regression guard — the chat request path must never be rewritten.
func TestChatCompletionsImagePassthrough(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"a cat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	raw := `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"https://x/cat.png"}}]}]}`
	resp := postChat(t, gs, raw, map[string]string{"X-Session-Id": "sess-chatimg"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}

	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	var upstream struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(seen[0].Body, &upstream); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, seen[0].Body)
	}
	if upstream.Model != "gpt-4o" || len(upstream.Messages) != 1 || upstream.Messages[0].Role != "user" {
		t.Errorf("upstream body = %s", seen[0].Body)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(upstream.Messages[0].Content, &parts); err != nil {
		t.Fatalf("upstream content not a part array: %v (%s)", err, upstream.Messages[0].Content)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "what is this?" ||
		parts[1].Type != "image_url" || parts[1].ImageURL.URL != "https://x/cat.png" {
		t.Errorf("upstream parts = %+v, want the image_url part forwarded verbatim", parts)
	}

	// Capture: the as-received body, image included.
	req := waitForRequest(t, st, "sess-chatimg", 5*time.Second)
	if req.Endpoint != "/v1/chat/completions" {
		t.Errorf("endpoint = %q, want /v1/chat/completions", req.Endpoint)
	}
	if string(req.RequestJSON) != raw {
		t.Errorf("request_json not the as-received body:\n got %q\nwant %q", req.RequestJSON, raw)
	}
}

func TestNonStreamNon2xxRecordedAndPassthrough(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key","type":"authentication_error","code":"invalid_api_key"}}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": "sess-err"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(string(body), "bad key") {
		t.Errorf("provider error body not passed through: %q", body)
	}

	req := waitForRequest(t, st, "sess-err", 5*time.Second)
	if req.StatusCode != http.StatusUnauthorized {
		t.Errorf("status_code = %d, want 401", req.StatusCode)
	}
	if req.Error == nil || req.Error.Code != "invalid_api_key" || req.Error.Message != "bad key" {
		t.Errorf("error_json = %+v, want {invalid_api_key, bad key}", req.Error)
	}
	if len(req.ResponseJSON) != 0 {
		t.Errorf("response_json should be empty for non-2xx, got %q", req.ResponseJSON)
	}
	if req.Truncated {
		t.Error("truncated = true, want false (complete error response)")
	}
	sess, err := st.GetSession(context.Background(), "sess-err")
	if err != nil {
		t.Fatal(err)
	}
	if sess.FailureCount != 1 {
		t.Errorf("failure_count = %d, want 1", sess.FailureCount)
	}
}

func TestSSEParityAndLiveForward(t *testing.T) {
	var provider *fakeProvider
	proceed := make(chan struct{})
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		write := func(s string) {
			w.Write([]byte(s))
			fl.Flush()
			provider.addWrite(s)
		}
		write(": keepalive\n\n")
		write("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1722600000,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		<-proceed
		write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":null}]}\n\n")
		write("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: [DONE]\n\n")
	})
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	// The keepalive comment and first chunk must arrive while the provider is
	// still blocked on proceed — proving the gateway forwards live and never
	// buffers the whole stream (§4.4).
	before := readUntilContent(t, br, 3*time.Second, "Hello")
	if !strings.Contains(before, "keepalive") {
		t.Errorf("expected the keepalive comment before the first chunk, got %q", before)
	}
	close(proceed)

	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read rest of stream: %v", err)
	}
	got := before + string(rest)
	want := provider.totalWrites()
	if got != want {
		t.Errorf("client bytes != provider bytes\nclient: %q\nprovider: %q", got, want)
	}
}

func TestStreamingReassemblyInterleavedToolsAndUsage(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeEvent := func(j string) {
			fmt.Fprintf(w, "data: %s\n\n", j)
			fl.Flush()
		}
		writeEvent(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`)
		writeEvent(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]},"finish_reason":null}]}`)
		writeEvent(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{\"tz"}}]},"finish_reason":null}]}`)
		writeEvent(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\": \"Paris\"}"}}]},"finish_reason":null}]}`)
		writeEvent(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\": \"UTC\"}"}}]},"finish_reason":null}]}`)
		writeEvent(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		writeEvent(`{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"openai/gpt-4o","stream":true,"messages":[{"role":"user","content":"weather"}]}`, map[string]string{"X-Session-Id": "sess-reasm"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	drainClose(t, resp)

	req := waitForRequest(t, st, "sess-reasm", 5*time.Second)

	// Capture completeness.
	if req.Alias != "openai" || req.Provider != "openai" || req.Model != "gpt-4o" {
		t.Errorf("routing = %s/%s/%s, want openai/openai/gpt-4o", req.Alias, req.Provider, req.Model)
	}
	if req.StatusCode != http.StatusOK || req.Truncated {
		t.Errorf("status/truncated = %d/%v, want 200/false", req.StatusCode, req.Truncated)
	}
	if req.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", req.FinishReason)
	}
	var usage struct {
		TotalTokens int `json:"total_tokens"`
	}
	if err := json.Unmarshal(req.Usage, &usage); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.TotalTokens != 30 {
		t.Errorf("usage total_tokens = %d, want 30", usage.TotalTokens)
	}
	var sent struct {
		Model string `json:"model"`
	}
	json.Unmarshal(req.RequestJSON, &sent)
	if sent.Model != "openai/gpt-4o" {
		t.Errorf("request_json model = %q, want openai/gpt-4o verbatim", sent.Model)
	}

	// Reassembled response: per-choice content, per-index arguments stable.
	var comp struct {
		Object  string `json:"object"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(req.ResponseJSON, &comp); err != nil {
		t.Fatalf("reassembled response: %v", err)
	}
	if comp.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", comp.Object)
	}
	if len(comp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(comp.Choices))
	}
	ch := comp.Choices[0]
	if ch.Message.Content != nil {
		t.Errorf("content = %v, want null when tool_calls present", ch.Message.Content)
	}
	if len(ch.Message.ToolCalls) != 2 {
		t.Fatalf("tool_calls = %d, want 2", len(ch.Message.ToolCalls))
	}
	tc0 := ch.Message.ToolCalls[0]
	if tc0.ID != "call_1" || tc0.Function.Name != "get_weather" || tc0.Type != "function" {
		t.Errorf("tool_call[0] = %+v", tc0)
	}
	if tc0.Function.Arguments != `{"city": "Paris"}` {
		t.Errorf("tool_call[0].arguments = %q, want interleaved concatenation", tc0.Function.Arguments)
	}
	tc1 := ch.Message.ToolCalls[1]
	if tc1.ID != "call_2" || tc1.Function.Name != "get_time" {
		t.Errorf("tool_call[1] = %+v", tc1)
	}
	if tc1.Function.Arguments != `{"tz": "UTC"}` {
		t.Errorf("tool_call[1].arguments = %q, want interleaved concatenation", tc1.Function.Arguments)
	}

	// Store tool_calls rows carry the parsed compact arguments.
	calls, err := st.ListToolCalls(context.Background(), store.ToolCallFilter{SessionID: "sess-reasm"})
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("tool call rows = %d, want 2", len(calls))
	}
	byID := map[string]*store.ToolCall{}
	for _, c := range calls {
		byID[c.ID] = c
	}
	if c := byID[req.ID+"/0"]; c == nil || c.ToolName != "get_weather" || string(c.ArgumentsJSON) != `{"city":"Paris"}` {
		t.Errorf("tool call row /0 = %+v, args %q", c, argsOf(c))
	}
	if c := byID[req.ID+"/1"]; c == nil || c.ToolName != "get_time" || string(c.ArgumentsJSON) != `{"tz":"UTC"}` {
		t.Errorf("tool call row /1 = %+v, args %q", c, argsOf(c))
	}

	// Append log: §8 lines — session_start + request + tool_call + tool_call,
	// with the request line carrying chunk_count (7 data chunks).
	lines := readAppendLog(t, st.Path()+".jsonl")
	if len(lines) != 4 {
		t.Fatalf("append log lines = %d, want 4: %v", len(lines), lines)
	}
	wantTypes := []string{"session_start", "request", "tool_call", "tool_call"}
	for i, want := range wantTypes {
		var r struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &r); err != nil {
			t.Fatalf("append log line %d: %v", i, err)
		}
		if r.Type != want {
			t.Errorf("append log line %d type = %q, want %q", i, r.Type, want)
		}
	}
	var reqLine struct {
		SessionID  string `json:"session_id"`
		RequestID  string `json:"request_id"`
		ChunkCount int    `json:"chunk_count"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &reqLine); err != nil {
		t.Fatal(err)
	}
	if reqLine.SessionID != "sess-reasm" || reqLine.RequestID != req.ID {
		t.Errorf("append log request line = %+v", reqLine)
	}
	if reqLine.ChunkCount != 7 {
		t.Errorf("append log chunk_count = %d, want 7", reqLine.ChunkCount)
	}

	// ExportSession shares the same canonical line stream (chunk_count is not
	// a §5 column, so stored rows read it as 0 and export omits it).
	var sb strings.Builder
	if err := st.ExportSession(context.Background(), "sess-reasm", &sb); err != nil {
		t.Fatal(err)
	}
	exportLines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	if len(exportLines) != 4 {
		t.Errorf("export lines = %d, want 4", len(exportLines))
	}
	if exportLines[1] == lines[1] {
		t.Errorf("export request line unexpectedly equals the append-log line (chunk_count should differ)")
	}
	if strings.Contains(exportLines[1], "chunk_count") {
		t.Errorf("export request line should not carry chunk_count (no DB column): %s", exportLines[1])
	}
}

func argsOf(c *store.ToolCall) string {
	if c == nil {
		return "<nil>"
	}
	return string(c.ArgumentsJSON)
}

func TestStreamErrorEventPassThroughAndRecording(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"error\":{\"message\":\"stream broke\",\"type\":\"server_error\",\"code\":null}}\n\n")
		fl.Flush()
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-streamerr"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE errors ride on 200)", resp.StatusCode)
	}
	if !strings.Contains(string(body), "stream broke") {
		t.Errorf("error event not passed through: %q", body)
	}

	req := waitForRequest(t, st, "sess-streamerr", 5*time.Second)
	if req.Truncated {
		t.Error("truncated = true, want false (clean EOF after error event)")
	}
	// The provider sent code:null, type:"server_error"; the gateway records
	// the provider's classification when code is absent.
	if req.Error == nil || req.Error.Code != "server_error" || req.Error.Message != "stream broke" {
		t.Errorf("error_json = %+v, want {server_error, stream broke}", req.Error)
	}
	sess, err := st.GetSession(context.Background(), "sess-streamerr")
	if err != nil {
		t.Fatal(err)
	}
	if sess.FailureCount != 1 {
		t.Errorf("failure_count = %d, want 1", sess.FailureCount)
	}
}

func TestClientDisconnectPersistsTruncated(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		i := 0
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"chunk-%d \"},\"finish_reason\":null}]}\n\n", i)
			fl.Flush()
			i++
			time.Sleep(10 * time.Millisecond)
		}
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-trunc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	readUntilContent(t, br, 3*time.Second, "chunk-")
	resp.Body.Close() // abort mid-stream

	req := waitForRequest(t, st, "sess-trunc", 5*time.Second)
	if !req.Truncated {
		t.Error("truncated = false, want true")
	}
	if len(req.ResponseJSON) == 0 {
		t.Fatal("no partial response captured")
	}
	var comp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(req.ResponseJSON, &comp); err != nil {
		t.Fatalf("partial response: %v", err)
	}
	if len(comp.Choices) != 1 || comp.Choices[0].Message.Content == "" {
		t.Errorf("expected partial content, got %q", req.ResponseJSON)
	}
}

func TestClientCloseAfterFullStreamIsNotTruncated(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		// Deliver a complete [DONE]-terminated stream, then keep the
		// connection open with keepalive comments so the gateway's forward
		// loop keeps iterating and its per-iteration context check can observe
		// the client close AFTER [DONE] was already forwarded.
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			fmt.Fprintf(w, ": keepalive\n\n")
			fl.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-cleanend"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Read the full [DONE]-terminated stream, then close the connection.
	br := bufio.NewReader(resp.Body)
	readUntilContent(t, br, 3*time.Second, "[DONE]")
	resp.Body.Close()

	req := waitForRequest(t, st, "sess-cleanend", 5*time.Second)
	if req.Truncated {
		t.Error("truncated = true, want false: client closed after receiving the full [DONE]-terminated stream")
	}
	if len(req.ResponseJSON) == 0 {
		t.Error("no response captured")
	}
}

func TestMultiAccountKeyIsolation(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// Route the same template's two accounts.
	drainClose(t, postChat(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"a"}]}`, map[string]string{"X-Session-Id": "sess-acct-1"}))
	drainClose(t, postChat(t, gs, `{"model":"openai-2/gpt-4o","messages":[{"role":"user","content":"b"}]}`, map[string]string{"X-Session-Id": "sess-acct-2"}))

	seen := provider.requests()
	if len(seen) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(seen))
	}
	// Each account must have seen only its own key.
	if seen[0].Authorization != "Bearer sk-account-1" {
		t.Errorf("account 1 saw %q, want Bearer sk-account-1", seen[0].Authorization)
	}
	if seen[1].Authorization != "Bearer sk-account-2" {
		t.Errorf("account 2 saw %q, want Bearer sk-account-2", seen[1].Authorization)
	}
	// The provider receives the plain resolved model name, not the routing key.
	for i, pr := range seen {
		var b struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(pr.Body, &b); err != nil {
			t.Fatalf("provider body %d: %v", i, err)
		}
		if b.Model != "gpt-4o" {
			t.Errorf("request %d forwarded model %q, want gpt-4o", i, b.Model)
		}
	}

	// Keys never reach the store: stored payloads must not contain them.
	for _, session := range []string{"sess-acct-1", "sess-acct-2"} {
		sess, err := st.GetSession(context.Background(), session)
		if err != nil {
			t.Fatalf("GetSession(%s): %v", session, err)
		}
		if len(sess.Requests) != 1 {
			t.Fatalf("session %s requests = %d, want 1", session, len(sess.Requests))
		}
		req := sess.Requests[0]
		all := append(append([]byte{}, req.RequestJSON...), req.ResponseJSON...)
		if strings.Contains(string(all), "sk-account") {
			t.Errorf("session %s stored payload contains an API key", session)
		}
	}
}

// sessionIDFromRequest resolves the session id from client headers: an
// explicit X-Session-Id always wins, the x-opencode-session header opencode-
// style clients send natively is honored next, and the x-session-id
// session-affinity compat spelling is matched by the first branch (header
// lookup is case-insensitive — setting x-session-id stores it as the
// canonical X-Session-Id).
func TestSessionIDFromRequest(t *testing.T) {
	newReq := func(kvs ...string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		for i := 0; i+1 < len(kvs); i += 2 {
			r.Header.Set(kvs[i], kvs[i+1])
		}
		return r
	}
	cases := []struct {
		name    string
		headers []string
		want    string
	}{
		{"no headers", nil, ""},
		{"explicit X-Session-Id", []string{"X-Session-Id", "sess-a"}, "sess-a"},
		{"affinity compat spelling x-session-id", []string{"x-session-id", "sess-c"}, "sess-c"},
		{"opencode native header", []string{"x-opencode-session", "sess-b"}, "sess-b"},
		{"X-Session-Id wins over x-opencode-session", []string{"X-Session-Id", "sess-a", "x-opencode-session", "sess-b"}, "sess-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionIDFromRequest(newReq(tc.headers...)); got != tc.want {
				t.Errorf("sessionIDFromRequest(%v) = %q, want %q", tc.headers, got, tc.want)
			}
		})
	}
}

// Templates may declare a session_header (opencode_go → x-opencode-session):
// the gateway synthesises the request's effective session id on every upstream
// call — the same id echoed as X-Gateway-Session-Id, stable across the turns
// of one conversation (client X-Session-Id when present, else a fresh UUID).
// Templates without a session_header never get the header.
func TestSessionHeaderForwardedToUpstream(t *testing.T) {
	const tmpl = `
[providers.opencode_go]
base_url = "FAKE"
api_key_env = "TEST_KEY_1"
models = ["deepseek-v4-flash"]
session_header = "x-opencode-session"

[[instances]]
alias = "opencode_go_ali"
template = "opencode_go"

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`
	var streamCount atomic.Int32
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// Requests are awaited sequentially, so a count tells the stream call
		// (request 2) from the rest without re-reading the consumed body.
		if streamCount.Add(1) == 2 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
			w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	cfg := strings.ReplaceAll(tmpl, "FAKE", provider.url())
	gs, st := newGatewayTest(t, provider, cfg, defaultEnv)

	// Non-stream: the client's X-Session-Id rides upstream verbatim.
	drainClose(t, postChat(t, gs, `{"model":"opencode_go_ali/deepseek-v4-flash","messages":[{"role":"user","content":"a"}]}`, map[string]string{"X-Session-Id": "sess-ocgo"}))
	// Stream: same header synthesis on the SSE path.
	drainClose(t, postChat(t, gs, `{"model":"opencode_go_ali/deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"b"}]}`, map[string]string{"X-Session-Id": "sess-ocgo2"}))
	// Template without a session_header: header never sent, even with an id.
	drainClose(t, postChat(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"c"}]}`, map[string]string{"X-Session-Id": "sess-plain"}))
	// No client id: a UUID is generated and still synthesised upstream.
	drainClose(t, postChat(t, gs, `{"model":"opencode_go_ali/deepseek-v4-flash","messages":[{"role":"user","content":"d"}]}`, nil))

	seen := provider.requests()
	if len(seen) != 4 {
		t.Fatalf("provider saw %d requests, want 4", len(seen))
	}
	// Order: opencode non-stream, opencode stream, openai (no session_header),
	// opencode with no client-supplied id (generated UUID instead).
	want := []string{"sess-ocgo", "sess-ocgo2", "", ""}
	for i, pr := range seen {
		if i == 2 {
			if pr.XOpencode != "" {
				t.Errorf("request %d (openai) got x-opencode-session %q, want none", i, pr.XOpencode)
			}
			continue
		}
		if want[i] != "" && pr.XOpencode != want[i] {
			t.Errorf("request %d x-opencode-session = %q, want %q", i, pr.XOpencode, want[i])
		}
		if want[i] == "" && pr.XOpencode == "" {
			t.Errorf("request %d x-opencode-session = empty, want synthesized", i)
		}
	}
	// The no-header request's synthesized id (a normalized UUID) is echoed
	// back upstream and persisted — the client never sent it.
	lastSess, err := st.GetSession(context.Background(), seen[3].XOpencode)
	if err != nil {
		t.Errorf("generated session id was not persisted: %v", err)
	}
	if lastSess == nil || len(lastSess.Requests) != 1 {
		t.Errorf("generated session request count = %v", lastSess)
	}
}

// The gateway identifies itself on every upstream call: the inbound client's
// User-Agent is forwarded verbatim when present (Go's http client injects
// "Go-http-client/1.1" by default, so the fallback path is exercised via an
// explicitly suppressed UA); a request with no client UA upstreams the shared
// config.UserAgent constant instead of Go's generic default.
func TestUserAgentForwardedToUpstream(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// Client UA forwarded verbatim (stream and non-stream share buildUpstream).
	drainClose(t, postChat(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"a"}]}`, map[string]string{"User-Agent": "my-agent/1.0"}))
	drainClose(t, postChat(t, gs, `{"model":"openai/gpt-4o","stream":true,"messages":[{"role":"user","content":"b"}]}`, map[string]string{"User-Agent": "stream-agent/2.0"}))
	// No client UA: the gateway's constant replaces Go's default.
	drainClose(t, postChatNoUA(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"c"}]}`, nil))

	seen := provider.requests()
	if len(seen) != 3 {
		t.Fatalf("provider saw %d requests, want 3", len(seen))
	}
	wantUA := []string{"my-agent/1.0", "stream-agent/2.0", config.UserAgent}
	for i, pr := range seen {
		if pr.UserAgent != wantUA[i] {
			t.Errorf("request %d User-Agent = %q, want %q", i, pr.UserAgent, wantUA[i])
		}
	}
}

// Templates may declare identity_headers (opencode_go → X-Opencode-Client /
// X-Opencode-Project): every upstream call to that template carries the
// declared values, and a client-supplied value for the same header name wins
// over the declared default. Templates without identity_headers never get
// them.
func TestIdentityHeadersForwardedToUpstream(t *testing.T) {
	const tmpl = `
[providers.opencode_go]
base_url = "FAKE"
api_key_env = "TEST_KEY_1"
models = ["deepseek-v4-flash"]
session_header = "x-opencode-session"
identity_headers = { "X-Opencode-Client" = "cli", "X-Opencode-Project" = "global" }

[[instances]]
alias = "opencode_go_ali"
template = "opencode_go"

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	cfg := strings.ReplaceAll(tmpl, "FAKE", provider.url())
	gs, _ := newGatewayTest(t, provider, cfg, defaultEnv)

	// Declared defaults ride upstream.
	drainClose(t, postChat(t, gs, `{"model":"opencode_go_ali/deepseek-v4-flash","messages":[{"role":"user","content":"a"}]}`, nil))
	// Client-supplied X-Opencode-Client wins over the declared default.
	drainClose(t, postChat(t, gs, `{"model":"opencode_go_ali/deepseek-v4-flash","messages":[{"role":"user","content":"b"}]}`, map[string]string{"X-Opencode-Client": "my-cli"}))
	// Template without identity_headers never gets them.
	drainClose(t, postChat(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"c"}]}`, nil))

	seen := provider.requests()
	if len(seen) != 3 {
		t.Fatalf("provider saw %d requests, want 3", len(seen))
	}
	if got := seen[0]; got.XOpencodeClient != "cli" || got.XOpencodeProject != "global" {
		t.Errorf("request 0 identity headers = (%q, %q), want (cli, global)", got.XOpencodeClient, got.XOpencodeProject)
	}
	if got := seen[1]; got.XOpencodeClient != "my-cli" || got.XOpencodeProject != "global" {
		t.Errorf("request 1 identity headers = (%q, %q), want (my-cli, global)", got.XOpencodeClient, got.XOpencodeProject)
	}
	if got := seen[2]; got.XOpencodeClient != "" || got.XOpencodeProject != "" {
		t.Errorf("request 2 (openai) identity headers = (%q, %q), want none", got.XOpencodeClient, got.XOpencodeProject)
	}
}

// ---- per-model style routing (model_styles) ----

// Model_styles routes each resolved model of one template to the protocol the
// upstream expects: the template default (empty = openai chat) serves
// /chat/completions, a "responses" override serves /responses, and an
// "anthropic" override serves /messages with x-api-key auth. The Phase 1
// session/identity/UA headers ride every branch.
func TestModelStylesPerModelRouting(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses"):
			w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"muse-spark-1.3-contributor","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"resp-hi","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Write([]byte(`{"id":"msg_1","model":"minimax-m3","content":[{"type":"text","text":"ant-hi"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`))
		default:
			w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"chat-hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
		}
	})
	toml := fmt.Sprintf(`
[settings]
default_alias = "ocg"

[providers.opencode_go]
base_url = %q
api_key_env = "TEST_KEY_1"
models = ["deepseek-v4-flash", "muse-spark-1.3-contributor", "minimax-m3"]
session_header = "x-opencode-session"
identity_headers = { "X-Opencode-Client" = "cli", "X-Opencode-Project" = "global" }
model_styles = { "muse-spark-1.3-contributor" = "responses", "minimax-m3" = "anthropic" }

[[instances]]
alias = "ocg"
template = "opencode_go"
`, provider.url()+"/v1")
	gs, st := newGatewayTest(t, provider, toml, defaultEnv)

	for _, tc := range []struct {
		model       string
		wantPath    string
		wantAuth    string // "bearer" or "x-api-key"
		wantContent string
	}{
		{"deepseek-v4-flash", "/v1/chat/completions", "bearer", "chat-hi"},
		{"muse-spark-1.3-contributor", "/v1/responses", "bearer", "resp-hi"},
		{"minimax-m3", "/v1/messages", "x-api-key", "ant-hi"},
	} {
		resp := postChat(t, gs, `{"model":"ocg/`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
			"X-Session-Id":      "sess-style-" + tc.model,
			"User-Agent":        "style-client/1.0",
			"X-Opencode-Client": "cli-custom",
		})
		body := drainClose(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (%s)", tc.model, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), tc.wantContent) {
			t.Errorf("%s client body missing %q: %q", tc.model, tc.wantContent, body)
		}
	}

	seen := provider.requests()
	if len(seen) != 3 {
		t.Fatalf("provider saw %d requests, want 3", len(seen))
	}
	for i, tc := range []struct {
		model       string
		wantPath    string
		wantAuth    string
		wantContent string
	}{
		{"deepseek-v4-flash", "/v1/chat/completions", "bearer", "chat-hi"},
		{"muse-spark-1.3-contributor", "/v1/responses", "bearer", "resp-hi"},
		{"minimax-m3", "/v1/messages", "x-api-key", "ant-hi"},
	} {
		mu.Lock()
		gotPath := paths[i]
		mu.Unlock()
		if !strings.HasSuffix(gotPath, tc.wantPath) {
			t.Errorf("request %d (%s) upstream path = %q, want %q suffix", i, tc.model, gotPath, tc.wantPath)
		}
		pr := seen[i]
		if tc.wantAuth == "x-api-key" {
			if pr.XAPIKey != "sk-account-1" {
				t.Errorf("request %d (%s) x-api-key = %q, want sk-account-1", i, tc.model, pr.XAPIKey)
			}
			if pr.Authorization != "" {
				t.Errorf("request %d (%s) Authorization = %q, want none on the anthropic branch", i, tc.model, pr.Authorization)
			}
		} else {
			if pr.Authorization != "Bearer sk-account-1" {
				t.Errorf("request %d (%s) Authorization = %q, want Bearer sk-account-1", i, tc.model, pr.Authorization)
			}
			if pr.XAPIKey != "" {
				t.Errorf("request %d (%s) x-api-key = %q, want none on the chat/responses branch", i, tc.model, pr.XAPIKey)
			}
		}
		// Phase 1 headers ride every protocol branch.
		if pr.XOpencode != "sess-style-"+tc.model {
			t.Errorf("request %d (%s) x-opencode-session = %q, want sess-style-%s", i, tc.model, pr.XOpencode, tc.model)
		}
		if pr.UserAgent != "style-client/1.0" {
			t.Errorf("request %d (%s) User-Agent = %q, want style-client/1.0", i, tc.model, pr.UserAgent)
		}
		// A client-supplied identity header value wins over the declared default.
		if pr.XOpencodeClient != "cli-custom" || pr.XOpencodeProject != "global" {
			t.Errorf("request %d (%s) identity headers = %q/%q, want cli-custom/global", i, tc.model, pr.XOpencodeClient, pr.XOpencodeProject)
		}
	}

	// Capture stays chat-shaped on every branch (translation happens at the
	// upstream boundary only).
	for _, model := range []string{"deepseek-v4-flash", "muse-spark-1.3-contributor", "minimax-m3"} {
		req := waitForRequest(t, st, "sess-style-"+model, 5*time.Second)
		if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
			t.Errorf("%s response_json should stay chat-shaped: %q", model, req.ResponseJSON)
		}
		if req.FinishReason != "stop" || req.Error != nil {
			t.Errorf("%s finish/error = %q/%+v, want stop/nil", model, req.FinishReason, req.Error)
		}
	}
}

// ---- Phase 5: §6.2 REST surface mounted on the gateway + hot reload ----

// apiDo performs a JSON request against the gateway's /api/ surface.
func apiDo(t *testing.T, gs *httptest.Server, method, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, gs.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.Unmarshal(mustReadAll(t, resp), &out)
	return resp, out
}

func mustReadAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAPIHotReloadAddInstanceRoutesNextRequest(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	// GATEWAY_TEST_KEY is the template default key, so a new instance with no
	// api_key_env routes with it.
	env := map[string]string{
		"TEST_KEY_1":       "sk-account-1",
		"TEST_KEY_2":       "sk-account-2",
		"GATEWAY_TEST_KEY": "sk-default",
	}
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, env)

	// POST /api/instances with no alias → server-side auto-naming assigns
	// openai-3 (openai and openai-2 are taken).
	resp, out := apiDo(t, gs, "POST", "/api/instances", `{"template":"openai"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/instances status = %d, body %v", resp.StatusCode, out)
	}
	if out["alias"] != "openai-3" {
		t.Fatalf("auto-named alias = %v, want openai-3", out["alias"])
	}

	// No restart: the very next request routes to the new instance and the
	// provider sees its key (the template default, not account 1 or 2).
	before := len(provider.requests())
	drainClose(t, postChat(t, gs, `{"model":"openai-3/gpt-4o","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-hot"}))
	seen := provider.requests()
	if len(seen) != before+1 {
		t.Fatalf("provider saw %d requests, want %d", len(seen), before+1)
	}
	if got := seen[len(seen)-1].Authorization; got != "Bearer sk-default" {
		t.Errorf("new instance routed with %q, want Bearer sk-default", got)
	}
}

func TestAPIDisableInstanceExcludedFromRouting(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// Disable openai-2 via the §6.2 PATCH.
	resp, out := apiDo(t, gs, "PATCH", "/api/instances/openai-2", `{"disabled":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH disable status = %d, body %v", resp.StatusCode, out)
	}
	if out["disabled"] != true {
		t.Fatalf("disabled = %v, want true", out["disabled"])
	}

	// Prefixed routing to the disabled alias → 400 INVALID_ARGUMENT listing
	// only the enabled aliases (the next request observes the new config).
	r := postChat(t, gs, `{"model":"openai-2/gpt-4o"}`, nil)
	body := drainClose(t, r)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", r.StatusCode)
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	json.Unmarshal(body, &e)
	if e.Code != "INVALID_ARGUMENT" {
		t.Errorf("code = %q, want INVALID_ARGUMENT", e.Code)
	}
	// The error names the attempted alias but lists only enabled aliases.
	if !strings.Contains(e.Message, `available aliases: openai`) {
		t.Errorf("message %q should list only enabled aliases", e.Message)
	}
	if strings.Contains(e.Message, "openai-2") && strings.Contains(e.Message, "available aliases: openai, openai-2") {
		t.Errorf("message %q listed the disabled alias as available", e.Message)
	}

	// Unprefixed routing still works (default_alias openai is enabled).
	drainClose(t, postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": "sess-dis"}))
	if len(provider.requests()) != 1 {
		t.Errorf("provider requests = %d, want 1 (only the unprefixed route)", len(provider.requests()))
	}

	// The disabled instance disappears from /v1/models.
	mresp, err := http.Get(gs.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	mbody := drainClose(t, mresp)
	if strings.Contains(string(mbody), "openai-2/") {
		t.Errorf("/v1/models still lists the disabled instance: %s", mbody)
	}
	if !strings.Contains(string(mbody), "openai/gpt-4o") {
		t.Errorf("/v1/models missing the enabled instance: %s", mbody)
	}
}

func TestUIAndAPIUnderGateway(t *testing.T) {
	provider := newFakeProvider(t, nil)
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// GET / serves the embedded single-page UI.
	uresp, err := http.Get(gs.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	ubody := drainClose(t, uresp)
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", uresp.StatusCode)
	}
	if !strings.Contains(string(ubody), "Shimmer Gateway") || !strings.Contains(string(ubody), "Traces") {
		t.Errorf("GET / does not serve the UI page: %q", snippet(string(ubody), 200))
	}

	// The REST surface is reachable on the same gateway port.
	rresp, out := apiDo(t, gs, "GET", "/api/status", "")
	if rresp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/status status = %d, body %v", rresp.StatusCode, out)
	}
	// healthz still sits alongside.
	hresp, err := http.Get(gs.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	drainClose(t, hresp)
	if hresp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", hresp.StatusCode)
	}
}

// TestUIServedFromDiskWhenPresent verifies a web/index.html in the working
// directory shadows the embedded UI: UI edits are visible after a browser
// refresh with no rebuild and no gateway restart.
func TestUIServedFromDiskWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "index.html"), []byte("<html><body>disk UI sentinel</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	provider := newFakeProvider(t, nil)
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	uresp, err := http.Get(gs.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	ubody := drainClose(t, uresp)
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", uresp.StatusCode)
	}
	if !strings.Contains(string(ubody), "disk UI sentinel") {
		t.Errorf("GET / served the embedded UI instead of the disk file: %q", snippet(string(ubody), 200))
	}
}

func TestSessionIDNormalizationEchoedAndPersisted(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// A valid id (common agent shape) passes through and is echoed verbatim.
	resp := postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": "sess-valid:1"})
	drainClose(t, resp)
	if got := resp.Header.Get("X-Gateway-Session-Id"); got != "sess-valid:1" {
		t.Errorf("echoed valid session id = %q, want sess-valid:1", got)
	}

	// An unsafe id is replaced with a UUID before it is echoed or persisted.
	malicious := "x'; alert(1); //"
	resp = postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": malicious})
	drainClose(t, resp)
	echoed := resp.Header.Get("X-Gateway-Session-Id")
	if echoed == "" || echoed == malicious {
		t.Fatalf("echoed malicious session id = %q, want a replacement UUID", echoed)
	}
	if !strings.Contains(echoed, "-") {
		t.Errorf("replacement %q is not UUID-shaped", echoed)
	}
	req := waitForRequest(t, st, echoed, 5*time.Second)
	if req.SessionID != echoed {
		t.Errorf("captured session id = %q, want echoed %q", req.SessionID, echoed)
	}
	if _, err := st.GetSession(context.Background(), malicious); err == nil {
		t.Error("malicious session id was persisted")
	}
}

func TestKeyPrecedenceEnvWinsOverSecretsFile(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	// TEST_KEY_1 (sk-account-1) is set in env; a different key is stored in
	// the secrets file via the API. Env must win.
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)
	apiDo(t, gs, "PATCH", "/api/instances/openai", `{"key":"sk-stored-1"}`)

	drainClose(t, postChat(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"a"}]}`, map[string]string{"X-Session-Id": "sess-prec-env"}))
	seen := provider.requests()
	if len(seen) == 0 {
		t.Fatal("provider saw no requests")
	}
	if got := seen[len(seen)-1].Authorization; got != "Bearer sk-account-1" {
		t.Errorf("env-wins Authorization = %q, want Bearer sk-account-1 (env over secrets file)", got)
	}
}

func TestKeyPrecedenceFallsBackToSecretsFile(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	// The instance names an env var that is NOT set; a stored key in the
	// secrets file must be used.
	instances := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "NEVER_SET_ENV"
`
	gs, _ := newGatewayTest(t, provider, instances, nil)
	apiDo(t, gs, "PATCH", "/api/instances/openai", `{"key":"sk-stored-2"}`)

	drainClose(t, postChat(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"b"}]}`, map[string]string{"X-Session-Id": "sess-prec-file"}))
	seen := provider.requests()
	if len(seen) == 0 {
		t.Fatal("provider saw no requests")
	}
	if got := seen[len(seen)-1].Authorization; got != "Bearer sk-stored-2" {
		t.Errorf("file-fallback Authorization = %q, want Bearer sk-stored-2 (secrets file used)", got)
	}
}

// ---- model aliases (§4.2 model_aliases) ----

func TestModelsListIncludesAliases(t *testing.T) {
	provider := newFakeProvider(t, nil)
	instances := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
model_aliases = { small = "gpt-4o-mini" }
`
	gs, _ := newGatewayTest(t, provider, instances, defaultEnv)

	resp, err := http.Get(gs.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var models struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &models); err != nil {
		t.Fatalf("models body: %v", err)
	}
	got := map[string]string{}
	for _, m := range models.Data {
		got[m.ID] = m.OwnedBy
	}
	for _, want := range []string{"small", "openai/small"} {
		if _, ok := got[want]; !ok {
			t.Errorf("models list missing alias id %q: %v", want, got)
		}
	}
	if got["small"] != "openai" {
		t.Errorf("unprefixed alias id owned_by = %q, want openai", got["small"])
	}
	if got["openai/small"] != "openai" {
		t.Errorf("prefixed alias id owned_by = %q, want openai", got["openai/small"])
	}
}

func TestChatCompletionAliasExpandsToMappedModel(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	instances := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
model_aliases = { small = "gpt-4o-mini" }
`
	gs, st := newGatewayTest(t, provider, instances, defaultEnv)

	drainClose(t, postChat(t, gs, `{"model":"openai/small","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-alias"}))

	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(seen[0].Body, &sent); err != nil {
		t.Fatalf("provider body: %v", err)
	}
	if sent.Model != "gpt-4o-mini" {
		t.Errorf("upstream model = %q, want gpt-4o-mini (expanded from openai/small)", sent.Model)
	}

	req := waitForRequest(t, st, "sess-alias", 5*time.Second)
	if req.Alias != "openai" || req.Model != "gpt-4o-mini" {
		t.Errorf("captured routing = %s/%s, want openai/gpt-4o-mini", req.Alias, req.Model)
	}
}

func TestModelsListAliasCollisionDeduped(t *testing.T) {
	// "small" is a literal model of the first instance (openai) and an alias
	// key of the second (openai-2, a different template). The unprefixed id is
	// listed once, owned by the earlier literal instance — the listing owner
	// may differ from the routed owner (documented shadowing).
	provider := newFakeProvider(t, nil)
	instances := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
models = ["small"]

[[instances]]
alias = "openai-2"
template = "ollama"
api_key_env = "TEST_KEY_2"
model_aliases = { small = "gpt-4o" }
`
	gs, _ := newGatewayTest(t, provider, instances, defaultEnv)

	resp, err := http.Get(gs.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var models struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &models); err != nil {
		t.Fatalf("models body: %v", err)
	}
	var unprefixed []string
	hasAliased := false
	for _, m := range models.Data {
		if m.ID == "small" {
			unprefixed = append(unprefixed, m.OwnedBy)
		}
		if m.ID == "openai-2/small" {
			hasAliased = true
		}
	}
	if len(unprefixed) != 1 {
		t.Errorf("unprefixed 'small' listed %d times (owners %v), want 1", len(unprefixed), unprefixed)
	}
	if len(unprefixed) == 1 && unprefixed[0] != "openai" {
		t.Errorf("unprefixed 'small' owned_by = %q, want openai (earlier literal instance)", unprefixed[0])
	}
	if !hasAliased {
		t.Error("models list missing the prefixed alias id openai-2/small")
	}
}

func TestChatCompletionsRewritesReasoningEffort(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
	})
	instances := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
model_aliases = { small = "deepseek-reasoner" }
model_reasoning = { small = "high" }
`
	gs, _ := newGatewayTest(t, provider, instances, defaultEnv)

	// Post chat with unprefixed model alias "small"
	resp := postChat(t, gs, `{"model":"small","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-reasoning"})
	drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(seen))
	}
	var sent struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(seen[0].Body, &sent); err != nil {
		t.Fatalf("provider body: %v", err)
	}
	if sent.Model != "deepseek-reasoner" {
		t.Errorf("upstream model = %q, want deepseek-reasoner", sent.Model)
	}
	if sent.ReasoningEffort != "high" {
		t.Errorf("upstream reasoning_effort = %q, want high", sent.ReasoningEffort)
	}
}
