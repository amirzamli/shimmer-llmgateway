package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func reqBody(t *testing.T, content, args string) []byte {
	t.Helper()
	msg := map[string]any{"role": "user", "content": content}
	if args != "" {
		msg["tool_calls"] = []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "submit", "arguments": args},
		}}
	}
	b, err := json.Marshal(map[string]any{"model": "gpt-4o", "messages": []any{msg}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func filterRequestBody(t *testing.T, p Plugin, body []byte) []byte {
	t.Helper()
	req := &Request{Body: body}
	if err := p.FilterRequest(context.Background(), req); err != nil {
		t.Fatalf("FilterRequest: %v", err)
	}
	return req.Body
}

func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	if !json.Valid(b) {
		t.Fatalf("output is not valid JSON: %q", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	return m
}

func messagesContent(t *testing.T, body []byte) string {
	t.Helper()
	m := decodeMap(t, body)
	msgs := m["messages"].([]any)
	content, _ := msgs[0].(map[string]any)["content"].(string)
	return content
}

func toolArgs(t *testing.T, body []byte) string {
	t.Helper()
	m := decodeMap(t, body)
	msgs := m["messages"].([]any)
	tcs := msgs[0].(map[string]any)["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	return args
}

func TestRedactDefaultPatternsInMessageContent(t *testing.T) {
	p, err := NewRedact(RedactConfig{})
	if err != nil {
		t.Fatalf("NewRedact: %v", err)
	}
	content := "reach me at alice@example.com or +1 (555) 123-4567, key sk-abcdefghijklmnop1234567890, ssn 123-45-6789"
	body := reqBody(t, content, "")
	filtered := filterRequestBody(t, p, body)

	out := messagesContent(t, filtered)
	if strings.Contains(out, "alice@example.com") ||
		strings.Contains(out, "555") ||
		strings.Contains(out, "sk-abcdefghijklmnop1234567890") ||
		strings.Contains(out, "123-45-6789") {
		t.Errorf("content still contains sensitive values: %q", out)
	}
	if got := strings.Count(out, "[REDACTED]"); got != 4 {
		t.Errorf("content has %d [REDACTED] markers, want 4: %q", got, out)
	}
}

func TestRedactLeavesCleanContentUntouched(t *testing.T) {
	p, _ := NewRedact(RedactConfig{})
	body := reqBody(t, "hello world, how are you?", "")
	filtered := filterRequestBody(t, p, body)
	if got := messagesContent(t, filtered); got != "hello world, how are you?" {
		t.Errorf("clean content changed: %q", got)
	}
}

func TestRedactToolArgumentsFieldNamesAndPatterns(t *testing.T) {
	patterns := []string{`\bLIVE-[0-9]{4}\b`}
	fields := []string{"password"}
	p, err := NewRedact(RedactConfig{Patterns: &patterns, FieldNames: &fields})
	if err != nil {
		t.Fatalf("NewRedact: %v", err)
	}
	args := `{"password":"hunter2","nested":{"token":"LIVE-1234"},"keep":"email me a@b.com"}`
	body := reqBody(t, "ignored", args)
	filtered := filterRequestBody(t, p, body)

	out := toolArgs(t, filtered)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("tool arguments no longer valid JSON: %q (%v)", out, err)
	}
	if parsed["password"] != "[REDACTED]" {
		t.Errorf("password = %v, want [REDACTED]", parsed["password"])
	}
	if nested, ok := parsed["nested"].(map[string]any); !ok || nested["token"] != "[REDACTED]" {
		t.Errorf("nested token = %v, want [REDACTED]", parsed["nested"])
	}
	// Field-list redaction must not regex-redact the untouched value; but note
	// the default patterns are replaced by the custom set, so a@b.com survives.
	if parsed["keep"] != "email me a@b.com" {
		t.Errorf("keep = %v, want untouched", parsed["keep"])
	}
	// The surrounding request structure survived.
	if got := messagesContent(t, filtered); got != "ignored" {
		t.Errorf("message content changed: %q", got)
	}
}

func TestRedactArgumentsInvalidJSONStillRegexRedacted(t *testing.T) {
	patterns := []string{`\bSECRET[0-9]+\b`}
	p, _ := NewRedact(RedactConfig{Patterns: &patterns})
	// Not valid JSON: falls back to regex over the raw string.
	body := reqBody(t, "x", `note SECRET42 here`)
	filtered := filterRequestBody(t, p, body)
	if got := toolArgs(t, filtered); got != "note [REDACTED] here" {
		t.Errorf("arguments = %q, want regex-redacted raw string", got)
	}
}

func TestRedactResponse(t *testing.T) {
	p, _ := NewRedact(RedactConfig{})
	respBody := []byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"call alice@example.com","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{\"email\":\"b@example.com\"}"}}]},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`)
	resp := &Response{Body: respBody}
	if err := p.FilterResponse(context.Background(), resp); err != nil {
		t.Fatalf("FilterResponse: %v", err)
	}
	m := decodeMap(t, resp.Body)
	choices := m["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if got := msg["content"].(string); got != "call [REDACTED]" {
		t.Errorf("response content = %q, want redacted", got)
	}
	tcs := msg["tool_calls"].([]any)
	args := tcs[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if args != `{"email":"[REDACTED]"}` {
		t.Errorf("response tool args = %q, want field redacted", args)
	}
	// Structure preserved: usage and id untouched.
	if m["id"] != "x" {
		t.Errorf("id lost during redaction: %q", m["id"])
	}
	if usage, ok := m["usage"].(map[string]any); !ok || usage["total_tokens"] != float64(5) {
		t.Errorf("usage changed: %v", m["usage"])
	}
}

func TestRedactNonJSONPassthrough(t *testing.T) {
	p, _ := NewRedact(RedactConfig{})
	body := []byte("not json at all")
	out := filterRequestBody(t, p, body)
	if string(out) != "not json at all" {
		t.Errorf("non-JSON body changed: %q", out)
	}
}

func TestRedactInvalidPatternBuildError(t *testing.T) {
	bad := []string{"(unclosed"}
	if _, err := NewRedact(RedactConfig{Patterns: &bad}); err == nil {
		t.Fatal("invalid pattern returned no error")
	}
}
