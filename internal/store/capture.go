package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Capture persists one completed request in a single transaction (resolved
// review decision #6): the session upsert with counters, the requests row with
// every §4.3 field, the tool calls extracted from the reassembled response,
// and result linking for role:"tool" messages in the request body.
//
// Capture sets rec.Seq to the per-session order and generates UUIDs for
// rec.ID / rec.SessionID when empty.
func (s *Store) Capture(ctx context.Context, rec *CaptureRecord) error {
	if rec == nil {
		return fmt.Errorf("store: capture: nil record")
	}
	// Defense in depth: a caller-supplied session id that is empty or outside
	// the safe charset is replaced with a fresh UUID (see NormalizeSessionID).
	rec.SessionID = NormalizeSessionID(rec.SessionID)
	if rec.ID == "" {
		rec.ID = NewID()
	}
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	ts := formatTS(createdAt)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: capture: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	toolCalls := extractToolCalls(rec.ResponseJSON)
	failed := rec.StatusCode >= 400 || rec.Error != nil
	if err := upsertSession(ctx, tx, rec.SessionID, ts, rec.Alias, rec.Model, len(toolCalls), failed); err != nil {
		return fmt.Errorf("store: capture: %w", err)
	}

	// Per-session order: next seq within this transaction.
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM requests WHERE session_id = ?`, rec.SessionID,
	).Scan(&rec.Seq); err != nil {
		return fmt.Errorf("store: capture: seq: %w", err)
	}

	if err := insertRequest(ctx, tx, rec, ts); err != nil {
		return fmt.Errorf("store: capture: %w", err)
	}

	for i, tc := range toolCalls {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tool_calls (id, request_id, session_id, seq, tool_name, arguments_json)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			toolCallID(rec.ID, i), rec.ID, rec.SessionID, rec.Seq, tc.ToolName, tc.Arguments,
		); err != nil {
			return fmt.Errorf("store: capture: insert tool_call: %w", err)
		}
	}

	if err := s.linkToolResults(ctx, tx, rec.SessionID, rec.RequestJSON); err != nil {
		return fmt.Errorf("store: capture: link tool results: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: capture: commit: %w", err)
	}
	return nil
}

// upsertSession inserts the session on first request (setting first_alias /
// first_model and starting counters at 1) or increments its counters.
func upsertSession(ctx context.Context, tx *sql.Tx, id, ts, alias, model string, toolCallCount int, failed bool) error {
	failureCount := 0
	if failed {
		failureCount = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, created_at, first_alias, first_model, request_count, tool_call_count, failure_count)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			request_count    = request_count + 1,
			tool_call_count  = tool_call_count + excluded.tool_call_count,
			failure_count    = failure_count + excluded.failure_count`,
		id, ts, alias, model, toolCallCount, failureCount,
	)
	return err
}

func insertRequest(ctx context.Context, tx *sql.Tx, rec *CaptureRecord, ts string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO requests (
			id, session_id, seq, created_at,
			alias, provider, model, endpoint,
			duration_ms, status_code, finish_reason,
			usage_json, request_json, request_filtered_json,
			response_json, response_filtered_json,
			plugins_applied, error_json, truncated,
			prompt_tokens, completion_tokens, cached_tokens,
			cost_input, cost_output, cost_cache_read, cost_cache_write, cost_total,
			cost_priced, cost_schema
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.SessionID, rec.Seq, ts,
		rec.Alias, rec.Provider, rec.Model, rec.Endpoint,
		rec.DurationMS, rec.StatusCode, rec.FinishReason,
		nullBytes(rec.Usage), nullBytes(rec.RequestJSON), nullBytes(rec.RequestFilteredJSON),
		nullBytes(rec.ResponseJSON), nullBytes(rec.ResponseFilteredJSON),
		nullStrings(rec.PluginsApplied), nullError(rec.Error),
		boolInt(rec.Truncated),
		rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens,
		rec.CostInput, rec.CostOutput, rec.CostCacheRead, rec.CostCacheWrite, rec.CostTotal,
		boolInt(rec.CostPriced), rec.CostSchema,
	)
	return err
}

// nullBytes returns a SQL NULL when b is empty, else the string form.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// nullStrings returns a SQL NULL when ss is empty, else the JSON array text.
func nullStrings(ss []string) any {
	if len(ss) == 0 {
		return nil
	}
	b, err := json.Marshal(ss)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// nullError returns a SQL NULL when e is nil, else the JSON {code, message}.
func nullError(e *ErrorInfo) any {
	if e == nil {
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil
	}
	return string(b)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
