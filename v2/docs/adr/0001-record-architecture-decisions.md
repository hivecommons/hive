# ADR-0001: Record architecture decisions

Status: Accepted

## Context

Hive's reference architecture documents the current system model, including the
single-container process model, deterministic pipeline, layered guardrails, ACMM
levels, beads ledger, hub/spoke topology, and observability surfaces. Those docs
explain how the system works, but they do not preserve why durable choices were
made or what trade-offs future maintainers should keep in mind.

## Decision

Adopt lightweight ADRs under `v2/docs/adr/`. Each ADR records one durable
architecture decision with `Status`, `Context`, `Decision`, and `Consequences`.
Retroactive ADRs may back-fill existing decisions when they are grounded in the
reference architecture or code.

## Consequences

Future architectural changes have a stable place to record rationale without
turning the reference architecture into a change log. ADRs are not a governance
bottleneck: small implementation details can remain in PRs and code comments,
while decisions that affect security boundaries, operator workflows, or public
interfaces should get an ADR.
