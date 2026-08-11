package dashboard

import (
	"context"
	"fmt"
	"net/http"
)

const ssrfMaxRedirects = 3

// noRedirectToPrivate returns an http.Client CheckRedirect function that
// rejects any redirect whose target resolves to a private/internal host.
// It also limits the redirect chain depth to ssrfMaxRedirects. Use this
// wherever the hive fetches a URL supplied (directly or indirectly) by a
// user, to prevent SSRF via open-redirect chains:
//
//	client := &http.Client{
//	    Timeout:       30 * time.Second,
//	    CheckRedirect: noRedirectToPrivate,
//	}
func noRedirectToPrivate(req *http.Request, via []*http.Request) error {
	if len(via) >= ssrfMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", ssrfMaxRedirects)
	}
	// Re-run the private-host check on every redirect target so a public
	// URL that returns 302 → internal address cannot bypass the preflight.
	if isPrivateURL(context.Background(), req.URL.String()) {
		return fmt.Errorf("redirect to private/internal host blocked: %s", req.URL.Host)
	}
	return nil
}
