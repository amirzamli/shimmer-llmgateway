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

## Quick start

```bash
# 1. Build both binaries
mkdir -p bin
PATH=$PATH:/usr/local/go/bin go build -o bin/gateway ./cmd/gateway
PATH=$PATH:/usr/local/go/bin go build -o bin/inspect-mcp ./cmd/inspect-mcp

# 2. Run the gateway (capture surface)
./bin/gateway -config gateway.toml          # serves http://127.0.0.1:8787

# 3. Point an OpenAI-compatible client at it
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# 4. Inspect what was captured via the MCP server (stdio)
./bin/inspect-mcp -db gateway.db
```

Everything captured lands in the SQLite store (`gateway.db` by default); the
UI and the MCP server both read that same store.

## Running the gateway

```
usage: gateway -config <path> [-allow-remote]
```

The gateway loads `gateway.toml`, opens the SQLite store, and serves:

| Path | Purpose |
| :--- | :------ |
| `GET /healthz` | health check → `{"status":"ok"}` |
| `GET /` | embedded HTML UI |
| `GET/POST/PATCH/DELETE /api/*` | UI REST surface (config mutations hot-reload) |
| `GET /v1/models` | models across all configured instances |
| `POST /v1/chat/completions` | the only capture surface (stream + non-stream) |

> **Security & binding.** The gateway binds loopback-only by default
> (`127.0.0.1:8787`); pass `-allow-remote` to permit a non-loopback listen
> address. The secrets master-key endpoints are localhost-only even then.
> Provider `base_url` is trusted config: the gateway validates an http(s)
> scheme + host and never follows redirects, but it will still forward to any
> configured http(s) host, including local ones (ollama, vllm).

### Configuration (`gateway.toml`)

```toml
listen = '127.0.0.1:8787'
store  = 'gateway.db'
retention_days = 30

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
  `plugins.Known()` / `GET /api/status`) but never transforms payloads.
  When active it makes the gateway re-issue an upstream chat-completion
  request — stream or non-stream — when the provider returns a
  premature-empty result (no content, no tool calls, empty/missing
  finish_reason), up to 3 attempts, transparently to the client. It is **on
  by default for `opencode_go` instances**: the built-in template seeds
  `plugins = ["retry_empty"]` and the gateway materializes that seed into the
  config file's `plugins` line for each `opencode_go` instance on write-back,
  so the default-on is visible in `gateway.toml`. Off everywhere else. At
  runtime, an instance with **no `plugins` line has retrying OFF**; enable it
  with `plugins = ["retry_empty"]` and disable it durably with `plugins = []`.
  Reasoning-only streams never count as content, so an `opencode_go` stream
  that emits reasoning and then nothing is held and re-issued.
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
- Streaming is forwarded line-by-line (never fully buffered); on client
  disconnect the captured response is marked `truncated`.
- Response plugins run post-reassembly (buffer mode); the store always keeps
  both original and filtered payloads plus `plugins_applied`.
- Sessions older than `retention_days` are purged at startup and daily.

## License

MIT — see [LICENSE](LICENSE).
