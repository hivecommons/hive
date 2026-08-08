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
