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

# The built-in opencode_go template seeds plugins = ["retry_empty"]: the
# gateway writes it into each opencode_go instance's plugins line on
# write-back, so the default-on is explicit in the file. Instances with no
# plugins line have retrying OFF at runtime; explicit `plugins = []` disables
# it durably.
[providers.opencode_go]
base_url = "https://opencode.ai/zen/go/v1"
api_key_env = "OPENCODE_API_KEY"
models = ["kimi-k2", "deepseek-chat"]
plugins = ["retry_empty"]

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

# run the MCP inspection server over stdio against the store
mcp db='gateway.db':
    go run ./cmd/inspect-mcp -db {{db}}

# run the MCP inspection server over streamable-http
mcp-http db='gateway.db' addr='127.0.0.1:9876':
    go run ./cmd/inspect-mcp -db {{db}} -http {{addr}}

# remove build artifacts
clean:
    rm -rf {{bin_dir}}
