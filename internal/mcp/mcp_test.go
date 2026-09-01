package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedTestStore captures the fixture used by the smoke tests: two sessions
// with tool calls + results, a failure, a cached verdict, and searchable text.
func seedTestStore(t *testing.T, st *store.Store) {
	t.Helper()

	req1 := &store.CaptureRecord{
		ID: "req-1", SessionID: "sess-a",
		CreatedAt:    time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		Alias:        "openai",
		Provider:     "openai",
		Model:        "gpt-4o",
		Endpoint:     "/v1/chat/completions",
		DurationMS:   100,
		StatusCode:   200,
		FinishReason: "tool_calls",
		RequestJSON:  json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"what is the weather in Paris?"}]}`),
		ResponseJSON: json.RawMessage(`{"id":"resp-1","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Paris\"}"}},
			{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"}}
		]},"finish_reason":"tool_calls"}]}`),
	}
	if err := st.Capture(context.Background(), req1); err != nil {
		t.Fatalf("capture req-1: %v", err)
	}

	req2 := &store.CaptureRecord{
		ID: "req-2", SessionID: "sess-a",
		CreatedAt:    time.Date(2026, 8, 1, 10, 1, 0, 0, time.UTC),
		Alias:        "openai",
		Provider:     "openai",
		Model:        "gpt-4o",
		Endpoint:     "/v1/chat/completions",
		DurationMS:   90,
		StatusCode:   200,
		FinishReason: "stop",
		RequestJSON: json.RawMessage(`{"model":"gpt-4o","messages":[
			{"role":"user","content":"what is the weather in Paris?"},
			{"role":"tool","tool_call_id":"call_2","content":"{\"time\":\"09:00\"}"},
			{"role":"tool","tool_call_id":"call_1","content":"{\"error\":\"unknown city\"}"},
			{"role":"user","content":"thanks"}
		]}`),
		ResponseJSON: json.RawMessage(`{"id":"resp-2","choices":[{"message":{"role":"assistant","content":"It is 18C in Paris."},"finish_reason":"stop"}]}`),
	}
	if err := st.Capture(context.Background(), req2); err != nil {
		t.Fatalf("capture req-2: %v", err)
	}

	req3 := &store.CaptureRecord{
		ID:          "req-3",
		SessionID:   "sess-a",
		CreatedAt:   time.Date(2026, 8, 1, 10, 2, 0, 0, time.UTC),
		Alias:       "openai",
		Provider:    "openai",
		Model:       "gpt-4o",
		Endpoint:    "/v1/chat/completions",
		DurationMS:  5,
		StatusCode:  500,
		RequestJSON: json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"again?"}]}`),
		Error:       &store.ErrorInfo{Code: "UPSTREAM_ERROR", Message: "provider boom"},
	}
	if err := st.Capture(context.Background(), req3); err != nil {
		t.Fatalf("capture req-3: %v", err)
	}

	// Cache a verdict on the get_weather call (the §5 on-demand pull model
	// that Phase 7 writes).
	if err := st.SetVerdict(context.Background(), "req-1/0", "BLIND_ERROR",
		json.RawMessage(`{"why":"missing required param"}`)); err != nil {
		t.Fatalf("SetVerdict: %v", err)
	}

	reqb := &store.CaptureRecord{
		ID: "req-b1", SessionID: "sess-b",
		CreatedAt:    time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
		Alias:        "anthropic",
		Provider:     "anthropic",
		Model:        "claude-3-5-sonnet-latest",
		Endpoint:     "/v1/chat/completions",
		DurationMS:   200,
		StatusCode:   200,
		FinishReason: "stop",
		RequestJSON:  json.RawMessage(`{"model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":"give me a recipe for noodles"}]}`),
		ResponseJSON: json.RawMessage(`{"id":"resp-b","choices":[{"message":{"role":"assistant","content":"Boil water, add noodles."},"finish_reason":"stop"}]}`),
	}
	if err := st.Capture(context.Background(), reqb); err != nil {
		t.Fatalf("capture req-b: %v", err)
	}
}

// transport abstracts one JSON-RPC transport for the smoke tests.
type transport struct {
	name   string
	call   func(t *testing.T, method string, params map[string]any) map[string]any
	notify func(t *testing.T, method string, params map[string]any)
}

func testTransports(t *testing.T, s *Server) []transport {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return []transport{
		{
			name: "stdio",
			call: func(t *testing.T, method string, params map[string]any) map[string]any {
				return stdioCall(t, s, method, params)
			},
			notify: func(t *testing.T, method string, params map[string]any) {
				stdioNotify(t, s, method, params)
			},
		},
		{
			name: "http",
			call: func(t *testing.T, method string, params map[string]any) map[string]any {
				return httpCall(t, ts, method, params)
			},
			notify: func(t *testing.T, method string, params map[string]any) {
				httpNotify(t, ts, method, params)
			},
		},
	}
}

func stdioCall(t *testing.T, s *Server, method string, params map[string]any) map[string]any {
	t.Helper()
	var in bytes.Buffer
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := json.NewEncoder(&in).Encode(req); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), &in, &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v (%q)", err, out.String())
	}
	return resp
}

func stdioNotify(t *testing.T, s *Server, method string, params map[string]any) {
	t.Helper()
	var in bytes.Buffer
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := json.NewEncoder(&in).Encode(req); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), &in, &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("notification produced output: %q", out.String())
	}
}

func httpCall(t *testing.T, ts *httptest.Server, method string, params map[string]any) map[string]any {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest("POST", ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("http call: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("MCP-Protocol-Version") == "" {
		t.Errorf("missing MCP-Protocol-Version response header")
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("bad http response: %v", err)
	}
	return out
}

func httpNotify(t *testing.T, ts *httptest.Server, method string, params map[string]any) {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest("POST", ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("http notify: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification status = %d, want 202", resp.StatusCode)
	}
	if resp.Header.Get("MCP-Protocol-Version") == "" {
		t.Errorf("missing MCP-Protocol-Version header")
	}
}

// callResult is the parsed CallToolResult of a tools/call response.
type callResult struct {
	isError bool
	text    string
}

func toolResult(t *testing.T, resp map[string]any) callResult {
	t.Helper()
	if resp["error"] != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call response has no result object: %v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tools/call response has no content: %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] not an object: %v", content[0])
	}
	text, _ := first["text"].(string)
	isErr, _ := result["isError"].(bool)
	return callResult{isError: isErr, text: text}
}

// assertToolError asserts the §7 error convention: CallToolResult(isError=true)
// whose text body is {errorCode, message}.
func assertToolError(t *testing.T, res callResult, wantCode string) {
	t.Helper()
	if !res.isError {
		t.Fatalf("expected isError=true, got text %q", res.text)
	}
	var body struct {
		ErrorCode string `json:"errorCode"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal([]byte(res.text), &body); err != nil {
		t.Fatalf("error body not JSON: %v (%q)", err, res.text)
	}
	if body.ErrorCode != wantCode {
		t.Errorf("errorCode = %q, want %q (message %q)", body.ErrorCode, wantCode, body.Message)
	}
	if body.Message == "" {
		t.Errorf("error message is empty")
	}
}

// ---------------------------------------------------------------------------
// Protocol handshake
// ---------------------------------------------------------------------------

func TestInitializeHandshake(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			resp := tr.call(t, "initialize", map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "smoke-test"},
			})
			if resp["error"] != nil {
				t.Fatalf("initialize JSON-RPC error: %v", resp["error"])
			}
			result, ok := resp["result"].(map[string]any)
			if !ok {
				t.Fatalf("initialize has no result: %v", resp)
			}
			if result["protocolVersion"] != "2025-06-18" {
				t.Errorf("protocolVersion = %v, want 2025-06-18", result["protocolVersion"])
			}
			caps, _ := result["capabilities"].(map[string]any)
			toolsCaps, ok := caps["tools"].(map[string]any)
			if !ok {
				t.Fatalf("capabilities.tools missing: %v", caps)
			}
			if toolsCaps["listChanged"] != false {
				t.Errorf("tools.listChanged = %v, want false", toolsCaps["listChanged"])
			}
			info, _ := result["serverInfo"].(map[string]any)
			if info["name"] != "inspect-mcp" || info["version"] == "" {
				t.Errorf("serverInfo = %v", info)
			}
		})
	}
}

func TestInitializeUnknownVersionNegotiatesLatest(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			resp := tr.call(t, "initialize", map[string]any{"protocolVersion": "2099-99-99"})
			result := resp["result"].(map[string]any)
			if result["protocolVersion"] != "2025-06-18" {
				t.Errorf("protocolVersion = %v, want latest supported 2025-06-18", result["protocolVersion"])
			}
		})
	}
}

func TestPing(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			resp := tr.call(t, "ping", nil)
			if resp["error"] != nil {
				t.Fatalf("ping error: %v", resp["error"])
			}
			result, ok := resp["result"].(map[string]any)
			if !ok || len(result) != 0 {
				t.Errorf("ping result = %v, want empty object", resp["result"])
			}
		})
	}
}

func TestNotificationInitialized(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			// Notifications get no JSON-RPC response (stdio) and a 202 with an
			// empty body (http) — the client then proceeds to tools/list.
			tr.notify(t, "notifications/initialized", nil)
		})
	}
}

func TestToolsList(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			resp := tr.call(t, "tools/list", nil)
			if resp["error"] != nil {
				t.Fatalf("tools/list error: %v", resp["error"])
			}
			result := resp["result"].(map[string]any)
			tools := result["tools"].([]any)
			names := map[string]bool{}
			for _, item := range tools {
				obj := item.(map[string]any)
				name, _ := obj["name"].(string)
				names[name] = true
				if obj["description"] == "" {
					t.Errorf("tool %s has empty description", name)
				}
				if obj["inputSchema"] == nil {
					t.Errorf("tool %s has no inputSchema", name)
				}
			}
			for _, want := range []string{
				"list_sessions", "get_conversation", "get_request",
				"list_tool_calls", "export_session", "gateway_status",
				"search_conversations", "validate_tool_call", "classify_failure",
			} {
				if !names[want] {
					t.Errorf("tools/list missing %q", want)
				}
			}
		})
	}
}

func TestUnknownMethodAndTool(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			resp := tr.call(t, "bogus/method", nil)
			if errObj, ok := resp["error"].(map[string]any); !ok || errObj["code"] != float64(-32601) {
				t.Errorf("unknown method err = %v, want -32601", resp["error"])
			}

			resp = tr.call(t, "tools/call", map[string]any{"name": "not_a_tool"})
			if errObj, ok := resp["error"].(map[string]any); !ok || errObj["code"] != float64(-32602) {
				t.Errorf("unknown tool err = %v, want -32602", resp["error"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tool happy paths over both transports
// ---------------------------------------------------------------------------

func TestToolsHappyPaths(t *testing.T) {
	st := openTestStore(t)
	seedTestStore(t, st)
	s := New(st, "")

	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			cases := []struct {
				name   string
				method string
				params map[string]any
				check  func(t *testing.T, res callResult)
			}{
				{"list_sessions", "list_sessions", nil, checkListSessions},
				{"list_sessions_filtered", "list_sessions", map[string]any{"alias": "openai"}, checkListSessionsFiltered},
				{"list_sessions_status_error", "list_sessions", map[string]any{"status": "error"}, checkListSessionsStatusError},
				{"get_conversation", "get_conversation", map[string]any{"session_id": "sess-a"}, checkGetConversation},
				{"get_conversation_include_full", "get_conversation", map[string]any{"session_id": "sess-a", "include_full": true}, checkGetConversationFull},
				{"get_request", "get_request", map[string]any{"session_id": "sess-a", "seq": 2}, checkGetRequest},
				{"list_tool_calls", "list_tool_calls", map[string]any{"session_id": "sess-a"}, checkListToolCalls},
				{"list_tool_calls_verdict", "list_tool_calls", map[string]any{"verdict": "BLIND_ERROR"}, checkListToolCallsVerdict},
				{"export_session", "export_session", map[string]any{"session_id": "sess-a"}, checkExportSession},
				{"gateway_status", "gateway_status", nil, checkGatewayStatus},
				{"search_conversations", "search_conversations", map[string]any{"query": "noodles"}, checkSearchNoodles},
				{"search_conversations_limit", "search_conversations", map[string]any{"query": "e", "limit": 1}, checkSearchLimit},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					resp := tr.call(t, "tools/call", map[string]any{"name": c.method, "arguments": c.params})
					if resp["error"] != nil {
						t.Fatalf("JSON-RPC error: %v", resp["error"])
					}
					res := toolResult(t, resp)
					if res.isError {
						t.Fatalf("unexpected isError: %s", res.text)
					}
					c.check(t, res)
				})
			}
		})
	}
}

func checkListSessions(t *testing.T, res callResult) {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(res.text), &list); err != nil {
		t.Fatalf("list_sessions text not JSON: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d sessions, want 2", len(list))
	}
	if list[0]["id"] != "sess-b" || list[1]["id"] != "sess-a" {
		t.Errorf("ordering = [%v, %v], want [sess-b, sess-a]", list[0]["id"], list[1]["id"])
	}
	// §5 failure counters on the summary.
	if list[1]["request_count"] != float64(3) || list[1]["tool_call_count"] != float64(2) || list[1]["failure_count"] != float64(1) {
		t.Errorf("sess-a counters = %v/%v/%v, want 3/2/1",
			list[1]["request_count"], list[1]["tool_call_count"], list[1]["failure_count"])
	}
}

func checkListSessionsFiltered(t *testing.T, res callResult) {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(res.text), &list); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(list) != 1 || list[0]["id"] != "sess-a" {
		t.Errorf("alias=openai filter = %v, want only sess-a", list)
	}
}

func checkListSessionsStatusError(t *testing.T, res callResult) {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(res.text), &list); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(list) != 1 || list[0]["id"] != "sess-a" {
		t.Errorf("status=error filter = %v, want only sess-a", list)
	}
}

func checkGetConversation(t *testing.T, res callResult) {
	t.Helper()
	var conv struct {
		SessionID string `json:"session_id"`
		Messages  []struct {
			Role       string           `json:"role"`
			Content    any              `json:"content"`
			ToolCallID string           `json:"tool_call_id"`
			IsError    bool             `json:"is_error"`
			ToolCalls  []replayToolCall `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(res.text), &conv); err != nil {
		t.Fatalf("conversation text not JSON: %v (%q)", err, res.text)
	}
	if conv.SessionID != "sess-a" {
		t.Errorf("session_id = %q", conv.SessionID)
	}
	roles := make([]string, 0, len(conv.Messages))
	for _, m := range conv.Messages {
		roles = append(roles, m.Role)
	}
	wantRoles := []string{"user", "assistant", "tool", "tool", "user", "assistant", "user"}
	if strings.Join(roles, ",") != strings.Join(wantRoles, ",") {
		t.Errorf("roles = %v, want %v", roles, wantRoles)
	}

	asst := conv.Messages[1]
	if len(asst.ToolCalls) != 2 {
		t.Fatalf("assistant tool_calls = %d, want 2", len(asst.ToolCalls))
	}
	first := asst.ToolCalls[0]
	if first.ID != "call_1" || first.ToolName != "get_weather" {
		t.Errorf("first tool call = %+v", first)
	}
	if first.ToolCallRowID != "req-1/0" {
		t.Errorf("tool_call_row_id = %q, want req-1/0 (Phase 7 hook)", first.ToolCallRowID)
	}
	if first.Verdict != "BLIND_ERROR" {
		t.Errorf("verdict = %q, want BLIND_ERROR", first.Verdict)
	}
	if string(first.Arguments) != `{"city":"Paris"}` {
		t.Errorf("arguments = %s, want parsed compact json", first.Arguments)
	}

	// Tool results interleaved after the calls, in request-body order.
	tr1, tr2 := conv.Messages[2], conv.Messages[3]
	if tr1.ToolCallID != "call_2" || tr1.IsError {
		t.Errorf("first tool result = %+v", tr1)
	}
	if tr2.ToolCallID != "call_1" || !tr2.IsError {
		t.Errorf("second tool result = %+v", tr2)
	}
}

func checkGetConversationFull(t *testing.T, res callResult) {
	t.Helper()
	var conv struct {
		Requests []struct {
			Seq      int             `json:"seq"`
			Request  json.RawMessage `json:"request"`
			Response json.RawMessage `json:"response"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(res.text), &conv); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(conv.Requests) != 3 {
		t.Fatalf("turns = %d, want 3", len(conv.Requests))
	}
	if conv.Requests[1].Seq != 2 || len(conv.Requests[1].Request) == 0 || len(conv.Requests[1].Response) == 0 {
		t.Errorf("include_full turn 2 = %+v", conv.Requests[1])
	}
	if !strings.Contains(string(conv.Requests[1].Response), "18C") {
		t.Errorf("full response missing assistant text: %s", conv.Requests[1].Response)
	}
}

func checkGetRequest(t *testing.T, res callResult) {
	t.Helper()
	var req struct {
		SessionID       string          `json:"session_id"`
		Seq             int             `json:"seq"`
		Request         json.RawMessage `json:"request"`
		Response        json.RawMessage `json:"response"`
		RequestFiltered json.RawMessage `json:"request_filtered"`
		Status          int             `json:"status_code"`
	}
	if err := json.Unmarshal([]byte(res.text), &req); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if req.SessionID != "sess-a" || req.Seq != 2 {
		t.Errorf("identity = %q/%d", req.SessionID, req.Seq)
	}
	if !strings.Contains(string(req.Request), "call_1") || !strings.Contains(string(req.Response), "18C") {
		t.Errorf("payloads = %s / %s", req.Request, req.Response)
	}
	if len(req.RequestFiltered) != 0 && string(req.RequestFiltered) != "null" {
		t.Errorf("request_filtered = %s, want null (no plugins)", req.RequestFiltered)
	}
	if req.Status != 200 {
		t.Errorf("status_code = %d", req.Status)
	}
}

func checkListToolCalls(t *testing.T, res callResult) {
	t.Helper()
	var calls []map[string]any
	if err := json.Unmarshal([]byte(res.text), &calls); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(calls))
	}
	first := calls[0]
	if first["id"] != "req-1/0" || first["tool_name"] != "get_weather" || first["verdict"] != "BLIND_ERROR" {
		t.Errorf("first tool call = %v", first)
	}
	args, _ := first["arguments"].(map[string]any)
	if args["city"] != "Paris" {
		t.Errorf("arguments = %v", first["arguments"])
	}
	if calls[1]["tool_name"] != "get_time" {
		t.Errorf("second tool call = %v", calls[1])
	}
}

func checkListToolCallsVerdict(t *testing.T, res callResult) {
	t.Helper()
	var calls []map[string]any
	if err := json.Unmarshal([]byte(res.text), &calls); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(calls) != 1 || calls[0]["id"] != "req-1/0" {
		t.Errorf("verdict=BLIND_ERROR = %v", calls)
	}
}

func checkExportSession(t *testing.T, res callResult) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(res.text, "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d export lines, want 9: %v", len(lines), lines)
	}
	var first struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line not JSON: %v", err)
	}
	if first.Type != "session_start" || first.SessionID != "sess-a" {
		t.Errorf("first line = %+v", first)
	}
	wantTypes := []string{
		"session_start",
		"request", "tool_call", "tool_call",
		"request", "tool_result", "tool_result",
		"request", "error",
	}
	for i, want := range wantTypes {
		var rec struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Type != want {
			t.Errorf("line %d type = %q, want %q", i, rec.Type, want)
		}
	}
}

func checkGatewayStatus(t *testing.T, res callResult) {
	t.Helper()
	var st struct {
		StorePath      string   `json:"store_path"`
		SessionCount   int      `json:"session_count"`
		RequestCount   int      `json:"request_count"`
		ToolCallCount  int      `json:"tool_call_count"`
		FailureCount   int      `json:"failure_count"`
		DiskUsageBytes int64    `json:"disk_usage_bytes"`
		RetentionDays  int      `json:"retention_days"`
		Aliases        []string `json:"aliases"`
	}
	if err := json.Unmarshal([]byte(res.text), &st); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if st.StorePath == "" || st.SessionCount != 2 || st.RequestCount != 4 ||
		st.ToolCallCount != 2 || st.FailureCount != 1 || st.DiskUsageBytes <= 0 {
		t.Errorf("status = %+v", st)
	}
	if len(st.Aliases) != 2 || st.Aliases[0] != "openai" || st.Aliases[1] != "anthropic" {
		t.Errorf("aliases = %v, want [openai anthropic]", st.Aliases)
	}
}

func checkSearchNoodles(t *testing.T, res callResult) {
	t.Helper()
	var results []map[string]any
	if err := json.Unmarshal([]byte(res.text), &results); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(results) != 1 || results[0]["session_id"] != "sess-b" {
		t.Errorf("search noodles = %v", results)
	}
	if !strings.Contains(results[0]["snippet"].(string), "noodles") {
		t.Errorf("snippet = %v", results[0]["snippet"])
	}
}

func checkSearchLimit(t *testing.T, res callResult) {
	t.Helper()
	var results []map[string]any
	if err := json.Unmarshal([]byte(res.text), &results); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("limit=1 search returned %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// Tool error paths over both transports (never throw)
// ---------------------------------------------------------------------------

func TestToolsErrorPaths(t *testing.T) {
	st := openTestStore(t)
	seedTestStore(t, st)
	s := New(st, "")

	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			cases := []struct {
				name   string
				method string
				params map[string]any
				code   string
			}{
				{"list_sessions_bad_status", "list_sessions", map[string]any{"status": "banana"}, "INVALID_ARGUMENT"},
				{"list_sessions_bad_limit", "list_sessions", map[string]any{"limit": -1}, "INVALID_ARGUMENT"},
				{"get_conversation_missing_session", "get_conversation", map[string]any{}, "INVALID_ARGUMENT"},
				{"get_conversation_unknown_session", "get_conversation", map[string]any{"session_id": "nope"}, "NOT_FOUND"},
				{"get_request_missing_seq", "get_request", map[string]any{"session_id": "sess-a"}, "INVALID_ARGUMENT"},
				{"get_request_unknown", "get_request", map[string]any{"session_id": "sess-a", "seq": 99}, "NOT_FOUND"},
				{"get_request_bad_seq", "get_request", map[string]any{"session_id": "sess-a", "seq": -1}, "INVALID_ARGUMENT"},
				{"list_tool_calls_bad_verdict", "list_tool_calls", map[string]any{"verdict": "MAYBE"}, "INVALID_ARGUMENT"},
				{"export_session_unknown", "export_session", map[string]any{"session_id": "nope"}, "NOT_FOUND"},
				{"search_conversations_missing_query", "search_conversations", map[string]any{}, "INVALID_ARGUMENT"},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					resp := tr.call(t, "tools/call", map[string]any{
						"name":      c.method,
						"arguments": c.params,
					})
					if resp["error"] != nil {
						t.Fatalf("tools/call should not throw JSON-RPC error: %v", resp["error"])
					}
					res := toolResult(t, resp)
					assertToolError(t, res, c.code)
				})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// --session scoping
// ---------------------------------------------------------------------------

func TestSessionScope(t *testing.T) {
	st := openTestStore(t)
	seedTestStore(t, st)
	s := New(st, "sess-a")

	for _, tr := range testTransports(t, s) {
		t.Run(tr.name, func(t *testing.T) {
			// list_sessions returns only the scoped session.
			resp := tr.call(t, "tools/call", map[string]any{"name": "list_sessions", "arguments": map[string]any{}})
			res := toolResult(t, resp)
			var list []map[string]any
			if err := json.Unmarshal([]byte(res.text), &list); err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || list[0]["id"] != "sess-a" {
				t.Errorf("scoped list_sessions = %v", list)
			}

			// get_request with a foreign session_id still resolves the scope.
			resp = tr.call(t, "tools/call", map[string]any{"name": "get_request", "arguments": map[string]any{"session_id": "sess-b", "seq": 1}})
			res = toolResult(t, resp)
			var req struct {
				SessionID string `json:"session_id"`
				RequestID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(res.text), &req); err != nil {
				t.Fatal(err)
			}
			if req.SessionID != "sess-a" || req.RequestID != "req-1" {
				t.Errorf("scoped get_request = %+v, want sess-a/req-1", req)
			}

			// list_tool_calls is forced to the scope.
			resp = tr.call(t, "tools/call", map[string]any{"name": "list_tool_calls", "arguments": map[string]any{}})
			res = toolResult(t, resp)
			var calls []map[string]any
			if err := json.Unmarshal([]byte(res.text), &calls); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 {
				t.Errorf("scoped list_tool_calls = %v, want 2 calls in sess-a", calls)
			}

			// get_conversation without session_id works via the scope.
			resp = tr.call(t, "tools/call", map[string]any{"name": "get_conversation", "arguments": map[string]any{}})
			res = toolResult(t, resp)
			if res.isError {
				t.Fatalf("scoped get_conversation failed: %s", res.text)
			}

			// search_conversations post-filters to the scope.
			resp = tr.call(t, "tools/call", map[string]any{"name": "search_conversations", "arguments": map[string]any{"query": "noodles"}})
			res = toolResult(t, resp)
			var results []map[string]any
			if err := json.Unmarshal([]byte(res.text), &results); err != nil {
				t.Fatal(err)
			}
			if len(results) != 0 {
				t.Errorf("scoped search found %v, want none (noodles is in sess-b)", results)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// streamable-http specifics
// ---------------------------------------------------------------------------

func TestHTTPRejectsNonJSON(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	r, err := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHTTPRejectsOversizedBody verifies handleHTTP caps the request body
// instead of reading it unboundedly.
func TestHTTPRejectsOversizedBody(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// A body larger than maxRequestBody: pad a valid JSON-RPC request with a
	// huge string parameter so it would decode fine if read fully.
	pad := strings.Repeat("x", maxRequestBody+1)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping", "params": map[string]any{"pad": pad}})
	if len(body) <= maxRequestBody {
		t.Fatalf("test body (%d bytes) does not exceed the cap (%d)", len(body), maxRequestBody)
	}
	r, err := http.NewRequest("POST", ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	rb, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rb), "too large") {
		t.Errorf("error body = %q, want a 'too large' message", rb)
	}
}

// TestHTTPNoCORS verifies the streamable-http endpoint sends no CORS headers:
// a browser page must never be able to read captured traffic cross-origin
// (non-browser MCP clients don't need CORS).
func TestHTTPNoCORS(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// OPTIONS (the would-be preflight) is answered with method negotiation only.
	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want 204", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("OPTIONS Allow = %q, want POST listed", allow)
	}
	for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("OPTIONS response must not set %s (got %q)", h, v)
		}
	}

	// POST responses carry no CORS headers either.
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if v := resp.Header.Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("POST response must not set Access-Control-Allow-Origin (got %q)", v)
	}
}

func TestHTTPNotificationSSE(t *testing.T) {
	st := openTestStore(t)
	s := New(st, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	r, err := http.NewRequest("POST", ts.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("read sse line: %v", err)
	}
	if !strings.Contains(line, "connected") {
		t.Errorf("first sse line = %q, want a connected comment", line)
	}
	// Closing the body cancels the server-side stream (no goroutine leak).
}
