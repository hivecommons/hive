package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#19843: an outside contributor files an issue and opens a
// PR for it. The reporter-trust gate keeps the issue in the triage bucket —
// correctly — but the row read a bare "Triage needed" with no hint the PR
// was ready, while the maintainer's real decision is "accept the issue and
// review PR #N". The tracker now names the open contributor PR on every
// triage-waiting issue (015, the needs-triage pill, a 000-new hold); the
// gate itself is unchanged.

func triageNewHold() *StageTrackerNew {
	return &StageTrackerNew{NextMover: StageTrackerMoverMaintainer, Gates: []StageTrackerGate{
		{Name: "triage", State: StageTrackerGateNext},
		{Name: "actionable", State: StageTrackerGateLater},
	}}
}

func TestStageTrackerTriagePRNamesContributorPR(t *testing.T) {
	items := []StageTrackerItem{
		// 015 Triage needed; the contributor's PR closes it.
		{Repo: "o/r", Kind: "issue", Number: 1, Stage: stagelabels.TriageNeeded.Number(), Caption: stageTrackerTriageNeededCaption},
		{Repo: "o/r", Kind: "pr", Number: 7, Title: "Fix the widget", Author: "zoe", AuthorKind: authorKindHuman, URL: "https://github.com/o/r/pull/7",
			LinkedIssues: []StageTrackerLinkedIssue{{Number: 1}}},
		// A 000-new reporter-trust hold before the awaiting label lands.
		{Repo: "o/r", Kind: "issue", Number: 2, New: triageNewHold(), Caption: stageTrackerTriageNeededCaption},
		{Repo: "o/r", Kind: "pr", Number: 8, Author: "zoe", AuthorKind: authorKindHuman, URL: "https://github.com/o/r/pull/8",
			LinkedIssues: []StageTrackerLinkedIssue{{Number: 2}}},
		// Triage already accepted: 010 with a covering PR is ordinary work, so the
		// row keeps the ordinary "Covered by PR" caption, not the triage one.
		{Repo: "o/r", Kind: "issue", Number: 3, Stage: stagelabels.Actionable.Number(), Caption: "Triaged and eligible"},
		{Repo: "o/r", Kind: "pr", Number: 9, Author: "zoe", AuthorKind: authorKindHuman, LinkedIssues: []StageTrackerLinkedIssue{{Number: 3}}},
		// Triage needed, but the only PR is the hive's own: not a contributor PR.
		{Repo: "o/r", Kind: "issue", Number: 4, Stage: stagelabels.TriageNeeded.Number(), Caption: stageTrackerTriageNeededCaption},
		{Repo: "o/r", Kind: "pr", Number: 10, AuthorKind: authorKindHive, LinkedIssues: []StageTrackerLinkedIssue{{Number: 4}}},
		// Triage needed with no PR at all.
		{Repo: "o/r", Kind: "issue", Number: 5, Modifiers: []string{modNeedsTriage}, Caption: stageTrackerTriageNeededCaption},
	}
	stageTrackerResolveLinkedPRs(items, nil)
	stageTrackerApplyTriagePR(items)

	byNum := map[int]StageTrackerItem{}
	for _, it := range items {
		if it.Kind == "issue" {
			byNum[it.Number] = it
		}
	}
	one := byNum[1]
	if one.TriagePR == nil || one.TriagePR.Number != 7 || one.TriagePR.URL != "https://github.com/o/r/pull/7" || one.TriagePR.Author != "zoe" || one.TriagePR.Title != "Fix the widget" {
		t.Fatalf("015 issue triage_pr = %+v, want PR #7 by zoe", one.TriagePR)
	}
	if want := "Triage needed · contributor PR #7 open — accept the issue to review the PR, or close"; one.Caption != want {
		t.Fatalf("015 caption = %q, want %q", one.Caption, want)
	}
	if one.Stage != stagelabels.TriageNeeded.Number() {
		t.Fatalf("display only: the stage must stay 015, got %d", one.Stage)
	}
	if two := byNum[2]; two.TriagePR == nil || two.TriagePR.Number != 8 || !strings.Contains(two.Caption, "contributor PR #8 open") {
		t.Fatalf("000-new hold = %+v / %q, want PR #8 named", two.TriagePR, two.Caption)
	}
	if three := byNum[3]; three.TriagePR != nil || three.Caption != "Covered by PR #9" {
		t.Fatalf("an accepted issue keeps its row: %+v / %q", three.TriagePR, three.Caption)
	}
	if four := byNum[4]; four.TriagePR != nil || four.Caption != stageTrackerTriageNeededCaption {
		t.Fatalf("a hive PR is not a contributor PR: %+v / %q", four.TriagePR, four.Caption)
	}
	if five := byNum[5]; five.TriagePR != nil || five.Caption != stageTrackerTriageNeededCaption {
		t.Fatalf("no PR, no change: %+v / %q", five.TriagePR, five.Caption)
	}
}

// A closed (superseded) PR still listed under the issue is not "open".
func TestStageTrackerTriagePRSkipsClosedPR(t *testing.T) {
	items := []StageTrackerItem{{Repo: "o/r", Kind: "issue", Number: 1, Stage: stagelabels.TriageNeeded.Number(),
		LinkedPRs: []StageTrackerLinkedPR{{Number: 7, State: "closed"}, {Number: 8, SupersededBy: 9}, {Number: 11, URL: "u11"}}}}
	stageTrackerApplyTriagePR(items)
	if items[0].TriagePR == nil || items[0].TriagePR.Number != 11 {
		t.Fatalf("triage_pr = %+v, want the open PR #11", items[0].TriagePR)
	}
}

// The row menu: Accept triage still leads, and the contributor PR to review
// sits right under it; Accept triage's hint says what accepting unlocks.
func TestStageTrackerTriagePRMenu(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const ids = as => as.map(a => a.id);
const ctx = { user: true, canHold: true, canHuman: true, login: 'octo' };
const triage = { repo: 'o/r', kind: 'issue', number: 1, url: 'https://github.com/o/r/issues/1', stage: 15, modifiers: [],
  triage_pr: { number: 7, url: 'https://github.com/o/r/pull/7', author: 'zoe' } };
const as = stageTrackerRowActions(triage, ctx);
assert.deepStrictEqual(ids(as).slice(0, 2), ['accept-triage', 'open:triage-pr'], ids(as).join(','));
const open = as[1];
assert.strictEqual(open.label, 'Open contributor PR #7');
assert.strictEqual(open.url, 'https://github.com/o/r/pull/7');
assert.strictEqual(open.enabled, true);
assert.strictEqual(open.hint, 'review the PR by @zoe on GitHub');
assert.ok(as[0].hint.endsWith('; PR #7 can then be reviewed and merged'), as[0].hint);

// Without a contributor PR the menu is exactly as before.
const plain = { ...triage, triage_pr: undefined };
assert.ok(!ids(stageTrackerRowActions(plain, ctx)).includes('open:triage-pr'));
assert.ok(!stageTrackerRowActions(plain, ctx)[0].hint.includes('PR #'));
// Once triage is accepted the row is no longer waiting on it.
const accepted = stageTrackerTriageAccepted(triage);
assert.strictEqual(accepted.triage_pr, undefined, 'the accepted row drops triage_pr');
assert.ok(!ids(stageTrackerRowActions(accepted, ctx)).includes('open:triage-pr'));
// A missing link disables the entry with the reason.
const noURL = stageTrackerTriagePRAction({ ...triage, triage_pr: { number: 7 } });
assert.strictEqual(noURL.enabled, false); assert.strictEqual(noURL.why, 'No GitHub link');
assert.strictEqual(stageTrackerTriagePRAction({ ...triage, kind: 'pr' }), null);
`)
}
