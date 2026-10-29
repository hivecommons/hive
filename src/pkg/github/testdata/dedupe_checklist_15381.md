<!-- hive:dedupe-checklist repo=hivecommons/hive -->
Decision required: no

## Summary

Triage pass over the 28 open issues and 12 open PRs in hivecommons/hive as of
2026-10-29. Found 0 near-duplicate pairs, 1 stacked-PR merge-order dependency,
1 same-root-cause family worth cross-referencing, and 0 missing PR↔issue links
(all 11 non-stacked open PRs carry a closing keyword). No actions have been
taken; this is a checklist for maintainers. Inventory note: `gh search prs`
returned an empty index for this repo, so the PR inventory comes from the hive
actionable snapshot (13 PR rows, 2026-10-29T07:32 ET) plus the held PR #15303;
issues from search (28 of 28).

## Conflicting PRs: merge order matters

- #15380 is stacked on #15308 (its base branch *is* #15308's head
  `hive/adopt-15226`; it edits `automerge_native_queue.go`,
  `derive_pr_classify.go`, `prstage/reasons.go` that only exist in #15308).
  Merge #15308 first, then retarget/merge #15380 — already declared in
  #15380's body, so this is a sequencing reminder only.

## Related but distinct: cross-link only

- #15311 and #15313 — two `agent_pane_timeout`-family self-reports filed the
  same day by the same bot, but different findings: #15311 is the truncated
  `tail_500` pane excerpt (fixed by PR #15319, `Closes #15311`), #15313 is the
  pre-restart kick-undeliverable WARN flood (fixed by PR #15315,
  `Fixes #15313`). Not duplicates; each has its own PR.
- PRs #15315 and #15374 both edit `src/pkg/fleetreport/fleetreport.go`, but in
  disjoint functions (`benignSubstrings` @787 vs `IsGitHubUpstreamUnavailable`/
  `IsBenignMessage` @853–894). No semantic overlap; whichever lands second gets
  a trivial context-shift rebase at most.
- PRs #15319 and #15374 both edit `slogfeed/slogfeed_test.go`, in disjoint
  tests (@76 vs @571). Not duplicates.
- Issues #15364 (rate-limited label reconciliation WARNs), #15369 (gateway
  errors counted), #15376 (missing review-verdict artifact warns) plus #15313
  are one symptom family — benign operational noise crossing the fleet
  self-report threshold — with four separate single-site fixes (PRs #15367,
  #15374, #15378, #15315). Sites are distinct; no consolidation needed, just
  awareness that they all tune the same classifier/threshold machinery.

## Checked and not duplicates

- #15340 vs #15182 — near-identical reviewer-backlog titles ("Previous
  security finding … unchanged"), but different findings on different PRs:
  #15340 tracks the run-time script fetch in
  `.github/workflows/crux-file-size-ratchet.yml:48` (blocked on open PR
  #15332); #15182 tracks `src/pkg/rebasemerge/rebasemerge.go` from PR #15104.
- PR↔issue pairs verified (closing keyword present in each PR body):
  #15307→#15133, #15308→#15198 (also supersedes closed #15226), #15312→#15273
  (body also covers the #15155 duplicate), #15315→#15313, #15319→#15311,
  #15324→#15320, #15332→#15331 (Refs #15330), #15343→#15338, #15367→#15364,
  #15374→#15369, #15378→#15376, #15380→#15377. Nothing to add.

---
*Filed by dedupe agent (issues-only mode). Recommendations only — nothing has been closed, labelled, or merged.*

---
🐝 **Hive Agent**: `dedupe` | **Instance:** `hosted-available-oke-11-placeholder-r05x` | **SHA:** `unknown`
<!-- hive:origin hive=hosted-available-oke-11-placeholder-r05x at=2026-10-29T11:43:37Z -->

— hive: agent=dedupe backend=copilot model=claude-fable-5 copilot=1.0.88