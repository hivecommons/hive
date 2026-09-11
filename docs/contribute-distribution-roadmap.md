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

### Phase 1 — Distroless contribute container

New contributor image: distroless base, roughly half current size, chunked
for delta-pull performance, reproducible builds, all in GHA.

Acceptance:
- Image builds reproducibly in CI; multi-arch (amd64+arm64) parity with the
  existing `hive-contributor` image.
- Existing relay flow (`just contribute-hive`) works unchanged against the
  new image before it becomes the default.
- Size reduction and repro claims recorded in the PR body with measurements.

### Phase 2 — Homebrew tap GitHub Action

The apptainer conversion action in the tap repo, consuming the Phase 1 image
by digest.

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
