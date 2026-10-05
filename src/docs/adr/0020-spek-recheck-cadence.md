# ADR-0020: Spek recheck cadence and delta generations

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

The evidence source interface is `RecheckEvidenceSource`. This ADR accepts only
the codebase-head source for now; external upstream/competitor/standards
discovery remains a follow-up.

## Consequences

Operators can keep a Spek campaign converging over time without duplicating
unchanged planner work. The generation contract stays durable and review-gated,
while future evidence sources can be added behind the same interface.
