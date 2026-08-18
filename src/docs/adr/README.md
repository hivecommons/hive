# Architecture Decision Records

Hive uses lightweight Architecture Decision Records (ADRs) to capture decisions
that shape the system's security model, operating model, and long-lived APIs.
ADRs are intentionally short: enough context to understand the choice, the
chosen direction, and consequences for future changes.

## Process

1. Copy the template below to `NNNN-short-title.md` using the next number.
2. Write in present tense for new decisions. For historical decisions, mark the
   status as `Accepted (retroactive)` and cite the existing docs or code.
3. Keep each ADR focused on one decision. If a later change supersedes it, add a
   new ADR and update the older status to `Superseded by ADR-NNNN`.
4. Link ADRs from related documentation when they become operator-facing.

## Template

```markdown
# ADR-NNNN: Title

Status: Proposed | Accepted | Accepted (retroactive) | Superseded by ADR-NNNN

## Context

What problem, constraint, or trade-off forced a durable architecture decision?

## Decision

What did we decide?

## Consequences

What becomes easier, harder, safer, or riskier because of this decision?
```

## Records

- [ADR-0001: Record architecture decisions](0001-record-architecture-decisions.md)
- [ADR-0002: MITM proxy network enforcement](0002-mitm-proxy-network-enforcement.md)
- [ADR-0003: ACMM autonomy levels](0003-acmm-autonomy-levels.md)
- [ADR-0004: Beads work ledger](0004-beads-work-ledger.md)
- [ADR-0005: Forge-neutral source control interface](0005-forge-abstraction.md)
- [ADR-0006: Planning intelligence with human review](0006-planning-intelligence.md)
- [ADR-0007: Mint short-lived scoped agent credentials](0007-token-mint.md)
- [ADR-0008: Redact untrusted kick input with visible markers](0008-ioscan-untrusted-input.md)
- [ADR-0009: Trajectory review lane](0009-trajectory-review.md)
- [ADR-0010: Escalation circuit breaker for CI fix loops](0010-escalation-circuit-breaker.md)
- [ADR-0011: Durable knowledge graph for agent context](0011-knowledge-graph.md)
- [ADR-0012: Skill registry and BYO-agent contract](0012-skill-registry.md)
- [ADR-0013: CEL triggers over normalized forge events](0013-cel-triggers.md)
- [ADR-0014: Hub/spoke fleet over heartbeat callbacks](0014-hub-spoke.md)
- [ADR-0015: Scope `style-src` as two directives and accept inline style attributes](0015-csp-style-src-scope.md)
- [ADR-0016: Scope `script-src` as two directives, close the element half with hashes](0016-csp-script-src-scope.md)
