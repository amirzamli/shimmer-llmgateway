// Package store implements the §5 SQLite capture store: the sessions,
// requests, and tool_calls tables (exact schema plus the four indexes), the
// one-transaction-per-request capture API, capture-time tool-call extraction
// and result linking, the query surface consumed by the UI and MCP servers,
// the §8 canonical JSONL export, and retention purge.
//
// Conventions shared with the rest of the gateway:
//
//   - timestamps are ISO-8601 UTC with millisecond precision
//     ("2006-01-02T15:04:05.000Z") so TEXT ordering is chronological;
//   - ids are UUIDv4 (github.com/google/uuid, already in the module graph);
//   - nullable JSON columns are stored as TEXT and are NULL when absent;
//   - tool_calls.id is "<request_id>/<index>" per §5 ("request id + tool_call
//     index").
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by query methods when the requested row does not
// exist.
var ErrNotFound = errors.New("store: not found")

// ResultSnippetLen is the maximum length of tool_calls.result_snippet.
const ResultSnippetLen = 512

// sessionIDRe matches the safe session-id charset — letters, digits, dots,
// dashes, underscores, colons — capped at 128 characters. Common agent
// session ids (UUIDs, "sess-1", "conv_abc", "my.session:1") pass through;
// anything else is rejected so no client-controlled string can reach the UI
// as an unvalidated id.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// tsLayout is the ISO-8601 UTC timestamp format used by sessions.created_at
// and requests.created_at. Fixed milliseconds keep TEXT comparisons
// chronological.
const tsLayout = "2006-01-02T15:04:05.000Z"

// The §5 schema, column definitions verbatim.
const (
	schemaSessions = `CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,            -- session_id
  created_at TEXT,                -- ISO-8601 UTC
  first_alias TEXT, first_model TEXT,
  request_count INTEGER, tool_call_count INTEGER, failure_count INTEGER
)`

	schemaRequests = `CREATE TABLE IF NOT EXISTS requests (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  seq INTEGER,                    -- per-session order
  created_at TEXT,
  alias TEXT, provider TEXT, model TEXT, endpoint TEXT,
  duration_ms INTEGER,
  status_code INTEGER,
  finish_reason TEXT,
  usage_json TEXT,                -- {prompt_tokens,...} or NULL
  request_json TEXT,              -- original body (messages + tool schemas)
  request_filtered_json TEXT,     -- body after request plugins (NULL if none)
  response_json TEXT,             -- reassembled completion(s)
  response_filtered_json TEXT,    -- after response plugins (NULL if none)
  plugins_applied TEXT,           -- JSON array of plugin names, or NULL
  error_json TEXT,                -- NULL or {code, message}
  truncated INTEGER DEFAULT 0
)`

	schemaToolCalls = `CREATE TABLE IF NOT EXISTS tool_calls (
  id TEXT PRIMARY KEY,            -- request id + tool_call index
  request_id TEXT NOT NULL REFERENCES requests(id),
  session_id TEXT NOT NULL,
  seq INTEGER,                    -- order within conversation
  tool_name TEXT,
  arguments_json TEXT,            -- parsed JSON args
  result_is_error INTEGER,        -- 1 if the tool result was an error
  result_snippet TEXT,            -- first N chars of the result
  verdict TEXT,                   -- NULL | SUCCESS | RECOVERABLE | BLIND_ERROR
  annotation_json TEXT            -- NULL | {why, nearest_tool, missing_params, ...}
)`

	idxSessionsCreatedAt  = `CREATE INDEX IF NOT EXISTS idx_sessions_created_at ON sessions(created_at)`
	idxRequestsSessionSeq = `CREATE INDEX IF NOT EXISTS idx_requests_session_seq ON requests(session_id, seq)`
	idxToolCallsSession   = `CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id)`
	idxToolCallsToolName  = `CREATE INDEX IF NOT EXISTS idx_tool_calls_tool_name ON tool_calls(tool_name)`
)

// Store is the SQLite-backed capture store. It is safe for concurrent use.
type Store struct {
	db   *sql.DB
	path string

	mu            sync.RWMutex
	retentionDays int
	// jsonlPurger, when set, is called with the purge cutoff (same string
	// used for the SQL deletes) after a successful Purge so the §8 JSONL
	// append log is trimmed in lockstep with the SQLite rows. It is nil for
	// standalone store users (the MCP inspector), which have no append log.
	jsonlPurger JSONLPurger
}

// Open opens (creating if needed) the SQLite store at path with the §5 schema
// and WAL + foreign_keys pragmas. The database is pure-Go via
// modernc.org/sqlite, so the gateway builds with CGO_ENABLED=0.
func Open(path string) (*Store, error) {
	// modernc.org/sqlite's file: URI rejects a bare relative path
	// ("invalid uri authority"); absolutize so the spec's relative
	// `store = "gateway.db"` works from any working directory.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	// modernc.org/sqlite would create the file via the DSN with umask-derived
	// perms (0644 by default); pre-create it at 0600 so captured payloads in
	// the DB are never world-readable. The mode only applies on creation — an
	// existing file's perms are left untouched (the MCP inspector opens the
	// same DB with the exact same code path).
	f, err := os.OpenFile(absPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create %s: %w", path, err)
	}
	f.Close()
	u := &url.URL{Scheme: "file", Path: absPath}
	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db, path: absPath}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := s.createSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the store file path (used by gateway_status).
func (s *Store) Path() string { return s.path }

func (s *Store) createSchema(ctx context.Context) error {
	for _, stmt := range []string{
		schemaSessions, schemaRequests, schemaToolCalls,
		idxSessionsCreatedAt, idxRequestsSessionSeq,
		idxToolCallsSession, idxToolCallsToolName,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: create schema: %w", err)
		}
	}
	return nil
}

// newID returns a UUIDv4 string.
func newID() string { return uuid.NewString() }

// NormalizeSessionID validates a client-supplied session id against the safe
// charset, returning it verbatim when it matches, and a fresh UUID otherwise
// (empty or invalid). The gateway normalizes before echoing the id in
// X-Gateway-Session-Id, and Capture applies the same rule as defense in depth,
// so a malicious id can never be persisted.
func NormalizeSessionID(id string) string {
	if sessionIDRe.MatchString(id) {
		return id
	}
	return newID()
}

// formatTS formats t as the §-wide ISO-8601 UTC timestamp.
func formatTS(t time.Time) string { return t.UTC().Format(tsLayout) }
