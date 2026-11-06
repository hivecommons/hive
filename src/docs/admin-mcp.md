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

| Transport              | Where it runs                                | Use it when                                                                                                       |
| ---------------------- | -------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| **HTTP** (recommended) | `POST /api/admin/mcp` on the dashboard       | Your client accepts a plain JSON-RPC HTTP endpoint. Add one entry per hive.                                        |
| **Stdio**              | the `hive-admin-mcp` binary on your machine  | Your client cannot use the HTTP endpoint, or you want one entry for several hives (`select_hive` is stdio-only). |

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

| Your hive                                                             | Credential                                       | Header                                    | You act as                                            |
| --------------------------------------------------------------------- | ------------------------------------------------ | ----------------------------------------- | ----------------------------------------------------- |
| Hosted by the hub (`https://<hive>.hive.hivecommons.dev`)             | Your GitHub token                                | `Authorization: Bearer <GitHub token>`    | Yourself, at your role on that hive                   |
| Self-hosted, `dashboard.auth_token` set, no `authorized_users`        | The dashboard token                              | `Authorization: Bearer <dashboard token>` | The owner (the configured owner's login, or `owner`) |
| Self-hosted spoke with an `authorized_users` allowlist (direct-route) | Your per-user session from `hivectl --server URL login` | `Cookie: hive_session=<session>`   | Yourself, at your allowlist role                      |

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

| Variable                       | Required | Meaning                                                                                                                                                                                         |
| ------------------------------ | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `HIVE_ADMIN_MCP_HIVES`         | Yes      | JSON array of `{"name","address","token"}`. All three fields are required and names must be unique. The binary exits with an error otherwise.                                                 |
| `HIVE_ADMIN_MCP_ACTIVE`        | No       | Name of the hive active at start. Defaults to the first entry; an unknown name is an error.                                                                                                    |
| `HIVE_ADMIN_MCP_ENABLE_WRITES` | No       | `1`, `true`, `yes` or `on` turns write tools on. Anything else leaves them off.                                                                                                                 |
| `HIVE_ADMIN_MCP_PENDING_FILE`  | No       | Where pending write confirmations are stored. Default on stdio: `<user config dir>/hive/admin-mcp-pending-confirmations.json`. Default on the HTTP endpoint: `/data/admin-mcp-pending-confirmations.json`. |

The dashboard process also reads `HIVE_ADMIN_MCP_ENABLE_WRITES` and
`HIVE_ADMIN_MCP_PENDING_FILE` for the HTTP endpoint.

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

| Tool                 | Returns                                                                                   |
| -------------------- | ----------------------------------------------------------------------------------------- |
| `hive_status`        | Hive status summary                                                                       |
| `fleet_status`       | Fleet state, including memory limit and out-of-memory kills (see note 1)                  |
| `version_read`       | Running build from `/api/version` (see note 2)                                            |
| `autonomy_readiness` | Autonomy readiness                                                                        |
| `spend_read`         | Spend                                                                                     |

**Agents, runs and plans**

| Tool                 | Returns                            |
| -------------------- | ---------------------------------- |
| `agents_list`        | Agents                             |
| `runs_list`          | Runs                               |
| `leases_list`        | Leases                             |
| `claims_list`        | Issue claims                       |
| `plans_list`         | Plans                              |
| `contributors_list`  | Contributors                       |
| `agent_nudge_status` | Outcome of a nudge; needs `agent`  |

**Issues, pull requests and the Activity monitor**

| Tool                        | Returns                                                                                                                                    |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `issues_by_band`            | Issues grouped by band; optional `repo`, `band`, `stale`, `held`, `offset`                                                                 |
| `prs_by_band`               | Pull requests grouped by band; optional `repo`, `band`, `stale`, `held`, `offset`                                                          |
| `stage_tracker_read`        | The Activity monitor (see note 3)                                                                                                          |
| `delegated_decision_status` | Status and outcome of the latest "Let hive decide" on one item; needs `owner`, `repo`, `number`                                            |
| `review_queue`              | The your-turn items (the dashboard inbox's N plus M), parked issues included, in `inbox_rank` order, each with its verb, reason code and reason; optional `repo`, `offset` |

**Settings, knowledge and advice**

| Tool                      | Returns                                                                                                                                                    |
| ------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `settings_read`           | A dashboard Settings view, read through the endpoint the Settings page uses, as the caller; optional `section` (default `governor`) and `agent` for the per-agent sections. See [Settings sections](#settings-sections) |
| `knowledge_read`          | Knowledge entries                                                                                                                                          |
| `hive_advisor`            | Hive advice                                                                                                                                                |
| `advisor_records`         | Advisor records                                                                                                                                            |
| `governor_setup_proposal` | Proposed governor setup: each setting with a reason, and the write operation and args that apply it                                                       |
| `audit_log`               | Audit entries                                                                                                                                              |

**Logs**

| Tool          | Returns                                                                                                    |
| ------------- | ---------------------------------------------------------------------------------------------------------- |
| `log_sources` | The log sources this hive can read, each marked available or not with a reason (see note 4)               |
| `log_read`    | The hive's own log, or another `log_sources` source (see note 5). Needs the read-write role                |

**Notes**

1. **`fleet_status`:** `systemResources` also carries the container's memory
   limit (`memLimit`, `unlimited` when none is set, with `memLimitBytes`) and
   out-of-memory kill count (`oomKillCount`). See
   [Start records, crash traces and out-of-memory evidence](#start-records-crash-traces-and-out-of-memory-evidence).
2. **`version_read`:** returns `currentCommit`, `branch`, `channel`,
   `commitsBehind` and `behind`.
   - Over stdio it also returns `stdio_build` (`version`, `sha`, `short`,
     `branch`) and `skew` (true when the stdio and hub short SHAs are both known
     and differ).
   - Over HTTP it also returns `last_seen_stdio_build` and `last_seen_stdio_at`
     alongside `hub_build`.
3. **`stage_tracker_read`:** returns the items with their stage, reason, what
   they wait on, decision question and options, linked pull requests and issues,
   blocker and hold, plus `stages[]` with a count per stage.
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

**Meta tools**

- `exclusion_catalogue` lists what the admin MCP will never do, and why.
- `refuse_operation` explains the refusal for a named operation.

Write tools (`write_preview`, `write_confirm`) appear only when writes are on.
On stdio, `select_hive` is also available.

### Settings sections

`settings_read` reaches every view the Settings dialog shows.

- It takes a section name, never a path.
- Each section is the endpoint that Settings tab reads, so the answer matches
  the page for the same caller. Most sections need a verified owner, as the tabs
  do.
- Secret values are never returned. Settings name the environment variable,
  file or header that supplies a secret, and the answer is scrubbed like every
  other read.

The sections:

- **Global settings:** `governor` (the default: modes, thresholds, labels,
  repos, budget, notifications, health, sensing, logging, security, features,
  hub, LiteLLM, watchdog, trajectory, classifier, release sentinel), `project`,
  `github_apps`, `authorized_users`, `dashboard`, `channels`, `escalation`,
  `auto_merge`, `review`, `convergence`, `sla`, `advisor`, `advisory`,
  `replan`, `question_autoclose`, `question_autoclose_schedule`,
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

### Diagnostic reads

These read-only tools (#11943) show what an operator would otherwise need a
shell for.

- They work the same over HTTP and stdio.
- Each one wraps a dashboard endpoint and keeps that endpoint's role.
- Every result is scrubbed the way `log_read` lines are, stays under 128 KiB,
  and says what it cut. The shared `limit` does not apply.
- Results carry the `untrusted_content` marker.

| Tool                    | Wraps                                                | Required role                                       | Returns                                                                                                                         |
| ----------------------- | ---------------------------------------------------- | --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `agent_log_read`        | `GET /api/agents/{name}/log`                         | Any authenticated role                              | The agent's live terminal log: the newest `max_bytes` (default 64 KiB, max 128 KiB); needs `agent`, optional `explain` (`only`/`hide`) |
| `agent_kicks_list`      | `GET /api/agents/{name}/kicks`                       | Any authenticated role                              | The agent's archived kick logs, newest first; needs `agent`                                                                    |
| `agent_kick_read`       | `GET /api/agents/{name}/kicks/{id}`                  | Any authenticated role                              | One archived kick log, same bounds as `agent_log_read`; needs `agent`, `kick_id`                                               |
| `run_log_read`          | `GET /api/runs/{key}/log`                            | Read                                                | A run's stage log, same bounds; needs `run`, optional `stage`, `gen`, `tail`                                                   |
| `run_detail_read`       | `GET /api/runs/{key}/detail`                         | Any authenticated role (pane tails need read-write) | A run's detail; the oldest events, stages, PRs and notes and the oldest raw log bytes are dropped first; needs `run`           |
| `health_deep_read`      | `GET /api/health/deep`                               | None (public)                                       | The deep health check, whole                                                                                                   |
| `prompt_history_read`   | `GET /api/prompt-history`                            | Read-write                                          | Delivered prompts, newest first; each prompt cut at 8 KiB (`cut`, `full_bytes`); optional `agent`, `window_hours`             |
| `config_effective_read` | `GET /api/diag/config` (as `/api/config/provenance`) | Owner, verified by the hive                         | Every configuration key and environment name, with the layer that set it; optional `offset`                                    |

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
  and the per-repository merge policy: `auto_merge.squash_only[*]`,
  `auto_merge.require_current_base[*]` and `auto_merge.human_merge_paths.*[*]`
  (the path globs listed under each `owner/repo`). A `*` segment is one map key
  and `[*]` any list index.
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

| `previous.verdict`    | Meaning                                                                                                                                       |
| --------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `clean`               | It finished shutting down.                                                                                                                    |
| `not-closed`          | It stopped without finishing shutdown, for any reason: for example SIGKILL, the out-of-memory killer, a crash or a fatal start-up error. |
| `self-upgrade-failed` | It exited after a failed self-upgrade.                                                                                                        |
| `unknown`             | There is no record of a previous run.                                                                                                         |

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

| Host-only evidence                                                                                                              | In-container substitute                                                                                                                                                         |
| ------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Unit restart counts (`systemctl show hive -p NRestarts`, Quadlet)                                                               | `starts`: one line per start, with how the run before it ended                                                                                                                  |
| Container inspect (`podman inspect` / `docker inspect`: exit code, `OOMKilled`, restart count)                                 | `starts` (`not-closed`, `self-upgrade-failed`, `previous.memory`) and `fleet_status` `systemResources.oomKillCount` / `memLimit`                                               |
| Kubernetes describe and events (`kubectl describe pod`, `kubectl get events`: last termination reason, probe failures, scheduling) | `starts` for how each run ended, and `health_deep_read` for what the probes see. Scheduling and probe events have none                                                      |
| Node-level out-of-memory kills and evictions (kernel log, kubelet evictions)                                                    | `oomKillCount` counts kills inside the hive container only. A kill or eviction that ends the hive shows as `not-closed` in `starts`, with the last reading before it. Kills elsewhere on the node have none |
| SELinux denials (`ausearch -m avc`, the host audit log)                                                                         | None. The refused operation's error may appear in the `hive` source                                                                                                             |
| The gateway's own logs                                                                                                          | None. `requests` shows what reached the dashboard; requests the gateway refused are not in it                                                                                   |
| The container start-up script's output                                                                                          | None for the script itself. `hive-output` keeps what the hive prints before its log is set up, and `proxy` keeps the auth proxy's output                                       |
| The web terminal server's output                                                                                                | None. `agent_log_read` reads what the agent terminals show                                                                                                                      |

**The role gate does not limit dashboard-token holders.** Whoever holds the
dashboard token is the owner, and every read and write role check passes for an
owner. Treat the token as owner access to all of the evidence above.

## Writes

Writes are off unless `HIVE_ADMIN_MCP_ENABLE_WRITES` is set on the process that
serves the MCP. Writes are also unavailable to a caller whose role on the hive
is read-only.

Every write takes two steps:

1. **`write_preview`** with `operation` and `args`. Nothing changes. You get a
   `confirmation_id`, an expiry, and a description of what would happen.
2. **`write_confirm`** with the `confirmation_id`. The confirmation is
   single-use and expires after 10 minutes. Only the caller who previewed a
   write can confirm it.

### Who a write runs as

A confirmed write runs as you.

- **On the HTTP endpoint**, that is the identity the hive verified on your
  request: your login when you come through a dashboard session or the hub, at
  your role on that hive. The endpoint's own permission checks apply to you, and
  the audit log and any comment the write leaves name you.
- **With the dashboard token** (the stdio binary, or an HTTP client sending the
  dashboard token), writes are recorded as the configured owner's login when
  `dashboard.authorized_users` names an owner, and as `owner` otherwise.

The audit log cannot tell an assistant-mediated write from a dashboard action.

### Registered operations

| Area                       | Operations                                                                                                                                       |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| Agents                     | `agent.pause`, `agent.resume`, `agent.nudge`, `agent.restart`, `agent.add`, `agent.remove`, `agent.model`, `agent.backend`, `agent.effort`, `agent.interaction_tier` |
| Fleet                      | `fleet.autonomy_level`                                                                                                                           |
| Plans                      | `plan.propose`, `plan.approve`, `plan.reject`                                                                                                    |
| Governor                   | `governor.feature_settings`, `governor.thresholds`, `governor.threshold_scaling`                                                                |
| Repositories               | `repository.pause`, `repository.resume`, `repository.item_hold`                                                                                  |
| Activity monitor row actions | see below                                                                                                                                      |
| Budget                     | `budget.update`, `budget.reset`, `budget.ignore`                                                                                                 |
| Contributors               | `contributor.trust`, `contributor.agent_role`, `contributor.agent_role_grants`, `contributor.revoke`, `contributor.requeue`, `contributor.delete` |
| Backup                     | `backup.create` (the archive itself is not returned through MCP)                                                                                 |
| Circuit breaker            | `circuit_breaker.engage`, `circuit_breaker.release`                                                                                              |

The Activity monitor's row actions:

| Operation                                 | Button                                                                                                                                |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `repository.rescan`                       | Re-evaluate now                                                                                                                       |
| `repository.item_accept_triage`           | Accept triage                                                                                                                         |
| `repository.item_needs_human`             | Mark needs-human                                                                                                                      |
| `repository.item_blocker_recheck`         | Re-check permissions                                                                                                                  |
| `repository.item_blocker_handoff`         | Hand to workflows lane                                                                                                                |
| `repository.item_blocker_close_partial`   | Mark partially-addressed and close                                                                                                    |
| `repository.item_decision`                | Decide: approve, direction, close-superseded, close-wontfix, close-read, close-issue, adopt, adopt-now, re-author, reroute            |
| `repository.item_delegate`                | Let hive decide; read the outcome with `delegated_decision_status`                                                                    |

The merge answers are not available, and a delegated decision made here never
queues an auto-merge.

Anything not listed is refused. The assistant must not approximate it by
combining other operations.

## Troubleshooting

| Symptom                                                      | Cause and fix                                                                                                                                                                                                                       |
| ------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| "confirmation was previewed by another caller"               | Only the caller who ran `write_preview` can confirm it. Preview the write again yourself.                                                                                                                                           |
| Write tools missing, or "writes unavailable"                 | Set `HIVE_ADMIN_MCP_ENABLE_WRITES=1` on the serving process, or read the reason `exclusion_catalogue` returns: a read-only role on this hive, or a request carrying no verified identity.                                         |
| `select_hive` fails: "unreachable host"                      | Check the address and network.                                                                                                                                                                                                      |
| `select_hive` fails: "wrong dashboard token"                 | The roster token does not match that hive.                                                                                                                                                                                          |
| `select_hive` fails: "direct-route spoke refused shared token" | That spoke needs per-user access and cannot be administered with a token.                                                                                                                                                         |
| 401 or 403 from a hive hosted by the hub                     | The GitHub token is invalid or expired, or your login has no role on that hive. Check the token with `curl -sH "Authorization: Bearer <token>" https://api.github.com/user`, and ask the hive's owner for access.                |
| 405 on the HTTP endpoint                                     | You sent a GET; use POST.                                                                                                                                                                                                           |
| Confirmation expired, or wrong hive                          | Run `write_preview` again on the hive you want to change.                                                                                                                                                                           |

For the design record behind this feature, see
[`design/admin-mcp.md`](design/admin-mcp.md).
