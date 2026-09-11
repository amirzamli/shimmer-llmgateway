package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenCreatesDBWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	if _, err := Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// W1: the store holds request payloads; it must not be world-readable.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("store file mode = %o, want 0600", perm)
	}
}

func TestNormalizeSessionID(t *testing.T) {
	valid := []string{
		"", // empty → generated (still returns a non-empty id)
		"abc123",
		"sess-1",
		"conv_abc",
		"my.session:1",
		"a1b2c3-d4e5-f6a7-b8c9-d0e1f2a3b4c5",
	}
	for _, id := range valid {
		got := NormalizeSessionID(id)
		if got == "" {
			t.Errorf("NormalizeSessionID(%q) returned empty", id)
		}
		if id != "" && got != id {
			t.Errorf("NormalizeSessionID(%q) = %q, want verbatim passthrough", id, got)
		}
	}
	// Anything outside [A-Za-z0-9._:-] (and overlong ids) is replaced by a
	// fresh UUID, never passed through.
	invalid := []string{
		"x;alert(1)",
		"x' onload=alert(1)",
		"<script>alert(1)</script>",
		"space id",
		"emoji😀",
		strings.Repeat("a", 129),
	}
	for _, id := range invalid {
		got := NormalizeSessionID(id)
		if got == id {
			t.Errorf("NormalizeSessionID(%q) passed the unsafe id through", id)
		}
		if !strings.Contains(got, "-") {
			t.Errorf("NormalizeSessionID(%q) = %q, want a UUID-shaped replacement", id, got)
		}
	}
}

func TestCaptureNormalizesUnsafeSessionID(t *testing.T) {
	st := openTestStore(t)
	rec := baseRecord()
	rec.SessionID = "bad' session</script>"
	mustCapture(t, st, rec)
	if rec.SessionID == "bad' session</script>" {
		t.Fatal("Capture persisted the unsafe session id")
	}
	// The stored session uses the normalized id.
	sess, err := st.GetSession(context.Background(), rec.SessionID)
	if err != nil {
		t.Fatalf("GetSession(normalized id): %v", err)
	}
	if sess.ID != rec.SessionID {
		t.Errorf("stored session id = %q, want %q", sess.ID, rec.SessionID)
	}
}

func mustCapture(t *testing.T, st *Store, rec *CaptureRecord) {
	t.Helper()
	if err := st.Capture(context.Background(), rec); err != nil {
		t.Fatalf("Capture: %v", err)
	}
}

// baseRecord returns a minimal valid capture record with deterministic ids.
func baseRecord() *CaptureRecord {
	return &CaptureRecord{
		ID:           "req-1",
		SessionID:    "sess-1",
		CreatedAt:    time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		Alias:        "openai",
		Provider:     "openai",
		Model:        "gpt-4o",
		Endpoint:     "/v1/chat/completions",
		DurationMS:   1234,
		StatusCode:   200,
		FinishReason: "stop",
		RequestJSON:  json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		ResponseJSON: json.RawMessage(`{"id":"resp_1","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`),
	}
}

func TestFirstUserMessagePreview(t *testing.T) {
	wrapped := `{"body":{"messages":[{"role":"user","content":"wrapped prompt"}]}}`
	doubleEncoded, err := json.Marshal(`{"messages":[{"role":"user","content":"encoded prompt"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "chat completions", raw: []byte(`{"messages":[{"role":"system","content":"ignore"},{"role":"user","content":"hello"}]}`), want: "hello"},
		{name: "responses input", raw: []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"response prompt"}]}]}`), want: "response prompt"},
		{name: "input text item", raw: []byte(`{"input":[{"type":"input_text","text":"direct prompt"}]}`), want: "direct prompt"},
		{name: "wrapped body", raw: []byte(wrapped), want: "wrapped prompt"},
		{name: "double encoded", raw: doubleEncoded, want: "encoded prompt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := firstUserMessagePreview(test.raw); got != test.want {
				t.Fatalf("preview = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOpenCreatesSchemaAndPragmas(t *testing.T) {
	st := openTestStore(t)

	tables := map[string]bool{}
	rows, err := st.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables[name] = true
	}
	rows.Close()
	for _, want := range []string{"sessions", "requests", "tool_calls"} {
		if !tables[want] {
			t.Errorf("table %q missing", want)
		}
	}

	// Exact §5 column definitions.
	wantCols := map[string][]struct {
		name, typ   string
		notnull, pk bool
	}{
		"sessions": {
			{"id", "TEXT", false, true},
			{"created_at", "TEXT", false, false},
			{"first_alias", "TEXT", false, false},
			{"first_model", "TEXT", false, false},
			{"request_count", "INTEGER", false, false},
			{"tool_call_count", "INTEGER", false, false},
			{"failure_count", "INTEGER", false, false},
			{"expired", "INTEGER", false, false},
		},
		"requests": {
			{"id", "TEXT", false, true},
			{"session_id", "TEXT", true, false},
			{"seq", "INTEGER", false, false},
			{"created_at", "TEXT", false, false},
			{"alias", "TEXT", false, false},
			{"provider", "TEXT", false, false},
			{"model", "TEXT", false, false},
			{"endpoint", "TEXT", false, false},
			{"duration_ms", "INTEGER", false, false},
			{"status_code", "INTEGER", false, false},
			{"finish_reason", "TEXT", false, false},
			{"usage_json", "TEXT", false, false},
			{"request_json", "TEXT", false, false},
			{"request_filtered_json", "TEXT", false, false},
			{"response_json", "TEXT", false, false},
			{"response_filtered_json", "TEXT", false, false},
			{"plugins_applied", "TEXT", false, false},
			{"error_json", "TEXT", false, false},
			{"truncated", "INTEGER", false, false},
			{"prompt_tokens", "INTEGER", false, false},
			{"completion_tokens", "INTEGER", false, false},
			{"cached_tokens", "INTEGER", false, false},
			{"cost_input", "REAL", false, false},
			{"cost_output", "REAL", false, false},
			{"cost_cache_read", "REAL", false, false},
			{"cost_cache_write", "REAL", false, false},
			{"cost_total", "REAL", false, false},
			{"cost_priced", "INTEGER", false, false},
			{"cost_schema", "TEXT", false, false},
		},
		"tool_calls": {
			{"id", "TEXT", false, true},
			{"request_id", "TEXT", true, false},
			{"session_id", "TEXT", true, false},
			{"seq", "INTEGER", false, false},
			{"tool_name", "TEXT", false, false},
			{"arguments_json", "TEXT", false, false},
			{"result_is_error", "INTEGER", false, false},
			{"result_snippet", "TEXT", false, false},
			{"verdict", "TEXT", false, false},
			{"annotation_json", "TEXT", false, false},
		},
	}
	for table, cols := range wantCols {
		rows, err := st.db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatalf("PRAGMA table_info(%s): %v", table, err)
		}
		got := map[string][2]any{}
		for rows.Next() {
			var cid int
			var name, typ string
			var notnull, pk int
			var dflt sql.NullString
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			got[name] = [2]any{typ, notnull != 0}
			if name == "truncated" && table == "requests" && dflt.String != "0" {
				t.Errorf("requests.truncated default = %q, want 0", dflt.String)
			}
		}
		rows.Close()
		if len(got) != len(cols) {
			t.Errorf("%s has %d columns, want %d", table, len(got), len(cols))
		}
		for _, c := range cols {
			g, ok := got[c.name]
			if !ok {
				t.Errorf("%s: missing column %q", table, c.name)
				continue
			}
			if g[0] != c.typ || g[1] != c.notnull {
				t.Errorf("%s.%s = (type=%v, notnull=%v), want (type=%s, notnull=%v)",
					table, c.name, g[0], g[1], c.typ, c.notnull)
			}
		}
	}

	// The four §5 indexes.
	indexes := map[string]bool{}
	rows, err = st.db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes[name] = true
	}
	rows.Close()
	for _, want := range []string{
		"idx_sessions_created_at", "idx_requests_session_seq",
		"idx_tool_calls_session", "idx_tool_calls_tool_name",
		"idx_requests_usage",
	} {
		if !indexes[want] {
			t.Errorf("index %q missing", want)
		}
	}

	var journalMode, foreignKeys string
	if err := st.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}
	if foreignKeys != "1" {
		t.Errorf("foreign_keys = %q, want 1", foreignKeys)
	}
}

func TestCaptureRoundTrip(t *testing.T) {
	st := openTestStore(t)

	rec := baseRecord()
	rec.ID = "req-1"
	rec.SessionID = "sess-1"
	rec.Usage = json.RawMessage(`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)
	rec.RequestFilteredJSON = json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"REDACTED"}]}`)
	rec.ResponseFilteredJSON = json.RawMessage(`{"choices":[{"message":{"role":"assistant","content":"bye"}}]}`)
	rec.PluginsApplied = []string{"redact"}
	mustCapture(t, st, rec)

	if rec.Seq != 1 {
		t.Errorf("Capture set Seq = %d, want 1", rec.Seq)
	}

	r, err := st.GetRequest(context.Background(), "sess-1", 1)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if r.ID != "req-1" || r.SessionID != "sess-1" || r.Seq != 1 {
		t.Errorf("identity mismatch: %+v", r)
	}
	if r.Alias != "openai" || r.Provider != "openai" || r.Model != "gpt-4o" || r.Endpoint != "/v1/chat/completions" {
		t.Errorf("routing fields mismatch: %+v", r)
	}
	if r.DurationMS != 1234 || r.StatusCode != 200 || r.FinishReason != "stop" {
		t.Errorf("record fields mismatch: %+v", r)
	}
	if string(r.Usage) != `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}` {
		t.Errorf("usage = %s", r.Usage)
	}
	if string(r.RequestJSON) != string(rec.RequestJSON) ||
		string(r.RequestFilteredJSON) != string(rec.RequestFilteredJSON) ||
		string(r.ResponseJSON) != string(rec.ResponseJSON) ||
		string(r.ResponseFilteredJSON) != string(rec.ResponseFilteredJSON) {
		t.Errorf("payload round-trip mismatch")
	}
	if len(r.PluginsApplied) != 1 || r.PluginsApplied[0] != "redact" {
		t.Errorf("plugins_applied = %v", r.PluginsApplied)
	}
	if r.Error != nil {
		t.Errorf("error = %+v, want nil", r.Error)
	}
	if r.Truncated {
		t.Errorf("truncated = true, want false")
	}
	if !strings.HasSuffix(r.CreatedAt, "Z") {
		t.Errorf("created_at %q not ISO-8601 UTC", r.CreatedAt)
	}
	if _, err := time.Parse(tsLayout, r.CreatedAt); err != nil {
		t.Errorf("created_at %q unparseable: %v", r.CreatedAt, err)
	}

	sess, err := st.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.FirstAlias != "openai" || sess.FirstModel != "gpt-4o" {
		t.Errorf("first_alias/first_model = %q/%q", sess.FirstAlias, sess.FirstModel)
	}
	if sess.RequestCount != 1 || sess.ToolCallCount != 0 || sess.FailureCount != 0 {
		t.Errorf("counters = %d/%d/%d, want 1/0/0",
			sess.RequestCount, sess.ToolCallCount, sess.FailureCount)
	}
	if len(sess.Requests) != 1 || len(sess.ToolCalls) != 0 {
		t.Errorf("session children = %d requests, %d tool calls, want 1/0",
			len(sess.Requests), len(sess.ToolCalls))
	}
}

func TestCaptureNullableDefaults(t *testing.T) {
	st := openTestStore(t)

	rec := baseRecord()
	rec.SessionID = "sess-min"
	rec.Usage = nil
	rec.RequestFilteredJSON = nil
	rec.ResponseFilteredJSON = nil
	rec.PluginsApplied = nil
	rec.Error = nil
	mustCapture(t, st, rec)

	r, err := st.GetRequest(context.Background(), "sess-min", 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage != nil || r.RequestFilteredJSON != nil || r.ResponseFilteredJSON != nil {
		t.Errorf("nullable JSON should be nil, got usage=%q filtered=%q/%q",
			r.Usage, r.RequestFilteredJSON, r.ResponseFilteredJSON)
	}
	if r.PluginsApplied != nil {
		t.Errorf("plugins_applied = %v, want nil", r.PluginsApplied)
	}
	if r.Error != nil {
		t.Errorf("error = %+v, want nil", r.Error)
	}
	if r.Truncated {
		t.Errorf("truncated = true, want false")
	}
}

func TestCaptureErrorAndTruncated(t *testing.T) {
	st := openTestStore(t)

	failed := baseRecord()
	failed.ID = "req-err"
	failed.SessionID = "sess-1"
	failed.StatusCode = 502
	failed.Error = &ErrorInfo{Code: "UPSTREAM_ERROR", Message: "provider boom"}
	failed.ResponseJSON = nil
	mustCapture(t, st, failed)

	trunc := baseRecord()
	trunc.ID = "req-trunc"
	trunc.SessionID = "sess-1"
	trunc.Truncated = true
	mustCapture(t, st, trunc)

	r, err := st.GetRequest(context.Background(), "sess-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 502 || r.Error == nil || r.Error.Code != "UPSTREAM_ERROR" || r.Error.Message != "provider boom" {
		t.Errorf("error request mismatch: %+v", r)
	}

	r2, err := st.GetRequest(context.Background(), "sess-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Truncated {
		t.Errorf("truncated = false, want true")
	}
	if r2.Error != nil {
		t.Errorf("error = %+v, want nil", r2.Error)
	}

	sess, err := st.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestCount != 2 || sess.FailureCount != 1 {
		t.Errorf("counters = %d requests / %d failures, want 2/1",
			sess.RequestCount, sess.FailureCount)
	}
	// first_alias/first_model stay from the first request.
	if sess.FirstAlias != "openai" || sess.FirstModel != "gpt-4o" {
		t.Errorf("first_alias/first_model = %q/%q", sess.FirstAlias, sess.FirstModel)
	}
}

func TestCaptureSeqPerSession(t *testing.T) {
	st := openTestStore(t)

	for i, id := range []string{"r1", "r2", "r3"} {
		rec := baseRecord()
		rec.ID = id
		rec.SessionID = "sess-a"
		mustCapture(t, st, rec)
		if rec.Seq != i+1 {
			t.Errorf("sess-a %s seq = %d, want %d", id, rec.Seq, i+1)
		}
	}
	other := baseRecord()
	other.ID = "r4"
	other.SessionID = "sess-b"
	mustCapture(t, st, other)
	if other.Seq != 1 {
		t.Errorf("sess-b seq = %d, want 1", other.Seq)
	}

	sess, err := st.GetSession(context.Background(), "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if sess.RequestCount != 3 {
		t.Errorf("sess-a request_count = %d, want 3", sess.RequestCount)
	}
}

func TestToolCallExtractionAndLinking(t *testing.T) {
	st := openTestStore(t)

	// Request 1 emits two tool calls.
	req1 := baseRecord()
	req1.ID = "req-t1"
	req1.SessionID = "sess-t"
	req1.ResponseJSON = json.RawMessage(`{
		"id":"resp_1",
		"choices":[{
			"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{  \"city\": \"Paris\" }"}},
				{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}
			]},
			"finish_reason":"tool_calls"
		}]
	}`)
	mustCapture(t, st, req1)

	// Request 2 returns results for both, interleaved/out of call order.
	req2 := baseRecord()
	req2.ID = "req-t2"
	req2.SessionID = "sess-t"
	req2.RequestJSON = json.RawMessage(`{"model":"gpt-4o","messages":[
		{"role":"user","content":"weather?"},
		{"role":"tool","tool_call_id":"call_2","content":"{\"time\":\"12:00\"}"},
		{"role":"tool","tool_call_id":"call_1","content":"{\"error\":\"unknown city\"}"}
	]}`)
	req2.ResponseJSON = json.RawMessage(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	mustCapture(t, st, req2)

	sess, err := st.GetSession(context.Background(), "sess-t")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ToolCallCount != 2 {
		t.Errorf("session tool_call_count = %d, want 2", sess.ToolCallCount)
	}

	calls, err := st.ListToolCalls(context.Background(), ToolCallFilter{SessionID: "sess-t"})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(calls))
	}

	byID := map[string]*ToolCall{}
	for _, c := range calls {
		byID[c.ID] = c
	}

	c0, ok := byID["req-t1/0"]
	if !ok {
		t.Fatalf("missing tool call id req-t1/0; got %v", calls)
	}
	if c0.ToolName != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", c0.ToolName)
	}
	if string(c0.ArgumentsJSON) != `{"city":"Paris"}` {
		t.Errorf("arguments_json = %s, want parsed compact json", c0.ArgumentsJSON)
	}
	if c0.RequestID != "req-t1" || c0.SessionID != "sess-t" {
		t.Errorf("tool call identity = %+v", c0)
	}
	if !c0.ResultIsError {
		t.Errorf("get_weather result should be an error")
	}
	if c0.ResultSnippet != `{"error":"unknown city"}` {
		t.Errorf("get_weather snippet = %q", c0.ResultSnippet)
	}

	c1, ok := byID["req-t1/1"]
	if !ok {
		t.Fatalf("missing tool call id req-t1/1")
	}
	if c1.ToolName != "get_time" {
		t.Errorf("tool name = %q, want get_time", c1.ToolName)
	}
	if c1.ResultIsError {
		t.Errorf("get_time result should not be an error")
	}
	if c1.ResultSnippet != `{"time":"12:00"}` {
		t.Errorf("get_time snippet = %q", c1.ResultSnippet)
	}

	// Both calls are in conversation order seq 1.
	if c0.Seq != 1 || c1.Seq != 1 {
		t.Errorf("tool call seq = %d/%d, want 1/1", c0.Seq, c1.Seq)
	}
}

func TestToolCallResultUnlinked(t *testing.T) {
	st := openTestStore(t)

	req := baseRecord()
	req.ID = "req-u1"
	req.SessionID = "sess-u"
	req.ResponseJSON = json.RawMessage(`{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"call_x","type":"function","function":{"name":"noop","arguments":"{}"}}
	]},"finish_reason":"tool_calls"}]}`)
	mustCapture(t, st, req)

	calls, err := st.ListToolCalls(context.Background(), ToolCallFilter{SessionID: "sess-u"})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ResultSnippet != "" {
		t.Errorf("snippet = %q, want empty (no result linked)", calls[0].ResultSnippet)
	}
	if calls[0].ResultIsError {
		t.Errorf("result_is_error = true, want false")
	}
	if calls[0].Verdict != "" {
		t.Errorf("verdict = %q, want empty until inspection caches it", calls[0].Verdict)
	}
}

func TestListSessionsFilters(t *testing.T) {
	st := openTestStore(t)

	// session A: alias openai, model gpt-4o, one success + one failure.
	ra := baseRecord()
	ra.SessionID = "sess-a"
	ra.ID = "ra1"
	ra.Alias, ra.Provider, ra.Model = "openai", "openai", "gpt-4o"
	ra.CreatedAt = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ra.RequestJSON = json.RawMessage(`{"messages":[{"role":"user","content":"sunny day forecast"}]}`)
	mustCapture(t, st, ra)

	ra2 := baseRecord()
	ra2.SessionID = "sess-a"
	ra2.ID = "ra2"
	ra2.Alias, ra2.Provider, ra2.Model = "openai", "openai", "gpt-4o"
	ra2.CreatedAt = time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	ra2.StatusCode = 500
	ra2.Error = &ErrorInfo{Code: "UPSTREAM_ERROR", Message: "boom"}
	mustCapture(t, st, ra2)

	// session B: alias anthropic, model claude, ok.
	rb := baseRecord()
	rb.SessionID = "sess-b"
	rb.ID = "rb1"
	rb.Alias, rb.Provider, rb.Model = "anthropic", "anthropic", "claude-3-5-sonnet-latest"
	rb.CreatedAt = time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	rb.RequestJSON = json.RawMessage(`{"messages":[{"role":"user","content":"recipe for noodles"}]}`)
	mustCapture(t, st, rb)

	ids := func(list []*SessionSummary) map[string]bool {
		m := map[string]bool{}
		for _, s := range list {
			m[s.ID] = true
		}
		return m
	}

	all, err := st.ListSessions(context.Background(), SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("all sessions = %d, want 2", len(all))
	}

	byAlias, err := st.ListSessions(context.Background(), SessionFilter{Alias: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if m := ids(byAlias); !m["sess-a"] || m["sess-b"] {
		t.Errorf("alias filter = %v, want only sess-a", m)
	}

	byModel, err := st.ListSessions(context.Background(), SessionFilter{Model: "claude-3-5-sonnet-latest"})
	if err != nil {
		t.Fatal(err)
	}
	if m := ids(byModel); !m["sess-b"] || m["sess-a"] {
		t.Errorf("model filter = %v, want only sess-b", m)
	}

	byErr, err := st.ListSessions(context.Background(), SessionFilter{Status: "error"})
	if err != nil {
		t.Fatal(err)
	}
	if m := ids(byErr); !m["sess-a"] || m["sess-b"] {
		t.Errorf("status=error filter = %v, want only sess-a", m)
	}

	byOK, err := st.ListSessions(context.Background(), SessionFilter{Status: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if m := ids(byOK); !m["sess-b"] || m["sess-a"] {
		t.Errorf("status=ok filter = %v, want only sess-b", m)
	}

	since := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 1, 11, 30, 0, 0, time.UTC)
	bySince, err := st.ListSessions(context.Background(), SessionFilter{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if m := ids(bySince); !m["sess-b"] || m["sess-a"] {
		t.Errorf("since filter = %v, want only sess-b", m)
	}
	byUntil, err := st.ListSessions(context.Background(), SessionFilter{Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if m := ids(byUntil); !m["sess-a"] || m["sess-b"] {
		t.Errorf("until filter = %v, want only sess-a", m)
	}

	bySearch, err := st.ListSessions(context.Background(), SessionFilter{Search: "noodles"})
	if err != nil {
		t.Fatal(err)
	}
	if m := ids(bySearch); !m["sess-b"] || m["sess-a"] {
		t.Errorf("search filter = %v, want only sess-b", m)
	}
	// LIKE wildcards are escaped, so this should match nothing.
	byWild, err := st.ListSessions(context.Background(), SessionFilter{Search: "100%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byWild) != 0 {
		t.Errorf("wildcard search matched %v, want none", ids(byWild))
	}

	// Newest first.
	if all[0].ID != "sess-b" || all[1].ID != "sess-a" {
		t.Errorf("ordering = [%s, %s], want [sess-b, sess-a]", all[0].ID, all[1].ID)
	}

	limited, err := st.ListSessions(context.Background(), SessionFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Errorf("limit=1 returned %d", len(limited))
	}
}

func TestListToolCallsFiltersAndSetVerdict(t *testing.T) {
	st := openTestStore(t)

	req := baseRecord()
	req.ID = "req-v"
	req.SessionID = "sess-v"
	req.ResponseJSON = json.RawMessage(`{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"X\"}"}},
		{"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{}"}}
	]},"finish_reason":"tool_calls"}]}`)
	mustCapture(t, st, req)

	// Set verdict + annotation on call_a.
	annot := json.RawMessage(`{"why":"missing required param","nearest_tool":"get_forecast"}`)
	if err := st.SetVerdict(context.Background(), "req-v/0", "BLIND_ERROR", annot); err != nil {
		t.Fatalf("SetVerdict: %v", err)
	}

	byVerdict, err := st.ListToolCalls(context.Background(), ToolCallFilter{Verdict: "BLIND_ERROR"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byVerdict) != 1 || byVerdict[0].ID != "req-v/0" {
		t.Errorf("verdict filter = %v, want [req-v/0]", byVerdict)
	}
	if string(byVerdict[0].AnnotationJSON) != string(annot) {
		t.Errorf("annotation = %s, want %s", byVerdict[0].AnnotationJSON, annot)
	}

	byName, err := st.ListToolCalls(context.Background(), ToolCallFilter{ToolName: "get_time"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byName) != 1 || byName[0].ToolName != "get_time" {
		t.Errorf("tool_name filter = %v", byName)
	}

	limited, err := st.ListToolCalls(context.Background(), ToolCallFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Errorf("limit=1 returned %d", len(limited))
	}

	// GetToolCall direct lookup.
	tc, err := st.GetToolCall(context.Background(), "req-v/0")
	if err != nil {
		t.Fatal(err)
	}
	if tc.Verdict != "BLIND_ERROR" {
		t.Errorf("GetToolCall verdict = %q", tc.Verdict)
	}

	// Invalid verdict rejected, unknown id -> ErrNotFound.
	if err := st.SetVerdict(context.Background(), "req-v/0", "MAYBE", nil); err == nil {
		t.Errorf("SetVerdict with invalid verdict succeeded")
	}
	if err := st.SetVerdict(context.Background(), "nope/0", "SUCCESS", nil); err != ErrNotFound {
		t.Errorf("SetVerdict unknown id err = %v, want ErrNotFound", err)
	}
}

func TestSearchConversations(t *testing.T) {
	st := openTestStore(t)

	rec := baseRecord()
	rec.ID = "req-s1"
	rec.SessionID = "sess-s"
	rec.RequestJSON = json.RawMessage(`{"messages":[{"role":"user","content":"tell me about the Eiffel Tower at midnight"}]}`)
	mustCapture(t, st, rec)

	rec2 := baseRecord()
	rec2.ID = "req-s2"
	rec2.SessionID = "sess-s"
	rec2.RequestJSON = json.RawMessage(`{"messages":[{"role":"user","content":"what about Tokyo"}]}`)
	mustCapture(t, st, rec2)

	res, err := st.SearchConversations(context.Background(), "Eiffel", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if res[0].RequestID != "req-s1" || res[0].SessionID != "sess-s" {
		t.Errorf("result identity = %+v", res[0])
	}
	if !strings.Contains(res[0].Snippet, "Eiffel") {
		t.Errorf("snippet %q missing query", res[0].Snippet)
	}

	// Case-insensitive LIKE.
	res, err = st.SearchConversations(context.Background(), "tokyo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].RequestID != "req-s2" {
		t.Errorf("case-insensitive search = %+v", res)
	}

	// No match.
	res, err = st.SearchConversations(context.Background(), "aardvark", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("no-match search returned %d results", len(res))
	}
}

func TestExportSessionShape(t *testing.T) {
	st := openTestStore(t)

	// Request 1: ok, no tools.
	r1 := baseRecord()
	r1.ID = "req-e1"
	r1.SessionID = "sess-e"
	r1.PluginsApplied = []string{"redact"}
	r1.RequestFilteredJSON = json.RawMessage(`{"messages":[{"role":"user","content":"hello"}]}`)
	r1.ResponseFilteredJSON = json.RawMessage(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	mustCapture(t, st, r1)

	// Request 2: emits two tool calls.
	r2 := baseRecord()
	r2.ID = "req-e2"
	r2.SessionID = "sess-e"
	r2.ResponseJSON = json.RawMessage(`{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"call_e1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"}},
		{"id":"call_e2","type":"function","function":{"name":"get_time","arguments":"{}"}}
	]},"finish_reason":"tool_calls"}]}`)
	mustCapture(t, st, r2)

	// Request 3: carries results and fails.
	r3 := baseRecord()
	r3.ID = "req-e3"
	r3.SessionID = "sess-e"
	r3.StatusCode = 500
	r3.Error = &ErrorInfo{Code: "UPSTREAM_ERROR", Message: "boom"}
	r3.RequestJSON = json.RawMessage(`{"messages":[
		{"role":"tool","tool_call_id":"call_e1","content":"{\"temp\":18}"},
		{"role":"tool","tool_call_id":"call_e2","content":"{\"time\":\"09:00\"}"}
	]}`)
	mustCapture(t, st, r3)

	var sb strings.Builder
	if err := st.ExportSession(context.Background(), "sess-e", &sb); err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	if len(lines) != 9 {
		t.Fatalf("got %d export lines, want 9: %v", len(lines), lines)
	}

	type rec struct {
		Type      string `json:"type"`
		TS        string `json:"ts"`
		SessionID string `json:"session_id"`
		RequestID string `json:"request_id"`
		Alias     any    `json:"alias"`
		Plugins   any    `json:"plugins"`
	}
	parsed := make([]rec, len(lines))
	for i, l := range lines {
		if err := json.Unmarshal([]byte(l), &parsed[i]); err != nil {
			t.Fatalf("line %d not JSON: %v (%q)", i, err, l)
		}
		r := parsed[i]
		if r.Type == "" || r.TS == "" || r.SessionID != "sess-e" {
			t.Errorf("line %d envelope missing fields: %+v", i, r)
		}
	}

	wantTypes := []string{
		"session_start",
		"request",                           // req-e1
		"request", "tool_call", "tool_call", // req-e2 + its two tool calls
		"request", "error", "tool_result", "tool_result", // req-e3 + error + results
	}
	for i, want := range wantTypes {
		if parsed[i].Type != want {
			t.Errorf("line %d type = %q, want %q", i, parsed[i].Type, want)
		}
	}

	// Records other than session_start carry a request_id.
	for i := 1; i < len(parsed); i++ {
		if parsed[i].RequestID == "" {
			t.Errorf("line %d missing request_id", i)
		}
	}
	if parsed[0].RequestID != "" {
		t.Errorf("session_start has request_id %q, want none", parsed[0].RequestID)
	}

	// Tool-call line carries tool name + parsed arguments.
	var tcLine map[string]any
	if err := json.Unmarshal([]byte(lines[3]), &tcLine); err != nil {
		t.Fatal(err)
	}
	if tcLine["tool_name"] != "get_weather" || tcLine["tool_call_id"] != "call_e1" {
		t.Errorf("tool_call line = %v", tcLine)
	}
	if args, ok := tcLine["arguments"].(map[string]any); !ok || args["city"] != "Rome" {
		t.Errorf("tool_call arguments = %v, want parsed object", tcLine["arguments"])
	}

	// Error line carries code/message/status.
	var errLine map[string]any
	if err := json.Unmarshal([]byte(lines[6]), &errLine); err != nil {
		t.Fatal(err)
	}
	if errLine["code"] != "UPSTREAM_ERROR" || errLine["message"] != "boom" || errLine["status_code"] != float64(500) {
		t.Errorf("error line = %v", errLine)
	}

	// Tool-result lines carry content + is_error + tool_call_id.
	var trLine map[string]any
	if err := json.Unmarshal([]byte(lines[7]), &trLine); err != nil {
		t.Fatal(err)
	}
	if trLine["tool_call_id"] != "call_e1" || trLine["content"] != `{"temp":18}` || trLine["is_error"] != false {
		t.Errorf("tool_result line = %v", trLine)
	}

	// Unknown session -> ErrNotFound.
	if err := st.ExportSession(context.Background(), "missing", &strings.Builder{}); err != ErrNotFound {
		t.Errorf("export unknown session err = %v, want ErrNotFound", err)
	}
}

func TestListAliases(t *testing.T) {
	st := openTestStore(t)

	a := baseRecord()
	a.SessionID = "sess-a"
	a.ID = "ra1"
	a.Alias = "openai"
	a.CreatedAt = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	mustCapture(t, st, a)

	b := baseRecord()
	b.SessionID = "sess-b"
	b.ID = "rb1"
	b.Alias = "anthropic"
	b.CreatedAt = time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	mustCapture(t, st, b)

	c := baseRecord()
	c.SessionID = "sess-c"
	c.ID = "rc1"
	c.Alias = "openai" // same alias, later
	c.CreatedAt = time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	mustCapture(t, st, c)

	aliases, err := st.ListAliases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Distinct aliases, in order of first appearance.
	if len(aliases) != 2 || aliases[0] != "openai" || aliases[1] != "anthropic" {
		t.Errorf("aliases = %v, want [openai anthropic]", aliases)
	}
}

func TestPurge(t *testing.T) {
	st := openTestStore(t)

	// Old session A with a tool call.
	a := baseRecord()
	a.ID = "req-p1"
	a.SessionID = "sess-old"
	a.ResponseJSON = json.RawMessage(`{"choices":[{"message":{"role":"assistant","tool_calls":[
		{"id":"call_p","type":"function","function":{"name":"noop","arguments":"{}"}}
	]},"finish_reason":"tool_calls"}]}`)
	mustCapture(t, st, a)
	oldTS := formatTS(time.Now().UTC().AddDate(0, 0, -10))
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, oldTS, "sess-old"); err != nil {
		t.Fatal(err)
	}

	// Fresh session B. baseRecord's CreatedAt is a fixed date, so both
	// sessions are pinned relative to now: the assertion must hold on any
	// date, not just before the fixed date ages past the "old" one.
	b := baseRecord()
	b.ID = "req-p2"
	b.SessionID = "sess-new"
	mustCapture(t, st, b)
	newTS := formatTS(time.Now().UTC())
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, newTS, "sess-new"); err != nil {
		t.Fatal(err)
	}

	st.SetRetention(1)
	n, err := st.Purge(context.Background())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("Purge expired %d sessions, want 1", n)
	}

	// The old session is expired, not deleted: metadata and usage stats
	// remain, payloads and tool calls are gone.
	sess, err := st.GetSession(context.Background(), "sess-old")
	if err != nil {
		t.Fatalf("expired session missing: %v", err)
	}
	if !sess.Expired {
		t.Error("expired session not flagged")
	}
	if sess.RequestCount != 1 {
		t.Errorf("expired session request_count = %d, want 1", sess.RequestCount)
	}
	req, err := st.GetRequest(context.Background(), "sess-old", 1)
	if err != nil {
		t.Fatalf("expired request missing: %v", err)
	}
	if len(req.RequestJSON) != 0 || len(req.ResponseJSON) != 0 {
		t.Error("expired session still carries payloads")
	}
	if req.Alias != "openai" || req.Model != "gpt-4o" || req.StatusCode != 200 {
		t.Errorf("expired request metadata lost: %+v", req)
	}

	// The fresh session is untouched.
	fresh, err := st.GetSession(context.Background(), "sess-new")
	if err != nil {
		t.Errorf("fresh session purged: %v", err)
	}
	if err == nil && fresh.Expired {
		t.Error("fresh session flagged expired")
	}
	freshReq, err := st.GetRequest(context.Background(), "sess-new", 1)
	if err != nil {
		t.Fatalf("fresh request missing: %v", err)
	}
	if len(freshReq.RequestJSON) == 0 {
		t.Error("fresh session lost its payloads")
	}

	// Tool calls of expired sessions are removed.
	var cnt int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM tool_calls`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Errorf("tool_calls after purge = %d, want 0", cnt)
	}
	// Both requests rows remain for the usage aggregates.
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Errorf("requests after purge = %d, want 2", cnt)
	}

	// A second pass expires nothing new (the expired flag makes it a no-op).
	n, err = st.Purge(context.Background())
	if err != nil || n != 0 {
		t.Errorf("second purge = (%d, %v), want (0, nil)", n, err)
	}

	// Disabled purge is a no-op.
	st.SetRetention(0)
	n, err = st.Purge(context.Background())
	if err != nil || n != 0 {
		t.Errorf("disabled purge = (%d, %v), want (0, nil)", n, err)
	}
}

// TestPurgeInvokesJSONLPurger verifies the §8 append-log trimmer is called
// with the purge cutoff (W1: JSONL data must not outlive its SQLite rows),
// and that a failing trimmer surfaces an error.
func TestPurgeInvokesJSONLPurger(t *testing.T) {
	st := openTestStore(t)

	old := baseRecord()
	old.ID = "req-jsonl-old"
	old.SessionID = "sess-jsonl-old"
	mustCapture(t, st, old)
	oldTS := formatTS(time.Now().UTC().AddDate(0, 0, -10))
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, oldTS, "sess-jsonl-old"); err != nil {
		t.Fatal(err)
	}
	recent := baseRecord()
	recent.ID = "req-jsonl-new"
	recent.SessionID = "sess-jsonl-new"
	mustCapture(t, st, recent)
	newTS := formatTS(time.Now().UTC())
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, newTS, "sess-jsonl-new"); err != nil {
		t.Fatal(err)
	}

	var cutoffs []string
	st.SetJSONLPurger(func(cutoff string) error {
		cutoffs = append(cutoffs, cutoff)
		return nil
	})
	st.SetRetention(1)
	n, err := st.Purge(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Purge = (%d, %v), want (1, nil)", n, err)
	}
	if len(cutoffs) != 1 {
		t.Fatalf("JSONL purger called %d times, want 1", len(cutoffs))
	}
	// The cutoff sits between the old and the newest session timestamp.
	if !(cutoffs[0] > oldTS && cutoffs[0] <= newTS) {
		t.Errorf("purge cutoff = %q, want between %q and %q", cutoffs[0], oldTS, newTS)
	}

	// A failing JSONL trim surfaces as a purge error (the SQL purge already
	// committed; the daily loop retries just the trim next tick).
	st.SetJSONLPurger(func(string) error { return errors.New("jsonl boom") })
	if _, err := st.Purge(context.Background()); err == nil {
		t.Error("Purge returned nil error despite failing JSONL purger")
	}
}

// TestPurgeSkipsJSONLPurgerWhenDisabled verifies the trimmer is not invoked
// when retention is disabled (no data is being purged).
func TestPurgeSkipsJSONLPurgerWhenDisabled(t *testing.T) {
	st := openTestStore(t)
	st.SetRetention(0)
	called := false
	st.SetJSONLPurger(func(string) error { called = true; return nil })
	if _, err := st.Purge(context.Background()); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if called {
		t.Error("JSONL purger called with retention disabled")
	}
}

// TestStartRetentionLoopFirstPassImmediate verifies the loop purges as soon as
// it starts — without waiting for the first tick — so a backlog that expired
// while the gateway was down is caught up right after startup, concurrently
// with serving.
func TestStartRetentionLoopFirstPassImmediate(t *testing.T) {
	st := openTestStore(t)

	old := baseRecord()
	old.ID = "req-loop-old"
	old.SessionID = "sess-loop-old"
	mustCapture(t, st, old)
	oldTS := formatTS(time.Now().UTC().AddDate(0, 0, -10))
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, oldTS, "sess-loop-old"); err != nil {
		t.Fatal(err)
	}
	// A fresh session anchors the cutoff to now (Purge never expires the
	// newest session relative to itself).
	fresh := baseRecord()
	fresh.ID = "req-loop-new"
	fresh.SessionID = "sess-loop-new"
	mustCapture(t, st, fresh)
	newTS := formatTS(time.Now().UTC())
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, newTS, "sess-loop-new"); err != nil {
		t.Fatal(err)
	}
	st.SetRetention(1)

	passed := make(chan int, 1)
	// interval = 1h: without an immediate first pass the callback cannot fire
	// within the test's lifetime.
	st.StartRetentionLoop(context.Background(), time.Hour, func(n int, err error) {
		if err != nil {
			t.Errorf("loop pass error: %v", err)
		}
		passed <- n
	})
	select {
	case n := <-passed:
		if n != 1 {
			t.Errorf("first loop pass expired %d sessions, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retention loop did not run its first pass immediately")
	}

	sess, err := st.GetSession(context.Background(), "sess-loop-old")
	if err != nil {
		t.Fatalf("expired session missing: %v", err)
	}
	if !sess.Expired {
		t.Error("session not expired by the loop's first pass")
	}
}

// TestStartRetentionLoopDropsCanceledPass verifies a context-canceled pass is
// not reported (the gateway is shutting down; a cancel during the initial
// purge is not an operational failure worth a warning).
func TestStartRetentionLoopDropsCanceledPass(t *testing.T) {
	st := openTestStore(t)

	old := baseRecord()
	old.ID = "req-loop-cancel"
	old.SessionID = "sess-loop-cancel"
	mustCapture(t, st, old)
	oldTS := formatTS(time.Now().UTC().AddDate(0, 0, -10))
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, oldTS, "sess-loop-cancel"); err != nil {
		t.Fatal(err)
	}
	fresh := baseRecord()
	fresh.ID = "req-loop-cancel-new"
	fresh.SessionID = "sess-loop-cancel-new"
	mustCapture(t, st, fresh)
	newTS := formatTS(time.Now().UTC())
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, newTS, "sess-loop-cancel-new"); err != nil {
		t.Fatal(err)
	}
	st.SetRetention(1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reported := make(chan int, 1)
	st.StartRetentionLoop(ctx, time.Hour, func(n int, err error) { reported <- n })
	select {
	case n := <-reported:
		t.Errorf("canceled first pass reported n=%d, want silence", n)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestForeignKeyEnforcement(t *testing.T) {
	st := openTestStore(t)

	_, err := st.db.Exec(`INSERT INTO requests (id, session_id) VALUES ('orphan', 'no-such-session')`)
	if err == nil {
		t.Errorf("insert with unknown session_id succeeded; foreign_keys pragma not enforced")
	}
}

func TestGetRequestNotFound(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.GetRequest(context.Background(), "sess-x", 1); err != ErrNotFound {
		t.Errorf("GetRequest err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetToolCall(context.Background(), "nope/0"); err != ErrNotFound {
		t.Errorf("GetToolCall err = %v, want ErrNotFound", err)
	}
}
