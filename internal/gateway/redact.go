package gateway

import (
	"encoding/json"

	"shimmer-llmgateway/internal/plugins"
)

// redactPayload returns a copy of payload with every value at a JSON key
// "content" or "arguments" replaced by plugins.Redacted, recursively, so prompt
// and tool-call contents never appear in the console payload log. All other
// structure and metadata (roles, model, usage, finish_reason, status, tool
// names) is preserved. The whole subtree at those keys is replaced — not just
// string leaves — so multimodal content arrays and nested parts cannot leak.
// Non-JSON input is returned unchanged.
func redactPayload(payload json.RawMessage) json.RawMessage {
	if !json.Valid(payload) {
		return payload
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return payload
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return payload
	}
	return out
}

// redactValue returns the redacted copy of a decoded JSON value: any value
// under a "content" or "arguments" key becomes plugins.Redacted wholesale,
// everything else is preserved recursively.
func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if k == "content" || k == "arguments" {
				out[k] = plugins.Redacted
				continue
			}
			out[k] = redactValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactValue(e)
		}
		return out
	default:
		return v
	}
}
