package dashboard

import (
	"strings"
	"testing"
)

// hivecommons/hive#15495 / #15500: an issue whose only two PRs were a
// contributor's original and the hive's adoption copy of it read "Let hive
// decide — all 2 PRs" with the caption "fell back to Covered by pr —
// Covered by PR #15537 — …; PR #15508 — … changes requested". The pair is
// one logical PR: the server caption speaks for the copy only and the SPA
// counts one open PR, so nothing on the row asks a person.

func adoptionPairIssue() StageTrackerItem {
	return StageTrackerItem{Kind: "issue", Repo: "hivecommons/hive", Number: 15495, Stage: trackerNumberCoveredByPR,
		LinkedPRs: []StageTrackerLinkedPR{
			{Number: 15508, Author: "mendezr", AuthorKind: "clanker", Stage: trackerNumberReturned, State: "open", AdoptedBy: 15537,
				Caption: "Hive adopted this PR; hive is addressing the review findings (superseded by #15537) — changes requested",
				Lineage: &StageTrackerLineage{Kind: lineageSuperseded, ToNumber: 15537}},
			{Number: 15537, Author: "hivecommons-hive[bot]", AuthorKind: "hive", Stage: trackerNumberReviewPending, State: "open",
				Caption: "Review swarm requested; waiting for a verdict on the current head — review gate: no review bundle",
				Lineage: &StageTrackerLineage{Kind: lineageAdopted, FromNumber: 15508, FromAuthor: "mendezr", FromAuthorKind: "clanker"}},
		}}
}

func TestStageTrackerLinkedPRProgressCollapsesAdoptionPair(t *testing.T) {
	it := adoptionPairIssue()
	stageTrackerLinkedPRProgress(&it)
	if it.Caption != "Covered by #15537 (adopted from @mendezr's #15508) — in CI / review" {
		t.Fatalf("caption = %q", it.Caption)
	}
	if strings.Contains(it.Caption, "changes requested") || strings.Contains(it.Caption, "PR #15508") {
		t.Fatalf("the superseded original's verdict reached the issue: %q", it.Caption)
	}

	// The copy closed without merging: the original speaks for itself again.
	it = adoptionPairIssue()
	it.LinkedPRs[0].AdoptedByState = stageTrackerAdoptedClosed
	it.LinkedPRs[1].State = "closed"
	stageTrackerLinkedPRProgress(&it)
	if !strings.HasPrefix(it.Caption, "Covered by PR #15508 — ") || strings.Contains(it.Caption, "PR #15537") {
		t.Fatalf("orphaned original must carry the issue: %q", it.Caption)
	}

	// A closed original whose copy merged is not listed as progress at all.
	it = adoptionPairIssue()
	it.LinkedPRs[0].State = "closed"
	it.LinkedPRs[0].Lineage = nil
	it.LinkedPRs[1].Lineage = nil
	stageTrackerLinkedPRProgress(&it)
	if it.Caption != "Covered by PR #15537 — "+adoptionPairIssue().LinkedPRs[1].Caption {
		t.Fatalf("closed original listed: %q", it.Caption)
	}

	// Two unrelated open PRs still list both — the dup-PR path is untouched.
	it = adoptionPairIssue()
	it.LinkedPRs[0].AdoptedBy, it.LinkedPRs[0].Lineage, it.LinkedPRs[1].Lineage = 0, nil, nil
	stageTrackerLinkedPRProgress(&it)
	if !strings.Contains(it.Caption, "PR #15508 — ") || !strings.Contains(it.Caption, "; PR #15537 — ") {
		t.Fatalf("independent PRs collapsed: %q", it.Caption)
	}
}

func TestStageTrackerAdoptionPairIsOneOpenPR(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const ids = as => as.map(a => a.id);
const ctx = { user: true, canHold: true, canHuman: true };
const pair = { repo: 'hivecommons/hive', kind: 'issue', number: 15495, url: 'u', stage: 40, modifiers: [],
  linked_prs: [
    { number: 15508, title: 'Link memo', url: 'p1', author: 'mendezr', author_kind: 'clanker', stage: 72, state: 'open', adopted_by: 15537, modifiers: ['needs-decision'] },
    { number: 15537, title: 'Link memo', url: 'p2', author: 'hivecommons-hive[bot]', author_kind: 'hive', stage: 50, state: 'open', modifiers: [], lineage: { kind: 'adopted', from_number: 15508, from_author: 'mendezr' } },
  ] };
assert.deepStrictEqual(stageTrackerNestedOpenPRs(pair).map(x => x.number), [15537], 'the adopted original is not an open PR of the issue');
assert.deepStrictEqual(stageTrackerParkedNestedPRs(pair), [], "the original's changes-requested does not park the issue");
assert.strictEqual(stageTrackerDecisionTarget(pair), null);
assert.deepStrictEqual(stageTrackerDelegateTargets(pair), [], 'nothing for a person to decide');
let as = stageTrackerRowActions(pair, ctx);
assert.ok(!ids(as).includes('decide:delegate-all') && !ids(as).includes('decide'), ids(as).join(','));
assert.strictEqual(stageTrackerLeadAction(pair, ctx), null, 'no inline "Let hive decide" lead on an adoption pair');
assert.deepStrictEqual(stageTrackerLinkedPRItem(pair, pair.linked_prs[1]).siblings, [], 'the copy has no competing sibling');

// The copy closed without merging: the original is open again and the two
// rows are what they are.
const orphaned = { ...pair, linked_prs: [{ ...pair.linked_prs[0], adopted_by_state: 'closed' }, { ...pair.linked_prs[1], state: 'closed' }] };
assert.deepStrictEqual(stageTrackerNestedOpenPRs(orphaned).map(x => x.number), [15508]);
// A copy listed under a different issue does not collapse this one.
const elsewhere = { ...pair, linked_prs: [pair.linked_prs[0], { number: 999, url: 'x', stage: 50, state: 'open', modifiers: [] }] };
assert.deepStrictEqual(stageTrackerNestedOpenPRs(elsewhere).map(x => x.number), [15508, 999]);
assert.strictEqual(stageTrackerRowActions(elsewhere, ctx)[0].id, 'decide:delegate-all', 'two genuinely competing PRs still offer the fan-out');
`)
}

func TestStageTrackerEventTextUnmirroredCoveredByPR(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_EVENT_COVERED_BY_PR"),
		jsConstBlock(t, html, "STAGE_TRACKER_EVENT_MOD_WORDS"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerStageName"),
		jsFunc(t, html, "stageTrackerStateMap"),
		jsFunc(t, html, "stageTrackerStageFell"),
		jsFunc(t, html, "stageTrackerEventText"),
		`
const stages = [{ number: 40, label: 'stage/040-covered-by-pr', short: 'Covered by PR' }, { number: 50, label: 'stage/050-review-pending', short: 'CI / Review' }];
const caption = "Covered by #15537 (adopted from @mendezr's #15508) — in CI / review";
const before = stageTrackerStateMap([{ repo: 'o/r', kind: 'issue', number: 15495, stage: 50, stage_label: 'stage/050-review-pending', mirrored: true, caption: 'Review swarm requested' }]);
const after = stageTrackerStateMap([{ repo: 'o/r', kind: 'issue', number: 15495, stage: 40, stage_label: 'stage/040-covered-by-pr', caption }]);
const key = 'o/r/issue#15495';
assert.strictEqual(before[key].kind, 'issue'); assert.strictEqual(before[key].mirrored, true); assert.strictEqual(after[key].mirrored, false);
// The claim moved from the original to the copy: not a fall, and the stage
// table's own word, never the slug humanised ("Covered by pr").
assert.strictEqual(stageTrackerEventText(before[key], after[key], stages), 'moved to Covered by PR — ' + caption);
// A PR genuinely dropping 50 → 40 (no falls_to table) still reads as a fall.
const prBefore = { stage: 50, label: 'stage/050-review-pending', caption: '', kind: 'pr', mirrored: false, mods: [] };
const prAfter = { stage: 40, label: 'stage/040-covered-by-pr', caption: 'PR closed', kind: 'pr', mirrored: false, mods: [] };
assert.strictEqual(stageTrackerEventText(prBefore, prAfter, stages), 'fell back to Covered by PR — PR closed');
// Without a stages table the label still humanises.
assert.strictEqual(stageTrackerEventText(prBefore, prAfter, []), 'fell back to Covered by pr — PR closed');
`,
	}, "\n"))
}
