# ChatGPT Plus OAuth

## Goal

Add an OpenAI/ChatGPT account that supports the headless device-code OAuth
method implemented by OpenCode, while preserving the existing API-key provider
path and allowing callers to select a currently supported codex model.

## Phase 1: Pin The Contract And Credential Model

- Verify the installed OpenCode 1.18.30 implementation before coding. Record
  its exact device/token endpoints, client identifier, device redirect URI,
  PKCE requirements, token response fields, account-id extraction, upstream
  URL/path, required headers, and model mapping. Do not guess or copy secrets.
- Add provider metadata for a dedicated ChatGPT OAuth-capable OpenAI template;
  leave the existing `openai` API-key template and `APIKeyEnv` precedence
  unchanged.
- Extend `internal/secrets` with a versioned encrypted OAuth credential record
  per instance (`access_token`, `refresh_token`, `expires_at`, and the verified
  account identifier). Keep legacy alias-to-string API-key records readable,
  migrate atomically, retain mode `0600`, and never expose token material.
- Add unit tests for legacy migration, OAuth round trips, encryption/no
  plaintext, replacement, deletion, and concurrent access.

## Phase 2: Device Login And Dashboard Lifecycle

- Add provider-specific device start/status, status, and disconnect handlers
  under `internal/api`; wire them through the existing guarded admin surface.
  Keep device identifiers, user codes, and PKCE verifiers server-side where
  possible, short-lived, and bound to the target instance and lifecycle
  generation; require the loopback or explicitly configured listener policy
  for credential-bearing OAuth operations.
- Start the verified OpenCode device-code request, validate device responses,
  exchange the returned code, validate the token/account response, then persist
  it through the encrypted secrets store. Return only the intended device user
  code and masked status; tokens must not occur in URLs, JSON, logs, or HTML.
- Update `web/index.html` so adding the ChatGPT template offers device-code
  login, shows connected/disconnected/error state, and supports reconnect and
  disconnect without displaying secrets. Preserve existing key entry and
  instance rename/delete behavior, including moving/removing the OAuth record.
- Cover API tests for pending/expired device transactions, provider failure,
  status masking, and lifecycle cleanup.

## Phase 3: Refresh And Request Routing

- Add an OAuth-aware resolver used by `internal/gateway/server.go` and the
  provider model-fetch path. Refresh with an expiry skew, serialize concurrent
  refreshes per instance, atomically persist rotated tokens, and return a
  sanitized authentication error when refresh fails.
- Extend upstream construction only for the ChatGPT OAuth template: use the
  verified OpenCode-compatible endpoint, bearer/token format, account
  identifier header, identity headers, `originator: opencode`, and the model
  identifier supplied by the caller.
  Never let a client `Authorization` or account header override the stored
  credential identity. Keep API-key and keyless routing unchanged.
- Use configured models when the OAuth endpoint does not provide the existing
  OpenAI `/models` contract; do not add speculative discovery behavior.
- Add gateway tests for exact URL/body/model/header routing, streaming and
  non-streaming requests, refresh and refresh failure, header isolation, and
  regression coverage for API-key instances. Run `go test ./...` and a manual
  device-code flow against a non-production/test account or mocked OAuth
  server.

## Assumptions

- One ChatGPT OAuth credential belongs to one gateway instance; the existing
  alias remains the routing/account boundary.
- The existing `SHIMMER_MASTER_KEY` and encrypted `<store>.secrets.json` are
  the persistence boundary; browser storage is not used for tokens.
- OAuth device start/status/disconnect are local-admin operations, and a
  gateway restart invalidates in-flight authorization transactions.
- The codex endpoint does not provide a stable OpenAI `/models` contract, so
  the built-in ChatGPT template does not hard-code a model allowlist.

## Unresolved Material Questions

- What exact OpenCode OAuth contract applies to the installed build, including
  endpoint hostnames, client/scopes, redirect behavior, token rotation, and
  account-id claim/header names?
- Does the ChatGPT OAuth route accept chat completions, Responses, or only the
  specific OpenCode request shape, and which translation is required here?
- What should reconnect do when a provider returns a new refresh token, and is
  explicit revocation required on disconnect?

## Explicitly Excluded

- API-key OAuth, API-key behavior changes, or a broad reusable OAuth framework.
- OAuth integrations for other providers or generic account federation.
- Token export, refresh-token display, browser token storage, distributed
  pending-state storage, HSM integration, or a new secrets service.
- OpenAI endpoint expansion beyond the verified ChatGPT/OpenCode request path,
  billing/quota integration, and speculative model discovery.
