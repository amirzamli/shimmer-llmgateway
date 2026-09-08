# Gateway Spec — Capture-Only LLM Gateway + Inspection UI/MCP

**Status**: Living — tracks the implementation.  **Scope**: v1.  **Version**: 0.3

## 1. Purpose

A capture-only LLM gateway that sits between any agent UI / MCP client and an
LLM provider, plus inspection surfaces (an embedded HTML UI and an MCP server)
that expose the captured traffic for debugging and troubleshooting. Clients
speak OpenAI-compatible chat (`/v1/chat/completions`) or the OpenAI Responses
API (`/v1/responses` — e.g. Codex CLI); upstreams may be OpenAI-compatible or
Anthropic-Messages-style (`style = "anthropic"`, §4.2).

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
- **Translation is limited to two fixed bridges**: OpenAI chat ↔ Anthropic
  Messages for anthropic-style templates, and the stateless Responses-API ↔
  chat shim (§4.1). Everything else is request in → request out; there is no
  free-form format translation.
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
| GET    | `/`                     | embedded HTML UI (a `web/index.html` in the working directory shadows the embedded copy) |
| GET/POST/PATCH/DELETE | `/api/*`      | UI REST surface (§6); host/origin-guarded          |
| GET    | `/v1/models`            | models across all configured instances             |
| POST   | `/v1/chat/completions`  | chat capture surface (stream + non-stream)         |
| POST   | `/v1/responses`         | stateless OpenAI Responses shim over the same pipeline (stream + non-stream) |
| GET    | `/v1/responses/{id}`    | always 404 — the shim is stateless (captures live in the store, not Responses state) |
| any other `/v1/*` | —            | 404                                               |

`POST /v1/responses` is a **stateless** translation shim that lets Responses
clients (Codex CLI) use the shared pipeline: the request is validated against
the stateless contract, translated to a Chat Completions body, and dispatched
into the existing routing / plugin / capture branches; 2xx replies are
converted back to Responses objects for the client. Rules: `previous_response_id`
→ 400 (no conversation state server-side); `store` / `include` / `metadata` /
`reasoning.summary` are accepted and ignored; input `reasoning` items are
dropped; only `function` tools are forwarded (other tool types are skipped with
a warn log — Codex sends newer tool groupings); reasoning is surfaced from
upstream deltas on output only. Streaming emits the typed Responses SSE
sequence (`response.created`, per-item `response.output_item.added/delta/done`,
`response.completed` with usage; upstream thinking deltas surface as a
`reasoning` output item); the chat `[DONE]` sentinel is swallowed. Capture
records `endpoint = "/v1/responses"` with the as-received Responses request
bytes and the chat-normalized response.

### 4.2 Provider templates and instances (aliases)

Two-level model — this is what makes multi-account work:

- **Template** = a provider definition: `name`, `base_url`, default
  `api_key_env`, `models`, optional `style`, docs, and an optional
  `session_header` — the header name an upstream requires to carry a stable
  per-conversation session id on every chat call (OpenCode Go declares
  `x-opencode-session`; without it the upstream rejects with HTTP 400
  `MissingSessionID`). When
  set, the gateway synthesises the request's effective session id (see
  §4.3) in that header on every upstream call; the model fetch and quota
  probes are unaffected. A template may also declare an
  `identity_headers` map — request headers the upstream expects every call to
  carry so it can identify the calling client (the built-in `opencode_go`
  declares `X-Opencode-Client: cli` and `X-Opencode-Project: global`,
  mirroring the real opencode client). For each declared header the gateway
  sends the declared value upstream on every request to that template; when
  the inbound client request carries the same header name (case-insensitive),
  the client's value wins. These are the entries in the
  dropdown. A set ships built-in (openai, anthropic, ollama, groq, vllm,
  lite_llm, openrouter, deepseek, gemini, mistral, kimi, zai, opencode_zen,
  opencode_go, commandcode, and more); users can add
  custom ones via the UI.

  **Outbound identification.** Every outbound provider request — chat, stream,
  Responses, the `/models` model fetch, and the quota probes — carries an
  explicit `User-Agent` header. On the forward path the inbound client's
  `User-Agent` is forwarded verbatim when present; otherwise the request uses
  the gateway's constant `shimmer-gateway/1.0`. The model fetch and quota
  probes have no inbound client and always use the constant. The gateway never
  leaves with Go's generic `Go-http-client/1.1` default, so upstream accounts
  are identified as coding-agent traffic, not an HTTP library.

  `style` selects the upstream protocol: `""`/`"openai"` (default) forwards
  OpenAI-shaped bodies to `<base_url>/chat/completions`; `"anthropic"`
  forwards to `<base_url>/messages` (x-api-key auth) and translates OpenAI
  chat ↔ Anthropic Messages in both directions and for both streaming and
  non-streaming (system hoisted to the top-level field, tool definitions /
  `tool_use`/`tool_result` mapping, `reasoning_effort` → extended thinking,
  image parts → image blocks, `max_tokens` defaulted); `"responses"` forwards
  to `<base_url>/responses` (Bearer auth like the default style) and
  translates OpenAI chat ↔ Responses in both directions and for both
  streaming and non-streaming (the first system message hoisted to
  `instructions`, tool calls become `function_call` / `function_call_output`
  input items, reasoning surfaces as `reasoning_content`, `max_tokens` /
  `max_completion_tokens` → `max_output_tokens`, and the effective session id
  is forwarded as `prompt_cache_key`). Anthropic and Responses SSE events are
  translated into `chat.completion.chunk` lines **before** reassembly, so the
  assembler, plugins, and capture only ever see OpenAI-shaped data.

  `model_reasoning_options` advertises the valid reasoning-effort levels per
  model (data only: it gates instance `model_reasoning` values and drives
  the UI dropdown; models without metadata accept the generic
  minimal/low/medium/high/xhigh/max vocabulary). A template-level `plugins`
  list is a **write-back materialization seed** only: when an instance of
  the template has no `plugins` of its own, write-back records a copy as the
  instance's `plugins` line so the file states the default explicitly. It is
  never consulted at runtime — an instance without a `plugins` line
  resolves from the global settings (empty by default = no plugins). No
  built-in template seeds a default; custom templates may.
- **Instance** = a concrete account of a template: `alias`, `template`,
  `api_key_env` (defaults to the template's when empty), optional model
  subset, and optional `model_aliases`, `model_reasoning`, `plugins`,
  `priority`, and `disabled` fields. You may
  add the same template many times. `plugins` overrides the global settings
  chain for that account (absent = inherit; explicit `[]` = durable off).
  `model_reasoning` maps a model key to a reasoning-effort level
  (`small` → `high`); the gateway rewrites `reasoning_effort` on the
  forwarded body (extended thinking on anthropic-style upstreams).
  `priority` orders unprefixed resolution; `disabled` excludes the instance
  from routing while keeping it visible for re-enabling.

**Alias rules:**

- First instance of a template defaults to the template name: `openai`.
- Each further instance auto-defaults to the next free name: `openai-2`,
  `openai-3`, … (skip already-taken aliases).
- The user may override with any unique alias matching `[A-Za-z0-9_.-]+`,
  optionally with interior single spaces (e.g. `My Provider`); `/` is excluded
  (it separates `alias/model`) and leading/trailing/double spaces are
  rejected. Matching is exact and case-sensitive.
- Aliases are the routing key: model prefix = alias, e.g. `openai-2/gpt-4o`
  → instance `openai-2`, model `gpt-4o`. Unprefixed model names resolve to
  the first enabled instance that maps or lists the model — see the
  precedence rules under Model aliases below.
- Unknown alias or unknown model → 400 `INVALID_ARGUMENT`; the unknown-alias
  error carries the list of routable (enabled) aliases and is a RECOVERABLE
  error, per the repo's error convention.

**Model aliases** — each instance may map a friendly name to a concrete model
string (e.g. `small` → `gpt-4o-mini`). Keys must match the alias pattern
(`[A-Za-z0-9_.-]+`, interior single spaces allowed); values
are non-empty and may contain slashes (e.g.
`meta-llama/Meta-Llama-3-8B-Instruct`). Expansion is single-level: the mapped
value is forwarded verbatim, never re-expanded. Precedence for an **unprefixed**
name: (1) the `default_alias` instance (its map, then its literal model
list); (2) the first enabled instance in **routing order** — explicit
`priority` ascending, then config file order — that maps or lists the name;
(3) 400 `INVALID_ARGUMENT`. An
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
retention_days = 7               # payloads purged after this many days

[settings]
default_alias = "openai"         # used when the client sends no prefix
request_plugins = []             # global defaults, overridable per instance
response_plugins = []
log_payloads = false             # opt-in redacted payload logging

[providers.openai]               # TEMPLATE — the dropdown entry
base_url = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"   # default for the first instance
models = ["gpt-4o", "gpt-4o-mini"]

[providers.ollama]               # template with no key
base_url = "http://localhost:11434/v1"
api_key_env = ""
models = ["llama3.1"]

[providers.opencode_go]          # speaks the Responses API upstream: style =
base_url = "https://opencode.ai/zen/go/v1"   # "responses" + session_header / identity
api_key_env = "OPENCODE_API_KEY"
models = ["deepseek-v4-flash", "deepseek-v4-pro"]
style = "responses"
session_header = "x-opencode-session"
identity_headers = { "X-Opencode-Client" = "cli", "X-Opencode-Project" = "global" }
# plugins = ["redact"]          # template-level materialization seed only

[[instances]]                    # INSTANCE 1 of openai — alias defaults to "openai"
alias = "openai"
template = "openai"
api_key_env = "OPENAI_API_KEY"
model_aliases = { small = "gpt-4o-mini", medium = "gpt-4o" }  # alias → model
priority = 1                     # lower = higher precedence for unprefixed names
model_reasoning = { small = "high" }  # effort injected on the forwarded call

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

1. **Session correlation.** The effective session id resolves in order: an
   explicit `X-Session-Id` request header (also covering the `x-session-id`
   session-affinity compat spelling); the `x-opencode-session` header
   opencode-style clients send natively; on the Responses surface only, the
   body's `prompt_cache_key` (deriving session id `responses-<key>`, so
   Codex turns of one conversation group into one capture session); else a
   fresh UUID per request. The gateway echoes the effective session id in an
   `X-Gateway-Session-Id` response
   header so any client can correlate without knowing it in advance. Values
   are normalized (safe charset, 128-char cap); unsafe or over-length ids
   degrade to the fresh-UUID fallback rather than being persisted. When
   the instance's template declares a `session_header` (OpenCode Go's
   `x-opencode-session`), the gateway sends that same effective session id
   upstream in the declared header — one stable id per conversation.
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
8. **Retention.** A background loop (catch-up pass at startup, then daily)
   purges sessions older than `retention_days` (default 7; `<= 0` disables
   purging). The cutoff is anchored to the newest recorded session, not the
   wall clock, so the most recent session can never fall on the expired
   side. Requests keep every metadata/statistics column; only the payload
   columns are nulled and their `tool_calls` rows deleted; the session row
   stays, flagged `expired = 1`, so the UI/MCP renders "payloads expired"
   instead of an empty conversation. The §8 JSONL append log is trimmed to
   the same cutoff. Purging never blocks serving or shutdown.

**Record fields** (`requests` row): id, session_id, seq, created_at, alias,
provider (template), model, endpoint (`/v1/chat/completions` or
`/v1/responses`), duration_ms, status_code, finish_reason,
usage_json, request_json (original — chat bytes as received, or the
as-received Responses request on the shim surface), request_filtered_json,
response_json (reassembled — chat-shaped even on the Responses surface),
response_filtered_json, plugins_applied, error_json, truncated, plus the
capture-time token and cost columns (§5): real token usage from the request
(cached tokens included) times the generated price table
(`internal/pricing/models.json`); unpriced models count $0 and are flagged.

### 4.4 Streaming correctness (the critical part)

- Forward SSE line-by-line, flush after each chunk; **never** buffer the whole
  stream before forwarding (latency is non-negotiable for chat UX).
- Handle `data: [DONE]`, keepalive comments, and error events.
- On client disconnect: cancel upstream; persist what was reassembled,
  marked `truncated: true`.
- Tool-call delta reassembly must be index-stable (chunks for tool_call[0] and
  tool_call[1] interleave; concatenate per index).
- Plugin interplay: response plugins run **after** reassembly. With no
  response plugins the stream is forwarded live, chunk by chunk (flush per
  chunk, never buffer the whole stream). Configuring a response plugin for
  a surface switches it to **buffer mode**: hold, reassemble, filter, then
  emit — for chat as one completion block, for the Responses surface as the
  whole typed event sequence in one burst. Per-chunk filtering is a later
  mode.

### 4.5 Plugins

The seam is designed now; only built-ins ship in v1.

```go
type Plugin interface {
    Name() string
    FilterRequest(ctx context.Context, req *Request) error   // input side
    FilterResponse(ctx context.Context, resp *Response) error // output side, post-reassembly
}

type RequestOnly interface{ RequestOnly() bool }  // optional capability:
// request-side only — never populate the response chain (a request-only
// plugin on the response side would just force buffer mode for a no-op).
```

- **Ordering**: `request_plugins` run in config order before forward;
  `response_plugins` run in config order on the reassembled response.
- **Scope**: per-instance `plugins` list overrides the global defaults
  (empty list on an instance = no plugins for that account; absent = inherit
  the global settings). The dashboard exposes this per provider: an
  "inherit global defaults" switch plus a checkbox per plugin, persisted via
  `PATCH /api/instances/{alias}` (`plugins` / `plugins_inherit`).
- **Built-in v1**: `redact` — regex/field-list redaction of messages and tool
  arguments, shipped as the reference implementation (per-plugin config via
  a `[plugins.redact]` table); `sanitize_tools` —
  request-side tool-schema repair that drops the redundant `oneOf`/`anyOf`
  combinator from schema nodes that also declare an `enum` (the duplication is
  valid JSON Schema but some OpenAI-compatible providers answer it with a
  silent empty completion); `fill_reasoning_content` — request-side backfill
  of an empty `reasoning_content` on assistant tool-call messages, so
  deepseek thinking-mode providers (opencode zen/go) stop rejecting replayed
  pure tool-call turns with "The `reasoning_content` in the thinking mode
  must be passed back to the API". The two request-side built-ins declare
  `RequestOnly` and are never added to a response chain.
  Examples of future plugins (v2): RAG context
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
  request_count INTEGER, tool_call_count INTEGER, failure_count INTEGER,
  expired INTEGER DEFAULT 0       -- 1 once retention purged the payloads
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
  truncated INTEGER DEFAULT 0,
  prompt_tokens INTEGER DEFAULT 0,     -- token counts from usage_json
  completion_tokens INTEGER DEFAULT 0,
  cached_tokens INTEGER DEFAULT 0,
  cost_input REAL DEFAULT 0,           -- estimated cost split, USD
  cost_output REAL DEFAULT 0,
  cost_cache_read REAL DEFAULT 0,
  cost_cache_write REAL DEFAULT 0,
  cost_total REAL DEFAULT 0,
  cost_priced INTEGER DEFAULT 0,       -- 1 when the model was in the pricing table
  cost_schema TEXT DEFAULT ''          -- pricing snapshot id that priced the row
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
`requests(created_at, provider, alias, model)` (usage aggregation),
`tool_calls(session_id)`, `tool_calls(tool_name)`. The token/cost columns
were added after the original §5 schema and are applied to pre-existing
databases via `ALTER TABLE` migration, so an old store converges on the same
shape.

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
| **Providers** | Dropdown of templates (built-in + user-added); "Add instance" flow: pick a template → alias auto-filled per §4.2 rules (`openai`, `openai-2`, …) → editable → optional model subset, per-model reasoning levels, plugins ("inherit global defaults" switch + checkbox per plugin), priority, key → save. Instance list with edit / enable / disable / delete and drag-to-reorder (the summary row is the drag handle; order is persisted via `PUT /api/instances/order` and is the routing order for unprefixed names). Per-instance quota/balance display. |
| **Settings** | Listen addrs, store path (read-only after start), retention, default alias, global plugin chain, payload logging, theme (light/dark, persisted in config) — plus the gateway-status card (store stats, disk usage, uptime, plugin list, aliases; formerly its own Status view). |
| **Traces**  | Session list with filters (alias, model, status, date range) + text search; trace detail rendering messages, tool calls, results, verdict badges (SUCCESS / RECOVERABLE / BLIND_ERROR), token/cost usage; per-session JSONL export; per-request raw view (original vs filtered). Expired sessions render as "payloads expired". |
| **Usage**   | Token & estimated-cost time-series from `GET /api/usage` (§6.2): day/week/month buckets, per-provider filter, optional split by model/provider, totals with the input/output/cache split. |

### 6.2 REST API

`GET /api/templates`, `POST /api/templates` (custom provider endpoints; the
`custom_openai` / `custom_anthropic` placeholders resolve to the template
already serving the supplied endpoint — a built-in or a user template, so a
custom add never duplicates a known provider — and only mint a new template
when the endpoint is unknown, named after the instance alias the user typed,
or after the endpoint when the alias is blank) · `GET /api/instances`,
`POST /api/instances`, `PUT /api/instances/order` (instance reordering for
unprefixed resolution), `PATCH /api/instances/{alias}` (rename / enable-
disable / `priority` / `model_aliases` / `model_reasoning` / `plugins` and
`plugins_inherit` / key), `DELETE /api/instances/{alias}`,
`GET /api/instances/{alias}/models` (provider model fetch) · `GET /api/quota`
(per-instance quota/balance, `?refresh=1` bypasses the cache) ·
`GET /api/settings`, `PATCH /api/settings` · `GET /api/plugins`,
`PATCH /api/plugins/{name}` (global enable/disable in the settings chains
plus the `[plugins.<name>]` config table, validated by building the plugin) ·
`GET /api/usage` (usage/cost aggregation, below) ·
`GET /api/secrets/master-key`, `POST /api/secrets/master-key/ack`
(one-time generated master-key exposure + acknowledge; localhost-only) ·
`GET /api/sessions`, `GET /api/sessions/{id}`,
`GET /api/sessions/{id}/export`, `GET /api/status` (store stats, disk usage,
uptime, retention, plugin list, aliases).

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
  an empty object `{}` clears it). Keys must match the alias pattern and values
  must be non-empty — a violation is a 400 `INVALID_ARGUMENT` (the same
  `config.Validate` used for `gateway.toml`). `model_reasoning` replaces its
  whole map the same way; `plugins` (per-instance override) and
  `plugins_inherit` (back to the global defaults) are a pair — the
  dashboard's per-provider "inherit global defaults" switch saves both.
- **Usage & costs** — `GET /api/usage?granularity=day|week|month&provider=&alias=&model=&group=&since=&until=`
  serves the per-period cost/usage buckets (newest last) plus summed totals
  from the aggregated `requests` rows; `group=model|provider` splits each
  period's bucket by the resolved upstream model or provider template for
  the token/cost time-series graphs. Costs are capture-time estimates — real
  token usage times the generated price table
  (`internal/pricing/models.json`, refreshed with `just update-prices`);
  unpriced models count $0 and are flagged, and a fresh checkout has no
  table until `just update-prices` runs (cost estimation stays off until
  then).

### 6.3 Relationship to the MCP server

Same store, different consumers: the UI is for humans scanning sessions; the
MCP server is for agents and scripted analysis. Both stay in sync because
neither owns data — SQLite does.

## 7. Inspection MCP server

Transport: hand-rolled Go MCP server (no framework — a minimal JSON-RPC
surface: `initialize`, `ping`, `notifications/initialized`, `tools/list`,
`tools/call`) over stdio (default) and optional streamable-http (`-http
<addr>` serves `POST /mcp` with no CORS headers). Config:
`-db <path>` (required), optional `-session <id>` scoping, optional
`-http <addr>`. All tools:
`structured_output=False`, return JSON text strings, never throw
(`CallToolResult(isError=True, ...)`); errors as `{errorCode, message}`.

### 7.1 Generic tools (v1)

| Tool | Signature | Returns |
| :--- | :-------- | :------ |
| `list_sessions` | `limit?, alias?, model?, status?, since?, until?` | session summaries + failure counts |
| `get_conversation` | `session_id, include_full?` | ordered replay: user/assistant/tool messages, tool calls interleaved with results |
| `get_request` | `session_id, seq` | raw request/response JSON (both filtered and original) |
| `list_tool_calls` | `session_id?, tool_name?, verdict?, limit?` | tool-call records |
| `validate_tool_call` | `session_id` + `seq`, or `tool_call_id` (the §5 row id, e.g. `req-1/0`) | diffs emitted args vs the schemas **declared in the originating request**: `{ok, unknown_params[], missing_required[], type_mismatches[], nearest_params[]}` (Levenshtein-based nearest-param suggestions, `internal/mcp/analysis.go`) |
| `classify_failure` | `session_id` + `seq`, or `tool_call_id`; `refresh?` | SUCCESS / RECOVERABLE / BLIND_ERROR + reason keywords (internal taxonomy, documented in `internal/mcp/analysis.go`). Caches the verdict + annotation back to `tool_calls` (pull model) — later calls return the cached verdict unless `refresh=true` |
| `export_session` | `session_id` | JSONL — messages-only replay, the eval-fixture format |
| `gateway_status` | — | store path, counts, disk usage, retention, configured aliases |
| `search_conversations` | `query, limit?` | LIKE-based text search over request/response payloads with a snippet around the first match (FTS5 later) |

The key design point: `validate_tool_call` needs **no external schema
knowledge** — the schemas the client declared travel inside the request body
itself.
## 8. Canonical JSONL export format

Every record serializes to one JSON line: `{type, ts, session_id, request_id,
alias, plugins, ...}` with `type ∈ {session_start, request, tool_call,
tool_result, error}`. `export_session`, the UI export button, and the
gateway's append log share this format, so the fuzzer, annotator, and any
future tooling all consume one shape. The append log lives at `<store>.jsonl`
(mode `0600` — it carries the same payloads as the store), is written from
the same capture record as the SQLite row, and is trimmed in sync with the
retention purge (§4.3).

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

