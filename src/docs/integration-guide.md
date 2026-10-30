# Integration guide

Use this guide to choose a connection to Hive v5. Configuring an existing integration is different from implementing a new one.

| Your task | Start here | Current boundary |
| --- | --- | --- |
| Read tasks from a planning service | [Work source providers](integrations/work-source-providers.md), then its configuration examples | GitHub Issues, GitHub Projects, Linear, and Jira are primary providers. Implementing another primary provider requires Go changes in Hive. |
| Lend your machine's AI coding tool | [Contributor relay: basic setup](contributor-relay.md#basic-setup) | The relay receives work from Hive and reports its result. |
| Have an external workflow engine run work | [ClankeR and Flue-style execution](integrations/clanker-flue.md) | Hive still decides what may run and verifies reported evidence; Flue dispatch is report-only. |
| Turn a specification into staged work | [Spektacular integration](integrations/spektacular.md) | The planning tool must implement the documented Spektacular CLI contract; there is no general planning-engine registry. |

In the diagram, the **work source** supplies tasks, the **governor** schedules agents, and a **run lease** records who owns a stage and when it expires. An *additive* source appends tasks alongside the selected primary source rather than replacing it.

```mermaid
flowchart LR
    WorkSource[Work source provider\nread tasks from a service] --> Governor[Governor queue\nschedule allowed work]
    Governor --> Agents[Hive agents and contributor relay]
    Clanker[ClankeR contributor relay\n/api/contribute/ws] --> Agents
    Flue[Flue external workflow\nextwork adapter] --> Clanker
    Crustify[Crustify / Wavefront\nmigration graph] --> WorkSource
    Spek[Spektacular CLI\nspec/plan status + plan export] --> Runs[Long-running run leases]
    Runs --> WorkSource
    Runs --> Governor
```

Two external projects shaped these surfaces and remain the reference integrations: [Flue](https://github.com/withastro/flue), the report-only external-execution pilot behind the `pkg/extwork` contract, and [Crustify](https://github.com/crustify-rs/crustify), the C/C++-to-Rust migration harness whose Wavefront migration graph is consumed as an additive work source (see [work sources](work-sources.md) and the `wavefront-smoke.yml` canary).

## Extension surfaces in v5

| Surface | What you can do today | Start here |
| --- | --- | --- |
| Work sources | Add or configure an adapter that turns source-native items into `worksource.Issue` values. The only primary adapters linked today are GitHub Issues, GitHub Projects, Linear, and Jira; run stages and the [Crustify](https://github.com/crustify-rs/crustify) Wavefront migration graph are additive sources. | [Work source providers](integrations/work-source-providers.md) |
| ClankeR + Flue-style external execution | Use the contributor relay as the transport and the `pkg/extwork` contract as the engine-neutral admission/observation seam. Flue is the reference HTTP adapter. | [ClankeR and Flue-style external execution](integrations/clanker-flue.md) |
| Spektacular | Let Hive poll a Spektacular-compatible CLI for `spec`/`plan` status and import final plan tasks into Hive's run flow. | [Spektacular and Project Inception](integrations/spektacular.md) |
| vibe-kanban | Mirror one repository's admitted hive queue onto a local [vibe-kanban](https://github.com/BloopAI/vibe-kanban) board through its MCP server, optionally record board pick-ups as shadow executions, or opt in to the `pkg/extwork/vibekanban` host adapter for report-only workspace dispatch. Default off. | [vibe-kanban mirror and dispatch](vibe-kanban.md) |

Related surfaces that are not redefined here: [agent configuration](agent-configuration.md), [CLI/backend setup](../../docs/backend-setup.md), [MCP write policy](security-model.md), [hub API](api-reference.md), [contributor relay](contributor-relay.md), [work sources](work-sources.md), [long-running runs](runs.md), and [Spektacular runner](spektacular.md).

## Terminology

Use source-neutral words in generic integration docs: **work source**, **project**, **item**, and **change request**. Keep product names only when talking about a specific adapter, such as GitHub Projects or Jira. The glossary and guard-test intent live in [Work-source terminology](work-source-terminology.md).

For implementers, *source-native* means retaining a service's own item identifier; an *admission/observation seam* is the code boundary for allowing work and checking its progress. A *canary* is a small verification workflow, not another installation requirement.

## Gaps tracked from this guide

- Work source adapters are compile-time Go integrations, not external plugins: [#10174](https://github.com/hivecommons/hive/issues/10174).
- Project Inception only wires a Spektacular-compatible CLI boundary; there is no generic named planning-engine registry: [#10175](https://github.com/hivecommons/hive/issues/10175).

Documentation maintenance: edit canonical Hive guides in this repository's `src/docs/` on `v5`. The `hivecommons/docs` Next.js/Nextra site mirrors that tree; its README describes syncing. This source relationship is not an integration prerequisite.
