package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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
	stripAuthOnCrossHostRedirect(req, via)
	return nil
}

// stripAuthOnCrossHostRedirect removes credential headers from a redirect hop
// that changes host (audit F13).
//
// Blocking private redirect targets is not sufficient on its own: Go PRESERVES
// the Authorization header across hops it considers same-host, and a redirect
// from one PUBLIC host to another public host it does not. Without this, an
// upstream answering /v1/models with "302 → attacker.example" walks the gateway
// API key straight to the attacker — the credential leaves with the request
// before any response is inspected.
//
// Comparing Host (not the full URL) keeps legitimate same-host redirects —
// http→https upgrades, path normalization, trailing-slash fixes — working with
// credentials intact, which is what upstreams actually do.
func stripAuthOnCrossHostRedirect(req *http.Request, via []*http.Request) {
	if len(via) == 0 {
		return
	}
	if strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return
	}
	for _, h := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-IBM-Project-ID"} {
		req.Header.Del(h)
	}
}
