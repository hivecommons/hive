package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// A `blocked` review-backlog issue is folded under its open source PR
// (review_note_of / review_notes) instead of standing as a row of its own:
// 16 of the 62 open hivecommons/hive items were such notes.
func TestStageTrackerFoldsBlockedReviewNotesUnderSourcePR(t *testing.T) {
	now := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	s := newTestServer()
	filed := now.Add(-5 * time.Hour)
	status := minimalPayload()
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: 15582, Title: "warn once per PR", Author: "pat", State: "open"}}},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 15627, Title: "Review backlog (style): New test re-covers prWarnOnce semantics", Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked on hivecommons/hive#15582"},
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 15629, Title: "[reviewer] Review backlog (intent-alignment): Previous style note still present", Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked on hivecommons/hive#15582"},
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 15628, Title: "Review backlog (security): Previous style finding still present", Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked on hivecommons/hive#15582"},
			// Source PR not in the snapshot (state unknown): still a note of that PR.
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 15630, Title: "Review backlog (correctness): Prior correctness note still present", Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked on hivecommons/hive#15621"},
			// Blocked on something that is not a review-backlog note: a row.
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 15700, Title: "Ship the thing", Labels: []string{"blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked on hivecommons/hive#15582"},
			// Blocked on a PR in another repo: not folded.
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 15701, Title: "Review backlog (style): elsewhere", Labels: []string{"agent/reviewer", "blocked"}, CreatedAt: filed, Bucket: "filtered", Reason: "blocked on hivecommons/pluk#7"},
		},
	}}
	s.UpdateStatus(status)
	tr := s.buildStageTracker(status, nil, now)

	for _, n := range []int{15627, 15628, 15629} {
		if it := trackerItem(t, tr, "issue", n); it.ReviewNoteOf != 15582 {
			t.Errorf("#%d review_note_of = %d, want 15582 (title %q)", n, it.ReviewNoteOf, it.Title)
		}
	}
	if it := trackerItem(t, tr, "issue", 15630); it.ReviewNoteOf != 15621 {
		t.Errorf("#15630 review_note_of = %d, want 15621", it.ReviewNoteOf)
	}
	for _, n := range []int{15700, 15701} {
		if it := trackerItem(t, tr, "issue", n); it.ReviewNoteOf != 0 {
			t.Errorf("#%d review_note_of = %d, want 0", n, it.ReviewNoteOf)
		}
	}
	pr := trackerItem(t, tr, "pr", 15582)
	got := make([]int, 0, len(pr.ReviewNotes))
	for _, n := range pr.ReviewNotes {
		got = append(got, n.Number)
	}
	if len(got) != 3 || got[0] != 15627 || got[1] != 15628 || got[2] != 15629 {
		t.Fatalf("PR 15582 review_notes = %v, want [15627 15628 15629]", got)
	}
	if pr.ReviewNotes[2].URL != "https://github.com/hivecommons/hive/issues/15629" || !strings.HasPrefix(pr.ReviewNotes[2].Title, "[reviewer] Review backlog") {
		t.Fatalf("review note carries url and title: %+v", pr.ReviewNotes[2])
	}
}

// A note whose source PR already merged or closed is about to be lifted or
// re-triaged by the blocked sweep: it stays a first-class row.
func TestStageTrackerReviewNoteOfResolvedPRStaysARow(t *testing.T) {
	items := []StageTrackerItem{
		{Repo: "o/r", Kind: "issue", Number: 2, Title: "Review backlog (style): x", Modifiers: []string{modBlocked}, BlockedOnPR: &StageTrackerBlockedOnPR{Repo: "o/r", Number: 1, State: blockedOnPRStateMerged}},
		{Repo: "o/r", Kind: "issue", Number: 3, Title: "Review backlog (style): y", Modifiers: []string{modBlocked}, BlockedOnPR: &StageTrackerBlockedOnPR{Repo: "o/r", Number: 1, State: blockedOnPRStateClosed}},
		{Repo: "o/r", Kind: "issue", Number: 4, Title: "Review backlog (style): z", Modifiers: []string{modBlocked}, BlockedOnPR: &StageTrackerBlockedOnPR{Repo: "o/r", Number: 1}},
		// Label lifted, marker still in the body: not a blocked note.
		{Repo: "o/r", Kind: "issue", Number: 5, Title: "Review backlog (style): w", BlockedOnPR: &StageTrackerBlockedOnPR{Repo: "o/r", Number: 1}},
	}
	stageTrackerFoldReviewNotes(items)
	for i, want := range []int{0, 0, 1, 0} {
		if items[i].ReviewNoteOf != want {
			t.Errorf("#%d review_note_of = %d, want %d", items[i].Number, items[i].ReviewNoteOf, want)
		}
	}
}

// SPA: folded notes are not rows (so the pill, which counts rows, counts a
// group once), the PR row carries one muted line naming them, a group whose
// PR is not drawn collapses into its lowest-numbered note, and the count
// line's breakdown still adds up to the open totals.
func TestStageTrackerReviewNotesFoldJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+jsFunc(t, html, "stageTrackerRowCount")+"\n"+stageTrackerRowsFixtureJS+`
const since = new Date(now - 3600000).toISOString();
const ctx2 = Object.assign({}, ctx);
const pr = (n, notes) => ({ repo: 'hivecommons/hive', number: n, kind: 'pr', title: 'PR ' + n, url: 'https://x/' + n, stage: 50, stage_since: since, author: 'pat', author_kind: 'human', modifiers: [], review_notes: notes });
const note = (n, of) => ({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'Review backlog (style): note ' + n, url: 'https://x/i' + n, stage: 0, stage_since: since, modifiers: ['blocked'], blocked_on_pr: { repo: 'hivecommons/hive', number: of, state: 'open' }, review_note_of: of });
const plain = (n) => ({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'Issue ' + n, url: 'https://x/i' + n, stage: 10, stage_since: since, modifiers: [] });
const n101 = note(101, 100), n102 = note(102, 100), n103 = note(103, 100), n104 = note(104, 200), n105 = note(105, 200);
const items = [pr(100, [n101, n102, n103].map(x => ({ number: x.number, title: x.title, url: x.url }))), n103, n101, n102, n105, n104, plain(106)];
ctx2.items = items;
const keys = rows => rows.map(r => r.kind + '#' + r.number);
assert.deepStrictEqual(keys(stageTrackerVisible(items, { kind: 'all', repo: '' })), ['pr#100', 'issue#104', 'issue#106'], 'three notes fold under their PR row; the two whose PR is not drawn collapse into #104');
assert.strictEqual(stageTrackerRowCount(items, ctx2), 3, 'the pill counts a group once');
assert.deepStrictEqual(keys(stageTrackerVisible(items, { kind: 'issue', repo: '' })), ['issue#101', 'issue#104', 'issue#106'], 'Issues view: no PR rows, so each group shows its lowest-numbered note');
assert.deepStrictEqual(keys(stageTrackerVisible(items, { kind: 'pr', repo: '' })), ['pr#100'], 'Pull requests view unchanged');

const prRow = stageTrackerRowHtml(items[0], stages, ctx2);
assert.ok(prRow.includes('<div class="ham-review-notes" data-ham-review-notes="3">3 review notes blocked on this PR · '), 'one muted line on the PR row: ' + prRow);
for (const n of [101, 102, 103]) assert.ok(prRow.includes('href="https://x/i' + n + '"'), 'note #' + n + ' stays reachable');
const leadRow = stageTrackerRowHtml(n104, stages, ctx2);
assert.ok(leadRow.includes('data-ham-review-notes="1">+1 more review note blocked on PR #200 · '), 'the lead note names the rest of its group: ' + leadRow);
assert.ok(leadRow.includes('href="https://x/i105"'));
assert.ok(!stageTrackerRowHtml(plain(106), stages, ctx2).includes('ham-review-notes'), 'a plain issue has no notes line');
assert.ok(!stageTrackerRowHtml(pr(300, []), stages, ctx2).includes('ham-review-notes'), 'a PR without notes has no notes line');

const folded = stageTrackerFoldedCount(items, ctx2);
assert.strictEqual(folded, 4, 'four of six issues are folded');
const counts = stageTrackerCountsText({ total: 3, from: 1, to: 3 }, 3, 0, { issues: 6, prs: 1 }, 0, folded);
assert.strictEqual(counts.badge, 3);
assert.ok(counts.badgeTitle.includes('3 rows (6 issues + 1 PR on its own row)'), counts.badgeTitle);
assert.ok(counts.badgeTitle.includes('4 review notes folded under their PR'), counts.badgeTitle);
assert.ok(!counts.badgeTitle.includes("issue's row"), 'no PR shares an issue row here: ' + counts.badgeTitle);
assert.deepStrictEqual(stageTrackerRowBreakdown(7, { issues: 6, prs: 1 }), { known: true, rows: 7, issues: 6, prs: 1, standalonePRs: 1, shared: 0, folded: 0 }, 'without folding the breakdown is as before');
`)
}
