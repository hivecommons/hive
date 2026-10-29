# ADR-0020: Spek continuous convergence

## Status

Accepted.

**Spek** is the Spektacular spec → plan → implement workflow. A **campaign** is
one tracked run; a **revision** is a linked new run that rechecks it without
overwriting its history.
A **spec lease** is the runner's ownership of the specification-stage work;
a **delta** is the new or changed task set, not the whole prior plan.

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

The outward-looking half has separate opt-ins and safety boundaries:

- **Typed signals:** `runs.spektacular.recheck.discovery` accepts typed
  release/activity/feed signals. It rejects undeclared kinds and hosts outside
  `variables.security.http_allowlist`, records evidence on `drift.external`,
  and includes that evidence as spec-stage prompt context.
- **Exact documents:** `runs.spektacular.recheck.sources` retains snapshots as
  `drift.drift_source` before the revision starts its spec lease. It requires
  an explicit relay egress proxy and exact-host allow-list, never follows
  redirects or links, and has fixed request, time and response-size bounds.
- **No implicit access:** empty source lists perform no discovery network access.
- **No new authority:** all discovery is read-only, non-fatal and untrusted
  evidence only. Failures are recorded; discovery never bypasses human
  checkpoints or automatically applies external changes.

## Consequences

Operators can keep a Spek campaign converging over time without duplicating
unchanged planner work. The generation contract stays durable and review-gated,
while future evidence sources can be added behind the same interface.
