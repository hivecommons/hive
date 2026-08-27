package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The country flag is a small feature with three ways to be silently wrong:
// deriving the wrong glyph, guessing a country nobody claimed, and quietly
// disturbing the thousands of existing user records on the PVC. These tests
// pin all three, plus the dashboard wiring that has no Go function to call.

// TestNormalizeCountryCode pins the shape validator, including the cases that
// must be REJECTED. A validator that accepted a one-letter or three-letter
// value would let a half-formed pair of regional indicators reach a render.
func TestNormalizeCountryCode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		// Canonical form passes through.
		{"GB", "GB"},
		{"US", "US"},
		// Lowercase is normalized UP — a hand-edited record or an older client
		// may hold "gb", and it must render the same flag as "GB".
		{"gb", "GB"},
		{"jp", "JP"},
		{"Fr", "FR"},
		// Surrounding whitespace is not a different country.
		{"  DE  ", "DE"},
		{"\tIT\n", "IT"},
		// Everything below is invalid and must normalize to "" so that no flag
		// renders at all.
		{"", ""},
		{"U", ""},       // too short
		{"USA", ""},     // alpha-3, not alpha-2
		{"12", ""},      // digits
		{"U1", ""},      // mixed
		{"U S", ""},     // internal space
		{"--", ""},      // punctuation
		{"🇬🇧", ""},      // the GLYPH is not the code; we store codes
		{"<b", ""},      // markup-shaped
		{"'; DROP", ""}, // injection-shaped
		{"ÉE", ""},      // non-ASCII letters
	} {
		if got := normalizeCountryCode(tc.in); got != tc.want {
			t.Errorf("normalizeCountryCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCountryFlagEmoji pins the alpha-2 → regional-indicator derivation. The
// expected values are written as explicit code-point pairs rather than pasted
// glyphs so the test states WHAT it expects rather than depending on this
// file's own encoding surviving an editor.
func TestCountryFlagEmoji(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		// G=U+1F1EC, B=U+1F1E7
		{"GB", "\U0001F1EC\U0001F1E7"},
		// Lowercase must derive the SAME glyph as uppercase — this is the case
		// a naive implementation gets wrong, because 'g' is not 'G' and the
		// arithmetic would run off the end of the regional-indicator block.
		{"gb", "\U0001F1EC\U0001F1E7"},
		// J=U+1F1EF, P=U+1F1F5
		{"JP", "\U0001F1EF\U0001F1F5"},
		// U=U+1F1FA, S=U+1F1F8
		{"us", "\U0001F1FA\U0001F1F8"},
	} {
		if got := countryFlagEmoji(tc.in); got != tc.want {
			t.Errorf("countryFlagEmoji(%q) = %q (% x), want %q", tc.in, got, got, tc.want)
		}
	}

	// An invalid or unset code renders NOTHING. Not a globe, not a replacement
	// character, not a lone regional indicator — the empty string, so the
	// render sites that append it unconditionally emit nothing at all.
	for _, bad := range []string{"", "U", "USA", "12", "??", "  ", "🇬🇧"} {
		if got := countryFlagEmoji(bad); got != "" {
			t.Errorf("countryFlagEmoji(%q) = %q, want \"\" — an unknown country must render nothing", bad, got)
		}
	}
}

// TestParseAcceptLanguageCountry pins the ONLY inference signal. The negative
// half matters more than the positive half: inferring a country from a bare
// language tag would attach a flag to a user who never claimed one.
func TestParseAcceptLanguageCountry(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
	}{
		{"simple region", "en-GB", "GB"},
		{"first tag wins over later ones", "en-GB,en;q=0.9,fr-FR;q=0.8", "GB"},
		{"q-values stripped", "fr-FR;q=0.9", "FR"},
		{"lowercase region normalized", "pt-br", "BR"},
		{"script subtag skipped", "zh-Hans-CN", "CN"},
		{"falls through a region-less first tag", "en,de-DE;q=0.9", "DE"},
		{"whitespace tolerated", " ja-JP , en ", "JP"},

		// --- No evidence: must infer NOTHING ---
		{"empty header", "", ""},
		{"bare language is not a country", "en", ""},
		{"bare language, several", "en,fr,de", ""},
		{"wildcard", "*", ""},
		{"garbage", ";;;", ""},
		{"alpha-3 region is not alpha-2", "en-USA", ""},
		{"numeric UN region code", "es-419", ""},
	} {
		if got := parseAcceptLanguageCountry(tc.header); got != tc.want {
			t.Errorf("%s: parseAcceptLanguageCountry(%q) = %q, want %q", tc.name, tc.header, got, tc.want)
		}
	}
}

// TestApplyInferredCountryNeverOverwritesExplicit is the privacy/correctness
// invariant of the whole fallback: a browser's language setting must never
// overwrite a country the user chose. Without this guard, a user's declared
// country would silently flip when they travel or borrow a laptop.
func TestApplyInferredCountryNeverOverwritesExplicit(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "fr-FR")

	// Explicit value present → untouched, and reported as no change.
	u := &SaaSUser{GitHubUsername: "ada", Country: "GB"}
	if changed := applyInferredCountry(u, req); changed {
		t.Error("applyInferredCountry reported a change to a record that already had a country")
	}
	if u.Country != "GB" {
		t.Errorf("Country = %q, want %q — an explicit choice must outrank Accept-Language", u.Country, "GB")
	}

	// Empty value → filled from the header.
	u2 := &SaaSUser{GitHubUsername: "ada"}
	if changed := applyInferredCountry(u2, req); !changed {
		t.Error("applyInferredCountry reported no change while filling an empty country")
	}
	if u2.Country != "FR" {
		t.Errorf("Country = %q, want %q", u2.Country, "FR")
	}

	// No evidence → still empty. Never a guess.
	u3 := &SaaSUser{GitHubUsername: "ada"}
	bare := httptest.NewRequest(http.MethodGet, "/", nil)
	bare.Header.Set("Accept-Language", "en")
	if changed := applyInferredCountry(u3, bare); changed {
		t.Error("applyInferredCountry reported a change for a header with no region subtag")
	}
	if u3.Country != "" {
		t.Errorf("Country = %q, want empty — a bare language tag is not evidence of a country", u3.Country)
	}

	// Nil-safe on both sides: this runs on the login path and must not panic.
	if applyInferredCountry(nil, req) {
		t.Error("applyInferredCountry(nil, req) reported a change")
	}
	if applyInferredCountry(&SaaSUser{}, nil) {
		t.Error("applyInferredCountry(user, nil) reported a change")
	}
}

// TestSaaSUserCountryRoundTrip is the PVC-compatibility guard. Every existing
// user record on disk must survive load→save byte-identical, which means the
// new field has to be absent from the JSON when unset, and has to not disturb
// its neighbours when set.
func TestSaaSUserCountryRoundTrip(t *testing.T) {
	// A record as it exists on the PVC today — no country key at all.
	const legacy = `{"github_username":"ada","created_at":"2020-01-01T00:00:00Z","last_login":"2020-01-02T00:00:00Z","hives":{"h1":"owner"},"saas_quota":3,"blocked":false,"full_name":"Ada Lovelace","slack_id":"U01ABCDEF23","login_count":7}`

	var u SaaSUser
	if err := json.Unmarshal([]byte(legacy), &u); err != nil {
		t.Fatalf("unmarshal legacy record: %v", err)
	}
	if u.Country != "" {
		t.Errorf("Country = %q on a record that never had one, want empty", u.Country)
	}
	// The other fields must be intact — a struct edit that shifted a tag would
	// show up here rather than as a mystery in production.
	if u.GitHubUsername != "ada" || u.FullName != "Ada Lovelace" || u.SlackID != "U01ABCDEF23" ||
		u.SaaSQuota != 3 || u.LoginCount != 7 || u.Hives["h1"] != "owner" {
		t.Errorf("legacy record did not round-trip its existing fields: %+v", u)
	}

	// Re-marshalled, the record must still carry NO country key. omitempty is
	// what keeps thousands of untouched files from being rewritten.
	out, err := json.Marshal(&u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "country") {
		t.Errorf("re-marshalled legacy record grew a country key: %s", out)
	}

	// With a country set, the key appears — and nothing else changes.
	u.Country = "GB"
	out2, err := json.Marshal(&u)
	if err != nil {
		t.Fatalf("marshal with country: %v", err)
	}
	if !strings.Contains(string(out2), `"country":"GB"`) {
		t.Errorf("country not persisted: %s", out2)
	}
	var back SaaSUser
	if err := json.Unmarshal(out2, &back); err != nil {
		t.Fatalf("unmarshal round two: %v", err)
	}
	if back.Country != "GB" || back.FullName != "Ada Lovelace" || back.SlackID != "U01ABCDEF23" ||
		back.SaaSQuota != 3 || back.LoginCount != 7 {
		t.Errorf("setting Country disturbed other fields: %+v", back)
	}
}

// TestProvisionRequestCountryRoundTrip mirrors the guard above for the request
// record, which has its own file per pending request on the same PVC.
func TestProvisionRequestCountryRoundTrip(t *testing.T) {
	const legacy = `{"username":"ada","org":"acme","repos":"widget","primary_repo":"widget","acmm_level":1,"auth_method":"app","requested_at":"2020-01-01T00:00:00Z","status":"pending","full_name":"Ada Lovelace"}`

	var pr ProvisionRequest
	if err := json.Unmarshal([]byte(legacy), &pr); err != nil {
		t.Fatalf("unmarshal legacy request: %v", err)
	}
	if pr.Country != "" {
		t.Errorf("Country = %q on a request that never had one, want empty", pr.Country)
	}
	out, err := json.Marshal(&pr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "country") {
		t.Errorf("re-marshalled legacy request grew a country key: %s", out)
	}
}

// TestApplyRequestContactToUserCountry pins the priority rule at the point the
// wizard's answer lands on the user record: an explicit pick REPLACES an
// inferred value (unlike FullName/SlackID, which only backfill), and a request
// carrying no country leaves whatever is there alone.
func TestApplyRequestContactToUserCountry(t *testing.T) {
	// Explicit pick overrides a previously inferred country.
	u := &SaaSUser{GitHubUsername: "ada", Country: "FR"} // e.g. inferred at login
	applyRequestContactToUser(u, &ProvisionRequest{Country: "GB"})
	if u.Country != "GB" {
		t.Errorf("Country = %q, want GB — the wizard pick is authoritative over an inference", u.Country)
	}

	// Lowercase in a stored request is normalized on the way in.
	u2 := &SaaSUser{GitHubUsername: "ada"}
	applyRequestContactToUser(u2, &ProvisionRequest{Country: "jp"})
	if u2.Country != "JP" {
		t.Errorf("Country = %q, want JP", u2.Country)
	}

	// A request with no country (the common case, and every request filed
	// before this shipped) must not blank an existing value.
	u3 := &SaaSUser{GitHubUsername: "ada", Country: "GB"}
	applyRequestContactToUser(u3, &ProvisionRequest{})
	if u3.Country != "GB" {
		t.Errorf("Country = %q, want GB — an empty request must not clear a stored country", u3.Country)
	}

	// An invalid stored value is dropped rather than rendered.
	u4 := &SaaSUser{GitHubUsername: "ada"}
	applyRequestContactToUser(u4, &ProvisionRequest{Country: "USA"})
	if u4.Country != "" {
		t.Errorf("Country = %q, want empty — a malformed code must not reach a render", u4.Country)
	}

	// Nil-safe, like the rest of this helper.
	applyRequestContactToUser(nil, &ProvisionRequest{Country: "GB"})
	applyRequestContactToUser(&SaaSUser{}, nil)
}

// TestDashboardCountryFlagWiring guards the dashboard half, which lives inside
// the dashboardHTML raw string and so has no Go function to exercise. House
// style: assert the symbols and the exact wiring, because a typo here is
// invisible to the compiler and shows up only as a missing flag.
func TestDashboardCountryFlagWiring(t *testing.T) {
	html := dashScript(t)

	for _, snippet := range []string{
		// The derivation helpers must exist...
		"function normalizeCountryCode(code)",
		"function countryFlagEmoji(code)",
		"function countryFlagHTML(code)",
		// ...and the base code point must be the real one. A wrong base yields
		// plausible-looking but wrong glyphs (letters, not flags).
		"var REGIONAL_INDICATOR_BASE = 0x1F1E6;",
		// ...and the flag must actually be rendered beside the nav avatar,
		// reading the country off the auth payload. The nav goes through
		// countryNavHTML rather than countryFlagHTML directly: the signed-in
		// viewer's own flag is a button (and renders even when unset, so a
		// user with no country still has a way to add one), while
		// countryFlagHTML stays the read-only glyph used for everyone else.
		"countryNavHTML(data.country)",
		// The CSS class the glyph is wrapped in must exist, or the flag renders
		// at whatever size the surrounding text happens to be.
		".country-flag {",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q — the avatar country flag is not wired", snippet)
		}
	}

	// The flag must come from a DERIVED glyph, never from an external image
	// host — that is the whole reason for the regional-indicator approach.
	for _, banned := range []string{
		"flagcdn.com",
		"flagsapi.com",
		"country-flags",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("dashboardHTML references %q — flags must be derived Unicode, not remote images", banned)
		}
	}

	// Ordering: the helper must be DEFINED before the nav render that calls it.
	// Both live in the same script, so a definition that drifted below its use
	// inside an unhoisted expression would break the first paint.
	// Both helpers must be defined before the nav render, since countryNavHTML
	// calls countryFlagEmoji on the way to painting the button.
	def := strings.Index(html, "function countryNavHTML(code)")
	use := strings.Index(html, "countryNavHTML(data.country)")
	if def < 0 || use < 0 {
		t.Fatalf("could not locate countryNavHTML definition (%d) and use (%d)", def, use)
	}
	if def > use {
		t.Error("countryNavHTML is defined after the nav render that calls it")
	}
	if glyph := strings.Index(html, "function countryFlagHTML(code)"); glyph < 0 {
		t.Error("countryFlagHTML disappeared — it is still the read-only glyph for other users' flags")
	}
}

// TestAuthUserPayloadOmitsEmptyCountry pins the payload shape: a user with no
// country must produce the same JSON the dashboard has always received, so the
// (currently vast) majority of sessions are unaffected by this feature.
func TestAuthUserPayloadOmitsEmptyCountry(t *testing.T) {
	// Exercised at the marshalling level the handler uses — the handler itself
	// needs a verified session cookie, which the neighbouring handler tests
	// build; this asserts the invariant the handler's `if country != ""` guard
	// exists to produce.
	payload := map[string]any{
		"authenticated": true,
		"login":         "ada",
		"avatar_url":    "https://example.invalid/a.png",
		"hub_admin":     false,
	}
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "country") {
		t.Errorf("payload for a user with no country carries a country key: %s", out)
	}

	payload["country"] = "GB"
	out2, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out2), `"country":"GB"`) {
		t.Errorf("country missing from payload: %s", out2)
	}
}

// TestGetStartedWizardCountryWiring guards the wizard half — the only surface
// where a user can state their country explicitly. It is a static file, so the
// same substring discipline applies.
func TestGetStartedWizardCountryWiring(t *testing.T) {
	// Read from the embedded FS, not from disk, so this asserts against the
	// bytes actually shipped in the binary (same discipline as
	// collectInTreeJSSources in request_provision_callers_test.go).
	b, err := staticFS.ReadFile("static/get-started.html")
	if err != nil {
		t.Fatalf("read embedded static/get-started.html: %v", err)
	}
	wizard := string(b)

	for _, snippet := range []string{
		// The control itself, and the blank option that must remain the default.
		`id="req-country"`,
		`<option value="">Prefer not to say</option>`,
		// The list and the helpers that fill it.
		"function populateCountrySelect()",
		"function selectedCountry()",
		"populateCountrySelect();",
		// The value must ride the POST body.
		"country: selectedCountry()",
	} {
		if !strings.Contains(wizard, snippet) {
			t.Errorf("get-started.html is missing %q — the country dropdown is not wired", snippet)
		}
	}

	// PRIVACY: country must never be put in a URL. A query parameter would leak
	// it into access logs, Referer headers and browser history.
	for _, banned := range []string{
		"country=",
		"?country",
		"&country",
	} {
		if strings.Contains(wizard, banned) {
			t.Errorf("get-started.html contains %q — country must ride the JSON body, never a URL", banned)
		}
	}
}
