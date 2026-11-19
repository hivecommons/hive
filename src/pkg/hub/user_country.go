package hub

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ── Provenance ───────────────────────────────────────────────────────────────
//
// WHO put the country on the record, which is what decides whether the next
// writer is allowed to replace it. Stored in SaaSUser.CountrySource.
//
// The boolean CountrySetByUser could express only "the user chose this" vs
// "nobody did", and that was sufficient while the only two writers were the
// user and the inference. An ADMIN assignment is a third kind of claim that
// sits between them — a best-effort attribution BY SOMEONE ELSE — and it has to
// be recorded as such, because the two things we need from it pull in opposite
// directions on a boolean:
//
//   - It must not read as user-chosen. CountrySetByUser is a statement about
//     what the USER said; setting it for an admin edit fabricates consent, and
//     it also permanently suppresses the "we don't know, ask them" state, so
//     the user would never again be prompted for a value they never gave.
//   - It must still beat inference, or the admin's work is silently undone at
//     the user's next login from a differently-configured browser. That is the
//     #4374 bug in a new costume: invisible, and discovered a login later.
//
// Hence a small ordered enum. The values are stable strings on disk, so they
// are named constants and never spelled inline.
const (
	// countrySourceInferred — the login path's Accept-Language guess. Weakest:
	// evidence about a browser's language, not about a person.
	countrySourceInferred = "inferred"
	// countrySourceAdmin — a hub admin assigned it on the user's behalf.
	countrySourceAdmin = "admin"
	// countrySourceUser — the user said so themselves (self-service endpoint or
	// the get-started wizard pick). Authoritative; nothing overrides it but
	// another statement from the same user.
	countrySourceUser = "user"
)

// profileProvenanceRank maps a source to its strength. Higher wins.
//
// Ranks rather than a chain of if-statements at each call site, so "user beats
// admin beats inference" is asserted in exactly one place and a future source
// slots in by picking a number instead of by auditing every comparison.
func profileProvenanceRank(source string) int {
	switch source {
	case countrySourceUser:
		return 3
	case countrySourceAdmin:
		return 2
	case countrySourceInferred:
		return 1
	default:
		// Unset — nothing has ever claimed this record. Weakest of all, so any
		// writer may fill it.
		return 0
	}
}

// effectiveCountrySource is the provenance of a record as it should be READ,
// bridging the pre-CountrySource era.
//
// LOAD-BEARING FOR BACKWARD COMPATIBILITY. Every record written before this
// field existed carries only CountrySetByUser, and nothing backfills them — a
// migration sweep would rewrite thousands of files on the PVC to encode
// something derivable for free, and would break the round-trip-byte-identical
// property the whole SaaSUser struct is built around. So the boolean is read as
// the legacy spelling of countrySourceUser, which is precisely what it always
// meant: the user chose this (a pick, or a deliberate clear).
//
// A record with a country and neither marker is a login-path inference from
// before the source field shipped, so it reads as inferred and stays
// overwritable — which matches how applyInferredCountry has always treated it.
// Nil-safe: callers sit on the login path.
func effectiveCountrySource(user *SaaSUser) string {
	if user == nil {
		return ""
	}
	if user.CountrySource != "" {
		return user.CountrySource
	}
	if user.CountrySetByUser {
		return countrySourceUser
	}
	if user.Country != "" {
		return countrySourceInferred
	}
	return ""
}

// mayOverwriteCountry reports whether a writer claiming `source` is allowed to
// replace what is already on the record.
//
// Ties are allowed: a user may always correct their own earlier pick, and an
// admin may always correct an earlier admin assignment. Only a STRICTLY
// stronger existing claim blocks the write — which is the whole precedence rule
// (user > admin > inferred > unset) in one line.
func mayOverwriteCountry(user *SaaSUser, source string) bool {
	return mayOverwriteProfileSource(effectiveCountrySource(user), source)
}

func mayOverwriteProfileSource(current, incoming string) bool {
	return profileProvenanceRank(incoming) >= profileProvenanceRank(current)
}

// setUserCountry is the ONE place a country and its provenance are written
// together, so the two can never drift apart — a country stored without its
// source, or a source stamped without the matching CountrySetByUser, is exactly
// the state the precedence rules cannot reason about.
//
// It also keeps the legacy boolean honest: CountrySetByUser tracks
// "source == user" in BOTH directions. Setting it for a user write keeps every
// existing reader of that field correct; CLEARING it when an admin overwrites a
// user's value is equally load-bearing, since leaving it true would leave a
// record claiming the user chose a value an admin typed.
//
// `code` is stored as given and must already be normalized by the caller (each
// one validates and reports its own 400), so this cannot become a second,
// divergent validator.
func setUserCountry(user *SaaSUser, code, source string) {
	if user == nil {
		return
	}
	user.Country = code
	user.CountrySource = source
	user.CountrySetByUser = source == countrySourceUser
}

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
// The provenance half of the guard covers the case `Country != ""` alone
// cannot: an explicit CLEAR. A user who deliberately removes their country
// leaves an empty field that is indistinguishable, by value, from "never asked"
// — so without a marker the next login would quietly put a flag back and
// "prefer not to say" would be unexpressible. Absence of a value is not the
// same as absence of a decision.
//
// The guard is expressed through mayOverwriteCountry rather than by reading
// CountrySetByUser directly, which is what extends it to the ADMIN case: an
// admin-assigned country is empty-or-not just like a user's, and must survive
// this pass for the same reason. It STACKS with the `Country != ""` test rather
// than replacing it: a record that already carries a value is left alone
// whatever its provenance, so an earlier inference is never re-guessed on a
// later login from a differently-configured browser.
//
// Returns whether it changed the record, so the caller can tell a real update
// from a no-op. Nil-safe: it sits on the login path beside other enrichment.
func applyInferredCountry(user *SaaSUser, r *http.Request) bool {
	if user == nil || user.Country != "" || !mayOverwriteCountry(user, countrySourceInferred) {
		return false
	}
	code := inferCountryFromRequest(r)
	if code == "" {
		return false
	}
	setUserCountry(user, code, countrySourceInferred)
	return true
}

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
	if r.Method == http.MethodGet {
		user := loadSaaSUser(username)
		if user == nil {
			writeJSONError(w, http.StatusUnauthorized, "unknown user — please log in again")
			return
		}
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

	customerProfileMu.Lock()
	defer customerProfileMu.Unlock()
	user := loadSaaSUser(username)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unknown user — please log in again")
		return
	}

	// Both a pick and a clear are deliberate, so BOTH stamp countrySourceUser
	// (and, through setUserCountry, the legacy CountrySetByUser). Stamping it on
	// the clear is the load-bearing half: it is what stops the next login's
	// Accept-Language inference from undoing the removal.
	//
	// The user's own statement is the strongest source there is, so this write
	// is unconditional — it overrides an admin's assignment as well as an
	// inference. An admin attributing a country on someone's behalf is a
	// best-effort guess about that person; the person themselves is not
	// guessing, and must always be able to correct the record about themselves.
	setUserCountry(user, code, countrySourceUser)
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
