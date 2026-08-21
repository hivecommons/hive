package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Admin-assigned country: a hub admin setting the country for ANOTHER user from
// the admin Users table, through the existing handleAdminUpdateUser edit path.
//
// The whole weight of this suite is on PROVENANCE, because the value is easy to
// store and easy to store WRONG. An admin edit is a best-effort attribution by
// someone else, so it has to land in a state that is simultaneously:
//
//   - not user-chosen (stamping CountrySetByUser would fabricate a statement by
//     the user and permanently suppress ever asking them for a real one), and
//   - stronger than Accept-Language inference (or it is silently reverted at the
//     user's next login — the #4374 bug wearing a different hat, and invisible
//     in exactly the same way).
//
// Precedence under test, strongest first: user > admin > inferred > unset.
//
// Every assertion that something did NOT happen carries a positive control, so
// none of these can pass because the machinery under them stopped running.

// adminAssignHub builds a hub with temp dirs and an admin record on disk.
// requireAdmin resolves the cookie through loadSaaSUser, so the admin needs a
// real record for an authenticated admin request to be possible at all.
func adminAssignHub(t *testing.T) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	ensureSaaSUser(hubAdminUsername)
	return NewHubServer(0, slog.Default(), "test", "v2")
}

// adminPut issues the admin edit through the REAL requireAdmin wrapper and the
// real route pattern, so authorization and path-value extraction are exercised
// rather than stubbed.
func adminPut(srv *HubServer, target, body, asUser string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/admin/users/{username}", srv.requireAdmin(srv.handleAdminUpdateUser))
	req := httptest.NewRequest(http.MethodPut, "/api/saas/admin/users/"+target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// AUDIT F4: mutations fail closed without Origin/Referer. Same-origin here so
	// the request reaches the handler and the test asserts about country, not
	// about CSRF (which csrf_f4_fail_closed_test.go owns).
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	if asUser != "" {
		req.AddCookie(testAuthCookie(asUser))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// inferenceLogin is a request whose Accept-Language would infer FR — the
// "user signs in from a differently-configured browser" event that must not be
// allowed to undo an admin's assignment.
func inferenceLogin() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Language", "fr-FR,fr;q=0.9")
	return r
}

// TestAdminAssignsCountryDoesNotClaimUserChoice is the core provenance
// assertion: the value persists, and the record does NOT claim the user picked
// it.
//
// CountrySetByUser is the field that says "this person told us". An admin
// filling in a country from a conference badge has not made the user say
// anything, and that marker is also what suppresses ever asking — so setting it
// here would both fabricate consent and permanently silence the question.
func TestAdminAssignsCountryDoesNotClaimUserChoice(t *testing.T) {
	s := adminAssignHub(t)
	mkUser(t, "ada")

	if rec := adminPut(s, "ada", `{"country":"gb"}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	u := loadSaaSUser("ada")
	if u == nil {
		t.Fatal("user disappeared")
	}
	// Normalized on the way in, like every other country path: alpha-2 is
	// case-insensitive inbound, canonical on disk.
	if u.Country != "GB" {
		t.Errorf("Country = %q, want GB", u.Country)
	}
	if u.CountrySource != countrySourceAdmin {
		t.Errorf("CountrySource = %q, want %q — an admin edit must record WHO set it",
			u.CountrySource, countrySourceAdmin)
	}
	if u.CountrySetByUser {
		t.Error("CountrySetByUser = true after an ADMIN edit — this asserts the user made a choice they never made, and permanently suppresses ever asking them")
	}

	// POSITIVE CONTROL: the same field on the same handler DOES record a user
	// choice when the user is the one writing, so the assertion above is about
	// provenance and not about the marker having quietly stopped working.
	if rec := putCountry(s, "ada", `{"country":"JP"}`); rec.Code != http.StatusOK {
		t.Fatalf("positive control: self-service status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ctl := loadSaaSUser("ada"); ctl == nil || !ctl.CountrySetByUser || ctl.CountrySource != countrySourceUser {
		t.Fatalf("positive control: self-service write did not mark the record user-chosen (%+v) — the assertion above proves nothing", ctl)
	}
}

// TestAdminAssignedCountrySurvivesInference is the regression this feature is
// most likely to lose: #4374 in its new form.
//
// An admin assignment that inference may overwrite is worse than no feature at
// all — the admin sees it save, and it is quietly reverted a login later, far
// from anything they can connect it to.
func TestAdminAssignedCountrySurvivesInference(t *testing.T) {
	s := adminAssignHub(t)
	mkUser(t, "ada")

	if rec := adminPut(s, "ada", `{"country":"GB"}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("assign: status = %d body=%s", rec.Code, rec.Body.String())
	}
	login := inferenceLogin()

	u := loadSaaSUser("ada")
	if changed := applyInferredCountry(u, login); changed {
		t.Error("inference reported a change to an ADMIN-assigned country")
	}
	if u.Country != "GB" {
		t.Errorf("Country = %q after an inference pass, want GB — an admin assignment must outrank Accept-Language", u.Country)
	}

	// An admin's explicit CLEAR is a decision too, and it is the half that has
	// no value to protect it: an empty country is byte-identical to "never
	// asked", so only the recorded source keeps inference from refilling it.
	if rec := adminPut(s, "ada", `{"country":""}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d body=%s", rec.Code, rec.Body.String())
	}
	u2 := loadSaaSUser("ada")
	if u2 == nil {
		t.Fatal("user disappeared")
	}
	if u2.Country != "" {
		t.Errorf("Country = %q after an admin clear, want empty", u2.Country)
	}
	if changed := applyInferredCountry(u2, login); changed || u2.Country != "" {
		t.Errorf("inference refilled a country an admin had explicitly cleared (changed=%v country=%q)", changed, u2.Country)
	}

	// POSITIVE CONTROL: the same header and the same helper against a record
	// nobody has claimed DOES fill in. Without this, both assertions above
	// would also pass if inference had simply stopped working.
	fresh := &SaaSUser{GitHubUsername: "fresh", Hives: map[string]string{}}
	if changed := applyInferredCountry(fresh, login); !changed || fresh.Country != "FR" {
		t.Fatalf("positive control: inference did not fill an untouched record (changed=%v country=%q) — the assertions above prove nothing",
			changed, fresh.Country)
	}
}

// TestUserOverridesAdminAssignedCountry: the user is the authority on where the
// user is. An admin's attribution is a guess made on their behalf, and the
// person themselves must always be able to correct the record about themselves
// — otherwise a wrong guess is permanent and unappealable.
func TestUserOverridesAdminAssignedCountry(t *testing.T) {
	s := adminAssignHub(t)
	mkUser(t, "ada")

	if rec := adminPut(s, "ada", `{"country":"GB"}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("admin assign: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := putCountry(s, "ada", `{"country":"JP"}`); rec.Code != http.StatusOK {
		t.Fatalf("self-service: status = %d body=%s", rec.Code, rec.Body.String())
	}

	u := loadSaaSUser("ada")
	if u == nil {
		t.Fatal("user disappeared")
	}
	if u.Country != "JP" {
		t.Errorf("Country = %q, want JP — the user must be able to correct an admin's attribution", u.Country)
	}
	if u.CountrySource != countrySourceUser || !u.CountrySetByUser {
		t.Errorf("after a self-service write: CountrySource = %q CountrySetByUser = %v, want %q/true",
			u.CountrySource, u.CountrySetByUser, countrySourceUser)
	}

	// And the reverse direction: the admin may NOT then step back on top of a
	// value the user stated about themselves. The request is not an error — the
	// rest of a multi-field edit still lands — but the country is left alone.
	rec := adminPut(s, "ada", `{"country":"DE","full_name":"Ada Lovelace"}`, hubAdminUsername)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin re-edit: status = %d body=%s, want 200 (declining the country is not a request failure)", rec.Code, rec.Body.String())
	}
	after := loadSaaSUser("ada")
	if after == nil {
		t.Fatal("user disappeared")
	}
	if after.Country != "JP" {
		t.Errorf("Country = %q after an admin re-edit, want JP — a user-chosen country outranks an admin assignment", after.Country)
	}
	if after.CountrySetByUser != true || after.CountrySource != countrySourceUser {
		t.Errorf("declined admin edit corrupted provenance: source=%q setByUser=%v", after.CountrySource, after.CountrySetByUser)
	}
	// POSITIVE CONTROL: the SAME request did apply its other field, so the
	// country was declined specifically — not the whole edit silently dropped.
	if after.FullName != "Ada Lovelace" {
		t.Fatalf("positive control: full_name = %q, want %q — the request did not reach the handler at all, so the country assertion proves nothing",
			after.FullName, "Ada Lovelace")
	}
}

// TestLegacyCountrySetByUserStillBeatsAdminAndInference covers every record
// already on the PVC. They carry ONLY CountrySetByUser — nothing backfills a
// CountrySource, because rewriting thousands of files to encode something
// derivable for free would also break the round-trip-byte-identical property.
//
// So the boolean must keep reading as exactly what it always meant: the user
// chose this. If effectiveCountrySource ever stops bridging it, the failure is
// silent and fleet-wide — every pre-existing explicit choice quietly demotes to
// "inferred" and becomes overwritable.
func TestLegacyCountrySetByUserStillBeatsAdminAndInference(t *testing.T) {
	s := adminAssignHub(t)

	// A record as it exists on disk today: the boolean, no source field.
	legacy := &SaaSUser{
		GitHubUsername:   "ada",
		Hives:            map[string]string{},
		Country:          "GB",
		CountrySetByUser: true,
	}
	if err := saveSaaSUser(legacy); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}

	if got := effectiveCountrySource(legacy); got != countrySourceUser {
		t.Errorf("effectiveCountrySource(legacy) = %q, want %q — a pre-CountrySource record must still read as user-chosen",
			got, countrySourceUser)
	}

	// Inference must not touch it.
	if changed := applyInferredCountry(legacy, inferenceLogin()); changed || legacy.Country != "GB" {
		t.Errorf("inference overwrote a legacy explicit choice (changed=%v country=%q)", changed, legacy.Country)
	}

	// Nor may an admin.
	if rec := adminPut(s, "ada", `{"country":"DE"}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("admin edit: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if after := loadSaaSUser("ada"); after == nil || after.Country != "GB" {
		t.Errorf("admin overwrote a legacy explicit choice: %+v", after)
	}

	// The legacy CLEAR shape too: CountrySetByUser with an EMPTY country is
	// "prefer not to say", and is the case a value check alone cannot see.
	legacyCleared := &SaaSUser{GitHubUsername: "grace", Hives: map[string]string{}, CountrySetByUser: true}
	if changed := applyInferredCountry(legacyCleared, inferenceLogin()); changed || legacyCleared.Country != "" {
		t.Errorf("inference refilled a legacy explicit clear (changed=%v country=%q)", changed, legacyCleared.Country)
	}

	// POSITIVE CONTROL: strip the boolean and the very same record IS writable
	// by both, so the assertions above are about the legacy bridge and not
	// about admin/inference having stopped working entirely.
	ctl := &SaaSUser{GitHubUsername: "ctl", Hives: map[string]string{}}
	if changed := applyInferredCountry(ctl, inferenceLogin()); !changed || ctl.Country != "FR" {
		t.Fatalf("positive control: inference did not fill an unmarked record (changed=%v country=%q)", changed, ctl.Country)
	}
	mkUser(t, "unmarked")
	if rec := adminPut(s, "unmarked", `{"country":"DE"}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("positive control: admin edit status = %d", rec.Code)
	}
	if after := loadSaaSUser("unmarked"); after == nil || after.Country != "DE" {
		t.Fatalf("positive control: admin could not set a country on an unmarked record (%+v) — the assertions above prove nothing", after)
	}
}

// TestAdminCountryValidation: an unusable code is refused with 400 and the
// stored value is UNCHANGED. A handler that 400s but has already mutated the
// record is the worst of both — the admin is told it failed and it did not.
func TestAdminCountryValidation(t *testing.T) {
	s := adminAssignHub(t)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"three letters", `{"country":"GBR"}`},
		{"one letter", `{"country":"G"}`},
		{"digits", `{"country":"12"}`},
		{"punctuation", `{"country":"G-"}`},
		{"markup", `{"country":"<b"}`},
		{"non-ascii", `{"country":"Ω1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh record per case, pre-seeded by an admin assignment, so
			// "unchanged" means something.
			mkUser(t, "ada")
			if rec := adminPut(s, "ada", `{"country":"GB"}`, hubAdminUsername); rec.Code != http.StatusOK {
				t.Fatalf("seed: status = %d", rec.Code)
			}
			rec := adminPut(s, "ada", tc.body, hubAdminUsername)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
			}
			if u := loadSaaSUser("ada"); u == nil || u.Country != "GB" {
				t.Errorf("stored country changed on a rejected edit: %+v", u)
			}
		})
	}

	// An explicit clear is NOT an invalid code — it is a legitimate value, and
	// the one an admin needs when they learn their guess was wrong.
	mkUser(t, "grace")
	if rec := adminPut(s, "grace", `{"country":"GB"}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("seed: status = %d", rec.Code)
	}
	if rec := adminPut(s, "grace", `{"country":"  "}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d body=%s, want 200 — whitespace is a clear, not a bad code", rec.Code, rec.Body.String())
	}
	if u := loadSaaSUser("grace"); u == nil || u.Country != "" {
		t.Errorf("explicit clear did not remove the country: %+v", u)
	}
}

// TestAdminCountryKeyAbsentLeavesCountryAlone: the pointer distinction. An edit
// to some OTHER field must not wipe a country as a side effect — the admin
// panel saves fields one at a time, so a plain string here would mean every
// quota change silently cleared the country.
func TestAdminCountryKeyAbsentLeavesCountryAlone(t *testing.T) {
	s := adminAssignHub(t)
	mkUser(t, "ada")

	if rec := adminPut(s, "ada", `{"country":"GB"}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("seed: status = %d", rec.Code)
	}
	if rec := adminPut(s, "ada", `{"saas_quota":5}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("quota edit: status = %d body=%s", rec.Code, rec.Body.String())
	}
	u := loadSaaSUser("ada")
	if u == nil || u.Country != "GB" {
		t.Errorf("an edit that never mentioned country changed it: %+v", u)
	}
	// POSITIVE CONTROL: that request did land, so the assertion is about the
	// absent key and not about the edit having been dropped.
	if u.SaaSQuota != 5 {
		t.Fatalf("positive control: SaaSQuota = %d, want 5 — the request did not apply", u.SaaSQuota)
	}
}

// TestNonAdminCannotSetAnotherUsersCountry is the privilege boundary. The
// self-service endpoint deliberately gives every user a country write; this
// asserts that write cannot be aimed at somebody else through the admin route.
func TestNonAdminCannotSetAnotherUsersCountry(t *testing.T) {
	s := adminAssignHub(t)
	mkUser(t, "ada")
	mkUser(t, "mallory")

	for _, tc := range []struct {
		name   string
		asUser string
	}{
		{"anonymous", ""},
		{"non-admin", "mallory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := adminPut(s, "ada", `{"country":"DE"}`, tc.asUser)
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
			}
			if u := loadSaaSUser("ada"); u == nil || u.Country != "" {
				t.Errorf("a rejected caller still wrote a country: %+v", u)
			}
		})
	}

	// POSITIVE CONTROL: the identical request from the real admin succeeds, so
	// the 403s above are authorization and not a handler that refuses everyone.
	if rec := adminPut(s, "ada", `{"country":"DE"}`, hubAdminUsername); rec.Code != http.StatusOK {
		t.Fatalf("positive control: admin status = %d body=%s — the 403s prove nothing", rec.Code, rec.Body.String())
	}
	if u := loadSaaSUser("ada"); u == nil || u.Country != "DE" {
		t.Fatalf("positive control: admin write did not land (%+v)", u)
	}
}

// TestUntouchedUserJSONRoundTripsByteIdentical is the compatibility guard for
// every record already on the PVC.
//
// CountrySource is new, so it MUST be omitempty: without it every one of the
// thousands of existing user files would grow a `"country_source":""` key the
// first time anything loaded and saved it — a fleet-wide rewrite to store
// nothing. The same property is why CountrySetByUser is omitempty and why
// nothing backfills either field.
func TestUntouchedUserJSONRoundTripsByteIdentical(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// A record in the shape production writes today: no country of any kind.
	original := &SaaSUser{GitHubUsername: "ada", Hives: map[string]string{"h1": "owner"}}
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"country", "country_source", "country_set_by_user"} {
		if strings.Contains(string(before), key) {
			t.Errorf("untouched user JSON contains %q — the field is not omitempty and every existing record would be rewritten to store nothing: %s",
				key, before)
		}
	}

	// Through the real save/load path, not just the encoder.
	if err := saveSaaSUser(original); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	loaded := loadSaaSUser("ada")
	if loaded == nil {
		t.Fatal("user disappeared")
	}
	after, err := json.Marshal(loaded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("round-trip changed the record:\n before=%s\n  after=%s", before, after)
	}

	// POSITIVE CONTROL: the keys DO appear once something actually sets them,
	// so the absence above is omitempty working and not the fields having been
	// dropped from the struct.
	set := &SaaSUser{GitHubUsername: "grace", Hives: map[string]string{}}
	setUserCountry(set, "GB", countrySourceUser)
	blob, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"country":"GB"`, `"country_source":"user"`, `"country_set_by_user":true`} {
		if !strings.Contains(string(blob), key) {
			t.Fatalf("positive control: a populated record is missing %s (%s) — the absence assertions prove nothing", key, blob)
		}
	}
}

// TestCountryProvenancePrecedenceTable states the ordering directly, so the
// rule is readable in one place rather than only as an emergent property of the
// handlers above. user > admin > inferred > unset, with ties allowed (a writer
// may always correct its own earlier claim).
func TestCountryProvenancePrecedenceTable(t *testing.T) {
	for _, tc := range []struct {
		existing string
		writer   string
		want     bool
	}{
		{"", countrySourceInferred, true},
		{"", countrySourceAdmin, true},
		{"", countrySourceUser, true},
		{countrySourceInferred, countrySourceInferred, true},
		{countrySourceInferred, countrySourceAdmin, true},
		{countrySourceInferred, countrySourceUser, true},
		{countrySourceAdmin, countrySourceInferred, false},
		{countrySourceAdmin, countrySourceAdmin, true},
		{countrySourceAdmin, countrySourceUser, true},
		{countrySourceUser, countrySourceInferred, false},
		{countrySourceUser, countrySourceAdmin, false},
		{countrySourceUser, countrySourceUser, true},
	} {
		u := &SaaSUser{GitHubUsername: "ada", Country: "GB", CountrySource: tc.existing}
		if got := mayOverwriteCountry(u, tc.writer); got != tc.want {
			t.Errorf("mayOverwriteCountry(existing=%q, writer=%q) = %v, want %v",
				tc.existing, tc.writer, got, tc.want)
		}
	}
}

// TestAdminUsersTableCountryEditorRendered: the control has to be REACHABLE.
// A handler with no way to call it is the same as no feature, and this is a raw
// string the compiler never looks at — a typo here is invisible until someone
// opens the panel and finds nothing there.
func TestAdminUsersTableCountryEditorRendered(t *testing.T) {
	html := dashboardHTML

	for _, snippet := range []string{
		// The input, inside the existing contact editor panel, keyed the same
		// way full_name / slack_id are so it rides the same save path.
		`data-contact-field="country"`,
		// The live preview that turns two letters into a flag and a name.
		"function countryPreviewHTML(",
		"function refreshContactCountryPreview(",
		"function contactCountryPreviewId(",
		// Built on #4371's helpers, not a second derivation of what a country
		// looks like.
		"countryFlagEmoji(c)",
		"countryDisplayName(c)",
		// Theme tokens only — the hub dashboard is single-theme, so a hardcoded
		// color here is a bug, not a shortcut.
		"color:var(--muted)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — the admin country editor is not wired", snippet)
		}
	}

	// PRIVACY: country rides the JSON body and never a URL. A query parameter or
	// a path segment would put personal data into access logs, Referer headers
	// and browser history. Mirrors the same assertion #4371/#4374 make.
	for _, banned := range []string{
		"country=",
		"?country",
		"users/' + encodeURIComponent(username) + '/country",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("dashboardHTML contains %q — country must ride the JSON body, never a URL", banned)
		}
	}
}
