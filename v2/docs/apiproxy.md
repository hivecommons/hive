# `apiproxy` binary

`cmd/apiproxy` is a small Anthropic-compatible HTTP proxy used to observe agent
model traffic. It forwards requests to an upstream API and emits JSON event logs
for requests/responses and SSE chunks. It can inject an upstream API key when the
client request does not already carry a valid `Authorization` or `X-Api-Key` header.

## Run

```bash
go build -o bin/apiproxy ./cmd/apiproxy
PROXY_AUTH_TOKEN=... bin/apiproxy --port 9000 --upstream https://api.anthropic.com --log apiproxy.jsonl
```

Flags verified from `cmd/apiproxy/main.go`:

| Flag | Default | Purpose |
|---|---|---|
| `--port` | `9000` | Listen port. |
| `--upstream` | `https://api.anthropic.com` | Upstream API base URL. |
| `--log` | stdout | Optional JSONL log file. |

Environment:

| Name | Purpose |
|---|---|
| `PROXY_AUTH_TOKEN` | Preferred upstream API key used to set an outbound bearer Authorization header when the client did not send a valid key. |
| `ANTHROPIC_API_KEY` | Fallback upstream API key if `PROXY_AUTH_TOKEN` is unset. |

## Deployment notes

By default the binary listens on `:<port>` (all interfaces). Treat it as an
internal sidecar and bind it behind a firewall or NetworkPolicy so nothing on
the pod network can reach it directly. If your build exposes a bind-address
flag, prefer binding to `127.0.0.1` so only same-container callers can reach the
proxy. Avoid relying on the `ANTHROPIC_API_KEY` fallback upstream key in
production — configure `PROXY_AUTH_TOKEN` explicitly instead.
