package hub

import (
	"strings"
	"testing"
	"time"
)

// The dashboard's upgrade counter and its two facet filters live inside the
// inline dashboardHTML JavaScript, which no Go test can execute. These tests
// therefore lock down the STRUCTURAL invariants that would silently break the
// feature — the wiring that has no other guard, and the two thresholds that
// must stay pinned to their server-side counterparts.

// TestUpgradeElapsedThresholdMatchesStuckUpgradeAlert is the important one.
//
// The red escalation exists to mark a hive the hub ALREADY considers stuck, so
// the row and the stuck-upgrade alert raised in alerts.go must flip at the same
// instant. If someone retunes staleUpgradeTimeout without retuning the UI
// constant, the row would go red before or after the alert fires and the two
// would tell the operator different stories about the same hive.
func TestUpgradeElapsedThresholdMatchesStuckUpgradeAlert(t *testing.T) {
	wantMs := int64(staleUpgradeTimeout / time.Millisecond)
	// 10 * 60 * 1000 — written as the product so the JS reads as a duration.
	const decl = "var UPGRADE_ELAPSED_RED_MS = 10 * 60 * 1000;"
	if !strings.Contains(dashboardHTML, decl) {
		t.Fatalf("UPGRADE_ELAPSED_RED_MS declaration not found as %q", decl)
	}
	if wantMs != 10*60*1000 {
		t.Fatalf("staleUpgradeTimeout is %v (%d ms), but the dashboard's "+
			"UPGRADE_ELAPSED_RED_MS is hardcoded to 10*60*1000 ms. Update the JS "+
			"constant so the red counter and the stuck-upgrade alert still agree.",
			staleUpgradeTimeout, wantMs)
	}
}

// TestUpgradeElapsedUsesNamedConstants guards the repo's no-magic-numbers rule
// for every threshold this feature introduced.
func TestUpgradeElapsedUsesNamedConstants(t *testing.T) {
	for _, decl := range []string{
		"var UPGRADE_ELAPSED_RED_MS =",
		"var UPGRADE_ELAPSED_AMBER_MS =",
		"var UPGRADE_ELAPSED_MIN_SHOW_MS =",
		"var UPGRADE_ELAPSED_SKEW_TOLERANCE_MS =",
		"var UPGRADE_FACET_STUCK_MS =",
	} {
		if !strings.Contains(dashboardHTML, decl) {
			t.Errorf("missing named constant %q — thresholds must never be inline literals", decl)
		}
	}
}

// TestUpgradeElapsedHandlesUnknownStartTime pins the guards that keep the
// counter from rendering "NaN" or a negative duration. Each of these is a
// distinct edge case: absent, empty, unparseable, and clock skew.
func TestUpgradeElapsedHandlesUnknownStartTime(t *testing.T) {
	for _, guard := range []string{
		// Missing / empty / non-string upgradeStartedAt.
		"if (!started || typeof started !== 'string') return null;",
		// Unparseable timestamp — Date.parse yields NaN.
		"if (isNaN(t)) return null;",
		// Negative elapsed from clock skew.
		"if (elapsed < 0) {",
		"if (elapsed > -UPGRADE_ELAPSED_SKEW_TOLERANCE_MS) return 0;",
		// Callers must render nothing when the elapsed time is unknown.
		"if (ms === null) return '';",
	} {
		if !strings.Contains(dashboardHTML, guard) {
			t.Errorf("missing edge-case guard: %q", guard)
		}
	}
	// formatUpgradeElapsed must refuse NaN and negatives outright rather than
	// letting them reach the DOM as "NaNm" or "-3m".
	if !strings.Contains(dashboardHTML,
		"if (typeof ms !== 'number' || isNaN(ms) || ms < 0) return '';") {
		t.Error("formatUpgradeElapsed must reject NaN and negative durations")
	}
}

// TestUpgradeCounterIsWiredIntoTheUpgradingPill catches the failure mode where
// every helper exists and is perfectly correct but nothing ever calls them, so
// the counter is invisible in production while all other tests pass.
func TestUpgradeCounterIsWiredIntoTheUpgradingPill(t *testing.T) {
	if !strings.Contains(dashboardHTML, "+ renderUpgradeElapsed(h);") {
		t.Error("renderUpgradeElapsed is never appended to the Upgrading pill — " +
			"the counter would never appear")
	}
}

// TestUpgradeFacetFilterWiring covers every call site a new filter must be
// registered in. Omitting the render-cache key is the classic miss: the filter
// toggles its state but the table never re-renders, so clicking it appears to
// do nothing at all.
func TestUpgradeFacetFilterWiring(t *testing.T) {
	for name, snippet := range map[string]string{
		"state variable":      "var _dashUpgradeFilter = '';",
		"match rule applied":  "if (!hiveMatchesUpgradeFilter(h)) return false;",
		"render cache key":    "'|' + _dashUpgradeFilter",
		"active filter count": "if (_dashUpgradeFilter) n++;",
		"clearAllHiveFilters / clearStatusFilters reset": "_dashUpgradeFilter = '';",
		"facet group rendered":                           "renderUpgradeFacetGroup(assignedReal)",
		"toggle handler":                                 "function toggleUpgradeFilter(state)",
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("%s: missing %q", name, snippet)
		}
	}
}

// TestUpgradeStateClassificationOrdering pins the precedence between the two
// facets. They are successive stages of one lifecycle, so an in-flight upgrade
// must win over a queued one; otherwise a hive counts in both facets and the
// two chips sum to more than the fleet.
func TestUpgradeStateClassificationOrdering(t *testing.T) {
	i := strings.Index(dashboardHTML, "function hiveUpgradeState(")
	if i < 0 {
		t.Fatal("hiveUpgradeState not found")
	}
	body := dashboardHTML[i : i+600]
	up := strings.Index(body, "return UPGRADE_FILTER_UPGRADING;")
	q := strings.Index(body, "return UPGRADE_FILTER_QUEUED;")
	if up < 0 || q < 0 {
		t.Fatalf("both upgrade states must be returned; got up=%d queued=%d", up, q)
	}
	if up > q {
		t.Error("h.upgrading must be tested BEFORE the queued condition, " +
			"or an upgrading hive would also be counted as queued")
	}
}

// TestQueuedHivesGetNoFabricatedCounter documents the server behaviour the UI
// depends on: UpgradeStartedAt is stamped only where Upgrading is set true, so
// a queued hive genuinely has no start time. If a future change started
// stamping it at queue time, the counter would begin reporting queue wait as
// upgrade duration and this test should be revisited deliberately.
func TestQueuedHivesGetNoFabricatedCounter(t *testing.T) {
	queued := RegistryEntry{
		ID: "queued-1", Upgrading: false, UpgradeTarget: "abc1234",
	}
	if !queued.UpgradeStartedAt.IsZero() {
		t.Fatal("a queued (not yet upgrading) hive must carry no UpgradeStartedAt")
	}
	// The JS mirrors that: the queued branch of the facet never reads a
	// timestamp, and upgradeElapsedMs returns null for an absent one.
	if !strings.Contains(dashboardHTML, "if (h.autoUpgrade && h.upgradeTarget) return UPGRADE_FILTER_QUEUED;") {
		t.Error("queued classification must not depend on upgradeStartedAt")
	}
}

// TestFailedUpgradeAlertHasADashboardLabel closes a gap left by #2308: the hub
// can raise AlertTypeFailedUpgrade, but the alert-type chip falls back to the
// raw key when ALERT_TYPE_LABELS has no entry, so the panel would read
// "failed-upgrade" instead of "Failed upgrade".
func TestFailedUpgradeAlertHasADashboardLabel(t *testing.T) {
	if !strings.Contains(dashboardHTML, "'"+AlertTypeFailedUpgrade+"': 'Failed upgrade'") {
		t.Errorf("ALERT_TYPE_LABELS has no entry for %q", AlertTypeFailedUpgrade)
	}
}

// TestUpgradeElapsedStuckWordingNamesTheFault is the anti-regression test for
// the incident itself. Past the red line the tooltip must stop describing
// progress and name the fault: a spinner that still reads as "in progress"
// after 20 minutes is precisely what made a wedged fleet look healthy.
func TestUpgradeElapsedStuckWordingNamesTheFault(t *testing.T) {
	if !strings.Contains(dashboardHTML, "almost certainly wedged, not slow") {
		t.Error("the past-threshold tooltip must name the fault rather than imply progress")
	}
}
