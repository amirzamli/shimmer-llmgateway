# Shimmer LLM Gateway — capture-only LLM gateway + inspection MCP server (Go)
# Common commands. Run `just --list` for an overview.

# Make `go`/`gofmt` resolvable in recipes (go lives outside the default PATH
# in some environments); still portable where PATH already has go.
export PATH := env_var_or_default('PATH', '') + ':/usr/local/go/bin'

bin_dir := 'bin'
config  := 'gateway.toml'

# fmt + vet + build + test
default: fmt vet build test

# gofmt check — fails if any file needs formatting (list the offenders)
fmt:
    gofmt -l . | (! grep .)

# format all Go source in place
fix:
    gofmt -w .

# go vet
vet:
    go vet ./...

# static binaries (CGO disabled, modernc.org/sqlite) into bin/
build:
    mkdir -p {{bin_dir}}
    CGO_ENABLED=0 go build -o {{bin_dir}}/gateway ./cmd/gateway
    CGO_ENABLED=0 go build -o {{bin_dir}}/inspect-mcp ./cmd/inspect-mcp

# run all tests
test:
    go test ./...

# run tests with the race detector (requires cgo)
race:
    go test -race ./...

# regenerate internal/pricing/models.json from models.dev (review the diff,
# then restart the gateway — the table is loaded from disk at startup)
update-prices:
    go run ./cmd/genpricing

# starter {{config}} template (spec §4.2 example, keyless)
starter_config := '''
listen_addrs = ["127.0.0.1:8787", "100.64.0.1:8787"]   # one socket per address
store  = "gateway.db"
retention_days = 30

[settings]
default_alias = "openai"
request_plugins = []
response_plugins = []
log_payloads = false

[providers.openai]
base_url = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"
models = ["gpt-4o", "gpt-4o-mini"]

[providers.ollama]
base_url = "http://localhost:11434/v1"
api_key_env = ""
models = ["llama3.1"]

# [providers.opencode_go] mirrors the built-in template: Console Go's full
# catalog defaults to the openai chat style (/chat/completions), needs
# the x-opencode-session header the gateway synthesises from the request's
# session id (session_header below), and expect the coding-agent identity
# headers (identity_headers). model_styles overrides the models that speak the
# Responses API (/responses — muse contributors) or the Anthropic Messages API
# (/messages — minimax/qwen); no plugin seed.
[providers.opencode_go]
base_url = "https://opencode.ai/zen/go/v1"
api_key_env = "OPENCODE_API_KEY"
models = [
  "grok-4.6", "gpt-5.6-luna",
  "glm-5.3-flash", "glm-5.3", "glm-5.2", "glm-5.1",
  "kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "longcat-2.0",
  "deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash",
  "deepseek-v4-flash-vision-exp",
  "mimo-v2.5", "mimo-v2.5-pro",
  "minimax-m3", "minimax-m2.7", "minimax-m2.5",
  "muse-spark-1.3-contributor", "muse-spark-1.2-contributor",
  "qwen3.8-max", "qwen3.8-flash", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus",
  "hy4-preview", "hy3",
]
session_header = "x-opencode-session"
identity_headers = { "X-Opencode-Client" = "cli", "X-Opencode-Project" = "global" }
model_styles = {
  "grok-4.6" = "responses",
  "gpt-5.6-luna" = "responses",
  "muse-spark-1.3-contributor" = "responses",
  "muse-spark-1.2-contributor" = "responses",
  "minimax-m3" = "anthropic",
  "minimax-m2.7" = "anthropic",
  "minimax-m2.5" = "anthropic",
  "qwen3.8-max" = "anthropic",
  "qwen3.8-flash" = "anthropic",
  "qwen3.7-max" = "anthropic",
  "qwen3.7-plus" = "anthropic",
  "qwen3.6-plus" = "anthropic",
}

[[instances]]
alias = "openai"
template = "openai"
api_key_env = "OPENAI_API_KEY"
'''

# write a starter {{config}} from the spec example if it does not exist
init:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -f {{config}} ]]; then
        echo "{{config}} already exists; leaving it untouched"
        exit 0
    fi
    printf '%s' '{{starter_config}}' > {{config}}
    echo "wrote {{config}} (set OPENAI_API_KEY or use the UI to add a key)"

# run the gateway (reads {{config}}; listen/store come from the config)
run:
    #!/usr/bin/env bash
    set -a
    source .env
    set +a
    go run ./cmd/gateway -config {{config}}

# per-worktree isolated test gateway: random free 127.0.0.1 port, isolated
# .run-test/<branch>/ store + generated config, mock provider, seeded dummy
# conversations on first run. UI at the printed URL; Ctrl-C stops it.
run-test *args:
    bash scripts/run-test.sh {{args}}

# re-send the dummy conversations (run-test gateway must be running)
run-test-seed:
    bash scripts/run-test.sh seed

# remove this worktree's run-test state dir (.run-test/<branch>)
run-test-clean:
    bash scripts/run-test.sh clean 

# run the MCP inspection server over stdio against the store
mcp db='gateway.db':
    go run ./cmd/inspect-mcp -db {{db}}

# run the MCP inspection server over streamable-http
mcp-http db='gateway.db' addr='127.0.0.1:9876':
    go run ./cmd/inspect-mcp -db {{db}} -http {{addr}}

# remove build artifacts
clean:
    rm -rf {{bin_dir}}
