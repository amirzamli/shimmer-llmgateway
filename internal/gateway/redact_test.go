package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/amirzamli/shimmer-llmgateway/internal/plugins"
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

// TestRedactPayloadMasksSensitiveFieldsAndPatterns asserts the console-log
// redaction shares the plugin redactor's coverage: credential-typed fields are
// masked wholesale and sensitive strings (email, SSN) are regex-redacted even
// under keys the field list does not name.
func TestRedactPayloadMasksSensitiveFieldsAndPatterns(t *testing.T) {
	in := json.RawMessage(`{"model":"gpt-4o","api_key":"sk-super-secret-key","messages":[{"role":"user","content":"hi"}],"metadata":"contact alice@example.com or ssn 123-45-6789"}`)
	out := redactPayload(in)
	m := decodeMap(t, out)
	if got := m["api_key"]; got != plugins.Redacted {
		t.Errorf("api_key = %v, want %q", got, plugins.Redacted)
	}
	if got := m["metadata"]; got != "contact "+plugins.Redacted+" or ssn "+plugins.Redacted {
		t.Errorf("metadata = %v, want email and SSN regex-redacted", got)
	}
	if s := string(out); strings.Contains(s, "sk-super-secret-key") || strings.Contains(s, "alice@example.com") || strings.Contains(s, "123-45-6789") {
		t.Errorf("redacted payload leaked a sensitive value: %s", s)
	}
}

// TestSnippetTruncatesByRune verifies snippet() never splits a UTF-8 rune when
// truncating (the previous byte-based slice could cut a multibyte character).
func TestSnippetTruncatesByRune(t *testing.T) {
	s := "héllo wörld"
	if got := snippet(s, 5); got != "héllo" {
		t.Errorf("snippet(%q, 5) = %q, want %q (cut at a rune boundary)", s, got, "héllo")
	}
	if got := snippet(s, len(s)); got != s {
		t.Errorf("snippet(%q, %d) = %q, want unchanged", s, len(s), got)
	}
	// A cut in the middle of a 2-byte rune yields a valid, shorter string
	// rather than a corrupt UTF-8 suffix.
	if got := snippet(s, 4); !utf8.ValidString(got) {
		t.Errorf("snippet(%q, 4) = %q, not valid UTF-8", s, got)
	}
}

// TestLogPayloadsMasksSensitiveFields drives the real log_payloads path: the
// request/response payloads logged to the console must not contain the api_key
// field value, an email, or an SSN.
func TestLogPayloadsMasksSensitiveFields(t *testing.T) {
	cfg := `
[settings]
log_payloads = true

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "TEST_KEY_1"
`
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"reply to bob@example.com"},"finish_reason":"stop"}]}`))
	})
	var buf bytes.Buffer
	srv, _ := newGatewayServer(t, provider, cfg, defaultEnv, &buf)
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	resp := postChat(t, gs, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"api_key":"sk-super-secret-key","metadata":"contact alice@example.com or 123-45-6789"}`, map[string]string{"X-Session-Id": "sess-redacted-log"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	drainClose(t, resp)

	raw := buf.String()
	if !strings.Contains(raw, "request_payloads") {
		t.Fatalf("no request_payloads record logged:\n%s", raw)
	}
	for _, leak := range []string{"sk-super-secret-key", "alice@example.com", "bob@example.com", "123-45-6789"} {
		if strings.Contains(raw, leak) {
			t.Errorf("log_payloads output leaked %q:\n%s", leak, raw)
		}
	}
	// The log record's fields are JSON-marshaled, so the api_key key appears
	// with escaped quotes inside the request field.
	if !strings.Contains(raw, `\"api_key\":\"[REDACTED]\"`) {
		t.Errorf("log_payloads output did not mask the api_key field:\n%s", raw)
	}
}
