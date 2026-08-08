# Hive public roadmap

> **Directional, not a promise.** This roadmap reflects the public v4 direction as
> of **2026-08-07**. It is maintained by pull request and can change as issues,
> security reviews, and operator feedback change the order of work. The umbrella
> tracking issue is [#2812](https://github.com/kubestellar/hive/issues/2812);
> this page is part of [#2811](https://github.com/kubestellar/hive/issues/2811).

Hive's roadmap is organized as **Now / Next / Later**: near-term work already in
flight, likely follow-ups, and longer-horizon bets. Items are listed in planning
order, not priority rank.

## Now

| Work | Outcome | Tracking |
| --- | --- | --- |
| Prompt-injection defense-in-depth | Canary-token checks, output redaction, and fail-closed scanning build on the existing deterministic `ioscan` kick-path redaction. | [#2805](https://github.com/kubestellar/hive/issues/2805), [security threat model](security-threat-model.md), [ADR-0008](adr/0008-ioscan-untrusted-input.md) |
| Intent verification | Tier-based change authorization is in the merge-gate path; trajectory-integrated intent-alignment review remains the next slice. | [#2803](https://github.com/kubestellar/hive/issues/2803), [intent verification](intent-verification.md) |
| Review fan-out | Structured review reports and deterministic aggregation are in place; scheduler fan-out to parallel review perspectives is the deferred wiring. | [#2807](https://github.com/kubestellar/hive/issues/2807), [review swarm](review-swarm.md) |
| Credential-free sandbox kick path | Move agent execution toward no live token and no direct network in the sandbox, with trusted host-side post-steps retaining the MITM proxy as an outer layer. | [#2804](https://github.com/kubestellar/hive/issues/2804), [security threat model](security-threat-model.md#known-gaps-and-roadmap) |
| Spoke-based lite enrollment | Keep `hivectl enroll OWNER/REPO` as a zero-secret on-ramp by adding repos to an existing spoke or provisioning a hosted lite spoke; the hub tracks only spokes. | [#2808](https://github.com/kubestellar/hive/issues/2808), [lite enrollment](lite-enrollment.md) |
| Retrospective learning lane | Build from deterministic post-completion advisory beads toward LLM-assisted retro summaries and knowledge extraction. | [#2809](https://github.com/kubestellar/hive/issues/2809), [retro lane](retro-lane.md) |

## Next

| Work | Outcome | Tracking |
| --- | --- | --- |
| Model-based injection classifier | Add a probabilistic classifier beside deterministic `ioscan`, with fail-closed policy choices for high-risk inputs. | [#2805](https://github.com/kubestellar/hive/issues/2805), [security threat model](security-threat-model.md#known-gaps-and-roadmap) |
| ADR back-fill for remaining subsystems | Capture decisions for the knowledge system, skill registry, CEL/channel triggers, and hub/spoke mechanics so architecture docs stay auditable. | [#2811](https://github.com/kubestellar/hive/issues/2811), [ADR index](adr/README.md), [knowledge design](design/knowledge-system.md), [agent configuration](agent-configuration.md) |
| Docs site publication | Turn `v2/docs/` into a published, navigable documentation site. This PR creates the index; the MkDocs/site pipeline remains deferred. | [#2811](https://github.com/kubestellar/hive/issues/2811), [docs index](README.md) |
| GitLab through `pkg/forge` | Move more GitHub-specific operations behind the forge abstraction and add GitLab support incrementally rather than forking the scheduler. | [ADR-0005](adr/0005-forge-abstraction.md), [#2812](https://github.com/kubestellar/hive/issues/2812) |

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
