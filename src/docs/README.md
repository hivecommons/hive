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

- [Manual provisioning](manual-provisioning.md) — heartbeat-only cluster provisioning, hub access roles, and common gotchas.
- [Hosted Hive Hub onboarding](hosted-hub.md) — signing in at `https://hive.hivecommons.dev`, requesting a hosted hive, installing the GitHub App, configuring model gateways and Copilot login, reading `/fleet`, and fixing common setup problems.
- [Hub login token](hub-login-token.md) — who consumes the stored GitHub user token from the Hub's browser sign-in, its lifetime, and the empty-scope caveat.
- [Self-hosted hub deployment](hub-deployment.md) — `HIVE_MODE=hub`, hub storage, heartbeat secrets, and SaaS spoke registration.
- [`CAP_NET_ADMIN` and self-hosted spokes](net-admin-requirement.md) — the container runs with or without `NET_ADMIN`; granting it (`--cap-add NET_ADMIN` / `securityContext.capabilities.add`) enables the full forced-proxy-egress gate, and what the degraded best-effort mode means without it.
- [Config layering](config-layering.md) — how ConfigMap seed, PVC dashboard overlay, and runtime config interact.
- [Operator reference](operator-reference.md) — top-level config blocks, hive flags/env, GitHub token scopes, and image provenance.
- [Token mint](token-mint.md) — the opt-in `mint:` block (`pkg/mint`): what a minted token grants, key lifecycle, and the trust boundary an operator must get right before enabling it. Companion to [ADR-0007](adr/0007-token-mint.md).
- [Changelog](../../CHANGELOG.md) — recent user-visible changes and release notes.
- [Release channels](release-channels.md) — `stable`/`candidate`/`edge` moving image tags, per-line channel ownership (`v5` → `candidate`/`latest`, `v6` → `edge`, `v4` → none), switching a hive to a channel, and the version pill.
- [Digest-verifiable rollback](release-rollback.md) - the operator runbook for pinning a hive back to a prior immutable short-SHA build for all three images (`hive`, `hive-contributor`, `hive-hub`), stopping the hub automation that would undo it, and verifying by digest rather than by tag that the pin landed on the running spoke.
- [v5 GA readiness bar](v5-ga.md) — measurable release-train, migration, safety, and governance criteria that must be evidenced before v5 can be promoted beyond the active-development `edge` channel.
- [v5 GA candidate week sequencing](v5-candidate-week.md) — dependency-ordered plan for the remaining human-gated GA-bar steps: candidate-SHA designation, candidate-pinned measurements, the two bound exercises, and the v4 freeze evaluation.
- [Stable soak and promotion policy (v5 line)](stable-soak-policy.md) — the CI-enforced gate between `candidate` and `stable`: soak conditions, immutable short-SHA rollback tags, the emergency exception path (what it waives, and the post-hoc evidence that closes its follow-up issue), and the ledger of exceptions taken with their per-image rollback digests.
- [HiveCommons migration tracker](hivecommons-migration.md) — phased org/package migration status, operator promises, and sequenced closeout checklist.
- [v4 → v5 forward-port sync policy](v5-sync-policy.md) — proposed cadence, ownership, merge-commit top-up procedure, and the PR/review contract for keeping `v5` topped up with `v4`; protects the v5 GA bar from unbounded drift.
- [v4 feature-freeze execution runbook](v4-freeze-runbook.md) — the mechanics behind the accepted freeze policy (#6346), already wired behind the `V4_FEATURE_FREEZE` repository variable: the one-command declaration, the freeze-marshal role, the ordered checklist (declare, gate v4 intake, retarget the agent fleet, retire batch-sync), the post-freeze v4 → v5 cherry-pick procedure, and the channel/default-branch remap at GA cut.
- [v5 migration — operator announcement](v5-migration-announcement.md) — **draft, do not publish** until #7721 Phase 0 completes: the migration-week announcement text (channel table before/after, per-selection operator actions, security-fix policy while `stable` lags, rollback, key dates) and the marshal's publication checklist.
- [dibs domain cutover](dibs-domain-cutover.md) — staged operator sequence for moving dibs to `dibs.hivecommons.dev`, including DNS, Let's Encrypt quota hold, Certificate/Ingress manifests, redirect verification, and rollback.
- [Serving spokes from the fleet wildcard certificate](spoke-wildcard-tls.md) — how to point a cluster's provisioned spokes at the wildcard instead of one certificate per hive, the two cluster prerequisites that must hold first, and which hosts a wildcard cannot cover.
- [Tagged releases](releases.md) — the automated `v1.2.3` release path: what triggers a release, how the version is inferred from `CHANGELOG.md`, the commit convention that drives it, the human escape hatch, how it relates to the moving release channels above, and the per-release SPDX SBOM attached to each GitHub Release (and why it is a release artifact, not an in-image attestation — see #3760).
- [Dashboard-triggered standalone upgrades](dashboard-standalone-upgrades.md) — the dashboard upgrade button for standalone (Compose/Podman-Quadlet) hives: the `HIVE_DEPLOYMENT_RUNTIME`/`HIVE_DEPLOYMENT_PODMAN_MODE` runtime contract, the closed host-side helper (`hive-dashboard-upgrade-helper.sh`), and why an unproven runtime deliberately hides the button.
- [Spoke dashboard](dashboard.md) — the static dashboard FAQ panel contract: not ACMM-gated, no JS/fetch, grouped L1-L6/runs/contributors/claims/cost help, and guarded config-key references.
- [Dashboard design system](dashboard-design-system.md) — shared token catalogue, component variants, migration rules, ratchet plan, and #8536 theme override contract for spoke, contributor, and hub dashboard surfaces.
- [The `auto-update` Compose profile](auto-update-profile.md) — what unattended Watchtower updates cost you, what the Docker socket proxy does and does **not** fix, and why Kubernetes should not use this profile at all.
- [Environment variable reference](env-vars.md) — centralized list of runtime, deployment, hub, backup, and contributor environment variables.
- [Kubernetes deployment](kubernetes-deployment.md) — the operator path for Kubernetes: prerequisites, namespace, secret, ConfigMap, PVC, Deployment, Service, Ingress, and published ports. The manifests it applies are [`src/deploy/k8s/`](../deploy/k8s/). See also [dashboard route and health checks](health-checks.md) and the Kubernetes CronJob in [backup and restore](backup-restore.md).
- [Troubleshooting](troubleshooting.md) — container logs, config validation, agent tmux sessions, dashboard auth, GitHub credential checks, and GitHub App workflow-permission push rejections.
- [Cross-cluster migration](cross-cluster-migration.md) — the manual procedure for moving a hive between clusters.
- [Try Hive on kind](try-hive-on-kind.md) — a local single-node Kubernetes quickstart: the `kind` kustomize overlay (no inference component, `standard` StorageClass, external OpenAI-compatible endpoint), port-forward sign-in, and kind gotchas (`NET_ADMIN`, netfilter modules, image pull time).
- [Self-hosted Kubernetes cluster move](move-kubernetes.md) — moving a self-hosted (non-hub) Kubernetes hive between clusters: which PVC/Secret/ConfigMap paths hold identity and state, a generic `kubectl`-based PVC copy, target prerequisites, and verification.
- [Hub-registered hive cutover](move-hub-registered-cutover.md) — cross-cutting rules for any hub-registered hive's move: what identity is, how the hub reads heartbeat/`dashboard_url`, why source and target must never run concurrently, the required cutover order, and rollback.
- [Moving a Hive between hosts, same runtime](move-host.md) — host-to-host move procedures preserving hive identity and state for Docker Compose → Compose, Podman Quadlet rootless → rootless (including a different `/etc/subuid` base on the target), and Podman Quadlet rootful → rootful: what's host-bound, a target preflight checklist, exact volumes/paths to archive, the stop-source-before-start-target rule, and post-restore verification. All procedures are DOCUMENTED, NOT EXECUTED.
- [Cross-runtime moves](move-cross-runtime.md) — Podman → Docker (the reverse of `backup-restore.md`'s executed Docker → Podman migration), and Compose/Quadlet ↔ Kubernetes: volume ↔ PVC, secrets ↔ Kubernetes Secrets, and which host-bound settings must change. Documented, not executed.
- [v2 → v4 migration](migration-v2-v4.md) — upgrading a v2 deployment: the config is compatible unmodified, and what actually changes is the image tag, the published `7681` port, and the Compose/Kubernetes security settings.
- [v4 → v5 migration](migration-v4-v5.md) — upgrading a v4 deployment before the 2026-12-21 EOL: channel/tag changes, additive config deltas, Kubernetes/Compose/Podman rollout steps, verification, and digest rollback.
- [Major-version upgrade guide](../../UPGRADE.md) — preparing for v4 → v5 and future major-version boundaries.
- [Dashboard route and health checks](health-checks.md) — `dashboard-route-rbac.yaml`, `route_exists`, listener probes, and alert behavior.
- [Fleet health: the verdict and remediation hints](fleet-health.md) — the per-hive green/amber/red/unknown verdict on `/fleet`: what each state means, ACMM-banded output expectations, precedence (App broken beats provider limit beats budget beats generic no-output), the cause → remediation table, detector semantics (error streaks, consent wedge, no cadence, channel lag) with the carry-forward rule that lets a recovered hive clear its own alarm, and a symptom → fix troubleshooting table.
- [Fleet drift signals](fleet-drift-signals.md) — the per-hive deviation badges on My Hives: all signal kinds (`heartbeat-stale`, `duplicate-spoke`, `identity-split`, `version-absent`, `pinned-image`, …) with severities and who fixes each (owner vs hub operator), the derived fleet norm behind `branch-mismatch`/`version-behind`, the deliberate suppression rules (placeholders, actively-upgrading hives, status-flipping yielding to duplicate-spoke), and the in-memory first-seen semantics.
- [Fleet self-reporting (`governor.fleet_report`)](fleet-report.md) — how a hive reports its own hive-attributable failures upstream to `hivecommons/hive`: the `file_upstream` opt-in (default off = dry-run preview on the dashboard), the two triggers (`acmm-shortfall` after two unmet weekly epochs with attributable evidence, and `hive-code-defect` on hive's own component allowlist with no shortfall required), exactly which fields leave the hive and which never do (the raw hive ID is replaced by a truncated SHA-256), fingerprint deduplication via comment + 👍 reaction instead of duplicate issues, and recovery comments that close only issues the hive itself opened.
- [Agent self-healing watchdog](agent-watchdog.md) — liveness and readiness reconciliation for launched agents: liveness classification, restart backoff, crash-loop escalation, the auth probe that refuses to restart into dead credentials, and the `conditions` array on `/api/agents`. Ships in `mode: observe`, which audits the restarts it would have made without making them.
- [Audit log format](audit-log.md) — the JSONL schema of `/data/audit.jsonl`: the five fields, how to parse the flat `detail` string (and why `repo` is not first-class), the pseudo-users, and why size-triggered rotation means the effective lookback varies per hive rather than being 90 days.
- [Per-repo agent pause](repo-pause.md) — quieting ONE repository without stopping the hive: `project.paused_repos`, the dashboard toggle and `POST /api/repos/pause`, the provenance every pause carries (who/when/why), and exactly which layers enforce it — the MITM proxy, the `hive-open-pr`/`hive-merge` relays, work enumeration and the auto-merge sweeps — plus what a pause deliberately does *not* stop (reads, the hive's own control plane, and your own pushes).
- [Token-access audit log](token-access-log.md) - the *other* audit log: `/var/run/hive-metrics/token-access.jsonl`, appended by `gh-wrapper.sh` on every agent `gh` call and by `git-credential-hive.sh` on every credential lookup, served (owner-gated, last 100 lines) by `GET /api/token-access`. Covers both line schemas, the entrypoint pre-creation/permission model, why an empty log is not "no activity", and the blind spots (contributor mode, wrapper bypass, tmpfs non-durability).
- [Per-repo agents](per-repo-agents.md) — scoping an agent to the repositories it serves with `repos:`, so which agents exist is a per-repo answer instead of a hive-wide one: what a scope narrows (kick contents, `$HIVE_REPO`/`$HIVE_REPOS`, the AUTHORIZED REPOS block) and what enforces it deterministically (the MITM proxy plus the `hive-open-pr`/`hive-merge`/`hive-open-issue` relays), the optional `repos:` key on a BYO `AgentSpec` and why it is an optional interface rather than a sixth contract method, the `repos_owner` marker that keeps a pack apply from widening a specialist, and what a scope deliberately does *not* block.
- [`hive-open-pr`](hive-open-pr.md) — how agents open pull requests as the App bot instead of via `gh pr create`: the flags, the UID-ownership anchor that makes the request forge-resistant, and the asynchronous contract (exit `0` means requested, not opened).
- [`hive-merge`](hive-merge.md) — how agents merge pull requests as the App bot instead of the GitHub MCP `merge_pull_request` tool: the flags, the F4 target-binding (pinned head SHA + governor merge-eligible list), and the retry/re-engagement behavior when required checks are still red.
- [`hive-open-issue`](hive-open-issue.md) — how agents create issues, post comments, and claim issues as the App bot instead of `gh issue create`/`gh issue comment`: the three request shapes, title dedupe (exact match first, canonical subject — trailing qualifier stripped — as the fallback, [#6927](https://github.com/hivecommons/hive/issues/6927)), and the exponential-backoff retry contract.
- [Review-bot threads on hive-mediated PRs](review-bot-threads.md) — how the hive addresses and resolves inline threads left by external review bots (Copilot, `chatgpt-codex-connector[bot]`, CodeRabbit) on PRs it opened: the `classification.review_bots` key, the `review-threads.json` monitor, the FIX-BEFORE-NEW kick block that routes each PR back to the agent that opened it, and the `hive-review --thread` / `--resolve-thread` relay whose watcher-side guard refuses to touch a human's thread ([#7360](https://github.com/hivecommons/hive/issues/7360)).
- [Network and port requirements](network-requirements.md) — inbound ports, proxy paths, egress, and firewall guidance.
- [TLS, HTTPS, and certificates](tls-setup.md) — termination patterns and certificate ownership.
- [Security notes](security.md) — log scrubbing and secret redaction guarantees/limits.
- [Token collection and usage tracking](token-tracking.md) — session JSONL, `/api/cost`, and hub usage rollups.
- [Notifications](notifications.md) — ntfy, Slack, and Discord alert channels, plus the two-way [Discord bot](notifications.md#discord-webhook-vs-discord-bot) and the [Slack Socket Mode / Telegram / Matrix / Teams bots](notifications.md#chat-bots-slack-socket-mode-telegram-matrix-teams).
- [State-triggered hooks](hooks.md) — declarative `transition → action` rules, the transition catalog, the vetted action set, and the security model (RFC #4001).
- [GitHub Actions trigger](github-actions-trigger.md) — call a hive from a workflow via the v6 comment-relay composite action, with examples for PR review and scheduled status kicks.
- [CEL-based agent triggers](cel-triggers.md) — the `triggers:` config key: declarative CEL rules that kick an agent on a normalized source-control event, additive to built-in label/governor triggering, the `event.*` field reference, and the fail-closed compile/runtime contract.
- [Jev smart classifier](jev-smart-classifier.md) — optional v6 typed-decision classifier for lane routing, haiku/sonnet/opus tier selection, and triage: prerequisites, dashboard/YAML enablement, cost, shadow stats/API, rollout, and troubleshooting.
- [Long-running runs](runs.md) — how a run starts, including default-off triage admission and inception completion admitting an explicit GitHub issue into the first `spec` lease.
- [Spektacular stage runner](spektacular.md) — the Hive side of long-running runs (#8303, umbrella #8290): a run moves through `spec` → `plan` → `implement` stages on one task lease, Spektacular owns each artifact's state while Hive owns the workflow. Covers the default-off `runs.spektacular` config block and Governor Features toggle, the `runs.engine` planning-engine selector and how to write an engine, the `max_stage_retries` budget, and what advances a lease. The design record for the artifacts a run leaves behind is [`design/run-artifacts.md`](design/run-artifacts.md).
- [Audit campaign](audit-campaign.md) — the runs three-gate model proven on a workload that never opens a pull request (#8327): owner-triggered activation, campaign/inspection/finding beads, shadow-mode execution with deterministic finding identity, per-inspection stage receipts and proof predicates, and the guard that refuses to run while `HIVE_GITHUB_TOKEN` is present. Publication remains default-off and is skipped unless `publication.enabled` is set.
- [Public snapshots](snapshots.md) — read-only `/snapshot`, custom CSS, and frame-ancestor sharing.
- [hivectl](hivectl.md) — command-line client for the dashboard API, including
  [`hivectl tui`](hivectl.md#tui--live-terminal-dashboard), the full-screen
  terminal dashboard: keybindings, pane cadence, and v1 boundaries. See
  [the design record](design/tui.md) for the reasoning behind it.
- [Admin MCP](admin-mcp.md) — administer a hive from any MCP client: the `POST /api/admin/mcp` endpoint and the `hive-admin-mcp` stdio binary, client config, multiple hives, tools, and the preview-and-confirm write flow (v6).
- [`bd` beads CLI](beads-cli.md) — work-ledger and knowledge command reference for operators and contributors.
- [Backup and restore](backup-restore.md) — `hive-backup`, Kubernetes CronJob, spoke backup scope, and setting the backup encryption key from Governor Config (hosted flow). Host-level backup, restore, and `docker compose down -v` are given per runtime: Docker Compose, and Podman/Quadlet with the executed backup → wipe → restore cycle in both root modes, the rootless mapped-UID trap that makes a host-shell `tar` skip the GitHub App key, and the Docker→Podman migration (the two volume stores are never shared). Also covers restoring a `pkg/spokebackup` archive: the archive-path → container-path mapping, and the executed `hive-backup restore` path that applies it — including the identity guard that refuses to splice one hive's config and GitHub App keys onto another's ([#6529](https://github.com/hivecommons/hive/issues/6529)).
- [Hub disaster recovery](../../docs/HUB_DISASTER_RECOVERY.md) — the hub-level runbook that goes beyond per-hive backup: hub backup and key escrow, spoke fleet recovery, Slack blast, and the full rebuild-from-zero procedure after a catastrophic loss.
- [Deployment helper scripts](deployment-scripts.md) — the all-in-one LXC setup, Proxmox LXC, and blue-green Compose helpers. All are Docker-only; the page states each script's runtime scope and where a Podman operator should go instead.
- [`bin/` pipeline script index](../../bin/README.md) — map of the deterministic pipeline and operational scripts, grouped by function.
- [Dashboard API reference](api-reference.md) — pragmatic route index for dashboard and hub endpoints.
- [Dashboard OpenAPI spec](../../dashboard/openapi.json) — machine-readable REST API reference for integrations.
- [ioscan status](ioscan.md) — the untrusted-input scanner/canary feature (live and default-on in v4).
- [Deployment scripts](../deploy/README.md) — inventory of deployment helpers, including dashboard TTY panes and `hive-panes`.

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
