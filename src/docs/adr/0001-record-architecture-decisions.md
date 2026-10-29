# ADR-0001: Record architecture decisions

Status: Accepted

An **Architecture Decision Record (ADR)** explains why a durable technical
choice was made. This series is for maintainers; for setup and daily operation,
start with the relevant operator guide.

## Context

Hive's reference architecture documents the current system model, including the
single-container process model, deterministic work pipeline, layered guardrails,
[ACMM autonomy levels](../acmm-policy-matrix.md), the [beads work ledger](0004-beads-work-ledger.md),
[hub/spoke fleet topology](0014-hub-spoke.md), and monitoring surfaces. Those docs
explain how the system works, but they do not preserve why durable choices were
made or what trade-offs future maintainers should keep in mind.

## Decision

Adopt lightweight ADRs under `src/docs/adr/`. Each ADR records one durable
architecture decision with `Status`, `Context`, `Decision`, and `Consequences`.
Retroactive ADRs may back-fill existing decisions when they are grounded in the
reference architecture or code.

## Consequences

Future architectural changes have a stable place to record rationale without
turning the reference architecture into a change log. ADRs are not a governance
bottleneck: small implementation details can remain in PRs and code comments,
while decisions that affect security boundaries, operator workflows, or public
interfaces should get an ADR.
