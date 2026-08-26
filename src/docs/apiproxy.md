# `apiproxy` binary

`cmd/apiproxy` is a small Anthropic-compatible HTTP proxy used to observe agent
model traffic. It forwards requests to an upstream API and emits JSON event logs
for requests/responses and SSE chunks.

Two independent credentials are involved:

- **Client auth token** (`PROXY_AUTH_TOKEN`) — **required**; what callers must
  present *to the proxy*. Every request must carry either
  `Authorization: Bearer <PROXY_AUTH_TOKEN>` or `X-Api-Key: <PROXY_AUTH_TOKEN>`,
  otherwise the proxy returns `401 Unauthorized` without logging or forwarding
  the request. The comparison is constant time. The gate token is stripped from
  the request and never forwarded upstream.
- **Upstream API key** (`ANTHROPIC_API_KEY`) — what the proxy sends *upstream*.
  Once a caller passes the gate, the proxy injects this key as
  `Authorization: Bearer <ANTHROPIC_API_KEY>` unless the caller supplied its own
  real upstream credential (a non-dummy `Authorization`/`X-Api-Key`), which is
  forwarded as-is. This preserves the dummy-key workflow used by agent CLIs.

The two are deliberately separate so that the host upstream key is never an
implicit credential for anyone who can reach the listener. The proxy fails
closed: if `PROXY_AUTH_TOKEN` is unset, `cmd/apiproxy` exits with an error at
startup, and the proxy handler refuses every request with
`503 Service Unavailable` rather than acting as an open relay. Otherwise any
co-resident loopback process — including a prompt-injected agent — could spend
the host `ANTHROPIC_API_KEY` unauthenticated.

## Run

```bash
go build -o bin/apiproxy ./cmd/apiproxy
PROXY_AUTH_TOKEN=... ANTHROPIC_API_KEY=... \
  bin/apiproxy --port 9000 --upstream https://api.anthropic.com --log apiproxy.jsonl
# To expose beyond the local host, opt in explicitly:
PROXY_AUTH_TOKEN=... ANTHROPIC_API_KEY=... \
  bin/apiproxy --host 0.0.0.0 --port 9000 --upstream https://api.anthropic.com
```

Flags verified from `cmd/apiproxy/main.go`:

| Flag | Default | Purpose |
|---|---|---|
| `--host` | `127.0.0.1` | Listen address. The default is localhost-only; use `--host 0.0.0.0` only when another container/pod must reach the proxy and network policy/firewall rules protect it. |
| `--port` | `9000` | Listen port. |
| `--upstream` | `https://api.anthropic.com` | Upstream API base URL. |
| `--log` | stdout | Optional JSONL log file. |

Environment:

| Name | Purpose |
|---|---|
| `PROXY_AUTH_TOKEN` | **Required.** Client auth token callers must present to the proxy (`Authorization: Bearer <token>` or `X-Api-Key: <token>`). Validated with a constant-time compare and stripped before the upstream request. When unset, `cmd/apiproxy` refuses to start and the proxy handler answers `503`. |
| `ANTHROPIC_API_KEY` | Upstream API key injected on the outbound request when the caller did not supply its own real upstream credential. Never used to authenticate callers. |

## Deployment notes

The binary listens on `127.0.0.1:<port>` by default. Treat `--host 0.0.0.0` as
an explicit compatibility opt-in for deployments that need cross-container or
cross-pod access, and pair it with network policy/firewall controls.
`PROXY_AUTH_TOKEN` is mandatory: without it the proxy would accept any caller
and lend them the host `ANTHROPIC_API_KEY`, so the binary refuses to start. Use a
high-entropy random value distinct from the upstream key, and rotate the two
independently.
