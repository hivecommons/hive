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

## Candidate producer trust boundary

The reusable technical workflows accept a typed `integration_candidate` input
that defaults to false. False preserves ordinary PR, push and native
`merge_group` behavior. Candidate calls require an explicitly dispatched
`refs/tags/hive-integration/` ref, run on disposable GitHub-hosted runners with
read-only technical-job permissions, do not persist checkout credentials, and
retain full test/coverage requirements without manual-run toolchain soft failure.
Their distinct concurrency groups do not cancel another reusable technical
workflow in the same caller run. Candidate code never runs in issue/release
writers; no repository secrets may be inherited by a candidate caller.

These call seams and the producer verifier are source support, not an installed
producer, execution controller, runtime opt-in, protection migration or
execution-readiness claim. Existing authoring enforce mode does not activate
them. The current eleven-context destination contract still needs genuine
candidate-aware producers; author-head green statuses must not be copied.

The producer identity protocol uses
[GitHub OIDC claims](https://docs.github.com/en/actions/reference/security/oidc)
including the actual finalizer `check_run_id`, run/attempt, repository/actor
identities, workflow SHA/ref and hosted runner environment. A custom audience
binds the SHA-256 digest of the exact payload bytes. Only the trusted finalizer
may obtain signing identity; untrusted technical jobs receive no such
permission. Verification uses only GitHub's fixed issuer/public signing keys,
without installation credentials or token-selected key URLs. Refreshes are
bounded/coalesced, and no cache mutex spans remote I/O. Missing issuer keys or
claims are unknown, not passing validation.

A signed producer identity is not proof that all CI passed. The consumer must
separately establish exact subject/tree/ordered-head membership, installed
reviewed workflow/action closure, real successful finalizer and required
technical jobs, immutable artifact identity/digest and complete observations.
Signed issuance must fall within the independently observed finalizer interval;
a historical attestation is not a currently valid bearer login or live
promotion authorization. Rotation/durable verification history and the actual
isolated provider execution remain required lifecycle work.

Executing untrusted candidate code from a default-ref dispatch can expose that
ref's Actions-cache namespace even with a read-only GitHub token. The intended
producer therefore requires controlled immutable candidate refs and verified
trusted definitions, not an arbitrary author workflow. Publication with a
trusted control workflow's `GITHUB_TOKEN` suppresses push-triggered workflows;
[GitHub documents explicit dispatch and certain approval-required PR-event
exceptions](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow).
The installation's trigger permissions, controlled-ref protections and real
candidate-required-context compatibility must be verified before opt-in. This
feature does not grant those permissions, change protections or install a live
pilot.

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
not skipping validation. The bounded authoring/admission layer supplies neither ML prioritization,
custom batching, batch bisection nor another merge authority. Native group
observations only describe groups GitHub created. The separately opt-in
candidate producer source described below constructs exact speculative prefixes;
it is not an installed promotion controller.

There is no automatic live pilot, adaptive concurrency growth, runner
procurement or claim of 5,000 merged PRs/day. That is an empirical capacity
target requiring observed quality and retained outcomes, not a code milestone.

## Optional candidate producer protocol: source phase

The reference source now includes a real preparation/validation workflow graph,
not just a green-status copier:

- `integration-candidate-prepare.yml` accepts an explicitly installed
  `hive.integration.prepare.v1` repository-dispatch transport, or an authorized
  manual workflow dispatch. It only uses trusted default-branch control code.
  The checked-in `.github/integration-producer-policy.json` is **disabled**, has
  no authorized actor and has no enrolled repo/base. Installation is not
  activation; existing authoring `enforce` does not enable a writer.
- The trusted materializer never checks out or executes candidate code. It
  fetches exact objects, obtains the actual merge base for each prior synthetic
  prefix and next head, uses `git merge-tree`, and creates two-parent commits
  with deterministic metadata and actual bot DCO sign-off. It refuses changed
  installed policy/harness paths, different heads, forks and moved starting
  bases. Clean earlier prefixes are retained if a later prefix conflicts.
- Each prefix is published as a controlled tag using the trusted job's
  `GITHUB_TOKEN`, then validation is dispatched immediately rather than waiting
  for the tail. The tag is never force-updated; author and destination refs are
  not changed. Default-ref dispatch does **not** execute candidate code.
- `integration-candidate.yml` checks the actual commit/tree/two-parent ordered
  lineage in one bounded GraphQL query before assigning technical jobs. Its
  three callee workflows are mechanically derived from the landed CI/test/
  security sources, with writer jobs absent, hosted runners and checkout
  credentials explicitly disabled. Mutable runner/cache/selection variables
  are fixed in the qualified candidate profile; test selection is disabled.
- An isolated finalizer, with no checkout or candidate execution, signs exact
  receipt bytes using job-bound GitHub OIDC and publishes an immutable
  attempt-specific artifact. Candidate validation and trusted materialization
  use distinct audiences/ref/event profiles. A receipt authenticates provenance:
  **its successful publication is not passing CI or promotion authorization**.

Ordinary PR/push/native-group workflows retain their typed default-false path.
To update source workflow definitions, regenerate the call-only artifacts with
`python3 .github/scripts/generate-integration-ci.py`. Canonical CI, not a local
guard run, checks generation and the materializer/isolation contracts. This
avoids granting extra permissions to a virtual caller merely because skipped
legacy reporter/release jobs still declare writes.

### Cross-hive ownership and explicit limits

The producer uses an atomic create-only forge ref for each exact initial
candidate CI-dispatch key. The intent records the trusted workflow run/attempt
and a random invocation nonce before the dispatch RPC. A competing materializer
becomes an observer; unreadable or ambiguous intent becomes unknown. The
candidate preflight rechecks that exact ref/owner before technical execution.
Reservations expiring, a process restarting or an elapsed cooldown do not
authorize replacing this intent. There is no force update, TTL steal or automatic
failover. These create-only records are audit/attempt ownership, not permanent
source-file locks.

This is narrowly **initial candidate validation**, not an implementation of
legacy PR CI rerun authority. The independent CI-reporting reconciler supplies
verified head/run/attempt/job observations and `owner-action-required`; an
observation is not mutation permission. Neither the local journal/flock,
post-effect comments nor destination CAS coordinates independent hive storage.
No second legacy rerun owner is installed here.

The initial supported execution model must explicitly enroll its designated
requesting principal and trusted workflow executor; other hives observe.
Namespace protections, all participating writer routing and credential custody
remain installation prerequisites. Cooperative create-only ownership does not
prevent an administrator deleting an intent or an unsupported writer ignoring
the protocol. GitHub's Actions dispatch/rerun RPC does not consume a caller
fencing/idempotency token: forge CAS alone cannot fence a paused RPC sender.
Ambiguous effects must be reconciled, not automatically reassigned after a TTL.
Bootstrap repository-dispatch/control-job overhead is real execution cost;
duplicate event delivery is not claimed to be free or exactly-once.

[GitHub documents installation-token Contents:write for repository dispatch](https://docs.github.com/en/rest/repos/repos#create-a-repository-dispatch-event)
and [its trusted default-branch SHA/ref](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#repository_dispatch).
This is an explicitly selected transport, **not** fallback after an Actions
permission error. No App Actions-write grant is requested. Only the trusted
materializer job has the minimum job-scoped Contents/Actions writes needed to
publish controlled refs and dispatch validation; technical jobs are read-only,
and only isolated finalizers have OIDC permission.

### Remaining readiness gates

Source contract tests are not isolated provider execution proof. The actual
artifact/run/job consumer, durable verification/key history, execution
controller and shared promotion authority handover are still required. Existing
eleven protected-branch contexts do not automatically become satisfied by
prefixed reusable jobs. No copied DCO/review greens, branch-rule bypass,
signature/linear-history downgrade or unproved conditional no-op is permitted.
Protection-compatible exact-commit promotion must be verified using the actual
designated actor and equivalent gates in an isolated installation first.

No producer, execution mode, rules, permissions, repository variable, image or
runtime has been activated by this source work. Source SHA, image publication,
deployed runtime, desired mode and effective writer are distinct facts.

| Metric | Current claim |
| --- | --- |
| Eligible-to-start / CI-to-promotion / end-to-end p50 and p95 | Unknown: no engine benchmark |
| Successful integrations/day attributable to this engine | Unknown: execution is OFF |
| Runner-minutes/success, canceled/rebuild waste, failure recovery | Unknown: actual job accounting/controlled comparison required |
| Global merges observed by the overnight collector | Existing-flow baseline, not autonomous attribution to this feature |

### Decision-to-evidence-to-test mapping

| Decision | Source/result and limitation | Required check |
| --- | --- | --- |
| Parallel exact prefixes, not textual independence | [Full FSE 2023 paper](https://arxiv.org/html/2308.13129): historical simulation, no merge conflicts/rerun testing, compile time excluded; not Hive production proof | Exact ordered parents/trees; actual conflict handling; publication precedes later materialization |
| No fixed batch-fill delay, ML or speculative proof transfer | PRediBatch primary abstract reports simulation; batching saves compute only under workload/recovery assumptions | Identical workload/resource/policy comparison; record discarded execution and rebuild cost |
| Preserve documented promoted prefixes; invalidate changed dependent subjects | [Zuul speculative execution](https://zuul-ci.org/docs/zuul/latest/gating.html) and GitHub merge-group semantics; provider behavior is not Hive parity | Prefix identity stable across observer/lease generations; never recast A+B proof as B-only |
| Fixed bounded admission before adaptation | Little's-law capacity accounting describes average WIP, not tail latency or fairness | Assigned-job wall minutes, shared-job dedup, unknown completeness, aging/fairness and explicit limits |
| Trusted control, isolated technical jobs/finalizers | GitHub trigger/cache/OIDC contracts; provider receipts and protections still need isolated verification | No candidate checkout in privileged/finalizer jobs; exact job-bound identities, no skipped writer permissions, ambiguous claims never retried blindly |
