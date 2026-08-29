# Telemetry Agent

The telemetry agent audits and, once opted in, improves the **observability of the managed project** — the repositories the hive watches, not the hive's own internal metrics/tracing (see [Config.OTel/Tracing](operator-reference.md) for that, a deliberately separate concern). It is an L5/L6-only agent: absent from the roster below L5 and paused in every governor mode at L5/L6 until an operator explicitly opts in.

## What the telemetry agent does

On each kick, telemetry follows its ACMM-level policy template (`telemetry-advisory.md`, `telemetry-holdgated.md`, or `telemetry-full.md`) and:

- Inspects tracing, metrics, structured logging, scrape targets, dashboards, monitoring custom resources, collector/exporter configuration, and web analytics in the repositories it is authorized to audit (`$HIVE_REPOS`).
- Detects the project's *existing* observability stack before recommending anything, and prefers OpenTelemetry as a vendor-neutral spine — but only acts on a backend an operator has explicitly configured.
- Flags unbounded metric labels, high-cardinality span attributes, missing scrape targets, inconsistent span names, and dashboards that aren't source-controlled.
- Never reveals credentials, endpoints, API keys, or secret values in its output — it refers only to environment-variable or secret names.

At **advisory** level (ACMM L2–L4, though telemetry does not actually appear in the roster until L5 — see [ACMM level gating](#acmm-level-gating) below), it writes each confirmed finding as an advisory bead owned by `telemetry` and returns an `AgentReport` with `kind: "findings"`. It never creates GitHub issues, branches, commits, or pull requests in this mode.

At **hold-gated** (L5) and **full** (L6) modes, telemetry runs in `ISSUES_AND_PRS` and, in addition to filing findings, can open bounded, hold-gated pull requests: OpenTelemetry SDK wiring and request-path spans, bounded metrics and `/metrics` endpoints, structured logging, dashboard JSON, alert-rule YAML, `ServiceMonitor`/`PodMonitor` resources, collector/exporter configuration, dashboard-lint CI, and GA4 wiring for an identified web property. It re-verifies and closes only stale beads it owns, and files findings with a `[telemetry]` title.

Telemetry must never, at any PR-capable level: commit credentials, literal collector endpoints, API keys, or secret values; add an exporter that sends data off-box without an explicitly configured backend; introduce unbounded labels or span attributes; merge its own PR; or (at L5) remove a `hold`/`on-hold`/`do-not-merge` label. At L6 it still never merges its own PR, and it must not modify work already labeled `hold`, `on-hold`, or `do-not-merge`.

## ACMM level gating

Telemetry and operations are **L5/L6-only opt-in agents**. Per the built-in ACMM packs (`src/pkg/config/packs/level-5.yaml`, `level-6.yaml`):

- They are absent from the roster entirely at L1–L4 — no pack below L5 defines a `telemetry` entry, so the agent does not appear in the dashboard, does not spawn a pane, and cannot be kicked.
- At L5 and L6 they are present but their cadence is `paused` in **every** governor mode (`surge`, `busy`, `quiet`, `idle`) until an operator opts in. This is not a description of typical defaults — the pack literally sets `telemetry: paused` in all four cadence tables at both levels.
- `kick_template` is `telemetry-holdgated.md` at L5 and `telemetry-full.md` at L6, matching the hold-gated-vs-full PR behavior described above.

## When to enable telemetry

Enable telemetry once you're comfortable with the rest of your L5/L6 roster and want automated observability hardening for the *managed* project — bounded metrics, tracing, and dashboards-as-code, on a backend you've explicitly named. Leave it paused if you have no observability backend in mind yet: with nothing configured, its policies fail closed, so it will only detect and report the existing stack rather than propose changes.

## How to opt in: `governor.project_observability`

Un-pausing the agent alone does nothing useful — telemetry's PR-capable policies fail closed without a confirmed target stack. Configure the opt-in under **Settings → Project Observability** in the dashboard:

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

Reference fields accept **names only** — an environment-variable name or a `secret-name/key` reference. Literal endpoints, tokens, and API keys are rejected. Selecting platforms and saving persists them under `governor.project_observability`, and replaces telemetry's (and operations') all-mode `paused` cadence with a conservative `24h` interval, which can then be tuned from the agent's Cadences tab.

After telemetry's first advisory run, platforms mentioned in its findings are preselected as suggestions in the Project Observability tab. They stay unsaved until an operator reviews and clicks **Save** — only then does the persisted declaration govern future telemetry (and operations) work.

## How telemetry interacts with other agents

Telemetry's lane keywords (`observability`, `opentelemetry`, `prometheus`, `grafana`, `tracing`, `metrics`, `structured-logging`, `servicemonitor`, `podmonitor`) are disjoint from operations' (health, SLO, runbook, incident, rollback, alerting terms), so the two agents do not compete for the same issues. Both share the same `${PROJECT_OBSERVABILITY}` prompt section and the same `governor.project_observability` configuration — telemetry adds the instrumentation; operations builds the health/SLO/runbook layer on top of it.

## Configuration reference

Registered defaults (`applyKnownAgentDefaults` in `src/pkg/config/config.go`), applied when a field is left blank in your own config:

| Field | Default |
|-------|---------|
| `emoji` | 📡 |
| `color` | `#00a8cc` |
| `aliases` | `["tm"]` |
| `bead_role` | `worker` |
| `sort_order` | `65` |
| `include_repos` | `true` |
| `lane_keywords` | `observability`, `opentelemetry`, `prometheus`, `grafana`, `tracing`, `metrics`, `structured-logging`, `servicemonitor`, `podmonitor` |
| `detect_keywords` | `telemetry`, `observability`, `opentelemetry`, `prometheus` |

ACMM packs additionally set `backend: copilot`, `model: claude-sonnet-4-6`, `mode: ISSUES_AND_PRS`, `stale_timeout: 28800`, and the level-appropriate `kick_template`.

## Cadence and budget considerations

Telemetry is a heavyweight, PR-capable agent once enabled. Follow the same guidance as every other agent un-paused at L5/L6: set all modes to `12h` or `1d` first (the automatic un-pause already lands at a conservative `24h`), watch its output for a few cycles, and only shorten the cadence once you understand what it's producing and how much budget it consumes per run.

## What to read next

- **[Operations Agent](operations.md)** — the companion L5/L6 opt-in agent for operational readiness (health checks, SLOs, runbooks).
- **[Agent Configuration](agent-configuration.md)** — every agent field, the ACMM level packs, and `project_observability` details.
- **[ACMM Policy Matrix](acmm-policy-matrix.md)** — the full per-level, per-agent policy table, including the L5/L6-only gating note.
- **[Getting Started](getting-started.md)** — when to opt in as part of the level-climbing journey.
