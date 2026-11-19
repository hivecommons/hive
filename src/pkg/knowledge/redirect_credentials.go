package knowledge

import (
	"net/http"
	"strings"
)

// redirectCredentialHeaders are removed from a redirect hop that leaves the
// original host or downgrades https to http. The set matches the dashboard's
// stripAuthOnCrossHostRedirect so every CheckRedirect in src/pkg strips the
// same credentials (hivecommons/hive#19222).
var redirectCredentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "X-IBM-Project-ID"}

// StripCredentialsOnCrossHostRedirect removes credential headers from a
// redirect hop whose Host (port included, case-insensitive) differs from the
// first request's, or that downgrades https to http.
//
// net/http alone keeps Authorization on a hop to the same domain on another
// port or to a subdomain, and keeps it across an https -> http downgrade, so a
// connector's bearer token (Confluence, Discord, Granola, Google Meet,
// Context7) could follow an upstream's redirect off the host it was issued
// for. Same-origin redirects keep their credentials.
func StripCredentialsOnCrossHostRedirect(req *http.Request, via []*http.Request) {
	if len(via) == 0 || via[0] == nil || via[0].URL == nil || req.URL == nil {
		return
	}
	first := via[0].URL
	downgrade := strings.EqualFold(first.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https")
	if !downgrade && strings.EqualFold(req.URL.Host, first.Host) {
		return
	}
	for _, h := range redirectCredentialHeaders {
		req.Header.Del(h)
	}
}

// knowledgeRedirectPolicy is knowledgeNoRedirectToPrivate plus credential
// stripping: the policy for every knowledge client that sends credentials.
func knowledgeRedirectPolicy(req *http.Request, via []*http.Request) error {
	if err := knowledgeNoRedirectToPrivate(req, via); err != nil {
		return err
	}
	StripCredentialsOnCrossHostRedirect(req, via)
	return nil
}
