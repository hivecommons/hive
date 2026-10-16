# Admin MCP

The admin MCP lets you administer a hive by conversation from any MCP client
(Claude Code, omp, and others): ask about fleet status, agents, leases, plans,
spend and audit history, and — when you switch it on — pause an agent, approve
a plan or change a budget through a two-step confirmation.

It is available on the `v6` line only. There are two ways to reach it:

| Transport | Where it runs | Use it when |
|-----------|---------------|-------------|
| **HTTP** (recommended) | `POST /api/admin/mcp` on the dashboard | Your client accepts a plain JSON-RPC HTTP endpoint. The running hub serves the tool catalogue, so it never drifts and there is nothing to rebuild after an upgrade. One entry per hive. |
| **Stdio** | the `hive-admin-mcp` binary on your machine | Your client cannot use the HTTP endpoint, or you manage several hives from one entry (`select_hive` is stdio-only). The catalogue is compiled into the binary, so re-extract or rebuild it after each hub upgrade. |

Use HTTP whenever your client supports it.

## Quick start

1. **Get the token.** The admin MCP uses the same dashboard token as
   [`hivectl`](hivectl.md): `HIVE_DASHBOARD_TOKEN` or `dashboard.auth_token`.
   See [Generating and rotating `HIVE_DASHBOARD_TOKEN`](env-vars.md#generating-and-rotating-hive_dashboard_token).
2. **Pick a transport** (HTTP if your client supports it) and paste the matching client config below.
3. **Run one tool.** Ask the client to call `hive_status`. A JSON status
   summary back means you are connected.

### Client config: HTTP

Claude Code:

```bash
claude mcp add --transport http hive http://127.0.0.1:3001/api/admin/mcp \
  --header "Authorization: Bearer $HIVE_DASHBOARD_TOKEN"
```

Or in an MCP JSON config (for example a project `.mcp.json`; omp has its own
config paths, see [omp](#omp)):

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
[HTTP endpoint](#http-endpoint)). omp is verified to work with it. A strict
streamable-HTTP client that insists on SSE or a session id may refuse it; if
you have not checked your client, try HTTP first and fall back to stdio if it
is refused.

#### omp

omp reads `~/.omp/agent/mcp.json` (user scope) or `.omp/mcp.json` (one
project). It only reads a root `.mcp.json` as a fallback. Keep the token out of
the file: use `${HIVE_DASHBOARD_TOKEN}`, or a `!command` header value that omp
runs at connect time:

```json
{
  "mcpServers": {
    "hive-admin": {
      "type": "http",
      "url": "http://127.0.0.1:3001/api/admin/mcp",
      "headers": { "Authorization": "Bearer ${HIVE_DASHBOARD_TOKEN}" }
    }
  }
}
```

```json
"headers": { "Authorization": "!jq -r '\"Bearer \" + (.[] | select(.name==\"local\").token)' /path/to/hives.json" }
```

omp 18.8.9 is verified against the stateless endpoint (`hive_status`,
`issues_by_band` and `prs_by_band` all returned data). The hub answers
`notifications/initialized` with `200` and `{"jsonrpc":"2.0","result":{}}`
rather than a bare `202`, which omp accepts. `GET` returns `405`, and omp
disables its optional SSE listener without an error.

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

The tool catalogue is compiled into the binary, so re-extract or rebuild it after each hub upgrade (HTTP has no such step). `version_read` shows the binary's own `stdio_build` and a `skew` flag next to the hub build, and `initialize` reports `serverInfo.version` as `<version>+<short sha>`. Over HTTP, `version_read` returns the recorded last-seen stdio build (`last_seen_stdio_build`) and timestamp (`last_seen_stdio_at`) alongside `hub_build`.

The stdio binary sends `User-Agent: hive-admin-mcp/<version>+<short sha>` on every hub request. The hub records the last-seen stdio client build and the time it was seen from that header on the REST paths the binary's read tools call — `GET /api/version`, `GET /api/status` and `GET /api/status/summary` — and on HTTP admin MCP read tool calls (`/api/admin/mcp`). Only a well-formed `hive-admin-mcp/<version>+<7–40 hex short sha|unknown>` of at most 64 bytes is recorded; any other User-Agent (browsers, `curl`, the regular `hivectl` CLI's default `hivectl/0.1`) is ignored and never overwrites a recorded sighting. The dashboard displays this in the Version menu under **Stdio client** next to the hub build, with a warning badge (`⚠ build skew`) when the short SHAs differ.

### Skew notice

The binary also tells the agent when it is out of step with the hub, so a stale
binary no longer fails tools with no hint. At startup it fetches
`/api/version` from the active hive (3-second timeout), and again whenever
`select_hive` switches hives. When its short SHA differs from the hub's, it
builds a one-line notice that names both SHAs and points here: re-extract or
rebuild the binary as described in [Getting the binary](#getting-the-binary).

- **`initialize`:** if skew is found at startup, the notice is sent as the
  server `instructions`, which MCP clients put into the agent's context.
  Instructions are fixed at `initialize`, so skew found after `select_hive`
  only shows up in errors.
- **Tool errors:** the notice is appended to errors the binary raises itself
  (`unsupported admin MCP read tool`, `unknown tool`) and to hub 404s on read
  tools. Other errors are not changed.

There is no notice when either SHA is `unknown`, when the hub cannot be
reached, or when the builds match; errors are then exactly as before. Binaries
built before this notice existed cannot warn, so keep re-extracting or
rebuilding after each hub upgrade.

Requests from the binary send `User-Agent: hive-admin-mcp/<version>+<short sha>`
(the same value as `serverInfo.version`) instead of hivectl's `hivectl/0.1`.

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
| `fleet_status` | Fleet state; `systemResources` also carries the container's memory limit (`memLimit`, `unlimited` when none is set, with `memLimitBytes`) and out-of-memory kill count (`oomKillCount`), see [Start records, crash traces and out-of-memory evidence](#start-records-crash-traces-and-out-of-memory-evidence) |
| `version_read` | Running build from `/api/version`: `currentCommit`, `branch`, `channel`, `commitsBehind`, `behind`; over stdio also `stdio_build` (`version`, `sha`, `short`, `branch`) and `skew` (true when the stdio and hub short SHAs are both known and differ); over HTTP also `last_seen_stdio_build` and `last_seen_stdio_at` alongside `hub_build` |
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
| `issues_by_band` | Issues grouped by band; optional `repo`, `band`, `stale`, `held`, `offset` |
| `prs_by_band` | Pull requests grouped by band; optional `repo`, `band`, `stale`, `held`, `offset` |
| `review_queue` | The your-turn items (the dashboard inbox's N plus M), parked issues included, in `inbox_rank` order, each with its verb, reason code and reason; optional `repo`, `offset` |
| `governor_setup_proposal` | Proposed governor setup, each setting with a reason and the write operation and args that apply it |
| `log_sources` | Log sources this hive can read, each available or not with a reason; `hive` is the default, plus `proxy`, `hive-output`, `litellm` and `requests` (see [design/admin-mcp.md](design/admin-mcp.md#output-and-request-log-sources)), and `starts` and `crash` (see [below](#start-records-crash-traces-and-out-of-memory-evidence)) |
| `log_read` | The hive's own log (or another `log_sources` source), newest matching lines oldest first, scrubbed; optional `source`, `lines` (default 100, max 1000), `since`/`until` (RFC 3339 with offset), `min_level`, `include`, `exclude`, `cursor`. Needs the read-write role. Not capped by `limit`; see [design/admin-mcp.md](design/admin-mcp.md#log-read) |

Meta tools:

- `exclusion_catalogue` lists what the admin MCP will never do, and why.
- `refuse_operation` explains the refusal for a named operation.

Write tools (`write_preview`, `write_confirm`) appear only when writes are on.
On stdio, `select_hive` is also available.

### Diagnostic reads

These read-only tools (#11943) show what an operator would otherwise need a shell
for. They work the same over HTTP and stdio. Each one wraps a dashboard endpoint
and keeps that endpoint's role. Every result is scrubbed the way `log_read` lines
are, stays under 128 KiB, and says what it cut. The shared `limit` does not apply.
Results carry the `untrusted_content` marker.

| Tool | Wraps | Required role | Returns |
|------|-------|---------------|---------|
| `agent_log_read` | `GET /api/agents/{name}/log` | Any authenticated role | The agent's live terminal log: the newest `max_bytes` (default 64 KiB, max 128 KiB); needs `agent`, optional `explain` (`only`/`hide`) |
| `agent_kicks_list` | `GET /api/agents/{name}/kicks` | Any authenticated role | The agent's archived kick logs, newest first; needs `agent` |
| `agent_kick_read` | `GET /api/agents/{name}/kicks/{id}` | Any authenticated role | One archived kick log, same bounds as `agent_log_read`; needs `agent`, `kick_id` |
| `run_log_read` | `GET /api/runs/{key}/log` | Read | A run's stage log, same bounds; needs `run`, optional `stage`, `gen`, `tail` |
| `run_detail_read` | `GET /api/runs/{key}/detail` | Any authenticated role (pane tails need read-write) | A run's detail; the oldest events, stages, PRs and notes and the oldest raw log bytes are dropped first; needs `run` |
| `health_deep_read` | `GET /api/health/deep` | None (public) | The deep health check, whole |
| `prompt_history_read` | `GET /api/prompt-history` | Read-write | Delivered prompts, newest first; each prompt cut at 8 KiB (`cut`, `full_bytes`); optional `agent`, `window_hours` |
| `config_effective_read` | `GET /api/diag/config` (as `/api/config/provenance`) | Owner, verified by the hive | Every configuration key and environment name, with the layer that set it; optional `offset` |

A plain-text log comes back as `{text, content_type, truncated, bytes_total,
bytes_returned, max_bytes, redactions}`. The dashboard's own plain-text
responses are not changed. A refused read returns the typed forbidden error.
The error never includes the hive's response body.

`config_effective_read` shows values only for the keys and names below. These
are known not to hold secrets. Every other key or environment variable is shown
only as set or not set. A listed value is still scrubbed. A listed URL that
holds a login is withheld. The result also includes provenance: the layer that
set each key, any lower layers it overrode (`also_set_by`), a rejected overlay
and its reason, the last-good fallback, and the GitHub ratchet. The
configuration file itself is never returned. Over HTTP, the hive must itself
have authenticated the caller as owner, with the dashboard token or a verified
owner session. An owner role claimed only in request headers is refused with
the typed forbidden error `ErrOwnerUnverified`.

- Configuration keys: `acmm_level`, `hive_id`, `project.org`, `project.name`,
  `project.repos[*]`, `project.primary_repo`, `project.ai_author`,
  `project.forge`, `hub.enabled`, `hub.url`, `hub.is_public`, `hub.hive_type`,
  `hub.auto_upgrade`, `hub.auto_upgrade_mode`, `github.app_id`,
  `github.installation_id`, `github.app_slug`, `github.api_url`,
  `github.base_url`, `governor.eval_interval_s`, `governor.logging.dir`,
  `governor.logging.level`, `governor.logging.max_size_mb`,
  `governor.logging.max_age_days`, `governor.logging.max_backups`,
  `governor.logging.compress`, `governor.logging.kick_log_source`,
  `governor.logging.read_scan_max_mb`, `agents.*.backend`, `agents.*.model`,
  `agents.*.enabled`, `agents.*.paused`
- Environment names: `HIVE_CONFIG`, `HIVE_ADMIN_MCP_ENABLE_WRITES`,
  `HIVE_ADMIN_MCP_PENDING_FILE`, `HOME`, `HOSTNAME`, `LANG`, `PATH`, `TZ`,
  `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT`

See [design/admin-mcp.md](design/admin-mcp.md#diagnostic-reads).

### Start records, crash traces and out-of-memory evidence

These tell an operator with no host shell whether the hive crashed, was killed
or ran out of memory (#11945). A hive whose process is down cannot be read
through the MCP at all; read the evidence after it is back.

- **`starts` log source.** Read it with `log_read` and `source: starts`. Each
  hive start writes one line: its short identifier (`start`, 8 hex characters),
  its start time and how the previous run ended in `previous.verdict`:
  - `clean`: it finished shutting down.
  - `not-closed`: it stopped without finishing shutdown, for any reason.
    Examples are SIGKILL, the out-of-memory killer, a crash or a fatal start-up
    error.
  - `self-upgrade-failed`: it exited after a failed self-upgrade.
  - `unknown`: there is no record of a previous run.

  The line also gives `previous.last_seen_alive`, which is refreshed about every
  30 seconds. It gives `previous.memory`, the last memory and out-of-memory
  reading taken before that run ended, so the count survives a container
  restart that resets it. `previous.crash_trace` is set when that run left a
  crash trace. Lines for `not-closed` and `self-upgrade-failed` are `WARN`, so
  `min_level: warn` lists only the bad ends. A second hive process refused at
  start-up records nothing. The newest 200 starts are kept.
- **Start identifier in the hive log.** Every line of the `hive` source carries
  `"start":"<identifier>"`. Use `include` with the identifier to read one run.
- **`crash` log source.** When the hive process dies of an unrecovered panic or
  a fatal Go runtime error, the runtime's trace is kept, one file per start. Each
  file opens with a line naming its start. Anything after that line is the trace.
  The last 10 traces are kept, each cut to at most 1 MiB. **A process killed by
  SIGKILL or the out-of-memory killer leaves no trace**: it runs no code on the
  way out. Its start then reads `not-closed` with no `crash_trace`.
- **`fleet_status`.** `systemResources` gains `oomKillCount`, the
  out-of-memory kills counted by the container's memory controller (cgroup
  `memory.events`, or `memory.oom_control` on cgroup v1). It also gains
  `memLimit`, either a size such as `2048 MiB` or `unlimited`, or `unknown`
  when no memory controller is readable. The existing fields, which the web UI
  reads, are unchanged. A count that is missing means the controller does not
  report one.

The files live in `HIVE_OUTPUT_LOG_DIR` (default `/data/.hive/logs`). They are
`0600` in a `0700` directory owned by the hive's runtime user, so agent users
cannot read them. Nothing new is mounted, published or installed on the host.
The gateway and the container start-up script are unchanged.

### What still needs a host shell

Some evidence exists only outside the hive container. The admin MCP cannot
read it. The table gives the closest in-container substitute.

| Host-only evidence | In-container substitute |
|---|---|
| Unit restart counts (`systemctl show hive -p NRestarts`, Quadlet) | `starts`: one line per start, with how the run before it ended |
| Container inspect (`podman inspect` / `docker inspect`: exit code, `OOMKilled`, restart count) | `starts` (`not-closed`, `self-upgrade-failed`, `previous.memory`) and `fleet_status` `systemResources.oomKillCount` / `memLimit` |
| Kubernetes describe and events (`kubectl describe pod`, `kubectl get events`: last termination reason, probe failures, scheduling) | `starts` for how each run ended, and `health_deep_read` for what the probes see. Scheduling and probe events have none |
| Node-level out-of-memory kills and evictions (kernel log, kubelet evictions) | `oomKillCount` counts kills inside the hive container only. A kill or eviction that ends the hive shows as `not-closed` in `starts`, with the last reading before it. Kills elsewhere on the node have none |
| SELinux denials (`ausearch -m avc`, the host audit log) | None. The refused operation's error may appear in the `hive` source |
| The gateway's own logs | None. `requests` shows what reached the dashboard; requests the gateway refused are not in it |
| The container start-up script's output | None for the script itself. `hive-output` keeps what the hive prints before its log is set up, and `proxy` keeps the auth proxy's output |
| The web terminal server's output | None. `agent_log_read` reads what the agent terminals show |

**The role gate does not limit dashboard-token holders.** Whoever holds the
dashboard token is the owner, and every read and write role check passes for
an owner. Treat the token as owner access to all of the evidence above.

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
