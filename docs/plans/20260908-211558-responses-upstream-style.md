# Plan: Responses-API upstream style (`style = "responses"`) for opencode_go

## Goal
Add a third upstream protocol style `"responses"` so the built-in `opencode_go` template speaks the OpenAI Responses API upstream (`POST <base>/responses`, stream + non-stream) instead of `/chat/completions`. The gateway's internal canonical shape stays `chat.completion`; the Responses protocol is translated at the upstream boundary only, so the client surface — including the existing `/v1/responses` client shim — needs no change. This is the durable fix so muse contributor models (which 500 on `/chat/completions` but work on `/responses`) route correctly through the gateway, while keeping opencode_go traffic typical coding-agent traffic (Phase 1 UA + identity headers preserved).

## Assumptions
- The upstream boundary translation is the **inverse** of the existing `/v1/responses` client shim (responses.go / responses_stream.go). New request translator `translateChatToResponses` inverts `translateResponsesToChat`; new response translator `translateResponsesToChatCompletion` inverts `chatCompletionToResponse`; the new stream reader `readResponsesStream` inverts the client-direction `responsesStreamState`. Reuse the existing type structs (`responsesRequest`, `responsesItem`, `responsesTool`, `responsesContentPart`, `responsesUsageFromChat` mapping) and the `anthropic.go` structural template (`translateOpenAIToAnthropic` / `translateAnthropicToOpenAI` / `translateAnthropicError` / `anthropicStreamState.translate` / `readAnthropicStream`). New upstream symbols live in a new file `responses_upstream.go` to avoid clashing with the client-direction `responsesStreamState`/`responsesEmitter` names.
- The canonical internal shape is chat throughout: the whole pipeline (plugins, assembler, `parseCompletionMeta`, cost, capture) sees OpenAI-shaped data. Responses payloads are translated **before** the assembler/`translateUpstreamBody`, exactly as Anthropic SSE is translated before reassembly.
- Zen/go behavior is treated as a set of verify-during-tests assumptions with graceful-degradation fallbacks (mirror `translateAnthropicError`'s "return unchanged if unparseable"):
  - **SSE termination**: the Responses stream ends at `response.completed` (no `[DONE]`); `io.EOF` is the accepted fallback end. A `response.completed` (or EOF) marks `cleanEnd` (not truncated). A stream that dies mid-flight without `response.completed` records `truncated` — inherited from `readStream`/`readAnthropicStream` semantics.
  - **Error envelope**: upstream non-2xx and mid-stream errors use the OpenAI `{"error":{...}}` envelope, already the gateway's canonical error shape → passthrough unchanged (no dedicated `translateResponsesError` needed; `translateUpstreamBody` for `responses` style only maps 2xx bodies and leaves errors/`[DONE]`/unparseable bodies unchanged). A mid-stream `response.failed` / `{"error":...}` event maps to an OpenAI error chunk for the client.
  - **Reasoning item form**: zen may emit the reasoning item as `summary_text` (mapped to `reasoning_content`) or as `encrypted_content` (only present when the request sets `include:["reasoning.encrypted_content"]`). We translate `summary_text` directly; for `encrypted_content` we surface it verbatim as `reasoning_content` if decryptable-in-shape, else degrade gracefully (empty reasoning, content still delivered) rather than failing the turn.
  - **`max_output_tokens` handling**: the opencode_go request sets `max_output_tokens: 32000`. `translateChatToResponses` maps chat `max_tokens` → `max_output_tokens` and **does not inject a default** (preserve whatever the client/upstream flow provides; only default when the upstream requires it and verification shows a missing-value 400 — recorded as an amendment if so).
  - **`prompt_cache_key` plumbing**: the real opencode body carries `prompt_cache_key: "ses_..."`. For the upstream style we derive `prompt_cache_key` from the request's effective session id (the same id sent via `SessionHeader` `x-opencode-session`) so upstream prompt caching and the gateway session grouping stay consistent. This does not change the existing `/v1/responses` client surface (whose `prompt_cache_key` session derivation at server.go is untouched).
- `reasoning.effort` → chat `reasoning_effort` and back; `reasoning.summary` accepted-and-ignored upstream. Tool `tool_choice` round-trips the nested `{type:"function",function:{name}}` form. `stream:true` is always set on the upstream body for streamed requests (the mirror of the Phase-2 client-shim fix that `stream` must be propagated).
- Phase 1 (UA forwarding + `IdentityHeaders` + `SessionHeader`) and `session_header` behavior are preserved: the responses branch keeps the same UA/identity/session-header injection code path in `buildUpstream`.

## Open Questions
- None blocking. Zen behavior specifics (SSE termination, error envelope, reasoning item form, `max_output_tokens` default requirement, `prompt_cache_key` acceptance) are recorded as assumptions above to be confirmed against a live muse provider during implementation/testing; any divergence becomes a documented amendment, not a plan blocker.

## Excluded Scope
- No changes to the client surface, `/v1/responses` client shim, or the `/v1/responses` session/`prompt_cache_key` derivation.
- No plugin-system, UI, or pricing-table changes.
- The separate "restore `opencode_go_ali` account" concern (out of scope; account permanently blocked).
- No speculative abstraction of the style dispatch beyond the concrete `"responses"` branch.
- No non-Responses-related cleanups or refactors of the existing client-direction translators.

## Phase 2a: Non-stream Responses upstream style (config + request/response translation; PURELY ADDITIVE)
**Objective**: The responses upstream style is accepted by config/validation/API and the request+response translators exist and are tested, with a non-stream E2E against a fake `/responses` upstream. **This phase does NOT re-point the built-in `opencode_go` template** — its Style switch lands in 2b together with the stream reader, so no phase ever leaves `opencode_go` on the responses style without streaming support (opencode traffic is streaming by default; pointing it early would break every stream until 2b).

**Files / Areas**:
- `internal/config/config.go` — add `StyleResponses = "responses"` constant (beside `StyleOpenAI`/`StyleAnthropic` :712-713); extend validation (:839-841) to accept `"responses"`.
- `internal/config/config_test.go` — extend `TestTemplateStyleValidation` (:1948) to accept `"responses"`.
- `internal/api/templates.go` — extend the `POST /api/templates` style whitelist (:82-85) to accept `"responses"` (consistency with config.Validate; without this, users could not create a custom responses-style template via the API while the config file could). Add/extend an API test asserting a `"responses"` custom template is accepted.
- `internal/gateway/responses_upstream.go` (new) — `translateChatToResponses` and `translateResponsesToChatCompletion`.
- `internal/gateway/server.go` — extend `buildUpstream` (:635, URL select :655-660, translation :646-653) with a `responses` branch; extend `translateUpstreamBody` (:782) with a `responses` 2xx branch.
- `internal/gateway/responses_upstream_test.go` (new) — translation round-trip + non-stream E2E (mirror `responses_test.go` `newResponsesGateway` harness and `TestResponsesNonStreamAnthropicUpstream` :882).

**Tasks**:
1. Config: add `StyleResponses`, extend validation (do NOT yet set it on the built-in `opencode_go`).
2. API: extend the `POST /api/templates` style whitelist to `"responses"` + test.
3. `translateChatToResponses(chatBody) ([]byte, error)`: system message → `instructions` (first system) / system input item; user/assistant → `message` input items (content text); assistant `tool_calls` → `function_call` input items (`call_id` = chat call id, name, arguments); `role:"tool"` → `function_call_output` (`call_id` = `tool_call_id`); `tools[].function` → flat `responsesTool`; nested `tool_choice` → responses `{type,name}` form; **both `max_tokens` and `max_completion_tokens`** → `max_output_tokens` (mirror `maxTokensOf`); `reasoning_effort` → `reasoning.effort`; `response_format` → `text.format`; `stream:true`; `prompt_cache_key` from effective session id.
4. `translateResponsesToChatCompletion(respBody) []byte`: `message` output item → assistant message (content text → `content`); `reasoning` item (`summary_text`/`encrypted_content`) → `reasoning_content`; `function_call` item → `tool_calls`; `usage` input_tokens→prompt_tokens / output_tokens→completion_tokens (+ `output_tokens_details.reasoning_tokens` → `completion_tokens_details.reasoning_tokens`); status → `finish_reason` (`completed`→`stop`, `incomplete`→`length`); unparseable → return body unchanged.
5. `server.go`: `buildUpstream` — `responses` branch posts to `<base>/responses`, translates body via `translateChatToResponses`, Bearer auth (openai-style), no anthropic headers, retains UA/identity/session-header injection; `translateUpstreamBody` — `responses` 2xx → `translateResponsesToChatCompletion`, errors/unparseable passthrough.
6. Tests: config validation; request/response translation round-trips (messages, tool_calls, tool role, tools, tool_choice, max_tokens + max_completion_tokens → max_output_tokens, reasoning_effort, response_format, usage, status, reasoning item forms, unparseable passthrough); httptest non-stream E2E: chat client → fake `/responses` upstream → assert correct `chat.completion` to the client and chat-shaped capture; also a `/v1/responses` client-surface → responses upstream non-stream round trip (Codex-style client inherits the style automatically).
7. `go build ./...`, `go vet ./...`, `go test ./...`.

**Validation**: `go test ./...` green; non-stream E2E against a fake `/responses` upstream returns a correct `chat.completion`; Phase 1 UA/identity/session headers still forwarded. The built-in `opencode_go` template still posts `/chat/completions` in this phase (unchanged behavior until 2b).

## Phase 2b: Responses upstream SSE streaming + opencode_go wiring + spec docs
**Objective**: `stream:true` chat requests routed to a `responses`-style template stream the Responses SSE upstream and translate each event into `chat.completion.chunk` lines (live per-delta, or one burst when response plugins run), with reasoning surfaced and usage/finish folded into the final chunk. This phase re-points the built-in `opencode_go` template to the responses style (now that both non-stream and stream paths exist) and documents the new style in the spec.

**Files / Areas**:
- `internal/gateway/responses_upstream.go` — `responsesUpstreamStreamState` + `readResponsesStream`.
- `internal/gateway/server.go` — stream branch dispatch (:983-987) adds the `responses` branch calling `readResponsesStream`; `translateUpstreamBody` error path unchanged.
- `internal/config/config.go` — set `Style: StyleResponses` on the built-in `opencode_go` template (:646-658).
- `internal/config/config_test.go` — assert the built-in `opencode_go` template now has `Style == StyleResponses` (e.g. extend `TestBuiltinTemplatesPresent`), alongside the 2a `TestTemplateStyleValidation` acceptance.
- `internal/gateway/responses_upstream_test.go` — stream translation + streaming E2E (mirror `anthropic_test.go` `TestAnthropicStreamTranslation` :347).
- `docs/gateway-spec.md` — document `style = "responses"` in the §4.2 style paragraph (:128-136) and the opencode_go template entry (:219-220).

**Tasks**:
1. `responsesUpstreamStreamState.translate(payload) [][]byte`, mirroring `anthropicStreamState.translate`: `response.created` → seed chunk id/model; `response.output_item.added` (message/function_call) → role/tool_call-declaration chunk; `response.output_text.delta` → content delta chunk; `response.reasoning_summary_text.delta` / reasoning `encrypted_content` delta → `reasoning_content` chunk; `response.function_call_arguments.delta` → `tool_calls[].function.arguments` delta chunk; `response.completed` → finish_reason + usage chunk (and marks clean end); `response.failed`/`{"error":...}` → OpenAI error chunk; `response.created`/`output_item.done`/`content_part.*`/unknown events → no-op.
2. `readResponsesStream(ctx, resp, asm, forward)` mirroring `readAnthropicStream` (:837): consume Responses SSE lines, translate to `data: <chat chunk>\n\n` lines forwarded to the client AND fed to the assembler; cleanEnd on `response.completed` or `io.EOF`; disconnect/truncation semantics inherited.
3. `server.go`: dispatch `readResponsesStream` when `rt.template.Style == config.StyleResponses`.
4. Config: set `Style: StyleResponses` on the built-in `opencode_go` template + test assertion.
5. Tests: chunk→`chat.completion.chunk` sequence (message/reasoning/tool deltas, interleaved tool indexes, `response.completed` usage+finish, error event → error chunk, clean-end on `response.completed`/EOF); httptest streaming E2E: fake `/responses` SSE upstream → assert ordered chat chunks + final usage; buffered-mode (response plugin) burst equivalence; non-2xx-before-headers and disconnect behaviors unchanged. Streaming captures fold only content/tool_calls/usage/finish into `ResponseJSON` (reasoning appears client-visible but not in capture — same as the anthropic streaming path; assert accordingly).
6. Spec docs; `go build ./...`, `go vet ./...`, `go test ./...`.

**Validation**: `go test ./...` green; streamed curl to an `opencode_go` alias shows chat `completion.chunk` SSE with reasoning and a final usage chunk; orchestrator runs a live muse-contributor-model turn (the previously-500ing path) through the gateway and confirms it completes; spec §4.2 documents the new style.

## Success Criteria
- [ ] `style = "responses"` is a valid, documented upstream style; the built-in `opencode_go` template uses it, so its instances post `POST <base>/responses` (not `/chat/completions`).
- [ ] Chat client surface is unchanged; a chat request routed to a `responses`-style upstream works non-stream and streamed (text + reasoning + tool calls) with correct `chat.completion` / `chat.completion.chunk` output.
- [ ] The whole pipeline (plugins, assembler, `parseCompletionMeta`, cost, capture) still sees OpenAI-shaped data only; capture records chat shapes with no upstream ever seeing `/chat/completions`.
- [ ] Phase 1 UA forwarding + `IdentityHeaders` + `SessionHeader` behavior is preserved on the responses branch (no regression).
- [ ] `go build ./...`, `go vet ./...`, `go test ./...` green.
- [ ] Orchestrator live gate: a muse contributor model turn (previously 500ing on `/chat/completions`) completes through the gateway; any zen-behavior divergence (SSE termination, error envelope, reasoning item form, `max_output_tokens` default, `prompt_cache_key`) is recorded as a documented amendment, not silently degraded.
