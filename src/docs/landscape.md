# Hive landscape and positioning

> **Conducted: 2026-08-07. This document will rot.** Agentic software-delivery
> tools are moving quickly; treat product details as time-sensitive and update
> this page by PR when public docs change.

Hive is an operations plane for AI-agent fleets: one operator-controlled system
runs multiple coding/review agents, applies deterministic policy before and
after model judgment, and exposes live dashboard, cost, hub/spoke, and
contributor-compute surfaces. This page positions that design against nearby
agentic orchestration tools.

## Fullsend

Public references: [fullsend.sh](https://fullsend.sh),
[fullsend-ai/fullsend](https://github.com/fullsend-ai/fullsend),
[Fullsend architecture](https://github.com/fullsend-ai/fullsend/blob/main/docs/architecture.md),
[Fullsend roadmap](https://github.com/fullsend-ai/fullsend/blob/main/docs/roadmap.md),
[Fullsend runtimes](https://github.com/fullsend-ai/fullsend/blob/main/docs/runtimes.md),
[Fullsend intent representation](https://github.com/fullsend-ai/fullsend/blob/main/docs/problems/intent-representation.md).

Fullsend is the most directly comparable open-source project. Its public README
positions it as autonomous agentic software development for Git-hosted
organizations, including GitHub, GitLab, and Forgejo. Its docs emphasize a
repo-visible coordination model: target repositories carry `.fullsend/`
configuration, GitHub installations use shim/reusable workflows and OIDC-minted
GitHub App tokens, and GitLab support is being built through native CI triggers
and polling. Its architecture names a vertical execution stack of dispatch,
infrastructure, sandbox, harness, and runtime; production runtime docs currently
list Claude Code as the production runtime and `dummy` for behavior tests, while
future runtimes are tracked separately.

Fullsend is also notably strong in public design discipline: many ADRs, a public
roadmap, security and governance problem documents, and a thoughtful intent
model. Its intent docs discuss git as an intent ledger and tiered authorization,
which directly influenced Hive's catch-up work in [#2812](https://github.com/hivecommons/hive/issues/2812).

### Where Hive differs

| Dimension | Fullsend, fairly summarized | Hive, today or in-flight |
| --- | --- | --- |
| Control plane | Repo-centered install and workflow dispatch, especially strong for GitHub Actions-native adoption. | A live fleet operations plane with governor modes, dashboard/SSE, terminal access, budget tracking, hub/spoke heartbeats, and contributor compute. |
| Execution model | Short-lived, workflow/sandbox-oriented agent runs; production docs currently center Claude Code. | Long-lived tmux-managed agents today, with multiple backends documented in config: Claude, Copilot, Gemini, Goose, and OpenAI-compatible gateways. |
| Autonomy model | Public docs discuss shadow/autonomous and intent-tier concepts. | ACMM L1-L6 maps operator-selected maturity to deterministic per-agent modes and merge authority. |
| Security enforcement | Sandbox, harness, scanner, and OIDC-mint controls are first-class in the docs. | Defense-in-depth combines CLI tool denial, scoped tokens, per-UID attribution, and a runtime-agnostic MITM proxy that enforces GitHub writes at the network boundary. |
| Fleet topology | Per-repo install is the public deployment model. | Hub/spoke registry, callbacks, leaderboard, SaaS/manual provisioning paths, and ClankeR contributor-compute relay are built into the product shape. |

Neither approach is inherently better for every team. Fullsend-style tooling is
lighter when the target is one repo or one GitHub organization, GitHub Actions is
already the trusted execution substrate, and the team wants minimal standing
infrastructure. Hive is a better fit when operators need live fleet visibility,
multiple runtimes, graduated autonomy, hub-managed spokes, contributor compute,
or network-level enforcement independent of agent runtime hooks.

## Single-agent and service-oriented tools

### GitHub Copilot coding agent

GitHub Copilot's coding agent is a hosted, GitHub-native way to assign issues or
PR follow-ups to an agent. It is the lowest-friction option for teams already in
GitHub that want a single background agent without operating their own control
plane. Hive differs by coordinating a fleet of specialized agents, enforcing
ACMM-derived permissions, aggregating fleet cost/status, and supporting
non-Copilot runtimes.

### Devin-class hosted services

Hosted software-engineering agents such as Devin-class services optimize for
outsourcing a task to a capable autonomous worker with a managed environment and
product UX. They can be a better fit when a team wants a vendor-operated agent
and does not want to run orchestration infrastructure. Hive is more appropriate
when the organization needs open-source control, explicit policy, self-hosted
operation, or integration with Kubernetes/hub/spoke workflows.

### SWE-agent and research harnesses

SWE-agent-style projects are excellent for benchmarking, experiments, and
single-task repair loops where the research question is the agent's ability to
solve an issue. Hive is not primarily a benchmark harness; it is an operating
system for repeated project maintenance, policy-bound merge decisions, and
multi-agent fleet operation.

## When to choose what

- Choose **GitHub Copilot coding agent** when you need the quickest hosted path
  for GitHub issues and do not need a separate fleet governor or custom policy
  plane.
- Choose **Fullsend-style tooling** when you have a small number of repos, want
  GitHub Actions or native CI to be the execution substrate, prefer repo-visible
  `.fullsend/` configuration, and want little or no always-on infrastructure.
- Choose **Devin-class services** when managed autonomy and vendor UX matter more
  than self-hosted controls or open implementation details.
- Choose **SWE-agent/research harnesses** when the goal is evaluation,
  reproducible experiments, or one-off issue repair rather than operations.
- Choose **Hive** when the problem is operating a live AI-agent fleet: multiple
  runtimes, ACMM maturity gates, deterministic merge policy, hub/spoke
  provisioning, contributor compute, cost visibility, and network-level MITM
  enforcement.

## Hive claims checked against this repo

- Live fleet operations plane: [architecture](architecture.md#3-the-governor-loop--from-queue-depth-to-a-kick), [dashboard and observability](architecture.md#10-dashboard--observability).
- Multi-runtime model: [architecture](architecture.md#9-model-backends--cost), [agent configuration](agent-configuration.md).
- ACMM autonomy: [architecture](architecture.md#6-acmm--controlling-agent-autonomy), [ACMM matrix](acmm-policy-matrix.md).
- Hub/spoke and contributor compute: [architecture](architecture.md#8-hub--spoke), [manual provisioning](manual-provisioning.md).
- MITM enforcement: [architecture](architecture.md#5-layered-guardrails-defense-in-depth), [ADR-0002](adr/0002-mitm-proxy-network-enforcement.md).
