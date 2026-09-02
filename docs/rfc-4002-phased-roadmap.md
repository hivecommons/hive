# RFC #4002 Phased Delivery Roadmap — Re-entrant Conversation-as-State Turn Model

Status: planning (hold-gated) · Owner: strategist · Refs: #4002, #5555, #4000

RFC #4002 proposes making the conversation the durable state and each agent
turn a re-entrant, plain function call. It touches the core execution loop of
every agent. This document phases the delivery so partial work cannot land
against the existing model without a migration story.

## Compatibility contract (must hold in every phase)

A re-entrant turn MUST be able to reconstruct everything it needs from:

1. **The conversation** — the ordered operation list (LLM call, tool-approval,
   tool-execution, compaction, elicitation, max-turns, retry, subagent-sync,
   hooks) persisted outside the process.
2. **A structured turn return** — each turn ends with a serializable result;
   no state may be suspended in an in-process coroutine between turns.
3. **The existing session/context model** — until Phase 3, the current
   in-process loop remains the default; the re-entrant path is opt-in and
   must not change observable behavior for agents that do not opt in.

Breaking changes to the turn loop require an UPGRADE.md entry (see PR #5559)
before they ship.

## Phase 0 — Spike and state inventory (exit: written report)

- Document hive's current agent-loop/state model and enumerate every place
  in-process suspended state exists (RFC scope item 1).
- Decide the minimal state envelope for handoff (RFC scope item 3).
- Deliverable: feasibility + migration-cost report attached to #4002.
- **Gate to Phase 1:** report reviewed by a human maintainer; go/no-go
  recorded on #4002.

## Phase 1 — Contributor agents only (opt-in)

- Prototype one contributor agent as a re-entrant turn function with fully
  externalized state (RFC scope item 2).
- Contributor agents are the lowest-blast-radius fleet: sessions are short,
  externally driven, and already tolerate reconnects (see #5090 flap history).
- **Gate to Phase 2:** zero regressions in contributor-agent CI for 2
  consecutive weeks; handoff exercised at least once across a process
  restart in a live hive.

## Phase 2 — Background agents (opt-in per agent)

- Extend to scanner/quality/architect-class background agents behind a
  per-agent config flag.
- Integrate #4000 (tool-approval as an explicit, ACMM-gated operation) as a
  first-class operation in the turn list — it is an individual operation
  within this umbrella model and should not ship separately.
- **Gate to Phase 3:** two full release cycles with the flag on for at least
  three agents; durable resume verified across a spoke roll.

## Phase 3 — Default for all agents

- Flip the default; retain the legacy loop for one deprecation release.
- Ship operator migration guidance in UPGRADE.md at the major version
  boundary where the default changes.

## Version targeting

Phases 0–1 are v5-compatible (opt-in, no default change). Phases 2–3 target
v6 unless the v5 GA bar (#5622) explicitly pulls them in. This RFC is not a
v5 GA blocker.

## Out of scope

- Queue selection / transport for cross-node handoff (decide in Phase 0).
- State-triggered hooks proposal (separate RFC; same umbrella).

