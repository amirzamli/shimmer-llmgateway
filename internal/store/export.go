package store

import (
	"context"
	"encoding/json"
	"io"
)

// The §8 canonical JSONL line writers. They are the single source of the
// export format, shared by ExportSession, the UI export button (Phase 5), and
// the gateway's append log (Phase 3): every record is one JSON line with the
// canonical envelope {type, ts, session_id, request_id, alias, plugins, ...}.
//
//	EncodeSessionStart — session_start
//	EncodeRequest       — per request (by seq): request, error (when failed),
//	                      tool_call (per emitted call), tool_result (per
//	                      role:"tool" message in the body).

// EncodeSessionStart writes the §8 session_start JSONL line to w.
func EncodeSessionStart(w io.Writer, sum SessionSummary) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(sessionStartLine(sum))
}

// EncodeRequest writes the §8 JSONL lines for one captured request to w: the
// request record followed by its error, tool_call, and tool_result records.
func EncodeRequest(w io.Writer, req *Request) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, line := range requestLines(req) {
		if err := enc.Encode(line); err != nil {
			return err
		}
	}
	return nil
}

// sessionStartLine returns the §8 session_start record.
func sessionStartLine(sum SessionSummary) map[string]any {
	return map[string]any{
		"type":       "session_start",
		"ts":         sum.CreatedAt,
		"session_id": sum.ID,
		"alias":      sum.FirstAlias,
		"model":      sum.FirstModel,
		"plugins":    nil,
	}
}

// requestLines returns the §8 records for one request: the request record,
// then the error record (when failed), the tool_call records (one per emitted
// call), and the tool_result records (one per role:"tool" message).
func requestLines(req *Request) []map[string]any {
	line := map[string]any{
		"type":          "request",
		"ts":            req.CreatedAt,
		"session_id":    req.SessionID,
		"request_id":    req.ID,
		"seq":           req.Seq,
		"alias":         req.Alias,
		"provider":      req.Provider,
		"model":         req.Model,
		"endpoint":      req.Endpoint,
		"duration_ms":   req.DurationMS,
		"status_code":   req.StatusCode,
		"finish_reason": req.FinishReason,
		"plugins":       req.PluginsApplied,
		"truncated":     req.Truncated,
	}
	// ChunkCount is not a §5 column; it surfaces only in the append-log line.
	if req.ChunkCount > 0 {
		line["chunk_count"] = req.ChunkCount
	}
	if len(req.Usage) > 0 {
		line["usage"] = req.Usage
	}
	if req.PromptTokens > 0 || req.CompletionTokens > 0 {
		line["tokens"] = map[string]any{
			"prompt":      req.PromptTokens,
			"completion":  req.CompletionTokens,
			"cached":      req.CachedTokens,
			"cost_total":  req.CostTotal,
			"cost_input":  req.CostInput,
			"cost_output": req.CostOutput,
			"cost_cached": req.CostCacheRead + req.CostCacheWrite,
			"priced":      req.CostPriced,
		}
	}
	if len(req.RequestJSON) > 0 {
		line["request"] = embedBytes(req.RequestJSON)
	}
	if len(req.RequestFilteredJSON) > 0 {
		line["request_filtered"] = embedBytes(req.RequestFilteredJSON)
	}
	if len(req.UpstreamRequestJSON) > 0 {
		line["upstream_request"] = embedBytes(req.UpstreamRequestJSON)
	}
	if len(req.ResponseJSON) > 0 {
		line["response"] = embedBytes(req.ResponseJSON)
	}
	if len(req.ResponseFilteredJSON) > 0 {
		line["response_filtered"] = embedBytes(req.ResponseFilteredJSON)
	}
	out := []map[string]any{line}

	if req.Error != nil {
		out = append(out, map[string]any{
			"type":        "error",
			"ts":          req.CreatedAt,
			"session_id":  req.SessionID,
			"request_id":  req.ID,
			"alias":       req.Alias,
			"plugins":     req.PluginsApplied,
			"status_code": req.StatusCode,
			"code":        req.Error.Code,
			"message":     req.Error.Message,
		})
	}

	for _, tc := range extractToolCalls(req.ResponseJSON) {
		out = append(out, map[string]any{
			"type":         "tool_call",
			"ts":           req.CreatedAt,
			"session_id":   req.SessionID,
			"request_id":   req.ID,
			"alias":        req.Alias,
			"plugins":      req.PluginsApplied,
			"seq":          req.Seq,
			"tool_call_id": tc.ProviderID,
			"tool_name":    tc.ToolName,
			"arguments":    embedJSON(tc.Arguments),
		})
	}

	for _, tr := range extractToolResults(req.RequestJSON) {
		out = append(out, map[string]any{
			"type":         "tool_result",
			"ts":           req.CreatedAt,
			"session_id":   req.SessionID,
			"request_id":   req.ID,
			"alias":        req.Alias,
			"plugins":      req.PluginsApplied,
			"tool_call_id": tr.ToolCallID,
			"content":      tr.Content,
			"is_error":     tr.IsError,
		})
	}
	return out
}

// ExportSession writes the §8 canonical JSONL for one session: every record is
// one JSON line with the canonical envelope {type, ts, session_id, request_id,
// alias, plugins, ...}. Type order follows conversation order:
//
//	session_start,
//	per request (by seq): request, error (when failed), tool_call (per emitted
//	call), tool_result (per role:"tool" message in the body).
//
// The same format is shared by the UI export, the MCP export_session tool, and
// the gateway's append log.
func (s *Store) ExportSession(ctx context.Context, sessionID string, w io.Writer) error {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := EncodeSessionStart(w, sess.SessionSummary); err != nil {
		return err
	}
	for _, req := range sess.Requests {
		if err := EncodeRequest(w, req); err != nil {
			return err
		}
	}
	return nil
}

// embedBytes returns b as a JSON value, falling back to the raw string when b
// is not valid JSON.
func embedBytes(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return b
	}
	s, _ := json.Marshal(string(b))
	return s
}

// embedJSON parses text and embeds it as a JSON value, falling back to the raw
// string when text is not valid JSON.
func embedJSON(text string) json.RawMessage {
	if text == "" {
		return nil
	}
	if json.Valid([]byte(text)) {
		return json.RawMessage(text)
	}
	s, _ := json.Marshal(text)
	return s
}
