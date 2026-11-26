package hub

// Mapping between the hub SaaSUser record and the colony store schema
// (hivecommons/hive#19856, the "Hub SaaSUser (P4)" table of
// src/docs/design/colony-customer-data-backend.md).
//
// Only person and fact data moves: name, organization, country, affiliation,
// Slack id, notes, the profile location and bio, and the top-repo cache,
// plus the identities and the customer consent records. Account and access
// data (hives, expiry, quota, tokens, engagement stats) stays in the
// SaaSUser JSON.
//
// Pure functions only. Nothing here opens a file or a store.

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/colony/resolver"
	"github.com/hivecommons/colony/store"
)

// Hub cache fields with no colony-defined name. All of them are local_only.
const (
	crmFieldBio               = "bio"
	crmFieldTopRepo           = "top_repo"
	crmFieldTopRepoURL        = "top_repo_url"
	crmFieldPublicActivity    = "public_activity"
	crmFieldCNCFContributions = "cncf_contributions"
)

// Fact and identity sources the hub records. Each fact source maps back to
// exactly one SaaSUser source string, so a colony read rebuilds the record.
const (
	// crmSourceUser: the person set it on the hub (*Source == "user").
	crmSourceUser = store.HubSourcePrefix + countrySourceUser
	// crmSourceAdmin: a hub admin set it (*Source == "admin").
	crmSourceAdmin = store.HubSourcePrefix + countrySourceAdmin
	// crmSourceUnrecorded: a legacy value saved before the hub recorded who
	// supplied it (*Source == ""). Kept at rank admin: only people (the
	// person or an admin) ever wrote these fields, and admin is the weaker
	// of the two, so a later user or admin write still replaces it, exactly
	// as mayOverwriteProfileSource allows today.
	crmSourceUnrecorded = store.HubSourcePrefix + "unrecorded"
	// crmSourceBrowserLanguage: the login path's Accept-Language guess.
	crmSourceBrowserLanguage = "browser_language"
	// crmSourceGitHubProfile: the forge profile (bio, location).
	crmSourceGitHubProfile = "github_profile"
	// crmSourceTopRepo: the top-repo footprint cache.
	crmSourceTopRepo = store.HubSourcePrefix + "top_repo"
	// crmSourceDevstats: the colony devstats contribution count.
	crmSourceDevstats = store.ColonySourcePrefix + "devstats"

	crmIdentitySourceLogin  = store.HubSourcePrefix + "login"
	crmIdentitySourceLinked = store.HubSourcePrefix + "linked_github"
	crmIdentitySourceEmail  = store.HubSourcePrefix + "provider_email"
)

// crmMaxFactValueBytes is the colony store's per-value cap. A longer value
// (only admin notes can be, at maxContactNotesLen runes) stays JSON-only:
// it is never truncated into colony, because a colony read would then put
// the truncated text back on the record.
const crmMaxFactValueBytes = 4096

// crmMaxIdentityBytes is the colony store's per-identity cap.
const crmMaxIdentityBytes = 512

// crmStorableText reports whether colony accepts s as a value: valid UTF-8,
// within max bytes, and no control character other than newline and tab. A
// value it would refuse (a long note, a pasted carriage return) stays
// JSON-only rather than failing the import transaction; a colony read then
// keeps the JSON value, because colony holds no fact for that field.
func crmStorableText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

// crmRankFromSource maps a SaaSUser *Source value for a person-entered field
// (FullName, Company) to its rank and fact source.
func crmRankFromSource(source string) (store.Rank, string) {
	switch source {
	case countrySourceUser:
		return store.RankUser, crmSourceUser
	case countrySourceAdmin:
		return store.RankAdmin, crmSourceAdmin
	default:
		return store.RankAdmin, crmSourceUnrecorded
	}
}

// crmSourceToSaaS is the inverse of crmRankFromSource for FullName/Company.
func crmSourceToSaaS(factSource string) string {
	switch factSource {
	case crmSourceUser:
		return countrySourceUser
	case crmSourceAdmin:
		return countrySourceAdmin
	default:
		return ""
	}
}

// crmAffiliationFactSource maps a SaaSUser AffiliationSource to a fact
// source: a colony:<provider> source is kept, a hub-derived one is prefixed
// ("hub:company_field"), and none is recorded as unrecorded.
func crmAffiliationFactSource(source string) string {
	source = strings.TrimSpace(source)
	switch {
	case source == "":
		return crmSourceUnrecorded
	case strings.HasPrefix(source, store.ColonySourcePrefix):
		return source
	default:
		return store.HubSourcePrefix + source
	}
}

// crmAffiliationSaaSSource is the inverse of crmAffiliationFactSource.
func crmAffiliationSaaSSource(factSource string) string {
	switch {
	case factSource == crmSourceUnrecorded:
		return ""
	case strings.HasPrefix(factSource, store.ColonySourcePrefix):
		return factSource
	default:
		return strings.TrimPrefix(factSource, store.HubSourcePrefix)
	}
}

// crmAffiliationShareable reports whether a SaaSUser affiliation of user u
// is derived from public github.com data only. The identity-provider and
// GHE-host fallbacks come from how a person signs in, and a GHE profile is
// not public, so those stay on the hub.
func crmAffiliationShareable(u *SaaSUser) bool {
	if u == nil || userProvider(u) != legacyProvider {
		return false
	}
	switch src := strings.TrimSpace(u.AffiliationSource); src {
	case affiliationSourceCompany, affiliationSourceOrgMembership, affiliationSourceContributions:
		return true
	default:
		return strings.HasPrefix(src, store.ColonySourcePrefix)
	}
}

// crmConfidence keeps only a confidence the colony schema knows.
func crmConfidence(c string) resolver.Confidence {
	switch conf := resolver.Confidence(strings.TrimSpace(c)); conf {
	case resolver.ConfidenceHigh, resolver.ConfidenceMedium, resolver.ConfidenceLow:
		return conf
	}
	return ""
}

// crmFactsFromUser derives the fact set the colony store must hold for u:
// one fact per field, at the rank the JSON source names. The JSON already
// applies the precedence (mayOverwriteProfileSource, mayOverwriteCountry),
// so its single value per field is the effective one.
//
// A person-entered field with a source and no value is an explicit clear
// and is kept; a field with neither is absent.
func crmFactsFromUser(u *SaaSUser) []store.Fact {
	if u == nil {
		return nil
	}
	var out []store.Fact
	add := func(f store.Fact) {
		if crmStorableText(f.Value, crmMaxFactValueBytes) {
			out = append(out, f)
		}
	}

	if v := strings.TrimSpace(u.FullName); v != "" || u.FullNameSource != "" {
		rank, src := crmRankFromSource(u.FullNameSource)
		add(store.Fact{Field: store.FieldRealName, Value: v, Rank: rank, Source: src, Locality: store.LocalityShareable})
	}
	if v := strings.TrimSpace(u.Company); v != "" || u.CompanySource != "" {
		rank, src := crmRankFromSource(u.CompanySource)
		add(store.Fact{Field: store.FieldOrganization, Value: v, Rank: rank, Source: src, Locality: store.LocalityShareable})
	}
	switch effectiveCountrySource(u) {
	case countrySourceUser:
		add(store.Fact{Field: store.FieldCountry, Value: u.Country, Rank: store.RankUser, Source: crmSourceUser, Locality: store.LocalityShareable})
	case countrySourceAdmin:
		add(store.Fact{Field: store.FieldCountry, Value: u.Country, Rank: store.RankAdmin, Source: crmSourceAdmin, Locality: store.LocalityShareable})
	case countrySourceInferred:
		add(store.Fact{Field: store.FieldCountry, Value: u.Country, Rank: store.RankEnrichment, Source: crmSourceBrowserLanguage, Locality: store.LocalityShareable})
	}
	if v := strings.TrimSpace(u.Affiliation); v != "" {
		loc := store.LocalityLocalOnly
		if crmAffiliationShareable(u) {
			loc = store.LocalityShareable
		}
		add(store.Fact{Field: store.FieldAffiliation, Value: v, Rank: store.RankEnrichment,
			Source: crmAffiliationFactSource(u.AffiliationSource), Confidence: crmConfidence(u.AffiliationConfidence), Locality: loc})
	}
	// Admin CRM notes and the Slack id never leave the hub (colony also
	// forces both local_only).
	if v := strings.TrimSpace(u.SlackID); v != "" {
		add(store.Fact{Field: store.FieldSlackID, Value: v, Rank: store.RankAdmin, Source: crmSourceAdmin, Locality: store.LocalityLocalOnly})
	}
	if v := strings.TrimSpace(u.Notes); v != "" {
		add(store.Fact{Field: store.FieldNotes, Value: v, Rank: store.RankAdmin, Source: crmSourceAdmin, Locality: store.LocalityLocalOnly})
	}
	// The forge-profile and top-repo cache. Location is consent-first and
	// bio is free text, so both stay local_only; the rest is hub bookkeeping.
	enrich := func(field, value, source string) {
		if v := strings.TrimSpace(value); v != "" {
			add(store.Fact{Field: field, Value: v, Rank: store.RankEnrichment, Source: source, Locality: store.LocalityLocalOnly})
		}
	}
	enrich(store.FieldLocation, u.ProfileLocation, crmSourceGitHubProfile)
	enrich(crmFieldBio, u.ProfileBio, crmSourceGitHubProfile)
	enrich(crmFieldTopRepo, u.TopRepo, crmSourceTopRepo)
	enrich(crmFieldTopRepoURL, u.TopRepoURL, crmSourceTopRepo)
	enrich(crmFieldPublicActivity, u.PublicActivity, crmSourceTopRepo)
	if u.CNCFContributions > 0 {
		enrich(crmFieldCNCFContributions, strconv.Itoa(u.CNCFContributions), crmSourceDevstats)
	}
	return out
}

// crmPersonID is the colony person id for a hub user: the user's canonical
// identity, the same key the JSON file is named by.
func crmPersonID(u *SaaSUser) string {
	return strings.TrimSpace(userCanonicalID(u))
}

// crmPrimaryIdentity is the identity the person signs in with.
func crmPrimaryIdentity(u *SaaSUser) (store.Identity, bool) {
	provider, subject, ok := parseCanonical(userCanonicalID(u))
	if !ok {
		return store.Identity{}, false
	}
	if !crmStorableText(subject, crmMaxIdentityBytes) || strings.ContainsAny(subject, "\n\t") {
		return store.Identity{}, false
	}
	if provider == legacyProvider {
		return store.Identity{Kind: store.KindGitHub, Value: subject, Verified: true,
			Locality: store.LocalityShareable, Source: crmIdentitySourceLogin}, true
	}
	return store.Identity{Kind: store.KindOIDC, Value: provider + canonicalSeparator + subject, Verified: true,
		Locality: store.LocalityLocalOnly, Source: crmIdentitySourceLogin}, true
}

// crmSecondaryIdentities are the other identities on the record. The email
// is recorded local_only whatever its domain: the hub never matches or sends
// an email (colony would force a private-domain one local_only anyway, and
// never exports any email).
func crmSecondaryIdentities(u *SaaSUser) []store.Identity {
	if u == nil {
		return nil
	}
	var out []store.Identity
	storable := func(v string) bool { return crmStorableText(v, crmMaxIdentityBytes) && !strings.ContainsAny(v, "\n\t") }
	if login := strings.TrimSpace(u.LinkedGitHubLogin); login != "" && storable(login) {
		// No hub flow proves a linked login (customerMatchLogin), so it is
		// recorded unverified and local_only.
		out = append(out, store.Identity{Kind: store.KindGitHub, Value: login,
			Locality: store.LocalityLocalOnly, Source: crmIdentitySourceLinked})
	}
	if email := strings.TrimSpace(u.Email); email != "" && strings.Contains(email, "@") && storable(email) {
		out = append(out, store.Identity{Kind: store.KindEmail, Value: email,
			Locality: store.LocalityLocalOnly, Source: crmIdentitySourceEmail})
	}
	return out
}

// crmConsentsFromRecords maps the hub consent file to colony Consent
// records. The hub-only delivery bookkeeping (delivered fields and rev)
// stays in the consent file, which remains what the match answer reads.
func crmConsentsFromRecords(records map[string]customerConsent) []store.Consent {
	out := make([]store.Consent, 0, len(records))
	for hiveID, c := range records {
		sc := store.Consent{Scope: hiveID, Fields: c.Fields.names(), TextVersion: c.TextVersion, GrantedAt: c.GrantedAt.UTC()}
		if c.WithdrawnAt != nil {
			sc.WithdrawnAt = c.WithdrawnAt.UTC()
		}
		if c.LastDeliveredAt != nil {
			sc.DeliveredAt = c.LastDeliveredAt.UTC()
		}
		if sc.Fields == nil {
			sc.Fields = []string{}
		}
		out = append(out, sc)
	}
	return out
}

// crmFactEqual compares what a mirror write would change.
func crmFactEqual(a, b store.Fact) bool {
	return a.Field == b.Field && a.Rank == b.Rank && a.Value == b.Value && a.Source == b.Source &&
		a.Confidence == b.Confidence && a.Locality == b.Locality
}

// crmOverlay writes the effective colony facts of p onto u: the colony-mode
// read. A field whose colony value already matches what the JSON holds is
// left untouched, so an unchanged record still serialises byte for byte. A
// field colony does not hold keeps its JSON value (dual-read).
func crmOverlay(u *SaaSUser, p store.Person) {
	if u == nil {
		return
	}
	want := make(map[string]store.Fact)
	for _, f := range crmFactsFromUser(u) {
		want[f.Field] = f
	}
	differs := func(field string) (store.Fact, bool) {
		f, ok := p.Effective(field)
		if !ok {
			return store.Fact{}, false
		}
		w, have := want[field]
		if have && w.Value == f.Value && w.Source == f.Source && w.Rank == f.Rank && w.Confidence == f.Confidence {
			return store.Fact{}, false
		}
		return f, true
	}
	if f, ok := differs(store.FieldRealName); ok {
		u.FullName, u.FullNameSource = f.Value, crmSourceToSaaS(f.Source)
	}
	if f, ok := differs(store.FieldOrganization); ok {
		u.Company, u.CompanySource = f.Value, crmSourceToSaaS(f.Source)
	}
	if f, ok := differs(store.FieldCountry); ok {
		switch f.Rank {
		case store.RankUser:
			setUserCountry(u, f.Value, countrySourceUser)
		case store.RankAdmin:
			setUserCountry(u, f.Value, countrySourceAdmin)
		default:
			setUserCountry(u, f.Value, countrySourceInferred)
		}
	}
	if f, ok := differs(store.FieldAffiliation); ok {
		u.Affiliation, u.AffiliationSource, u.AffiliationConfidence = f.Value, crmAffiliationSaaSSource(f.Source), string(f.Confidence)
	}
	str := func(field string, dst *string) {
		if f, ok := differs(field); ok {
			*dst = f.Value
		}
	}
	str(store.FieldSlackID, &u.SlackID)
	str(store.FieldNotes, &u.Notes)
	str(store.FieldLocation, &u.ProfileLocation)
	str(crmFieldBio, &u.ProfileBio)
	str(crmFieldTopRepo, &u.TopRepo)
	str(crmFieldTopRepoURL, &u.TopRepoURL)
	str(crmFieldPublicActivity, &u.PublicActivity)
	if f, ok := differs(crmFieldCNCFContributions); ok {
		if n, err := strconv.Atoi(f.Value); err == nil && n >= 0 {
			u.CNCFContributions = n
		}
	}
}
