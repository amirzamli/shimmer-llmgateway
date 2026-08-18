package gateway

import (
	"encoding/json"

	"github.com/amirzamli/shimmer-llmgateway/internal/plugins"
)

// redactPayload returns a copy of payload safe for console logging: values at
// the default sensitive field set (content, arguments, api_key, password,
// token) are replaced wholesale with plugins.Redacted, and string values are
// regex-redacted against the shared default patterns (API keys, emails,
// phones, SSNs). It is the same redaction engine the redact plugin runs, so
// log_payloads console output can never leak a field the plugin redactor would
// mask. Non-JSON input is returned unchanged.
func redactPayload(payload json.RawMessage) json.RawMessage {
	return plugins.RedactPayload(payload)
}
