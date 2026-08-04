package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"shimmer-llmgateway/internal/store"
)

// ---------------------------------------------------------------------------
// Verdict taxonomy (classify_failure)
//
// docs/agent-hallucination-report.md does not exist in this repo, so the
// taxonomy is derived from the spec and documented here for review (Phase 7
// warrants review on this mapping):
//
//   - SUCCESS     — the emitted call conforms to the client-declared schema
//     (the validate_tool_call diff is ok) and carries no error signal
//     (result_is_error is false or unknown). Nothing went wrong.
//   - RECOVERABLE — a concrete, schema-grounded defect the model can fix by
//     re-emitting against the declared contract, or a visible tool failure.
//     The §4.2 exemplar: an unknown alias is "a RECOVERABLE error" because the
//     error message lists the available aliases, so the agent can self-correct.
//     By analogy, calling a tool not present in the request's tools[]
//     (tool_not_found), passing unknown parameters, omitting required
//     parameters, or passing wrong-typed parameters are all recoverable: the
//     declared schema is the correction target. A tool-result error
//     (result_is_error) is likewise recoverable — the model sees the tool's
//     error output and can respond or retry.
//   - BLIND_ERROR — the failure cannot be fixed from available signals: the
//     request declared no tools[] at all (no contract to fix against) or the
//     emitted arguments are structurally unusable (not a JSON object), so no
//     schema-grounded suggestion is possible.
//
// Classification inputs (all deterministic): the validate_tool_call diff,
// result_is_error, whether the tool name is declared, whether any schema was
// declared at all, unknown params, missing required params, type mismatches.
// The decision order is fixed: no schema → malformed args → tool not declared
// → schema deviations → result error → success.
// ---------------------------------------------------------------------------

const (
	verdictSuccess     = "SUCCESS"
	verdictRecoverable = "RECOVERABLE"
	verdictBlindError  = "BLIND_ERROR"
)

// Reason keywords returned by classify_failure and stored in annotation_json.
const (
	reasonValidCall          = "valid_call"
	reasonResultError        = "result_error"
	reasonUnknownParams      = "unknown_params"
	reasonMissingRequired    = "missing_required"
	reasonTypeMismatch       = "type_mismatch"
	reasonToolNotInSchemas   = "tool_not_in_schemas"
	reasonNoSchemaDeclared   = "no_schema_declared"
	reasonMalformedArguments = "malformed_arguments"
)

// ---------------------------------------------------------------------------
// Client-declared schema extraction
//
// validate_tool_call / classify_failure need no external schema knowledge:
// the schemas the client declared travel inside the stored request body
// (request_json.tools[]). We parse the standard OpenAI chat-completions shape
// (tools[].function.{name,parameters}) and the flat variant some clients send
// (tools[].{name,parameters}).
// ---------------------------------------------------------------------------

// declaredTool is one client-declared tool schema.
type declaredTool struct {
	Name       string
	Properties map[string]jsonSchemaType
	Required   []string
}

// jsonSchemaType is the set of allowed JSON types for a property. A property
// with no declaration or an unknown type is unconstrained (matches anything).
type jsonSchemaType map[string]bool

// declaredTools extracts the client-declared tools[] schemas from a request
// body. It returns nil when the request declared no tools (or the body is
// unparseable — treated as "no schema declared").
func declaredTools(requestJSON []byte) []declaredTool {
	if len(requestJSON) == 0 {
		return nil
	}
	var req struct {
		Tools []struct {
			Type string `json:"type"`
			// Nested OpenAI form: tools[].function.{name,parameters}.
			Function *struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
			// Flat form: tools[].{name,parameters}.
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return nil
	}
	var out []declaredTool
	for _, t := range req.Tools {
		name := t.Name
		params := t.Parameters
		if t.Function != nil && t.Function.Name != "" {
			name = t.Function.Name
			params = t.Function.Parameters
		}
		if name == "" {
			continue
		}
		dt := declaredTool{Name: name}
		dt.Properties, dt.Required = parseSchemaProperties(params)
		out = append(out, dt)
	}
	return out
}

// parseSchemaProperties interprets a JSON Schema object's properties/required.
func parseSchemaProperties(params json.RawMessage) (map[string]jsonSchemaType, []string) {
	if len(params) == 0 {
		return nil, nil
	}
	var schema struct {
		Properties map[string]struct {
			Type json.RawMessage `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(params, &schema); err != nil {
		return nil, nil
	}
	props := make(map[string]jsonSchemaType, len(schema.Properties))
	for name, p := range schema.Properties {
		props[name] = parseJSONTypes(p.Type)
	}
	return props, schema.Required
}

// parseJSONTypes parses a JSON Schema type (a string or an array of strings)
// into a set.
func parseJSONTypes(raw json.RawMessage) jsonSchemaType {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return jsonSchemaType{s: true}
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		set := make(jsonSchemaType, len(arr))
		for _, t := range arr {
			set[t] = true
		}
		return set
	}
	return nil
}

// matches reports whether an emitted argument value satisfies the declared
// type(s). An unconstrained declaration matches anything.
func (t jsonSchemaType) matches(v any) bool {
	if len(t) == 0 {
		return true
	}
	gt := jsonTypeOf(v)
	if t[gt] {
		return true
	}
	// JSON numbers decode as float64; accept integral floats for "integer".
	if gt == "number" && t["integer"] {
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			return true
		}
	}
	return false
}

// describe renders the declared type set for a mismatch report.
func (t jsonSchemaType) describe() string {
	names := make([]string, 0, len(t))
	for n := range t {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// jsonTypeOf maps a decoded JSON value to its JSON type name.
func jsonTypeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// The validate_tool_call diff
// ---------------------------------------------------------------------------

// toolCallDiff is the validate_tool_call result for one tool call.
type toolCallDiff struct {
	OK              bool
	UnknownParams   []string
	MissingRequired []string
	TypeMismatches  []typeMismatch
	NearestParams   []nearestParam
	// ToolNotFound is set when the emitted tool name has no declared schema.
	ToolNotFound bool
	// Malformed is non-empty when the emitted arguments are not a JSON object,
	// so no diff is possible.
	Malformed string
}

type typeMismatch struct {
	Param    string
	Expected string
	Got      string
}

type nearestParam struct {
	Param       string
	Suggestions []paramSuggestion
}

type paramSuggestion struct {
	Name  string
	Score float64
}

// newToolCallDiff returns a diff with the spec's []-shaped fields initialized
// so the JSON output uses [] (never null) for empty arrays.
func newToolCallDiff() toolCallDiff {
	return toolCallDiff{
		UnknownParams:   []string{},
		MissingRequired: []string{},
		TypeMismatches:  []typeMismatch{},
		NearestParams:   []nearestParam{},
	}
}

// diffToolCall validates one tool call's emitted arguments against the schemas
// declared in the originating request. All output slices are deterministic
// (sorted) regardless of JSON map ordering.
func diffToolCall(toolName string, argumentsJSON json.RawMessage, tools []declaredTool) toolCallDiff {
	if len(argumentsJSON) == 0 {
		d := newToolCallDiff()
		d.Malformed = "arguments are empty"
		return d
	}
	var args map[string]any
	if err := json.Unmarshal(argumentsJSON, &args); err != nil || args == nil {
		d := newToolCallDiff()
		d.Malformed = "arguments are not a JSON object"
		return d
	}

	var schema *declaredTool
	for i := range tools {
		if tools[i].Name == toolName {
			schema = &tools[i]
			break
		}
	}
	if schema == nil {
		d := newToolCallDiff()
		d.ToolNotFound = true
		return d
	}

	d := newToolCallDiff()
	propertyNames := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		propertyNames = append(propertyNames, name)
	}
	sort.Strings(propertyNames)

	argNames := make([]string, 0, len(args))
	for name := range args {
		argNames = append(argNames, name)
	}
	sort.Strings(argNames)

	for _, name := range argNames {
		if _, ok := schema.Properties[name]; ok {
			continue
		}
		d.UnknownParams = append(d.UnknownParams, name)
		d.NearestParams = append(d.NearestParams, nearestFor(name, schema.Properties))
	}

	for _, name := range schema.Required {
		if _, ok := args[name]; !ok {
			d.MissingRequired = append(d.MissingRequired, name)
		}
	}

	for _, name := range argNames {
		allowed, ok := schema.Properties[name]
		if !ok {
			continue
		}
		got := jsonTypeOf(args[name])
		if allowed.matches(args[name]) {
			continue
		}
		d.TypeMismatches = append(d.TypeMismatches, typeMismatch{
			Param: name, Expected: allowed.describe(), Got: got,
		})
	}

	d.OK = len(d.UnknownParams) == 0 && len(d.MissingRequired) == 0 && len(d.TypeMismatches) == 0
	return d
}

// nearestSuggestionsCap bounds the per-param suggestion list.
const nearestSuggestionsCap = 3

// nearestScoreThreshold is the minimum normalized similarity for a suggestion.
const nearestScoreThreshold = 0.5

// nearestFor ranks the declared property names nearest to an unknown emitted
// param by Levenshtein similarity (rapidfuzz is Python-only, so the gateway
// implements the distance directly). Suggestions are sorted by score desc,
// then name asc, and capped.
func nearestFor(unknown string, properties map[string]jsonSchemaType) nearestParam {
	n := nearestParam{Param: unknown}
	var scored []paramSuggestion
	for name := range properties {
		s := levenshteinSimilarity(unknown, name)
		if s < nearestScoreThreshold {
			continue
		}
		scored = append(scored, paramSuggestion{Name: name, Score: s})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Name < scored[j].Name
	})
	if len(scored) > nearestSuggestionsCap {
		scored = scored[:nearestSuggestionsCap]
	}
	n.Suggestions = scored
	return n
}

// levenshteinSimilarity returns 1 - dist/maxLen (identical strings score 1; an
// empty operand scores 0 unless both are empty).
func levenshteinSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	dist := levenshteinDistance(a, b)
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	return 1 - float64(dist)/float64(maxLen)
}

// levenshteinDistance is the classic dynamic-programming edit distance.
func levenshteinDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 0
			if ra[i-1] != rb[j-1] {
				cost = 1
			}
			cur[j] = min(min(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// classifyToolCall derives a verdict + reason keywords for one tool call from
// the diff evidence. See the taxonomy comment above.
func classifyToolCall(t *store.ToolCall, tools []declaredTool) (verdict string, reasons []string, why string, diff toolCallDiff) {
	diff = diffToolCall(t.ToolName, t.ArgumentsJSON, tools)

	if len(tools) == 0 {
		return verdictBlindError,
			[]string{reasonNoSchemaDeclared},
			"the request declared no tools[] schema, so the emitted call cannot be checked or fixed against a contract",
			diff
	}
	if diff.Malformed != "" {
		return verdictBlindError,
			[]string{reasonMalformedArguments},
			"the emitted arguments are not a valid JSON object, so no schema-grounded correction is possible",
			diff
	}
	if diff.ToolNotFound {
		return verdictRecoverable,
			[]string{reasonToolNotInSchemas},
			"the model called a tool not declared in the request's tools[] (the §4.2 unknown-alias exemplar); it can re-emit against a declared tool",
			diff
	}
	if len(diff.UnknownParams) > 0 || len(diff.MissingRequired) > 0 || len(diff.TypeMismatches) > 0 {
		var rs []string
		if len(diff.UnknownParams) > 0 {
			rs = append(rs, reasonUnknownParams)
		}
		if len(diff.MissingRequired) > 0 {
			rs = append(rs, reasonMissingRequired)
		}
		if len(diff.TypeMismatches) > 0 {
			rs = append(rs, reasonTypeMismatch)
		}
		return verdictRecoverable,
			rs,
			"the emitted arguments deviate from the declared schema; the model can re-emit with correct parameters",
			diff
	}
	if t.ResultIsError {
		return verdictRecoverable,
			[]string{reasonResultError},
			"the tool returned an error the model can see and respond to",
			diff
	}
	return verdictSuccess,
		[]string{reasonValidCall},
		"the emitted arguments conform to the declared schema and the call shows no error signal",
		diff
}

// ---------------------------------------------------------------------------
// Tool handlers
// ---------------------------------------------------------------------------

// resolveToolCallTargets resolves the target tool call(s) for
// validate_tool_call / classify_failure from either the tool_call_id form (the
// §5 row id "<request_id>/<index>") or the (session_id, seq) form:
//
//   - tool_call_id form: exactly one target (the referenced row).
//   - (session_id, seq) form: every tool call emitted by that request.
//
// Providing both forms is ambiguous and rejected. The --session scope
// overrides a caller-supplied session_id (the Phase 6 scoping convention).
func (s *Server) resolveToolCallTargets(ctx context.Context, args map[string]any) ([]*store.ToolCall, *toolError) {
	id, hasID := argString(args, "tool_call_id")
	sessionID, hasSession := argString(args, "session_id")
	seq, hasSeq := argInt(args, "seq")

	if hasID && id != "" && ((hasSession && sessionID != "") || hasSeq) {
		return nil, &toolError{code: "INVALID_ARGUMENT", message: "provide either tool_call_id or session_id+seq, not both"}
	}

	if hasID && id != "" {
		tc, err := s.store.GetToolCall(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, &toolError{code: "NOT_FOUND", message: "tool call not found: " + id}
			}
			return nil, internalToolError(err)
		}
		return []*store.ToolCall{tc}, nil
	}

	// session_id + seq form.
	if s.sessionScope != "" {
		sessionID = s.sessionScope
	}
	if sessionID == "" {
		return nil, &toolError{code: "INVALID_ARGUMENT", message: "missing required parameter: session_id (or pass tool_call_id)"}
	}
	if !hasSeq {
		return nil, &toolError{code: "INVALID_ARGUMENT", message: "missing required parameter: seq (or pass tool_call_id)"}
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
	calls, err := s.store.ListToolCalls(ctx, store.ToolCallFilter{SessionID: sessionID})
	if err != nil {
		return nil, internalToolError(err)
	}
	var targets []*store.ToolCall
	for _, tc := range calls {
		if tc.RequestID == req.ID {
			targets = append(targets, tc)
		}
	}
	if len(targets) == 0 {
		return nil, &toolError{code: "NOT_FOUND", message: fmt.Sprintf("no tool calls emitted by session %s seq %d", sessionID, seq)}
	}
	return targets, nil
}

// schemasForToolCall loads the client-declared tools[] schemas from the
// request that emitted the tool call.
func (s *Server) schemasForToolCall(ctx context.Context, tc *store.ToolCall) ([]declaredTool, *toolError) {
	req, err := s.store.GetRequest(ctx, tc.SessionID, tc.Seq)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &toolError{code: "NOT_FOUND", message: fmt.Sprintf("originating request not found: session %s seq %d", tc.SessionID, tc.Seq)}
		}
		return nil, internalToolError(err)
	}
	return declaredTools(req.RequestJSON), nil
}

// handleValidateToolCall implements validate_tool_call: a schema diff of the
// emitted arguments against the schemas declared in the originating request.
// One resolved call returns a single diff object; a (session_id, seq) request
// that emitted several calls returns a JSON array of per-call diffs.
func (s *Server) handleValidateToolCall(ctx context.Context, args map[string]any) (any, *toolError) {
	targets, terr := s.resolveToolCallTargets(ctx, args)
	if terr != nil {
		return nil, terr
	}
	out := make([]toolCallDiffView, 0, len(targets))
	for _, tc := range targets {
		schemas, terr := s.schemasForToolCall(ctx, tc)
		if terr != nil {
			return nil, terr
		}
		out = append(out, toolCallDiffViewOf(tc, diffToolCall(tc.ToolName, tc.ArgumentsJSON, schemas)))
	}
	if len(out) == 1 {
		return out[0], nil
	}
	return out, nil
}

// handleClassifyFailure implements classify_failure: a verdict with reason
// keywords derived from the spec taxonomy, cached back to tool_calls.verdict +
// annotation_json (§5 on-demand pull model). A cached verdict is returned on
// subsequent calls unless refresh=true recomputes (and overwrites) it.
func (s *Server) handleClassifyFailure(ctx context.Context, args map[string]any) (any, *toolError) {
	targets, terr := s.resolveToolCallTargets(ctx, args)
	if terr != nil {
		return nil, terr
	}
	refresh, _ := argBool(args, "refresh")

	out := make([]toolCallVerdictView, 0, len(targets))
	for _, tc := range targets {
		if !refresh && tc.Verdict != "" {
			out = append(out, verdictViewFromCached(tc))
			continue
		}
		schemas, terr := s.schemasForToolCall(ctx, tc)
		if terr != nil {
			return nil, terr
		}
		verdict, reasons, why, d := classifyToolCall(tc, schemas)
		view := toolCallVerdictViewOf(tc, verdict, reasons, why, d, len(schemas) > 0)
		if err := s.store.SetVerdict(ctx, tc.ID, verdict, annotationFor(view)); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, &toolError{code: "NOT_FOUND", message: "tool call not found: " + tc.ID}
			}
			return nil, internalToolError(err)
		}
		out = append(out, view)
	}
	if len(out) == 1 {
		return out[0], nil
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Result views
// ---------------------------------------------------------------------------

// toolCallDiffView is one validate_tool_call result. Field names follow §7.1's
// {ok, unknown_params[], missing_required[], type_mismatches[],
// nearest_params[]} shape; tool_call_id / tool_name identify the call and
// tool_not_found / malformed carry the no-schema / unparseable cases.
type toolCallDiffView struct {
	ToolCallID      string             `json:"tool_call_id"`
	ToolName        string             `json:"tool_name"`
	OK              bool               `json:"ok"`
	UnknownParams   []string           `json:"unknown_params"`
	MissingRequired []string           `json:"missing_required"`
	TypeMismatches  []typeMismatchView `json:"type_mismatches"`
	NearestParams   []nearestParamView `json:"nearest_params"`
	ToolNotFound    bool               `json:"tool_not_found,omitempty"`
	Malformed       string             `json:"malformed,omitempty"`
}

type typeMismatchView struct {
	Param    string `json:"param"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
}

type nearestParamView struct {
	Param       string                `json:"param"`
	Suggestions []paramSuggestionView `json:"suggestions"`
}

type paramSuggestionView struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
}

func toolCallDiffViewOf(tc *store.ToolCall, d toolCallDiff) toolCallDiffView {
	v := toolCallDiffView{
		ToolCallID:      tc.ID,
		ToolName:        tc.ToolName,
		OK:              d.OK,
		UnknownParams:   d.UnknownParams,
		MissingRequired: d.MissingRequired,
		TypeMismatches:  typeMismatchViews(d.TypeMismatches),
		NearestParams:   nearestParamViews(d.NearestParams),
		ToolNotFound:    d.ToolNotFound,
		Malformed:       d.Malformed,
	}
	if v.UnknownParams == nil {
		v.UnknownParams = []string{}
	}
	if v.MissingRequired == nil {
		v.MissingRequired = []string{}
	}
	if v.TypeMismatches == nil {
		v.TypeMismatches = []typeMismatchView{}
	}
	if v.NearestParams == nil {
		v.NearestParams = []nearestParamView{}
	}
	return v
}

// toolCallVerdictView is one classify_failure result. It mirrors the
// annotation written back to tool_calls.annotation_json so the cached path can
// replay it without recomputation.
type toolCallVerdictView struct {
	ToolCallID     string             `json:"tool_call_id"`
	ToolName       string             `json:"tool_name"`
	Verdict        string             `json:"verdict"`
	Reasons        []string           `json:"reasons"`
	Why            string             `json:"why"`
	Cached         bool               `json:"cached,omitempty"`
	SchemaPresent  bool               `json:"schema_present"`
	ToolNotFound   bool               `json:"tool_not_found,omitempty"`
	MissingParams  []string           `json:"missing_params,omitempty"`
	UnknownParams  []string           `json:"unknown_params,omitempty"`
	TypeMismatches []typeMismatchView `json:"type_mismatches,omitempty"`
	Malformed      string             `json:"malformed,omitempty"`
}

// annotationJSON is the §5 annotation_json payload written with a verdict:
// {why, reasons, ...} — the classification evidence.
type annotationJSON struct {
	Why            string             `json:"why"`
	Reasons        []string           `json:"reasons"`
	Verdict        string             `json:"verdict"`
	SchemaPresent  bool               `json:"schema_present"`
	ToolNotFound   bool               `json:"tool_not_found,omitempty"`
	MissingParams  []string           `json:"missing_params,omitempty"`
	UnknownParams  []string           `json:"unknown_params,omitempty"`
	TypeMismatches []typeMismatchView `json:"type_mismatches,omitempty"`
	Malformed      string             `json:"malformed,omitempty"`
}

func toolCallVerdictViewOf(tc *store.ToolCall, verdict string, reasons []string, why string, d toolCallDiff, schemaPresent bool) toolCallVerdictView {
	return toolCallVerdictView{
		ToolCallID:     tc.ID,
		ToolName:       tc.ToolName,
		Verdict:        verdict,
		Reasons:        reasons,
		Why:            why,
		SchemaPresent:  schemaPresent,
		ToolNotFound:   d.ToolNotFound,
		MissingParams:  d.MissingRequired,
		UnknownParams:  d.UnknownParams,
		TypeMismatches: typeMismatchViews(d.TypeMismatches),
		Malformed:      d.Malformed,
	}
}

// verdictViewFromCached rebuilds the classification view from a stored verdict
// + annotation, so a subsequent classify_failure call returns the cached result
// without recomputation (§5 pull model).
func verdictViewFromCached(tc *store.ToolCall) toolCallVerdictView {
	v := toolCallVerdictView{
		ToolCallID: tc.ID,
		ToolName:   tc.ToolName,
		Verdict:    tc.Verdict,
		Cached:     true,
	}
	var a annotationJSON
	if len(tc.AnnotationJSON) > 0 {
		_ = json.Unmarshal(tc.AnnotationJSON, &a)
	}
	v.Reasons = a.Reasons
	v.Why = a.Why
	v.SchemaPresent = a.SchemaPresent
	v.ToolNotFound = a.ToolNotFound
	v.MissingParams = a.MissingParams
	v.UnknownParams = a.UnknownParams
	v.TypeMismatches = a.TypeMismatches
	v.Malformed = a.Malformed
	if len(v.Reasons) == 0 {
		v.Reasons = []string{"cached_verdict"}
	}
	if v.Why == "" {
		v.Why = "returned from the cached verdict (pass refresh=true to recompute)"
	}
	return v
}

// annotationFor serializes a classification view into the §5 annotation_json.
func annotationFor(view toolCallVerdictView) json.RawMessage {
	a := annotationJSON{
		Why:            view.Why,
		Reasons:        view.Reasons,
		Verdict:        view.Verdict,
		SchemaPresent:  view.SchemaPresent,
		ToolNotFound:   view.ToolNotFound,
		MissingParams:  view.MissingParams,
		UnknownParams:  view.UnknownParams,
		TypeMismatches: view.TypeMismatches,
		Malformed:      view.Malformed,
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil
	}
	return b
}

func typeMismatchViews(in []typeMismatch) []typeMismatchView {
	out := make([]typeMismatchView, 0, len(in))
	for _, m := range in {
		out = append(out, typeMismatchView{Param: m.Param, Expected: m.Expected, Got: m.Got})
	}
	return out
}

func nearestParamViews(in []nearestParam) []nearestParamView {
	out := make([]nearestParamView, 0, len(in))
	for _, n := range in {
		view := nearestParamView{Param: n.Param}
		for _, s := range n.Suggestions {
			view.Suggestions = append(view.Suggestions, paramSuggestionView{Name: s.Name, Score: s.Score})
		}
		out = append(out, view)
	}
	return out
}
