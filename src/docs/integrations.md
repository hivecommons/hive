# Integrations

What Hive works with today, and what is planned. One row per integration.
Each row links the doc that explains it. Planned rows link the tracking issue.

Status key:

| Status | Meaning |
| --- | --- |
| **Supported** | Wired into a running code path and covered by tests. |
| **Opt-in** | Works, but an operator must turn it on, or it needs a per-backend escape hatch. |
| **Planned** | Not usable today. The linked issue tracks the work. |

## Agent backends

Tiers (T1 core, T2 confined, T3 experimental) come from
[backend-support-tiers.md](backend-support-tiers.md). The source list is
`KNOWN_BACKENDS` in `config/backends.conf`. Setup for each CLI is in
[backend-setup.md](../../docs/backend-setup.md). T3 backends run in
container mode by default; local mode needs an explicit opt-in.

<table>
<tr><th>Logo</th><th>Integration</th><th>Status</th><th>Tier</th><th>Docs</th></tr>
<tr><td><img src="assets/logos/claude.svg" height="40" alt="Claude logo"></td><td>Claude Code</td><td>Supported</td><td>T1 / T2</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/githubcopilot.svg" height="40" alt="GitHub Copilot logo"></td><td>GitHub Copilot CLI</td><td>Supported</td><td>T1 / T2</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/codex-badge.svg" height="40" alt="Codex"></td><td>Codex CLI (OpenAI)</td><td>Supported</td><td>T1 / T2</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/goose-badge.svg" height="40" alt="Goose"></td><td>Goose (AAIF)</td><td>Supported</td><td>T1 pod / T3 local</td><td><a href="../../docs/goose-at-scale.md">goose-at-scale.md</a></td></tr>
<tr><td><img src="assets/logos/antigravity-badge.svg" height="40" alt="Antigravity"></td><td>Google Antigravity (<code>agy</code>)</td><td>Opt-in</td><td>T3</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/gemini-badge.svg" height="40" alt="Gemini"></td><td>Gemini CLI (hub-managed only)</td><td>Supported</td><td>Go-only, no local tier</td><td><a href="backend-support-tiers.md#the-three-tiers">backend-support-tiers.md</a></td></tr>
<tr><td><img src="assets/logos/gateway-badge.svg" height="40" alt="OpenAI-compatible"></td><td>OpenAI-compatible gateways: vLLM, llm-d, LiteLLM, watsonx, OpenRouter</td><td>Supported</td><td>Gateway path</td><td><a href="../../docs/inference-backends.md">inference-backends.md</a>, <a href="backend-support-tiers.md#gateway-path-tier">gateway-path tier</a></td></tr>
<tr><td><img src="assets/logos/muse-badge.svg" height="40" alt="Muse"></td><td>Muse Code</td><td>Supported</td><td>T2</td><td><a href="backend-support-tiers.md#retroactive-tier-record-muse-code">backend-support-tiers.md</a></td></tr>
<tr><td><img src="assets/logos/opencode-badge.svg" height="40" alt="OpenCode"></td><td>OpenCode</td><td>Supported</td><td>T2 (deny-list)</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/bob-badge.svg" height="40" alt="Bob"></td><td>IBM Bob</td><td>Opt-in</td><td>T3</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/pi-badge.svg" height="40" alt="Pi"></td><td>Pi</td><td>Opt-in</td><td>T3</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/aider-badge.svg" height="40" alt="Aider"></td><td>Aider</td><td>Opt-in</td><td>T3</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/kilo-badge.svg" height="40" alt="Kilo"></td><td>Kilo Code</td><td>Opt-in</td><td>T3</td><td><a href="../../docs/backend-setup.md">backend-setup.md</a></td></tr>
<tr><td><img src="assets/logos/omp-badge.svg" height="40" alt="OMP"></td><td>Oh My Pi (OMP)</td><td>Opt-in</td><td>T3</td><td><a href="agent-configuration.md">agent-configuration.md</a></td></tr>
<tr><td><img src="assets/logos/openhands-badge.svg" height="40" alt="OpenHands"></td><td>OpenHands (legacy CLI)</td><td>Opt-in</td><td>T3</td><td><a href="backend-support-tiers.md#the-three-tiers">backend-support-tiers.md</a></td></tr>
<tr><td><img src="assets/logos/claude.svg" height="40" alt="Claude logo"></td><td>Claude Managed Agents</td><td>Experimental</td><td>Hosted execution; private self_hosted spoke, report-only, USD 25/day ledger cap</td><td><a href="agent-configuration.md#claude-managed-agents-hosted-execution">agent-configuration.md</a>, <a href="backend-support-tiers.md#hosted-execution-tier">backend-support-tiers.md</a></td></tr>
<tr><td><img src="assets/logos/openai-agents-badge.svg" height="40" alt="OpenAI Agents"></td><td>OpenAI Agents API</td><td>Planned</td><td>—</td><td><a href="https://github.com/hivecommons/hive/issues/14093">#14093</a></td></tr>
<tr><td><img src="assets/logos/omnigent-badge.svg" height="40" alt="Omnigent"></td><td>Omnigent</td><td>Opt-in</td><td>T3</td><td><a href="backend-support-tiers.md#the-three-tiers">backend-support-tiers.md</a></td></tr>
</table>

## Local contributor inference

| Integration | Status | Execution and limitations | Docs |
| --- | --- | --- | --- |
| Apple MLX / MLX-LM through Pi | **Opt-in** | ClankeR on Apple Silicon macOS 14+, native Python 3.10+, Pi 1.4.0+; user-managed tool-capable models. MLX-LM runs on the Mac; Pi executes tools inside the contributor container with Docker Desktop host networking. No cloud fallback. Live Apple GPU inference remains unverified. | [Install and use with just](contributor-relay.md#apple-mlx-local-inference) |

Apple MLX is a first-class inference-engine choice on `/contribute`, mapped to
the actual `pi` coding backend and `mlx/<model>` provider selection. It is not
a standalone coding CLI, hosted gateway, identity provider, or Apple endorsement.
`KNOWN_BACKENDS` correctly continues to list coding executors, not inference
frameworks.

## Work sources and trackers

The governor reads work through one provider contract. The adapters live in
`src/pkg/worksource/` and are selected by `governor.work_source.type`.

<table>
<tr><th>Logo</th><th>Integration</th><th>Status</th><th>Docs</th></tr>
<tr><td><img src="assets/logos/github.svg" height="40" alt="GitHub logo"></td><td>GitHub Issues</td><td>Supported</td><td><a href="work-sources.md#type-github-default">work-sources.md</a></td></tr>
<tr><td><img src="assets/logos/github.svg" height="40" alt="GitHub logo"></td><td>GitHub Projects (v2)</td><td>Supported</td><td><a href="work-sources.md#type-github_projects--github-projects-projects-v2">work-sources.md</a></td></tr>
<tr><td><img src="assets/logos/linear.svg" height="40" alt="Linear logo"></td><td>Linear</td><td>Supported</td><td><a href="work-sources.md#type-linear">work-sources.md</a></td></tr>
<tr><td><img src="assets/logos/jira.svg" height="40" alt="Jira logo"></td><td>Jira Cloud and Data Center</td><td>Supported</td><td><a href="work-sources.md#type-jira--jira-cloud-or-jira-data-center">work-sources.md</a></td></tr>
</table>

## Forges

GitHub is the only forge agents can work against today. The GitLab and
Gitea/Forgejo adapters in `src/pkg/forge/` cover reads and core writes
(comment, label, hold), and GitLab's can merge. Two governor writes use them,
but the GitHub-only enumeration that feeds those writes means they never fire
on a non-GitHub hive. See [Forges](integrations/forges.md).

<table>
<tr><th>Logo</th><th>Integration</th><th>Status</th><th>Docs</th></tr>
<tr><td><img src="assets/logos/github.svg" height="40" alt="GitHub logo"></td><td>GitHub.com and GitHub Enterprise</td><td>Supported</td><td><a href="github-app-setup.md">github-app-setup.md</a></td></tr>
<tr><td><img src="assets/logos/gitlab.svg" height="40" alt="GitLab logo"></td><td>GitLab (SaaS or self-managed)</td><td>Planned</td><td><a href="forge-app-setup.md">forge-app-setup.md</a>, <a href="https://github.com/hivecommons/hive/issues/14096">#14096</a></td></tr>
<tr><td><img src="assets/logos/gitea.svg" height="40" alt="Gitea logo"></td><td>Gitea</td><td>Planned</td><td><a href="forge-app-setup.md">forge-app-setup.md</a>, <a href="https://github.com/hivecommons/hive/issues/14096">#14096</a></td></tr>
<tr><td><img src="assets/logos/forgejo.svg" height="40" alt="Forgejo logo"></td><td>Forgejo</td><td>Planned</td><td><a href="forge-app-setup.md">forge-app-setup.md</a>, <a href="https://github.com/hivecommons/hive/issues/14096">#14096</a></td></tr>
</table>

## Isolation

The hub-side agent sandbox is off by default. See
[sandbox-isolation.md](sandbox-isolation.md) for the two launchers and the
per-backend confinement matrix.

<table>
<tr><th>Logo</th><th>Integration</th><th>Status</th><th>Docs</th></tr>
<tr><td><img src="assets/logos/podman.svg" height="40" alt="Podman logo"></td><td>Rootless Podman agent sandbox</td><td>Opt-in</td><td><a href="sandbox-isolation.md#current-wiring">sandbox-isolation.md</a></td></tr>
<tr><td><img src="assets/logos/kubernetes.svg" height="40" alt="Kubernetes logo"></td><td>Kubernetes Job sandbox runtime</td><td>Opt-in</td><td><a href="sandbox-isolation.md#running-sandboxed-kicks-as-kubernetes-jobs">sandbox-isolation.md</a></td></tr>
<tr><td><img src="assets/logos/nvidia.svg" height="40" alt="NVIDIA logo"></td><td>NVIDIA OpenShell (evaluation)</td><td>Planned</td><td><a href="https://github.com/hivecommons/hive/issues/14092">#14092</a></td></tr>
<tr><td><img src="assets/logos/opensandbox-badge.svg" height="40" alt="OpenSandbox"></td><td>AAIF OpenSandbox (evaluation)</td><td>Planned</td><td><a href="https://github.com/hivecommons/hive/issues/14092">#14092</a></td></tr>
</table>

## Ecosystem and upstream

<table>
<tr><th>Logo</th><th>Integration</th><th>Status</th><th>Docs</th></tr>
<tr><td><img src="assets/logos/cncf.svg" height="40" alt="CNCF logo"></td><td>CNCF sandbox onboarding</td><td>Planned</td><td><a href="cncf-sandbox-onboarding.md">cncf-sandbox-onboarding.md</a></td></tr>
<tr><td><img src="assets/logos/gh-aw-badge.svg" height="40" alt="gh-aw"></td><td>GitHub Agentic Workflows (gh-aw) report-only sample</td><td>Supported (sample)</td><td><a href="../deploy/gh-aw/README.md">deploy/gh-aw</a>, <a href="backend-support-tiers.md#github-agentic-workflows-on-ramp-not-a-cli-backend">tiers note</a></td></tr>
<tr><td><img src="assets/logos/goose-badge.svg" height="40" alt="Goose"></td><td>Goose-at-scale guide upstreamed to AAIF</td><td>Planned</td><td><a href="../../docs/goose-at-scale.md">goose-at-scale.md</a>, <a href="https://github.com/hivecommons/hive/issues/14095">#14095</a></td></tr>
</table>

## See also

- [Landscape and positioning](landscape.md) — how Hive compares to nearby tools.
- [Work-source provider contract](integrations/work-source-providers.md).
- [Forges: GitLab, Gitea and Forgejo](integrations/forges.md).
- [Logo sources and licenses](assets/logos/SOURCES.md).

Logos are trademarks of their respective owners. They are used here for
identification only and do not imply endorsement.
