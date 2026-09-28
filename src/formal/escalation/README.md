# Formal model: escalation + reviewer-lane lifecycle

A Spin (Promela) model of the fix-loop escalation machinery and the reviewer
adjudication lane, at the protocol level: one hive-authored PR's lifecycle as
nine interacting processes. It verifies the invariants the code comments
promise — and pins down, with minimized counterexamples, the places where the
shipped machinery does not deliver them.

The model targets the **post-#5485** machinery (v5 with the reviewer lane,
#5480, merged; machinery-generation amnesty, #5471) **with the #5511 G1/G3/G4
fixes applied**: the reviewer-verdict ledger sync in `Store.Sweep`, the
escalated guard in `Store.TryReEngage`, and the recommend-close work-list
marker. Model and code moved together — the formerly-hypothetical
`-DPATCH_REVIEWER` behavior is now the shipped (and modeled) default, and the
properties it was shown to close (P4b, P5) now hold on the default build.
Gap G2 (the pending-window ledger wipe) was closed by the #5617 follow-up:
pending observations are now no-ops in `Store.Sweep`/`Store.ObserveRed`, and
the formerly-pinned `w_pending_wipe` witness is expected to hold.

Run everything:

```console
$ bash run.sh        # requires spin + a C compiler; ~1 minute
```

`run.sh` runs every property exhaustively and compares each verdict against
its expected result. Properties that must hold must pass; a property pinned
with a **documented counterexample** (a still-open design gap) must still
*fail* — if one stops failing, the code or the model changed and a human must
re-check. (All four gaps are now fixed, so every current row expects pass.)
Either divergence exits nonzero. CI runs this via
`.github/workflows/formal-verify.yml` (reporting-only, not a required check).

## Model ↔ code mapping

| Promela | Go |
|---|---|
| `Sweeper` proctype | `runEscalationSweep` (`cmd/hive/main.go`) driving `Store.Sweep` (`pkg/escalation/escalation.go`) once per governor tick |
| `Reaper` proctype | `reapStuckRedPRs` (`cmd/hive/main.go`) → `Store.TryReEngage` |
| `Agent` proctype | the FIX-BEFORE-NEW fix lane (`addRedPRFixFirst` / `formatRedPRFixData`, `pkg/scheduler/policy_overlays.go:184-307`): red, non-escalated PRs are routed back to their author agent, which pushes another attempt |
| `Reviewer` proctype | the reviewer lane (`pkg/scheduler/reviewer_lane.go`): adjudicates escalated rows from `ci-failing.json`, excluding rows labeled `reviewer-passed` or `reviewer-recommend-close`; REPAIR and DE-ESCALATE are collapsed into one branch (both = label swap + push); RECOMMEND-CLOSE closes at ACMM ≥ 6 (`-DACMM6`), else comments, adds `reviewer-recommend-close`, and stands down |
| `Human` proctype | the operator watching the `needs-human` label queue; eventually takes ownership or closes. **The queue is the label** — a PR without `needs-human` is invisible to the human |
| `CI` proctype | per-SHA check verdict: each pushed SHA resolves nondeterministically red or green, once |
| `Merger` proctype | the auto-merge sweep: a green open PR is eventually merged |
| `Timer` proctype | wall clock for `Store.StaleRed`: the tracked `CurRedSHA`, unchanged, eventually exceeds `RedPRStaleAfter` |
| `GenBumper` proctype | `MachineryVersion` bumps (a new fix-dispatch generation ships) |
| `MergeWatcher` proctype (`-DWATCHER`) | the merge-request watcher's terminal re-engage path (`pkg/github/merge_request_watcher.go:276`) |
| `entryExists`, `attempts`, `escalated`, `curRedSha`, `reeng`, `entryGen` | `escalation.Entry`: existence in the ledger, `len(RedSHAs)`, `Escalated`, `CurRedSHA`, `ReEngagements`, `Machinery` |
| `headCounted` | `containsSHA(e.RedSHAs, o.HeadSHA)` for the current head (sound: SHAs are never reused, so only the current head's membership is ever queried) |
| `stale` | `now − e.FirstRedAt ≥ RedPRStaleAfter` (`Store.StaleRed`) |
| `escalatedView` | the `escalatedPRs` map returned by `runEscalationSweep` and consumed same-tick by `reapStuckRedPRs` and `writeMergeEligible` (`ci-failing.json` `row.Escalated`); refreshed at the end of each sweep pass |
| `needsHuman`, `reviewerPassed`, `recommendedClose` | the `needs-human` / `reviewer-passed` / `reviewer-recommend-close` labels on the forge |
| `amnesty_check()` inline | the machinery-amnesty block shared by `Store.Sweep` (escalation.go:181-186) and `Store.TryReEngage` (escalation.go:372-377) |
| `THRESHOLD=3`, `MAX_REENG=6` | `DefaultThreshold`, `MaxReEngagements` (actual production constants) |
| `NSHA=4`, `GEN_MAX=3` | finite bounds: distinct head SHAs per PR, machinery generations |

Faithfully carried quirks: `Store.TryReEngage` creates missing entries as
`&Entry{}` (Machinery **0**, → immediate no-op amnesty), while `Store.Sweep`
creates them with `Machinery: MachineryVersion`; since the #5617 G2 fix,
`Sweep` distinguishes green (delete entry) from CI-pending (no-op) — only
green clears, only red counts.

Faithfully carried **fixes** (#5511): the reviewer still edits labels only —
the ledger sync happens SWEEP-side, exactly as shipped: `Store.Sweep` resets
an `Escalated` entry whose observation labels carry `reviewer-passed` without
`needs-human` (gap G1); `TryReEngage` refuses escalated entries after the
amnesty check (gap G3); a below-ACMM-6 RECOMMEND-CLOSE marks the row with
`reviewer-recommend-close`, which the work list excludes like
`reviewer-passed` (gap G4).

## What is abstracted away

- **One PR.** All properties are per-PR; the ledger's cross-PR pruning
  (`seen` map) reduces to "clear on close/merge". `maxTrackedSHAs=20` is
  unreachable at `NSHA=4`.
- **SHAs are ordinals.** Pushes mint strictly increasing SHA ids; a SHA's CI
  verdict resolves once (a flake re-run would be a new push). Rebases and
  repairs are pushes.
- **Time is nondeterminism.** Sweep cadence, `RedPRStaleAfter`, and CI
  latency are unordered events; every interleaving is explored, which is
  strictly more behavior than any real timing.
- **Evidence/comments/ntfy** (excerpts, `CommentBody`, notifications) carry
  no protocol state and are dropped. `MarkEscalated`'s comment-retry loop is
  collapsed into the escalation step.
- **ObserveRed** (`recordRedStaleness`) is folded into `Timer` + the sweep's
  SHA sync; its entry-creation path is represented by `TryReEngage`'s
  (both create `&Entry{}` with Machinery 0).
- **ACMM** is fixed at 5 (lane active) — below 5 the reviewer is a no-op and
  every property reduces to the pre-#5480 machinery; `-DACMM6` models
  close authority.
- Property monitors are compiled in per run (`-DMON_*`) so each exhaustive
  search carries only the instrumentation it needs; monitor counters saturate
  at the bounds they are compared against.

## Verification results

Spin 6.5.2, exhaustive (`./pan -m500000`, DFS + COLLAPSE; liveness with weak
fairness `-a -f`). Every state count is a completed full search, on the
post-#5511 model (the G1/G3/G4 fixes are the modeled default, so the counts
differ from the pre-fix runs recorded in the #5511 discussion). The #5617 G2
fix changes the Sweeper's pending branch, so counts will shift slightly on
the next full run — the CI `formal-verify` workflow reports current numbers.

| # | Property | Run(s) | Result | States |
|---|---|---|---|---|
| P1 | Amnesty is one-shot per generation (per entry lifetime) | `p1_p2_amnesty` (assert in `amnesty_check`) | **holds** | 8,822,910 |
| P2 | No instant re-escalation after amnesty: never in the amnesty pass itself, and only with ≥ `THRESHOLD−1` genuinely new red SHAs or a full fresh re-engage budget | `p1_p2_amnesty` (asserts P2a/P2b) | **holds** | 8,822,910 |
| P3 | Re-engage budget: `ReEngagements ≤ 6` always; per unchanged red SHA, total ≤ (1 + amnesties) × 6 — i.e. ≤ 12 across one generation bump, ≤ 18 across the 3 modeled generations | `p3_budget_asserts`, `p3_cap_ltl`, `p3_total_ltl` | **holds** | 15,779,561 / 3,215,657 / 15,779,561 |
| P4a | Once `reviewer-passed`, the reviewer never adjudicates the PR again | `p4a_p6_safety` (assert) | **holds** | 3,215,657 |
| P4b | …and such a PR, if (still) escalated, eventually reaches the human queue | `p4b_handoff` (LTL, weak fairness) | **holds** — gap G1 FIXED (#5511: sweep-side reviewer-verdict ledger sync) | 3,217,019 |
| P5 | Every escalated PR eventually terminal (merged / closed / human-owned) | `p5_termination` (LTL, weak fairness) | **holds** — gap G1 FIXED | 3,224,760 |
| P6 | Escalated entries are invisible to the reaper and to FIX-BEFORE-NEW | `p4a_p6_safety`, `p6_watcher_safety` (asserts) | **holds** | 3,215,657 / 27,818,920 |
| W1 | "One reviewer pass per PR, ever" — every verdict class, every level | `w_onepass_acmm5` / `w_onepass_acmm6` | **holds** at both levels — gap G4 FIXED (#5511: `reviewer-recommend-close` marker) | 3,215,657 / 2,140,034 |
| W2 | Ledger history survives until green | `w_pending_wipe` | **holds** — gap G2 FIXED (#5617 follow-up: pending is a no-op observation; pre-fix witness at 7,706 states) | see CI |
| W3 | The merge watcher never burns re-engage budget on an escalated PR | `w_watcher_reengage` (`-DWATCHER`) | **holds** — gap G3 FIXED (#5511: `TryReEngage` escalated guard) | 27,818,920 |

Pre-fix baseline (the counterexamples that motivated #5511): P4b acceptance
cycle at 7,207 states, P5 at 5,169; W1 witness at 4,113 states (ACMM 5); W3
witness at 207,685. The former `-DPATCH_REVIEWER` runs — which proved the G1
fix shape closes P4b/P5 before it was implemented — are retired: the patch is
now the default build.

## Design gaps found

Gaps G1, G3, and G4 were **fixed in #5511** (the same PR that promoted this
model's `-DPATCH_REVIEWER` hypothetical to shipped behavior); gap G2 was
**fixed in the #5617 follow-up**. The sections below keep the original
counterexample narratives, followed by the shipped fix.

### G1 — a reviewer pass on a PR that stays red orphans it (FIXED in #5511)

`Entry.Escalated` is cleared only by: the PR going green (entry deleted), the
PR leaving the open set, or machinery amnesty. The reviewer's verdicts edit
**labels only**. Minimized counterexample (`spin -t -p` on `p5_termination`):

1. Three distinct red SHAs → `Sweep` escalates: `Escalated=true`,
   `needs-human` added.
2. The reviewer REPAIRs (or DE-ESCALATEs): removes `needs-human`, adds
   `reviewer-passed`, pushes a fix SHA.
3. The fix resolves **red**.
4. Now: the ledger still says `Escalated` → the row is excluded from
   FIX-BEFORE-NEW and the reaper; `reviewer-passed` excludes it from the
   reviewer lane; `NewlyEscala` requires `!e.Escalated`, so the `needs-human`
   label (and ntfy) can never fire again — the human queue never sees it.
   The PR sits red and open forever, owned by nobody.

The escalation comment's promise — "Remove the `needs-human` label … to
return the PR to the automated fix lane" — is broken by the same root cause:
the lane consults the ledger flag, not the label, so a human removing the
label (without the PR reaching green) also does not re-enable the fix lane.

Two nondeterministic rescues exist, and neither is a guarantee: a later
`MachineryVersion` bump (amnesty un-escalates the ledger), or gap G2's
pending-wipe happening to fire during the fix's CI window. The model shows
runs where neither occurs.

The model's `-DPATCH_REVIEWER` variant proved the fix shape before it was
written: when the reviewer removes `needs-human`, also reset the ledger
(`Escalated=false`, `RedSHAs=nil` — the same reset amnesty performs). With
that, P4b and P5 hold: a still-red repair re-enters the fix lane, re-escalates
on fresh evidence, `NewlyEscala` re-fires, and the `reviewer-passed` exclusion
hands it to the human.

**Shipped fix (#5511)**: implemented SWEEP-side, because the reviewer's label
edits happen through a direct `gh pr edit` the hub may never observe.
`escalation.Observation` now carries the PR's labels (threaded from the
enumeration in `runEscalationSweep`), and `Store.Sweep` reconciles the
verdict: an `Escalated` entry whose observation labels contain
`reviewer-passed` and not `needs-human` is reset (un-escalated, `RedSHAs`
cleared, fresh re-engagement budget, `Machinery` untouched). The reset fires
once — clearing `Escalated` disarms it — so fresh reds re-accumulate to a
normal re-escalation. The reset keys on the label PAIR (`reviewer-passed`
present, `needs-human` absent), so a human removing `needs-human` on a PR
without a reviewer pass still does not re-enter the lane — the escalation
comment's "remove the label" promise remains label-pair-scoped, not fully
honored (deliberate: an unlabeled un-escalation has no marker distinguishing
it from a label sync race). P4b and P5 now hold on the default build.

### G2 — a CI-pending observation wipes the attempt ledger (FIXED in #5617 follow-up)

`Store.Sweep` branched on `!o.Red`, and `runEscalationSweep` computes
`Red: pr.CIStatus == "failure"` — so a **pending** observation (every fresh
push has a pending window) took the "went green" branch and **deleted the
entry**, discarding the distinct-SHA attempt count. A PR that always happened
to be enumerated mid-window restarted its count every time and could evade the
threshold indefinitely (witness `w_pending_wipe`). In practice sweeps usually
caught settled states, which is why escalation still fired — but the breaker
was probabilistic where the code intends it to be deterministic. (This wipe is
also what accidentally rescued some G1 orphans pre-#5511.)

**Shipped fix (#5617 follow-up)**: `escalation.Observation` carries a
`Pending` flag (`CIStatus == "pending"`, threaded from both enumeration
sites in `main.go`), and pending is a **no-op observation**: `Store.Sweep`
neither counts nor clears (the entry still reports its current
attempts/escalated verdict so the `escalatedPRs` view does not flicker
mid-CI-window), and `Store.ObserveRed` leaves the staleness clock and
re-engagement counter untouched. Only green clears, only red counts.
`w_pending_wipe` now holds.

### G3 — the merge watcher re-engages escalated PRs (FIXED in #5511)

`Store.TryReEngage` never checked `Entry.Escalated`, and the merge-request
watcher's terminal path (`merge_request_watcher.go`) calls it without
consulting the escalated set (unlike `reapStuckRedPRs`, which does). A merge
request filed before escalation that exhausts its attempts on a red required
check therefore burned re-engagement budget on a `needs-human` PR and logged
"re-engaged fix loop" for a PR the fix loop is standing down from. Benign in
effect — the dispatch surfaces (FIX-BEFORE-NEW, reaper) still exclude
escalated rows, and P3/P6 held even then — but the counter and the log line
lied, and a budget burned this way could later trigger an exhaustion-based
re-escalation prematurely after amnesty.

**Shipped fix (#5511)**: `TryReEngage` returns false for escalated entries,
guarded AFTER the machinery-amnesty block so an older-generation escalated
entry is still amnestied (un-escalated) and then re-engaged on its fresh
budget, exactly as amnesty intends. No budget increment, no false log line.
Invariant `w_watcher` (budget never granted while the entry is escalated)
now holds in the `-DWATCHER` build.

### G4 — RECOMMEND-CLOSE below ACMM 6 re-adjudicates every kick (FIXED in #5511)

The work-list builder's "one reviewer pass per PR, ever" held only for
REPAIR/DE-ESCALATE, because only those add `reviewer-passed`. Below ACMM 6, a
RECOMMEND-CLOSE verdict left the row fully adjudicable, so the reviewer
re-adjudicated (and re-commented) it on every kick until a human acted
(witness `w_onepass_acmm5`; at ACMM 6 the close is immediate and the
invariant held).

**Shipped fix (#5511)**: the kick contract instructs the reviewer to add the
`reviewer-recommend-close` label alongside the recommend-close comment, and
the work-list builder excludes labeled rows exactly like `reviewer-passed`.
`w_onepass` now holds at every ACMM level.

## Reproducing a counterexample

No expected counterexamples remain: every run in `run.sh` must pass. To
replay the RETIRED G2 pending-wipe witness (pre-fix machinery), check out the
model as it stood before the #5617 follow-up and run:

```console
$ spin -a -DMON_PENDING escalation.pml && gcc -O2 -o pan pan.c
$ ./pan -a -i -N w_pending         # -i: iteratively shorten the trail
$ spin -t -p -g -DMON_PENDING escalation.pml   # replay with state annotations
```

To replay the RETIRED G1 counterexamples (pre-fix machinery), check out the
model as introduced by #5511's model commit and run `p5_term`/`p4_handoff`
with `-DNFAIR=12` and `-a -f`.
