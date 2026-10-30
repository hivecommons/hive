# Activity monitor copy inventory

Every string builder whose output an operator can read on an activity
monitor row, hover, chip popover, ⋯ menu, toast or feed line — with the
words it produced before and after [#15394](https://github.com/hivecommons/hive/issues/15394)
("this is WAY too much information … use easy to understand language and
much much less text").

Counts are the words the builder can emit for one row (the longest branch
where a builder has several), not the source size. The budget every hover
now meets: **one sentence of state, ≤ 40 visible words, one recommended
action, everything technical behind one collapsed Details, nothing said
twice.** The tests that hold the line are named in the last column.

## The reference case: hive#15315, chip "Ready for human merge · untrusted author"

| Surface part | Before (words) | After (words) |
|---|---:|---:|
| Question (`decisionForHumanMerge`) | 72 | 14 |
| Blocker (`decisionDCOBlocker` / hold) | 15 | 0 (folded into Details) |
| Action buttons | 7 buttons · 20 | 1 primary + "More ⋯" · 4 |
| Bulleted legend (`consequences`) | 120 | 0 (each line is now the ⋯ menu item's hint) |
| "Asked by the hive · 7 hours ago" | 7 | 7 |
| Stage line echo (`data-ham-tip`: caption + reason + "Next move") | 150 | 0 visible (deduplicated; what is new sits in Details) |
| Details (collapsed) | — | 27 |
| **Visible total** | **≈ 384** | **≈ 29** |

The new hover, verbatim (title line omitted):

> **Ready to merge. Hive won't merge it alone — @odttlnt isn't a trusted author yet.**
> [ Let hive merge ] [ More ⋯ ]
> Asked by the hive · 7 hours ago
> Details ▸ *Hive never merges this on its own; a maintainer merges here or on GitHub. To trust @odttlnt: add them as a merger under ⚙ Security → Authorized users. · Ready for human merge · untrusted author · Next move: human — a maintainer merges it*

## Server-side builders (Go, `src/pkg/dashboard`)

| Builder | Reaches | Before | After | Held by |
|---|---|---:|---:|---|
| `stageTrackerUntrustedAuthorWhy` (all 10 trust reasons, summed) | question, reason text, delegated outcome | 314 | 68 | `TestStageTrackerHumanMergeSaysItOnce` (no config keys, no jargon) |
| `stageTrackerUntrustedAuthorFix` (new; the how-to-trust line, Details only) | Details | — | 81 | same |
| `stageTrackerHumanMergeReasonWords` tips (9 codes, summed) | chip hover, 090 chevron tip | 227 | 101 | `TestStageTrackerHumanMergeReason` |
| `stageTrackerHumanMergeReasonWords[breaking-change]` (new: chip `human merge`, caption *Ready for human merge (breaking change — owner merges)*) | chip, caption, chevron tip, `new.gates[]` | — | 14 | `TestStageTrackerHumanMergeRequiredRow` |
| `HumanMergeReasonText` composition (090 untrusted author) | chevron tip, row tooltip, feed | 43 + 24 ("Until then a maintainer merges — Approve & merge…") | 11 | `TestStageTrackerHumanMergeSaysItOnce` |
| `HumanMergeReasonText` admitted prefix | same | 41 | 9 | `TestStageTrackerDecisionForHumanMerge/admitted…` |
| `stageTrackerCaption` 090 | row caption, feed | 19 | 7 | `TestStageTrackerCaption…` cases in `stage_tracker_test.go` |
| `stageTrackerStageActor` 090 `Why` ("Next move: human — …") | chevron tip, row tooltip | 43 (echoed `HumanMergeReasonText`) | 4 | `TestStageTrackerHumanMergeSaysItOnce` (no echo) |
| `decisionForHumanMerge` (question + blocker + 7 consequences) | chip popover, ⋯ menu hints | 293 | 169 (of which Details 27, consequences ≤ 12 each) | `TestStageTrackerDecisionForHumanMerge` |
| `decisionMergeNowOption` consequence | popover title, menu hint | 11 | 8 | same |
| `decisionReauthorOption` consequence / disabled reason | menu hint | 31 / 29 | 12 / 14 | `TestStageTrackerDecisionForHumanMerge/unsigned…` |
| `decisionDCOBlocker` / `decisionDCOConsequence` | Details / menu hint | 39 / 17 | 29 / 8 | same |
| Other `decisionFor*` delegate / retry / close / resolve consequences (10 strings) | ⋯ menu hints | 112 | 66 | word budget in `TestStageTrackerDecisionForHumanMerge` |
| `delegatedHumanMergeOutcome` rationale | "Let hive decide" outcome | 43 | 11 + fix line | `TestDelegatedDecisionReadyForHumanMergeAnswersWithMergeOptions` |

## SPA builders (`static-src/js`)

| Builder | Reaches | Before | After | Held by |
|---|---|---:|---:|---|
| `stageTrackerDecisionPopHtml` (`tracker-actions.js`) | chip popover / chevron popover | question + blocker + N buttons + legend + meta + stage echo (≈ 384 on #15315) | question + 1 button + "More ⋯" + meta (≤ 40), Details collapsed | `TestStageTrackerDecisionPopoverContent` (≤ 40 visible words, no duplicate sentence, banned-jargon list, no `a.b.c` keys) |
| `stageTrackerSayOnce` (new) | popover Details | — | — | same |
| `stageTrackerStripItems` 090 chevron tip (`tracker-row.js`) | chevron `title` / popover Details | word + reason + stage description + actor (≈ 70) | word + reason + actor (≈ 20) | `TestStageTrackerSentinelLane…` whyTip case |
| `stageTrackerTooltip` author line | row tooltip | `@x (author/clanker, inferred)` | `@x (agent, inferred)`; duplicate lines dropped | `TestStageTrackerRows` |
| `stageTrackerDCOCaveat` | confirm dialogs | 55 | 46 | `TestStageTrackerReauthor…` in `stage_tracker_row_actions_test.go` |
| Confirm: Approve and let hive merge (lead) | dialog | 62 | 49 | same |
| Confirm: Merge now as maintainer | dialog | 76 | 53 | same |
| ⋯ menu hints (`stageTrackerLeadActions` → `ham-menu-hint`) | ⋯ menu | server consequences (11–31 each) | server consequences (≤ 12 each) | Go budget test |
| `stageTrackerOpenTriageNeeded` → `stageTrackerSegments` / `stageTrackerHeadline` Open word (`tracker-row.js`, `tracker-words.js`) | Open chevron, chevron hover first word, row tooltip headline | *New · since 28m* on a reporter-trust hold; the ask lived only in the caption ([#15437](https://github.com/hivecommons/hive/issues/15437)) | *Triage needed · since 28m* (same word as the `needs-triage` chip / 015) | `TestStageTrackerNewRowRendering`, `TestStageTracker_ReporterTrustHoldReadsTriageNeeded` |
| `stageTrackerTriageNeededCaption` (`stageTrackerCaption` 015, `stageTrackerNewState` hold text) | row caption, feed | 110 ("Actionable, but no maintainer has looked at it yet; waiting for triage — triage needed: carries needs triage": *triage* ×3 after the chevron word) / 84 ("New · waiting for a maintainer's triage (add `triage/accepted`); the hive's next pass in 7m") | 31 ("Triage needed — accept or close"; the next-mover and next-pass narration stay on the chevron hover) | `TestStageTracker_TriageNeededStage`, `TestStageTracker_ReporterTrustHoldReadsTriageNeeded`, `TestStageTrackerNewState` |
| `stageTrackerLeadActionHtml` (`tracker-actions.js`) | the row header's inline lead button (chip slot) | — (the lead sat in the ⋯ menu only) | the menu entry's label (1–4), its hint as the hover | `TestStageTrackerLeadActionInline`, `TestStageTrackerMaintainerRowsHaveLeadAction` |
| Toasts (`STAGE_TRACKER_ACTING_CAPTIONS`, acted labels) | toast | 2–4 each | unchanged | — |
| `stageTrackerStripItems` holder token + `stageTrackerClaimantIconKind` (`tracker-row.js`) on the PR in progress chevron ([#15541](https://github.com/hivecommons/hive/issues/15541)) | lit chevron word, chevron hover | *PR in progress* (bare; the holder token only reached PRs from review on) | *PR in progress · scanner ⬡*, *PR in progress · @odttlnt's clanker 🤖*, *PR in progress · @clubanderson 👤* — the same token and hover line as *Claimed ·*; the name ellipsizes before the one-line sub-row wraps; a detour word that already names them (*Returned to @bob's clanker*) does not repeat them | `TestStageTrackerClaimantChevronJS`, `TestStageTrackerPRClaimant`, `TestStageTrackerClaimantStage` |
| `stageTrackerHolderPresenceHtml` (`tracker-words.js`) after the holder token; `stageTrackerOfflineAuthorCaption` + `stageTrackerWaitingOn` at stage None (Go) | lit chevron holder suffix, chevron hover, row caption | *PR in progress · @odttlnt's clanker 🤖* (no live state; a fork PR the gate kept label-less read as unclassified even when the engine held it at 085) | *PR in progress · @odttlnt's clanker 🤖 · ■ offline 3h* (muted; *· ○ not registered* for a login with no profile here; nothing when online); caption "@odttlnt's clanker is offline 3h; a green PR is reviewed and merged without its author — hive adopts it only once it is returned (red CI, conflicts or review asks) and nothing is pushed for 15m"; a suppressed-label 085 record keeps *Needs human decision* | `TestStageTrackerHolderPresenceJS`, `TestStageTrackerUnclassifiedClankerPROfflineCaption`, `TestStageTrackerSuppressedDecisionRecordKeepsStage` |
| `stageTrackerLinkedPRProgress` (Go) + `stageTrackerEventText` / `stageTrackerNestedOpenPRs` (`tracker-motion.js`, `tracker-row.js`) on an adoption pair ([#15495](https://github.com/hivecommons/hive/issues/15495)) | issue row caption, feed line, inline lead button | *fell back to Covered by pr — Covered by PR #15537 — Review swarm requested; … ; PR #15508 — Hive adopted this PR … — changes requested* (48) and a *Let hive decide — all 2 PRs* lead on a row with nothing to decide | *Covered by #15537 (adopted from @mendezr's #15508) — in CI / review* (11); the feed says *moved to* when an issue stops mirroring its PR; the pair counts as one open PR so no lead button | `TestStageTrackerLinkedPRProgressCollapsesAdoptionPair`, `TestStageTrackerAdoptionPairIsOneOpenPR`, `TestStageTrackerEventTextUnmirroredCoveredByPR` |
| `stageTrackerSelectionTriage` (`tracker-actions.js`) | selection bar lead button / bulk ⋯ menu entry, summary toast | — (no bulk Accept triage; one row at a time) | *Accept triage (4 selected)* / *Accept triage (2 of 4)* with the hint *skips the 2 not waiting on triage*; one toast *Accepted 4 issues* | `TestStageTrackerBulkAcceptTriage` |
| Settings → Features → Auto-merge → "Merge only on current base" help (`renderGovFeatures`, `config-features.js`) and the Repos-tab override row / repo-card `current-base` chip ([#15335](https://github.com/hivecommons/hive/issues/15335)) | settingHelpMark popover, checkbox label, chip hover | — (new) | 56 (help), 5 (row label), 1 (chip) | `TestCurrentBaseSettingsMarkupNodeGuard` |

## Totals

| | Before | After |
|---|---:|---:|
| Server builders above (sum of longest branches) | ≈ 1,350 | ≈ 620 (of which ≈ 110 only ever shown inside Details) |
| #15315 hover, visible | ≈ 384 | ≈ 29 |

## Left as-is (follow-up)

- Merge-gate (080/095/100) chip details in `stageTrackerMergeGateInfo` are
  live clocks ("next pass in 42s", "soak 12m left"), short already. The
  word *sweep* left them in hivecommons/hive#15575 (see *Soaking rows*
  below); *soak* stays — it is the state, not the mechanism.
- The stage descriptions in `stagelabels.Table` are shared with the labels
  and the docs and were not shortened; the 090 chevron tip simply stopped
  appending them.

## Merge queue (hivecommons/hive#15395)

The owner read the train's slugs on the page — "front", "queued-behind" —
and asked what they meant. Every place the train's position appears now
uses the same words, and no user-facing train string carries either slug
(`TestStageTrackerMergeTrainOrdinalWords` and the Go `Sentence` test fail
on one). The column itself is **Merge queue** (was *Merge gate*; ids,
classes and JSON keys — `merge_gate`, `trackerColumnMergeGate` — keep the
old name).

| Builder | Reaches | Before | After | Held by |
|---|---|---|---|---|
| `STAGE_TRACKER_TRAIN_STATE_WORD` (`tracker-words.js`) | chevron label, gate pill | `front` → *Next to merge*, `queued-behind` → *In merge queue* | both *Merge queued*; the place tells them apart | `TestStageTrackerMergeTrainStateSlugsNeverLeak` |
| `stageTrackerTrainWord` | chevron label | *In merge queue · 2 of 6* / *front* (leaked) | *Merge queued · 1st* / *· 2nd* / *· 3rd* … + *· ~4m* countdown | `TestStageTrackerMergeTrainOrdinalWords` |
| `stageTrackerTrainOrdinalOfN` + `stageTrackerTrainTagSuffix` / `stageTrackerTrainTagTip` | chevron label, gate pill, provisional suffix, chevron hover | *Merge queued · 1st* twice over on a page with two trains (owner: "how are there 2 'merge queued 1st'?") | *Merge queued · 1st of 2*; past one train on the page *· train main.go* (overlap basename, else the head `#N`) ends the word and the hover leads *Train of 2 PRs overlapping main.go; merges after #15567.*; one train → no tag | `TestStageTrackerTrainTagOnTwoTrains` |
| `stageTrackerUnderRepair` → `stageTrackerAnchor` (Go: `stageTrackerTrainUnderRepair` → `stageTrackerItemColumn`) | chevron column | *Fixing · @hivecommons-hive[bot]* lit in the **Merge queue** column over the caption *fell back to Fixing — … — conflicting* (hive's own #15574: the refused merge's `merge-failed` gate still promoted the column) | the column follows the current state: under repair → CI / Review, whatever the stale gate or planner train says; the caption keeps the history | `TestStageTrackerItemColumnRatchet`, `TestStageTrackerFixingAfterMergeFailedColumn`, `stage_tracker_fixing_column.test.js` |
| `stageTrackerLeftTrainNote` + `STAGE_TRACKER_LEFT_TRAIN_NOTE` (Go: `stageTrackerLeftTrainNote`, `left_train`) | row hover | *merge train · 2 PRs · 1st: #15405* over a row whose chevron read *Fixing* (hive's own #15405, a maintainer requested changes) | the fixing row rides no train (no header, no ordinal, no rail); its hover ends *Merge train: left the merge train while being fixed; rejoins when green*; a train of one is no train | `TestStageTrackerTrainTagOnTwoTrains`, `TestStageTrackerMergeTrainUnderRepairLeaves` |
| `stageTrackerOrdinal` | chevron, hover, popover | — | `1st 2nd 3rd 4th … 11th 12th 13th 21st 22nd 23rd` | same |
| `stageTrackerTrainStalledWord` + `STAGE_TRACKER_TRAIN_HEAD_STALL_WORD` | chevron label (stalled train), Merge queue popover | *Merge queue stalled · head #15425 waiting for a maintainer (untrusted author) 1h21m* — painted `--yellow` on the amber active chevron, so the row read as an **empty** chevron (#15529, #15530) | rider: *Merge queue · stalled — waiting on #15425 (needs maintainer merge) · 1h21m*; head row: *Merge queue · stalled — needs maintainer merge · 1h21m*; the why comes from `head_state` (`needs-human` → *needs maintainer merge* / *needs a human decision*, `ci` → *waiting for CI*, `soaking` → *soak overran*, `ready` → *waiting for a merge sweep*, `merging` → *merge call in flight*, `fixing` → *being fixed by its author*, `unknown` → *head not in this view*); an unmapped `merge_train.state` falls back to its humanised name + place (*Waiting for front · 3rd*), never `''`; `.ham-seg.active .ham-queue-stalled` inherits the chevron's text colour | `TestStageTrackerMergeTrainStateSlugsNeverLeak` (every state × stalled × head_state × place), `TestStageTrackerMergeTrainPayloadStates` |
| `stageTrackerTrainQueueTip` | chevron hover | *Behind #15253 … Estimated turn: ~25m.* / *Front of the merge queue* | *2nd of 6 in the merge queue · behind #15253 · ETA ~25m.* / *1st of 6 in the merge queue · merges next · ETA ~4m.*; *· merge sweep running* lives here only (one label per chevron, #15416) | same |
| `stageTrackerTrainHeaderText` + `stageTrackerTrainIconHtml` | train header chip | `⛓ merge train · 2 PRs · front #15400` | 🚆 (inline-SVG railcar, hover *Merge queue*) `merge train · 2 PRs · 1st: #15400` | `TestStageTrackerMergeTrain` |
| `stageTrackerQueueMemberState` (`tracker-queue.js`) | Merge queue popover | *front of the queue* | *merges next* | `TestStageTrackerMergeQueuePopover` |
| `stageTrackerAnchor` (`tracker-row.js`) | which column lights | a 1st-in-train head at 050/070 lit **CI / Review** | every train state lights **Merge queue** | `TestStageTrackerMergeTrainStatesLightMergeColumn` |
| `stageTrackerHeaderHtml` | column header | *Merge gate* | 🚆 *Merge queue* (same icon as the chip and the 080 chevron) | `TestStageTrackerHeader…` |
| Go: `prstage.MergeTrain.Sentence`, `stageTrackerMergeTrainFrontCaption`, `eta_basis` | gate reason, caption, hover | *front of the merge train (1st of 3)*, *Front of a merge train of N PRs* | *1st in the merge queue; merges next (1st of 3)*, *1st in a merge queue of N PRs* | `TestParseMergeTrainRejectsMalformed` |
| "merge gate" in decision copy (`delegatedecision` glossary, ladder, dashboard decided/new/vocabulary) | glossary, ⋯ hints, PR comments | *the merge gate merges it once every check passes* | *the merge queue merges it …* | `TestStageTrackerDecisionGlossaryMirrorsGo` |

## Soaking rows (hivecommons/hive#15575)

The owner read a soaking row as **🕓 Soaking · ready — merge sweep…**
(truncated) with the caption *…the pre-merge soak window is running —
soaking, 0s left at the last sweep*. "Merge sweep" is the lane's internal
name for its pass, not a state the reader cares about, and "0s left" on a
row still called *soaking* was the lane's last read, not the time now. The
three states, in the owner's words:

| State | Chevron | Hover / caption |
|---|---|---|
| soak running | **Soaking · 4m left** (counts down in place) | *Head abc1234 must sit 30 minutes without new commits before the hive merges it · 4m 10s left · soak ends 8:42 PM* / caption *Admitted by the automerge lane; soaking — 4m left* |
| soak elapsed, waiting for the lane's next pass | **Soaking · complete** | *… soaked since 8:42 PM. Soak complete — merges on the next pass (next pass in 3m).* / caption *Admitted by the automerge lane; soak complete — merges on the next pass* |
| soak elapsed and the lane's pass is on it | **Mergeable · merging now** (⟳ spins) | *… Mergeable — merging now.* |

| Builder | Reaches | Before | After | Held by |
|---|---|---|---|---|
| `stageTrackerSoakWords` (`tracker-words.js`) | 095 chevron label + hover, 060/080 detail | *Soaking · ready — next sweep in 3m* / *ready — merge sweep running* / *ready — sweep due now*; 060/080 *soaked — next sweep in 42s* | *Soaking · complete* / **Mergeable · merging now**; 060/080 *soak complete* / *soaked — merging now* | `TestStageTrackerSoakNode` |
| `stageTrackerSoakTick` | the same label between polls | detail only | detail and, at 095, the word (*Soaking* → *Mergeable* when the pass was already on it) | same |
| `stageTrackerNextPassWords` (new) + `stageTrackerGateCountdownSync` | 080 detail, soaking-without-clock detail, hovers | *next sweep in 42s* / *sweep due now* / *soak window complete* | *next pass in 42s* / *pass due now* / *complete* | `TestStageTrackerMergeGatePillNode` |
| `stageTrackerMergeGateInfoFor` (pass on a pre-admission row) | 060 detail + hover | *merge sweep running* / *hive's merge sweep is running on this PR now.* | *checking now* / *hive is checking this PR now.* | same |
| `stageTrackerTrainEtaDetail`, `STAGE_TRACKER_TRAIN_HEAD_STALL_WORD.ready` | queue chip, stalled train | *any sweep now*, *waiting for a merge sweep* | *any moment now*, *waiting for the next merge pass* | `TestStageTrackerMergeTrainQueueChip` |
| Go `stageTrackerSoakCaption` (new), `humanizeStageReason` | 095 caption | *…soak window is running — soaking, 0s left at the last sweep* (the lane's last read, never aged) | *…soaking — 7m left* aged from the last read, *…soak complete — merges on the next pass* once it runs out | `TestStageTrackerSnapshot…` (pr102) |
| Go `mergeTrainHead…` reasons, `eta_basis`, `stageTrackerMergingCaption`, decided *Approved* text | gate reason, hover, caption | *merges on hive's next sweep*, *next sweep in 40s*, *the automerge sweep's merge call* | *merges on hive's next pass*, *next pass in 40s*, *the automerge lane's merge call* | `TestStageTrackerMergeTrainQueue…` |

Left for a follow-up under the same issue: the ⋯ decision consequences and
`Next` sentences (*Hive's merge sweep merges it on its next pass…* in
`api_repo_item_decision.go`, `row_action_result.go`,
`delegated_decision_human_merge.go`) and `STAGE_TRACKER_MERGE_PATH_WORDS.sweep`
(*automerge sweep* as a merge **path** name on merged rows).

## Hive notes, show then tuck

| Builder | Reaches | Words | Held by |
|---|---|---:|---|
| `stageTrackerEventHtml` with auto-collapse on (`tracker-motion.js`) | the row's event line, open for 30 s then tucked into a ⬡-i button | the line itself unchanged; the button adds `aria-label` *Hive note* (2) | `TestStageTrackerNoteTuckNode` |
| `stageTrackerNoteTitle` | the button's tooltip | the line and its age: *Hive: moved to CI / Review · 2m ago* (≤ line + 3) | same |
| `stageTrackerNoteTuckSectionHtml` (`tracker-sound.js`) | ⚙ → Hive notes | title (2), hint (≈ 70), *Auto-collapse hive notes (default on)* (5) | `TestStageTrackerNoteTuckWiring` |
