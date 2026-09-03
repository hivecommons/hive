# RFC #4002 Phased Delivery Roadmap — Re-entrant Conversation-as-State Turn Model

Status: planning; v5 spike complete, production rollout deferred to v6  
Refs: #4002, #4000

## Compatibility contract

A re-entrant turn must reconstruct everything it needs from a persisted turn
envelope, structured input, and the existing session/context model. Until the
v6 rollout proves safe, the current tmux loop remains the default and the
re-entrant path is opt-in only. Breaking changes require operator migration
guidance.

## Delivered in v5

- State inventory: `src/docs/design/agent-turn-model.md` and
  `src/docs/design/agent-state-inventory.md`.
- Re-entrant prototype: `src/pkg/turn` with serialized envelopes, structured
  returns, scrubbed persistence, pending approvals, and journaled replay tests.
- Handoff evaluation: `src/docs/design/agent-turn-handoff.md`, which defers a
  queue until durable ownership is fixed/reused.
- Accepted design: `docs/rfc-4002-reentrant-conversation-state.md`.

## Phase 2 — v6 opt-in production path

- Fix/reuse one atomic durable ownership primitive for handoff/adoption.
- Wire one low-blast-radius production path to the envelope behind an explicit
  opt-in flag.
- Persist #4000 tool approvals as first-class ACMM-gated operations.
- Prove restart and replay behaviour with no duplicated external effects.

## Phase 3 — v6+ fleet rollout

- Extend opt-in to the background fleet.
- Run at least two release cycles with multiple agent classes enabled.
- Publish migration and rollback guidance before changing defaults.
- Flip the default only after the legacy loop has a deprecation window.
