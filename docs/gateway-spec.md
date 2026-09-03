# Gateway Spec — Capture-Only LLM Gateway + Inspection UI/MCP

**Status**: Draft for review.  **Scope**: v1.  **Version**: 0.2

## 1. Purpose

A capture-only LLM gateway that sits between any agent UI / MCP client and an
LLM provider, plus inspection surfaces (an embedded HTML UI and an MCP server)
that expose the captured traffic for debugging and troubleshooting.

Four outcomes:

1. **Full-fidelity recording** — every request/response (including streamed
   responses) is persisted, so any agent conversation can be replayed and
   inspected after the fact.
2. **Tool-call diagnosis** — tool calls emitted by the model are validated
   against the schemas the client itself declared in the request, and failures
   are classified (SUCCESS / RECOVERABLE / BLIND_ERROR) so "why is the agent
   calling tools wrong" is answerable mechanically.
3. **Multi-account providers** — the same provider can be added multiple times
   under aliases (one company, several API accounts), with a dropdown-driven
   setup flow like common AI coding-agent harnesses.

## 2. Non-Goals (v1)

- **No** routing intelligence: no load balancing, model fallbacks, retries,
  budgets, or rate limiting (that is LiteLLM territory).
- **No** auth / multi-tenancy (localhost tooling; not a public service).
- **No** format translation. v1 speaks OpenAI-compatible only, request in →
  request out. Anthropic-native is a later extension.
- **No** external plugin loading. Plugins are built-in Go plugins selected by
  name in config; WASM / external binaries are v2.

## 3. Architecture

```
                          request filters        response filters
[agent UI / MCP client] ─►[ gateways plugins ]─► [plugins] ─► [LLM provider]
                              │  ▲
                              │  │  append-only (original + filtered payloads)
                              ▼  │
                          [SQLite store]
                 ▲              ▲
   [HTML UI (embedded,        [inspection
    same Go binary)]           MCP server]
                 ▲              ▲
              humans         any MCP client (opencode, Claude Desktop, Cursor)
```

| Component          | Choice                                              | Rationale                                  |
| :----------------- | :-------------------------------------------------- | :----------------------------------------- |
| Gateway            | Go, single static binary                            | easy deployment next to any customer stack |
| Store              | SQLite (embedded, zero-dep)                         | no external service, trivially inspectable |
| HTML UI            | embedded in the gateway (`embed.FS`, no build step) | one binary, one port, zero extra services  |
| Plugins            | built-in Go plugins by name, per-instance ordering  | seam designed now, external loading v2     |
| Inspection MCP     | hand-rolled Go MCP server (no framework)             | all-Go repo; no Python dependency         |

## 4. Gateway (Go)

### 4.1 HTTP surface

| Method | Path                    | Purpose                                            |
| :----- | :---------------------- | :------------------------------------------------- |
| GET    | `/healthz`              | `{"status":"ok"}` (repo convention)                |
| GET    | `/`                     | embedded HTML UI                                   |
| GET/POST/PATCH/DELETE | `/api/*`      | UI REST surface (§6)                               |
| GET    | `/v1/models`            | models across all configured instances             |
| POST   | `/v1/chat/completions`  | the only capture surface (stream + non-stream)     |

### 4.2 Provider templates and instances (aliases)

Two-level model — this is what makes multi-account work:

- **Template** = a provider definition: `name`, `base_url`, default
  `api_key_env`, `models`, optional docs. These are the entries in the
  dropdown. A set ships built-in (openai, anthropic, ollama, groq, vllm,
  lite_llm, openrouter, deepseek, gemini, mistral, and more); users can add
  custom ones via the UI.
- **Instance** = a concrete account of a template: `alias`, `template`,
  `api_key_env`, optional model subset, optional `model_aliases` map. You may
  add the same template many times.

**Alias rules:**

- First instance of a template defaults to the template name: `openai`.
- Each further instance auto-defaults to the next free name: `openai-2`,
  `openai-3`, … (skip already-taken aliases).
- The user may override with any unique alias matching `[a-z0-9._-]+`.
- Aliases are the routing key: model prefix = alias, e.g. `openai-2/gpt-4o`
  → instance `openai-2`, model `gpt-4o`. Unprefixed model names resolve to
  the first instance that lists the model.
- Unknown alias → 400 `INVALID_ARGUMENT` with the list of available aliases
  (a RECOVERABLE error, per the repo's error convention).

**Model aliases** — each instance may map a friendly name to a concrete model
string (e.g. `small` → `gpt-4o-mini`). Keys must match `[a-z0-9._-]+`; values
are non-empty and may contain slashes (e.g.
`meta-llama/Meta-Llama-3-8B-Instruct`). Expansion is single-level: the mapped
value is forwarded verbatim, never re-expanded. Precedence for an **unprefixed**
name: (1) the `default_alias` instance's map; (2) the first enabled instance in
config order whose map contains the name; (3) the literal fallback below. An
alias mapping **shadows** literal model membership. A **prefixed** `alias/x`
expands only when `x` is a key of that instance's map; otherwise `x` is
forwarded verbatim (the existing no-membership-check behavior). Disabled
instances contribute no aliases and are skipped in both expansion and
`/v1/models`. `GET /v1/models` advertises each alias as the bare key and
`alias/key`, deduped with the real model list (an alias key colliding with a
literal model is listed once).

**Config (`gateway.toml`):** the repo ships `gateway.toml.example` — copy it
to `gateway.toml` and edit (see the Quick start in the README):

```toml
listen_addrs = ["127.0.0.1:8787", "100.64.0.1:8787"]  # one socket per address
store  = "gateway.db"
retention_days = 30

[settings]
default_alias = "openai"         # used when the client sends no prefix
request_plugins = []             # global defaults, overridable per instance
response_plugins = []

[providers.openai]               # TEMPLATE — the dropdown entry
base_url = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"   # default for the first instance
models = ["gpt-4o", "gpt-4o-mini"]

[providers.ollama]               # template with no key
base_url = "http://localhost:11434/v1"
api_key_env = ""
models = ["llama3.1"]

[[instances]]                    # INSTANCE 1 of openai — alias defaults to "openai"
alias = "openai"
template = "openai"
api_key_env = "OPENAI_API_KEY"
model_aliases = { small = "gpt-4o-mini", medium = "gpt-4o" }  # alias → model

[[instances]]                    # INSTANCE 2 — the second account
alias = "openai-2"
template = "openai"
api_key_env = "OPENAI_API_KEY_2"
plugins = ["redact"]             # per-instance override of the global chain
```

Keys NEVER live in this file; per-instance keys come from `api_key_env`
(env var) or the UI-managed secrets file (§6).

### 4.3 Capture semantics

Per request:

1. **Session correlation.** Client may send `X-Session-Id`; the gateway
   echoes the effective session id in an `X-Gateway-Session-Id` response
   header so any client can correlate without knowing it in advance.
2. **Record** the full request body — messages, tool definitions/schemas,
   sampling params. The gateway captures the body only; it never stores
   client-supplied headers. The resolved provider key is injected into the
   outbound request itself and never touches the store. Content redaction is a
   plugin concern (§4.5), not core.
3. **Plugins** run before forward (request filters) and on the way back
   (response filters). The store keeps BOTH payloads: what the client sent
   (original) and what the provider actually saw / what the client actually
   received (filtered), plus `plugins_applied`.
4. **Streaming:** reassemble the SSE stream into a single completion (append
   deltas; concatenate chunked `tool_calls[].function.arguments` by index),
   record start + end, duration, `finish_reason`, chunk count. Record `usage`
   when the provider sends it; `NULL` otherwise.
5. **Non-streaming:** same row shape, no reassembly.
6. **Errors:** record non-2xx provider responses with status + body snippet.
7. One SQLite transaction per request.

**Record fields** (`requests` row): id, session_id, seq, created_at, alias,
provider (template), model, endpoint, duration_ms, status_code, finish_reason,
usage_json, request_json (original), request_filtered_json,
response_json (reassembled), response_filtered_json, plugins_applied,
error_json, truncated.

### 4.4 Streaming correctness (the critical part)

- Forward SSE line-by-line, flush after each chunk; **never** buffer the whole
  stream before forwarding (latency is non-negotiable for chat UX).
- Handle `data: [DONE]`, keepalive comments, and error events.
- On client disconnect: cancel upstream; persist what was reassembled,
  marked `truncated: true`.
- Tool-call delta reassembly must be index-stable (chunks for tool_call[0] and
  tool_call[1] interleave; concatenate per index).
- Plugin interplay: response plugins run **after** reassembly (buffer mode —
  the v1 default, simple and safe). Per-chunk filtering is a later mode (see
  open decisions).

### 4.5 Plugins

The seam is designed now; only built-ins ship in v1.

```go
type Plugin interface {
    Name() string
    FilterRequest(ctx context.Context, req *Request) error   // input side
    FilterResponse(ctx context.Context, resp *Response) error // output side, post-reassembly
}
```

- **Ordering**: `request_plugins` run in config order before forward;
  `response_plugins` run in config order on the reassembled response.
- **Scope**: per-instance `plugins` list overrides the global defaults
  (empty list on an instance = no plugins for that account; absent = inherit
  the global settings). The dashboard exposes this per provider: an
  "inherit global defaults" switch plus a checkbox per plugin, persisted via
  `PATCH /api/instances/{alias}` (`plugins` / `plugins_inherit`).
- **Built-in v1**: `redact` — regex/field-list redaction of messages and tool
  arguments, shipped as the reference implementation; `sanitize_tools` —
  request-side tool-schema repair that drops the redundant `oneOf`/`anyOf`
  combinator from schema nodes that also declare an `enum` (the duplication is
  valid JSON Schema but some OpenAI-compatible providers answer it with a
  silent empty completion). Examples of future plugins (v2): RAG context
  injection (input side), output truncation / cost reduction (output side),
  guardrails.
- **Capture interplay**: plugins never see the stored original payload; the
  store always has both sides so debugging shows exactly what the plugin
  changed.
- **External loading** (WASM / `go-plugin` binaries) is v2, explicitly out of
  scope — the interface above is designed so that swap is mechanical.

### 4.6 Logging

One-line JSON to stderr (see `internal/logging/logging.go`):
`{timestamp, level, component, event, fields}` — component `gateway`.

## 5. Store schema (SQLite)

```sql
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,            -- session_id
  created_at TEXT,                -- ISO-8601 UTC
  first_alias TEXT, first_model TEXT,
  request_count INTEGER, tool_call_count INTEGER, failure_count INTEGER
);

CREATE TABLE requests (
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
);

CREATE TABLE tool_calls (
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
);
```

Indexes: `sessions(created_at)`, `requests(session_id, seq)`,
`tool_calls(session_id)`, `tool_calls(tool_name)`.

**Tool-call derivation (capture-time, lightweight).** The gateway never sees
the tool execution — it sees the model emit `tool_calls` in a response, then
the client return a `role:"tool"` result message in a later request of the
same session. Capture extracts tool calls from responses, and links each to
its result (match by `tool_call_id`) when it appears. Schema validation and
verdict classification are **on-demand** in the inspection server (pull
model), cached back into `tool_calls.verdict`.

## 6. HTML UI (v1, embedded in the gateway)

One static page (`embed.FS`, no JS framework, no build step) driven by a
small REST API. **Read-only for traces; writes only for config.** A
`web/index.html` in the gateway's working directory shadows the embedded copy
(served fresh per request, so UI edits need no rebuild or restart); installs
without the file serve the embedded asset.

### 6.1 Views

| View        | Contents                                                                 |
| :---------- | :----------------------------------------------------------------------- |
| **Providers** | Dropdown of templates (built-in + user-added); "Add instance" flow: pick a template → alias auto-filled per §4.2 rules (`openai`, `openai-2`, …) → editable → optional model subset + key → save. Instance list with edit / disable / delete. |
| **Settings** | listen addrs, store path (read-only after start), retention, default alias, global plugin chain. |
| **Traces**  | Session list with filters (alias, model, status, date range) + text search; conversation view rendering messages, tool calls, results, and verdict badges (SUCCESS / RECOVERABLE / BLIND_ERROR); per-session JSONL export; per-request raw view (original vs filtered). |
| **Status**  | Store stats, disk usage, uptime, plugin list.                            |

### 6.2 REST API

`GET /api/templates`, `POST /api/templates` · `GET /api/instances`,
`POST /api/instances`, `PATCH /api/instances/{alias}` (rename/disable/
`model_aliases`), `DELETE /api/instances/{alias}`,
`GET /api/instances/{alias}/models` (provider model fetch) · `GET /api/quota`
(per-instance quota/balance) · `GET /api/settings`, `PATCH /api/settings` ·
`GET /api/secrets/master-key`, `POST /api/secrets/master-key/ack`
(one-time generated master-key exposure + acknowledge; localhost-only) ·
`GET /api/sessions`, `GET /api/sessions/{id}`,
`GET /api/sessions/{id}/export`, `GET /api/status`.

- Config mutations reload atomically — instance changes apply to the next
  request, no restart. (Plugin changes and new templates also hot-reload in
  v1; template schema changes are rare enough to allow restart.)
- **Keys**: stored in `<store>.secrets.json` with `chmod 0600` (UI can accept
  a key and persist it), or left to env vars per instance (`api_key_env`).
  The UI never displays a stored key in full — masked, replaceable.
- **Provider model fetch** — `GET /api/instances/{alias}/models` returns the
  provider's model list, fetched from `GET <base_url>/models` (OpenAI list
  shape) with the instance's resolved key (env var first, then the secrets
  file), or with no Authorization header for keyless providers (ollama, vllm):
  ```json
  {
    "alias": "openai",
    "models": ["gpt-4o", "gpt-4o-mini"],
    "source": "provider",
    "fetched_at": "2026-08-04T12:00:00Z",
    "error": ""
  }
  ```
  `source` is `"provider"` on a live fetch and `"config"` when the fetch was
  skipped or failed and the configured models were returned; `error` is the
  generic `"provider models unavailable"` string, present only on an actual
  failure (base_url/network detail goes to the gateway log only — the `/api/`
  surface is unauthenticated). This includes the built-in `anthropic`
  template, whose `/models` fetch can fail on its non-native path (accepted
  limitation). Results are cached **in-memory for 5 minutes** and never
  persisted; `?refresh=1` bypasses the cache, and a successful PATCH or DELETE
  on the instance invalidates it. Disabled instances are served (the UI needs
  the list to re-enable them). Unknown alias → 404 `NOT_FOUND`.
- **Instance PATCH `model_aliases`** — `PATCH /api/instances/{alias}` accepts
  an optional `model_aliases` object that **replaces the whole map** (same
  semantics as the other patch fields; absent or `null` leaves it unchanged,
  an empty object `{}` clears it). Keys must match `[a-z0-9._-]+` and values
  must be non-empty — a violation is a 400 `INVALID_ARGUMENT` (the same
  `config.Validate` used for `gateway.toml`).

### 6.3 Relationship to the MCP server

Same store, different consumers: the UI is for humans scanning sessions; the
MCP server is for agents and scripted analysis. Both stay in sync because
neither owns data — SQLite does.

## 7. Inspection MCP server

Transport: hand-rolled Go MCP server (no framework — a minimal JSON-RPC
surface: `initialize`, `ping`, `notifications/initialized`, `tools/list`,
`tools/call`) over stdio (default) and optional streamable-http. Config:
`--db <path>`, optional `--session` scoping. All tools:
`structured_output=False`, return JSON text strings, never throw
(`CallToolResult(isError=True, ...)`); errors as `{errorCode, message}`.

### 7.1 Generic tools (v1)

| Tool | Signature | Returns |
| :--- | :-------- | :------ |
| `list_sessions` | `limit?, alias?, model?, status?, since?, until?` | session summaries + failure counts |
| `get_conversation` | `session_id, include_full?` | ordered replay: user/assistant/tool messages, tool calls interleaved with results |
| `get_request` | `session_id, seq` | raw request/response JSON (both filtered and original) |
| `list_tool_calls` | `session_id?, tool_name?, verdict?, limit?` | tool-call records |
| `validate_tool_call` | `session_id, seq` (or tool-call id) | diffs emitted args vs the schemas **declared in the originating request**: `{ok, unknown_params[], missing_required[], type_mismatches[], nearest_params[]}` (Levenshtein-based nearest-param suggestions, `internal/mcp/analysis.go`) |
| `classify_failure` | `session_id, seq` | SUCCESS / RECOVERABLE / BLIND_ERROR + reason keywords (internal taxonomy, documented in `internal/mcp/analysis.go`) |
| `export_session` | `session_id` | JSONL — messages-only replay, the eval-fixture format |
| `gateway_status` | — | store path, counts, disk usage, retention, configured aliases |
| `search_conversations` | `query, limit?` | LIKE-based text search over messages (FTS5 later) |

The key design point: `validate_tool_call` needs **no external schema
knowledge** — the schemas the client declared travel inside the request body
itself.
## 8. Canonical JSONL export format

Every record serializes to one JSON line: `{type, ts, session_id, request_id,
alias, plugins, ...}` with `type ∈ {session_start, request, tool_call,
tool_result, error}`. `export_session`, the UI export button, and the
gateway's append log share this format, so the fuzzer, annotator, and any
future tooling all consume one shape.

## 9. Verification plan

1. **Unit**: SSE tool-call reassembly (interleaved multi-tool chunks);
   alias rules (auto-naming, uniqueness, routing, unknown-alias error);
   config parsing; plugin chain ordering; SQLite writes.
2. **Integration**: run against a fake OpenAI-compatible server (or Ollama if
   present); assert SSE-event-level parity between provider-out and
   client-in, capture completeness, `truncated` flag, and original-vs-filtered
   storage with the `redact` plugin active.
3. **Multi-account scenario**: two instances of one template with different
   keys; route `alias/gpt-4o` and `alias-2/gpt-4o` to different accounts;
   verify keys are never mixed.
4. **UI smoke**: template + instance CRUD via the API, alias auto-naming,
   key masking, session export.
5. **MCP smoke**: scripted JSON-RPC drive of every inspection tool over stdio.
6. **Dogfood**: point opencode / LibreChat at the gateway, run a real agent
   task, then replay + validate it through the UI and the inspection server.

