# hivectl

`hivectl` is the non-interactive command-line client for a running Hive
dashboard API. The `hive` binary starts the service; `hivectl` inspects and
operates that service.

## Build

```bash
mkdir -p bin
go build -o bin/hivectl ./cmd/hivectl
```

The default server is `http://127.0.0.1:3001`. When dashboard authentication is
enabled, provide the token through an environment variable:

```bash
export HIVE_DASHBOARD_TOKEN="..."
bin/hivectl system status
```

Use another server or token environment variable without exposing the token in
the process list:

```bash
bin/hivectl --server https://hive.example.com \
  --token-env MY_HIVE_TOKEN system health
```

## Output and automation

Every command is non-interactive. Machine-readable formats are available
through the global output option:

```bash
bin/hivectl agent list --output json
bin/hivectl governor get --output yaml
bin/hivectl system events --follow --timeout 2m --output jsonl
```

Supported formats are `table`, `json`, `yaml`, and `jsonl`. Results are written
to stdout and errors to stderr.

Exit codes:

| Code | Meaning |
| ---: | --- |
| 0 | Success |
| 1 | Local/unclassified failure |
| 2 | Invalid arguments or missing confirmation |
| 3 | Authentication or authorization failure |
| 4 | Hive connection or timeout failure |
| 5 | Hive API or server-side validation failure |

## System

```bash
hivectl system health
hivectl system status --output json
hivectl system version
hivectl system snapshot
hivectl system events --follow --timeout 2m --output jsonl
```

Events are an SSE stream. `--follow` is required so scripts do not
accidentally start a long-running command. Each JSONL record preserves both the
SSE event name and its data:

```json
{"event":"agent-status","data":{"timestamp":"...","agents":[]}}
```

## Agents

```bash
hivectl agent list
hivectl agent get quality --output yaml
hivectl agent logs quality --lines 200
hivectl agent kick quality --prompt "Review the current work list"
hivectl agent pause quality
hivectl agent resume quality
hivectl agent restart quality
hivectl agent export quality --output yaml
```

Import a portable `AgentDefinition` from a file, stdin, or URL:

```bash
hivectl agent import --file ux-discovery.yaml --preview
hivectl agent import --file ux-discovery.yaml
cat ux-discovery.yaml | hivectl agent import --stdin --preview
hivectl agent import --url https://example.com/ux-discovery.yaml --preview
```

Only managed agents can be deleted. Deletion requires explicit confirmation:

```bash
hivectl agent delete ux-discovery --yes
```

## Knowledge

```bash
hivectl knowledge health
hivectl knowledge stats
hivectl knowledge list --layer project --type regression
hivectl knowledge search "user journey" --limit 20
hivectl knowledge get project create-project
hivectl knowledge graph --root create-project --depth 3 --output json
hivectl knowledge export > hive-knowledge.md
```

Create and update requests accept JSON or YAML matching the Hive Knowledge API:

```bash
hivectl knowledge create --file fact.yaml
hivectl knowledge update project create-project --file update.yaml
cat facts.md | hivectl knowledge import --stdin --format markdown --layer project
hivectl knowledge delete project create-project --yes
```

## Beads

The current Hive API supports list, create, and bulk reset operations:

```bash
hivectl bead list
hivectl bead list --agent quality --output json
hivectl bead create \
  --agent quality \
  --title "Review UX journey" \
  --type advisory \
  --priority 2 \
  --external-ref "ux://review/create-project"
hivectl bead reset --agent quality --reason "replace stale findings" --yes
```

Single-bead get, update, and close commands are intentionally absent until the
Hive API provides those lifecycle operations.

## Governor

```bash
hivectl governor get --output yaml
hivectl governor thresholds-set --file thresholds.yaml
hivectl governor budget-set --file budget.yaml
hivectl governor repos-set --file repos.yaml
hivectl governor agent-add ux-discovery \
  --backend copilot \
  --model claude-sonnet-4-6
hivectl governor agent-remove ux-discovery --yes
```

The update files are passed to the corresponding existing Hive API endpoint.
For example:

```yaml
# thresholds.yaml
surge: 20
busy: 10
quiet: 2
```

```yaml
# budget.yaml
totalTokens: 1000000
periodDays: 30
criticalPct: 90
```

```yaml
# repos.yaml
repos:
  - kubestellar/console
primaryRepo: kubestellar/console
```

## Safety boundary

- `agent import --preview` uses Hive's real preview API and makes no config
  change.
- APIs without preview support are not presented as dry-runs.
- Agent deletion, knowledge deletion, bead reset, and governor agent removal
  require `--yes`.
- Authentication tokens are read from environment variables rather than
  command-line values.
- `agent logs` is a bounded snapshot from `/api/pane/{agent}`, not a dedicated
  log stream.
