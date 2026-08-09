# Supervisor Agent

Author: RawNuke

The supervisor agent is an internal orchestrator. It manages other agents, monitors their health, and coordinates the pipeline. It does not interact with GitHub.

## What the supervisor does

The supervisor is an AI agent that runs in a tmux session. On every kick from the governor, it:

- Reads its policy file and its ACMM level fragment.
- Checks every agent session with `hive status`. It looks for stalls, rate limits, login expiry, and idle prompts.
- Reads the bead store for each agent. It compares the last activity timestamp against the `stale_timeout`.
- Kicks idle agents or stuck agents through the internal kick mechanism.
- Delegates analysis to specialist agents. The supervisor is an orchestrator, not a fixer. It never writes code, opens issues, or merges pull requests.
- Reports pipeline health and agent status in a structured summary.

The supervisor answers agent questions, coordinates lane boundaries to prevent duplicate work, and flags stale agents that show no bead activity in their expected window.

## Supervisor vs. governor

The governor and the supervisor are two different layers of the hive:

| Layer | Component | What it does |
|-------|-----------|--------------|
| Scheduler | Governor (Go binary) | Evaluates queue depth on a timer. Switches between idle, quiet, busy, and surge modes. Adjusts per-agent kick cadences. Enforces budget limits. The governor controls *when* an agent is kicked. |
| Orchestrator | Supervisor (AI agent) | Monitors session health. Detects stalls, rate limits, and stuck agents. Prioritizes work and dispatches to specialist agents. Ensures agents are working on the correct thing. The supervisor controls *what* the agent works on. |

The governor handles cadence timing and budget. The supervisor handles direction and health. Both run together in a production hive. The governor kicks the supervisor like any other agent. The supervisor then monitors the agents the governor just kicked.

## When to enable the supervisor

Enable the supervisor agent in a multi-agent deployment. The hive works without it when:

- The hive runs one or two agents. The bare governor timer is enough.
- The operator watches the dashboard and handles health manually.
- The agents are simple scanners that do not need coordination.

Enable the supervisor when:

- The hive runs three or more agents. The supervisor prevents duplicate work across lanes.
- The operator cannot watch the dashboard all the time. The supervisor detects stalls and stuck agents.
- The agents have overlapping responsibilities. The supervisor enforces lane boundaries.

The supervisor is safe to keep enabled. It costs one extra agent session. It never touches GitHub. It only reads the bead store and sends kick instructions to the other agents.

### Budget considerations

The supervisor agent uses model tokens like any other agent. At a 5-minute kick cadence, it runs frequently. Reduce the kick cadence first when your token budget is tight. Do not remove the supervisor. A 15-minute or 30-minute cadence is enough for health monitoring in a small hive.

## `bead_role: supervisor` vs. `bead_role: worker`

The `bead_role` field in the agent configuration controls how the dashboard renders the agent:

| Field | Value | Dashboard effect |
|-------|-------|------------------|
| `bead_role` | `supervisor` | Sort order defaults to 0. The agent appears first in the sidebar. |
| `bead_role` | `worker` | Sort order defaults to 100. The agent appears after all supervisors. |

The `bead_role` value also sets the default `sort_order`. You can override it with an explicit `sort_order` field. An agent with `bead_role: supervisor` and `sort_order: 10` still appears before an agent with `bead_role: worker` and `sort_order: 20`.

For known agent names, `bead_role` defaults to `worker`. The supervisor is the only agent that ships with `bead_role: supervisor` in `hive.yaml.example`.

## Policy modes: `supervisor-advisory.md` vs. `supervisor-nogithub.md`

Two policy templates exist for the supervisor agent. Both are advisory. Both restrict the supervisor from GitHub interaction. The ACMM policy matrix assigns `supervisor-advisory.md` to the supervisor at every level from L2 to L6.

### `supervisor-advisory.md`

Location: `v2/policies/supervisor-advisory.md`

This is the default policy template at all ACMM levels. The supervisor runs in advisory mode:

- **No GitHub interaction.** No `gh` commands, no API calls, no reading issues or pull requests.
- **Internal orchestration only.** Kick agents, monitor health, read beads, coordinate the pipeline.
- **No GitHub-referencing beads.** Beads must not carry `--external-ref "gh-*"`.
- **Delegates all analysis.** The supervisor never writes code or opens pull requests. Specialist agents handle every analysis task.

### `supervisor-nogithub.md`

Location: `v2/policies/supervisor-nogithub.md`

This template is a variant with the same rules as `supervisor-advisory.md`. The NO-GitHub constraint is explicit in the heading. Use this template when the operator wants an additional visible reminder that the supervisor has no GitHub access. The two templates are functionally identical. Both enforce the same advisory mode with no GitHub interaction.

### Why the supervisor never gets GitHub access

The ACMM policy matrix shows the supervisor at advisory mode at every level (L2-L6). This is by design:

- The supervisor orchestrates agents. It does not fix bugs or write code.
- The supervisor coordinates the pipeline. It delegates every code action to a specialist agent with the correct mode (measured, holdgated, or full).
- A supervisor with GitHub access would add audit risk. Its actions cannot be traced to a single lane.
- The supervisor monitors the beads ledger. It does not need to cross-check against GitHub issues.

The policy files enforce this constraint at the agent level. The `${GH_AUTH}` template variable is never injected into a supervisor policy template. The governor also sets the mode to advisory for the supervisor, regardless of the ACMM level.

## How the supervisor interacts with other agents

The supervisor reads the bead store for every agent. It checks:

- **Recent activity.** When was the last bead created or updated by each agent?
- **Stale agents.** Has an agent been silent longer than its `stale_timeout`?
- **Stuck agents.** Is an agent displaying an idle prompt when it should be working?
- **Lane enforcement.** Are two agents working on the same issue?

When the supervisor finds a problem, it:

1. Kicks the stuck agent with a work order.
2. Flags the issue in the structured status report.
3. Sends an ntfy notification when configured.

The supervisor does not pause or unpause agents. Pausing is an operator-only action. The supervisor does not stop or restart sessions. It reports problems and lets the operator or the governor handle recovery.

## Configuration reference

The supervisor agent entry in `hive.yaml`:

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

The `bead_role: supervisor` field sets the dashboard sort order. The `clear_on_kick: true` field resets the agent session context before each kick. The supervisor runs on a short cadence in the governor mode tables (typically 5 minutes).

The supervisor uses the `supervisor-advisory.md` kick template at every ACMM level. The template is resolved from the hive's policies checkout, falling back to the embedded default in the binary.

## What to read next

- **[Agent Configuration](agent-configuration.md)** — every agent field, methods, model pinning, cadences, and ACMM packs.
- **[ACMM Policy Matrix](acmm-policy-matrix.md)** — the full per-level, per-agent policy table.
- **[Architecture](https://kubestellar.io/docs/hive/overview/architecture)** — the two scheduling models and how the supervisor drives agents.
- **[Troubleshooting](https://kubestellar.io/docs/hive/getting-started/troubleshooting)** — stuck sessions, login expiry, restart loops.
