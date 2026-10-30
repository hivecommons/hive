package hub

import (
	"strings"
	"testing"
)

// The hub card renders the derived upgrade state (#14950): a pill for the
// in-flight/missed states with the drain countdown and the agents holding
// the upgrade, an "Upgrade history" tooltip, and an "Upgrade now (interrupt
// turns)" action that goes through the existing upgrade POST with
// ?interrupt=1 — never a parallel endpoint, never a native dialog.
func TestDashboardRendersUpgradeProgressPillAndHistory(t *testing.T) {
	for _, want := range []string{
		"function renderUpgradeProgress(",
		"function renderUpgradeHistory(",
		"function upgradeAgentsInTurnText(",
		"h.upgradeState",
		"h.upgradeHistory",
		"st.drainRemainingS",
		"st.agentsInTurn",
		"Upgrade now (interrupt turns)",
		"async function upgradeHiveInterrupt(",
		"/upgrade?interrupt=1'",
		"h.upgradeInterruptPending",
		"upgradeProgressLine",
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboard.html missing %q", want)
		}
	}
	// Every state the hub can emit has a human label.
	labels := extractHubJSVar(t, dashboardHTML, "UPGRADE_PROGRESS_LABELS")
	for _, state := range []string{"scheduled", "delayed", "draining", "pulling", "rolling", "verifying", "done", "missed"} {
		if !strings.Contains(labels, state+":") {
			t.Errorf("UPGRADE_PROGRESS_LABELS lacks %q: %s", state, labels)
		}
	}
	fn := extractHubJSFunction(t, dashboardHTML, "upgradeHiveInterrupt")
	if !strings.Contains(fn, "await hiveConfirm(") || strings.Contains(fn, "window.confirm(") || strings.Contains(fn, " confirm(") {
		t.Errorf("upgradeHiveInterrupt must confirm through hiveConfirm, not a native dialog:\n%s", fn)
	}
	if !strings.Contains(fn, "interrupt in-flight agent turns") {
		t.Errorf("upgradeHiveInterrupt confirm wording must say turns are interrupted:\n%s", fn)
	}
}

func extractHubJSVar(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "var "+name+" = ")
	if start < 0 {
		t.Fatalf("%s not found in dashboard.html", name)
	}
	end := strings.Index(html[start:], ";\n")
	if end < 0 {
		t.Fatalf("%s has no terminating semicolon", name)
	}
	return html[start : start+end]
}
