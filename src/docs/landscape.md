# Hive landscape and positioning

Last reviewed: 2026-11-02 (focused update: gh-aw GA, Entire, Devin and five engine releases)

> **Baseline conducted: 2026-10-23; focused update: 2026-11-02. This document will rot.** Tools for AI coding
> agents change fast. Treat every product detail here as time-sensitive and
> update it by pull request when public docs change.

Hive helps an operator run a group of coding and review agents. For example,
the operator can check costs, pause a worker, review its proposed fix, and
decide which checks must pass before it merges. This live management layer is
the **operations plane**. A **fleet** is the group being managed; a **hub**
registers and observes running hives (**spokes**).

Each comparison below names the public page checked. Sections outside the
focused update retain their earlier observation dates; this is not a fresh
audit of every vendor. This is evaluation reference, not a setup guide.

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

**Updated 2026-11-02:** GitHub announced
[general availability on 2026-10-28][gh-aw-ga] on github.com for Free, Team
and Enterprise. Its default Copilot CLI engine requires a Copilot license.
This is the Markdown-to-Actions product, not the separate Agent HQ or
Copilot parallel-agent announcements.

gh-aw compiles Markdown plus YAML frontmatter into a normal GitHub Actions
workflow: an Actions-native on-ramp, not a fleet scheduler. Since August its
engine list added **Pi** alongside Copilot, Claude Code, Codex and Gemini.
Agent jobs are read-only and sandboxed by default; writes go through
`safe-outputs` jobs with scoped permissions.

| Dimension | gh-aw | Hive |
| --- | --- | --- |
| Execution | Event, dispatch and scheduled Actions runs | Long-running fleet with queue-depth cadence |
| Authoring | Markdown prompt plus frontmatter | Project config, fixed pipeline, agent policies |
| Policy | Read-only agent, integrity filter, firewall, threat detection and permission-scoped safe outputs | Fixed filtering before judgment, proxy write checks, gated merge rights after |
| Operations | Per-workflow logs and artifacts | Live dashboard, budgets, hub/spoke, contributor compute |
| Best start | A repo on Actions that wants bounded agent jobs | Operators running continuous work across repos |

Hive's [gh-aw sample](../deploy/gh-aw/README.md) is a manually run,
report-only issue triage with no merge rights. Keep existing gh-aw workflows
when adopting Hive. Hive dispatching gh-aw as an external host is **not
implemented**; see
[backend support tiers](backend-support-tiers.md#github-agentic-workflows-on-ramp-not-a-cli-backend).

## Focused comparison: maintenance, forge and managed agents

Checked **2026-11-02** against the first-party sources below. **Documented**
means the vendor says it works, not that Hive tested it. **Unknown**
means we did not verify it. It does not mean the vendor lacks it.
Hive's source baseline is v5
[`ddf4a56fc1a66f379b97f9fa64c834fd7896a034`](https://github.com/hivecommons/hive/commit/ddf4a56fc1a66f379b97f9fa64c834fd7896a034),
committed 2026-11-02. We did not check v6 or prove the same behavior in live fleets.

| Actual job | Hive at that baseline | GitHub Agentic Workflows GA | Entire / Devin, documented as checked |
| --- | --- | --- | --- |
| Execute tasks and maintain repositories | Standing governor and role-based fleet; operator owns deployment and policy ([architecture][hive-architecture]) | Event/scheduled Actions jobs can perform ongoing triage, CI analysis, docs and tests; not just one-off coding ([GA][gh-aw-ga]) | **Devin:** event/schedule automations can start or message sessions and run persistent triage; not merely a single worker ([automations][devin-automations]). **Entire:** a forge hosting agent-human work, not evidence of a Hive-like maintenance scheduler ([launch][entire-launch]). |
| Admit work and enforce writes | ACMM modes and GitHub proxy controls; enforcement depends on lane/config; shadow evaluation is not a blocking gate ([policy comparison][hive-policy]) | Read-only agent by default, integrity filtering, sandbox/firewall, threat detection, then separate permission-controlled jobs ([GA][gh-aw-ga], [safe outputs][gh-aw-outputs]) | **Devin:** mandatory security profiles intersect network/MCP/git restrictions and can remove the `gh` token. Running sessions pick up profile changes at next wake, not immediately ([profiles][devin-profiles]). **Entire:** launch describes evals, not an equivalent out-of-agent write-policy contract. |
| Review and merge authority | CI, holds and configured review gates; L6 eligibility is not unconditional merge permission ([policy][hive-policy]) | Safe outputs can merge PRs when configured; applicable policy gates still matter ([safe outputs][gh-aw-outputs]). Do not describe gh-aw as having no merge authority. | **Entire:** Trails evaluate each push, expose findings through CLI/web and let developers merge when evals are satisfied ([launch][entire-launch]). **Devin:** review comments/approvals and optional beta Autoapprover; availability varies. This is not proof of Hive-equivalent merge policy ([review][devin-review]). |
| Orchestrate many agents | Live hub/spoke fleet, budgets, operator controls ([architecture][hive-architecture]) | Actions runners/policies and per-workflow engine/model routing; cross-repo isolation is documented ([GA][gh-aw-ga]) | **Devin:** deterministic Python Dynamic Workflows fan out sessions, record calls and resume; enterprise admin enablement required ([workflows][devin-workflows]). **Entire:** Trails/CI integration are not evidence of a fleet scheduler. |
| Preserve provenance and measure outcomes | Review/merge evidence and scoped observational outcomes; no cross-vendor causal benchmark ([evidence][hive-evidence], [October outcomes][hive-outcomes]) | Actions artifacts/logs and validated output jobs; Accenture figures in GA announcement are a customer account, not a controlled Hive comparison ([GA][gh-aw-ga]) | **Entire:** Trails preserve intent/trace/outcome; Continuous Memory syncs sessions into Git, with graph/brain components. Speed/token savings are vendor claims, not reproduced here ([launch][entire-launch]). **Devin:** test recordings and workflow call history are documented evidence, not directly comparable outcome rates ([testing][devin-testing], [workflows][devin-workflows]). |
| Source and forge scope | Apache-2.0 source; GitHub operational path, GitLab and Gitea/Forgejo adapters with substantial end-to-end gaps ([license][hive-license], [forge contract][hive-forge], [limits][hive-forges]) | Open-source project running on GitHub Actions; GA announcement establishes github.com availability, not general forge portability ([GA][gh-aw-ga]) | **Entire:** independent Git-compatible private-repo hosting; launch lists public repositories and self-hosted Git nodes as upcoming, not delivered ([launch][entire-launch]). **Devin:** documented GitHub, GitLab, Bitbucket and Azure DevOps integrations; this is broader documented forge coverage than Hive's complete operation today, not tested parity ([integrations][devin-integrations]). |

Entire's **2026-10-26** launch is a first-party source for Trails,
Continuous Memory and Buildkite/Depot CI. Its open-source CLI/graph/brain
components do not make the hosted forge an open-source Hive replacement.
Support for Git alone does not prove that Hive can use Trails.

Devin's own docs show that its policy and fleet tools overlap with Hive.
The weekly report's specific **2026-10-30 Cognition enterprise-governance
announcement remains unconfirmed** here. Live docs with no date show what
the vendor says now. They do not prove when it shipped.
A guessed URL returning 404 does not prove that a launch claim is false.

### Forge correction and existing decision

Neither “Hive is GitHub-only” without qualification nor “Hive supports all
three forges end-to-end” is right. The existing
[`Forge` interface and `NewForge` factory][hive-forge] already select GitHub,
GitLab and Gitea/Forgejo. However,
[`main_eval_cycle.go`][hive-enumeration] and
[`repos_rescan.go`][hive-rescan] still call `ghClient.EnumerateActionable`.
Some writes use the adapter. Gaps remain in how Hive finds work, runs agents,
merges code and checks traffic ([full list][hive-forges]).
We found no proof of a live non-GitHub Hive.

Do not redesign the interface. [ADR-0005](adr/0005-forge-abstraction.md),
[#6170](https://github.com/hivecommons/hive/issues/6170) and the
[roadmap sequencing decision][hive-roadmap] already cover it.
The September 9 decision under #6177/#6167 put the work to split out listing
policy after the GA cut. Earlier work was allowed only if it would not reset
the required-gate soak (the period used to prove that checks stay green).
A closed design issue does not prove full support.
Revisit that choice with a real user's need and proof that the full path works.
This page does not reverse it.

### Five report releases: integration compatibility

**Method:** read published, non-draft release notes and the source contracts
linked below. Read Hive's launch code, config, tests and CI steps.
**No vendor binary, authenticated task, launcher smoke, build or local test
suite was executed for this assessment.** These are source-contract results,
not proof that the tools ran well together. Release dates say when a release
was published. Linked source versions have their own commit dates.

| Report release (published, 2026) | Hive contract and source result | Action / remaining uncertainty |
| --- | --- | --- |
| [OpenHands v1.34.0](https://github.com/OpenHands/OpenHands/releases/tag/v1.34.0) — Oct 27 | Tagged [manifest][openhands-manifest] (source Oct 27) is `@openhands/agent-canvas`, binary `agent-canvas`. Hive's [experimental contract][hive-backends] is the separate legacy `openhands` CLI, previously checked at CLI 1.16.0, using `--always-approve --override-with-envs`; its support page says headless-only. | **Different surface; not a CLI upgrade instruction.** No demonstrated launcher break attributable to this release. Retain T3; Canvas Docker/runtime features do not confer confinement on Hive's legacy CLI. That CLI's continued viability needs separate evidence before promotion. |
| [Claude Code v2.1.316](https://github.com/anthropics/claude-code/releases/tag/v2.1.316) — Oct 31 | Hive already pins 2.1.316 in [main image][hive-image] and [contributor image][hive-contributor-image]. [Launcher][hive-launch] uses `--model`, unattended permissions and write/host deny flags. Notes fix process cleanup and `claude -p` reporting; they do not establish removal of those launch flags. | **No release-note break identified; already pinned.** Do not bump again. Permission honoring, readiness and output behavior still require executed CI smoke evidence; an existing pin is not a passing smoke result. |
| [Goose v1.57.0](https://github.com/aaif-goose/goose/releases/tag/v1.57.0) — Oct 28 | Already pinned in both images above. Hive emits `goose run -s [--model …]` ([launcher][hive-launch]); tagged [CLI source][goose-cli] (source Oct 27) retains run input, model override and `-s` interactive behavior. Release's supplied-session-ID and ACP changes do not demonstrate removal of that path. | **Inspected launch contract retained.** No version change. Resume/log extraction is a separate seam, previously verified against 1.52.0 ([Hive resume contract][hive-resume]); 1.57.0 resume and confinement were not smoke-tested here. T1 pod credentials do not imply local sandboxing. |
| [OpenCode v2.2.5](https://github.com/anomalyco/opencode/releases/tag/v2.2.5) — Oct 30 | Hive's [shell contract][hive-backends] uses `--auto` and local `OPENCODE_PERMISSION` host-state denials. Tagged [run handler][opencode-run] (source Oct 30) still passes model and auto/yolo settings into execution. Notes describe `bash` → `shell` tool-name handling with the same permissions. | **Inspected model/auto seam retained; denial semantics unverified.** Check existing local-posture tests and a credential-free permission fixture before promoting a new pin; do not infer that a tool alias bypasses denials or that deny-listing is an OS sandbox. |
| [Cline SDK `sdk/sdk/v0.2.0`](https://github.com/cline/cline/releases/tag/sdk/sdk/v0.2.0) — Oct 26 | No `cline` in Hive's [15 CLI backend identities][hive-cli-list]. Notes change session reattachment, capability routing and SDK packaging; none is an implemented Hive SDK contract. | **Not an integrated engine; no current launcher to break.** No automatic SDK adoption. Any future integration must satisfy the [existing backend acceptance bar](backend-support-tiers.md) or external-host admission contract, including credentials, cancellation and evidence. |

Existing guards include launch-command fixtures, backend list parity and
contributor-local posture tests. The
[scheduled live backend smoke][hive-smoke] currently covers **Claude/Codex**,
not these five releases as a group. Its pinned and latest lanes must not be
conflated. This review did not retrieve a version-bound passing run for any of
the five; doc-PR CI is not evidence that vendor CLI tasks succeeded.
No release-specific launcher defect was established, so this update makes no
engine, dependency, configuration or pin changes.

### Ranked decisions: keep, integrate, defer, drop

These are suggested next steps as of **2026-11-02**. They are not measured
returns on investment or approved roadmap changes.

1. **Keep the governed maintenance plane; drop the “single-worker competitors”
   claim.** The docs fix is cheap. It helps teams choose without claiming that
   gh-aw/Devin lack ongoing jobs or policy. Use the
   jobs table above and existing [ACMM policy](acmm-policy-matrix.md), not a
   claimed universal quality/cost advantage.
2. **Integrate with existing gh-aw jobs rather than replace them.** Lowest-risk
   entry is the [report-only sample](../deploy/gh-aw/README.md). Broader dispatch
   stays behind the existing [external-workflow admission design](design/external-workflow-admission.md)
   and [acceptance bar](backend-support-tiers.md#github-agentic-workflows-on-ramp-not-a-cli-backend):
   checked receipts, a way to cancel, and refusal of stages that can write.
   Benefit: reuse Actions work; cost: real checks on who may run what,
   not a CLI name added to a list. No dispatch adapter is shipped by this doc.
3. **Revisit the forge plan only for a real user's need.**
   Reuse [#6167](https://github.com/hivecommons/hive/issues/6167) and the
   [recorded decision][hive-roadmap]. Benefit: work on more forges; high cost:
   listing/filter rules, write/merge/proxy/auth paths and proof in a live hive.
   Entire makes forge choice and traces worth a look. It is not a reason to
   implement a fourth adapter before the existing three work end-to-end.
4. **Keep evidence tied to heads, populations and provenance.** Reuse
   [evidence](evidence.md) and [outcomes](benchmarks/outcomes-2026-10.md);
   check whether Trail-style intent and session context help reviewers before
   bringing in outside transcripts. First review what data to store and for how long.
   No vendor benchmark was reproduced. Hive's historical estimated cost per
   merge is not a current rate for all users or a fair vendor score.
   Trials and transcript links cost time to build and review.
   This docs update does not promise them.
5. **Drop speculative release churn and SDK parity work.** Keep current tiers
   and use the five-release assessment above. Benefit: avoid mistaking app/SDK
   releases for launcher contracts; cost: unresolved seams still need targeted
   CI evidence before a future pin or support promotion.

**Licensing and maintenance:** Hive's [Apache-2.0 source license][hive-license]
does not license vendor models, binaries or hosted services. Check the exact
component/version license and service terms before redistribution; GitHub API
`license: null`/`NOASSERTION` is not a product-license determination. Crux is
maintained under `src/crux` here, not by changing the generated standalone
repository ([sync marker, source revision 2026-11-02][crux-sync]).
Backend-list membership is not a support/service guarantee.
When updating this section, record the observation date, release publication
date, immutable source revision and whether evidence is documented, tested or
unknown; recheck it before procurement or a roadmap decision.

[gh-aw-ga]: https://github.blog/changelog/2026-10-28-github-agentic-workflows-generally-available/
[gh-aw-outputs]: https://github.github.com/gh-aw/reference/safe-outputs/
[entire-launch]: https://entire.com/news/entire-launches-an-independent-git-forge-for-agent-human-collaboration.md
[devin-automations]: https://docs.devin.ai/product-guides/automations.md
[devin-profiles]: https://docs.devin.ai/product-guides/security-profiles.md
[devin-workflows]: https://docs.devin.ai/work-with-devin/dynamic-workflows.md
[devin-review]: https://docs.devin.ai/work-with-devin/devin-review.md
[devin-testing]: https://docs.devin.ai/work-with-devin/testing-and-recordings.md
[devin-integrations]: https://docs.devin.ai/integrations/overview.md
[hive-architecture]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/docs/architecture.md
[hive-policy]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/docs/copilot-permissions-comparison.md#what-hive-does
[hive-evidence]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/docs/evidence.md
[hive-outcomes]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/docs/benchmarks/outcomes-2026-10.md
[hive-license]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/LICENSE
[hive-forge]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/pkg/forge/forge.go#L182-L295
[hive-enumeration]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/cmd/hive/main_eval_cycle.go#L559
[hive-rescan]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/cmd/hive/repos_rescan.go#L56
[hive-forges]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/docs/integrations/forges.md
[hive-roadmap]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/docs/roadmap.md#L120-L122
[hive-backends]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/config/backends.conf#L635-L655
[hive-launch]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/crux/agent/manager_launch.go#L911-L965
[hive-image]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/Dockerfile#L324-L345
[hive-contributor-image]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/Dockerfile.contributor#L240-L366
[hive-resume]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/crux/agent/backend_resume.go#L156-L173
[hive-cli-list]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/src/crux/config/agent_runtime.go#L1136-L1163
[hive-smoke]: https://github.com/hivecommons/hive/blob/ddf4a56fc1a66f379b97f9fa64c834fd7896a034/.github/workflows/backend-smoke.yml#L1-L39
[openhands-manifest]: https://github.com/OpenHands/OpenHands/blob/6c120073f5a1e1c838ba6fdd9a075c79ff997b98/package.json#L1-L20
[goose-cli]: https://github.com/aaif-goose/goose/blob/30bba95105e59bc3e7df624cf6b4d433690cfe12/crates/goose-cli/src/cli.rs#L335-L385
[opencode-run]: https://github.com/anomalyco/opencode/blob/9bbb8ce18c80f099e0e448775f59d965ead40a21/packages/cli/src/commands/handlers/run.ts#L15-L28
[crux-sync]: https://github.com/hivecommons/crux/blob/9952e1127f1859bff2b406a71b2eb60db9fba7b8/.hive-sync#L1-L9

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

That claim covers **trackers**, not **forges**. Complete Hive operation still
requires GitHub.com or GitHub Enterprise. Hive has GitLab and Gitea/Forgejo adapters
(`project.forge`) that read, comment, label and hold, and GitLab's can merge,
but the governor, merge gate and agent `gh` path are not yet forge-neutral
([#14096](https://github.com/hivecommons/hive/issues/14096)). Fullsend
already ships GitHub, GitLab and Forgejo. See [Forges](integrations/forges.md).
The [integrations page](integrations.md) lists every tracker, forge and backend
with its status.

## Related projects and vendors

**GitHub Copilot coding agent** hands one issue to one hosted agent inside
GitHub; it is now one of the agents under Agent HQ. **Devin** also documents
ongoing automations, multi-session orchestration and mandatory security
profiles; see the [focused comparison](#focused-comparison-maintenance-forge-and-managed-agents).
**SWE-agent and research harnesses** target benchmarks and repair loops.
Choose on deployment ownership, integration contracts and enforcement needs,
not an assumption that hosted services lack fleets or explicit policy.

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
