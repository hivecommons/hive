# Admin MCP

The admin MCP lets you administer a hive by conversation from any MCP client
(Claude Code, omp, and others): ask about fleet status, agents, leases, plans,
spend and audit history, and — when you switch it on — pause an agent, approve
a plan or change a budget through a two-step confirmation.

It is available on the `v6` line only. There are two ways to reach it:

| Transport | Where it runs | Use it when |
|-----------|---------------|-------------|
| **HTTP** | `POST /api/admin/mcp` on the dashboard | Your client accepts a plain JSON-RPC HTTP endpoint. One entry per hive. |
| **Stdio** | the `hive-admin-mcp` binary on your machine | Your client only speaks stdio or strict streamable HTTP, or you manage several hives from one entry. |

## Quick start

1. **Get the token.** The admin MCP uses the same dashboard token as
   [`hivectl`](hivectl.md): `HIVE_DASHBOARD_TOKEN` or `dashboard.auth_token`.
   See [Generating and rotating `HIVE_DASHBOARD_TOKEN`](env-vars.md#generating-and-rotating-hive_dashboard_token).
2. **Pick a transport** and paste the matching client config below.
3. **Run one tool.** Ask the client to call `hive_status`. A JSON status
   summary back means you are connected.

### Client config: HTTP

Claude Code:

```bash
claude mcp add --transport http hive http://127.0.0.1:3001/api/admin/mcp \
  --header "Authorization: Bearer $HIVE_DASHBOARD_TOKEN"
```

Or in an MCP JSON config (`.mcp.json`, omp's MCP config):

```json
{
  "mcpServers": {
    "hive": {
      "type": "http",
      "url": "http://127.0.0.1:3001/api/admin/mcp",
      "headers": { "Authorization": "Bearer <dashboard token>" }
    }
  }
}
```

Replace the address with your dashboard's. The endpoint is stateless (see
[HTTP endpoint](#http-endpoint)); a strict streamable-HTTP client that insists
on SSE or a session id may refuse it. Use stdio instead.

### Client config: stdio

First get the binary onto the host (see [Getting the binary](#getting-the-binary)),
then:

```json
{
  "mcpServers": {
    "hive-admin": {
      "command": "/usr/local/bin/hive-admin-mcp",
      "env": {
        "HIVE_ADMIN_MCP_HIVES": "[{\"name\":\"prod\",\"address\":\"http://127.0.0.1:3001\",\"token\":\"<dashboard token>\"}]"
      }
    }
  }
}
```

Claude Code equivalent:

```bash
claude mcp add hive-admin \
  --env HIVE_ADMIN_MCP_HIVES='[{"name":"prod","address":"http://127.0.0.1:3001","token":"<dashboard token>"}]' \
  -- /usr/local/bin/hive-admin-mcp
```

## HTTP endpoint

`POST /api/admin/mcp` is always registered on v6; there is no enable flag.

- Stateless JSON-RPC 2.0: one request per POST, no SSE, no session id.
- `GET` returns 405.
- Request bodies are capped at 64 KiB.
- Authenticate with `Authorization: Bearer <token>`. A `?token=` query
  parameter is rejected.
- Do not send `X-Hive-User`, `X-Hive-Role` or `X-Hive-Owner-Role-Verified`.
- A spoke with an `authorized_users` allowlist refuses the shared token, so a
  token client cannot administer it.

## Stdio binary

### Getting the binary

The image carries it at `/usr/local/share/hive/hive-admin-mcp`. There is no
separate release artifact today. Copy it out of the container:

```bash
podman create --name hive-admin-mcp-extract ghcr.io/hivecommons/hive:stable
podman cp hive-admin-mcp-extract:/usr/local/share/hive/hive-admin-mcp ~/.local/bin/hive-admin-mcp
podman rm hive-admin-mcp-extract
```

Or build it from a checkout of the `v6` branch: `cd src && go build -o hive-admin-mcp ./cmd/hive-admin-mcp`.

### Environment variables

| Variable | Required | Meaning |
|----------|----------|---------|
| `HIVE_ADMIN_MCP_HIVES` | Yes | JSON array of `{"name","address","token"}`. All three fields are required and names must be unique. The binary exits with an error otherwise. |
| `HIVE_ADMIN_MCP_ACTIVE` | No | Name of the hive active at start. Defaults to the first entry; an unknown name is an error. |
| `HIVE_ADMIN_MCP_ENABLE_WRITES` | No | `1`, `true`, `yes` or `on` turns write tools on. Anything else leaves them off. |
| `HIVE_ADMIN_MCP_PENDING_FILE` | No | Where pending write confirmations are stored. Default on stdio: `<user config dir>/hive/admin-mcp-pending-confirmations.json`. Default on the HTTP endpoint: `/data/admin-mcp-pending-confirmations.json`. |

`HIVE_ADMIN_MCP_ENABLE_WRITES` and `HIVE_ADMIN_MCP_PENDING_FILE` are also read
by the dashboard process for the HTTP endpoint.

## Several hives

- **HTTP:** the endpoint is the scope. Add one client entry per hive.
- **Stdio:** one binary holds a roster of hives and acts on one active hive at
  a time. Ask the assistant to call `select_hive` with a roster name to switch.
  There is no fan-out across hives.

`select_hive` first checks the hive with `GET /api/status/summary` and only
switches if that succeeds. It tells you whether the host is unreachable, the
token is wrong, or the spoke refuses the shared token.

Pending write confirmations are bound to the hive name they were created on.
Confirming one while a different hive is active fails.

## Tools

Read tools take an optional `limit` (default 20, maximum 50). Text results are
capped at 256 KiB.

| Tool | Returns |
|------|---------|
| `hive_status` | Hive status summary |
| `fleet_status` | Fleet state |
| `agents_list` | Agents |
| `runs_list` | Runs |
| `leases_list` | Leases |
| `claims_list` | Issue claims |
| `plans_list` | Plans |
| `audit_log` | Audit entries |
| `settings_read` | Settings |
| `autonomy_readiness` | Autonomy readiness |
| `spend_read` | Spend |
| `contributors_list` | Contributors |
| `knowledge_read` | Knowledge entries |
| `hive_advisor` | Hive advice |
| `advisor_records` | Advisor records |
| `agent_nudge_status` | Outcome of a nudge; needs `agent` |
| `issues_by_band` | Issues grouped by band; optional `repo`, `band`, `stale` |
| `prs_by_band` | Pull requests grouped by band; optional `repo`, `band`, `stale` |
| `review_queue` | Issues and pull requests that need a human now, in priority order, each with a reason; optional `repo`, `offset` |
| `governor_setup_proposal` | Proposed governor setup, each setting with a reason and the write operation and args that apply it |

Meta tools:

- `exclusion_catalogue` lists what the admin MCP will never do, and why.
- `refuse_operation` explains the refusal for a named operation.

Write tools (`write_preview`, `write_confirm`) appear only when writes are on.
On stdio, `select_hive` is also available.

## Writes

Writes are off unless `HIVE_ADMIN_MCP_ENABLE_WRITES` is set on the process that
serves the MCP. They take two steps:

1. `write_preview` with `operation` and `args`. Nothing changes. You get a
   `confirmation_id`, an expiry and a description of what would happen.
2. `write_confirm` with the `confirmation_id`. The confirmation is single-use
   and expires after 10 minutes.

Registered operations:

- `agent.pause`, `agent.resume`, `agent.nudge`, `agent.restart`, `agent.add`,
  `agent.remove`, `agent.model`, `agent.backend`, `agent.effort`,
  `agent.interaction_tier`
- `fleet.autonomy_level`
- `plan.propose`, `plan.approve`, `plan.reject`
- `governor.feature_settings`, `governor.thresholds`,
  `governor.threshold_scaling`
- `repository.pause`, `repository.resume`, `repository.item_hold`
- `budget.update`, `budget.reset`, `budget.ignore`
- `contributor.trust`, `contributor.agent_role`,
  `contributor.agent_role_grants`, `contributor.revoke`,
  `contributor.requeue`, `contributor.delete`
- `backup.create` (the archive itself is not returned through MCP)
- `circuit_breaker.engage`, `circuit_breaker.release`

Anything not on this list is refused; the assistant must not approximate it by
combining other operations.

Every write is recorded in the audit log as user `local`, whoever confirmed it.

Confirmed writes re-authenticate with your `Authorization: Bearer` token. A
caller admitted another way (dashboard session, hub proxy, internal header)
can preview but not confirm, so on the HTTP endpoint writes are reported
unavailable for that caller. Writes are likewise unavailable on a spoke that
enforces per-user direct-route authorization.

## Troubleshooting

- **401 on `write_confirm` after a successful preview.** The request was
  admitted without the bearer token (session, hub proxy or internal header).
  Connect with the dashboard token in the `Authorization` header.
- **Write tools missing, or "writes unavailable".** Set
  `HIVE_ADMIN_MCP_ENABLE_WRITES=1` on the serving process, or read the reason
  returned by `exclusion_catalogue`: a per-user direct-route spoke, or a caller
  not using the bearer token.
- **`select_hive` fails.** "unreachable host": check the address and network.
  "wrong dashboard token": the roster token does not match that hive. "direct-route
  spoke refused shared token": that spoke needs per-user access and cannot be
  administered with a token.
- **405 on the HTTP endpoint.** You sent a GET; use POST.
- **Confirmation expired or wrong hive.** Run `write_preview` again on the hive
  you want to change.

For the design record behind this feature, see
[`design/admin-mcp.md`](design/admin-mcp.md).
