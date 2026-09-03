# RFC #4002: Re-entrant conversation-as-state turn model

Status: accepted design; v5 opt-in production rollout started
Refs: #4002, #4000, #4001, #4036, #4053, #4879, #5272, #5635, #5799

## Summary

Hive's current agent loop drives long-lived interactive CLI processes through
tmux. A "turn" is inferred from terminal text: hive types a prompt, scrapes the
pane, waits for idle markers, and keeps watchdog counters in memory. That model
has served v5, but it cannot make an in-flight turn durable across a pod roll or
hand the same turn to another process without losing the backend's opaque
conversation state.

The accepted direction is a re-entrant turn model where a turn is a plain
function over a durable envelope:

```go
Step(ctx, envelope, input) (nextEnvelope, output, error)
```

The envelope is the durable state. It records messages, operation intents,
settlements, pending approvals, subagent synchronization, task/claim identity,
and enough metadata to resume without depending on a suspended goroutine or a
live tmux pane. v5 delivered the investigation, prototype, and handoff evaluation needed to
accept the design. Issue #5799 starts the production rollout on v5, still
default-off and explicitly opt-in.

## Goals

- Make every turn reconstructible from a persisted envelope plus a structured
  input.
- Journal side-effectful operations with stable idempotency keys so re-entry
  reconciles landed effects instead of replaying them.
- Treat tool approval, tool execution, compaction, elicitation, max-turns,
  retry, subagent sync, and hooks as ordered operations in the envelope.
- Reuse or fix Hive's existing durable ownership paths before adding handoff
  transport.
- Preserve current tmux-backed behaviour until explicit per-agent opt-in gates
  prove the new path safe.

## Non-goals

- No v5 default flip from tmux-hosted agents to re-entrant agents.
- No new queue until claim/ownership semantics are atomic across processes.
- No promise that opaque backend CLI transcripts are portable. Backend-native
  resume can be an optimization, but Hive-owned structured turns are the design
  foundation.
- No separate state store unless existing stores cannot satisfy the envelope,
  claim, and journal requirements after targeted fixes.

## Findings from completed v5 work

### Stage 1: current-state inventory

`src/docs/design/agent-turn-model.md` and
`src/docs/design/agent-state-inventory.md` document the current loop. The
important conclusion is that Hive's control plane is already fairly durable, but
the live turn is not. The backend owns its conversation in private CLI state;
Hive owns prompts, rendered logs, pane observations, kick bookkeeping, and
watchdog counters. A restart abandons the active turn and waits for a future
governor tick rather than resuming the in-flight work.

### Stage 2: re-entrant prototype

`src/pkg/turn` proves an isolated, contribute-shaped `Step` over a serialized
conversation envelope. It includes scrubbed persistence, structured outputs,
pending approval state, and journaled side effects. Replay tests kill execution
at operation boundaries and verify exactly-once effects against fake external
surfaces. This closes the biggest correctness problem named in the issue:
non-idempotent tool execution after a crash.

### Step 3: handoff evaluation

`src/docs/design/agent-turn-handoff.md` answers the queue question: not yet. The
blocker is durable ownership, not transport. Hive already has several partial
mechanisms — beads, `pkg/convergence/mutation`, and `pkg/turn` — but none has
all of atomic cross-process claim, safe persistence, and envelope serialization
wired into production. Handoff must first reuse/fix the ownership path instead
of adding a parallel queue and recreating duplicate-work bugs.

### Related tool-approval work

#4000 is one operation within this model. A paused tool approval is not an
out-of-band flag; it is an operation whose request, ACMM decision, settlement,
and resume input are serialized in the turn envelope.

## v5 phase 2/3 rollout status

- Durable ownership uses one primitive: `pkg/convergence/mutation.Ledger`. Its
  claim acquisition and state transitions are cross-process serialized and
  epoch-fenced; replacements adopt expired/waiting claims at a higher epoch.
- The first production path is contributor assignment. When explicitly opted
  in, the hub persists a `pkg/turn.SessionEnvelope` for the assignment while
  preserving the existing websocket relay execution path.
- The opt-in surface is:
  - global: `turn.reentrant.enabled` or `HIVE_REENTRANT_TURN_ENABLED`;
  - named agent: `agents.<name>.reentrant_turn`;
  - fleet: `turn.reentrant.background_fleet_enabled` or
    `HIVE_REENTRANT_TURN_BACKGROUND_FLEET`.
- Tool approvals now appear in `SessionEnvelope.operations` as
  `tool_approval` operations with the ACMM verdict, tool call, pending/settled
  status, and operator decision.
- Rollback remains one flag flip: disable the global gate. Persisted envelopes
  are passive audit/replay state and are not consumed when the gate is off.

## State envelope

A production envelope contains at least:

- Stable session and agent identity.
- ACMM level and policy pack provenance.
- Ordered conversation messages and compacted summaries.
- Ordered operation list with operation IDs, status, inputs, outputs, and error
  categories.
- Pending tool approvals and operator decisions.
- Journal entries for side-effectful operations: intent, settlement,
  idempotency key, external reference, reconciliation status, and timestamps.
- Task/bead/claim identity, owner, epoch/lease, and expiry metadata once
  handoff is enabled.
- Subagent references and synchronization results.
- Scrubbed persistence metadata and schema version.

The durable representation is secret-bearing. It may include prompts, code,
tool output, remote errors, and references. It must be scrubbed on persist and
protected by the same or stronger access controls as existing agent homes and
logs. In-memory execution uses unsanitized values; only persisted or displayed
forms are scrubbed.

## Ordered operations

A turn is a deterministic sequence over the envelope. Implementations may skip
operations that do not apply, but must preserve their ordering semantics:

1. Assimilate input: user/operator message, approval decision, subagent result,
   retry signal, or resume signal.
2. Run pre-turn hooks.
3. Check max-turn and terminal status.
4. Compact context while preserving policy/system invariants.
5. Invoke the model or backend adapter.
6. Elicit structured assistant output and tool calls.
7. Resolve each tool call through the ACMM tool-approval operation.
8. Persist operation intent before any external side effect.
9. Execute or reconcile the side effect.
10. Persist settlement and append tool result/error messages.
11. Synchronize subagent state.
12. Run post-turn hooks and return a structured `TurnOutput`.

If a process dies after intent and before settlement, re-entry must reconcile
against external state before retrying. A model-provided tool call ID is useful
for traceability but cannot be the idempotency key because model calls may be
re-minted on replay.

## Relationship to beads checkpointing

Beads are the durable task substrate, but current bead writes happen at task
lifecycle boundaries. They do not capture mid-turn context, partial tool
results, or non-idempotent effects unless the agent wrote a note before dying.
The accepted path is therefore not "conversation state instead of beads". It is:
use beads/claims for durable ownership and task lifecycle, and use the turn
envelope for turn-granular conversation and operation state. A future v6 rollout
should still measure whether aggressive bead checkpointing plus idempotency
covers enough workloads before enabling re-entrant agents broadly.

## Handoff decision

A queue is deferred. Before queue-backed handoff, Hive must have one production
ownership primitive with these properties:

- Atomic compare-and-set claim/lease across processes.
- Monotonic owner epoch or equivalent fencing checked at mutation boundaries.
- Crash-safe, corruption-resistant persistence.
- Explicit handoff/adoption semantics for expired or abandoned turns.
- Integration with the operation journal so a replacement process reconciles
  existing intents instead of replaying effects.

After that exists, a queue, hub websocket, or direct worker API is a transport
choice rather than the source of correctness.

## Compatibility and rollout

The compatibility contract from the phased roadmap remains binding:

1. A re-entrant turn reconstructs everything from the conversation envelope,
   structured input, and existing session/context model.
2. No state needed for correctness is suspended in an in-process coroutine
   between turns.
3. The legacy tmux loop remains default until explicit phase gates are met.
4. Breaking changes to turn execution require operator migration guidance.

Rollout sequence:

- **Phase 1 (v5, delivered as spike/prototype):** state inventory,
  contribute-shaped re-entrant prototype, journaled replay tests, scrubbed
  persistence, handoff evaluation, and this RFC.
- **Phase 2 (v6 target):** one opt-in production contributor/background path
  using the envelope with fixed durable ownership; #4000 tool approval recorded
  as an explicit ACMM operation.
- **Phase 3 (v6+ target):** opt-in coverage for the full background fleet,
  durability/restart soak, then a later default flip with deprecation guidance
  for the legacy loop.

## Acceptance criteria for closing #4002

This issue asked for an RFC/spike before a direct rewrite. The v5 artifacts now
provide the current-state inventory, prototype, replay/idempotency evidence,
handoff evaluation, phased roadmap, and this accepted design. Remaining
production wiring is intentionally split to follow-up issues so #4002 can close
as the umbrella RFC.
