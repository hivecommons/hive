# Operations Agent

The operations agent audits and, once opted in, improves the **operational readiness of the managed project** — health checks, SLO/SLI definitions, alerting, runbooks, and release/rollback safety. It is an L5/L6-only agent: absent from the roster below L5 and paused in every governor mode at L5/L6 until an operator explicitly opts in.

## What the operations agent does

On each kick, operations follows its ACMM-level policy template (`operations-advisory.md`, `operations-holdgated.md`, or `operations-full.md`) and:

- Inspects health and readiness endpoints, SLO/SLI definitions, user-impact alert rules, runbooks, incident/postmortem templates, release procedures, and rollback paths in the repositories it is authorized to audit (`$HIVE_REPOS`).
- Verifies that health/readiness probes check every dependency required to serve the state they claim (a probe that reports healthy without checking a required dependency is treated as a defect, not a pass).
- Flags alerts without runbook links, machine-state alerts with no user impact, undocumented rollback paths, and SLOs without measurable indicators.
- Never weakens an existing alert or SLO to make reported health look better — this is a hard rule in every mode, including PR-capable ones.

At **advisory** level (ACMM L2–L4, though operations does not actually appear in the roster until L5 — see [ACMM level gating](#acmm-level-gating) below), it writes each confirmed finding as an advisory bead owned by `operations` and returns an `AgentReport` with `kind: "findings"`. It never creates GitHub issues, branches, commits, or pull requests in this mode.

At **hold-gated** (L5) and **full** (L6) modes, operations runs in `ISSUES_AND_PRS` and, in addition to filing findings, can open bounded, hold-gated pull requests: health and readiness handlers, SLO/SLI definitions, user-impact alert rules with runbook links, `runbooks/*.md`, incident and postmortem templates, and release/rollback documentation or safeguards. It re-verifies and closes only stale beads it owns, and files findings with an `[operations]` title.

Operations must never, at any PR-capable level: merge its own PR; weaken an existing alert or SLO to improve reported health; or add a probe that reports healthy without checking a dependency required to serve traffic. At L5 it additionally must never remove a `hold`/`on-hold`/`do-not-merge` label; at L6 it must not modify work already labeled `hold`, `on-hold`, or `do-not-merge`.

## ACMM level gating

Telemetry and operations are **L5/L6-only opt-in agents**. Per the built-in ACMM packs (`src/pkg/config/packs/level-5.yaml`, `level-6.yaml`):

- They are absent from the roster entirely at L1–L4 — no pack below L5 defines an `operations` entry, so the agent does not appear in the dashboard, does not spawn a pane, and cannot be kicked.
- At L5 and L6 they are present but their cadence is `paused` in **every** governor mode (`surge`, `busy`, `quiet`, `idle`) until an operator opts in — the pack literally sets `operations: paused` in all four cadence tables at both levels.
- `kick_template` is `operations-holdgated.md` at L5 and `operations-full.md` at L6, matching the hold-gated-vs-full PR behavior described above.

## When to enable operations

Enable operations once you're comfortable with the rest of your L5/L6 roster and want automated operational hardening for the *managed* project — health/readiness handlers, SLO definitions, alert rules tied to runbooks, and rollback documentation. It is most useful once telemetry (or an existing observability stack) already gives it signal to reason about: SLOs and alert rules are only as good as the metrics behind them.

## How to opt in: `governor.project_observability`

Un-pausing the agent alone does nothing useful — operations' PR-capable policies fail closed without a confirmed target stack. Configure the opt-in under **Settings → Project Observability** in the dashboard:

```yaml
governor:
  project_observability:
    open_source: [opentelemetry, prometheus, grafana]
    kube_native: [servicemonitor]
    commercial: [honeycomb]
    references:
      honeycomb:
        endpoint_env: OTEL_EXPORTER_OTLP_ENDPOINT
        credential_secret: observability/honeycomb-key
```

Reference fields accept **names only** — an environment-variable name or a `secret-name/key` reference. Literal endpoints, tokens, and API keys are rejected. Selecting platforms and saving persists them under `governor.project_observability`, and replaces operations' (and telemetry's) all-mode `paused` cadence with a conservative `24h` interval, which can then be tuned from the agent's Cadences tab.

After telemetry's first advisory run, platforms mentioned in its findings are preselected as suggestions in the Project Observability tab. They stay unsaved until an operator reviews and clicks **Save** — only then does the persisted declaration govern future operations (and telemetry) work.

## How operations interacts with other agents

Operations' lane keywords (`healthz`, `readyz`, `readiness`, `slo-`, `sli-`, `service-level-objective`, `service-level-indicator`, `error-budget`, `runbook`, `incident-response`, `rollback`, `alerting`) are disjoint from telemetry's instrumentation-focused keywords, so the two agents do not compete for the same issues. Both share the same `${PROJECT_OBSERVABILITY}` prompt section and the same `governor.project_observability` configuration — telemetry adds the instrumentation; operations builds the health/SLO/runbook layer on top of it.

## Configuration reference

Registered defaults (`applyKnownAgentDefaults` in `src/pkg/config/config.go`), applied when a field is left blank in your own config:

| Field | Default |
|-------|---------|
| `emoji` | 🚨 |
| `color` | `#d35400` |
| `aliases` | `["op"]` |
| `bead_role` | `worker` |
| `sort_order` | `66` |
| `include_repos` | `true` |
| `lane_keywords` | `healthz`, `readyz`, `readiness`, `slo-`, `sli-`, `service-level-objective`, `service-level-indicator`, `error-budget`, `runbook`, `incident-response`, `rollback`, `alerting` |
| `detect_keywords` | `operations`, `operability`, `healthz`, `runbook` |

ACMM packs additionally set `backend: copilot`, `model: claude-sonnet-4-6`, `mode: ISSUES_AND_PRS`, `stale_timeout: 28800`, and the level-appropriate `kick_template`.

## Cadence and budget considerations

Operations is a heavyweight, PR-capable agent once enabled. Follow the same guidance as every other agent un-paused at L5/L6: set all modes to `12h` or `1d` first (the automatic un-pause already lands at a conservative `24h`), watch its output for a few cycles, and only shorten the cadence once you understand what it's producing and how much budget it consumes per run.

## What to read next

- **[Telemetry Agent](telemetry.md)** — the companion L5/L6 opt-in agent that instruments the project operations can then build SLOs and alerts on top of.
- **[Agent Configuration](agent-configuration.md)** — every agent field, the ACMM level packs, and `project_observability` details.
- **[ACMM Policy Matrix](acmm-policy-matrix.md)** — the full per-level, per-agent policy table, including the L5/L6-only gating note.
- **[Getting Started](getting-started.md)** — when to opt in as part of the level-climbing journey.
