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

// DefaultFieldNames are the JSON keys the console payload log masks wholesale:
// message content and tool-call arguments (the redact plugin's structural
// defaults) plus credential-ish keys that could otherwise leak into logs
// through other paths.
var DefaultFieldNames = []string{"content", "arguments", "api_key", "password", "token"}

// redactor is the shared redaction engine: object fields listed in fields are
// replaced wholesale with Redacted, and string values are regex-redacted with
// re. It backs both the redact plugin (configurable per [plugins.redact]) and
// the gateway's console payload logging (defaults), so the two can never
// diverge on what counts as sensitive.
type redactor struct {
	re     []*regexp.Regexp
	fields map[string]bool
}

// newRedactor builds the engine from a config. Invalid regex patterns are a
// build-time (config) error, not a runtime one.
func newRedactor(cfg RedactConfig) (*redactor, error) {
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
	return &redactor{re: res, fields: fields}, nil
}

// mustRedactor builds the console-log default engine. All inputs are
// compile-time constants (DefaultFieldNames + defaultPatterns), so this cannot
// fail.
func mustRedactor(cfg RedactConfig) *redactor {
	r, err := newRedactor(cfg)
	if err != nil {
		panic("plugins: build default redactor: " + err.Error())
	}
	return r
}

// logRedactor is the console-log redaction instance: the default sensitive
// patterns plus the default field set.
var logRedactor = mustRedactor(RedactConfig{FieldNames: &DefaultFieldNames})

// RedactPayload returns a redacted copy of payload for console/access logging:
// values at the default field set are masked wholesale and string values are
// regex-redacted with the default sensitive patterns, so the output can never
// leak a field the redact plugin would mask. Non-JSON input is returned
// unchanged.
func RedactPayload(payload json.RawMessage) json.RawMessage {
	if !json.Valid(payload) {
		return payload
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return payload
	}
	out, err := json.Marshal(logRedactor.redactValue(v))
	if err != nil {
		return payload
	}
	return out
}

// redactValue returns the redacted copy of a decoded JSON value: strings are
// regex-redacted, and object fields listed in the config are replaced
// wholesale with [REDACTED] (everything else is preserved recursively).
func (r *redactor) redactValue(v any) any {
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

// redactText regex-redacts one string against the configured patterns.
func (r *redactor) redactText(s string) string {
	for _, re := range r.re {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

// redactArguments redacts a tool-call arguments string. When the string is
// valid JSON it is redacted structurally (field-list + regex over string
// values) so fields survive intact; otherwise the raw string is regex-redacted.
func (r *redactor) redactArguments(args string) string {
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

// redact is the §4.5 reference plugin: regex + field-list redaction of message
// content and tool-call arguments, structure-preserving. Matches are replaced
// with [REDACTED].
type redact struct {
	r *redactor
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

// NewRedact builds the redact plugin over the shared redaction engine.
func NewRedact(cfg RedactConfig) (Plugin, error) {
	r, err := newRedactor(cfg)
	if err != nil {
		return nil, err
	}
	return &redact{r: r}, nil
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
		msg["content"] = r.r.redactValue(content)
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
			fn["arguments"] = r.r.redactArguments(args)
		}
	}
}
