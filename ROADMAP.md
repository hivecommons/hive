# Hive Roadmap

A living document, updated by the maintainers as milestones land.
Direction-setting follows [GOVERNANCE.md](GOVERNANCE.md): items appear here
after discussion in public issues, and breaking changes go through the
supermajority process defined there.

This page covers the **release-line trajectory** — what v4 is, what v5 is
becoming, and how builds reach operators. For the detailed near-term work
plan (Now / Next / Later, item by item), see the
[public roadmap in the docs tree](src/docs/roadmap.md).

## Release lines at a glance

| Line | Branch | Status | Publishes channels |
| --- | --- | --- | --- |
| v4 | `v4` | Maintenance line (feature-frozen; security/critical fixes only) | none — builds publish `v4-latest` and short-SHA tags only ([#7721](https://github.com/hivecommons/hive/issues/7721) Phase 1) |
| v5 | `v5` (default) | Supported stable line since the 2026-09-21 cut-over | `stable`, `candidate`, `latest` |
| v6 | `v6` | Active development, tracks v5 by forward-merge; the #7563 dashboard-optional tracks are merged on the branch (see [v6 section](#v6--dashboard-optional-operation-line-open)) | `edge` |

Operators select a line by pointing a hive at a
[release channel](src/docs/release-channels.md) rather than a branch tag.

## v4 — Maintenance Line

v4 was the default branch and supported stable line until 2026-09-21, when
the channels were re-based onto v5 and `v5` became the default branch
([#7721](https://github.com/hivecommons/hive/issues/7721); the cut-over was
executed as an emergency exception to the announced sequencing — see the
deviation record at the top of that tracker and the post-hoc operator
notice, [#8105](https://github.com/hivecommons/hive/pull/8105)). v4 now
publishes no release channel: its builds carry `v4-latest` and short-SHA
tags only.

- Bug fixes, security fixes, dependency updates, docs, and operability
  improvements (for example, the operator TUI shipped here in August 2026).
- Continued hardening from the security-audit remediation backlog
  (see [SECURITY.md](SECURITY.md)).
- Structural or protocol-level changes do not land here directly; they go
  through the v5 RFC process first, keeping v4 low-risk to track.
- **Feature freeze — in effect since 2026-09-21** at v4 SHA `c354a5008`
  (policy accepted 2026-09-09,
  [#6346](https://github.com/hivecommons/hive/issues/6346); declaration:
  [#6016 comment](https://github.com/hivecommons/hive/issues/6016#issuecomment-5766618218)).
  The freeze was declared by Hub Admin decision ahead of the rows-green /
  2026-10-15 trigger, once every fleet spoke was on v5/v6 and the
  `stable`/`candidate`/`latest` channels had moved to v5. v4 now accepts
  security fixes and critical fixes only (PRs must carry `security`,
  `agent/security`, `priority/critical-urgent`, or `v4-freeze-exempt` to pass
  the required `freeze-gate` check), and v4→v5 movement is cherry-pick-only
  (no batch syncs), each cherry-pick carrying its own DCO-valid sign-off. See
  the
  [v4 lifecycle policy](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md#v4-lifecycle-policy-accepted)
  section for the full accepted text.
- Support window: **v4 reaches end-of-life on 2026-12-21** — 90 days after
  the first stable v5 release (v5.0.0, 2026-09-21), the conventional
  security/critical support window. Until that date v4 receives security and
  critical fixes only (see the freeze rules above); after it, no further v4
  releases or image retags are produced, the `v4` branch is archived
  read-only, and operators still on v4 should follow the
  [v4→v5 migration guide](https://github.com/hivecommons/hive/blob/v5/src/docs/migration-v4-v5.md).
  The date was set on 2026-09-22
  ([#8168](https://github.com/hivecommons/hive/issues/8168)) once the
  [v5 GA readiness bar](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)
  tracker ([#6016](https://github.com/hivecommons/hive/issues/6016)) closed
  and v5.0.0 shipped, per the accepted lifecycle policy
  ([#6346](https://github.com/hivecommons/hive/issues/6346),
  [#6140](https://github.com/hivecommons/hive/issues/6140)); the operator
  notice thread is [#8105](https://github.com/hivecommons/hive/pull/8105).

## v5 — Current Stable Line

v5 is the default branch and the supported stable line: since the
2026-09-21 cut-over ([#7721](https://github.com/hivecommons/hive/issues/7721)
Phases 1–3, releases v5.0.0–v5.2.0), every green merge to `v5` retags
`candidate` and `latest`, and `stable` advances by digest through the
soak-gated promotion workflow. Structural changes were gated by public
`[v5 RFC]` issues during development. Accepted workstreams:

- **Reviewer lane.** A dedicated agent role with authority to adjudicate
  escalated (`needs-human`) PRs, so the human-escalation queue has an
  owner instead of being a one-way door
  ([#5480](https://github.com/hivecommons/hive/issues/5480)). A first
  implementation is merged on `v5`
  ([#5485](https://github.com/hivecommons/hive/pull/5485)). Per
  governance, reviewer-lane output is advisory where a human-approval
  requirement exists — it never substitutes for one.
- **Formal verification of protocol invariants.** Promela/Spin models of
  protocol-shaped subsystems live in
  [`src/formal/`](https://github.com/hivecommons/hive/tree/v5/src/formal)
  on `v5`, wired as an opt-in quality-lane capability gated at ACMM L5
  ([#5512](https://github.com/hivecommons/hive/issues/5512),
  [#5518](https://github.com/hivecommons/hive/pull/5518)). The approach
  has already paid for itself: the first model proved a liveness violation
  in the escalation ledger — a PR could become red, open, and excluded
  from every lane at once ([#5511](https://github.com/hivecommons/hive/pull/5511)) —
  and the fix ([#5515](https://github.com/hivecommons/hive/pull/5515))
  flipped the violated properties to holding across millions of states.
  Goal: changes to modeled protocols update the corresponding model in
  the same PR.
- **Long-running runs and convergence generality.** The runs vocabulary is now
  documented in the
  [reference architecture](src/docs/architecture.md#archetypes-for-long-running-work):
  Oracle (`pkg/worksource`), Generator, Executor
  (`pkg/convergence/mutation`), and Gate (`pkg/convergence`), with Proof and
  Outcome as Gate inputs while the #8295 handoff/proof contract remains
  provisional. The convergence rollout definition of done is not merely "opens
  and merges PRs": the mutation, proof, and outcome path must be exercisable
  end to end by a workload that never opens a pull request, with the report-only
  audit campaign ([#8300](https://github.com/hivecommons/hive/issues/8300)) as
  the proving workload.
- **Channel-based release trains.** The three channels — `edge` (newest
  good build), `candidate` (awaiting soak), `stable` (promoted after
  soak) — exist as moving GHCR tags with enforced divergence: since the
  2026-09-21 re-base, `v5` builds publish `candidate` (and `latest`), the
  stable-promotion workflow advances `stable` by digest after the
  soak/evidence/blocker/smoke gate, and `v6` builds publish `edge`; `v4`
  publishes no channel. Remaining work is digest-verifiable deployment, so
  what a spoke runs is provable rather than inferred from a tag. See
  [release channels](src/docs/release-channels.md).
- **Multi-spoke constellations.** A production external deployment
  (tunaos.org: a self-hosted hub coordinating two spokes at ACMM L5/L6
  across 43 repos) showed that large adopters need hub-level fleet
  visibility without making the hub a hard control plane. The accepted
  v5 constellation RFC anchors repo-claim overlap warnings (phase 1
  shipped in [#5705](https://github.com/hivecommons/hive/pull/5705)),
  spoke charters, fleet headroom, GitHub App budget display,
  shared-credential guidance, and route recommendations
  ([#5691](https://github.com/hivecommons/hive/issues/5691),
  [RFC doc](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5691-constellation.md),
  [#5796](https://github.com/hivecommons/hive/pull/5796)).
- **Per-repo policy scoping.** Proposed — awaiting maintainer sign-off on [#6208](https://github.com/hivecommons/hive/issues/6208): use one repo-scoped policy model for pause, per-repo agents, and per-repo ACMM/onboarding; hive-wide ACMM remains the ceiling, and repo overrides only narrow scope.
- **Backend capacity, model inventory, and placement.** The same
  multi-spoke evidence made provider quota and model availability the
  practical constraint on splitting a hive. The accepted v5 capacity RFC
  makes backend choice an operator-controlled placement problem with
  normalized capacity readings, authoritative model inventory where
  available, tier floors, scoped limit handling, and opt-in pacing /
  placement policy
  ([#5698](https://github.com/hivecommons/hive/issues/5698),
  [RFC doc](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5698-backend-capacity-model-inventory-placement.md),
  [#5784](https://github.com/hivecommons/hive/pull/5784)).
- **Capability-aware contributor task assignment.** Proposed — the design is
  written down but not adopted, and five decisions in it are still open
  (benchmark source and licensing, acceptable model-only proxies, where task
  assessments and maintainer overrides live, how contributor-offered inventory
  extends the shared v5 contracts, and which launch paths may select
  automatically). Extends the capacity RFC above to contributor-owned
  resources: match a task's assessed capability requirement against the
  configurations a contributor has *offered*, advisory first, with automatic
  selection a later opt-in. Keeps imported benchmark evidence separate from
  adopted policy, and keeps model capability distinct from backend support
  tier and contributor trust
  ([#6825](https://github.com/hivecommons/hive/issues/6825),
  [RFC doc](docs/rfc-6825-capability-aware-contributor-task-assignment.md)).

The [v5 GA readiness bar](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)
(live tracker: [#6016](https://github.com/hivecommons/hive/issues/6016),
closed 2026-09-21) governed this line's promotion to `stable`; the
post-hoc soak evidence for the emergency promotion is recorded in
[#8062](https://github.com/hivecommons/hive/issues/8062).

## v6 — Dashboard-Optional Operation (line open)

The `v6` branch was opened by maintainer decision on 2026-09-18, cut from
v5 at `c66a944bd`. This supersedes the earlier "designation, not a line"
policy for this section: v6-targeted implementation PRs are in scope **on
the `v6` branch only** — they remain out of scope on v4 and v5, and v6
tracks v5 through the same forward-merge convention v5 uses for v4.
The epic tracking the whole line is
[#7563](https://github.com/hivecommons/hive/issues/7563).

**Priority relative to v5 GA — resolved.** This gate is satisfied: the v5
GA bar tracker ([#6016](https://github.com/hivecommons/hive/issues/6016))
closed on 2026-09-21 and the channel re-base shipped the same day
([#7721](https://github.com/hivecommons/hive/issues/7721) Phases 1–3, with
the emergency-exception deviation recorded on that tracker). `v6` now
publishes the `edge` channel, and v6 work no longer queues behind v5 GA
evidence. The line's own release bar is
[`src/docs/v6-readiness.md`](https://github.com/hivecommons/hive/blob/v6/src/docs/v6-readiness.md)
with live tracker [#7683](https://github.com/hivecommons/hive/issues/7683).
(Original sequencing decision: [#7577](https://github.com/hivecommons/hive/issues/7577);
v4 freeze policy: [#6346](https://github.com/hivecommons/hive/issues/6346).)

**Theme.** Every operator interaction the dashboard offers should be
reachable from the places humans already are — a GitHub thread, a chat
workspace, an inbox, a phone. The dashboard remains the richest surface,
never the only one.

**Guard invariant (non-negotiable).** Every non-dashboard surface routes
through the *same* authorization and safety machinery: the dashboard role
floor, the mode ladder and capabilities checked at the proxy, `Converse`
for replies, `ioscan` enforcement on all inbound text, and canary/secret
scrubbing on all outbound text. No surface grows its own authz.

Workstreams (details and sequencing in
[#7563](https://github.com/hivecommons/hive/issues/7563)). **Status below is
`v6`-branch status, not a stable-release claim** — none of this code exists
on `v4` or `v5`. Since the 2026-09-21 re-base, `edge` is built from `v6`,
so an operator tracking `edge` runs these surfaces as active-development
builds; operators on `stable`/`candidate` (v5) do not have them:

- **`pkg/chat` spine** — the transport-agnostic core of the Discord bot
  (command router, dashboard REST/SSE client, notification fan-out) so every
  chat backend shares one implementation. **Merged on `v6`**
  ([#7572](https://github.com/hivecommons/hive/pull/7572)); `pkg/discord`
  became transport-only in the same PR.
- **Discord fix** — reconnect/backoff contract, heartbeat watchdog, and
  parity with the operator surfaces the bot predates. **Merged on `v6`**
  ([#7586](https://github.com/hivecommons/hive/pull/7586)), as reconnect and
  cancellation parity across every backend rather than a Discord-only fix.
- **Slack** — Socket Mode first (works from pull-only clusters with no
  public URL), Events API as a later accelerator. **Socket Mode merged on
  `v6`** ([#7585](https://github.com/hivecommons/hive/pull/7585),
  [design doc](src/docs/design/slack-integration.md)); the Events API
  accelerator is not built.
- **GitHub @-mention triggers** — a human summons an agent by mentioning
  the App on an issue or PR, mirroring the existing Linear inbound-mention
  path ([#7483](https://github.com/hivecommons/hive/issues/7483),
  [design doc](src/docs/design/github-mention-triggers.md)). **All three
  phases merged on `v6`** — poller and guards
  ([#7582](https://github.com/hivecommons/hive/pull/7582)), in-thread replies
  through `Converse` ([#7597](https://github.com/hivecommons/hive/pull/7597)),
  webhook accelerator ([#7623](https://github.com/hivecommons/hive/pull/7623))
  — which is also what makes GitHub Mobile a hive remote on that line.
- **More chat backends** — Microsoft Teams, Matrix, Telegram, as
  `chat.Backend` implementations on the spine. **All three merged on `v6`**
  ([#7621](https://github.com/hivecommons/hive/pull/7621),
  [#7617](https://github.com/hivecommons/hive/pull/7617),
  [#7616](https://github.com/hivecommons/hive/pull/7616)).
- **Escalation surfaces** — email (outbound digest and HUMAN DECISION
  NEEDED escalation; allowlisted inbound reply-to-act) and push/on-call
  (ntfy / Pushover / PagerDuty) so a `requires_human` verdict pages a
  person instead of waiting in a queue. **Outbound merged on `v6`**
  ([#7618](https://github.com/hivecommons/hive/pull/7618),
  [design doc](src/docs/design/escalation-surfaces.md)): `pkg/escalate`
  severity routing, SMTP sink with daily digest, and the three push sinks.
  Inbound reply-to-act is still unbuilt, as the design sequenced it.

Beyond the dashboard-optional theme, the line has begun accepting new
tracks by RFC, each measured against the same readiness bar
([#7683](https://github.com/hivecommons/hive/issues/7683) §Scope
discipline):

- **Task-scoped MCP** — expose the hive's view of a task (issue, PR,
  advisory context) to contributor agents as a Model Context Protocol
  endpoint, so the environment is queried rather than pasted into prompts
  ([#8033](https://github.com/hivecommons/hive/issues/8033),
  [design doc](https://github.com/hivecommons/hive/blob/v6/src/docs/design/task-mcp.md)).
  **Phases 1–3 merged on `v6`**: Phase 1
  ([#8164](https://github.com/hivecommons/hive/pull/8164),
  [#8177](https://github.com/hivecommons/hive/pull/8177),
  [#8179](https://github.com/hivecommons/hive/pull/8179)) — `pkg/taskmcp`,
  the `/api/contribute/mcp` endpoint, agent-manager wiring, and the
  `task_context`, `context_bundle`, `related_work` and `ci_health` tools;
  Phase 2 (lease-scoped auth for remote contributors,
  [#8228](https://github.com/hivecommons/hive/pull/8228)); Phase 3 (repo
  tools, [#8244](https://github.com/hivecommons/hive/pull/8244)). The RFC
  closed on 2026-09-22 with the first live token-delta measurement, and
  the remaining saving — eliding stuffed issue/PR lists when the MCP
  pointer is present ([#8261](https://github.com/hivecommons/hive/issues/8261)) —
  shipped in [#8272](https://github.com/hivecommons/hive/pull/8272).
- **Standby contributors** — a lane paused for budget hands its queue to
  volunteer contributors, behind a model floor
  ([#7629](https://github.com/hivecommons/hive/issues/7629),
  [design doc](https://github.com/hivecommons/hive/blob/v6/src/docs/design/standby-contributors.md),
  [RFC doc](https://github.com/hivecommons/hive/blob/v6/docs/rfc-7629-standby-contributors.md)).
  **S0–S7 merged on `v6`** (design
  [#8038](https://github.com/hivecommons/hive/pull/8038); config floor,
  matcher, manual dispatch, outcome ledger and suspend rule, item-tier
  matching: [#8083](https://github.com/hivecommons/hive/pull/8083)–[#8163](https://github.com/hivecommons/hive/pull/8163)).
  S8 waits on live-hive runbook evidence.
- **Long-running runs** — a spec, plan or implement stage, or an audit
  campaign, modelled as one task lease whose stage advances by generation,
  with admission, verification and publication as three separate gates and
  Unknown as a real state. Umbrella epic
  [#8290](https://github.com/hivecommons/hive/issues/8290) gathers three
  design homes that stay open:
  [#7620](https://github.com/hivecommons/hive/issues/7620) archetype
  vocabulary, [#8201](https://github.com/hivecommons/hive/issues/8201)
  external-workflow admission contract, and
  [#8227](https://github.com/hivecommons/hive/issues/8227) Spektacular under
  Hive orchestration. **Spine landed on v5, surfaces landed on v6**: the
  convergence-layer prerequisites
  ([#8287](https://github.com/hivecommons/hive/issues/8287),
  [#8288](https://github.com/hivecommons/hive/issues/8288)), the lease
  `stage` field, the `stage_completed` hook handoff and the read-only
  `GET /api/runs` projection are in v5; chat checkpoint approvals, the
  dashboard and TUI Runs panes, hub-health run verdicts and the GitHub
  status-comment/receipt surfaces are in v6. The #8201 Gate 0 option is
  decided (contributor protocol plus a durable external-execution binding,
  one bounded workflow, report-only, no publication credentials; record in
  [#8302](https://github.com/hivecommons/hive/issues/8302)), and **Gate 1
  shipped on v5 on 2026-09-23**
  ([#8361](https://github.com/hivecommons/hive/issues/8361), merged via
  [#8404](https://github.com/hivecommons/hive/pull/8404)): the
  engine-neutral external-execution contract in `pkg/extwork` plus a Flue
  adapter proving #8201 against a foreign durable-workflow engine — keyed
  admission with the task lease as the authority record, five-state
  observation with Unknown real, digest-verified receipt fetch, and
  crash-window recovery by pinned incarnation. The pilot stays report-only
  with no publication credentials, defaults off behind
  `runs.external.flue`, and the adapter links into the binary only under
  the `extwork_flue` build tag. **Two proving workloads have landed**: the
  report-only audit campaign
  ([#8300](https://github.com/hivecommons/hive/issues/8300)), and the
  Crustify/Wavefront migration-graph work source
  ([#8362](https://github.com/hivecommons/hive/issues/8362), closed) — a
  `pkg/worksource` oracle that reads a Wavefront dependency graph, admits
  ready nodes as run stages
  ([#8392](https://github.com/hivecommons/hive/pull/8392)), records
  completed items back into the graph
  ([#8525](https://github.com/hivecommons/hive/pull/8525)) and exposes
  per-run burndown through `GET /api/runs`
  ([#8552](https://github.com/hivecommons/hive/pull/8552)), with a
  scheduled smoke canary against a real graph
  ([#8521](https://github.com/hivecommons/hive/pull/8521)). Both were
  exercised live on 2026-09-23 with evidence recorded on the acceptance
  tracker [#8466](https://github.com/hivecommons/hive/issues/8466), which
  is the remaining work that proves Flue, Crustify/Wavefront and
  Spektacular together. A cross-repo audit index landed on 2026-09-24
  ([#8617](https://github.com/hivecommons/hive/issues/8617), shipped via
  [#8621](https://github.com/hivecommons/hive/pull/8621)): an owner-gated,
  read-only `GET /api/runs/audit` join over the existing audit log,
  timeline, lease-receipt and plan-epic artifacts, with the retention and
  `expired`/`Unknown` marker contract documented rather than a new store —
  giving operators one query surface across the report-only campaigns
  above. Remaining constituents
  (#8301–#8319, #8345–#8364) are ticked off on #8290 as they merge; no
  store, CRD, DSL or GitHub credentials are added to the pilot and every
  surface goes through the existing guard invariant.
- Named for later, not scheduled: Jira (mirroring the Linear agent), an
  IDE extension over the dashboard API, a subscribable calendar feed of
  scheduled kicks.

With the dashboard-optional tracks merged, what *that theme* still owes is
**release-path evidence**, while the line itself stays open to new tracks
entering via RFC (as the two above did): since 2026-09-21, `v6` publishes
the `edge` channel
([#8060](https://github.com/hivecommons/hive/pull/8060)), so the surfaces
above reach operators tracking `edge` — but `edge` is an active-development
build, not a stable claim. The line's promotion bar is
[`src/docs/v6-readiness.md`](https://github.com/hivecommons/hive/blob/v6/src/docs/v6-readiness.md)
(live tracker: [#7683](https://github.com/hivecommons/hive/issues/7683)):
guard-invariant conformance rows are checked for all eight shipped
surfaces; the per-surface live exercises remain open.

## Hosted Hive Hub

The hosted hub at [hive.hivecommons.dev](https://hive.hivecommons.dev) is the
project's zero-friction on-ramp: OAuth-protected dashboards, a public
registry, cross-hive leaderboards, and provisioned hives with no cluster
required. It is developed in this repo (`src/pkg/hub/`) and has recently
absorbed significant work — hosted-hive provisioning and admin tooling,
multi-login, the fleet wildcard certificate for provisioned spokes
([#5981](https://github.com/hivecommons/hive/pull/5981)), and the domain
cutover to `hive.hivecommons.dev`
([#5955](https://github.com/hivecommons/hive/pull/5955), see
[UPGRADE.md](UPGRADE.md)).

Direction:

- **Hosted is the funnel, self-hosted is the destination.** The hosted hub
  exists to let adopters evaluate and run small hives without
  infrastructure; larger fleets are expected to graduate to a self-hosted
  hub (as tunaos.org did). Feature work lands in the shared codebase so
  both deployments stay equivalent.
- **Fleet features land on the hosted hub first.** The v5 constellation
  ([#5691](https://github.com/hivecommons/hive/issues/5691)) and backend
  capacity/placement ([#5698](https://github.com/hivecommons/hive/issues/5698))
  workstreams are exercised by the hosted fleet before they are
  recommended to self-hosted operators.
- **Best-effort service, explicit expectations.** The hosted hub carries
  no SLA today; treat hosted hives as evaluation-grade unless an operator
  makes a separate commitment. Account creation is OAuth/OIDC-backed and
  passwordless, new non-admin users have no self-service hosted quota until
  an admin grants it, and admins may block or delete hub account records
  (`src/pkg/hub/saas.go`). Tenancy is bounded by per-user quota
  (`SaaSUser.SaaSQuota`), per-cluster `max_hives`, placeholder-pool
  `pool_min`/`pool_target`, and the live Scale Controls defaults in
  `src/pkg/hub/scale_settings.go`; provisioning throughput is bounded by
  `HIVE_PROVISION_WORKERS` / `HIVE_PROVISION_PER_CLUSTER`
  (`src/pkg/hub/provision_queue.go`). Data retention is operational rather
  than SLA-backed: hub user and hive records live under `/data/saas`, usage
  history is capped by `usageSnapshotMaxPoints`, timeline history by
  `timelineMaxEvents`, and unconfigured or inactive hosted hives may be
  reclaimed to free capacity. Operational docs live in
  [hosted-hub.md](src/docs/hosted-hub.md),
  [manual-provisioning.md](src/docs/manual-provisioning.md),
  [hub-deployment.md](src/docs/hub-deployment.md),
  [spoke-wildcard-tls.md](src/docs/spoke-wildcard-tls.md), and
  [troubleshooting.md](src/docs/troubleshooting.md).

## Recently shipped (September 2026)

Roughly ninety v4 releases (v4.19.0 → v4.51.2) landed between 2026-09-08 and
2026-09-17. Themes, with representative releases ([CHANGELOG.md](CHANGELOG.md)
has the full record):

- **Merge-safety hardening**: the merge-request watcher independently
  verifies CI and no longer treats absent check runs as passing (v4.19.0),
  refuses merges into base branches without branch protection unless
  explicitly allowlisted (v4.22.0), and `docker.yml` refuses a dispatched
  `release_sha` that is not an ancestor of the branch (v4.23.1). A
  post-merge DCO trailer check surfaces squash commits with missing
  sign-offs (v4.21.0).
- **Hub↔spoke trust**: hub→spoke heartbeat responses are independently
  signed (v4.36.0) with persisted verifier state (v4.37.0); agent-control
  mutation endpoints require the owner role (v4.27.2); `ioscan` canary
  egress detection covers encoded/obfuscated exfiltration (v4.28.3).
- **Large-spoke scale envelope**: the backlog size one spoke is known to
  work at is now a stated design dimension —
  [`src/docs/scale-envelope.md`](src/docs/scale-envelope.md) (v4.51.0,
  [#7392](https://github.com/hivecommons/hive/issues/7392)) — with
  per-repo governor cadences (v4.31.0–v4.32.0) and held-red-PR repair
  routed back to the owning agent instead of deadlocking
  (v4.51.2, [#7438](https://github.com/hivecommons/hive/issues/7438)).
- **Operator visibility**: struggling-contributor diagnosis from the
  dashboard without hub shell access (v4.46.0–v4.49.0), contributor-queue
  placement explanations (v4.30.0), and `backend_auth` / `agent_auth`
  canaries for inference-provider failures (v4.26.0).
- **Fleet self-reporting**: an opted-in hive can file calibrated
  hive-defect reports upstream, from dry-run previews (v4.41.0) to
  documented filing (v4.44.0).
- **Backends**: muse (v4.21.0) and Oh My Pi interactive (v4.28.0)
  contributor backends, per-agent `reasoning_effort` with the gpt-6-astra
  default (v4.19.0), and Copilot device-flow login that verifies the seat
  before reporting success (v4.47.0).
- **Attribution and issue lifecycle**: issue authors credited as
  co-authors on resolving commits (v4.26.0), requester attribution on
  hive-opened PRs (v4.39.0), and task-list sweeps that close hive-filed
  issues when every checkbox is ticked and a merged PR references them
  (v4.34.0–v4.35.0).
- **v4→v5 mechanics**: the forward-merge is a mechanical cadence
  (v4.47.0, [#7297](https://github.com/hivecommons/hive/issues/7297)),
  and `v5` collapsed to v4's file layout to cut merge friction toward the
  GA bar ([#6016](https://github.com/hivecommons/hive/issues/6016)).

## Recently shipped (August 2026)

Highlights from the last month of merges to `v4` (and `v5` where noted):

- **v4.0.1 and v4.0.2 releases** through the gated release workflow, with
  the release gate now mirrored as a commit status.
- **Operator TUI**: a terminal dashboard with agent kick, model picker,
  ACMM overlay, governor header, token/cost estimates, and an operator
  guide in the [hivectl docs](src/docs/hivectl.md).
- **Escalation visibility**: `needs-human` escalated PRs surfaced in the
  dashboard repo section, plus remediation hints (detectors, verdicts,
  and UI) so an operator sees *why* something escalated.
- **Backends**: Google Antigravity models offered in the dashboard, and a
  backend smoke-test matrix in CI.
- **Scheduling**: split poll cadences for issues vs. PRs, and an
  events/audit refresh in the dashboard.
- **Security and privacy**: ttyd bound to loopback, snapshot checkout
  guard, and contributor-relay redaction fixes.
- **Forge abstraction**: governor escalation writes are now typed against
  the neutral forge seam rather than the GitHub client, the first
  production caller on the multi-forge path.
- **v5**: reviewer lane first implementation, the formal-verification
  quality lane, and the first Spin model plus the real bug it found.
- **CI and test hardening**: sharded hub tests, hermetic test fixtures,
  and a series of flaky-test fixes.

## Future (post-v5)

Candidate themes, deliberately not committed — see the
[Later section of the detailed roadmap](src/docs/roadmap.md#later):

- Gitea/Forgejo forge program sequencing is proposed against the v5 GA bar: Wave 0 docs-only ADRs now, Wave 1 behind `pkg/forge` on `v5`, and enumeration-policy extraction only post-GA-cut or v5-first without resetting the required-gate soak ([#6177](https://github.com/hivecommons/hive/issues/6177), [#6167](https://github.com/hivecommons/hive/issues/6167)).
- Cross-forge orchestration (GitHub, GitLab, Forgejo/Gitea) on the
  `pkg/forge` abstraction.
- Memory and learning maturation: durable, auditable priming from retro
  findings and curated knowledge.
- Kubernetes-native, policy-isolated agent sandboxes where that
  complexity is justified.

## Non-goals (current)

- **Replacing human judgment.** Agent and reviewer-lane output is
  advisory wherever a human-approval requirement exists; Hive automates
  the pipeline around judgment, not the judgment itself.
- **A second docs site.** Docs are published by syncing `src/docs/` into
  the org docs site; this repo deliberately carries no site generator of
  its own.
