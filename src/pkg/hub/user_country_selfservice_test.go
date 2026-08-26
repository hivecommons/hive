package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Self-service country: /api/saas/me/country is the FIRST non-admin write to a
// SaaSUser in the hub, so the tests below are weighted toward the ways that
// could go wrong rather than toward the happy path.
//
// The load-bearing one is TestMyCountryCannotSetAnotherUsersCountry: the acting
// identity must come from the session and from nowhere else. Every other
// property here (validation, clearing, inference precedence) is a correctness
// bug; that one is a privilege-escalation bug.

// countryTestHub builds a hub with temp dirs, matching the neighbouring handler
// suites. The returned cleanup restores the package-level path variables.
func countryTestHub(t *testing.T) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	s := newHandlerHub()
	return s
}

// putCountry issues a PUT as `username`, through the SESSION cookie — the same
// cookie production mints. The username is never in the body or the URL, which
// is the point of the endpoint.
func putCountry(s *HubServer, username, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPut, "/api/saas/me/country", body, username)
	// Same-origin, so requireAuth's fail-closed CSRF gate is satisfied when the
	// request goes through the mux (see TestMyCountryRequiresAuth).
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	s.handleMyCountry(rec, req)
	return rec
}

// TestMyCountryUpdatesOwnCountry is the happy path: an authenticated user sets
// their own country and it lands on their record, normalized and marked
// explicit.
func TestMyCountryUpdatesOwnCountry(t *testing.T) {
	s := countryTestHub(t)
	mkUser(t, "ada")

	rec := putCountry(s, "ada", `{"country":"gb"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	// Persisted, upper-cased (the input was lowercase — alpha-2 is
	// case-insensitive on the way in, canonical on the way out).
	u := loadSaaSUser("ada")
	if u == nil {
		t.Fatal("user disappeared")
	}
	if u.Country != "GB" {
		t.Errorf("Country = %q, want GB", u.Country)
	}
	// The marker is what makes this survive the next login's inference.
	if !u.CountrySetByUser {
		t.Error("CountrySetByUser = false after an explicit self-service write — inference will clobber it")
	}

	// The response echoes the stored CODE (not a glyph) so the caller needs no
	// second round-trip and derives the flag the same way every render does.
	var resp struct {
		Country  string `json:"country"`
		Explicit bool   `json:"explicit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Country != "GB" || !resp.Explicit {
		t.Errorf("response = %+v, want {GB true}", resp)
	}

	// A subsequent GET reads back the same thing.
	getRec := httptest.NewRecorder()
	s.handleMyCountry(getRec, reqWithUser(http.MethodGet, "/api/saas/me/country", "", "ada"))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s, want 200", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), `"country":"GB"`) {
		t.Errorf("GET body = %s, want country GB", getRec.Body.String())
	}
}

// TestMyCountryCannotSetAnotherUsersCountry is the SECURITY test.
//
// The endpoint resolves its subject from the session cookie. A caller who names
// someone else — by any of the field names a naive implementation might have
// honoured — must not touch that person's record. The write is not rejected; it
// is simply applied to the CALLER, because the body never had a say in who the
// subject was. Both halves are asserted: the victim is unchanged AND the caller
// is the one who moved.
//
// The positive control at the end is what keeps this test honest: it proves the
// write path works at all, so a future change that broke saving entirely could
// not make this test pass for the wrong reason.
func TestMyCountryCannotSetAnotherUsersCountry(t *testing.T) {
	s := countryTestHub(t)
	mkUser(t, "attacker")

	// The victim has a country they chose.
	victim := &SaaSUser{GitHubUsername: "victim", Hives: map[string]string{}, Country: "JP", CountrySetByUser: true}
	if err := saveSaaSUser(victim); err != nil {
		t.Fatal(err)
	}

	// Every plausible way to name a target in the body. None may be honoured.
	for _, body := range []string{
		`{"country":"FR","username":"victim"}`,
		`{"country":"FR","github_username":"victim"}`,
		`{"country":"FR","login":"victim"}`,
		`{"country":"FR","user":"victim"}`,
		`{"country":"FR","id":"victim"}`,
		`{"country":"FR","target":"victim"}`,
	} {
		rec := putCountry(s, "attacker", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("body %s: status = %d, want 200 (the extra key must be ignored, not fatal)", body, rec.Code)
		}
		v := loadSaaSUser("victim")
		if v == nil {
			t.Fatalf("body %s: victim record disappeared", body)
		}
		if v.Country != "JP" {
			t.Fatalf("body %s: victim Country = %q, want JP — a request named another user and MUTATED them", body, v.Country)
		}
		if !v.CountrySetByUser {
			t.Fatalf("body %s: victim CountrySetByUser was cleared", body)
		}
		// The write landed on the SESSION's user instead, which is the correct
		// (and only possible) reading of the request.
		a := loadSaaSUser("attacker")
		if a == nil || a.Country != "FR" {
			t.Fatalf("body %s: caller's own Country = %v, want FR — the write should apply to the session user", body, a)
		}
		// Reset for the next case so each iteration observes a real transition.
		a.Country = ""
		a.CountrySetByUser = false
		if err := saveSaaSUser(a); err != nil {
			t.Fatal(err)
		}
	}

	// POSITIVE CONTROL: the same handler, same victim record, but acting AS the
	// victim — proves the victim's record is writable and that the assertions
	// above failed to move it because of the session gate, not because nothing
	// in this test could ever write anything.
	if rec := putCountry(s, "victim", `{"country":"FR"}`); rec.Code != http.StatusOK {
		t.Fatalf("positive control: status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if v := loadSaaSUser("victim"); v == nil || v.Country != "FR" {
		t.Fatalf("positive control: victim Country = %v, want FR — the write path itself is broken, so the negative cases prove nothing", v)
	}
}

// TestMyCountryRejectsInvalidCode pins the 400s. An endpoint that quietly
// stored garbage — or quietly stored "" for garbage — would put a broken value
// (or a silent deletion) one typo away.
func TestMyCountryRejectsInvalidCode(t *testing.T) {
	s := countryTestHub(t)
	mkUser(t, "ada")

	// Seed a known-good value so a bad request that WROTE anything is visible.
	if rec := putCountry(s, "ada", `{"country":"GB"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed: status = %d", rec.Code)
	}

	for _, tc := range []struct{ name, body string }{
		{"alpha-3", `{"country":"USA"}`},
		{"one letter", `{"country":"U"}`},
		{"digits", `{"country":"12"}`},
		{"mixed", `{"country":"U1"}`},
		{"punctuation", `{"country":"--"}`},
		{"the glyph rather than the code", `{"country":"🇦🇺"}`},
		{"markup-shaped", `{"country":"<b"}`},
		{"non-ASCII letters", `{"country":"ÉE"}`},
		// A body with no country key states no intention. It is refused rather
		// than read as a clear — otherwise `{}` would silently wipe a country.
		{"missing key", `{}`},
		{"key present but null", `{"country":null}`},
		{"not JSON at all", `not json`},
	} {
		rec := putCountry(s, "ada", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d body=%s, want 400", tc.name, rec.Code, rec.Body.String())
		}
		if u := loadSaaSUser("ada"); u == nil || u.Country != "GB" {
			t.Errorf("%s: a rejected request changed the stored country to %v, want GB untouched", tc.name, u)
		}
	}
}

// TestMyCountryExplicitClear: an empty string is a legitimate value — "prefer
// not to say" — and must actually remove the country while RECORDING that the
// removal was deliberate.
func TestMyCountryExplicitClear(t *testing.T) {
	s := countryTestHub(t)
	mkUser(t, "ada")

	if rec := putCountry(s, "ada", `{"country":"GB"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed: status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := putCountry(s, "ada", `{"country":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	u := loadSaaSUser("ada")
	if u == nil {
		t.Fatal("user disappeared")
	}
	if u.Country != "" {
		t.Errorf("Country = %q after an explicit clear, want empty", u.Country)
	}
	// The marker MUST survive the clear. It is the only thing distinguishing
	// "chose to have no flag" from "never asked", and without it the next login
	// puts a flag back.
	if !u.CountrySetByUser {
		t.Error("CountrySetByUser = false after an explicit clear — the next login's inference will undo the removal")
	}
	// Whitespace-only is the same explicit clear, not a validation error: the
	// input is trimmed before the empty check.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "ada", Hives: map[string]string{}, Country: "GB"}); err != nil {
		t.Fatal(err)
	}
	if rec := putCountry(s, "ada", `{"country":"   "}`); rec.Code != http.StatusOK {
		t.Fatalf("whitespace clear: status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if u := loadSaaSUser("ada"); u == nil || u.Country != "" {
		t.Errorf("whitespace clear left Country = %v, want empty", u)
	}
}

// TestMyCountryExplicitChoiceSurvivesInference is the precedence invariant, and
// the reason CountrySetByUser exists at all.
//
// Both directions matter, and the SECOND is the one a value-only guard gets
// wrong: an explicit PICK is protected by `Country != ""` alone, but an
// explicit CLEAR leaves a field that looks identical to "never asked", so
// without the marker the next login would helpfully refill it.
func TestMyCountryExplicitChoiceSurvivesInference(t *testing.T) {
	s := countryTestHub(t)
	mkUser(t, "ada")

	// A login from a browser that would infer FR.
	login := httptest.NewRequest(http.MethodGet, "/", nil)
	login.Header.Set("Accept-Language", "fr-FR,fr;q=0.9")

	// --- Direction 1: an explicit PICK is not overwritten. ---
	if rec := putCountry(s, "ada", `{"country":"GB"}`); rec.Code != http.StatusOK {
		t.Fatalf("pick: status = %d body=%s", rec.Code, rec.Body.String())
	}
	u := loadSaaSUser("ada")
	if changed := applyInferredCountry(u, login); changed {
		t.Error("inference reported a change to an explicitly-picked country")
	}
	if u.Country != "GB" {
		t.Errorf("Country = %q after an inference pass, want GB — an explicit pick must win", u.Country)
	}

	// --- Direction 2: an explicit CLEAR is not undone. ---
	if rec := putCountry(s, "ada", `{"country":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d body=%s", rec.Code, rec.Body.String())
	}
	u2 := loadSaaSUser("ada")
	if u2 == nil {
		t.Fatal("user disappeared")
	}
	if changed := applyInferredCountry(u2, login); changed {
		t.Error("inference refilled a country the user had explicitly cleared")
	}
	if u2.Country != "" {
		t.Errorf("Country = %q after an inference pass, want empty — an explicit clear must stick", u2.Country)
	}

	// POSITIVE CONTROL: the same header, the same helper, on a record that has
	// made NO deliberate choice, does fill in. Without this the two assertions
	// above would also pass if inference had simply stopped working.
	fresh := &SaaSUser{GitHubUsername: "fresh", Hives: map[string]string{}}
	if changed := applyInferredCountry(fresh, login); !changed || fresh.Country != "FR" {
		t.Fatalf("positive control: inference did not fill an untouched record (changed=%v country=%q) — the assertions above prove nothing",
			changed, fresh.Country)
	}
}

// TestMyCountryRequiresAuth: an unauthenticated caller is refused, at the route
// (through requireAuth, which is how it is registered) and at the handler.
func TestMyCountryRequiresAuth(t *testing.T) {
	s := countryTestHub(t)

	// Through the real gate. A same-origin header is supplied so the request
	// gets PAST the fail-closed CSRF check and the 401 proves something about
	// AUTH — the same reasoning as TestLiteEnrollRouteRequiresAuth.
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/me/country", s.requireAuth(s.handleMyCountry))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/saas/me/country", strings.NewReader(`{"country":"GB"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}

	// And with NO origin information at all, the CSRF gate refuses it first —
	// a mutation must positively demonstrate it is same-origin or not-a-browser.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPut, "/api/saas/me/country", strings.NewReader(`{"country":"GB"}`))
	req2.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("headerless mutation: status = %d body=%s, want 403 (CSRF fails closed)", rec2.Code, rec2.Body.String())
	}

	// The handler alone, with no cookie, must also refuse rather than rely on
	// its caller having been wrapped. A future re-registration that forgot the
	// wrapper would then still fail closed.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPut, "/api/saas/me/country", strings.NewReader(`{"country":"GB"}`))
	req3.Header.Set("Content-Type", "application/json")
	s.handleMyCountry(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("bare handler, no session: status = %d body=%s, want 401", rec3.Code, rec3.Body.String())
	}
}

// TestMyCountryRouteRegisteredNonAdmin asserts the route is actually wired, and
// wired behind requireAuth rather than requireAdmin. The whole premise of this
// endpoint is that a NON-admin can reach it; registering it under the admin
// gate would compile, pass every handler test above, and ship a feature nobody
// it was built for can use.
func TestMyCountryRouteRegisteredNonAdmin(t *testing.T) {
	s := countryTestHub(t)
	s.mux = http.NewServeMux()
	s.registerSaaSRoutes()
	mkUser(t, "plain") // deliberately NOT a hub admin

	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPut, "/api/saas/me/country", `{"country":"GB"}`, "plain")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-admin PUT: status = %d body=%s, want 200 — the route must be requireAuth, not requireAdmin", rec.Code, rec.Body.String())
	}
	if u := loadSaaSUser("plain"); u == nil || u.Country != "GB" {
		t.Fatalf("non-admin write did not persist: %v", u)
	}

	// The GET verb is registered too, so the editor can read the current value.
	getRec := httptest.NewRecorder()
	s.mux.ServeHTTP(getRec, reqWithUser(http.MethodGet, "/api/saas/me/country", "", "plain"))
	if getRec.Code != http.StatusOK {
		t.Fatalf("non-admin GET: status = %d body=%s, want 200", getRec.Code, getRec.Body.String())
	}
}

// TestMyCountryScopeIsOneField guards the blast radius. This is the first
// non-admin write to a SaaSUser, and the reason it is safe is that it writes
// exactly ONE field. A future edit that widened the request struct would make
// quota, blocked, role and hives self-writable — so the request shape itself is
// pinned here, not just its behaviour.
func TestMyCountryScopeIsOneField(t *testing.T) {
	s := countryTestHub(t)

	// A user with everything an attacker would want to change about themselves.
	seed := &SaaSUser{
		GitHubUsername: "ada",
		Hives:          map[string]string{"h1": "viewer"},
		SaaSQuota:      1,
		Blocked:        false,
		FullName:       "Ada Lovelace",
		SlackID:        "U01ABCDEF23",
		LoginCount:     7,
	}
	if err := saveSaaSUser(seed); err != nil {
		t.Fatal(err)
	}

	rec := putCountry(s, "ada", `{"country":"GB","saas_quota":999,"blocked":false,`+
		`"hives":{"h1":"owner","h2":"owner"},"full_name":"Someone Else","slack_id":"UHACKED",`+
		`"login_count":9999,"notes":"pwned"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	u := loadSaaSUser("ada")
	if u == nil {
		t.Fatal("user disappeared")
	}
	// The one field this endpoint owns moved...
	if u.Country != "GB" {
		t.Errorf("Country = %q, want GB", u.Country)
	}
	// ...and nothing else did.
	if u.SaaSQuota != 1 {
		t.Errorf("SaaSQuota = %d, want 1 — a self-service write escalated quota", u.SaaSQuota)
	}
	if u.Hives["h1"] != "viewer" {
		t.Errorf("Hives[h1] = %q, want viewer — a self-service write escalated a hive role", u.Hives["h1"])
	}
	if _, ok := u.Hives["h2"]; ok {
		t.Error("a self-service write granted access to a new hive")
	}
	if u.FullName != "Ada Lovelace" || u.SlackID != "U01ABCDEF23" {
		t.Errorf("admin-maintained contact fields were overwritten: FullName=%q SlackID=%q", u.FullName, u.SlackID)
	}
	if u.LoginCount != 7 {
		t.Errorf("LoginCount = %d, want 7", u.LoginCount)
	}
	if u.Notes != "" {
		t.Errorf("Notes = %q, want empty — admin notes are not self-writable", u.Notes)
	}
}

// TestSaaSUserCountrySetByUserRoundTrip is the PVC guard for the new marker.
// Every existing record on disk must survive load→save byte-identical, so the
// bool has to be absent from the JSON when false.
func TestSaaSUserCountrySetByUserRoundTrip(t *testing.T) {
	const legacy = `{"github_username":"ada","created_at":"2020-01-01T00:00:00Z","hives":{"h1":"owner"},"saas_quota":3,"blocked":false,"country":"GB"}`

	var u SaaSUser
	if err := json.Unmarshal([]byte(legacy), &u); err != nil {
		t.Fatalf("unmarshal legacy record: %v", err)
	}
	// A record that predates the marker reads as NOT explicit. That is the
	// right default: its country came from the wizard or from inference, and
	// the wizard path re-stamps the marker on the next approval.
	if u.CountrySetByUser {
		t.Error("CountrySetByUser = true on a record that never had the key")
	}
	out, err := json.Marshal(&u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "country_set_by_user") {
		t.Errorf("re-marshalled legacy record grew a country_set_by_user key: %s", out)
	}

	u.CountrySetByUser = true
	out2, err := json.Marshal(&u)
	if err != nil {
		t.Fatalf("marshal with marker: %v", err)
	}
	if !strings.Contains(string(out2), `"country_set_by_user":true`) {
		t.Errorf("marker not persisted: %s", out2)
	}
}

// TestApplyRequestContactToUserStampsExplicit: the wizard pick is also a
// deliberate statement, so it must set the same marker the self-service
// endpoint does. Otherwise an approved request would leave a record looking
// "inferred", and the priority rule would hold only by the accident of the
// value happening to be non-empty.
func TestApplyRequestContactToUserStampsExplicit(t *testing.T) {
	u := &SaaSUser{GitHubUsername: "ada"}
	applyRequestContactToUser(u, &ProvisionRequest{Country: "gb"})
	if u.Country != "GB" {
		t.Fatalf("Country = %q, want GB", u.Country)
	}
	if !u.CountrySetByUser {
		t.Error("CountrySetByUser = false after a wizard pick — the pick must be marked explicit")
	}

	// A request with NO country must not stamp the marker: nothing was chosen,
	// so a later inference is still legitimate.
	u2 := &SaaSUser{GitHubUsername: "ada"}
	applyRequestContactToUser(u2, &ProvisionRequest{})
	if u2.CountrySetByUser {
		t.Error("CountrySetByUser = true after a request that stated no country")
	}
}

// TestDashboardCountryEditorWiring guards the UI half, which lives inside the
// dashboardHTML raw string and has no Go function to call. House style: assert
// the exact symbols, because a typo here is invisible to the compiler and shows
// up only as a dead click.
func TestDashboardCountryEditorWiring(t *testing.T) {
	html := dashScript(t)

	for _, snippet := range []string{
		// The editor and the nav control that opens it.
		"function openCountryEditor()",
		"function countryNavHTML(code)",
		"function countryDisplayName(code)",
		`onclick="openCountryEditor()"`,
		// The nav must render the CLICKABLE control, not the read-only glyph,
		// for the viewer's own country — countryFlagHTML returns '' when there
		// is none, which would hide the control from exactly the users who need
		// it.
		`'<span id="nav-country">' + countryNavHTML(data.country) + '</span>'`,
		// The empty state must exist, or a user with no country has no
		// affordance at all — the entire problem this PR fixes.
		"country-flag-empty",
		".country-edit-btn {",
		// The endpoint, the verb, and the JSON body.
		"'/api/saas/me/country'",
		"method: 'PUT'",
		"JSON.stringify({country: normalizeCountryCode(raw)})",
		// Theme tokens, so the control is light- and dark-safe rather than
		// carrying hardcoded colors.
		"color: var(--muted);",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — the self-service country editor is not wired", snippet)
		}
	}

	// PRIVACY: the country must ride the JSON body and never a URL. A query
	// parameter or a path segment would put personal data into access logs,
	// Referer headers and browser history. This mirrors the assertion #4371
	// makes over get-started.html.
	for _, banned := range []string{
		"me/country/",
		"me/country?",
		"country=",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("dashboardHTML contains %q — country must ride the JSON body, never a URL", banned)
		}
	}

	// Ordering: both helpers must be DEFINED before the nav render that calls
	// them, since they are referenced from an unhoisted expression.
	def := strings.Index(html, "function countryNavHTML(code)")
	use := strings.Index(html, "countryNavHTML(data.country)")
	if def < 0 || use < 0 {
		t.Fatalf("could not locate countryNavHTML definition (%d) and use (%d)", def, use)
	}
	if def > use {
		t.Error("countryNavHTML is defined after the nav render that calls it")
	}
}
