# RFC #4002 Phased Delivery Roadmap — Re-entrant Conversation-as-State Turn Model

Status: v5 phase 2/3 opt-in rollout in progress
Refs: #4002, #4000, #5799

## Compatibility contract

A re-entrant turn must reconstruct everything it needs from a persisted turn
envelope, structured input, and the existing session/context model. Until the
phase 3 soak proves safe, the current tmux loop remains the default and the
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
- Phase 2 production primitive: `pkg/convergence/mutation.Ledger` is the single
  durable ownership primitive for re-entrant handoff/adoption. It now refreshes
  under a cross-process flock and rewrites through unique synced temp files, so
  independent holders cannot both acquire the same claim or erase each other's
  disjoint claims.
- Phase 2 opt-in production path: contributor assignments can persist a
  `pkg/turn.SessionEnvelope` when `turn.reentrant.enabled` and either the
  per-agent `reentrant_turn` flag or `turn.reentrant.background_fleet_enabled`
  are set. The legacy websocket/tmux relay remains the execution default.
- Tool approvals are first-class envelope operations: every tool approval
  verdict is recorded in `SessionEnvelope.operations`, and operator decisions
  settle the same operation before resuming execution.
- Restart/replay proof: targeted `pkg/turn` tests persist an envelope through
  `FileStore`, reload it as a fresh process would, and assert the journal
  suppresses duplicate external effects.

## Phase 2 — v5 opt-in production path

- Keep `turn.reentrant.enabled` false by default in all packaged config.
- Enable one named low-blast-radius contributor/background agent with:

  ```yaml
  turn:
    reentrant:
      enabled: true
  agents:
    scanner:
      reentrant_turn: true
  ```

- Confirm envelopes appear under `/data/contributors/turn-envelopes` and that
  dashboard assignment, task completion, failure cooldowns, and approval-desk
  behaviour remain unchanged.
- If symptoms appear, roll back by setting `turn.reentrant.enabled: false` or
  `HIVE_REENTRANT_TURN_ENABLED=false`; existing envelopes are inert and can be
  kept for audit or deleted after capture.

## Phase 3 — v5+ opt-in fleet rollout and default flip

- Extend opt-in to the background fleet with
  `turn.reentrant.background_fleet_enabled: true` or
  `HIVE_REENTRANT_TURN_BACKGROUND_FLEET=true` only after the single-agent path
  has clean restart/replay evidence.
- Soak for at least two release cycles with contributor, scheduler/background,
  and approval-producing agents represented. Track restart-cost/turn-loss
  counters, envelope write errors, duplicate-effect reconciliations, pending
  approval settlements, and stale-epoch refusals.
- Default-flip migration:
  1. Announce the target release and keep the legacy loop available for one
     deprecation window.
  2. Flip packaged defaults only after soak shows no duplicate external effects
     and no unresolved approval operations.
  3. Preserve `HIVE_REENTRANT_TURN_ENABLED=false` as the one-step rollback.
  4. During rollback, leave mutation ledgers and turn envelopes in place; they
     are replay/audit state and do not force the re-entrant path on.
