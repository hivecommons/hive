# `bin/` script index

The scripts in this directory are the deterministic shell/Node/Python layer around Hive's agent runtime. They enumerate work, classify it, gate merges, enforce GitHub permissions, launch/supervise CLIs, and publish operational telemetry before an LLM is asked to act.

Most production scripts are installed under `/usr/local/bin` by `bin/hive-deploy.sh`; several write machine-readable state under `/var/run/hive-metrics` for the dashboard, governor, and agent prompts. For the architecture context, see [`src/docs/architecture.md`](../src/docs/architecture.md#4-the-deterministic-pipeline).

## Pre-kick pipeline and merge gates

| Script | Stage | Purpose |
|---|---|---|
| `run-pipeline.sh` | Orchestrator | Runs configured pre-kick stages in dependency order: enumerator, classifier, gate, and monitor. Supports `--agent` and `--stage` selectors. |
| `enumerate-actionable.sh` | Enumerator | Builds `/var/run/hive-metrics/actionable.json` with actionable issues and PRs, excluding holds, blocked external-dependency work, drafts, selected labels, ADOPTERS-only PRs, and external issues missing a commit SHA. |
| `issue-classifier.sh` | Classifier | Enriches `actionable.json` with deterministic metadata such as complexity tier, model recommendation, tracker status, cluster key, lane, and architecture-review flag. |
| `architecture-detector.sh` | Classifier | Adds architecture signals to actionable issues from `hive-project.yaml` rules so the classifier can route them to the architect lane. |
| `pr-cluster-detector.sh` | Classifier | Groups related actionable issues into clusters using component, reporter timing, label-combo, and failure-mode signals. |
| `hive-baseline-check.sh` | Classifier | Compares one exact failing check with the repository's default branch and open sibling PRs, returning shared/isolated/unknown so agents do not retry a repository-wide incident per PR. |
| `merge-gate.sh` | Gate | Writes `/var/run/hive-metrics/merge-eligible.json`; PRs qualify only when required CI passes, they are not drafts or excluded, and author/review policy allows merge. |
| `conflict-sweeper.sh` | Gate/enforcer | Processes AI-authored PRs with `mergeable=CONFLICTING`, attempts a rebase, force-pushes clean rebases, or closes unrebasable PRs and reopens the original issue. |
| `copilot-comment-checker.sh` | Monitor | Prefetches unaddressed Copilot review comments into `/var/run/hive-metrics/copilot-comments.json` for reviewer agents. |
| `ga4-anomaly-detector.sh` | Monitor | Compares recent GA4 error counts with a 7-day baseline and writes `/var/run/hive-metrics/ga4-anomalies.json`; emits a no-data result if GA4 is unavailable. |
| `outreach-tracker.sh` | Monitor | Tracks outreach PRs opened or merged on external repositories and writes `/var/run/hive-metrics/outreach-prs.json`. |
| `contributor-dispatcher.sh` | Monitor | Reports current contributor assignment state from `/var/run/hive-metrics/contributors.json` into `/var/run/hive-metrics/contributor-assignments.json`; assignment itself happens in the dashboard WebSocket server. |

## Agent launch, supervision, and scheduling

| Script | Stage | Purpose |
|---|---|---|
| `agent-launch.sh` | Launcher | Unified AI CLI launcher for backends such as Claude and Copilot. Reads `--backend`/`--model` flags or `AGENT_BACKEND`/`AGENT_MODEL` environment variables. |
| `supervisor.sh` | Supervisor | Keeps a tmux session alive with an AI CLI agent and supports optional rate-limit failover between backends. |
| `supervisor-kick.sh` | Supervisor | Reliably restarts or creates a Copilot-backed session, sends one kick message, and verifies the agent began processing. |
| `kick-agents.sh` | Kick sender | Sends work orders directly to named tmux sessions (`scanner`, `ci-maintainer`, `architect`, `outreach`, or `all`), handles backend rate-limit switching, and calls `run-pipeline.sh`. |
| `kick-governor.sh` | Scheduler | Legacy adaptive governor that measures issue/PR backlog, chooses surge/busy/quiet/idle cadences, records kick audit entries, and calls `kick-agents.sh`. |
| `agent-healthcheck.sh` | Health | Watches an agent log's mtime, kills stalled tmux sessions for supervisor respawn, and escalates after repeated failed respawns. |
| `kick-outcome-tracker.sh` | Metrics | Correlates governor kicks with queue, MTTR, and token outcomes after a cooldown period. |
| `gh-rate-check.sh` | Health | Scans tmux panes for GitHub API rate-limit messages, writes `/var/run/hive-metrics/gh_rate_limits.json`, and temporarily pauses affected CLI cohorts. |
| `gh-zombie-reaper.sh` | Health | Kills long-running `gh` processes older than the configured threshold to stop rate-limit retry storms. |
| `ttyd-tmux.sh` | Terminal UI | Wraps ttyd-to-tmux sessions and disables tmux mouse mode while the browser session is attached so normal text selection works. |

## GitHub credentials, policy enforcement, and PR creation

| Script | Stage | Purpose |
|---|---|---|
| `gh-app-token.sh` | Credentials | Generates and caches (0600, hub-only) a GitHub App installation token; `--export` prints shell exports for callers; `--scoped <tier> [repos]` prints a JSON tier-scoped token for a contributor agent and never touches the shared cache. |
| `git-credential-hive.sh` | Credentials | Git credential helper that serves cached GitHub App tokens and honors the host requested by Git. |
| `gh-wrapper.sh` | Enforcement | `gh` wrapper that injects App tokens and enforces global/per-agent restriction rules from `/etc/hive/restrictions/<agent-id>.json`. |
| `hive-open-pr.sh` | Enforcement | Agent-side wrapper for PR creation requests. It writes a request file for the Hive watcher so PRs are opened by the GitHub App bot and pass the same ACMM authorization checks. See [`src/docs/hive-open-pr.md`](../src/docs/hive-open-pr.md). |
| `hive-open-issue.sh` | Enforcement | Agent-side wrapper for issue creation and comments. Agents call it INSTEAD of `gh issue create` / `gh issue comment`; it writes a request file for the Hive watcher so the work is attributed to the GitHub App bot and passes the same ACMM authorization checks. See [`src/docs/hive-open-issue.md`](../src/docs/hive-open-issue.md). |
| `hive-merge.sh` | Enforcement | Agent-side wrapper for merging a PR. Agents call it INSTEAD of the GitHub MCP `merge_pull_request` tool, so the merge is performed by the App bot under the same ACMM gating rather than with an agent's own credential. See [`src/docs/hive-merge.md`](../src/docs/hive-merge.md). |
| `hive-review.sh` | Enforcement | Agent-side wrapper for `gh pr review`. Writes a review-request file the Hive submits with the App token and audits as `agent_pr_reviewed`, so PR-review activity is attributed and visible to hive-health (a direct `gh pr review` is invisible). |
| `setup-proxy-iptables.sh` | Enforcement | Installs iptables rules in the container to force GitHub HTTPS traffic through the ACMM proxy even if an agent unsets proxy variables. |
| `agent-env-scrub.sh` | Enforcement | Sourced (never executed) at the start of every shell in an agent's process tree, via `BASH_ENV`/`ENV` from `agent-launch.sh` and an `/etc/bash.bashrc` guard, to unset the live GitHub credentials backend CLIs re-export into agent tool shells (#4045). |
| `hive-config.sh` | Config | Shared shell config reader that exposes project, repo, agent, dashboard, health, and policy values parsed from `hive-project.yaml`. |

## Deployment and local operation

| Script | Stage | Purpose |
|---|---|---|
| `hive.sh` | CLI | Operator command wrapper for starting the supervisor, checking status, attaching, kicking agents, reading logs, and stopping agents. |
| `hive-setup.sh` | Bootstrap | All-in-one Ubuntu 24.04 LXC setup for a Docker-based Hive v2 instance, including generated `hive.yaml` and env files. **Docker-only**: it installs Docker Engine and drives `docker compose`, and predates `HIVE_DEPLOY_RUNTIME`, so that variable does not redirect it. For a Podman install follow the [README's Quick Start (Podman)](../README.md#quick-start-podman); see [Deployment helper scripts](../src/docs/deployment-scripts.md#all-in-one-lxc-setup). |
| `hive-prereq-check.sh` | Bootstrap | Validates host prerequisites for a Docker-based Hive v2 install and can attempt fixes with `--fix`. |
| `hive-deploy.sh` | Deploy | Pulls the latest Hive repository and syncs scripts to `/usr/local/bin`; also restarts Discord bot components when their files change. |
| `hive-standalone-runtime.sh` | Deploy | Selects the standalone container engine from `HIVE_DEPLOY_RUNTIME` (`docker` by default) and runs commands against it without ever falling back to the other engine. |
| `hive-podman-cleanup.sh` | Deploy | Defines the Hive ownership labels for Podman containers, pods, networks, volumes, and images, and guards cleanup so it can never widen into a store-wide Podman/Buildah operation. See [`src/docs/podman-ownership-cleanup.md`](../src/docs/podman-ownership-cleanup.md). |
| `hive-podman-teardown.sh` | Deploy | Tears down one standalone Podman deployment through the #4210 ownership contract: every resource is selected by the ownership label, every command is passed through the cleanup guard before it runs, and a command the guard rejects aborts the teardown. Plans by default; `run --yes` removes. Unlabelled containers, pods, volumes, and networks are invisible to it. See [`src/docs/podman-ownership-cleanup.md`](../src/docs/podman-ownership-cleanup.md). |
| `hive-podman-preflight.sh` | Bootstrap | Read-only Podman diagnostics before a lifecycle runs: engine/version, the connection it is actually talking to, rootless vs rootful, and cgroup version. Runs only when `HIVE_DEPLOY_RUNTIME` selects podman; `hive-prereq-check.sh` invokes it. |
| `hive-podman-preflight-host.sh` | Deploy | Read-only Podman preflight for SELinux state and mount labeling, configuration/secrets readability, and published host-port availability. Runs only when `HIVE_DEPLOY_RUNTIME=podman`. See [`src/docs/podman-preflight-host.md`](../src/docs/podman-preflight-host.md). |
| `hive-podman-preflight-ids.sh` | Deploy | Read-only Podman preflight for rootless subordinate UID/GID delegation, unsupported (NFS and other distributed) container storage, and the rootless network backend/helper. Never edits `/etc/subuid` or `/etc/subgid`. Runs only when `HIVE_DEPLOY_RUNTIME=podman`. See [`src/docs/podman-preflight-ids.md`](../src/docs/podman-preflight-ids.md). |
| `hive-podman-setup.sh` | Bootstrap | One-command standalone Podman install (#4470), the Podman counterpart to `hive-setup.sh`'s Docker path. |
| `hive-podman-update.sh` | Deploy | Deliberate manual update and rollback for the Hive Quadlet unit (#4378). Updates are explicit rather than automatic, per ADR-0017. |
| `hive-podman-lifecycle-probe.sh` | Deploy | Exercises the Quadlet lifecycle — stop, start, restart, recreate, and boot wiring (#4377) — to verify the unit behaves correctly across each transition. |
| `federation-heartbeat.sh` | Federation | Sends live contributor and actionable-work stats to the Hive federation registry. |
| `notify.sh` | Notifications | Shared Bash notification library for ntfy, Slack incoming webhooks, and Discord webhooks. |

## Contributor relay

| Script | Stage | Purpose |
|---|---|---|
| `contributor-agent.sh` | Contributor runtime | Contributor-container entrypoint: detects authenticated CLI backend, starts the relay, launches the CLI in tmux, and creates `${HOME}/agent.md` only from a verified live knowledge export. |
| `contributor-relay.sh` | Contributor runtime | Node.js WebSocket client for ClankeR contributor agents. It authenticates to one or more hubs, receives tasks, injects GitHub tokens, reports progress/results, and supports interactive tmux or headless one-shot delivery. |
| `pi-backend.js` | Contributor runtime | Pi contributor adapter contract (#5039). `AGENT_MODEL` is the one contributor-owned selection input; the adapter derives the provider's official credential variable names from it so only the selected provider's keys are handed to the container. |

## Model, token, and experiment helpers

| Script | Stage | Purpose |
|---|---|---|
| `copilot-models.mjs` | Model discovery | Uses `@github/copilot-sdk` and the installed Copilot CLI's own auth to print the available Copilot model catalog as one JSON line. |
| `token-collector.sh` | Cost metrics | Reads scanner beads and token metrics to attribute issue cost; supports recent windows and `--all`. |
| `token-usage.py` | Cost metrics | Aggregates Claude and Copilot CLI session token usage by agent and rolling time window. |
| `nous-install.sh` | Nous | Clones or updates the external Nous strategy-evolution framework under `NOUS_DIR`, creates a venv, installs it editable, and prepares run directories. |
| `nous-runner.sh` | Nous | Strategist helper that invokes the installed Nous framework on each kick. |
| `nous-hive-gate.py` | Nous | Implements the Nous Gate protocol, posting suggest-mode decisions to the dashboard and polling for operator approval. |
| `nous-sync.py` | Nous | Reads Nous governor/repo outputs, applies confidence decay and conflict detection, and writes principles, recommendations, and ledger data. |

## Regression tests

| Script | Covers |
|---|---|
| `contributor-agent.test.sh` | Contributor-agent regression for knowledge export handling. |
| `contributor-relay.test.js` | Contributor relay task/restart/headless behavior; loads `contributor-relay.sh` as JavaScript with stubs. |
| `gh-wrapper.test.sh` | `gh-wrapper.sh` author-gate and restriction regressions using a mock `gh` binary. |
| `test_hive_baseline_check.sh` | Shared-CI classifier regressions for red default branches, exact-name sibling thresholds, reruns, pending checks, and fail-closed API errors. |
| `test_bin_suites_wired.sh` | Fails when a test suite in this directory is not run by any workflow, Justfile target, or hook (#4363). |
| `test_hive_standalone_runtime.sh` | `hive-standalone-runtime.sh` engine selection: Docker default, explicit Podman, and no silent fallback. |
| `test_hive_podman_cleanup.sh` | `hive-podman-cleanup.sh` ownership labels and cleanup guard. Analyses arguments only: it contacts no container engine and deletes nothing. |
| `test_hive_podman_teardown.sh` | `hive-podman-teardown.sh` against a fake Podman whose store holds Hive's resources next to a Distrobox, an unrelated development container, and somebody's Postgres volume. Asserts the unowned resources survive every teardown, replays every command the teardown issued through the #4210 guard, and checks that a hostile resource name aborts the run instead of widening it. |
| `test_hive_podman_preflight.sh` | `hive-podman-preflight.sh` engine, connection, root-mode, and cgroup checks. Drives a stub engine, so the whole matrix runs on a host with no Podman installed. |
| `test_hive_podman_preflight_host.sh` | `hive-podman-preflight-host.sh` across enforcing, permissive, and disabled SELinux, wrong and missing mount labels, unreadable and over-permissive secrets, and occupied and sub-floor ports. Every input is mocked; it runs no container and changes nothing on the host. |
| `test_hive_podman_preflight_ids.sh` | `hive-podman-preflight-ids.sh` across delegated, missing, short, and multi-range subordinate IDs, NFS and other unsupported graphroots, tmpfs, a missing rootless network helper, and rootful hosts. Every input is mocked; `PATH` is the fake bin alone, so no real helper on the test machine can mask an absent one. |
