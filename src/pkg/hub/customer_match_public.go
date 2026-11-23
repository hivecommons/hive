package hub

// Labeled public fields in the customer match answer (#19306, the 2026-11-19
// decision change in src/docs/design/customers-hub-consent.md).
//
// Country and organization are public information. For a hive's grant
// holders the hub may answer them without a per-person consent record, each
// labeled with where the value came from. Real name still needs consent;
// location, notes, email and Slack ids never cross. An organization derived
// from an email domain crosses only for a domain the operator listed as an
// organization's (HIVE_HUB_CUSTOMERS_ORG_DOMAINS); a free-mail, personal or
// unlisted domain is dropped. The email address and the domain never cross.
//
// Labels are sent only to a hive that asks for them (customerMatchLabelsHeader),
// so an older hive, which would store every value as consented, keeps the
// consent-only answer.

import (
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/customers"
)

const (
	// customerMatchLabelsHeader is how a hive asks for labeled public fields.
	// A header, not a body field: the request body rejects unknown fields, so
	// a new hive asking an older hub would fail outright.
	customerMatchLabelsHeader = "X-Hive-Customer-Match-Labels"
	customerMatchLabelsV1     = "1"

	// customersOrgDomainsEnv lists organization email domains and the name
	// each maps to: "ibm.com=IBM,redhat.com=Red Hat". Empty (the default)
	// means no organization is ever derived from an email domain.
	customersOrgDomainsEnv = "HIVE_HUB_CUSTOMERS_ORG_DOMAINS"
	// maxCustomersOrgDomains bounds the parsed list.
	maxCustomersOrgDomains = 1000
	// maxOrgDomainLen is the longest domain name accepted (RFC 1035).
	maxOrgDomainLen = 253
)

// Source labels on the wire, one per public field in an answer. They say who
// put the value on the hub record, which is what the hive's precedence reads.
const (
	// customerMatchSourceConsent: entered by the person, shared with consent.
	// The only label real_name ever carries.
	customerMatchSourceConsent = "consent"
	// customerMatchSourcePerson: entered by the person on the hub.
	customerMatchSourcePerson = "person"
	// customerMatchSourceAdmin: set by a hub admin on the person's behalf.
	customerMatchSourceAdmin = "admin"
	// customerMatchSourceEmailDomain: derived from a listed organization
	// email domain.
	customerMatchSourceEmailDomain = "email_domain"
	// customerMatchSourceBrowserLanguage: a country inferred at login from
	// the browser's Accept-Language region.
	customerMatchSourceBrowserLanguage = "browser_language"
	// customerMatchSourceUnrecorded: a legacy value saved before the hub
	// recorded who supplied it.
	customerMatchSourceUnrecorded = "unrecorded"
)

func customerMatchWantsLabels(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(customerMatchLabelsHeader)) == customerMatchLabelsV1
}

// parseCustomerOrgDomains reads the organization-domain list. An entry
// without a name, a name-less or dot-less domain, and a free-mail or
// code-host domain are dropped: those name no organization.
func parseCustomerOrgDomains(raw string) map[string]string {
	out := make(map[string]string)
	for _, item := range strings.Split(raw, ",") {
		domain, org, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		domain = strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
		org = truncateRunes(strings.TrimSpace(org), maxContactCompanyLen)
		if domain == "" || org == "" || len(domain) > maxOrgDomainLen ||
			!strings.Contains(domain, ".") || strings.ContainsAny(domain, "@/ ") ||
			customers.IsFreeMailDomain(domain) {
			continue
		}
		if len(out) >= maxCustomersOrgDomains {
			break
		}
		out[domain] = org
	}
	return out
}

func customerOrgDomains() map[string]string {
	return parseCustomerOrgDomains(os.Getenv(customersOrgDomainsEnv))
}

// orgFromEmailDomain maps an email to a listed organization, or "". A
// subdomain matches its listed parent ("us.ibm.com" -> "ibm.com").
func orgFromEmailDomain(email string, domains map[string]string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || len(domains) == 0 {
		return ""
	}
	domain := strings.ToLower(strings.Trim(strings.TrimSpace(email[at+1:]), "."))
	if domain == "" || customers.IsFreeMailDomain(domain) {
		return ""
	}
	for d := domain; strings.Contains(d, "."); {
		if org, ok := domains[d]; ok {
			return org
		}
		_, rest, _ := strings.Cut(d, ".")
		d = rest
	}
	return ""
}

// customerPublicOrganization is the organization the hub holds or derives for
// u and its label. A value someone set wins; an explicit clear (a source with
// no value) stops derivation, so a person who removed their organization is
// not given one from their email.
func customerPublicOrganization(u *SaaSUser, domains map[string]string) (string, string) {
	if v := truncateRunes(strings.TrimSpace(u.Company), maxContactCompanyLen); v != "" {
		switch u.CompanySource {
		case countrySourceUser:
			return v, customerMatchSourcePerson
		case countrySourceAdmin:
			return v, customerMatchSourceAdmin
		}
		return v, customerMatchSourceUnrecorded
	}
	if u.CompanySource != "" {
		return "", ""
	}
	if v := orgFromEmailDomain(u.Email, domains); v != "" {
		return v, customerMatchSourceEmailDomain
	}
	return "", ""
}

// customerPublicCountry is the country the hub holds for u and its label.
func customerPublicCountry(u *SaaSUser) (string, string) {
	code := normalizeCountryCode(u.Country)
	if code == "" {
		return "", ""
	}
	switch effectiveCountrySource(u) {
	case countrySourceUser:
		return code, customerMatchSourcePerson
	case countrySourceAdmin:
		return code, customerMatchSourceAdmin
	case countrySourceInferred:
		return code, customerMatchSourceBrowserLanguage
	}
	return code, customerMatchSourceUnrecorded
}

// customerMatchPublic adds the labeled public fields to an answer entry for a
// current grant holder of hiveID. It never adds real_name, location, notes,
// email or Slack ids, and it labels a consented real_name as such. It returns
// the public fields it set.
func customerMatchPublic(e customerMatchEntry, u *SaaSUser, hiveID string, domains map[string]string, consented customerConsentFields) (customerMatchEntry, customerConsentFields) {
	var public customerConsentFields
	if u == nil || u.Blocked || u.Hives[hiveID] == "" {
		return e, public
	}
	sources := make(map[string]string)
	if consented.RealName && e.RealName != "" {
		sources["real_name"] = customerMatchSourceConsent
	}
	if v, src := customerPublicOrganization(u, domains); v != "" {
		e.Organization, sources["organization"], public.Organization = v, src, true
	}
	if v, src := customerPublicCountry(u); v != "" {
		e.Country, sources["country"], public.Country = v, src, true
	}
	if len(sources) > 0 {
		e.Sources = sources
	}
	return e, public
}
