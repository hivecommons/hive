# Hive v2 documentation

This index organizes the versioned Hive docs under `v2/docs/`. It is the
precursor to a published docs site.

## Getting Started

- [Agent configuration](agent-configuration.md) — configure lanes, models,
  channels, policy modes, and ACMM packs.
- [Hive CLI](hivectl.md) — inspect agents, beads, events, config, knowledge, and
  lite enrollment.
- [Hive lite enrollment](lite-enrollment.md) — five-minute, zero-secret advisory
  enrollment for a repository.
- [Credly badges](credly-badges.md) — public badge/credential surfaces.

## Architecture

- [Reference architecture](architecture.md) — process model, governor,
  deterministic pipeline, guardrails, ACMM, beads, hub/spoke, model backends,
  and observability.
- [CNCF reference architecture](cncf-reference-architecture.md) — cloud-native
  framing for Hive's components and security boundaries.
- [ACMM policy matrix](acmm-policy-matrix.md) — autonomy levels mapped to agent
  policy modes.
- [Configuration layering](config-layering.md) — seed, overlay, dashboard, and
  hub/spoke configuration precedence.
- [Planning intelligence](planning-intelligence.md) — goal decomposition, plan
  review, and stall-replan concepts.
- [Knowledge system design](design/knowledge-system.md) — layered knowledge,
  primers, and extraction design.

## Security

- [Security threat model](security-threat-model.md) — actors, boundaries,
  layered defenses, known gaps, and reporting.
- [Architecture Decision Records](adr/README.md) — lightweight ADR process and
  records 0001-0010.
- [Intent verification](intent-verification.md) — tier-based change
  authorization for merge eligibility.
- [Trajectory review](trajectory-review.md) — drift detection and agent pause
  semantics.
- [Rootless Podman CI seam](podman-rootless-ci.md) — documented test intent and
  static contract for contributor-container runtime handling.

## Operations

- [Manual provisioning](manual-provisioning.md) — hub/spoke provisioning paths,
  authentication modes, and troubleshooting.
- [Cross-cluster migration](cross-cluster-migration.md) — moving spokes across
  clusters, including heartbeat-only clusters.
- [Retro lane](retro-lane.md) — deterministic post-completion analysis and
  advisory beads.
- [Review swarm](review-swarm.md) — structured review perspectives, verdicts,
  and merge-gate integration.

## Roadmap & Positioning

- [Public roadmap](roadmap.md) — Now / Next / Later direction for v4 catch-up and
  follow-on work.
- [Landscape and positioning](landscape.md) — comparison with Fullsend,
  single-agent tools, hosted services, and research harnesses.
