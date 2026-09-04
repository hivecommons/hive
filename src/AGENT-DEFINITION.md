# Hive Agent Definition Reference

The Hive Agent Definition is the declarative interface for configuring autonomous agents. Every agent is defined by a set of fields in `hive.yaml` (or an overlay file in `/data/agent-configs/`), and can be imported/exported as a portable YAML document.

## Portable Format

Agents can be shared as standalone YAML files using the `AgentDefinition` kind:

```yaml
apiVersion: hive.kubestellar.io/v1
kind: AgentDefinition
metadata:
  name: scanner
  displayName: "Code Scanner"
  description: "Triages GitHub issues and PRs"
  emoji: "🔍"
  color: "#3498db"
  specVersion: 1       # 0=original, 1=channels+tools+connections
spec:
  backend: claude
  model: claude-sonnet-4-6
  role: scanner
  mode: ISSUES_PRS_MERGE
  # ... all spec fields below
```

Import via the dashboard (⚙️ → Import tab) or `POST /api/agents/import`.  
Export via `GET /api/config/agent/{name}/export`.

---

## Spec Fields

These defaults match the loader in `src/pkg/config`: an omitted `enabled` defaults to `true`, and an omitted `clear_on_kick` defaults to `true`. For the narrative configuration guide, see [docs/agent-configuration.md](docs/agent-configuration.md).


### Identity & Display

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `backend` | string | `"copilot"` | CLI backend: `claude`, `copilot`, `goose`, `codex`, `pi`, `bob`, `aider` |
| `model` | string | — | Model identifier (e.g. `claude-sonnet-4-6`) |
| `role` | string | — | Semantic role for behavioral dispatch |
| `display_name` | string | YAML key | Human-readable name shown in dashboard |
| `description` | string | — | One-line description |
| `emoji` | string | — | Discord/dashboard identity icon |
| `color` | string | — | Dashboard color (hex, e.g. `"#3498db"`) |
| `sort_order` | int | 100 | Sidebar ordering (lower = higher) |
| `aliases` | []string | — | Discord shortcodes (e.g. `["sc"]`) |

### Runtime

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | true | Whether the agent is active unless explicitly set to `false` |
| `on_demand` | bool | false | Skip governor scheduling; only manual kicks |
| `clear_on_kick` | bool | true | Send `/clear` before each kick unless explicitly set to `false` |
| `stale_timeout` | int | 28800 | Seconds before an agent is considered stale |
| `restart_strategy` | string | `"immediate"` | How to restart after crash |
| `launch_cmd` | string | — | Override the auto-generated launch command |
| `agent_spec` | string | — | BYO-agent spec file or directory; applies its backend, model, mode, launch command, prompt, tools, and skills at launch |
| `cli_pinned` | bool | false | Pin the CLI binary version |
| `caveman_mode` | string | — | Output compression: `lite`, `full`, `ultra`, `wenyan` |
| `beads_dir` | string | — | Directory for bead storage |
| `bead_role` | string | `"worker"` | `"supervisor"` or `"worker"` |

### Classification & Routing

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | — | Permission mode: `ADVISORY`, `ISSUES_ONLY`, `ISSUES_AND_PRS`, `ISSUES_PRS_MERGE` |
| `lane_keywords` | []string | — | Issue classification routing keywords |
| `detect_keywords` | []string | — | Token session detection keywords |
| `kick_template` | string | — | Prompt template filename |
| `include_repos` | bool | true | Append repos section to kicks. Prompt text only — it authorizes repos, it does not clone or provision any of them on disk |
| `metrics_collector` | string | — | Custom metrics collector name |
| `acmm_levels` | []int | — | ACMM maturity levels this agent operates at |

### Stats Display

| Field | Type | Description |
|-------|------|-------------|
| `stats_display` | []StatsEntry | Custom stats for sidebar display |

Each `StatsEntry`:
```yaml
stats_display:
  - key: actionable
    label: Actionable
    source: status           # status, health, agentMetrics, tokens
    field: actionableCount
    style: spark             # number, dot, pct, pct-bar, spark
    trend_field: actionable
    target: 0
```

---

## Channels

Channels declare how an agent gets triggered. When omitted or empty, the agent uses **governor timer kicks by default** (implicit kick channel).

```yaml
channels:
  - type: kick                              # governor timer (default behavior)
```

### Channel Types

| Type | Required Fields | Description |
|------|----------------|-------------|
| `kick` | — | Governor timer kicks (cadence-based) |

`kick` is the only supported type. The former `webhook` / `schedule` / `discord` /
`bead` trigger types were declarative-only: the runtime meant to serve them
(`pkg/channels`) was never wired into the binary and was removed (#5591).
Declaring one of them used to validate cleanly and then silently suppress
governor kicks, leaving the agent permanently dormant — config validation now
rejects them instead.

### Common Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | **required** | Channel type |
| `enabled` | *bool | true | Toggle without removing |

### Backward Compatibility

- **No channels field** → agent uses governor timer kicks (identical to pre-v3 behavior)
- **Explicit `kick` channel** → same as no channels, but makes the intent visible
- `on_demand: true` still overrides all scheduling regardless of channels

---

## Tools

Tools declare what tools an agent can use. When omitted, the existing `mode` field governs tool access through the legacy three-layer enforcement.

```yaml
tools:
  preset: full                              # advisory, issues-only, issues-prs, full
  rules:
    - pattern: "mcp__github__delete_*"
      action: deny
      reason: "scanners should not delete repos"
    - pattern: "mcp__github__create_pull_request"
      action: allow
      reason: "override preset deny for outreach"
```

### Presets

Presets map to the existing mode-based deny lists:

| Preset | Denied Tools | Equivalent Mode |
|--------|-------------|-----------------|
| `advisory` | create_pull_request, create_issue, update_issue, merge_pull_request | `ADVISORY` |
| `issues-only` | create_pull_request, merge_pull_request | `ISSUES_ONLY` |
| `issues-prs` | *(none)* | `ISSUES_AND_PRS` |
| `full` | *(none)* | `ISSUES_PRS_MERGE` |

### Rules

Rules override preset behavior. Each rule has:

| Field | Type | Description |
|-------|------|-------------|
| `pattern` | string | **required** — Tool name or glob pattern |
| `action` | string | **required** — `allow` or `deny` |
| `reason` | string | Optional explanation |

An explicit `allow` rule overrides a preset `deny` for the same pattern.

### Backward Compatibility

- **No tools field** → `mode` field drives tool restrictions (identical to pre-v3)
- **Tools set** → takes precedence over `mode`; a warning is logged if both are set
- The MITM proxy defense-in-depth layer continues to use `mode` for HTTP-level blocking

### How It Works

1. Preset is expanded to its deny list
2. Each rule is applied: matching patterns are replaced (allow overrides deny)
3. Final deny list is passed as `--disallowed-tools` (Claude) or `--deny-tool` (Copilot)
4. An effective mode is derived for the proxy's defense-in-depth layer

---

## Connections

Connections declare external service integrations. When omitted, no additional MCP servers, env vars, or knowledge sources are injected.

```yaml
connections:
  - name: github-mcp
    type: mcp
    uri: "stdio:///usr/local/bin/mcp-github"

  - name: jira
    type: api
    uri: "https://jira.example.com/rest/api/2"
    auth:
      type: env
      env_var: JIRA_TOKEN
    env_name: JIRA_BASE_URL

  - name: team-wiki
    type: knowledge
    uri: "/data/vaults/team-wiki"
    options:
      layer: project
```

### Connection Types

| Type | Description | Runtime Effect |
|------|-------------|----------------|
| `mcp` | MCP server | `--mcp-server` flag on Claude launch command |
| `api` | REST API | Sets `<env_name>=<uri>` and auth token env vars |
| `knowledge` | Knowledge source | Additional vault/layer for knowledge primer |

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | **required** — Unique name within the agent |
| `type` | string | **required** — `mcp`, `api`, or `knowledge` |
| `uri` | string | Required for mcp/api — endpoint or stdio URI |
| `env_name` | string | Env var name for the URI (auto-generated if empty: `HIVE_CONN_<NAME>_URL`) |
| `auth` | object | Authentication config |
| `options` | map | Type-specific options (e.g. `layer: project` for knowledge) |

### Auth

| Field | Type | Description |
|-------|------|-------------|
| `auth.type` | string | `env` (env var) or `file` (file path) |
| `auth.env_var` | string | Required when type=env — env var containing the token |
| `auth.file` | string | Required when type=file — path to token file |

Auth secrets are never displayed in the dashboard (masked with `***`).

### Backward Compatibility

- **No connections field** → no changes to launch command or env vars
- Existing MCP servers configured in CLAUDE.md continue to work alongside declared connections

---

## Cadences

Cadences control kick frequency per governor mode. These are set at the governor level, not on the agent directly:

```yaml
governor:
  modes:
    surge:
      threshold: 20
      scanner: 15m
      ci-maintainer: pause
    busy:
      threshold: 10
      scanner: 15m
      ci-maintainer: 1h
    quiet:
      threshold: 2
      scanner: 15m
      ci-maintainer: 45m
    idle:
      threshold: 0
      scanner: 15m
      ci-maintainer: 15m
```

In portable format, cadences are nested under `spec`:
```yaml
spec:
  cadences:
    idle: 15m
    quiet: 15m
    busy: 15m
    surge: 15m
```

---

## Schema Versioning

The `specVersion` field in metadata tracks feature availability:

| Version | Features |
|---------|----------|
| 0 (or absent) | Original fields only |
| 1 | Channels, Tools, Connections |

The `apiVersion` remains `hive.kubestellar.io/v1` — all changes are additive with `omitempty` tags. Old exporters produce YAML that imports cleanly on new versions (new fields default to zero-value = legacy behavior). New exporters produce YAML that old importers parse without error (unknown fields are silently ignored).

---

## Dashboard

All three features are configurable through the per-agent config dialog (⚙️ button):

| Tab | Description |
|-----|-------------|
| **Channels** | Add/remove/toggle trigger channels with type-specific forms |
| **Tools** | Select presets, add allow/deny rules with pattern+reason |
| **Connections** | Add/remove MCP servers, APIs, and knowledge sources |

---

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/config/agent/{name}` | Full agent config (includes channels, tools, connections) |
| `PUT` | `/api/config/agent/{name}/channels` | Update channels |
| `PUT` | `/api/config/agent/{name}/tools` | Update tools |
| `PUT` | `/api/config/agent/{name}/connections` | Update connections |
| `GET` | `/api/config/agent/{name}/export` | Export as portable YAML |
| `POST` | `/api/agents/import` | Import from YAML (URL or paste) |
| `POST` | `/api/webhook/github` | GitHub webhook receiver for channel triggers |

---

## Full Example

```yaml
agents:
  scanner:
    enabled: true
    backend: claude
    model: claude-sonnet-4-6
    display_name: Code Scanner
    description: Triages GitHub issues and PRs
    role: scanner
    emoji: "🔍"
    color: "#3498db"
    sort_order: 20
    clear_on_kick: true
    beads_dir: /data/beads/scanner
    bead_role: worker
    caveman_mode: full
    lane_keywords: ["bug", "triage", "fix"]
    detect_keywords: ["scanner", "triage"]
    kick_template: scanner-CLAUDE.md
    include_repos: true

    channels:
      - type: kick

    tools:
      preset: full
      rules:
        - pattern: "mcp__github__delete_*"
          action: deny
          reason: "scanners should not delete repos"

    connections:
      - name: github-mcp
        type: mcp
        uri: "stdio:///usr/local/bin/mcp-github"
      - name: jira
        type: api
        uri: "https://jira.example.com/rest/api/2"
        auth:
          type: env
          env_var: JIRA_TOKEN
        env_name: JIRA_BASE_URL
```
