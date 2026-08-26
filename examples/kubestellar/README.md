# KubeStellar Production Deployment

> **Historical v1 case study.** This example describes the launchd/`worker.sh`
> deployment used by the KubeStellar project before the current containerized
> Go runtime (branch `v4`; code under `src/`). It is kept as a worked example
> of the v1 approach and as context for the agent definitions in
> [`agents/`](agents/), which remain current-style. Do not follow the
> installation steps below for a new deployment — use the
> [Quick Start](../../README.md#quick-start) instead, and see
> [`docs/LEGACY-v1-architecture.md`](../../docs/LEGACY-v1-architecture.md)
> for the retired v1 architecture this example ran on.

Production configuration for the hive system deployed on the [KubeStellar](https://kubestellar.io) project — a CNCF Sandbox multi-cluster management platform.

## Architecture

The system runs on a Mac Mini and autonomously scans, triages, and fixes issues across 6 GitHub repositories every 15 minutes.

```
launchd (every 15 min)
  └─► worker.sh (scanner)
        ├─► GitHub API (gh issue list / pr list)
        ├─► SQLite state.db (upsert findings)
        └─► ntfy.sh (mobile notifications)

Copilot CLI (on-demand or triggered)
  ├─► fix-loop-skill.md   — reads DB, fixes actionable items
  ├─► auto-qa-skill.md    — processes Auto-QA issues + stalled PRs
  └─► hygiene-skill.md    — full operational sweep (builds, PRs, issues, deploys)
```

## Files

| File | Purpose |
|------|---------|
| `worker.sh` | Production scanner — scans 6 repos, updates SQLite, sends ntfy summaries |
| `io.kubestellar.fix-loop.plist` | macOS launchd plist — runs worker.sh every 900s (15 min) |
| `fix-loop-skill.md` | Copilot CLI skill — reads SQLite DB, triages, and fixes items autonomously |
| `auto-qa-skill.md` | Copilot CLI skill — processes Auto-QA issues and stalled copilot PRs |
| `hygiene-skill.md` | Copilot CLI skill — comprehensive hygiene sweep (builds, CI, PRs, issues, deploys, branches) |

## Repos Scanned

- `kubestellar/console` — Main console UI + API
- `kubestellar/console-kb` — Mission knowledge base (1480 missions)
- `kubestellar/console-marketplace` — Card marketplace
- `kubestellar/docs` — Documentation site
- `kubestellar/kubestellar-mcp` — MCP server
- `kubestellar/claude-plugins` — Claude integrations

## Results

As of April 2026:
- **54+ issues auto-closed** in a single day
- **6 actionable items** remaining at any given time
- **<30 min SLA** from issue filed to PR merged for auto-fixable items
- **Zero human intervention** for routine fixes (test timeouts, perf budget rebaselines, duplicate closures)

## Installation

```bash
# 1. Copy worker.sh and configure
cp worker.sh ~/.kubestellar-fix-loop/worker.sh
chmod +x ~/.kubestellar-fix-loop/worker.sh

# 2. Install launchd plist
cp io.kubestellar.fix-loop.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/io.kubestellar.fix-loop.plist

# 3. Install Copilot CLI skills
cp fix-loop-skill.md auto-qa-skill.md hygiene-skill.md ~/.claude/commands/
```

See the [case study](../kubestellar-fixer.md) for the full design rationale.
