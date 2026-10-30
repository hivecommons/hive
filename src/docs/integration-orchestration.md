# Integration orchestration

This layer coordinates authoring admission above existing coordinated preparation
and GitHub's native queue. It is not a merge engine and does not activate either
authority. Every existing CI, review, trust, DCO, coverage, soak, hold and
current-base guard remains authoritative. Nothing deploys automatically.

## Configuration and rollback

```yaml
integration_orchestration:
  mode: off                 # off | advisory | enforce; default off
  scopes:                   # exact allowlist; no wildcard or empty enrollment
    - repo: example/project
      base: release
  lease_ttl: 5m             # 1–15m, clipped to the underlying live claim
  window: 1h                # 1m–24h
  max_active: 2             # 1–32 per repository/base
  max_runner_minutes: 60
  max_jobs: 20
  max_attempts: 2
  max_observation_age: 30m  # 1m–1h
```

OFF preserves dispatch and never observes workflow events or polls. ADVISORY
records intents and observations but cannot withhold work. ENFORCE additionally
withholds scoped new authoring admissions on validated unsatisfied/unknown DAG
edges, incomplete/stale CI accounting, exhausted fixed budgets or active slots.
Native admission affects only **new** Hive-owned enqueues: it does not cancel,
reorder or withdraw queued work. Direct merges do not wait on a capacity budget
after they have already completed their mandatory checks.

Settings → Features exposes the existing configuration dialog controls and
owner-only evidence inspector. Scope ambiguity across multiple bases is unknown,
not inferred from an issue. The existing contributor queue priorities, own-work
preference and failure routing remain ahead of within-band intent aging.

Changing to advisory/off releases only this layer's reservations. Claim and
native queue state is never deleted. Evidence and the append-only journal remain
for diagnosis. A corrupt journal refuses observations/enforcement rather than
resetting to empty. Runtime configuration writes report persistence failure.

## Owner API

- `GET /api/integration-orchestration`: configured mode, runtime commit, schema,
  source, writer limitations, intents, exact candidate observations, jobs,
  coverage and live reservations. Unknown evidence is not a green status.
- `PUT /api/config/integration-orchestration`: validated full configuration.
  Duration JSON values are nanoseconds; YAML uses Go duration strings.
- `POST /api/integration-orchestration/intents`: versioned canonical work key,
  repository/base, generation, plan/node/revision, anticipated paths, components,
  contracts, generated outputs, scope confidence/provenance and prerequisites.
  Hard prerequisite edges must match bound child nodes in the current approved
  plan DAG. Textual/semantic hints never authorize a hard edge.
- `POST /api/integration-orchestration/reconcile`: bounded observation repair;
  shared with the governor's three-minute minimum reconciliation cadence.

Dispatch creates unknown-scope intents for previously unseen canonical work.
An owner/planner can replace one with a higher generation. The original issue
claim/lease remains ownership authority; shared prerequisite work is not cloned.
Reservations renew only with a live authoritative lease, expire after a bounded
TTL, and are released on intent generation changes, failed lease persistence,
claim release/takeover or rollback. Same-file and cross-file semantic relations
are distinct advisory hints, never global file locks.

## Candidate and CI contracts

The versioned candidate identity includes ordered PR/full-head membership,
repository/base, authority/ref, actual tested commit/tree, workflow, policy,
producer, attempt and generation. It is deliberately separate from convergence's
single-PR base observation. Removing a predecessor produces a different subject:
no proof is copied to the successor. Hold/review/member-head changes supersede
affected observations; a merged native prefix is not automatically discarded.

Signed `merge_group` observations read the bounded authoritative queue prefix,
its tested tree and synthetic ref. A queue entry ID alone is not group identity.
Membership observations are **not** a passing policy attestation: until the
trusted group-aware producer supplies complete workflow/check-suite/job/policy
binding, policy evidence remains unknown. Native readiness/activation and
external DCO provider support are separately owned prerequisites; this layer
cannot manufacture them.

Actual GitHub workflow jobs are keyed by repository/job/run attempt, so shared
jobs count once. Accounting clips assigned-runner wall time to the observation
window, includes canceled execution, and excludes queued/skipped work that never
executed. Unfinished work is accounted only through its last observation.
Incomplete cohorts, ambiguous base attribution and bounded pagination overflow
are unknown, not zero. Reconciliation has fixed limits; it does not create one
poller per run or cancel shared/safety-network jobs.

There is no automatic live pilot, adaptive concurrency growth, runner
procurement or claim of 5,000 merged PRs/day. That is an empirical capacity
target requiring observed quality and retained outcomes, not a code milestone.
