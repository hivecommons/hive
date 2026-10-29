# Hive landscape and positioning

Last reviewed: 2026-10-28

> **Conducted: 2026-10-23. This document will rot.** Tools for AI coding
> agents change fast. Treat every product detail here as time-sensitive and
> update it by pull request when public docs change.

Hive helps an operator run a group of coding and review agents. For example,
the operator can check costs, pause a worker, review its proposed fix, and
decide which checks must pass before it merges. This live management layer is
the **operations plane**. A **fleet** is the group being managed; a **hub**
registers and observes running hives (**spokes**).

Each comparison below names the public page checked on the conducted date
above. This is evaluation reference, not a setup guide.

## Choose a starting point

- For one bounded job in a repository already using GitHub Actions, a workflow-based tool may be simpler.
- For a vendor-run agent with little infrastructure to operate, consider a hosted service.
- For ongoing workers, shared budgets, varied agent backends, and operator-controlled permissions, consider Hive. Start with [hosted or self-hosted access](README.md#get-started).
- Use [When to choose what](#when-to-choose-what) for the full decision list, then read the comparisons you need.

### Terms used in the comparisons

**Control plane** means the component that configures and coordinates work.
A **harness** launches and supervises an agent; a **sandbox** limits its access.
**OIDC** is OpenID Connect, used here to prove a workflow's identity without a
stored long-lived secret. A **shim workflow** is a small workflow that calls
another one. **Queue-depth cadence** changes run frequency with the amount of
pending work. **Forge-neutral adapters** translate common operations for
different code hosts; an adapter alone does not mean a full deployment is
supported on that host.

## Agentic AI Foundation (AAIF) and Goose

The Linux Foundation's [Agentic AI Foundation](https://aaif.io/) hosts
[Goose](https://github.com/aaif-goose/goose), first built at Block. Goose's
README now describes it as a general-purpose agent (desktop app, CLI and API)
that works with 15 or more model providers. Goose runs tasks; Hive sits above
it and dispatches work to Goose at fleet scale. Hive is an independent CNCF project. It is not an AAIF
member, is not governed by AAIF, and claims no AAIF endorsement.

Review material: [OpenSSF Best Practices badge](https://www.bestpractices.dev/projects/14261),
[Apache-2.0 license](../../LICENSE), [security self-assessment](security-self-assessment.md)
and [CNCF reference architecture](cncf-reference-architecture.md).

<details>
<summary>Contribution-process history: why the unattended Goose guide is local</summary>

The [unattended Goose guide](../../docs/goose-at-scale.md) lives here for now.
Goose's [contribution workflow](https://github.com/aaif-goose/goose/blob/main/CONTRIBUTING.md#from-issue-to-pull-request)
still requires a human-written **Ready** issue before any outside docs PR.
None is open, so the guide is not upstream.

</details>

## Fullsend

Checked: [fullsend.sh](https://fullsend.sh),
[README](https://github.com/fullsend-ai/fullsend),
[architecture](https://github.com/fullsend-ai/fullsend/blob/main/docs/architecture.md),
[roadmap](https://github.com/fullsend-ai/fullsend/blob/main/docs/roadmap.md),
[runtimes](https://github.com/fullsend-ai/fullsend/blob/main/docs/runtimes.md),
[intent representation](https://github.com/fullsend-ai/fullsend/blob/main/docs/problems/intent-representation.md).

Fullsend is the closest open-source comparison. Its README describes
autonomous software development for GitHub, GitLab and Forgejo, "secure by
design". Each target repo carries a `.fullsend/` folder. On GitHub a thin
shim workflow calls one upstream reusable workflow, which mints short-lived
GitHub App tokens through OIDC (identity proof without stored secrets). Its
stack is dispatch, infrastructure, sandbox, harness and runtime, with one
sandbox per agent role.

Changed since August: per-org install was removed; GitLab uses a webhook fast
path plus a cron poller; the runtime table lists `claude` (default) with `pi`
and `codex` opt-in; Jira intake is a "Now" roadmap theme. Fullsend still
publishes many ADRs (short design decision records). Its tiered-intent idea
shaped [#2812](https://github.com/hivecommons/hive/issues/2812).

### Where Hive differs

| Dimension | Fullsend | Hive |
| --- | --- | --- |
| Control plane | Repo-centered install; GitHub Actions or GitLab CI runs each stage. | A live fleet plane: governor modes, dashboard, terminal access, budgets, hub/spoke heartbeats, contributor compute. |
| Execution model | Short runs, one sandbox per agent role; Claude Code by default, `pi` and `codex` opt-in. | Long-lived agents in tmux; backends include Claude, Copilot, Codex, Antigravity, Goose and OpenAI-compatible gateways. |
| Autonomy model | Tiered intent and shadow/autonomous concepts in its problem docs. | ACMM (AI Codebase Maturity Model) L1–L6 maps repo maturity to per-agent modes and merge rights. |
| Security | Sandbox, harness, scanner and OIDC token minting are first-class. | Layered controls: tool denial, scoped tokens, per-user attribution, and a proxy that checks every GitHub write on the network. See [security model](security-model.md) and [self-assessment](security-self-assessment.md). |
| Fleet topology | Per-repo install. | Hub/spoke registry, callbacks, leaderboard, provisioning paths, contributor relay. |

Fullsend is lighter for one repo or org where Actions is already trusted.
Hive fits when operators need live fleet visibility, many runtimes, graduated
autonomy, hub-managed spokes, contributor compute, or network-level checks.

## OpenAI Symphony

Checked: [openai/symphony](https://github.com/openai/symphony) at
[`be10a1b`](https://github.com/openai/symphony/tree/be10a1b79df723d6d7612b5651c8522704dafb2e)
([README](https://github.com/openai/symphony/blob/be10a1b79df723d6d7612b5651c8522704dafb2e/README.md),
[SPEC.md](https://github.com/openai/symphony/blob/be10a1b79df723d6d7612b5651c8522704dafb2e/SPEC.md)).
`main` has not moved since 2026-09-15. Symphony is Apache-2.0; its Elixir
build is an "engineering preview for trusted environments". The demo watches
Linear, runs isolated agents and shows proof of work before a PR lands. Those
extras are **not** required by the spec. Hive borrows the idea of managing
work rather than watching every coding turn. Hive does not implement
Symphony's `WORKFLOW.md` or Codex app-server contracts.

| Dimension | Symphony | Hive |
| --- | --- | --- |
| Work source | Linear in the demo; the spec defines a tracker-neutral adapter. | Trackers: GitHub Issues, GitHub Projects, Linear and Jira. Forge: GitHub; GitLab and Gitea/Forgejo adapters are not yet end-to-end (see Work sources below). |
| Pre-agent gating | Config preflight, state checks, labels, claims and concurrency limits. | Shell-level listing, classification and merge checks before any kick, plus ACMM and proxy write checks. |
| Agents | Codex app-server is the runner protocol. | Many CLI backends; isolation depends on backend and deployment. |
| Scale model | One workspace per issue, bounded runs, retry. | Queue-depth cadence, long-lived agents, hub/spoke, convergence audits. |
| Packaging | Draft spec plus Elixir preview. | Go runtime, Compose/Quadlet deploy, dashboard, contributor relay. |

See the [alignment review](../../docs/design/symphony-spec-alignment.md),
[work sources](work-sources.md) and the
[provider contract](integrations/work-source-providers.md).

## GitHub Agentic Workflows (gh-aw)

Checked: [github/gh-aw](https://github.com/github/gh-aw),
[docs](https://github.github.com/gh-aw/),
[githubnext/agentics](https://github.com/githubnext/agentics).

gh-aw compiles Markdown plus YAML frontmatter into a normal GitHub Actions
workflow: an Actions-native on-ramp, not a fleet scheduler. Since August its
engine list added **Pi** alongside Copilot, Claude Code, Codex and Gemini.
Agent jobs are read-only and sandboxed by default; writes go through
`safe-outputs` jobs with scoped permissions.

| Dimension | gh-aw | Hive |
| --- | --- | --- |
| Execution | Event, dispatch and scheduled Actions runs | Long-running fleet with queue-depth cadence |
| Authoring | Markdown prompt plus frontmatter | Project config, fixed pipeline, agent policies |
| Policy | Engine judgment with declared tools and safe outputs | Fixed filtering before judgment, gated merge rights after |
| Operations | Per-workflow logs and artifacts | Live dashboard, budgets, hub/spoke, contributor compute |
| Best start | A repo on Actions that wants bounded agent jobs | Operators running continuous work across repos |

Hive's [gh-aw sample](../deploy/gh-aw/README.md) is a manually run,
report-only issue triage with no merge rights. Keep existing gh-aw workflows
when adopting Hive. Hive dispatching gh-aw as an external host is **not
implemented**; see
[backend support tiers](backend-support-tiers.md#github-agentic-workflows-on-ramp-not-a-cli-backend).

## New since August 2026

Verified 2026-10-23; all were missing from this page. "Backend" means Hive
could drive it; "orchestrator" means it competes with Hive's control plane.

**[GitHub Agent HQ](https://github.blog/news-insights/company-news/welcome-home-agents/)**
(announced 2025-10-28, rolling out since) adds a "mission control" to assign
and track agents from Anthropic, OpenAI, Google, Cognition and xAI inside a
paid Copilot plan. Orchestrator, GitHub-only, hosted. Copilot stays a Hive
backend.

**[Claude Managed Agents](https://claude.com/resources/articles/claude-managed-agents)**
(2026-04-08, public beta) is Anthropic's hosted harness: state, permissions,
scheduled runs, and self-hosted sandboxes, billed per session-hour. Backend
or hosting layer, not a work-source orchestrator.

**[OpenAI Agents API](https://platform.openai.com/docs/guides/agents)** runs
persistent agents on OpenAI infrastructure with the Codex harness. Backend
or hosting layer; Codex CLI remains a Hive backend.

**[Jules Action](https://github.com/google-labs-code/jules-action)** triggers
Google's Jules cloud agent from GitHub events. Per-repo orchestrator on
Actions, like gh-aw; Jules itself is a possible backend.

**[Omnigent](https://github.com/omnigent-ai/omnigent)** (Apache-2.0) is an
open-source "meta-harness" over Claude Code, Codex, Cursor, Pi and others
with policy and sandboxing. Registered as a T3 experimental CLI backend (#14094);
Omnigent's own policies and sandboxing are additive, with Hive's egress proxy
deciding all GitHub writes.

**[Zeron](https://github.com/zeronsh/zeron)** (MIT) is a local-first control
plane for coding agents. Orchestrator for one developer or VPS; no
work-source adapters.

| Dimension | Agent HQ | Managed Agents / Agents API | Omnigent, Zeron |
| --- | --- | --- | --- |
| Control plane | Hosted mission control in GitHub | Hosted agent runtime, no tracker | Local or VPS app |
| Work source | GitHub issues and PRs | None built in | Chat and terminal sessions |
| Policy | Enterprise controls, agentic review | Scoped permissions, vaults | Omnigent policies; Zeron local |
| Isolation | Vendor-run | Managed or self-hosted sandbox | Omnigent sandboxing; Zeron none stated |
| Role for Hive | Competitor; Copilot backend | Backend / hosting | Backend (T3) / competitor |

## Agent isolation

By default Hive agents share one container, separated by Unix user. An
opt-in sandbox mode runs each task in its own rootless Podman container or
Kubernetes Job with no credentials at all; pushes go through a broker. See
[sandbox isolation](sandbox-isolation.md) and the
[security model](security-model.md#3-agent-runtime-isolation).

The project is evaluating stronger isolation runtimes. One candidate is
[NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) (Apache-2.0), a
"safe, private runtime for fleets of autonomous AI agents" with kernel-level
file, syscall and network policy per sandbox. AAIF's
[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) is another.
[ADR-0026](adr/0026-sandbox-runtime-selection.md) (Proposed) compares both
against the sandbox contract and recommends OpenShell, pending a prototype on
a hive spoke. No integration exists today.

| Project | Isolation unit | Checked |
| --- | --- | --- |
| Hive (default) | Shared container, per-user separation | this repo |
| Hive (opt-in) | Rootless container or K8s Job per task, no credentials | [sandbox-isolation.md](sandbox-isolation.md) |
| Fullsend | One sandbox per agent role, policy in `.fullsend/` | [architecture](https://github.com/fullsend-ai/fullsend/blob/main/docs/architecture.md) |
| gh-aw | Actions runner, read-only by default | [README](https://github.com/github/gh-aw) |
| Symphony | Workspace per issue; not a sandbox by itself | [SPEC.md](https://github.com/openai/symphony/blob/be10a1b79df723d6d7612b5651c8522704dafb2e/SPEC.md) |
| OpenShell | Kernel-enforced sandbox with policy gateway | [README](https://github.com/NVIDIA/OpenShell) |
| OpenSandbox (AAIF) | Sandbox API over Docker or Kubernetes; Firecracker micro-VMs | [README](https://github.com/opensandbox-group/OpenSandbox) |

## Work sources

Hive is work-source agnostic. The governor reads GitHub Issues, GitHub
Projects, Linear or Jira through one
[provider contract](integrations/work-source-providers.md); Hive Commons
itself runs on it. Symphony is Linear-first; Fullsend, gh-aw and Agent HQ
are forge-first. Hive states this as a differentiator: the same fleet,
policy and dashboard apply whichever tracker holds the work.

That claim covers **trackers**, not **forges**. The code still has to live on
GitHub.com or GitHub Enterprise. Hive has GitLab and Gitea/Forgejo adapters
(`project.forge`) that read, comment, label and hold, and GitLab's can merge,
but the governor, merge gate and agent `gh` path are not yet forge-neutral
([#14096](https://github.com/hivecommons/hive/issues/14096)). Fullsend
already ships GitHub, GitLab and Forgejo. See [Forges](integrations/forges.md).
The [integrations page](integrations.md) lists every tracker, forge and backend
with its status.

## Related projects and vendors

**GitHub Copilot coding agent** hands one issue to one hosted agent inside
GitHub; it is now one of the agents under Agent HQ (below). **Devin-class
hosted services** sell a managed worker with a vendor UX for teams that do
not want to run orchestration. **SWE-agent and research harnesses** are
built for benchmarks and single-task repair loops. Hive differs from all
three: open-source, self-hosted, fleet-scale, with explicit policy.

## When to choose what

- **gh-aw or Jules Action**: bounded agent jobs in a repo already on
  Actions, no standing fleet.
- **Agent HQ / Copilot coding agent**: the quickest hosted path for GitHub
  issues with no separate policy plane.
- **Symphony**: spec-first `WORKFLOW.md`, Codex app-server, one workspace
  per ticket.
- **Fullsend**: a few repos, CI as the substrate, repo-visible `.fullsend/`
  config.
- **Hosted services (Devin-class, Managed Agents)**: vendor-run autonomy
  over self-hosted control.
- **Research harnesses**: evaluation and one-off repair, not operations.
- **Hive**: operating a live agent fleet across any work source, with many
  runtimes, ACMM gates, fixed merge policy, hub/spoke provisioning,
  contributor compute, cost visibility and proxy-level write checks. See
  [integrations.md](integrations.md) for what Hive works with today.

## Hive claims checked against this repo

- Fleet operations plane: [architecture](architecture.md#3-the-governor-loop--from-queue-depth-to-a-kick), [dashboard](architecture.md#10-dashboard--observability).
- Multi-runtime: [architecture](architecture.md#9-model-backends--cost), [agent configuration](agent-configuration.md).
- ACMM: [architecture](architecture.md#6-acmm--controlling-agent-autonomy), [ACMM matrix](acmm-policy-matrix.md).
- Hub/spoke and contributor compute: [architecture](architecture.md#8-hub--spoke), [manual provisioning](manual-provisioning.md).
- Proxy write checks: [architecture](architecture.md#5-layered-guardrails-defense-in-depth), [ADR-0002](adr/0002-mitm-proxy-network-enforcement.md).
- Work sources: [work-sources.md](work-sources.md).
- Isolation: [sandbox-isolation.md](sandbox-isolation.md), [security model](security-model.md#3-agent-runtime-isolation).
