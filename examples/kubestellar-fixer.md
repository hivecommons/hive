# KubeStellar fixer case study

This example documents the original KubeStellar fixer pattern referenced by the launchd and worker examples in this repository.

## Goal

Run a small supervised worker on an always-on machine that repeatedly finds actionable KubeStellar issues, asks an AI coding CLI to fix one item, opens a signed PR, and reports status without giving the agent direct control over scheduling or credentials.

## Pieces

- `examples/worker.sh.example` shows the outer worker loop.
- `launchd/com.hive.scanner.plist.example` shows how macOS keeps the loop alive.
- `bin/enumerate-actionable.sh`, `bin/run-pipeline.sh`, and `bin/merge-gate.sh` are the deterministic shell gates that decide what reaches the agent.
- Agent policy files under `examples/kubestellar/agents/` define lane boundaries and heartbeat rules.

## Flow

1. The scheduler starts the worker on a cadence.
2. The deterministic pipeline lists candidate issues/PRs and filters out unsafe or already-claimed work.
3. The agent receives one bounded task plus policy instructions.
4. The worker verifies tests/builds and opens a PR rather than pushing to the protected branch.
5. Merge gates and humans decide whether the PR lands.

## Why this pattern matters

The important design choice is separation of duties: shell scripts own credentials, filtering, and merge gates; the AI agent owns reasoning and code changes. That makes failures observable and keeps policy enforceable even when an agent misunderstands a prompt.

## Adapting it

- Replace the KubeStellar repo list with your own `project.repos`.
- Start with advisory or issue-only mode before enabling PR creation.
- Keep a heartbeat log and stale timeout longer than the worker cadence.
- Require signed commits and CI before merge.
- Prefer v2 ACMM packs for new deployments; use this case study when you need the older standalone worker shape.
