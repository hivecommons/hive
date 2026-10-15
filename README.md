<p align="center">
  <img src="docs/hive-bee.png" alt="Hive" width="120" height="120">
</p>

# Hive

[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/14261/badge)](https://www.bestpractices.dev/projects/14261)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2Fhivecommons%2Fhive.svg?type=large&issueType=license)](https://app.fossa.com/projects/git%2Bgithub.com%2Fhivecommons%2Fhive?ref=badge_large&issueType=license)

Hive is a co-maintainer for your GitHub project that never sleeps. Point it at a repo, set the limits, and let short-lived AI agents handle the maintenance chores while humans keep the decisions that matter.

## What Hive is

Hive watches the work that piles up in a repository — issues, pull requests, failing CI, CVEs, and dependabot alerts — and sends AI coding agents (Claude, Copilot, Gemini, Goose…) to deal with it: triage, fixes, reviews, releases. You decide how much it is allowed to do. Humans keep every decision that matters; agents do the chores.

## "I already use an agent. How is this different?"

A terminal agent is a power tool: you hold it, aim it, and it stops when you stop. Hive is the workshop that runs the power tools for you — on three rules the industry is only now discovering:

1. **Short-lived agents.** Every task starts a fresh agent with a clean context and ends it. No "long-running" agent drifting for hours.
2. **External context.** What the agent needs to know lives outside the model — in your repo, your knowledge base, your audit trail — and is re-injected each time, so nothing is forgotten and nothing is hallucinated from stale memory.
3. **Prompts driven by deterministic facts.** The instructions an agent gets are templates filled from real data (the issue, the CI log, the diff, the trust level), not hand-tuned prose.

Around that, Hive picks work from your real queue, sets limits in code with six trust levels from the **AI Codebase Maturity Model (ACMM)** — a framework for how much autonomy a codebase is ready to give agents ([levels](#acmm-levels) · [paper](https://arxiv.org/abs/2604.09388)), checks before and after with deterministic filters and gates, writes every action to an audit log owned by a named human, and runs around the clock. Same agents you use today — governed, scheduled, scoped, and accountable.

## Hub and spoke

- **Spoke** — one running Hive, serving your repo(s). It is where agents actually work. Runs on your Kubernetes cluster, Podman, or Docker Compose.
- **Hub** — the control plane for many spokes: registers, provisions, upgrades and watches them; fleet view, leaderboards, routes contributor compute to the hives that need it.

A spoke works alone. A hub is how you see and manage a fleet of them.

## Hive the project vs the hosted service

- **Hive the project**: Apache-2.0 code in this repo. Run it yourself.
- **Hive the hosted service**: [hive.hivecommons.dev](https://hive.hivecommons.dev) — a hub we operate so you can onboard a spoke without a cluster. Same software, one deployment of it.

## Does Hive fit my project?

**Do I need a paid agent subscription?** No. Hive drives the agent CLIs you already have (Claude, Copilot, Gemini, Goose) *or* any OpenAI-compatible model through its Model Gateway (OpenRouter key, no CLI login). Contributors can bring their own.

**Who tells it what to work on?** Your backlog does. The Governor reads your issues/PRs/CI, filters them deterministically (labels, areas, complexity), and only hands agents work you have allowed. You choose the areas and the trust level. Nothing is invented.

**We require design discussion before non-trivial PRs.** That is a trust level, not a workaround: *Measured* mode = agents may only file findings and proposals as issues — no code, no PRs — so the discussion happens first; *Holdgated* = PRs are opened with a `hold` label and a human must review and release; *Full* (L6) = merge on green. Start at Measured. You never have to move up.

**Can it open PRs to our upstream repo?** Yes, if you install it there. Or run it on a fork (`upstream_watch` keeps the fork current) and push reviewed PRs upstream yourself.

More answers: https://docs.hivecommons.dev/docs/community/what-is-hive-commons#does-hive-fit-my-project

## Run it

| Path | Start here |
| --- | --- |
| Docker Compose | [Quick Start (Docker Compose)](src/docs/quickstart-docker-compose.md) |
| Podman | [Quick Start (Podman)](src/docs/quickstart-podman.md) |
| Kubernetes | [Kubernetes deployment](src/docs/kubernetes-deployment.md) |
| Hosted hub | [https://hive.hivecommons.dev](https://hive.hivecommons.dev) and the [hosted hub guide](src/docs/hosted-hub.md) |
| Getting started | [Zero to Automation: Getting Started with Hive](src/docs/getting-started.md) |

Configuration lives in `hive.yaml`; see the [operator reference](src/docs/operator-reference.md), [environment variable reference](src/docs/env-vars.md), and [agent configuration](src/docs/agent-configuration.md).

## ACMM Levels

Hive uses an **AI Codebase Maturity Model** (ACMM) with six levels that control what agents are allowed to do:

| Level | Name | What agents can do |
|-------|------|-------------------|
| L1 | Inception (Assisted) | Interactive advisor and project inception. Advisory beads only. |
| L2 | Advisory (Instructed) | Observe and report findings as dashboard beads. No GitHub interaction. |
| L3 | Quality-Gated (Measured) | Quality agent opens issues and PRs gated by the literal `hold` level-gate label. Others remain advisory. |
| L4 | Security-Aware (Adaptive) | All agents file issues. Quality, sec-check, and CI open PRs gated by `hold`. |
| L5 | Semi-Autonomous (Semi-Automated) | All agents open PRs gated by `hold`. Humans batch-review and approve. |
| L6 | Fully Autonomous | Agents open PRs and auto-merge when checks pass. Non-outreach PRs have no level hold; outreach PRs remain held. |

See [src/docs/acmm-policy-matrix.md](src/docs/acmm-policy-matrix.md) for the full matrix and [src/docs/labels-and-control-signals.md](src/docs/labels-and-control-signals.md) for label behavior.

## Architecture

Hive runs as a single container with a Go control plane, an authenticated Node.js gateway, and ttyd for web terminal access. The Governor evaluates queue depth, deterministic filters decide what an agent may see, scoped tokens and a MITM proxy enforce what it may do, and gates decide what may land.

See [src/docs/architecture.md](src/docs/architecture.md) for process model, governor loop, guardrails, hub/spoke, and walkthrough diagrams.

## Terminal dashboard

`hivectl tui` is a full-screen terminal view of the same dashboard API the web UI uses: agents, governor, token spend, live activity, pause/resume, kick, ACMM level, and hive-profile actions.

See [`hivectl tui` in the command reference](src/docs/hivectl.md#tui--live-terminal-dashboard).

## Contribute to a Hive

Community members can contribute compute to any hive through **ClankeR**, the contributor relay: it hands tasks from a hive's backlog to the CLI agent running on your own machine, under the hive's prompts and limits.

Start with [src/docs/contributor-relay.md](src/docs/contributor-relay.md) or the [Hive Hub contribute page](https://hive.hivecommons.dev).

## Moved sections

## Quick Start

Moved to [Quick Start (Docker Compose)](src/docs/quickstart-docker-compose.md), [Quick Start (Podman)](src/docs/quickstart-podman.md), and [Kubernetes deployment](src/docs/kubernetes-deployment.md).

## Quick Start (Docker Compose)

Moved to [src/docs/quickstart-docker-compose.md](src/docs/quickstart-docker-compose.md).

## Quick Start (Podman)

Moved to [src/docs/quickstart-podman.md](src/docs/quickstart-podman.md).

## Kubernetes Deployment

Moved to [src/docs/kubernetes-deployment.md](src/docs/kubernetes-deployment.md).

## Related projects

- [Goose](https://github.com/aaif-goose/goose), an agent backend whose upstream moved to the [Agentic AI Foundation (AAIF)](https://aaif.io/). Hive integrates Goose as an independent CNCF project; Hive does not join AAIF, seek AAIF membership, or rely on AAIF to host or govern Hive. See [unattended Goose with Hive](docs/goose-at-scale.md) and [Goose upstream tracking](src/docs/landscape.md#agentic-ai-foundation-aaif-and-goose).

## Repositories

Hive is one of several repositories in the hivecommons organization. See [docs/repositories.md](docs/repositories.md) for the full list, what each one is for, and which ones ship inside a Hive release.

## Contributing

See the [Hive Hub](https://hive.hivecommons.dev) to browse registered hives, view leaderboards, and find hives accepting contributions.

To contribute to Hive itself, see [CONTRIBUTING.md](CONTRIBUTING.md) and open issues or PRs on this repository. Contributors are expected to follow the [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md), adopted in [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

Recent user-visible changes are recorded in [CHANGELOG.md](CHANGELOG.md).

## Security

Please see [SECURITY.md](SECURITY.md) for the vulnerability disclosure process. Data collection and telemetry paths are documented in [src/docs/telemetry.md](src/docs/telemetry.md). Do not report security vulnerabilities through public issues or pull requests.

---

Apache 2.0

Hive is a [Cloud Native Computing Foundation](https://www.cncf.io/) sandbox project candidate; onboarding status is tracked in [src/docs/cncf-sandbox-onboarding.md](src/docs/cncf-sandbox-onboarding.md). This does not imply CNCF acceptance or endorsement.
Copyright The Linux Foundation and Hive contributors. The Linux Foundation has registered trademarks and uses trademarks. For a list of trademarks of The Linux Foundation, please see the [Linux Foundation Trademark Usage page](https://www.linuxfoundation.org/legal/trademark-usage). Hive project names and logos are used in accordance with that policy once ownership is transferred to the Linux Foundation/CNCF.
