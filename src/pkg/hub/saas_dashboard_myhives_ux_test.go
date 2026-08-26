package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The My Hives search / facet / collapsible-section UX lives entirely inside the
// dashboardHTML raw string, so there is no Go function to exercise. These tests
// guard the wiring instead: every handler the markup calls must exist, and the
// invariants that are easy to break during a refactor (a filter that eats the
// unassigned pool, a render cache that ignores the search term) must stay
// asserted somewhere CI runs.

// dashScript returns the dashboard HTML once, for substring assertions.
func dashScript(t *testing.T) string {
	t.Helper()
	if dashboardHTML == "" {
		t.Fatal("dashboardHTML is empty")
	}
	return dashboardHTML
}

// TestDashboardMyHivesUXSymbols asserts every function referenced from an inline
// onclick/oninput handler is also defined. A typo here is invisible to the Go
// compiler and only surfaces as a dead click in the browser.
func TestDashboardMyHivesUXSymbols(t *testing.T) {
	html := dashScript(t)
	for _, fn := range []string{
		"isPlaceholderHive",
		"hiveSearchText",
		"hiveMatchesSearch",
		"onHiveSearchInput",
		"clearHiveSearch",
		"clearAllHiveFilters",
		"hiveFacetValues",
		"hiveMatchesFacets",
		"toggleFacetValue",
		"toggleFacetGroup",
		"clearHiveFacets",
		"renderFacetRail",
		"toggleHiveSection",
		"openAssignForUser",
	} {
		if !strings.Contains(html, "function "+fn+"(") &&
			!strings.Contains(html, "async function "+fn+"(") {
			t.Errorf("dashboardHTML is missing a definition for %s()", fn)
		}
		if !strings.Contains(html, fn+"(") {
			t.Errorf("dashboardHTML defines %s() but never calls it", fn)
		}
	}
}

// TestDashboardMyHivesUXMarkup asserts the elements the JS reaches for by id are
// actually present in the markup — getElementById returning null is a silent
// no-op, not an error.
func TestDashboardMyHivesUXMarkup(t *testing.T) {
	html := dashScript(t)
	for _, id := range []string{
		`id="hive-search"`,
		`id="hive-search-row"`,
		`id="hive-facet-rail"`,
		`id="hives-container"`,
		`id="hive-filter-bar"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("dashboardHTML is missing element %s", id)
		}
	}
	// The two-column shell and its stacked mobile fallback.
	if !strings.Contains(html, ".hive-layout {") {
		t.Error("dashboardHTML is missing the .hive-layout grid")
	}
	if !strings.Contains(html, ".hive-layout { grid-template-columns: minmax(0, 1fr); }") {
		t.Error("dashboardHTML is missing the narrow-screen .hive-layout fallback")
	}
}

// TestDashboardSectionCollapseDefaults pins the required defaults: Assigned
// starts EXPANDED, Unassigned starts COLLAPSED. Both are read from localStorage
// with the shared 'hive-' key prefix. The comparison operator encodes the
// default, so an accidental flip is a one-character change — worth pinning.
func TestDashboardSectionCollapseDefaults(t *testing.T) {
	html := dashScript(t)
	for _, key := range []string{
		`var LS_SECTION_ASSIGNED_COLLAPSED = 'hive-section-assigned-collapsed';`,
		`var LS_SECTION_UNASSIGNED_COLLAPSED = 'hive-section-unassigned-collapsed';`,
	} {
		if !strings.Contains(html, key) {
			t.Errorf("dashboardHTML is missing localStorage key constant: %s", key)
		}
	}
	// Assigned: collapsed only when explicitly stored 'true' => default expanded.
	if !strings.Contains(html, `localStorage.getItem(LS_SECTION_ASSIGNED_COLLAPSED) === 'true'`) {
		t.Error("Assigned section must default to EXPANDED (=== 'true')")
	}
	// Unassigned: expanded only when explicitly stored 'false' => default collapsed.
	if !strings.Contains(html, `localStorage.getItem(LS_SECTION_UNASSIGNED_COLLAPSED) !== 'false'`) {
		t.Error("Unassigned section must default to COLLAPSED (!== 'false')")
	}
}

// TestDashboardRenderCacheSignature guards the render cache: renderHives short-
// circuits when the signature is unchanged, so every piece of filter state must
// be part of it. Omitting the search term makes typing a silent no-op.
func TestDashboardRenderCacheSignature(t *testing.T) {
	html := dashScript(t)
	i := strings.Index(html, "var sig = JSON.stringify(allHives) + '|' + JSON.stringify(_dashStatusFilters)")
	if i < 0 {
		t.Fatal("could not find the renderHives cache signature")
	}
	// The signature is built across a few concatenated lines and grows as new
	// view state is added, so bound it by the end of the statement rather than
	// by a byte count that every extra filter would push it past.
	sig := html[i:]
	if end := strings.Index(sig, "if (!force"); end >= 0 {
		sig = sig[:end]
	}
	for _, state := range []string{
		"_dashSearchQuery",
		"_dashFacets",
		"_dashFacetCollapsed",
		"_dashSectionCollapsed",
	} {
		if !strings.Contains(sig, state) {
			t.Errorf("renderHives cache signature omits %s — changes to it would be a silent no-op", state)
		}
	}
}

// TestDashboardPlaceholdersBypassNonSearchFilters asserts the split scoping
// rule: status/facet filters do not hide unassigned inventory, but free-text
// search still narrows placeholders so the visible rows match the search box.
func TestDashboardPlaceholdersBypassNonSearchFilters(t *testing.T) {
	html := dashScript(t)
	const want = "var filteredUnassigned = (_dashSearchQuery || '').trim()"
	if !strings.Contains(html, want) {
		t.Error("renderHives must apply free-text search to unassigned placeholders")
	}
	if !strings.Contains(html, "var hives = filteredAssigned.concat(filteredUnassigned);") {
		t.Error("renderHives must render the search-filtered placeholder set")
	}
	// Each assigned-row predicate is checked independently: applyDashFilters
	// ANDs these together, but other features (e.g. the fleet-alert drill-down)
	// add their own terms to the same chain, so pinning one exact expression
	// would break on every such addition without indicating a real regression.
	for _, pred := range []string{
		"hiveMatchesFilters(h)",
		"hiveMatchesSearch(h)",
		"hiveMatchesFacets(h)",
	} {
		if !strings.Contains(html, pred) {
			t.Errorf("applyDashFilters must AND %s into the filter chain", pred)
		}
	}
}

// TestDashboardAssignRoutesAreAdminOnly asserts the user-first assign entry
// point posts to the same admin-only endpoints as the pre-existing flows, i.e.
// that it is a UI shortcut and not a new, weaker path.
func TestDashboardAssignRoutesAreAdminOnly(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, "if (!_isAdmin) return;") {
		t.Error("openAssignForUser must bail out for non-admins")
	}
	// It must reuse the existing modals rather than introduce a third path.
	for _, reused := range []string{
		"openApproveModal(username)",
		"openAssignModal(placeholders[0].id, username, placeholders)",
		"'/api/saas/admin/available-placeholders'",
	} {
		if !strings.Contains(html, reused) {
			t.Errorf("openAssignForUser must reuse %s", reused)
		}
	}
}

// TestAssignHiveStillAdminOnlyForUserFirstFlow is the server-side half of the
// same claim. The new "click a user to assign them a hive" shortcut reaches
// handleAssignHive with exactly the body the pre-existing ⋮ → "Assign / Claim"
// modal sends; making the entry point easier to reach must not make the
// endpoint easier to call. A non-admin gets 403 before any hive is touched.
func TestAssignHiveStillAdminOnlyForUserFirstFlow(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	mkUser(t, "mallory")
	s := newHandlerHub2()

	const placeholderID = "ph-user-first"
	saveSaaSHive(&SaaSHive{ID: placeholderID, Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})

	// The exact payload openAssignModal submits when opened from a user row.
	const body = `{"owner":"bob","org":"acme","repos":"widget","primary_repo":"widget","acmm_level":2}`

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/assign", body, "mallory"), "id", placeholderID)
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin assign status = %d, want 403", rec.Code)
	}

	// The rejected call must not have claimed the placeholder.
	if h := loadSaaSHive(placeholderID); h == nil || h.Status != statusAvailable {
		t.Fatalf("placeholder was mutated by a rejected assign: %+v", h)
	}

	// The same body from the admin is accepted, so the 403 above is about the
	// caller and not a malformed payload.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/assign", body, hubAdminUsername), "id", placeholderID)
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin assign status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive(placeholderID); h == nil || h.Status == statusAvailable {
		t.Fatalf("admin assign did not claim the placeholder: %+v", h)
	}
}
