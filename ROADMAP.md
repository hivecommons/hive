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
| v4 | `v4` (default) | Supported stable line | `stable`, `candidate` |
| v5 | `v5` | Active development, RFC-gated | `edge` |

Operators select a line by pointing a hive at a
[release channel](src/docs/release-channels.md) rather than a branch tag.

## v4 — Stable Line

v4 is the default branch and the supported stable line.

- Bug fixes, security fixes, dependency updates, docs, and operability
  improvements (for example, the operator TUI shipped here in August 2026).
- Continued hardening from the security-audit remediation backlog
  (see [SECURITY.md](SECURITY.md)).
- Structural or protocol-level changes do not land here directly; they go
  through the v5 RFC process first, keeping v4 low-risk to track.
- Feature freeze (accepted 2026-09-09,
  [#6346](https://github.com/hivecommons/hive/issues/6346)): v4
  feature-freezes when all **Release train** rows of the
  [v5 GA readiness bar](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)
  are green, **or on 2026-10-15, whichever comes first**. After the freeze,
  v4 accepts security fixes and critical fixes only, and v4→v5 movement is
  cherry-pick-only (no batch syncs), each cherry-pick carrying its own
  DCO-valid sign-off. See the
  [v4 lifecycle policy](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md#v4-lifecycle-policy-accepted)
  section for the full accepted text.
- Support window: v4 remains supported through v5 development. The v4 EOL
  announcement is **blocked on completion of the
  [v5 GA readiness bar](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)**
  (live tracker: [#6016](https://github.com/hivecommons/hive/issues/6016)):
  no EOL date is announced before that checklist is closed, and any EOL is
  stated relative to the first stable v5 release.

## v5 — Next Generation

v5 development happens on the `v5` branch, gated by public `[v5 RFC]`
issues. Accepted workstreams:

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
- **Channel-based release trains.** The three channels — `edge` (newest
  good build), `candidate` (awaiting soak), `stable` (promoted after
  soak) — exist as moving GHCR tags with enforced divergence: `v4` builds
  publish `candidate`, the stable-promotion workflow advances `stable` by
  digest after the soak/evidence/blocker/smoke gate, and `v5` builds
  publish `edge`. Remaining work is digest-verifiable deployment, so what
  a spoke runs is provable rather than inferred from a tag. See
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

A documented migration path from v4 hubs and spokes, with dual-version
operation during the transition, is part of the
[v5 GA readiness bar](https://github.com/hivecommons/hive/blob/v5/src/docs/v5-ga.md)
(live tracker: [#6016](https://github.com/hivecommons/hive/issues/6016)).

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
