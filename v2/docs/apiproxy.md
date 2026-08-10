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

At v2 HEAD, the binary listens on `:<port>` (all interfaces). Treat it as an
internal sidecar or bind it behind a firewall/network policy unless you have a
newer change that binds to localhost by default. Security PR #2935, which tracks
localhost binding and warning on `ANTHROPIC_API_KEY` key-injection fallback, was still open when
this document was written.
