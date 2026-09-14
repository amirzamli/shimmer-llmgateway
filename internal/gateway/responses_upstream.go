package gateway

// Responses-API upstream style ("responses"): templates with style="responses"
// post to <base_url>/responses and speak the OpenAI Responses API instead of
// /chat/completions. This file translates between the OpenAI chat format the
// gateway and its clients speak and the Responses wire format, in both
// directions for non-stream bodies (SSE streaming lands in Phase 2b):
//
//   - translateChatToResponses: client/chat body → Responses request
//     (the first system message → top-level instructions, later system
//     messages → system input items, user/assistant → message input items,
//     assistant tool_calls → function_call items, role:"tool" →
//     function_call_output, tools flattened to the responsesTool form,
//     tool_choice mapped, max_tokens/max_completion_tokens →
//     max_output_tokens (except for the restricted ChatGPT Codex contract),
//     reasoning_effort → reasoning.effort,
//     response_format → text.format, prompt_cache_key from the effective
//     session id);
//   - translateResponsesToChatCompletion: non-stream Responses response →
//     chat.completion (message output items → content, reasoning items →
//     reasoning_content, function_call items → tool_calls, usage/status
//     mapped); unparseable bodies pass through unchanged (mirrors
//     translateAnthropicToOpenAI / translateAnthropicError).
//
// These are the structural inverses of the /v1/responses client shim
// (translateResponsesToChat / chatCompletionToResponse in responses.go): the
// gateway's internal canonical shape stays chat.completion, and Responses
// protocol handling happens at the upstream boundary only, exactly like the
// anthropic style (§4.2).

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// ---------- request translation ----------

// translateChatToResponses converts an OpenAI chat.completions body into an
// OpenAI Responses API request body. Anything the Responses API does not
// accept (stop, logprobs, presence_penalty, …) is deliberately dropped — the
// translated body carries only known Responses fields, mirroring
// translateOpenAIToAnthropic. sessionID is the request's effective session id
// (the same id sent via the template's SessionHeader); when non-empty it is
// forwarded as prompt_cache_key so upstream prompt caching and the gateway
// session grouping stay consistent. chatGPTCodex selects the restricted
// ChatGPT Codex contract, which requires streaming and rejects
// max_output_tokens.
func translateChatToResponses(chatBody []byte, sessionID string, chatGPTCodex bool) ([]byte, error) {
	var req struct {
		Model               string          `json:"model"`
		Messages            []openAIMsg     `json:"messages"`
		Tools               []openAITool    `json:"tools"`
		ToolChoice          json.RawMessage `json:"tool_choice"`
		ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
		Temperature         *float64        `json:"temperature"`
		TopP                *float64        `json:"top_p"`
		MaxTokens           *int            `json:"max_tokens"`
		MaxCompletionTokens *int            `json:"max_completion_tokens"`
		Stream              bool            `json:"stream"`
		ReasoningEffort     string          `json:"reasoning_effort"`
		ResponseFormat      json.RawMessage `json:"response_format"`
	}
	if err := json.Unmarshal(chatBody, &req); err != nil {
		return nil, err
	}

	instructions, items := translateChatMessages(req.Messages)

	out := map[string]any{
		"model": req.Model,
		"input": items,
	}
	if chatGPTCodex {
		// The ChatGPT Codex endpoint requires an explicit false value.
		out["store"] = false
	}
	if instructions != "" {
		out["instructions"] = instructions
	}
	if len(req.Tools) > 0 {
		if tools := translateChatTools(req.Tools); len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if tc := translateChatToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	// Both chat max_tokens spellings map to the Responses max_output_tokens;
	// ChatGPT Codex rejects that field, so its client-side limit is deliberately
	// omitted. No default is injected.
	if !chatGPTCodex {
		if req.MaxTokens != nil {
			out["max_output_tokens"] = *req.MaxTokens
		} else if req.MaxCompletionTokens != nil {
			out["max_output_tokens"] = *req.MaxCompletionTokens
		}
	}
	if chatGPTCodex || req.Stream {
		out["stream"] = true
	}
	if req.ReasoningEffort != "" {
		out["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	if len(req.ResponseFormat) > 0 && string(req.ResponseFormat) != "null" {
		out["text"] = map[string]any{"format": req.ResponseFormat}
	}
	if sessionID != "" {
		out["prompt_cache_key"] = sessionID
	}
	return json.Marshal(out)
}

// translateChatMessages converts OpenAI messages into Responses input items.
// The first system message is hoisted into the returned top-level instructions
// string (the Responses API's system prompt field); later system messages
// become system-role input items (the inverse of translateResponsesToChat,
// which maps instructions and developer-role items to chat system messages).
// Assistant tool_calls become separate function_call items, and tool messages
// become function_call_output items.
func translateChatMessages(messages []openAIMsg) (string, []any) {
	var instructions string
	items := make([]any, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case "system":
			if t := contentText(m.Content); t != "" {
				if instructions == "" {
					instructions = t
				} else {
					items = append(items, map[string]any{"type": "message", "role": "system", "content": t})
				}
			}
		case "developer":
			// The chat surface accepts no developer role; translate it the
			// same way the client shim maps developer → chat system.
			if t := contentText(m.Content); t != "" {
				items = append(items, map[string]any{"type": "message", "role": "system", "content": t})
			}
		case "assistant":
			if t := contentText(m.Content); t != "" {
				items = append(items, map[string]any{"type": "message", "role": "assistant", "content": t})
			}
			for _, tc := range m.ToolCalls {
				items = append(items, functionCallItem(tc))
			}
		case "tool":
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  contentText(m.Content),
			})
		default: // user and anything else
			items = append(items, map[string]any{"type": "message", "role": "user", "content": contentText(m.Content)})
		}
	}
	return instructions, items
}

// functionCallItem renders one chat assistant tool_call as a Responses
// function_call input item; the chat call id rides as call_id so a follow-up
// function_call_output pairs with it.
func functionCallItem(tc openAIToolCall) map[string]any {
	var args string
	if len(tc.Function.Arguments) > 0 {
		_ = json.Unmarshal(tc.Function.Arguments, &args)
	}
	if args == "" {
		args = "{}"
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   tc.ID,
		"name":      tc.Function.Name,
		"arguments": args,
	}
}

// translateChatTools converts OpenAI function tools into the Responses flat
// tool form (name at the tool level, unlike chat's nested "function" object).
// Non-function tools are skipped (the client shim applies the same tolerance);
// absent description/parameters are omitted rather than defaulted.
func translateChatTools(tools []openAITool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		tool := map[string]any{"type": "function", "name": t.Function.Name}
		if t.Function.Description != "" {
			tool["description"] = t.Function.Description
		}
		if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
			tool["parameters"] = t.Function.Parameters
		}
		out = append(out, tool)
	}
	return out
}

// translateChatToolChoice maps OpenAI tool_choice to the Responses form:
// scalar forms pass through; the nested function object
// {"type":"function","function":{"name":N}} becomes the flat
// {"type":"function","name":N}. Unrecognized shapes are dropped rather than
// rejected, mirroring translateToolChoice.
func translateChatToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none", "auto", "required":
			return s
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
	if obj.Type == "function" && obj.Function != nil && obj.Function.Name != "" {
		return map[string]any{"type": "function", "name": obj.Function.Name}
	}
	return nil
}

// ---------- response translation ----------

// translateResponsesToChatCompletion converts a non-stream Responses response
// object into an OpenAI chat.completion body: message output items → content,
// reasoning items (summary_text / encrypted_content) → reasoning_content,
// function_call items → tool_calls, usage mapped to the OpenAI names, and
// status → finish_reason (completed→stop, incomplete→length). An unparseable
// body is returned unchanged (mirrors translateAnthropicToOpenAI /
// translateAnthropicError).
func translateResponsesToChatCompletion(respBody []byte) []byte {
	var resp struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Type      string          `json:"type"`
			Content   json.RawMessage `json:"content"`
			Summary   json.RawMessage `json:"summary"`
			CallID    string          `json:"call_id"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
		} `json:"output"`
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			OutputTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return respBody
	}

	var text, reasoning strings.Builder
	toolCalls := make([]map[string]any, 0, 2)
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			text.WriteString(responsesOutputText(item.Content))
		case "reasoning":
			reasoning.WriteString(responsesReasoningText(item.Summary))
		case "function_call":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      item.Name,
					"arguments": args,
				},
			})
		}
	}

	msg := map[string]any{"role": "assistant", "content": nil}
	if text.Len() > 0 {
		msg["content"] = text.String()
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	id := resp.ID
	if id == "" {
		id = "msg_" + store.NewID()
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   resp.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": responsesFinishReason(resp.Status),
		}},
	}
	if resp.Usage != nil {
		usage := map[string]any{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		}
		if resp.Usage.TotalTokens == 0 {
			usage["total_tokens"] = resp.Usage.InputTokens + resp.Usage.OutputTokens
		}
		if resp.Usage.OutputTokensDetails != nil {
			usage["completion_tokens_details"] = map[string]any{
				"reasoning_tokens": resp.Usage.OutputTokensDetails.ReasoningTokens,
			}
		}
		out["usage"] = usage
	}
	b, err := json.Marshal(out)
	if err != nil {
		return respBody
	}
	return b
}

// responsesOutputText extracts the text of a Responses output message content
// field (a plain string or an array of output_text parts). Anything else
// yields "".
func responsesOutputText(content json.RawMessage) string {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "output_text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// responsesReasoningText extracts the text of a Responses reasoning item
// summary: summary_text parts are joined, and an encrypted_content part
// (present only when the request asked for reasoning.encrypted_content) is
// surfaced verbatim via its ciphertext since the stateless gateway cannot
// decrypt it. An absent/unparseable summary degrades to an empty reasoning
// string while the turn's content is still delivered.
func responsesReasoningText(summary json.RawMessage) string {
	if len(summary) == 0 || string(summary) == "null" {
		return ""
	}
	var parts []struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(summary, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "summary_text":
			b.WriteString(p.Text)
		case "encrypted_content":
			b.WriteString(p.Ciphertext)
		}
	}
	return b.String()
}

// responsesFinishReason maps a Responses response status to the OpenAI
// finish_reason vocabulary; unknown statuses default to stop (the anthropic
// mapStopReason convention).
func responsesFinishReason(status string) string {
	switch status {
	case "completed":
		return "stop"
	case "incomplete":
		return "length"
	}
	return "stop"
}

// ---------- streaming translation ----------

// responsesUpstreamStreamState folds the Responses SSE event sequence into
// OpenAI chat.completion.chunk payloads (the structural mirror of
// anthropicStreamState): response.created seeds the chunk id/model/created and
// emits a role chunk; response.output_item.added for a message/function_call
// item emits the role / tool_call-declaration chunk; output_text /
// reasoning_summary_text / function_call_arguments deltas become content /
// reasoning_content / tool_call-arguments delta chunks; response.completed
// emits the finish_reason + usage chunk and marks the clean end; and
// response.failed / a top-level {"error":...} payload emits an OpenAI error
// chunk. Events with nothing to fold (created/in_progress, item .done,
// content_part.*, unknown) are no-ops.
type responsesUpstreamStreamState struct {
	id            string
	model         string
	created       int64
	nextToolIndex int
	// toolItemIndex maps a function_call item id to the OpenAI tool_call index
	// assigned when the item was added, so interleaved arguments deltas stay
	// index-stable (the assembler's own rule).
	toolItemIndex map[string]int
	// done is set once response.completed is seen; readResponsesStream uses it
	// to mark a clean end. The upstream carries no [DONE] sentinel — the reader
	// synthesizes one for the chat surface on a clean end (like message_stop
	// on the anthropic style).
	done bool
}

// translate converts one Responses SSE data payload into zero or more OpenAI
// SSE data payloads.
func (st *responsesUpstreamStreamState) translate(payload []byte) [][]byte {
	var ev struct {
		Type       string          `json:"type"`
		Model      string          `json:"model"`
		Item       json.RawMessage `json:"item"`
		ItemID     string          `json:"item_id"`
		Delta      json.RawMessage `json:"delta"`
		Ciphertext string          `json:"ciphertext"`
		Response   *struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Status string `json:"status"`
			Usage  *struct {
				InputTokens         int64 `json:"input_tokens"`
				OutputTokens        int64 `json:"output_tokens"`
				OutputTokensDetails *struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
			Error json.RawMessage `json:"error"`
		} `json:"response"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}
	// A top-level {"error":{...}} payload (no type field) and the typed
	// response.failed event both become the canonical OpenAI error chunk.
	if len(ev.Error) > 0 {
		return st.fail(ev.Error)
	}
	if ev.Model != "" {
		st.model = ev.Model
	}

	switch ev.Type {
	case "response.created":
		if ev.Response != nil {
			if st.id == "" {
				st.id = ev.Response.ID
				st.model = ev.Response.Model
				st.created = time.Now().Unix()
			}
		}
		return [][]byte{st.chunk(map[string]any{"role": "assistant"}, nil, nil)}
	case "response.output_item.added":
		return st.itemAdded(ev.Item)
	case "response.output_text.delta":
		if d := deltaText(ev.Delta); d != "" {
			return [][]byte{st.chunk(map[string]any{"content": d}, nil, nil)}
		}
	case "response.reasoning_summary_text.delta":
		if d := deltaText(ev.Delta); d != "" {
			return [][]byte{st.chunk(map[string]any{"reasoning_content": d}, nil, nil)}
		}
	case "response.reasoning_encrypted_content.delta":
		// Surfacable reasoning carried as ciphertext (only present when the
		// request asked for reasoning.encrypted_content); surfaced verbatim.
		if ev.Ciphertext != "" {
			return [][]byte{st.chunk(map[string]any{"reasoning_content": ev.Ciphertext}, nil, nil)}
		}
	case "response.function_call_arguments.delta":
		if d := deltaText(ev.Delta); d != "" {
			if toolIdx, ok := st.toolItemIndex[ev.ItemID]; ok {
				return [][]byte{st.chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index":    toolIdx,
					"function": map[string]any{"arguments": d},
				}}}, nil, nil)}
			}
		}
	case "response.completed":
		st.done = true
		return st.completed(ev.Response)
	case "response.failed":
		if ev.Response != nil {
			return st.fail(ev.Response.Error)
		}
	}
	return nil
}

// deltaText extracts the plain-string delta of a *.<type>.delta event: the
// Responses SSE delta field is the string itself (unlike the anthropic
// deltaChunks form, where the delta is an object).
func deltaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return ""
	}
	return s
}

// itemAdded maps a response.output_item.added payload to its OpenAI chunk:
// a message item emits the role chunk; a function_call item emits the
// tool_call declaration (assigning the tool index that later arguments deltas
// target). All other item types are no-ops.
func (st *responsesUpstreamStreamState) itemAdded(raw json.RawMessage) [][]byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var item struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil
	}
	switch item.Type {
	case "message":
		return [][]byte{st.chunk(map[string]any{"role": "assistant"}, nil, nil)}
	case "function_call":
		idx := st.nextToolIndex
		st.nextToolIndex++
		if item.ID != "" {
			st.toolItemIndex[item.ID] = idx
		}
		id := item.CallID
		if id == "" {
			id = item.ID
		}
		return [][]byte{st.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    idx,
			"id":       id,
			"type":     "function",
			"function": map[string]any{"name": item.Name, "arguments": ""},
		}}}, nil, nil)}
	}
	return nil
}

// completed maps the response.completed event to a finish_reason + usage chunk
// (usage folded from the event's response.usage object). A nil response is a
// no-op.
func (st *responsesUpstreamStreamState) completed(resp *struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Usage  *struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		OutputTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}) [][]byte {
	if resp == nil {
		return nil
	}
	finish := responsesFinishReason(resp.Status)
	var usage map[string]any
	if resp.Usage != nil {
		in := resp.Usage.InputTokens
		out := resp.Usage.OutputTokens
		usage = map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": out,
			"total_tokens":      in + out,
		}
		if resp.Usage.OutputTokensDetails != nil && resp.Usage.OutputTokensDetails.ReasoningTokens > 0 {
			usage["completion_tokens_details"] = map[string]any{
				"reasoning_tokens": resp.Usage.OutputTokensDetails.ReasoningTokens,
			}
		}
	}
	return [][]byte{st.chunk(nil, &finish, usage)}
}

// fail turns a mid-stream error envelope (response.failed / top-level error)
// into the canonical OpenAI error chunk the assembler records.
func (st *responsesUpstreamStreamState) fail(raw json.RawMessage) [][]byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var e struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	_ = json.Unmarshal(raw, &e)
	code := stringOf(e.Code)
	if code == "" {
		code = e.Type
	}
	if code == "" {
		code = "STREAM_ERROR"
	}
	if e.Message == "" {
		e.Message = string(raw)
	}
	errChunk, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": e.Message,
		"type":    e.Type,
		"code":    code,
	}})
	return [][]byte{errChunk}
}

// chunk renders one OpenAI chat.completion.chunk SSE payload. id/model/created
// come from response.created; a nil delta yields an empty delta (finish
// chunks).
func (st *responsesUpstreamStreamState) chunk(delta map[string]any, finish *string, usage map[string]any) []byte {
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

// readResponsesStream reads one Responses SSE stream to completion (or context
// end), translating each event into OpenAI chat.completion.chunk lines that
// are forwarded to the client and folded into the assembler — so the shared
// gateway machinery (assembler, buffered/live emitters, held-mode retry,
// plugins, capture) sees only OpenAI-shaped data, exactly like
// readAnthropicStream. The stream ends cleanly at response.completed (this
// style has no [DONE] sentinel); io.EOF is the accepted fallback end. On a
// clean end the terminating data: [DONE] line is synthesized for the chat
// surface (mirroring readAnthropicStream's message_stop behavior); a stream
// that dies mid-flight without response.completed records truncated and must
// NOT emit [DONE].
func readResponsesStream(ctx context.Context, resp *http.Response, asm *completionAssembler, forward func([]byte) error) streamOutcome {
	lines := make(chan []byte, 64)
	asmDone := make(chan struct{})
	go func() {
		defer close(asmDone)
		for payload := range lines {
			asm.add(payload)
		}
	}()

	st := &responsesUpstreamStreamState{toolItemIndex: map[string]int{}}
	cleanEnd := false
	reader := bufio.NewReader(resp.Body)
loop:
	for {
		select {
		case <-ctx.Done():
			// Client disconnected; the transport aborts the upstream read.
			// The clean-end flag still decides truncation.
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
				}
				if st.done {
					// response.completed ended the stream cleanly.
					cleanEnd = true
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

	// Synthesize the terminating [DONE] data line the chat surface expects on
	// a clean end (response.completed or io.EOF; this style has no upstream
	// [DONE] sentinel), mirroring readAnthropicStream. A truncated stream must
	// NOT emit [DONE]. Buffered mode (response plugins) ignores this Write —
	// bufferedEmitter.Done() emits the completion block plus its own [DONE].
	if cleanEnd {
		_ = forward([]byte("data: [DONE]\n\n"))
	}

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
