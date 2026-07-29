package hub

import (
	"regexp"
	"strings"
	"testing"
)

// The My Hives facet panel is a click-toggled left tray, collapsed by default,
// with the health chips and the failing-check picker moved into it from the old
// standalone chip bar. All of it lives inside the dashboardHTML raw string, so
// these tests guard the wiring the Go compiler cannot see.

// TestFacetTrayMarkup asserts the tray's structural elements exist. Every one of
// them is reached by getElementById at runtime, where a missing id is a silent
// no-op rather than an error.
func TestFacetTrayMarkup(t *testing.T) {
	html := dashScript(t)
	for _, id := range []string{
		`id="hive-facet-shell"`,
		`id="hive-facet-toggle"`,
		`id="hive-facet-tray"`,
		`id="hive-facet-close"`,
		`id="hive-facet-active-badge"`,
		`id="hive-facet-rail"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("dashboardHTML is missing tray element %s", id)
		}
	}
	// The toggle must be a real focusable control with the disclosure
	// semantics, not a div with a click handler — keyboard users reach the
	// tray through it and nothing else.
	if !strings.Contains(html, `<button type="button" class="facet-rail-tab" id="hive-facet-toggle"`) {
		t.Error("the facet tray toggle must be a real <button>")
	}
	for _, attr := range []string{`aria-expanded="false"`, `aria-controls="hive-facet-tray"`} {
		if !strings.Contains(html, attr) {
			t.Errorf("the facet tray toggle is missing %s", attr)
		}
	}
}

// TestFacetTrayDefaultsCollapsedAndPersists pins the two behaviours the product
// decision rests on: the tray is CLOSED on a first visit (so the dashboard is
// uncluttered), and the open/closed choice survives a reload under the shared
// 'hive-*' localStorage convention. The comparison operator encodes the default,
// so an accidental flip is a one-character change — exactly what a test is for.
func TestFacetTrayDefaultsCollapsedAndPersists(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, `var LS_FACET_TRAY_OPEN = 'hive-facet-tray-open';`) {
		t.Error("the facet tray's localStorage key must be a named constant following the hive-* convention")
	}
	// Open only when explicitly stored 'true' => default collapsed, and a
	// corrupted value falls back to collapsed rather than to open.
	if !strings.Contains(html, `localStorage.getItem(LS_FACET_TRAY_OPEN) === 'true'`) {
		t.Error("the facet tray must default to COLLAPSED (=== 'true')")
	}
	if !strings.Contains(html, `localStorage.setItem(LS_FACET_TRAY_OPEN,`) {
		t.Error("toggling the facet tray must persist the new state")
	}
	// CSS alone must never reveal the tray: a :hover rule would be unreachable
	// on touch and would flicker across the rail/tray gap. .facet-open, set by
	// JS from the persisted flag, is the only thing that opens it.
	if !strings.Contains(html, `.facet-shell.facet-open .facet-tray { display: block; }`) {
		t.Error("the tray must be revealed by the JS-driven .facet-open class")
	}
	if regexp.MustCompile(`\.facet-shell:hover`).MatchString(html) {
		t.Error("the facet tray must not be hover-revealed — it is unreachable on touch")
	}
}

// TestFacetTraySymbols asserts every function the tray's handlers call is also
// defined. These are wired by delegation and by inline onclick; a typo in either
// is invisible to the Go compiler and surfaces only as a dead control.
func TestFacetTraySymbols(t *testing.T) {
	html := dashScript(t)
	for _, fn := range []string{
		"applyFacetTrayState",
		"setFacetTrayOpen",
		"toggleFacetTray",
		"activeFilterCount",
		"renderFacetTrayAffordance",
		"facetGroupShell",
		"renderStatusFacetGroup",
		"renderFailingCheckFacetGroup",
		"clearFacetsButton",
	} {
		if !strings.Contains(html, "function "+fn+"(") {
			t.Errorf("dashboardHTML is missing a definition for %s()", fn)
		}
		if strings.Count(html, fn+"(") < 2 {
			t.Errorf("dashboardHTML defines %s() but never calls it", fn)
		}
	}
}

// TestStatusFiltersMovedIntoTray asserts the health chips and the failing-check
// picker now render inside the tray rather than as a standalone chip row, and —
// crucially — that moving them did NOT change how they combine. The state
// variables and the matching predicate are untouched; only the markup moved.
func TestStatusFiltersMovedIntoTray(t *testing.T) {
	html := dashScript(t)
	// Rendered as facet groups inside the tray.
	for _, want := range []string{
		"function renderStatusFacetGroup(assignedNoPlaceholders)",
		"function renderFailingCheckFacetGroup(assignedNoPlaceholders)",
		`facetGroupShell(FACET_GROUP_HEALTH, 'Health'`,
		`facetGroupShell(FACET_GROUP_FAILING_CHECK, 'Failing check (one at a time)'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the moved filters must render as tray groups; missing: %s", want)
		}
	}
	// Semantics unchanged: same state, same predicate, same handlers.
	for _, want := range []string{
		"_dashStatusFilters",       // health chips still OR among themselves
		"_dashFailingCheckFilter",  // failing check still single-select, ANDed
		"toggleStatusFilter(",
		"setFailingCheckFilter(",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("moving the filters must not change their semantics; missing: %s", want)
		}
	}
	// The old standalone chip row is gone: renderStatusFilterBar must no longer
	// emit filter-chip buttons, only the summary and the way out of it.
	i := strings.Index(html, "function renderStatusFilterBar(allHivesIn, shownCount)")
	if i < 0 {
		t.Fatal("renderStatusFilterBar not found")
	}
	body := html[i:]
	if end := strings.Index(body, "\n    /* renderViewBar"); end >= 0 {
		body = body[:end]
	}
	if strings.Contains(body, `class="filter-chip on"`) || strings.Contains(body, "filter-chip-dot") {
		t.Error("renderStatusFilterBar must no longer draw the status chip row — it moved into the tray")
	}
	if !strings.Contains(body, `class="filter-summary"`) {
		t.Error("renderStatusFilterBar must still show the Showing X of Y summary")
	}
	// An empty bar must not reserve vertical space now that its chips are gone.
	if !strings.Contains(body, "bar.style.display = total ? '' : 'none';") {
		t.Error("the filter bar must collapse when it has no summary to show")
	}
}

// TestActiveFilterCountCoversEveryFilter is the anti-invisible-filter guard.
// With the tray collapsed by default, the rail's badge is the ONLY thing telling
// an operator why the table is short. Every control that can narrow the list
// must therefore be counted; omitting one produces a filtered list with no
// visible cause, which is worse than no filtering at all.
func TestActiveFilterCountCoversEveryFilter(t *testing.T) {
	html := dashScript(t)
	i := strings.Index(html, "function activeFilterCount()")
	if i < 0 {
		t.Fatal("activeFilterCount not found")
	}
	body := html[i:]
	if end := strings.Index(body, "\n    /* renderFacetTrayAffordance"); end >= 0 {
		body = body[:end]
	}
	for _, state := range []string{
		"_dashStatusFilters",
		"_dashFailingCheckFilter",
		"_dashDriftFilter",
		"_alertTypeFilter",
		"_dashSearchQuery",
		"_dashFacets",
	} {
		if !strings.Contains(body, state) {
			t.Errorf("activeFilterCount omits %s — that filter would narrow the list "+
				"with nothing on screen explaining why", state)
		}
	}
	// The collapsed rail must actually surface the count.
	for _, want := range []string{
		`toggle.classList.toggle('has-active', n > 0)`,
		"badge.textContent = n > 0 ? String(n) : '';",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the collapsed rail must indicate active filters; missing: %s", want)
		}
	}
}

// TestRenderCacheSignatureIncludesTrayState extends the existing signature guard
// to the tray. renderHives short-circuits on an unchanged signature, so any view
// state left out of it makes the corresponding control a silent no-op.
func TestRenderCacheSignatureIncludesTrayState(t *testing.T) {
	html := dashScript(t)
	i := strings.Index(html, "var sig = JSON.stringify(allHives) + '|' + JSON.stringify(_dashStatusFilters)")
	if i < 0 {
		t.Fatal("could not find the renderHives cache signature")
	}
	sig := html[i:]
	if end := strings.Index(sig, "if (!force"); end >= 0 {
		sig = sig[:end]
	}
	// Everything that was already in the signature must stay in it — a move
	// must never quietly drop a filter's state.
	for _, state := range []string{
		"_dashStatusFilters",
		"_dashFailingCheckFilter",
		"_dashDriftFilter",
		"_alertTypeFilter",
		"_alertShowAcked",
		"_dashSearchQuery",
		"_dashFacets",
		"_dashFacetCollapsed",
		"_dashSectionCollapsed",
		"_dashGroupBy",
		"_dashGroupCollapsed",
		"_dashActiveView",
		"_dashDefaultView",
		"_dashSavedViews",
		"_dashFacetTrayOpen",
	} {
		if !strings.Contains(sig, state) {
			t.Errorf("renderHives cache signature omits %s — changes to it would be a silent no-op", state)
		}
	}
}

// TestClearAllHiveFiltersStillClearsEverything pins the escape hatch. It is now
// reachable from three places (the empty state, the summary bar and the tray),
// so a filter it forgets would leave the operator looking at an empty table with
// a Clear button that does nothing.
func TestClearAllHiveFiltersStillClearsEverything(t *testing.T) {
	html := dashScript(t)
	i := strings.Index(html, "function clearAllHiveFilters()")
	if i < 0 {
		t.Fatal("clearAllHiveFilters not found")
	}
	body := html[i:]
	if end := strings.Index(body, "\n    function toggleAlertAcked"); end >= 0 {
		body = body[:end]
	}
	for _, reset := range []string{
		"_dashStatusFilters = {};",
		"_dashFailingCheckFilter = '';",
		"_dashDriftFilter = '';",
		"_alertTypeFilter = '';",
		"_dashFacets = {};",
		"_dashSearchQuery = '';",
	} {
		if !strings.Contains(body, reset) {
			t.Errorf("clearAllHiveFilters must reset %s", reset)
		}
	}
	// clearStatusFilters remains the group-scoped reset for the health and
	// failing-check groups, and must still clear all three of its own filters.
	j := strings.Index(html, "function clearStatusFilters()")
	if j < 0 {
		t.Fatal("clearStatusFilters not found")
	}
	sub := html[j:]
	if end := strings.Index(sub, "\n    /* toggleDriftFilter"); end >= 0 {
		sub = sub[:end]
	}
	for _, reset := range []string{
		"_dashStatusFilters = {};",
		"_dashFailingCheckFilter = '';",
		"_dashDriftFilter = '';",
	} {
		if !strings.Contains(sub, reset) {
			t.Errorf("clearStatusFilters must reset %s", reset)
		}
	}
}

// TestFacetTrayLayersBelowRowHoverPanels pins the stacking decision. The row
// hover panels (.hive-access-pop and healthBadge's custom panel) sit at
// z-index 60; the tray must stay strictly below them, or a panel on a row the
// tray overlaps would render behind it and be unreadable.
func TestFacetTrayLayersBelowRowHoverPanels(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, "z-index: 40;") {
		t.Error("the facet tray must declare an explicit z-index")
	}
	trayZ := regexp.MustCompile(`\.facet-tray \{[^}]*z-index: (\d+)`).FindStringSubmatch(html)
	if trayZ == nil {
		t.Fatal("could not read the .facet-tray z-index")
	}
	if trayZ[1] != "40" {
		t.Errorf(".facet-tray z-index is %s, want 40 — it must stay below the "+
			"z-index:60 row hover panels", trayZ[1])
	}
	// The panels' own layer must not have drifted either, or "below 60" is a
	// claim about a number that no longer exists.
	if !strings.Contains(html, "z-index:60;") {
		t.Error("the row hover panels are expected at z-index:60")
	}
}

// TestHiveTableColumnCountsAgree is the column-discipline guard. The header and
// every row must emit the same number of cells, and the colspan constants that
// span the table must match that number. A past bug moved a <th> without moving
// its <td>, which silently shifted every column right of it.
func TestHiveTableColumnCountsAgree(t *testing.T) {
	html := dashScript(t)

	// --- header: count <th> cells between the table open and </tr></thead>.
	start := strings.Index(html, `'<div class="table-wrap"><table class="hive-table"><thead><tr>'`)
	if start < 0 {
		t.Fatal("could not find the hive table header")
	}
	end := strings.Index(html[start:], `</tr></thead><tbody>' + rows`)
	if end < 0 {
		t.Fatal("could not find the end of the hive table header")
	}
	header := html[start : start+end]
	// "<th" only — "<thead" is excluded by requiring the next char to be '>' or
	// whitespace, which is what makes this a count of cells and not of tags.
	thCount := len(regexp.MustCompile(`<th[\s>]`).FindAllString(header, -1))

	// --- body: count the <td> cells buildRow emits, plus the one contributed
	// by the bulkCheckboxCell() helper, which emits no literal tag here.
	bStart := strings.Index(html, "var buildRow = function(h, i, section) {")
	if bStart < 0 {
		t.Fatal("could not find buildRow")
	}
	retIdx := strings.Index(html[bStart:], "return '<tr>' +")
	if retIdx < 0 {
		t.Fatal("could not find buildRow's return")
	}
	rStart := bStart + retIdx
	rEnd := strings.Index(html[rStart:], "'</tr>' + pendingExpandRow;")
	if rEnd < 0 {
		t.Fatal("could not find the end of buildRow's return")
	}
	rowExpr := html[rStart : rStart+rEnd]
	tdCount := len(regexp.MustCompile(`'<td[\s>]`).FindAllString(rowExpr, -1))
	if !strings.Contains(rowExpr, "bulkCheckboxCell(h, section || 'all')") {
		t.Fatal("buildRow no longer emits bulkCheckboxCell — the body count below is wrong")
	}
	tdCount++ // bulkCheckboxCell contributes exactly one cell

	if thCount != tdCount {
		t.Errorf("header emits %d <th> cells but a row emits %d cells — "+
			"every column right of the mismatch is shifted", thCount, tdCount)
	}
	// 16 since the Public column was folded into Location (visibility stacks
	// beneath the location badge) — see the Location cell in buildRow.
	const wantColumns = 16
	if thCount != wantColumns {
		t.Errorf("hive table has %d columns, want %d", thCount, wantColumns)
	}
	// The colspan constants span the whole table; a stale value leaves the
	// section separators and the pending-requests row visibly short.
	for _, decl := range []string{
		"var TOTAL_COLUMNS = 16;",
		"var TOTAL_COLUMNS_HEADER = 16;",
	} {
		if !strings.Contains(html, decl) {
			t.Errorf("missing or stale colspan constant: %s", decl)
		}
	}
}

// TestDriftColumnSitsRightOfJourney pins the column ORDER, in both the header
// and the body, at the same index. Asserting only the header is how the last
// column-move bug shipped.
func TestDriftColumnSitsRightOfJourney(t *testing.T) {
	html := dashScript(t)

	// --- header order.
	start := strings.Index(html, `'<div class="table-wrap"><table class="hive-table"><thead><tr>'`)
	end := strings.Index(html[start:], `</tr></thead><tbody>' + rows`)
	if start < 0 || end < 0 {
		t.Fatal("could not isolate the hive table header")
	}
	header := html[start : start+end]
	journeyTh := strings.Index(header, ">Journey ")
	driftTh := strings.Index(header, ">Drift<")
	if journeyTh < 0 || driftTh < 0 {
		t.Fatal("could not find the Journey and Drift header cells")
	}
	if driftTh < journeyTh {
		t.Fatal("Drift header cell comes before Journey")
	}
	// Nothing but the closing tag of Journey's own cell may sit between them:
	// any other <th> would mean Drift is not IMMEDIATELY right of Journey.
	between := header[journeyTh:driftTh]
	if n := len(regexp.MustCompile(`<th[\s>]`).FindAllString(between, -1)); n != 1 {
		t.Errorf("found %d header cells between Journey and Drift, want exactly 1 "+
			"(Drift's own <th>) — Drift must sit immediately right of Journey", n)
	}

	// --- body order, over the same expression the row is built from.
	bStart := strings.Index(html, "var buildRow = function(h, i, section) {")
	retIdx := strings.Index(html[bStart:], "return '<tr>' +")
	rStart := bStart + retIdx
	rEnd := strings.Index(html[rStart:], "'</tr>' + pendingExpandRow;")
	if bStart < 0 || retIdx < 0 || rEnd < 0 {
		t.Fatal("could not isolate buildRow's return expression")
	}
	rowExpr := html[rStart : rStart+rEnd]
	journeyTd := strings.Index(rowExpr, "journeyBadge(h.journey)")
	driftTd := strings.Index(rowExpr, "driftBadge(h)")
	if journeyTd < 0 || driftTd < 0 {
		t.Fatal("could not find the Journey and Drift body cells")
	}
	if driftTd < journeyTd {
		t.Fatal("the Drift body cell comes before the Journey body cell")
	}
	between = rowExpr[journeyTd:driftTd]
	if n := len(regexp.MustCompile(`'<td[\s>]`).FindAllString(between, -1)); n != 1 {
		t.Errorf("found %d body cells between Journey and Drift, want exactly 1 "+
			"(Drift's own <td>) — the body must match the header order", n)
	}
	// Drift must appear exactly once in each, i.e. the move did not leave a copy
	// behind at the old position.
	if n := strings.Count(header, ">Drift<"); n != 1 {
		t.Errorf("Drift appears %d times in the header, want 1", n)
	}
	if n := strings.Count(rowExpr, "driftBadge(h)"); n != 1 {
		t.Errorf("driftBadge appears %d times in a row, want 1", n)
	}
}
