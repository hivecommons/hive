# Architecture Decision Records

Hive uses lightweight Architecture Decision Records (ADRs) to capture decisions
that shape the system's security model, operating model, and long-lived APIs.
ADRs are intentionally short: enough context to understand the choice, the
chosen direction, and consequences for future changes.

**Audience and reading route:** maintainers use these records for rationale
and implementation contracts, not installation steps. Operators should start
with [Securing your hive](../securing-your-hive.md) or
[Troubleshooting](../troubleshooting.md). Read a record's current-state note
before its historical context or amendments. **Accepted** records document a
decision, not proof that a control is enabled on your deployment; **Proposed**
records are not setup instructions. Retroactive/back-filled records explain
choices already present when recorded.

Suggested first records: [autonomy levels](0003-acmm-autonomy-levels.md) for
permission boundaries, [network enforcement](0002-mitm-proxy-network-enforcement.md)
for the proxy, [work ledger](0004-beads-work-ledger.md) for agent coordination,
and [escalation](0019-escalation-over-stalling.md) for human decisions. The
numbered list remains the complete chronological index.

## Process

1. Copy the template below to `NNNN-short-title.md` using the next number.
   Numbers below `0100` are allocated on `v5` first; on `v6`, ADRs that exist
   only on `v6` use the reserved `0100`+ block so forward-merges from `v5`
   never collide with them (hivecommons/hive#12816).
2. Write in present tense for new decisions. For historical decisions, mark the
   status as `Accepted (retroactive)` or `Accepted (back-filled)` and cite the existing docs or code.
3. Keep each ADR focused on one decision. If a later change supersedes it, add a
   new ADR and update the older status to `Superseded by ADR-NNNN`.
4. Link ADRs from related documentation when they become operator-facing.

## Template

```markdown
# ADR-NNNN: Title

Status: Proposed | Accepted | Accepted (retroactive) | Accepted (back-filled) | Superseded by ADR-NNNN

## Context

What problem, constraint, or trade-off forced a durable architecture decision?

## Decision

What did we decide?

## Consequences

What becomes easier, harder, safer, or riskier because of this decision?
```

## Records

All records below are **Accepted** (including retroactive/back-filled records)
unless explicitly marked **Proposed**. ADR-0015/0016 preserve historical CSP
sequencing; ADR-0019/0025 give their current action/mode contract first.

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
- [ADR-0011: Knowledge system for durable agent context](0011-knowledge-system.md)
- [ADR-0012: Skill registry and BYO-agent contract](0012-skill-registry.md)
- [ADR-0013: CEL triggers over normalized forge events](0013-cel-triggers.md)
- [ADR-0014: Hub/spoke fleet over heartbeat callbacks](0014-hub-spoke.md)
- [ADR-0015: Scope `style-src` as two directives and accept inline style attributes](0015-csp-style-src-scope.md)
- [ADR-0016: Scope `script-src` as two directives, close the element half with hashes](0016-csp-script-src-scope.md)
- [ADR-0017: Quadlet `.container`/`.pod` units as the Podman persistent lifecycle](0017-podman-quadlet-lifecycle.md)
- [ADR-0018: Shared dashboard design tokens and component layer](0018-dashboard-design-tokens.md)
- [ADR-0019: Escalate to direction, spec, signal, or meta-issue instead of stalling](0019-escalation-over-stalling.md)
- [ADR-0020: External work sources over a versioned HTTP/JSON contract](0020-external-work-source-boundary.md)
- [ADR-0021: Named planning-engine boundary for Project Inception runs](0021-planning-engine-boundary.md)
- [ADR-0022: Opt-in redemption of earned Codex resets](0022-opt-in-earned-codex-reset-redemption.md)
- [ADR-0023: Hive may merge PRs whose author could merge them](0023-trusted-author-automerge.md)
- [ADR-0024: Provider credential lifecycle](0024-provider-credential-lifecycle.md) — **Proposed**, not a credential-pool or refresh-service setup guide.
- [ADR-0025: One stage engine derives and writes every `stage/*` label](0025-central-stage-engine.md)
- [ADR-0026: Sandbox runtime selection](0026-sandbox-runtime-selection.md) (Proposed)

### v6-only ADRs (`0100`+)

- [ADR-0100: Spek continuous convergence](0100-spek-continuous-convergence.md) (v6 addendum: run-rewind semantics for recheck)
