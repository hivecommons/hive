package dashboard

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// After a restart the governor serves the snapshot it restored from disk,
// marked stale, until its first enumeration completes. The status must say
// that is a restart window (restartRefresh with this process's clock and
// the scan tally), not merely "stale since <previous process's time>", and
// the marker must clear the moment a fresh enumeration publishes.
func TestRestartRefreshInfo_StaleToFreshAcrossRestart(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1", "repo2", "repo3"}},
	}
	previousRead := time.Date(2026, 10, 28, 21, 15, 0, 0, time.UTC)
	restartedAt := time.Date(2026, 10, 28, 22, 56, 33, 0, time.UTC)

	// 1. Boot: the restored snapshot is a previous process's read.
	restored := &github.ActionableResult{
		GeneratedAt: previousRead,
		TotalByRepo: map[string]github.RepoCounts{
			"repo1": {Issues: 16, PRs: 20},
			"repo2": {Issues: 1},
			"repo3": {PRs: 2},
		},
	}
	restored.MarkAllStale()
	restored.RestoredAt = restartedAt
	repos := buildRepos(cfg, restored, governor.State{})
	rr := restartRefreshInfo(restored, repos)
	if rr == nil {
		t.Fatal("restored snapshot must report the restart window")
	}
	if rr.RestoredAt != "2026-10-28T22:56:33Z" {
		t.Fatalf("RestoredAt = %q, want this process's restore time", rr.RestoredAt)
	}
	if rr.SnapshotAt != "2026-10-28T21:15:00Z" {
		t.Fatalf("SnapshotAt = %q, want the previous read time", rr.SnapshotAt)
	}
	if rr.ReposScanned != 0 || rr.ReposTotal != 3 {
		t.Fatalf("scan tally = %d/%d, want 0/3 before the first enumeration", rr.ReposScanned, rr.ReposTotal)
	}
	if got := staleSinceRFC3339(restored); got != "2026-10-28T21:15:00Z" {
		t.Fatalf("staleSinceRFC3339 = %q, want the previous read time kept as the raw marker", got)
	}

	// 2. A whole-pass failure on the restored snapshot keeps the window:
	// still the restored copy, still no fresh repo.
	kept := restored.Clone()
	kept.MarkAllStale()
	if rr := restartRefreshInfo(kept, buildRepos(cfg, kept, governor.State{})); rr == nil || rr.RestoredAt != "2026-10-28T22:56:33Z" {
		t.Fatalf("a re-marked restored snapshot must keep the restart window, got %+v", rr)
	}

	// 3. The first enumeration publishes a fresh result (no RestoredAt):
	// the window closes with the stale marker.
	fresh := &github.ActionableResult{
		GeneratedAt: restartedAt.Add(10 * time.Minute),
		TotalByRepo: map[string]github.RepoCounts{
			"repo1": {Issues: 23, PRs: 19},
			"repo2": {Issues: 1},
			"repo3": {PRs: 2},
		},
	}
	freshRepos := buildRepos(cfg, fresh, governor.State{})
	if rr := restartRefreshInfo(fresh, freshRepos); rr != nil {
		t.Fatalf("fresh enumeration must clear the restart window, got %+v", rr)
	}
	if staleSinceRFC3339(fresh) != "" || overviewStaleSince(freshRepos) != "" {
		t.Fatal("fresh enumeration must carry no stale marker")
	}

	// 4. A later refusal on the fresh process is an ordinary stale window,
	// not a restart one: RestoredAt is gone with the restored snapshot.
	refused := fresh.Clone()
	refused.MarkAllStale()
	if rr := restartRefreshInfo(refused, buildRepos(cfg, refused, governor.State{})); rr != nil {
		t.Fatalf("post-restart refusal must not claim a restart window, got %+v", rr)
	}

	// 5. Partial progress: a restored snapshot with some repos already
	// re-read counts them as scanned.
	partial := restored.Clone()
	partial.StaleRepos = []string{"repo2"}
	if rr := restartRefreshInfo(partial, buildRepos(cfg, partial, governor.State{})); rr == nil || rr.ReposScanned != 2 || rr.ReposTotal != 3 {
		t.Fatalf("partial scan tally = %+v, want 2/3", rr)
	}
}

// The Overview badge reads "refreshing after restart (<ago>) · N/M repos
// scanned · showing snapshot from <t>" in the restart window, and falls
// back to "stale since <t> · serving last-good" for an ordinary refusal.
func TestOverviewStaleBadgeWordsRestartWindow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview restart badge rule was not executed")
	}
	html := indexHTML(t)
	script := overviewKPIRenderPrelude(t, html) + `
const pillEl = () => document.getElementById('overview-sidebar-badge');
const state = { showKPIs: true };
const issueSlices = [], prSlices = [];
const repos = [{ name: 'hivecommons/hive', issues: 16, prs: 20 }];
_overviewLastRepos = repos;
const snapshotAt = new Date(Date.now() - 100 * 60e3).toISOString();
const restoredAt = new Date(Date.now() - 12 * 60e3).toISOString();
window._lastStatus = {
  repos,
  overviewTotals: { issues: { forge: 16 }, prs: { forge: 20 } },
  staleSince: snapshotAt,
  restartRefresh: { restoredAt, snapshotAt, reposScanned: 5, reposTotal: 10 },
};
let out = renderOverviewKPIs(repos, issueSlices, prSlices, state);
const badge = (out.match(/<span class="overview-kpi-stale"[^>]*>([^<]*)<\/span>/) || [])[1] || '';
assert.match(badge, /^⟳ refreshing after restart \(12m ago\) · 5\/10 repos scanned · showing snapshot from /, badge);
assert.doesNotMatch(badge, /stale since/, 'restart window must not read as stale since the previous pod');
assert.doesNotMatch(badge, /1h ago/, 'the relative time is the restart, not the previous read');
assert.match(out, /title="Hive restarted 12m ago and its first full scan/);
assert.match(pillEl().title, /\(refreshing after restart \(12m ago\) · 5\/10 repos scanned · showing snapshot from .+\)$/, pillEl().title);

// The window closes: the server drops restartRefresh with staleSince.
delete window._lastStatus.restartRefresh;
delete window._lastStatus.staleSince;
out = renderOverviewKPIs(repos, issueSlices, prSlices, state);
assert.doesNotMatch(out, /overview-kpi-stale/);
assert.doesNotMatch(pillEl().title, /refreshing|stale/);

// An ordinary refusal later keeps the old wording.
window._lastStatus.staleSince = snapshotAt;
out = renderOverviewKPIs(repos, issueSlices, prSlices, state);
assert.match(out, /⚠ stale since .+ \(1h ago\) · serving last-good</);
assert.doesNotMatch(out, /refreshing after restart/);

// Scoped to one repo the server-wide restart window does not apply.
window._lastStatus.restartRefresh = { restoredAt, snapshotAt, reposScanned: 5, reposTotal: 10 };
assert.equal(overviewRestartRefresh() && overviewRestartRefresh().restoredAt, restoredAt);
_overviewKPILocalScope = 'hivecommons/hive';
assert.equal(overviewRestartRefresh(), null);
console.log('ok');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node overview restart badge failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected output: %s", out)
	}
}
