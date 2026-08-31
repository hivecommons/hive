# ADR-0004: Beads work ledger

Status: Accepted (retroactive)

## Context

Hive agents need durable coordination state that is separate from GitHub's issue
queue. The reference architecture describes beads as a git-backed JSON ledger
per agent, with typed work items, priorities, dependencies, metadata, and
actor-scoped ready queues ([architecture §7](../architecture.md#7-beads--the-work-ledger)).
The deterministic pipeline can turn GitHub work into internal artifacts before
agents act ([architecture §4](../architecture.md#4-the-deterministic-pipeline)).

## Decision

Use beads as Hive's internal work ledger. Agents claim ready work with their
actor identity, record progress and dependencies in the ledger, and close beads
when associated work is merged or otherwise complete. Advisory findings and
blocked-write observations stay in the ledger/governor digest rather than being
mirrored automatically to GitHub.

## Consequences

Hive gains a durable, inspectable coordination layer without requiring a central
queue service. Agents can coordinate on dependencies and priorities even when the
external GitHub issue does not map one-to-one to internal tasks. The trade-off is
that operators must understand both GitHub issues/PRs and beads: beads are
source-of-truth for internal agent work, while GitHub remains the public project
interface.
