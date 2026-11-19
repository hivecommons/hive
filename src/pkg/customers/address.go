package customers

import "strings"

// Addresses that are never a person. A calendar invite lists group, room and
// list addresses next to the people, and the hive's own mailbox sits on every
// invite it is sent. None of them is someone to build a profile of, so none
// is suggested.
var (
	// nonPersonDomains name domains, and their subdomains, whose addresses
	// are calendars, rooms or mailing lists (hivecommons/hive#18742).
	nonPersonDomains = []string{
		"calendar.google.com",
		"googlegroups.com",
	}
	// nonPersonLocalParts are mailboxes that are service accounts.
	nonPersonLocalParts = []string{
		"mailer-daemon", "postmaster", "notifications", "notification", "calendar-notification", "nobody",
	}
	// nonPersonLocalPrefixes start the local part of a no-reply or bounce
	// mailbox.
	nonPersonLocalPrefixes = []string{
		"noreply", "no-reply", "donotreply", "do-not-reply", "bounce",
	}
)

// IsNonPersonAddress reports whether an email address is a calendar group, a
// mailing list, a service or no-reply mailbox, or one of own (this hive's
// own addresses, such as hive@<host>). The comparison ignores case.
func IsNonPersonAddress(addr string, own ...string) bool {
	a := strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(a, '@')
	if at <= 0 {
		return false
	}
	local, domain := a[:at], a[at+1:]
	for _, o := range own {
		if o != "" && strings.EqualFold(strings.TrimSpace(o), a) {
			return true
		}
	}
	for _, d := range nonPersonDomains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	for _, l := range nonPersonLocalParts {
		if local == l {
			return true
		}
	}
	for _, p := range nonPersonLocalPrefixes {
		if strings.HasPrefix(local, p) {
			return true
		}
	}
	return false
}

// githubLoginMax is the longest GitHub login.
const githubLoginMax = maxLoginLen

// LooksLikeGitHubLogin reports whether a dashboard user name is shaped like a
// GitHub login: letters, digits and single hyphens, at most 39 characters.
// A name with a provider prefix other than github:, an address, a dot or an
// underscore is not.
func LooksLikeGitHubLogin(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" || len(n) > githubLoginMax || strings.HasPrefix(n, "-") || strings.HasSuffix(n, "-") || strings.Contains(n, "--") {
		return false
	}
	for i := 0; i < len(n); i++ {
		if !isLoginByte(n[i]) {
			return false
		}
	}
	return true
}
