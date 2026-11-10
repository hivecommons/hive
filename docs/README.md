# docs/

This directory holds contributor guides, design records and some older pages.
The current documentation lives in [`src/docs/`](../src/docs/); its full
catalogue is the [documentation map](../src/docs/documentation-map.md). Start
there unless you are looking for one of the pages below.

## Start here

- [Getting started as a first-time contributor](getting-started-contributing.md) — the end-to-end path for a first code or documentation contribution.
- [Local development](development.md) — prerequisites, build and test loop for the Go codebase.
- [Hive Commons repositories](repositories.md) — the sibling repositories developed alongside Hive.

## Running and configuring agents

- [Agent backend setup](backend-setup.md) — setup notes for the agent CLI backends.
- [Inference backends](inference-backends.md) — routing agents through OpenAI-compatible model gateways.
- [Running Goose unattended at scale with Hive](goose-at-scale.md)
- [Hive Hub — Backup & Disaster Recovery](HUB_DISASTER_RECOVERY.md)
- [Runs](runs.md) — how a run is represented as a staged task lease.
- [Readiness-first preparation](readiness-preparation.md)
- [Weekly competitor report grounding](weekly-competitor-report.md)
- [Outreach anti-spam and deduplication ruleset](outreach-antispam.md)

## Design records and RFCs

Proposals and designs here are not commitments; check each page for its status.

- [Hive Federation](federation-design.md)
- [RFC #4002: Re-entrant conversation-as-state turn model](rfc-4002-reentrant-conversation-state.md) and its [phased delivery roadmap](rfc-4002-phased-roadmap.md)
- [RFC #5691: Constellation hubs and coordinated spokes](rfc-5691-constellation.md)
- [RFC #5698: Backend capacity, model inventory, pacing, and placement](rfc-5698-backend-capacity-model-inventory-placement.md)
- [RFC #6825: Capability-aware contributor task assignment](rfc-6825-capability-aware-contributor-task-assignment.md)
- [RFC #17054: LLM-ranked knowledge advisor](rfc-17054-llm-ranked-knowledge-advisor.md)
- [Contribute-app distribution roadmap](contribute-distribution-roadmap.md)
- [Proposed upstream issue: Goose-at-scale guide](goose-at-scale-upstream-proposal.md)
- [OpenAI Symphony specification alignment review](design/symphony-spec-alignment.md)
- [Hive: Hub & Spoke presentation](presentations/hive-hub-and-spoke.html) (HTML slides)

## Moved or retired

These pages are kept so existing links keep working. Do not use them for the
current deployment.

- [Architecture](architecture.md), [Hive landscape and positioning](landscape.md) and [Advisory digest staleness](advisory-staleness.md) — redirects to their `src/docs/` pages.
- [Legacy v1 architecture](LEGACY-v1-architecture.md), [macOS support (launchd)](macos.md) and [Troubleshooting](troubleshooting.md) — retired v1 documentation.
- [Migrating from Hive v1 to v2](migration-v1-v2.md) — historical; both ends of the migration are retired.

## Other

- [`readme/`](readme/README.md) — graphics and chart data used by the root README.
