# Hive introduction

Hive is a governed fleet of AI coding agents for maintaining real GitHub and forge repositories. A hive helps with triage, fixes, reviews, release operations, and operational follow-up by watching the work a project already has — issues, pull requests, review queues, and health signals — then routing appropriate tasks to agents while deterministic guardrails handle filtering, permissions, merge eligibility, audit trails, and policy. Maintainers set the autonomy boundary through the Governor and ACMM levels, so Hive can range from advisory triage to carefully gated fixes and reviews without turning repository control over to an unconstrained model.

Hive is for maintainers of busy open source projects who need help keeping queues moving, and for platform teams that want a repeatable operations plane for agent work across many repositories. It is especially useful when the hard part is not invoking one agent, but coordinating many agents with clear ownership, safety checks, and a path for human oversight.

## Core concepts

- **[Hive and spoke](architecture.md#8-hub--spoke):** a spoke is the running hive that serves repositories; a hub can register, provision, and observe multiple spokes.
- **[Agents](agent-configuration.md):** configured AI workers with scopes, models, cadences, and permissions for specific lanes of work.
- **[Governor](architecture.md#3-the-governor-loop--from-queue-depth-to-a-kick):** the scheduler and policy loop that decides what work is actionable and when agents should be kicked.
- **[ACMM levels](acmm-policy-matrix.md):** the autonomy model that maps project maturity to what agents may observe, propose, open, review, or merge.
- **[Hub](hosted-hub.md):** the hosted or self-hosted control plane for onboarding hives, heartbeats, fleet views, and hosted-spoke lifecycle.
- **[Contributors and ClankeR relay](contributor-relay.md):** a way for trusted contributors to donate compute and run delegated work through hub-mediated roles.

## How it works

1. Connect a repository through the Forge App and configure the project, agents, model backends, and desired ACMM level.
2. The Governor enumerates actionable work and applies deterministic filters before any agent sees a task.
3. Agents receive bounded kicks, work in their configured lanes, and use Hive relays for audited writes such as issues, pull requests, reviews, or merges.
4. Dashboards, logs, fleet health, and hub heartbeats show what happened and what still needs human attention.

## Ways to run Hive

- **Hosted Hive Hub:** start at [`https://hive.hivecommons.dev`](https://hive.hivecommons.dev) and follow the [hosted onboarding guide](hosted-hub.md) for the no-cluster path.
- **Self-hosted:** run your own spoke on [Kubernetes](../../README.md#kubernetes-deployment) or [Podman](../../README.md#quick-start-podman), or operate a [self-hosted hub](hub-deployment.md) for a fleet.

## Get started

Start with [Zero to Automation: Getting Started with Hive](getting-started.md). If you want the hosted path, read [Hosted Hive Hub onboarding](hosted-hub.md) next.

## Where to go next

- [Architecture](architecture.md)
- [Getting started](getting-started.md)
- [Operator reference](operator-reference.md)
- [Security model](security-model.md)
- [Documentation map](documentation-map.md)

Current docs target branch `v5`; use the [documentation map](documentation-map.md) for v2 → v4 and v4 → v5 migration pointers.
