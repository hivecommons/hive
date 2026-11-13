# Admin MCP

The admin MCP lets you run a hive by conversation from any MCP client, such as
Claude Code or omp. You can ask about fleet status, agents, leases, plans,
spend and audit history. When writes are switched on, you can also make
changes, such as pausing an agent, approving a plan or changing a budget. Every
write takes a two-step confirmation.

It is available on the `v6` line only.

**On this page:** [Quick start](#quick-start) ·
[Authentication](#authentication) · [Connect your client](#connect-your-client) ·
[HTTP endpoint](#http-endpoint) · [Stdio binary](#stdio-binary) ·
[Several hives](#several-hives) · [Tools](#tools) · [Writes](#writes) ·
[Troubleshooting](#troubleshooting)

## Two ways to connect

| Transport              | Where it runs                               | Use it when                                                                                                      |
| ---------------------- | ------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| **HTTP** (recommended) | `POST /api/admin/mcp` on the dashboard      | Your client accepts a plain JSON-RPC HTTP endpoint. Add one entry per hive.                                      |
| **Stdio**              | the `hive-admin-mcp` binary on your machine | Your client cannot use the HTTP endpoint, or you want one entry for several hives (`select_hive` is stdio-only). |

Use HTTP whenever your client supports it. The running hub serves the HTTP tool
catalogue, so it never drifts and there is nothing to rebuild after an upgrade.
The stdio binary has the catalogue compiled in, so you must re-extract or
rebuild it after each hub upgrade.

## Quick start

1. **Find out which credential your hive takes** (see [Authentication](#authentication)).
   A hive hosted by the hub takes **your GitHub token**, and you act as yourself.
   A self-hosted hive takes its **dashboard token**, which acts as the owner.
2. **Pick a transport** (HTTP if your client supports it) and paste the
   matching config from [Connect your client](#connect-your-client).
3. **Run one tool.** Ask the client to call `hive_status`. If you get a JSON
   status summary back, you are connected.

## Authentication

How the hive is deployed decides which credential works, not the client. The
endpoint accepts whatever the dashboard accepts:

| Your hive                                                             | Credential                                              | Header                                    | You act as                                           |
| --------------------------------------------------------------------- | ------------------------------------------------------- | ----------------------------------------- | ---------------------------------------------------- |
| Hosted by the hub (`https://<hive>.hive.hivecommons.dev`)             | Your GitHub token                                       | `Authorization: Bearer <GitHub token>`    | Yourself, at your role on that hive                  |
| Self-hosted, `dashboard.auth_token` set, no `authorized_users`        | The dashboard token                                     | `Authorization: Bearer <dashboard token>` | The owner (the configured owner's login, or `owner`) |
| Self-hosted spoke with an `authorized_users` allowlist (direct-route) | Your per-user session from `hivectl --server URL login` | `Cookie: hive_session=<session>`          | Yourself, at your allowlist role                     |

### Hosted by the hub: your GitHub token

- **How it is checked.** The hub asks GitHub who the token belongs to
  (`GET /user`, cached for five minutes). It lets you in only if your login has
  a role on that hive. It then passes your login and role to the hive, so your
  reads, your writes and the audit log are all yours.
- **Which token to use.** The token only proves who you are. Give the MCP a
  **fine-grained token with no repository permissions**
  (<https://github.com/settings/personal-access-tokens/new>), not your everyday
  `gh` login, which can usually push code.
- **Where to keep it.** Keep it in a file only you can read, and have the client
  read that file at connect time, so the token never sits in the client's
  config.
- `gh auth token` also works, but it sends that broader token to the hub on
  every connection.

### Self-hosted: the dashboard token

The dashboard token is `HIVE_DASHBOARD_TOKEN` or `dashboard.auth_token`. See
[Generating and rotating `HIVE_DASHBOARD_TOKEN`](env-vars.md#generating-and-rotating-hive_dashboard_token).

**The dashboard token is the owner credential.** Anyone holding it acts as the
hive's owner, with no personal identity. That is why a spoke with an allowlist
refuses it. Use it only on a self-hosted hive you run.

### Allowlist spokes: a per-user session

These spokes accept only a per-user session. Send it the same way
[`hivectl`](hivectl.md#credentials) does, as a `Cookie` header. This path is not
yet verified with an MCP client.

## Connect your client

Replace the addresses below with your own hive's or dashboard's.

### Claude Code

**Hosted by the hub.** Put the server in `~/.claude.json` or a project
`.mcp.json` with a `headersHelper`: a command Claude Code runs at connect time
to produce the headers. In this example the fine-grained token is kept in
`~/.config/hive/github-mcp-token` (mode `0600`):

```json
{
  "mcpServers": {
    "hive": {
      "type": "http",
      "url": "https://<hive>.hive.hivecommons.dev/api/admin/mcp",
      "headersHelper": "printf '{\"Authorization\":\"Bearer %s\"}' \"$(cat ~/.config/hive/github-mcp-token)\""
    }
  }
}
```

To use your `gh` login instead, replace the `cat …` with `gh auth token`.

**Self-hosted, with the dashboard token.** From the command line:

```bash
claude mcp add --transport http hive http://127.0.0.1:3001/api/admin/mcp \
  --header "Authorization: Bearer $HIVE_DASHBOARD_TOKEN"
```

Or in an MCP JSON config, such as a project `.mcp.json`:

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

### omp

omp has its own config paths. It reads `~/.omp/agent/mcp.json` (user scope) or
`.omp/mcp.json` (one project), and reads a root `.mcp.json` only as a fallback.

Keep the token out of the file. Use either an environment variable, such as
`${HIVE_DASHBOARD_TOKEN}`, or a `!command` header value, which omp runs at
connect time.

**Hosted by the hub, with your GitHub token:**

```json
{
  "mcpServers": {
    "hive-admin": {
      "type": "http",
      "url": "https://<hive>.hive.hivecommons.dev/api/admin/mcp",
      "headers": { "Authorization": "!printf 'Bearer %s' \"$(cat ~/.config/hive/github-mcp-token)\"" }
    }
  }
}
```

**Self-hosted, with the dashboard token:**

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

Or read the token from a stdio roster file with a `!command`:

```json
"headers": { "Authorization": "!jq -r '\"Bearer \" + (.[] | select(.name==\"local\").token)' /path/to/hives.json" }
```

**Verified.** omp 18.8.9 works against the stateless endpoint: `hive_status`,
`issues_by_band` and `prs_by_band` all returned data.

- The hub answers `notifications/initialized` with `200` and
  `{"jsonrpc":"2.0","result":{}}` rather than a bare `202`, which omp accepts.
- `GET` returns `405`, and omp turns off its optional SSE listener without an
  error.

### Other HTTP clients

The endpoint is stateless (see [HTTP endpoint](#http-endpoint)). A strict
streamable-HTTP client that insists on SSE or a session id may refuse it. If you
have not checked your client, try HTTP first, and fall back to stdio if it is
refused.

### Stdio clients

First get the binary onto the host (see [Getting the binary](#getting-the-binary)).
Then:

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

The same with the Claude Code command line:

```bash
claude mcp add hive-admin \
  --env HIVE_ADMIN_MCP_HIVES='[{"name":"prod","address":"http://127.0.0.1:3001","token":"<dashboard token>"}]' \
  -- /usr/local/bin/hive-admin-mcp
```

## HTTP endpoint

`POST /api/admin/mcp` is always registered on v6. There is no flag to turn it
on.

- **Protocol:** stateless JSON-RPC 2.0. One request per POST, no SSE, no
  session id. `GET` returns 405.
- **Ping and notifications:** `ping` is answered with an empty result. A
  notification (any `notifications/...` method) is acknowledged with `200` and
  `{"jsonrpc":"2.0","result":{}}`, never with an error.
- **Size:** request bodies are capped at 64 KiB.
- **Credential:** as in [Authentication](#authentication). Send
  `Authorization: Bearer` with your GitHub token (hosted by the hub) or the
  dashboard token (self-hosted), or a `Cookie` session on an allowlist spoke. A
  `?token=` query parameter is rejected. A spoke with an `authorized_users`
  allowlist refuses the dashboard token.
- **Identity headers:** do not send `X-Hive-User`, `X-Hive-Role` or
  `X-Hive-Owner-Role-Verified`. The hive strips them; your identity comes only
  from the credential.

## Stdio binary

### Getting the binary

The image carries the binary at `/usr/local/share/hive/hive-admin-mcp`. There is
no separate release artifact today. Copy it out of the container:

```bash
podman create --name hive-admin-mcp-extract ghcr.io/hivecommons/hive:edge
podman cp hive-admin-mcp-extract:/usr/local/share/hive/hive-admin-mcp ~/.local/bin/hive-admin-mcp
podman rm hive-admin-mcp-extract
```

Use the image tag for the hive's own release line: `:edge` for v6 (today's v6
publishing channel), `:stable` once v6 is promoted to stable, and so on. The
binary warns when its build differs from the hub's, so matching lines keep them
agreeing on the protocol.

Or build it from a checkout of the `v6` branch:
`cd src && go build -o hive-admin-mcp ./cmd/hive-admin-mcp`.

### Keeping it current

The tool catalogue is compiled into the binary, so re-extract or rebuild it
after each hub upgrade. (HTTP has no such step.) Three places show which build
you have:

- `version_read` shows the binary's own `stdio_build` and a `skew` flag next to
  the hub build.
- `initialize` reports `serverInfo.version` as `<version>+<short sha>`.
- Over HTTP, `version_read` returns the last stdio build the hub saw
  (`last_seen_stdio_build`) and when (`last_seen_stdio_at`), alongside
  `hub_build`.

**How the hub records the stdio build.** The binary sends
`User-Agent: hive-admin-mcp/<version>+<short sha>` on every hub request (the
same value as `serverInfo.version`), instead of hivectl's `hivectl/0.1`.

- The hub records the build, and when it saw it, from that header on the REST
  paths the binary's read tools call (`GET /api/version`, `GET /api/status` and
  `GET /api/status/summary`) and on HTTP admin MCP read tool calls
  (`/api/admin/mcp`).
- Only a well-formed `hive-admin-mcp/<version>+<7–40 hex short sha|unknown>` of
  at most 64 bytes is recorded. Any other User-Agent (browsers, `curl`, the
  regular `hivectl` CLI's default `hivectl/0.1`) is ignored and never overwrites
  a recorded sighting.
- The dashboard shows it in the Version menu under **Stdio client**, next to the
  hub build, with a warning badge (`⚠ build skew`) when the short SHAs differ.

### Skew notice

The binary also tells the agent when it is out of step with the hub, so a stale
binary no longer fails tools with no hint.

- **When it checks:** at startup it fetches `/api/version` from the active hive
  (3-second timeout), and again whenever `select_hive` switches hives.
- **What it says:** when its short SHA differs from the hub's, it builds a
  one-line notice that names both SHAs and points here: re-extract or rebuild
  the binary as described in [Getting the binary](#getting-the-binary).
- **Where it appears:**
  - **`initialize`:** if skew is found at startup, the notice is sent as the
    server `instructions`, which MCP clients put into the agent's context.
    Instructions are fixed at `initialize`, so skew found after `select_hive`
    shows up only in errors.
  - **Tool errors:** the notice is added to errors the binary raises itself
    (`unsupported admin MCP read tool`, `unknown tool`) and to hub 404s on read
    tools. Other errors are not changed.
- **When it stays quiet:** there is no notice when either SHA is `unknown`, when
  the hub cannot be reached, or when the builds match. Errors are then exactly
  as before.

Binaries built before this notice existed cannot warn, so keep re-extracting or
rebuilding after each hub upgrade.

### Environment variables

| Variable                       | Required | Meaning                                                                                                                                                                                                    |
| ------------------------------ | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `HIVE_ADMIN_MCP_HIVES`         | Yes      | JSON array of `{"name","address","token"}`. All three fields are required and names must be unique. The binary exits with an error otherwise.                                                              |
| `HIVE_ADMIN_MCP_ACTIVE`        | No       | Name of the hive active at start. Defaults to the first entry; an unknown name is an error.                                                                                                                |
| `HIVE_ADMIN_MCP_ENABLE_WRITES` | No       | `1`, `true`, `yes` or `on` turns write tools on. Anything else leaves them off. On the stdio binary this is the only switch.                                                                               |
| `HIVE_ADMIN_MCP_PENDING_FILE`  | No       | Where pending write confirmations are stored. Default on stdio: `<user config dir>/hive/admin-mcp-pending-confirmations.json`. Default on the HTTP endpoint: `/data/admin-mcp-pending-confirmations.json`. |

The dashboard process also reads `HIVE_ADMIN_MCP_ENABLE_WRITES` and
`HIVE_ADMIN_MCP_PENDING_FILE` for the HTTP endpoint. There,
`HIVE_ADMIN_MCP_ENABLE_WRITES` is only the default: once the owner uses the
Settings → Channels switch (`admin_mcp.writes_enabled`), the switch decides. See
[Writes](#writes).

## Several hives

- **HTTP:** the endpoint is the scope. Add one client entry per hive.
- **Stdio:** one binary holds a roster of hives and acts on one active hive at a
  time. To switch, ask the assistant to call `select_hive` with a roster name.
  There is no fan-out across hives.

`select_hive` first checks the hive with `GET /api/status/summary`, and switches
only if that succeeds. If it fails, it tells you whether the host is
unreachable, the token is wrong, or the spoke refuses the shared token.

Pending write confirmations are bound to the hive name they were created on.
Confirming one while a different hive is active fails.

## Tools

Read tools take an optional `limit` (default 20, maximum 50). Text results are
capped at 256 KiB.

**Status and version**

| Tool                 | Returns                                                                  |
| -------------------- | ------------------------------------------------------------------------ |
| `hive_status`        | Hive status summary                                                      |
| `fleet_status`       | Fleet state, including memory limit and out-of-memory kills (see note 1) |
| `version_read`       | Running build from `/api/version` (see note 2)                           |
| `claude_auth_status` | Claude auth mode (`long-lived token` or `dashboard sign-in`); no token   |
| `autonomy_readiness` | Autonomy readiness                                                       |
| `spend_read`         | Spend                                                                    |
| `github_api_usage`   | GitHub API budget and who is spending it (see note 7); optional `limit`  |

**Agents, runs and plans**

| Tool                 | Returns                           |
| -------------------- | --------------------------------- |
| `agents_list`        | Agents                            |
| `runs_list`          | Runs                              |
| `leases_list`        | Leases                            |
| `claims_list`        | Issue claims                      |
| `plans_list`         | Plans                             |
| `contributors_list`  | Contributors                      |
| `agent_nudge_status` | Outcome of a nudge; needs `agent` |

**Issues, pull requests and the Activity monitor**

| Tool                        | Returns                                                                                                                                                                    |
| --------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `issues_by_band`            | Issues grouped by band; optional `repo`, `band`, `stale`, `held`, `offset`                                                                                                 |
| `prs_by_band`               | Pull requests grouped by band; optional `repo`, `band`, `stale`, `held`, `offset`                                                                                          |
| `stage_tracker_read`        | The Activity monitor (see note 3)                                                                                                                                          |
| `delegated_decision_status` | Status and outcome of the latest "Let hive decide" on one item; needs `owner`, `repo`, `number`                                                                            |
| `review_queue`              | The your-turn items (the dashboard inbox's N plus M), parked issues included, in `inbox_rank` order, each with its verb, reason code and reason; optional `repo`, `offset` |
| `contributor_queue`         | The contributor ready-work queue as the Operations card shows it (see note 6); optional `withheld`, `offset`                                                               |

**Settings, knowledge and advice**

| Tool                      | Returns                                                                                                                                                                                                                 |
| ------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `settings_read`           | A dashboard Settings view, read through the endpoint the Settings page uses, as the caller; optional `section` (default `governor`) and `agent` for the per-agent sections. See [Settings sections](#settings-sections) |
| `knowledge_read`          | Knowledge entries                                                                                                                                                                                                       |
| `hive_advisor`            | Hive advice                                                                                                                                                                                                             |
| `advisor_records`         | Advisor records                                                                                                                                                                                                         |
| `governor_setup_proposal` | Proposed governor setup: each setting with a reason, and the write operation and args that apply it                                                                                                                     |
| `audit_log`               | Audit entries                                                                                                                                                                                                           |

**Logs**

| Tool          | Returns                                                                                     |
| ------------- | ------------------------------------------------------------------------------------------- |
| `log_sources` | The log sources this hive can read, each marked available or not with a reason (see note 4) |
| `log_read`    | The hive's own log, or another `log_sources` source (see note 5). Needs the read-write role |

**Notes**

1. **`fleet_status`:** `systemResources` also carries the container's memory
   limit (`memLimit`, `unlimited` when none is set, with `memLimitBytes`) and
   out-of-memory kill count (`oomKillCount`). See
   [Start records, crash traces and out-of-memory evidence](#start-records-crash-traces-and-out-of-memory-evidence).
2. **`version_read`:** returns `hash` and `short` (the running commit),
   `branch`, `channel`, `commitsBehind` and `behind`.
   - Over stdio it also returns `stdio_build` (`version`, `sha`, `short`,
     `branch`) and `skew` (true when the stdio and hub short SHAs are both known
     and differ).
   - Over HTTP it also returns `last_seen_stdio_build` and `last_seen_stdio_at`
     alongside `hub_build`.
3. **`stage_tracker_read`:** returns the items with their stage, reason, what
   they wait on, decision question and options, linked pull requests and issues,
   blocker and hold, plus `stages[]` with a count per stage. An issue row whose
   last contributor relay ended with no pull request and no verdict carries
   `last_idle` (`text`, `reason`, `contributor`, `at`) while the completion
   cooldown that task booked is still running: the clause the dashboard row's
   tooltip shows.
   - Filters (all optional): `repo`, `kind`, `number`, `q` (the monitor's search
     box), `stage`, `needs_human`, `on_hold`, `waiting_on_you`.
   - `view` is `summary` or `full`; `recent` adds recently merged and closed
     items; `offset` pages.
   - Before the monitor has built its first snapshot, it answers with a
     `board_unavailable` refusal rather than an empty board. Retry shortly.
4. **`log_sources`:** `hive` is the default. The others are `proxy`,
   `hive-output`, `litellm` and `requests` (see
   [design/admin-mcp.md](design/admin-mcp.md#output-and-request-log-sources)),
   and `starts` and `crash` (see
   [below](#start-records-crash-traces-and-out-of-memory-evidence)).
5. **`log_read`:** returns the newest matching lines, oldest first, scrubbed.
   - Optional: `source`, `lines` (default 100, max 1000), `since`/`until`
     (RFC 3339 with offset), `min_level`, `include`, `exclude`, `cursor`.
   - Not capped by `limit`. See
     [design/admin-mcp.md](design/admin-mcp.md#log-read).
6. **`contributor_queue`:** reads `GET /api/contribute/queue`, the request
   the Operations card (Contribute → Operations → Contributor Queue) makes,
   so its rows are the card's rows in the card's order: pinned issues first
   in pinned order, then the default order, held issues last with `held` and
   `held_reason`. Each row carries its `position`.
   - Anyone who can read the card can read this; the GET is public.
   - `withheld: true` adds `withheld[]`, the candidates the queue keeps out,
     each with the gate that refused it, and `withheld_total`. That list is
     bounded by the hive's own queue limit, not by `limit`.
   - `offset` pages the rows past `limit`, with `next_offset`.
   - If you have label interests on the hive, matching rows float to the
     front, as the card shows them to you.
   - To change the queue, see [Contributor queue](#contributor-queue).
7. **`github_api_usage`:** reads `GET /api/gh-rate-limits`, the endpoint
   behind the dashboard's GitHub API card: the core bucket, the API budget
   mode, the calls that cost the most, the same hour summed per caller, the
   per-agent caps and the ETag cache.
   - It needs what the endpoint needs: a signed-in caller, with no role floor.
   - Reading it spends no core API call.
   - See [GitHub API usage](#github-api-usage) for how to read the answer.

**Meta tools**

- `exclusion_catalogue` lists what the admin MCP will never do, and why.
- `refuse_operation` explains the refusal for a named operation.

Write tools (`write_preview`, `write_confirm`) are always listed. When writes
are off, or not available to the caller, their `metadata.preview.enabled` is
`false` with a note saying why, and a call is refused. On stdio, `select_hive`
is also available.

### Settings sections

`settings_read` reaches every view the Settings dialog shows.

- It takes a section name, never a path.
- Each section is the endpoint that Settings tab reads, so the answer matches
  the page for the same caller. Most sections need a verified owner, as the tabs
  do.
- Secret values are never returned. Settings name the environment variable,
  file or header that supplies a secret, and the answer is scrubbed like every
  other read.
- It takes no `limit`. A section is returned whole, however many agents or
  entries it lists, bounded only by the 256 KiB text cap.

The sections:

- **Global settings:** `governor` (the default: modes, thresholds, labels,
  repos, budget, notifications, health, sensing, logging, security, features,
  hub, LiteLLM, watchdog, trajectory, classifier, release sentinel), `project`,
  `github_apps`, `authorized_users`, `dashboard`, `channels`, `escalation`,
  `auto_merge`, `review`, `convergence`, `sla`, `triggers`, `advisor`,
  `advisory`, `replan`, `decisions_delegate`, `question_autoclose`,
  `question_autoclose_schedule`,
  `fleet_report`, `work_source`, `project_observability`, `general_advanced`,
  `cadence_scope`, `threshold_scaling`, `backup`, `bob`, `inference_auth`,
  `gateways`, `backends`, `variables`, `write_surface`, `stat_sources`.
- **Knowledge:** `knowledge_pipeline`, `knowledge_connectors`,
  `knowledge_connectors_status`, `knowledge_agent_scopes`, `knowledge_public`.
- **Panels the tabs load:** `upstream_watch`, `linear_agent`,
  `compliance_status`, `classifier_stats`, `watchdog_activity`.
- **One agent** (needs `agent`): `agent`, `agent_prompt`, `agent_variables`.
  Prompt history is `prompt_history_read`.

Not reachable:

- the raw configuration download and the full export, because they carry
  credentials (`config_effective_read` is the vetted view of the effective
  configuration and its provenance);
- an agent's export bundle (the `agent` and `agent_prompt` sections hold it);
- the theme and sidebar, which are purely presentational.

A test fails when the dashboard serves a new `GET /api/config` route that is
neither a section nor one of these exclusions.

### GitHub API usage

`github_api_usage` answers "why does this hive keep running out of GitHub API
calls?". It returns what the dashboard shows on the GitHub API card and under
Debug → GitHub API budget, plus the last hour summed per caller.

The tool reads `GET /api/gh-rate-limits?usage=1`, the dashboard's own endpoint,
as the caller. That endpoint asks for a signed-in caller and nothing more, and
the tool adds no role floor of its own.

**It spends no core API call.** The hive makes one `GET /rate_limit`, which
GitHub does not charge to any bucket. Everything else in the answer is the
hive's own in-memory count of the requests it made in the last hour.

| Field                 | What it holds                                                                                                                                 |
| --------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `core`                | The core REST bucket: `limit`, `remaining`, `reset`, `observed_at` (when the hive last saw that reading) and `reset_in_seconds`               |
| `api_budget`          | `mode`, `since` (when the mode last changed), `skipped_steps`, and the configured `reserve`, `critical` and `agent_reserve_floor` thresholds  |
| `top_consumers`       | The most charged rows, one per caller, method and endpoint: `requests`, `charged`, `not_modified`, `rate_limited`, `last_status`, `last_seen` |
| `top_consumers_total` | How many such rows the hour holds. When it is more than `limit`, `top_consumers_truncated` is `true`                                          |
| `by_caller`           | The same hour summed per caller: `requests`, `charged`, `not_modified`, `rate_limited`. Never cut by `limit`                                  |
| `window_totals`       | The whole hour summed. `by_caller` adds up to it                                                                                              |
| `agent_caps`          | The fleet cap, and each agent's proxy cap, where the cap comes from, and its calls in the current hour                                        |
| `etag_cache`          | `hits`, `misses`, `entries`, and the fill level (`bytes`, `max_bytes`, `max_entries`, `evictions`)                                            |
| `graphql`, `search`   | The other two buckets: `limit`, `remaining`, `reset`                                                                                          |
| `window_seconds`      | How far back the counts look: `3600`, one hour                                                                                                |

`limit` (default 20, maximum 50) sets how many `top_consumers` rows come back.
It does not cut `by_caller` or `window_totals`.

**Reading the counts**

- `requests` is every request a caller made.
- `charged` is the requests GitHub billed: every answer except a `304` and
  `GET /rate_limit`. This is what spends the budget.
- `not_modified` is the conditional requests GitHub answered `304`. They are
  free. A caller with many requests and few charged is polling cheaply.
- `rate_limited` is the answers refused because a limit was used up.
- A row on `/graphql` is charged to the GraphQL bucket, not to core.

**Reading the callers**

- `agent:<name>` is an agent, going through the hive's GitHub proxy.
- `hive:<name>` is one of the hive's own sweeps or steps, such as
  `hive:eval_cycle`. See
  [the sweep names](operator-reference/sweeps-and-operations.md#github-api-budget-governor).
- Plain `hive` is what none of those names cover.

Start with `by_caller`. A caller that spreads its calls over many endpoints
can be small in `top_consumers` and still be the biggest spender.

**The budget modes**

| Mode       | When                                                    | What the hive does                                                                  |
| ---------- | ------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| `normal`   | `core.remaining` is at or above `reserve` (default 800) | Everything runs                                                                     |
| `conserve` | `core.remaining` is below `reserve`                     | Skips the optional sweeps and slows the eval loop. Merging and unsticking still run |
| `critical` | `core.remaining` is below `critical` (default 250)      | Also skips the unstick steps, and slows the eval loop further                       |

- `skipped_steps` names what the last cycle left out.
- The hive leaves `conserve` or `critical` once `remaining` is 100 calls above
  the threshold, or when the bucket resets.
- Below `agent_reserve_floor` (default 400) the proxy refuses agent reads,
  whatever each agent's own cap says.

See [GitHub API quota](operator-reference/runtime-and-quota.md#github-api-quota)
for the caps and the floor, and
[GitHub API budget governor](operator-reference/sweeps-and-operations.md#github-api-budget-governor)
for the modes.

**An older hive.** A hive built before this tool answers with `usage_view`
`false`, its fixed ten `top_consumers`, and a `note` saying that `by_caller`,
`window_totals`, `agent_caps` and `window_seconds` are missing. Upgrade the
hive for the full answer.

### What a read masks

Every read result is scrubbed in two ways.

- **By shape:** GitHub tokens, JWTs, bearer tokens, private keys and similar
  are replaced in any string, wherever they appear.
- **By key:** a value under a key that contains `token`, `secret`, `password`,
  `credential` or `authorization` is replaced by `[masked:...]`, unless it
  cannot hold a credential:
  - `true` or `false`, `null`, or an empty string, list or object;
  - a timestamp (`token_expires_at`);
  - a token count or rate (`totalTokens`, `tokens_per_coin`);
  - the self-authorization hold settings (`selfAuthorizationHold`,
    `repoSelfAuthorizationHold`), which are a merge policy and not a header.

The name of an environment variable under a key such as `token_env` is a
string like any other there, so it is masked too when it is set.

How to read the result:

- `[masked:token]` means a value is there and was withheld. An empty value
  means nothing is set.
- A list or object with content under such a key is masked whole (for example
  `credentialWarnings` when there are warnings); read it on the dashboard.
- The one exception is a token count block, an object under a count key such
  as `tokens`: it is read like any other object, and the keys inside it are
  judged by the same rules.

### Diagnostic reads

These read-only tools (#11943) show what an operator would otherwise need a
shell for.

- They work the same over HTTP and stdio.
- Each one wraps a dashboard endpoint and keeps that endpoint's role.
- Every result is scrubbed the way `log_read` lines are, stays under 128 KiB,
  and says what it cut. The shared `limit` does not apply.
- Results carry the `untrusted_content` marker.

| Tool                    | Wraps                                                | Required role                                       | Returns                                                                                                                                |
| ----------------------- | ---------------------------------------------------- | --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `agent_log_read`        | `GET /api/agents/{name}/log`                         | Any authenticated role                              | The agent's live terminal log: the newest `max_bytes` (default 64 KiB, max 128 KiB); needs `agent`, optional `explain` (`only`/`hide`) |
| `agent_kicks_list`      | `GET /api/agents/{name}/kicks`                       | Any authenticated role                              | The agent's archived kick logs, newest first; needs `agent`                                                                            |
| `agent_kick_read`       | `GET /api/agents/{name}/kicks/{id}`                  | Any authenticated role                              | One archived kick log, same bounds as `agent_log_read`; needs `agent`, `kick_id`                                                       |
| `run_log_read`          | `GET /api/runs/{key}/log`                            | Read                                                | A run's stage log, same bounds; needs `run`, optional `stage`, `gen`, `tail`                                                           |
| `run_detail_read`       | `GET /api/runs/{key}/detail`                         | Any authenticated role (pane tails need read-write) | A run's detail; the oldest events, stages, PRs and notes and the oldest raw log bytes are dropped first; needs `run`                   |
| `health_deep_read`      | `GET /api/health/deep`                               | None (public)                                       | The deep health check, whole                                                                                                           |
| `prompt_history_read`   | `GET /api/prompt-history`                            | Read-write                                          | Delivered prompts, newest first; each prompt cut at 8 KiB (`cut`, `full_bytes`); optional `agent`, `window_hours`                      |
| `config_effective_read` | `GET /api/diag/config` (as `/api/config/provenance`) | Owner, verified by the hive                         | Every configuration key and environment name, with the layer that set it; optional `offset`                                            |

**Plain-text logs** come back as `{text, content_type, truncated, bytes_total,
bytes_returned, max_bytes, redactions}`. The dashboard's own plain-text
responses are not changed.

**Refusals:** a refused read returns the typed forbidden error. The error never
includes the hive's response body.

**`config_effective_read`:**

- **Values shown:** only for the keys and names listed below, which are known
  not to hold secrets. Every other key or environment variable is shown only as
  set or not set. A listed value is still scrubbed, and a listed URL that holds
  a login is withheld.
- **Provenance:** the layer that set each key, any lower layers it overrode
  (`also_set_by`), a rejected overlay and its reason, the last-good fallback,
  and the GitHub ratchet.
- **Never returned:** the configuration file itself.
- **Owner check:** over HTTP, the hive must itself have authenticated the caller
  as owner, with the dashboard token or a verified owner session. An owner role
  claimed only in request headers is refused with the typed forbidden error
  `ErrOwnerUnverified`.

The keys and names whose values are shown:

- **Configuration keys:** `acmm_level`, `hive_id`, `project.org`,
  `project.name`, `project.repos[*]`, `project.primary_repo`,
  `project.ai_author`, `project.forge`, `hub.enabled`, `hub.url`,
  `hub.is_public`, `hub.hive_type`, `hub.auto_upgrade`, `hub.auto_upgrade_mode`,
  `github.app_id`, `github.installation_id`, `github.app_slug`,
  `github.api_url`, `github.base_url`, `governor.eval_interval_s`,
  `governor.logging.dir`, `governor.logging.level`,
  `governor.logging.max_size_mb`, `governor.logging.max_age_days`,
  `governor.logging.max_backups`, `governor.logging.compress`,
  `governor.logging.kick_log_source`, `governor.logging.read_scan_max_mb`,
  `agents.*.backend`, `agents.*.model`, `agents.*.enabled`, `agents.*.paused`,
  the API proxy's GitHub API hourly cap `agents.github_api_hourly_cap` and
  `agents.*.github_api_hourly_cap`, and the per-repository merge policy: `auto_merge.squash_only[*]`,
  `auto_merge.require_current_base[*]` and `auto_merge.human_merge_paths.*[*]`
  (the path globs listed under each `owner/repo`). A `*` segment is one map key
  and `[*]` any list index.
- **Per-agent GitHub API caps:** the result also carries
  `agent_github_api_hourly_caps`, the API proxy's cap as this configuration
  resolves it: `fleet` and `fleet_set` for `agents.github_api_hourly_cap`,
  `agents` mapping each declared agent to the cap the proxy applies to it
  (`0` = no cap), and `overrides` naming the agents with their own value.
- **Environment names:** `HIVE_CONFIG`, `HIVE_ADMIN_MCP_ENABLE_WRITES`,
  `HIVE_ADMIN_MCP_PENDING_FILE`, `HOME`, `HOSTNAME`, `LANG`, `PATH`, `TZ`,
  `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT`

See [design/admin-mcp.md](design/admin-mcp.md#diagnostic-reads).

### Start records, crash traces and out-of-memory evidence

These tell an operator with no host shell whether the hive crashed, was killed
or ran out of memory (#11945). A hive whose process is down cannot be read
through the MCP at all; read the evidence once it is back.

**`starts` log source.** Read it with `log_read` and `source: starts`. Each hive
start writes one line, with its short identifier (`start`, 8 hex characters),
its start time, and how the previous run ended in `previous.verdict`:

| `previous.verdict`    | Meaning                                                                                                                                  |
| --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `clean`               | It finished shutting down.                                                                                                               |
| `not-closed`          | It stopped without finishing shutdown, for any reason: for example SIGKILL, the out-of-memory killer, a crash or a fatal start-up error. |
| `self-upgrade-failed` | It exited after a failed self-upgrade.                                                                                                   |
| `unknown`             | There is no record of a previous run.                                                                                                    |

The line also gives:

- `previous.last_seen_alive`, refreshed about every 30 seconds;
- `previous.memory`, the last memory and out-of-memory reading taken before that
  run ended, so the count survives a container restart that resets it;
- `previous.crash_trace`, set when that run left a crash trace.

Lines for `not-closed` and `self-upgrade-failed` are `WARN`, so
`min_level: warn` lists only the bad ends. A second hive process refused at
start-up records nothing. The newest 200 starts are kept.

**Start identifier in the hive log.** Every line of the `hive` source carries
`"start":"<identifier>"`. Use `include` with the identifier to read one run.

**`crash` log source.** When the hive process dies of an unrecovered panic or a
fatal Go runtime error, the runtime's trace is kept, one file per start. Each
file opens with a line naming its start; everything after that line is the
trace. The last 10 traces are kept, each cut to at most 1 MiB.

**A process killed by SIGKILL or the out-of-memory killer leaves no trace**,
because it runs no code on the way out. Its start then reads `not-closed`, with
no `crash_trace`.

**`fleet_status`.** `systemResources` gains two fields:

- `oomKillCount`: the out-of-memory kills counted by the container's memory
  controller (cgroup `memory.events`, or `memory.oom_control` on cgroup v1). A
  missing count means the controller does not report one.
- `memLimit`: a size such as `2048 MiB`, or `unlimited`, or `unknown` when no
  memory controller is readable.

The existing fields, which the web UI reads, are unchanged.

**Where the files live.** In `HIVE_OUTPUT_LOG_DIR` (default `/data/.hive/logs`).
They are `0600` in a `0700` directory owned by the hive's runtime user, so agent
users cannot read them. Nothing new is mounted, published or installed on the
host. The gateway and the container start-up script are unchanged.

### What still needs a host shell

Some evidence exists only outside the hive container, and the admin MCP cannot
read it. The table gives the closest in-container substitute.

| Host-only evidence                                                                                                                 | In-container substitute                                                                                                                                                                                     |
| ---------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Unit restart counts (`systemctl show hive -p NRestarts`, Quadlet)                                                                  | `starts`: one line per start, with how the run before it ended                                                                                                                                              |
| Container inspect (`podman inspect` / `docker inspect`: exit code, `OOMKilled`, restart count)                                     | `starts` (`not-closed`, `self-upgrade-failed`, `previous.memory`) and `fleet_status` `systemResources.oomKillCount` / `memLimit`                                                                            |
| Kubernetes describe and events (`kubectl describe pod`, `kubectl get events`: last termination reason, probe failures, scheduling) | `starts` for how each run ended, and `health_deep_read` for what the probes see. Scheduling and probe events have none                                                                                      |
| Node-level out-of-memory kills and evictions (kernel log, kubelet evictions)                                                       | `oomKillCount` counts kills inside the hive container only. A kill or eviction that ends the hive shows as `not-closed` in `starts`, with the last reading before it. Kills elsewhere on the node have none |
| SELinux denials (`ausearch -m avc`, the host audit log)                                                                            | None. The refused operation's error may appear in the `hive` source                                                                                                                                         |
| The gateway's own logs                                                                                                             | None. `requests` shows what reached the dashboard; requests the gateway refused are not in it                                                                                                               |
| The container start-up script's output                                                                                             | None for the script itself. `hive-output` keeps what the hive prints before its log is set up, and `proxy` keeps the auth proxy's output                                                                    |
| The web terminal server's output                                                                                                   | None. `agent_log_read` reads what the agent terminals show                                                                                                                                                  |

**The role gate does not limit dashboard-token holders.** Whoever holds the
dashboard token is the owner, and every read and write role check passes for an
owner. Treat the token as owner access to all of the evidence above.

## Writes

Writes are off until someone turns them on.

- **On the HTTP endpoint** (`/api/admin/mcp`), the hive's owner turns them on
  or off with **Allow admin MCP writes** on Settings → Channels. It is stored as
  `admin_mcp.writes_enabled`, applies on the next MCP call with no restart, and
  each change is recorded in the audit log as `config_admin_mcp_writes` with
  who made it and the old and new value. Only the owner can change it. Once
  used, the switch overrides `HIVE_ADMIN_MCP_ENABLE_WRITES` in both directions.
  Until then that variable on the hive process decides, as before: `1`, `true`,
  `yes` or `on` turns writes on, and anything else, or no variable, leaves them
  off. **Use environment default**, shown while the switch is in effect, clears
  it again so the variable (or the default, off) decides; the tab shows what
  that would give before you save, and the reset is audited the same way. A
  hosted hive's owner no longer needs the hub operator to set the variable.
- **On the stdio binary**, `HIVE_ADMIN_MCP_ENABLE_WRITES` in the binary's own
  environment is the only switch. It is a local process with no hive config,
  and its writes call the hive's REST endpoints with the dashboard token, so
  the dashboard switch does not apply to it.

Writes are also unavailable to a caller whose role on the hive is read-only,
whatever the switch says.

Every write takes two steps:

1. **`write_preview`** with `operation` and `args`. Nothing changes. You get a
   `confirmation_id`, an expiry, and a description of what would happen.
2. **`write_confirm`** with the `confirmation_id`. The confirmation is
   single-use and expires after 10 minutes. Only the caller who previewed a
   write can confirm it. The HTTP endpoint checks the writes switch again at
   this step, so a confirmation previewed before the owner turned writes off is
   refused. An operation built from the current settings (`auto_merge.update`)
   reads them again here and is refused if they changed after the preview.

### Who a write runs as

A confirmed write runs as you.

- **On the HTTP endpoint**, that is the identity the hive verified on your
  request: your login when you come through a dashboard session or the hub, at
  your role on that hive. The endpoint's own permission checks apply to you, and
  the audit log and any comment the write leaves name you.
- **Your own rights are the limit.** An operation can do no more than the same
  button on the dashboard would let you do. For example, approving a pull
  request or deciding an item needs write access to that repository, checked
  against your own GitHub permission: with read access the approve is refused
  and nothing is written. A read-only role on the hive cannot write at all.
- **With the dashboard token** (the stdio binary, or an HTTP client sending the
  dashboard token), writes are recorded as the configured owner's login when
  `dashboard.authorized_users` names an owner, and as `owner` otherwise.

The audit log cannot tell an assistant-mediated write from a dashboard action.

### Registered operations

| Area                         | Operations                                                                                                                                                                                                            |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Agents                       | `agent.pause`, `agent.resume`, `agent.nudge`, `agent.restart`, `agent.add`, `agent.remove`, `agent.model`, `agent.backend`, `agent.effort`, `agent.interaction_tier`, `agent.github_api_hourly_cap`, `agent.cadences` |
| GitHub                       | `github.agent_api_hourly_cap`                                                                                                                                                                                         |
| Fleet                        | `fleet.autonomy_level`                                                                                                                                                                                                |
| Plans                        | `plan.propose`, `plan.approve`, `plan.reject`                                                                                                                                                                         |
| Governor                     | `governor.feature_settings`, `governor.general_settings`, `governor.thresholds`, `governor.threshold_scaling`                                                                                                         |
| Issue filter                 | `project.issue_filter`                                                                                                                                                                                                |
| Sentinel                     | `sentinel.settings`                                                                                                                                                                                                   |
| Write-back                   | `writeback.kind_policy`                                                                                                                                                                                               |
| Merge settings               | `auto_merge.update`                                                                                                                                                                                                   |
| Let hive decide              | `decisions.delegate_settings`                                                                                                                                                                                         |
| Review settings              | `review.settings`                                                                                                                                                                                                     |
| Repositories                 | `repository.pause`, `repository.resume`, `repository.item_hold`                                                                                                                                                       |
| Activity monitor row actions | see below                                                                                                                                                                                                             |
| Budget                       | `budget.update`, `budget.reset`, `budget.ignore`                                                                                                                                                                      |
| Contributor queue            | `contributor_queue.order`, `contributor_queue.hold`, `contributor_queue.hold_clear`                                                                                                                                   |
| Contributors                 | `contributor.trust`, `contributor.agent_role`, `contributor.agent_role_grants`, `contributor.revoke`, `contributor.requeue`, `contributor.delete`                                                                     |
| Backup                       | `backup.create` (the archive itself is not returned through MCP)                                                                                                                                                      |
| Circuit breaker              | `circuit_breaker.engage`, `circuit_breaker.release`                                                                                                                                                                   |

The two GitHub API cap operations change the API proxy's per-agent hourly
cap, the same settings as the dashboard's Forge App and agent General
controls:

- `agent.github_api_hourly_cap` takes `agent` and `cap`: a whole number, `0`
  for no cap, or `inherit` to follow the fleet default. Set it on a base
  agent; a replica or review head is refused, because it takes its base
  agent's cap.
- `github.agent_api_hourly_cap` takes `cap`: a whole number, `0` for no cap,
  or `default` to remove `agents.github_api_hourly_cap` so the default of 300
  applies.

The preview reads the current value first. Raising a cap, removing one, or
inheriting a higher fleet value is disclosed as widening, naming the App
installation's shared hourly bucket and the agent reserve floor
(`github.agent_reserve_floor`); lowering is not. When the current value cannot
be read, the preview says the change may be widening. See
[Changing the agent caps](operator-reference/runtime-and-quota.md#changing-the-agent-caps).

The issue filter and Sentinel operations change the same settings as
Settings → Labels and Settings → Security, through the endpoints those tabs
save with. Only the hive's owner can confirm them: the endpoint refuses any
other role.

- `project.issue_filter` changes `project.issue_filter`: `require_labels`, the
  `hard_suppress_labels` buckets (`needs_human`, `needs_direction`,
  `needs_decision`, `needs_spec`), and `reporter_trust` (`enabled`,
  `trusted_associations`, `trusted_logins`, `untrusted_require_labels`).
- `sentinel.settings` changes Sentinel's `sensitive_paths`,
  `disabled_behaviors`, `exempt_logins` and `trusted_authors_block`. A
  repository with its own list
  (`project.repo_policies[].sentinel_sensitive_paths`) keeps it when
  `sensitive_paths` changes: the preview names those repositories and the
  hive-wide globs their lists leave out. A per-repository list added or
  changed after the preview refuses the confirm.
- `sentinel.settings` also sets one repository's own list, with
  `repo_sensitive_paths`: `{"owner/repo": [every glob for it]}`. `[]` flags
  no path there, and `null` removes the repository's list so it uses the
  hive-wide list again. Only the repositories you name change; the
  hive-wide list does not. A blank or malformed glob, or a repository that
  is not in `project.repos`, is refused.

Each list takes a change, not a new list: `{"add": [...], "remove": [...]}`,
or `{"clear": true}` to empty it first. For example, to turn reporter trust on
and trust one more reporter:

```json
{"operation": "project.issue_filter",
 "args": {"reporter_trust": {"enabled": true, "trusted_logins": {"add": ["ext-maint"]}}}}
```

The preview reads the current settings, shows each list before and after, and
sends the merged list. Removing an entry that is not there is refused. If the
settings change between the preview and the confirm, the confirm is refused
and nothing is written; preview again. Emptying a list that has a default (a
hard-suppress bucket, `trusted_associations`, `untrusted_require_labels`,
`sensitive_paths`) puts the default back, and the preview shows it.

The preview says plainly when a change lets more through:

- **More issues admitted without triage:** `require_labels` cleared or given
  another label, reporter trust turned off, trusted logins, associations or
  triage labels added, or a hard-suppress label removed. Turning reporter
  trust on is not widening: it makes issues from untrusted reporters wait for
  a triage label.
- **Fewer pull requests flagged by Sentinel:** sensitive paths removed, a
  behavior disabled, an exempt login added, or `trusted_authors_block` turned
  off. The preview names the pull requests that would no longer be held. For
  a repository's own list it names the globs per repository, for example
  `Sentinel stops flagging Justfile, **/requirements*.txt in org/a`.

On a hive managed by a hub, the issue filter preview also says the change may
be overwritten. If the hub's record for the hive carries an issue filter, the
next heartbeat replaces the whole filter with the hub's copy, and the hive
cannot tell from its side whether the record carries one.

Neither operation itself adds or removes a label, approves, or merges
anything. Turning reporter trust on does make later scans mark each held
issue as waiting for triage, with a label and a one-time comment.

`agent.cadences` and `writeback.kind_policy` change two more settings an
owner otherwise changes on the dashboard, through the endpoints those
controls save with. Only the hive's owner can confirm them: the endpoint
refuses any other role.

- `agent.cadences` sets one agent's cadence in one or more governor modes,
  through `PUT /api/config/agent/{name}/cadences` (the agent's Cadences
  settings). It takes `agent` and `modes`, an object with any of `surge`,
  `busy`, `quiet` and `idle`, each a duration such as `1h` or `90m`, or
  `pause`. Modes you do not name keep their cadence. A mode the hive does not
  configure is refused, because the endpoint would skip it. Time-of-day and
  cron schedules are still set on the dashboard. Each mode it sets becomes
  operator-owned (`governor.cadence_owners`), as a dashboard edit does, so a
  later ACMM level change (`fleet.autonomy_level`) or pack apply no longer
  resets it to the pack default. The preview says so.
- `writeback.kind_policy` changes the write-back gate's per-kind overrides
  (`writeback.kinds.<kind>`: `min_level`, `require_label`,
  `unlabeled_min_level` and `own_only`), through
  `PUT /api/config/governor/features` (Settings → Features). `kinds` maps a
  kind to the fields to set, with `null` for a field dropping it, or to `null`
  to remove that kind's override. An unknown kind or field is refused. That
  endpoint replaces the whole override map it is sent, so the preview reads
  the current overrides and sends them all with the change merged in: kinds
  you do not name keep their override.

For example, to run the scanner hourly when the queue is busy or surging, and
to let the stale-red branch refresh reach pull requests without the opt-in
label at L6:

```json
{"operation": "agent.cadences",
 "args": {"agent": "scanner", "modes": {"surge": "1h", "busy": "1h"}}}
```

```json
{"operation": "writeback.kind_policy",
 "args": {"kinds": {"base_refresh": {"unlabeled_min_level": 6}}}}
```

The preview shows each mode or kind before and after. It names as widening a
cadence that gets shorter ("scanner runs more often in surge: more spend"),
and a kind that would write at a lower level, without the opt-in label, or on
items the hive did not author. If the agent's cadences or the overrides
change between the preview and the confirm, the confirm is refused; preview
again. Neither operation kicks an agent, labels, approves or merges anything
itself.

The dashboard's write-back kind table (Settings → Features → Write-back
containment) edits the same four fields. Saving there sends every kind's
override from the table, and a field nobody changed on the page is sent as
it was, so an override written by `writeback.kind_policy` survives a later
dashboard edit.

`auto_merge.update` changes merge settings, the same settings as the
dashboard's merge controls, through `PATCH /api/config/auto-merge`. It is owner
only.

- **Pass only what changes,** shaped like the endpoint's body, for example
  `{"squash_only": {"add": ["org/c"]}}` or
  `{"contributor_lane": {"repos": {"remove": ["org/a"]}}}`. A list takes `add`
  and `remove`. `human_merge_paths` takes `owner/repo` and every pattern that
  repository should hold; `null` or `[]` removes the repository.
  `human_merge_pin_only` takes `owner/repo` and that repository's whole
  pin-only exception, `{"enabled": true, "paths": [...], "kinds": [...],
  "trusted_authors": [...]}`; `null` removes it. An enabled exception needs
  `human_merge_paths` for the repository, set already or in the same change.
  `kinds` takes the same list the dashboard offers (`action-sha`,
  `image-digest`, `version-pin`, `action-sha-ref`), and the schema says what
  each admits. `action-sha-ref` lets a bare `key: <40-hex>` line follow a
  `uses:` pin bumped in the same pull request; it needs `action-sha`, and the
  preview says so when an exception lists it without.
  An unknown field is refused.
- **One repository's workflow fence scope:**
  `{"contributor_lane": {"workflow_fence_scope_repos": {"org/a": "human_merge_paths"}}}`
  gives `org/a` its own scope (`project.repo_policies[].workflow_fence_scope`,
  `all` or `human_merge_paths`); `null` removes it so the repository uses the
  hive-wide `workflow_fence_scope` again. The endpoint takes this map per
  repository, so the preview sends only the repositories that change; the
  hive-wide scope is never changed. A hive too old to take it says so, and
  the preview is refused.
- **Entries you do not name are kept.** The endpoint replaces each list and
  map it is sent, so the preview reads the current settings and sends the
  merged whole value. It shows every change before and after.
- **A stale preview is refused.** `write_confirm` reads the settings again and
  refuses if any setting the change sends was changed after the preview.
  Preview again to build on the new value.
- **Widening is disclosed.** The preview says when a change lets the hive
  merge more on its own: turning on the trusted-author lane, removing a
  human-merge path, adding a repository to the native merge queue, emptying a
  repository allow-list (empty means every repository), and the like. Turning
  a pin-only exception on, or giving it more paths, kinds or trusted authors,
  is widening too, and the preview names what merges as a result, for example
  "pin bumps (action-sha) to .github/workflows/*.yml in org/a by renovate[bot]
  will merge without a hand merge". Removing any of those is not. A
  repository whose fence scope becomes `human_merge_paths` (its own, or the
  hive-wide one after its own is removed) is named: "workflow edits outside
  human-merge paths in org/a merge without an owner Approve". The preview
  also reminds that a repository with no `human_merge_paths` entry keeps the
  full fence, and names the repositories in the change that have none. The
  hive-wide scope and the human-merge paths it read are part of the stale
  check.
- It never merges a pull request.

`decisions.delegate_settings` changes the "Let hive decide" settings
(`decisions.delegate`) through `PUT /api/config/decisions/delegate`, the same
endpoint as Settings → Features → *Let hive decide*. It is owner only.

- **Pass only what changes:** `enabled` (the hive-wide switch), `repos`
  (`{"owner/repo": true | false | null}`, where `null` drops that repository's
  override so it follows the hive-wide switch), `llm`
  (`{"enabled": true, "model": "provider/model"}`) and `effect_timeout` (for
  example `45m`, or empty for the default). An unknown field is refused.
- **Repositories you do not name are kept.** The endpoint replaces the
  override map it is sent, so the preview reads the current settings and sends
  the merged map. It shows every change before and after.
- **A stale preview is refused,** as for `auto_merge.update`.
- **What it opens is disclosed.** Turning the LLM step on sends an item's text
  (title, body, comments and linked items) to the configured model when the
  rules leave the item at the reversible default; the preview names the model
  and says when no reviewer endpoint is configured yet. Turning the delegate on
  for a repository lets Let hive decide act there, from the dashboard and
  `repository.item_delegate`, and lets the reviewer-question sweep use it on the
  hive's own pull requests.
- `llm.model` takes a model name only. A value that looks like an API key is
  refused; the model's key stays on the governor reviewer endpoint.
- It starts no decision and merges nothing. A delegated decision does what it
  did before.

`review.settings` changes the review settings (the `review` block, Settings →
Review Gate) through `PUT /api/config/review`, the same endpoint as the
dashboard's Review Gate controls. It is owner only.

- **Pass only what changes,** shaped like the endpoint's body, for example
  `{"reviewer_autoscale": {"enabled": true, "max": 2}}` or
  `{"severity": {"block_at": "P1"}, "require_approval": true}`. Only those
  fields are sent. Every other review setting keeps its value, including one
  changed on the dashboard after the preview: the op never reads the section
  and sends it back whole.
- **Sub-objects change field by field:** `reviewer_autoscale`, `severity`,
  `backlog`, `contributor_prs` and `review_bots`. A list (`reviewer_agents`,
  `perspectives`, `require_labels`, ...) and `perspective_prompts` are
  replaced by the value you give. `verdict_labels` and `recommendations` are
  replaced whole by the endpoint, so a key you leave out of them is cleared
  (`recommendations.labels` is kept); the preview shows each cleared key.
- **An unknown field is refused,** at the top level or inside a sub-object,
  and the refusal lists the accepted names, so a typo cannot silently change
  nothing. `null` is refused too, because the endpoint reads it as no change.
  The accepted fields are the endpoint's own: a test fails when the
  endpoint's body and the op's field list differ.
- **The preview shows each named field before and after,** read through
  `settings_read` section `review` (`review_bots.min_priority` from section
  `governor`). A field already at the requested value is not sent. Turning
  `require_approval` off, a less strict `severity.block_at`, and turning
  `reviewer_autoscale` on or raising its `max` are disclosed as widening.
- **The endpoint's checks are its own.** A value it refuses, such as an
  autoscale `min` above `max` or an autoscale `agent` the review swarm does
  not dispatch to, comes back as its error, unchanged.
- **A stale preview is refused,** as for `auto_merge.update`, when a named
  field changed after the preview. After the write, `write_confirm` reads the
  settings back and reports the named fields as the hive now holds them
  (`after`), because the endpoint answers only `{"status":"updated"}`.
- The endpoint records a `config_review` audit entry naming you. The op
  merges, approves and closes nothing.

`governor.general_settings` changes the governor settings on Settings →
General through `PUT /api/config/governor/general-advanced`, the endpoint that
tab saves through. It is owner only. Use it to set the eval interval or to
turn [API budget pacing](operator-reference/runtime-and-quota.md#api-budget-pacing)
on or off.

- **Pass only what changes.** The fields are `eval_interval_s` (10 to 86400),
  `api_budget_pacing` (`true` or `false`), `eval_interval_max_s`,
  `eval_interval_webhook_s`, `webhook_wake_debounce_s`,
  `webhook_wake_min_gap_s`, `conserve_interval_multiplier`,
  `optional_sweep_every_n_cycles`, `provider_budget_probe_interval_s` (for
  these seven, `0` restores the default), `explain_mode` (`off`, `brief`,
  `full`, or empty to clear it) and `attribution_trailer`. An unknown field is
  refused, and the refusal lists the fields that are accepted.
- **Only the fields that differ are sent.** The preview reads the current
  settings and shows each change before and after. A field already at the
  requested value is listed as not sent; if nothing would change, the preview
  is refused.
- **A stale preview is refused.** `write_confirm` reads the settings again
  and refuses if a setting the change names moved after the preview.
- **What it opens is disclosed.** A shorter eval interval, a lower cap or
  multiplier, or turning pacing off lets the hive call GitHub more often, and
  the preview says so. Turning pacing on is not widening.
- The loop reads these settings every cycle, so a change applies from the
  next eval cycle without a restart. It kicks, merges and labels nothing.

```json
{"operation": "governor.general_settings", "args": {"api_budget_pacing": true}}
{"operation": "governor.general_settings", "args": {"eval_interval_s": 300}}
```

### Contributor queue

The three `contributor_queue.*` operations are the Operations card's queue
controls (Contribute → Operations → Contributor Queue). Each goes through the
endpoint the card uses, so it needs the owner or read-write role, as on the
card: the endpoint refuses anyone else, and a read-only caller is refused at
`write_preview`. Every change is recorded in the audit log, as a card change
is. None of them assigns, labels, approves or merges anything.

- `contributor_queue.order` sets the pinned order (`contribute_queue_order`)
  through `PUT /api/contribute/queue/order`. Pass exactly one of:
  - `order`: the whole pinned list, for example
    `{"order": ["acme/widgets#12", "acme/widgets#7"]}`. `[]` removes every pin.
  - `pin` with an optional `position`: one key inserted, or moved if it is
    already pinned, at that place in the pinned list. `position` counts from
    `0` (the default, the front); a number past the end puts it last.
  - `unpin`: one key removed from the pinned list.
- `contributor_queue.hold` holds or resumes one issue through
  `POST /api/contribute/queue/hold`. It takes `key`, `held` (`true` holds,
  `false` resumes) and, when holding, an optional `reason` of at most 200
  characters, shown in the card's on-hold badge. A held issue is never offered
  until it is resumed.
- `contributor_queue.hold_clear` resumes every held issue through
  `POST /api/contribute/queue/hold/clear`. It takes no arguments.

What the previews show and check:

- **Before and after.** The order endpoint replaces the whole list, so the
  preview reads the stored pinned list (including pins that are not in the
  queue now) and shows it before and after. `pin` and `unpin` edit that list,
  so you never retype the rest. The hold preview shows the issue's hold and
  reason before and after; the `hold_clear` preview lists every issue it will
  resume, with its reason.
- **Keys.** A key must be `owner/repo#number`. It must also be one the queue
  knows: a queue row, a withheld row, or a key already pinned or held. Any
  other key is refused in the preview and nothing is written.
- **Stale previews.** `write_confirm` reads the stored list again. For
  `contributor_queue.order`, the preview is stale when the pinned list is not
  exactly the one the preview showed, because someone saved another order in
  between and confirming would overwrite their edit unseen. For
  `contributor_queue.hold_clear`, it is stale when the hold set changed,
  because confirming would resume an issue the preview did not list. A stale
  confirm is refused and nothing is written; preview again. Issues entering or
  leaving the queue (work assigned, a cooldown ending) do not make a preview
  stale.
- **Widening.** Pinning changes only which admissible issue is offered first,
  so it is not widening. Resuming an issue is: it can be offered to
  contributors again once it passes admission.

The previews read the stored lists through
`GET /api/contribute/queue?controls=1`, which answers owner and read-write
only. A hive too old to answer it says so, and the preview is refused.

### Activity monitor row actions

The Activity monitor's row actions:

| Operation                               | Button                                                                                                                     |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `repository.rescan`                     | Re-evaluate now                                                                                                            |
| `repository.item_accept_triage`         | Accept triage                                                                                                              |
| `repository.item_needs_human`           | Mark needs-human                                                                                                           |
| `repository.item_blocker_recheck`       | Re-check permissions                                                                                                       |
| `repository.item_blocker_handoff`       | Hand to workflows lane                                                                                                     |
| `repository.item_blocker_close_partial` | Mark partially-addressed and close                                                                                         |
| `repository.item_decision`              | Decide: approve, direction, close-superseded, close-wontfix, close-read, close-issue, adopt, adopt-now, re-author, reroute |
| `repository.item_delegate`              | Let hive decide; read the outcome with `delegated_decision_status`                                                         |

The merge answers are not available, and a delegated decision made here never
queues an auto-merge.

Anything not listed is refused. The assistant must not approximate it by
combining other operations.

## Troubleshooting

| Symptom                                                        | Cause and fix                                                                                                                                                                                                                                                                                                                                                                                                               |
| -------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| "confirmation was previewed by another caller"                 | Only the caller who ran `write_preview` can confirm it. Preview the write again yourself.                                                                                                                                                                                                                                                                                                                                   |
| A write is refused, or `metadata.preview.enabled` is `false`   | Read the note on the write tools, or the reason `exclusion_catalogue` returns. On the HTTP endpoint, ask the owner to turn on **Allow admin MCP writes** on Settings → Channels (it overrides `HIVE_ADMIN_MCP_ENABLE_WRITES` once used). On the stdio binary, set `HIVE_ADMIN_MCP_ENABLE_WRITES=1` in its environment. Otherwise the caller has a read-only role on this hive, or the request carries no verified identity. |
| `select_hive` fails: "unreachable host"                        | Check the address and network.                                                                                                                                                                                                                                                                                                                                                                                              |
| `select_hive` fails: "wrong dashboard token"                   | The roster token does not match that hive.                                                                                                                                                                                                                                                                                                                                                                                  |
| `select_hive` fails: "direct-route spoke refused shared token" | That spoke needs per-user access and cannot be administered with a token.                                                                                                                                                                                                                                                                                                                                                   |
| 401 or 403 from a hive hosted by the hub                       | The GitHub token is invalid or expired, or your login has no role on that hive. Check the token with `curl -sH "Authorization: Bearer <token>" https://api.github.com/user`, and ask the hive's owner for access.                                                                                                                                                                                                           |
| 405 on the HTTP endpoint                                       | You sent a GET; use POST.                                                                                                                                                                                                                                                                                                                                                                                                   |
| Confirmation expired, or wrong hive                            | Run `write_preview` again on the hive you want to change.                                                                                                                                                                                                                                                                                                                                                                   |

For the design record behind this feature, see
[`design/admin-mcp.md`](design/admin-mcp.md).
