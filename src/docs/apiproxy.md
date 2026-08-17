# `apiproxy` binary

`cmd/apiproxy` is a small Anthropic-compatible HTTP proxy used to observe agent
model traffic. It forwards requests to an upstream API and emits JSON event logs
for requests/responses and SSE chunks. It can inject an upstream API key when the
client request does not already carry a valid `Authorization` or `X-Api-Key` header.

## Run

```bash
go build -o bin/apiproxy ./cmd/apiproxy
PROXY_AUTH_TOKEN=... bin/apiproxy --port 9000 --upstream https://api.anthropic.com --log apiproxy.jsonl
# To expose beyond the local host, opt in explicitly:
PROXY_AUTH_TOKEN=... bin/apiproxy --host 0.0.0.0 --port 9000 --upstream https://api.anthropic.com
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
| `PROXY_AUTH_TOKEN` | Preferred upstream API key used to set an outbound bearer Authorization header when the client did not send a valid key. |
| `ANTHROPIC_API_KEY` | Fallback upstream API key if `PROXY_AUTH_TOKEN` is unset. The proxy logs a warning when this fallback is used because any client that can reach the proxy can use the host Anthropic key. |

## Deployment notes

The binary listens on `127.0.0.1:<port>` by default. Treat `--host 0.0.0.0` as
an explicit compatibility opt-in for deployments that need cross-container or
cross-pod access, and pair it with network policy/firewall controls.
Avoid relying on the `ANTHROPIC_API_KEY` fallback upstream key in production —
configure `PROXY_AUTH_TOKEN` explicitly instead.
