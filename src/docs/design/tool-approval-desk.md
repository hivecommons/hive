# The Tool-Approval Desk

Status: implemented (vertical slice), default-off — RFC [#4000](https://github.com/hivecommons/hive/issues/4000)

The desk is the single decision point every approval-shaped request resolves
through. It replaces four independently-grown gates with one testable function,
keys the answer on the hive's ACMM level, lets operators express policy as data,
and produces an auditable record of *why* each action was allowed or blocked.

---

## 1. The problem: four gates, one shape

Hive did not have a decision point. It had four, each grown out of its own
incident, each implementing the same `(requested action, ACMM level, identity) →
verdict` shape with a different failure mode:

| Gate | Location | Failure mode it grew from |
| --- | --- | --- |
| Self-merge | `SelfMergeMinACMMLevel` / `AutoMergeConfig.SelfAuthoredAutoMergeAllowed` (`pkg/config/config.go`) | The self-authored sweep originally had **no ACMM check at all** — an L4 hive wrongly self-merged its own PR, and the gate was bolted on afterward. |
| Plan approval | `PlanAutoApproveForLevel` (`pkg/config/acmm_packs.go`) | A separate pack lookup consulted only by the decomposition path. |
| Plan-from-label | `PlanningConfig.PlanFromLabelEnabled` (`pkg/config/config.go`) | A third, independent level check. |
| Merge queue | `isTrustedMerger` + `latestHiveQueueApproval` (`pkg/github/automerge_sweep.go`) | Role- and review-based approval implemented *inside* the sweep. |

The desk is the refactor all four were independently converging on.

---

## 2. The decision point

`toolapprove.Desk.Resolve` resolves every request to one of four verdicts:

```
auto-approve | security-scan | operator-approve | deny
```

It evaluates three layers **in a fixed order**:

```
  1. BASE POLICY   hard guardrails, tool policy, agent mode, repo allowlist,
                   and the ACMM level mapping the legacy gate encoded
                        ↓
  2. OPERATOR RULES CEL expressions from config; may steer a request
                   into a different lane
                        ↓
  3. ACMM CEILING  applied LAST, unconditionally — a rule can never approve
                   above the hive's level
```

That ordering is the entire safety argument. **Rules are an input to a decision
the ceiling always has the final word on.** A rule asking for `auto-approve` on
an L1 hive is clamped to `operator-approve`, the clamp is recorded on the
verdict (`CeilingApplied`), and it shows up in the audit record. Pinned by
`TestACMMCeilingClampsPermissiveRule` and `TestACMMCeilingNeverExceedsLevel`.

### The ceiling

`ACMMCeiling(level)` gives the most permissive **lane** a level may enter:

| Level | Ceiling | Meaning |
| --- | --- | --- |
| L0–L2 | `operator-approve` | Nothing side-effectful resolves without a human. |
| L3–L5 | `security-scan` | May reach `auto-approve`, but only *via* a green scan. |
| L6+ | `auto-approve` | Full autonomy; the desk is an audit record, not a gate. |

An unknown or negative level clamps to the most restrictive lane, so a
mis-parsed or absent `acmm_level` can never widen authority
(`TestACMMCeilingUnknownLevelFailsClosed`).

Hard guardrails — direct PR creation, direct PR merge, explicit tool denies,
repo allowlist, agent-mode restrictions — are **not rule-overridable**. Letting
config relax them would reopen exactly the holes they were added to close
(`TestRuleCannotOverrideHardDeny`).

---

## 3. Rules as data

Approval rules are CEL expressions in config, compiled at load, reusing the
posture `pkg/celtrigger` already established:

- **Malformed rules are rejected at config load**, not at decision time. A typo
  like `request.checks_gren` is a compile error because `RuleRequest` is
  registered as a native CEL type.
- **A runtime evaluation error is a no-match**, never a match. A broken rule
  silently stops widening authority; it can never accidentally grant it.
- **Evaluation cost is bounded** (`maxRuleEvalCost`), so a pathological
  expression cannot stall the decision point that sits in every agent turn.

```yaml
tool_approval:
  enabled: true
  rules:
    # The canonical case: bulk-approve green dependabot patch bumps, while a
    # feature PR is NOT swept up.
    - name: green-dependabot-patch
      expr: |
        request.kind == "self-merge" && request.checks_green &&
        request.author == "dependabot[bot]" &&
        request.title.startsWith("chore(deps)")
      action: auto-approve

    # Hold anything touching guardrail-critical paths, at any level.
    - name: workflows-need-human
      expr: request.file_path.startsWith(".github/workflows/")
      action: operator-approve
      priority: 100
```

The `request` activation exposes: `kind`, `tool`, `agent`, `repo`, `number`,
`labels`, `author`, `title`, `checks_green`, `read_only`, `side_effectful`,
`command`, `file_path`, plus a `hasLabel(request.labels, "…")` helper.

`RuleRequest` is deliberately a **separate type** from the internal `Request`:
it is a stable, documented policy surface, so internal refactors cannot silently
break every deployed rule.

---

## 4. The L6 throughput contract

From the maintainer's binding addendum — the desk **must not reduce the
effective autonomy or throughput of high-ACMM hives**. Four requirements, each
with a test:

**1. Auto-approve is synchronous and in-loop.** A request resolving to
`auto-approve` never enters a queue and never waits on any external process.
This is enforced *structurally*, not by convention: `Inbox.Enqueue` **refuses**
any verdict that is not `operator-approve`, returning `ErrNotOperatorLane`.
There is no code path by which an auto-approved request reaches durable storage.

> `TestL6AutoApproveNeverQueues` points the inbox at a temp path and asserts the
> file **does not exist** after a full L6 auto-approve flow. A future change
> that starts journaling auto-approvals "for the audit trail" fails here.
> `TestL6RuleAutoApproveNeverQueues` covers the rule-driven path.

**2. The scan lane must not become a hidden serialization point.** A verdict
that is already `auto-approve` or `operator-approve` returns without consulting
the scanner, so a slow scanner never taxes an L6 turn whose answer is always
yes.

> `TestL6AutoApproveIsSynchronous` wires a scanner that *fails the test if
> called* and blocks forever if it is, then asserts the resolve completes.

**3. Migration maps current behavior 1:1.** Whatever a level auto-permits today,
the desk auto-permits on day one. New restrictions arrive only as explicit,
audited policy changes — never as conservative shipped defaults.

> `pkg/toolapprove/parity.go` computes each legacy gate's answer by calling **the
> same config predicates the production gate calls**, and `parity_test.go`
> asserts equivalence for every level 0–7. Because both sides delegate to the
> real predicates, a future threshold change moves them together and the tests
> keep passing — they pin *equivalence*, not a table that would rot.
> `TestNoConservativeDowngradeAtHighAutonomy` states requirement 3 directly.

**4. Operator-lane items at L6 are probable policy bugs.** An inbox that quietly
accumulates on a hive whose level says "full autonomy" is a misconfiguration
signal. `Inbox.PendingAtFullAutonomy` surfaces them, the API returns
`policy_bug_count`, and the dashboard renders a warning banner rather than
letting them wait politely (`TestPendingAtFullAutonomyIsFlagged`).

Unchanged and deliberate: operator kill-switches and the `hold` label sit
**above** the desk at every level.

---

## 5. The operator queue

Durable, **operator-lane only**, at `/data/approvals/inbox.json` on the spoke's
PVC (override with `tool_approval.inbox_path`).

An approval that waits overnight must survive a spoke roll — hives auto-roll
frequently, so an in-memory pending list would be wiped by the very machinery it
is meant to outlive. The store follows the idiom already used by the hub's
upgrade-pause kill switch and the contribute ledger:

- One small JSON file, loaded lazily on first use.
- Written **atomically** (temp file in the same directory, then rename), so a
  crash mid-write leaves either the old file or the new one, never a truncated
  inbox.
- A missing or corrupt file degrades to "empty" rather than wedging the hive.
- Growth is bounded (`maxInboxEntries`, `maxJournalEntries`, 30-day journal
  retention).

`TestInboxSurvivesRestart` constructs a **second, independent** `Inbox` over the
same path — which is exactly what a spoke roll looks like from the inbox's point
of view — and asserts pending items and the resolved journal both survive.

### Idempotency

Every request carries an idempotency key: either caller-supplied, or derived by
`DeriveIdempotencyKey` from the fields that identify *what* is being requested
(lane, tool, target, arguments — hashed in sorted key order so map iteration
cannot make the same request hash two different ways). Timestamps and
per-delivery metadata are deliberately excluded, which is what makes a
re-delivery match.

Resolution is two-phase — **resolve → execute → mark executed** — so a crash
between "approved" and "actually ran" is distinguishable on restart: the former
is safe to retry, the latter must not be.

- `Enqueue` on an already-resolved request returns `ErrAlreadyResolved`.
- `Resolve` on an already-journaled ID returns `ErrAlreadyResolved` with the
  original record and does **not** journal a second time.
- The API surfaces a replayed resolve as **409 Conflict**, returning the
  original outcome so a client can reconcile stale UI.

> `TestGrantedVerdictDoesNotDoubleExecute` is the direct assertion.
> `TestIdempotencyKeyDistinguishesDifferentRequests` is its positive control —
> without it, "idempotency" could just mean "everything is a duplicate".

**Reconciliation with #4002 stage 2:** the turn model's journaled re-entrant
turn is landing concurrently. This slice defines its own key derivation and
keeps it *additive* — `Request.IdempotencyKey` accepts an externally-supplied
key, so once stage 2's per-operation identity lands, the turn runner passes its
own key and the desk journals under that identity instead of deriving one. No
schema change is required on either side.

---

## 6. API

All three endpoints are **owner-gated** via `requireOwnerRole`, which requires
both the owner role *and* the server-verified marker header — the same bar
token-access and backup enforce. Approving a pending tool call is at least as
privileged: a granted verdict lets an agent take an action the ACMM level
otherwise reserved for a human.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/approvals` | List pending; returns rows, rule chips, `policy_bug_count`. |
| `POST` | `/api/approvals/resolve` | Resolve one. 409 on replay. |
| `POST` | `/api/approvals/bulk` | Resolve many. |

### Bulk is never a parallel path

`/api/approvals/bulk` calls `Inbox.ResolveMany`, which calls `Inbox.Resolve`
once per item — **the same function the single-item handler calls**. Likewise
`Desk.ResolveBatch` calls `Desk.Resolve` per request. This mirrors how
`runBulkAction`/`applyBulkAction` in `pkg/hub/saas_bulk.go` decompose a bulk
request into per-item authorized operations, and it is what RFC #4000 requires:
*a bulk approve is N individual evaluations through the same decision function
every agent request goes through.*

Partial failure is the normal case (another operator may have resolved an item
between the list and the click), so the response is a **per-item result list**,
not a single boolean.

> `TestResolveBatchEqualsNIndividualResolves` asserts
> `ResolveBatch(reqs) == [Resolve(r) for r in reqs]` across every level and
> every lane. `TestResolveManyEqualsNIndividualResolutions` does the same for
> the inbox path. `TestApprovalsSourceGate` additionally pins the owner gate at
> the *source* level with a count floor, so a refactor cannot silently drop it.

---

## 7. Operator UX

The **Approvals** panel appears in the spoke dashboard only when the desk is
enabled — a hive that has not opted in sees no new chrome.

- A **pending-count badge** on the section header, hidden at zero.
- **Rule chips as filters**: narrow to "everything the `large-diff` rule would
  hold", then bulk-resolve just those.
- **Multi-select** with Approve/Reject-selected actions.
- **Each row shows which rule would resolve it** — re-evaluated server-side
  against the *current* rule set rather than replayed from the enqueue-time
  verdict, so editing policy is immediately visible in the queue. This is the
  "tuning knobs embedded in the review loop" the fleet-owner feedback asked for.
- A **policy-bug banner** when items are queued at L6.

---

## 8. What is wired today

Three real producers, **behind a default-OFF flag**:

- The self-authored auto-merge sweep (`pkg/github/automerge_desk.go`).
- The trusted human merge queue (`trySweepQueuedPR`).
- The plan-from-label trigger in the governor evaluation cycle.

The merge producers were the cheapest honest starting point: they already have
the shape the desk generalizes and compute repo, number, author, reviewed SHA,
and a real `ChecksGreen` from `commitGreen`. The plan-from-label producer wraps
the existing `PlanningConfig.PlanFromLabelEnabled` result so the desk can
withhold or audit the operation without making the label trigger more
permissive.

The consultation happens **after** the sweep's own eligibility checks, so the
desk can only ever *withhold* a merge the legacy gate already permitted — it can
never widen authority. That asymmetry is what makes enabling the flag safe to
try on a live hive.

With `tool_approval.enabled: false` (the default) no hook is installed and
`consultApprovalDesk` allows unconditionally: **shipping this changes nothing
about any running hive** (`TestNoDeskInstalledIsNoOp`).

### The #4001 hook seam

RFC #4001 (state-triggered hooks) landed alongside this work and shipped an
`enqueue-approval` action over a deliberately narrow `hooks.ApprovalQueue`
interface with a **nil sink** — so an `enqueue-approval` hook reported a loud
unwired-sink error rather than silently dropping an approval.

`cmd/hive/approvalhook.go` is the adapter that interface anticipated. A
hook-produced approval lands in the **same** durable inbox, renders in the
**same** Approvals panel, and resolves through the **same** idempotent path as
one produced by the sweep:

```yaml
hooks:
  - name: needs-human
    on: review_rejected
    action: enqueue-approval
    params:
      kind: review-rejected
      summary: A rejected review needs an operator decision
```

Two deliberate choices:

- **The adapter does not re-decide.** A hook firing `enqueue-approval` has
  already expressed the operator's intent that this transition needs a human.
  Running it back through `Desk.Resolve` would let an auto-approve rule silently
  discard an approval the operator explicitly asked for — and `Inbox.Enqueue`
  refuses anything that is not `operator-approve` anyway, so re-deciding could
  only ever *drop* the request.
- **The idempotency key is derived from hook name + transition + scope**, not
  the default content hash, so a flapping transition produces one pending row
  rather than one per firing. A resolved approval is not re-raised
  (`TestHookApprovalAdapterDoesNotReAskAfterResolution`).

### Remaining

- Wire any future production caller of the decomposition `plan_auto_approve`
  gate through the desk. The parity function exists, but the current v5 tree has
  no long-running hive call site beyond direct `bd decompose --auto-approve`.
- Route `security-scan` through the sec-check agent surface
  (`buildSecCheckMessage`) rather than the in-process `DefaultSecurityScanner`,
  with the async/post-hoc path for pattern-matched-known-safe requests that
  requirement 2 anticipates.
- Suspend/resume an operator-approval wait as a #4002 re-entrant turn, and adopt
  stage 2's idempotency key (see §5).
- An `on_approval_pending` transition so an L6 policy bug *alerts* rather than
  waiting. The hook plumbing now exists (above); this needs the desk to emit
  that transition when it parks an item at L6.
