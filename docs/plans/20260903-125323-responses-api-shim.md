# Plan: OpenAI Responses API shim (`POST /v1/responses`) over the Chat Completions pipeline

## Goal
Codex CLI 0.153.0 can use the gateway via `POST /v1/responses` (streamed text + tool calls + multi-turn): requests are translated to Chat Completions and run through the existing shared pipeline (routing, plugins, anthropic composition, capture); replies are translated back to Responses shape (non-stream JSON object or typed SSE event stream) for the client only — the store keeps chat-normalized shapes and records `endpoint = "/v1/responses"`. Stateless-only.

## Assumptions
- Translation-only for ALL upstreams; the existing `buildUpstream` anthropic composition (`translateOpenAIToAnthropic`, server.go:428-435) applies unchanged to the chat body we produce.
- Streaming usage arrives only when the upstream sends it (assembler already folds the final usage chunk, sse.go:105-107; gateway injects no `stream_options` today). `response.completed` carries usage when available; we do NOT start injecting `stream_options.include_usage` in this milestone.
- Upstream reasoning deltas (`delta.reasoning_content` / `delta.reasoning`, field names already recognized at server.go:1062-1074) are surfaced to Codex via the `reasoning` output item + `response.reasoning_summary_text.delta/.done` events (Codex's long-supported thinking display path). If E2E shows Codex ignores them, switching the event name is a one-line change; dropping them is the documented fallback, not the default.
- `retry_empty` held-mode streaming is not implemented for this surface. The live acceptance path (priority-1 z.ai alias) runs `plugins = []`, so the held branch is unreachable in the Codex E2E gate; the README records the limitation.
- Upstream error responses keep the existing `{"error": {...}}` envelope (same shape both surfaces; `translateUpstreamBody` + `passthrough` reused as-is).
- `GET /v1/models` already satisfies Codex startup; no changes there.
- Encrypted reasoning input items are dropped on request translation; `include`, `store`, `metadata`, `reasoning.summary` are accepted and ignored (settled prior decisions).

## Open Questions
- None. All product/architecture decisions are settled by the four confirmed constraints and the choices recorded under Assumptions.

## Excluded Scope
- Statefulness: `store`, `previous_response_id`, response retrieval (`GET /v1/responses/{id}` → 404), Conversations API.
- Native passthrough to upstream `/v1/responses`.
- Built-in/native tools (`local_shell`, `web_search`, `custom`, computer use, MCP tool types) → clear Responses-shaped 400.
- Image/file input content parts, WebSocket transport, `background` mode, `previous_response_id` chaining.
- Store schema changes; changes to plugin implementations; retry_empty support on this surface.
- Injecting `stream_options.include_usage` upstream.

## Phase 1: Translation core, endpoint, stateless guards, non-stream path, capture
**Objective**: `POST /v1/responses` works end-to-end in non-stream mode against any existing upstream (openai- or anthropic-style): a curl request returns a correct `response` object, tool-call turns translate correctly, and every request is captured with `endpoint = "/v1/responses"`, as-received `request_json`, and chat-shaped `response_json`.

**Files / Areas**:
- Create `internal/gateway/responses.go` — request/response translation + guards + Responses error writer.
- Modify `internal/gateway/server.go` — route registration (`POST /v1/responses`, `GET /v1/responses/{id}` in `Handler()`, adjacent to :152), `handleResponses` (parse minimal `{model, stream}` → guards → `cfg.Resolve` → `route` → session normalize → requestID → `translateResponsesToChat` → dispatch into the existing branches), and a minimal client-shaping seam threaded through `handleNonStream`/`handleStream`: per-request `{endpoint string, requestJSON []byte (capture body, as-received), client shaper}` where the chat surface passes identity (no behavior change) and the Responses surface converts 2xx bodies via `chatCompletionToResponse` for the client while capture keeps the chat body. Reject the `retryEmptyActive` branch for this surface (normal live/buffered path only).
- Tests: `internal/gateway/responses_test.go`; extend `server_test.go` patterns (fake upstreams, capture-record assertions — cf. existing endpoint assertion at server_test.go:433).

**Key design decisions**:
- `translateResponsesToChat(body) ([]byte, error)`: `instructions` → prepended `system` message; input items: `message` (roles user/assistant/system/developer → chat roles; text content parts joined; `output_text` accepted from assistant turns) → chat messages; `function_call` → assistant message with `tool_calls` (synthesized `call_N` id when upstream history omits one); `function_call_output` → `role:"tool"` message (`tool_call_id = call_id`); `reasoning` items → dropped (prior decision). `tools[].type == "function"` → chat `{"type":"function","function":{name,description,parameters,strict}}`; any other tool type → 400. `tool_choice` object form `{type:"function",name}` → chat nested form; `parallel_tool_calls`, `temperature`, `top_p` passthrough; `max_output_tokens` → `max_tokens`; `text.format` → `response_format`; `reasoning.effort` → `reasoning_effort`; `store`/`include`/`metadata`/`reasoning.summary` accepted-and-ignored. Unknown input item types / non-text content parts → 400.
- Guards before upstream: `previous_response_id` non-empty → 400 (`type: "invalid_request_error"`, param `previous_response_id`, message stating stateless-only). Errors on this surface use the OpenAI envelope via a dedicated `writeResponsesError` (the existing `writeError` §4.2 shape stays for the chat surface).
- `chatCompletionToResponse(chatBody) ([]byte)`: fresh `resp_` id, `object: "response"`, `created_at`, `model`; status from finish_reason (`stop`/`tool_calls`/empty → `completed`, `length` → `incomplete` + `incomplete_details:{reason:"max_output_tokens"}`); output items: `reasoning` item when `message.reasoning_content` present, `message` item with `content:[{type:"output_text",text,annotations:[]}]` (content null → empty text), one `function_call` item per tool_call (`call_id` and item `id` reuse the chat call id); usage mapped `{prompt_tokens→input_tokens, completion_tokens→output_tokens, total_tokens}` with `completion_tokens_details.reasoning_tokens` → `output_tokens_details.reasoning_tokens` when present; error/incomplete_details per status.
- Capture invariants (constraint 3): `RequestJSON` = as-received Responses bytes (via the seam's `requestJSON`, not the translated body); `ResponseJSON`/`ResponseFilteredJSON` = chat shapes; `Endpoint = "/v1/responses"`. No store changes; `extractToolCalls` keeps working.
- Upstream non-2xx / transport errors: existing `translateUpstreamBody` + `passthrough` + error-record paths reused (envelope already correct for both surfaces).

**Tasks**:
1. Add `responses.go`: request structs, `translateResponsesToChat`, `chatCompletionToResponse`, guards, `writeResponsesError`.
2. Register routes; add `handleResponses`; thread the endpoint/requestJSON/shaper seam through the non-stream path.
3. Unit tests: translation round-trips (string input, item array with all supported item types, reasoning-drop, tools/tool_choice/text.format/max_output_tokens mapping), guard 400s (previous_response_id, non-function tools, image parts, unknown items), `chatCompletionToResponse` status/usage/tool-call mapping; httptest non-stream E2E against a fake chat upstream and a fake anthropic upstream: assert Responses JSON to the client, capture record fields (endpoint, request_json raw, response_json chat-shaped).
4. Run `go build ./...`, `go vet ./...`, `go test ./...`.

**Validation**: `go test ./...` green; non-stream curl against a resolved alias returns a `response` object with correct output items, usage, and status; capture UI/§8 export shows `endpoint = /v1/responses` with tool calls extracted.

## Phase 2: SSE event-stream translation, reasoning/usage handling, docs
**Objective**: `stream: true` on `POST /v1/responses` yields the typed Responses SSE event sequence Codex requires — live per-delta when no response plugins run, one translated burst when they do — with reasoning deltas surfaced and usage carried into `response.completed` when the upstream provides it.

**Files / Areas**:
- Extend `internal/gateway/responses.go` — `responsesStreamState` (chunk→events translator) + `responsesEmitter` implementing the existing `Emitter` interface (emit.go:22-29).
- Modify `internal/gateway/server.go` — the stream branch constructs the Responses emitter via the Phase 1 seam (chat surface keeps `emitterFactory` output); capture block unchanged (chat-shaped `out.reassembled`).
- Modify `README.md` — endpoint list + stateless/limitation notes (previous_response_id, retrieval, retry_empty, native-tool 400s).

**Key design decisions**:
- One stateful translator `responsesStreamState.translate(chatPayload []byte) []responseEvent`, mirroring the `anthropicStreamState.translate` precedent (anthropic.go:614): consumes chat chunk payloads, emits zero or more typed events with a monotonic `sequence_number` starting at 0.
- Emission order: `response.created` (status `in_progress`, empty output) + `response.in_progress` on first data chunk; per output item in first-appearance order (reasoning → message → function_calls): `response.output_item.added`, then message: `response.content_part.added` → `response.output_text.delta`* → `response.output_text.done` → `response.content_part.done`; reasoning: `response.reasoning_summary_text.delta`* → `.done`; function_call: `response.function_call_arguments.delta`* → `.done`; each item closed with `response.output_item.done`; finally `response.completed` embedding the final response object (full `output[]`, mapped usage, status/incomplete_details). No trailing `[DONE]` (chat `[DONE]` is swallowed).
- Emitter wiring (mirrors live/buffered modes, emit.go:14-29): live mode — `Write(line)` extracts the SSE payload (`ssePayload`), translates, writes `event: <type>\ndata: {...}\n\n` and flushes per event; buffered mode (response plugins configured) — `Write` holds, `Done()` runs the plugin filter on `source()` (reassembled chat body), stores the filtered CHAT body as `emitted` (so `ResponseFilteredJSON` stays chat-shaped per constraint 3), converts it once via `chatCompletionToResponse`, and emits the full event sequence as a burst. Finalization (item `.done` + `response.completed`) happens in `Done()` — called after `readStream` returns (server.go:822), so `asm.result()` (finish_reason, usage) is safe to consult, exactly like `bufferedEmitter`.
- Item state for live mode (item ids `msg_`/`fc_`, output indices, accumulated text/args) is the translator's own; the assembler is untouched and keeps not folding `reasoning_content` (holdStreamForward semantics unaffected).
- Upstream mid-stream `{"error": ...}` payload → `response.failed` (with `response.error`) + `error` event; translator no-ops subsequent chunks; capture keeps the existing `streamErr` path.
- Client disconnect / truncation: inherited from `readStream` unchanged (capture truncated, no fabricated `response.completed`).

**Tasks**:
1. Implement `responsesStreamState` + `responsesEmitter` (live + buffered); wire into the stream branch via the seam.
2. Unit tests: chunk-sequence → expected ordered events (seq numbers, item/content indices, `[DONE]` swallowed, `response.completed` usage/finish mapping, reasoning_content → reasoning item events, tool-call arg deltas with interleaved indices, error payload → `response.failed`); httptest streaming E2E: fake chat SSE upstream → assert event order and final usage; buffered-mode burst equivalence (with a response plugin configured) including chat-shaped `ResponseFilteredJSON`; non-2xx-before-headers and disconnect behaviors unchanged.
3. README update; run `go build ./...`, `go vet ./...`, `go test ./...`.

**Validation**: `go test ./...` green; streamed curl shows the ordered event sequence with deltas; orchestrator runs the Codex CLI 0.153.0 E2E gate: multi-turn conversation with streamed text and a tool call against the priority-1 z.ai alias.

## Outcome & post-plan amendments (2026-09-03, post-implementation)

All success criteria met; Codex CLI 0.153.0 E2E gate passed live (streamed text + tool calls + multi-turn against the priority-1 z.ai alias). Three live-evidence amendments supersede plan text above:

1. **Unknown tool types are skipped, not rejected** (plan Phase 1 said 400): Codex 0.153 sends `namespace`/`web_search` tool types alongside function tools; rejecting aborted every Codex turn. Non-function tools are now skipped with a `responses_unsupported_tool_skipped` warn log.
2. **`developer` role maps to chat `system`** (plan said roles pass through): Codex sends `developer`-role items; z.ai/GLM rejects them (error 1214 "Incorrect role information").
3. **Session grouping via `prompt_cache_key`**: Codex sends no `X-Session-Id`, so each turn captured as an orphan session. When the header is absent, the session id is derived as `responses-<prompt_cache_key>`; explicit header still wins; fresh UUID remains the final fallback.

Also fixed live: `translateResponsesToChat` initially never set `stream` on the upstream body (streamed Responses requests hit upstream non-streaming) — now propagated.

- [x] `POST /v1/responses` serves non-stream and streamed requests (text + function calling + multi-turn) for openai-style and anthropic-style templates; no upstream sees anything but the existing Chat/Messages shapes.
- [x] Stateless contract enforced: `previous_response_id` → clear Responses-shaped 400; unsupported tool types skipped (amended); `GET /v1/responses/{id}` → 404; `store`/`include` accepted and ignored.
- [x] Every /v1/responses request captured with `endpoint = "/v1/responses"`, as-received `request_json`, chat-normalized `response_json`; tool-call extraction, verdicts, pricing, §8 export unchanged and working.
- [x] Chat Completions surface byte-for-byte unchanged (identity seam; existing tests untouched and green).
- [x] `go build ./...`, `go vet ./...`, `go test ./...` green.
- [x] Codex CLI 0.153.0 E2E gate passes (orchestrator-run): streamed text + tool calls + multi-turn, one session per conversation.

## Success Criteria (original)
- [ ] `POST /v1/responses` serves non-stream and streamed requests (text + function calling + multi-turn) for openai-style and anthropic-style templates; no upstream sees anything but the existing Chat/Messages shapes.
- [ ] Stateless contract enforced: `previous_response_id` → clear Responses-shaped 400; non-function tools → 400; `GET /v1/responses/{id}` → 404; `store`/`include` accepted and ignored.
- [ ] Every /v1/responses request captured with `endpoint = "/v1/responses"`, as-received `request_json`, chat-normalized `response_json`; tool-call extraction, verdicts, pricing, §8 export unchanged and working.
- [ ] Chat Completions surface byte-for-byte unchanged (identity seam; existing tests untouched and green).
- [ ] `go build ./...`, `go vet ./...`, `go test ./...` green.
- [ ] Codex CLI 0.153.0 E2E gate passes (orchestrator-run): streamed text + tool calls + multi-turn.
