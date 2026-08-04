package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// Redacted is the literal every redacted value is replaced with.
const Redacted = "[REDACTED]"

// RedactConfig is the optional [plugins.redact] TOML table. Pointer fields
// distinguish "unset" from "explicitly empty": when Patterns is nil the
// default sensitive patterns are used; when FieldNames is nil no fields are
// redacted by name.
type RedactConfig struct {
	Patterns   *[]string `json:"patterns"`
	FieldNames *[]string `json:"field_names"`
}

// defaultPatterns are the plan-assumption 11 sensitive defaults: API-key-like,
// email, phone, and SSN-like.
var defaultPatterns = []string{
	`\bsk-[A-Za-z0-9_-]{16,}\b`,                      // API key-like (OpenAI style)
	`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`, // email
	`\+?[0-9][0-9\s().-]{7,}[0-9]`,                   // phone
	`\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`,                 // SSN-like
}

// redact is the §4.5 reference plugin: regex + field-list redaction of message
// content and tool-call arguments, structure-preserving. Matches are replaced
// with [REDACTED].
type redact struct {
	re     []*regexp.Regexp
	fields map[string]bool
}

var _ Plugin = (*redact)(nil)

func init() {
	Register("redact", func(o Options) (Plugin, error) {
		cfg := RedactConfig{}
		if o.Config != nil {
			b, err := json.Marshal(o.Config)
			if err != nil {
				return nil, fmt.Errorf("plugins: redact config: %w", err)
			}
			if err := json.Unmarshal(b, &cfg); err != nil {
				return nil, fmt.Errorf("plugins: redact config: %w", err)
			}
		}
		return NewRedact(cfg)
	})
}

// NewRedact builds the redact plugin. Invalid regex patterns are a build-time
// (config) error, not a runtime one.
func NewRedact(cfg RedactConfig) (Plugin, error) {
	patterns := defaultPatterns
	if cfg.Patterns != nil {
		patterns = *cfg.Patterns
	}
	res := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("plugins: redact: invalid pattern %q: %w", p, err)
		}
		res = append(res, re)
	}
	fields := map[string]bool{}
	if cfg.FieldNames != nil {
		for _, f := range *cfg.FieldNames {
			fields[f] = true
		}
	}
	return &redact{re: res, fields: fields}, nil
}

func (r *redact) Name() string { return "redact" }

func (r *redact) FilterRequest(ctx context.Context, req *Request) error {
	filtered, err := r.filterRequestJSON(req.Body)
	if err != nil {
		return err
	}
	req.Body = filtered
	return nil
}

func (r *redact) FilterResponse(ctx context.Context, resp *Response) error {
	filtered, err := r.filterResponseJSON(resp.Body)
	if err != nil {
		return err
	}
	resp.Body = filtered
	return nil
}

// filterRequestJSON redacts message content and tool-call arguments in a chat
// completions request body. Non-JSON bodies pass through unchanged.
func (r *redact) filterRequestJSON(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	if msgs, ok := m["messages"].([]any); ok {
		for _, mi := range msgs {
			if msg, ok := mi.(map[string]any); ok {
				r.redactMessage(msg)
			}
		}
	}
	return json.Marshal(m)
}

// filterResponseJSON redacts message content and tool-call arguments in a chat
// completion response (streaming reassembled or non-stream). Non-JSON bodies
// pass through unchanged.
func (r *redact) filterResponseJSON(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, nil
	}
	if choices, ok := m["choices"].([]any); ok {
		for _, ci := range choices {
			ch, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			if msg, ok := ch["message"].(map[string]any); ok {
				r.redactMessage(msg)
			}
		}
	}
	return json.Marshal(m)
}

// redactMessage redacts one message: its content (string or array of content
// parts) and its tool_calls' function.arguments.
func (r *redact) redactMessage(msg map[string]any) {
	if content, ok := msg["content"]; ok {
		msg["content"] = r.redactValue(content)
	}
	if tcs, ok := msg["tool_calls"].([]any); ok {
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tc["function"].(map[string]any)
			if !ok {
				continue
			}
			args, ok := fn["arguments"].(string)
			if !ok {
				continue
			}
			fn["arguments"] = r.redactArguments(args)
		}
	}
}

// redactArguments redacts a tool-call arguments string. When the string is
// valid JSON it is redacted structurally (field-list + regex over string
// values) so fields survive intact; otherwise the raw string is regex-redacted.
func (r *redact) redactArguments(args string) string {
	if json.Valid([]byte(args)) {
		var v any
		if err := json.Unmarshal([]byte(args), &v); err == nil {
			if b, err := json.Marshal(r.redactValue(v)); err == nil {
				return string(b)
			}
		}
	}
	return r.redactText(args)
}

// redactValue returns the redacted copy of a decoded JSON value: strings are
// regex-redacted, and object fields listed in the config are replaced
// wholesale with [REDACTED] (everything else is preserved recursively).
func (r *redact) redactValue(v any) any {
	switch t := v.(type) {
	case string:
		return r.redactText(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = r.redactValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if r.fields[k] {
				out[k] = Redacted
			} else {
				out[k] = r.redactValue(val)
			}
		}
		return out
	default:
		return v
	}
}

func (r *redact) redactText(s string) string {
	for _, re := range r.re {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}
