package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// openItemsFixture is a hosted-hive shaped snapshot: one big repo whose
// enumeration moved on without its WorkBreakdown (a webhook re-read closed
// two issues and one PR in the lists and TotalByRepo only), one repo served
// last-good and stale, one the enumeration refused but the forge cache
// lists, and one with a PR echoed by the issues feed.
func openItemsFixture() (*config.Config, *github.ActionableResult, fakeForgeOpen) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons", Repos: []string{"hive", "homebrew-hive", "pluk", "rationguard"}}}
	now := time.Date(2026, 10, 21, 17, 48, 0, 0, time.UTC)
	hiveDetails := github.RepoWorkDetails{}
	for n := 300; n < 309; n++ {
		hiveDetails.Issues = append(hiveDetails.Issues, github.RepoWorkIssue{Repo: "hive", Number: n, Bucket: "filtered", Reason: "needs-human"})
	}
	hiveDetails.Issues = append(hiveDetails.Issues,
		github.RepoWorkIssue{Repo: "hive", Number: 400, Bucket: "hive_advisory"},
		github.RepoWorkIssue{Repo: "hive", Number: 401, Bucket: "dependency_dashboard"},
		github.RepoWorkIssue{Repo: "hive", Number: 402, Bucket: "reporter_triage"},
	)
	hiveDetails.PRs = append(hiveDetails.PRs,
		github.RepoWorkPR{Repo: "hive", Number: 500, Bucket: "draft", Draft: true},
		github.RepoWorkPR{Repo: "hive", Number: 501, Bucket: "filtered"},
	)
	actionable := &github.ActionableResult{
		GeneratedAt: now,
		Issues: github.IssueResult{Items: []github.Issue{
			{Repo: "hive", Number: 1, Title: "a"}, {Repo: "hive", Number: 2, Title: "b"}, {Repo: "hive", Number: 3, Title: "c"},
			{Repo: "homebrew-hive", Number: 10, Title: "h"},
			// The issues feed echo of a PR: dropped in favour of the PR row.
			{Repo: "pluk", Number: 77, Title: "echo", URL: "https://github.com/hivecommons/pluk/pull/77"},
		}},
		PRs: github.PRResult{
			Items: []github.PullRequest{
				{Repo: "hive", Number: 20, Title: "p"}, {Repo: "hive", Number: 21, Title: "q"},
				{Repo: "pluk", Number: 77, Title: "pr"},
			},
			Held: []github.PullRequest{{Repo: "hive", Number: 22, Title: "held"}},
		},
		Hold: github.HoldResult{Items: []github.HoldItem{
			{Repo: "hive", Number: 4, Type: "issue"}, {Repo: "hive", Number: 5, Type: "issue"},
			{Repo: "hive", Number: 22, Type: "pr"},
		}},
		TotalByRepo: map[string]github.RepoCounts{
			// 3 actionable + 2 held + 12 sidelined = 17 listed; the
			// enumeration counted 17 after its webhook re-read.
			"hive":          {Issues: 17, PRs: 5},
			"homebrew-hive": {Issues: 1, PRs: 0},
			"pluk":          {Issues: 0, PRs: 1},
		},
		WorkBreakdownByRepo: map[string]github.RepoWorkBreakdown{
			// Pre-re-read breakdown: two issues and one PR more than the
			// lists now carry (webhook patches never touch the breakdown).
			"hive": {
				Issues: github.RepoIssueBreakdown{Actionable: 5, Hold: 2, HiveAdvisory: 1, DependencyDashboard: 1, Filtered: 9, NeedsHuman: 9, ReporterTriage: 1},
				PRs:    github.RepoPRBreakdown{Actionable: 3, Hold: 1, Draft: 1, Filtered: 1},
			},
			"homebrew-hive": {Issues: github.RepoIssueBreakdown{Actionable: 1}},
			"pluk":          {PRs: github.RepoPRBreakdown{Actionable: 1}},
		},
		WorkDetailsByRepo: map[string]github.RepoWorkDetails{"hive": hiveDetails},
		StaleRepos:        []string{"homebrew-hive"},
		StaleSince:        now.Add(-30 * time.Minute),
	}
	forge := fakeForgeOpen{"hivecommons/rationguard": {Issues: 1, PRs: 0, Ready: true}}
	return cfg, actionable, forge
}

// The Overview tile totals, the sidebar pill's inputs and the activity
// monitor's items are one count: the canonical open-items set
// (hivecommons/hive#13373). Stale repos count their last-good items and
// carry the stale mark; a webhook-patched repo counts what its lists hold,
// not its pre-patch breakdown; a cache-only repo's items are seeded so the
// tracker can list them.
func TestOpenItemsTileEqualsPillEqualsTracker(t *testing.T) {
	cfg, actionable, forge := openItemsFixture()
	repos := buildReposWithForge(cfg, actionable, governor.State{}, forge)
	status := &StatusPayload{Repos: repos, StaleSince: overviewStaleSince(repos)}

	srv := &Server{}
	now := time.Date(2026, 10, 21, 17, 50, 0, 0, time.UTC)
	banded := srv.statusWithOverviewBands(status, now)
	tracker := srv.buildStageTracker(status, nil, now)

	// hive: 3 actionable + 2 held + 12 sidelined issues; 2 + 1 held + 2
	// sidelined PRs. homebrew-hive 1/0 (stale). pluk 0/1 (echo dropped).
	// rationguard 1/0 seeded from the cache.
	const wantIssues, wantPRs = 17 + 1 + 0 + 1, 5 + 0 + 1 + 0
	tot := banded.OverviewTotals
	if tot.Issues.Forge != wantIssues || tot.PRs.Forge != wantPRs {
		t.Fatalf("tile totals = %d/%d, want %d/%d", tot.Issues.Forge, tot.PRs.Forge, wantIssues, wantPRs)
	}
	oi := banded.OpenItems
	if oi.Issues != wantIssues || oi.PRs != wantPRs || oi.Total != wantIssues+wantPRs {
		t.Fatalf("openItems = %+v, want %d/%d", oi, wantIssues, wantPRs)
	}
	if tracker.OpenIssues != wantIssues || tracker.OpenPRs != wantPRs || len(tracker.Items) != wantIssues+wantPRs {
		t.Fatalf("tracker = %d issues / %d prs / %d items, want %d/%d/%d", tracker.OpenIssues, tracker.OpenPRs, len(tracker.Items), wantIssues, wantPRs, wantIssues+wantPRs)
	}
	gotIssues, gotPRs := stageTrackerOpenCounts(tracker.Items)
	if gotIssues != tracker.OpenIssues || gotPRs != tracker.OpenPRs {
		t.Fatalf("tracker header counts %d/%d disagree with its items %d/%d", tracker.OpenIssues, tracker.OpenPRs, gotIssues, gotPRs)
	}

	// Per repo, the card's numbers, the summary row and the tracker's rows
	// agree, and the partition sums to the card.
	trackerByRepo := map[string]openItemCounts{}
	for _, it := range tracker.Items {
		c := trackerByRepo[it.Repo]
		if it.Kind == "pr" {
			c.PRs++
		} else {
			c.Issues++
		}
		trackerByRepo[it.Repo] = c
	}
	byName := map[string]FrontendRepo{}
	for _, r := range banded.Repos {
		byName[overviewRepoName(r)] = r
	}
	for _, row := range oi.Repos {
		r := byName[row.Repo]
		if r.Issues != row.Issues || r.PRs != row.PRs {
			t.Fatalf("%s: card %d/%d vs openItems %d/%d", row.Repo, r.Issues, r.PRs, row.Issues, row.PRs)
		}
		if tr := trackerByRepo[row.Repo]; tr.Issues != row.Issues || tr.PRs != row.PRs {
			t.Fatalf("%s: tracker %d/%d vs openItems %d/%d", row.Repo, tr.Issues, tr.PRs, row.Issues, row.PRs)
		}
		if r.WorkBreakdown != nil && (r.WorkBreakdown.Issues.Total() != r.Issues || r.WorkBreakdown.PRs.Total() != r.PRs) {
			t.Fatalf("%s: breakdown %d/%d does not sum to the card %d/%d", row.Repo, r.WorkBreakdown.Issues.Total(), r.WorkBreakdown.PRs.Total(), r.Issues, r.PRs)
		}
	}

	// The webhook-patched repo was rebuilt from its lists; the named
	// sub-buckets were clamped, not invented.
	hive := byName["hivecommons/hive"]
	if wb := hive.WorkBreakdown.Issues; wb.Actionable != 3 || wb.Hold != 2 || wb.Filtered != 9 || wb.NeedsHuman != 9 || wb.HiveAdvisory != 1 || wb.DependencyDashboard != 1 || wb.ReporterTriage != 1 || wb.Other != 0 {
		t.Fatalf("hive issue breakdown = %+v", wb)
	}
	if wb := hive.WorkBreakdown.PRs; wb.Actionable != 2 || wb.Hold != 1 || wb.Draft != 1 || wb.Filtered != 1 {
		t.Fatalf("hive PR breakdown = %+v", wb)
	}
	if hive.UnlistedIssues != 0 || hive.UnlistedPRs != 0 {
		t.Fatalf("hive unlisted = %d/%d, want none", hive.UnlistedIssues, hive.UnlistedPRs)
	}

	// Stale rides along: the repo keeps its last-good items and says so.
	var stale *FrontendRepoOpenItems
	for i := range oi.Repos {
		if oi.Repos[i].Repo == "hivecommons/homebrew-hive" {
			stale = &oi.Repos[i]
		}
	}
	if stale == nil || !stale.Stale || stale.StaleSince != "2026-10-21T17:18:00Z" || stale.Issues != 1 {
		t.Fatalf("stale repo row = %+v", stale)
	}
	if oi.StaleSince != "2026-10-21T17:18:00Z" || banded.StaleSince != oi.StaleSince {
		t.Fatalf("openItems.staleSince = %q, status.staleSince = %q", oi.StaleSince, banded.StaleSince)
	}

	// The partition the tile prints is the same set split four ways.
	b := oi.Buckets
	if sum := b.Actionable.Issues + b.Held.Issues + b.Blocked.Issues + b.Outside.Issues; sum != oi.Issues {
		t.Fatalf("issue buckets %+v sum to %d, want %d", b, sum, oi.Issues)
	}
	if sum := b.Actionable.PRs + b.Held.PRs + b.Blocked.PRs + b.Outside.PRs; sum != oi.PRs {
		t.Fatalf("PR buckets %+v sum to %d, want %d", b, sum, oi.PRs)
	}

	// The cache-only repo's items are rows, not an Other bucket with
	// nothing behind it.
	if rg := byName["hivecommons/rationguard"]; rg.Issues != 1 || len(rg.NonActionableIssues) != 1 || rg.CountsIncomplete {
		t.Fatalf("rationguard = %+v", rg)
	}
	// The payload carries the set under a stable key.
	body, err := json.Marshal(banded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"openItems":{"issues":19,"prs":6,"total":25,`) {
		t.Fatalf("status JSON missing openItems summary: %s", body[:min(len(body), 400)])
	}
}

// A repo that reports more open items than it can list keeps the listed
// count on the card and names the remainder as unlisted, so the tile, the
// pill and the tracker still agree.
func TestOpenItemsUnlistedStaysBesideTheTotal(t *testing.T) {
	r := FrontendRepo{Name: "x", Issues: 6, PRs: 2,
		ActionableIssues: []any{github.Issue{Repo: "x", Number: 1}, github.Issue{Repo: "x", Number: 2}},
		OpenPrs:          []any{FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 9}}},
		WorkBreakdown:    &github.RepoWorkBreakdown{Issues: github.RepoIssueBreakdown{Actionable: 2, Other: 4}, PRs: github.RepoPRBreakdown{Actionable: 1, Other: 1}},
	}
	c := reconcileRepoOpenItems(&r)
	if c.Issues != 2 || c.PRs != 1 || r.Issues != 2 || r.PRs != 1 || r.UnlistedIssues != 4 || r.UnlistedPRs != 1 {
		t.Fatalf("reconciled = %+v counts %+v", r, c)
	}
	if r.WorkBreakdown.Issues.Total() != 2 || r.WorkBreakdown.Issues.Other != 0 || r.WorkBreakdown.PRs.Total() != 1 {
		t.Fatalf("breakdown = %+v, want rebuilt to the listed items", r.WorkBreakdown)
	}
	status := &StatusPayload{Repos: []FrontendRepo{r}}
	if tot := overviewTotals(status); tot.Issues.Forge != 2 || tot.PRs.Forge != 1 {
		t.Fatalf("totals = %+v", tot)
	}
	srv := &Server{}
	tr := srv.buildStageTracker(status, nil, time.Now())
	if tr.OpenIssues != 2 || tr.OpenPRs != 1 {
		t.Fatalf("tracker = %d/%d", tr.OpenIssues, tr.OpenPRs)
	}
}

// A not-ready cache snapshot seeds nothing; a ready one adds only what the
// lists lack and never a closed item or a PR echoed as an issue.
func TestSeedOpenItemsFromForge(t *testing.T) {
	r := FrontendRepo{Name: "x", ActionableIssues: []any{github.Issue{Repo: "o/x", Number: 1}}, OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "o/x", Number: 7}}}}
	snap := forgestate.Snapshot{Ready: false, Issues: map[int]forgestate.Item{2: {Number: 2}}}
	if n := seedOpenItemsFromForge(&r, "o/x", snap); n != 0 || len(r.NonActionableIssues) != 0 {
		t.Fatalf("not-ready snapshot seeded %d", n)
	}
	snap = forgestate.Snapshot{Ready: true,
		Issues: map[int]forgestate.Item{1: {Number: 1}, 2: {Number: 2, Title: "two"}, 3: {Number: 3, Closed: true}, 7: {Number: 7}},
		PRs:    map[int]forgestate.Item{7: {Number: 7}, 8: {Number: 8, Draft: true}},
	}
	if n := seedOpenItemsFromForge(&r, "o/x", snap); n != 2 {
		t.Fatalf("seeded %d, want issue 2 and PR 8", n)
	}
	w, _ := repoWorkIssue(r.NonActionableIssues[0])
	if w.Number != 2 || w.Title != "two" || w.Bucket != "other" || w.Reason != forgeUnclassifiedReason || w.Repo != "o/x" {
		t.Fatalf("seeded issue = %+v", w)
	}
	p, _ := repoWorkPR(r.NonActionablePrs[0])
	if p.Number != 8 || !p.Draft || p.Bucket != "draft" {
		t.Fatalf("seeded PR = %+v", p)
	}
	c := countOpenItems(repoOpenItems(r))
	if c.Issues != 2 || c.PRs != 2 {
		t.Fatalf("open items after seeding = %+v", c)
	}
}

// The SPA side of the same contract: the Overview sidebar pill prints the
// tile's totals and the activity monitor's header prints the tracker's
// open_issues/open_prs — fed from one status snapshot they are the same
// numbers, stale or not.
func TestOpenItemsPillAndTrackerHeaderDerivation(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	cfg, actionable, forge := openItemsFixture()
	repos := buildReposWithForge(cfg, actionable, governor.State{}, forge)
	status := &StatusPayload{Repos: repos, StaleSince: overviewStaleSince(repos)}
	srv := &Server{}
	now := time.Date(2026, 10, 21, 17, 50, 0, 0, time.UTC)
	banded := srv.statusWithOverviewBands(status, now)
	tracker := srv.buildStageTracker(status, nil, now)
	statusJSON, err := json.Marshal(banded)
	if err != nil {
		t.Fatal(err)
	}
	trackerJSON, err := json.Marshal(tracker)
	if err != nil {
		t.Fatal(err)
	}
	script := overviewKPIRenderPrelude(t, html) + `
` + jsFunc(t, html, "stageTrackerCountsText") + `
` + jsFunc(t, html, "stageTrackerRowBreakdown") + `
` + jsFunc(t, html, "stageTrackerOpenCounts") + `
const status = ` + string(statusJSON) + `;
const tracker = ` + string(trackerJSON) + `;
window._lastStatus = status;
_overviewKPILocalScope = 'all';
_overviewLastRepos = status.repos;
const issueSlices = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done'].map(key => ({ key, count: 0, items: [] }));
const prSlices = ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft'].map(key => ({ key, count: 0, items: [] }));
renderOverviewKPIs(status.repos, issueSlices, prSlices, { showKPIs: true });
const pillEl = document.getElementById('overview-sidebar-badge');
const tile = status.overviewTotals;
assert.strictEqual(pillEl.textContent, tile.issues.forge + '/' + tile.prs.forge, 'pill mirrors the tile');
assert.strictEqual(pillEl.hidden, false);
assert.ok(pillEl.title.includes('stale since'), 'pill title names the last-good state: ' + pillEl.title);
assert.strictEqual(tile.issues.forge, status.openItems.issues);
assert.strictEqual(tile.prs.forge, status.openItems.prs);
const open = stageTrackerOpenCounts(tracker);
assert.deepStrictEqual(open, { issues: tile.issues.forge, prs: tile.prs.forge }, 'tracker counts the same set');
const page = { total: tracker.items.length, from: 1, to: tracker.items.length };
const counts = stageTrackerCountsText(page, tracker.items.length, 2, open, 3);
// Owner: the tracker counts ROWS. With every item on its own row the pill,
// the header lead and the tile agree on the same number.
const issuesN = tile.issues.forge, prsN = tile.prs.forge;
const breakdown = issuesN + ' issues + ' + prsN + ' PRs on their own row';
assert.strictEqual(counts.header, tracker.items.length + ' rows (' + breakdown + ') · 2 need attention');
assert.strictEqual(counts.badge, tracker.items.length, 'activity-monitor pill is the row count of the default view');
assert.strictEqual(counts.badge, tile.issues.forge + tile.prs.forge, 'no PR shares an issue row here, so rows = the tile total');
assert.strictEqual(counts.badgeTitle, tracker.items.length + ' rows (' + breakdown + ')' + ' · 3 need attention', 'attention moves to the pill tooltip; the row make-up is named');
assert.strictEqual(counts.note, '', 'nothing to footnote when rows = items');
// A PR drawn on its issue's row shrinks the row count; the pill follows the
// rows and the gap to the item total is a footnote, never a second total.
const rows = tracker.items.length - 1;
const nested = stageTrackerCountsText({ total: rows, from: 1, to: rows }, rows, 0, open);
assert.strictEqual(nested.badge, rows, 'pill = rows, not items');
assert.strictEqual(nested.showing, 'showing 1–' + rows + ' of ' + rows, 'no filter → no "filtered out", no items total');
assert.strictEqual(nested.note, "1 PR shown on its issue's row");
assert.strictEqual(nested.breakdown, issuesN + ' issues + ' + (prsN - 1) + ' PRs on their own row', 'rows − issues = PRs on their own row');
assert.ok(nested.badgeTitle.endsWith(" · 1 PR shown on its issue's row"), nested.badgeTitle);
const filtered = stageTrackerCountsText({ total: 4, from: 1, to: 4 }, tracker.items.length, 0, open).header;
assert.strictEqual(filtered, '4 of ' + tracker.items.length + ' rows (' + breakdown + ') · nothing needs attention');
assert.deepStrictEqual(stageTrackerOpenCounts({ items: [{ kind: 'issue' }, { kind: 'pr' }, { kind: 'issue' }] }), { issues: 2, prs: 1 }, 'falls back to counting items');
console.log('ok');
`
	dir := t.TempDir()
	path := filepath.Join(dir, "open_items.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected output: %s", out)
	}
}

// A repo whose enumeration listed open PRs but no issues must not render a
// confident 0 while its issue side is stale or unlisted (hosted hive,
// 2026-10-22: every repo "0 issues · N PRs" after an update). The repo is
// flagged issuesSyncing and stale, and the flag propagates to the openItems
// summary. A confirmed empty issue list (nothing stale, nothing unlisted)
// and a repo with no PRs are left alone.
func TestOpenItemsZeroIssuesBesideOpenPRsIsSyncingNotZero(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons", Repos: []string{"hive", "empty", "quiet"}}}
	now := time.Date(2026, 10, 22, 16, 23, 0, 0, time.UTC)
	actionable := &github.ActionableResult{
		GeneratedAt: now,
		PRs: github.PRResult{Items: []github.PullRequest{
			{Repo: "hive", Number: 20, Title: "p"}, {Repo: "hive", Number: 21, Title: "q"},
			{Repo: "empty", Number: 30, Title: "e"},
		}},
		TotalByRepo: map[string]github.RepoCounts{
			// The enumeration verified the empty issue list against the
			// repo record and reported the remainder as unlisted.
			"hive":  {Issues: 25, PRs: 2},
			"empty": {Issues: 0, PRs: 1},
			"quiet": {Issues: 0, PRs: 0},
		},
		StaleRepos: []string{"hive"},
		StaleSince: now,
	}
	repos := buildReposWithForge(cfg, actionable, governor.State{}, nil)
	byName := map[string]FrontendRepo{}
	for _, r := range repos {
		byName[r.Name] = r
	}
	hive := byName["hive"]
	if hive.Issues != 0 || hive.PRs != 2 || hive.UnlistedIssues != 25 {
		t.Fatalf("hive = issues %d prs %d unlisted %d, want 0/2/25", hive.Issues, hive.PRs, hive.UnlistedIssues)
	}
	if !hive.IssuesSyncing || !hive.CountsStale || hive.CountsStaleSince == "" {
		t.Fatalf("hive syncing=%v stale=%v since=%q, want the issue side flagged as pending", hive.IssuesSyncing, hive.CountsStale, hive.CountsStaleSince)
	}
	if e := byName["empty"]; e.IssuesSyncing || e.CountsStale {
		t.Fatalf("a confirmed empty issue list must not be flagged: %+v", e)
	}
	if q := byName["quiet"]; q.IssuesSyncing {
		t.Fatalf("a repo with no PRs must not be flagged: %+v", q)
	}

	status := &StatusPayload{Repos: repos}
	oi := openItemsSummary(status)
	if !oi.IssuesSyncing || oi.Unlisted.Issues != 25 || oi.StaleSince == "" {
		t.Fatalf("openItems = %+v, want issuesSyncing with 25 unlisted and a staleSince", oi)
	}
	var flagged int
	for _, row := range oi.Repos {
		if row.IssuesSyncing {
			flagged++
			if row.Repo != "hivecommons/hive" || !row.Stale {
				t.Fatalf("flagged row = %+v, want stale hivecommons/hive", row)
			}
		}
	}
	if flagged != 1 {
		t.Fatalf("flagged %d repo rows, want 1", flagged)
	}

	// Stale without unlisted still counts as syncing: a last-good
	// enumeration that itself had no issues beside PRs.
	r := FrontendRepo{Name: "x", Issues: 0, PRs: 1, CountsStale: true,
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 9}}}}
	reconcileRepoOpenItems(&r)
	markIssuesSyncing(&r, "2026-10-22T16:23:00Z")
	if !r.IssuesSyncing || r.CountsStaleSince != "2026-10-22T16:23:00Z" {
		t.Fatalf("stale repo = %+v, want syncing with the supplied since", r)
	}
}
