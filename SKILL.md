# Shimmer LLM Gateway — Project Skill

Capture-only LLM gateway in a single static Go binary. It proxies OpenAI-compatible
`/v1/chat/completions` **and** the OpenAI Responses API (stream + non-stream) to
configured upstreams, recording every request/response full-fidelity to SQLite. An
embedded UI (single HTML file) manages providers/instances/traces, and a separate MCP
server (`inspect-mcp`) exposes captured traffic to agents. **It never answers prompts
itself — it records what passes through it.**

Authoritative references: `README.md` (usage), `web/index.html` (UI), the
implementation and tests, and the `justfile`.

## Core concepts (read before touching anything)

- **Templates vs instances.** `[providers.<name>]` = templates (built-ins: openai,
  anthropic, ollama, groq, vllm, lite_llm, opencode_go, …). `[[instances]]` = concrete
  accounts of a template, each with an `alias` (`openai`, `openai-2`, …). Aliases match
  `[A-Za-z0-9_.-]+` (single interior spaces allowed for display).
- **Alias routing.** A model id `alias/model` routes to that instance. Unprefixed names
  resolve via `settings.default_alias`, then the first enabled instance (lowest
  `priority` first) listing the model. `model_aliases` per instance map friendly names →
  concrete models (`small = 'gpt-4o-mini'`); single-level expansion only.
- **Capture-only.** The gateway injects real upstream API keys, so **clients send no
  auth**. Everything is stored in the SQLite store (`gateway.db` by default); UI + MCP
  both read that same store.
- **Secrets never live in `gateway.toml`.** Keys come from `api_key_env` env vars or the
  secrets file `<store>.secrets.json` (mode 0600, AES-256-GCM under `SHIMMER_MASTER_KEY`
  — a base64-encoded 32-byte key set **before first run**; losing it = keys unrecoverable).
- **Hot config write-back.** UI/REST config edits rewrite `gateway.toml` wholesale
  (comments lost) and apply atomically to the next request — no restart.
- **Plugins.** Per-instance `plugins` list overrides `settings.request_plugins` /
  `settings.response_plugins` (absent → global, empty = off). Built-ins:
  `redact`, `sanitize_tools` (request-only), `fill_reasoning_content`
  (request-only). Note: `retry_empty` appears in the justfile/example config
  but is **not implemented** — don't rely on it.
- **Session-requiring upstreams.** Templates may declare a `session_header` (e.g.
  `opencode_go` → `x-opencode-session`); the gateway sends the client's `X-Session-Id`
  or a generated UUID (echoed as `X-Gateway-Session-Id`) in that header.
- **Security posture.** Binds only loopback / CGNAT (`100.64.0.0/10`) / ULA — refuses
  wildcard/LAN. `/api/*` + UI check `Host`/`Origin`. A localhost dev tool, not multi-user.

## Repository map

```
cmd/gateway/        gateway main (-config gateway.toml)
cmd/inspect-mcp/    MCP inspection server (-db <store> [-session <id>] [-http <addr>])
cmd/genpricing/     regenerates internal/pricing/models.json from models.dev
internal/config/    TOML parsing/validation, providers.json built-ins, config manager
internal/gateway/   HTTP server: proxy, streaming (SSE), capture, redaction, responses shim
internal/store/     SQLite schema (sessions, requests, tool_calls) + capture/query
internal/mcp/       MCP tool registry, replay, views, analysis (validate/classify)
internal/api/       REST API (instances, secrets, sessions, usage, quota, plugins…)
internal/plugins/   redact, sanitize_tools (request-only), fill_reasoning_content (request-only)
internal/quota/     per-instance token/cost quotas
internal/pricing/   cost estimation (models.json, generated, NOT tracked in git)
internal/secrets/   master-key encryption of stored API keys
internal/netutil/   bindable-host policy, logging
web/index.html      the entire UI, no build step (embedded at build; served live from
                    the working dir when present)
scripts/run-test.sh per-worktree isolated test gateway; scripts/mockprovider = fake upstream
docs/                 current documentation and dashboard screenshots
```

## Getting going (agent quick start)

```bash
just build          # static binaries into bin/ (CGO disabled, modernc sqlite)
# quickest path to a working gateway + dummy data, isolated per branch:
just run-test       # random loopback port, own store under .run-test/<branch>/,
                    # mock provider, seeds tool round-trip / streaming / failure traffic
just run-test-seed  # re-send dummy conversations (gateway must be running)
just run-test-clean # wipe this worktree's run-test state
```

Or run the real thing:

```bash
just init               # write starter gateway.toml (only if missing)
export OPENAI_API_KEY=… # and/or SHIMMER_MASTER_KEY=<base64 32-byte> before first run
just run                # go run ./cmd/gateway -config gateway.toml  (default 127.0.0.1:8787)
```

Exercise it (no auth needed):

```bash
curl http://127.0.0.1:8787/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

## Inspecting captured traffic (the MCP server)

`bin/inspect-mcp -db <store>` reads the store directly — the gateway needn't run. In this
repo's pi environment `.pi/mcp.json` already wires the `shimmer-inspect` MCP server to
`bin/inspect-mcp -db gateway.db` (the root store), so agents can query live captures
through MCP tools. Tools (all return JSON, never throw; errors as `{errorCode, message}`):

- `list_sessions` — newest first; filters alias/model/status/since/until/limit
- `get_conversation` — ordered replay of one session (`include_full` embeds raw payloads)
- `get_request` — raw request/response JSON by `session_id` + `seq`
- `list_tool_calls` — filter by session/tool_name/verdict (SUCCESS|RECOVERABLE|BLIND_ERROR)
- `export_session` — §8 canonical JSONL eval-fixture format
- `gateway_status` — store path, counts, retention, aliases
- `search_conversations` — LIKE text search over payloads with snippets
- `validate_tool_call` — diffs emitted tool-call args against client-declared schemas
- `classify_failure` — SUCCESS/RECOVERABLE/BLIND_ERROR with reasons; caches verdicts to DB

Also `-session <id>` scopes all tools to one session; `-http <addr>` serves
streamable-http (POST /mcp) with no CORS instead of stdio.

## UI / REST surface

- UI: single page at `/` — Traces, Trace detail, Sessions, Usage tabs, provider/instance
  management, secrets, plugins, quota. `web/index.html` edits appear on browser refresh
  (served from cwd when present; otherwise the build-time embedded copy).
- REST: unauthenticated `/api/*` (instances, templates, secrets, sessions, usage,
  settings, quota, status), `GET /healthz`, `GET /v1/models` (5-min cache, `?refresh=1`),
  `PATCH/DELETE` on instances invalidates the cache. Config mutations write back to
  `gateway.toml`.
- `POST /v1/responses` is a **stateless** Responses shim over the chat pipeline:
  `previous_response_id` → 400, `GET /v1/responses/{id}` → 404, `store`/`include` are
  accepted and ignored; typed SSE events on stream; only function tools forwarded.

## Common tasks

- `just fmt` / `just vet` / `just test` (or `go test ./...`) / `just race` / `just build`.
- `just update-prices` — regenerate the untracked pricing table (needs network).
- `just mcp db=…` / `just mcp-http db=…` — run the inspector ad hoc.

## Gotchas & conventions

- **Go 1.26+**, no CGO: `CGO_ENABLED=0 go build` with modernc.org/sqlite. Don't add cgo deps.
- Config file is rewritten wholesale on UI changes — don't rely on comments/formatting
  surviving; but do keep `gateway.toml` free of real keys.
- `.run-test/` (isolated gateway state per branch), `bin/`, `*.db*` files, and
  `internal/pricing/models.json` are not tracked — don't commit them.
- The gateway serves `web/index.html` from the working directory when present — that is
  intentional for live UI editing, not a bug.
- Code comments cite spec sections (`§4.2`, `§7.1`…) — when behavior is unclear, read
  the relevant implementation and tests first.
- Retention: payloads older than `retention_days` get purged (catch-up at startup, then
  daily); metadata rows stay, expired sessions flagged `expired`.
