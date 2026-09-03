package plugins

import (
	"context"
	"encoding/json"
)

// sanitizeTools is the request-side plugin that rewrites client tool
// definitions before forward. Wherever a JSON-schema node inside
// tools[].function.parameters declares a non-empty "enum" together with a
// "oneOf" or "anyOf" combinator, the combinator is dropped and the enum
// (the authoritative value set) is kept.
//
// The enum+combinator combination is valid JSON Schema, but some
// OpenAI-compatible providers mishandle it: commandcode.ai serving
// openai/gpt-5.6-luna answers with a silent empty completion (HTTP 200,
// content null, finish_reason "length", no usage) instead of an error or a
// real answer. Removing either side of the duplication was verified to fix
// the failure, so the combinator — the redundant half — is what this plugin
// removes. Bodies without tools, and schemas without the duplication, pass
// through byte-for-byte.
type sanitizeTools struct{}

var _ Plugin = (*sanitizeTools)(nil)

func init() {
	Register("sanitize_tools", func(o Options) (Plugin, error) {
		return NewSanitizeTools(), nil
	})
	registerInfo(Info{
		Name:         "sanitize_tools",
		Kind:         "transform",
		Source:       "built-in",
		Configurable: false,
		Description:  "Rewrites client tool schemas before forward: wherever a tool parameter declares both an enum and a redundant oneOf/anyOf combinator, the combinator is dropped and the enum kept. The enum+oneOf duplication is valid JSON Schema but some OpenAI-compatible providers answer it with a silent empty completion (200, finish_reason \"length\", no usage) instead of an error — observed on commandcode.ai serving gpt-5.6-luna. Requests without tool definitions pass through byte-for-byte.",
	})
}

// NewSanitizeTools builds the sanitize_tools plugin. It takes no
// [plugins.sanitize_tools] configuration in v1.
func NewSanitizeTools() Plugin { return &sanitizeTools{} }

func (s *sanitizeTools) Name() string { return "sanitize_tools" }

// FilterRequest sanitizes the tools array in place. Unparseable bodies and
// unexpected tools shapes pass through unchanged; the body is re-serialized
// only when at least one tool actually changed.
func (s *sanitizeTools) FilterRequest(ctx context.Context, req *Request) error {
	if len(req.Body) == 0 || !json.Valid(req.Body) {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &m); err != nil {
		return nil // not a JSON object: pass through
	}
	raw, ok := m["tools"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil // tools is not an array: leave it alone
	}
	changed := false
	for i, tr := range tools {
		nt, ok := s.sanitizeTool(tr)
		if !ok {
			continue
		}
		tools[i] = nt
		changed = true
	}
	if !changed {
		return nil
	}
	nb, err := json.Marshal(tools)
	if err != nil {
		return nil
	}
	m["tools"] = nb
	body, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	req.Body = body
	return nil
}

// RequestOnly declares sanitize_tools a request-side plugin (it repairs client
// tool schemas, which only appear in requests). Listing it in a plugin list
// therefore never populates the response chain, so it cannot switch streaming
// into buffer mode — the response side would be a no-op anyway.
func (s *sanitizeTools) RequestOnly() bool { return true }

// FilterResponse is a no-op: provider responses never carry client tool
// schemas. The method exists to satisfy the §4.5 plugin seam; chain builders
// skip this plugin on the response side entirely (see RequestOnly).
func (s *sanitizeTools) FilterResponse(ctx context.Context, resp *Response) error {
	return nil
}

// sanitizeTool rewrites one tool definition, reporting whether anything
// changed. Untouched tools keep their exact bytes.
func (s *sanitizeTools) sanitizeTool(tr json.RawMessage) (json.RawMessage, bool) {
	var tool map[string]json.RawMessage
	if err := json.Unmarshal(tr, &tool); err != nil {
		return nil, false
	}
	fnRaw, ok := tool["function"]
	if !ok {
		return nil, false
	}
	var fn map[string]json.RawMessage
	if err := json.Unmarshal(fnRaw, &fn); err != nil {
		return nil, false
	}
	paramsRaw, ok := fn["parameters"]
	if !ok {
		return nil, false
	}
	var schema any
	if err := json.Unmarshal(paramsRaw, &schema); err != nil {
		return nil, false
	}
	if !sanitizeSchemaNode(schema) {
		return nil, false
	}
	nb, err := json.Marshal(schema)
	if err != nil {
		return nil, false
	}
	fn["parameters"] = nb
	nfn, err := json.Marshal(fn)
	if err != nil {
		return nil, false
	}
	tool["function"] = nfn
	nt, err := json.Marshal(tool)
	if err != nil {
		return nil, false
	}
	return nt, true
}

// sanitizeSchemaNode walks one JSON-schema value in place, dropping
// "oneOf"/"anyOf" from every object node that also declares a non-empty
// "enum", at any depth (properties, items, $defs, prefixItems, ...). It
// reports whether anything was removed.
func sanitizeSchemaNode(v any) bool {
	changed := false
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if sanitizeSchemaNode(e) {
				changed = true
			}
		}
	case map[string]any:
		if enums, ok := t["enum"].([]any); ok && len(enums) > 0 {
			for _, k := range []string{"oneOf", "anyOf"} {
				if _, exists := t[k]; exists {
					delete(t, k)
					changed = true
				}
			}
		}
		for _, val := range t {
			if sanitizeSchemaNode(val) {
				changed = true
			}
		}
	}
	return changed
}
