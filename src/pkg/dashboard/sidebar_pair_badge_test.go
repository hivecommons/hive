package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSidebarPairBadgeWiring pins the Overview and Governor "x/y" sidebar
// pills: same .badge-count/.oc-nav-count recipe as the other nav counts,
// hidden until their feed has answered, Overview mirroring the Overview
// tile totals (renderOverviewKPIs) and Governor the shared actionableNow
// split on /api/status.
func TestSidebarPairBadgeWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<span class="oc-nav-text">Overview</span><span id="overview-sidebar-badge" class="badge-count oc-nav-count" data-zero hidden>0/0</span></a>`,
		`<span class="oc-nav-text">Governor</span><span id="governor-sidebar-badge" class="badge-count oc-nav-count" data-zero hidden>0/0</span></a>`,
		`.oc-nav-count[hidden] { display: none; }`,
		`function overviewSidebarPill(openIssues, openPRs, equationLabel, staleSince, unlisted)`,
		`function sidebarPairBadgeText(issues, prs)`,
		`function setSidebarPairBadge(el, issues, prs, title)`,
		`const pill = overviewSidebarPill(openIssues, openPRs, equationLabel, staleSince, unlisted);`,
		`setSidebarPairBadge(document.getElementById('overview-sidebar-badge'), pill.issues, pill.prs, pill.title);`,
		`if (data && data.actionableNow) setSidebarPairBadge(document.getElementById('governor-sidebar-badge'), issueCount, prCount, 'actionable: ' + issueCount + ' issues / ' + prCount + ' PRs')`,
		// Contributors reads online/total off /api/status.contributorPool
		// with the tracker's presence window in the tooltip (#15651).
		`<span class="oc-nav-text">Contributors</span><span id="contributors-sidebar-badge" class="badge-count oc-nav-count" data-zero hidden>0/0</span></a>`,
		`if (sidebarBadge) setSidebarPairBadge(sidebarBadge, onlineCount, totalCount, tip);`,
		`function contributorsOnlineTitle(online, total, window)`,
		`<span id="contributors-status-badge" class="contrib-status-badge" title="0 online · 0 registered"></span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing sidebar x/y pill wiring %q", want)
		}
	}
	// The Overview pill is no longer fed by the stage tracker (that counted
	// only tracked items and drifted from the tile's "total open" figures).
	if strings.Contains(jsFunc(t, html, "renderStageTracker"), "overview-sidebar-badge") {
		t.Fatalf("renderStageTracker must not write overview-sidebar-badge; the pill mirrors renderOverviewKPIs")
	}
	// The tracker's own open counts feed only its section header; the pill
	// never reads them.
	if strings.Contains(jsFunc(t, html, "renderOverviewKPIs"), "stageTrackerOpenCounts") {
		t.Fatalf("renderOverviewKPIs must not read stageTrackerOpenCounts: the Overview pill has a single source of truth")
	}
	// The pill is written inside renderOverviewKPIs from the same openIssues /
	// openPRs the tiles print, only once /api/status has answered, and before
	// the showKPIs early return so hiding the KPI strip keeps the pill live.
	render := jsFunc(t, html, "renderOverviewKPIs")
	pill := strings.Index(render, "overviewSidebarPill(openIssues, openPRs")
	guard := strings.Index(render, "if (window._lastStatus) {")
	hide := strings.Index(render, "if (!state.showKPIs) return '';")
	if pill < 0 || guard < 0 || hide < 0 || guard > pill || pill > hide {
		t.Fatalf("overview-sidebar-badge must be set in renderOverviewKPIs after the _lastStatus guard and before the showKPIs return")
	}
}

// TestSidebarPairBadgeCounts executes the pure text helper under node:
// the text is always "issues/prs" with non-numbers read as zero.
func TestSidebarPairBadgeCounts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar x/y pill helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		jsFunc(t, html, "sidebarPairBadgeText"),
		`const out = [sidebarPairBadgeText(3, 2), sidebarPairBadgeText(undefined, null), sidebarPairBadgeText('7', NaN)];
		process.stdout.write(out.join('|'));`,
	}, "\n")
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if got, want := string(out), "3/2|0/0|7/0"; got != want {
		t.Fatalf("sidebar pair badge counts = %q, want %q", got, want)
	}
}

// TestContributorsOnlineBadgeHelpers executes the Contributors pill
// helpers under node: the tooltip names the window the tracker uses and
// the list sorts online contributors first without mutating its input.
func TestContributorsOnlineBadgeHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — Contributors online/total helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		jsFunc(t, html, "contributorsOnlineTitle"),
		jsFunc(t, html, "contributorsOnlineFirst"),
		`const src = [{github_username:'zed'}, {github_username:'amy', online:true}, {github_username:'bob', online:false}, {github_username:'kim', online:true}];
		const sorted = contributorsOnlineFirst(src).map(c => c.github_username).join(',');
		process.stdout.write([contributorsOnlineTitle(3, 12, '15m'), contributorsOnlineTitle(undefined, '2'), sorted, src[0].github_username].join('|'));`,
	}, "\n")
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if got, want := string(out), "3 online · 12 registered (heartbeat within 15m)|0 online · 2 registered|amy,kim,bob,zed|zed"; got != want {
		t.Fatalf("contributors online helpers = %q, want %q", got, want)
	}
}
