# RFC: Re-entrant Conversation-as-State Agent Turn Model (#4002)

**Status**: Proposed (RFC / Spike Phase)  
**Author**: Douglas Baggett (`@Danathar`)  
**Related Issues**: [#4002](https://github.com/hivecommons/hive/issues/4002) (RFC), [#4000](https://github.com/hivecommons/hive/issues/4000) (Tool Approval Operation), [#4001](https://github.com/hivecommons/hive/issues/4001) (State-Triggered Hooks)

---

## 1. Executive Summary & Problem Statement

Hive manages autonomous AI coding agents running 24/7 in Kubernetes cluster spokes. Currently, agent execution relies on persistent CLI processes hosted inside local `tmux` sessions. An agent's runtime state (conversation history, current reasoning step, pending tool execution, retry backoff, and local memory) is suspended directly within the running process and ephemeral tmux memory buffers.

While this model enabled fast prototyping with interactive CLI backends (Claude Code, GitHub Copilot CLI, Goose), it imposes significant operational challenges at scale:

1. **Vulnerability to Spoke Rolls & Pod Restarts**: When a spoke pod restarts or rolls during deployment, all suspended in-flight agent state is destroyed. The governor must rely on complex stall watchdogs and heuristic restart recovery mechanisms.
2. **Barrier to Horizontal Scaling & Agent Migration**: Agents cannot be migrated between spokes or distributed across worker nodes without bespoke coordination machinery, because their state is pinned to local process memory.
3. **Ad-Hoc Turn Gating**: Guardrails, ACMM authorization checks, tool approvals, and security scans are scattered across loop call sites rather than forming a deterministic pipeline.
4. **Testing Complexity**: Verifying multi-turn behavior requires spawning real tmux sessions and mocking terminal I/O rather than executing pure function calls over structured state.

### The Proposed Solution: Conversation as Durable State

We propose migrating to a **re-entrant, conversation-as-state agent turn model**. In this model:
- The **Conversation Transcript / Session Envelope** is the single source of truth and the complete, durable state of the agent.
- Each agent turn is an **explicit, re-entrant function call**:
  $$\text{Step}(\text{SessionEnvelope}, \text{TurnInput}) \longrightarrow (\text{SessionEnvelope}', \text{TurnOutput}, \text{error})$$
- Because **zero state is suspended in-process between turns**, a session can be paused, persisted to disk/database, serialized across network boundaries, or handed off between different spoke pods with zero context loss.

---

## 2. Current Architecture vs. Re-entrant Architecture

> **Authoritative current-state audit**: this section sketches the current model only far enough to contrast it with the proposal. The exhaustive inventory of today's in-process and durable agent state (spike stage 1 for #4002) is maintained separately in [`agent-state-inventory.md`](agent-state-inventory.md); where the two disagree, the inventory is authoritative.

### Current In-Process Suspended State Model

```
┌─────────────────────────────────────────────────────────────┐
│ Spoke Pod (Ephemeral Lifecycle)                             │
│                                                             │
│   ┌─────────────────────────────────────────────────────┐   │
│   │ tmux session (hive-agent-<name>)                    │   │
│   │                                                     │   │
│   │   ┌───────────────┐     ┌───────────────────────┐   │   │
│   │   │ CLI Process   │ ──► │ In-Memory State       │   │   │
│   │   │ (Claude/Cop)  │     │ - Call stack          │   │   │
│   │   └───────────────┘     │ - Unflushed history   │   │   │
│   │                         │ - Pending tool future │   │   │
│   │                         └───────────────────────┘   │   │
│   └─────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
                             ▼ (Pod Restart / Crash)
                       [STATE LOST]
```

### Proposed Re-entrant Conversation-as-State Model

```
┌───────────────────────────────────────────────────────────────┐
│ Durable Store / Hub / Beads Ledger                            │
│                                                               │
│   ┌───────────────────────────────────────────────────────┐   │
│   │ SessionEnvelope JSON                                  │   │
│   │  - SessionID, AgentIdentity, ACMMLevel, TurnCount     │   │
│   │  - Messages: [System, User, Assistant, Tool, Subagent]│   │
│   │  - Status: Active | WaitingApproval | Completed       │   │
│   │  - Variables, Subagents, PendingApprovals             │   │
│   └───────────────────────────────────────────────────────┘   │
└───────────────────────────────┬───────────────────────────────┘
                                │ Re-entrant Handoff
           ┌────────────────────┴────────────────────┐
           ▼                                         ▼
┌───────────────────────────────┐   ┌───────────────────────────────┐
│ Spoke Worker A (Turn N)       │   │ Spoke Worker B (Turn N+1)     │
│                               │   │                               │
│ Step(Env_N, Input)            │   │ Step(Env_N+1, Input)          │
│   ► Ordered Turn Operations   │   │   ► Ordered Turn Operations   │
│   ► Returns Env_N+1           │   │   ► Returns Env_N+2           │
└───────────────────────────────┘   └───────────────────────────────┘
```

### Comparison Matrix

| Dimension | Current Model (`pkg/agent` + tmux) | Re-entrant Turn Model (`pkg/turn`) |
| :--- | :--- | :--- |
| **State Storage** | Ephemeral process memory + tmux buffer | Externalized `SessionEnvelope` (JSON/Database) |
| **Pod Restart Recovery** | Stall detection + full agent restart from scratch | Instant resumption from latest turn checkpoint |
| **Cross-Spoke Handoff** | Impossible without shared filesystem & terminal attach | Native: Pass `SessionEnvelope` payload |
| **Tool Approval Gate** | Ad-hoc checks scattered in proxy & scripts | First-class `toolapprove.Resolve` operation (#4000) |
| **Subagent Sync** | Polling files / background cron tracking | Explicit `subagent_sync` state transition |
| **Deterministic Unit Tests** | Difficult (requires tmux daemon & fake PTYS) | Pure function test: `runner.Step(env, in)` |

---

## 3. Ordered Turn Operations

Each execution of `runner.Step(env, in)` executes a sequence of 10 discrete, deterministic operations:

```mermaid
flowchart TD
    Start([Turn Invocation: Step]) --> Op1[1. Input Assimilation\nUser msg, operator decision, subagent sync]
    Op1 --> Op2[2. Pre-Turn Hooks\non_turn_start]
    Op2 --> Op3{3. Max Turns Check\nTurnCount >= MaxTurns?}
    Op3 -- Yes --> DoneMax[Status = Completed\nDone = true]
    Op3 -- No --> Op4[4. Context Compaction\nSliding window / token pruning]
    Op4 --> Op5[5. LLM Inference Call\nGenerate completion]
    Op5 --> Op6[6. Output Elicitation\nExtract assistant msg & tool calls]
    Op6 --> Op7{7. Tool Calls Present?}
    Op7 -- No --> Finalize[Status = Completed\nDone = true]
    Op7 -- Yes --> Op8[8. ACMM Tool Approval Gate\ntoolapprove.Resolve]
    Op8 --> DecCheck{Verdict Decision}
    DecCheck -- operator-approve --> OpPause[Set StatusWaitingApproval\nRecord PendingApproval]
    DecCheck -- deny --> OpDeny[Append Tool Error Msg]
    DecCheck -- auto-approve --> OpExec[9. Tool Execution\nRun in sandbox / shell]
    OpExec --> OpAppend[Append Tool Result Msg]
    OpPause --> Op10[10. Post-Turn Hooks\non_turn_complete]
    OpDeny --> Op10
    OpAppend --> Op10
    Finalize --> Op10
    DoneMax --> Op10
    Op10 --> Finish([Return new SessionEnvelope + TurnOutput])
```

### Operation Breakdown

1. **Input Assimilation**:
   - Resumes paused sessions when an operator approval/rejection decision is supplied in `TurnInput.OperatorDecision`.
   - Appends incoming `UserMessage` to the conversation history.
   - Synchronizes completed background tasks from `SubagentResults`.
2. **Pre-Turn Hooks**:
   - Triggers declarative hooks (`on_turn_start`) for observability, metrics, and tracing span initialization.
3. **Max Turns & Termination Check**:
   - Prevents runaway loops by checking `TurnCount >= MaxTurns`.
4. **Context Compaction**:
   - Prunes older conversation turns using configurable sliding-window or summarization strategies (`Compactor`), while always preserving critical system instructions.
5. **LLM Inference Call**:
   - Dispatches the compacted message array to the active backend via `LLMClient.Generate`.
6. **Elicitation & Parsing**:
   - Extracts structured tool calls and reasoning outputs from model response.
7. **Explicit Tool-Approval Gate (#4000)**:
   - Every tool call is routed through `toolapprove.Resolve(ctx, req, acmmLevel, agent, scanner)`:
     - `auto-approve`: Tool proceeds to execution.
     - `security-scan`: Pre-execution security check runs via `ioscan` / command validator.
     - `operator-approve`: Session transitions to `StatusWaitingApproval` and yields.
     - `deny`: Prohibits execution, returning structured policy rationale to the model.
8. **Tool Execution**:
   - Approved tools execute via `ToolExecutor.Execute`, capturing standard output and exit codes.
9. **Subagent Synchronization**:
   - Subagent invocation tools register child session references in `env.Subagents`.
10. **Post-Turn Hooks & Audit Log**:
    - Emits structured audit records and triggers post-turn notifications.

---

## 4. Minimal State Envelope & Handoff Mechanics

### The `SessionEnvelope` Schema

```go
type SessionEnvelope struct {
    SessionID        string                     `json:"session_id"`
    Agent            toolapprove.AgentIdentity  `json:"agent"`
    ACMMLevel        int                        `json:"acmm_level"`
    TurnCount        int                        `json:"turn_count"`
    MaxTurns         int                        `json:"max_turns"`
    Status           SessionStatus              `json:"status"`
    Messages         []Message                  `json:"messages"`
    WorkingRepo      string                     `json:"working_repo,omitempty"`
    WorkingBranch    string                     `json:"working_branch,omitempty"`
    BeadID           string                     `json:"bead_id,omitempty"`
    Variables        map[string]string          `json:"variables,omitempty"`
    Subagents        map[string]string          `json:"subagents,omitempty"`
    PendingApprovals []PendingApproval          `json:"pending_approvals,omitempty"`
    CreatedAt        time.Time                  `json:"created_at"`
    UpdatedAt        time.Time                  `json:"updated_at"`
}
```

### Handoff Mechanics: Queue-Based vs. Stateless API Dispatch

We evaluated two architectural patterns for multi-node execution:

| Architecture | Mechanism | Pros | Cons |
| :--- | :--- | :--- | :--- |
| **Pattern A: Queue-Based Handoff** (Recommended) | Spokes publish `SessionEnvelope` checkpoints to a persistent queue (Hub WebSocket / Redis / Beads Store). Any available spoke worker dequeues the envelope to run turn $N+1$. | Complete decoupling; resilient to spoke crashes; natural work distribution. | Requires message queue storage backend. |
| **Pattern B: Direct HTTP RPC Dispatch** | Central Hub dispatches `POST /api/v1/agent/step` to an available spoke pod with the envelope. | Simple point-to-point transport. | Synchronous failure if receiving spoke pod dies mid-request. |

**Recommendation**: Adopt **Pattern A (Queue-Backed Handoff via Hub WebSocket)**. Hive's Hub already maintains WebSocket connections to all spokes (`/contribute` relay). Extending this protocol with structured session handoffs allows seamless horizontal scale-out.

---

## 5. Prototype Implementation & Validation

A complete prototype has been implemented in [`pkg/turn`](../../pkg/turn) and validated through tests in [`turn_test.go`](../../pkg/turn/turn_test.go):

- **Process Restart Handoff**: Tested serializing `SessionEnvelope` to JSON after Turn 1, instantiating a fresh runtime, deserializing the JSON, and successfully executing Turn 2 with zero context loss (`TestDurableStateHandoffAcrossProcessRestarts`).
- **ACMM Gated Operator Pauses**: Verified that side-effectful write tools at ACMM L4 enter `StatusWaitingApproval`, pause execution, and seamlessly resume upon receiving operator approval (`TestOperatorApprovalPauseAndResume`).
- **Subagent Synchronization**: Verified that background subagent task completions delivered in `TurnInput` update state and conversation transcript cleanly (`TestSubagentSynchronization`).
- **Scrub-on-Persist**: The serialized envelope is the durable, handoff-able form of the conversation, and transcripts are secret-bearing (tokens, repo contents, tool output). `ToJSON`/`ToPrettyJSON` therefore redact credential-shaped substrings from every content-bearing field via `pkg/logscrub` — the fleet's single reusable scrubber — before serialization; in-memory state used by `Step` is never scrubbed (`TestToJSONScrubsSecretsOnPersist`).
- **Journaled Re-entrancy (stage 2)**: Side-effectful operations are journaled with content-derived idempotency keys and survive a kill at every operation boundary with exactly-once effects (`TestKillMidTurnReplayExactlyOnce`). See Section 5a for the key design, the write protocol, the widened scrub boundary, and the residual non-re-entrant surfaces.

---

## 5a. Stage 2 — The Journaled, Re-entrant Turn (hard problem 1 + 3)

Stage 2 implements the mandated spike deliverable from the maintainer review: a journaled, re-entrant contribute-headless turn with a kill-mid-turn replay test. It closes hard problem 1 (tool-execution idempotency) and completes hard problem 3's scrub boundary. It is **prototype only** — nothing in the live agent loop or the running contribute path constructs any of it; it is exercised entirely by tests.

New files, all additive within the existing package: [`journal.go`](../../pkg/turn/journal.go), [`journaled_exec.go`](../../pkg/turn/journaled_exec.go), [`replay_test.go`](../../pkg/turn/replay_test.go).

### The idempotency-key design

Every side-effectful operation is identified by a key that must satisfy two constraints in tension:

1. **Stable across re-entry** — recomputable in a fresh process from the persisted envelope alone. This excludes everything ambient: timestamps, random nonces, PIDs, attempt counters, process-local sequence numbers.
2. **Unique per logical effect** — two genuinely different effects must not collide. Collision is the *worse* failure: a duplicate PR is visible and embarrassing, whereas a suppressed second comment is silent under-performance nobody notices.

The derivation is a length-prefixed SHA-256 over the semantic content of the effect and nothing else:

```
key = v1 + sha256( version | session_id | kind | repo | target | body )[:32]
```

| Field | Why it is in the key |
| :--- | :--- |
| `session_id` | Scopes the key to one task's turn sequence, so two agents posting an identical comment on the same issue are distinct operations. Handoff preserves `SessionID`, which is what keeps the key stable across processes and spokes. |
| `kind` + `repo` + `target` | Locate the effect. |
| `body` | The content hash — what separates "post comment A" from "post comment B", and equally what makes a *replay* of comment A recognizable as the same operation. |

Deliberately **excluded**: `ToolCallID`. Model-assigned call IDs are freshly minted on every inference, so a re-entry that re-runs the LLM step would produce a new ID for a semantically identical effect and defeat the mechanism entirely. The ID is recorded on the entry for traceability but never keyed on. Fields are length-prefixed so `("ab","c")` cannot hash the same as `("a","bc")`. The `v1` prefix is the escape hatch: bumping it invalidates all prior keys without making old journals unreadable.

**On the "natural GitHub idempotency surrogate" alternative.** GitHub exposes no idempotency-key header on the writes we care about. The available surrogates are all *query-after-the-fact*: search for an open PR from head branch X, list comments and match the body. This design uses them — but as **reconciliation**, not as the key. They cost a round trip, they are eventually consistent, and critically they cannot be computed offline, which violates constraint 1. The content hash is the primary key; the remote query is the tie-breaker for the single state where the local journal cannot know the answer.

This mirrors what `pkg/github` already does ad hoc: `CreatePR` dedupes on the head branch and reports reuse via `CreatePRResult.AlreadyExisted`. `EffectResult.AlreadyExisted` reuses that vocabulary rather than inventing a parallel one.

**Lineage.** At-least-once delivery over non-idempotent GitHub writes is exactly the duplicate-work class the fleet eradicated one incident at a time: [#3768](https://github.com/hivecommons/hive/issues/3768) → [#3792](https://github.com/hivecommons/hive/pull/3792) (issues re-offered while an open PR already claimed them) → [#3980](https://github.com/hivecommons/hive/issues/3980) (non-closing `Refs #N` PRs re-offered every cooldown window, forever) → [#3987](https://github.com/hivecommons/hive/issues/3987) (maintainer-gated issues with only merged reference PRs re-entering the pool, fixed by the `no_work_needed` verdict in #3997). Each was the same bug — a task re-entering a pool and re-performing an effect whose completion was not durably recorded — patched at a different layer. This journal is the generalization: **record the effect, not the attempt.**

### The write protocol

`JournaledExecutor.Do` performs one operation exactly once across any number of re-entries:

1. Derive the key (offline, stable).
2. **Succeeded entry → short-circuit.** Return the recorded `ExternalRef` without touching the remote.
3. **Ambiguous entry → reconcile.** Intent was written, outcome was not; ask the remote whether the effect landed. Found → settle as succeeded. Not found → fall through and perform.
4. **Write `OpIntended` and persist it.** If persistence fails, the effect is never attempted — an unpersisted intent is an unprotected write on the next re-entry.
5. Perform the effect.
6. Settle with the outcome and persist again.

The window between 4 and 6 is the only place a crash can leave ambiguity, and step 3 is what closes it on the next entry. The three-state `OpStatus` (`intended` / `succeeded` / `failed`) exists precisely so re-entry can distinguish *never started* from *may have happened* from *definitely happened* — a two-state journal cannot express the middle case, and the middle case is the whole problem.

### The scrub boundary — verdict

**The pre-existing boundary was correct but its coverage was incomplete, and stage 2 widened it.**

`ToJSON`/`ToPrettyJSON` were already the single persistence boundary, already routing every content-bearing field through `logscrub.ScrubString` — the fleet's canonical scrubber (GitHub tokens, AWS keys, bearer tokens, JWTs, PEM private-key blocks, hive canaries), the same machinery guarding stderr and logs. `Clone`/`Step` correctly do **not** scrub: in-memory state must stay intact or the agent would read redacted content back as fact. That design is sound and unchanged.

What stage 2 added is a new secret-bearing surface that the existing scrub did not cover — the journal itself:

| Journal field | Exposure |
| :--- | :--- |
| `Error` | Raw remote error text. **The most common accidental credential channel of the three** — git and gh routinely echo authenticated URLs into failure messages. |
| `ExternalRef` | Can carry an `x-access-token:…@github.com` authenticated URL. |
| `Target` | Can carry a branch name minted from a token-bearing remote. |
| `IdempotencyKey` | A hash. Needs no scrubbing and **must not be altered** — scrubbing it would break re-entry matching and cause replay. Explicitly left untouched. |

`OpIntent.Body` participates in the key but is never stored on the entry, so scrubbing can never destabilize a key. `TestReplayPersistedArtifactCarriesNoCredential` asserts both halves: no plaintext credential in the artifact, *and* every key still resolving after the scrub.

### What the replay test proves

`TestKillMidTurnReplayExactlyOnce` is table-driven over **every operation boundary** in a contribute-shaped turn (comment → push → PR create → label). Each op contributes two boundaries — the intent persist and the settle persist — for **8 kill sites**. `killablePersister` fails the write at boundary *N*, so death occurs *before* that envelope reaches disk (the strictly harder case). The only thing crossing the simulated process boundary is the serialized JSON, so no in-memory state can leak into the recovery.

| Boundary | Position | What re-entry must do |
| :--- | :--- | :--- |
| 1, 3, 5, 7 (odd) | Intent write, pre-effect | Nothing landed → perform normally. Exactly-once holds with no remote help. |
| 2, 4, 6, 8 (even) | Settle write, post-effect | Effect landed but is unrecorded → **must reconcile, not replay.** This is the duplicate-PR window. |

Assertions per case: **(a)** exactly-once effects, counted at the fake GitHub surface (calls actually made to the remote), not inferred from the journal's own bookkeeping; **(b)** the same terminal verdict (`shipped`, the contribute plane's vocabulary) and an effect summary identical to the control, with no operation left ambiguous; **(c)** no credential in the persisted artifact.

Supporting tests: `TestReplayReconcilesAmbiguousIntent` (the post-effect/pre-settle window resolves by query, not guesswork); `TestReplayNotFoundAfterIntentPerformsEffect` (**the anti-vacuity control** — a journal that simply skipped every ambiguous entry would pass the exactly-once test while silently dropping work); `TestIntentIsPersistedBeforeEffect` (runs *without* a reconciler, since reconciliation otherwise masks the ordering bug, and since effects with no queryable surrogate — a push, a bare comment — have no reconciler in practice); `TestReplayWithoutReconcilerStillNeverDuplicatesSettledEffects`; `TestIdempotencyKeyStability`.

**Positive control**: an unkilled run establishes the reference effect set, verdict, and boundary count, and the test fails if that control produces no effects or duplicates — so the kill cases cannot pass vacuously.

**Mutation-checked.** Per the lesson of audits 6 and 7 (every finding hid behind a test passing for the wrong reason), the suite was verified by breaking the implementation four ways and confirming a red: removing the already-done short-circuit (12 assertions fire); removing reconciliation (reproduces the duplicate-PR bug exactly, at every even boundary); treating ambiguous entries as always-landed (caught by the anti-vacuity control); and performing the effect before persisting intent. The fourth mutant initially **survived** — reconciliation masked it — which is why `TestIntentIsPersistedBeforeEffect` exists.

### Pending-approval operation shape (#4000)

`OpApprovalWait` and `SuspendForApproval` exist so a turn blocked on an operator decision is a **journaled, re-enterable position** rather than in-process suspended state. This is why re-entrancy had to come before handoff: an operator-approval wait *is* a suspended turn.

**Shape only.** No approvals UI, inbox, or routing is built here — that is #4000's scope. The wait performs no external effect, never counts as a landed effect, is rejected by `Do` (which is for side-effectful ops), and re-entering while still pending does not mint a second wait. Consistent with the L6 throughput contract on #4000: `auto-approve` resolves synchronously in-loop and never enters this lane; the journal entry is for the operator lane only.

### Residual — what is still NOT re-entrant

Recorded honestly, because the value of this stage is knowing precisely where the guarantee stops:

1. **The LLM call is not journaled.** A crash between inference and the first op re-runs inference. This is usually benign (it costs tokens, not correctness) but it is *not* free: a non-deterministic model may choose a different tool call on re-entry, so mid-turn recovery can diverge in plan even while every already-landed effect stays protected. Journaling inference responses by prompt hash is the obvious next step.
2. **Reconciliation is only as good as its query.** Effects with no reliable remote surrogate — a push (a force-push can hide it), a comment on a high-traffic thread — cannot be reconciled with certainty. For those, exactly-once degrades to at-most-once-per-boundary. `TestReplayWithoutReconcilerStillNeverDuplicatesSettledEffects` measures exactly this weaker property.
3. **Persistence is assumed atomic and durable.** `Persister` is an interface with a test implementation. A real one must do tmp-write + `os.Rename` (the pattern the no-work-verdict ledger already uses in `contribute_ws.go`) and survive a torn write. A partially written envelope is currently unhandled.
4. **`Runner.Step` does not use the journal.** The two are deliberately decoupled in this prototype. Wiring the journal into the tool-execution operation of `Step` — including the `PendingApprovals` re-execution gap the RFC already names — is Phase 2 work.
5. **No cross-process claim integration.** Per hard problem 2, handoff must reuse the contribute plane's atomic offer→claim path rather than parallel it. Nothing here touches claims, and the replay test simulates re-entry *within* one logical claim holder. Two processes racing on the same envelope are not covered — the journal makes re-entry safe, not concurrency safe.
6. **The journal grows unboundedly** within a session and is not compacted, unlike `Messages`.

---

## 6. Feasibility, Migration Costs & Staged Rollout Plan

### Migration Phases

1. **Phase 1: RFC & Prototype (Current Phase)**:
   - Merge this RFC and the baseline `pkg/turn` and `pkg/toolapprove` packages.
   - Solicit community feedback from maintainers.
2. **Phase 2: Self-Hosted Inference & Sandbox Agent Runner**:
   - Introduce an in-process re-entrant runner for agents using self-hosted backends (`vllm`, `llm-d`, `litellm`) and sandbox executors where direct API access exists.
   - Dual-run alongside legacy tmux agents for validation.
3. **Phase 3: Hub-Coordinated Spoke Session Migration**:
   - Wire `SessionEnvelope` checkpoints into the Hub contribution relay and Beads store.
   - Enable spoke rebalancing and rolling updates without interrupting multi-step autonomous tasks.
4. **Phase 4: Full Unification & Deprecation of Suspended State**:
   - Provide standard bridge adapters for external CLI backends so all agent executions flow through the re-entrant turn pipeline.

### Open Hard Problems (from maintainer review of #4002)

The maintainer review on #4002 names three hard problems. **Stage 2 (Section 5a) closes problem 1 and completes problem 3's scrub boundary**; problem 2 remains open and is the acceptance bar for Phase 2. Status is tracked below so the prototype is neither mistaken for a complete design nor undersold:

1. **Tool-execution idempotency on re-entry — ADDRESSED in stage 2 (Section 5a), with residuals.** Side-effectful operations are now journaled inside the envelope with a content-derived idempotency key under a before/after write protocol, and the mandated kill-mid-turn replay test passes over all 8 operation boundaries of a contribute-shaped turn. The guarantee is bounded: see the six residual non-re-entrant surfaces in Section 5a — most importantly that the LLM call itself is not journaled, that reconciliation is only as strong as its remote query, and that `Runner.Step` does not yet use the journal. The `PendingApprovals` re-execution gap named in the original review is **not** yet closed in `Step`; only the pending-approval *operation shape* exists (`OpApprovalWait`).
2. **Handoff must reuse the claim machinery, not parallel it.** Cross-node handoff (Section 4, Pattern A) is a task re-entering a pool. Whatever queue carries the envelope must go through the existing atomic offer→claim path and completion-verdict recording in the contribute plane, not introduce a parallel claim system.
3. **The conversation blob is a secret-bearing artifact.** The scrub boundary is now complete over every persisted field, including the journal surfaces stage 2 added (Section 5a, "The scrub boundary — verdict"). The remaining questions are authorization, not redaction: who may read a persisted envelope, and its trust boundary when it crosses spokes on handoff. Those are Phase 3 prerequisites.

Additional Phase 2 constraints from the maintainer positions on #4000: the ACMM ladder hard-coded in `toolapprove.Decide` is illustrative — the production decision function must read the ACMM packs (`pkg/config/acmm_packs.go`) rather than re-hardcode thresholds, must map each level's *current* permitted behavior 1:1 on day one, and at L6 must resolve synchronously in-loop with no queue residence (the scan lane must never become a hidden serialization point). From #4001: production hooks must fire on the durable commit of a transition (outbox over the journaled record), not on the in-memory flip as the prototype's `HookHandler` callbacks do.

---

## 7. Relationship to Sibling Issues

- **#4000 (Tool Approval Operation)**: Implemented in `pkg/toolapprove`. Integrated directly as Operation 7 in the turn pipeline.
- **#4001 (State-Triggered Hooks)**: Integrated via `HookHandler` callbacks (`OnTurnStart`, `OnTurnComplete`, `OnStatusChange`) to allow declarative operator actions upon state transitions.

---

## Conclusion & Next Steps

The re-entrant conversation-as-state model elevates agent execution in Hive from fragile in-process coroutines to a resilient, distributed, and testable system. We recommend approving this RFC for Phase 2 implementation.
