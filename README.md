# Shim**mer, an LLM Gateway**

<p align="center">
  <img src="docs/screenshots/dashboard-tour.gif" alt="Shimmer Gateway dashboard tour" width="1200">
</p>

**Shim**mer is an LLM gateway for easily switching between providers. I built it
because many coding harnesses don’t let you save multiple configurations for
the same provider. They can also make it difficult to see what’s actually being
sent to and received from an LLM.

Shimmer includes a few tools to make this easier:

- browse chat sessions and inspect raw API requests and responses for each message
- use the MCP inspect tool to let your agent look back at previous LLM requests and responses
- track cost/token usage over time

Cost estimates are approximate and may vary between providers.

## Setup
[![CI](https://img.shields.io/github/actions/workflow/status/amirzamli/shimmer-llmgateway/ci.yml?branch=main&label=CI)](https://github.com/amirzamli/shimmer-llmgateway/actions/workflows/ci.yml)  [![Go 1.26](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev) [![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)


Clone the repository:

```bash
git clone https://github.com/amirzamli/shimmer-llmgateway.git
cd shimmer-llmgateway
```

Provider API keys can be supplied in either of two ways: set the corresponding
environment variable, or add the key through the dashboard. You do not need to
set any provider environment variables to get started—skip this step and add
your provider keys from the dashboard after launching the gateway.

If you prefer environment variables, the built-in providers and their variable
names are summarized in [`gateway.toml.example`](gateway.toml.example). For
custom or overridden providers, use the `api_key_env` field. If you add the key
through the dashboard instead, you do not need to set or look up an environment
variable. For example:

```bash
export MISTRAL_API_KEY="your-provider-key"
# Optional, but recommended if you use UI-managed provider keys:
export SHIMMER_MASTER_KEY="$(openssl rand -base64 32)"
```

`SHIMMER_MASTER_KEY` is optional. If it is omitted, the gateway generates a key
on first run; set and keep the displayed key if you want UI-managed keys to
survive restarts.

### Running the gateway (without `just`)

```bash
cp gateway.toml.example gateway.toml
go run ./cmd/gateway -config gateway.toml
```

### Running the gateway with `just`
Utilizing the justfile is optional, it's my personal preference for collecting useful commands in one place.

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
  -d '{"model":"mistral/mistral-large-latest","messages":[{"role":"user","content":"Hello"}]}'
```

Configure providers and instances in `gateway.toml` or from the dashboard.
[`gateway.toml.example`](gateway.toml.example) contains a minimal configuration
you can use as a starting point.

### OpenCode Harness

The OpenCode harness does not automatically detect models exposed by the gateway. When
using Shimmer with OpenCode, the
[opencode-models-discovery](https://github.com/yuhp/opencode-models-discovery)
plugin is recommended for discovering available models.

## LLM API providers

Built-in templates are available for `openai`, `anthropic`, `ollama`, `groq`,
`vllm`, `lite_llm`, `openrouter`, `deepseek`, `gemini`, `mistral`, `kimi`,
`zai`, `opencode_zen`, `opencode_go`, and `commandcode`.

For your own endpoint, use `custom_openai` for an OpenAI-compatible Chat
Completions API or `custom_anthropic` for an Anthropic Messages API.

## ChatGPT Plus/Pro OAuth

The `chatgpt` template connects a ChatGPT Plus/Pro account with device-code
OAuth. Add an instance from the dashboard and choose **Sign in with code**; no
API key is required. Credentials are stored encrypted and refreshed
automatically. Available models depend on the account and provider rollout.

## API compatibility

Clients use Shimmer's OpenAI-compatible `/v1` interface. Provider templates can
target OpenAI Chat Completions, OpenAI Responses, or Anthropic Messages APIs;
Shimmer translates between these formats where needed. Add instances from the
dashboard or configure them in `gateway.toml`; see
[`gateway.toml.example`](gateway.toml.example) for the configuration shape.

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
