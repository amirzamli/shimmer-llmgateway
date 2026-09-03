package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// redactTOML configures the redact plugin with a custom pattern so tests are
// not coupled to the default sensitive-value patterns.
const redactTOML = `
[settings]
request_plugins = ["redact"]
response_plugins = ["redact"]

[plugins.redact]
patterns = ["SECRET-[0-9]+"]

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`

func assertBodyContains(t *testing.T, label, body string, secrets []string, redacted bool) {
	t.Helper()
	for _, s := range secrets {
		contains := strings.Contains(body, s)
		if redacted && contains {
			t.Errorf("%s contains %q (should be redacted): %s", label, s, body)
		}
		if !redacted && !contains {
			t.Errorf("%s missing %q: %s", label, s, body)
		}
	}
}

func TestNonStreamRedactOriginalAndFilteredStored(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"confirmation SECRET-9999"},"finish_reason":"stop"}]}`))
	})
	gs, st := newGatewayTest(t, provider, redactTOML, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","messages":[{"role":"user","content":"my pin is SECRET-1234"}]}`, map[string]string{"X-Session-Id": "sess-redact-ns"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The client receives the FILTERED response.
	assertBodyContains(t, "client body", string(body), []string{"SECRET-9999"}, true)

	// The provider saw the FILTERED request.
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	assertBodyContains(t, "provider request", string(seen[0].Body), []string{"SECRET-1234"}, true)

	req := waitForRequest(t, st, "sess-redact-ns", 5*time.Second)
	if len(req.PluginsApplied) != 1 || req.PluginsApplied[0] != "redact" {
		t.Errorf("plugins_applied = %v, want [redact]", req.PluginsApplied)
	}
	// Original payload untouched in the store: both sides are kept verbatim.
	assertBodyContains(t, "request_json", string(req.RequestJSON), []string{"SECRET-1234"}, false)
	assertBodyContains(t, "response_json", string(req.ResponseJSON), []string{"SECRET-9999"}, false)
	// Filtered payloads: what the provider saw / what the client received.
	assertBodyContains(t, "request_filtered_json", string(req.RequestFilteredJSON), []string{"SECRET-1234"}, true)
	assertBodyContains(t, "response_filtered_json", string(req.ResponseFilteredJSON), []string{"SECRET-9999"}, true)
}

func TestStreamResponsePluginsBufferMode(t *testing.T) {
	// Response plugins configured → buffer mode: the client receives the
	// filtered reassembled completion as one SSE block, not the raw chunks.
	cfg := `
[settings]
request_plugins = []
response_plugins = ["redact"]

[plugins.redact]
patterns = ["SECRET-[0-9]+"]

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeEvent := func(j string) {
			fmt.Fprintf(w, "data: %s\n\n", j)
			fl.Flush()
		}
		writeEvent(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hello SECRET-42"},"finish_reason":null}]}`)
		writeEvent(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeEvent(`data: [DONE]`)
	})
	gs, st := newGatewayTest(t, provider, cfg, defaultEnv)

	resp := postChat(t, gs, `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-buffer"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw := string(drainClose(t, resp))

	// One data line carrying the filtered completion, then [DONE].
	var dataLines []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dataLines = append(dataLines, line)
	}
	if len(dataLines) != 2 || dataLines[1] != "data: [DONE]" {
		t.Fatalf("unexpected buffered SSE block: %q", raw)
	}
	var comp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLines[0], "data: ")), &comp); err != nil {
		t.Fatalf("filtered block is not a chat.completion: %v (%q)", err, dataLines[0])
	}
	if comp.Object != "chat.completion" || len(comp.Choices) != 1 {
		t.Fatalf("filtered block shape wrong: %+v", comp)
	}
	assertBodyContains(t, "client stream", comp.Choices[0].Message.Content, []string{"SECRET-42"}, true)

	req := waitForRequest(t, st, "sess-buffer", 5*time.Second)
	if len(req.PluginsApplied) != 1 || req.PluginsApplied[0] != "redact" {
		t.Errorf("plugins_applied = %v, want [redact]", req.PluginsApplied)
	}
	// Provider-out reassembled (original) vs client-received filtered.
	assertBodyContains(t, "response_json", string(req.ResponseJSON), []string{"SECRET-42"}, false)
	assertBodyContains(t, "response_filtered_json", string(req.ResponseFilteredJSON), []string{"SECRET-42"}, true)
	// No request plugins → request_filtered_json stays NULL.
	if len(req.RequestFilteredJSON) != 0 {
		t.Errorf("request_filtered_json = %q, want empty (no request plugins)", req.RequestFilteredJSON)
	}
	if req.Truncated {
		t.Error("truncated = true, want false")
	}
}

func TestPerInstanceEmptyPluginsOverridesGlobals(t *testing.T) {
	// Global defaults redact both sides; the second account overrides with an
	// empty list → no plugins for that account (original passes through).
	cfg := `
[settings]
request_plugins = ["redact"]
response_plugins = ["redact"]

[plugins.redact]
patterns = ["SECRET-[0-9]+"]

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"

[[instances]]
alias = "openai-2"
template = "openai"
api_key_env = "TEST_KEY_2"
plugins = []
`
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"reply SECRET-9999"},"finish_reason":"stop"}]}`))
	})
	gs, st := newGatewayTest(t, provider, cfg, defaultEnv)

	// Account 2 (empty override): nothing filtered.
	drainClose(t, postChat(t, gs, `{"model":"openai-2/gpt-4o","messages":[{"role":"user","content":"token SECRET-1234"}]}`, map[string]string{"X-Session-Id": "sess-no-plugins"}))
	// Account 1 (defaults): both sides filtered.
	drainClose(t, postChat(t, gs, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"token SECRET-5678"}]}`, map[string]string{"X-Session-Id": "sess-plugins"}))

	// Provider saw original for account 2 and filtered for account 1.
	seen := provider.requests()
	if len(seen) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(seen))
	}
	assertBodyContains(t, "account 2 provider request", string(seen[0].Body), []string{"SECRET-1234"}, false)
	assertBodyContains(t, "account 1 provider request", string(seen[1].Body), []string{"SECRET-5678"}, true)

	reqNone := waitForRequest(t, st, "sess-no-plugins", 5*time.Second)
	if reqNone.PluginsApplied != nil {
		t.Errorf("plugins_applied = %v, want nil (NULL) for the overridden account", reqNone.PluginsApplied)
	}
	if len(reqNone.RequestFilteredJSON) != 0 || len(reqNone.ResponseFilteredJSON) != 0 {
		t.Errorf("filtered payloads present for overridden account: %q / %q", reqNone.RequestFilteredJSON, reqNone.ResponseFilteredJSON)
	}
	assertBodyContains(t, "account 2 request_json", string(reqNone.RequestJSON), []string{"SECRET-1234"}, false)
	assertBodyContains(t, "account 2 response_json", string(reqNone.ResponseJSON), []string{"SECRET-9999"}, false)

	reqOn := waitForRequest(t, st, "sess-plugins", 5*time.Second)
	if len(reqOn.PluginsApplied) != 1 || reqOn.PluginsApplied[0] != "redact" {
		t.Errorf("plugins_applied = %v, want [redact]", reqOn.PluginsApplied)
	}
	assertBodyContains(t, "account 1 request_filtered_json", string(reqOn.RequestFilteredJSON), []string{"SECRET-5678"}, true)
}

func TestNonStreamNoPluginsAllFieldsNil(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	drainClose(t, postChat(t, gs, `{"model":"gpt-4o"}`, map[string]string{"X-Session-Id": "sess-plain"}))
	req := waitForRequest(t, st, "sess-plain", 5*time.Second)
	if req.PluginsApplied != nil {
		t.Errorf("plugins_applied = %v, want nil (NULL when no plugins ran)", req.PluginsApplied)
	}
	if len(req.RequestFilteredJSON) != 0 {
		t.Errorf("request_filtered_json = %q, want empty", req.RequestFilteredJSON)
	}
	if len(req.ResponseFilteredJSON) != 0 {
		t.Errorf("response_filtered_json = %q, want empty", req.ResponseFilteredJSON)
	}
	// Reassembled original response is still captured.
	if len(req.ResponseJSON) == 0 {
		t.Error("response_json empty")
	}
}

// sanitizeTOML enables the sanitize_tools request plugin on the instance.
const sanitizeTOML = `
[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
plugins = ["sanitize_tools"]
`

func TestNonStreamSanitizeToolsUpstreamReceivesCleanSchema(t *testing.T) {
	// The enum+oneOf duplication that some providers answer with a silent
	// empty completion must be stripped before the body leaves the gateway.
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	gs, st := newGatewayTest(t, provider, sanitizeTOML, defaultEnv)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object","properties":{"action":{"type":"string","enum":["a","b"],"oneOf":[{"const":"a"},{"const":"b"}]}}}}},{"type":"function","function":{"name":"plain","parameters":{"type":"object","properties":{"p":{"oneOf":[{"type":"string"}]}}}}}]}`
	resp := postChat(t, gs, body, map[string]string{"X-Session-Id": "sess-sanitize-ns"})
	drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	var upstream struct {
		Tools []struct {
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]map[string]any `json:"properties"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(seen[0].Body, &upstream); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, seen[0].Body)
	}
	if len(upstream.Tools) != 2 {
		t.Fatalf("upstream tools = %d, want 2", len(upstream.Tools))
	}
	if _, has := upstream.Tools[0].Function.Parameters.Properties["action"]["oneOf"]; has {
		t.Error("upstream body still carries oneOf alongside enum")
	}
	if enum, ok := upstream.Tools[0].Function.Parameters.Properties["action"]["enum"].([]any); !ok || len(enum) != 2 {
		t.Errorf("upstream enum = %v, want [a b]", upstream.Tools[0].Function.Parameters.Properties["action"]["enum"])
	}
	if _, has := upstream.Tools[1].Function.Parameters.Properties["p"]["oneOf"]; !has {
		t.Error("untouched tool's oneOf (no enum) was stripped")
	}

	// The store keeps both sides: original verbatim, filtered sanitized.
	req := waitForRequest(t, st, "sess-sanitize-ns", 5*time.Second)
	if len(req.PluginsApplied) != 1 || req.PluginsApplied[0] != "sanitize_tools" {
		t.Errorf("plugins_applied = %v, want [sanitize_tools]", req.PluginsApplied)
	}
	assertBodyContains(t, "request_json", string(req.RequestJSON), []string{"oneOf"}, false)

	var filtered struct {
		Tools []struct {
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]map[string]any `json:"properties"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(req.RequestFilteredJSON, &filtered); err != nil {
		t.Fatalf("request_filtered_json is not JSON: %v (%s)", err, req.RequestFilteredJSON)
	}
	if len(filtered.Tools) != 2 {
		t.Fatalf("filtered tools = %d, want 2", len(filtered.Tools))
	}
	if _, has := filtered.Tools[0].Function.Parameters.Properties["action"]["oneOf"]; has {
		t.Error("request_filtered_json still carries oneOf alongside enum")
	}
	if enum, ok := filtered.Tools[0].Function.Parameters.Properties["action"]["enum"].([]any); !ok || len(enum) != 2 {
		t.Errorf("filtered enum = %v, want [a b]", filtered.Tools[0].Function.Parameters.Properties["action"]["enum"])
	}
	if _, has := filtered.Tools[1].Function.Parameters.Properties["p"]["oneOf"]; !has {
		t.Error("untouched tool's oneOf was stripped in request_filtered_json")
	}
}

func TestStreamSanitizeToolsStaysInLiveDeltaMode(t *testing.T) {
	// sanitize_tools is request-only, so listing it on an instance must NOT
	// switch streaming into buffer mode: buffer mode re-emits the completion
	// as one message-shaped block, which OpenAI-stream clients (that parse
	// delta.tool_calls) drop on the floor for tool-calling turns. The client
	// must receive the provider's delta chunks verbatim, live.
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeEvent := func(j string) {
			fmt.Fprintf(w, "data: %s\n\n", j)
			fl.Flush()
		}
		writeEvent(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"t","arguments":""}}]},"finish_reason":null}]}`)
		writeEvent(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":\"b\"}"}}]},"finish_reason":null}]}`)
		writeEvent(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		writeEvent(`[DONE]`)
	})
	gs, st := newGatewayTest(t, provider, sanitizeTOML, defaultEnv)

	body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object","properties":{"action":{"type":"string","enum":["a","b"],"oneOf":[{"const":"a"},{"const":"b"}]}}}}}]}`
	resp := postChat(t, gs, body, map[string]string{"X-Session-Id": "sess-sanitize-stream"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw := string(drainClose(t, resp))

	var dataLines []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dataLines = append(dataLines, line)
	}
	// Live mode: every provider event forwarded as its own data line, deltas
	// intact, then [DONE]. Buffer mode would collapse this into ONE
	// message-shaped block (choices[].message, no "delta").
	if len(dataLines) != 4 || dataLines[3] != "data: [DONE]" {
		t.Fatalf("expected 3 live delta events + [DONE], got %d lines: %q", len(dataLines), raw)
	}
	var chunk struct {
		Object  string `json:"object"`
		Choices []struct {
			Delta json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLines[0], "data: ")), &chunk); err != nil {
		t.Fatalf("first event is not a chunk: %v (%q)", err, dataLines[0])
	}
	if chunk.Object != "chat.completion.chunk" || len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta) == 0 {
		t.Fatalf("first event shape wrong (want delta chunk): %+v", chunk)
	}
	if !strings.Contains(raw, `"finish_reason":"tool_calls"`) {
		t.Errorf("client stream missing delta finish event: %q", raw)
	}

	// The request side still ran (that is the point of the plugin).
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	assertBodyContains(t, "provider request", string(seen[0].Body), []string{"oneOf"}, true)

	req := waitForRequest(t, st, "sess-sanitize-stream", 5*time.Second)
	if len(req.PluginsApplied) != 1 || req.PluginsApplied[0] != "sanitize_tools" {
		t.Errorf("plugins_applied = %v, want [sanitize_tools]", req.PluginsApplied)
	}
	if len(req.RequestFilteredJSON) == 0 {
		t.Error("request_filtered_json empty, want the sanitized body")
	}
	// No response plugin ran → response_filtered_json stays NULL and the live
	// stream was never buffered.
	if len(req.ResponseFilteredJSON) != 0 {
		t.Errorf("response_filtered_json = %q, want empty (request-only plugin)", req.ResponseFilteredJSON)
	}
	if req.Truncated {
		t.Error("truncated = true, want false")
	}
}
