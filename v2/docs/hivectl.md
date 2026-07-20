# hivectl

`hivectl` is the non-interactive command-line client for a running Hive
dashboard API. The `hive` binary runs the service; `hivectl` inspects and
operates it.

## Quick start

```bash
go build -o bin/hivectl ./cmd/hivectl

# Point at a server (default: http://127.0.0.1:3001) and provide the token
# via an environment variable, never as a flag value.
export HIVE_DASHBOARD_TOKEN="..."
bin/hivectl system status
```

## Running against a local Hive

Read-only and operational commands (`system`, `agent list/get/logs/kick/...`,
`bead`, `governor get`, `observe`) work against a plain local Hive. A few
commands need extra setup that a hand-run instance may lack:

- **knowledge writes** (`create/update/delete/import`) require the knowledge
  layer to have a wiki `url` in `hive.yaml` — Hive does not bundle llm-wiki, so
  a `path`-only layer returns "no configured endpoint".
- **config writes** (`agent prompt/model/backend/pipeline set`, `governor
  *-set`) write to the server's `/data/policies` dir, which must be writable
  (e.g. a container or Linux run); some take effect only after reload/restart.
- **auth**: set `dashboard.auth_token` (or `HIVE_DASHBOARD_TOKEN`) and pass it
  via `--token-env`. Bearer auth is rejected on direct-route spokes
  (`authorized_users` set with `hub_proxied: false`), which require a per-user
  session instead.

## Global options

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | `http://127.0.0.1:3001` | Dashboard API base URL |
| `--token-env` | `HIVE_DASHBOARD_TOKEN` | Env var holding the auth token |
| `--timeout` | `30s` | Per-request (or stream) timeout |
| `-o, --output` | `table` | Output format: `table`, `json`, `yaml`, `jsonl` |

Results go to stdout, errors to stderr. Exit codes:

| Code | Meaning |
| ---: | --- |
| 0 | Success |
| 1 | Local/unclassified failure |
| 2 | Invalid arguments or missing `--yes` |
| 3 | Authentication/authorization failure |
| 4 | Connection or timeout failure |
| 5 | Hive API/server-side error |

## Commands

### system — runtime state

```bash
hivectl system status -o json
hivectl system health
hivectl system version
hivectl system snapshot
hivectl system events --follow --timeout 2m -o jsonl   # SSE stream as JSONL
```

`events` is a stream, so `--follow` is required and output must be `jsonl`.

### agent — manage and operate agents

```bash
hivectl agent list
hivectl agent get quality -o yaml
hivectl agent logs quality --lines 200
hivectl agent kick quality --prompt "Review the work list"
hivectl agent pause|resume|restart quality
hivectl agent export quality -o yaml            # portable AgentDefinition
hivectl agent import --file agent.yaml --preview # validate without applying
hivectl agent delete ux-discovery --yes
```

Configure an agent (prompt template, model/backend, pipeline toggles):

```bash
hivectl agent prompt get quality --raw
hivectl agent prompt set quality --file quality.md
hivectl agent model-set quality claude-sonnet-4-6
hivectl agent backend-set quality claude
hivectl agent pipeline-set quality --file pipeline.yaml   # map of step: bool
```

### knowledge — inspect and maintain knowledge

```bash
hivectl knowledge search "user journey" --limit 20
hivectl knowledge list --layer project        # layer: personal|project|org|community
hivectl knowledge get project create-project
hivectl knowledge stats
hivectl knowledge export > hive-knowledge.md

hivectl knowledge create --file fact.yaml     # title + body required
hivectl knowledge update project create-project --file update.yaml
hivectl knowledge delete project create-project --yes
```

### bead — work items

```bash
hivectl bead list --agent quality -o json
hivectl bead create --agent quality --title "Review UX journey" --type advisory --priority 2
hivectl bead reset --agent quality --reason "replace stale findings" --yes
```

### governor — scheduler configuration

```bash
hivectl governor get -o yaml
hivectl governor thresholds-set --file thresholds.yaml
hivectl governor budget-set --file budget.yaml
hivectl governor repos-set --file repos.yaml
hivectl governor agent-add ux-discovery --backend copilot --model claude-sonnet-4-6
hivectl governor agent-remove ux-discovery --yes
```

### observe — read-only metrics

```bash
hivectl observe tokens -o json
hivectl observe costs
hivectl observe audit
hivectl observe history
hivectl observe timeline
hivectl observe trends --range week        # or --hours 12 (1-720); not both
```

## Input and safety

- Write commands take `--file <path>` or `--stdin` (JSON or YAML).
- Destructive actions (`agent delete`, `knowledge delete`, `bead reset`,
  `governor agent-remove`) require `--yes`.
- Request bodies are capped at 1 MiB, measured on the JSON-encoded payload;
  oversized input is rejected before anything is sent.
- Tokens are read from an environment variable, never passed as a flag value.
