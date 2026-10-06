# Hive landscape and positioning

> **Conducted: 2026-08-07. This document will rot.** Agentic software-delivery
> tools are moving quickly; treat product details as time-sensitive and update
> this page by PR when public docs change.

Hive is an operations plane for AI-agent fleets: one operator-controlled system
runs multiple coding/review agents, applies deterministic policy before and
after model judgment, and exposes live dashboard, cost, hub/spoke, and
contributor-compute surfaces. This page positions that design against nearby
agentic orchestration tools.

## Agentic AI Foundation (AAIF) and Goose

The Linux Foundation's [Agentic AI Foundation](https://aaif.io/) hosts
[Goose](https://github.com/aaif-goose/goose), originally developed at Block.
Goose is an execution backend; Hive is a downstream orchestration layer that
dispatches to Goose at fleet scale, with operator-controlled policy and
contributor-compute relays. Hive integrates Goose as an independent CNCF
project; Hive does not join AAIF, seek AAIF membership, rely on AAIF to host or
govern Hive, or claim AAIF endorsement or compliance.

This page tracks Goose's upstream move so Hive's backend documentation,
Dockerfiles, and pin-bump tooling follow the correct release source. It does not
request an AAIF landscape listing or describe Hive as part of the foundation's
membership, governance, or project set.

Review material: [OpenSSF Best Practices badge](https://www.bestpractices.dev/projects/14261),
[Apache-2.0 license](../../LICENSE), [security self-assessment](security-self-assessment.md),
and [CNCF reference architecture](cncf-reference-architecture.md).

The [unattended Goose integration guide](../../docs/goose-at-scale.md) is
maintained here for now. Goose's [contribution workflow](https://github.com/aaif-goose/goose/blob/main/CONTRIBUTING.md#from-issue-to-pull-request)
requires a **Ready** issue on its board before any external docs PR, and asks
humans to write new issues themselves. No open Hive issue was found there
when checked for #10627, so no upstream PR has been opened. A human sponsor
must file the documentation proposal and obtain Ready status before the
page can be proposed upstream; the local guide is not upstream approval.

## Fullsend

Public references: [fullsend.sh](https://fullsend.sh),
[fullsend-ai/fullsend](https://github.com/fullsend-ai/fullsend),
[Fullsend architecture](https://github.com/fullsend-ai/fullsend/blob/main/docs/architecture.md),
[Fullsend roadmap](https://github.com/fullsend-ai/fullsend/blob/main/docs/roadmap.md),
[Fullsend runtimes](https://github.com/fullsend-ai/fullsend/blob/main/docs/runtimes.md),
[Fullsend intent representation](https://github.com/fullsend-ai/fullsend/blob/main/docs/problems/intent-representation.md).

Fullsend is a closely comparable open-source project. Its public README
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

## OpenAI Symphony

Reviewed against [OpenAI Symphony](https://github.com/openai/symphony) at
[`be10a1b`](https://github.com/openai/symphony/tree/be10a1b79df723d6d7612b5651c8522704dafb2e).
Public references: [README](https://github.com/openai/symphony/blob/be10a1b79df723d6d7612b5651c8522704dafb2e/README.md),
[Draft v1 SPEC.md](https://github.com/openai/symphony/blob/be10a1b79df723d6d7612b5651c8522704dafb2e/SPEC.md).
Symphony is Apache-2.0 licensed and describes its Elixir reference implementation
as an engineering preview for trusted environments. Its demo monitors Linear,
runs isolated coding agents, and presents proof of work (CI status, review
feedback, complexity analysis, and walkthrough videos) before accepted PRs land.
Those demo artifacts and auto-landing are **not** mandatory spec requirements:
a successful spec run can stop at a human-review handoff.

Hive applies the Symphony operating model—manage work rather than supervise
every coding turn—to GitHub/GitLab-native projects, with deterministic policy
gating in front of the agent. This is a positioning analogy, **not** a claim
that Hive implements Symphony's `WORKFLOW.md` or Codex app-server contracts.

| Dimension | Symphony | Hive |
| --- | --- | --- |
| Work source | Linear in the demo; the current spec defines a provider-neutral tracker adapter. | GitHub/GitLab forge workflows; primary planning adapters include GitHub Issues/Projects, Linear, and Jira. |
| Pre-agent gating | Config preflight, active/terminal states, required labels, adapter dispatchability, claims, and concurrency checks (spec §§6–8). | Deterministic shell enumeration/classification/merge eligibility before a kick, plus ACMM and network-level write enforcement. |
| Agents | Codex app-server is the specified runner protocol. | Multiple CLI backends, including Claude, Copilot, Gemini, Goose, and Codex; confinement depends on backend and deployment. |
| Scale model | Per-issue persistent workspace, bounded concurrent runs, reconciliation, continuation, and retry. | Queue-depth-driven cadence, long-lived agents, isolated execution paths, hub/spoke, and convergence audits. |
| Packaging | Language-neutral draft specification and experimental Elixir reference implementation. | Go runtime and supporting scripts, Compose/Quadlet deployment, dashboard, and contributor relay. |

Choose Symphony when a repo-owned `WORKFLOW.md`, the Codex app-server contract,
and per-ticket workspace lifecycle are the desired integration surface. Choose
Hive when fleet operations, multiple runtimes, forge-native review/merge policy,
and graduated autonomy are central. Neither tool's workspace isolation alone
constitutes a sandbox.

See the [section-by-section alignment review](../../docs/design/symphony-spec-alignment.md)
for gaps and intentional divergences, the existing
[Linear work source](work-sources.md), and
[work-source provider contract](integrations/work-source-providers.md).

## GitHub Agentic Workflows (gh-aw)

Public references: [github/gh-aw](https://github.com/github/gh-aw),
[gh-aw documentation](https://github.github.com/gh-aw/), and the
[githubnext/agentics sample gallery](https://github.com/githubnext/agentics).

GitHub Agentic Workflows compiles Markdown-authored agent instructions and
frontmatter into GitHub Actions workflows. It is an Actions-native on-ramp,
not a replacement for Hive's fleet scheduler. Its engines include Copilot,
Claude, Codex, and Gemini; shared engine names do not imply shared credentials,
confinement, or Hive backend-tier acceptance.

| Dimension | gh-aw | Hive |
| --- | --- | --- |
| Execution substrate | Event, dispatch, and scheduled GitHub Actions runs | Long-running fleet with queue-depth cadence and convergence audits |
| Authoring | Markdown prompt plus workflow frontmatter compiled to Actions | Project config, deterministic pipeline, and agent policies |
| Judgment and policy | Engine judgment with declared tools, permissions, and safe outputs | Deterministic filtering/classification before judgment and gated merge authority afterward |
| Operations | Per-workflow Actions logs and artifacts | Live dashboard, budgets, hub/spoke, and contributor compute |
| Best starting point | A repo already using Actions that wants bounded agentic jobs | Operators coordinating continuous work across agents and repositories |

Our [Hive workflow sample and installation guide](../deploy/gh-aw/README.md)
provides manually dispatched **report-only issue triage**. A deterministic
pre-agent admission step filters held/blocked issues, then runs Hive's existing
classifier before the configured engine produces an advisory report. It does
not give the engine merge authority or reproduce the whole production pipeline.
Keep existing gh-aw workflows when adopting Hive: add the relay/fleet for the
queues and stages requiring continuous operation, rather than rewriting those
workflows or letting both systems claim the same tasks.

The inverse path (Hive dispatching gh-aw as an external host through
`pkg/extwork`) is **not implemented**. A future adapter is bounded to report-only
and shadow modes, with the same credential, capability, and verified-receipt
admission bar as external OMP; see
[backend support tiers](backend-support-tiers.md#github-agentic-workflows-on-ramp-not-a-cli-backend).

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

- Choose **gh-aw** when you want Markdown-authored, bounded agentic Actions
  jobs in an existing repository without running a standing fleet.
- Choose **GitHub Copilot coding agent** when you need the quickest hosted path
  for GitHub issues and do not need a separate fleet governor or custom policy
  plane.
- Choose **Symphony** when you want a spec-first, repo-owned `WORKFLOW.md`
  and Codex app-server orchestration with per-ticket persistent workspaces.
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
