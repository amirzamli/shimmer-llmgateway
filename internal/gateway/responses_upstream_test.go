package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---- request translation ----

func TestTranslateChatToResponsesMessages(t *testing.T) {
	body := `{
	  "model": "zen-x",
	  "messages": [
	    {"role":"system","content":"sys prompt"},
	    {"role":"system","content":"more rules"},
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":"calling"},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
	    {"role":"tool","tool_call_id":"call_abc","content":"Sunny"}
	  ]
	}`
	out, err := translateChatToResponses([]byte(body), "sess-1")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var m struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Input        []struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			Content   string `json:"content"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Output    string `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if m.Model != "zen-x" {
		t.Errorf("model = %q, want zen-x", m.Model)
	}
	// The first system message hoists to instructions; the second becomes a
	// system input item.
	if m.Instructions != "sys prompt" {
		t.Errorf("instructions = %q, want %q", m.Instructions, "sys prompt")
	}
	if len(m.Input) != 5 {
		t.Fatalf("input len = %d, want 5: %+v", len(m.Input), m.Input)
	}
	if m.Input[0].Type != "message" || m.Input[0].Role != "system" || m.Input[0].Content != "more rules" {
		t.Errorf("system input item = %+v", m.Input[0])
	}
	if m.Input[1].Type != "message" || m.Input[1].Role != "user" || m.Input[1].Content != "hi" {
		t.Errorf("user input item = %+v", m.Input[1])
	}
	if m.Input[2].Type != "message" || m.Input[2].Role != "assistant" || m.Input[2].Content != "calling" {
		t.Errorf("assistant input item = %+v", m.Input[2])
	}
	fc := m.Input[3]
	if fc.Type != "function_call" || fc.CallID != "call_abc" || fc.Name != "get_weather" || fc.Arguments != `{"city":"Paris"}` {
		t.Errorf("function_call item = %+v", fc)
	}
	toolOut := m.Input[4]
	if toolOut.Type != "function_call_output" || toolOut.CallID != "call_abc" || toolOut.Output != "Sunny" {
		t.Errorf("function_call_output item = %+v", toolOut)
	}
}

func TestTranslateChatToResponsesParams(t *testing.T) {
	out, err := translateChatToResponses([]byte(`{
	  "model": "zen-x",
	  "messages": [{"role":"user","content":"hi"}],
	  "tools": [{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}],
	  "tool_choice": "auto",
	  "parallel_tool_calls": false,
	  "temperature": 0.3,
	  "top_p": 0.9,
	  "max_completion_tokens": 32000,
	  "reasoning_effort": "high",
	  "response_format": {"type":"json_object"},
	  "stream": true
	}`), "sess-1")
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
	if tool["type"] != "function" || tool["name"] != "get_weather" {
		t.Errorf("tool = %v", tool)
	}
	if tool["description"] != "w" {
		t.Errorf("tool description = %v", tool["description"])
	}
	if _, ok := tool["parameters"]; !ok {
		t.Errorf("tool missing parameters: %v", tool)
	}
	if _, ok := tool["function"]; ok {
		t.Errorf("tool should be flat, not nested: %v", tool)
	}
	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "auto" {
		t.Errorf("tool_choice = %v", m["tool_choice"])
	}
	if m["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v", m["parallel_tool_calls"])
	}
	if m["temperature"] != 0.3 || m["top_p"] != 0.9 {
		t.Errorf("sampling params = %v/%v", m["temperature"], m["top_p"])
	}
	if m["max_output_tokens"] != float64(32000) {
		t.Errorf("max_output_tokens = %v, want 32000 (from max_completion_tokens)", m["max_output_tokens"])
	}
	if m["stream"] != true {
		t.Errorf("stream = %v, want true", m["stream"])
	}
	reasoning, ok := m["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" {
		t.Errorf("reasoning = %v", m["reasoning"])
	}
	text, ok := m["text"].(map[string]any)
	if !ok || text["format"] == nil {
		t.Errorf("text = %v", m["text"])
	}
	if m["prompt_cache_key"] != "sess-1" {
		t.Errorf("prompt_cache_key = %v, want sess-1", m["prompt_cache_key"])
	}
	for _, absent := range []string{"max_tokens", "max_completion_tokens", "response_format", "reasoning_effort", "messages"} {
		if _, ok := m[absent]; ok {
			t.Errorf("translated body should not carry %q: %v", absent, m)
		}
	}
}

func TestTranslateChatToResponsesMaxTokens(t *testing.T) {
	// max_tokens wins over max_completion_tokens when both are present.
	out, err := translateChatToResponses([]byte(`{"model":"zen-x","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"max_completion_tokens":200}`), "")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if m["max_output_tokens"] != float64(100) {
		t.Errorf("max_output_tokens = %v, want 100", m["max_output_tokens"])
	}

	// Neither present → no max_output_tokens key (no default injected).
	out, err = translateChatToResponses([]byte(`{"model":"zen-x","messages":[{"role":"user","content":"hi"}]}`), "")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(out, &m2); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if _, ok := m2["max_output_tokens"]; ok {
		t.Errorf("max_output_tokens should be absent (no default): %v", m2)
	}
}

func TestTranslateChatToResponsesAbsentFields(t *testing.T) {
	// Non-stream request with no session id: stream and prompt_cache_key are
	// absent (not false/empty), like the other optional passthrough fields.
	out, err := translateChatToResponses([]byte(`{"model":"zen-x","messages":[{"role":"user","content":"hi"}]}`), "")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	for _, absent := range []string{"stream", "prompt_cache_key", "tool_choice", "reasoning", "text"} {
		if _, ok := m[absent]; ok {
			t.Errorf("key %q should be absent: %v", absent, m)
		}
	}
}

func TestTranslateChatToResponsesUnparseable(t *testing.T) {
	if _, err := translateChatToResponses([]byte(`not json`), ""); err == nil {
		t.Errorf("translate accepted an unparseable body")
	}
}

// ---- request round trip through the client shim ----

func TestTranslateChatToResponsesRoundTrip(t *testing.T) {
	chatBody := `{
	  "model": "zen-x",
	  "messages": [
	    {"role":"system","content":"sys"},
	    {"role":"system","content":"more"},
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":"calling"},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
	    {"role":"tool","tool_call_id":"call_abc","content":"Sunny"}
	  ],
	  "tools": [{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}],
	  "tool_choice": {"type":"function","function":{"name":"get_weather"}},
	  "max_tokens": 256,
	  "reasoning_effort": "high",
	  "response_format": {"type":"json_object"}
	}`
	out, err := translateChatToResponses([]byte(chatBody), "sess-1")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	req, err := parseResponsesRequest(out)
	if err != nil {
		t.Fatalf("parse translated body: %v (%s)", err, out)
	}
	chatOut, err := translateResponsesToChat(req)
	if err != nil {
		t.Fatalf("translate back: %v", err)
	}
	var v struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools       []any `json:"tools"`
		ToolChoice  any   `json:"tool_choice"`
		MaxTokens   any   `json:"max_tokens"`
		Reasoning   any   `json:"reasoning_effort"`
		ResponseFmt any   `json:"response_format"`
	}
	if err := json.Unmarshal(chatOut, &v); err != nil {
		t.Fatalf("round-trip output not JSON: %v (%s)", err, chatOut)
	}
	if len(v.Messages) != 6 {
		t.Fatalf("messages len = %d, want 6: %+v", len(v.Messages), v.Messages)
	}
	if v.Messages[0].Role != "system" || v.Messages[0].Content != "sys" {
		t.Errorf("instructions round trip = %+v", v.Messages[0])
	}
	if v.Messages[1].Role != "system" || v.Messages[1].Content != "more" {
		t.Errorf("second system message round trip = %+v", v.Messages[1])
	}
	if v.Messages[2].Role != "user" || v.Messages[2].Content != "hi" {
		t.Errorf("user round trip = %+v", v.Messages[2])
	}
	if v.Messages[3].Role != "assistant" || v.Messages[3].Content != "calling" {
		t.Errorf("assistant round trip = %+v", v.Messages[3])
	}
	fc := v.Messages[4]
	if fc.Role != "assistant" || len(fc.ToolCalls) != 1 || fc.ToolCalls[0].ID != "call_abc" ||
		fc.ToolCalls[0].Function.Name != "get_weather" || fc.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("function_call round trip = %+v", fc)
	}
	tool := v.Messages[5]
	if tool.Role != "tool" || tool.ToolCallID != "call_abc" || tool.Content != "Sunny" {
		t.Errorf("tool round trip = %+v", tool)
	}
	if len(v.Tools) != 1 {
		t.Errorf("tools round trip = %v, want 1", v.Tools)
	}
	if v.MaxTokens != float64(256) {
		t.Errorf("max_tokens round trip = %v, want 256", v.MaxTokens)
	}
	if v.Reasoning != "high" {
		t.Errorf("reasoning_effort round trip = %v, want high", v.Reasoning)
	}
	if v.ResponseFmt == nil {
		t.Errorf("response_format lost in round trip")
	}
}

// ---- response translation ----

func TestTranslateResponsesToChatCompletion(t *testing.T) {
	body := `{
	  "id": "resp_1",
	  "object": "response",
	  "status": "completed",
	  "model": "zen-x",
	  "output": [
	    {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]},
	    {"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]},
	    {"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}
	  ],
	  "usage": {"input_tokens":5,"output_tokens":3,"total_tokens":8,"output_tokens_details":{"reasoning_tokens":4}}
	}`
	out := translateResponsesToChatCompletion([]byte(body))
	var c struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int64 `json:"prompt_tokens"`
			CompletionTokens        int64 `json:"completion_tokens"`
			TotalTokens             int64 `json:"total_tokens"`
			CompletionTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("chat body not JSON: %v (%s)", err, out)
	}
	if c.Object != "chat.completion" {
		t.Errorf("object = %q", c.Object)
	}
	if len(c.Choices) != 1 || c.Choices[0].FinishReason != "stop" {
		t.Errorf("choices = %+v", c.Choices)
	}
	msg := c.Choices[0].Message
	if msg.Content != "hi" || msg.ReasoningContent != "think" {
		t.Errorf("message = %+v", msg)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_abc" ||
		msg.ToolCalls[0].Function.Name != "get_weather" || msg.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool_calls = %+v", msg.ToolCalls)
	}
	if c.Usage == nil || c.Usage.PromptTokens != 5 || c.Usage.CompletionTokens != 3 || c.Usage.TotalTokens != 8 ||
		c.Usage.CompletionTokensDetails == nil || c.Usage.CompletionTokensDetails.ReasoningTokens != 4 {
		t.Errorf("usage = %+v", c.Usage)
	}
}

func TestTranslateResponsesToChatCompletionStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"completed", "stop"},
		{"incomplete", "length"},
		{"failed", "stop"},
		{"", "stop"},
	} {
		body := fmt.Sprintf(`{"id":"resp_1","status":%q,"output":[{"type":"message","role":"assistant","content":"hi"}]}`, tc.status)
		out := translateResponsesToChatCompletion([]byte(body))
		var c struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(out, &c); err != nil {
			t.Fatalf("status %q: output not JSON: %v (%s)", tc.status, err, out)
		}
		if got := c.Choices[0].FinishReason; got != tc.want {
			t.Errorf("status %q → finish_reason %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestTranslateResponsesToChatCompletionReasoningForms(t *testing.T) {
	// summary_text joins into reasoning_content; encrypted_content is surfaced
	// verbatim via its ciphertext; an unparseable summary degrades to empty.
	body := `{"id":"resp_1","status":"completed","output":[
	  {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"a"},{"type":"summary_text","text":"b"}]},
	  {"type":"reasoning","id":"rs_2","summary":[{"type":"encrypted_content","ciphertext":"abc123","iv":"x","tag":"y"}]},
	  {"type":"reasoning","id":"rs_3","summary":"not-an-array"},
	  {"type":"message","role":"assistant","content":"hi"}
	]}`
	out := translateResponsesToChatCompletion([]byte(body))
	var c struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, out)
	}
	msg := c.Choices[0].Message
	if msg.Content != "hi" {
		t.Errorf("content = %q, want hi (content survives reasoning degradation)", msg.Content)
	}
	if msg.ReasoningContent != "ababc123" {
		t.Errorf("reasoning_content = %q, want ababc123", msg.ReasoningContent)
	}
}

func TestTranslateResponsesToChatCompletionPassthrough(t *testing.T) {
	body := []byte(`not json`)
	if got := translateResponsesToChatCompletion(body); string(got) != string(body) {
		t.Errorf("unparseable body changed: %s", got)
	}
}

// ---- response round trip through the client shim ----

func TestResponsesResponseTranslationRoundTrip(t *testing.T) {
	responsesBody := `{
	  "id": "resp_1",
	  "object": "response",
	  "status": "completed",
	  "model": "zen-x",
	  "output": [
	    {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]},
	    {"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]},
	    {"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}
	  ],
	  "usage": {"input_tokens":5,"output_tokens":3,"total_tokens":8,"output_tokens_details":{"reasoning_tokens":4}}
	}`
	chatBody := translateResponsesToChatCompletion([]byte(responsesBody))
	back := chatCompletionToResponse("resp_x", chatBody)
	var v struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(back, &v); err != nil {
		t.Fatalf("responses body not JSON: %v (%s)", err, back)
	}
	if v.Status != "completed" {
		t.Errorf("status = %q", v.Status)
	}
	if len(v.Output) != 3 {
		t.Fatalf("output len = %d, want 3: %s", len(v.Output), back)
	}
	if v.Output[0].Type != "reasoning" || len(v.Output[0].Summary) != 1 || v.Output[0].Summary[0].Text != "think" {
		t.Errorf("reasoning output = %+v", v.Output[0])
	}
	if v.Output[1].Type != "message" || len(v.Output[1].Content) != 1 || v.Output[1].Content[0].Text != "hi" {
		t.Errorf("message output = %+v", v.Output[1])
	}
	fc := v.Output[2]
	if fc.Type != "function_call" || fc.CallID != "call_abc" || fc.Name != "get_weather" || fc.Arguments != `{"city":"Paris"}` {
		t.Errorf("function_call output = %+v", fc)
	}
	if v.Usage.InputTokens != 5 || v.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", v.Usage)
	}
}

// ---- non-stream E2E against a fake /responses upstream ----

func TestResponsesStyleNonStreamChatE2E(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			provider.t.Errorf("responses upstream path = %q, want /responses suffix", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"zen-x","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking..."}]},{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`))
	})
	toml := fmt.Sprintf(`
[settings]
default_alias = "resp"

[providers.responses]
base_url = %q
style = "responses"
session_header = "x-opencode-session"
identity_headers = { "X-Opencode-Client" = "cli", "X-Opencode-Project" = "global" }
models = ["zen-x"]

[[instances]]
alias = "resp"
template = "responses"
api_key_env = "TEST_KEY_1"
`, provider.url()+"/v1")
	gs, st := newResponsesGateway(t, toml, defaultEnv)

	resp := postChatWithUA(t, gs, `{"model":"resp/zen-x","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"max_tokens":128,"reasoning_effort":"high"}`, map[string]string{
		"X-Session-Id": "sess-resp-e2e",
		"User-Agent":   "my-client/1.0",
	}, true)
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var c struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("client body not a chat completion: %v (%s)", err, body)
	}
	if c.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", c.Object)
	}
	if len(c.Choices) != 1 || c.Choices[0].Message.Content != "Hello" || c.Choices[0].Message.ReasoningContent != "thinking..." || c.Choices[0].FinishReason != "stop" {
		t.Errorf("choices = %+v", c.Choices)
	}
	if c.Usage.PromptTokens != 5 || c.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v", c.Usage)
	}

	// The upstream saw a Responses request at <base>/responses, translated
	// from the chat body (instructions hoisted, max_output_tokens mapped,
	// prompt_cache_key = the effective session id).
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	var up struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Input        []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"input"`
		MaxOutputTokens int `json:"max_output_tokens"`
		Reasoning       struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Stream         any    `json:"stream"`
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(seen[0].Body, &up); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, seen[0].Body)
	}
	if up.Model != "zen-x" || up.Instructions != "be brief" {
		t.Errorf("upstream model/instructions = %q/%q", up.Model, up.Instructions)
	}
	if len(up.Input) != 1 || up.Input[0].Type != "message" || up.Input[0].Role != "user" || up.Input[0].Content != "hi" {
		t.Errorf("upstream input = %+v", up.Input)
	}
	if up.MaxOutputTokens != 128 {
		t.Errorf("max_output_tokens = %d, want 128", up.MaxOutputTokens)
	}
	if up.Reasoning.Effort != "high" {
		t.Errorf("reasoning.effort = %q, want high", up.Reasoning.Effort)
	}
	if up.Stream != nil {
		t.Errorf("stream should be absent on a non-stream request: %s", seen[0].Body)
	}
	if up.PromptCacheKey != "sess-resp-e2e" {
		t.Errorf("prompt_cache_key = %q, want sess-resp-e2e", up.PromptCacheKey)
	}

	// Phase 1 identity/session/UA headers still ride the responses branch.
	if seen[0].Authorization != "Bearer sk-account-1" {
		t.Errorf("authorization = %q, want Bearer sk-account-1", seen[0].Authorization)
	}
	if seen[0].XOpencode != "sess-resp-e2e" {
		t.Errorf("x-opencode-session = %q, want sess-resp-e2e", seen[0].XOpencode)
	}
	if seen[0].XOpencodeClient != "cli" || seen[0].XOpencodeProject != "global" {
		t.Errorf("identity headers = %q/%q, want cli/global", seen[0].XOpencodeClient, seen[0].XOpencodeProject)
	}
	if seen[0].UserAgent != "my-client/1.0" {
		t.Errorf("user-agent = %q, want my-client/1.0", seen[0].UserAgent)
	}

	// Capture stays chat-shaped (translation happened at the upstream
	// boundary only).
	req := waitForRequest(t, st, "sess-resp-e2e", 5*time.Second)
	if req.Endpoint != "/v1/chat/completions" {
		t.Errorf("endpoint = %q, want /v1/chat/completions", req.Endpoint)
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
	if req.FinishReason != "stop" || req.StatusCode != http.StatusOK {
		t.Errorf("status/finish = %d/%q, want 200/stop", req.StatusCode, req.FinishReason)
	}
	if req.Error != nil {
		t.Errorf("error = %+v", req.Error)
	}
}

func TestResponsesSurfaceResponsesUpstreamE2E(t *testing.T) {
	// A Codex-style client hits /v1/responses; the template's responses style
	// is inherited automatically, so the upstream sees a Responses body and
	// the client gets a Responses object.
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			provider.t.Errorf("responses upstream path = %q, want /responses suffix", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"zen-x","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`))
	})
	toml := fmt.Sprintf(`
[settings]
default_alias = "resp"

[providers.responses]
base_url = %q
style = "responses"
models = ["zen-x"]

[[instances]]
alias = "resp"
template = "responses"
api_key_env = "TEST_KEY_1"
`, provider.url()+"/v1")
	gs, st := newResponsesGateway(t, toml, defaultEnv)

	resp := postResponses(t, gs, `{"model":"resp/zen-x","input":"hi","max_output_tokens":128}`, map[string]string{"X-Session-Id": "sess-codex"})
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
	if v.Usage.InputTokens != 5 || v.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", v.Usage)
	}

	// The upstream saw a Responses request (translated from chat, which was
	// translated from the client's Responses request): the full circle.
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	var up struct {
		Model string `json:"model"`
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"input"`
		MaxOutputTokens int `json:"max_output_tokens"`
		Stream          any `json:"stream"`
	}
	if err := json.Unmarshal(seen[0].Body, &up); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, seen[0].Body)
	}
	if up.Model != "zen-x" || len(up.Input) != 1 || up.Input[0].Type != "message" || up.Input[0].Role != "user" || up.Input[0].Content != "hi" {
		t.Errorf("upstream body = %s", seen[0].Body)
	}
	if up.MaxOutputTokens != 128 {
		t.Errorf("max_output_tokens = %d, want 128", up.MaxOutputTokens)
	}
	if up.Stream != nil {
		t.Errorf("stream should be absent on a non-stream request: %s", seen[0].Body)
	}

	// Capture records the responses surface endpoint and a chat-shaped
	// response_json (the pipeline never sees Responses shapes).
	req := waitForRequest(t, st, "sess-codex", 5*time.Second)
	if req.Endpoint != "/v1/responses" {
		t.Errorf("endpoint = %q, want /v1/responses", req.Endpoint)
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
	if req.FinishReason != "stop" {
		t.Errorf("finish = %q, want stop", req.FinishReason)
	}
	if req.Error != nil {
		t.Errorf("error = %+v", req.Error)
	}
}

// ---- streaming translation ----

func TestResponsesUpstreamStreamTranslation(t *testing.T) {
	events := []string{
		`{"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1722600000,"status":"in_progress","model":"zen-x"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"hmm"}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":" done"}`,
		`{"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"hmm done"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"hmm done"}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","item_id":"msg_1","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"Hel"}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"lo"}`,
		`{"type":"response.output_text.done","item_id":"msg_1","output_index":1,"content_index":0,"text":"Hello"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello","annotations":[]}]}}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"","status":"in_progress"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"ci"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"ty\":\"Paris\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":2,"arguments":"{\"city\":\"Paris\"}"}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1722600000,"status":"completed","model":"zen-x","output":[],"usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14,"output_tokens_details":{"reasoning_tokens":4}}}}`,
	}

	st := &responsesUpstreamStreamState{toolItemIndex: map[string]int{}}
	asm := newAssembler()
	var got [][]byte
	for _, e := range events {
		for _, ch := range st.translate([]byte(e)) {
			got = append(got, ch)
			asm.add(ch)
		}
	}

	// The first chunk seeds id/object/role.
	var first struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Choices []struct {
			Delta struct {
				Role string `json:"role"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(got[0], &first); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if first.ID != "resp_1" || first.Object != "chat.completion.chunk" || first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk = %s", got[0])
	}

	// Reasoning deltas map to reasoning_content, text to content, tool
	// argument deltas concatenate per index.
	var sawReasoning, sawText string
	for _, ch := range got {
		var c struct {
			Choices []struct {
				Delta struct {
					ReasoningContent *string `json:"reasoning_content"`
					Content          *string `json:"content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(ch, &c) != nil {
			continue
		}
		if len(c.Choices) == 0 {
			continue
		}
		d := c.Choices[0].Delta
		if d.ReasoningContent != nil {
			sawReasoning += *d.ReasoningContent
		}
		if d.Content != nil {
			sawText += *d.Content
		}
	}
	if sawReasoning != "hmm done" {
		t.Errorf("reasoning = %q, want %q", sawReasoning, "hmm done")
	}
	if sawText != "Hello" {
		t.Errorf("text = %q, want Hello", sawText)
	}

	// The reassembled completion carries content + tool_calls + finish + usage.
	reassembled, finish, usage := asm.result()
	var comp struct {
		Choices []struct {
			Message struct {
				Content   any   `json:"content"`
				ToolCalls []any `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(reassembled, &comp); err != nil {
		t.Fatalf("reassembled: %v (%s)", err, reassembled)
	}
	if len(comp.Choices) != 1 || comp.Choices[0].Message.Content != "Hello" {
		t.Errorf("reassembled choices = %+v", comp.Choices)
	}
	if len(comp.Choices[0].Message.ToolCalls) != 1 {
		t.Errorf("reassembled tool_calls = %v", comp.Choices[0].Message.ToolCalls)
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	if !strings.Contains(string(usage), `"completion_tokens":9`) {
		t.Errorf("usage = %s", usage)
	}
	if !strings.Contains(string(usage), `"reasoning_tokens":4`) {
		t.Errorf("usage should carry reasoning_tokens: %s", usage)
	}
	if !st.done {
		t.Errorf("response.completed should mark the stream done")
	}
}

func TestResponsesUpstreamStreamErrorAndIncomplete(t *testing.T) {
	// A response.failed / top-level error payload becomes the canonical OpenAI
	// error chunk the assembler records.
	st := &responsesUpstreamStreamState{toolItemIndex: map[string]int{}}
	errChunks := st.translate([]byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"upstream died"}}}`))
	if len(errChunks) != 1 {
		t.Fatalf("error chunks = %v", errChunks)
	}
	var ev struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(errChunks[0], &ev); err != nil || len(ev.Error) == 0 {
		t.Errorf("error chunk = %s", errChunks[0])
	}
	asm := newAssembler()
	asm.add(errChunks[0])
	if err := asm.streamError(); err == nil || err.Message != "upstream died" {
		t.Errorf("streamError = %+v", err)
	}

	// A top-level {"error":...} payload (no type field) is handled the same.
	st2 := &responsesUpstreamStreamState{toolItemIndex: map[string]int{}}
	errChunks2 := st2.translate([]byte(`{"error":{"message":"busy","type":"server_error","code":null}}`))
	if len(errChunks2) != 1 {
		t.Fatalf("top-level error chunks = %v", errChunks2)
	}

	// status "incomplete" → finish_reason "length".
	st3 := &responsesUpstreamStreamState{toolItemIndex: map[string]int{}}
	chunks := st3.translate([]byte(`{"type":"response.created","response":{"id":"resp_1","model":"zen-x"}}`))
	chunks = append(chunks, st3.translate([]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"incomplete","model":"zen-x","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`))...)
	var last struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunks[len(chunks)-1], &last); err != nil {
		t.Fatalf("completed chunk: %v (%s)", err, chunks[len(chunks)-1])
	}
	if got := last.Choices[0].FinishReason; got != "length" {
		t.Errorf("finish_reason = %q, want length", got)
	}
}

// chatChunkPayloads splits a chat SSE body into its `data:` payloads.
func chatChunkPayloads(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(line, "data: ") {
			p := strings.TrimPrefix(line, "data: ")
			if p != "[DONE]" {
				out = append(out, p)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	return out
}

func TestResponsesStyleStreamingChatE2E(t *testing.T) {
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			provider.t.Errorf("responses upstream path = %q, want /responses suffix", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, line := range []string{
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1722600000,"status":"in_progress","model":"zen-x"}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
			`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"thinking..."}`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}`,
			`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"Hello "}`,
			`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"world"}`,
			`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"lookup","arguments":"","status":"in_progress"}}`,
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"q\":\"x\"}"}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1722600000,"status":"completed","model":"zen-x","output":[],"usage":{"input_tokens":5,"output_tokens":8,"total_tokens":13,"output_tokens_details":{"reasoning_tokens":4}}}}`,
		} {
			fmt.Fprintf(w, "%s\n\n", line)
			fl.Flush()
		}
	})
	toml := fmt.Sprintf(`
[settings]
default_alias = "resp"

[providers.responses]
base_url = %q
style = "responses"
session_header = "x-opencode-session"
identity_headers = { "X-Opencode-Client" = "cli", "X-Opencode-Project" = "global" }
models = ["zen-x"]

[[instances]]
alias = "resp"
template = "responses"
api_key_env = "TEST_KEY_1"
`, provider.url()+"/v1")
	gs, st := newResponsesGateway(t, toml, defaultEnv)

	resp := postChat(t, gs, `{"model":"resp/zen-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-resp-stream"})
	body := drainClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", got)
	}

	// The live chat SSE body keeps its conventional contract: the clean end
	// synthesizes a terminating data: [DONE] line (this style has no upstream
	// [DONE] sentinel), mirroring readAnthropicStream.
	if !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Errorf("live stream must end with data: [DONE]\\n\\n, got: %q", body)
	}

	// The chat client receives ordered chat.completion.chunk lines with
	// reasoning surfaced and usage/finish folded into the final chunk.
	payloads := chatChunkPayloads(t, body)
	var sawReasoning, sawText, finalUsage string
	var finish string
	for _, p := range payloads {
		var c struct {
			Choices []struct {
				Delta struct {
					ReasoningContent *string `json:"reasoning_content"`
					Content          *string `json:"content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(p), &c); err != nil {
			t.Fatalf("client chunk not JSON: %v (%s)", err, p)
		}
		if len(c.Choices) == 0 {
			continue
		}
		d := c.Choices[0].Delta
		if d.ReasoningContent != nil {
			sawReasoning += *d.ReasoningContent
		}
		if d.Content != nil {
			sawText += *d.Content
		}
		if c.Choices[0].FinishReason != nil {
			if fr, ok := c.Choices[0].FinishReason.(string); ok {
				finish = fr
			}
		}
		if len(c.Usage) > 0 {
			finalUsage = string(c.Usage)
		}
	}
	if sawReasoning != "thinking..." {
		t.Errorf("client reasoning = %q", sawReasoning)
	}
	if sawText != "Hello world" {
		t.Errorf("client text = %q", sawText)
	}
	if finish != "stop" {
		t.Errorf("client finish_reason = %q, want stop", finish)
	}
	if !strings.Contains(finalUsage, `"completion_tokens":8`) || !strings.Contains(finalUsage, `"prompt_tokens":5`) {
		t.Errorf("client final usage = %s", finalUsage)
	}

	// The upstream saw a translated Responses body with stream:true and the
	// Phase 1 identity/session/UA headers still riding the branch.
	seen := provider.requests()
	if len(seen) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(seen))
	}
	var up struct {
		Stream         any    `json:"stream"`
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(seen[0].Body, &up); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, seen[0].Body)
	}
	if up.Stream != true {
		t.Errorf("upstream stream = %v, want true", up.Stream)
	}
	if up.PromptCacheKey != "sess-resp-stream" {
		t.Errorf("prompt_cache_key = %q, want sess-resp-stream", up.PromptCacheKey)
	}
	if seen[0].XOpencode != "sess-resp-stream" {
		t.Errorf("x-opencode-session = %q, want sess-resp-stream", seen[0].XOpencode)
	}
	if seen[0].XOpencodeClient != "cli" || seen[0].XOpencodeProject != "global" {
		t.Errorf("identity headers = %q/%q, want cli/global", seen[0].XOpencodeClient, seen[0].XOpencodeProject)
	}
	if seen[0].UserAgent != "Go-http-client/1.1" {
		// postChat sends Go's default UA, faithfully forwarded.
		t.Errorf("user-agent = %q, want Go-http-client/1.1", seen[0].UserAgent)
	}

	// Capture folds only content/tool_calls/usage/finish (reasoning stays
	// client-visible but not in the captured response_json) and records a
	// clean end (not truncated).
	req := waitForRequest(t, st, "sess-resp-stream", 5*time.Second)
	if req.Truncated {
		t.Errorf("truncated = true, want a clean end")
	}
	if req.FinishReason != "stop" || req.StatusCode != http.StatusOK {
		t.Errorf("status/finish = %d/%q, want 200/stop", req.StatusCode, req.FinishReason)
	}
	if req.Error != nil {
		t.Errorf("error = %+v", req.Error)
	}
	if !strings.Contains(string(req.ResponseJSON), `"chat.completion"`) {
		t.Errorf("response_json should stay chat-shaped: %q", req.ResponseJSON)
	}
	if !strings.Contains(string(req.ResponseJSON), `"content":"Hello world"`) {
		t.Errorf("response_json should carry the content: %q", req.ResponseJSON)
	}
	if strings.Contains(string(req.ResponseJSON), "reasoning_content") {
		t.Errorf("response_json should not fold reasoning_content: %q", req.ResponseJSON)
	}
	if req.Usage == nil || !strings.Contains(string(req.Usage), `"completion_tokens":8`) {
		t.Errorf("captured usage = %s", req.Usage)
	}
}

func TestResponsesStyleStreamingDisconnectTruncated(t *testing.T) {
	// A stream that dies mid-flight without response.completed records
	// truncated (inherited from readStream/readAnthropicStream semantics). A
	// graceful upstream close is io.EOF — the plan's accepted fallback clean
	// end — so this test resets the connection abruptly (SO_LINGER=0), which
	// surfaces as a non-EOF read error and must be truncated.
	var provider *fakeProvider
	provider = newFakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"zen-x\"}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"partial\"}\n\n")
		fl.Flush()
		// Abruptly reset the connection: no graceful EOF, no
		// response.completed.
		hj := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		conn.Close()
	})
	toml := fmt.Sprintf(`
[settings]
default_alias = "resp"

[providers.responses]
base_url = %q
style = "responses"
models = ["zen-x"]

[[instances]]
alias = "resp"
template = "responses"
api_key_env = "TEST_KEY_1"
`, provider.url()+"/v1")
	gs, st := newResponsesGateway(t, toml, defaultEnv)

	resp := postChat(t, gs, `{"model":"resp/zen-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"X-Session-Id": "sess-resp-trunc"})
	body := drainClose(t, resp)

	req := waitForRequest(t, st, "sess-resp-trunc", 5*time.Second)
	if !req.Truncated {
		t.Errorf("truncated = false, want a truncated stream (no response.completed)")
	}
	// A truncated stream must NOT emit [DONE] (inherited from
	// readStream/readAnthropicStream truncation semantics).
	if strings.Contains(string(body), "data: [DONE]") {
		t.Errorf("truncated stream must not emit data: [DONE], got: %q", body)
	}
}

// ---- io.EOF fallback end is a clean end ----

func TestResponsesUpstreamReadEOFIsCleanEnd(t *testing.T) {
	// The stream ends at io.EOF without response.completed: accepted as a
	// clean end (the plan's fallback), so it is not recorded truncated, and
	// the terminating [DONE] is synthesized for the chat surface.
	asm := newAssembler()
	var forwarded []string
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"zen-x\"}}\n\n")),
	}
	out := readResponsesStream(context.Background(), resp, asm, func(line []byte) error {
		forwarded = append(forwarded, string(line))
		return nil
	})
	if out.truncated {
		t.Errorf("truncated = true, want clean end on io.EOF")
	}
	if !out.cleanEnd {
		t.Errorf("cleanEnd = false, want true on io.EOF")
	}
	if len(forwarded) != 2 || forwarded[1] != "data: [DONE]\n\n" {
		t.Errorf("forwarded lines = %q, want 2 lines ending in %q", forwarded, "data: [DONE]\n\n")
	}
}
