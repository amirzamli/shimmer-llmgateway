package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"shimmer-llmgateway/internal/store"
)

// toolError is a tool execution failure surfaced as
// CallToolResult(isError=true) with the §7 {errorCode, message} body.
type toolError struct {
	code    string
	message string
}

// internalToolError wraps an unexpected store failure as a tool error. Tools
// never throw; the message keeps the underlying detail for debugging.
func internalToolError(err error) *toolError {
	return &toolError{code: "INTERNAL", message: err.Error()}
}

// tool is one registered MCP tool.
type tool struct {
	name        string
	description string
	inputSchema map[string]any
	handler     func(s *Server, ctx context.Context, args map[string]any) (any, *toolError)
}

// resultJSONText renders a handler result as the CallToolResult text: a
// handler that returns a string uses it verbatim (export_session returns the
// §8 JSONL text); anything else is JSON-marshaled.
func resultJSONText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return `{"errorCode":"INTERNAL","message":"result marshal failed"}`
	}
	return string(b)
}

// Tool input schema helpers (JSON Schema subset used by MCP).
func schemaProp(typ string) map[string]any { return map[string]any{"type": typ} }

func objectSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// registerTools populates the table-driven registry with the §7.1 tools
// (the seven generic tools plus the validate_tool_call / classify_failure
// analysis tools).
func (s *Server) registerTools() {
	tools := []tool{
		{
			name:        "list_sessions",
			description: "List captured sessions (newest first) with failure counts. Filters: alias, model, status (ok|error), since/until (RFC3339 or YYYY-MM-DD), limit.",
			inputSchema: objectSchema(map[string]any{
				"limit":  schemaProp("integer"),
				"alias":  schemaProp("string"),
				"model":  schemaProp("string"),
				"status": schemaProp("string"),
				"since":  schemaProp("string"),
				"until":  schemaProp("string"),
			}, nil),
			handler: (*Server).handleListSessions,
		},
		{
			name:        "get_conversation",
			description: "Ordered conversation replay for a session: user/assistant/tool messages with tool calls interleaved with results. include_full embeds the raw request/response payloads per request.",
			inputSchema: objectSchema(map[string]any{
				"session_id":   schemaProp("string"),
				"include_full": schemaProp("boolean"),
			}, []string{"session_id"}),
			handler: (*Server).handleGetConversation,
		},
		{
			name:        "get_request",
			description: "Raw request/response JSON for one captured request (both original and filtered payloads) by session_id + seq.",
			inputSchema: objectSchema(map[string]any{
				"session_id": schemaProp("string"),
				"seq":        schemaProp("integer"),
			}, []string{"session_id", "seq"}),
			handler: (*Server).handleGetRequest,
		},
		{
			name:        "list_tool_calls",
			description: "List tool-call records, optionally filtered by session_id, tool_name, verdict (SUCCESS|RECOVERABLE|BLIND_ERROR), and limit.",
			inputSchema: objectSchema(map[string]any{
				"session_id": schemaProp("string"),
				"tool_name":  schemaProp("string"),
				"verdict":    schemaProp("string"),
				"limit":      schemaProp("integer"),
			}, nil),
			handler: (*Server).handleListToolCalls,
		},
		{
			name:        "export_session",
			description: "Export a session in the §8 canonical JSONL eval-fixture format (messages-only replay): session_start, request, tool_call, tool_result, error records.",
			inputSchema: objectSchema(map[string]any{
				"session_id": schemaProp("string"),
			}, []string{"session_id"}),
			handler: (*Server).handleExportSession,
		},
		{
			name:        "gateway_status",
			description: "Store path, session/request/tool-call/failure counts, disk usage, retention, and configured aliases.",
			inputSchema: objectSchema(nil, nil),
			handler:     (*Server).handleGatewayStatus,
		},
		{
			name:        "search_conversations",
			description: "LIKE-based text search over request/response payloads. Returns matching requests with a snippet around the first match.",
			inputSchema: objectSchema(map[string]any{
				"query": schemaProp("string"),
				"limit": schemaProp("integer"),
			}, []string{"query"}),
			handler: (*Server).handleSearchConversations,
		},
		{
			name:        "validate_tool_call",
			description: "Diffs a tool call's emitted arguments against the client-declared schemas in the originating request: {ok, unknown_params, missing_required, type_mismatches, nearest_params} with Levenshtein-based nearest-param suggestions. Accepts tool_call_id (the §5 row id, e.g. req-1/0) OR session_id+seq; the request form validates every tool call that request emitted (one call returns one object, several return a JSON array).",
			inputSchema: objectSchema(map[string]any{
				"session_id":   schemaProp("string"),
				"seq":          schemaProp("integer"),
				"tool_call_id": schemaProp("string"),
			}, nil),
			handler: (*Server).handleValidateToolCall,
		},
		{
			name:        "classify_failure",
			description: "Classifies a tool call as SUCCESS / RECOVERABLE / BLIND_ERROR with reason keywords (taxonomy derived from §5 + §4.2's unknown-alias exemplar; the referenced report does not exist — the mapping is documented in analysis.go). Caches verdict + annotation back to tool_calls (§5 pull model); later calls return the cached verdict unless refresh=true. Accepts tool_call_id OR session_id+seq.",
			inputSchema: objectSchema(map[string]any{
				"session_id":   schemaProp("string"),
				"seq":          schemaProp("integer"),
				"tool_call_id": schemaProp("string"),
				"refresh":      schemaProp("boolean"),
			}, nil),
			handler: (*Server).handleClassifyFailure,
		},
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = make(map[string]tool, len(tools))
	s.toolOrder = make([]string, 0, len(tools))
	for _, t := range tools {
		s.tools[t.name] = t
		s.toolOrder = append(s.toolOrder, t.name)
	}
}

// requiredSession returns the effective session id for a tool: the --session
// scope when set (constraining the tool), else the caller's session_id param.
func (s *Server) requiredSession(args map[string]any, key string) (string, *toolError) {
	if s.sessionScope != "" {
		return s.sessionScope, nil
	}
	id, ok := argString(args, key)
	if !ok || id == "" {
		return "", &toolError{code: "INVALID_ARGUMENT", message: "missing required parameter: " + key}
	}
	return id, nil
}

// handleListSessions maps list_sessions onto store.ListSessions. When the
// server is scoped (--session), only the scoped session's summary is returned.
func (s *Server) handleListSessions(ctx context.Context, args map[string]any) (any, *toolError) {
	if s.sessionScope != "" {
		sess, err := s.store.GetSession(ctx, s.sessionScope)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, &toolError{code: "NOT_FOUND", message: "session not found: " + s.sessionScope}
			}
			return nil, internalToolError(err)
		}
		return []sessionSummaryView{sessionSummaryViewOf(&sess.SessionSummary)}, nil
	}

	f := store.SessionFilter{}
	if v, ok := argString(args, "alias"); ok {
		f.Alias = v
	}
	if v, ok := argString(args, "model"); ok {
		f.Model = v
	}
	if v, ok := argString(args, "status"); ok && v != "" {
		if v != "ok" && v != "error" {
			return nil, &toolError{code: "INVALID_ARGUMENT", message: "status must be \"ok\" or \"error\""}
		}
		f.Status = v
	}
	if v, ok := argInt(args, "limit"); ok {
		if v < 0 {
			return nil, &toolError{code: "INVALID_ARGUMENT", message: "limit must be >= 0"}
		}
		if v > 0 {
			f.Limit = v
		}
	}
	since, until, terr := parseSinceUntil(args)
	if terr != nil {
		return nil, terr
	}
	f.Since, f.Until = since, until

	sums, err := s.store.ListSessions(ctx, f)
	if err != nil {
		return nil, internalToolError(err)
	}
	out := make([]sessionSummaryView, 0, len(sums))
	for _, sum := range sums {
		out = append(out, sessionSummaryViewOf(sum))
	}
	return out, nil
}

// handleGetConversation returns the ordered conversation replay (§7.1).
func (s *Server) handleGetConversation(ctx context.Context, args map[string]any) (any, *toolError) {
	sessionID, terr := s.requiredSession(args, "session_id")
	if terr != nil {
		return nil, terr
	}
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &toolError{code: "NOT_FOUND", message: "session not found: " + sessionID}
		}
		return nil, internalToolError(err)
	}
	includeFull, _ := argBool(args, "include_full")
	conv, terr := buildConversation(sess, includeFull)
	if terr != nil {
		return nil, terr
	}
	return conv, nil
}

// handleGetRequest returns the raw original/filtered payloads for one request.
func (s *Server) handleGetRequest(ctx context.Context, args map[string]any) (any, *toolError) {
	sessionID, terr := s.requiredSession(args, "session_id")
	if terr != nil {
		return nil, terr
	}
	seq, ok := argInt(args, "seq")
	if !ok {
		return nil, &toolError{code: "INVALID_ARGUMENT", message: "missing required parameter: seq"}
	}
	if seq <= 0 {
		return nil, &toolError{code: "INVALID_ARGUMENT", message: "seq must be a positive integer"}
	}
	req, err := s.store.GetRequest(ctx, sessionID, seq)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &toolError{code: "NOT_FOUND", message: fmt.Sprintf("request not found: session %s seq %d", sessionID, seq)}
		}
		return nil, internalToolError(err)
	}
	return requestDetailViewOf(req), nil
}

// handleListToolCalls maps list_tool_calls onto store.ListToolCalls. The
// --session scope forces the session filter.
func (s *Server) handleListToolCalls(ctx context.Context, args map[string]any) (any, *toolError) {
	f := store.ToolCallFilter{}
	if s.sessionScope != "" {
		f.SessionID = s.sessionScope
	} else if v, ok := argString(args, "session_id"); ok && v != "" {
		f.SessionID = v
	}
	if v, ok := argString(args, "tool_name"); ok {
		f.ToolName = v
	}
	if v, ok := argString(args, "verdict"); ok && v != "" {
		if !validVerdict(v) {
			return nil, &toolError{code: "INVALID_ARGUMENT", message: "verdict must be one of SUCCESS, RECOVERABLE, BLIND_ERROR"}
		}
		f.Verdict = v
	}
	if v, ok := argInt(args, "limit"); ok {
		if v < 0 {
			return nil, &toolError{code: "INVALID_ARGUMENT", message: "limit must be >= 0"}
		}
		if v > 0 {
			f.Limit = v
		}
	}
	calls, err := s.store.ListToolCalls(ctx, f)
	if err != nil {
		return nil, internalToolError(err)
	}
	out := make([]toolCallRecordView, 0, len(calls))
	for _, c := range calls {
		out = append(out, toolCallRecordViewOf(c))
	}
	return out, nil
}

// handleExportSession returns the §8 canonical JSONL as the result text (the
// eval-fixture, messages-only replay format).
func (s *Server) handleExportSession(ctx context.Context, args map[string]any) (any, *toolError) {
	sessionID, terr := s.requiredSession(args, "session_id")
	if terr != nil {
		return nil, terr
	}
	var buf jsonBuffer
	if err := s.store.ExportSession(ctx, sessionID, &buf); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &toolError{code: "NOT_FOUND", message: "session not found: " + sessionID}
		}
		return nil, internalToolError(err)
	}
	return buf.String(), nil
}

// handleGatewayStatus reports the §7.1 gateway_status payload: store stats,
// disk usage, retention, and the aliases observed in the store (the standalone
// server has no gateway.toml).
func (s *Server) handleGatewayStatus(ctx context.Context, args map[string]any) (any, *toolError) {
	st, err := s.store.Status(ctx)
	if err != nil {
		return nil, internalToolError(err)
	}
	aliases, err := s.store.ListAliases(ctx)
	if err != nil {
		return nil, internalToolError(err)
	}
	return gatewayStatusView{
		StorePath:      st.StorePath,
		SessionCount:   st.SessionCount,
		RequestCount:   st.RequestCount,
		ToolCallCount:  st.ToolCallCount,
		FailureCount:   st.FailureCount,
		DiskUsageBytes: st.DiskUsageBytes,
		RetentionDays:  st.RetentionDays,
		Aliases:        aliases,
	}, nil
}

// handleSearchConversations maps search_conversations onto
// store.SearchConversations. The --session scope post-filters results to that
// session (LIKE search itself is store-wide).
func (s *Server) handleSearchConversations(ctx context.Context, args map[string]any) (any, *toolError) {
	query, ok := argString(args, "query")
	if !ok || query == "" {
		return nil, &toolError{code: "INVALID_ARGUMENT", message: "missing required parameter: query"}
	}
	limit := 0
	if v, ok := argInt(args, "limit"); ok {
		if v < 0 {
			return nil, &toolError{code: "INVALID_ARGUMENT", message: "limit must be >= 0"}
		}
		limit = v
	}
	results, err := s.store.SearchConversations(ctx, query, limit)
	if err != nil {
		return nil, internalToolError(err)
	}
	out := make([]searchResultView, 0, len(results))
	for _, r := range results {
		if s.sessionScope != "" && r.SessionID != s.sessionScope {
			continue
		}
		out = append(out, searchResultViewOf(r))
	}
	return out, nil
}

// Argument helpers. JSON numbers decode as float64; argInt accepts both.
func argString(args map[string]any, key string) (string, bool) {
	if args == nil {
		return "", false
	}
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func argInt(args map[string]any, key string) (int, bool) {
	if args == nil {
		return 0, false
	}
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func argBool(args map[string]any, key string) (bool, bool) {
	if args == nil {
		return false, false
	}
	v, ok := args[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// parseSinceUntil parses the since/until date filters (RFC3339 or YYYY-MM-DD).
func parseSinceUntil(args map[string]any) (time.Time, time.Time, *toolError) {
	var since, until time.Time
	if v, ok := argString(args, "since"); ok && v != "" {
		t, err := parseTimeArg(v)
		if err != nil {
			return since, until, &toolError{code: "INVALID_ARGUMENT", message: "since: " + err.Error()}
		}
		since = t
	}
	if v, ok := argString(args, "until"); ok && v != "" {
		t, err := parseTimeArg(v)
		if err != nil {
			return since, until, &toolError{code: "INVALID_ARGUMENT", message: "until: " + err.Error()}
		}
		until = t
	}
	return since, until, nil
}

func parseTimeArg(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q (want RFC3339 or YYYY-MM-DD)", v)
}

func validVerdict(v string) bool {
	switch v {
	case "SUCCESS", "RECOVERABLE", "BLIND_ERROR":
		return true
	}
	return false
}

// jsonBuffer is an io.Writer accumulating bytes for the export text.
type jsonBuffer struct{ b []byte }

func (b *jsonBuffer) Write(p []byte) (int, error) {
	b.b = append(b.b, p...)
	return len(p), nil
}
func (b *jsonBuffer) String() string { return string(b.b) }
