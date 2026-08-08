package hub

import (
	"strings"
	"testing"
)

// TestHiveNameSecondLineLinksToForgeRepo guards the deliberate split of the two
// hive-name lines: the TOP line (org/hive name) opens the SPOKE DASHBOARD via
// ssoDisplayLink, while the SECOND line (the org/repo path) opens the actual
// FORGE REPOSITORY (github.com / GHE host), not the dashboard. Operators asked
// for the repo path to take them to the code; a regression that routed the
// second line back through the dashboard link would silently undo that.
func TestHiveNameSecondLineLinksToForgeRepo(t *testing.T) {
	// The second-line helper must exist and target the forge repo href (rpHref,
	// the same doubling-safe ghRepoURL the GitHub icon uses), never dh/ssoDisplayLink.
	mustContain := []string{
		"var repoLink = function(text)",
		// It links to the forge repo URL.
		"if (rpHref) {",
		// The second line is rendered with repoLink, not link().
		"repoLink(repoName) + ' ' + ghIcon",
	}
	for _, s := range mustContain {
		if !strings.Contains(dashboardHTML, s) {
			t.Errorf("dashboardHTML missing forge-repo link snippet %q", s)
		}
	}
	// The TOP line must still open the dashboard via link() — unchanged.
	if !strings.Contains(dashboardHTML, "var line1 = dot + ' ' + link(orgName, true)") {
		t.Errorf("top line (orgName) must still use link() to open the spoke dashboard")
	}
	// Guard the regression directly: the second line must NOT wrap repoName in
	// the dashboard link() helper anymore.
	if strings.Contains(dashboardHTML, "link(repoName, false)") {
		t.Errorf("second line still routes the repo path through the dashboard link(); it must use repoLink() to open the forge repo")
	}
}
