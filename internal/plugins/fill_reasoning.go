package plugins

import (
	"context"
	"encoding/json"
)

// fillReasoningContent is the request-side plugin that backfills an empty
// reasoning_content on assistant messages that lack it.
//
// deepseek-v4-flash behind opencode.ai/zen/go ("Console Go") enforces a
// thinking-mode round-trip: once a conversation carries reasoning, every
// assistant message with tool_calls must carry a reasoning_content field back
// to the API, or the upstream rejects the request with
//
//	invalid_request_error: The `reasoning_content` in the thinking mode must
//	be passed back to the API.
//
// Agent clients (opencode, pi) hold thinking in their own message parts and
// occasionally re-send an assistant turn without the field — usually a pure
// tool-call turn whose reasoning the client did not persist. The upstream
// check is presence-based: a missing field is fatal, an empty string is
// accepted. This plugin adds `reasoning_content: ""` to every assistant
// message that has no (non-null) reasoning_content, so those re-sent turns
// satisfy the round-trip contract. Messages that already carry a value pass
// through byte-for-byte.
type fillReasoningContent struct{}

var _ Plugin = (*fillReasoningContent)(nil)

func init() {
	Register("fill_reasoning_content", func(o Options) (Plugin, error) {
		return NewFillReasoningContent(), nil
	})
	registerInfo(Info{
		Name:         "fill_reasoning_content",
		Kind:         "transform",
		Source:       "built-in",
		Configurable: false,
		Description:  `Fixes deepseek thinking-mode 400s ("The reasoning_content in the thinking mode must be passed back to the API") from opencode.ai/zen/go ("Console Go"): backfills an empty reasoning_content on every assistant message that lacks it, satisfying the upstream's presence check. Opt-in per instance (e.g. the opencode_go template).`,
	})
}

// NewFillReasoningContent builds the fill_reasoning_content plugin. It takes
// no [plugins.fill_reasoning_content] configuration in v1.
func NewFillReasoningContent() Plugin { return &fillReasoningContent{} }

func (f *fillReasoningContent) Name() string { return "fill_reasoning_content" }

// FilterRequest backfills reasoning_content on assistant messages in place.
// Unparseable bodies and unexpected shapes pass through unchanged; the body is
// re-serialized only when at least one message actually changed.
func (f *fillReasoningContent) FilterRequest(ctx context.Context, req *Request) error {
	if len(req.Body) == 0 || !json.Valid(req.Body) {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &m); err != nil {
		return nil // not a JSON object: pass through
	}
	raw, ok := m["messages"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil // messages is not an array: leave it alone
	}
	changed := false
	for i, mr := range msgs {
		nf, ok := fillAssistantReasoning(mr)
		if !ok {
			continue
		}
		msgs[i] = nf
		changed = true
	}
	if !changed {
		return nil
	}
	nb, err := json.Marshal(msgs)
	if err != nil {
		return nil
	}
	m["messages"] = nb
	body, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	req.Body = body
	return nil
}

// fillAssistantReasoning adds an empty reasoning_content to one assistant
// message that lacks it, reporting whether anything changed. Non-assistant
// messages and messages that already carry a non-null reasoning_content stay
// untouched (kept as their exact bytes).
func fillAssistantReasoning(mr json.RawMessage) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(mr, &msg); err != nil {
		return nil, false
	}
	var role string
	if err := json.Unmarshal(msg["role"], &role); err != nil {
		return nil, false
	}
	if role != "assistant" {
		return nil, false
	}
	if rc, ok := msg["reasoning_content"]; ok && string(rc) != "null" {
		return nil, false
	}
	msg["reasoning_content"] = json.RawMessage(`""`)
	nb, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	return nb, true
}

// RequestOnly declares fill_reasoning_content a request-side plugin: it
// repairs client history, which only appears in requests. Listing it in a
// plugin list therefore never populates the response chain, so it cannot
// switch streaming into buffer mode — the response side would be a no-op
// anyway.
func (f *fillReasoningContent) RequestOnly() bool { return true }

// FilterResponse is a no-op: the provider response never needs its own
// reasoning_content repaired. The method exists to satisfy the §4.5 plugin
// seam; chain builders skip this plugin on the response side entirely (see
// RequestOnly).
func (f *fillReasoningContent) FilterResponse(ctx context.Context, resp *Response) error {
	return nil
}
