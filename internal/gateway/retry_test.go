package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a mutex-guarded strings.Builder for capturing gateway logs.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// retryTOML enables retry_empty on the openai account.
const retryTOML = `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
plugins = ["retry_empty"]
`

// ---- non-stream retry (Phase 2) ----

func TestNonStreamRetryEmptyThenContent(t *testing.T) {
	var provider *fakeProvider
	attempt := 0
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		attempt++
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.Write([]byte(`{"id":"c-empty","choices":[]}`))
			return
		}
		w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"total_tokens":7}}`))
	})

	var logBuf lockedBuffer
	srv, st := newGatewayServer(t, provider, retryTOML, defaultEnv, &logBuf)
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	resp := postChat(t, gs, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-retry-ns"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("client did not receive the retried content: %q", body)
	}

	seen := provider.requests()
	if len(seen) != 2 {
		t.Errorf("provider saw %d requests, want 2 (empty then content)", len(seen))
	}

	req := waitForRequest(t, st, "sess-retry-ns", 5*time.Second)
	if req.FinishReason != "stop" || req.StatusCode != http.StatusOK {
		t.Errorf("captured = status %d finish %q, want 200/stop (final attempt)", req.StatusCode, req.FinishReason)
	}
	sess, err := st.GetSession(context.Background(), "sess-retry-ns")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestCount != 1 || len(sess.Requests) != 1 {
		t.Errorf("session request_count/requests = %d/%d, want 1/1 (one capture per client request)", sess.RequestCount, len(sess.Requests))
	}

	// Each retry is logged with the request id shared with the capture.
	lines := logBuf.String()
	if !strings.Contains(lines, `"event":"retry_empty"`) {
		t.Errorf("expected a retry_empty warn log line:\n%s", lines)
	}
	if !strings.Contains(lines, `"request_id":"`+req.ID+`"`) {
		t.Errorf("retry log request_id should match the captured id %q:\n%s", req.ID, lines)
	}
}

func TestNonStreamRetryNonEmptySingleAttempt(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	gs, _ := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o"}`, nil)
	drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (non-empty result is not retried)", len(provider.requests()))
	}
}

func TestNonStreamNoRetryWhenOff(t *testing.T) {
	// retry_empty off: an empty result passes through and is captured with a
	// single provider request, exactly as before the feature.
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[]}`))
	})
	cfg := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`
	gs, st := newGatewayTest(t, provider, cfg, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": "sess-no-retry-ns"})
	drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (no retry when plugin is off)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-no-retry-ns", 5*time.Second)
	if len(req.ResponseJSON) == 0 {
		t.Error("empty result was not captured")
	}
}

func TestNonStreamRetryCapsAtThree(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[]}`))
	})
	gs, st := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": "sess-cap"})
	drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(provider.requests()) != 3 {
		t.Errorf("provider saw %d requests, want 3 (bounded to maxRetryAttempts)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-cap", 5*time.Second)
	if req.StatusCode != http.StatusOK || req.FinishReason != "" {
		t.Errorf("final capture = status %d finish %q, want the last empty attempt (200, empty)", req.StatusCode, req.FinishReason)
	}
}

// ---- stream retry (held mode, Phase 3) ----

// dataLines extracts the "data: ..." SSE payload lines from a raw body.
func dataLines(t *testing.T, raw string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func TestStreamRetryEmptyThenContent(t *testing.T) {
	var provider *fakeProvider
	attempt := 0
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		attempt++
		fl := w.(http.Flusher)
		if attempt == 1 {
			// Reasoning-only attempt: the assembler ignores reasoning_content,
			// so the reassembled completion is empty; a clean [DONE] follows.
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking...\"},\"finish_reason\":null}]}\n\n")
			fl.Flush()
			fmt.Fprintf(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		fmt.Fprintf(w, "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	gs, st := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-retry-stream"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw := string(drainClose(t, resp))

	// One completion block plus [DONE]; the reasoning-only attempt must never
	// reach the client.
	lines := dataLines(t, raw)
	if len(lines) != 2 || lines[1] != "data: [DONE]" {
		t.Fatalf("unexpected held SSE output: %q", raw)
	}
	var comp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], "data: ")), &comp); err != nil {
		t.Fatalf("emitted block is not a chat.completion: %v (%q)", err, lines[0])
	}
	if len(comp.Choices) != 1 || comp.Choices[0].Message.Content != "Hello" {
		t.Errorf("emitted completion = %+v, want content Hello", comp.Choices)
	}

	if len(provider.requests()) != 2 {
		t.Errorf("provider saw %d requests, want 2 (empty reasoning then content)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-retry-stream", 5*time.Second)
	if req.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", req.FinishReason)
	}
	if req.Truncated {
		t.Error("truncated = true, want false")
	}
	sess, err := st.GetSession(context.Background(), "sess-retry-stream")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestCount != 1 || len(sess.Requests) != 1 {
		t.Errorf("session request_count/requests = %d/%d, want 1/1 (one capture per client request)", sess.RequestCount, len(sess.Requests))
	}
}

func TestStreamRetryUncleanDropThenContent(t *testing.T) {
	var provider *fakeProvider
	attempt := 0
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking\"},\"finish_reason\":null}]}\n\n")
			fl.Flush()
			// Unclean drop: hijack the connection and RST it before any [DONE].
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				tcp.SetLinger(0) //nolint:errcheck // force RST on close
			}
			conn.Close()
			return
		}
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"recovered\"},\"finish_reason\":null}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	gs, st := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-retry-drop"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw := string(drainClose(t, resp))
	lines := dataLines(t, raw)
	if len(lines) != 2 || lines[1] != "data: [DONE]" {
		t.Fatalf("unexpected held SSE output after unclean drop: %q", raw)
	}
	if !strings.Contains(raw, "recovered") {
		t.Errorf("client did not receive the recovered completion: %q", raw)
	}

	if len(provider.requests()) != 2 {
		t.Errorf("provider saw %d requests, want 2 (unclean drop then content)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-retry-drop", 5*time.Second)
	if req.FinishReason != "stop" || req.Truncated {
		t.Errorf("final capture = finish %q truncated %v, want stop/false", req.FinishReason, req.Truncated)
	}
}

func TestStreamRetryNonEmptySingleAttempt(t *testing.T) {
	// retry_empty active, provider returns a normal stream: the held path
	// still emits the full completion once, with no re-issue.
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	gs, _ := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw := string(drainClose(t, resp))
	lines := dataLines(t, raw)
	if len(lines) != 2 || lines[1] != "data: [DONE]" {
		t.Fatalf("unexpected held SSE output: %q", raw)
	}
	if !strings.Contains(raw, "ok") {
		t.Errorf("client did not receive the completion: %q", raw)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (non-empty stream is not retried)", len(provider.requests()))
	}
}

func TestStreamRetryFirstAttempt401(t *testing.T) {
	// The first upstream attempt completes before the SSE headers are
	// committed, so a 401 reaches the client as a real 401, not a 200.
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key","type":"authentication_error","code":"invalid_api_key"}}`))
	})
	gs, st := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-retry-401"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (headers must not be committed as 200)", resp.StatusCode)
	}
	if !strings.Contains(string(body), "invalid_api_key") {
		t.Errorf("client did not receive the provider error body: %q", body)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (401 is never retried)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-retry-401", 5*time.Second)
	if req.StatusCode != http.StatusUnauthorized || req.Error == nil {
		t.Errorf("captured = status %d error %v, want 401 with an error record", req.StatusCode, req.Error)
	}
}

func TestStreamRetryFirstAttemptTransportError502(t *testing.T) {
	// The upstream connection dies before any response: client.Do fails at the
	// transport level, so the client must receive a 502, not a committed 200.
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	})
	gs, st := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-retry-502"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (headers must not be committed as 200)", resp.StatusCode)
	}
	if !strings.Contains(string(body), "UPSTREAM_ERROR") {
		t.Errorf("client did not receive the upstream error body: %q", body)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-retry-502", 5*time.Second)
	if req.StatusCode != http.StatusBadGateway || req.Error == nil {
		t.Errorf("captured = status %d error %v, want 502 with an error record", req.StatusCode, req.Error)
	}
}

func TestStreamRetryFirstAttempt3xxNotRetried(t *testing.T) {
	// A 3xx response (surfaced by the no-redirect policy) is not a 2xx, so it
	// must pass through with its real status and never trigger the
	// empty-result retry loop.
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.com/redirected")
		w.WriteHeader(http.StatusFound)
	})
	gs, st := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-retry-3xx"})
	drainClose(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 (3xx is passed through, never retried)", resp.StatusCode)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (3xx must not trigger an empty-result retry)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-retry-3xx", 5*time.Second)
	if req.StatusCode != http.StatusFound {
		t.Errorf("captured status = %d, want 302", req.StatusCode)
	}
}

func TestStreamRetrySecondAttemptNon2xxEmitsErrorEvent(t *testing.T) {
	// After a premature-empty 2xx attempt, a 401 on the retry attempt (headers
	// already committed) must end the stream cleanly with an SSE error event
	// plus [DONE] and still capture exactly one error record.
	var provider *fakeProvider
	attempt := 0
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking\"},\"finish_reason\":null}]}\n\n")
			fl.Flush()
			fmt.Fprintf(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"expired","type":"authentication_error","code":"expired_key"}}`))
	})
	gs, st := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-retry-2nd-401"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers committed after the first 2xx attempt)", resp.StatusCode)
	}
	raw := string(drainClose(t, resp))
	lines := dataLines(t, raw)
	if len(lines) != 2 || lines[1] != "data: [DONE]" {
		t.Fatalf("unexpected SSE error output: %q", raw)
	}
	if !strings.Contains(lines[0], "error") || !strings.Contains(lines[0], "401") {
		t.Errorf("expected an SSE error event naming the status: %q", lines[0])
	}
	if len(provider.requests()) != 2 {
		t.Errorf("provider saw %d requests, want 2 (empty then 401)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-retry-2nd-401", 5*time.Second)
	if req.StatusCode != http.StatusUnauthorized || req.Error == nil {
		t.Errorf("captured = status %d error %v, want 401 with an error record", req.StatusCode, req.Error)
	}
}

func TestStreamRetryDisconnectDuringHeldRetryCapturesOnce(t *testing.T) {
	// The client disconnects while the held first attempt is still in flight.
	// The gateway must abort the upstream read, capture the truncated record,
	// and never drop or duplicate the capture row.
	var provider *fakeProvider
	proceed := make(chan struct{})
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking\"},\"finish_reason\":null}]}\n\n")
		fl.Flush()
		// Hold the attempt open so the client can disconnect mid-stream.
		<-proceed
	})
	t.Cleanup(func() { close(proceed) })
	gs, st := newGatewayTest(t, provider, retryTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, map[string]string{"X-Session-Id": "sess-retry-disc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (held stream commits once the first attempt is 2xx)", resp.StatusCode)
	}
	// Disconnect while the held attempt is still in flight.
	resp.Body.Close()

	req := waitForRequest(t, st, "sess-retry-disc", 5*time.Second)
	if !req.Truncated {
		t.Error("captured record should be truncated (client disconnected mid-stream)")
	}
	sess, err := st.GetSession(context.Background(), "sess-retry-disc")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestCount != 1 || len(sess.Requests) != 1 {
		t.Errorf("session request_count/requests = %d/%d, want 1/1 (one capture row despite the disconnect)", sess.RequestCount, len(sess.Requests))
	}
}

// TestNonStreamRetryExplicitInstancePlugins verifies retry_empty activates
// end-to-end via an explicit instance-level plugins list — the only runtime
// activation path (no template-default fallback).
func TestNonStreamRetryExplicitInstancePlugins(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		attempt := len(provider.requests()) // current request already recorded
		if attempt == 1 {
			w.Write([]byte(`{"id":"empty","choices":[]}`))
			return
		}
		w.Write([]byte(`{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"via explicit instance plugins"},"finish_reason":"stop"}]}`))
	})
	customTemplate := fmt.Sprintf(`
[providers.custom]
base_url = %q
api_key_env = "TEST_KEY_1"
models = ["m1"]

[[instances]]
alias = "custom"
template = "custom"
plugins = ["retry_empty"]
`, provider.url()+"/v1")
	gs, st := newGatewayTest(t, provider, customTemplate, defaultEnv)

	resp := postChat(t, gs, `{"model":"custom/m1","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-explicit-plugins"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "via explicit instance plugins") {
		t.Errorf("client did not receive the retried content: %q", body)
	}
	if len(provider.requests()) != 2 {
		t.Errorf("provider saw %d requests, want 2 (explicit instance plugins enabled retry)", len(provider.requests()))
	}
	sess, err := st.GetSession(context.Background(), "sess-explicit-plugins")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestCount != 1 {
		t.Errorf("session request_count = %d, want 1", sess.RequestCount)
	}
}

// TestNonStreamOpenCodeGoAbsentPluginsNoRetry pins the explicit-in-file model:
// an opencode_go instance with no plugins line is OFF at runtime even though
// its template carries the ["retry_empty"] seed — the seed is write-back
// materialization only, never a hidden runtime default.
func TestNonStreamOpenCodeGoAbsentPluginsNoRetry(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[]}`))
	})
	// Override the built-in opencode_go to route at the fake provider while
	// keeping the built-in's seed shape (template plugins = ["retry_empty"]).
	cfg := fmt.Sprintf(`
[providers.opencode_go]
base_url = %q
api_key_env = "TEST_KEY_1"
models = ["kimi-k2"]
plugins = ["retry_empty"]

[[instances]]
alias = "go"
template = "opencode_go"
`, provider.url()+"/v1")
	gs, st := newGatewayTest(t, provider, cfg, defaultEnv)

	resp := postChat(t, gs, `{"model":"go/kimi-k2","messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-go-absent"})
	drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (absent instance plugins = off, no retry)", len(provider.requests()))
	}
	req := waitForRequest(t, st, "sess-go-absent", 5*time.Second)
	if len(req.ResponseJSON) == 0 {
		t.Error("empty result was not captured")
	}
}

func TestStreamNoRetryWhenOffKeepsLiveForward(t *testing.T) {
	// retry_empty off: a reasoning-only stream is forwarded live, byte-for-byte,
	// and never re-issued — the non-regression guard for the held-mode branch.
	var provider *fakeProvider
	proceed := make(chan struct{})
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		write := func(s string) {
			w.Write([]byte(s))
			fl.Flush()
			provider.addWrite(s)
		}
		// Reasoning-only delta is forwarded verbatim (nothing is held or
		// reassembled away on this path).
		write(": keepalive\n\n")
		write("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking...\"},\"finish_reason\":null}]}\n\n")
		<-proceed
		write("data: [DONE]\n\n")
	})
	cfg := `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`
	gs, _ := newGatewayTest(t, provider, cfg, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Live proof: the first reasoning delta arrives while the provider is
	// still blocked on proceed — line-by-line forward, never buffered.
	br := bufio.NewReader(resp.Body)
	before := readUntilContent(t, br, 3*time.Second, "thinking...")
	if !strings.Contains(before, "keepalive") {
		t.Errorf("expected the keepalive before the first delta, got %q", before)
	}
	close(proceed)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read rest of stream: %v", err)
	}
	got := before + string(rest)
	if want := provider.totalWrites(); got != want {
		t.Errorf("client bytes != provider bytes\nclient: %q\nprovider: %q", got, want)
	}
	if len(provider.requests()) != 1 {
		t.Errorf("provider saw %d requests, want 1 (no retry when plugin is off)", len(provider.requests()))
	}
}
