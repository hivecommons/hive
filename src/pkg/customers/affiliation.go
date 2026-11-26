package customers

import (
	"strings"
	"time"
	"unicode"
)

// A likely affiliation is a suggestion about a person's organization, never a
// stored fact. SuggestAffiliation is pure and offline: it reads only what the
// profile already holds and makes no request. Accepting a suggestion writes it
// as a by-hand organization; dismissing it remembers the refusal.
//
// Rules, in order, for a profile whose organization a person has not set or
// cleared by hand:
//
//  1. company: the organization filled from the person's GitHub company text,
//     tidied (legal suffix and "@" dropped, a bare lower-case handle given
//     capitals). Offered only when the tidied form differs from what is shown.
//  2. email domain: the domain of an email identity a person confirmed by
//     hand, when the organization is still empty and the domain is not a
//     free-mail or code-host provider.
//
// Public GitHub organization membership is not used: it is not fetched today,
// and reading it would be a new network call.

// Affiliation suggestion evidence kinds.
const (
	AffiliationFromCompany = "github_company"
	AffiliationFromEmail   = "email_domain"
	// AffiliationFromGitdm is a current affiliation in the public cncf/gitdm
	// files (customers.enrichment.gitdm, on when enrichment is enabled). It outranks the
	// two offline rules because gitdm is curated, and like them it is only
	// ever a suggestion.
	AffiliationFromGitdm = "gitdm"
)

// AffiliationSuggestion is one suggested organization with where it came from.
// Evidence is human-readable and never includes the email local part. Source,
// From and Until are set for a suggestion that came from enrichment, so the
// UI can show the source and the dates.
type AffiliationSuggestion struct {
	Value    string `json:"value"`
	Rule     string `json:"rule"`
	Evidence string `json:"evidence"`
	Source   string `json:"source,omitempty"`
	From     string `json:"from,omitempty"`
	Until    string `json:"until,omitempty"`
}

// shortOrgLen is the longest bare handle shown in capitals ("ibm" -> "IBM").
const shortOrgLen = 3

var legalSuffixes = map[string]bool{
	"inc": true, "llc": true, "ltd": true, "corp": true, "corporation": true,
	"gmbh": true, "co": true, "company": true, "ag": true, "plc": true,
	"limited": true, "incorporated": true, "bv": true, "oy": true, "ab": true,
}

// freeMailDomains name no organization: a person's mailbox provider says
// nothing about where they work.
var freeMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true,
	"live.com": true, "msn.com": true, "yahoo.com": true, "icloud.com": true, "me.com": true,
	"proton.me": true, "protonmail.com": true, "aol.com": true, "gmx.com": true, "gmx.de": true,
	"qq.com": true, "163.com": true, "126.com": true, "yandex.com": true, "mail.com": true,
	"fastmail.com": true, "hey.com": true, "pm.me": true, "github.com": true,
	"users.noreply.github.com": true, "noreply.github.com": true,
}

// secondLevelLabels are the generic labels that precede a country code in a
// registrable domain ("example.co.uk").
var secondLevelLabels = map[string]bool{"co": true, "com": true, "org": true, "net": true, "ac": true, "gov": true, "edu": true}

// TidyCompany makes a GitHub company string presentable: it drops "@", a
// trailing legal suffix and trailing punctuation, and gives a lower-case
// single word capitals (up to three letters) or a leading capital.
func TidyCompany(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "@")
	words := strings.Fields(s)
	for len(words) > 1 {
		last := strings.ToLower(strings.Trim(words[len(words)-1], ".,"))
		if !legalSuffixes[last] {
			break
		}
		words = words[:len(words)-1]
	}
	out := strings.TrimRight(strings.Join(words, " "), ".,;")
	if len(words) == 1 && out == strings.ToLower(out) {
		return capitalizeOrg(out)
	}
	return out
}

func capitalizeOrg(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= shortOrgLen {
		return strings.ToUpper(s)
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// orgFromDomain names the organization a mail domain implies, or "" for a
// free-mail or code-host domain.
func orgFromDomain(domain string) string {
	domain = strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
	if domain == "" || freeMailDomains[domain] || !strings.Contains(domain, ".") {
		return ""
	}
	labels := strings.Split(domain, ".")
	n := len(labels)
	idx := n - 2
	if n >= 3 && len(labels[n-1]) == isoCodeLen && secondLevelLabels[labels[n-2]] {
		idx = n - 3
	}
	if labels[idx] == "" {
		return ""
	}
	return capitalizeOrg(labels[idx])
}

func affiliationDismissed(pr Profile, key string) bool {
	for _, d := range pr.DismissedAffiliations {
		if d == key {
			return true
		}
	}
	return false
}

// SuggestAffiliationWith is SuggestAffiliation with enrichment facts as one
// more source. A current gitdm affiliation is offered first when it differs
// from what is shown; a value a person set by hand or shared from the hub is
// never challenged, and a dismissed one is not offered again. With no
// enrichment it is exactly SuggestAffiliation.
func SuggestAffiliationWith(p Person, enriched []EnrichedAffiliation, now time.Time) *AffiliationSuggestion {
	org := p.Profile.Organization
	if org.Source == SourceManual || org.Source == SourceHub {
		return nil
	}
	var gitdm []EnrichedAffiliation
	for _, a := range enriched {
		if a.Source == EnrichSourceGitdm {
			gitdm = append(gitdm, a)
		}
	}
	if cur, ok := currentEnrichedAffiliation(gitdm, now); ok {
		v := cur.Org
		shown := strings.EqualFold(strings.TrimSpace(org.Value), v) || strings.EqualFold(TidyCompany(org.Value), v)
		if !shown && v != "" && len(v) <= MaxProfileFieldLen && !affiliationDismissed(p.Profile, strings.ToLower(v)) {
			evidence := "from cncf/gitdm"
			if cur.From != "" {
				evidence += ", since " + cur.From
			}
			return &AffiliationSuggestion{Value: v, Rule: AffiliationFromGitdm, Evidence: evidence,
				Source: EnrichSourceGitdm, From: cur.From, Until: cur.Until}
		}
	}
	return SuggestAffiliation(p)
}

// SuggestAffiliation returns the likely organization for p, or nil when a
// person has settled the field, nothing supports one, or the person refused it.
func SuggestAffiliation(p Person) *AffiliationSuggestion {
	org := p.Profile.Organization
	if org.Source == SourceManual || org.Source == SourceHub {
		return nil
	}
	ok := func(v string) bool {
		return v != "" && len(v) <= MaxProfileFieldLen && !affiliationDismissed(p.Profile, strings.ToLower(v))
	}
	if org.Value != "" && IsForgeProfileRule(org.Rule) {
		if v := TidyCompany(org.Value); v != org.Value && ok(v) {
			return &AffiliationSuggestion{Value: v, Rule: AffiliationFromCompany,
				Evidence: "from " + ProfileSourceLabel(org.Rule) + " company field \"" + org.Value + "\""}
		}
		return nil
	}
	if org.Value != "" {
		return nil
	}
	for _, id := range p.Identities {
		if id.Kind != KindEmail || !id.Manual || id.Claimed {
			continue
		}
		_, domain, found := strings.Cut(id.Value, "@")
		if !found {
			continue
		}
		if v := orgFromDomain(domain); ok(v) {
			return &AffiliationSuggestion{Value: v, Rule: AffiliationFromEmail,
				Evidence: "from email domain " + strings.ToLower(domain)}
		}
	}
	return nil
}

// AffiliationAutoAcceptedGitdm is the Rule an organization carries when the
// gitdm_high auto-accept policy (customers.enrichment.auto_accept) filled it
// from a current gitdm affiliation matched by GitHub login. It is an
// automatic value: a person's edit replaces it, and dismissing it clears it
// and keeps it from being accepted again.
const AffiliationAutoAcceptedGitdm = "gitdm_auto_accepted"

// AutoAcceptCandidate is the gitdm affiliation the gitdm_high policy may
// write into p's organization, if any. Only a current gitdm affiliation
// matched by GitHub login (confidence high) qualifies; an email match never
// does. The field must be empty or hold an earlier auto-accepted value: a
// value a person set or cleared, one shared from the hub, and the company on
// the person's own public forge profile (their own statement, which outranks
// gitdm on the hub too) are never replaced. A dismissed value is not
// accepted again.
func AutoAcceptCandidate(p Person, enriched []EnrichedAffiliation, now time.Time) (EnrichedAffiliation, bool) {
	org := p.Profile.Organization
	switch {
	case org.Source == SourceManual, org.Source == SourceHub:
		return EnrichedAffiliation{}, false
	case org.Value != "" && org.Rule != AffiliationAutoAcceptedGitdm:
		return EnrichedAffiliation{}, false
	}
	var high []EnrichedAffiliation
	for _, a := range enriched {
		if a.Source == EnrichSourceGitdm && a.Confidence == EnrichConfidenceHigh && a.MatchedBy == EnrichMatchedByLogin {
			high = append(high, a)
		}
	}
	cur, ok := currentEnrichedAffiliation(high, now)
	if !ok || cur.Org == "" || len(cur.Org) > MaxProfileFieldLen {
		return EnrichedAffiliation{}, false
	}
	if affiliationDismissed(p.Profile, strings.ToLower(cur.Org)) || org.Value == cur.Org {
		return EnrichedAffiliation{}, false
	}
	return cur, true
}
