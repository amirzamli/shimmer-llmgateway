package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"shimmer-llmgateway/internal/plugins"
)

// decodeMap unmarshals b into a map, failing the test on invalid JSON.
func decodeMap(t *testing.T, b json.RawMessage) map[string]any {
	t.Helper()
	if !json.Valid(b) {
		t.Fatalf("redacted payload is not valid JSON: %q", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal redacted payload: %v", err)
	}
	return m
}

func TestRedactPayloadRequestMessageContent(t *testing.T) {
	in := json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"my secret prompt"},{"role":"assistant","content":"a secret reply"}]}`)
	out := redactPayload(in)
	m := decodeMap(t, out)
	if m["model"] != "gpt-4o" {
		t.Errorf("model = %v, want gpt-4o", m["model"])
	}
	msgs := m["messages"].([]any)
	for i, mi := range msgs {
		msg := mi.(map[string]any)
		if got := msg["content"]; got != plugins.Redacted {
			t.Errorf("message %d content = %v, want %q", i, got, plugins.Redacted)
		}
		if got := msg["role"]; got == nil {
			t.Errorf("message %d role lost", i)
		}
	}
	if strings.Contains(string(out), "secret") {
		t.Errorf("redacted payload still contains prompt content: %s", out)
	}
}

func TestRedactPayloadToolCallArguments(t *testing.T) {
	in := json.RawMessage(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"query\":\"secret term\"}"}}]}]}`)
	out := redactPayload(in)
	m := decodeMap(t, out)
	tcs := m["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("tool call id = %v, want call_1", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Errorf("tool name = %v, want search", fn["name"])
	}
	if got := fn["arguments"]; got != plugins.Redacted {
		t.Errorf("tool arguments = %v, want %q", got, plugins.Redacted)
	}
	if strings.Contains(string(out), "secret term") {
		t.Errorf("redacted payload still contains tool arguments: %s", out)
	}
}

func TestRedactPayloadResponseChoices(t *testing.T) {
	in := json.RawMessage(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"secret answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	out := redactPayload(in)
	m := decodeMap(t, out)
	if m["id"] != "chatcmpl-1" {
		t.Errorf("id = %v, want chatcmpl-1", m["id"])
	}
	choice := m["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if got := msg["content"]; got != plugins.Redacted {
		t.Errorf("response content = %v, want %q", got, plugins.Redacted)
	}
	usage := m["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
		t.Errorf("usage changed: %v", usage)
	}
	if strings.Contains(string(out), "secret") {
		t.Errorf("redacted payload still contains response content: %s", out)
	}
}

func TestRedactPayloadMultimodalContentArray(t *testing.T) {
	in := json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"a secret prompt"},{"type":"image_url","image_url":{"url":"https://example.com/img.png"}}]}]}`)
	out := redactPayload(in)
	m := decodeMap(t, out)
	msg := m["messages"].([]any)[0].(map[string]any)
	// The whole multimodal content subtree is replaced, so the text part and
	// any nested strings cannot leak.
	if got := msg["content"]; got != plugins.Redacted {
		t.Errorf("multimodal content = %v, want %q", got, plugins.Redacted)
	}
	if strings.Contains(string(out), "secret prompt") || strings.Contains(string(out), "example.com") {
		t.Errorf("redacted payload still contains multimodal content: %s", out)
	}
}

func TestRedactPayloadNestedContent(t *testing.T) {
	// "content" nested deeper than a message (e.g. inside a content part or a
	// schema) is still redacted wherever it appears.
	in := json.RawMessage(`{"objects":[{"name":"outer","nested":{"content":"deep secret","keep":"visible"}}]}`)
	out := redactPayload(in)
	m := decodeMap(t, out)
	nested := m["objects"].([]any)[0].(map[string]any)["nested"].(map[string]any)
	if got := nested["content"]; got != plugins.Redacted {
		t.Errorf("nested content = %v, want %q", got, plugins.Redacted)
	}
	if nested["keep"] != "visible" {
		t.Errorf("nested metadata changed: %v", nested["keep"])
	}
}

func TestRedactPayloadInvalidJSONPassthrough(t *testing.T) {
	in := json.RawMessage(`not json at all`)
	out := redactPayload(in)
	if string(out) != string(in) {
		t.Errorf("non-JSON payload changed: %q -> %q", in, out)
	}
}

func TestRedactPayloadPreservesMetadata(t *testing.T) {
	in := json.RawMessage(`{"messages":[{"role":"system","content":"system instructions"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}],"stream":false}`)
	out := redactPayload(in)
	m := decodeMap(t, out)
	if m["stream"] != false {
		t.Errorf("stream = %v, want false", m["stream"])
	}
	msgs := m["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("first message role lost: %v", msgs[0])
	}
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Errorf("assistant role lost: %v", assistant)
	}
	tcs := assistant["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool name lost: %v", fn["name"])
	}
	if got := fn["arguments"]; got != plugins.Redacted {
		t.Errorf("tool arguments = %v, want %q", got, plugins.Redacted)
	}
	if strings.Contains(string(out), "system instructions") || strings.Contains(string(out), "Paris") {
		t.Errorf("redacted payload leaked content: %s", out)
	}
}
