# PR reach telemetry (#3973)

Status: Phase 1 landed (version-stamped spans + component attribute); Phase 2
complete (2a spoke-side counters in #3993; 2b PR mapping & ancestry join in
#3994; 2c error-rate deltas, status table, & ACMM advisor wiring in #3995).

## The problem

Every quality signal we have — acceptance rate, coverage, CI — measures work at
proposal or approval time. None answers the question that actually matters for
merged code: **did it ever run in production, on how many hives, and did error
rates move?** The framing is *reach*, not execution-time: a nil-check that
executes once a month and prevents data loss matters more than a hot loop that
runs constantly.

## Anchoring rule: merged ≠ deployed ≠ running

Reach MUST attribute to the commit actually executing, never to the merge or
publish event. We have seen the published digest advance while the running
binary stayed old (#3816); without a running-commit anchor, undeployed code
reads as "unused" and the metric lies. The only trustworthy version source is
the SHA baked into the binary at build time (the same ldflags value
`hive --version` prints), not any tag, channel, or Deployment spec.

## What phase 1 emits

Resource attributes, on every span from every process with tracing enabled
(`pkg/tracing.Init`, values wired in `cmd/hive/main.go`):

| Attribute         | Source                                    | Meaning |
|-------------------|-------------------------------------------|---------|
| `service.version` | ldflags `main.gitShort` (7-char SHA)      | Commit of the RUNNING binary; standard semconv, so backends group by it natively |
| `hive.commit`     | same value                                | Explicit alias alongside the other `hive.*` identity attrs |
| `hive.image`      | `hub.SelfDeploymentImage()` (cached read) | Deployment-DECLARED image ref; informational |
| `hive.id`, `hive.branch` | config (pre-existing)              | Per-hive / per-branch dimensions |

Rules:

- A build without ldflags injection (`gitShort == "unknown"`) stamps **no**
  version attributes. Reach queries must never group spans under a placeholder
  pseudo-commit.
- `hive.image` is deliberately separate from `hive.commit` because the two can
  disagree (#3816): the Deployment spec can declare a digest the pod never
  pulled. Disagreement is itself signal (declared-but-not-running code), but
  `hive.commit` is always the authoritative anchor. Empty outside a cluster.

Span attribute, added automatically by `tracing.StartSpan` with zero call-site
changes:

- `hive.component` — the prefix of the span name before the first dot, under
  the existing `<component>.<operation>` naming convention
  (`governor.eval_cycle` → `governor`, `pr.merged` → `pr`).

## The coarse PR → reach mapping

Per the options weighed in #3973, mapping is **package-level**, not per-PR:

1. A PR's changed files map to packages (`src/pkg/governor/...` → `governor`,
   `src/cmd/hive/...` → the components whose spans main.go emits).
2. Package maps to the `hive.component` values its spans carry.
3. Reach query: spans where `hive.commit` ∈ {SHAs at-or-after the merge that
   contain the PR} AND `hive.component` ∈ {the PR's components}, grouped by
   `hive.id` for fleet distribution.

This is deliberately coarse: it answers "has the subsystem this PR touched
executed on a build containing it, and on how many hives" — not "did this exact
diff's lines run." Line/function-level attribution is out of scope for phase 1
(cost and cardinality; see the issue's options list).

## Phase 2 Implementation

The four design decisions, accepted in #3973:

- **D1 — Aggregation is spoke-side and rides the heartbeat.** A span-backend
  query can never see the pull-only spokes, and the `otel:` exporter is opt-in
  — most spokes export nowhere. A hub-side span query would report ~0 reach
  for most of the fleet and call it "unused": the anchoring-rule trap, one
  level up. The heartbeat is the outbound channel every spoke already opens.
- **D2 — Reach counters are independent of the OTel exporter.** They hook the
  same `tracing.StartSpan` call sites but count in-process regardless of
  exporter config. Every spoke reports; zero new network dependencies. Spans
  remain the deep-inspection layer where backends exist.
- **D3 — PR→component mapping with an honest `unattributable` bucket** (2b):
  `src/pkg/<name>/**` → `<name>`; `src/cmd/hive/**` → `main`; `src/proxy/**` →
  `proxy`; workflows/deploy/docs → `unattributable`, reported, never dropped.
- **D4 — Co-attribution when PRs batch into one deploy** (2c): PRs sharing a
  deploy window share reach and error-deltas by construction, labeled shared.
  No fake precision.

### Phase 2a (#3993)
Per-(component, running commit) counters — `spans_total`, `spans_error` (span ended
with error status), `first_seen`, `last_seen`, cumulative-since-boot plus a rolling
1h bucket — capped at `tracing.MaxReachComponents` (64) with overflow counted and
truncation logged. Persisted to `/data/reach-state.json`. Spoke heartbeats include
`component_reach`, which the hub sanitizes and indexes.

### Phase 2b (#3994)
Ancestry-joined reach metrics:
- `PRReachReport` with `Attribution`, `DeployWindow`, `SharedWith` (D4 co-attribution),
  `ReachHives`, `ReachCount`, `FirstExecution`, `FirstExecutionLatencySeconds`, and
  `NeverRan` flag with grace period suppression.
- `GET /api/reach` endpoint behind `requireAdmin`.

### Phase 2c (#3995)
- **Error-rate delta pre/post deploy**: Computes `ErrorRateBefore`, `ErrorRateAfter`,
  and `ErrorRateDelta` across the commit boundary ($C_{\text{prev}} \to C_{\text{curr}}$)
  per (component, deploy-window), including full `ComponentErrorDeltas` breakdown.
- **ACMM Advisor wiring**: `PRReachRate` is wired alongside `MergeSuccessRate`
  (#3972) in `acmmadvisor.Signals` and `StatusInputs`. Reach answers "did anyone use it",
  merge-success answers "did it last", acceptance answers "did I approve it" —
  complements, never collapsed into one number. Zero when unmeasured (never-fabricate).
- **Status surface reach table**: Rendered on the existing status surface alongside
  ACMM recommendation and lifecycle timeline in `src/pkg/dashboard/static/index.html`.

## Testing

Comprehensive test suites across the reach pipeline:
- `pkg/tracing/reach_test.go`: Spoke counters, persistence, overflow cap, and rolling window.
- `pkg/reach/mapping_test.go`: File-to-component mapping and attribution coverage.
- `pkg/reach/windows_test.go`: Deploy window derivation and D4 co-attribution assignment.
- `pkg/reach/metrics_test.go`: Ancestry join, latency, and never-ran grace periods.
- `pkg/reach/error_rate_test.go`: Pre/post deploy error deltas and PR reach rate.
- `pkg/acmmadvisor/acmmadvisor_test.go`: Reach rate threshold evaluations and pass-through.
- `pkg/hub/reach_api_test.go`: Hub `/api/reach` authentication, join, and error deltas.
- `pkg/dashboard/api_reach_test.go`: Spoke dashboard `/api/reach` handler and advisor wiring.
- `pkg/dashboard/static_index_test.go`: Static status surface reach table markup and handlers.
