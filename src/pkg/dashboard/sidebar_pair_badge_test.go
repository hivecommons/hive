package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestSidebarPairBadgeWiring pins the Overview and Governor "x/y" sidebar
// pills: same .badge-count/.oc-nav-count recipe as the other nav counts,
// hidden until their feed has answered, Overview fed by the stage tracker
// and Governor by the shared actionableNow split on /api/status.
func TestSidebarPairBadgeWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<span class="oc-nav-text">Overview</span><span id="overview-sidebar-badge" class="badge-count oc-nav-count" data-zero hidden>0/0</span></a>`,
		`<span class="oc-nav-text">Governor</span><span id="governor-sidebar-badge" class="badge-count oc-nav-count" data-zero hidden>0/0</span></a>`,
		`.oc-nav-count[hidden] { display: none; }`,
		`function stageTrackerOpenCounts(items)`,
		`function sidebarPairBadgeText(issues, prs)`,
		`function setSidebarPairBadge(el, issues, prs, title)`,
		`setSidebarPairBadge(document.getElementById('overview-sidebar-badge'), openCounts.issues, openCounts.prs, openCounts.issues + ' issues / ' + openCounts.prs + ' PRs')`,
		`if (data && data.actionableNow) setSidebarPairBadge(document.getElementById('governor-sidebar-badge'), issueCount, prCount, 'actionable: ' + issueCount + ' issues / ' + prCount + ' PRs')`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing sidebar x/y pill wiring %q", want)
		}
	}
	// The Overview pill must be written inside renderStageTracker AFTER the
	// "waiting for first scan" early return, so it never shows 0/0 before data.
	render := jsFunc(t, html, "renderStageTracker")
	waiting := strings.Index(render, "Waiting for the first scan")
	pill := strings.Index(render, "overview-sidebar-badge")
	if waiting < 0 || pill < 0 || pill < waiting {
		t.Fatalf("overview-sidebar-badge must be set after the initializing early return in renderStageTracker")
	}
}

// TestSidebarPairBadgeCounts executes the pure helpers under node: the
// Overview pill counts every open issue and open PR on the tracker
// regardless of stage, and the text is always "issues/prs".
func TestSidebarPairBadgeCounts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — sidebar x/y pill helpers were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		jsFunc(t, html, "stageTrackerOpenCounts"),
		jsFunc(t, html, "sidebarPairBadgeText"),
		`const items = [
		  { kind: 'issue', stage: 0 }, { kind: 'issue', stage: 10 }, { kind: 'issue', stage: 40, mirrored: true },
		  { kind: 'pr', stage: 30 }, { kind: 'pr', stage: 95 }, null, { kind: 'other' },
		];
		const c = stageTrackerOpenCounts(items);
		const out = [sidebarPairBadgeText(c.issues, c.prs), sidebarPairBadgeText(stageTrackerOpenCounts(null).issues, stageTrackerOpenCounts(undefined).prs), sidebarPairBadgeText('7', NaN)];
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
