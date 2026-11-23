package customers

import "strings"

// IsFreeMailDomain reports whether domain is a free-mail or code-host
// provider, which names no organization. It is the same list the likely
// affiliation suggestion uses (affiliation.go), shared so the hub can refuse
// to map such a domain to an organization (#19306).
func IsFreeMailDomain(domain string) bool {
	domain = strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
	return freeMailDomains[domain]
}
