package hub

import (
	"encoding/json"
	"net/http"
	"strings"
)

// User country: an OPTIONAL ISO 3166-1 alpha-2 code on a hub user record, shown
// as a small flag beside their avatar.
//
// Three sources, in strict priority order:
//
//  1. EXPLICIT, self-service — the signed-in user sets or clears their own
//     country at any time via PUT /api/saas/me/country (handleMyCountry, at the
//     bottom of this file). This is the only surface an EXISTING user has: the
//     wizard below is a one-time gate you pass through before you have a hive,
//     so without this endpoint a country could never be corrected or removed.
//  2. EXPLICIT, wizard — the requester picks a country in the get-started
//     wizard (ProvisionRequest.Country), which is copied onto the SaaSUser
//     record on approval alongside the other contact fields.
//  3. INFERRED — best-effort, from the region subtag of the browser's
//     Accept-Language header on a completed login. Only ever fills a record
//     that has NO country and whose user has made no deliberate choice; it
//     never overwrites, and never undoes, an explicit one.
//
// Sources 1 and 2 both stamp SaaSUser.CountrySetByUser, which is what tells
// source 3 that an EMPTY country is a decision ("prefer not to say") rather
// than an absence to be helpfully filled in.
//
// PRIVACY. Country is personal data, so the inference is deliberately the
// weakest thing that works:
//
//   - Accept-Language is already on the request. Nothing new is collected, no
//     IP is geolocated, no third-party geo/analytics service is called, and no
//     user identifier leaves the box. A GeoIP database or a lookup API would
//     each have been a new dependency shipping user data (or a user's IP) off
//     the hub, so neither is used — see the PR description.
//   - It is a language preference, not a location. "en-GB" says the browser
//     asked for British English, which is evidence of a GB association but is
//     not proof the person is in GB. That is exactly why it is the FALLBACK
//     and the dropdown is the authority, and why a bare "en" (no region)
//     infers nothing at all rather than guessing US.
//   - The value is never put in a URL query string; it rides JSON bodies and
//     the user's own record.
//   - Inference is non-blocking: parseAcceptLanguageCountry cannot fail a
//     login. It returns "" for anything it does not understand and the login
//     proceeds exactly as before.
//
// The stored form is the two-letter code; the flag glyph is derived at render
// time from regional-indicator code points, so the hub never depends on an
// external image host for a flag.

// countryCodeLen is the length of an ISO 3166-1 alpha-2 code. Named because it
// is load-bearing in three places (validation, the emoji derivation, and the
// Accept-Language region check), not because two is hard to read.
const countryCodeLen = 2

// regionalIndicatorBase is the Unicode code point of REGIONAL INDICATOR SYMBOL
// LETTER A (U+1F1E6). A flag emoji is the two regional indicators for the
// country's alpha-2 letters, so the glyph for "GB" is the pair at base+('G'-'A')
// and base+('B'-'A'). This is why no image asset is needed.
const regionalIndicatorBase = 0x1F1E6

// normalizeCountryCode validates an ISO 3166-1 alpha-2 code and returns it
// upper-cased, or "" if it is not two ASCII letters.
//
// Deliberately a SHAPE check, not a membership check against a list of assigned
// codes: the hub does not need to adjudicate which territories exist, the list
// changes over time, and an unassigned-but-well-formed code renders a harmless
// pair of regional indicators. What this does reject is everything that would
// break a render or a filename — digits, punctuation, whitespace, and any
// length other than two.
func normalizeCountryCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) != countryCodeLen {
		return ""
	}
	code = strings.ToUpper(code)
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return code
}

// countryFlagEmoji returns the regional-indicator flag glyph for an alpha-2
// code, or "" when the code is not usable.
//
// Empty in, empty out is the load-bearing case: an unknown or unset country
// must render NOTHING — no globe placeholder, no "??" box, no tofu. Every
// render site appends this result unconditionally, so returning "" is what
// makes "we do not know" look like silence rather than like a broken image.
func countryFlagEmoji(code string) string {
	code = normalizeCountryCode(code)
	if code == "" {
		return ""
	}
	var sb strings.Builder
	for _, r := range code {
		sb.WriteRune(rune(regionalIndicatorBase + (r - 'A')))
	}
	return sb.String()
}

// parseAcceptLanguageCountry extracts a country from an Accept-Language header,
// best effort, or "" when the header offers no region evidence.
//
// Accept-Language is a q-weighted list, highest preference first:
//
//	en-GB,en;q=0.9,fr-FR;q=0.8
//
// The tags are scanned IN ORDER and the first one carrying a region subtag
// wins, so the browser's own stated preference order decides. The q-values are
// not parsed: browsers emit the list already sorted by descending q, and a
// hand-rolled q parser would be more code and more failure modes for a signal
// this soft.
//
// What is deliberately NOT inferred:
//
//   - A tag with no region ("en", "fr") yields "". Mapping a bare language to
//     its "main" country is exactly the guess this feature must not make: "en"
//     is not US, "es" is not ES, "pt" is not PT. No evidence, no flag.
//   - "*" (the wildcard) and malformed tags yield "".
//   - A script subtag is skipped, so "zh-Hans-CN" resolves to CN rather than
//     reading "Hans" as a region: the region subtag is the alpha-2 one.
//
// Never errors. The caller is a login path and must not be able to fail on a
// header a browser sent.
func parseAcceptLanguageCountry(header string) string {
	if header == "" {
		return ""
	}
	for _, tag := range strings.Split(header, ",") {
		// Drop any q-value / parameters, then split the tag into subtags.
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = tag[:i]
		}
		tag = strings.TrimSpace(tag)
		if tag == "" || tag == "*" {
			continue
		}
		parts := strings.Split(tag, "-")
		// parts[0] is the language; the region is a later alpha-2 subtag. Scan
		// past a script subtag (4 letters, e.g. "Hans") to find it.
		for _, sub := range parts[1:] {
			if code := normalizeCountryCode(sub); code != "" {
				return code
			}
		}
	}
	return ""
}

// inferCountryFromRequest is the single inference entry point: the best-effort
// country for the browser making this request, or "" when there is no evidence.
//
// Kept as its own function so every caller reads one name and so the set of
// signals consulted is auditable in one place. Today that set is exactly one
// header the browser already sends. Adding an IP-geolocation source here would
// change the privacy posture of the feature and must not be done silently.
func inferCountryFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return parseAcceptLanguageCountry(r.Header.Get("Accept-Language"))
}

// applyInferredCountry fills a user's country from request evidence, but ONLY
// when the record carries no country AND the user has never made a deliberate
// choice.
//
// The guard is the whole function. An explicit pick is the user speaking about
// themselves; a language header is a hint. Once the record carries a country,
// no amount of logging in from a differently-configured browser may overwrite
// it — otherwise a user's stated country would silently flip when they travel
// or borrow a laptop, which is both wrong and invisible.
//
// The CountrySetByUser half of the guard covers the case `Country != ""` alone
// cannot: an explicit CLEAR. A user who deliberately removes their country
// leaves an empty field that is indistinguishable, by value, from "never asked"
// — so without the marker the next login would quietly put a flag back and
// "prefer not to say" would be unexpressible. Absence of a value is not the
// same as absence of a decision.
//
// Returns whether it changed the record, so the caller can tell a real update
// from a no-op. Nil-safe: it sits on the login path beside other enrichment.
func applyInferredCountry(user *SaaSUser, r *http.Request) bool {
	if user == nil || user.Country != "" || user.CountrySetByUser {
		return false
	}
	code := inferCountryFromRequest(r)
	if code == "" {
		return false
	}
	user.Country = code
	return true
}

// ── Self-service: a user setting their OWN country ───────────────────────────
//
// Before this, every write to a SaaSUser was requireAdmin-gated and the only
// place a person could state their country was the get-started wizard — a
// surface you pass through exactly once, before you have a hive. That left the
// entire existing fleet with no way to set, correct, or remove their country
// ever again, which is a strange thing to say about a field whose whole purpose
// is for the user to speak about themselves.
//
// /api/saas/me/country is the smallest fix: GET reads the acting user's own
// country, PUT sets or clears it. Deliberately NOT a general profile endpoint —
// widening it to "any field on my own record" would put quota, blocked, hives
// and role one typo away from being self-writable, which is exactly the reason
// the admin gate exists. One field, allowlisted, is the whole surface.
//
// SECURITY — the identity comes from the SESSION, never from the request.
// The body carries a country and nothing else: no login, no id, no target. That
// is not an oversight to be politely ignored if someone sends one; it is the
// property that makes this endpoint safe to expose to non-admins at all. An
// unknown JSON key is discarded by the decoder, so a request naming another
// user is honoured as a write to the CALLER's own record and can never touch
// the named one. See TestMyCountryCannotSetAnotherUsersCountry.
//
// PRIVACY — country rides the JSON BODY, never the path or a query string, so
// it stays out of access logs, Referer headers and browser history. That is why
// this is a PUT with a body rather than the more obvious
// POST /api/saas/me/country/{code}.

// maxCountryRequestBodyBytes caps the request body. The only legitimate payload
// is a two-letter code in a one-key object — a few dozen bytes — so 1 KiB is
// already enormous. Named, and generous rather than tight, because the point is
// to refuse an unbounded read, not to police whitespace.
const maxCountryRequestBodyBytes = 1 << 10

// myCountryRequest is the PUT body. One field, on purpose.
//
// Country is a POINTER so the handler can tell "key absent" from
// `"country": ""`. Those must not mean the same thing: the empty string is an
// explicit CLEAR ("prefer not to say"), while an absent key is a malformed
// request that states no intention and is refused rather than silently treated
// as a clear. A plain string would collapse the two and make it possible to
// wipe a country by sending `{}`.
type myCountryRequest struct {
	Country *string `json:"country"`
}

// handleMyCountry reads (GET) or writes (PUT) the ACTING user's own country.
//
// Registered behind requireAuth, which supplies the CSRF gate, the blocked-user
// check, and — importantly here — the refusal of writes while an admin is
// impersonating. An admin viewing as someone else must not be able to edit that
// person's profile through a self-service route; requireAuth's
// blockIfImpersonatingWrite already returns 403 for any non-GET, so the PUT
// half inherits that without a check of its own. The GET half still resolves to
// the impersonated target, matching every other per-user read in the hub.
func (s *HubServer) handleMyCountry(w http.ResponseWriter, r *http.Request) {
	// The acting identity, from the verified session cookie. This is the only
	// place a username enters this handler.
	username := s.getAuthUser(r)
	if username == "" {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	user := loadSaaSUser(username)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unknown user — please log in again")
		return
	}

	if r.Method == http.MethodGet {
		writeMyCountry(w, user)
		return
	}

	var body myCountryRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCountryRequestBodyBytes))
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Country == nil {
		// No "country" key at all. Refused rather than read as a clear — see
		// the pointer rationale on myCountryRequest.
		writeJSONError(w, http.StatusBadRequest, "country is required (send \"\" to clear it)")
		return
	}

	raw := strings.TrimSpace(*body.Country)
	code := ""
	if raw != "" {
		// Reuse the SAME validator every other country path uses, so the
		// endpoint cannot drift into accepting a shape the render sites reject
		// (or vice versa). Case-insensitive by way of normalizeCountryCode.
		code = normalizeCountryCode(raw)
		if code == "" {
			writeJSONError(w, http.StatusBadRequest, "country must be an ISO 3166-1 alpha-2 code (two letters), or \"\" to clear it")
			return
		}
	}

	user.Country = code
	// Both a pick and a clear are deliberate, so BOTH set the marker. Setting
	// it on the clear is the load-bearing half: it is what stops the next
	// login's Accept-Language inference from undoing the removal.
	user.CountrySetByUser = true
	if err := saveSaaSUser(user); err != nil {
		s.logger.Warn("handleMyCountry: save failed", "user", username, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "could not save country")
		return
	}
	writeMyCountry(w, user)
}

// writeMyCountry emits the shared response shape for both verbs, so a client
// reads back exactly what a write produced without a second round-trip.
//
// The country is echoed as the CODE, never as the glyph — same rule as the auth
// payload — so the caller derives the flag the same way every other render site
// does, and an empty value is unambiguously "no country" rather than an
// empty-looking emoji. `explicit` lets the UI distinguish a value the user
// chose from one the hub guessed, which is the difference between showing
// "United Kingdom" and "United Kingdom (guessed from your browser)".
func writeMyCountry(w http.ResponseWriter, user *SaaSUser) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"country":  normalizeCountryCode(user.Country),
		"explicit": user.CountrySetByUser,
	})
}
