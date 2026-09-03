---
title: Agent Roles and Responsibilities
tags: [agents, roles, reference]
---

# Agent Roles and Responsibilities

Hive deploys several specialized agents, each with a distinct role. The
Governor controls when each agent wakes based on the current mode and
repository queue depth.

## Scanner

The first-responder agent. Scans open issues and PRs for actionable items,
triages new issues, and handles quick fixes. Runs frequently in all modes.

## Quality

Owns test suites, coverage gates, and regression checks. Typically the first
agent an operator grants write access to when moving to L3.

## Guide

Audits documentation, onboarding material, and contributor experience,
identifying gaps that make the project harder to pick up.

## Brainstorm

Idea incubation. **Advisory only** — it files beads, never GitHub issues or
pull requests, at every level. Commonly left paused until an operator wants
its output.

## CI Maintainer

Monitors CI pipelines, fixes flaky tests, updates workflows, and ensures
nightly jobs stay green. Activated when CI failures or stale workflows are
detected.

## Architect

Handles large refactors, new features, and cross-cutting design work. Only
activated in idle mode when the queue is clear, giving it long uninterrupted
sessions.

## Supervisor

Observes agent behavior and repository health. Reports anomalies but does
**not** take direct action -- it is strictly observe-and-report. Runs in
every mode.

## Outreach

Manages community engagement, documentation updates, and external
communications. Activated during idle periods.

## Sec-Check

Security-focused agent. Reviews code for vulnerabilities, checks
dependencies, and audits access patterns. Runs frequently across all modes.

## Strategist

Long-horizon planning agent. Analyzes trends, proposes roadmap items, and
evaluates technical debt. Only activated in idle mode.

## A note on testing

There is no built-in `tester` agent. Test suites, coverage gates, and
regression checks are the `quality` lane's work. An operator who wants a
separate tester must define it as a custom agent with its own metadata and
policy template — see
[acmm-policy-matrix.md](https://github.com/hivecommons/hive/blob/v4/src/docs/acmm-policy-matrix.md).

## Adding a New Agent

To add an agent, define it in `hive.yaml` under `agents:` with at minimum:

```yaml
my-agent:
  enabled: true
  backend: copilot
  model: claude-opus-4.6
  caveman_mode: full                    # lite | full | ultra | wenyan — compresses output ~65%
  beads_dir: /data/beads/my-agent
  stale_timeout: 1800
  restart_strategy: immediate
  launch_cmd: "/usr/bin/copilot --allow-all --model claude-opus-4.6"
```

Then add cadence entries for each governor mode.
