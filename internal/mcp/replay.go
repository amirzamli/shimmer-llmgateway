package mcp

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
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
	Expired       bool              `json:"expired,omitempty"`
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
	Upstream   json.RawMessage `json:"upstream_request,omitempty"`
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
		Expired:       sess.Expired,
	}

	var prev []openAIMessage
	for _, req := range sess.Requests {
		if msgs, ok := parseOpenAIMessages(req.RequestJSON); ok {
			fresh := newMessagesTail(prev, msgs)
			for _, m := range fresh {
				conv.Messages = append(conv.Messages, messageFromOpenAI(m))
			}
			// Keep the logical replay, including prior assistant responses, as
			// the next alignment baseline. Clients normally resend the whole
			// history, but a changed system prompt or a retry can make the
			// request no longer share a byte-for-byte prefix with the prior
			// request.
			prev = append(prev, fresh...)
		}
		if asst := assistantReplayMessage(req, rowsByReq[req.ID]); asst != nil {
			conv.Messages = append(conv.Messages, *asst)
			if m, ok := assistantOpenAIMessage(req.ResponseJSON); ok {
				prev = append(prev, m)
			}
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
			turn.Upstream = embedRaw(req.UpstreamRequestJSON)
			turn.Response = embedRaw(req.ResponseJSON)
		}
		conv.Requests = append(conv.Requests, turn)
	}
	return conv, nil
}

// openAIMessage is a message object inside a chat completions request body.
type openAIMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolCallID   string          `json:"tool_call_id"`
	Name         string          `json:"name"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
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

// newMessagesTail returns the messages in cur that are not part of the
// longest common subsequence with prev. OpenAI clients re-send the full
// history on every request, so this is the content actually added in the
// current turn (including role:"tool" results). LCS is intentional here:
// changing a system/developer prompt at index zero must not make every later
// message look new, and a prior response already shown in the replay must not
// be shown again when the client echoes it in the next request.
func newMessagesTail(prev, cur []openAIMessage) []openAIMessage {
	if len(prev) == 0 {
		return cur
	}

	// dp[i][j] is the LCS length for prev[i:] and cur[j:]. Message counts are
	// normally small even when individual system messages are large, so the
	// straightforward O(n*m) table keeps the alignment deterministic and easy
	// to audit.
	dp := make([][]int, len(prev)+1)
	for i := range dp {
		dp[i] = make([]int, len(cur)+1)
	}
	for i := len(prev) - 1; i >= 0; i-- {
		for j := len(cur) - 1; j >= 0; j-- {
			if sameOpenAIMessage(prev[i], cur[j]) {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	var fresh []openAIMessage
	i, j := 0, 0
	for i < len(prev) && j < len(cur) {
		if sameOpenAIMessage(prev[i], cur[j]) {
			i++
			j++
			continue
		}
		if dp[i+1][j] >= dp[i][j+1] {
			i++
		} else {
			fresh = append(fresh, cur[j])
			j++
		}
	}
	fresh = append(fresh, cur[j:]...)
	return fresh
}

// sameOpenAIMessage compares the fields that can change a model's view of a
// chat message. Comparing tool calls as well as content matters when the
// assistant response is echoed in the following request.
func sameOpenAIMessage(a, b openAIMessage) bool {
	return a.Role == b.Role && a.ToolCallID == b.ToolCallID && a.Name == b.Name &&
		bytes.Equal(compactRaw(a.Content), compactRaw(b.Content)) &&
		bytes.Equal(compactRaw(a.ToolCalls), compactRaw(b.ToolCalls)) &&
		bytes.Equal(compactRaw(a.FunctionCall), compactRaw(b.FunctionCall))
}

// assistantOpenAIMessage extracts the provider response in the same shape as
// the assistant message clients normally echo into their next request. It is
// used only as an alignment baseline; assistantReplayMessage remains the
// richer public representation with tool-call verdicts.
func assistantOpenAIMessage(body []byte) (openAIMessage, bool) {
	var resp struct {
		Choices []struct {
			Message openAIMessage `json:"message"`
		} `json:"choices"`
	}
	if len(body) == 0 || json.Unmarshal(body, &resp) != nil || len(resp.Choices) == 0 {
		return openAIMessage{}, false
	}
	return resp.Choices[0].Message, true
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
