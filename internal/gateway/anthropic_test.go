package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------- request translation ----------

func TestTranslateOpenAIToAnthropic(t *testing.T) {
	body := `{
	  "model": "claude-sonnet-4",
	  "stream": true,
	  "max_tokens": 1000,
	  "temperature": 0.5,
	  "stop": ["END"],
	  "reasoning_effort": "high",
	  "tools": [{"type": "function", "function": {"name": "get_weather", "description": "weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}],
	  "tool_choice": "auto",
	  "messages": [
	    {"role": "system", "content": "You are helpful."},
	    {"role": "system", "content": "Be brief."},
	    {"role": "user", "content": "Hi"},
	    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}]},
	    {"role": "tool", "tool_call_id": "call_1", "content": "Sunny"},
	    {"role": "user", "content": [{"type": "text", "text": "Thanks"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}]}
	  ]
	}`
	out, err := translateOpenAIToAnthropic([]byte(body))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if v["model"] != "claude-sonnet-4" {
		t.Errorf("model = %v", v["model"])
	}
	if v["system"] != "You are helpful.\n\nBe brief." {
		t.Errorf("system = %q", v["system"])
	}
	if v["max_tokens"] != float64(1000) {
		t.Errorf("max_tokens = %v", v["max_tokens"])
	}
	if v["stream"] != true {
		t.Errorf("stream = %v", v["stream"])
	}
	if _, ok := v["temperature"]; !ok || v["temperature"] != 0.5 {
		t.Errorf("temperature = %v", v["temperature"])
	}
	thinking, ok := v["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16384) {
		t.Errorf("thinking = %v", v["thinking"])
	}
	stops, ok := v["stop_sequences"].([]any)
	if !ok || len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop_sequences = %v", v["stop_sequences"])
	}
	tools, ok := v["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", v["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Errorf("tool name = %v", tool["name"])
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Errorf("tool missing input_schema: %v", tool)
	}
	if v["tool_choice"].(map[string]any)["type"] != "auto" {
		t.Errorf("tool_choice = %v", v["tool_choice"])
	}

	msgs, ok := v["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %v", v["messages"])
	}
	// 3 messages: user, assistant(tool_use), then the merged user turn that
	// carries the tool_result plus the follow-up text/image content.
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3: %v", len(msgs), msgs)
	}
	// assistant message carries the tool_use block
	assistant := msgs[1].(map[string]any)
	blocks, ok := assistant["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("assistant content = %v", assistant["content"])
	}
	tu := blocks[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "get_weather" {
		t.Errorf("tool_use block = %v", tu)
	}
	if input := tu["input"].(map[string]any); input["city"] != "Paris" {
		t.Errorf("tool_use input = %v", tu["input"])
	}
	// tool result and the following user turn merged into one user message:
	// tool_result + text + image blocks
	mergedUser := msgs[2].(map[string]any)
	if mergedUser["role"] != "user" {
		t.Errorf("merged role = %v", mergedUser["role"])
	}
	userBlocks, ok := mergedUser["content"].([]any)
	if !ok || len(userBlocks) != 3 {
		t.Fatalf("merged user content = %v", mergedUser["content"])
	}
	tr := userBlocks[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" {
		t.Errorf("tool_result block = %v", tr)
	}
	if text := userBlocks[1].(map[string]any); text["type"] != "text" || text["text"] != "Thanks" {
		t.Errorf("text block = %v", userBlocks[1])
	}
	img := userBlocks[2].(map[string]any)
	if img["type"] != "image" {
		t.Errorf("image block = %v", img)
	}
	src := img["content"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AAAA" {
		t.Errorf("image source = %v", img["content"])
	}
}

func TestTranslateOpenAIToAnthropicDefaultsAndMerge(t *testing.T) {
	// No max_tokens → default; consecutive same-role messages merged; empty
	// assistant turn dropped.
	body := `{"model":"m","messages":[
	  {"role":"assistant","content":""},
	  {"role":"user","content":"a"},
	  {"role":"user","content":"b"},
	  {"role":"assistant","content":"one"},
	  {"role":"assistant","content":"two"}
	]}`
	out, err := translateOpenAIToAnthropic([]byte(body))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v map[string]any
	json.Unmarshal(out, &v)
	if v["max_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Errorf("max_tokens = %v, want default %d", v["max_tokens"], defaultAnthropicMaxTokens)
	}
	msgs := v["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	if m := msgs[1].(map[string]any); m["role"] != "assistant" {
		t.Errorf("last role = %v", m["role"])
	}
	if _, ok := v["thinking"]; ok {
		t.Errorf("thinking set without reasoning_effort: %v", v["thinking"])
	}
}

func TestTranslateToolChoiceFunctionObject(t *testing.T) {
	out, err := translateOpenAIToAnthropic([]byte(`{"model":"m","tool_choice":{"type":"function","function":{"name":"f"}},"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v map[string]any
	json.Unmarshal(out, &v)
	tc := v["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "f" {
		t.Errorf("tool_choice = %v", v["tool_choice"])
	}
}

// ---------- non-stream response translation ----------

func TestTranslateAnthropicToOpenAI(t *testing.T) {
	body := `{
	  "id": "msg_abc",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-sonnet-4",
	  "content": [
	    {"type": "thinking", "thinking": "let me think"},
	    {"type": "text", "text": "Hello"},
	    {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}}
	  ],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 12, "output_tokens": 34}
	}`
	out, err := translateAnthropicToOpenAI([]byte(body))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var v struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          any    `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []any  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if v.Object != "chat.completion" {
		t.Errorf("object = %q", v.Object)
	}
	if len(v.Choices) != 1 {
		t.Fatalf("choices = %v", v.Choices)
	}
	ch := v.Choices[0]
	if ch.Message.Content != "Hello" {
		t.Errorf("content = %v", ch.Message.Content)
	}
	if ch.Message.ReasoningContent != "let me think" {
		t.Errorf("reasoning_content = %q", ch.Message.ReasoningContent)
	}
	if len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %v", ch.Message.ToolCalls)
	}
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", ch.FinishReason)
	}
	if v.Usage.PromptTokens != 12 || v.Usage.CompletionTokens != 34 || v.Usage.TotalTokens != 46 {
		t.Errorf("usage = %+v", v.Usage)
	}
}

func TestTranslateAnthropicToOpenAIStopReasons(t *testing.T) {
	for reason, want := range map[string]string{
		"end_turn":       "stop",
		"max_tokens":     "length",
		"stop_sequence":  "stop",
		"tool_use":       "tool_calls",
		"refusal":        "refusal",
		"something_else": "stop",
	} {
		out, err := translateAnthropicToOpenAI([]byte(`{"content":[{"type":"text","text":"x"}],"stop_reason":"` + reason + `","usage":{"input_tokens":1,"output_tokens":1}}`))
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		var v struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		json.Unmarshal(out, &v)
		if got := v.Choices[0].FinishReason; got != want {
			t.Errorf("stop_reason %q → finish_reason %q, want %q", reason, got, want)
		}
	}
}

func TestTranslateAnthropicError(t *testing.T) {
	in := `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`
	out := translateAnthropicError([]byte(in))
	var v struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if v.Error.Message != "bad request" || v.Error.Type != "invalid_request_error" {
		t.Errorf("error = %+v", v.Error)
	}
	// Non-Anthropic bodies pass through unchanged.
	openAIErr := []byte(`{"error":{"message":"boom","code":"X"}}`)
	if got := translateAnthropicError(openAIErr); string(got) != string(openAIErr) {
		t.Errorf("non-anthropic error mangled: %s", got)
	}
}

// ---------- streaming translation ----------

func TestAnthropicStreamTranslation(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" done"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"ty\":\"Paris\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}

	st := &anthropicStreamState{blockToolIndex: map[int]int{}}
	asm := newAssembler()
	var got [][]byte
	for _, e := range events {
		for _, ch := range st.translate([]byte(e)) {
			got = append(got, ch)
			asm.add(ch)
		}
	}

	// The first chunk seeds id/object/role; the last chunk is [DONE].
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
	if first.ID != "msg_1" || first.Object != "chat.completion.chunk" || first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk = %s", got[0])
	}
	if string(got[len(got)-1]) != "[DONE]" {
		t.Errorf("last payload = %s, want [DONE]", got[len(got)-1])
	}

	// Reasoning deltas map to reasoning_content.
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
		for _, tc := range d.ToolCalls {
			if tc.ID == "toolu_1" && tc.Function.Name != "get_weather" {
				t.Errorf("tool_use chunk name = %q", tc.Function.Name)
			}
		}
	}
	if sawReasoning != "hmm done" {
		t.Errorf("reasoning = %q", sawReasoning)
	}
	if sawText != "Hello" {
		t.Errorf("text = %q", sawText)
	}

	// The reassembled completion carries content + tool_calls + finish_reason.
	reassembled, finish, _ := asm.result()
	var comp struct {
		Choices []struct {
			Message struct {
				Content   any   `json:"content"`
				ToolCalls []any `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(reassembled, &comp); err != nil {
		t.Fatalf("reassembled: %v", err)
	}
	if len(comp.Choices) != 1 {
		t.Fatalf("reassembled choices = %v", comp.Choices)
	}
	if comp.Choices[0].Message.Content != "Hello" {
		t.Errorf("reassembled content = %v", comp.Choices[0].Message.Content)
	}
	if len(comp.Choices[0].Message.ToolCalls) != 1 {
		t.Errorf("reassembled tool_calls = %v", comp.Choices[0].Message.ToolCalls)
	}
	if comp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("reassembled finish_reason = %q", comp.Choices[0].FinishReason)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q", finish)
	}
	if !strings.Contains(string(comp.Usage), `"completion_tokens":9`) {
		t.Errorf("usage = %s", comp.Usage)
	}
}

func TestAnthropicStreamEmptyAndError(t *testing.T) {
	// A stream with only thinking (no text, no tool_use) reassembles to an
	// empty completion — what isEmptyCompletion flags for retry_empty.
	st := &anthropicStreamState{blockToolIndex: map[int]int{}}
	asm := newAssembler()
	for _, e := range []string{
		`{"type":"message_start","message":{"id":"m","model":"x"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"..."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	} {
		for _, ch := range st.translate([]byte(e)) {
			asm.add(ch)
		}
	}
	reassembled, finish, _ := asm.result()
	if !isEmptyCompletion(reassembled) {
		t.Errorf("thinking-only reassembly should be empty: %s", reassembled)
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}

	// An SSE error event becomes an OpenAI-shaped error chunk the assembler
	// records.
	st2 := &anthropicStreamState{blockToolIndex: map[int]int{}}
	errChunks := st2.translate([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	if len(errChunks) != 1 {
		t.Fatalf("error chunks = %v", errChunks)
	}
	var ev struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(errChunks[0], &ev); err != nil || len(ev.Error) == 0 {
		t.Errorf("error chunk = %s", errChunks[0])
	}
	asm2 := newAssembler()
	asm2.add(errChunks[0])
	if err := asm2.streamError(); err == nil || err.Message != "busy" {
		t.Errorf("streamError = %+v", err)
	}
}
