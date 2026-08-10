# `bin/` — deterministic pipeline scripts

The shell/Python scripts that make up Hive's **deterministic pipeline** — the
non-LLM layer that filters, classifies, gates, and enforces before any agent is
kicked, plus the operational helpers that launch agents, wrap `gh`, and keep
sessions alive. Agents handle judgment; these scripts handle everything that
should be deterministic.

Most are invoked by the hive process, the governor, or `run-pipeline.sh` rather
than run by hand. Each script's own header comment is the authoritative
description; this index is a map.

## Pipeline (pre-kick stages)

| Script | Purpose |
|--------|---------|
| `run-pipeline.sh` | Execute the pre-kick pipeline stages in dependency order. |
| `enumerate-actionable.sh` | Canonical enumerator for actionable issues and PRs. |
| `issue-classifier.sh` | Pre-classify actionable issues with deterministic metadata. |
| `architecture-detector.sh` | Detect issues needing architecture review / lane transfer. |
| `pr-cluster-detector.sh` | Group related actionable issues into clusters for bundled dispatch. |
| `contributor-dispatcher.sh` | Report contributor assignment state. |
| `merge-gate.sh` | Determine which open PRs are eligible for merge. |
| `conflict-sweeper.sh` | Rebase or close CONFLICTING PRs. |

## Agent lifecycle

| Script | Purpose |
|--------|---------|
| `agent-launch.sh` | Unified launcher for any AI coding CLI backend. |
| `supervisor.sh` | Keep a tmux session alive with an AI CLI agent inside. |
| `supervisor-kick.sh` | Reliable agent session restart + kick (Copilot backend). |
| `agent-healthcheck.sh` | Stall detector + auto-respawn + ntfy push. |
| `kick-agents.sh` | Fire work orders at the scanner/ci-maintainer/architect/outreach sessions. |
| `kick-governor.sh` | Adaptive kick governor for supervised agents. |
| `supervisor-kick.sh` | Session restart + kick helper. |
| `kick-outcome-tracker.sh` | Correlate governor kicks with queue/MTTR/token outcomes. |
| `ttyd-tmux.sh` | Wrapper for ttyd → tmux (disables mouse mode for the browser). |

## GitHub access and safety

| Script | Purpose |
|--------|---------|
| `gh-wrapper.sh` | `gh` wrapper — enforces per-agent + global restrictions and injects the App token. |
| `gh-app-token.sh` | Generate a GitHub App installation token for the hive. |
| `git-credential-hive.sh` | Git credential helper using the GitHub App token. |
| `hive-open-pr.sh` | Open a PR via the App bot (not `gh pr create`). |
| `gh-rate-check.sh` | Scan agent tmux panes for GitHub API rate-limit messages. |
| `gh-zombie-reaper.sh` | Kill `gh` API processes older than `MAX_AGE_SECONDS`. |
| `copilot-comment-checker.sh` | Pre-fetch unaddressed Copilot review comments. |
| `setup-proxy-iptables.sh` | Force all GitHub HTTPS traffic through the ACMM proxy. |

## Contributor relay (ClankeR)

| Script | Purpose |
|--------|---------|
| `contributor-relay.sh` | ClankeR — the WebSocket client connecting a contributor agent to the hub. |
| `contributor-agent.sh` | Entrypoint for the contributor container. |

## Metrics, notifications, tokens

| Script | Purpose |
|--------|---------|
| `token-collector.sh` | Correlate bead data with token usage for per-issue cost attribution. |
| `token-usage.py` | Token usage aggregator for Hive agents. |
| `notify.sh` | Shared notification library for hive scripts. |
| `federation-heartbeat.sh` | Periodically send this hive's live stats to the federation. |
| `outreach-tracker.sh` | Track outreach PRs opened/merged on external repos. |
| `ga4-anomaly-detector.sh` | Pre-compute GA4 error anomalies for the ci-maintainer agent. |
| `copilot-models.mjs` | List Copilot models available to this machine's auth (via `@github/copilot-sdk`). |

## Setup and deployment

| Script | Purpose |
|--------|---------|
| `hive.sh` | KubeStellar AI hive entrypoint. |
| `hive-config.sh` | Shared config reader for all hive scripts. |
| `hive-setup.sh` | Bootstrap a fresh Ubuntu 24.04 LXC into a Hive v2 instance (Docker). |
| `hive-prereq-check.sh` | Validate prerequisites for a Docker-based Hive v2 deployment. |
| `hive-deploy.sh` | Pull the latest hive repo and sync scripts to `/usr/local/bin`. |

## Strategy Lab (Nous)

| Script | Purpose |
|--------|---------|
| `nous-install.sh` | Install the Nous framework into `/opt/nous` (venv-based). |
| `nous-runner.sh` | Strategist helper — invokes the real Nous framework. |
| `nous-hive-gate.py` | Custom Nous Gate — bridges experiment-approval decisions into Hive. |
| `nous-sync.py` | Analyst helper — reads Nous output and syncs findings to the dashboard. |

## Tests

`*.test.sh` / `*.test.js` files are regression tests for the scripts alongside
them (e.g. `gh-wrapper.test.sh`, `contributor-agent.test.sh`,
`contributor-relay.test.js` — the last is JavaScript despite the extension).
