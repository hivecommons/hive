# Contribute-App Distribution Roadmap — apptainer/homebrew one-command install

Status: planning (hold-gated) · Owner: strategist · Refs: #6635, #6641

Issue #6635 (castrojo) proposes collapsing the contribute on-ramp into a single
command:

```bash
brew install kubestellar/hive/contribute
```

An apptainer (LF-donated Singularity) build converts the OCI contributor image
into a self-contained binary shippable through a homebrew tap, working across
macOS, Windows, and Linux. It auto-detects KVM on the host and uses it, falls
back to gVisor when present — a materially better local-confinement story for
a contribution tool than the current flow.

This document places that work on the roadmap: phases, acceptance criteria,
and the upstream-vs-tap-repo ownership split, so each piece lands as a small
reviewable PR instead of one ad-hoc drop. Implementation stays with #6635;
this doc tracks planning only.

## Why this matters

The current contribute flow is four steps (`brew install just gh`, clone,
`just contribute-setup <cli>`, `just contribute-hive`) and requires a full
repo checkout. It is also repeatedly flagged as under-documented — the README
names the commands but never explains what ClankeR does or what the just
targets set up. A one-command install is the single highest-leverage
adoption improvement available, and the KVM/gVisor detection strengthens the
"your credentials never leave your machine" story with actual confinement.

## Ownership split (decision to ratify in review)

- **Upstream (this repo)** owns: the contributor OCI image
  (`ghcr.io/hivecommons/hive-contributor`, already published multi-arch by
  digest with immutable short-SHA tags plus a `candidate` channel via
  `.github/workflows/docker.yml`), its size/reproducibility improvements, and
  documentation of the install path.
- **Tap repo (`kubestellar/homebrew-hive` or equivalent)** owns: the GitHub
  Action that runs the apptainer conversion and publishes the brew formula.
  Upstream's only contract is a stable image reference and tag/digest
  discipline — scanner has confirmed this precondition is already satisfied,
  so no upstream publishing changes are required for Phase 2.

## Phases

### Phase 0 — First-run reliability gate (added per #6656)

Distribution amplifies whatever first-run experience exists. Two bugs filed
2026-09-11 show the containerized flow currently fails on the first command
of every task, so the funnel Phase 2 widens would land on a broken first
impression:

- #6654 — the assignment prompt (`src/pkg/dashboard/contribute_ws.go:5555`)
  emits a `gh repo fork ... --remote=true <dir>` invocation that current `gh`
  rejects (`--remote` invalid with a repository argument, no destination
  positional), and unconditionally assumes a fork is possible even when the
  contributor owns the repo. The documented first step fails 100% of the
  time, and the #2545 workspace-path contract depends on the checkout path it
  drops.
- #6653 — `ghcr.io/hivecommons/hive-contributor:latest` ships no `bwrap` and
  the container blocks unprivileged user namespaces, so codex (the container
  default) burns its first tool call on a sandbox failure and runs with a
  weaker sandbox posture than the launch flags request — undercutting the
  KVM/gVisor confinement story this roadmap claims.

Acceptance (all must land before Phase 2 publishes the formula):
- #6654 fixed: the prompt emits a `gh`-valid clone/fork sequence that lands
  the checkout at the #2545 contract path, with an owner==contributor branch
  path.
- #6653 resolved or explicitly postured: `bwrap` present in the image and the
  userns decision documented, or codex launched with flags matching what the
  container actually provides.
- A first-run smoke test in CI (container up → task assigned → first prompted
  command exits 0), also added to Phase 1 acceptance so the distroless image
  cannot regress it.

Implementation belongs to the bug issues; this phase is a sequencing gate
only.

### Phase 1 — Distroless contribute container

New contributor image: distroless base, roughly half current size, chunked
for delta-pull performance, reproducible builds, all in GHA.

Acceptance:
- Image builds reproducibly in CI; multi-arch (amd64+arm64) parity with the
  existing `hive-contributor` image.
- The Phase 0 first-run smoke test passes against the new image.
- Existing relay flow (`just contribute-hive`) works unchanged against the
  new image before it becomes the default.
- Size reduction and repro claims recorded in the PR body with measurements.

### Phase 2 — Homebrew tap GitHub Action

The apptainer conversion action in the tap repo, consuming the Phase 1 image
by digest. Blocked on Phase 0 completion (#6656).

Acceptance:
- `brew install kubestellar/hive/contribute` produces a working binary on
  macOS and Linux (Windows path documented separately).
- Binary name decided and recorded here (open question below).
- KVM/gVisor detection behavior documented, including the fallback when
  neither is present.
- README "Contribute to a Hive" section updated to lead with the brew
  one-liner; the just-based flow remains documented as the from-source path.

### Phase 3 — Agent-harness integration (omp / pi.dev)

"Hive as a plugin to an agent harness": oh-my-pi integration for caching and
throughput. Explicitly last — it depends on the Phase 1/2 surface being
stable and has the largest design surface (cf. the T3 refusal gate landed for
`omp` in #6626).

Acceptance:
- A design note lands before implementation, covering the harness boundary
  and what hive-side configuration it reads.

Out of scope for this roadmap: the "review version" castrojo mentions
(requires hive/bluefin syncing) — it should get its own issue when proposed.

## Open questions

1. **Binary name** — `contribute` (as in the formula path), `hive-contribute`,
   or `clanker`? Needs a maintainer decision in #6635 before Phase 2 ships.
2. **Tap repo location and ownership** — under `kubestellar` or `hivecommons`?
3. **Channel policy** — does the formula track the `candidate` channel or
   only tagged releases?

## Success signals

- Time-to-first-contribution for a new contributor drops from a multi-step
  clone flow to a single install plus one command.
- Contributor relay registrations from hosts without a repo checkout appear
  in hub telemetry.
