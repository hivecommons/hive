# ADR-0020: Spek continuous convergence

## Status

Accepted.

## Context

Spektacular campaigns previously ran `spec -> plan -> implement` once. That
converges one generation, but long-lived projects drift as the codebase and
surrounding requirements change.

## Decision

Continuous convergence is opt-in under `runs.spektacular.recheck`. A due
campaign creates a linked revision (`revision_of`, `revision`) and starts that
revision at `spec`; the original campaign is never overwritten. Manual
`POST /api/campaigns/{id}/recheck` uses the same path, returns `409` if a
revision is already in flight, and requires either enabled rechecks or
`?force=true`.

Cadence is scheduler-owned and runs from the existing Spek runner tick. The
default interval is `168h`; per-campaign metadata can override it. Recheck
revisions keep the existing spec and plan human approval checkpoints.

When the revision plan is final, Hive diffs its task list against the prior
final plan by normalized task-content hash. Only new or changed tasks are
imported into the planner. The import is capped by
`runs.spektacular.recheck.max_delta_tasks` (default `50`) and records an audit
entry when capped. Empty deltas create no planner children.

Each revision records drift evidence:

- `codebase_changed`
- `prior_revision`
- `delta_count`
- `recheck_reason` (`cadence` or `manual`)

The codebase evidence source interface is `RecheckEvidenceSource`. Optional
external document snapshots are declared in `recheck.sources` and retained as
`drift.drift_source` before the revision starts its spec lease. They require an
explicit relay egress proxy and exact-host allow-list, never follow redirects
or links, and have fixed request, time and response-size bounds. Empty sources
perform no discovery network access. Snapshots are untrusted evidence only;
they do not bypass human checkpoints or automatically apply external changes.

## Consequences

Operators can keep a Spek campaign converging over time without duplicating
unchanged planner work. The generation contract stays durable and review-gated,
while future evidence sources can be added behind the same interface.
