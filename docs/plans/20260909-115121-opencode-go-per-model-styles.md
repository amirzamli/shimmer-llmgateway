# Plan: Per-model protocol routing for `opencode_go` (fix blanket `style = "responses"`)

## Handoff summary

This plan fixes a design flaw in the already-shipped Phase 2 Responses-API upstream work
(`docs/plans/20260908-211558-responses-upstream-style.md`, commits `6907bb6` + `d429f46` on
`main`). Phase 2 set the **whole** `opencode_go` built-in template to `style = "responses"`,
which is wrong: the official opencode.go usage list shows the provider serves **three
protocols** on one base URL, and the majority of models (including the user's configured
defaults `large`/`small` → `deepseek-v4-flash`) use `/chat/completions`, not `/responses`.

The gateway is currently running the new binary with the blanket responses style, so
**flash may be broken through the gateway right now**. This plan restores correct per-model
routing and keeps the muse contributor models (which genuinely require `/responses`) working.

## The official opencode.go model → protocol matrix (authoritative, from user)

All models share base `https://opencode.ai/zen/go/v1`. The `@ai-sdk/*` column is how the
real opencode client talks; the gateway's job is to speak that protocol upstream.

| Endpoint / protocol | Models |
|---|---|
| `/responses` (openai Responses, `@ai-sdk/openai`) | `grok-4.6`, `gpt-5.6-luna`, `muse-spark-1.3-contributor`, `muse-spark-1.2-contributor` |
| `/chat/completions` (openai-compatible, `@ai-sdk/openai-compatible`) | `glm-5.3-flash`, `glm-5.3`, `glm-5.2`, `glm-5.1`, `kimi-k3`, `kimi-k2.7-code`, `kimi-k2.6`, `longcat-2.0`, `deepseek-v4-pro`, `deepseek-v4-flash`, `deepseek-v4-flash-vision-exp`, `mimo-v2.5`, `mimo-v2.5-pro`, `hy4-preview`, `hy3`, `omen-alpha` |
| `/messages` (anthropic Messages, `@ai-sdk/anthropic`) | `minimax-m3`, `minimax-m2.7`, `minimax-m2.5`, `qwen3.8-max`, `qwen3.8-flash`, `qwen3.7-max`, `qwen3.7-plus`, `qwen3.6-plus` |

## Goal

`opencode_go` instances route **each resolved model** to the protocol the upstream expects:
`openai` (chat) default, `responses` for the 4 Responses models, `anthropic` for the 8
Messages models — while preserving Phase 1 (User-Agent + `X-Opencode-*` identity headers +
`session_header`) and the Phase 2 Responses translation machinery (used only by the 4
Responses models).

## Design decision (already made — option A)

Per-model **`model_styles` map on the template**, following the existing `ModelReasoningOptions`
precedent (a per-model template map). One provider, one key, one instance. The template's
`style` stays the default (`""`/`openai`); `model_styles` overrides per concrete model id.

```toml
[providers.opencode_go]
base_url = "https://opencode.ai/zen/go/v1"
api_key_env = "OPENCODE_API_KEY"
models = ["deepseek-v4-flash", "deepseek-v4-pro", "grok-4.6", "gpt-5.6-luna", "glm-5.3-flash", "glm-5.3", "glm-5.2", "glm-5.1", "kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "longcat-2.0", "deepseek-v4-flash-vision-exp", "mimo-v2.5", "mimo-v2.5-pro", "minimax-m3", "minimax-m2.7", "minimax-m2.5", "qwen3.8-max", "qwen3.8-flash", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "hy4-preview", "hy3", "omen-alpha", "muse-spark-1.3-contributor", "muse-spark-1.2-contributor"]
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
```

(Whether the built-in template's `models` list should grow to the full catalog is a
product choice for the executing session; the minimum fix is `model_styles` + default
`style` reverting to `openai`.)

## Current code state (verified line refs, as of commit `d429f46`)

- `internal/config/config.go`:
  - `StyleResponses = "responses"` const at :717 (landed in commit `6907bb6` via a mixed
    hunk — currently unused-standalone there, consumed by the feature commit).
  - `StyleOpenAI`/`StyleAnthropic`/`StyleResponses` constants :711-717.
  - `Template` struct :93-128 — has `Style` :102, `ModelReasoningOptions` :115 (the
    per-model precedent), `SessionHeader` :126, `IdentityHeaders` :133-143 (new field, Phase 1).
  - `Clone()` deep-copies `ModelReasoningOptions` and `IdentityHeaders` — `model_styles`
    needs the same deep copy.
  - Built-in `opencode_go` template :646-658 — currently sets `Style: StyleResponses`,
    `SessionHeader`, `IdentityHeaders`. **Must change**: default style back to openai
    (omit Style), add `ModelStyles` map.
  - `Validate` :840-852 — style whitelist already accepts `"responses"`; add validation
    for `model_styles` keys (must be non-empty, values must be a valid style, and the map
    must not overlap a template-level style conflict).
- `internal/gateway/server.go`:
  - `buildUpstream` :629-712 — `anthropic := rt.template.Style == config.StyleAnthropic`
    and `responses := rt.template.Style == config.StyleResponses` at :650-651; translation
    branch :652-663; URL select :665-670 (`/messages` / `/responses` / `/chat/completions`);
    auth :681-687; Phase 1 UA/identity/session-header block :700-712 (shared, must stay).
    **Change**: compute the effective style as `modelStyles[rt.model]` when set, else
    `rt.template.Style`; use that for anthropic/responses/url/translation branches.
  - `translateUpstreamBody` :782-805 — dispatches on the passed `style`; the callers pass
    `rt.template.Style` (:806, :901). **Change**: callers must pass the effective
    per-model style, or translateUpstreamBody takes the resolved style.
  - `handleStream` :1004-1010 — dispatch `readResponsesStream` when `rt.template.Style ==
    config.StyleResponses`. **Change**: use the effective per-model style.
- `internal/gateway/responses_upstream.go` (new file, Phase 2) — `translateChatToResponses`,
  `translateResponsesToChatCompletion`, `responsesUpstreamStreamState`/`readResponsesStream`.
  **Unchanged**; only its dispatch becomes per-model.
- `internal/gateway/responses_upstream_test.go` (new, Phase 2) — tests assume
  `opencode_go` is responses style; update where they assert the built-in template style.
- `internal/gateway/server_test.go` — Phase 1 UA/identity tests; add per-model style cases.
- `internal/config/config_test.go` — `TestBuiltinTemplatesPresent` asserts
  `opencode_go.Style == StyleResponses` (added in 2b); must be updated to assert the
  default is openai and `ModelStyles` covers the 12 override models.
- `docs/gateway-spec.md` §4.2 — documents `style = "responses"` on opencode_go; must be
  corrected to document `model_styles` and the per-model matrix.
- `gateway.toml.example` and `justfile` starter config — currently show
  `style = "responses"` on opencode_go; must be corrected to default openai + `model_styles`.

## Execution phases (fewest independently valuable steps)

### Phase A — Config + routing: per-model style resolution
1. `config.go`: add `ModelStyles map[string]string` to `Template` (toml
   `model_styles,omitempty`), doc comment citing §4.2; deep-copy in `Clone`; extend
   `Validate`: reject empty model keys, reject invalid style values (reuse the style
   whitelist incl. `responses`), reject CR/LF not needed (map keys are model ids).
2. Built-in `opencode_go`: remove `Style: StyleResponses` (defaults to openai), add the
   full `ModelStyles` map (12 override entries).
3. `server.go`: add a helper `effectiveStyle(tmpl, model) string` (or compute inline):
   `if s, ok := tmpl.ModelStyles[model]; ok { return s }; return tmpl.Style` (empty →
   openai). Use it in `buildUpstream` (translation + URL + auth decisions), in
   `translateUpstreamBody` callers, and in `handleStream` dispatch.
4. Tests: config validation + builtin assertions; per-model routing unit test (chat default,
   responses override, anthropic override) via `fakeProvider` harness; non-stream E2E for a
   chat-default model and an anthropic model on the opencode base.

### Phase B — Docs + config examples + full-suite validation
5. Correct `docs/gateway-spec.md` §4.2 (document `model_styles`, the matrix, default
   openai; remove the claim that opencode_go is responses-only), `gateway.toml.example`,
   `justfile` starter config.
6. `go build ./...`, `go vet ./...`, `go test ./...` green.

### Phase C — Live gate (needs explicit user go-ahead; sends real requests to opencode.ai)
7. Rebuild + restart the gateway (currently running the stale blanket-responses binary).
8. Update the user's `gateway.toml` `opencode-go` instance `models`/`model_aliases` to
   include the muse contributor models (and any other models the user wants routable).
9. One minimal flash turn through the gateway (chat path restored) and one minimal muse
   turn (responses path) — confirm both complete; resolve the 5 zen-behavior assumptions
   (SSE termination, error envelope, reasoning item form, `max_output_tokens` default,
   `prompt_cache_key`) against live output; record any divergence as a documented amendment.

## Validation criteria

- `opencode_go` default style is `openai` (`/chat/completions`) and `model_styles` routes
  the 12 override models correctly; muse still works; flash works again.
- Phase 1 UA/identity/session-header behavior preserved on all three protocols.
- Chat surface unchanged; capture/cost/pipeline sees chat shapes on every protocol.
- Full suite green; live gate (flash + muse) completes.

## Excluded scope

- No plugin-system, UI, pricing, or client-surface changes.
- No new models added to the built-in catalog beyond what's needed for `model_styles`
  routing correctness (product choice for the executing session).
- `opencode_go_ali` account remains permanently blocked (401) — out of scope.
- No speculative abstraction beyond the per-model style map.

## Open questions for the executing session

- Should the built-in `opencode_go` template's `models` list grow to the full catalog
  (28 models) so `model_styles` entries are routable by default, or stay minimal?
- For `/messages` (anthropic) models: confirm zen accepts `x-api-key` auth + the existing
  `translateOpenAIToAnthropic` output (the anthropic path already exists; per-model
  dispatch just needs to select it). Live gate should include one anthropic-model turn
  (e.g. `qwen3.6-plus` or `minimax-m2.5`) if the user wants those models usable.

## Handoff state (this session)

- `main` @ `d429f46` (2 commits ahead of origin, not pushed): `6907bb6` (pre-existing
  custom-endpoint work), `d429f46` (Phase 1 identification + Phase 2 responses upstream).
- Working tree clean. Gateway running the new binary (restarted Sep 8 23:58), `/healthz` ok,
  currently serving flash + muse via the blanket responses style (flash routing is the bug
  this plan fixes).
- Prior plan file `docs/plans/20260908-211558-responses-upstream-style.md` remains as the
  Phase 2 record; this plan supersedes its opencode_go style wiring.