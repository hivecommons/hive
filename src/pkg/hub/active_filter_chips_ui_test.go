package hub

import (
	"regexp"
	"strings"
	"testing"
)

// The hive table's "Showing 1 of 57 · 1 filter active" summary used to name
// the COUNT of active filters but never WHICH ones, and the only place the
// selections were visible — the facet tray — is collapsed by default. Several
// filters are set from outside the tray (an alert-type chip in Attention
// needed, a usage-rollup row writing the search box, a saved default view
// restored on load), so an operator could land on a one-row table with no
// visible cause. These tests run the SHIPPED chip renderer and remover out of
// dashboardHTML under node, the same way evidence_drilldown_ui_test.go does.

// activeFilterPrefixJS slices the `ACTIVE_FILTER_PREFIX[...] = ...;` statement
// lines out of dashboardHTML so the harness renders the shipped labels.
func activeFilterPrefixJS(t *testing.T) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^ {4}(ACTIVE_FILTER_PREFIX\[[^\]]+\] = .*;)$`)
	ms := re.FindAllStringSubmatch(dashboardHTML, -1)
	if len(ms) == 0 {
		t.Fatal("ACTIVE_FILTER_PREFIX assignments not found in dashboardHTML")
	}
	lines := make([]string, 0, len(ms))
	for _, m := range ms {
		lines = append(lines, m[1])
	}
	return strings.Join(lines, "\n")
}

// activeFilterHarness is every filter state variable, the shipped toggles that
// own them, and the chip renderer/remover. Multi-line constant tables
// (HIVE_FILTER_CHIPS, ALERT_TYPE_LABELS, HIVE_FACET_GROUPS) are stubbed with
// the shape the code reads; everything that computes is the shipped source.
func activeFilterHarness(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		"function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }",
		"function escAttr(s) { return esc(s).replace(/\"/g, '&quot;').replace(/'/g, '&#39;'); }",
		// DOM stand-in: the toggles look the search box up and renderHives is
		// a no-op so state changes are observable without a page.
		"var _barEl = {innerHTML: '', style: {}};",
		"var document = {getElementById: function(id) { return id === 'hive-filter-bar' ? _barEl : null; }, addEventListener: function() {}};",
		"function renderHives() {}",
		"function isPlaceholderHive(h) { return !!(h && h.placeholder); }",
		"var HIVE_FILTER_CHIPS = [{key: 'degraded', label: 'Degraded'}, {key: 'ok', label: 'OK'}];",
		"function alertTypeLabel(t) { return ({'advisory-stale': 'Stale advisory'})[t] || t; }",
		"function driftKindLabel(k) { return ({'no-app': 'GitHub App missing'})[k] || k; }",
		"var HIVE_FACET_GROUPS = [{key: 'cluster', label: 'Location'}, {key: 'branch', label: 'Branch'}];",
		countryJSDecl(t, "_dashStatusFilters"),
		countryJSDecl(t, "_dashFailingCheckFilter"),
		countryJSDecl(t, "_dashDriftFilter"),
		countryJSDecl(t, "_driftSearchStash"),
		countryJSDecl(t, "UPGRADE_FILTER_UPGRADING"),
		countryJSDecl(t, "UPGRADE_FILTER_QUEUED"),
		countryJSDecl(t, "_dashUpgradeFilter"),
		countryJSDecl(t, "_dashAdvisoryStaleFilter"),
		countryJSDecl(t, "_alertTypeFilter"),
		countryJSDecl(t, "_dashSearchQuery"),
		countryJSDecl(t, "_dashFacets"),
		countryJSDecl(t, "_allDashHives"),
		countryJSDecl(t, "ACTIVE_FILTER_STATUS"),
		countryJSDecl(t, "ACTIVE_FILTER_FAILING_CHECK"),
		countryJSDecl(t, "ACTIVE_FILTER_DRIFT"),
		countryJSDecl(t, "ACTIVE_FILTER_UPGRADE"),
		countryJSDecl(t, "ACTIVE_FILTER_ADVISORY_STALE"),
		countryJSDecl(t, "ACTIVE_FILTER_ALERT"),
		countryJSDecl(t, "ACTIVE_FILTER_SEARCH"),
		countryJSDecl(t, "ACTIVE_FILTER_FACET"),
		countryJSDecl(t, "ACTIVE_FILTER_PREFIX"),
		activeFilterPrefixJS(t),
		jsFunc(t, "activeFilterCount"),
		jsFunc(t, "toggleStatusFilter"),
		jsFunc(t, "setFailingCheckFilter"),
		jsFunc(t, "toggleDriftFilter"),
		jsFunc(t, "toggleUpgradeFilter"),
		jsFunc(t, "toggleAdvisoryStaleFilter"),
		jsFunc(t, "clearAlertFilter"),
		jsFunc(t, "clearHiveSearch"),
		jsFunc(t, "toggleFacetValue"),
		jsFunc(t, "clearAllHiveFilters"),
		jsFunc(t, "upgradeFilterLabel"),
		jsFunc(t, "facetGroupLabel"),
		jsFunc(t, "activeFilterChips"),
		jsFunc(t, "activeFilterChipsHTML"),
		jsFunc(t, "removeHiveFilter"),
		jsFunc(t, "renderStatusFilterBar"),
		jsFunc(t, "fleetQuadrantScopeLabel"),
		// Turn every narrowing control on at once: the observed case was a
		// single alert-type drill-down, but the chips must enumerate all of
		// them or the badge count and the chip row disagree.
		"function armEveryFilter() {",
		"  _dashStatusFilters = {degraded: true};",
		"  _dashFailingCheckFilter = 'github_auth';",
		"  _dashDriftFilter = 'no-app';",
		"  _dashUpgradeFilter = UPGRADE_FILTER_QUEUED;",
		"  _dashAdvisoryStaleFilter = true;",
		"  _alertTypeFilter = 'advisory-stale';",
		"  _dashSearchQuery = 'kalantar \"x\"';",
		"  _dashFacets = {cluster: {'cluster-a': true}, branch: {v5: true}};",
		"}",
	}, "\n")
}

// TestActiveFilterChipsEnumerateEveryCountedFilter: one chip per selection,
// equal to activeFilterCount(), with the shipped group label on each.
func TestActiveFilterChipsEnumerateEveryCountedFilter(t *testing.T) {
	src := strings.Join([]string{
		activeFilterHarness(t),
		"armEveryFilter();",
		"var chips = activeFilterChips();",
		"result = {count: activeFilterCount(), chips: chips.length, html: activeFilterChipsHTML(), none: activeFilterChips().length};",
		"clearAllHiveFilters();",
		"result.afterClear = activeFilterChips().length;",
		"result.afterClearHTML = activeFilterChipsHTML();",
	}, "\n")
	var got struct {
		Count, Chips, AfterClear int
		HTML, AfterClearHTML     string
	}
	runEvidenceJS(t, src, &got)
	if got.Count == 0 || got.Chips != got.Count {
		t.Fatalf("chips (%d) must equal activeFilterCount (%d) — a counted filter without a chip is exactly the invisible filter being fixed", got.Chips, got.Count)
	}
	for _, want := range []string{
		"Health: Degraded",
		"Failing check: github_auth",
		"Drift: GitHub App missing",
		"Upgrade: Queued for auto-upgrade",
		"Advisory digest: Stale advisory",
		"Alert: Stale advisory",
		// The search term is escaped for text and for the data attribute.
		"Search: kalantar &quot;x&quot;",
		`data-filter-kind="search" data-filter-value="kalantar &quot;x&quot;"`,
		"Location: cluster-a",
		"Branch: v5",
		`class="filter-chip filter-chip-active"`,
		`aria-label="Remove filter Alert: Stale advisory"`,
		`<span class="filter-chip-x" aria-hidden="true">&times;</span>`,
	} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("chip row is missing %q:\n%s", want, got.HTML)
		}
	}
	if got.AfterClear != 0 || got.AfterClearHTML != "" {
		t.Errorf("Clear filters left %d chips (%q)", got.AfterClear, got.AfterClearHTML)
	}
}

// TestRemoveHiveFilterTurnsOffExactlyOneSelection: the × on a chip routes
// through the same toggle the tray uses and removes only that selection.
func TestRemoveHiveFilterTurnsOffExactlyOneSelection(t *testing.T) {
	src := strings.Join([]string{
		activeFilterHarness(t),
		"armEveryFilter();",
		"var before = activeFilterCount();",
		"removeHiveFilter('alert', 'advisory-stale');",
		"var afterAlert = {count: activeFilterCount(), alert: _alertTypeFilter, search: _dashSearchQuery};",
		"removeHiveFilter('facet', 'cluster\\ncluster-a');",
		"var afterFacet = {count: activeFilterCount(), facets: JSON.parse(JSON.stringify(_dashFacets))};",
		// Remove every remaining chip through the chip row itself.
		"var guard = 0;",
		"while (activeFilterChips().length && guard++ < 20) { var c = activeFilterChips()[0]; removeHiveFilter(c.kind, c.value); }",
		"result = {before: before, afterAlert: afterAlert, afterFacet: afterFacet, end: activeFilterCount()};",
	}, "\n")
	var got struct {
		Before, End int
		AfterAlert  struct {
			Count         int
			Alert, Search string
		}
		AfterFacet struct {
			Count  int
			Facets map[string]map[string]bool
		}
	}
	runEvidenceJS(t, src, &got)
	if got.AfterAlert.Count != got.Before-1 || got.AfterAlert.Alert != "" {
		t.Errorf("removing the alert chip: count %d→%d, alert=%q", got.Before, got.AfterAlert.Count, got.AfterAlert.Alert)
	}
	if got.AfterAlert.Search == "" {
		t.Error("removing the alert chip must not touch the search filter")
	}
	if got.AfterFacet.Count != got.Before-2 {
		t.Errorf("removing one facet value: count %d, want %d", got.AfterFacet.Count, got.Before-2)
	}
	if _, still := got.AfterFacet.Facets["cluster"]; still {
		t.Errorf("cluster facet still selected after its chip was removed: %v", got.AfterFacet.Facets)
	}
	if _, gone := got.AfterFacet.Facets["branch"]; !gone {
		t.Errorf("removing the cluster chip dropped the branch facet too: %v", got.AfterFacet.Facets)
	}
	if got.End != 0 {
		t.Errorf("removing every chip left %d filters active", got.End)
	}
}

// TestStatusFilterBarShowsChipsOnlyWhileNarrowed: the summary line carries the
// chips when anything filters, and stays the plain count otherwise.
func TestStatusFilterBarShowsChipsOnlyWhileNarrowed(t *testing.T) {
	src := strings.Join([]string{
		activeFilterHarness(t),
		"var hives = [{id: 'a'}, {id: 'b'}, {id: 'p', placeholder: true}];",
		"renderStatusFilterBar(hives, 2);",
		"var idle = _barEl.innerHTML;",
		"_alertTypeFilter = 'advisory-stale';",
		"renderStatusFilterBar(hives, 1);",
		"result = {idle: idle, narrowed: _barEl.innerHTML};",
	}, "\n")
	var got struct{ Idle, Narrowed string }
	runEvidenceJS(t, src, &got)
	if strings.Contains(got.Idle, "filter-chip") || !strings.Contains(got.Idle, "2 assigned hives") {
		t.Errorf("unfiltered bar must be the plain count with no chips:\n%s", got.Idle)
	}
	for _, want := range []string{
		"Showing 1 of 2 assigned hives &middot; 1 filter active",
		`<span class="filter-chips filter-chips-active">`,
		`data-filter-kind="alert" data-filter-value="advisory-stale"`,
		"Alert: Stale advisory",
		`onclick="clearAllHiveFilters()">Clear filters</button>`,
	} {
		if !strings.Contains(got.Narrowed, want) {
			t.Errorf("narrowed bar is missing %q:\n%s", want, got.Narrowed)
		}
	}
}

// TestFleetQuadrantScopeLabelNeverClaimsInView: the fleet median is computed
// server-side over every hive and does not follow the table's filters, so the
// card says "all N hives" and, while the table is narrowed, says the filters
// do not apply — instead of "N hives in view" beside a "Showing 1 of 57" table.
func TestFleetQuadrantScopeLabelNeverClaimsInView(t *testing.T) {
	src := strings.Join([]string{
		activeFilterHarness(t),
		"var plain = fleetQuadrantScopeLabel(99), one = fleetQuadrantScopeLabel(1);",
		"_alertTypeFilter = 'advisory-stale';",
		"var narrowed = fleetQuadrantScopeLabel(99);",
		"result = {plain: plain, narrowed: narrowed, one: one};",
	}, "\n")
	var got struct{ Plain, Narrowed, One string }
	runEvidenceJS(t, src, &got)
	if got.Plain != "all 99 hives" {
		t.Errorf("unfiltered scope label = %q", got.Plain)
	}
	if got.Narrowed != "all 99 hives (table filters not applied)" {
		t.Errorf("narrowed scope label = %q", got.Narrowed)
	}
	if got.One != "all 1 hive" {
		t.Errorf("singular scope label = %q", got.One)
	}
	header := jsFunc(t, "fleetQuadrantHeaderHTML")
	if strings.Contains(header, "in view") {
		t.Error("fleetQuadrantHeaderHTML still claims the median is over hives \"in view\"")
	}
	if !strings.Contains(header, "fleetQuadrantScopeLabel(") {
		t.Error("fleetQuadrantHeaderHTML does not use fleetQuadrantScopeLabel for its hive count")
	}
}

// TestActiveFilterChipsWiredIntoBarAndClickHandler pins the plumbing: the bar
// renders the chips, and a delegated listener (no inline handler literals to
// escape) turns a chip click into removeHiveFilter.
func TestActiveFilterChipsWiredIntoBarAndClickHandler(t *testing.T) {
	bar := jsFunc(t, "renderStatusFilterBar")
	if !strings.Contains(bar, "activeFilterChipsHTML()") {
		t.Error("renderStatusFilterBar does not render the active filter chips")
	}
	for _, want := range []string{
		"var chip = t.closest('.filter-chip-active[data-filter-kind]');",
		"removeHiveFilter(chip.getAttribute('data-filter-kind'), chip.getAttribute('data-filter-value') || '');",
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboardHTML is missing the delegated chip click handler line %q", want)
		}
	}
}
