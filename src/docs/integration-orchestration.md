# Integration orchestration

This layer coordinates authoring admission above existing coordinated preparation
and GitHub's native queue. It is not a merge engine and does not activate either
authority. Every existing CI, review, trust, DCO, coverage, soak, hold and
current-base guard remains authoritative. Nothing deploys automatically.

## Trusted candidate required-context profile

The candidate entry workflow is generated from
`.github/integration-candidate.template.yml` and the existing technical CI
sources by `.github/scripts/generate-integration-ci.py`. Its six technical
required contexts execute the actual top-level build, lint, test, coverage,
gosec and govulncheck jobs, with their full prerequisite DAG. They are not
aliases of reusable-workflow checks or copied author-head statuses. Every
technical job, including jobs using `always()`, requires successful immutable
subject validation before execution. Candidate shard cancellation cannot count
as complete testing.

The five governance contexts run trusted read-only policy:

* `gate` validates the immutable tag, repository, ordered parents/trees,
  workflow closure and exact shared single-attempt owner. Candidates cannot
  publish release images. This gate is **not** an ARM/Docker build receipt.
* `DCO` validates the full introduced commit range, without bot/waiver
  shortcuts, and explicitly checks each generated merge's own author signoff.
* `changelog-fragment-guard` validates every member separately against the
  immutable base. One member's fragment cannot excuse another member. Combined
  citation checks still apply; candidate validation never autofixes, pushes
  or comments.
* `verify PR contents` runs a read-only adaptation of the pure title/type and
  issue-reference rules in `kubernetes-sigs/kubebuilder-release-tools` commit
  `012269a88fa4c034a0acf1ba84c26b195c0dbab4` (v0.4.3, Apache-2.0). It does not
  execute the advisory image wrapper, grant its checks-writing permissions,
  silently accept an unavailable image, or impersonate the DCO App. Unsupported
  titles, including prefixes not accepted by that immutable policy, fail
  explicitly rather than nominally succeeding.
* `Sign-off survives the squash` validates the original exact member heads and
  authors under their reviewed merge policy. Bounded read-only forge GraphQL
  batches resolve original commit-author identities before invoking the pinned
  checker; an unavailable/unlinked author cannot use its legacy degraded-success
  fallback. That context stops explicitly until truthful attribution is
  available. The candidate itself is a tested merge object, not a fabricated
  bot-authored squash PR.

The verifier's enrolled `.github` closure contains a literal pin to the
`src/scripts` tree, not merely a comparison against caller-supplied
`preparedBy.sourceSHA`. Both the declared source and checkout must match that
pin before any checker executes. Helper changes require a deliberately reviewed
pin/profile update; choosing the candidate itself as a new source cannot
authorize changed verifier code.

Candidate receipt schema 2 signs the actual job/check/App/suite identities and
conclusions for all eleven contexts, plus the exact subject/run/attempt and a
digest of member author, head/ref, base identity, draft, title, body and labels.
Only the isolated OIDC finalizer has `id-token: write`; technical/control jobs
have no write authority. Missing, duplicate, skipped, foreign-head or
wrong-producer jobs cannot become passing candidate proof. The observer
verifies those physical identities against actual Actions/check observations;
the live promotion guard re-reads current member metadata. State/base/time
changes from a documented exact-prefix merge do not themselves invalidate
otherwise identical metadata. Historical schema 1 remains replayable, but
cannot satisfy the new literal protected-context profile.
Metadata drift invalidates proof; it does not authorize an automatic rerun or
steal an existing dispatch claim. Existing bounded owner-repair/authority rules
still apply.

This source profile does **not** enable the producer policy, create authority,
change GitHub protection or App permissions, dispatch candidates, deploy a
runtime, or replace isolated protected-promotion qualification. A bounded
read on 2026-10-31 found no resolved v5 rulesets and all eleven classic required
contexts unbound to a specific App; that user-credential observation is not
proof that the live installation App can promote. Any additional required
image/architecture or producer contract must be genuinely qualified, not
inferred from a check name. Execution remains OFF until separately authorized.
The composed DAG includes prerequisite/matrix jobs beyond the eleven context
names. Qualification must measure that actual fanout and assigned-runner cost
against explicitly configured admission bounds; eleven names are not an
eleven-job capacity or throughput claim.

## Exact protected-promotion protocol

The authoring layer remains distinct from separately opted-in Hive-owned
candidate execution. Neither the parent `mode: enforce` nor upgrading Hive
authorizes a new destination writer. The source includes the trusted producer,
execution controller, independent configuration and inline tracker captions
described below. Source availability is not a deployed or qualified executor.
Execution OFF and ADVISORY never dispatch or promote; existing merge lanes
retain their normal gates and route enrolled execution subjects explicitly.

The first protocol is `integration-first-parent-v1`, intentionally limited to
preserving-history merge commits on GitHub. For a tested step `S`, its ordered
parents must be exactly `[expected destination, exact author head]`; its commit
OID and tree must equal the recorded tested OIDs. Promotion moves the destination
to **that existing commit**, not a newly generated squash/rebase/merge result
and not a merely similar tree. Linear-history-only or mandatory-signature
destinations without a compatible producer are unsupported, not bypassed.

GitHub documents an actual compare-and-swap:
[`updateRefs` with `RefUpdate.beforeOid`, `afterOid`, and `force:false`](https://docs.github.com/en/graphql/reference/git#updaterefs).
All listed updates are atomic. The controller updates one destination while
conditionally keeping the same-repository author ref and registered authority
epoch at their exact old commits. If the author pushes or authority is revoked
before that transaction, none of the ref updates applies.
This no-op comparison is not an author-branch rewrite or persistent lock.
The provider must accept the conditional no-op under its actual rules; refusal
blocks promotion and never removes the head fence. Fork heads cannot participate
in the same repository transaction and are explicitly unsupported by this first
promotion protocol, not silently trusted from a prior REST read.
It does not use REST `UpdateRef(force:true)`, a read followed by a write, or the
PR merge API's author-head fence as a destination fence. Its stable mutation
claim binds protocol, repository/base, expected destination, tested commit/tree,
author head/repository/ref, authority epoch, subject, policy and generation through the existing
effects boundary.
`clientMutationId` binds acknowledgement; it is **not** provider idempotency.
The resolved repository identity must also match the enrolled name: a redirect
after a repository rename/transfer is not authority to write a different scope.

A durable boundary and non-nil live eligibility guard are mandatory. The
transport refuses a no-op boundary. The opt-in controller supplies exclusive
destination authority, exact complete trusted CI, review, trust, DCO, coverage,
soak, release and hold checks through the original full boot-policy evaluator.
Its read-only probes isolate tracker/preparation state and disable planner
writes. Provider branch rules remain authoritative and rejection does not
permit a fallback, force update or privilege expansion. Destination CAS does
not atomically snapshot GitHub's independently mutable PR/review/hold fields
other than the separately fenced same-repository source ref:
their current freshness contract and any remaining race must be explicit, not
misrepresented as a multi-resource transaction.

The explicit `ExecuteJournaled` boundary contract refuses OFF/missing
persistence, freezes the effective mode once, syncs the journal intent before
the external effect and syncs its result before acknowledgment. Ordinary
`Execute` retains its existing OFF passthrough behavior; method availability
alone never establishes that journaling is active. The new sync requirement is
limited to explicitly journal-required operations, not added to legacy writes.

An ambiguous mutation is reconciled, never called successful from a timeout.
The exact tested destination proves application; the unchanged expected
destination permits only the same fenced retry. A different/unreadable
destination remains an explicit unresolved condition. There is no blind
TTL-expiry retry and no proof transfer to a changed head. A destination that
advanced after an ambiguous acknowledgement needs further authoritative
lineage reconciliation; the transport does not guess that it was promoted.

### Execution feasibility gates

Before production execution may be enabled and qualified:

1. The source producer must be installed and verified on the trusted
   base with its exact reviewed workflow/action closure. Ordinary `v2-ci.yml`
   dispatch is not a candidate producer; ordinary `v2-tests.yml`
   manual runs are not candidate attestations and include scheduled/manual
   soft-failure behavior. Merely dispatching either does not supply combined
   validation. The current direct-path artifact consumer is single-PR/event
   bound and cannot be relabeled as a multi-member producer.
2. The actual installation must already have the permissions required by the
   chosen trigger. Hive normally has Actions read and Checks read. A direct
   workflow dispatch needs Actions write; it is not silently requested. A
   repository-dispatch control plane is a separate API/provenance design, not
   an interchangeable claim that an Actions-read App can dispatch workflows.
3. Candidate publication must not trigger unreviewed privileged `push`
   workflows. Untrusted candidate code must run with read-only credentials,
   no repository secrets, no privileged runner access and isolated disposable
   execution. A successful trusted finalizer, exact run/attempt/job binding
   and immutable artifact identity are necessary; an artifact name alone
   does not establish its producer.
4. Every actual required context must have a genuine candidate-aware producer.
   Author-head DCO/review receipts remain separate; copying their green
   statuses onto a combined commit is forbidden. Required-context/provider
   incompatibility blocks promotion rather than changing branch rules.
5. Native-config-selected repositories are reserved to the native authority
   unless a narrower **landed** selection contract proves otherwise. Native
   detection errors are unknown, not permission for a Hive writer. Existing
   default-branch preparation and its inline tracker rails are unchanged.
6. Accounting must cover the entire candidate attempt cohort. The current
   ledger size and bounded historical pagination limits remain real scale
   blockers; missing history is neither zero cost nor growth permission.

### Parallel architecture and evidence criteria

The execution target is parallel speculative prefixes from the outset:
`B -> S(A) -> S(A,B) -> S(A,B,C)`, with CI on each exact step allowed to
overlap. The one-step transport proves a protocol, **not** performance
completion. Passing later prefixes never authorizes a rejected predecessor.
This first transport also serializes destination advances: at two seconds per
advance, a 32-member ready prefix could add roughly 64 seconds for its last
member. A future single-CAS contiguous-prefix protocol needs its own verified
first-parent lineage, passing proof for every required step, and live guards
for every not-yet-promoted member; this adapter does not pretend to support it.
Removing A invalidates A-dependent subjects and rebuilds B against the actual
destination; an A+B receipt cannot validate B alone. Other destinations and
new legally formed independent subjects can proceed within their own budgets.
Disjoint paths are not a semantic independence proof.

The implementation sequence is trusted producer/permission/protection
feasibility; durable candidate identities and parallel bounded dispatch;
live-gated exact promotion and recovery; opt-in cached execution diagnostics;
and comparative benchmarks. Existing authoring admission and preparation are
reused, not replaced. This sequence is an engineering decomposition, not a
claim that the strategy is optimal or empirically qualified.

The evidence behind these choices is deliberately qualified:

| Decision | Source/result and limitation | Required comparison or regression |
| --- | --- | --- |
| Parallel prefixes and scoped predecessor rebuilds | [GitHub's documented native behavior](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue) and [Zuul dependent gating](https://zuul-ci.org/docs/zuul/latest/gating.html) test ordered speculative prefixes. This is a production design baseline, not Hive parity evidence. | A fails after A+B passes; rebuild B-only; preserve an exactly promoted prefix; prohibit old evidence resurrection. |
| No fixed batch-fill delay initially | [FSE 2023 parallel batch testing](https://arxiv.org/html/2308.13129), sections 2–6: historical simulation, nonlinear capacity effects, fixed batching can worsen latency when resources are plentiful. It assumes no merge conflicts and excludes compile time. | Idle arrivals, burst arrivals, full compile/setup time, text and semantic conflicts, and p50/p95 rather than averages alone. |
| Bounded adaptive window only after measured static operation | Zuul describes additive growth, failure shrink and per-destination windows. The paper's historical gains do not establish Hive's useful concurrency. | Sweep fixed windows first; measure wasted runner-minutes, failure recovery and marginal latency benefit; freeze growth on incomplete telemetry. |
| No test skipping, prediction or assumed content equivalence | The paper's batching/failure model is not a proof about a different combined Git subject. [Tide](https://docs.prow.k8s.io/docs/components/core/tide/) also requires current-base evidence and exposes missing requirements. | Changed base/member/order/policy, rerun attempts, cold restart, duplicate and reordered events, missing producers and authoritative live holds. |

Proposed orchestration budgets are **hypotheses requiring benchmark approval**:
durable event-to-controller action p95 at most 250 ms on an idle process;
provider dispatch acceptance p95 at most 2 s excluding runner queueing; final
CI evidence received to promotion request p95 at most 2 s once every live gate
is satisfied; and idle eligible-to-dispatch p95 at most 3 s. Measure monotonic
internal spans and provider request spans separately; do not subtract slow
API calls, delayed event delivery, reviewer wait, runner wait or serial holds
from reported end-to-end latency.

Compare identical workloads, CI definitions, runner type/count, cache state,
retry policy and failure distributions against GitHub's documented parallel
baseline. Report eligible-to-CI-start, complete-CI-to-promotion and end-to-end
p50/p95, integrations/minute, actual runner-minutes/successful integration,
invalidated/canceled execution and recovery. No safety-network job is canceled
to make the comparison look better. Native merge-size limits do not batch
native CI builds. A simulator and correctness CI are not a production benchmark.

The stated 5,000/day target is about 3.47 integrations/minute **aggregate**,
not an achieved result or a single-repository promise. Little's law requires
about 35 in-flight candidates if mean CI is ten minutes at that rate, before
waste and headroom. Forty runner-minutes per successful integration at 70%
utilization needs about 199 simultaneous runner slots before speculative waste.
A non-overlapping 30-second per-destination hold caps that destination at two
integrations/minute regardless of controller speed. Reviewer availability,
API budgets, storage, policy holds and runner capacity must appear in the
capacity report rather than being hidden behind a fast dispatch measurement.

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

These call seams and the producer verifier are used by the separately opted-in
controller below. Their source availability is not runtime activation,
protection migration or execution-readiness proof. Existing authoring enforce
mode does not activate execution. Every actual required destination context
still needs a genuine compatible candidate-aware producer; author-head green
statuses must not be copied.

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
promotion authorization. Verified historical signatures and pinned key material
are archived privately for durable replay; an actual isolated provider execution
remains an operator qualification requirement.

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
candidate producer constructs exact speculative prefixes; only the separately
enrolled and qualified controller below can promote them.

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

Source contract tests are not isolated provider execution proof. Read-only
candidate and materializer observers now bind authoritative run/attempt/job/check
metadata, immutable artifact digest, exact signed bytes and enrolled ordered
commit/tree/workflow closure. Their source tests include changed attempts,
partial publication and mismatched producers/subjects; they do not prove an
installed provider execution. Actual job observations include materialization
and finalizer overhead, not just local recorder time. Durable verification/key
history and controller wiring are implemented in source; deployment, shared
authority enrollment/handover and real qualification remain operator steps.
Existing protected-branch contexts do not automatically become satisfied by
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
## Opt-in candidate execution controller

The separate `integration_orchestration.execution` configuration defaults to
**off**. Source delivery is not deployment, published-image verification,
runtime enablement, protected-provider qualification, or a throughput receipt.
The existing authoring layer and preparation/native queue authorities remain
separate. No existing convergence flag enables this controller.

The controller journals exact ordered recipes and their canonical work/DAG
generations before preparation, observes the installed materializer and
candidate workflows, archives verified producer signatures privately, and
accounts actual assigned-runner jobs across run attempts. It discovers bounded
workflow cohorts; absent, duplicated, truncated, stale, or unowned observations
remain unknown. Duplicate deliveries and historical attempts consume accounting
budget and are never treated as free. Shared physical jobs are deduplicated.
Canceled execution counts; queued/skipped jobs are not execution. A restart
retains job facts and signed evidence but rebuilds accounting completeness.

Verified terminal receipts are reused only while actual discovery confirms the
same completed run/attempt. Oldest-observed pending prefixes rotate through a
bounded read budget without changing stored prefix order. A partially published
recipe cannot become a wholly passed recipe: valid prefix receipts survive
independently, and the unpublished tail remains rejected/unresolved.

## Authority and promotion limits

Hive authority supports an explicitly designated installation executor and
observer-only enrolled peers. Its create-only private installation identity
does not register or grant permissions. An immutable forge authority record,
exact epoch, actual GitHub App installation, and identical reviewed producer
profile are prerequisites. Copied identities/credentials, automatic takeover,
TTL stealing, and independent legacy writers are not supported. A local
filesystem lock is not cross-hive fencing.

Preparation dispatch is durably journaled before the RPC. Confirmed non-applied
refusals alone permit bounded retry after cooldown; unknown outcomes do not.
Repository-dispatch/bootstrap overhead is **not exactly once**. This protocol
does not authorize legacy Actions reruns or solve crash ambiguity for an RPC
that cannot consume a fence. CI-reporting observations remain detection and
owner-action-required, not recovery permission.

Promotion uses only the exact tested prefix commit/tree, the exact current
predecessor, and documented actually merged earlier prefixes. Fresh promotion
rechecks unique candidate run/claim/latest attempt, canonical work/DAG/scope,
the complete original merge lane, current heads/base, reviews/holds and provider
policy. Its atomic mutation fences destination, author branch and authority
epoch together. No copied green status, squash/rebase substitute, native
fallback, or unsupported protection bypass is permitted.

The initial supported provider profile is deliberately conservative. Resolved
ruleset/classic protection and bypass metadata must be readable and supported;
ambiguous permission/404 responses, native merge-queue rules on a Hive
destination, or unsupported bypass/merge policy refuse qualification. A real
one-member isolated protected probe must demonstrate the exact actor, technical
context compatibility, no-op reference-fence behavior, actual CAS and exact PR
merge acknowledgement. A configured qualification ID is not a receipt.
An ambiguous crash/reconciled destination without a durable successful provider
RPC acknowledgement cannot qualify the actor; it requires a new isolated probe.
Multi-member empirical scale remains a separate operator-approved experiment.

Native authority uses only ordinary Hive enqueue admission and existing cached
native observations. GitHub owns native promotion, group composition and build
windows. Hive cannot jump the queue or control unsupported GitHub concurrency.

## Configuration and safe rollback

Settings → Features exposes an independent execution mode and a bounded profile
JSON editor in the existing integration subsection. Explicit repository/base
allowlists, actual actor/App/workflow IDs, policy/workflow hashes, executor/epoch
and qualification pins, and validated member/job/minute/attempt/time bounds are
required. Durations in that JSON editor are nanoseconds. Configuration updates
compare `expected_execution` against the loaded profile; stale updates fail
instead of silently replacing another owner's enrollment.

**Advisory** may observe but cannot block, dispatch or promote. **Enforce** may
use only authoritative permitted constraints and requires the parent authoring
observer enrolled for every identical repository/base. Static finite budgets
come first; missing telemetry freezes growth, not safety-network execution.
This implementation does not automatically change live concurrency or enable
an adaptive pilot.

The initial controller is deliberately bounded, not throughput-qualified: its
fallback turn is three minutes, event nudges coalesce to at most one per minute,
and each turn visits at most two enrolled scopes and promotes at most one
exact step per visited Hive scope. These source limits are real performance
constraints, not measured 5,000/day capacity. Eligibility probes rotate bounded
windows past older unready PRs; repository/base turns rotate independently.
Declared expected jobs/minutes are conservative admission reservations to be
reviewed against the actual qualified producer fanout and measured cohorts,
not provider-enforced execution quotas or permission to guess runner capacity.
Already admitted work may exceed an estimate; actual observed spend remains
charged and freezes further admission at the configured bounds.

Before exclusive activation, operators must deploy a verified image, stop or
upgrade/enroll every participating legacy writer, register one executor,
install and verify the narrowly scoped App/workflow/producer policy, run the
isolated protected qualification, and independently inspect actual source,
image, runtime mode, effective writer, cohort completeness and fresh gates.
No such live action follows automatically from merging source.

Before releasing an enforced Hive scope to off/advisory or another authority,
move its shared forge authority ref away from the old epoch and verify that
revocation. The configuration endpoint requires a live revocation observation,
cancels its bounded controller, and releases only this layer's scoped admission
reservations. Unknown effects, signed evidence and mutation audit remain.
An outage is not proof of revocation, and revocation does not deduplicate an
already ambiguous preparation RPC. Operator handover must account for that
explicit limitation before allowing another executor.

The activity monitor adds bounded cached, archive-free captions to matching
actual root/nested PR rows only. Exact repository, PR, full author head and base
must match; a proof for A+B does not become A's proof. Captions distinguish stale
unknown observations and technical success from live merge permission. Existing
stages, sorting/filtering/tree, inline preparation rails and native queue region
remain unchanged; there is no second gold/reference panel or new poller.

There is **no claim of 5,000/day or a live candidate benchmark** from source CI,
fixture timings, the pre-existing merge stream, or a disabled controller.
Production measurement must separately report actual candidate/cohort identities,
runner execution cost including cancellation/bootstrap, author-review versus
integration receipts, outcomes/latency, and completeness over fixed windows.
Published image and current runtime provenance remain unknown until measured.
