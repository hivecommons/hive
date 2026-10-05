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
- optional `external[]`, `external_count`, and `sources_failed[]` discovery
  evidence when outward discovery is enabled

The evidence source interface is `RecheckEvidenceSource`.

### Discovery sources

The outward-looking half is opt-in under
`runs.spektacular.recheck.discovery`. Operators declare bounded sources of kind
`upstream_release`, `repo_activity`, `standards_feed`, or `landscape`; Hive
rejects undeclared kinds and hosts outside `variables.security.http_allowlist`.
Discovery runs before the recheck `spec` stage generator, records evidence on
the linked revision's `drift.external` block, and includes that evidence in the
spec-stage prompt as context. It is read-only and non-fatal: source failures are
recorded in `sources_failed` and never block the recheck. Evidence is never
auto-applied; the existing spec and plan human checkpoints remain authoritative.

## Consequences

Operators can keep a Spek campaign converging over time without duplicating
unchanged planner work. The generation contract stays durable and review-gated,
while future evidence sources can be added behind the same interface.
