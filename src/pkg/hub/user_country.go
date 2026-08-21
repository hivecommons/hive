package hub

import (
	"net/http"
	"strings"
)

// User country: an OPTIONAL ISO 3166-1 alpha-2 code on a hub user record, shown
// as a small flag beside their avatar.
//
// Two sources, in strict priority order:
//
//  1. EXPLICIT — the requester picks a country in the get-started wizard
//     (ProvisionRequest.Country), which is copied onto the SaaSUser record on
//     approval alongside the other contact fields. This is the authoritative
//     source: the user chose it about themselves.
//  2. INFERRED — best-effort, from the region subtag of the browser's
//     Accept-Language header on a completed login. Only ever fills a record
//     that has NO country yet, and never overwrites an explicit choice.
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
// when the record has none.
//
// The guard is the whole function. An explicit pick in the wizard is the user
// speaking about themselves; a language header is a hint. Once the record
// carries a country, no amount of logging in from a differently-configured
// browser may overwrite it — otherwise a user's stated country would silently
// flip when they travel or borrow a laptop, which is both wrong and invisible.
//
// Returns whether it changed the record, so the caller can tell a real update
// from a no-op. Nil-safe: it sits on the login path beside other enrichment.
func applyInferredCountry(user *SaaSUser, r *http.Request) bool {
	if user == nil || user.Country != "" {
		return false
	}
	code := inferCountryFromRequest(r)
	if code == "" {
		return false
	}
	user.Country = code
	return true
}
