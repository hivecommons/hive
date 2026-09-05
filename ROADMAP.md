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
- Support window: v4 remains supported through v5 development; an explicit
  EOL relative to the first stable v5 release will be announced before
  v5 GA.

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

A documented migration path from v4 hubs and spokes, with dual-version
operation during the transition, is part of the v5 GA bar.

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
  no SLA today. Tenancy limits, data-retention, and account expectations
  will be documented as they are settled; until then, treat hosted hives
  as evaluation-grade ([#6017](https://github.com/hivecommons/hive/issues/6017)
  tracks this documentation gap).

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
