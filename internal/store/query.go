package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	requestSelect = `SELECT id, session_id, seq, created_at, alias, provider, model, endpoint,
		duration_ms, status_code, finish_reason, usage_json, request_json,
		request_filtered_json, response_json, response_filtered_json,
		plugins_applied, error_json, truncated,
		prompt_tokens, completion_tokens, cached_tokens,
		cost_input, cost_output, cost_cache_read, cost_cache_write, cost_total,
		cost_priced FROM requests`

	toolCallSelect = `SELECT id, request_id, session_id, seq, tool_name, arguments_json,
		result_is_error, result_snippet, verdict, annotation_json FROM tool_calls`
)

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSessionSummary(s rowScanner) (SessionSummary, error) {
	var out SessionSummary
	err := s.Scan(&out.ID, &out.CreatedAt, &out.FirstAlias, &out.FirstModel,
		&out.RequestCount, &out.ToolCallCount, &out.FailureCount)
	return out, err
}

func scanRequest(s rowScanner) (Request, error) {
	var r Request
	var usage, reqJSON, reqFiltJSON, respJSON, respFiltJSON, plugins, errJSON []byte
	var truncated, priced int
	var costInput, costOutput, costCacheRead, costCacheWrite, costTotal float64
	err := s.Scan(&r.ID, &r.SessionID, &r.Seq, &r.CreatedAt, &r.Alias, &r.Provider,
		&r.Model, &r.Endpoint, &r.DurationMS, &r.StatusCode, &r.FinishReason,
		&usage, &reqJSON, &reqFiltJSON, &respJSON, &respFiltJSON,
		&plugins, &errJSON, &truncated,
		&r.PromptTokens, &r.CompletionTokens, &r.CachedTokens,
		&costInput, &costOutput, &costCacheRead, &costCacheWrite, &costTotal, &priced)
	if err != nil {
		return r, err
	}
	r.Usage = usage
	r.RequestJSON = reqJSON
	r.RequestFilteredJSON = reqFiltJSON
	r.ResponseJSON = respJSON
	r.ResponseFilteredJSON = respFiltJSON
	r.Truncated = truncated != 0
	r.CostInput = costInput
	r.CostOutput = costOutput
	r.CostCacheRead = costCacheRead
	r.CostCacheWrite = costCacheWrite
	r.CostTotal = costTotal
	r.CostPriced = priced != 0
	if len(plugins) > 0 {
		_ = json.Unmarshal(plugins, &r.PluginsApplied)
	}
	if len(errJSON) > 0 {
		_ = json.Unmarshal(errJSON, &r.Error)
	}
	return r, nil
}

func scanToolCall(s rowScanner) (ToolCall, error) {
	var tc ToolCall
	var args, snippet, verdict, annot []byte
	var isErr sql.NullInt64
	err := s.Scan(&tc.ID, &tc.RequestID, &tc.SessionID, &tc.Seq, &tc.ToolName,
		&args, &isErr, &snippet, &verdict, &annot)
	if err != nil {
		return tc, err
	}
	tc.ArgumentsJSON = args
	tc.ResultIsError = isErr.Valid && isErr.Int64 != 0
	tc.ResultSnippet = string(snippet)
	tc.Verdict = string(verdict)
	tc.AnnotationJSON = annot
	return tc, nil
}

// ListSessions returns session summaries matching the filter, newest first.
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]*SessionSummary, error) {
	var conds []string
	var args []any
	if f.Alias != "" {
		conds = append(conds, `EXISTS (SELECT 1 FROM requests r WHERE r.session_id = sessions.id AND r.alias = ?)`)
		args = append(args, f.Alias)
	}
	if f.Model != "" {
		conds = append(conds, `EXISTS (SELECT 1 FROM requests r WHERE r.session_id = sessions.id AND r.model = ?)`)
		args = append(args, f.Model)
	}
	switch f.Status {
	case "ok":
		conds = append(conds, `failure_count = 0`)
	case "error":
		conds = append(conds, `failure_count > 0`)
	}
	if !f.Since.IsZero() {
		conds = append(conds, `created_at >= ?`)
		args = append(args, formatTS(f.Since))
	}
	if !f.Until.IsZero() {
		conds = append(conds, `created_at <= ?`)
		args = append(args, formatTS(f.Until))
	}
	if f.Search != "" {
		like := "%" + escapeLike(f.Search) + "%"
		conds = append(conds, `EXISTS (SELECT 1 FROM requests r WHERE r.session_id = sessions.id AND
			(r.request_json LIKE ? ESCAPE '\' OR r.request_filtered_json LIKE ? ESCAPE '\'
			 OR r.response_json LIKE ? ESCAPE '\' OR r.response_filtered_json LIKE ? ESCAPE '\'))`)
		args = append(args, like, like, like, like)
	}

	q := `SELECT id, created_at, first_alias, first_model, request_count, tool_call_count, failure_count FROM sessions`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SessionSummary
	for rows.Next() {
		sum, err := scanSessionSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &sum)
	}
	return out, rows.Err()
}

// GetSession loads a session with its requests (by seq) and tool calls.
func (s *Store) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, created_at, first_alias, first_model, request_count, tool_call_count, failure_count
		 FROM sessions WHERE id = ?`, sessionID)
	sum, err := scanSessionSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	reqs, err := loadRequests(ctx, s.db, `WHERE session_id = ? ORDER BY seq ASC, created_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	tcs, err := loadToolCalls(ctx, s.db, `WHERE session_id = ? ORDER BY seq ASC, id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	return &Session{SessionSummary: sum, Requests: reqs, ToolCalls: tcs}, nil
}

// GetRequest loads one request by (session_id, seq).
func (s *Store) GetRequest(ctx context.Context, sessionID string, seq int) (*Request, error) {
	row := s.db.QueryRowContext(ctx, requestSelect+` WHERE session_id = ? AND seq = ?`, sessionID, seq)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetToolCall loads one tool call by its row id ("<request_id>/<index>").
func (s *Store) GetToolCall(ctx context.Context, id string) (*ToolCall, error) {
	row := s.db.QueryRowContext(ctx, toolCallSelect+` WHERE id = ?`, id)
	tc, err := scanToolCall(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &tc, nil
}

// ListToolCalls returns tool calls matching the filter, in conversation order.
func (s *Store) ListToolCalls(ctx context.Context, f ToolCallFilter) ([]*ToolCall, error) {
	var conds []string
	var args []any
	if f.SessionID != "" {
		conds = append(conds, `session_id = ?`)
		args = append(args, f.SessionID)
	}
	if f.ToolName != "" {
		conds = append(conds, `tool_name = ?`)
		args = append(args, f.ToolName)
	}
	if f.Verdict != "" {
		conds = append(conds, `verdict = ?`)
		args = append(args, f.Verdict)
	}
	q := toolCallSelect
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY seq ASC, id ASC`
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ToolCall
	for rows.Next() {
		tc, err := scanToolCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &tc)
	}
	return out, rows.Err()
}

// SetVerdict caches an inspection verdict (+ optional annotation JSON) on a
// tool call (§5 on-demand pull model, written back by Phase 7). verdict must
// be SUCCESS, RECOVERABLE, or BLIND_ERROR.
func (s *Store) SetVerdict(ctx context.Context, toolCallID, verdict string, annotation json.RawMessage) error {
	switch verdict {
	case "SUCCESS", "RECOVERABLE", "BLIND_ERROR":
	default:
		return fmt.Errorf("store: SetVerdict: invalid verdict %q", verdict)
	}
	if len(annotation) == 0 {
		annotation = nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tool_calls SET verdict = ?, annotation_json = ? WHERE id = ?`,
		verdict, nullBytes(annotation), toolCallID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SearchConversations runs a LIKE text search over request and response
// payloads, returning the matching requests newest first with a snippet around
// the first match.
func (s *Store) SearchConversations(ctx context.Context, query string, limit int) ([]*SearchResult, error) {
	if query == "" {
		return nil, nil
	}
	like := "%" + escapeLike(query) + "%"
	q := `
		SELECT id, session_id, seq, created_at, alias, model,
		       request_json, request_filtered_json, response_json, response_filtered_json
		FROM requests
		WHERE request_json LIKE ? ESCAPE '\'
		   OR request_filtered_json LIKE ? ESCAPE '\'
		   OR response_json LIKE ? ESCAPE '\'
		   OR response_filtered_json LIKE ? ESCAPE '\'
		ORDER BY created_at DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.db.QueryContext(ctx, q, like, like, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SearchResult
	for rows.Next() {
		var res SearchResult
		var reqJSON, reqFiltJSON, respJSON, respFiltJSON []byte
		if err := rows.Scan(&res.RequestID, &res.SessionID, &res.Seq, &res.CreatedAt,
			&res.Alias, &res.Model, &reqJSON, &reqFiltJSON, &respJSON, &respFiltJSON); err != nil {
			return nil, err
		}
		res.Snippet = matchSnippet(query, reqJSON, reqFiltJSON, respJSON, respFiltJSON)
		out = append(out, &res)
	}
	return out, rows.Err()
}

// ListAliases returns the distinct aliases observed in captured requests, in
// order of first appearance. It backs the §7.1 gateway_status "configured
// aliases" for the standalone inspection MCP server, which has no
// gateway.toml; the API's status endpoint instead reports config-derived
// aliases (which include accounts with no captured traffic yet).
func (s *Store) ListAliases(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT alias FROM requests WHERE alias IS NOT NULL AND alias != ''
		 GROUP BY alias ORDER BY MIN(created_at)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Status reports store counts, failure totals, disk usage, and retention.
func (s *Store) Status(ctx context.Context) (*Status, error) {
	st := &Status{StorePath: s.path}
	st.RetentionDays = s.retention()

	var err error
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&st.SessionCount); err != nil {
		return nil, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&st.RequestCount); err != nil {
		return nil, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_calls`).Scan(&st.ToolCallCount); err != nil {
		return nil, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(failure_count), 0) FROM sessions`).Scan(&st.FailureCount); err != nil {
		return nil, err
	}
	st.DiskUsageBytes, err = diskUsage(s.path)
	if err != nil {
		return nil, err
	}
	return st, nil
}

func diskUsage(path string) (int64, error) {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		total += fi.Size()
	}
	return total, nil
}

// escapeLike escapes LIKE wildcards so user input matches literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// matchSnippet returns a text window around the first case-insensitive match
// of query in the candidate fields, in priority order.
func matchSnippet(query string, fields ...[]byte) string {
	needle := []byte(strings.ToLower(query))
	for _, f := range fields {
		if len(f) == 0 {
			continue
		}
		idx := bytes.Index(bytes.ToLower(f), needle)
		if idx < 0 {
			continue
		}
		start := idx - 80
		if start < 0 {
			start = 0
		}
		end := idx + len(query) + 80
		if end > len(f) {
			end = len(f)
		}
		return string(f[start:end])
	}
	return ""
}

func loadRequests(ctx context.Context, db *sql.DB, where string, args ...any) ([]*Request, error) {
	rows, err := db.QueryContext(ctx, requestSelect+` `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func loadToolCalls(ctx context.Context, db *sql.DB, where string, args ...any) ([]*ToolCall, error) {
	rows, err := db.QueryContext(ctx, toolCallSelect+` `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ToolCall
	for rows.Next() {
		tc, err := scanToolCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &tc)
	}
	return out, rows.Err()
}
