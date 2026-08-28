# Security Policy

## Scope

Shimmer LLM Gateway is a **localhost developer tool**: it captures and inspects
LLM traffic for debugging. It is not designed to be exposed to untrusted
networks.

By default the gateway refuses to bind anything but loopback addresses; the
config's `listen_addrs` additionally allow CGNAT (`100.64.0.0/10`, the
Tailscale default range) and ULA (`fc00::/7`) addresses so a localhost +
Tailscale setup needs no special flag. Anything else — a wildcard, a public
IP, or a plain LAN address — is refused. Understand the exposure described
below before adding non-loopback addresses.

## Reporting a vulnerability

Please report security-sensitive issues privately, not as a public issue, via
GitHub **private vulnerability reporting** on this repository
(Security → Report a vulnerability). You should receive a response within a
few days.

## Threat model

- The gateway binds `127.0.0.1` by default. A non-loopback `listen_addrs`
  entry (e.g. a Tailscale CGNAT address) makes the unauthenticated `/api/*`
  surface reachable from that network (on a Tailscale tailnet, only other
  devices on the tailnet). The secrets master-key endpoint additionally
  refuses non-loopback client sources.
- Provider `base_url` values are treated as trusted configuration. The gateway
  validates the scheme (`http`/`https`) and does not follow redirects, but it
  will still forward to the http(s) hosts you configure — including
  private/loopback hosts such as a local Ollama or vLLM.
- API keys are stored encrypted at rest (AES-256-GCM) in
  `<store>.secrets.json`, keyed by `SHIMMER_MASTER_KEY`. Keep that master key
  secret and set it explicitly before first run.
