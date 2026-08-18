# Supervisor Agent

Author: RawNuke

The supervisor agent is the hive's internal health-and-orchestration agent. It runs under the same agent machinery as the workers, but its policy keeps it in advisory, no-GitHub mode: it coordinates agents, reads beads and health state, and reports operator-facing status instead of writing code or acting on GitHub directly.

## What the supervisor does

On each governor kick, the supervisor follows its configured policy template and work order. In the built-in ACMM packs this means it:

- Monitors all configured agents for stalls, rate limits, login problems, and other anomalies.
- Reads recent bead activity and compares it with each agent's `stale_timeout` when that field is configured.
- Uses the internal kick/orchestration path to prompt agents that need to run.
- Coordinates agent lanes so duplicate or conflicting work is surfaced early.
- Delegates code, issue, pull request, and repository analysis to specialist agents.
- Produces sweep reports and health assessments for the dashboard/operator.

The supervisor is an orchestrator, not a fixer. Its policy forbids GitHub issue, pull request, API, and `gh` access.

## Supervisor vs. governor

The governor and supervisor are different layers:

| Layer | Component | What it does |
|-------|-----------|--------------|
| Scheduler | Governor (Go binary) | Evaluates queue depth on a timer. Selects idle, quiet, busy, or surge mode. Applies configured agent cadences, including `pause` and time-of-day/cron cadence objects. Controls *when* agents are kicked. |
| Orchestrator | Supervisor (AI agent) | Reviews agent health and bead activity, detects stalls, coordinates lane boundaries, and routes work to specialist agents. Controls *what operational guidance* agents receive. |

The governor can kick the supervisor like any other enabled agent. In the ACMM packs, the supervisor then performs a health sweep and may kick or redirect workers through the internal orchestration path.

## When to enable the supervisor

Enable the supervisor for multi-agent deployments where agents have overlapping responsibilities or where operators are not continuously watching the dashboard. It helps detect stalled sessions, rate limits, stale beads, and duplicate work.

A small hive can run without it when one or two simple agents are watched manually. Keeping it enabled costs one extra agent session and model usage, but gives the system a dedicated health sweep lane.

### Cadence and budget considerations

Supervisor cadence is configuration-driven:

- `hive.yaml.example` defines a supervisor agent, but its sample governor table does not assign supervisor cadences.
- Built-in ACMM packs set supervisor cadences. L2 uses `5m`; L3/L4 pause the supervisor in surge mode and use `5m` otherwise; L5 uses `5m`; L6 uses `1m`, `2m`, `3m`, or `5m` depending on mode.
- Cadences may be plain intervals, `pause`/`off`, or the time-of-day and cron cadence objects supported by the governor.

If token budget is tight, lengthen the supervisor cadence before disabling the agent entirely.

## `bead_role: supervisor` vs. `bead_role: worker`

The `bead_role` field controls how the dashboard partitions and sorts agent beads:

| Field | Value | Dashboard/config effect |
|-------|-------|-------------------------|
| `bead_role` | `supervisor` | `GetSortOrder()` defaults to `0` when no explicit `sort_order` is set; supervisor beads are grouped as supervisor beads. |
| `bead_role` | `worker` | `GetSortOrder()` defaults to `100` when no explicit `sort_order` is set; worker beads are grouped as worker beads. |

Known-agent defaults also populate metadata. For the built-in `supervisor` agent, the code default is `bead_role: supervisor` and `sort_order: 10`; `hive.yaml.example` also sets `sort_order: 10`. Explicit `sort_order` always wins.

## Policy modes: `supervisor-nogithub.md` and `supervisor-advisory.md`

Two supervisor policy files exist:

- `src/policies/supervisor-nogithub.md`
- `src/policies/supervisor-advisory.md`

They currently contain the same no-GitHub advisory rules. The built-in ACMM pack files (`src/pkg/config/packs/level-2.yaml` through `level-6.yaml`) use `supervisor-nogithub.md` for the supervisor. Older documentation may still mention `supervisor-advisory.md`; the current pack source is authoritative.

Both templates enforce:

- No `gh` commands, GitHub API calls, or issue/PR reads.
- Internal orchestration only: kick agents, monitor health, read beads, and coordinate the pipeline.
- No GitHub-referencing beads with `--external-ref "gh-*"`.
- Delegation of analysis and code actions to specialist agents.

The ACMM mode remains advisory for the supervisor at every level. `${GH_AUTH}` is only injected for measured, hold-gated, and full templates, so the supervisor does not receive GitHub credentials through the policy template path.

## How the supervisor interacts with other agents

The supervisor checks each agent's health inputs, including recent bead activity, configured stale timeouts, and visible session state. When it finds a problem, it reports the problem and uses the internal kick/orchestration path where appropriate.

Some ACMM pack descriptions refer to restarts or self-healing at higher levels. In current policy, the supervisor still remains no-GitHub and delegates code or repository actions; runtime restart behavior is controlled by agent configuration such as `restart_strategy`, not by GitHub access.

## Configuration reference

The supervisor entry in `hive.yaml.example` is:

```yaml
agents:
  supervisor:
    enabled: true
    backend: claude
    model: claude-sonnet-4-6
    beads_dir: /data/beads/supervisor
    clear_on_kick: true
    display_name: Supervisor
    description: Orchestrates all agents — sweeps, enforces cadence, monitors health
    sort_order: 10
    bead_role: supervisor
```

ACMM packs may override these values. For example, the pack definitions use `backend: copilot`, `kick_template: supervisor-nogithub.md`, `clear_on_kick: false`, and level-specific `stale_timeout` values.

## What to read next

- **[Agent Configuration](agent-configuration.md)** — every agent field, methods, model pinning, cadences, and ACMM packs.
- **[ACMM Policy Matrix](acmm-policy-matrix.md)** — the full per-level, per-agent policy table.
- **[Architecture](architecture.md)** — the process model, governor loop, and how the supervisor drives agents.
- **[Dashboard route and health checks](health-checks.md)** — listener probes and alert behavior for stuck sessions and restart loops.
