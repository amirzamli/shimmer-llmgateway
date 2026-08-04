package mcp

import (
	"bytes"
	"encoding/json"
	"strings"

	"shimmer-llmgateway/internal/store"
)

// The get_conversation replay. It is built from the stored request/response
// payloads so it needs no schema knowledge: the ordered replay shows user and
// assistant messages, the tool calls the model emitted (interleaved in the
// assistant message), and the role:"tool" results the client returned in later
// requests.

// conversationView is the get_conversation result.
type conversationView struct {
	SessionID     string            `json:"session_id"`
	CreatedAt     string            `json:"created_at"`
	FirstAlias    string            `json:"first_alias"`
	FirstModel    string            `json:"first_model"`
	RequestCount  int               `json:"request_count"`
	ToolCallCount int               `json:"tool_call_count"`
	FailureCount  int               `json:"failure_count"`
	Messages      []conversationMsg `json:"messages"`
	Requests      []requestTurnView `json:"requests"`
}

// conversationMsg is one message in the ordered replay.
type conversationMsg struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	IsError    bool             `json:"is_error,omitempty"`
	ToolCalls  []replayToolCall `json:"tool_calls,omitempty"`
}

// replayToolCall is a tool call inside an assistant replay message, carrying
// the tool_calls row id so Phase 7's validate_tool_call (which accepts a
// tool-call id per resolved decision #4) can be invoked directly.
type replayToolCall struct {
	ID            string          `json:"id"` // provider tool_call id ("call_1")
	ToolName      string          `json:"tool_name"`
	Arguments     json.RawMessage `json:"arguments"`
	ToolCallRowID string          `json:"tool_call_row_id"`
	Verdict       string          `json:"verdict,omitempty"`
}

// requestTurnView is per-request metadata; the raw payloads are included only
// when include_full is set.
type requestTurnView struct {
	Seq        int             `json:"seq"`
	RequestID  string          `json:"request_id"`
	Alias      string          `json:"alias"`
	Model      string          `json:"model"`
	CreatedAt  string          `json:"created_at"`
	StatusCode int             `json:"status_code"`
	Request    json.RawMessage `json:"request,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
}

// buildConversation assembles the ordered replay for a loaded session.
func buildConversation(sess *store.Session, includeFull bool) (*conversationView, *toolError) {
	rowsByReq := map[string][]*store.ToolCall{}
	for _, tc := range sess.ToolCalls {
		rowsByReq[tc.RequestID] = append(rowsByReq[tc.RequestID], tc)
	}

	conv := &conversationView{
		SessionID:     sess.ID,
		CreatedAt:     sess.CreatedAt,
		FirstAlias:    sess.FirstAlias,
		FirstModel:    sess.FirstModel,
		RequestCount:  sess.RequestCount,
		ToolCallCount: sess.ToolCallCount,
		FailureCount:  sess.FailureCount,
	}

	var prev []openAIMessage
	for _, req := range sess.Requests {
		if msgs, ok := parseOpenAIMessages(req.RequestJSON); ok {
			for _, m := range newMessagesTail(prev, msgs) {
				conv.Messages = append(conv.Messages, messageFromOpenAI(m))
			}
			prev = msgs
		}
		if asst := assistantReplayMessage(req, rowsByReq[req.ID]); asst != nil {
			conv.Messages = append(conv.Messages, *asst)
		}

		turn := requestTurnView{
			Seq:        req.Seq,
			RequestID:  req.ID,
			Alias:      req.Alias,
			Model:      req.Model,
			CreatedAt:  req.CreatedAt,
			StatusCode: req.StatusCode,
		}
		if includeFull {
			turn.Request = embedRaw(req.RequestJSON)
			turn.Response = embedRaw(req.ResponseJSON)
		}
		conv.Requests = append(conv.Requests, turn)
	}
	return conv, nil
}

// openAIMessage is a message object inside a chat completions request body.
type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}

// parseOpenAIMessages extracts the messages array from a request body.
func parseOpenAIMessages(body []byte) ([]openAIMessage, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var req struct {
		Messages []openAIMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false
	}
	return req.Messages, true
}

// newMessagesTail returns the tail of cur after the longest common prefix with
// prev. OpenAI clients re-send the full history on every request, so the tail
// is the content actually added in this turn (including role:"tool" results).
func newMessagesTail(prev, cur []openAIMessage) []openAIMessage {
	if len(prev) == 0 {
		return cur
	}
	n := 0
	for n < len(prev) && n < len(cur) && sameOpenAIMessage(prev[n], cur[n]) {
		n++
	}
	if n >= len(cur) {
		return nil
	}
	return cur[n:]
}

// sameOpenAIMessage compares two message objects by canonical JSON.
func sameOpenAIMessage(a, b openAIMessage) bool {
	return a.Role == b.Role && a.ToolCallID == b.ToolCallID &&
		bytes.Equal(compactRaw(a.Content), compactRaw(b.Content))
}

// compactRaw normalizes raw JSON for comparison (falls back to the raw bytes).
func compactRaw(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

// messageFromOpenAI converts a request-body message into a replay message.
func messageFromOpenAI(m openAIMessage) conversationMsg {
	out := conversationMsg{Role: m.Role, Content: contentValue(m.Content)}
	if m.Role == "tool" {
		out.ToolCallID = m.ToolCallID
		out.IsError = resultIsError(contentText(m.Content))
	}
	return out
}

// contentValue parses raw content as a JSON value (string, array of parts, or
// raw text when unparsable).
func contentValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// assistantReplayMessage builds the assistant replay message for a response:
// the first choice's message content plus any tool calls, matched to their
// tool_calls rows by flattened index.
func assistantReplayMessage(req *store.Request, rows []*store.ToolCall) *conversationMsg {
	var resp struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if len(req.ResponseJSON) == 0 || json.Unmarshal(req.ResponseJSON, &resp) != nil {
		return nil
	}

	msg := &conversationMsg{Role: "assistant"}
	callIdx := 0
	for _, ch := range resp.Choices {
		if len(msg.ToolCalls) == 0 {
			msg.Content = contentValue(ch.Message.Content)
		}
		for _, tc := range ch.Message.ToolCalls {
			rtc := replayToolCall{ID: tc.ID, ToolName: tc.Function.Name}
			if callIdx < len(rows) {
				row := rows[callIdx]
				rtc.ToolCallRowID = row.ID
				rtc.Verdict = row.Verdict
				if len(row.ArgumentsJSON) > 0 {
					rtc.Arguments = row.ArgumentsJSON
				}
			}
			if len(rtc.Arguments) == 0 {
				rtc.Arguments = compactJSONArg(tc.Function.Arguments)
			}
			msg.ToolCalls = append(msg.ToolCalls, rtc)
			callIdx++
		}
	}
	if len(msg.ToolCalls) == 0 && msg.Content == nil {
		return nil
	}
	return msg
}

// compactJSONArg parses a tool-call arguments string and re-serializes it
// compactly; the raw string is returned when it is not valid JSON.
func compactJSONArg(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			b, _ := json.Marshal(v)
			return b
		}
	}
	b, _ := json.Marshal(s)
	return b
}

// contentText flattens a message content value (string or array of parts) to
// text, mirroring the store's §-wide convention.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return string(raw)
}

// resultIsError applies the store's §-wide heuristic for tool_calls
// result_is_error: a result is an error when its content parses as a JSON
// object carrying a non-null "error" member or a true "is_error" member.
func resultIsError(text string) bool {
	if text == "" {
		return false
	}
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if errVal, ok := obj["error"]; ok && errVal != nil {
		if s, ok := errVal.(string); ok {
			return s != ""
		}
		return true
	}
	if isErr, ok := obj["is_error"].(bool); ok {
		return isErr
	}
	return false
}
