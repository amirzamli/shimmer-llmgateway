# Shimmer LLM Gateway

A capture-only LLM gateway (Go, single static binary) that sits between any
agent / MCP client and an LLM provider, plus inspection surfaces for the
captured traffic:

- **Gateway** — OpenAI-compatible proxy (`/v1/chat/completions`, stream +
  non-stream) that records every request/response full-fidelity into SQLite
  (original *and* filtered payloads, tool calls, errors, usage).
- **HTML UI** — embedded in the gateway binary; manage providers/instances,
  browse sessions, export JSONL.
- **Inspection MCP server** — a separate binary exposing the captured traffic
  to any MCP client (opencode, Claude Desktop, Cursor) via stdio or
  streamable-http, including tool-call validation and failure classification.

## Requirements

- **Go 1.26+** (`go.mod` requires 1.26.2) — no CGO needed (the build uses
  modernc.org/sqlite).
- [just](https://github.com/casey/just) (optional) — convenient build/run task
  shortcuts; everything is plain `go` commands underneath.
- Network access for `just update-prices`, which downloads the models.dev
  pricing catalog (see [Usage statistics](#usage-statistics-estimated-costs)).

## Quick start

```bash
# 1. Build both binaries
just build                                 # writes bin/gateway + bin/inspect-mcp
# ...or without just:
#   mkdir -p bin
#   go build -o bin/gateway ./cmd/gateway
#   go build -o bin/inspect-mcp ./cmd/inspect-mcp

# 2. Generate the pricing table (used by the Usage view)
just update-prices                         # writes internal/pricing/models.json

# 3. Run the gateway (capture surface)
./bin/gateway -config gateway.toml          # serves http://127.0.0.1:8787

# 4. Point an OpenAI-compatible client at it
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# 5. Inspect what was captured via the MCP server (stdio)
./bin/inspect-mcp -db gateway.db
```

Everything captured lands in the SQLite store (`gateway.db` by default); the
UI and the MCP server both read that same store.

## Running the gateway

```
usage: gateway -config <path>
```

The gateway loads `gateway.toml`, opens the SQLite store, and serves one
listener per configured `listen_addrs` entry:

| Path | Purpose |
| :--- | :------ |
| `GET /healthz` | health check → `{"status":"ok"}` |
| `GET /` | embedded HTML UI |
| `GET/POST/PATCH/DELETE /api/*` | UI REST surface (config mutations hot-reload) |
| `GET /v1/models` | models across all configured instances |
| `POST /v1/chat/completions` | the only capture surface (stream + non-stream) |

> **Security & binding.** The gateway refuses to bind anything but loopback
> (`127.0.0.0/8`, `::1`, `localhost`), CGNAT (`100.64.0.0/10` — the Tailscale
> default range), and ULA (`fc00::/7`) addresses; a wildcard (`0.0.0.0`) or a
> plain LAN address (`192.168.x.x`) is refused at startup. This lets you serve
> localhost and a Tailscale IP side by side — `listen_addrs = ["127.0.0.1:8787",
> "100.64.0.1:8787"]` — without opening the unauthenticated API to your whole
> LAN. The secrets master-key endpoints are localhost-only regardless of what
> you bind. Provider `base_url` is trusted config: the gateway validates an
> http(s) scheme + host and never follows redirects, but it will still forward
> to any configured http(s) host, including local ones (ollama, vllm).

### Configuration (`gateway.toml`)

```toml
listen_addrs = ['127.0.0.1:8787', '100.64.0.1:8787']   # one socket per address
store  = 'gateway.db'
retention_days = 7                                     # default when unset; <= 0 disables

[settings]
default_alias = 'openai'        # used when a client sends no alias prefix
request_plugins = []            # global defaults, overridable per instance
response_plugins = []

[providers.openai]              # TEMPLATE: the dropdown entry
base_url = 'https://api.openai.com/v1'
api_key_env = 'OPENAI_API_KEY'  # default for the first instance
models = ['gpt-4o', 'gpt-4o-mini']
plugins = ['retry_empty']       # template-level seed (see plugin resolution below)

[[instances]]                   # INSTANCE 1 of openai — alias defaults to "openai"
alias = 'openai'
template = 'openai'
api_key_env = 'OPENAI_API_KEY'

[[instances]]                   # INSTANCE 2 — a second account
alias = 'openai-2'
template = 'openai'
api_key_env = 'OPENAI_API_KEY_2'
plugins = ['redact']            # per-instance override of the global chain
```

Key rules:

- **Templates** (`[providers.<name>]`) define providers; a set of built-ins
  ships with the binary (openai, anthropic, ollama, groq, vllm, lite_llm,
  plus extras). **Instances** (`[[instances]]`) are concrete accounts of a
  template.
- **Aliases** are the routing key: a model named `alias/model` routes to that
  instance; an unprefixed model resolves via `settings.default_alias` (then
  the first instance listing it). Auto-naming: `openai`, `openai-2`, `openai-3`, …
- **Routing priority.** Unprefixed names resolve over enabled instances in
  config file order by default. A per-instance `priority = <n>` line reorders
  that: a lower value wins over a higher one, and any instance with an
  explicit priority beats instances without one (which keep file order among
  themselves). Prefixed `alias/model` routing is never affected. Set
  `priority = 1` on a primary account and `priority = 2` on a fallback to pin
  which one serves unprefixed names without reordering the file. `0` (or an
  absent line) means unset. Editable in the UI per instance.
- **Plugin resolution.** Each instance's effective plugin chain comes from the
  instance's own `plugins` list when set — `plugins = ["retry_empty"]` turns a
  plugin on, an explicit `plugins = []` turns all plugins off for that account
  (and survives write-backs) — else the global `settings.request_plugins` /
  `settings.response_plugins`. An instance with **no `plugins` line resolves
  from the global settings only, which are empty by default = OFF**: there is
  no hidden code-level or template-level runtime fallback. A template-level
  `plugins` list (`[providers.<name>]`) is a *materialization seed*: when an
  instance of that template has no `plugins` of its own, a config write-back
  records a copy of the seed as the instance's `plugins` line, so defaults are
  explicit in the file. Lists apply to both the request and response sides.
- **`retry_empty`** is a control plugin: it is valid config (listed by
  `plugins.Known()` / `GET /api/status`) but never transforms payloads. When
  active it re-issues an upstream request that comes back prematurely empty
  (no content, no tool calls — regardless of `finish_reason`), up to 3
  attempts, so the client sees a completed turn instead of a reasoning trace
  followed by silence. It is **on by default for `opencode_go` instances** and
  off everywhere else — see [The `retry_empty` control plugin](#the-retry_empty-control-plugin) below.
- **API keys never live in `gateway.toml`.** Per-instance keys come from the
  `api_key_env` environment variable, or from the UI-managed secrets file
  `<store>.secrets.json`, which is an AES-256-GCM encrypted envelope (`chmod
  0600`). The gateway injects the resolved key itself, so clients never need
  one. See [Master key (`SHIMMER_MASTER_KEY`)](#master-key-shimmer_master_key)
  for the key setup, generation flow, and loss warning.
- Config mutations made through the UI/REST API are written back to
  `gateway.toml` and applied atomically to the next request — no restart.
  The file is rewritten wholesale, so hand-edited comments and formatting are
  not preserved.

#### The `retry_empty` control plugin

**What it is for.** Some providers (notably those behind `opencode_go`
instances, and OpenRouter reasoning models like `stealth/ox-alpha`)
intermittently stream reasoning/thinking deltas and then stop with **no
content and no tool calls** — the client sees a reasoning trace followed by
silence and often needs a manual "continue". `retry_empty` re-issues the
upstream request when it detects this premature-empty result, so the turn
completes without operator intervention.

**How it works.** Only a 2xx response whose trimmed message content is empty
(whitespace-only counts) **and** has no `tool_calls` is considered empty;
reasoning/thinking tokens never count as content, and `len(choices) == 0` also
counts as empty. The `finish_reason` is deliberately **not** consulted: an
empty assistant turn is useless to the client whether the provider stopped
with an empty `finish_reason` or a populated one (`stop`, `length`, …), so
both are retried. Transport errors and non-2xx responses are **never**
retried. On a match the gateway re-issues the identical upstream request —
stream or non-stream — up to **3 attempts total** (constant cap, no config
knob). For streams the client sees `200` + SSE headers and then the provider's
reasoning/thinking deltas forwarded live (the stream is held/buffered only for
the final content block, so the agent never waits in silence and idle timeouts
never fire); the final attempt — even if still empty — is emitted to the
client. Exactly one capture row is written per client request; retries are
logged via `logger.Warn` ("retry_empty", with request id, alias, attempt).

**Enabling / disabling.** `retry_empty` is a control plugin: it is valid
config (listed by `plugins.Known()` / `GET /api/status`) but never transforms
payloads, and a per-instance `plugins` line applies it to both the request and
response sides. It is **on by default for `opencode_go` instances**: the
built-in template seeds `plugins = ["retry_empty"]` and the gateway
materializes that seed into each instance's `plugins` line on config
write-back, so the default-on is visible in `gateway.toml`. All other
templates default **off**. Set `plugins = ["retry_empty"]` on an instance to
turn it on; an explicit `plugins = []` turns it off durably. An instance with
**no `plugins` line has retrying OFF** at runtime (it resolves from the global
`settings.request_plugins` / `settings.response_plugins`, empty by default).

**When it does NOT help.** It does not retry upstream transport errors or
4xx/5xx responses, and a deterministic provider failure (e.g. context
exhaustion in a very long session) may still end empty after 3 attempts — the
empty result is then passed through. It masks intermittent stops; it does not
cure provider-side aborts.

### Model aliases

Per-instance `model_aliases` map a friendly name (`small`, `medium`,
`large`, …) to any concrete provider model string, so harness users can
request an alias and the operator decides which model it resolves to. Alias
keys follow the same `[a-z0-9._-]+` rule as instance aliases; values are
non-empty model strings (slashes allowed, e.g.
`meta-llama/Meta-Llama-3-8B-Instruct`).

```toml
[[instances]]
alias = 'openai'
template = 'openai'
api_key_env = 'OPENAI_API_KEY'

[instances.model_aliases]
small  = 'gpt-4o-mini'
medium = 'gpt-4o'
large  = 'gpt-4o'
```

(The inline `model_aliases = { small = 'gpt-4o-mini' }` form inside the
`[[instances]]` block works too.)

Routing / expansion:

- **Unprefixed names** (`small`, `medium`) resolve in order: (1) the
  `settings.default_alias` instance's map; (2) the first enabled instance in
  config order whose map contains the name; (3) the existing literal fallback
  (`default_alias` list, then the first instance listing the model).
- **Prefixed names** (`openai/small`) expand only when the name is a key of
  that instance's map; otherwise the name passes through verbatim.
- **Shadowing:** an alias mapping wins over literal model membership, so an
  alias key may legitimately collide with a real model name.
- **Single-level:** the mapped value is forwarded verbatim and never
  re-expanded (`small = 'medium'` routes the literal model `medium`).
- **Disabled instances** contribute no aliases and are never routed to;
  `GET /v1/models` lists alias ids (bare key and `alias/key`) for enabled
  instances only, alongside the real models.

Provider model fetching — the dashboard's model dropdown is populated from the
provider itself. The gateway fetches `GET <base_url>/models` (OpenAI list
shape) with the instance's resolved key, or with no Authorization header for
keyless providers (ollama, vllm), caches the result **in-memory for 5
minutes**, and never persists it. A manual **Refresh** in the dashboard (or
`?refresh=1` on the API) bypasses the cache; a PATCH or DELETE on the instance
invalidates it. On any fetch failure — including the built-in `anthropic`
template, whose `/models` fetch can fail on its non-native path — the
configured models are returned instead (`source: "config"`) with only the
generic error `"provider models unavailable"`; the underlying detail is
written to the gateway log.

Dashboard: the **Providers** view has a **Model aliases** editor per instance —
add/remove rows (alias name + model select from the fetched list, or type a
custom model), Refresh, and Save (a PATCH replacing the whole map). Edits
persist to `gateway.toml` like the rest of the config UI.

### Usage statistics (estimated costs)

The **Usage** view aggregates cost and token usage over all captured requests:
daily, weekly, or monthly buckets, filterable per provider/alias/model, with
the cost split on input, output, cache reads (cache writes are not derivable
from the OpenAI usage shape, so they estimate at $0). Costs are **estimates**
computed at capture time from the request's real token usage — including cache
reads when the provider reports them (`prompt_tokens_details.cached_tokens`,
or DeepSeek's `prompt_cache_hit_tokens`) — times a generated price table (USD
per 1M tokens) loaded from disk at startup
(`internal/pricing/models.json`). Models not in the table count
as unpriced ($0) and are flagged (`unpriced` badge), so a total is never
mistaken for a complete ledger. Models absent from the generated table are
unpriced — not necessarily free — and `just update-prices` refreshes the
table from the models.dev catalog.

- The data lands in new `requests` columns (`prompt_tokens`, `cost_input`, …,
  `cost_total`, `cost_priced`, `cost_schema`); existing stores get them via an
  automatic schema migration on next start (cheap `ALTER TABLE`, no rewrite).
- **The price table is generated, not hand-curated**: `just update-prices`
  (or `go run ./cmd/genpricing`) fetches the models.dev catalog
  (https://models.dev/api.json) and rewrites `internal/pricing/models.json`;
  the gateway loads the table from disk at startup, so restart to take
  effect. A fresh checkout has no table until you generate one — cost
  estimation then stays off (the gateway logs `pricing_load_failed`) and
  requests are captured unpriced.
  Pricing is per provider — the serving provider (the gateway template name)
  selects the price row, and a user-defined custom endpoint whose base URL
  matches a catalog provider's API base inherits that provider's prices.
  Providers absent from the catalog (e.g. commandcode) are emitted with
  prices **inherited** from the origin lab's own catalog entry, flagged
  `inherited_from` in the JSON. Tiered context pricing is approximated with
  the flat base rates.
- **Historical rows are frozen**: the cost split is computed once at capture
  time and never rewritten by a table update — `requests.cost_schema` records
  which snapshot priced each row. Requests captured while their model was
  unpriced (unknown or since-learned price) are priced once at startup by an
  idempotent backfill using the current table.
- Surface: `GET /api/usage?granularity=day|week|month&provider=&alias=&model=&group=&since=&until=`
  (buckets + summed totals), plus the embedded UI (Usage tab). A `group=model`
  or `group=provider` parameter additionally splits every period's bucket by
  the **resolved upstream model** (never the client-facing alias) or provider
  template, which the Usage tab renders as per-period stacked token charts
  with a per-series cost legend. Token counts and the cost split also ride the
  §8 append-log/export request line (`tokens` object).
- The generated table mirrors models.dev, whose prices drift — treat totals as
  approximations, not an invoice.

### Master key (`SHIMMER_MASTER_KEY`)

The UI-managed secrets file `<store>.secrets.json` holds per-instance API keys
and is encrypted at rest with AES-256-GCM (stdlib crypto only) as a single JSON
envelope, kept `chmod 0600`. The master key is the only way to decrypt it:

- **Set your own key (recommended).** Export a base64-encoded 32-byte key
  before first run so the gateway never generates one:
  ```bash
  openssl rand -base64 32
  export SHIMMER_MASTER_KEY="<output>"
  ```
  The value must be valid base64 of exactly 32 bytes; any other value is a
  fatal startup error (the value itself is never logged).
- **First run without the env var.** The gateway generates a random key, keeps
  it in memory, and offers it once in the dashboard: a banner appears on page
  load with the base64 key, a **Copy** button, and an **"I've saved it"**
  button that is enabled only after you click **Copy**. Clicking **"I've saved
  it"** acknowledges the key and clears it from memory — reloading the page
  will not show it again. **Copy and save it before acknowledging.**
- **Migration.** A legacy plaintext `<store>.secrets.json` is re-encrypted in
  place automatically — at startup when `SHIMMER_MASTER_KEY` is set, or when
  you acknowledge a generated key in the dashboard (the file stays plaintext
  on disk until then).
- **Loss warning.** If you lose the master key, the stored API keys cannot be
  recovered — there is no recovery path. Keep it somewhere safe (a password
  manager).
- **Recovery for a fresh, empty store.** If you lose the generated key for a
  store that holds no real secrets yet, deleting `<store>.secrets.json` resets
  to a fresh store: the next start generates a new key and offers it again. Do
  **not** delete the file once it holds real keys.
- **Restart behavior.** With `SHIMMER_MASTER_KEY` set, the gateway opens the
  encrypted file with your key. With it unset, an existing encrypted file
  refuses to start with a clear error — set the env var to continue.

Verify it is up:

```bash
curl http://127.0.0.1:8787/healthz          # {"status":"ok"}
curl http://127.0.0.1:8787/v1/models        # alias/model ids
```

## Configuring harnesses to use the gateway

The gateway speaks OpenAI-compatible Chat Completions only. Any client that
can point at a custom `baseURL` can route through it; every request is
captured. Point clients at `http://127.0.0.1:8787/v1` and use `alias/model`
as the model name (or unprefixed, resolved via `default_alias`).

### opencode

Add a custom provider and select a model in `opencode.json` (project or
`~/.config/opencode/opencode.json`):

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "shimmer": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Shimmer Gateway",
      "options": { "baseURL": "http://127.0.0.1:8787/v1", "apiKey": "gateway" },
      "models": {
        "openai/gpt-4o":   { "name": "gpt-4o (openai account)" },
        "openai-2/gpt-4o": { "name": "gpt-4o (openai-2 account)" },
        "ollama/llama3.1": { "name": "llama3.1 (ollama)" }
      }
    }
  },
  "model": "shimmer/openai/gpt-4o"
}
```

- `apiKey` is a dummy — the gateway ignores client credentials and injects the
  real key from the instance config.
- Model ids are the routing key: `openai/gpt-4o` → instance `openai`, model
  `gpt-4o`; use the prefix form to select a specific account.
- Model aliases work the same way: any id `GET /v1/models` lists is routable —
  `shimmer/small` for an unprefixed alias, `shimmer/openai/small` for a
  prefixed one (see [Model aliases](#model-aliases)).
- opencode loads config at startup — **restart opencode** after editing.

### Other OpenAI-compatible clients (LibreChat, custom scripts, …)

Set `base_url` to `http://127.0.0.1:8787/v1` and request any configured
`alias/model`. No auth header needed.

## Running the MCP inspection server

```
usage: inspect-mcp -db <path> [-session <id>] [-http <addr>]
```

| Flag | Purpose |
| :--- | :------ |
| `-db <path>` | path to the gateway SQLite store (**required**) |
| `-session <id>` | optional scope: constrain the generic tools to one session |
| `-http <addr>` | serve streamable-http (e.g. `127.0.0.1:9876`) instead of stdio |

The MCP server reads the store directly — the gateway does not need to be
running for it to work, but the gateway must run to capture new traffic.

**stdio** (default — what agents launch):

```bash
./bin/inspect-mcp -db /abs/path/to/gateway.db
```

**streamable-http** (optional, standalone):

```bash
./bin/inspect-mcp -db gateway.db -http 127.0.0.1:9876
# endpoint: POST http://127.0.0.1:9876/mcp
```

Smoke test over stdio:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
  | ./bin/inspect-mcp -db gateway.db
```

### Tools

| Tool | What it does |
| :--- | :----------- |
| `list_sessions` | sessions with failure counts; filters `limit/alias/model/status/since/until` |
| `get_conversation` | ordered replay of a session (messages, tool calls, results); `include_full` |
| `get_request` | raw request/response for one request (`session_id`, `seq`) — original + filtered |
| `list_tool_calls` | tool-call records; filters `session_id/tool_name/verdict/limit` |
| `export_session` | session as §8 canonical JSONL (eval-fixture format) |
| `gateway_status` | store path, counts, disk usage, retention, aliases |
| `search_conversations` | LIKE-based text search over payloads (`query`, `limit`) |
| `validate_tool_call` | diffs emitted args vs the schemas the client declared — `{ok, unknown_params[], missing_required[], type_mismatches[], nearest_params[]}` |
| `classify_failure` | SUCCESS / RECOVERABLE / BLIND_ERROR + reasons; caches verdict into the store |

All tools return JSON text, never throw, and report errors as
`{errorCode, message}`.

## Installing the MCP server in agents

### opencode

`opencode.json` (project or global):

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "shimmer-inspect": {
      "type": "local",
      "command": [
        "/abs/path/to/bin/inspect-mcp",
        "--db", "/abs/path/to/gateway.db"
      ]
    }
  }
}
```

Or, for the streamable-http transport:

```jsonc
{
  "mcp": {
    "shimmer-inspect": {
      "type": "remote",
      "url": "http://127.0.0.1:9876/mcp"
    }
  }
}
```

**Restart opencode** — MCP servers are loaded at startup. Then check the
tools are live (`/mcp` or the tools list) and try `list_sessions`.

### Claude Desktop

`~/Library/Application Support/Claude/claude_desktop_config.json`
(Linux: `~/.config/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "shimmer-inspect": {
      "command": "/abs/path/to/bin/inspect-mcp",
      "args": ["--db", "/abs/path/to/gateway.db"]
    }
  }
}
```

### Cursor

`~/.cursor/mcp.json` — same `mcpServers` shape as Claude Desktop.

## End-to-end flow

1. Start the gateway: `./bin/gateway -config gateway.toml`
2. Point a harness at it (e.g. opencode provider `shimmer/...`) and run a task
   — every request is captured to `gateway.db`.
3. Inspect: browse sessions in the UI (`http://127.0.0.1:8787/`), or run
   `validate_tool_call` / `classify_failure` on a tool call from an MCP
   client.

## Notes

- Only `/v1/chat/completions` is proxied; any other `/v1/*` path returns 404.
- Streaming is forwarded line-by-line (never fully buffered) unless a control
  plugin such as `retry_empty` is active, in which case the stream is
  held/buffered until the attempt completes; on client disconnect the captured
  response is marked `truncated`.
- Response plugins run post-reassembly (buffer mode); the store always keeps
  both original and filtered payloads plus `plugins_applied`.
- Sessions older than `retention_days` (7 by default; `<= 0` disables) have
  their payloads expired at startup and daily: the conversation payloads
  (`request_json`, `response_json`, filtered variants) and tool calls are
  removed, while the sessions/requests rows stay as metadata — timestamps,
  model, status, token counts, and costs — so the usage statistics and
  aggregates outlive the chats. Expired sessions are flagged `expired` in the
  API/UI ("payloads expired").

## Security

Shimmer LLM Gateway is a localhost developer tool, not a hardened multi-user
service: it is not designed to be exposed to untrusted networks. The shipped
defaults:

- **Listen-address binding policy.** The gateway refuses to bind anything but
  loopback, CGNAT (`100.64.0.0/10` — the Tailscale default range), and ULA
  (`fc00::/7`) addresses; wildcard and plain LAN addresses are refused at
  startup (see [Security & binding](#security--binding)).
- **Admin API + UI request validation.** The unauthenticated `/api/*` surface
  and the embedded UI only serve requests whose `Host` names an address the
  gateway actually serves (a bound listen address, a loopback host, or a
  literal loopback/CGNAT/ULA address), and reject cross-site `Origin` /
  `Sec-Fetch-Site` requests on state-changing methods — a web page cannot
  drive the admin API from your browser. The capture surface
  (`/v1/chat/completions`) is not subject to these checks, so non-browser
  clients are unaffected.
- **Secrets at rest.** Per-instance API keys live in `<store>.secrets.json`
  (mode `0600`), encrypted with AES-256-GCM under a master key you set via
  `SHIMMER_MASTER_KEY` or a one-time generated key acknowledged in the UI
  (see [Master key](#master-key-shimmer_master_key)). The master-key
  endpoints refuse non-loopback client sources regardless of what you bind.
- **No CORS on the MCP HTTP transport.** The `inspect-mcp` streamable-http
  endpoint sends no `Access-Control-Allow-*` headers, so browser pages cannot
  read captured traffic; non-browser MCP clients don't need CORS.
- **Payload redaction.** The `redact` plugin and the opt-in console payload
  logging mask sensitive patterns and fields (`[REDACTED]`) before data
  leaves the process.
- **Retention.** Payloads older than `retention_days` are purged at startup
  and daily (see [Notes](#notes)); only request metadata survives expiry.

See [SECURITY.md](SECURITY.md) for the threat model and how to report a
vulnerability.

## License

MIT — see [LICENSE](LICENSE).
