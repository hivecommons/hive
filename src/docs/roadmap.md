# Hive public roadmap

> **Directional, not a promise.** This roadmap reflects the public v4 direction as
> of **2026-09-03**. It is maintained by pull request and can change as issues,
> security reviews, and operator feedback change the order of work.
>
> The umbrella issues this page grew out of —
> [#2812](https://github.com/hivecommons/hive/issues/2812) (catch-up epic) and
> [#2811](https://github.com/hivecommons/hive/issues/2811) (docs catch-up) — were
> **both closed on 2026-08-08**. They are cited below as the origin of a line of
> work, not as live trackers: a closed umbrella says nothing about whether any
> particular item under it shipped, so each row states its own status.

Hive's roadmap is organized as **Now / Next / Later**: near-term work already in
flight, likely follow-ups, and longer-horizon bets. Items are listed in planning
order, not priority rank.

## Now

| Work | Outcome | Tracking |
| --- | --- | --- |
| Prompt-injection defense-in-depth | Canary-token checks, output redaction, fail-closed scanning, and the optional model-based semantic classifier build on deterministic `ioscan` kick-path redaction. | [#2805](https://github.com/hivecommons/hive/issues/2805), [security threat model](security-threat-model.md), [ADR-0008](adr/0008-ioscan-untrusted-input.md) |
| Intent verification | Tier-based change authorization is in the merge-gate path; trajectory-integrated intent-alignment review remains the next slice. | [#2803](https://github.com/hivecommons/hive/issues/2803), [intent verification](intent-verification.md) |
| Review fan-out | Structured review reports and deterministic aggregation are in place; scheduler fan-out to parallel review perspectives is the deferred wiring. | [#2807](https://github.com/hivecommons/hive/issues/2807), [review swarm](review-swarm.md) |
| Credential-free sandbox kick path | Move agent execution toward no live token and no direct network in the sandbox, with trusted host-side post-steps retaining the MITM proxy as an outer layer. | [#2804](https://github.com/hivecommons/hive/issues/2804), [security threat model](security-threat-model.md#residual-risks-and-known-gaps) |
| Spoke-based lite enrollment | Keep `hivectl enroll OWNER/REPO` as a zero-secret on-ramp by adding repos to an existing spoke or provisioning a hosted lite spoke; the hub tracks only spokes. | [#2808](https://github.com/hivecommons/hive/issues/2808), [lite enrollment](lite-enrollment.md) |
| Multi-spoke constellation foundations | Accepted for v5 phase 1 after production evidence from tunaos.org (self-hosted hub, two ACMM L5/L6 spokes, 43 repos). The first slice, repo-claim overlap detection, shipped in #5705; remaining slices add spoke charters, fleet headroom, GitHub App budget visibility, shared-credential guidance, and route recommendations without turning the hub into a hard control plane. | [#5691](https://github.com/hivecommons/hive/issues/5691), [RFC #5691](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5691-constellation.md), [#5705](https://github.com/hivecommons/hive/pull/5705), [#5796](https://github.com/hivecommons/hive/pull/5796) |
| Backend capacity, model inventory, and placement | Accepted for v5 phase 1 because multi-spoke fleets are constrained by shared provider quota, model availability, and policy, not only by repo ownership. The work standardizes capacity readings, inventory authority, tier floors, scoped limits, and opt-in placement / pacing before scheduler behaviour depends on them. | [#5698](https://github.com/hivecommons/hive/issues/5698), [RFC #5698](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5698-backend-capacity-model-inventory-placement.md), [#5784](https://github.com/hivecommons/hive/pull/5784) |
| Retrospective learning lane | Build from deterministic post-completion advisory beads toward LLM-assisted retro summaries and knowledge extraction. | [#2809](https://github.com/hivecommons/hive/issues/2809), [retro lane](retro-lane.md) |
| ADR back-fill for remaining subsystems | **Done.** Back-filled accepted ADRs capture the knowledge system, skill registry, CEL/channel triggers, and hub/spoke mechanics, so architecture decisions stay auditable. | [ADR-0011](adr/0011-knowledge-system.md), [ADR-0012](adr/0012-skill-registry.md), [ADR-0013](adr/0013-cel-triggers.md), [ADR-0014](adr/0014-hub-spoke.md) |

## Next

| Work | Outcome | Tracking |
| --- | --- | --- |
| Docs site publication | **Live, not deferred.** The org already runs one docs site — [kubestellar/docs](https://github.com/kubestellar/docs) (Next.js on Netlify) — and pulls a growing subset of `src/docs/` straight from this repo's `v4` branch on every build, publishing at [kubestellar.io/docs/hive](https://kubestellar.io/docs/hive) with links rewritten to site routes. There is deliberately no second site generator in this tree (a per-repo MkDocs/Docusaurus config would duplicate that pipeline); `hive.kubestellar.io` itself serves the product/dashboard landing page, not docs. This repo's job is keeping `src/docs/` a correct source for that sync — see [Docs Link Check](https://github.com/hivecommons/hive/blob/v4/.github/workflows/docs-link-check.yml), which gates relative links and heading anchors on every PR touching `src/docs/`. Remaining follow-up, tracked separately: expanding the sync manifest (`kubestellar/docs:scripts/sync-hive-docs.ts`) to cover the ~65 pages not yet on it is a change to that repo, not this one. | [docs index](README.md), origin: [#2811](https://github.com/hivecommons/hive/issues/2811) (closed), tracker: [#5258](https://github.com/hivecommons/hive/issues/5258) |
| GitLab through `pkg/forge` | `pkg/forge` ships GitHub, GitLab, and Gitea/Forgejo adapters with the read path and core write path implemented and tested; `Merge` is left an explicit interface TODO because merge semantics diverge across forges. **First production caller landed:** the governor's escalation writes (evidence comment + `needs-human` label) are now typed against the `forge.IssueWriter` seam, with the adapter selected from `project.forge` — so that key is no longer display-only. A GitHub hive is unchanged, still on `*github.Client`. Those writes are not yet *reached* on a non-GitHub hive, because the read path is still GitHub-shaped: `EnumerateActionable` feeds the whole governor cycle and owns hold-label filtering, issue filters and SLA tracking inside `pkg/github`. Neutralizing enumeration — lifting that policy above the forge boundary, and adding a bulk list method so an N-repo hive does not enumerate N times — is what remains. | [ADR-0005](adr/0005-forge-abstraction.md), [#5259](https://github.com/hivecommons/hive/issues/5259), origin: [#2812](https://github.com/hivecommons/hive/issues/2812) (closed) |

## Later

| Work | Outcome | Tracking |
| --- | --- | --- |
| Cross-forge orchestration | Coordinate issues, merge requests, policy, and evidence across GitHub, GitLab, and Forgejo/Gitea-style forges. | [ADR-0005](adr/0005-forge-abstraction.md) |
| Memory and learning maturation | Turn retro findings and curated knowledge into durable, testable priming without hidden or unauditable agent memory. | [knowledge design](design/knowledge-system.md), [retro lane](retro-lane.md) |
| Kubernetes-native agent sandboxes | Graduate from tmux/container execution toward k8s-native, policy-isolated agent workloads where that complexity is justified. | [architecture](architecture.md), [security threat model](security-threat-model.md) |

## Reading this roadmap

- **Now** does not mean all work is complete; it means active phase-2 work or
  freshly merged foundations with known wiring still in progress.
- **Next** items are expected follow-ups, not release commitments.
- **Later** items are strategic directions that may split into narrower issues
  before implementation.
