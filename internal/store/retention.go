package store

import (
	"context"
	"errors"
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
func (s *Store) Purge(ctx context.Context) (int, error) {
	days := s.retention()
	if days <= 0 {
		return 0, nil
	}
	cutoff := formatTS(time.Now().UTC().AddDate(0, 0, -days))

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
