# Hive introduction

Hive helps maintain a GitHub project. It watches reports and tasks (issues), proposed code changes (pull requests, or PRs), failed automated build and test checks (CI), known security vulnerabilities (CVEs), and Dependabot alerts. It sends AI coding agents to sort reports, make fixes, review changes, and prepare releases. See [Integrations](integrations.md) for supported agent backends.

For example, Hive can spot a missing test, record the finding on its dashboard, and — if the configured permissions allow it — ask the quality agent to propose a test in a pull request. At lower levels, a person reviews and merges that change. Autonomous merging is optional, not the starting goal.

You decide what agents may do. The **AI Codebase Maturity Model (ACMM)** ties permissions to the project's safeguards: tests, coverage, review practice, and security posture. It is not a checklist for turning every issue into a PR.

## Get started

Choose how to obtain a running hive **before** configuring its agents:

| Your situation | Start here | Expected result |
|---|---|---|
| You want a hosted hive, without running a cluster | [Hosted onboarding](hosted-hub.md); request at [hive.hivecommons.dev/get-started](https://hive.hivecommons.dev/get-started) | After admin approval and provisioning, **My Hives** links to your dashboard. |
| You want to run one hive yourself | [Docker Compose](quickstart-docker-compose.md), [Podman](quickstart-podman.md), or [Kubernetes](kubernetes-deployment.md) | Your own running container and dashboard, with storage and authentication configured. |
| Someone has already given you dashboard access | [Getting started](getting-started.md#step-0--before-you-start-do-this-first) | Connect the GitHub App, authenticate a model backend, set a budget, then enable agents conservatively. |

You need a GitHub identity and a repository on GitHub.com or GitHub Enterprise.
For the App route, have permission to install/authorize the GitHub App for that
repository, or help from someone who can. Hosted requests need admin approval;
requesting a hive does not itself install the App. Self-hosting also requires
the chosen runtime, deployment privileges, and repository credentials; the
Compose guide supports a PAT as well as an App. An agent separately needs its
supported backend login, subscription, or model-gateway credentials.

Once you can open a dashboard, follow [Getting started](getting-started.md) for the first session and its success checks. Operating a fleet is a separate task: see [self-hosted hub deployment](hub-deployment.md).

## How Hive is organized

A terminal agent is a power tool: you hold it, aim it, and it stops when you stop. Hive is the workshop that runs the power tools for you on three rules:

1. **Short-lived agents.** Every task starts a fresh agent with a clean context and ends it.
2. **External context.** What the agent needs to know lives outside the model and is re-injected each time.
3. **Prompts driven by deterministic facts.** Instructions are templates filled from real data, not hand-tuned prose.

### Core concepts

- **[Hive and spoke](architecture.md#8-hub--spoke):** a spoke is the running hive that serves repositories; a hub can register, provision, and observe multiple spokes.
- **[Agents](agent-configuration.md):** AI workers assigned a job, model, schedule (cadence), and permissions.
- **[Governor](architecture.md#3-the-governor-loop--from-queue-depth-to-a-kick):** the scheduler that checks for work and sends an agent a task (a kick).
- **[ACMM levels](acmm-policy-matrix.md):** the **AI Codebase Maturity Model** — six levels that map how mature a codebase's tests, reviews and security posture are to what agents may observe, propose, open, review, or merge ([paper](https://arxiv.org/abs/2604.09388)).
- **[Hub](hosted-hub.md):** the service that registers hives, receives their periodic status reports (heartbeats), and shows the fleet.
- **[Contributors and ClankeR relay](contributor-relay.md):** a way for trusted contributors to donate compute and run delegated work with permissions checked by the hub.

## How it works

1. Connect a repository through the Forge App and configure the project, agents, model backends, and desired ACMM level.
2. The Governor enumerates actionable work and applies deterministic filters before any agent sees a task.
3. Agents receive bounded kicks, work in their configured lanes, and use Hive relays for audited writes such as issues, pull requests, reviews, or merges.
4. Dashboards, logs, fleet health, and hub heartbeats show what happened and what still needs human attention.

## Where to go next

- [Architecture](architecture.md)
- [Integrations](integrations.md) — backends, trackers, forges and sandboxes Hive works with, at a glance.
- [Getting started](getting-started.md)
- [Operator reference](operator-reference.md)
- [GitHub webhooks: cadence, patch and reconcile](webhooks.md) — polling fallback, webhook patch and reconcile, and the state-accuracy guarantees.
- [Dashboard sections explained](dashboard-sections.md) — what each dashboard section shows and what to do about it.
- [Activity monitor copy inventory](dashboard-copy-inventory.md) — every operator-visible string builder in the activity monitor, with before/after word counts and the hover budget.
- [Hive on your iPhone Home Screen](ios-widget.md) — a Home Screen widget with issue/PR counts and sparklines (Scriptable, no server change).
- [Dashboard settings explained](settings.md) — what each Settings tab controls and where each setting is documented.
- [Maintainer commands](maintainer-commands.md) — slash commands for un-parking
  issues, requesting help, confirming fixes, reopening issues, and using label
  or assignment helpers.
- [Release channels](release-channels.md) — what `stable`, `candidate`, and `edge` each mean and what to expect from them.
- [Security model](security-model.md) — agent safety control inventory, threats, enforcement modes, residual gaps and operator audit checklist.
- [Hive trust levels vs Copilot assisted permissions](copilot-permissions-comparison.md) — what GitHub's per-action permission mode (GA 2026-10-26) does, what Hive's levels, lanes, proxy and gates do, and where each covers ground the other does not.
- [Data collection and telemetry](telemetry.md)
- [Monitoring a hive with Prometheus and Grafana](monitoring.md)
- [Securing your hive: a first-time operator's guide](securing-your-hive.md)
- [Community and support](community.md)
- [Documentation map](documentation-map.md)

Current docs target branch `v5`; use the [documentation map](documentation-map.md) for v2 → v4 and v4 → v5 migration pointers.

## Contributor reference: maintaining security documentation

`SECURITY.md`, every `src/docs/security*.md` / `ioscan*.md` page, and `landscape.md` must score Flesch-Kincaid grade 11 or lower (`src/scripts/check-security-docs-readability.sh`). The same pages need a `Last reviewed: YYYY-MM-DD` line in their first 15 lines, no older than 120 days, and must not describe a closed issue as "tracked in", "in progress", or "planned" (`src/scripts/check-security-docs-rot.sh`).
