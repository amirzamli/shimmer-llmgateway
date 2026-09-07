package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func fillOnce(t *testing.T, body string) string {
	t.Helper()
	p := NewFillReasoningContent()
	req := &Request{Body: json.RawMessage(body)}
	if err := p.FilterRequest(context.Background(), req); err != nil {
		t.Fatalf("FilterRequest: %v", err)
	}
	return string(req.Body)
}

func TestFillReasoningContentAddsEmptyToAssistantWithoutIt(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}
	]}`
	out := fillOnce(t, body)
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not JSON: %v (%s)", err, out)
	}
	if got := parsed.Messages[1]["reasoning_content"]; got != "" {
		t.Errorf("reasoning_content = %#v, want empty string", got)
	}
	if _, has := parsed.Messages[2]["reasoning_content"]; has {
		t.Errorf("tool message gained a reasoning_content key: %v", parsed.Messages[2])
	}
}

func TestFillReasoningContentKeepsAssistantValue(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"assistant","content":"","reasoning_content":"think","tool_calls":[]}]}`
	if out := fillOnce(t, in); out != in {
		t.Errorf("assistant with existing reasoning_content was rewritten:\n in: %s\nout: %s", in, out)
	}
}

func TestFillReasoningContentTreatsNullAsMissing(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"assistant","content":"","reasoning_content":null,"tool_calls":[]},{"role":"assistant","content":"text"}]}`
	out := fillOnce(t, body)
	if strings.Count(out, `"reasoning_content":""`) != 2 {
		t.Errorf("want null and missing assistant messages filled: %s", out)
	}
}

func TestFillReasoningContentNoAssistantLeavesByteForByte(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if out := fillOnce(t, body); out != body {
		t.Errorf("user-only body was re-serialized:\n in: %s\nout: %s", body, out)
	}
}

func TestFillReasoningContentMalformedPassesThrough(t *testing.T) {
	for _, body := range []string{
		``,
		`not json`,
		`{"model":"m"}`,
		`{"model":"m","messages":"oops"}`,
	} {
		if out := fillOnce(t, body); out != body {
			t.Errorf("malformed body changed (%q):\n in: %s\nout: %s", body, body, out)
		}
	}
}

func TestFillReasoningContentRequestOnlyAndResponseNoop(t *testing.T) {
	p := NewFillReasoningContent()
	if !IsRequestOnly(p) {
		t.Errorf("fill_reasoning_content must be request-only")
	}
	if err := p.FilterResponse(context.Background(), &Response{Body: json.RawMessage(`{"x":1}`)}); err != nil {
		t.Errorf("FilterResponse: %v", err)
	}
}
