package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/config"
	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// ---- helpers ----

// lockedBuffer is a mutex-guarded strings.Builder for log sinks written from
// request goroutines while a test reads them.
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

// postResponses issues a POST /v1/responses against the gateway test server.
func postResponses(t *testing.T, gs *httptest.Server, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gs.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/responses: %v", err)
	}
	return resp
}

// newResponsesGateway starts the gateway with a complete providers+instances
// TOML, so tests can point more than one provider block (openai and
// anthropic styles) at their own fake upstreams.
func newResponsesGateway(t *testing.T, toml string, env map[string]string) (*httptest.Server, *store.Store) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(config.New(cfg), st, logging.New(io.Discard), filepath.Join(dir, "gateway.toml"), nil, nil)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)
	return gs, st
}

// responsesErrorBody is the OpenAI error envelope the Responses surface
// returns for gateway-rejected requests.
type responsesErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// ---- request translation ----

func TestTranslateResponsesStringInput(t *testing.T) {
	req, err := parseResponsesRequest([]byte(`{"model":"gpt-4o","input":"hi there"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if v.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", v.Model)
	}
	if len(v.Messages) != 1 || v.Messages[0].Role != "user" || v.Messages[0].Content != "hi there" {
		t.Errorf("messages = %+v, want one user message", v.Messages)
	}
}

func TestTranslateResponsesInstructionsAndItems(t *testing.T) {
	body := `{
	  "model": "gpt-4o",
	  "instructions": "sys prompt",
	  "input": [
	    {"type":"message","role":"system","content":"be brief"},
	    {"type":"message","role":"developer","content":"dev rules"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]},
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"hm"}]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling"}]},
	    {"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},
	    {"type":"function_call","name":"ping","arguments":"{}"},
	    {"type":"function_call_output","call_id":"call_abc","output":"Sunny"}
	  ]
	}`
	req, err := parseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	// instructions → prepended system message; 8 input items → 8 messages
	// (the reasoning item is dropped, so 8 items yield 7 + 1 system).
	if len(v.Messages) != 8 {
		t.Fatalf("messages len = %d, want 8: %+v", len(v.Messages), v.Messages)
	}
	if v.Messages[0].Role != "system" || v.Messages[0].Content != "sys prompt" {
		t.Errorf("instructions message = %+v", v.Messages[0])
	}
	if v.Messages[1].Role != "system" || v.Messages[1].Content != "be brief" {
		t.Errorf("system message = %+v", v.Messages[1])
	}
	// developer → system in the chat body: chat upstreams reject
	// role:"developer" (z.ai/GLM error 1214).
	if v.Messages[2].Role != "system" || v.Messages[2].Content != "dev rules" {
		t.Errorf("developer message should map to chat system: %+v", v.Messages[2])
	}
	if v.Messages[3].Role != "user" || v.Messages[3].Content != "ab" {
		t.Errorf("user text parts should join: %+v", v.Messages[3])
	}
	if v.Messages[4].Role != "assistant" || v.Messages[4].Content != "calling" {
		t.Errorf("assistant output_text message = %+v", v.Messages[4])
	}
	fc := v.Messages[5]
	if fc.Role != "assistant" || len(fc.ToolCalls) != 1 || fc.ToolCalls[0].ID != "call_abc" ||
		fc.ToolCalls[0].Type != "function" || fc.ToolCalls[0].Function.Name != "get_weather" ||
		fc.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("function_call message = %+v", fc)
	}
	// Missing call_id is synthesized as call_<n> (n counts function_call items).
	fc2 := v.Messages[6]
	if len(fc2.ToolCalls) != 1 || fc2.ToolCalls[0].ID != "call_2" || fc2.ToolCalls[0].Function.Name != "ping" {
		t.Errorf("synthesized call id message = %+v", fc2)
	}
	toolMsg := v.Messages[7]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_abc" || toolMsg.Content != "Sunny" {
		t.Errorf("function_call_output message = %+v", toolMsg)
	}
}

func TestTranslateResponsesParams(t *testing.T) {
	body := `{
	  "model": "gpt-4o",
	  "input": "hi",
	  "tools": [{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object"},"strict":true}],
	  "tool_choice": {"type":"function","name":"get_weather"},
	  "parallel_tool_calls": false,
	  "temperature": 0.3,
	  "top_p": 0.9,
	  "max_output_tokens": 256,
	  "text": {"format": {"type":"json_object"}},
	  "reasoning": {"effort":"high","summary":"auto"},
	  "store": true,
	  "include": ["reasoning.encrypted_content"],
	  "metadata": {"k":"v"},
	  "prompt_cache_key": "cache-xyz"
	}`
	req, err := parseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if param, msg := validateResponsesRequest(req); param != "" || msg != "" {
		t.Fatalf("accepted-and-ignored fields rejected: %q %q", param, msg)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", m["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v", tool["type"])
	}
	fn := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "w" || fn["strict"] != true {
		t.Errorf("function tool = %v", fn)
	}
	if _, ok := fn["parameters"]; !ok {
		t.Errorf("function tool missing parameters: %v", fn)
	}
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" {
		t.Fatalf("tool_choice = %v", m["tool_choice"])
	}
	if nested, ok := tc["function"].(map[string]any); !ok || nested["name"] != "get_weather" {
		t.Errorf("tool_choice function = %v", tc["function"])
	}
	if m["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v", m["parallel_tool_calls"])
	}
	if m["temperature"] != 0.3 || m["top_p"] != 0.9 {
		t.Errorf("sampling params = %v/%v", m["temperature"], m["top_p"])
	}
	if m["max_tokens"] != float64(256) {
		t.Errorf("max_tokens = %v, want 256 (from max_output_tokens)", m["max_tokens"])
	}
	rf, ok := m["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v", m["response_format"])
	}
	if m["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", m["reasoning_effort"])
	}
	for _, absent := range []string{"store", "include", "metadata", "reasoning", "text", "instructions", "max_output_tokens", "prompt_cache_key"} {
		if _, ok := m[absent]; ok {
			t.Errorf("translated body should not carry %q: %v", absent, m)
		}
	}
}

func TestTranslateResponsesStreamFlag(t *testing.T) {
	// stream:true → the outgoing chat body must ask the upstream to stream.
	req, err := parseResponsesRequest([]byte(`{"model":"gpt-4o","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if m["stream"] != true {
		t.Errorf("stream = %v, want true: %s", m["stream"], out)
	}

	// Without streaming the key is absent (not false), like the other
	// optional passthrough fields.
	for _, name := range []string{"no stream field", "stream:false"} {
		raw := `{"model":"gpt-4o","input":"hi"}`
		if name == "stream:false" {
			raw = `{"model":"gpt-4o","input":"hi","stream":false}`
		}
		req, err := parseResponsesRequest([]byte(raw))
		if err != nil {
			t.Fatalf("parse (%s): %v", name, err)
		}
		out, err := translateResponsesToChat(req)
		if err != nil {
			t.Fatalf("translate (%s): %v", name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("output not JSON (%s): %v", name, err)
		}
		if _, ok := m["stream"]; ok {
			t.Errorf("%s: stream key should be absent: %s", name, out)
		}
	}
}

// ---- guards and translation rejections ----

func TestTranslateResponsesRejections(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantArg  string
		wantMsg  string
		validate bool // run validateResponsesRequest instead of translate
	}{
		{
			name:     "previous_response_id",
			body:     `{"model":"gpt-4o","input":"hi","previous_response_id":"resp_old"}`,
			wantArg:  "previous_response_id",
			wantMsg:  "stateless",
			validate: true,
		},
		{
			name:    "unknown item type",
			body:    `{"model":"gpt-4o","input":[{"type":"item_reference","id":"x"}]}`,
			wantMsg: "unsupported input item type",
		},
		{
			name:    "structurally invalid content",
			body:    `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":42}]}`,
			wantMsg: "invalid content",
		},
		{
			name:    "unknown role",
			body:    `{"model":"gpt-4o","input":[{"type":"message","role":"robot","content":"hi"}]}`,
			wantMsg: "unsupported message role",
		},
		{
			name:    "unknown tool_choice string",
			body:    `{"model":"gpt-4o","input":"hi","tool_choice":"moose"}`,
			wantMsg: "unsupported tool_choice",
		},
		{
			name:    "non-function tool_choice object",
			body:    `{"model":"gpt-4o","input":"hi","tool_choice":{"type":"web_search"}}`,
			wantMsg: "unsupported tool_choice",
		},
		{
			name:    "input not string or array",
			body:    `{"model":"gpt-4o","input":42}`,
			wantMsg: "invalid input",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parseResponsesRequest([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var gotMsg string
			if tc.validate {
				param, msg := validateResponsesRequest(req)
				if param != tc.wantArg {
					t.Errorf("guard param = %q, want %q", param, tc.wantArg)
				}
				gotMsg = msg
			} else {
				_, err := translateResponsesToChat(req)
				if err == nil {
					t.Fatalf("translate succeeded, want rejection")
				}
				gotMsg = err.Error()
			}
			if !strings.Contains(gotMsg, tc.wantMsg) {
				t.Errorf("message %q missing %q", gotMsg, tc.wantMsg)
			}
		})
	}
}

// Unknown tool types are skipped, not rejected: Codex CLI 0.153 sends newer
// tool groupings (e.g. "namespace") alongside function tools, and a 400
// aborted every turn. Only the function tools reach the chat body.
func TestTranslateResponsesUnknownToolsSkipped(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","tools":[
	  {"type":"function","name":"get_weather","description":"w","parameters":{"type":"object"}},
	  {"type":"namespace","name":"browser","tools":[]},
	  {"type":"local_shell"}]}`
	req, err := parseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if param, msg := validateResponsesRequest(req); param != "" || msg != "" {
		t.Fatalf("guard rejected unknown tool types: %q %q", param, msg)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want only the function tool", m["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v, want function", tool["type"])
	}
	if fn, ok := tool["function"].(map[string]any); !ok || fn["name"] != "get_weather" {
		t.Errorf("function = %v, want get_weather", tool["function"])
	}

	// Only unknown types → no tools key at all (never an empty array).
	req, err = parseResponsesRequest([]byte(`{"model":"gpt-4o","input":"hi","tools":[{"type":"local_shell"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err = translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	m = map[string]any{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if _, ok := m["tools"]; ok {
		t.Errorf("tools key should be absent when every tool was skipped: %s", out)
	}
}

// input_image parts map to chat image_url parts: the Responses image_url is a
// plain string forwarded verbatim, the optional detail hint rides along, and
// text parts keep their order.
func TestTranslateResponsesImageParts(t *testing.T) {
	body := `{"model":"gpt-4o","input":[
	  {"type":"message","role":"user","content":[
	    {"type":"input_text","text":"what is this?"},
	    {"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"high"},
	    {"type":"input_image","image_url":"https://x/y.png"}]},
	  {"type":"message","role":"user","content":[
	    {"type":"input_image","image_url":"https://x/z.png"}]}]}`
	req, err := parseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if len(v.Messages) != 2 {
		t.Fatalf("messages = %+v", v.Messages)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL    string `json:"url"`
			Detail string `json:"detail"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(v.Messages[0].Content, &parts); err != nil {
		t.Fatalf("content not a part array: %v (%s)", err, v.Messages[0].Content)
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "what is this?" {
		t.Errorf("part 0 = %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL.URL != "data:image/png;base64,AAAA" || parts[1].ImageURL.Detail != "high" {
		t.Errorf("part 1 = %+v, want image_url with the data URI and detail high", parts[1])
	}
	if parts[2].Type != "image_url" || parts[2].ImageURL.URL != "https://x/y.png" || parts[2].ImageURL.Detail != "" {
		t.Errorf("part 2 = %+v, want image_url with the https URL and no detail", parts[2])
	}
	if err := json.Unmarshal(v.Messages[1].Content, &parts); err != nil {
		t.Fatalf("second content not a part array: %v (%s)", err, v.Messages[1].Content)
	}
	if len(parts) != 1 || parts[0].Type != "image_url" || parts[0].ImageURL.URL != "https://x/z.png" {
		t.Errorf("image-only message = %+v", parts)
	}
}

// Image parts that reference a file (file_id, no URL) are skipped, not
// rejected: surrounding text is preserved, and a message whose parts are ALL
// skipped gets a placeholder text part so the chat content is never empty.
func TestTranslateResponsesFileIDImageSkipped(t *testing.T) {
	body := `{"model":"gpt-4o","input":[
	  {"type":"message","role":"user","content":[
	    {"type":"input_text","text":"describe"},
	    {"type":"input_image","file_id":"file-1"}]},
	  {"type":"message","role":"user","content":[
	    {"type":"input_image","file_id":"file-2"}]}]}`
	req, err := parseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if len(v.Messages) != 2 {
		t.Fatalf("messages = %+v", v.Messages)
	}
	var content string
	if err := json.Unmarshal(v.Messages[0].Content, &content); err != nil {
		t.Fatalf("first content should collapse to the remaining text: %v (%s)", err, v.Messages[0].Content)
	}
	if content != "describe" {
		t.Errorf("content = %q, want describe (file_id image skipped)", content)
	}
	var parts []map[string]any
	if err := json.Unmarshal(v.Messages[1].Content, &parts); err != nil {
		t.Fatalf("all-skipped content should be a placeholder part array: %v (%s)", err, v.Messages[1].Content)
	}
	if len(parts) != 1 || parts[0]["type"] != "text" || parts[0]["text"] != contentOmittedPlaceholder {
		t.Errorf("placeholder content = %v, want one %q text part", parts, contentOmittedPlaceholder)
	}
}

// Unknown part types (e.g. input_audio) are skipped, not rejected — the same
// tolerance as unknown tool types; remaining text is preserved.
func TestTranslateResponsesUnknownPartSkipped(t *testing.T) {
	body := `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[
	  {"type":"input_audio","input_audio":{"format":"wav","data":"x"}},
	  {"type":"input_text","text":"hi"}]}]}`
	req, err := parseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if len(v.Messages) != 1 || v.Messages[0].Content != "hi" {
		t.Errorf("messages = %+v, want one user message with the text preserved", v.Messages)
	}
}

// function_call_output accepts array content: text-only arrays still collapse
// to a string, arrays with image parts become a chat tool content array of
// text/image_url parts.
func TestTranslateResponsesFunctionCallOutputArray(t *testing.T) {
	body := `{"model":"gpt-4o","input":[
	  {"type":"function_call_output","call_id":"call_1","output":[
	    {"type":"output_text","text":"chart:"},
	    {"type":"input_image","image_url":"https://x/c.png"}]},
	  {"type":"function_call_output","call_id":"call_2","output":[
	    {"type":"output_text","text":"plain"}]}]}`
	req, err := parseResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if len(v.Messages) != 2 {
		t.Fatalf("messages = %+v", v.Messages)
	}
	if v.Messages[0].Role != "tool" || v.Messages[0].ToolCallID != "call_1" {
		t.Errorf("first tool message = %+v", v.Messages[0])
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(v.Messages[0].Content, &parts); err != nil {
		t.Fatalf("tool content not a part array: %v (%s)", err, v.Messages[0].Content)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "chart:" ||
		parts[1].Type != "image_url" || parts[1].ImageURL.URL != "https://x/c.png" {
		t.Errorf("tool content parts = %+v", parts)
	}
	var content string
	if err := json.Unmarshal(v.Messages[1].Content, &content); err != nil {
		t.Fatalf("text-only tool content should collapse to a string: %v (%s)", err, v.Messages[1].Content)
	}
	if content != "plain" {
		t.Errorf("tool content = %q, want plain", content)
	}
}

// ---- response translation ----

func TestChatCompletionToResponse(t *testing.T) {
	chat := `{"id":"chatcmpl-1","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	out := chatCompletionToResponse("resp_fixed", []byte(chat))
	var v struct {
		ID                string `json:"id"`
		Object            string `json:"object"`
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails any    `json:"incomplete_details"`
		Output            []struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Role    string `json:"role"`
			Status  string `json:"status"`
			Content []struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Annotations []any  `json:"annotations"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, out)
	}
	if v.ID != "resp_fixed" || v.Object != "response" || v.Model != "gpt-4o" {
		t.Errorf("id/object/model = %q/%q/%q", v.ID, v.Object, v.Model)
	}
	if v.Status != "completed" || v.IncompleteDetails != nil {
		t.Errorf("status/incomplete_details = %q/%v", v.Status, v.IncompleteDetails)
	}
	if len(v.Output) != 1 {
		t.Fatalf("output = %+v", v.Output)
	}
	msg := v.Output[0]
	if msg.Type != "message" || msg.Role != "assistant" || msg.Status != "completed" {
		t.Errorf("message item = %+v", msg)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "output_text" || msg.Content[0].Text != "Hello" {
		t.Errorf("content = %+v", msg.Content)
	}
	if len(msg.Content[0].Annotations) != 0 {
		t.Errorf("annotations = %+v, want empty", msg.Content[0].Annotations)
	}
	if v.Usage.InputTokens != 5 || v.Usage.OutputTokens != 3 || v.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", v.Usage)
	}
}

func TestChatCompletionToResponseStatusAndDetails(t *testing.T) {
	// length → incomplete with max_output_tokens reason.
	out := chatCompletionToResponse("resp_1", []byte(`{"model":"m","choices":[{"message":{"content":"x"},"finish_reason":"length"}]}`))
	var v struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if v.Status != "incomplete" || v.IncompleteDetails == nil || v.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("length status = %q/%+v", v.Status, v.IncompleteDetails)
	}

	// tool_calls finish stays completed.
	out = chatCompletionToResponse("resp_1", []byte(`{"model":"m","choices":[{"message":{"content":null},"finish_reason":"tool_calls"}]}`))
	v.Status = ""
	json.Unmarshal(out, &v)
	if v.Status != "completed" {
		t.Errorf("tool_calls status = %q, want completed", v.Status)
	}

	// Unparseable bodies pass through unchanged rather than being fabricated.
	raw := []byte(`not json at all`)
	if got := chatCompletionToResponse("resp_1", raw); string(got) != string(raw) {
		t.Errorf("unparseable body mangled: %s", got)
	}
}

func TestChatCompletionToResponseReasoningAndToolCalls(t *testing.T) {
	chat := `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"thinking hard","tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16,"completion_tokens_details":{"reasoning_tokens":4}}}`
	out := chatCompletionToResponse("resp_1", []byte(chat))
	var v struct {
		Output []struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Status  string `json:"status"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			OutputTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, out)
	}
	if len(v.Output) != 3 {
		t.Fatalf("output len = %d, want reasoning+message+function_call: %s", len(v.Output), out)
	}
	rs := v.Output[0]
	if rs.Type != "reasoning" || len(rs.Summary) != 1 || rs.Summary[0].Text != "thinking hard" {
		t.Errorf("reasoning item = %+v", rs)
	}
	if !strings.HasPrefix(rs.ID, "rs_") {
		t.Errorf("reasoning id = %q, want rs_ prefix", rs.ID)
	}
	if v.Output[1].Type != "message" {
		t.Errorf("message item = %+v", v.Output[1])
	}
	fc := v.Output[2]
	if fc.Type != "function_call" || fc.ID != "call_9" || fc.CallID != "call_9" ||
		fc.Name != "get_weather" || fc.Arguments != `{"city":"Paris"}` || fc.Status != "completed" {
		t.Errorf("function_call item = %+v", fc)
	}
	if v.Usage.OutputTokensDetails == nil || v.Usage.OutputTokensDetails.ReasoningTokens != 4 {
		t.Errorf("output_tokens_details = %+v", v.Usage.OutputTokensDetails)
	}
}

// ---- end-to-end ----

func TestResponsesNonStreamEndToEndChatUpstream(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	raw := `{"model":"gpt-4o","input":"hi","store":true,"include":["reasoning.encrypted_content"],"metadata":{"k":"v"}}`
	resp := postResponses(t, gs, raw, map[string]string{"X-Session-Id": "sess-resp"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Gateway-Session-Id"); got != "sess-resp" {
		t.Errorf("X-Gateway-Session-Id = %q, want sess-resp", got)
	}
	var v struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("client body not a response object: %v (%s)", err, body)
	}
	if !strings.HasPrefix(v.ID, "resp_") {
		t.Errorf("id = %q, want resp_ prefix", v.ID)
	}
	if v.Object != "response" || v.Status != "completed" {
		t.Errorf("object/status = %q/%q", v.Object, v.Status)
	}
	if len(v.Output) != 1 || v.Output[0].Type != "message" || len(v.Output[0].Content) != 1 || v.Output[0].Content[0].Text != "hi" {
		t.Errorf("output = %+v", v.Output)
	}
	if v.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", v.Usage)
	}

	// The upstream saw a Chat Completions body, not the Responses request.
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	var upstream struct {
		Model    string `json:"model"`
		Messages []any  `json:"messages"`
		Input    any    `json:"input"`
		Stream   any    `json:"stream"`
		Store    any    `json:"store"`
	}
	if err := json.Unmarshal(seen[0].Body, &upstream); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, seen[0].Body)
	}
	if upstream.Model != "gpt-4o" || len(upstream.Messages) != 1 || upstream.Input != nil || upstream.Store != nil || upstream.Stream != nil {
		t.Errorf("upstream body = %s", seen[0].Body)
	}

	// Capture: endpoint = /v1/responses, request_json as-received (Responses
	// bytes), response_json chat-shaped.
	req := waitForRequest(t, st, "sess-resp", 5*time.Second)
	if req.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", req.Endpoint)
	}
	if string(req.RequestJSON) != raw {
		t.Errorf("request_json not the as-received Responses bytes:\n got %q\nwant %q", req.RequestJSON, raw)
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
	if req.StatusCode != http.StatusOK || req.FinishReason != "stop" {
		t.Errorf("status/finish = %d/%q, want 200/stop", req.StatusCode, req.FinishReason)
	}
	if req.Error != nil {
		t.Errorf("error = %+v", req.Error)
	}
	// Tool-call extraction keeps working off the chat-shaped response_json
	// (none here) and the session counted one success.
	sess, err := st.GetSession(context.Background(), "sess-resp")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestCount != 1 || sess.FailureCount != 0 {
		t.Errorf("session counters = %d/%d, want 1/0", sess.RequestCount, sess.FailureCount)
	}
}

func TestResponsesNonStreamAnthropicUpstream(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			provider.t.Errorf("anthropic upstream path = %q, want /messages suffix", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":2}}`))
	})
	toml := fmt.Sprintf(`
[settings]
default_alias = "ant"

[providers.anthropicp]
base_url = %q
style = "anthropic"
models = ["claude-x"]

[[instances]]
alias = "ant"
template = "anthropicp"
api_key_env = "TEST_KEY_1"
`, provider.url()+"/v1")
	gs, st := newResponsesGateway(t, toml, defaultEnv)

	resp := postResponses(t, gs, `{"model":"claude-x","input":"hi","max_output_tokens":128,"instructions":"be brief"}`, map[string]string{"X-Session-Id": "sess-ant"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var v struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("client body not a response object: %v (%s)", err, body)
	}
	if v.Object != "response" || v.Status != "completed" {
		t.Errorf("object/status = %q/%q", v.Object, v.Status)
	}
	if len(v.Output) != 1 || v.Output[0].Type != "message" || v.Output[0].Content[0].Text != "Hello" {
		t.Errorf("output = %+v", v.Output)
	}
	if v.Usage.InputTokens != 4 || v.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v", v.Usage)
	}

	// The upstream saw an Anthropic Messages body (max_tokens is injected by
	// the anthropic composition, never present in the chat body here).
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	if !strings.Contains(string(seen[0].Body), `"max_tokens":128`) {
		t.Errorf("anthropic body missing max_tokens: %s", seen[0].Body)
	}

	req := waitForRequest(t, st, "sess-ant", 5*time.Second)
	if req.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", req.Endpoint)
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
	if req.FinishReason != "stop" {
		t.Errorf("finish = %q, want stop (from end_turn)", req.FinishReason)
	}
}

func TestResponsesGuardsEndToEnd(t *testing.T) {
	provider := newFakeProvider(t, nil)
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// previous_response_id → Responses-shaped 400, provider never contacted.
	resp := postResponses(t, gs, `{"model":"gpt-4o","input":"hi","previous_response_id":"resp_old"}`, nil)
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("previous_response_id status = %d, want 400", resp.StatusCode)
	}
	var e responsesErrorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body: %v (%s)", err, body)
	}
	if e.Error.Type != "invalid_request_error" || e.Error.Param != "previous_response_id" || !strings.Contains(e.Error.Message, "stateless") {
		t.Errorf("error = %+v", e.Error)
	}

	// Unknown alias → 400 with the alias list, param model.
	resp = postResponses(t, gs, `{"model":"nope/gpt-4o","input":"hi"}`, nil)
	body = drainClose(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown alias status = %d, want 400", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body: %v (%s)", err, body)
	}
	if e.Error.Param != "model" || !strings.Contains(e.Error.Message, "openai-2") {
		t.Errorf("error = %+v", e.Error)
	}

	// GET /v1/responses/{id} → 404 (stateless: nothing retrievable).
	resp2, err := http.Get(gs.URL + "/v1/responses/resp_x")
	if err != nil {
		t.Fatal(err)
	}
	body = drainClose(t, resp2)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/responses/{id} status = %d, want 404", resp2.StatusCode)
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body: %v (%s)", err, body)
	}
	if e.Error.Type != "invalid_request_error" {
		t.Errorf("404 error type = %q", e.Error.Type)
	}

	if n := len(provider.requests()); n != 0 {
		t.Errorf("provider saw %d requests for rejected input, want 0", n)
	}
}

// Unknown tool types ride through the full pipeline: the client gets a normal
// response, the upstream chat body carries only the function tools, the skip
// is logged once, and capture still stores the as-received Responses request.
func TestResponsesUnknownToolsSkippedEndToEnd(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})
	var logBuf lockedBuffer
	srv, st := newGatewayServer(t, provider, twoInstanceTOML, defaultEnv, &logBuf)
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	raw := `{"model":"gpt-4o","input":"hi","tools":[{"type":"function","name":"get_weather"},{"type":"namespace","name":"browser"},{"type":"local_shell"}]}`
	resp := postResponses(t, gs, raw, map[string]string{"X-Session-Id": "sess-skip"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}

	// The upstream chat body carries only the function tool.
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	var upstream struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(seen[0].Body, &upstream); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, seen[0].Body)
	}
	if len(upstream.Tools) != 1 || upstream.Tools[0].Type != "function" || upstream.Tools[0].Function.Name != "get_weather" {
		t.Errorf("upstream tools = %+v, want only get_weather", upstream.Tools)
	}

	// The skip is logged once, naming the dropped types.
	lines := logBuf.String()
	if !strings.Contains(lines, `"event":"responses_unsupported_tool_skipped"`) {
		t.Errorf("expected a responses_unsupported_tool_skipped warn log line:\n%s", lines)
	}
	if !strings.Contains(lines, `"types":["namespace","local_shell"]`) {
		t.Errorf("skip log should name the dropped types:\n%s", lines)
	}

	// Capture invariants unchanged: endpoint + as-received request bytes.
	req := waitForRequest(t, st, "sess-skip", 5*time.Second)
	if req.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", req.Endpoint)
	}
	if string(req.RequestJSON) != raw {
		t.Errorf("request_json not the as-received Responses bytes:\n got %q\nwant %q", req.RequestJSON, raw)
	}
}

// Image input rides the full pipeline: the provider sees the chat image_url
// part, the client gets a normal Responses object, and capture keeps the
// as-received Responses bytes (image included) with a chat-shaped response.
func TestResponsesImageEndToEnd(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"a cat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	raw := `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this?"},{"type":"input_image","image_url":"https://x/cat.png"}]}]}`
	resp := postResponses(t, gs, raw, map[string]string{"X-Session-Id": "sess-img"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var v struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("client body not a response object: %v (%s)", err, body)
	}
	if v.Object != "response" || v.Status != "completed" {
		t.Errorf("object/status = %q/%q", v.Object, v.Status)
	}
	if len(v.Output) != 1 || v.Output[0].Type != "message" || len(v.Output[0].Content) != 1 || v.Output[0].Content[0].Text != "a cat" {
		t.Errorf("output = %+v", v.Output)
	}

	// The upstream chat body carries the image as an image_url part.
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
		t.Errorf("upstream parts = %+v", parts)
	}

	// Capture: as-received Responses bytes (image included), chat-shaped
	// response.
	req := waitForRequest(t, st, "sess-img", 5*time.Second)
	if req.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", req.Endpoint)
	}
	if string(req.RequestJSON) != raw {
		t.Errorf("request_json not the as-received Responses bytes:\n got %q\nwant %q", req.RequestJSON, raw)
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
	if req.StatusCode != http.StatusOK || req.FinishReason != "stop" {
		t.Errorf("status/finish = %d/%q, want 200/stop", req.StatusCode, req.FinishReason)
	}
}

// A request whose image parts cannot be forwarded (file references) still
// succeeds end to end: the skip is warned once with the dropped part type and
// capture keeps the as-received Responses bytes.
func TestResponsesUnsupportedPartSkippedEndToEnd(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})
	var logBuf lockedBuffer
	srv, st := newGatewayServer(t, provider, twoInstanceTOML, defaultEnv, &logBuf)
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)

	raw := `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","file_id":"file-1"}]}]}`
	resp := postResponses(t, gs, raw, map[string]string{"X-Session-Id": "sess-partskip"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}

	lines := logBuf.String()
	if !strings.Contains(lines, `"event":"responses_unsupported_part_skipped"`) {
		t.Errorf("expected a responses_unsupported_part_skipped warn log line:\n%s", lines)
	}
	if !strings.Contains(lines, `"types":["input_image"]`) {
		t.Errorf("skip log should name the dropped part type:\n%s", lines)
	}

	req := waitForRequest(t, st, "sess-partskip", 5*time.Second)
	if string(req.RequestJSON) != raw {
		t.Errorf("request_json not the as-received Responses bytes:\n got %q\nwant %q", req.RequestJSON, raw)
	}
}

func TestResponsesToolCallTurnEndToEnd(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(provider.requests()[len(provider.requests())-1].Body), `"tool_calls"`) {
			// Second turn: the tool result round-trips back as a final answer.
			w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Sunny in Paris"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`))
			return
		}
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	// First turn: plain input → function_call output item.
	resp := postResponses(t, gs, `{"model":"gpt-4o","input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`, map[string]string{"X-Session-Id": "sess-fc"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var v struct {
		Output []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("client body: %v (%s)", err, body)
	}
	if v.Status != "completed" {
		t.Errorf("status = %q", v.Status)
	}
	if len(v.Output) != 2 {
		t.Fatalf("output = %+v", v.Output)
	}
	fc := v.Output[1]
	if fc.Type != "function_call" || fc.CallID != "call_abc" || fc.ID != "call_abc" || fc.Name != "get_weather" {
		t.Errorf("function_call item = %+v", fc)
	}

	// Second turn: the tool result comes back as a function_call_output item.
	resp = postResponses(t, gs, `{"model":"gpt-4o","input":[{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},{"type":"function_call_output","call_id":"call_abc","output":"Sunny"}],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`, map[string]string{"X-Session-Id": "sess-fc2"})
	body = drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second turn status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Sunny in Paris") {
		t.Errorf("second turn body = %s", body)
	}

	// The second capture's response_json is chat-shaped; the upstream saw the
	// tool result as a role:"tool" chat message.
	seen := provider.requests()
	if len(seen) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(seen))
	}
	if !strings.Contains(string(seen[1].Body), `"tool_call_id":"call_abc"`) {
		t.Errorf("upstream body missing tool result message: %s", seen[1].Body)
	}
	waitForRequest(t, st, "sess-fc2", 5*time.Second)
}

// Codex CLI sends no X-Session-Id header but does send a stable
// per-conversation prompt_cache_key in the body; without grouping each turn
// captured as its own 1-request session. Resolution order: an explicit header
// always wins; else the body key derives the session (prefixed
// "responses-" to avoid cross-surface collisions); else a fresh UUID per
// request, the unchanged no-header fallback.
func TestResponsesSessionGrouping(t *testing.T) {
	provider := newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})
	gs, st := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	const key = "cache-abc.123"
	body := `{"model":"gpt-4o","input":"hi","prompt_cache_key":"` + key + `"}`
	derived := "responses-" + key

	// (a) Same body key, no header → one captured session, seq 1 then 2.
	resp := postResponses(t, gs, body, nil)
	drainClose(t, resp)
	if got := resp.Header.Get("X-Gateway-Session-Id"); got != derived {
		t.Errorf("first X-Gateway-Session-Id = %q, want %q", got, derived)
	}
	first := waitForRequest(t, st, derived, 5*time.Second)
	if first.Seq != 1 || first.SessionID != derived {
		t.Errorf("first capture seq/session = %d/%q, want 1/%q", first.Seq, first.SessionID, derived)
	}

	drainClose(t, postResponses(t, gs, body, nil))
	sess := waitForSessionRequests(t, st, derived, 2, 5*time.Second)
	if sess.Requests[1].Seq != 2 {
		t.Errorf("second capture seq = %d, want 2", sess.Requests[1].Seq)
	}
	if sess.RequestCount != 2 || sess.FailureCount != 0 {
		t.Errorf("session counters = %d/%d, want 2/0", sess.RequestCount, sess.FailureCount)
	}

	// (b) An explicit X-Session-Id header wins over the body key.
	resp = postResponses(t, gs, body, map[string]string{"X-Session-Id": "sess-header-wins"})
	drainClose(t, resp)
	if got := resp.Header.Get("X-Gateway-Session-Id"); got != "sess-header-wins" {
		t.Errorf("header X-Gateway-Session-Id = %q, want sess-header-wins", got)
	}
	if req := waitForRequest(t, st, "sess-header-wins", 5*time.Second); req.Seq != 1 {
		t.Errorf("header capture seq = %d, want 1", req.Seq)
	}
	// The derived session gained nothing from the header turn.
	if got := sessionCount(t, st, derived); got != 2 {
		t.Errorf("derived session request count = %d, want 2 (header turn went elsewhere)", got)
	}

	// (c) Neither header nor key → distinct fresh-UUID sessions per request.
	bodyNoKey := `{"model":"gpt-4o","input":"hi"}`
	resp1 := postResponses(t, gs, bodyNoKey, nil)
	sid1 := resp1.Header.Get("X-Gateway-Session-Id")
	drainClose(t, resp1)
	resp2 := postResponses(t, gs, bodyNoKey, nil)
	sid2 := resp2.Header.Get("X-Gateway-Session-Id")
	drainClose(t, resp2)
	if sid1 == "" || sid2 == "" {
		t.Fatalf("fallback session ids empty: %q/%q", sid1, sid2)
	}
	if sid1 == sid2 || sid1 == derived {
		t.Errorf("fallback sessions should be distinct per request: %q vs %q", sid1, sid2)
	}
	waitForRequest(t, st, sid1, 5*time.Second)
	waitForRequest(t, st, sid2, 5*time.Second)
}

// waitForSessionRequests polls the store until the session holds n captured
// requests, returning it.
func waitForSessionRequests(t *testing.T, st *store.Store, sessionID string, n int, timeout time.Duration) *store.Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess, err := st.GetSession(context.Background(), sessionID)
		if err == nil && len(sess.Requests) >= n {
			return sess
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %q never reached %d captured requests within %v", sessionID, n, timeout)
	return nil
}

// sessionCount returns a session's captured request count (0 when it does not
// exist yet).
func sessionCount(t *testing.T, st *store.Store, sessionID string) int {
	t.Helper()
	sess, err := st.GetSession(context.Background(), sessionID)
	if err != nil {
		return 0
	}
	return sess.RequestCount
}

// TestResponsesStreamUpstreamSawStreamFlag is the regression test for the
// shim's missing upstream stream flag: a streamed Responses request must
// reach the chat upstream with "stream":true. Unlike the other fake
// upstreams, this one streams ONLY when the received body asks for it and
// otherwise answers with one plain non-stream JSON body — so a client-visible
// text delta proves the flag round-tripped. With the flag missing, the
// upstream answer carries no SSE frames and the client is left with lifecycle
// events and empty output.
func TestResponsesStreamUpstreamSawStreamFlag(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(string(provider.requests()[len(provider.requests())-1].Body), `"stream":true`) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, line := range []string{
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1722600000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
			`data: [DONE]`,
		} {
			fmt.Fprintf(w, "%s\n\n", line)
			fl.Flush()
		}
	})
	gs, _ := newGatewayTest(t, provider, twoInstanceTOML, defaultEnv)

	raw := `{"model":"gpt-4o","input":"hi","stream":true}`
	resp := postResponses(t, gs, raw, map[string]string{"X-Session-Id": "sess-rflag"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}

	// The client must see real text deltas, not just lifecycle events.
	var deltas []string
	for _, ev := range parseResponsesSSE(t, body) {
		if ev.Event == "response.output_text.delta" {
			if s, ok := ev.Data["delta"].(string); ok {
				deltas = append(deltas, s)
			}
		}
	}
	if got, want := strings.Join(deltas, ""), "Hello world"; got != want {
		t.Errorf("output_text deltas joined = %q, want %q (stream body: %s)", got, want, body)
	}

	// And the upstream must have been asked to stream.
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	if !strings.Contains(string(seen[0].Body), `"stream":true`) {
		t.Errorf(`upstream body missing "stream":true: %s`, seen[0].Body)
	}
}
