package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// JSONLPurger trims the §8 JSONL append log after a retention purge. cutoff is
// the already-formatted ISO-8601 UTC ms cutoff the SQL deletes used, so the
// two stores converge on the same boundary. The callback is registered by the
// gateway (which owns the append log); standalone store users never set it.
type JSONLPurger func(cutoff string) error

// SetJSONLPurger registers the append-log trimmer invoked after each
// successful Purge.
func (s *Store) SetJSONLPurger(p JSONLPurger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jsonlPurger = p
}

func (s *Store) purger() JSONLPurger {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jsonlPurger
}

// SetRetention sets the retention window in days used by Purge. Values <= 0
// disable purging.
func (s *Store) SetRetention(days int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retentionDays = days
}

func (s *Store) retention() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retentionDays
}

// Purge expires the conversation payloads of sessions older than the
// configured retention window while keeping the usage-statistics rows: the
// requests rows stay (timestamps, model, status, token counts, costs —
// everything the usage aggregates sum), but the payload columns are nulled and
// the tool_calls rows are deleted. The sessions rows remain, flagged
// expired = 1 so the UI/MCP can show "payloads expired" instead of an empty
// conversation. StartRetentionLoop invokes it once at loop start and then on
// its ticker. Returns the number of sessions newly expired. Disabled when
// retention is <= 0.
//
// The cutoff is anchored to the newest session in the store rather than the
// wall clock: retention counts backward from the most recent recorded
// activity, so the most recent session (and anything within the window of it)
// can never fall on the expired side of a time.Now()-based cutoff.
func (s *Store) Purge(ctx context.Context) (int, error) {
	days := s.retention()
	if days <= 0 {
		return 0, nil
	}

	// Anchor to MAX(created_at) instead of time.Now() so the purge never
	// classifies the newest recorded session as expired. NULL (empty store)
	// scans as invalid: nothing to purge.
	var newest sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM sessions`).Scan(&newest); err != nil {
		return 0, err
	}
	if !newest.Valid || newest.String == "" {
		return 0, nil // empty store: nothing to purge
	}
	newestTS, err := time.Parse(tsLayout, newest.String)
	if err != nil {
		return 0, fmt.Errorf("store: purge: parse newest created_at %q: %w", newest.String, err)
	}
	cutoff := formatTS(newestTS.AddDate(0, 0, -days))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// The expired = 0 guard keeps the daily pass a no-op: sessions already
	// expired match nothing, so their rows are not rewritten every tick.
	// tool_calls are debug payloads with no usage value; they are dropped.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tool_calls WHERE session_id IN (SELECT id FROM sessions WHERE created_at < ? AND expired = 0)`,
		cutoff); err != nil {
		return 0, err
	}
	// Requests keep every metadata column the usage aggregates read; only the
	// payload columns (and the error message body) are dropped.
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET
		request_json = NULL, request_filtered_json = NULL,
		response_json = NULL, response_filtered_json = NULL,
		plugins_applied = NULL, error_json = NULL
		WHERE session_id IN (SELECT id FROM sessions WHERE created_at < ? AND expired = 0)`,
		cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET expired = 1 WHERE created_at < ? AND expired = 0`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// Trim the §8 JSONL append log with the same cutoff so log data does not
	// outlive its SQLite rows. A trim failure is surfaceable (the startup
	// purge logs it and the daily loop retries; the SQL purge already
	// committed, so the next tick's SQL pass is a no-op and only the log
	// rewrite runs again).
	if p := s.purger(); p != nil {
		if err := p(cutoff); err != nil {
			return int(n), fmt.Errorf("store: purge jsonl: %w", err)
		}
	}
	return int(n), nil
}

// StartRetentionLoop purges in the background until ctx is done: one pass
// immediately when the loop starts (catching up on anything that expired while
// the gateway was down, without blocking startup or serving), then a pass per
// tick. interval <= 0 defaults to daily. onPurge, when non-nil, receives every
// completed pass's result (context-canceled passes are dropped — the gateway
// is shutting down); the loop owns no logger, so the gateway logs through it.
func (s *Store) StartRetentionLoop(ctx context.Context, interval time.Duration, onPurge func(n int, err error)) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		pass := func() {
			n, err := s.Purge(ctx)
			if errors.Is(err, context.Canceled) {
				return
			}
			if onPurge != nil {
				onPurge(n, err)
			}
		}
		pass()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pass()
			}
		}
	}()
}
