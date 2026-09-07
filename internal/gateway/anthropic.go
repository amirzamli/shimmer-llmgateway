package gateway

// Anthropic-style templates speak the Anthropic Messages API
// (<base_url>/messages, x-api-key auth) instead of OpenAI chat/completions.
// This file translates between the OpenAI chat format the gateway and its
// clients speak and the Anthropic wire format, in both directions and for
// both streaming and non-streaming responses:
//
//   - translateOpenAIToAnthropic: client body → Anthropic messages request
//     (system hoisted to the top-level field, tool definitions/choices mapped,
//     tool results become user messages with tool_result blocks, consecutive
//     same-role messages merged, max_tokens defaulted, reasoning_effort mapped
//     to extended thinking, image parts mapped to image blocks with base64 or
//     URL sources and unsupported ones skipped with a warn log);
//   - translateAnthropicToOpenAI: non-stream messages response → chat.completion
//     (thinking → reasoning_content, tool_use blocks → tool_calls);
//   - translateAnthropicError: Anthropic error bodies → OpenAI error shape;
//   - readAnthropicStream: SSE translation with a stateful event translator
//     (message_start → role chunk, text/thinking/input_json deltas → content /
//     reasoning_content / tool_call argument chunks, message_delta stop_reason
//     → finish_reason chunk, message_stop → [DONE]).

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/logging"
	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// anthropicVersionHeader is the API version the gateway speaks to Anthropic
// Messages endpoints.
const anthropicVersionHeader = "2023-06-01"

// anthropicThinkingBetaHeader enables extended thinking (only sent when the
// translated request actually enables thinking, so strict proxies that reject
// unknown beta values stay compatible).
const anthropicThinkingBetaHeader = "extended-thinking-2025-02-19"

// defaultAnthropicMaxTokens is used when the client body carries no
// max_tokens/max_completion_tokens: Anthropic requires the field, OpenAI does
// not.
const defaultAnthropicMaxTokens = 4096

// thinkingBudget maps the OpenAI reasoning_effort levels to Anthropic extended
// thinking budget_tokens (minimum is 1024; 4k/8k/16k/32k are the common steps).
func thinkingBudget(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return 4096
	case "medium":
		return 8192
	case "high":
		return 16384
	case "max":
		return 32000
	}
	return 8192
}

// bodyHasThinking reports whether a translated Anthropic request enables
// extended thinking (controls the beta header).
func bodyHasThinking(body []byte) bool {
	var v struct {
		Thinking json.RawMessage `json:"thinking"`
	}
	_ = json.Unmarshal(body, &v)
	return len(v.Thinking) > 0
}

// ---------- request translation ----------

// openAIMsg is the OpenAI message subset the translator understands.
type openAIMsg struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCallID string           `json:"tool_call_id"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
}

// openAIToolCall is one assistant tool invocation in an OpenAI message.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// openAITool is one function definition in the OpenAI tools array.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// anthropicBlock is one Anthropic content block (text / image / tool_use /
// tool_result). Image blocks carry the source object (base64 or URL
// reference); tool_result blocks carry the string-or-block-array Content.
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// translateOpenAIToAnthropic converts an OpenAI chat.completions body into an
// Anthropic messages body. Anything the Anthropic API does not accept (extra
// fields like presence_penalty or logprobs) is deliberately dropped — the
// translated body carries only known Anthropic fields. The logger (nil in
// unit tests) receives a warn per dropped image part.
func translateOpenAIToAnthropic(body []byte, logger *logging.Logger) ([]byte, error) {
	var req struct {
		Model               string          `json:"model"`
		Messages            []openAIMsg     `json:"messages"`
		Tools               []openAITool    `json:"tools"`
		ToolChoice          json.RawMessage `json:"tool_choice"`
		Temperature         *float64        `json:"temperature"`
		TopP                *float64        `json:"top_p"`
		MaxTokens           *int            `json:"max_tokens"`
		MaxCompletionTokens *int            `json:"max_completion_tokens"`
		Stream              bool            `json:"stream"`
		Stop                json.RawMessage `json:"stop"`
		ReasoningEffort     string          `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}

	system, messages := translateMessages(logger, req.Messages)

	out := map[string]any{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": maxTokensOf(req.MaxTokens, req.MaxCompletionTokens),
	}
	if system != "" {
		out["system"] = system
	}
	if len(req.Tools) > 0 {
		out["tools"] = translateTools(req.Tools)
	}
	if tc := translateToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.Stream {
		out["stream"] = true
	}
	if len(req.Stop) > 0 {
		if seqs := translateStop(req.Stop); len(seqs) > 0 {
			out["stop_sequences"] = seqs
		}
	}
	if req.ReasoningEffort != "" {
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": thinkingBudget(req.ReasoningEffort)}
	}
	return json.Marshal(out)
}

// maxTokensOf picks the OpenAI max_tokens / max_completion_tokens values and
// defaults to defaultAnthropicMaxTokens when both are absent (Anthropic
// requires the field).
func maxTokensOf(maxTokens, maxCompletionTokens *int) int {
	if maxTokens != nil {
		return *maxTokens
	}
	if maxCompletionTokens != nil {
		return *maxCompletionTokens
	}
	return defaultAnthropicMaxTokens
}

// translateMessages converts OpenAI messages to Anthropic messages: system
// messages are hoisted into the returned top-level system string (joined with
// blank lines), tool results become user messages carrying tool_result blocks,
// and consecutive same-role messages are merged (the Anthropic API requires
// strictly alternating user/assistant roles).
func translateMessages(logger *logging.Logger, messages []openAIMsg) (string, []anthropicMessage) {
	var systems []string
	converted := make([]anthropicMessage, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case "system":
			if t := contentText(m.Content); t != "" {
				systems = append(systems, t)
			}
		case "assistant":
			var blocks []anthropicBlock
			if t := contentText(m.Content); t != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: t})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: toolUseInput(tc.Function.Arguments),
				})
			}
			if len(blocks) == 0 {
				// An empty assistant turn is not representable on the
				// Anthropic side; drop it.
				continue
			}
			converted = append(converted, anthropicMessage{Role: "assistant", Content: marshalBlocks(blocks)})
		case "tool":
			converted = append(converted, anthropicMessage{Role: "user", Content: marshalBlocks([]anthropicBlock{{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   marshalTextOrBlocks(logger, m.Content),
			}})})
		default: // user and anything else
			converted = append(converted, anthropicMessage{Role: "user", Content: marshalUserContent(logger, m.Content)})
		}
	}

	merged := make([]anthropicMessage, 0, len(converted))
	for _, m := range converted {
		if n := len(merged); n > 0 && merged[n-1].Role == m.Role {
			merged[n-1].Content = concatBlocks(merged[n-1].Content, m.Content)
		} else {
			merged = append(merged, m)
		}
	}
	return strings.Join(systems, "\n\n"), merged
}

// anthropicMessage is one Anthropic messages[] entry. Content is always a JSON
// block array (never a bare string) so same-role merges can concatenate.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentText extracts the plain text of an OpenAI content field (string, or
// the text of every text part in an array).
func contentText(content json.RawMessage) string {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// marshalUserContent converts an OpenAI user message content (string or part
// array) into a block array. Text parts are kept; image parts become image
// blocks when their URL is forwardable — data:image/ URIs inline as base64,
// https URLs by reference. Anything else (other schemes, malformed data URIs)
// is skipped with a warn log — never emitted as a source-less image block.
// When a part array yields no blocks at all, a placeholder text block stands
// in so the message content is never null.
func marshalUserContent(logger *logging.Logger, content json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if s == "" {
			return nil
		}
		return marshalBlocks([]anthropicBlock{{Type: "text", Text: s}})
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil
	}
	var blocks []anthropicBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			switch {
			case strings.HasPrefix(p.ImageURL.URL, "data:image/"):
				src := marshalImageSource(p.ImageURL.URL)
				if src == nil {
					// Malformed data URI (no comma/payload): skip rather
					// than send a source-less image block upstream.
					warnImageSkipped(logger, p.ImageURL.URL)
					continue
				}
				blocks = append(blocks, anthropicBlock{Type: "image", Source: src})
			case strings.HasPrefix(p.ImageURL.URL, "https://"):
				blocks = append(blocks, anthropicBlock{Type: "image", Source: marshalURLImageSource(p.ImageURL.URL)})
			default:
				warnImageSkipped(logger, p.ImageURL.URL)
			}
		}
	}
	if len(blocks) == 0 {
		// Every part was skipped or dropped (unsupported image references,
		// empty text): the placeholder keeps the content a valid block array.
		return marshalBlocks([]anthropicBlock{{Type: "text", Text: contentOmittedPlaceholder}})
	}
	return marshalBlocks(blocks)
}

// warnImageSkipped logs one dropped image part (unsupported scheme or
// malformed data URI). The logger is nil in unit tests; URLs are truncated so
// base64 data payloads never reach the log stream.
func warnImageSkipped(logger *logging.Logger, url string) {
	if logger == nil {
		return
	}
	logger.Warn("anthropic_image_part_skipped", map[string]any{"url": snippet(url, 64)})
}

// marshalImageSource converts a data:image/<type>;base64,<data> URI into an
// Anthropic base64 image source object. It returns nil for a malformed URI
// (no comma separating media type and payload); callers skip the part rather
// than emit a source-less image block.
func marshalImageSource(dataURI string) json.RawMessage {
	rest := strings.TrimPrefix(dataURI, "data:")
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil
	}
	mediaType := strings.Split(rest[:comma], ";")[0]
	if mediaType == "" {
		mediaType = "image/png"
	}
	src := map[string]string{"type": "base64", "media_type": mediaType, "data": rest[comma+1:]}
	b, _ := json.Marshal(src)
	return b
}

// marshalURLImageSource builds an Anthropic image source that references a
// public https URL.
func marshalURLImageSource(url string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": "url", "url": url})
	return b
}

// marshalTextOrBlocks renders a tool result content field: strings stay
// strings, part arrays become text/image blocks (unsupported parts skipped
// with a warn log; an all-skipped array yields the placeholder text block).
func marshalTextOrBlocks(logger *logging.Logger, content json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		b, _ := json.Marshal(s)
		return b
	}
	if b := marshalUserContent(logger, content); b != nil {
		return b
	}
	b, _ := json.Marshal("ok")
	return b
}

// toolUseInput parses an OpenAI tool_call arguments JSON string into an
// Anthropic tool_use input object (falling back to {} on unparseable input).
func toolUseInput(arguments json.RawMessage) json.RawMessage {
	if len(arguments) == 0 {
		return json.RawMessage(`{}`)
	}
	var input string
	if err := json.Unmarshal(arguments, &input); err == nil {
		var v any
		if err := json.Unmarshal([]byte(input), &v); err == nil {
			b, err := json.Marshal(v)
			if err == nil {
				return b
			}
		}
	}
	return json.RawMessage(`{}`)
}

// translateTools converts OpenAI function tools into Anthropic tools
// (input_schema carries the OpenAI parameters schema).
func translateTools(tools []openAITool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tool := map[string]any{"name": t.Function.Name, "input_schema": schema}
		if t.Function.Description != "" {
			tool["description"] = t.Function.Description
		}
		out = append(out, tool)
	}
	return out
}

// translateToolChoice maps OpenAI tool_choice to Anthropic form: "auto"→auto,
// "none"→none, "required"→any, and a function object → tool with the name.
func translateToolChoice(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none", "auto", "any", "required":
			return map[string]any{"type": map[string]string{
				"required": "any",
				"auto":     "auto",
				"any":      "any",
				"none":     "none",
			}[s]}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if obj.Function != nil && obj.Function.Name != "" {
		return map[string]any{"type": "tool", "name": obj.Function.Name}
	}
	if obj.Type != "" {
		return map[string]any{"type": obj.Type}
	}
	return nil
}

// translateStop converts the OpenAI stop field (string or array) into
// Anthropic stop_sequences.
func translateStop(raw json.RawMessage) []string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "" {
			return []string{s}
		}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	seqs := arr[:0]
	for _, a := range arr {
		if a != "" {
			seqs = append(seqs, a)
		}
	}
	return seqs
}

// marshalBlocks serializes content blocks into the array JSON the Anthropic
// messages field expects.
func marshalBlocks(blocks []anthropicBlock) json.RawMessage {
	b, _ := json.Marshal(blocks)
	return b
}

// concatBlocks joins two block-array payloads into one (used by the same-role
// message merge).
func concatBlocks(a, b json.RawMessage) json.RawMessage {
	var ab, bb []json.RawMessage
	if err := json.Unmarshal(a, &ab); err != nil {
		return b
	}
	if err := json.Unmarshal(b, &bb); err != nil {
		return a
	}
	out, _ := json.Marshal(append(ab, bb...))
	return out
}

// ---------- non-stream response translation ----------

// mapStopReason converts an Anthropic stop_reason into the OpenAI
// finish_reason vocabulary.
func mapStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "refusal"
	}
	return "stop"
}

// translateAnthropicToOpenAI converts a non-stream Anthropic messages response
// into an OpenAI chat.completion object: text blocks → content, thinking →
// reasoning_content, tool_use blocks → tool_calls, stop_reason → finish_reason,
// usage tokens mapped to the OpenAI names.
func translateAnthropicToOpenAI(body []byte) ([]byte, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	var text, thinking strings.Builder
	toolCalls := make([]map[string]any, 0, 2)
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			thinking.WriteString(block.Thinking)
		case "tool_use":
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": args,
				},
			})
		}
	}

	msg := map[string]any{"role": "assistant", "content": nil}
	if text.Len() > 0 {
		msg["content"] = text.String()
	}
	if thinking.Len() > 0 {
		msg["reasoning_content"] = thinking.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	id := resp.ID
	if id == "" {
		id = "msg_" + store.NewID()
	}
	usage := map[string]int64{
		"prompt_tokens":     resp.Usage.InputTokens,
		"completion_tokens": resp.Usage.OutputTokens,
		"total_tokens":      resp.Usage.InputTokens + resp.Usage.OutputTokens,
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   resp.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": mapStopReason(resp.StopReason),
		}},
		"usage": usage,
	}
	return json.Marshal(out)
}

// translateAnthropicError converts an Anthropic error body into the OpenAI
// error shape; non-Anthropic bodies (recognized by the top-level
// {"type":"error",...} wrapper Anthropic always uses) pass through unchanged.
func translateAnthropicError(body []byte) []byte {
	var e struct {
		Type  string `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Type != "error" || e.Error == nil || e.Error.Message == "" {
		return body
	}
	out, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": e.Error.Message, "type": e.Error.Type, "code": e.Error.Type},
	})
	if err != nil {
		return body
	}
	return out
}

// ---------- streaming translation ----------

// anthropicStreamState folds the Anthropic event sequence into OpenAI chunks:
// message_start seeds the chunk id/model/created, content_block_start for a
// tool_use block emits the tool_call declaration, text/thinking deltas become
// content/reasoning_content, input_json deltas append tool_call arguments,
// message_delta (with a stop_reason) emits the finish_reason + usage chunk, and
// message_stop maps to [DONE].
type anthropicStreamState struct {
	id            string
	model         string
	created       int64
	inputTokens   int64
	nextToolIndex int
	// blockToolIndex maps an Anthropic content block index to the OpenAI
	// tool_call index assigned when the block started.
	blockToolIndex map[int]int
}

// translate converts one Anthropic SSE data payload into zero or more OpenAI
// SSE data payloads.
func (st *anthropicStreamState) translate(payload []byte) [][]byte {
	if string(payload) == "[DONE]" {
		return [][]byte{[]byte("[DONE]")}
	}
	var ev struct {
		Type    string `json:"type"`
		Index   *int   `json:"index"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				InputTokens int64 `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta json.RawMessage `json:"delta"`
		Usage *struct {
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}

	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			if st.id == "" {
				st.id = ev.Message.ID
				st.model = ev.Message.Model
				st.created = time.Now().Unix()
			}
			if ev.Message.Usage != nil {
				st.inputTokens = ev.Message.Usage.InputTokens
			}
		}
		return [][]byte{st.chunk(map[string]any{"role": "assistant"}, nil, nil)}
	case "content_block_start":
		if ev.ContentBlock == nil || ev.ContentBlock.Type != "tool_use" || ev.Index == nil {
			return nil
		}
		idx := st.nextToolIndex
		st.nextToolIndex++
		st.blockToolIndex[*ev.Index] = idx
		return [][]byte{st.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx,
			"id":    ev.ContentBlock.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      ev.ContentBlock.Name,
				"arguments": "",
			},
		}}}, nil, nil)}
	case "content_block_delta":
		return st.deltaChunks(ev.Index, ev.Delta)
	case "content_block_stop":
		return nil
	case "message_delta":
		if len(ev.Delta) == 0 {
			return nil
		}
		var d struct {
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(ev.Delta, &d); err != nil || d.StopReason == "" {
			return nil
		}
		var output int64
		if ev.Usage != nil {
			output = ev.Usage.OutputTokens
		}
		finish := mapStopReason(d.StopReason)
		usage := map[string]int64{
			"prompt_tokens":     st.inputTokens,
			"completion_tokens": output,
			"total_tokens":      st.inputTokens + output,
		}
		return [][]byte{st.chunk(nil, &finish, usage)}
	case "message_stop":
		return [][]byte{[]byte("[DONE]")}
	case "error":
		if ev.Error == nil {
			return nil
		}
		errChunk, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": ev.Error.Message,
			"type":    ev.Error.Type,
			"code":    ev.Error.Type,
		}})
		return [][]byte{errChunk}
	}
	return nil
}

// deltaChunks maps a content_block_delta event (text_delta / thinking_delta /
// input_json_delta) to its OpenAI chunk(s).
func (st *anthropicStreamState) deltaChunks(index *int, raw json.RawMessage) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	var d struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil
	}
	switch d.Type {
	case "text_delta":
		if d.Text == "" {
			return nil
		}
		return [][]byte{st.chunk(map[string]any{"content": d.Text}, nil, nil)}
	case "thinking_delta":
		if d.Thinking == "" {
			return nil
		}
		return [][]byte{st.chunk(map[string]any{"reasoning_content": d.Thinking}, nil, nil)}
	case "input_json_delta":
		if d.PartialJSON == "" || index == nil {
			return nil
		}
		toolIdx, ok := st.blockToolIndex[*index]
		if !ok {
			return nil
		}
		return [][]byte{st.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    toolIdx,
			"function": map[string]any{"arguments": d.PartialJSON},
		}}}, nil, nil)}
	}
	return nil
}

// chunk renders one OpenAI chat.completion.chunk SSE payload. id/model/created
// come from message_start; a nil delta yields an empty delta (finish chunks).
func (st *anthropicStreamState) chunk(delta map[string]any, finish *string, usage map[string]int64) []byte {
	if delta == nil {
		delta = map[string]any{}
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = *finish
	} else {
		choice["finish_reason"] = nil
	}
	out := map[string]any{
		"id":      st.id,
		"object":  "chat.completion.chunk",
		"created": st.created,
		"model":   st.model,
		"choices": []any{choice},
	}
	if usage != nil {
		out["usage"] = usage
	}
	b, _ := json.Marshal(out)
	return b
}

// readAnthropicStream reads one Anthropic SSE stream to completion (or context
// end), translating each event into OpenAI chat.completion.chunk lines that
// are forwarded to the client and folded into the assembler — so the shared
// gateway machinery (assembler, buffered/live emitters, held-mode retry,
// plugins, capture) sees only OpenAI-shaped data. message_stop synthesizes the
// terminating [DONE]. Event/ping/blank lines are consumed without forwarding.
func readAnthropicStream(ctx context.Context, resp *http.Response, asm *completionAssembler, forward func([]byte) error) streamOutcome {
	lines := make(chan []byte, 64)
	asmDone := make(chan struct{})
	go func() {
		defer close(asmDone)
		for payload := range lines {
			asm.add(payload)
		}
	}()

	st := &anthropicStreamState{blockToolIndex: map[int]int{}}
	cleanEnd := false
	reader := bufio.NewReader(resp.Body)
loop:
	for {
		select {
		case <-ctx.Done():
		default:
		}
		line, err := reader.ReadString('\n')
		if line != "" {
			if payload := ssePayload(strings.TrimRight(line, "\r\n")); payload != nil {
				chunks := st.translate(payload)
				for _, ch := range chunks {
					if werr := forward([]byte("data: " + string(ch) + "\n\n")); werr != nil {
						break loop
					}
					lines <- ch
					if string(ch) == "[DONE]" {
						cleanEnd = true
					}
				}
				if cleanEnd {
					break loop
				}
			}
			// event:/ping/blank lines are consumed, never forwarded.
		}
		if err != nil {
			if err == io.EOF {
				cleanEnd = true
			}
			break loop
		}
	}
	close(lines)
	<-asmDone

	reassembled, finish, usage := asm.result()
	return streamOutcome{
		reassembled: reassembled,
		finish:      finish,
		usage:       usage,
		streamErr:   asm.streamError(),
		chunks:      asm.chunks(),
		truncated:   !cleanEnd,
		cleanEnd:    cleanEnd,
		statusCode:  resp.StatusCode,
	}
}
