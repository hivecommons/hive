package hub

import (
	"strings"
	"testing"
)

// #5925 — dibs moves to dibs.hivecommons.dev, with dibs.kubestellar.io demoted
// to a redirect.
//
// That issue reads as a branding rename. It is not: it is the repair for a
// broken identity bridge, and these tests pin the invariant that makes it so.
//
// Dibs has no login of its own (#4171). It forwards the browser's hive_hub_user
// cookie to GET /api/saas/whoami server-to-server, which works ONLY because the
// browser sends that cookie to dibs at all — and the browser does that solely
// because of the cookie's Domain attribute, which sessionCookieDomain derives
// from the registrable domain of the hub's own canonical host.
//
// So the bridge has a precondition nothing in the tree stated until now:
//
//	the hub and the sibling product must share a registrable domain.
//
// When the hub became hive.hivecommons.dev, the session cookie became
// Domain=.hivecommons.dev and dibs.kubestellar.io stopped receiving it. Moving
// dibs under hivecommons.dev is what restores the precondition.
//
// The mirror case is pinned too, because the invariant is symmetric: a future
// move in either direction has the same failure mode.

// cookieReaches reports whether a browser holding a hive_hub_user cookie that
// was set on setHost with Domain=cookieDomain would send it to host.
//
// This is RFC 6265 §5.1.3 domain-matching, spelled out rather than asserted on
// the raw Domain string, because the string is not the thing that matters — what
// every case below is really asking is "does the sibling see the session". An
// empty cookieDomain is a host-only cookie, which reaches only the exact host
// that set it.
func cookieReaches(cookieDomain, setHost, host string) bool {
	if cookieDomain == "" {
		return host == setHost
	}
	d := strings.TrimPrefix(cookieDomain, ".")
	return host == d || strings.HasSuffix(host, "."+d)
}

// TestSessionCookieReachesSiblingOnlyOnSharedRegistrableDomain is the core of
// #5925: the sibling bridge lives or dies on the hub and the sibling sharing a
// registrable domain, and it fails CLOSED and SILENTLY — dibs simply renders
// every visitor as signed out.
func TestSessionCookieReachesSiblingOnlyOnSharedRegistrableDomain(t *testing.T) {
	const (
		dibsNew = "dibs.hivecommons.dev"
		dibsOld = "dibs.kubestellar.io"
	)
	cases := []struct {
		name       string
		hubURL     string // "" leaves HIVE_HUB_PUBLIC_URL unset (historical default)
		hubHost    string
		wantDomain string
		reaches    map[string]bool
	}{
		{
			// Where the fleet sits after the hub moved on 2026-09-04 and before
			// #5925 lands: dibs is stranded on the old registrable domain.
			name:       "hub on hivecommons.dev, dibs still on kubestellar.io",
			hubURL:     "https://hive.hivecommons.dev",
			hubHost:    "hive.hivecommons.dev",
			wantDomain: ".hivecommons.dev",
			reaches: map[string]bool{
				dibsNew: true,
				dibsOld: false,
			},
		},
		{
			// The historical arrangement #4171 was designed for, kept as the
			// mirror case: the invariant is about SHARING a registrable domain,
			// not about which one.
			name:       "hub on kubestellar.io, dibs on kubestellar.io",
			hubURL:     "",
			hubHost:    "hive.kubestellar.io",
			wantDomain: ".kubestellar.io",
			reaches: map[string]bool{
				dibsNew: false,
				dibsOld: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_HUB_PUBLIC_URL", tc.hubURL)
			t.Setenv("HIVE_HUB_SPOKE_DOMAIN", "")

			got := sessionCookieDomain(tc.hubHost)
			if got != tc.wantDomain {
				t.Fatalf("sessionCookieDomain(%q) = %q, want %q", tc.hubHost, got, tc.wantDomain)
			}
			for sibling, want := range tc.reaches {
				if reaches := cookieReaches(got, tc.hubHost, sibling); reaches != want {
					t.Errorf("session cookie (Domain=%q) reaches %s = %v, want %v — the dibs SSO "+
						"bridge (#4171) can resolve a session only for a sibling the browser actually "+
						"sends hive_hub_user to", got, sibling, reaches, want)
				}
			}
		})
	}
}

// TestNoCookieScopeKeepsSiblingOnForeignRegistrableDomain is the finding that
// makes the redirect in #5925 step 5 a REQUIREMENT rather than a tidy-up.
//
// The tempting cheaper option, once dibs.hivecommons.dev works, is to leave
// dibs.kubestellar.io serving the same application instead of redirecting —
// "both hosts answer, nothing left to do". It cannot be made to work, and no
// amount of configuration rescues it: a browser ignores a Set-Cookie whose
// Domain does not cover the host that sent it (RFC 6265 §5.3), so a response
// served by the hub on hivecommons.dev can never scope a cookie to
// kubestellar.io. There is no second Domain to configure, which is why
// sessionCookieDomain falls back to a host-only cookie for any host outside the
// hub's own parent instead of trying.
//
// A redirect works precisely BECAUSE it is not dual-serving: the browser ends up
// issuing its request to the .dev host, which is inside the cookie's scope.
func TestNoCookieScopeKeepsSiblingOnForeignRegistrableDomain(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hive.hivecommons.dev")
	t.Setenv("HIVE_HUB_SPOKE_DOMAIN", "")

	// The scope collapses to host-only for a foreign host: it reaches that one
	// host and nothing beneath it, which is no use to a sibling bridge.
	foreign := sessionCookieDomain("dibs.kubestellar.io")
	if foreign != "" {
		t.Fatalf("sessionCookieDomain(foreign host) = %q, want \"\" (host-only) — a browser ignores "+
			"a Set-Cookie whose Domain does not cover the sending host, so emitting one would be a "+
			"cookie no browser stores", foreign)
	}
	if !cookieReaches(foreign, "dibs.kubestellar.io", "dibs.kubestellar.io") {
		t.Error("host-only cookie does not reach its own host — the fallback would be inert")
	}
	if cookieReaches(foreign, "dibs.kubestellar.io", "hive.hivecommons.dev") {
		t.Error("host-only cookie leaked beyond its own host")
	}
}

// TestSiblingMoveKeepsSpokesOnTheSessionCookie guards the blast radius. The
// session cookie is load-bearing for every hosted tenant: each spoke's Node
// proxy (src/proxy/server.js) independently verifies it and can only verify a
// cookie the browser sends it. Whatever #5925 does on the sibling side must
// leave that path intact — see the AUDIT F4 note in oauth.go.
func TestSiblingMoveKeepsSpokesOnTheSessionCookie(t *testing.T) {
	cases := []struct {
		name      string
		hubURL    string
		hubHost   string
		spokeHost string
	}{
		{
			name:      "hivecommons hub keeps hivecommons spokes",
			hubURL:    "https://hive.hivecommons.dev",
			hubHost:   "hive.hivecommons.dev",
			spokeHost: "hosted-acme-web-ab12.hive.hivecommons.dev",
		},
		{
			name:      "default hub keeps kubestellar spokes",
			hubURL:    "",
			hubHost:   "hive.kubestellar.io",
			spokeHost: "hosted-acme-web-ab12.hive.kubestellar.io",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_HUB_PUBLIC_URL", tc.hubURL)
			t.Setenv("HIVE_HUB_SPOKE_DOMAIN", "")

			domain := sessionCookieDomain(tc.hubHost)
			if domain == "" {
				t.Fatalf("session cookie went host-only for hub %q — every hosted tenant's dashboard "+
					"and terminal would log out fleet-wide", tc.hubHost)
			}
			if !cookieReaches(domain, tc.hubHost, tc.spokeHost) {
				t.Fatalf("session cookie (Domain=%q) does not reach spoke %q", domain, tc.spokeHost)
			}
		})
	}
}
