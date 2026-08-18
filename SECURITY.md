# Security Policy

## Scope

Shimmer LLM Gateway is a **localhost developer tool**: it captures and inspects
LLM traffic for debugging. It is not designed to be exposed to untrusted
networks.

By default the gateway refuses to bind a non-loopback address; the
`-allow-remote` flag opts out of that guard. Only use `-allow-remote` on a
trusted network, and understand the exposure described below.

## Reporting a vulnerability

Please report security-sensitive issues privately, not as a public issue, via
GitHub **private vulnerability reporting** on this repository
(Security → Report a vulnerability). You should receive a response within a
few days.

## Threat model

- The gateway binds `127.0.0.1` by default. With `-allow-remote`, the
  unauthenticated `/api/*` surface becomes reachable over the network. The
  secrets master-key endpoint additionally refuses non-loopback client sources.
- Provider `base_url` values are treated as trusted configuration. The gateway
  validates the scheme (`http`/`https`) and does not follow redirects, but it
  will still forward to the http(s) hosts you configure — including
  private/loopback hosts such as a local Ollama or vLLM.
- API keys are stored encrypted at rest (AES-256-GCM) in
  `<store>.secrets.json`, keyed by `SHIMMER_MASTER_KEY`. Keep that master key
  secret and set it explicitly before first run.
