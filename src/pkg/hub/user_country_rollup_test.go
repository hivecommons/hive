package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Tests for the geographic REPORTING surface: the Country column in the admin
// Users table and the fleet-wide rollup beneath it.
//
// Two halves, matching the two kinds of code involved:
//
//   - buildCountryRollup is real Go and is tested by calling it.
//   - The table cell and the rollup list live inside the dashboardHTML raw
//     string, so they are guarded house-style (see
//     saas_dashboard_hive_sort_test.go): assert on the rendered markup, because
//     a typo in there is invisible to the compiler and only shows up as a blank
//     column or a dead click in a browser.

// mkUserCountry writes a user carrying a country, so the rollup and the render
// tests have a roster with real geography on it.
func mkUserCountry(t *testing.T, username, country string) {
	t.Helper()
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: username,
		Country:        country,
		Hives:          map[string]string{},
	}); err != nil {
		t.Fatalf("saveSaaSUser(%s): %v", username, err)
	}
}

// --- The Country column -----------------------------------------------------

// TestUsersTableCountryColumnRendersFlagAndCode asserts the row cell exists and
// is built from #4371's helpers rather than a second, duplicate derivation.
//
// The specific failure this guards: someone adds a country column that emits a
// raw code with no flag, or hand-rolls its own regional-indicator arithmetic
// that then drifts from countryFlagEmoji. There must be exactly one place that
// decides what a country looks like.
func TestUsersTableCountryColumnRendersFlagAndCode(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		// The cell renderer itself.
		"function countryCell(u)",
		// Flag comes from #4371's helper, not from a fresh implementation.
		"countryFlagHTML(code)",
		// ...and the alpha-2 code renders beside it, escaped.
		"esc(code)",
		// The cell is actually placed in the row. Without this the function
		// exists, the tests on it pass, and the column never appears.
		"'<td>' + countryCell(u) + '</td>'",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — the Users table country cell is not wired", snippet)
		}
	}
}

// TestUsersTableCountryUnknownRendersNothing is the negative half, and the one
// that is easy to regress: a user with NO country must render an empty cell —
// no globe, no em dash, no "unknown" chip. That is #4371's rule, and it matters
// most in a column, where a screenful of placeholders reads as data we have.
//
// Asserted by pinning the early return: countryCell bails before emitting any
// markup when normalizeCountryCode yields "".
func TestUsersTableCountryUnknownRendersNothing(t *testing.T) {
	html := dashScript(t)
	i := strings.Index(html, "function countryCell(u)")
	if i < 0 {
		t.Fatal("countryCell is missing from dashboardHTML")
	}
	// Bound the search to the function body so a matching string elsewhere in
	// the 20k-line dashboard cannot make this pass for the wrong reason.
	body := html[i:]
	if j := strings.Index(body, "\n    }"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "if (!code) return '';") {
		t.Error("countryCell does not return '' for an unknown country — an unset country must render NOTHING, no placeholder glyph")
	}
	// Positive control: the same body must still be capable of rendering a
	// flag. Without this, deleting the whole function body would pass the
	// assertion above by accident.
	if !strings.Contains(body, "countryFlagHTML(") {
		t.Error("countryCell no longer renders a flag at all — the assertion above would pass vacuously")
	}
}

// TestUsersTableCountryColumnSortableAndColspan asserts the column follows the
// table's existing mechanisms instead of adding new ones.
//
// The colspan is the sharp edge: the contact-editor and hive-expand rows span
// USERS_TABLE_COLSPAN, so a new <th> that does not bump it leaves those rows
// one column short and the table visibly ragged.
func TestUsersTableCountryColumnSortableAndColspan(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, `sortUsers(\'country\')`) {
		t.Error("the Country header is not sortable — neighbouring columns are, so it must use the same sortUsers mechanism")
	}
	if !strings.Contains(html, "var USERS_TABLE_COLSPAN = 9;") {
		t.Error("USERS_TABLE_COLSPAN was not bumped to 9 for the Country column — panel/expand rows will be one column short")
	}
}

// --- The rollup aggregation -------------------------------------------------

// TestBuildCountryRollupGroupsAndRanks covers the core aggregation: counts group
// per country, the list is ranked descending, and the unknown bucket is both
// present and correct.
func TestBuildCountryRollupGroupsAndRanks(t *testing.T) {
	users := []SaaSUser{
		{GitHubUsername: "a", Country: "US"},
		{GitHubUsername: "b", Country: "US"},
		{GitHubUsername: "c", Country: "US"},
		{GitHubUsername: "d", Country: "GB"},
		{GitHubUsername: "e", Country: "GB"},
		{GitHubUsername: "f", Country: "DE"},
		// Unknown, three different ways: never set, blank, and a malformed
		// legacy value. All three must land in the unknown bucket rather than
		// inventing a phantom country.
		{GitHubUsername: "g"},
		{GitHubUsername: "h", Country: "   "},
		{GitHubUsername: "i", Country: "XYZ"},
	}
	got := buildCountryRollup(users)

	if got.Total != len(users) {
		t.Errorf("Total = %d, want %d — the rollup must account for every user", got.Total, len(users))
	}
	if got.Unknown != 3 {
		t.Errorf("Unknown = %d, want 3 (unset, blank, malformed)", got.Unknown)
	}
	want := []countryRollupEntry{
		{Code: "US", Count: 3},
		{Code: "GB", Count: 2},
		{Code: "DE", Count: 1},
	}
	if len(got.Countries) != len(want) {
		t.Fatalf("Countries = %+v, want %+v", got.Countries, want)
	}
	for i := range want {
		if got.Countries[i] != want[i] {
			t.Errorf("Countries[%d] = %+v, want %+v (descending by count)", i, got.Countries[i], want[i])
		}
	}
	// The counted totals must reconcile with Total, or the percentages the UI
	// draws are over a denominator that does not match the list.
	sum := got.Unknown
	for _, c := range got.Countries {
		sum += c.Count
	}
	if sum != got.Total {
		t.Errorf("counts sum to %d but Total = %d — a user was dropped or double-counted", sum, got.Total)
	}
}

// TestBuildCountryRollupNormalizesCase asserts a lowercase or padded stored code
// folds into the same bucket as its canonical form. Two buckets for "us" and
// "US" would split a country's count in half and quietly understate it.
func TestBuildCountryRollupNormalizesCase(t *testing.T) {
	got := buildCountryRollup([]SaaSUser{
		{GitHubUsername: "a", Country: "us"},
		{GitHubUsername: "b", Country: "US"},
		{GitHubUsername: "c", Country: " Us "},
	})
	if len(got.Countries) != 1 || got.Countries[0] != (countryRollupEntry{Code: "US", Count: 3}) {
		t.Fatalf("Countries = %+v, want a single US bucket of 3", got.Countries)
	}
	if got.Unknown != 0 {
		t.Errorf("Unknown = %d, want 0", got.Unknown)
	}
}

// TestBuildCountryRollupAllUnknown is the day-one state and the one most likely
// to break something: nobody has a country yet.
//
// Two invariants. Unknown must equal the roster (not be hidden), and Countries
// must be an EMPTY slice rather than nil — a nil slice marshals to JSON null,
// and `null.length` throws in the browser where `[].length` is 0, so the whole
// rollup would fail to render exactly when the operator most needs to see that
// the data is empty.
func TestBuildCountryRollupAllUnknown(t *testing.T) {
	users := []SaaSUser{{GitHubUsername: "a"}, {GitHubUsername: "b"}, {GitHubUsername: "c"}}
	got := buildCountryRollup(users)

	if got.Unknown != 3 || got.Total != 3 {
		t.Errorf("Unknown=%d Total=%d, want 3 and 3", got.Unknown, got.Total)
	}
	if got.Countries == nil {
		t.Fatal("Countries is nil — it must be an empty slice so it marshals to [] not null")
	}
	if len(got.Countries) != 0 {
		t.Errorf("Countries = %+v, want empty", got.Countries)
	}
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(blob), `"countries":null`) {
		t.Errorf("rollup marshalled countries as null: %s", blob)
	}
	// Unknown must survive the wire even at zero, so "we know where everyone
	// is" stays distinguishable from "this build forgot the field".
	empty, err := json.Marshal(buildCountryRollup(nil))
	if err != nil {
		t.Fatalf("Marshal(empty): %v", err)
	}
	if !strings.Contains(string(empty), `"unknown":0`) {
		t.Errorf("empty rollup dropped the unknown field: %s", empty)
	}
}

// --- The rollup endpoint ----------------------------------------------------

// TestHandleAdminUserCountriesServesRollup exercises the handler end to end
// against real stored users, so a break in listAllSaaSUsers wiring is caught
// here and not only in the pure aggregation test.
func TestHandleAdminUserCountriesServesRollup(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUserCountry(t, "alpha", "US")
	mkUserCountry(t, "bravo", "US")
	mkUserCountry(t, "charlie", "GB")
	mkUser(t, "delta") // no country

	rec := httptest.NewRecorder()
	s.handleAdminUserCountries(rec, httptest.NewRequest(http.MethodGet, "/api/saas/admin/user-countries", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var got countryRollup
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if got.Total != 4 {
		t.Errorf("Total = %d, want 4", got.Total)
	}
	if got.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", got.Unknown)
	}
	if len(got.Countries) != 2 || got.Countries[0].Code != "US" || got.Countries[0].Count != 2 {
		t.Fatalf("Countries = %+v, want US:2 then GB:1", got.Countries)
	}
	// PRIVACY: the rollup is an aggregate. No username may ride the response,
	// or an "aggregate" surface quietly becomes a per-user country lookup.
	for _, name := range []string{"alpha", "bravo", "charlie", "delta"} {
		if strings.Contains(rec.Body.String(), name) {
			t.Errorf("rollup response leaked username %q — it must emit counts only: %s", name, rec.Body.String())
		}
	}
}

// TestHandleAdminUserCountriesAdminGated is the access-control regression.
//
// It asserts the gate on the SOURCE — the route must be registered inside
// requireAdmin — and then proves the gate actually refuses a non-admin. The
// source assertion matters on its own: a future refactor that registers the
// route bare would leave every user's country distribution readable by any
// logged-in user, and no behavioral test of the handler function itself would
// notice, because the handler is not where the gate lives.
func TestHandleAdminUserCountriesAdminGated(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// 1. The route is wired behind requireAdmin. Read from source the same way
	// the neighbouring route assertions do (assigned_not_placeholder_test.go),
	// because the registration is a mux call with no runtime handle to inspect.
	src, err := os.ReadFile("saas.go")
	if err != nil {
		t.Fatalf("read saas.go: %v", err)
	}
	if !strings.Contains(string(src), `s.requireAdmin(s.handleAdminUserCountries)`) {
		t.Error("GET /api/saas/admin/user-countries is not registered behind requireAdmin — the country rollup would be readable by any logged-in user")
	}

	// 2. The gate refuses a non-admin, with no data in the body.
	s := newHandlerHub()
	mkUser(t, hubAdminUsername)
	mkUserCountry(t, "someone", "US")
	mkUser(t, "notanadmin")

	gated := s.requireAdmin(s.handleAdminUserCountries)

	rec := httptest.NewRecorder()
	gated(rec, reqWithUser(http.MethodGet, "/api/saas/admin/user-countries", "", "notanadmin"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET: code = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	// A 403 that still shipped the rollup would be worse than useless.
	if strings.Contains(rec.Body.String(), `"countries"`) || strings.Contains(rec.Body.String(), `"total"`) {
		t.Errorf("non-admin got rollup data in a 403 body: %q", rec.Body.String())
	}

	// 3. Positive control. Without it, a requireAdmin that rejected EVERYONE
	// would satisfy the check above while breaking the feature outright.
	rec = httptest.NewRecorder()
	gated(rec, reqWithUser(http.MethodGet, "/api/saas/admin/user-countries", "", hubAdminUsername))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET: code = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"US"`) {
		t.Errorf("admin GET did not return the rollup: %q", rec.Body.String())
	}
}

// --- The rollup UI ----------------------------------------------------------

// TestCountryRollupUIWired asserts the rollup actually renders somewhere an
// admin will see it, and that it does so without an external dependency.
func TestCountryRollupUIWired(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		// The container, inside the admin section so it inherits its gating
		// and its collapse.
		`id="user-countries-container"`,
		"function renderCountryRollup(data)",
		"function loadUserCountries()",
		// Fetched from the admin-gated endpoint...
		"fetch('/api/saas/admin/user-countries')",
		// ...and actually invoked on the admin poll. Without this the whole
		// surface is dead code that every other assertion here still passes.
		"loadUserCountries();",
		// Flags in the rollup come from #4371's helper too.
		"countryFlagHTML(code)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — the country rollup is not wired", snippet)
		}
	}
}

// TestCountryRollupShowsUnknownBucket pins the invariant the operator called out:
// the unknown count is reported, not hidden. Early on it will be nearly the
// whole roster, and a ranked list of three known countries with no denominator
// would misrepresent a 3-user sample as the fleet's geography.
func TestCountryRollupShowsUnknownBucket(t *testing.T) {
	html := dashScript(t)
	i := strings.Index(html, "function renderCountryRollup(data)")
	if i < 0 {
		t.Fatal("renderCountryRollup is missing from dashboardHTML")
	}
	body := html[i:]
	if j := strings.Index(body, "\n    async function loadUserCountries"); j > 0 {
		body = body[:j]
	}
	// The unknown bucket must be rendered as its own row, unconditionally —
	// appended outside any `if`, the same way an unknown country renders as
	// silence in the table but as a NUMBER here.
	if !strings.Contains(body, "rows += countryRollupRow(unknownLabel, unknown, max, total);") {
		t.Error("renderCountryRollup does not unconditionally append the unknown bucket row")
	}
	// ...and it must be summarized in the heading, so the ratio is legible
	// without reading the bars.
	if !strings.Contains(body, "' unknown of '") {
		t.Error("the rollup heading does not state the unknown count against the total")
	}
}

// TestCountryRollupHandlesEmptyAndAllUnknown guards the two degenerate
// populations against a divide-by-zero and against a broken/empty render.
//
// An all-unknown roster is NOT the empty case: it must still draw, as a single
// 100% Unknown bar. Only a genuinely empty roster short-circuits.
func TestCountryRollupHandlesEmptyAndAllUnknown(t *testing.T) {
	html := dashScript(t)
	i := strings.Index(html, "function renderCountryRollup(data)")
	if i < 0 {
		t.Fatal("renderCountryRollup is missing from dashboardHTML")
	}
	body := html[i:]
	if j := strings.Index(body, "\n    async function loadUserCountries"); j > 0 {
		body = body[:j]
	}
	// Only an empty roster short-circuits...
	if !strings.Contains(body, "if (total <= 0)") {
		t.Error("renderCountryRollup does not handle an empty roster")
	}
	// ...and the bar scale is floored, so an all-zero population cannot divide
	// by zero into NaN% bars.
	if !strings.Contains(body, "if (max < 1) max = 1;") {
		t.Error("the bar scale is not floored at 1 — an all-zero population would divide by zero")
	}
	// The row helper guards its own divide too, so it is safe independent of
	// its caller.
	if !strings.Contains(html, "var pct = (max > 0) ? Math.round((count / max) * COUNTRY_BAR_MAX_PCT) : 0;") {
		t.Error("countryRollupRow does not guard its percentage divide")
	}
	// No external dependency: the hub serves no CDN and no external images, so
	// the rollup must be plain markup rather than a charting library or a map.
	for _, banned := range []string{"cdn.jsdelivr", "unpkg.com", "chart.js", "d3.min.js"} {
		if strings.Contains(html, banned) {
			t.Errorf("dashboardHTML references %q — the rollup must not pull an external asset", banned)
		}
	}
}
