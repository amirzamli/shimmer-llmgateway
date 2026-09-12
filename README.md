# Shim**mer, an LLM Gateway**

<p align="center">
  <img src="docs/screenshots/dashboard-tour.gif" alt="Shimmer Gateway dashboard tour" width="1200">
</p>

**Shim**mer is a capture-only, OpenAI-compatible LLM gateway. It proxies chat
completions and the Responses API, records traffic to SQLite, and includes a
dashboard and an MCP inspector.

## Setup
[![Go 1.26](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev) [![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Utilizing the justfile is optional, but my personal preference for collecting the useful commands in one place.

Clone the repository:

```bash
git clone https://github.com/amirzamli/shimmer-llmgateway.git
cd shimmer-llmgateway
```

Set the provider credentials you need. For example:

```bash
export OPENAI_API_KEY="your-provider-key"
# Optional, but recommended if you use UI-managed provider keys:
export SHIMMER_MASTER_KEY="$(openssl rand -base64 32)"
```

`SHIMMER_MASTER_KEY` is optional for environment-only keys. If it is omitted,
the gateway generates a key on first run; set and keep the displayed key if
you want UI-managed keys to survive restarts.

### Without `just`

```bash
cp gateway.toml.example gateway.toml
go run ./cmd/gateway -config gateway.toml
```

### With `just`

```bash
just init
just run
```

No build step is required for either setup: `go run` and `just run` compile the
gateway as needed. The bare `just` target is a development aggregate that does
include a build. Use `go build` or `just build` only when you want reusable
binaries in `bin/` (including `inspect-mcp`); `just build` disables CGO.

## Use the gateway

The dashboard is at <http://127.0.0.1:8787>. Set an OpenAI-compatible client's
base URL to:

```text
http://127.0.0.1:8787/v1
```

Client API keys are not required; provider credentials stay inside the
gateway. A quick smoke test:

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"openai/gpt-4o","messages":[{"role":"user","content":"Hello"}]}'
```

Configure providers and instances in `gateway.toml` or from the dashboard.
The available configuration fields are documented in
[`gateway.toml.example`](gateway.toml.example).

## MCP inspector (optional)

Inspect captures from the same SQLite store with:

```bash
go run ./cmd/inspect-mcp -db gateway.db
```

The equivalent Just command is `just mcp db=gateway.db`. For streamable HTTP,
add `-http 127.0.0.1:9876` (or use `just mcp-http`); keep that endpoint on a
trusted network because it has no authentication.

Captures are stored in `gateway.db`. UI-managed keys are stored encrypted in
`gateway.db.secrets.json`; keep `SHIMMER_MASTER_KEY` safe. Shimmer is intended
for local development and trusted tailnets, not as a public service.

## Development

```bash
go test ./...
```

With `just`, common commands are `just fmt`, `just vet`, `just test`, and
`just run-test`.

## License

MIT — see [`LICENSE`](LICENSE).
