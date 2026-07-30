package hub

import (
	"strings"
	"testing"
)

// The per-check health detail lives in the inline dashboardHTML JS. These tests
// pin the invariants that are easy to break during a rebase (nine agents are
// editing this same file) and impossible to catch with a Go compile.

// TestHealthCheckHelpersPresent asserts the shared per-check helpers exist, so a
// merge that drops one fails here rather than at runtime with a blank table.
func TestHealthCheckHelpersPresent(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"healthChecks normaliser", "function healthChecks(h)"},
		{"failingChecks selector", "function failingChecks(h)"},
		{"inline row summary", "function failingCheckSummary(h)"},
		{"fleet tally", "function failingCheckCounts(hives)"},
		{"check-name filter setter", "function setFailingCheckFilter(name)"},
		{"failing status set", "var FAILING_CHECK_STATUSES"},
		{"named inline-name budget", "var INLINE_FAILING_CHECK_NAMES"},
		{"named name-length budget", "var INLINE_CHECK_NAME_MAXLEN"},
		{"named filter-option cap", "var MAX_FAILING_CHECK_FILTER_OPTIONS"},
		{"named fleet-signal threshold", "var FLEET_CHECK_SIGNAL_MIN_HIVES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}

// TestHealthChecksDefensiveAccess asserts every read of a spoke-supplied check
// field is guarded. Health is map[string]any off the wire, so checks may be
// absent, null, empty, or hold entries missing fields; one malformed spoke must
// not blank the whole My Hives table.
func TestHealthChecksDefensiveAccess(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"health object guarded", "var hp = (h && h.health) || {};"},
		{"absent/empty checks short-circuit", "if (!raw || !raw.length) return [];"},
		{"non-object entries skipped", "if (!ck || typeof ck !== 'object') continue;"},
		{"name type-checked", "typeof ck.name === 'string' ? ck.name : ''"},
		{"status type-checked with fallback", "typeof ck.status === 'string' ? ck.status : 'unknown'"},
		{"detail type-checked with fallback", "typeof ck.detail === 'string' ? ck.detail : ''"},
		{"unnamed checks dropped", "if (!nm) continue;"},
		{"filter iterates guarded list", "failingChecks(h).some(function(ck)"},
		{"tally iterates guarded list", "failingChecks(h).forEach(function(ck)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing defensive guard %q", tc.snippet)
			}
		})
	}
}

// TestFailingCheckSummaryEscapes asserts the spoke-supplied check name and
// detail are escaped everywhere they reach the DOM. Both are user-influenced.
func TestFailingCheckSummaryEscapes(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"inline pill tooltip escaped", "'<span title=\"' + esc(full) + '\""},
		{"inline pill label escaped", "esc(label) + '</span>';"},
		{"filter chip tooltip escaped", "title=\"' + esc(tip) + '\""},
		// The failing-check picker moved from the old standalone chip bar into
		// the facet tray, so the label now sits in its own facet-value-label
		// span. The invariant is unchanged: the spoke-supplied check name is
		// escaped before it reaches the DOM.
		{"filter chip label escaped", "'<span class=\"facet-value-label\">' + esc(nm) + '</span>'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing escaping %q", tc.snippet)
			}
		})
	}
}

// TestSingleHoverPanelInvariant guards the regression that was just fixed: a
// native title attribute AND a custom panel on the same element make the browser
// draw its own tooltip on top of the panel — two overlapping boxes. The custom
// panel branch must never carry a title.
func TestSingleHoverPanelInvariant(t *testing.T) {
	const panelMarker = `'<span class="hive-access-wrap"`
	idx := strings.Index(dashboardHTML, panelMarker)
	if idx < 0 {
		t.Fatalf("consolidated hover panel markup not found in dashboardHTML")
	}
	// The panel is built as one concatenated return expression; scan to the end
	// of that statement.
	//
	// The end is anchored on the statement TERMINATOR rather than on whichever
	// variable happens to be rendered last. An earlier version matched
	// "accessRows + '</span></span>';", so appending another section to the
	// panel (recent activity) silently moved the marker and the guard stopped
	// finding the expression at all. Matching the closing tags + ';' keeps this
	// test working as the panel gains sections — which is exactly when the
	// title-attribute regression is most likely to be reintroduced.
	rest := dashboardHTML[idx:]
	end := strings.Index(rest, "'</span></span>';")
	if end < 0 {
		t.Fatalf("could not find the end of the consolidated hover panel expression")
	}
	panel := rest[:end]
	if strings.Contains(panel, "title=") {
		t.Errorf("consolidated hover panel carries a native title attribute; "+
			"that stacks a browser tooltip on top of the custom panel. Panel markup:\n%s", panel)
	}
}

// TestFilterComposition asserts the new check-name filter composes with the
// existing status chips and stays scoped away from unassigned placeholders.
func TestFilterComposition(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		// The render signature must include the check filter or toggling it on
		// unchanged data is treated as a no-op and nothing re-renders.
		// Matched on the fragment alone: later filters append their own state to
		// the signature and may wrap it across source lines, so neither a
		// trailing ';' nor a leading '+ ' can be relied on. The invariant is
		// that the check filter is PART of the signature, not where it sits.
		{"render signature includes check filter", "'|' + _dashFailingCheckFilter"},
		// Clearing must clear both filter kinds, else "Clear filters" leaves the
		// list mysteriously short.
		{"clear resets check filter", "_dashFailingCheckFilter = '';"},
		// Placeholder scoping is unchanged and still splits before filtering.
		{"placeholder split retained", "(isPlaceholderHive(allHives[_si]) ? unassignedAll : assignedAll)"},
		{"filters applied to assigned only", "applyDashFilters(assignedAll).concat(unassignedAll)"},
		// Any-active must account for the check filter so the Clear button
		// shows. The per-filter OR chain was replaced by activeFilterCount(),
		// which also drives the "N filters active" summary; the check filter now
		// participates by being counted there. Both halves are asserted so the
		// link cannot be broken at either end: the counter must include the
		// check filter, and anyActive must be derived from the counter.
		{"check filter is counted", "if (_dashFailingCheckFilter) n++;"},
		{"clear button accounts for check filter", "var anyActive = activeN > 0;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}

// TestHoverGroupsFailuresFirst asserts the hover panel sorts failing checks
// ahead of passing ones so the reason for a bad status reads first.
func TestHoverGroupsFailuresFirst(t *testing.T) {
	if !strings.Contains(dashboardHTML, "var fa = FAILING_CHECK_STATUSES[a.status] ? 0 : 1;") {
		t.Error("hover panel does not sort failing checks first")
	}
	if !strings.Contains(dashboardHTML, "healthChecks(h).slice().sort(") {
		t.Error("hover panel should sort a COPY of the normalised checks, not the raw payload")
	}
}
