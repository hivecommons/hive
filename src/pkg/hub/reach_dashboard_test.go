package hub

// Surface tests for the PR Reach table on the hub /dashboard status surface
// (#3995, phase 2c of #3973). dashboardHTML is a single served string, so
// these pin the load-bearing wiring: the section exists, it is fed from
// /api/reach (the admin-gated endpoint that actually serves fleet reach —
// never a spoke-side surface that cannot see it), it renders unmeasured
// deltas as the word "unmeasured" (distinguishable from a numeric zero), and
// it participates in the page's poll cycle. The existing brace-balance test
// (dashboard_js_structure_test.go) covers the new script's syntax class.

import (
	"strings"
	"testing"
)

func TestDashboardReachSectionWired(t *testing.T) {
	for _, marker := range []string{
		// Markup: the collapsible admin section, cluster-health pattern.
		`id="reach-section"`,
		`id="reach-table-container"`,
		`id="reach-summary-bar"`,
		// Data source: the hub's own admin-gated reach endpoint.
		`fetch('/api/reach')`,
		// Poll wiring: initial load plus the slow reach interval.
		`loadReach();`,
		`setInterval(loadReach, REACH_POLL_MS)`,
		// Admin gating mirror: the panel hides on 403 like cluster health.
		`if (!_isAdmin) return;`,
		// Table columns the epic promised: reach, latency, delta, flags.
		`Reach (hives)`,
		`First exec`,
		`Error Δ`,
		// Never-ran and D4 co-attribution surface as labeled flags.
		`never ran`,
		`shared`,
	} {
		if !strings.Contains(dashboardHTML, marker) {
			t.Errorf("dashboardHTML missing %q — the reach table is not fully wired", marker)
		}
	}
}

// TestDashboardReachUnmeasuredDistinguishable: the never-fabricate contract
// at the UI layer — an unmeasured delta renders as the WORD "unmeasured",
// and the renderer branches on the measured flag rather than treating a
// missing number as 0.0.
func TestDashboardReachUnmeasuredDistinguishable(t *testing.T) {
	if !strings.Contains(dashboardHTML, ">unmeasured</span>") {
		t.Error("dashboardHTML never renders the literal 'unmeasured' state")
	}
	if !strings.Contains(dashboardHTML, "d.measured") {
		t.Error("dashboardHTML delta renderer does not branch on the measured flag")
	}
}
