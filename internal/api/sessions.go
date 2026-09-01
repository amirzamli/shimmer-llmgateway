package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/store"
)

// sessionSummaryView is the §6.2 GET /api/sessions item.
type sessionSummaryView struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	FirstAlias    string `json:"first_alias"`
	FirstModel    string `json:"first_model"`
	RequestCount  int    `json:"request_count"`
	ToolCallCount int    `json:"tool_call_count"`
	FailureCount  int    `json:"failure_count"`
	// Expired is true once retention removed the session's payloads; the
	// usage metadata (counts, costs) is retained.
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

// requestView is a requests row in a session detail, carrying both the
// original and (when plugins ran) filtered payloads so the UI can show the
// raw original-vs-filtered view.
type requestView struct {
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
	CostSchema           string           `json:"cost_schema,omitempty"`
	Usage                json.RawMessage  `json:"usage"`
	RequestJSON          json.RawMessage  `json:"request_json"`
	RequestFilteredJSON  json.RawMessage  `json:"request_filtered_json"`
	ResponseJSON         json.RawMessage  `json:"response_json"`
	ResponseFilteredJSON json.RawMessage  `json:"response_filtered_json"`
	PluginsApplied       []string         `json:"plugins_applied"`
	Error                *store.ErrorInfo `json:"error"`
	Truncated            bool             `json:"truncated"`
}

func requestViewOf(r *store.Request) requestView {
	return requestView{
		ID:                   r.ID,
		SessionID:            r.SessionID,
		Seq:                  r.Seq,
		CreatedAt:            r.CreatedAt,
		Alias:                r.Alias,
		Provider:             r.Provider,
		Model:                r.Model,
		Endpoint:             r.Endpoint,
		DurationMS:           r.DurationMS,
		StatusCode:           r.StatusCode,
		FinishReason:         r.FinishReason,
		CostSchema:           r.CostSchema,
		Usage:                r.Usage,
		RequestJSON:          r.RequestJSON,
		RequestFilteredJSON:  r.RequestFilteredJSON,
		ResponseJSON:         r.ResponseJSON,
		ResponseFilteredJSON: r.ResponseFilteredJSON,
		PluginsApplied:       r.PluginsApplied,
		Error:                r.Error,
		Truncated:            r.Truncated,
	}
}

// toolCallView is a tool_calls row in a session detail.
type toolCallView struct {
	ID             string          `json:"id"`
	RequestID      string          `json:"request_id"`
	Seq            int             `json:"seq"`
	ToolName       string          `json:"tool_name"`
	ArgumentsJSON  json.RawMessage `json:"arguments_json"`
	ResultIsError  bool            `json:"result_is_error"`
	ResultSnippet  string          `json:"result_snippet"`
	Verdict        string          `json:"verdict"`
	AnnotationJSON json.RawMessage `json:"annotation_json"`
}

func toolCallViewOf(t *store.ToolCall) toolCallView {
	return toolCallView{
		ID:             t.ID,
		RequestID:      t.RequestID,
		Seq:            t.Seq,
		ToolName:       t.ToolName,
		ArgumentsJSON:  t.ArgumentsJSON,
		ResultIsError:  t.ResultIsError,
		ResultSnippet:  t.ResultSnippet,
		Verdict:        t.Verdict,
		AnnotationJSON: t.AnnotationJSON,
	}
}

// handleSessionsList maps the §6.2 session filters onto the store query.
func (a *API) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.SessionFilter{
		Alias:  q.Get("alias"),
		Model:  q.Get("model"),
		Status: q.Get("status"),
		Search: q.Get("q"),
	}
	if since, err := time.Parse(time.RFC3339, q.Get("since")); err == nil {
		f.Since = since
	}
	if until, err := time.Parse(time.RFC3339, q.Get("until")); err == nil {
		f.Until = until
	}
	if limit, err := strconv.Atoi(q.Get("limit")); err == nil && limit > 0 {
		f.Limit = limit
	}
	sums, err := a.store.ListSessions(r.Context(), f)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "list sessions: "+err.Error())
		return
	}
	views := make([]sessionSummaryView, 0, len(sums))
	for _, s := range sums {
		views = append(views, sessionSummaryViewOf(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": views})
}

// handleSessionGet returns the session conversation: requests in order with
// their tool calls and verdict badges.
func (a *API) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	sess, err := a.store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.writeError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "get session: "+err.Error())
		return
	}
	reqs := make([]requestView, 0, len(sess.Requests))
	for _, req := range sess.Requests {
		reqs = append(reqs, requestViewOf(req))
	}
	calls := make([]toolCallView, 0, len(sess.ToolCalls))
	for _, tc := range sess.ToolCalls {
		calls = append(calls, toolCallViewOf(tc))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            sess.ID,
		"created_at":    sess.CreatedAt,
		"first_alias":   sess.FirstAlias,
		"first_model":   sess.FirstModel,
		"request_count": sess.RequestCount,
		"failure_count": sess.FailureCount,
		"expired":       sess.Expired,
		"requests":      reqs,
		"tool_calls":    calls,
	})
}

// handleSessionExport streams the §8 canonical JSONL for one session — the
// same format shared by the MCP export_session tool and the gateway append
// log.
func (a *API) handleSessionExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	if err := a.store.ExportSession(r.Context(), r.PathValue("id"), w); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.writeError(w, http.StatusNotFound, "NOT_FOUND", "session not found")
			return
		}
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "export session: "+err.Error())
		return
	}
}
