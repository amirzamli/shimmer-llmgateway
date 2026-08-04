package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractedToolCall is one tool call found in a reassembled response.
type ExtractedToolCall struct {
	// ProviderID is the provider's tool_call id ("call_abc"), used to link
	// role:"tool" results.
	ProviderID string
	ToolName   string
	// Arguments is the parsed JSON arguments text (compact); the raw string
	// when unparsable.
	Arguments string
}

// ToolResultMessage is one role:"tool" result message found in a request body.
type ToolResultMessage struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// toolCallID is the §5 tool_calls.id: "request id + tool_call index".
func toolCallID(requestID string, index int) string {
	return fmt.Sprintf("%s/%d", requestID, index)
}

// extractToolCalls pulls tool calls out of the reassembled response JSON in
// index-stable order (choices order, then tool_calls array order). The index
// is the flattened position, matching toolCallID.
func extractToolCalls(response []byte) []ExtractedToolCall {
	if len(response) == 0 {
		return nil
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(response, &resp); err != nil {
		return nil
	}
	var out []ExtractedToolCall
	for _, ch := range resp.Choices {
		for _, tc := range ch.Message.ToolCalls {
			out = append(out, ExtractedToolCall{
				ProviderID: tc.ID,
				ToolName:   tc.Function.Name,
				Arguments:  compactJSON(tc.Function.Arguments),
			})
		}
	}
	return out
}

// extractToolResults walks a request body for role:"tool" messages.
func extractToolResults(request []byte) []ToolResultMessage {
	if len(request) == 0 {
		return nil
	}
	var req struct {
		Messages []struct {
			Role       string          `json:"role"`
			ToolCallID string          `json:"tool_call_id"`
			Content    json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return nil
	}
	var out []ToolResultMessage
	for _, m := range req.Messages {
		if m.Role != "tool" {
			continue
		}
		content := contentText(m.Content)
		out = append(out, ToolResultMessage{
			ToolCallID: m.ToolCallID,
			Content:    content,
			IsError:    resultIsError(content),
		})
	}
	return out
}

// compactJSON parses s and re-serializes it compactly; the raw string is
// returned when s is not valid JSON (or empty).
func compactJSON(s string) string {
	if s == "" {
		return s
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

// contentText flattens a message content value (string or array of parts) to
// text.
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

// resultIsError applies a lightweight heuristic for tool_calls.result_is_error:
// the spec does not define a machine-checkable error marker, so a result is an
// error when its content parses as a JSON object carrying a non-null "error"
// member or a true "is_error" member. Non-JSON content is treated as OK.
func resultIsError(content string) bool {
	if content == "" {
		return false
	}
	var v any
	if err := json.Unmarshal([]byte(content), &v); err != nil {
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

// snippet truncates a tool result to the first ResultSnippetLen chars.
func snippet(content string) string {
	if len(content) <= ResultSnippetLen {
		return content
	}
	return content[:ResultSnippetLen]
}

// linkToolResults matches role:"tool" messages in the request body against
// tool calls previously extracted from responses of the same session, then
// backfills result_is_error / result_snippet on the tool_calls rows.
func (s *Store) linkToolResults(ctx context.Context, tx *sql.Tx, sessionID string, requestJSON []byte) error {
	results := extractToolResults(requestJSON)
	if len(results) == 0 {
		return nil
	}
	for _, res := range results {
		id, found, err := findToolCallRow(ctx, tx, sessionID, res.ToolCallID)
		if err != nil {
			return err
		}
		if !found {
			continue // result for an unknown call id; leave unlinked
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tool_calls SET result_is_error = ?, result_snippet = ? WHERE id = ?`,
			boolInt(res.IsError), snippet(res.Content), id,
		); err != nil {
			return err
		}
	}
	return nil
}

// findToolCallRow locates the tool_calls row for a provider tool_call id by
// scanning the session's requests (the provider id is not a stored column, so
// it is re-extracted from response_json). A LIKE prefilter bounds the scan to
// requests that mention the id; exact match is confirmed by parsing.
func findToolCallRow(ctx context.Context, tx *sql.Tx, sessionID, providerID string) (string, bool, error) {
	if providerID == "" {
		return "", false, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, response_json FROM requests
		WHERE session_id = ? AND response_json IS NOT NULL AND response_json LIKE '%' || ? || '%'
		ORDER BY seq ASC`,
		sessionID, providerID,
	)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var reqID string
		var resp []byte
		if err := rows.Scan(&reqID, &resp); err != nil {
			return "", false, err
		}
		if idx := findToolCallIndex(resp, providerID); idx >= 0 {
			return toolCallID(reqID, idx), true, nil
		}
	}
	return "", false, rows.Err()
}

// findToolCallIndex returns the flattened index of the tool call whose
// provider id matches, or -1.
func findToolCallIndex(response []byte, providerID string) int {
	for i, tc := range extractToolCalls(response) {
		if tc.ProviderID == providerID {
			return i
		}
	}
	return -1
}
