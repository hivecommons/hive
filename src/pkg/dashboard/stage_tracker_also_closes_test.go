package dashboard

import (
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerLinkedPRAlsoCloses reproduces hivecommons/hive#15806: PR
// #15759 closes the open, listed #15502 and #15503 (and a closed #15500).
// Its strip is drawn once, under #15502; that listing names #15503 back in
// also_closes (number, title, url) so the sub-row can say "also closes
// #15503", while the #15503 listing — the "↳ … under #15502" chip — carries
// none. A PR closing a single listed issue gets no also_closes at all, and
// a soft reference (`Refs #15504`, listed) is never an "also closes".
func TestStageTrackerLinkedPRAlsoCloses(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	clock = now.Add(-2 * time.Hour)
	store.Observe(stagelabels.Key("acme/hive", 15759), stagelabels.ReviewPending, "ci green")
	store.Observe(stagelabels.Key("acme/hive", 15502), stagelabels.CoveredByPR, "open PR #15759 covers it (PR at 050-review-pending)")
	store.Observe(stagelabels.Key("acme/hive", 15503), stagelabels.CoveredByPR, "open PR #15759 covers it (PR at 050-review-pending)")
	store.Observe(stagelabels.Key("acme/hive", 15760), stagelabels.ReviewPending, "ci green")
	store.Observe(stagelabels.Key("acme/hive", 15510), stagelabels.CoveredByPR, "open PR #15760 covers it (PR at 050-review-pending)")
	clock = now

	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "acme/hive",
		ActionableIssues: []any{
			github.Issue{Number: 15502, Title: "Review backlog (correctness): nil deref", URL: "https://github.com/acme/hive/issues/15502", Author: "hive-app[bot]", Labels: []string{"stage/040-covered-by-pr"},
				LinkedPRs: []github.IssueLinkedPR{{Number: 15759, State: "open"}}},
			github.Issue{Number: 15503, Title: "Review backlog (correctness): off-by-one", URL: "https://github.com/acme/hive/issues/15503", Author: "hive-app[bot]", Labels: []string{"stage/040-covered-by-pr"},
				LinkedPRs: []github.IssueLinkedPR{{Number: 15759, State: "open"}}},
			github.Issue{Number: 15504, Title: "Review backlog (correctness): referenced only", URL: "https://github.com/acme/hive/issues/15504", Author: "hive-app[bot]", Labels: []string{"stage/030-in-progress"}},
			github.Issue{Number: 15510, Title: "a lone issue", URL: "https://github.com/acme/hive/issues/15510", Author: "hive-app[bot]", Labels: []string{"stage/040-covered-by-pr"},
				LinkedPRs: []github.IssueLinkedPR{{Number: 15760, State: "open"}}},
		},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 15759, Title: "fix the review backlog", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/050-review-pending", "author/hive"},
				LinkedIssues:     []github.PRLinkedIssue{{Number: 15500, State: "closed"}, {Number: 15503}, {Number: 15502}},
				ReferencedIssues: []github.PRLinkedIssue{{Number: 15504, Repo: "acme/hive", Via: github.PRLinkViaRefs}}}},
			FrontendPR{PullRequest: github.PullRequest{Number: 15760, Title: "fix the lone issue", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/050-review-pending", "author/hive"},
				LinkedIssues: []github.PRLinkedIssue{{Number: 15510}}}},
		},
	}}
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	resp := s.buildStageTracker(status, store, now)

	if pr := trackerItem(t, resp, "pr", 15759); pr.NestedUnder != 15502 {
		t.Fatalf("nested_under = %d, want 15502", pr.NestedUnder)
	}
	owner := trackerItem(t, resp, "issue", 15502)
	if len(owner.LinkedPRs) != 1 || owner.LinkedPRs[0].DrawnUnder != 15502 {
		t.Fatalf("issue 15502 linked_prs = %+v, want one listing of #15759 drawn here", owner.LinkedPRs)
	}
	got := owner.LinkedPRs[0].AlsoCloses
	if len(got) != 1 || got[0].Number != 15503 || got[0].Title != "Review backlog (correctness): off-by-one" || got[0].URL != "https://github.com/acme/hive/issues/15503" {
		t.Fatalf("issue 15502 linked PR also_closes = %+v, want only #15503 (not the soft-referenced #15504) with its title and url", got)
	}
	if got[0].Listed || got[0].State != "" {
		t.Fatalf("also_closes carries only number/title/url, got %+v", got[0])
	}
	other := trackerItem(t, resp, "issue", 15503)
	if len(other.LinkedPRs) != 1 || other.LinkedPRs[0].DrawnUnder != 15502 {
		t.Fatalf("issue 15503 linked_prs = %+v, want one listing of #15759 drawn under 15502", other.LinkedPRs)
	}
	if other.LinkedPRs[0].AlsoCloses != nil {
		t.Fatalf("the listing drawn elsewhere must not carry also_closes, got %+v", other.LinkedPRs[0].AlsoCloses)
	}
	// The soft `Refs #15504` is listed on the PR (so the UI can mark it) yet
	// is a reference, not a closing: it must not be named as "also closes".
	pr := trackerItem(t, resp, "pr", 15759)
	if k := slices.IndexFunc(pr.LinkedIssues, func(li StageTrackerLinkedIssue) bool { return li.Number == 15504 }); k < 0 || !pr.LinkedIssues[k].Listed || pr.LinkedIssues[k].Via != github.PRLinkViaRefs {
		t.Fatalf("PR 15759 linked_issues = %+v, want a listed refs link to #15504", pr.LinkedIssues)
	}
	referenced := trackerItem(t, resp, "issue", 15504)
	if len(referenced.LinkedPRs) != 1 || referenced.LinkedPRs[0].DrawnUnder != 15502 || referenced.LinkedPRs[0].AlsoCloses != nil {
		t.Fatalf("issue 15504 linked_prs = %+v, want one listing of #15759 drawn under 15502 with no also_closes", referenced.LinkedPRs)
	}
	lone := trackerItem(t, resp, "issue", 15510)
	if len(lone.LinkedPRs) != 1 || lone.LinkedPRs[0].AlsoCloses != nil {
		t.Fatalf("a PR closing one listed issue has no also_closes, got %+v", lone.LinkedPRs)
	}
}

// The SPA side: the PR drawn here wears a gray "also closes #15503" pill in
// the lineage slot, every number linking to its issue, truncated to three
// numbers plus "+N" and the hover naming every issue's title; the "↳ …
// under …" listing and a PR with nothing else to close get no pill. The
// pill rides both the single-covering-PR issue row and the sub-row.
func TestStageTrackerLinkedPRAlsoClosesJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+jsFunc(t, html, "stageTrackerLinkedSupersededText")+"\n"+stageTrackerActorIconJS(t, html)+stageTrackerRowsFixtureJS+`
const since = new Date(now - 3600000).toISOString();
const li = (n) => ({ number: n, title: 'Review backlog (correctness): item ' + n, url: 'https://x/issues/' + n });
const lp = (n, under, extra) => Object.assign({ number: n, title: 'fix', url: 'https://x/' + n, author: 'hive-bot', author_kind: 'hive', stage: 50, stage_since: since, drawn_under: under }, extra || {});
const issue = (n, prs) => ({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'Issue ' + n, url: 'https://x/issues/' + n, stage: 40, stage_since: since, caption: 'open PR #15759 covers it', linked_prs: prs });
const also = (html) => Array.from(html.matchAll(/<span class="ham-badge ham-also-closes" title="([^"]*)">([\s\S]*?)<\/span>/g)).map(m => ({ title: m[1], text: text(m[2]), links: Array.from(m[2].matchAll(/<a href="([^"]*)"[^>]*>#(\d+)<\/a>/g)).map(a => a[1]) }));

// The pure chip.
assert.strictEqual(stageTrackerAlsoClosesChipHtml(lp(15759, 15502)), '', 'no also_closes → no chip');
assert.strictEqual(stageTrackerAlsoClosesChipHtml(lp(15759, 15502, { also_closes: [] })), '');
let chip = stageTrackerAlsoClosesChipHtml(lp(15759, 15502, { also_closes: [li(15503)] }));
assert.deepStrictEqual(also(chip), [{ title: 'this PR also resolves #15503 (Review backlog (correctness): item 15503)', text: 'also closes #15503', links: ['https://x/issues/15503'] }], chip);
chip = stageTrackerAlsoClosesChipHtml(lp(15759, 15502, { also_closes: [li(15503), li(15504), li(15505), li(15506), li(15507)] }));
assert.strictEqual(also(chip)[0].text, 'also closes #15503, #15504, #15505 +2', 'three numbers then +N: ' + chip);
assert.deepStrictEqual(also(chip)[0].links, ['https://x/issues/15503', 'https://x/issues/15504', 'https://x/issues/15505']);
assert.ok(also(chip)[0].title.includes('#15507 (Review backlog (correctness): item 15507)'), 'the hover names every issue: ' + also(chip)[0].title);
chip = stageTrackerAlsoClosesChipHtml(lp(15759, 15502, { also_closes: [{ number: 15503, title: 'a "quoted" <title>' }] }));
assert.ok(chip.includes('title="this PR also resolves #15503 (a &quot;quoted&quot; &lt;title&gt;)"'), 'the hover is escaped: ' + chip);
assert.ok(chip.includes('href="#"'), 'no url → a harmless anchor: ' + chip);

// The owning issue with one covering PR: the pill sits in the PR column beside the PR link.
let row = stageTrackerRowHtml(issue(15502, [lp(15759, 15502, { also_closes: [li(15503)] })]), stages, ctx);
assert.strictEqual(prCol(row).length, 1, row);
const head = row.slice(0, row.indexOf('aria-label="Pipeline progress"'));
assert.deepStrictEqual(also(head).map(a => a.text), ['also closes #15503'], 'the pill rides the PR column: ' + head);
assert.ok(head.indexOf('ham-pr-ref') < head.indexOf('ham-also-closes'), 'PR link first, then the pill');

// The other issue lists the PR as "↳ … under …" and never wears the reciprocal pill.
row = stageTrackerRowHtml(issue(15503, [lp(15759, 15502)]), stages, ctx);
assert.strictEqual(prCol(row).length, 0);
assert.ok(row.includes('↳ #15759 · under #15502'), row);
assert.deepStrictEqual(also(row), []);

// Two covering PRs drawn here: each sub-row carries its own pill (or none).
row = stageTrackerRowHtml(issue(15502, [lp(15759, 15502, { also_closes: [li(15503)] }), lp(15761, 15502, { stage: 70 })]), stages, ctx);
assert.strictEqual(subs(row).length, 2, row);
const sub = (n) => subs(row).find(s => s.includes('data-ham-sub-key="hivecommons/hive/pr#' + n + '"'));
assert.deepStrictEqual(also(sub(15759)).map(a => a.text), ['also closes #15503'], sub(15759));
assert.deepStrictEqual(also(sub(15761)), [], 'a PR with nothing else to close wears no pill');

// With a lineage chip too, both pills show: lineage first, then also-closes.
const lineage = { kind: 'adopted', from_number: 15700, from_author: 'alice', from_author_kind: 'human', from_url: 'https://x/15700', at: since, reason: 'ci-red-contributor-offline' };
row = stageTrackerRowHtml(issue(15502, [lp(15759, 15502, { also_closes: [li(15503)], lineage })]), stages, ctx);
assert.ok(row.includes('ham-lineage') && row.includes('ham-also-closes'), row);
assert.ok(row.indexOf('ham-lineage') < row.indexOf('ham-also-closes'), 'lineage chip before the also-closes pill');

// A superseded adopted original (hive's copy #15762 listed open alongside it) draws as one muted line with neither pill; the copy keeps its own.
row = stageTrackerRowHtml(issue(15502, [lp(15759, 15502, { also_closes: [li(15503)], lineage, adopted_by: 15762 }), lp(15762, 15502, { also_closes: [li(15503)] })]), stages, ctx);
const allSubs = row.split('<div class="ham-sub').slice(1);
assert.strictEqual(allSubs.length, 2, row);
const anySub = (n) => allSubs.find(s => s.includes('data-ham-sub-key="hivecommons/hive/pr#' + n + '"'));
assert.ok(anySub(15759).startsWith(' ham-sub-superseded"') && anySub(15759).includes('ham-superseded-note'), 'the original reads as superseded: ' + anySub(15759));
assert.deepStrictEqual(also(anySub(15759)), [], 'a superseded original wears no also-closes pill: ' + anySub(15759));
assert.ok(!anySub(15759).includes('ham-lineage'), 'nor a lineage chip');
assert.deepStrictEqual(also(anySub(15762)).map(a => a.text), ['also closes #15503'], anySub(15762));
`)
}
