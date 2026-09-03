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

## Already delivered before implementation

The investigation work is intentionally separate from rollout:

- **Stage 1: state inventory shipped.**
  [`src/docs/design/agent-turn-model.md`](../src/docs/design/agent-turn-model.md)
  documents hive's current agent-loop/state model and the in-process state that
  would not survive a process restart.
- **Stage 2: prototype shipped.** The isolated `pkg/turn` prototype demonstrates
  a re-entrant `Step` over a serialized conversation envelope with journaled
  external effects. It is not wired into the live tmux loop or contributor relay.
- **Step 3: handoff evaluation documented.**
  [`src/docs/design/agent-turn-handoff.md`](../src/docs/design/agent-turn-handoff.md)
  records the handoff finding: do not add a queue yet; fix/reuse durable
  ownership first, and avoid creating another durable state store.

These shipped artifacts do not change runtime behavior. They are the evidence
base for the opt-in rollout below.

## Phase 1 — Contributor agents only (opt-in)

- Wire one contributor-agent path to the re-entrant turn envelope behind an
  explicit opt-in flag.
- Contributor agents are the lowest-blast-radius fleet: sessions are short,
  externally driven, and already tolerate reconnects (see #5090 flap history).
- Validate the compatibility contract end-to-end: persisted envelope, structured
  return, no suspended in-process state between turns, and no observable change
  for non-opted-in agents.
- **Gate to Phase 2:** zero regressions in contributor-agent CI for 2
  consecutive weeks; handoff exercised at least once across a process restart
  in a live hive; no unreconciled ambiguous journal entry may complete as
  success.

## Phase 2 — All background agents (opt-in per agent)

- Extend the opt-in path to every background-agent class (scanner, quality,
  architect, security, guide, strategist, reviewer, and other non-contributor
  scheduled/manager-driven agents). This is not a partial background rollout:
  Phase 2 is the point where the whole background fleet has an opt-in path.
- Integrate #4000 (tool-approval as an explicit, ACMM-gated operation) as a
  first-class operation in the turn list — it is an individual operation within
  this umbrella model and should not ship separately.
- Keep the legacy in-process loop available and default while per-agent opt-in
  burns down compatibility issues.
- **Phase-2 success gate:** two full release cycles with the flag enabled for at
  least three different background-agent classes, durable resume verified across
  a spoke roll, and no increase in failed/duplicated external effects versus the
  legacy loop.

## Phase 3 — Default for all agents

- Flip the default; retain the legacy loop for one deprecation release.
- Ship operator migration guidance in UPGRADE.md at the major-version boundary
  where the default changes.
- Remove the legacy loop only after the deprecation release and after rollback
  guidance has been exercised on a live spoke.

## Version targeting

The delivered investigation artifacts and Phase 1 are v5-compatible
(documentation/prototype plus opt-in contributor pilot, no default change).
Phases 2–3 target v6 unless the v5 GA bar (#5622) explicitly pulls them in.
This RFC is not a v5 GA blocker.

## Out of scope

- Queue selection / transport for cross-node handoff until durable ownership is
  settled.
- State-triggered hooks proposal (separate RFC; same umbrella).
