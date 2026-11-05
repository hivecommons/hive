package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#13257 took bot Dependency Dashboards and the Hive Advisory
// Report off the activity monitor (#13261); the v5 → v6 top-up that brought
// in the single open-items set (#13434) dropped that filter, and
// projectbluefin/bluefin-report#52 (mergeraptor[bot]'s Dependency Dashboard)
// came back as an "Open · since 4d" row. The payload keeps the items, so
// open_issues still equals the Overview tile; the SPA does not draw them.
func TestStageTrackerPayloadKeepsStandingMetaIssues(t *testing.T) {
	now := time.Date(2026, 11, 5, 16, 0, 0, 0, time.UTC)
	s := newTestServer()
	status := minimalPayload()
	status.Repos = []FrontendRepo{{
		Name: "bluefin-report", Full: "projectbluefin/bluefin-report",
		ActionableIssues: []any{
			github.Issue{Number: 15, Title: "GNOME GUI specification audit", Author: "pat", AuthorIsHuman: true},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "projectbluefin/bluefin-report", Number: 52, Title: "Dependency Dashboard", Author: "mergeraptor[bot]", Labels: []string{"triage/accepted", "needs-kind"}, Bucket: repoWorkBucketDependencyDashboard, Reason: "Dependency dashboard control panel"},
			github.RepoWorkIssue{Repo: "projectbluefin/bluefin-report", Number: 53, Title: "Hive Advisory Report", Bucket: repoWorkBucketHiveAdvisory, Reason: "Hive advisory report"},
		},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 60, Title: "Update dependency foo", Author: "renovate[bot]", AppAuthored: true}},
		},
	}}
	s.UpdateStatus(status)
	tr := s.buildStageTracker(s.status, nil, now)

	if it := trackerItem(t, tr, "issue", 52); it.Bucket != repoWorkBucketDependencyDashboard {
		t.Errorf("#52 bucket = %q, want %q (the SPA keys on it)", it.Bucket, repoWorkBucketDependencyDashboard)
	}
	if it := trackerItem(t, tr, "issue", 53); it.Bucket != repoWorkBucketHiveAdvisory {
		t.Errorf("#53 bucket = %q, want %q", it.Bucket, repoWorkBucketHiveAdvisory)
	}
	if tr.OpenIssues != 3 || tr.OpenPRs != 1 {
		t.Fatalf("open = %d issues / %d PRs, want 3 / 1 (the Overview tile's total)", tr.OpenIssues, tr.OpenPRs)
	}
}

// SPA: a standing meta issue is never a row, in any view; it does not count
// toward the pill, needs attention or the label chips; and the count line
// names it, so the breakdown still adds up to the open totals.
func TestStageTrackerHidesStandingMetaIssuesJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+jsFunc(t, html, "stageTrackerRowCount")+"\n"+stageTrackerRowsFixtureJS+`
const since = new Date(now - 4 * 86400000).toISOString();
const ctx2 = Object.assign({}, ctx);
const repo = 'projectbluefin/bluefin-report';
const issue = (n, bucket, labels) => ({ repo, number: n, kind: 'issue', title: 'Issue ' + n, url: 'https://x/i' + n, stage: 0, stage_since: since, modifiers: [], labels, bucket });
const plain = issue(15, '', ['kind/audit']);
const deps = Object.assign(issue(52, 'dependency_dashboard', ['triage/accepted', 'needs-kind']), { title: 'Dependency Dashboard', modifiers: ['needs-triage'] });
const advisory = Object.assign(issue(53, 'hive_advisory', ['hive/advisory']), { title: 'Hive Advisory Report' });
const filtered = issue(54, 'filtered', ['needs-human']);
const pr = { repo, number: 60, kind: 'pr', title: 'Update dependency foo', url: 'https://x/60', stage: 50, stage_since: since, author: 'renovate[bot]', author_kind: 'clanker', modifiers: [], labels: [] };
const items = [plain, deps, advisory, filtered, pr];
ctx2.items = items;
const keys = rows => rows.map(r => r.kind + '#' + r.number);

assert.strictEqual(stageTrackerIsStandingMeta(deps), true);
assert.strictEqual(stageTrackerIsStandingMeta(advisory), true);
assert.strictEqual(stageTrackerIsStandingMeta(filtered), false, 'another sidelined bucket is still a row');
assert.strictEqual(stageTrackerIsStandingMeta(Object.assign({}, pr, { bucket: 'dependency_dashboard' })), false, 'only issues');
assert.deepStrictEqual(keys(stageTrackerVisible(items, { kind: 'all', repo: '' })), ['issue#15', 'issue#54', 'pr#60'], 'All: no dashboard or advisory row');
assert.deepStrictEqual(keys(stageTrackerVisible(items, { kind: 'issue', repo: '' })), ['issue#15', 'issue#54'], 'Issues: same');
assert.deepStrictEqual(keys(stageTrackerVisible(items, { kind: 'all', repo: '', q: '52' })), [], 'a search does not bring it back');
assert.strictEqual(stageTrackerRowCount(items, ctx2), 3, 'the pill counts rows');
assert.strictEqual(stageTrackerStandingCount(items), 2);

const fac = stageTrackerLabelFacets(items, {});
for (const l of ['triage/accepted', 'needs-kind', 'hive/advisory']) assert.ok(!fac.all.some(x => x.label === l), 'no chip for a hidden row\'s label: ' + l);
assert.ok(fac.all.some(x => x.label === 'kind/audit'));

const counts = stageTrackerCountsText({ total: 3, from: 1, to: 3 }, 3, 0, { issues: 4, prs: 1 }, 0, 0, 2);
assert.ok(counts.header.startsWith('3 rows (4 issues + 1 PR on its own row)'), counts.header);
assert.ok(counts.header.includes('2 bot dashboards or advisory reports not listed'), counts.header);
assert.ok(!counts.header.includes("issue's row"), 'the hidden issues are not mistaken for PRs sharing a row: ' + counts.header);
assert.deepStrictEqual(stageTrackerRowBreakdown(3, { issues: 4, prs: 1 }, 0, 2), { known: true, rows: 3, issues: 4, prs: 1, standalonePRs: 1, shared: 0, folded: 0, standing: 2 });
assert.ok(stageTrackerCountsText({ total: 4, from: 1, to: 4 }, 4, 0, { issues: 4, prs: 1 }, 0, 0, 1).header.includes('1 bot dashboard or advisory report not listed'), 'singular');
`)
}
