# PR reach telemetry (#3973)

Status: phase 1 landed (version-stamped spans + component attribute); phase 2
design accepted (2a implemented; 2b #3994 / 2c #3995 pending).

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

## The coarse PR → reach mapping (phase 1 granularity)

Per the options weighed in #3973, phase 1 is **package-level**, not per-PR:

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

## Phase 2 (accepted)

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
  `v2/pkg/<name>/**` → `<name>`; `v2/cmd/hive/**` → `main`; `v2/proxy/**` →
  `proxy`; workflows/deploy/docs → `unattributable`, reported, never dropped.
- **D4 — Co-attribution when PRs batch into one deploy** (2c): PRs sharing a
  deploy window share reach and error-deltas by construction, labeled shared.
  No fake precision.

Phase 2a implementation (#3993): per-(component, running commit) counters —
`spans_total`, `spans_error` (span ended with error status), `first_seen`,
`last_seen`, cumulative-since-boot plus a rolling 1h bucket — capped at
`tracing.MaxReachComponents` (64) with overflow counted and truncation logged,
never silent. Persisted to `/data/reach-state.json` (its own file, not the
main state file — resolved OQ-2) on the existing state-save cadence; commit
keying means a new binary starts fresh keys naturally. The heartbeat carries
the report as `component_reach`; the hub sanitizes and clips it (same 64-entry
cap — a hostile spoke must not grow hub memory) and stores the latest report
per hive on the registry entry. Storage only: no endpoint, no mapping, no UI
until #3994. The entry keys `component` / `commit` / `spans_total` /
`spans_error` / `first_seen` / `last_seen` are 2b's fixed read interface.

## What phase 2+ needs (deferred)

- **Error-rate deltas pre/post merge**: compare span error status rates for a
  component across the commit boundary that introduced a PR. Needs span status
  discipline (`span.SetStatus` on failure paths) audited per component first.
- **First-execution latency**: time from merge → first span carrying a
  `hive.commit` that contains the PR. Needs a merge-SHA → containing-build
  index (the hub already stores per-spoke `GitHash`; join there).
- **Per-hive distinct counts**: `count_distinct(hive.id)` per (commit,
  component) — the raw data lands with phase 1, the aggregation/query layer is
  future work.
- **Finer-than-package mapping**, if ever justified: per-function span naming
  or PR-annotation of spans. Explicitly rejected for phase 1.

Non-goals at any phase soon: dashboards, an aggregation service, new HTTP
endpoints. Phase 1 is emit-only; queries run in whatever OTLP backend the
collector feeds.

## Testing

`pkg/tracing/tracing_test.go` asserts: commit/image attributes present on the
resource (`newResource`), placeholder `unknown` version omitted, empty image
omitted, `hive.component` auto-attached by `StartSpan`, and the span-name →
component mapping. Version *injection* is the linker's job (Dockerfile
`-ldflags -X main.gitShort=...`); tests cover the plumbing function, not the
linker.
