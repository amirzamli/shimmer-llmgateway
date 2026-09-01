package mcp

import (
	"encoding/json"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// sessionSummaryView is a list_sessions item: a session summary with the §5
// failure counters.
type sessionSummaryView struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	FirstAlias    string `json:"first_alias"`
	FirstModel    string `json:"first_model"`
	RequestCount  int    `json:"request_count"`
	ToolCallCount int    `json:"tool_call_count"`
	FailureCount  int    `json:"failure_count"`
	// Expired is true once retention removed the session's payloads; the
	// usage metadata is retained.
	Expired bool `json:"expired"`
}

func sessionSummaryViewOf(s *store.SessionSummary) sessionSummaryView {
	return sessionSummaryView{
		ID:            s.ID,
		CreatedAt:     s.CreatedAt,
		FirstAlias:    s.FirstAlias,
		FirstModel:    s.FirstModel,
		RequestCount:  s.RequestCount,
		ToolCallCount: s.ToolCallCount,
		FailureCount:  s.FailureCount,
		Expired:       s.Expired,
	}
}

// requestDetailView is the get_request payload: every stored column plus the
// original/filtered request and response bodies.
type requestDetailView struct {
	ID           string `json:"id"`
	SessionID    string `json:"session_id"`
	Seq          int    `json:"seq"`
	CreatedAt    string `json:"created_at"`
	Alias        string `json:"alias"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Endpoint     string `json:"endpoint"`
	DurationMS   int64  `json:"duration_ms"`
	StatusCode   int    `json:"status_code"`
	FinishReason string `json:"finish_reason"`
	// CostSchema identifies the pricing snapshot that priced the row
	// (requests.cost_schema); empty for legacy/unpriced rows.
	CostSchema       string           `json:"cost_schema,omitempty"`
	Usage            json.RawMessage  `json:"usage"`
	Request          json.RawMessage  `json:"request"`
	RequestFiltered  json.RawMessage  `json:"request_filtered"`
	Response         json.RawMessage  `json:"response"`
	ResponseFiltered json.RawMessage  `json:"response_filtered"`
	PluginsApplied   []string         `json:"plugins_applied"`
	Error            *store.ErrorInfo `json:"error"`
	Truncated        bool             `json:"truncated"`
}

func requestDetailViewOf(r *store.Request) requestDetailView {
	return requestDetailView{
		ID:               r.ID,
		SessionID:        r.SessionID,
		Seq:              r.Seq,
		CreatedAt:        r.CreatedAt,
		Alias:            r.Alias,
		Provider:         r.Provider,
		Model:            r.Model,
		Endpoint:         r.Endpoint,
		DurationMS:       r.DurationMS,
		StatusCode:       r.StatusCode,
		FinishReason:     r.FinishReason,
		CostSchema:       r.CostSchema,
		Usage:            embedRaw(r.Usage),
		Request:          embedRaw(r.RequestJSON),
		RequestFiltered:  embedRaw(r.RequestFilteredJSON),
		Response:         embedRaw(r.ResponseJSON),
		ResponseFiltered: embedRaw(r.ResponseFilteredJSON),
		PluginsApplied:   r.PluginsApplied,
		Error:            r.Error,
		Truncated:        r.Truncated,
	}
}

// toolCallRecordView is a list_tool_calls item: a tool_calls row.
type toolCallRecordView struct {
	ID            string          `json:"id"`
	RequestID     string          `json:"request_id"`
	SessionID     string          `json:"session_id"`
	Seq           int             `json:"seq"`
	ToolName      string          `json:"tool_name"`
	Arguments     json.RawMessage `json:"arguments"`
	ResultIsError bool            `json:"result_is_error"`
	ResultSnippet string          `json:"result_snippet"`
	Verdict       string          `json:"verdict"`
	Annotation    json.RawMessage `json:"annotation"`
}

func toolCallRecordViewOf(t *store.ToolCall) toolCallRecordView {
	return toolCallRecordView{
		ID:            t.ID,
		RequestID:     t.RequestID,
		SessionID:     t.SessionID,
		Seq:           t.Seq,
		ToolName:      t.ToolName,
		Arguments:     embedRaw(t.ArgumentsJSON),
		ResultIsError: t.ResultIsError,
		ResultSnippet: t.ResultSnippet,
		Verdict:       t.Verdict,
		Annotation:    embedRaw(t.AnnotationJSON),
	}
}

// searchResultView is a search_conversations item.
type searchResultView struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Seq       int    `json:"seq"`
	Alias     string `json:"alias"`
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Snippet   string `json:"snippet"`
}

func searchResultViewOf(r *store.SearchResult) searchResultView {
	return searchResultView{
		RequestID: r.RequestID,
		SessionID: r.SessionID,
		Seq:       r.Seq,
		Alias:     r.Alias,
		Model:     r.Model,
		CreatedAt: r.CreatedAt,
		Snippet:   r.Snippet,
	}
}

// gatewayStatusView is the gateway_status payload.
type gatewayStatusView struct {
	StorePath      string   `json:"store_path"`
	SessionCount   int      `json:"session_count"`
	RequestCount   int      `json:"request_count"`
	ToolCallCount  int      `json:"tool_call_count"`
	FailureCount   int      `json:"failure_count"`
	DiskUsageBytes int64    `json:"disk_usage_bytes"`
	RetentionDays  int      `json:"retention_days"`
	Aliases        []string `json:"aliases"`
}

// embedRaw returns b as a JSON value, falling back to a JSON string when b is
// not valid JSON (the mirror of the store's §8 embedBytes convention).
func embedRaw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return b
	}
	s, _ := json.Marshal(string(b))
	return s
}
