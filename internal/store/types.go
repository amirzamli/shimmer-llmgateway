package store

import (
	"encoding/json"
	"time"
)

// ErrorInfo is the {code, message} error shape stored in requests.error_json.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CaptureRecord carries every §4.3 record field for one request. Capture
// fills Seq from the per-session order and generates ID/SessionID UUIDs when
// empty.
type CaptureRecord struct {
	ID        string // request id; UUIDv4 generated when empty
	SessionID string // UUIDv4 generated when empty
	// CreatedAt is the request start time; time.Now() when zero.
	CreatedAt    time.Time
	Alias        string
	Provider     string // provider template name
	Model        string
	Endpoint     string
	DurationMS   int64
	StatusCode   int
	FinishReason string
	// Usage is the provider usage object {prompt_tokens,...} or nil.
	Usage json.RawMessage
	// RequestJSON is the original client body (messages + tool schemas).
	RequestJSON json.RawMessage
	// RequestFilteredJSON is the body after request plugins (nil if none).
	RequestFilteredJSON json.RawMessage
	// ResponseJSON is the reassembled completion(s).
	ResponseJSON json.RawMessage
	// ResponseFilteredJSON is the body after response plugins (nil if none).
	ResponseFilteredJSON json.RawMessage
	// PluginsApplied is the plugin names that ran, nil if none.
	PluginsApplied []string
	// Error is the {code, message} of a non-2xx provider response, or nil.
	Error     *ErrorInfo
	Truncated bool
	// ChunkCount is the number of SSE data chunks reassembled for a streaming
	// request (0 for non-stream). The §5 schema has no column for it, so it
	// surfaces only in the §8 append-log request line.
	ChunkCount int

	// Seq is the per-session request order. Capture sets it; callers may read
	// it after Capture returns.
	Seq int
}

// SessionSummary is a sessions row.
type SessionSummary struct {
	ID            string
	CreatedAt     string
	FirstAlias    string
	FirstModel    string
	RequestCount  int
	ToolCallCount int
	FailureCount  int
}

// Session is a session with its requests and tool calls loaded (the
// conversation replay data the UI/MCP need).
type Session struct {
	SessionSummary
	Requests  []*Request
	ToolCalls []*ToolCall
}

// Request is a requests row.
type Request struct {
	ID                   string
	SessionID            string
	Seq                  int
	CreatedAt            string
	Alias                string
	Provider             string
	Model                string
	Endpoint             string
	DurationMS           int64
	StatusCode           int
	FinishReason         string
	Usage                json.RawMessage
	RequestJSON          json.RawMessage
	RequestFilteredJSON  json.RawMessage
	ResponseJSON         json.RawMessage
	ResponseFilteredJSON json.RawMessage
	PluginsApplied       []string
	Error                *ErrorInfo
	Truncated            bool
	// ChunkCount mirrors CaptureRecord.ChunkCount for the §8 append-log line;
	// stored rows read it as 0 because the §5 schema has no column.
	ChunkCount int
}

// Request returns the requests-row view of a captured record, used by the §8
// append log after Capture.
func (r *CaptureRecord) Request() *Request {
	return &Request{
		ID:                   r.ID,
		SessionID:            r.SessionID,
		Seq:                  r.Seq,
		CreatedAt:            formatTS(r.CreatedAt),
		Alias:                r.Alias,
		Provider:             r.Provider,
		Model:                r.Model,
		Endpoint:             r.Endpoint,
		DurationMS:           r.DurationMS,
		StatusCode:           r.StatusCode,
		FinishReason:         r.FinishReason,
		Usage:                r.Usage,
		RequestJSON:          r.RequestJSON,
		RequestFilteredJSON:  r.RequestFilteredJSON,
		ResponseJSON:         r.ResponseJSON,
		ResponseFilteredJSON: r.ResponseFilteredJSON,
		PluginsApplied:       r.PluginsApplied,
		Error:                r.Error,
		Truncated:            r.Truncated,
		ChunkCount:           r.ChunkCount,
	}
}

// SessionSummary returns the session row a first request (seq == 1) would have
// created, for the §8 session_start append-log line.
func (r *CaptureRecord) SessionSummary() SessionSummary {
	return SessionSummary{
		ID:            r.SessionID,
		CreatedAt:     formatTS(r.CreatedAt),
		FirstAlias:    r.Alias,
		FirstModel:    r.Model,
		RequestCount:  1,
		ToolCallCount: len(extractToolCalls(r.ResponseJSON)),
		FailureCount:  boolInt(r.StatusCode >= 400 || r.Error != nil),
	}
}

// ToolCall is a tool_calls row.
type ToolCall struct {
	ID             string // "<request_id>/<index>"
	RequestID      string
	SessionID      string
	Seq            int
	ToolName       string
	ArgumentsJSON  json.RawMessage
	ResultIsError  bool
	ResultSnippet  string
	Verdict        string
	AnnotationJSON json.RawMessage
}

// SessionFilter filters ListSessions. Zero fields are not applied.
type SessionFilter struct {
	Alias  string
	Model  string
	Status string // "", "ok", or "error" (error = session has failures)
	Since  time.Time
	Until  time.Time
	Search string // LIKE text search over request/response payloads
	Limit  int    // 0 = no limit
}

// ToolCallFilter filters ListToolCalls. Zero fields are not applied.
type ToolCallFilter struct {
	SessionID string
	ToolName  string
	Verdict   string // one of SUCCESS | RECOVERABLE | BLIND_ERROR
	Limit     int    // 0 = no limit
}

// SearchResult is one request that matched a conversation text search.
type SearchResult struct {
	RequestID string
	SessionID string
	Seq       int
	Alias     string
	Model     string
	CreatedAt string
	Snippet   string // text window around the first match
}

// Status is the §7.1 gateway_status payload: store path, counts, disk usage,
// and retention.
type Status struct {
	StorePath      string
	SessionCount   int
	RequestCount   int
	ToolCallCount  int
	FailureCount   int
	DiskUsageBytes int64
	RetentionDays  int
}
