---
title: Getting Started with Hive
tags: [overview, onboarding]
---

# Getting Started with Hive

Hive is an autonomous multi-agent system that manages software repositories
using AI-powered agents. Each agent has a specific role and operates within
governance boundaries set by the **Governor**.

## Core Concepts

- **Governor** -- evaluates repository state (open issues, PRs, SLA breaches)
  and decides which agents to wake and how often.
- **Modes** -- the governor selects a mode (surge, busy, quiet, idle) based on
  queue depth. Each mode defines cadences for every agent.
- **Beads** -- structured logs of each agent session. Beads capture what the
  agent was asked, what it did, and the outcome.
- **Knowledge Layer** -- a wiki of facts (patterns, gotchas, regressions)
  extracted from merged PRs and fed back to agents as primer context.

## Quick Links

- Dashboard: accessible on the configured port (default 3001)
- Policies: stored in the `policies` repo path and hot-reloaded
- Vaults: Obsidian-compatible markdown directories auto-indexed by Hive

## Editing This Wiki

This vault is an Obsidian-compatible directory of markdown files. You can:

1. Edit files directly on disk
2. Open the vault in Obsidian and enable the **Obsidian Git** community plugin
3. Push changes to the configured git remote -- Hive will pull them
   automatically every 60 seconds
