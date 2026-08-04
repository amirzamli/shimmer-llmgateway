package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

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

// Purge deletes sessions (and their requests and tool calls) older than the
// configured retention window. It is the startup purge; the daily ticker runs
// StartRetentionLoop. Returns the number of sessions purged. Disabled when
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
	// classifies the newest recorded session as expired.
	var newest string
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM sessions`).Scan(&newest); err != nil {
		return 0, err
	}
	if newest == "" {
		return 0, nil // empty store: nothing to purge
	}
	newestTS, err := time.Parse(tsLayout, newest)
	if err != nil {
		return 0, fmt.Errorf("store: purge: parse newest created_at %q: %w", newest, err)
	}
	cutoff := formatTS(newestTS.AddDate(0, 0, -days))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// tool_calls has no cascade clause in the §5 schema, so children are
	// removed before their parents.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tool_calls WHERE session_id IN (SELECT id FROM sessions WHERE created_at < ?)`,
		cutoff); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM requests WHERE session_id IN (SELECT id FROM sessions WHERE created_at < ?)`,
		cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE created_at < ?`, cutoff)
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
	return int(n), nil
}

// StartRetentionLoop runs Purge on a ticker until ctx is done. interval <= 0
// defaults to daily. The gateway calls Purge once at startup and starts this
// loop for the recurring purge.
func (s *Store) StartRetentionLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.Purge(ctx); err != nil && !errors.Is(err, context.Canceled) {
					// No logger lives in the store; the gateway surfaces
					// startup purge errors and the loop retries next tick.
					continue
				}
			}
		}
	}()
}
