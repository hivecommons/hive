# `bin/` script index

The scripts in this directory are the deterministic shell/Node/Python layer around Hive's agent runtime. They enumerate work, classify it, gate merges, enforce GitHub permissions, launch/supervise CLIs, and publish operational telemetry before an LLM is asked to act.

Most production scripts are installed under `/usr/local/bin` by `bin/hive-deploy.sh`; several write machine-readable state under `/var/run/hive-metrics` for the dashboard, governor, and agent prompts. For the architecture context, see [`v2/docs/architecture.md`](../v2/docs/architecture.md#4-the-deterministic-pipeline).

## Pre-kick pipeline and merge gates

| Script | Stage | Purpose |
|---|---|---|
| `run-pipeline.sh` | Orchestrator | Runs configured pre-kick stages in dependency order: enumerator, classifier, gate, and monitor. Supports `--agent` and `--stage` selectors. |
| `enumerate-actionable.sh` | Enumerator | Builds `/var/run/hive-metrics/actionable.json` with actionable issues and PRs, excluding holds, drafts, selected labels, ADOPTERS-only PRs, and external issues missing a commit SHA. |
| `issue-classifier.sh` | Classifier | Enriches `actionable.json` with deterministic metadata such as complexity tier, model recommendation, tracker status, cluster key, lane, and architecture-review flag. |
| `architecture-detector.sh` | Classifier | Adds architecture signals to actionable issues from `hive-project.yaml` rules so the classifier can route them to the architect lane. |
| `pr-cluster-detector.sh` | Classifier | Groups related actionable issues into clusters using component, reporter timing, label-combo, and failure-mode signals. |
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
| `gh-app-token.sh` | Credentials | Generates and caches a GitHub App installation token; `--export` prints shell exports for callers. |
| `git-credential-hive.sh` | Credentials | Git credential helper that serves cached GitHub App tokens and honors the host requested by Git. |
| `gh-wrapper.sh` | Enforcement | `gh` wrapper that injects App tokens and enforces global/per-agent restriction rules from `/etc/hive/restrictions/<agent-id>.json`. |
| `hive-open-pr.sh` | Enforcement | Agent-side wrapper for PR creation requests. It writes a request file for the Hive watcher so PRs are opened by the GitHub App bot and pass the same ACMM authorization checks. |
| `setup-proxy-iptables.sh` | Enforcement | Installs iptables rules in the container to force GitHub HTTPS traffic through the ACMM proxy even if an agent unsets proxy variables. |
| `hive-config.sh` | Config | Shared shell config reader that exposes project, repo, agent, dashboard, health, and policy values parsed from `hive-project.yaml`. |

## Deployment and local operation

| Script | Stage | Purpose |
|---|---|---|
| `hive.sh` | CLI | Operator command wrapper for starting the supervisor, checking status, attaching, kicking agents, reading logs, and stopping agents. |
| `hive-setup.sh` | Bootstrap | All-in-one Ubuntu 24.04 LXC setup for a Docker-based Hive v2 instance, including generated `hive.yaml` and env files. |
| `hive-prereq-check.sh` | Bootstrap | Validates host prerequisites for a Docker-based Hive v2 install and can attempt fixes with `--fix`. |
| `hive-deploy.sh` | Deploy | Pulls the latest Hive repository and syncs scripts to `/usr/local/bin`; also restarts Discord bot components when their files change. |
| `federation-heartbeat.sh` | Federation | Sends live contributor and actionable-work stats to the Hive federation registry. |
| `notify.sh` | Notifications | Shared Bash notification library for ntfy, Slack incoming webhooks, and Discord webhooks. |

## Contributor relay

| Script | Stage | Purpose |
|---|---|---|
| `contributor-agent.sh` | Contributor runtime | Contributor-container entrypoint: detects authenticated CLI backend, starts the relay, launches the CLI in tmux, and creates `${HOME}/agent.md` only from a verified live knowledge export. |
| `contributor-relay.sh` | Contributor runtime | Node.js WebSocket client for ClankeR contributor agents. It authenticates to one or more hubs, receives tasks, injects GitHub tokens, reports progress/results, and supports interactive tmux or headless one-shot delivery. |

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
