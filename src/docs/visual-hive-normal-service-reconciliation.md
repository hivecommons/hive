# Visual Hive normal-service reconciliation

Status: live private disposable-repository vertical complete, 2026-07-19, on
exact Hive `45ff9ab54723baee41410875eec1fa7dbb64911d` and Visual Hive
`3015c9e7cc7b357bbd4f5551b115fb7b7f4847ec`. A healthy ordinary cadence,
prepared one-line defect, one Governor-admitted Worker repair PR, sealed
exact-head verdict, and duplicate-free ordinary Hive restart/replay all passed.
The fully gated code candidate is
`a220ca78da60027d51456365187faaafe5239928`; the contract reconciliation after
that checkpoint is documentation/scope cleanup only. The repair PR remains open
and unmerged. KubeStellar Console fork P0 and any release or upstream promotion
remain separate later steps.

The normative product contract is
`docs/visual-hive-integration-contract.md`. This document records the actual
implementation and proof boundary; it does not grant additional authority.

## Safety and source boundary

Implementation and tests remain on the exact authorized local Hive integration
worktree for branch `dd`. Source integration authority is limited to that branch; it
does not extend to Hive `main` or any other Hive branch.

The live proof changed only the private disposable repository
`DavidDiaz0317/visual-hive-ai-hpc-proof-20260719-v10`. No Hive upstream branch,
Hive `main`, KubeStellar Console checkout/remote/workflow/issue/PR, or production
Hive state was changed. The immutable Visual Hive PR producer is
`3015c9e7cc7b357bbd4f5551b115fb7b7f4847ec` (tree
`678c4632833786bf419df8e48a8c92ef51a65807`) in the separate
`vis-proof-harness` worktree. It is a producer input, not a Hive commit, and
must not be cherry-picked into Hive.

## Product topology

There is no Visual-specific manager, role registry, queue, dashboard, wiki, or
graph. The normal service is a serial reconciler around existing Hive owners:

```text
existing production workflow intent and verified bundle transport
    -> native Visual Hive intake Controller
    -> existing Governor admission and normal routed bead store
    -> existing Scheduler (role policy + project + knowledge)
    -> existing SpecialistMailbox: one content-derived swo-*
    -> existing ordinary agent.Manager facade
    -> isolated one-shot Codex proposal child
    -> existing repair.Worker
    -> one branch/commit/pull request, never merge
    -> exact-head Visual Hive verifier with check-evidence-only capability
    -> receipt-only controller completion, never resolution
```

Visual Hive owns deterministic findings and verdict evidence. Hive owns
admission, policy, work state, proposal dispatch, patch validation, Git,
GitHub, lifecycle, and audit. The one-shot proposal child owns none of those
capabilities.

## Implemented path

### Service ownership and transport

- The normal daemon claims the existing OS-backed `daemon.lease` before it
  configures the ordinary Manager for Visual Hive. The legacy daemon and
  one-shot `hive run` claim the same ownership before constructing their
  specialist runtime, so two Managers cannot be configured for the same
  repository state.
- `integrated.AcquireNormalVisualWorkLease` claims the existing production-run
  lock only for an active, unpaused service epoch. Pause or incompatible config
  change cancels and unwinds the cycle, releases that production lease, and
  remains lease-free until normal operation resumes. Daemon ownership remains
  held so a second Manager still cannot appear during the pause.
- Production-lease contention is an idle retry in a dedicated goroutine. The
  normal Governor loop, persistent agents, and dashboard remain independent.
- `integrated.FetchNormalVisualWork` reuses installed setup verification,
  exact correlated workflow dispatch/resume, job/run/artifact verification,
  full v3 source fetch, producer pin, and live installed-workflow head check.
  It calls no legacy lifecycle, manager, repair, baseline, or merge path.
- `integrated.ConsumeNormalVisualWork` deletes only the exact durable workflow
  intent. An already-absent intent is accepted only after the service has
  durably recorded that consumption began.

### Intake, Governor, and Scheduler

- `visualhive/controller.Controller.Import` is the only bundle-to-Hive intake.
  It routes to existing normal role stores and calls the existing Governor.
- `BuildSchedulerAdmittedWork` is the only intake-envelope projection into
  `scheduler.AdmittedWork`. It preserves canonical work, packet, finding,
  evidence receipts, exact base/tree, recurrence, role, paths, contracts,
  validation, and deadline. It independently rejects valid JSON receipt bytes
  that differ from the exact receipt carried by the evidence identity.
- The existing Scheduler composes the governed request with the current normal
  role snapshot, project context, knowledge primer, capability/policy digests,
  and a separately injected contained-Codex executor profile.
- Worker owns the source composer. It reads the admitted exact Git tree through
  Git object IDs and reads only receipt-bound bounded JSON below the verified
  evidence root. The child receives bytes, not a checkout or evidence path.
- `PrepareGoverned` creates one canonical `swo-<request-sha256>`. Intake
  durably reserves that exact ID and digest before a fresh model launch.

### Existing Manager and Worker

- `configureNormalVisualWorkRunner` installs the contained child dispatcher on
  the already existing ordinary `agent.Manager`; it does not construct another
  Manager.
- The proposal executor is one-shot Codex in a private contained directory.
  Normal role backend, launch command, tools, connections, and persistent pane
  remain unchanged and inert with respect to the child.
- `repair.Worker` remains the sole component allowed to validate/apply the
  diff, re-run installed argv commands, commit, push, and create/reuse a pull
  request.
- The Worker policy is forced to `repair-pr` even when the installed repository
  is configured for `auto-merge`. This vertical cannot merge.
- The normal service now installs one narrow verdict adapter over the existing
  GitHub client and lifecycle store. It creates no poller, Manager, role,
  queue, dashboard, wiki, graph, or repository writer.
- The adapter independently supplies only the installed repository/ID,
  selected PR, exact base/head refs and SHAs, fixed workflow identity, GitHub
  Actions App ID, producer pin, protected destination, and current effective
  ACMM. Artifact-internal identities are derived by the verifier, not accepted
  from the service.

## Revalidation boundaries

| Boundary | Current state rechecked |
| --- | --- |
| Fetch | Installed repository identity, setup, pause request, workflow/ref/producer |
| Intake/admission | Governor mode, role enablement/config, budget, WIP, pause, installed policy |
| Scheduler preparation | Exact intake envelope, Worker base/tree, contained executor readiness |
| `swo-*` reservation | Current Governor/role/budget/WIP/pause/policy and immutable envelope |
| Fresh child launch | Same current controller checks immediately before dispatch |
| Worker side effects | Dynamic installed policy plus controller guard before apply, validation, commit, push, and PR |
| Exact-head verdict | Exact order, request, PR, head, and current controller policy immediately before verifier |
| Receipt completion | Exact open Worker PR and exact-head receipt; no merge/baseline/resolution authority |

Already leased work is not redispatched when a later fresh-launch guard denies.
Recovery observes only the exact persisted order, lease, Manager completion
spool, receipt, and proposal. This prevents a pause or policy change from
turning an ambiguous model call into a second call while still denying new side
effects.

The contained provider executable/configuration is fixed for the process
lifetime because the existing Manager dispatcher cannot be safely hot-swapped.
A change requires a controlled Hive restart. Normal role, Governor, ACMM, and
installed repository policy continue to reload through their existing owners.

## Crash and replay model

The service ledger is
`<state-dir>/visual-hive/normal-service/active.json`. It is a small exact-binding
checkpoint, not a queue. It records the workflow, packet digest, source ref,
the exact selected source ref, every unselected launchable source ref and its
controller-owned deferral checkpoint, one work-order/request identity, Worker
PR, verdict receipt, completion, and intent-consumption checkpoints using
durable atomic replacement. Canonical
verdict JSON is stored as opaque encoded bytes, never re-indented, and its
SHA-256 is rechecked after every disk load. The complete ledger state machine
is validated before fetch, import, Worker, verifier, completion, or consume.

Ordering is:

1. bind exact workflow and packet;
2. import once and persist the controller-owned source ref;
3. prepare/reserve one `swo-*` and let Worker create/recover one PR;
4. verify the exact PR and atomically apply its sealed check evidence;
5. persist the exact-head verdict identity bytes and digest;
6. atomically close the routed bead with the exact completion receipt;
7. persist `consume_started`;
8. consume the exact workflow intent;
9. persist `consumed` and clear on the next cadence.

After the source ref is persisted, restart reopens and revalidates the
controller-owned dispatch. It does not refetch or reimport the artifact.
Worker response loss may re-enter `Worker.Run`, but Worker/mailbox state returns
the same durable order/proposal/PR; it cannot create a second model side effect.
Controller completion is exact-byte idempotent, including the crash window
after bead close but before the service saved `completion_recorded`.

## Exact-head verdict: composed, locally proven, and live-proven

The production normal-service option is no longer `nil`. Its adapter uses the
reviewed `FetchAndVerifyVisualHivePullRequestBundle` primitive and can apply
only the opaque `verified.ApplyCheckEvidence(store, fingerprint)` capability.
The verifier and adapter now enforce all of the following:

1. The PR evidence producer is pinned to
   `3015c9e7cc7b357bbd4f5551b115fb7b7f4847ec`. The target and evidence services
   use distinct UIDs; the evidence root is root-owned mode `0700`; and the
   authenticated root binding uses a root-only random key.
2. Both service identities are killed and quiesced before verification. The
   verifier rejects unexpected ownership, modes, links, devices, file types,
   counts, byte totals, paths, digests, source binding, and authentication.
3. Hive supplies only independently knowable repository, PR, exact head/base,
   workflow, GitHub Actions App, and pinned producer facts. The verifier
   discovers the unique successful run attempt, jobs, checks, and artifact IDs
   through authenticated GitHub metadata.
4. Workflow/plan/report/config/changed/contracts/scopes/runtime/execution/
   baseline identities are derived inside the verifier and cross-bound to the
   complete content-addressed index, exact Git blobs, source binding, and
   authenticated root receipt.
5. The service persists the selected finding fingerprint and exact base SHA in
   ledger schema v4. It revalidates current policy and the exact Worker PR,
   applies sealed evidence, stores the canonical identity bytes, and passes the
   same receipt digest to controller completion.
6. Controller completion rejects caller-asserted verdicts while the lifecycle
   is merely PR-open. It requires the sealed evidence application to have
   moved the exact finding to `ready`, and cross-binds repository IDs, base,
   head, workflow, producer, conclusion, and check-evidence-only authority.

The current verifier intentionally accepts only a unique successful workflow
run carrying a deterministic `ready` bundle. A failing/red run therefore
leaves the Worker PR open and the source intent unconsumed; it cannot be
mistaken for permission to complete, merge, resolve, or update a baseline.
Capturing red telemetry without granting completion authority is later work,
not a prerequisite for the first working repair proof.

The privileged hostile-producer isolation test passed in an ephemeral Linux
environment with distinct service identities and the root-owned evidence seal.
The private v10 proof then exercised the same composition with real GitHub
workflow metadata, content-addressed artifacts, and the contained Codex
executor while merge authority remained disabled. Its exact runs, repair PR,
and duplicate-free restart are recorded below.

## Local/no-GitHub proof currently passing

- daemon ownership is claimed before ordinary-Manager configuration and blocks
  both the legacy daemon and one-shot legacy Manager factory;
- pause/config quiescence releases the production lease only after an active
  cycle unwinds, then reacquires it on resume without duplicating work;
- lifetime ownership contention performs no fetch/import/Worker work;
- a no-dispatch pause/WIP/green path runs no proposal or PR;
- crash after one Worker PR side effect recovers through the same controller
  dispatch without another fetch/import or side effect;
- crash after verdict persistence performs no second fetch, import, proposal,
  PR, or verdict;
- ambiguous workflow-intent consumption produces one deletion side effect,
  including the no-dispatch path without starting another workflow;
- a missing verifier still fails closed in the generic service fixture, while
  the production normal-service composition installs the exact verifier;
- drift in repository, fingerprint, PR, base/head, workflow, App, producer,
  authority, conclusion, or receipt digest is rejected before lifecycle apply;
- the exact opaque receipt moves the finding to `ready`; controller completion
  before that application is rejected and exact replay after it is idempotent;
- identical controller completion replay succeeds and an altered receipt fails;
- malformed workflow/order/PR/verdict/consume ledger transitions fail before
  any source, intake, Worker, or verifier call;
- Scheduler composition produces and reserves one canonical `swo-*`;
- fresh-launch pause denial occurs before model dispatch;
- exact leased recovery performs no recomposition, second reservation,
  readiness restart, guard rerun, or redispatch;
- source composition uses a real temporary Git repository and returns bytes
  from the sealed tree even after the checkout changes.
- the genuine vertical test fetches untrusted bundle ZIP/API bytes through the
  production source verifier, imports through the real Controller and
  Governor, composes policy/project/knowledge through the real Scheduler,
  dispatches through the existing ordinary Manager and specialist provider,
  and lets the real Worker create exactly one branch and pull request in a
  local bare Git remote;
- the same vertical applies a production-verified PR-v3 check-only receipt at
  the exact Worker head, injects a lost consume response, restarts without a
  second fetch/admission/model/PR/verdict effect, blocks the next packet while
  the exact PR is open, and releases only after read-only observation of its
  closure;
- no persistent parent agent, PR edit, merge, baseline write, or lifecycle
  resolution occurs in that proof.

The local GitHub API, source dispatch/consume, issue writer, contained model
response, and wiki search endpoint remain bounded test substitutes. Those
fixtures do not by themselves establish live GitHub scheduling or real Codex
behavior; the private v10 proof below supplies that operational evidence. The
local fixtures do not claim race-detector coverage, and CGO remains disabled in
the Windows host environment.

Existing controller tests additionally cover live pause, automation downgrade,
role disable/re-enable, nested role-capability drift, Governor mode/cadence
drift, ACMM drift, installed path-policy drift, manual review, expiry, WIP,
budget, runtime-config reload, immutable-envelope tamper, and terminal WIP
retirement.

## Current one-dispatch scope boundary

The runnable vertical selects the first launchable controller dispatch (ordered
by source ref), durably marks every other launchable peer as deferred on its
existing routed role bead with the exact packet, selected source ref, workflow
correlation, and reason, then creates or recovers one specialist work order and
one Worker PR. Only after that PR's exact-head verdict is recorded does it
consume the source workflow intent. Import still admits and represents every
finding through existing lifecycle and bead owners; packet consumption does
not delete deferred beads. Every imported bead also stores the explicit
`unavailable_no_verified_facts` keyword state instead of fabricating or writing
knowledge; a later selected specialist still reads the existing Scheduler
primer. An exact same-packet replay keeps those peers deferred, while a later
verified packet may re-admit them through the Governor. The implementation
therefore preserves a multi-finding packet without claiming multi-PR processing.

That limit is acceptable for the one-defect disposable-repository proof and the
one-defect Console-fork P0 scenario. General packet fan-out needs a later
controller-owned selection/completion contract and a ledger that binds every
selected dispatch without becoming another queue or Manager. It is not safe to
add an ad hoc service loop that consumes the packet after only some work or that
bypasses the existing Governor, Scheduler, Manager, mailbox, or Worker owners.

Focused commands passing at this checkpoint:

```text
go test ./cmd/hive -run '^TestNormalVisualWorkVerticalAcceptance$' -count=3 -timeout 210s
go test ./cmd/hive -count=1 -timeout 210s
go test ./pkg/visualhive -count=1 -timeout 180s
go test ./pkg/visualhive/controller -count=1 -timeout 180s
go test ./pkg/visualhive/normalservice -count=1
go test ./pkg/visualhive/controller -run '^TestVisualWorkControllerAdmitsBeforeIssueAndLeavesSchedulerDispatchPending$' -count=1
go test ./cmd/hive -run '^TestNormalVisualPullRequestVerifier' -count=1
go test ./pkg/github -run 'VisualHivePullRequest|PullRequestBundle' -count=1
go test ./pkg/visualhive -run 'PullRequest|BuildImportPlan' -count=1
go test ./pkg/internal/visualhivepr -count=1
go test ./pkg/integrated -run 'WorkflowIsolation|VisualHive' -count=1
go test ./pkg/repair -run '^TestGovernedSourceComposerBindsSchedulerToWorkerSealedTree$' -count=1
go test ./pkg/repair -run '^(TestSpecialistProviderProposalIsBrokeredAndCompletedReplayDoesNotRedispatch|TestGovernedSchedulerCompositionReservesOnceAndLeasedRecoveryDoesNotRecompose)$' -count=1
go test -run '^$' ./cmd/hive ./pkg/repair ./pkg/visualhive/controller ./pkg/visualhive/normalservice ./pkg/integrated
```

The vertical acceptance passed three consecutive times. Exact Hive
`45ff9ab54723baee41410875eec1fa7dbb64911d` passed the pre-handoff Linux gate.
The final code candidate `a220ca78da60027d51456365187faaafe5239928`
subsequently passed every Go package in a clean, read-only, privileged Linux
clone using a serialized main shard and one fresh isolated tmux-resume shard;
full `go vet ./...` also passed. The split kept an installed-image ACMM fallback
fixture out of source-unit discovery and prevented leaked asynchronous tmux
activity from one concurrency test from interfering with the immediately
following resume test. Both affected paths passed in their intended isolated
environments; no product failure was hidden.

## Completed live disposable-repository proof

The private v10 proof exercised the product through its ordinary user path:

1. Normal Hive/dashboard became healthy and completed cadence run
   [`29689343229`](https://github.com/DavidDiaz0317/visual-hive-ai-hpc-proof-20260719-v10/actions/runs/29689343229)
   at exact installed setup head `db3190b6e558e5e4495860c96aefd320adcb7bc4`.
2. The prepared one-line defect was committed as
   `25c7deaa254d7d53bc1812663c6cc8f7d2788f26`; its exact-head production run
   [`29689756141`](https://github.com/DavidDiaz0317/visual-hive-ai-hpc-proof-20260719-v10/actions/runs/29689756141)
   produced the deterministic finding and one Hive issue.
3. Existing Governor/Scheduler/Manager/mailbox/Worker ownership created exactly
   one unmerged repair PR,
   [`#3`](https://github.com/DavidDiaz0317/visual-hive-ai-hpc-proof-20260719-v10/pull/3),
   on branch `hive/repair-32ccc1d99d33-a1` at exact head
   `d3c8c51287697db5beb27ce95518d1f4fb831d44`.
4. Exact-head Visual Hive PR run
   [`29689952857`](https://github.com/DavidDiaz0317/visual-hive-ai-hpc-proof-20260719-v10/actions/runs/29689952857)
   completed successfully with the audited producer. The separate
   `pull_request_target` event remained skipped and executed no PR code.
5. One ordinary Hive service restart/replay reused the same durable issue, work
   order, model attempt, branch, PR, and exact head. It created no duplicate
   bead, work order, model call, branch, or PR, and the dashboard/cadence owner
   remained the existing ordinary Hive process.

The next product test is the contract's KubeStellar Console **fork-only** P0:
use a dedicated namespaced ordinary Hive built from exact `dd`, the Console
fork as both PR base and head repository, separate state/data/dashboard
identity, and `repair-pr` authority. Preserve Console Auto-QA, test generation,
visual regression, trust workflows, existing checks, and its production Hive.
Leave every demo PR unmerged. Upstream Console and Hive `main` remain read-only.

Release packaging, new roles, new dashboards, baseline automation, broad tool
creation, direct Visual Hive writes, and Console `kc-agent`/MCP integration are
not on this critical path.

## Upstream review map

- `d93992e0` through `728ce71b` (inclusive): integrated setup, delivery, and hosted-control foundation.
- `844cb9e4` through `e372db54` (inclusive): normative contract, normal-service integration, exact-head verification, and replay.
- `b4f8df98` through `ecaef46d` (inclusive): vertical-proof consolidation.
- `bd0c6700` through `a220ca78` (inclusive, first-parent review): exact-`dd` integration, observed production fixes, and final runtime-ownership guidance.

At the fully gated `a220ca78` code checkpoint, the integration merge carries the
development lineage, so the first-parent handoff is 23 commits even though the
complete ancestry contains 185 commits. The later reconciliation is
documentation/scope cleanup only; no history rewrite is required for review.

## Checkpoint ledger

| Commit | Result |
| --- | --- |
| `15a4560b` | atomic runtime-config compatibility base |
| `39527e5c` | native Visual Hive intake foundation |
| `f34a9085` | governed contained Codex dispatcher hardening |
| `65bd48f6` | normal-service fetch/intake/Scheduler/Manager/Worker vertical |
| `c8058fbe` | service replay/lease/idle fake proof |
| `a89e22d2` | exact completion replay and sealed-tree binding |
| `0a5f95ab` | one reservation/fresh guard/leased recovery proof |
| `102c4d94` | controller-owned resume without refetch/reimport |
| `1d15ab47` | crash-safe no-dispatch workflow consumption |
| `630d5fad` | exact verdict-byte persistence and ledger state validation |
| `55e704ff` | lossless Scheduler projection and exact receipt cross-binding |
| `05e7c51a` | bounded normal service to one dispatch/Worker PR |
| `7bf4bc84` | controller-owned deterministic deferral of every unselected finding |
| `7c5e9249` | exact PR evidence verifier and sealed check-evidence capability |
| `dd3cff1e` | production adapter, ledger v4 exact identity, and sealed-only completion |
| `170a905a` | sealed verdict-path reconciliation record |
| `3e090b8f` | audited immutable Visual Hive producer pin |
| `c9f20c28` | privileged Linux cross-principal evidence-seal proof |
| `aeadfe78` | daemon-wide ownership before ordinary-Manager Visual configuration |
| `100d31ae` | pause-safe active-epoch lease and exact one-open-PR gate |
| `e6ec0e77` | one-shot legacy Manager exclusion under the same daemon ownership |
| `abfdca0b` | exact merged-successor recovery for a consumed normal-service ledger |
| `13331d55` | sealed import clone fidelity and import-only bead boundary fix |
| `e372db54` | genuine local Governor-to-Worker-to-verdict vertical acceptance proof |
| `bd0c6700` | integrated governed Visual Hive runtime on exact `dd` lineage |
| `45ff9ab5` | production and audited PR pins verified separately; v10 exact-head producer |
| `a220ca78` | fully gated normal-Hive runtime ownership guidance and operator handoff |

These commits are checkpoints on exact `dd`, not a release, merge, or claim
that Hive `main` or upstream Console changed.
