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
> [ Merge it ] [ More ⋯ ]
> Asked by the hive · 7 hours ago
> Details ▸ *Hive never merges this on its own; a maintainer merges here or on GitHub. To trust @odttlnt: add them as a merger under ⚙ Security → Authorized users. · Ready for human merge · untrusted author · Next move: human — a maintainer merges it*

## Server-side builders (Go, `src/pkg/dashboard`)

| Builder | Reaches | Before | After | Held by |
|---|---|---:|---:|---|
| `stageTrackerUntrustedAuthorWhy` (all 10 trust reasons, summed) | question, reason text, delegated outcome | 314 | 68 | `TestStageTrackerHumanMergeSaysItOnce` (no config keys, no jargon) |
| `stageTrackerUntrustedAuthorFix` (new; the how-to-trust line, Details only) | Details | — | 81 | same |
| `stageTrackerHumanMergeReasonWords` tips (9 codes, summed) | chip hover, 090 chevron tip | 227 | 101 | `TestStageTrackerHumanMergeReason` |
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
| Confirm: Approve & merge as maintainer | dialog | 76 | 53 | same |
| Confirm: Approve — let hive merge | dialog | 62 | 49 | same |
| ⋯ menu hints (`stageTrackerLeadActions` → `ham-menu-hint`) | ⋯ menu | server consequences (11–31 each) | server consequences (≤ 12 each) | Go budget test |
| Toasts (`STAGE_TRACKER_ACTING_CAPTIONS`, acted labels) | toast | 2–4 each | unchanged | — |

## Totals

| | Before | After |
|---|---:|---:|
| Server builders above (sum of longest branches) | ≈ 1,350 | ≈ 620 (of which ≈ 110 only ever shown inside Details) |
| #15315 hover, visible | ≈ 384 | ≈ 29 |

## Left as-is (follow-up)

- Merge-gate (080/095/100) chip tips in `stageTrackerMergeGateInfo` still say
  *sweep* and *soak* ("next sweep in 42s", "soak 12m left"); they are live
  clocks, short already, and a rename touches the gate's own tests. Tracked
  under the same issue.
- The stage descriptions in `stagelabels.Table` are shared with the labels
  and the docs and were not shortened; the 090 chevron tip simply stopped
  appending them.
