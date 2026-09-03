# Evaluating a handoff path for the re-entrant turn model (#4002, step 3)

Status: **spike / investigation — no decision taken.** This page answers the two
questions RFC #4002 scopes to step 3 ("does hive want queue-based handoff, and
what is the minimal state envelope?") and the beads-checkpointing challenge
raised on the issue. It recommends a **sequence**, not an architecture, and it
does not propose wiring anything into the live agent loop.

Read [The agent turn model and where in-process state lives](agent-turn-model.md)
first: it is steps 1 and 2, and this page assumes its findings rather than
repeating them.

Every claim carries a `file:line` citation against `v4` at the time of writing.
Line numbers drift; function names are the durable handle. Three findings are
additionally pinned by characterization tests, named where they appear, so a
future change that closes a gap surfaces as a test asking to be rewritten rather
than as a stale paragraph nobody re-reads.

---

## Summary of findings

1. **Hive has already built handoff's two hard mechanisms — twice — and wired
   neither.** `pkg/convergence/mutation` (accepted on #4255) implements a
   durable, epoch-fenced claim ledger *and* an idempotent operation journal.
   `pkg/turn` (step 2 of this RFC, #4933) implements a second operation journal
   with its own idempotency-key derivation. **Nothing in the repository imports
   either package.** Step 3's most consequential finding is not a missing
   mechanism; it is duplication between two unwired prototypes.
2. **No existing store has all three properties handoff needs.** Atomic
   compare-and-set on claim, cross-process serialization, and a
   corruption-resistant persist are spread across three implementations, each
   holding a different two of the three. §2 has the table.
3. **The claim path hard problem 2 tells handoff to reuse is not atomic.**
   `beads.Store.Claim` writes `in_progress` unconditionally, records no
   claimant, and `Ready` reserves nothing. Two callers are both told they hold
   the task.
4. **The recommendation is: no queue, and not yet.** Handoff's blocker is
   ownership, not transport. A queue added before ownership is fixed becomes the
   fifth durable store the issue explicitly warned against, and inherits the
   duplicate-work bug family it was meant to end.
5. **The beads-checkpointing challenge has a narrower answer than it was
   asked.** The "idempotency guards" half of the proposed cheaper alternative
   already exists — twice. What remains genuinely undecided is only whether
   resuming *mid-turn context* pays for hive owning the conversation, and the
   instrument that would size it measures the one execution path where owning
   the conversation is impossible.

---

## 1. What steps 1 and 2 settled

- **The turn envelope exists and is re-entrant.** `SessionEnvelope`
  (`src/pkg/turn/envelope.go:71`) carries messages, plan, journal, status and
  task ref; `Runner.Step` (`src/pkg/turn/runner.go:120`) is a plain function
  over it, returning a structured `TurnOutput`.
- **Landed effects are protected across re-entry.**
  `JournaledExecutor.Do` (`src/pkg/turn/runner.go:46`) persists intent, performs
  the effect, then persists the settlement; a re-entry that finds an `intended`
  entry reconciles instead of replaying.
- **Persistence is atomic and scrubbed.** `FileStore.Persist`
  (`src/pkg/turn/store.go:17`) writes to a uniquely-named temp file, chmods,
  fsyncs, renames, and fsyncs the directory; `ToJSON`
  (`src/pkg/turn/envelope.go:113`) routes every content-bearing field through
  `logscrub` on the way out. This closes residual item 3 from the stage-2 report
  on the issue, which listed atomic persistence as assumed rather than done.
- **The motivating problem now has an instrument.** `TurnLoss`
  (`src/pkg/agent/turn_loss.go:96`), recorded by `noteTurnInterruptedLocked`
  (`:117`) through the single teardown funnel `tearDownTurnLocked` (`:178`).

What steps 1 and 2 explicitly did **not** settle is concurrency: the stage-2
report's residual item 5 says the journal makes re-entry safe, not concurrency
safe, and that two processes racing one envelope is out of scope. That residual
is this page's subject.

---

## 2. Hive already has three durable-ownership implementations

This is the finding that should shape step 4, so it comes before the answers.

### 2.1 `pkg/convergence/mutation` — the fenced lease, already built

`Ledger` (`src/pkg/convergence/mutation/ledger.go:81`) is a durable claim ledger
whose every transition is a compare-and-set on `{key, expected epoch, expected
state}`. `Acquire` (`:166`) grants at `prev+1` and persists before returning, so
an epoch is never handed out that a restart could forget. `ValidateEpoch`
(`:257`) is the fence, checked at the mutation boundary. Expiry reconciles an
`ActiveMutation` entry to `Waiting` rather than `Released` (`:134`), so a crashed
holder is fenced without its ownership being silently discarded.

Alongside it, `Journal` (`journal.go:166`) records each logical operation with an
ID computed **without owner or epoch** (`Effect.LogicalID`, `journal.go:110`), so
a reassigned replacement adopting the same desired effect finds the same entry
rather than minting a second. `Reconcile` (`journal.go:334`) resolves an
uncertain effect against authoritative external state before any retry, and
`Executor.Execute` (`executor.go:70`) sequences validate → begin → effect →
record under the epoch.

That is, in substance, the handoff design this step was asked to evaluate —
already accepted, already merged, and inert by default (`JournalingEnabled` /
`FencingEnabled`, `executor.go:17`, `:22`).

### 2.2 `pkg/turn` — a second operation journal

Step 2 independently built `Journal` / `JournalEntry`
(`src/pkg/turn/journal.go:40`) and `DeriveIdempotencyKey` (`:62`), deriving a key
from `{version, session, kind, repo, target, body}` and deliberately excluding
the model's tool-call ID. That reasoning is sound and matches
`Effect.LogicalID`'s "no owner, no epoch" rule arrived at independently for
#4255 — which is precisely why the duplication matters: two teams reasoned to
the same rule and wrote it twice.

### 2.3 Neither is wired, and the gaps are complementary

Nothing outside those two package directories imports either. Both are
prototypes.

| | atomic CAS on claim | cross-process serialization | corruption-resistant persist |
|---|---|---|---|
| `beads.Store` | **no** — `Claim` (`beads.go:441`) sets `in_progress` unconditionally through `Update` (`:419`) | **yes** — `lockAndRefresh` takes an exclusive `flock` and re-reads (`xproc_lock.go:32`) | **yes** — unique temp name per writer (`beads.go:773`), fixed in #4742 |
| `mutation.Ledger` | **yes** — every transition is a CAS (`ledger.go:220`) with a monotonic epoch | **no** — serialized by an in-process `sync.Mutex` only (`ledger.go:81`) | **no** — fixed `path + ".tmp"`, no fsync (`ledger.go:285`) |
| `turn.FileStore` | **no** — `SessionEnvelope` carries no owner, epoch or lease at all | **no** | **yes** — `CreateTemp` + `Sync` + rename + dir fsync (`store.go:17`) |

Each implementation holds two of the three properties, and a different two. The
`mutation.Ledger` persist gap is the *same* fixed-temp-name pattern that #4742
removed from beads — the beads code carries the explanatory comment
(`beads.go:789-794`) and the ledger, written later and independently, does not.

**Pinned by test.** `TestTwoOpenLedgersBothAcquireTheSameClaim` and
`TestReopeningAfterAConcurrentAcquireSeesOnlyTheLastWriter`
(`src/pkg/convergence/mutation/ledger_crossprocess_test.go`) demonstrate the
middle column: two handles opened on one path both acquire the same claim **at
the same epoch**, so `ValidateEpoch` authorizes both, and the whole-file rewrite
erases the loser's entry rather than merging it. This is not a live defect — the
package is inert — but it is exactly the property a cross-process handoff would
need and would otherwise assume.

---

## 3. Question A — does hive want queue-based handoff?

**Recommendation: no queue, and not yet.** Three reasons, in order of weight.

### 3.1 The blocker is ownership, not transport

Handoff means a second process safely adopting work a first process may still
believe it holds. That is a mutual-exclusion problem. A queue moves *messages*;
it does not by itself decide who owns a task, and every queue-based design still
needs the lease underneath. Hive's lease already exists (§2.1) and needs
cross-process serialization; adding a queue first solves the part that is not
blocking.

### 3.2 The claim path handoff was told to reuse cannot yet exclude anyone

The issue's hard problem 2 is explicit: cross-process handoff "must go through
the existing atomic offer→claim path". The path exists. The atomicity does not:

- `Store.Claim` (`beads.go:441`) sets `StatusInProgress` unconditionally via
  `Update` (`:419`). The cross-process `flock` (`xproc_lock.go:32`) serializes
  the two *writes*; nothing compares against the prior status, so the second
  caller is told it succeeded. `bd update --claim` (`src/cmd/bd/main.go:243`)
  prints "Claimed" either way.
- A claim records **no claimant**. `Bead.Actor` (`beads.go:111`) is set at
  `Create` and means *addressee*, not *holder*, and `Claim` never touches it. So
  re-entry cannot distinguish "I already hold this, resume it" from "somebody
  else holds this, leave it alone" — the one distinction a lease exists to make.
- `Ready` (`beads.go:578`) is a pure read with no reservation, consumed only by
  `bd ready` (`src/cmd/bd/main.go:169`). Offer→claim is therefore a
  read-then-write across two separate short-lived CLI processes.

**Pinned by test.** `TestClaimDoesNotRejectAnAlreadyClaimedBead`,
`TestClaimRecordsNoClaimant` and `TestReadyOffersTheSameBeadToRepeatedReaders`
(`src/pkg/beads/claim_handoff_test.go`) pin all three. They characterize current
behaviour and skip with a rewrite instruction if a compare-and-set ever lands.

Whether that is a defect depends on a fact this spike cannot settle from the
code: beads are addressed to a named `Actor` and today only that actor's agent
polls them, so the exclusion may simply never have been needed. It becomes
needed the moment a handoff exists, which is why it is named here rather than
filed as a bug.

### 3.3 A queue now would be durable store number five

The issue's own caution — "the state envelope should consolidate or at least map
onto these, not become store number five" — applies with more force after §2.3.
Hive currently has, for this problem alone, three partial ownership stores and
two unwired journals. The next thing built here should reduce that number.

### 3.4 The ordered prerequisites, if handoff is wanted later

1. **Pick one journal.** `pkg/turn`'s journal and `pkg/convergence/mutation`'s
   journal answer the same question with the same rule. Converging them — most
   plausibly by having `turn` depend on `mutation` rather than the reverse,
   since `mutation` already carries the epoch — is the cheapest possible step
   and removes a whole class of future divergence. Phase 1 is done for identity
   derivation: `turn.DeriveIdempotencyKey` is now a deprecated wrapper around
   `mutation.DeriveLogicalID`, so new operation-key changes have one canonical
   helper while the unwired `SessionEnvelope` compatibility shape still exists.
2. **Give the chosen ledger cross-process serialization**, using the pattern
   beads already proved: `flock` plus re-read-under-lock (`xproc_lock.go:32`),
   plus the unique-temp-name persist beads adopted in #4742.
3. **Join the envelope to the lease.** §4.
4. **Only then** ask whether a transport is wanted. With a fenced lease and a
   shared journal, "handoff" may reduce to a replacement process acquiring at a
   higher epoch and loading the envelope from the shared path — no queue.

---

## 4. Question B — the minimal state envelope

The envelope hive would hand off is smaller than the RFC implies, because most
of it already exists.

**Already carried** by `SessionEnvelope` (`src/pkg/turn/envelope.go:71`):
`Messages` (the conversation), `Plan` with bound idempotency keys, `Journal`
(what landed), `TaskRef` (the join to the work item), `Status`, and the
version field that makes the format evolvable.

**Must be added** — and this is the whole of the addition:

| field | why |
|---|---|
| `Owner` | who holds this envelope now. `TaskRef` names the work, not the holder; §3.2 shows the bead cannot supply it. |
| `Epoch` | the fencing token. Must be minted by the ledger, not by the envelope, so it is monotonic across processes. `mutation.Entry.Epoch` (`ledger.go:58`) is that value. |
| `LeaseExpiry` | so a crashed holder's work becomes adoptable without a human. `mutation.Ledger` already reconciles expiry to `Waiting` (`ledger.go:134`). |

And one behaviour change: `Persist` must become a **compare-and-set on
`Epoch`**, refusing a write from a holder the ledger has already fenced.
Without it, two processes that both believe they hold the envelope each rewrite
the whole file and the loser's journal entries vanish — reopening exactly the
duplicate-effect class the step-2 journal closed. This is the same failure the
`mutation.Ledger` test in §2.3 demonstrates, one layer up.

**Explicitly not in the envelope, and not portable:**

- **Backend session references.** `~/.claude.json`, per-agent `CODEX_HOME`, and
  copilot session-state directories are spoke-local paths holding
  backend-private formats. §5.1 of the step-1 page establishes hive can neither
  parse nor migrate them. A handed-off envelope therefore cannot carry a
  tmux-hosted agent's conversation, only a headless turn's.
- **Everything on `AgentProcess`** — pane observations, nudge budgets, tmux
  identity. These describe a terminal on one host and are meaningless on
  another. The step-1 page inventories them.

**The consequence is the fork, forced.** Step 1 named the choice between
backend-specific resume envelopes and an API-shaped backend, and deliberately
took no position. Handoff removes the option of not choosing: the envelope
described above is only handoff-able on the headless path. A tmux-hosted agent
can have durable *control-plane* state — it already does — but not a
handoff-able conversation, at any envelope design.

---

## 5. Question C — why not just beads-checkpointing?

The challenge on the issue proposes a cheaper alternative: a `bd note`
checkpoint verb, prompt-pack discipline, and idempotency guards, priced against
conversation-as-state. Two corrections narrow it.

**The guards half already exists — twice.** The proposed "idempotency guards"
are `pkg/convergence/mutation`'s journal and `pkg/turn`'s journal (§2). Whichever
alternative wins, that work is done and should be converged, not rebuilt. So the
comparison is not "conversation-as-state versus checkpoints plus guards"; it is
"conversation-as-state versus checkpoints, given guards either way".

**That leaves exactly one undecided variable.** Of the three things the challenge
lists as uncaptured by beads today — mid-turn context, side-effect awareness,
turn-granular resume — the journal already supplies side-effect awareness, and
turn-granular resume is a consequence of context, not an independent benefit. So
the whole decision reduces to: **does resuming mid-turn context save enough to
justify hive owning the conversation?**

**And the instrument cannot answer it yet.** `TurnLoss`
(`src/pkg/agent/turn_loss.go:96`) is honest about what it measures: `UpperBound`
is explicitly the most a restart *could* have cost, `Producing` is the
threshold-free count of interruptions that certainly hit a working agent. Both
are collected on the **tmux path** — the path where, by §4, hive cannot own the
conversation at all. The instrument therefore sizes the *problem* on the fleet's
normal mode, and the *solution* is only available on a different mode that
carries no instrument.

The killed-at-50% experiment the challenge asks for is still the right
acceptance criterion. Stated against fields that now exist, it needs:

1. A baseline from fleet `TurnLoss` data — `Producing` over `Interruptions`
   answers "how often does a teardown actually discard work", which nothing has
   answered yet. Until that ratio is known, both arms of the comparison are
   being priced against an unsized problem.
2. A headless task instrumented on both arms, since arm (a) cannot run on the
   tmux path. That makes the experiment a **prototype cost**, not a measurement
   cost — worth stating plainly, because the challenge's premise was that
   measuring is cheaper than prototyping, and for arm (a) specifically it is not.

If (1) shows `Producing` is a small fraction of `Interruptions`, the honest
recommendation is to close this RFC in favour of the smaller effort, exactly as
the challenge proposes — and the guards are already built either way.

---

## 6. The stage-2 residual list, re-checked against `v4`

The stage-2 report on the issue listed six residuals. Their status today:

1. **The LLM call is not journaled** — stands. `Runner.Step` binds a plan
   supplied by the caller (`runner.go:120`, `bindPlan` `:153`); inference is
   outside the envelope transition.
2. **Reconciliation is only as good as its query** — stands, and is inherent.
   `mutation.Reconcile` (`journal.go:334`) takes external state as an argument
   for the same reason.
3. **Persistence is assumed atomic** — **closed.** `FileStore.Persist`
   (`store.go:17`) does temp + chmod + fsync + rename + directory fsync. Worth
   noting the sibling ledger did not get this treatment (§2.3).
4. **`Runner.Step` does not use the journal** — partly stale as written. `Step`
   drives every operation through `JournaledExecutor.Do` (`runner.go:141`). What
   remains true is that nothing in the live agent loop constructs a `Runner`.
5. **No claim integration** — stands, and is this page's subject.
6. **The journal grows unboundedly** — stands. `Journal.Entries`
   (`journal.go:78`) is appended and never compacted, while `Messages` is
   nominally compactable. A handed-off envelope crossing spokes makes its size a
   transfer cost, not only a disk cost.

---

## 7. What step 4 still needs

Step 4 is the feasibility and migration-cost call. Its remaining inputs:

1. **Fleet `TurnLoss` data** — the `Producing`/`Interruptions` ratio (§5). This
   is now a matter of collecting from log aggregation, not of building anything.
2. **A decision on §4's fork**, which handoff forces and step 1 deferred.
3. **A decision on the two journals** (§3.4 item 1), which is worth making even
   if the RFC stalls, because two unwired implementations of one rule will
   diverge.
4. **An answer to whether beads' claim should exclude** (§3.2) — a question for
   whoever owns the contribute plane, not a code reading.

Nothing on that list requires more prototyping. Two of the four are questions
for maintainers, and the `hold` label should stay until they are answered.

---

## What this page does not say

It does not recommend for or against the RFC — step 4 owns that. It does not
propose changing `beads.Claim`: making a claim start failing changes live agent
behaviour, and the case for it rests on a handoff that does not exist yet. It
does not file the `mutation.Ledger` persist and serialization gaps as defects,
because the package is inert by design and reaching them requires wiring that
has not happened. All three are recorded here so that whoever does the wiring
meets them as known constraints rather than as incidents.
