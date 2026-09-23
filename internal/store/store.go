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
	"strings"
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
  request_count INTEGER, tool_call_count INTEGER, failure_count INTEGER,
  expired INTEGER DEFAULT 0       -- 1 once retention removed the payloads
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
  upstream_request_json TEXT,     -- exact body sent to the provider
  response_json TEXT,             -- reassembled completion(s)
  response_filtered_json TEXT,    -- after response plugins (NULL if none)
  plugins_applied TEXT,           -- JSON array of plugin names, or NULL
  error_json TEXT,                -- NULL or {code, message}
  truncated INTEGER DEFAULT 0,
  prompt_tokens INTEGER DEFAULT 0,     -- token counts from usage_json
  completion_tokens INTEGER DEFAULT 0,
  cached_tokens INTEGER DEFAULT 0,
  cost_input REAL DEFAULT 0,           -- estimated cost split in USD
  cost_output REAL DEFAULT 0,
  cost_cache_read REAL DEFAULT 0,
  cost_cache_write REAL DEFAULT 0,
  cost_total REAL DEFAULT 0,
  cost_priced INTEGER DEFAULT 0,       -- 1 when the model was in the pricing table
  cost_schema TEXT DEFAULT ''          -- pricing snapshot id that priced the row
)`

	// idxRequestsUsage supports the cost/usage aggregation queries over the
	// created_at + provider + alias + model dimensions.
	idxRequestsUsage = `CREATE INDEX IF NOT EXISTS idx_requests_usage ON requests(created_at, provider, alias, model)`

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

// requestCostColumns are the columns added after the original §5 schema.
// They are applied by migrate to pre-existing databases via ALTER TABLE, so
// the schema above and an older on-disk DB converge on the same shape.
var requestCostColumns = []string{
	"prompt_tokens INTEGER DEFAULT 0",
	"completion_tokens INTEGER DEFAULT 0",
	"cached_tokens INTEGER DEFAULT 0",
	"cost_input REAL DEFAULT 0",
	"cost_output REAL DEFAULT 0",
	"cost_cache_read REAL DEFAULT 0",
	"cost_cache_write REAL DEFAULT 0",
	"cost_total REAL DEFAULT 0",
	"cost_priced INTEGER DEFAULT 0",
	"cost_schema TEXT DEFAULT ''",
}

// requestCaptureColumns are payload columns added after the original §5
// schema. They are applied by migrate so existing capture databases gain the
// exact provider-wire request without requiring a table rewrite.
var requestCaptureColumns = []string{
	"upstream_request_json TEXT",
}

// sessionColumns are the columns added after the original §5 sessions schema
// (see requestCostColumns for the migration convention).
var sessionColumns = []string{
	"expired INTEGER DEFAULT 0",
}

// journalSizeLimit bounds the WAL file: after any checkpoint that runs to the
// end of the WAL, SQLite truncates the file to this size. Without it the WAL
// grows unboundedly when long-lived reader connections (the standalone MCP
// inspectors) prevent checkpointing from keeping up with capture traffic, and
// every restart then pays a multi-gigabyte WAL recovery.
const journalSizeLimit = 256 << 20 // 256 MiB

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
	q.Add("_pragma", fmt.Sprintf("journal_size_limit(%d)", journalSizeLimit))
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
		idxToolCallsSession, idxToolCallsToolName, idxRequestsUsage,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: create schema: %w", err)
		}
	}
	return s.migrate(ctx)
}

// migrate brings pre-existing tables up to the current column set by adding
// any missing columns. ALTER TABLE ADD COLUMN is a schema-only change in
// SQLite (no table rewrite), so this is cheap even on a multi-GB store;
// existing rows keep NULL and the new columns default to 0 on read.
func (s *Store) migrate(ctx context.Context) error {
	for _, m := range []struct {
		table string
		cols  []string
	}{
		{"requests", requestCaptureColumns},
		{"requests", requestCostColumns},
		{"sessions", sessionColumns},
	} {
		if err := s.ensureColumns(ctx, m.table, m.cols); err != nil {
			return err
		}
	}
	return nil
}

// ensureColumns adds every def in cols (e.g. "name TYPE [DEFAULT v]") that the
// table does not already have.
func (s *Store) ensureColumns(ctx context.Context, table string, cols []string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("store: migrate: read table_info(%s): %w", table, err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("store: migrate: scan table_info(%s): %w", table, err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, col := range cols {
		name := col[:strings.IndexByte(col, ' ')]
		if have[name] {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+col); err != nil {
			return fmt.Errorf("store: migrate: add %s column %s: %w", table, name, err)
		}
	}
	return nil
}

// Checkpoint runs a TRUNCATE WAL checkpoint: all committed frames are written
// back into the database file and the WAL is truncated to zero bytes. main
// calls it once after the blocking startup work and before serving — the one
// moment the WAL is quiescent — so an unclean previous shutdown never leaves a
// multi-gigabyte WAL for the next startup to recover. Long-lived readers (the
// standalone MCP inspectors) can keep it from finishing; the busy timeout
// bounds the wait and the failure is logged, not fatal.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, walPages, checkpointed int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &walPages, &checkpointed); err != nil {
		return fmt.Errorf("store: wal_checkpoint: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("store: wal_checkpoint: blocked by another connection")
	}
	return nil
}

// NewID returns a UUIDv4 string. The gateway pre-generates the request id via
// NewID so retry log lines and the final capture share one id; Capture also
// falls back to NewID when a record id is left empty.
func NewID() string { return uuid.NewString() }

// NormalizeSessionID validates a client-supplied session id against the safe
// charset, returning it verbatim when it matches, and a fresh UUID otherwise
// (empty or invalid). The gateway normalizes before echoing the id in
// X-Gateway-Session-Id, and Capture applies the same rule as defense in depth,
// so a malicious id can never be persisted.
func NormalizeSessionID(id string) string {
	if sessionIDRe.MatchString(id) {
		return id
	}
	return NewID()
}

// formatTS formats t as the §-wide ISO-8601 UTC timestamp.
func formatTS(t time.Time) string { return t.UTC().Format(tsLayout) }
