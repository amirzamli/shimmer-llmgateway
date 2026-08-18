package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// ---------------------------------------------------------------------------
// Unit tests: schema extraction, Levenshtein, diff
// ---------------------------------------------------------------------------

func TestDeclaredToolsExtraction(t *testing.T) {
	body := json.RawMessage(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"type": "function", "function": {
				"name": "get_weather",
				"description": "weather",
				"parameters": {"type": "object", "properties": {
					"city": {"type": "string"},
					"days": {"type": ["integer", "null"]}
				}, "required": ["city"]}
			}},
			{"type": "function", "name": "get_time", "parameters": {
				"type": "object", "properties": {"tz": {"type": "string"}}, "required": ["tz"]
			}}
		]
	}`)
	tools := declaredTools(body)
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools))
	}
	if tools[0].Name != "get_weather" {
		t.Errorf("tools[0].Name = %q", tools[0].Name)
	}
	if len(tools[0].Required) != 1 || tools[0].Required[0] != "city" {
		t.Errorf("get_weather required = %v", tools[0].Required)
	}
	if !tools[0].Properties["city"].matches("Paris") || tools[0].Properties["city"].matches(3) {
		t.Errorf("get_weather.city type not enforced")
	}
	if !tools[0].Properties["days"].matches(float64(3)) || !tools[0].Properties["days"].matches(nil) {
		t.Errorf("get_weather.days should accept integer and null")
	}
	if tools[1].Name != "get_time" {
		t.Errorf("tools[1].Name = %q, want get_time (flat form)", tools[1].Name)
	}
	if len(declaredTools(nil)) != 0 {
		t.Errorf("nil body should declare no tools")
	}
	if len(declaredTools(json.RawMessage(`{"model":"gpt-4o"}`))) != 0 {
		t.Errorf("body without tools should declare none")
	}
}

func TestLevenshteinDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "ab", 2},
		{"city", "city", 0},
		{"kitten", "sitting", 3},
		{"citty", "city", 1},
	}
	for _, c := range cases {
		if got := levenshteinDistance(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNearestSuggestionRanking(t *testing.T) {
	props := map[string]jsonSchemaType{
		"city":      {"string": true},
		"city_name": {"string": true},
		"units":     {"string": true},
	}
	n := nearestFor("cityname", props)
	if len(n.Suggestions) < 2 {
		t.Fatalf("suggestions = %+v, want >= 2 (city_name and city)", n.Suggestions)
	}
	if n.Suggestions[0].Name != "city_name" {
		t.Errorf("top suggestion = %q, want city_name", n.Suggestions[0].Name)
	}
	if n.Suggestions[0].Score <= n.Suggestions[1].Score {
		t.Errorf("suggestions not ranked by score desc: %+v", n.Suggestions)
	}
	// No close match: below the threshold, empty suggestions.
	if got := nearestFor("zzz", props); len(got.Suggestions) != 0 {
		t.Errorf("zzz suggestions = %+v, want none", got.Suggestions)
	}
}

func TestDiffToolCallCases(t *testing.T) {
	tools := []declaredTool{
		{Name: "get_weather", Properties: map[string]jsonSchemaType{
			"city":  {"string": true},
			"units": {"string": true},
			"days":  {"integer": true},
		}, Required: []string{"city"}},
		{Name: "get_time", Properties: map[string]jsonSchemaType{
			"tz": {"string": true},
		}, Required: []string{"tz"}},
	}

	cases := []struct {
		name          string
		toolName      string
		args          json.RawMessage
		wantOK        bool
		wantUnknown   []string
		wantMissing   []string
		wantMismatch  []typeMismatch
		wantNotFound  bool
		wantMalformed bool
		wantNearest   map[string][]string
	}{
		{"valid", "get_weather", json.RawMessage(`{"city":"Paris"}`), true, nil, nil, nil, false, false, nil},
		{"unknown_param", "get_weather", json.RawMessage(`{"city":"Paris","unit":"C"}`), false, []string{"unit"}, nil, nil, false, false, map[string][]string{"unit": {"units"}}},
		{"missing_required", "get_weather", json.RawMessage(`{}`), false, nil, []string{"city"}, nil, false, false, nil},
		{"type_mismatch", "get_time", json.RawMessage(`{"tz":5}`), false, nil, nil, []typeMismatch{{Param: "tz", Expected: "string", Got: "number"}}, false, false, nil},
		{"tool_not_found", "bogus_tool", json.RawMessage(`{"x":1}`), false, nil, nil, nil, true, false, nil},
		{"malformed", "get_weather", json.RawMessage(`not-json`), false, nil, nil, nil, false, true, nil},
		{"empty_args", "get_weather", json.RawMessage(``), false, nil, nil, nil, false, true, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := diffToolCall(c.toolName, c.args, tools)
			if d.OK != c.wantOK {
				t.Errorf("ok = %v, want %v", d.OK, c.wantOK)
			}
			if d.ToolNotFound != c.wantNotFound {
				t.Errorf("tool_not_found = %v, want %v", d.ToolNotFound, c.wantNotFound)
			}
			if (d.Malformed != "") != c.wantMalformed {
				t.Errorf("malformed = %q, want malformed=%v", d.Malformed, c.wantMalformed)
			}
			if !equalStrings(d.UnknownParams, c.wantUnknown) {
				t.Errorf("unknown_params = %v, want %v", d.UnknownParams, c.wantUnknown)
			}
			if !equalStrings(d.MissingRequired, c.wantMissing) {
				t.Errorf("missing_required = %v, want %v", d.MissingRequired, c.wantMissing)
			}
			if len(d.TypeMismatches) != len(c.wantMismatch) {
				t.Errorf("type_mismatches = %+v, want %+v", d.TypeMismatches, c.wantMismatch)
			} else {
				for i := range c.wantMismatch {
					if d.TypeMismatches[i] != c.wantMismatch[i] {
						t.Errorf("type_mismatches[%d] = %+v, want %+v", i, d.TypeMismatches[i], c.wantMismatch[i])
					}
				}
			}
			for param, wantSugg := range c.wantNearest {
				found := false
				for _, n := range d.NearestParams {
					if n.Param != param {
						continue
					}
					found = true
					names := make([]string, 0, len(n.Suggestions))
					for _, s := range n.Suggestions {
						names = append(names, s.Name)
					}
					if !equalStrings(names, wantSugg) {
						t.Errorf("nearest for %q = %v, want %v", param, names, wantSugg)
					}
				}
				if !found {
					t.Errorf("no nearest_params entry for %q", param)
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Store-level fixtures
// ---------------------------------------------------------------------------

const analysisToolsJSON = `{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{
	"city":{"type":"string"},"units":{"type":"string"},"days":{"type":"integer"}
},"required":["city"]}}}` + `,` + `{"type":"function","function":{"name":"get_time","parameters":{"type":"object","properties":{
	"tz":{"type":"string"}
},"required":["tz"]}}}`

func analysisRequestBody() string {
	return `{"model":"gpt-4o","messages":[{"role":"user","content":"call tools"}],"tools":[` + analysisToolsJSON + `]}`
}

// seedAnalysisStore captures the analysis fixture into st:
//
//	req-1: declares tools; emits 4 calls (valid / unknown param / missing
//	        required / valid).
//	req-2: returns the result for call_1 (error) — links result_is_error.
//	req-3: declares tools; emits a type-mismatch call and an undeclared tool.
//	req-4: declares no tools; emits a call (no-schema case).
//	req-5: declares tools; emits malformed (non-JSON) arguments.
func seedAnalysisStore(t *testing.T, st *store.Store) {
	t.Helper()
	reqs := []*store.CaptureRecord{
		{
			ID: "req-1", SessionID: "sess-ana",
			CreatedAt: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
			Alias:     "openai", Provider: "openai", Model: "gpt-4o",
			Endpoint: "/v1/chat/completions", DurationMS: 10, StatusCode: 200,
			FinishReason: "tool_calls",
			RequestJSON:  json.RawMessage(analysisRequestBody()),
			ResponseJSON: json.RawMessage(`{"id":"resp-1","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Paris\"}"}},
				{"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Paris\", \"unit\": \"C\"}"}},
				{"id":"call_3","type":"function","function":{"name":"get_weather","arguments":"{}"}},
				{"id":"call_4","type":"function","function":{"name":"get_time","arguments":"{\"tz\": \"UTC\"}"}}
			]},"finish_reason":"tool_calls"}]}`),
		},
		{
			ID: "req-2", SessionID: "sess-ana",
			CreatedAt: time.Date(2026, 8, 2, 12, 1, 0, 0, time.UTC),
			Alias:     "openai", Provider: "openai", Model: "gpt-4o",
			Endpoint: "/v1/chat/completions", DurationMS: 10, StatusCode: 200,
			FinishReason: "stop",
			RequestJSON: json.RawMessage(`{"model":"gpt-4o","messages":[
				{"role":"user","content":"call tools"},
				{"role":"tool","tool_call_id":"call_1","content":"{\"error\":\"boom\"}"},
				{"role":"user","content":"continue"}
			],"tools":[` + analysisToolsJSON + `]}`),
			ResponseJSON: json.RawMessage(`{"id":"resp-2","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`),
		},
		{
			ID: "req-3", SessionID: "sess-ana",
			CreatedAt: time.Date(2026, 8, 2, 12, 2, 0, 0, time.UTC),
			Alias:     "openai", Provider: "openai", Model: "gpt-4o",
			Endpoint: "/v1/chat/completions", DurationMS: 10, StatusCode: 200,
			FinishReason: "tool_calls",
			RequestJSON:  json.RawMessage(analysisRequestBody()),
			ResponseJSON: json.RawMessage(`{"id":"resp-3","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_5","type":"function","function":{"name":"get_time","arguments":"{\"tz\": 5}"}},
				{"id":"call_6","type":"function","function":{"name":"bogus_tool","arguments":"{\"x\": 1}"}}
			]},"finish_reason":"tool_calls"}]}`),
		},
		{
			ID: "req-4", SessionID: "sess-ana",
			CreatedAt: time.Date(2026, 8, 2, 12, 3, 0, 0, time.UTC),
			Alias:     "openai", Provider: "openai", Model: "gpt-4o",
			Endpoint: "/v1/chat/completions", DurationMS: 10, StatusCode: 200,
			FinishReason: "tool_calls",
			RequestJSON:  json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"call tools"}]}`),
			ResponseJSON: json.RawMessage(`{"id":"resp-4","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_7","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Paris\"}"}}
			]},"finish_reason":"tool_calls"}]}`),
		},
		{
			ID: "req-5", SessionID: "sess-ana",
			CreatedAt: time.Date(2026, 8, 2, 12, 4, 0, 0, time.UTC),
			Alias:     "openai", Provider: "openai", Model: "gpt-4o",
			Endpoint: "/v1/chat/completions", DurationMS: 10, StatusCode: 200,
			FinishReason: "tool_calls",
			RequestJSON:  json.RawMessage(analysisRequestBody()),
			ResponseJSON: json.RawMessage(`{"id":"resp-5","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_8","type":"function","function":{"name":"get_weather","arguments":"not-json"}}
			]},"finish_reason":"tool_calls"}]}`),
		},
	}
	for _, r := range reqs {
		if err := st.Capture(context.Background(), r); err != nil {
			t.Fatalf("capture %s: %v", r.ID, err)
		}
	}
}

func openAnalysisStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "analysis.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// analysisToolCall returns the parsed tool call result of a tools/call.
func analysisToolCall(t *testing.T, s *Server, name string, args map[string]any) callResult {
	t.Helper()
	resp := stdioCall(t, s, "tools/call", map[string]any{"name": name, "arguments": args})
	if resp["error"] != nil {
		t.Fatalf("tools/call JSON-RPC error: %v", resp["error"])
	}
	return toolResult(t, resp)
}

// ---------------------------------------------------------------------------
// validate_tool_call
// ---------------------------------------------------------------------------

func TestValidateToolCallTool(t *testing.T) {
	st := openAnalysisStore(t)
	seedAnalysisStore(t, st)
	s := New(st, "")

	diffView := func(res callResult) map[string]any {
		t.Helper()
		if res.isError {
			t.Fatalf("unexpected isError: %s", res.text)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(res.text), &v); err != nil {
			t.Fatalf("not JSON: %v (%q)", err, res.text)
		}
		return v
	}

	// Single call via tool_call_id.
	valid := diffView(analysisToolCall(t, s, "validate_tool_call", map[string]any{"tool_call_id": "req-1/0"}))
	if valid["tool_name"] != "get_weather" || valid["ok"] != true {
		t.Errorf("valid diff = %v", valid)
	}
	if n := len(valid["unknown_params"].([]any)) + len(valid["missing_required"].([]any)) + len(valid["type_mismatches"].([]any)) + len(valid["nearest_params"].([]any)); n != 0 {
		t.Errorf("valid call should have empty arrays, got %v", valid)
	}

	// Unknown param + nearest suggestion.
	unknown := diffView(analysisToolCall(t, s, "validate_tool_call", map[string]any{"tool_call_id": "req-1/1"}))
	if unknown["ok"] != false {
		t.Errorf("unknown call ok = %v", unknown["ok"])
	}
	ups, _ := unknown["unknown_params"].([]any)
	if len(ups) != 1 || ups[0] != "unit" {
		t.Errorf("unknown_params = %v, want [unit]", unknown["unknown_params"])
	}
	nps, _ := unknown["nearest_params"].([]any)
	if len(nps) != 1 {
		t.Fatalf("nearest_params = %v", unknown["nearest_params"])
	}
	np := nps[0].(map[string]any)
	if np["param"] != "unit" {
		t.Errorf("nearest param = %v", np)
	}
	suggs, _ := np["suggestions"].([]any)
	if len(suggs) != 1 || suggs[0].(map[string]any)["name"] != "units" {
		t.Errorf("suggestions for unit = %v, want [units]", np["suggestions"])
	}

	// Missing required.
	missing := diffView(analysisToolCall(t, s, "validate_tool_call", map[string]any{"tool_call_id": "req-1/2"}))
	mr, _ := missing["missing_required"].([]any)
	if len(mr) != 1 || mr[0] != "city" {
		t.Errorf("missing_required = %v, want [city]", missing["missing_required"])
	}

	// Type mismatch.
	tm := diffView(analysisToolCall(t, s, "validate_tool_call", map[string]any{"tool_call_id": "req-3/0"}))
	mis, _ := tm["type_mismatches"].([]any)
	if len(mis) != 1 {
		t.Fatalf("type_mismatches = %v", tm["type_mismatches"])
	}
	m := mis[0].(map[string]any)
	if m["param"] != "tz" || m["expected"] != "string" || m["got"] != "number" {
		t.Errorf("type mismatch = %v, want {tz string number}", m)
	}

	// Tool not declared.
	notFound := diffView(analysisToolCall(t, s, "validate_tool_call", map[string]any{"tool_call_id": "req-3/1"}))
	if notFound["tool_not_found"] != true || notFound["ok"] != false {
		t.Errorf("tool_not_found diff = %v", notFound)
	}

	// Malformed args.
	malformed := diffView(analysisToolCall(t, s, "validate_tool_call", map[string]any{"tool_call_id": "req-5/0"}))
	if malformed["ok"] != false || malformed["malformed"] == "" {
		t.Errorf("malformed diff = %v", malformed)
	}

	// session_id+seq form: request with several calls returns an array; a
	// single-call request returns one object.
	multi := analysisToolCall(t, s, "validate_tool_call", map[string]any{"session_id": "sess-ana", "seq": 1})
	var arr []map[string]any
	if err := json.Unmarshal([]byte(multi.text), &arr); err != nil || len(arr) != 4 {
		t.Fatalf("session+seq (4 calls) = %q, want array of 4", multi.text)
	}
	if arr[1]["tool_call_id"] != "req-1/1" || arr[1]["ok"] != false {
		t.Errorf("array[1] = %v", arr[1])
	}

	single := analysisToolCall(t, s, "validate_tool_call", map[string]any{"session_id": "sess-ana", "seq": 5})
	var one map[string]any
	if err := json.Unmarshal([]byte(single.text), &one); err != nil {
		t.Fatalf("single-call request form = %q, want one object", single.text)
	}
	if one["tool_call_id"] != "req-5/0" || one["ok"] != false {
		t.Errorf("single object = %v", one)
	}
}

// TestValidateToolCallFormEquivalence: the tool_call_id form and the
// session_id+seq form of the same single call return the same diff.
func TestValidateToolCallFormEquivalence(t *testing.T) {
	st := openAnalysisStore(t)
	seedAnalysisStore(t, st)
	s := New(st, "")

	byID := analysisToolCall(t, s, "validate_tool_call", map[string]any{"tool_call_id": "req-5/0"})
	byReq := analysisToolCall(t, s, "validate_tool_call", map[string]any{"session_id": "sess-ana", "seq": 5})
	if byID.text != byReq.text {
		t.Errorf("id form = %s, request form = %s", byID.text, byReq.text)
	}
}

func TestValidateToolCallErrors(t *testing.T) {
	st := openAnalysisStore(t)
	seedAnalysisStore(t, st)
	s := New(st, "")

	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{"no_args", map[string]any{}, "INVALID_ARGUMENT"},
		{"both_forms", map[string]any{"tool_call_id": "req-1/0", "session_id": "sess-ana", "seq": 1}, "INVALID_ARGUMENT"},
		{"missing_seq", map[string]any{"session_id": "sess-ana"}, "INVALID_ARGUMENT"},
		{"bad_seq", map[string]any{"session_id": "sess-ana", "seq": -1}, "INVALID_ARGUMENT"},
		{"unknown_session", map[string]any{"session_id": "nope", "seq": 1}, "NOT_FOUND"},
		{"unknown_seq", map[string]any{"session_id": "sess-ana", "seq": 99}, "NOT_FOUND"},
		{"unknown_tool_call_id", map[string]any{"tool_call_id": "nope/0"}, "NOT_FOUND"},
		{"no_tool_calls_in_request", map[string]any{"session_id": "sess-ana", "seq": 2}, "NOT_FOUND"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := analysisToolCall(t, s, "validate_tool_call", c.args)
			assertToolError(t, res, c.code)
		})
	}
}

// ---------------------------------------------------------------------------
// classify_failure
// ---------------------------------------------------------------------------

// verdictView is the parsed classify_failure payload.
type verdictView struct {
	ToolCallID    string   `json:"tool_call_id"`
	ToolName      string   `json:"tool_name"`
	Verdict       string   `json:"verdict"`
	Reasons       []string `json:"reasons"`
	Why           string   `json:"why"`
	Cached        bool     `json:"cached"`
	SchemaPresent bool     `json:"schema_present"`
	ToolNotFound  bool     `json:"tool_not_found"`
	MissingParams []string `json:"missing_params"`
	UnknownParams []string `json:"unknown_params"`
	Malformed     string   `json:"malformed"`
}

func classifyVerdict(t *testing.T, s *Server, args map[string]any) verdictView {
	t.Helper()
	res := analysisToolCall(t, s, "classify_failure", args)
	if res.isError {
		t.Fatalf("unexpected isError: %s", res.text)
	}
	var v verdictView
	if err := json.Unmarshal([]byte(res.text), &v); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, res.text)
	}
	return v
}

func TestClassifyFailureVerdicts(t *testing.T) {
	st := openAnalysisStore(t)
	seedAnalysisStore(t, st)
	s := New(st, "")

	hasReason := func(v verdictView, want string) bool {
		for _, r := range v.Reasons {
			if r == want {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name              string
		id                string
		verdict           string
		reasons           []string
		wantSchemaPresent bool
	}{
		{"success", "req-1/3", verdictSuccess, []string{reasonValidCall}, true},
		{"result_error", "req-1/0", verdictRecoverable, []string{reasonResultError}, true},
		{"unknown_params", "req-1/1", verdictRecoverable, []string{reasonUnknownParams}, true},
		{"missing_required", "req-1/2", verdictRecoverable, []string{reasonMissingRequired}, true},
		{"type_mismatch", "req-3/0", verdictRecoverable, []string{reasonTypeMismatch}, true},
		{"tool_not_in_schemas", "req-3/1", verdictRecoverable, []string{reasonToolNotInSchemas}, true},
		{"no_schema_declared", "req-4/0", verdictBlindError, []string{reasonNoSchemaDeclared}, false},
		{"malformed_arguments", "req-5/0", verdictBlindError, []string{reasonMalformedArguments}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := classifyVerdict(t, s, map[string]any{"tool_call_id": c.id})
			if v.Verdict != c.verdict {
				t.Errorf("verdict = %q, want %q (why %q)", v.Verdict, c.verdict, v.Why)
			}
			if v.Why == "" {
				t.Errorf("missing why for %s", c.id)
			}
			for _, r := range c.reasons {
				if !hasReason(v, r) {
					t.Errorf("reasons = %v, missing %q", v.Reasons, r)
				}
			}
			if v.SchemaPresent != c.wantSchemaPresent {
				t.Errorf("schema_present = %v, want %v", v.SchemaPresent, c.wantSchemaPresent)
			}
		})
	}
}

func TestClassifyFailureVerdictCaching(t *testing.T) {
	st := openAnalysisStore(t)
	seedAnalysisStore(t, st)
	s := New(st, "")

	// First call computes and caches.
	v1 := classifyVerdict(t, s, map[string]any{"tool_call_id": "req-1/3"})
	if v1.Verdict != verdictSuccess || v1.Cached {
		t.Fatalf("first call = %+v, want SUCCESS uncached", v1)
	}
	if v1.Why == "" {
		t.Errorf("first call missing why")
	}

	// The verdict + annotation are written to tool_calls (§5 pull model).
	tc, err := st.GetToolCall(context.Background(), "req-1/3")
	if err != nil {
		t.Fatalf("GetToolCall: %v", err)
	}
	if tc.Verdict != verdictSuccess {
		t.Errorf("stored verdict = %q, want SUCCESS", tc.Verdict)
	}
	if len(tc.AnnotationJSON) == 0 {
		t.Errorf("stored annotation is empty")
	} else {
		var a annotationJSON
		if err := json.Unmarshal(tc.AnnotationJSON, &a); err != nil {
			t.Fatalf("stored annotation not JSON: %v", err)
		}
		if a.Verdict != verdictSuccess || a.Why == "" {
			t.Errorf("stored annotation = %+v", a)
		}
	}

	// A subsequent call returns the cached verdict.
	v2 := classifyVerdict(t, s, map[string]any{"tool_call_id": "req-1/3"})
	if v2.Verdict != verdictSuccess || !v2.Cached {
		t.Errorf("cached call = %+v, want SUCCESS cached=true", v2)
	}
	if v2.Why == "" {
		t.Errorf("cached call missing why")
	}

	// refresh=true recomputes (and overwrites) instead of reading the cache.
	v3 := classifyVerdict(t, s, map[string]any{"tool_call_id": "req-1/3", "refresh": true})
	if v3.Verdict != verdictSuccess || v3.Cached {
		t.Errorf("refresh call = %+v, want SUCCESS uncached", v3)
	}

	// list_tool_calls verdict filter now finds the cached row.
	calls, err := st.ListToolCalls(context.Background(), store.ToolCallFilter{Verdict: verdictSuccess})
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].ID != "req-1/3" {
		t.Errorf("verdict=SUCCESS rows = %v, want only req-1/3", calls)
	}
}

// TestClassifyFailureRequestForm: the session_id+seq form classifies every
// tool call that request emitted and caches each verdict.
func TestClassifyFailureRequestForm(t *testing.T) {
	st := openAnalysisStore(t)
	seedAnalysisStore(t, st)
	s := New(st, "")

	res := analysisToolCall(t, s, "classify_failure", map[string]any{"session_id": "sess-ana", "seq": 1})
	var arr []verdictView
	if err := json.Unmarshal([]byte(res.text), &arr); err != nil || len(arr) != 4 {
		t.Fatalf("classify req-1 = %q, want array of 4", res.text)
	}
	want := map[string]string{
		"req-1/0": verdictRecoverable,
		"req-1/1": verdictRecoverable,
		"req-1/2": verdictRecoverable,
		"req-1/3": verdictSuccess,
	}
	for _, v := range arr {
		if w, ok := want[v.ToolCallID]; !ok || v.Verdict != w {
			t.Errorf("%s verdict = %q, want %q", v.ToolCallID, v.Verdict, w)
		}
		if v.Cached {
			t.Errorf("%s unexpectedly cached on first classification", v.ToolCallID)
		}
	}
	// All four are cached in the store now.
	tc, err := st.GetToolCall(context.Background(), "req-1/0")
	if err != nil {
		t.Fatalf("GetToolCall: %v", err)
	}
	if tc.Verdict != verdictRecoverable || len(tc.AnnotationJSON) == 0 {
		t.Errorf("req-1/0 cached = %q %q", tc.Verdict, tc.AnnotationJSON)
	}
}

func TestClassifyFailureErrors(t *testing.T) {
	st := openAnalysisStore(t)
	seedAnalysisStore(t, st)
	s := New(st, "")

	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{"no_args", map[string]any{}, "INVALID_ARGUMENT"},
		{"unknown_tool_call_id", map[string]any{"tool_call_id": "nope/0"}, "NOT_FOUND"},
		{"unknown_session", map[string]any{"session_id": "nope", "seq": 1}, "NOT_FOUND"},
		{"unknown_seq", map[string]any{"session_id": "sess-ana", "seq": 99}, "NOT_FOUND"},
		{"no_tool_calls_in_request", map[string]any{"session_id": "sess-ana", "seq": 2}, "NOT_FOUND"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := analysisToolCall(t, s, "classify_failure", c.args)
			assertToolError(t, res, c.code)
		})
	}
}

// TestClassifyFailureScopedSession: the --session scope overrides the
// caller-supplied session_id for the session+seq form.
func TestClassifyFailureScopedSession(t *testing.T) {
	st := openAnalysisStore(t)
	seedAnalysisStore(t, st)
	s := New(st, "sess-ana")

	res := analysisToolCall(t, s, "classify_failure", map[string]any{"session_id": "sess-bogus", "seq": 1})
	if res.isError {
		t.Fatalf("scoped classify failed: %s", res.text)
	}
	var arr []verdictView
	if err := json.Unmarshal([]byte(res.text), &arr); err != nil || len(arr) != 4 {
		t.Fatalf("scoped classify = %q, want array of 4 from sess-ana", res.text)
	}
}
