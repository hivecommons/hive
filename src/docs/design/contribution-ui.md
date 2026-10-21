# Contribution UI

**Status: partly shipped (v5).** Design record and acceptance checklist for
RFC #13153, which asked for one shared-model contribution tree behind
`hivectl hives tree`, the `hivectl tui --hives` overlay and the loopback
`hivectl hives web` page. The operator-facing documentation is
[`hivectl.md`](../hivectl.md#hives--named-profiles-for-the-hives-you-contribute-to);
this page records what each RFC section demanded, where it lives in code, how
it is tested, and what is still open. It is not rewritten as later phases land;
the checklist below says which rows are shipped and which are not.

## Problem

A contributor who belongs to several Hives reached by several hubs had three
different ways to change what their relay works on (`hives repos`, the TUI and
the Web page), none of which showed the same picture. Each surface kept its own
idea of "selected", the Web page could save a change that silently lost a
concurrent CLI edit, a saved change looked identical to an applied one, and
excluded repositories were indistinguishable from repositories that were simply
not picked. Legacy profiles (hive membership without a reviewed selection) were
either migrated silently or shown with a vacuous checked state.

## Shape of the answer

One read model, one staging model, one apply path; three renderers.

- **Read model** — `hivectl.ContributionTree` (`pkg/hivectl/contribution_tree.go`)
  is built from the saved `ProfileSet`, the recorded hubs (`hubs-seen.json`) and
  the discovered Hive→repository catalogue. Every node carries an explicit
  tri-state (`selected`, `deselected`, `excluded`, `legacy`, `unknown`), the
  source of each exclusion, and the follow policy that applies to it. Nothing
  about the tree is inferred from the absence of data.
- **Staging model** — `hivectl.StagedEdits` (`pkg/hivectl/contribution_stage.go`)
  holds a reviewed diff against the revision the tree was read from. Edits are
  pure; `Apply` is the only write.
- **Apply path** — `ProfileStore.CommitExpecting` under the cross-process lock
  (`pkg/hivectl/profiles_lock.go`) refuses to write over a revision the operator
  did not review (`ErrStaleRevision`), then `SignalRunningRelay` asks the relay
  to reload. The `ApplyReceipt` reports `Saved`, `RelayReload` and `Acknowledged`
  as three separate facts because they are three separate outcomes.
- **Runtime overlay** — `TreeRuntime{Live, Durable}`
  (`pkg/hivectl/contribution_runtime.go`) layers relay liveness and local
  activity on the tree after it is built; the overlay is read-only and a
  missing relay or missing activity store is a labelled state, never a blank.
- **Renderers** — CLI (`commands/hives_tree.go`), Web
  (`commands/hives_web_tree*.go`) and TUI (`tui/hives_tree.go`,
  `tui/panes/hives_tree.go`) each format the same `ContributionTree` and
  `StagedEdits`; none of them owns selection logic.

## Invariants the RFC fixed and where they are enforced

| Invariant | Enforced by | Test |
|---|---|---|
| No vacuous checked state: a legacy profile is never shown selected | `TreeSelection.Legacy` + `StagedEdits.ReviewAndRestrict` required before any legacy edit | `TestStagedLegacyConversionAndAtomicBranchRefusal` |
| Exclusion ≠ deselection | `ProfileSet.DeselectCurrent` keeps exclusions; clearing an exclusion never selects | `TestDeselectCurrentPreservesFollowAndOverrides`, `TestStagedClearExclusionDoesNotApproveOrRemoveInclusion` |
| Deselect-current ≠ select-none | `DeselectCurrent` retains follow policy and personal overrides; `SetSelectionMode(none)` is a different edit | `TestStagedDeselectRetainsFollowAndExclusions` |
| No silent legacy migration | legacy branches refuse atomic conversion unless reviewed | `TestStagedLegacyConversionAndAtomicBranchRefusal` |
| No lost concurrent edit | `CommitExpecting` + lock; stale revision needs a fresh review | `TestStagedStaleApplyRequiresNewReview`, `TestCommonsWebRejectsAnEditReviewedAgainstAStaleRevision` |
| Truthful receipt | `ApplyReceipt{Saved, RelayReload, Acknowledged}`; a signal failure never hides a saved change | `TestStagedApplyReceipts`, `TestCommonsWebSignalErrorDoesNotHideSavedChange` |
| No new kill guarantee | apply only signals reload; no process control is added | `TestStagedApplyUsesRealMissingRelayResult` |
| No cross-Hive upload | the tree and receipts carry no credentials; cross-Hive exclusion is disclosed, not copied | `TestStagedDiffNeverContainsSecrets`, `TestStagedCrossHiveExclusionDisclosure` |
| Web is loopback-only with token, Host and Origin checks | `validateLoopbackListenAddr`, header/token middleware | `TestCommonsWebListenAddressMustBeLoopback`, `TestCommonsWebRejectsNonLoopbackHost`, `TestCommonsWebOriginValidation` |
| No inline styles in Web HTML | style ratchet | `TestCommonsTreePageMarkupIsAccessibleAndStyleFree` |
| No `time.Sleep` in tests | sleep ratchet; runtime tests use package-var overrides for relay probes | `contribution_runtime_test.go` |

## Acceptance checklist by RFC section

Each row names the shipped behaviour, the pull request that landed it, and the
test that pins it. "Stack" means the PR merged into `stack/contribution-ui-base`,
which lands on `v5` as one reviewed sequence.

### §1 Tree

| Criterion | Status | Where | PR |
|---|---|---|---|
| hub → Hive → repository tree from saved profiles + `hubs-seen.json` + discovery | shipped | `ContributionTree`, `BuildContributionTree` | #13205 |
| tri-state per node, exclusion source, follow policy visible | shipped | `TreeRepository.Selected`, `TreeRepository.ExcludedBy`, `TreeSubscription.Follow`, `TreeSelection.State` | #13205 |
| CLI `hives tree` text/json/yaml | shipped | `commands/hives_tree.go` | #13205 |
| Web tree page with the same model | shipped | `commands/hives_web_tree_page.go` | #13306 |
| TUI tree pane with the same model | shipped | `tui/panes/hives_tree.go` | #13315, #13324 |

### §2 Selection and exclusions

| Criterion | Status | Where | PR |
|---|---|---|---|
| select / deselect / deselect-current / select-none as distinct edits | shipped | `ProfileSet.SetSelectionMode`, `ProfileSet.DeselectCurrent`, `StagedEdits.SelectRepository`, `StagedEdits.SelectCurrent` | v5 base, #13295 |
| exclusions survive deselection; clearing an exclusion is its own edit | shipped | `TestStagedClearExclusionDoesNotApproveOrRemoveInclusion` | #13295 |
| cross-Hive exclusion disclosed with remaining source named | shipped | `TestStagedClearingOneVetoNamesRemainingSource` | #13295 |
| exclusion union across profiles vs Hive-local | **owner decision pending** | see "Open questions" | — |

### §3 Future additions and upstream review

| Criterion | Status | Where | PR |
|---|---|---|---|
| follow-new-repositories per Hive, toggled from every surface | shipped | `StagedEdits.SetFollow`; TUI `f`/`F`; Web follow control | v5 base, #13324, this page's PR |
| hub follow policy shown read-only in the tree | shipped | `TreeHub.Follow`; edited via `hives hub follow` only | #13205 |
| hub enable/disable keeps membership and follow | shipped | `TestStagedHubDisablePreservesMembershipAndFollow` | #13295 |

### §4 Legacy profiles

| Criterion | Status | Where | PR |
|---|---|---|---|
| legacy shown as its own state, never checked | shipped | `TreeSelection.Legacy` | #13205 |
| "Review and restrict" converts explicitly, per Hive | shipped | `StagedEdits.ReviewAndRestrict`; TUI `R`; Web button | #13295, #13324, #13306 |
| atomic branch conversion refused without review | shipped | `TestStagedLegacyConversionAndAtomicBranchRefusal` | #13295 |

### §5 Staged apply, receipts, cross-process lock

| Criterion | Status | Where | PR |
|---|---|---|---|
| staged diff reviewed before write; empty diff writes nothing | shipped | `StagedEdits.Diff`, `TestStagedEmptyApplyWritesAndSignalsNothing` | #13295 |
| lock + expected revision around read/validate/commit | shipped | `profiles_lock.go`, `CommitExpecting`, `ErrStaleRevision` | v5 base |
| receipt separates Saved / Relay-reload / Acknowledged | shipped | `ApplyReceipt` | #13295, #13296 |
| Web apply form shows receipt verbatim | shipped | `hives_web_tree.go` | #13306 |
| TUI apply (`A`) shows receipt in detail | shipped | `tui/hives_tree.go` | #13324 |

### §6 Live and durable activity

| Criterion | Status | Where | PR |
|---|---|---|---|
| live relay status (adopted scope version, idle reason, subscriptions) on every surface | shipped | `TreeLive`, `ProfileStore.LiveTreeRuntime` | #13296, #13322 |
| durable activity per repository (attempts, completed, last activity, retention) | **interface shipped, store pending** | `DurableActivitySource`, `TreeDurable`; `UnavailableDurable` labels the gap | #13322; store is #13297 |
| unavailable runtime is a labelled state, never blank | shipped | `TreeRuntimeUnavailable`, `DurableActivityNotShipped` | #13322 |

### §10 Surface parity

| Criterion | Status | Note |
|---|---|---|
| CLI / Web / TUI render the same tree and staging model | shipped | all three call `BuildContributionTree` and `StagedEdits` |
| TUI `tab` focus cycling between tree and detail | **not applicable** | the TUI detail is inline under the selected row; `A`/`U` act on staging directly, so there is no second focus target |
| TUI explicit "stop following" distinct from toggle | shipped | `F` key; `f` remains the toggle |

## Open questions (owner decisions, not guessed)

1. **Exclusion union.** Today an exclusion recorded in any profile that reaches
   a repository applies to that repository in every Hive (union semantics), and
   the tree discloses the remaining source when one veto is cleared. The RFC
   left open whether exclusions should instead be Hive-local. Changing this
   alters saved-profile semantics and needs an owner call; the tree and staging
   model already carry per-source exclusions so either answer is a model change
   only, not a renderer change. Recommendation: keep union, document it.
2. **Durable activity store.** #13297 implements the JSONL store with retention
   but targets `v6`. Options: (a) retarget to `v5` and wire it into
   `DurableActivitySource` (small, recommended); (b) cherry-pick onto the stack;
   (c) re-implement on `v5`. Until one lands, every surface shows
   `no local activity store on this build (#13297)` rather than an empty table.

## Testing and CI notes

- Coverage floors: `pkg/hivectl` 88 %, `pkg/hivectl/commands`, `pkg/tui`,
  `pkg/tui/panes` 90 %. Measured with `go test -short -race -cover`.
- Pull requests whose base is `stack/contribution-ui-base` do not get the Go
  test workflow (it triggers on `v4`/`v5`/`v6` bases only); they were validated
  locally and are covered again when the stack lands on `v5`.
