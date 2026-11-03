# What Hive is

This page holds the project overview that used to live in the repository
README: what Hive does, how it differs from a terminal agent, the hub/spoke
model, the contributor relay, the ACMM trust levels, and the questions people
ask before adopting it. The README itself is kept short and carries the
measured outcomes; everything else is here or linked from here.

## Overview

Hive watches the work that piles up in a repository — issues, pull requests,
failing CI, CVEs, and dependabot alerts — and sends AI coding agents (Claude,
Copilot, Codex, Antigravity, Goose…) to deal with it: triage, fixes, reviews,
releases. You decide how much it is allowed to do. Humans keep every decision
that matters; agents do the chores.

## "I already use an agent. How is this different?"

A terminal agent is a tool you hold, aim, and stop. Hive runs those tools for
you, on three rules:

1. **Short-lived agents.** Every task starts a fresh agent with a clean context
   and ends it. No long-running agent drifting for hours.
2. **External context.** What the agent needs to know lives outside the model —
   in your repo, your knowledge base, your audit trail — and is re-injected
   each time, so nothing is forgotten and nothing is reconstructed from stale
   memory.
3. **Prompts driven by deterministic facts.** The instructions an agent gets
   are templates filled from real data (the issue, the CI log, the diff, the
   trust level), not hand-tuned prose.

Around that, Hive picks work from your real queue, sets limits in code with six
trust levels from the **AI Codebase Maturity Model (ACMM)** — a framework for
how much autonomy a codebase is ready to give agents ([levels](#acmm-levels) ·
[paper](https://arxiv.org/abs/2604.09388)), checks before and after with
deterministic filters and gates, writes every action to an audit log owned by
a named human, and runs around the clock. Same agents you use today —
governed, scheduled, scoped, and accountable.

## Hub and spoke

- **Spoke** — one running Hive, serving your repo(s). It is where agents
  actually work. Runs on your Kubernetes cluster, Podman, or Docker Compose.
- **Hub** — the control plane for many spokes: registers, provisions, upgrades
  and watches them; fleet view, leaderboards, routes contributor compute to the
  hives that need it.

A spoke works alone. A hub is how you see and manage a fleet of them.

## Contributor relay: a swarm working on your behalf

Your own agents are only the start. The **contributor relay** (ClankeR) lets
anyone who wants to help your project donate their compute and model tokens
without you handing out anything:

- **They bring their own agent.** A contributor runs one command on their
  laptop, cluster, or Compose box; it connects outbound to your hive, takes
  one curated task at a time, and runs *their* CLI with *their* model and
  *their* subscription.
- **Their secrets never reach you.** API keys and CLI logins stay on the
  contributor's machine. The hive never sees them.
- **Your forge settings never change.** Nobody is added as a collaborator. For
  each task the hive mints a short-lived, task-scoped GitHub token so the PR
  can be opened, then it expires. Revoke a contributor and their access is
  gone.
- **Your prompts, your limits.** Contributors work the queue you curated,
  under the same trust level, filters and gates as your resident agents.

The result is an elastic swarm: a three-person project can have a dozen
contributors' agents working its backlog overnight, each accountable to a
named human, while the maintainers hold every merge decision. See
[contributor-relay.md](contributor-relay.md).

### Contribute to a Hive

<a id="contribute-to-a-hive"></a>

Community members can contribute compute to any hive through ClankeR: it hands
tasks from a hive's backlog to the CLI agent running on your own machine, under
the hive's prompts and limits, with no repo permissions and no shared secrets.

Start with [contributor-relay.md](contributor-relay.md) or the
[Hive Hub contribute page](https://hive.hivecommons.dev).

Homebrew is an additional install option:

```bash
brew install hivecommons/hive/contribute
```

Follow the [Homebrew setup instructions](contributor-relay.md#homebrew) to
register without a checkout before running `hive-contribute`. The
[`just` workflow](contributor-relay.md#from-source) remains the from-source
path.

## Hive the project vs the hosted service

- **Hive the project**: Apache-2.0 code in this repo. Run it yourself.
- **Hive the hosted service**: [hive.hivecommons.dev](https://hive.hivecommons.dev)
  — a hub we operate so you can onboard a spoke without a cluster. Same
  software, one deployment of it. See [hosted-hub.md](hosted-hub.md).

## Does Hive fit my project?

**Do I need a paid agent subscription?** No. Hive drives the agent CLIs you
already have (Claude, Copilot, Codex, Antigravity, Goose) *or* any
OpenAI-compatible model through its Model Gateway (OpenRouter key, no CLI
login). Contributors can bring their own.

**Who tells it what to work on?** Your backlog does. The Governor reads your
issues/PRs/CI, filters them deterministically (labels, areas, complexity), and
only hands agents work you have allowed. You choose the areas and the trust
level. Nothing is invented.

**We require design discussion before non-trivial PRs.** That is a trust
level, not a workaround: *Measured* mode = agents may only file findings and
proposals as issues — no code, no PRs — so the discussion happens first;
*Holdgated* = PRs are opened with a `hold` label and a human must review and
release; *Full* (L6) = merge on green. Start at Measured. You never have to
move up.

**Can it open PRs to our upstream repo?** Yes, if you install it there. Or run
it on a fork (`upstream_watch` keeps the fork current) and push reviewed PRs
upstream yourself.

More answers:
<https://docs.hivecommons.dev/docs/community/what-is-hive-commons#does-hive-fit-my-project>

## Run it

<a id="quick-start"></a>

| Path | Start here |
| --- | --- |
| Docker Compose | [Quick Start (Docker Compose)](quickstart-docker-compose.md) |
| Podman | [Quick Start (Podman)](quickstart-podman.md) |
| Kubernetes | [Kubernetes deployment](kubernetes-deployment.md) |
| Hosted hub | [https://hive.hivecommons.dev](https://hive.hivecommons.dev) and the [hosted hub guide](hosted-hub.md) |
| Getting started | [Zero to Automation: Getting Started with Hive](getting-started.md) |

Configuration lives in `hive.yaml`; see the
[operator reference](operator-reference.md),
[environment variable reference](env-vars.md), and
[agent configuration](agent-configuration.md).

## ACMM Levels

Hive uses an **AI Codebase Maturity Model** (ACMM) with six levels that control
what agents are allowed to do:

| Level | Name | What agents can do |
|-------|------|-------------------|
| L1 | Inception (Assisted) | Interactive advisor and project inception. Advisory beads only. |
| L2 | Advisory (Instructed) | Observe and report findings as dashboard beads. No GitHub interaction. |
| L3 | Quality-Gated (Measured) | Quality agent opens issues and PRs gated by the literal `hold` level-gate label. Others remain advisory. |
| L4 | Security-Aware (Adaptive) | All agents file issues. Quality, sec-check, and CI open PRs gated by `hold`. |
| L5 | Semi-Autonomous (Semi-Automated) | All agents open PRs gated by `hold`. Humans batch-review and approve. |
| L6 | Autonomous | Agents open PRs and auto-merge when checks pass. Non-outreach PRs have no level hold; outreach PRs remain held. |

See [acmm-policy-matrix.md](acmm-policy-matrix.md) for the full matrix and
[labels-and-control-signals.md](labels-and-control-signals.md) for label
behavior.

## Architecture

Hive runs as a single container with a Go control plane, an authenticated
Node.js gateway, and ttyd for web terminal access. The Governor evaluates
queue depth, deterministic filters decide what an agent may see, scoped tokens
and a MITM proxy enforce what it may do, and gates decide what may land.

See [architecture.md](architecture.md) for process model, governor loop,
guardrails, hub/spoke, and walkthrough diagrams.

## Terminal dashboard

`hivectl tui` is a full-screen terminal view of the same dashboard API the web
UI uses: agents, governor, token spend, live activity, pause/resume, kick,
ACMM level, and hive-profile actions. See
[`hivectl tui` in the command reference](hivectl.md#tui--live-terminal-dashboard).

## Works with

Hive drives Claude Code, GitHub Copilot CLI, Codex, Goose, Antigravity and
OpenAI-compatible gateways; reads work from GitHub Issues, GitHub Projects,
Linear and Jira; and runs on GitHub today with GitLab and Gitea/Forgejo
planned. See the [integrations page](integrations.md) for the full logo grid
with status.

## Related projects

- [Goose](https://github.com/aaif-goose/goose), an agent backend whose
  upstream moved to the [Agentic AI Foundation (AAIF)](https://aaif.io/). Hive
  integrates Goose as an independent CNCF project; Hive does not join AAIF,
  seek AAIF membership, or rely on AAIF to host or govern Hive. See
  [unattended Goose with Hive](../../docs/goose-at-scale.md) and
  [Goose upstream tracking](landscape.md#agentic-ai-foundation-aaif-and-goose).

## Repositories, adopters, contributing, security

- [Repositories](../../docs/repositories.md) — the other repositories in the
  hivecommons organization, what each is for, and which ship inside a release.
- [ADOPTERS.md](../../ADOPTERS.md) — organizations running Hive. Add yours
  with a pull request.
- [CONTRIBUTING.md](../../CONTRIBUTING.md) — contributing to Hive itself.
  Contributors follow the
  [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md),
  adopted in [CODE_OF_CONDUCT.md](../../CODE_OF_CONDUCT.md). Recent changes
  are in [CHANGELOG.md](../../CHANGELOG.md).
- [Security model](security-model.md) — code-grounded inventory of agent
  safety controls, enforcing versus advisory modes, known gaps, and audit
  steps. Vulnerability disclosure: [SECURITY.md](../../SECURITY.md); telemetry
  and data collection: [telemetry.md](telemetry.md). Do not report security
  vulnerabilities through public issues or pull requests.
