# Visual Hive -> Hive Integration Contract

Status: **normative** | Effective: 2026-07-16 | Reconciled: 2026-07-19 | Owners: Hive and Visual Hive maintainers
Scope: the working Visual Hive -> existing Hive product

This document is the single architectural and acceptance source of truth for the integration.
Requirements use **MUST**, **MUST NOT**, **SHOULD**, and **MAY** in their ordinary
normative meanings. When another goal, roadmap, prompt, demo, or implementation
disagrees, this document wins until it is deliberately amended.

## Audit basis and truth labels

The original architecture audit used these historical bases:

- Hive `728ce71b01ad86d0187371bb16102ef6a3058063`.
- Visual Hive `7ce03fab983bd5f966da7edcde49f694ca3bd058`, plus the then-uncommitted
  salvage candidate based on it.
- Live upstream KubeStellar Console `8214151d6ab9b9973d79e159eb8054d8ccabf156`.

The implemented and demonstrated handoff basis is now:

- live-proof Hive `45ff9ab54723baee41410875eec1fa7dbb64911d`;
- fully gated code candidate Hive `a220ca78da60027d51456365187faaafe5239928`;
- immutable Visual Hive producer `3015c9e7cc7b357bbd4f5551b115fb7b7f4847ec`;
- the same read-only Console audit basis above; no upstream Console state changed.

`Implemented` below describes audited candidate behavior. `Target` now means the
remaining Console-fork coexistence proof and later release decision, not a second
runtime implementation. Exact live runs and replay evidence are recorded in
`visual-hive-normal-service-reconciliation.md`.

## North star

The north star is a working product proven on a real repository. Visual Hive
evidence MUST enter normal Hive controls/specialists and return for an exact
deterministic verdict without interrupting ordinary Hive work.

A disposable private real-code repo MAY provide initial no-merge safety proof, but
cannot satisfy P0 or unlock release work; only the later Console fork/PR can. Other
tests are supporting evidence. Until P0 passes, packaging/polish is not completion.

### Fork-only repository boundary

The repository owner has lifted the Hive source-work boundary only for the exact
`kubestellar/hive:dd` branch. That authority does not extend to Hive `main`, any
other Hive branch, upstream release tags/assets/attestations, or merges. The
upstream KubeStellar Console repository, its checkouts, remotes, issues, PRs, and
workflows, and its production Hive state remain read-only. Console P0 means an
actual PR whose base and head are both inside the Console fork, operated by a
dedicated namespaced normal Hive built from the reviewed Hive candidate. It does
not authorize an upstream Console PR, an upstream workflow mutation, or use of
the production KubeStellar Hive.

## Exact ownership split

Visual Hive owns only the deterministic testing boundary:

- repository-aware test selection and Playwright execution;
- selector, flow, screenshot, mutation, and other deterministic evidence;
- sanitized, content-addressed evidence production; and
- the final Visual Hive verdict for the exact tested source.

Hive owns the product and orchestration boundary:

- setup, repository binding, credentials, runtime health, packet verification, and admission;
- governor cadence/admission/budget/WIP decisions;
- classifier/scheduler/supervisor/policy routing and composition, plus ordinary
  `agent.Manager` task execution through existing roles;
- tool/MCP authority enforcement, knowledge/wiki/graph, and priming;
- beads and all durable work/lifecycle state;
- issues, branches, proposals, PRs, checks, retries, closure, merge policy, and merge;
- the existing dashboard, audit trail, and operator controls.

Visual Hive MUST NOT become a second governor, scheduler, specialist manager, bead
store, knowledge system, GitHub writer, dashboard, or merge engine. Only an exact
Visual Hive rerun, never an agent/provider/UI opinion, supplies its verdict.

## Canonical target flow

One target service seam (controller name not mandated) MUST inject the existing
Governor, Scheduler, ordinary Agent Manager, role bead stores, knowledge,
GitHub/lifecycle controller, and dashboard-visible state. Its only flow is:

1. A real repository at an exact commit is the subject.
2. Visual Hive runs the approved plan and emits one verified, content-addressed Integration Packet.
3. Hive verifies a receipt and resolves one stable role bead/external reference.
4. The real `Governor.AdmitWork` records its cadence/budget/WIP allow/deny reason.
5. A Scheduler builder composes role policy/project/knowledge into existing `agent.SpecialistWorkOrder` before `SpecialistMailbox.Prepare`; no fake `github.Issue` or classifier reroute.
6. Ordinary Manager implements existing `repair.SpecialistDispatcher` and dispatches an isolated child session; Mailbox owns the durable lease, receipt, and proposal.
7. Existing repair `Worker` alone owns diff/path validation, apply, branch, and PR.
8. Visual Hive reruns the same applicable plan against the exact PR head.
9. Verified output updates that same bead, lifecycle, dashboard state, and audit.

No shortcut may jump to execution or lifecycle. No `visualhive`/`vhw` work-order
queue, `ProposalTaskRequest` store, new lease/receipt, or parallel repair state may
become another source of truth.

## Implemented runtime and compatibility boundary

Implemented:

- `v2/cmd/hive/main.go` builds normal governor, scheduler, `agent.Manager`, policy,
  knowledge/wiki, beads, GitHub proxy, and dashboard.
- The ordinary service claims the normal runtime-owner lease before Visual Hive
  configuration, verifies one immutable packet through the integrated adapter,
  and admits it through the existing Governor and Scheduler.
- The existing Manager/mailbox and role policies invoke the existing repair Worker;
  Hive alone owns the issue, branch, PR, lifecycle, audit, and dashboard state.
- The exact-head Visual Hive receipt returns through that same controller-owned
  lifecycle, and restart/replay reuses the same durable work without another side effect.

Compatibility boundary:

- `v2/cmd/hive/specialist_runtime.go` and the integrated controller retain a
  separate specialist runtime only for compatibility paths. They are not the
  owner for local Visual Hive `repair-pr` or `auto-merge` operation.
- The remaining Console-fork target MUST reuse the implemented normal-service
  path, existing roles/policies/knowledge/beads/lifecycle, and existing dashboard.

The second-manager runtime is transitional compatibility, not precedent. New work
MUST move toward the target and MUST NOT deepen the parallel path.

## One Integration Packet

The only admitted handoff is `visual-hive.bundle.v3`: `manifest.json` and its
declared `files/` payload form one indivisible packet. Hive MUST reject partial,
mixed-run, mutable, or log-scraped handoffs.

The packet MUST bind at least:

- schema and digest algorithm;
- packet ID, generation/expiry, nonce/replay key, producer/version/immutable commit;
- immutable repository ID/name, ref/commit, event, workflow/run/attempt/artifact,
  conclusion, and independently verified provenance;
- project, mode, verdict, requested ACMM ceiling, scan scope, evaluated contracts/files,
  plan version, and whether absence is authoritative for resolution;
- observations with stable fingerprints, dependencies, severity, source artifacts,
  affected contracts, and validation command;
- each payload path/type/schema/length/SHA-256, the complete content-addressed index,
  capability-parity receipt, canonical aggregate digest, and safety claims.

The packet is data, never asserted authority. Producer counters/trust claims stay
advisory until Hive verifies them. Unsafe paths, secrets, undeclared/duplicate files,
digest/size mismatch, expiry, incomplete provenance, or parity drift fail before state.

Visual Hive Evidence/Handoff/Agent Packets, Hive exports, repair envelopes, bead
previews, knowledge/graph/wiki, and issue markdown MAY be versioned payloads. They
are not protocols or Hive state; only Hive projects verified content into its stores.

Authority classes are derived from verified provenance:

- an authoritative default-branch non-PR run may open/update work and prove absence
  only when its complete scan is authoritative for resolution;
- an exact `pull_request` packet may drive its check/review, but cannot resolve
  target findings or approve a baseline/merge;
- a local/fork packet is proof-only and cannot mutate Hive or GitHub lifecycle;
- `pull_request_target` is never an evidence-execution or verdict-producing lane.

## Capability and authority intersection

Effective permission is the intersection of:

1. actual backend/runtime capability and the specialist's tool/MCP policy;
2. installed automation level and governor/admission decision;
3. repository/path/risk/protection/budget/pause/hold gates;
4. credentials intentionally supplied to the controller; and
5. the packet's verified scope and authority class.

A deny wins; missing/untested enforcement means no capability. Packets cannot expand authority; prompts, prose, manifests, or responses are not enforcement proof.
Every backend, especially Codex, needs positive/negative tests. Existing specialist policy supplies expertise/view; proposal-mode operational instructions are not authority.
Effective proposal capability denies GitHub/Hive/bead/wiki/graph/MCP/API/subagent writes; read-only primer context is allowed, and only the controller validates/persists proposed knowledge.

## Safety boundaries

### Transitional proposal executor (P0)

- Proof profile `v1` is an explicitly injected, one-shot **Codex-only** executor
  owned through the ordinary Manager facade. Claude, Copilot, inference backends,
  launch-command overrides, live connections, and fallback to persistent agent
  panes fail before process launch. Other backends wait for equivalent enforcement.
- This executor profile is separate from the existing role's normal backend/model/
  launch/connection configuration. Hive MUST NOT require an existing Copilot, Claude,
  or other role to be reconfigured to Codex. That normal role configuration is
  preserved unchanged and may be digest-bound only as inert expertise/policy context;
  it cannot select, configure, or grant capability to the proposal child. The child
  binds its own Codex model/provider/config/containment identity and explicitly makes
  no backend-parity claim.
- The child receives no checkout, `.git`, repository instructions, or repository
  read/write capability. Existing Worker code seals the exact base tree and supplies
  bounded regular-file source context from exact Git blobs; Worker remains the sole
  component that validates, applies, branches, and creates a PR.
- The child runs in a neutral private directory with private home/config/temp state,
  a clean allowlisted environment, reviewed capability disables, and exact child-tree
  cleanup. It cannot touch persistent agents, beads, wiki/graph, dashboard, GitHub,
  Hive control state, or model-visible network/tools.
- A started model call is ambiguous until a complete, content-bound response is
  durably recoverable. Crash recovery MUST reuse that response or hold the lease;
  it MUST NOT redispatch the same work-order/model-invocation identity.

### Baselines

- Baseline pixels and comparison semantics belong to Visual Hive evidence.
- Hive owns candidate PR creation, exact binding, approval, and merge policy.
- PRs and repair agents MUST NOT update baselines to make a failure green.
- Baseline changes require a candidate-only path, image inventory/digests, exact
  head/base/diff binding, and accountable human approval; never model/replay inference.

### `pull_request_target`

- Untrusted PR code MUST execute only on `pull_request`, with read-only credentials.
- `pull_request_target` MAY inspect metadata/trusted base files and post bounded
  directions/status after a trust check.
- It MUST NOT use PR-head code/artifacts, interpolate untrusted shell text, expose
  secrets, or issue a Visual Hive verdict/admission receipt.

### PR evidence producer and verifier isolation

- The untrusted target service and the evidence producer MUST run under
  distinct operating-system identities. The target identity MUST NOT be able
  to create, replace, rename, link, truncate, or chmod files in the producer's
  evidence root before or after sealing.
- The producer MUST emit one authenticated root receipt that binds the complete
  artifact index, exact PR head/base, pinned producer, workflow/run attempt,
  runtime, plan/config/contracts/scopes, changed-file set, and baselines. A
  hostile target-service overwrite test is required evidence, not optional
  hardening.
- Hive MUST discover the exact run attempt and named artifacts from
  authenticated GitHub metadata using independently known repository, PR,
  head/base, workflow, and producer facts. Internal artifact digests are
  untrusted claims until Hive recomputes and cross-binds them to the root
  receipt, index, and exact-head Git blobs.
- A caller MUST NOT pre-parse the downloaded artifact and feed its own claimed
  internal SHA values back as "expected" verifier inputs. If a verifier API
  cannot be called without such self-asserted pins, it is not a production
  verification boundary.

### Replay and deduplication

- `(repository ID, replay key)` identifies one immutable-digest admission; different
  digest reuse is an incident and fails closed.
- Same-digest reuse is idempotent and creates zero beads, issues, dispatches,
  branches, PRs, comments, outbox entries, or closures.
- Observation external references and linked repair identities MUST also remain
  stable across restarts and retries.

### WIP and concurrency

- Draft/WIP source or repair PRs may collect review evidence but are never merge
  eligible and MUST NOT trigger a competing repair branch or PR.
- Hive MUST reuse exact linked work/branch/PR and serialize transitions.
- Head/base drift, supersession, concurrent delivery, or expiry requires fresh verification.

### Credentials

- Hive setup/controller owns GitHub credentials and supplies the minimum scope.
- Visual Hive PR execution receives no write token or protected secret.
- Specialists receive no GitHub credential and no authenticated MCP connection;
  they return proposals to the controller-owned mailbox/API.
- Console OAuth, `kc-agent`, provider/cluster keys, and repo secrets are not integration credentials.
- Credential values never enter packets, prompts, logs, beads, knowledge, or UI.

### No merge

- Visual Hive, packets, specialists, standalone publishers, and PR workflows never
  merge. `repair-pr` authority also never merges.
- Only Hive in `auto-merge` may merge, through existing exact-SHA verdict, check,
  protection, path/risk, hold, review, and audit gates; never direct GitHub merge.

## KubeStellar Console coexistence

KubeStellar Console is the mandatory coexistence case, not a blank host. Preserve:

- its already persistent Hive/bot operation and ordinary work queue;
- repository-local trusted workflows, including Hive interaction/trust gate;
- `.github/workflows/auto-qa.yml` and its issue-producing quality program;
- `.github/workflows/auto-test-gen.yml` and its PR test-coverage feedback;
- `.github/workflows/visual-regression.yml`, committed baselines, and test rules;
- other required checks, protection, labels, branches, and maintainers' policy.

Hive/Visual Hive MAY consume these outcomes through the packet/admission, but MUST
NOT disable, duplicate, rename, replace, or claim them. Their current trust bounds remain.

Console `cmd/kc-agent` plus `pkg/agent`/MCP is a separate self-hosted application
feature. It is not a Hive specialist/server, packet transport, credential broker,
or integration control plane; its port, auth, lifecycle, and authority stay independent.

Because `ProjectConfig` is single-org and normal Hive forms `project.org/repo`, a
`DavidDiaz0317` disposable proof uses a dedicated namespaced **normal** Hive config:
separate state root, tmux/session prefix, and dashboard port, with existing policies.
That is isolation, not a new runtime. Console P0 uses the same fork-only isolation:
the Console fork and a dedicated namespaced normal Hive built from the Hive fork.
Upstream Console and the production `kubestellar` Hive remain read-only and
uninterrupted. The preliminary disposable-repository proof still cannot satisfy P0.

## Migration and reuse map

| Concern | Reuse | Required migration |
| --- | --- | --- |
| Deterministic run/verdict | Visual Hive Playwright, report, verdict | Bind exact reruns to packet/source identity |
| Packet verification | `bundle.v3`, artifact index, GitHub verifier | Make it the sole admitted interface; retire v2 for new production |
| Replay/dedupe | lifecycle replay keys; bead external refs | Move idempotence ahead of all normal admission side effects |
| Admission/routing | Governor admission; classifier/scheduler/supervisor/policy | Admit receipt; Scheduler builder fills existing work order before `Prepare` |
| Specialists | `SpecialistWorkOrder`, Mailbox, `SpecialistDispatcher`, Manager | Reuse leases/receipts/proposals; dispatch an isolated child session |
| Policy/tools | agent definitions, proxy, policy watcher | Prove consistent backend enforcement, including Codex |
| Knowledge | existing primer, wiki, graph/services | Ingest verified evidence; treat VH knowledge exports as payload only |
| Work state | existing beads | Project admitted observations once, with stable external refs |
| GitHub lifecycle | repair `Worker`, Hive client, gates/lifecycle | Keep Worker sole diff/path/apply/PR authority; Hive sole merger |
| Visibility | existing dashboard/audit/status | Add normal packet/work status; no new integration dashboard |
| Console | existing workflows and persistent Hive | Observe/coexist; do not replace repo automation |
| Channels | `v2/pkg/channels` design | Wire only through normal manager/governor after tests |

Compatibility adapters MAY read old artifacts only to produce the canonical packet;
they MUST NOT sustain a second durable lifecycle.

## Acceptance gates

All gates are blocking:

1. **Contract:** implementation PRs cite this file and add no competing goal,
   packet, lifecycle, role, dashboard, or writer.
2. **Packet:** malformed, tampered, mixed-run, expired, replay-conflicting, secret-
   bearing, and wrong-provenance packets fail before side effects.
3. **Admission:** stable role bead/external ref reaches real `Governor.AdmitWork`;
   its reason appears in normal audit/dashboard.
4. **Execution:** Scheduler fills the existing work order; Mailbox prepares/leases/
   receipts the proposal; ordinary Manager dispatches an isolated child session.
   Existing Worker alone validates/applies the diff and owns branch/PR authority.
5. **Proposal:** Hive creates/reuses at most one linked work/branch/PR; Visual Hive none.
6. **Rerun:** the exact proposal head is tested with the pinned Visual Hive commit,
   contracts, browser/runtime, and baseline digest; head drift fails. Product-code
   repairs also pin the plan/config digest. An explicitly classified test-plan repair
   MAY change only work-order-allowlisted plan/config paths: bind both before and
   after digests, require that change to be the sole authorized repair, forbid any
   reduction in required checks, and keep the producer commit, mutation operator
   implementation/ID, target contract, runtime, and baseline digests fixed.
   The producer/verifier isolation requirements above, including distinct UIDs,
   protected root, authenticated root receipt, trusted run/artifact discovery,
   internal recomputation, and hostile-overwrite rejection, are part of this gate.
7. **Lifecycle:** closure/merge uses verified rerun evidence and normal gates;
   `repair-pr` proves no merge.
8. **Coexistence:** normal Hive and Console workflows continue without lost cadence,
   replaced sessions, duplicate work, credential crossover, or dashboard outage.

### Preliminary disposable-repository proof environment

The preliminary private-fork proof MUST begin from a healthy, reviewed visual
contract. Retained AI-HPC screenshots that show an API-down/loading/error state, or
whose producing tool, repository tree, browser image, fonts, or approval provenance
cannot be reproduced, are not accepted baselines for working-product evidence.

One bounded baseline-candidate ceremony MAY occur before the proof run, solely to
establish the healthy starting contract in the private proof fork:

- generate the candidate from the exact detached repository and Visual Hive commits
  in one disposable Linux namespace, with API and frontend health-gated in that same
  namespace;
- pin and record the Playwright container image digest, browser/runtime sidecar,
  locale/timezone, viewport/DPR, font files and digests, config, and snapshot digests;
- wait for a settled success-or-error UI state rather than only for the application
  root to mount;
- require explicit accountable human review of the rendered candidate and its
  provenance; Visual Hive, Hive, models, retries, or replay MUST NOT approve it; and
- after approval, hash-check the config and snapshots before and after every proof
  run, forbid bootstrap/update, and retain the existing strict threshold.

This ceremony establishes the test oracle; it is not a repair, does not count as a
passing run, cannot close a finding, and cannot satisfy Console P0. Any later baseline
change invalidates the proof lineage and requires a new human-reviewed candidate.

### Mandatory P0 real-repository scenario

Before release work, run an actual KubeStellar Console fork and PR (not a fixture),
with both PR refs in the fork, a dedicated namespaced normal Hive built from the
Hive fork, reviewed baselines, and no-merge authority. Upstream Console and the
production KubeStellar Hive remain read-only:

1. Record exact fork base/head, installed fork commits/policy, namespaced Hive
   health/cadence, upstream read-only proof, and existing checks.
2. Select one reproducible real UI defect or deterministic failing contract; do
   not create/update a baseline, weaken a check, or expose a credential.
3. Verify one packet; create its stable role bead/ref; record `Governor.AdmitWork` reason.
4. Show Scheduler -> existing work order/Mailbox -> Manager child session producing
   one proposal; repair Worker creates/reuses at most one repair PR.
5. Rerun exact Visual Hive on the PR head; record verdict/check/digests/lifecycle.
   For a test-plan repair, record both plan/config digests and prove the allowlisted
   plan/config change is the only repair while producer/operator/runtime/baselines
   remain pinned.
6. Replay both packets; each reports idempotence and **zero duplicate work**.
7. Leave the PR unmerged. Show that ordinary Hive processed an unrelated existing
   work item/cadence through the same normal manager during the scenario, that the
   existing dashboard/audit remained available, and that Auto-QA, test generation,
   visual regression, and trust workflows were neither disabled nor replaced.

Record commands/times/SHAs, workflow/artifact IDs, packet/replay keys/digests,
before/after work counts, specialist identity, health/dashboard, and denials.

## Delivery phases

1. **Freeze:** adopt this contract; add contract/static tests and audit anchors.
2. **Intake:** adapt `bundle.v3` into normal admission with fail-closed replay.
3. **Reuse:** inject existing governor/scheduler/Manager, role beads, policy/knowledge,
   lifecycle, audit, and dashboard into the normal-service seam.
4. **Lifecycle:** make Hive proposal/PR ownership and exact Visual Hive rerun the
   only repair loop; retire parallel specialist/lifecycle writes.
5. **Coexistence:** pass the mandatory Console P0 scenario and negative cases.
6. **Release readiness:** only after P0, decide packaging/release work separately.

Each phase needs executable evidence and rollback/disable. Later phases cannot hide failures.

## Deferred and non-goals

Until P0 passes, the following are explicitly deferred/non-goals:

- releases/installers/distribution, providers, new dashboards/control planes/cards;
- new/renamed roles, a Visual Hive fleet, or another work-order queue;
- baseline capture/update/migration outside the bounded preliminary private-fork
  candidate ceremony above, or standalone Visual Hive publishers;
- direct Visual Hive writes to Hive state, GitHub, or merges;
- Console `kc-agent`/MCP authority, broad cleanup, or release/promotion work.

## Task ownership rules

- Every implementation task has one primary repository and owner: Visual Hive for
  deterministic execution/packet production, Hive for everything after emission,
  or Console only for an explicitly required repo-local compatibility change.
- Cross-repo changes split into linked tasks with exact schema/acceptance dependency;
  no task silently redefines another repository's responsibility.
- Use existing Hive roles. A new role is not a solution to integration wiring.
- Evidence producers cannot approve their own authority or lifecycle side effect.
- Work on a later phase cannot begin by declaring an unpassed gate complete.
- Any intentional contract change updates this file first, records a decision,
  names affected tests/migrations, and receives both owner groups' review.

## Verified implementation gaps

1. `v2/pkg/channels` is unwired: it defines manager/schedule/webhook/bead types,
   but no outside code imports it. Agent-definition prose is not runtime proof.
2. Backend tool/MCP enforcement is uneven: normal launches translate denies for
   Claude/Copilot, other backends fall through, and MCP flags are Claude-specific.
   Proposal-only Codex isolation exists, but ordinary Codex/intersection tests do not.

Neither gap may be treated as implemented merely because config parses or UI/API
fields exist.

## Historical/non-authoritative documents

Retain these Visual Hive histories, but they are non-authoritative on conflict:

- `docs/goals/visual-hive-complete-product.md`;
- `docs/roadmap.md` and `docs/research/visual-hive-vision-and-rationale.md`;
- `docs/agent-forward-v2/visual-hive-{complete-product-goal,codex-goal-prompt,roadmap}-agent-forward-v2.md`; and
- `docs/agent-forward-v2/visual-hive-agent-forward-integration-path.md`.

Their publishers, exports, MCP/control-plane/provider ambitions are context or
deferred ideas, not authority for parallel state, roles, release work, or writes.

## Decision log

- **2026-07-16 -- one path:** Hive orchestrates/lifecycles; Visual Hive tests/verdicts.
- **2026-07-16 -- one packet:** `bundle.v3`; other packets/exports are payload/previews.
- **2026-07-16 -- transition:** the second manager/repair path is compatibility only.
- **2026-07-16 -- proof:** isolated disposable safety first; fork-only Console P0
  with namespaced fork-built Hive gates release; upstream and production stay read-only.
- **2026-07-16 -- proof oracle:** failure-state/unproven AI-HPC screenshots are
  rejected; one exact-environment, human-reviewed healthy candidate may establish the
  private proof oracle, after which every run is strict and no-update.
- **2026-07-19 -- working vertical:** the private v10 repository passed healthy
  cadence, one prepared defect, one governed unmerged repair PR, exact-head verdict,
  and duplicate-free ordinary-service restart/replay.
- **2026-07-19 -- source boundary:** the owner lifted Hive source authority only for
  exact `kubestellar/hive:dd`; Hive main/other branches and upstream Console remain
  read-only, and release publication remains a separate decision.
