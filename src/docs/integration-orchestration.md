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
  admission_runner_minutes: 10 # explicit outstanding allocation per admission
  admission_jobs: 1            # explicit fan-out allocation, not predicted capacity
  policies: []                # optional pinned existing workflow reader contracts
```

OFF preserves dispatch and never observes workflow events or polls. ADVISORY
records intents and observations but cannot withhold work. ENFORCE additionally
withholds scoped new authoring admissions on unknown scope, validated unsatisfied/unknown DAG
edges, incomplete/stale CI accounting, exhausted fixed budgets or active slots.
Native admission affects only **new** Hive-owned enqueues: it does not cancel,
reorder or withdraw queued work. Direct merges do not wait on a capacity budget
after they have already completed their mandatory checks.

The fixed budgets include both observed execution and outstanding admission
allocations. These are admission charges, not control over GitHub's physical
fan-out or queue build window. A confirmed, producer-bound failed combined
candidate reduces future Hive admission slots, bounded between one and the
configured maximum, for the accounting window. It does not change configuration,
cancel existing work, reorder native entries, or increase capacity. Exact-head
failed/canceled/timed-out attempts also stop new admissions at `max_attempts`,
leaving the existing owner-first repair/cooldown path responsible.

Settings → Features exposes the existing configuration dialog controls and
owner-only evidence inspector. Scope ambiguity across multiple bases is unknown,
not inferred from an issue. The existing contributor queue priorities, own-work
preference and failure routing remain ahead of within-band intent aging.

Every configuration transition releases only this layer's reservations. Claim and
native queue state is never deleted. Evidence and the append-only journal remain
for diagnosis. A corrupt journal refuses observations/enforcement rather than
resetting to empty. Runtime configuration writes report persistence failure.
Disabling remains possible if the journal is unavailable. Re-enabling enforcement
requires healthy durable state; cleanup failure never deletes the underlying claim.

## Owner API

- `GET /api/integration-orchestration`: configured mode, runtime commit, schema,
  source, writer limitations, intents, exact candidate observations, jobs,
  coverage, drift and live reservations. Actual configured preparation/native
  flags are distinct from the explicitly unknown effective merge writer.
  Window CI cost, observed/complete merged denominators and shared-job
  attribution are separate; an unknown denominator is null, never zero.
- `PUT /api/config/integration-orchestration`: validated full configuration.
  Duration JSON values are nanoseconds; YAML uses Go duration strings.
- `POST /api/integration-orchestration/intents`: versioned canonical work key,
  repository/base, generation, plan/node/revision, anticipated paths, components,
  contracts, generated outputs, scope confidence/provenance and prerequisites.
  Hard prerequisite edges must match bound child nodes in the current approved
  plan DAG. Textual/semantic hints never authorize a hard edge.
  Prerequisites distinguish `developed`, `integrated` (the default) and `deployed`.
  Closure only satisfies an explicitly developed edge; integration requires a
  real merged-PR receipt linked by the approved plan. Deployment stays unknown
  without an authoritative deployment producer.
- `POST /api/integration-orchestration/reconcile`: bounded observation repair;
  shared with the governor's three-minute minimum reconciliation cadence.

Dispatch creates unknown-scope intents for previously unseen canonical work and
attaches an existing approved DAG when a canonical issue/external-ref binding is
available. Ambiguous/missing bindings remain unknown, not invented dependencies.
An owner/planner can replace one with a higher generation. The original issue
claim/lease remains ownership authority; shared prerequisite work is not cloned.
Reservations renew only with a live authoritative lease, expire after a bounded
TTL, and are released on intent generation changes, failed lease persistence,
claim release/takeover or rollback. Same-file and cross-file semantic relations
are distinct advisory hints, never global file locks. Shadow evaluation compares
existing live-claim scopes without taking reservations. The inspector's
`dispatch_advice` exposes the last actual internal/contributor evaluations,
including semantic hints, false-positive assessments and would-withhold reasons.
These bounded, boot-local diagnostics are historical advice, not durable merge
proof or a new queue; OFF records none. The durable ledger retains the underlying
intent, claim-bound reservation and candidate evidence contracts separately.

The existing head-keyed merge-train file probe feeds observed scope drift without
another GitHub file scanner. Its head freshness is explicitly unverified, so drift
does not become a hard dependency or passing proof. Cached hotspot analysis is
included in the inspector. An intent's optional `relations` records a canonical
related `key`, `kind` (`semantic` or `file`), `disposition` (`unreviewed`,
`confirmed`, `false-positive`) and `provenance`; assessments annotate actual
dispatch hints and never create locks. Approved-plan changes, intent generations
and holder/generation-fenced releases prevent stale owners deleting new leases.

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

A pinned reader contract contains exact `repo`, `base`, `engine`
(`native` or `direct`), workflow path, full workflow `blob` SHA, producer
`app_id`, and exact required `jobs`. The reader verifies event/source/path,
workflow blob on both the tested revision and trusted target base, workflow
run/attempt, source/target repositories, suite, physical job membership and check
App/head/suite/name. One receipt is **one workflow**, not all CI/review/trust gates.
Changing/removing a policy supersedes affected historical receipts durably.
`observed_state` retains the producer outcome separately from derived
supersession. Bounded reconciliation can complete unavailable proof at the same
workflow update timestamp; late waiting/unknown events cannot regress completed
proof, and contradictory equal-time terminal evidence fails conservatively.
Destroyed-group and member-change tombstones prevent out-of-order old receipts
reviving after restart. A documented native prefix promotion retains the original
subject only when the exact remaining chain and tested tree/ref match; it never
copies an A+B receipt onto an unrelated B-only subject.

Direct-path checkout attestation is an explicit external producer dependency.
The pinned workflow must publish artifact `hive-integration-subject-v1-<attempt>`
containing only `subject.json`:

```json
{
  "version": 1,
  "repo": "example/project",
  "base": "release",
  "base_sha": "<full event base SHA>",
  "tested_sha": "<actual checkout merge commit SHA>",
  "tested_tree": "<actual checkout tree SHA>",
  "run_id": 123,
  "attempt": 1,
  "policy": "<configured contract fingerprint from inspector>",
  "members": [{"pr": 42, "head": "<full exact author head SHA>"}]
}
```

The consumer checks the artifact's run/head/attempt, pinned producer contract,
GitHub commit/tree and both merge parents (event base plus exact author head).
Downloads are size-bounded, in memory, without sending GitHub credentials to
artifact storage. No publisher, workflow, provider or queue activation is installed
by this feature. Missing or unsupported attestations remain unknown;
generic author workflow metadata is not actual combined-checkout proof.

Actual GitHub workflow jobs are keyed by repository/job/run attempt, so shared
jobs count once. Reordered queued deliveries and partial completion metadata
cannot erase already observed physical runner execution. Accounting clips
assigned-runner wall time to the observation
window, includes canceled execution, and excludes queued/skipped work that never
executed. Unfinished work is accounted only through its last observation.
Incomplete cohorts, ambiguous base attribution and bounded pagination overflow
are unknown, not zero. Reconciliation has fixed limits; it does not create one
poller per run or cancel shared/safety-network jobs.

The shared reconciliation batch has a three-minute minimum interval and
45-second deadline: at most eight scopes, eight relevant runs per scope,
24 attempt-job reads and four policy-run proof reads, with a 100-item page bound.
Scope service rotates between batches. It repairs missed job, merged-PR and
pinned-policy observations, reconstructing native membership from the live
authoritative queue when a group delivery was missed. Creation time is not used
to exclude older completed executions. A repository whose historical listing
exceeds the bounded page remains incomplete; this deliberately blocks that
cohort's enforcement activation rather than claiming scale-grade telemetry.
The merged-PR denominator uses a separately bounded updated-order closed-PR
listing. Reported minutes per merged PR are an observed-window aggregate, not a
claim that every individual change's eventual cost is known. Unfinished execution
and unattributed jobs remain explicit.

The append-only journal uses local cross-process fencing, suffix replay and
fsync; no remote reads occur under its lock. Replay is bounded to 256 MiB,
100,000 records and 1 MiB per record. At a bound or corruption, new observations
and admission fail explicitly; audit is retained and disabling remains possible.
There is no automatic archive/compaction or unlimited-throughput claim.

## Existing preparation visibility

The inspector composes the existing cached
`/api/stage-tracker.base_preparation` DTO and cached hotspot source; it adds no
tracker polling, store reader, queue seats or preparation priority algorithm.
Current preparation ownership/backlog, inactive tickets and unverified tickets
are not ready-train membership or native queue entries. The standalone
preparation visibility and bounded review-owner handoff remain useful with this
layer OFF. Source landing is not publication or runtime rollout.

## Design evidence and exclusions

The reviewed primary abstracts are useful challenges, not Hive benchmarks:
[ICSME 2025 prioritization](https://arxiv.org/abs/2508.08342) studies historical
CI/metadata prioritization against FIFO;
[FSE 2023 parallel batch testing](https://2023.esec-fse.org/details/fse-2023-research-papers/14/Accelerating-Continuous-Integration-with-Parallel-Batch-Testing)
exposes nonlinear machine/feedback tradeoffs; and
[ICST 2026 PRediBatch](https://conf.researchr.org/details/icst-2026/icst-2026-industry/8/Predictive-Pull-Request-Batching-to-Accelerate-Merge-Pipelines-in-Continuous-Integrat)
reports a BMW-data simulation, not Hive production results. These support checking
bounded work-in-progress, anti-starvation and invalidation/failed-execution cost,
not skipping validation. This implementation supplies neither ML prioritization,
custom batching, speculative group construction, batch bisection nor another
merge authority. Native group observations only describe groups GitHub created.

There is no automatic live pilot, adaptive concurrency growth, runner
procurement or claim of 5,000 merged PRs/day. That is an empirical capacity
target requiring observed quality and retained outcomes, not a code milestone.
