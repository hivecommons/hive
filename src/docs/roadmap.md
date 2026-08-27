# Hive public roadmap

> **Directional, not a promise.** This roadmap reflects the public v4 direction as
> of **2026-08-27**. It is maintained by pull request and can change as issues,
> security reviews, and operator feedback change the order of work.
>
> The umbrella issues this page grew out of —
> [#2812](https://github.com/kubestellar/hive/issues/2812) (catch-up epic) and
> [#2811](https://github.com/kubestellar/hive/issues/2811) (docs catch-up) — were
> **both closed on 2026-08-08**. They are cited below as the origin of a line of
> work, not as live trackers: a closed umbrella says nothing about whether any
> particular item under it shipped, so each row states its own status.

Hive's roadmap is organized as **Now / Next / Later**: near-term work already in
flight, likely follow-ups, and longer-horizon bets. Items are listed in planning
order, not priority rank.

## Now

| Work | Outcome | Tracking |
| --- | --- | --- |
| Prompt-injection defense-in-depth | Canary-token checks, output redaction, fail-closed scanning, and the optional model-based semantic classifier build on deterministic `ioscan` kick-path redaction. | [#2805](https://github.com/kubestellar/hive/issues/2805), [security threat model](security-threat-model.md), [ADR-0008](adr/0008-ioscan-untrusted-input.md) |
| Intent verification | Tier-based change authorization is in the merge-gate path; trajectory-integrated intent-alignment review remains the next slice. | [#2803](https://github.com/kubestellar/hive/issues/2803), [intent verification](intent-verification.md) |
| Review fan-out | Structured review reports and deterministic aggregation are in place; scheduler fan-out to parallel review perspectives is the deferred wiring. | [#2807](https://github.com/kubestellar/hive/issues/2807), [review swarm](review-swarm.md) |
| Credential-free sandbox kick path | Move agent execution toward no live token and no direct network in the sandbox, with trusted host-side post-steps retaining the MITM proxy as an outer layer. | [#2804](https://github.com/kubestellar/hive/issues/2804), [security threat model](security-threat-model.md#known-gaps-and-roadmap) |
| Spoke-based lite enrollment | Keep `hivectl enroll OWNER/REPO` as a zero-secret on-ramp by adding repos to an existing spoke or provisioning a hosted lite spoke; the hub tracks only spokes. | [#2808](https://github.com/kubestellar/hive/issues/2808), [lite enrollment](lite-enrollment.md) |
| Retrospective learning lane | Build from deterministic post-completion advisory beads toward LLM-assisted retro summaries and knowledge extraction. | [#2809](https://github.com/kubestellar/hive/issues/2809), [retro lane](retro-lane.md) |
| ADR back-fill for remaining subsystems | **Done.** Back-filled accepted ADRs capture the knowledge system, skill registry, CEL/channel triggers, and hub/spoke mechanics, so architecture decisions stay auditable. | [ADR-0011](adr/0011-knowledge-system.md), [ADR-0012](adr/0012-skill-registry.md), [ADR-0013](adr/0013-cel-triggers.md), [ADR-0014](adr/0014-hub-spoke.md) |

## Next

| Work | Outcome | Tracking |
| --- | --- | --- |
| Docs site publication | The [docs index](README.md) ships and is maintained; the MkDocs/site pipeline is still deferred — no site config exists in the tree. Its originating issue is closed, so this item currently has no open tracker. | [docs index](README.md), origin: [#2811](https://github.com/kubestellar/hive/issues/2811) (closed) |
| GitLab through `pkg/forge` | `pkg/forge` ships GitHub, GitLab, and Gitea/Forgejo adapters with the read path and core write path implemented and tested; `Merge` is left an explicit interface TODO because merge semantics diverge across forges. What remains is moving scheduler operations behind the abstraction — production callers still use the forge-specific client directly. Its originating epic is closed, so this item currently has no open tracker. | [ADR-0005](adr/0005-forge-abstraction.md), origin: [#2812](https://github.com/kubestellar/hive/issues/2812) (closed) |

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
