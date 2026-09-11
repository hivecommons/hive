package hub

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard builds "<id>.<spoke-domain>" URLs for hosted hives that carry
// no explicit dashboardUrl. It used to build them from a hardcoded hostname,
// which kept pointing at the pre-move domain after HIVE_HUB_SPOKE_DOMAIN was
// repointed — and that is not a cosmetic defect: the retired name is outside
// the fleet wildcard, so the browser refuses the TLS handshake and the tenant
// gets a certificate interstitial rather than their hive.

// No literal tenant hostname may remain in the dashboard JS. Pinned as a
// string check because the failure is invisible to a Go test otherwise: the
// JS is a Go string constant, so a reintroduced literal compiles, passes every
// existing test, and only fails in a browser.
func TestDashboardJSBuildsTenantURLsFromServerSpokeDomain(t *testing.T) {
	for _, literal := range []string{
		"'.hive.kubestellar.io'",
		".hive.kubestellar.io'",
		"'https://' + esc(h.id) + '.hive.",
	} {
		if strings.Contains(dashboardHTML, literal) {
			t.Errorf("dashboard JS still contains a hardcoded tenant hostname (%q).\n"+
				"Build tenant URLs from _hubSpokeDomain (server: hub_spoke_domain) instead — a\n"+
				"hardcoded host survives a domain move and lands tenants on a name the fleet\n"+
				"wildcard does not cover, which fails TLS rather than redirecting.", literal)
		}
	}
	// And the replacement must actually be wired: the global, the assignment
	// from the payload, and at least one use.
	for _, want := range []string{
		"var _hubSpokeDomain = ''",
		"if (data.hub_spoke_domain) _hubSpokeDomain = data.hub_spoke_domain;",
		"'.' + esc(_hubSpokeDomain)",
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboard JS is missing %q — the spoke domain is not wired through", want)
		}
	}
}

// An unknown spoke domain must suppress the link, not fall back to a literal.
// Guarded in the JS by `isHosted && _hubSpokeDomain`; this pins that the guard
// is present at every construction site, since dropping it would silently
// reintroduce a wrong-host URL on the first render before the payload lands.
func TestTenantURLConstructionIsGuardedOnKnownDomain(t *testing.T) {
	guards := strings.Count(dashboardHTML, "_hubSpokeDomain)")
	if guards < 3 {
		t.Errorf("expected every hosted-URL construction site to be guarded on a known spoke domain, found %d", guards)
	}
}

// The legacy cookie domain is the ONE constant that must keep naming the old
// domain: its only job is to expire cookies minted by the previous build.
// Repointing it would make the expiry a no-op and strand those cookies.
func TestLegacyCookieDomainStillNamesTheOldDomain(t *testing.T) {
	if !strings.Contains(defaultLegacyHubCookieDomain, "kubestellar.io") {
		t.Errorf("defaultLegacyHubCookieDomain = %q; it must keep naming the PREVIOUS domain, "+
			"otherwise the cookies it exists to expire are never expired", defaultLegacyHubCookieDomain)
	}
	if !strings.Contains(legacyImpersonateCookieDomain, "kubestellar.io") {
		t.Errorf("legacyImpersonateCookieDomain = %q; same reason", legacyImpersonateCookieDomain)
	}
}

// The public landing page has the same fallback, and it is static — served raw
// with no server templating — so it cannot be handed hub_spoke_domain the way
// the dashboard is. It derives the parent domain from location.hostname
// instead: the page is served BY the hub, so the hub's own host is the domain
// hosted spokes hang off, and unlike a literal it follows a domain move.
func TestLandingPageBuildsTenantURLsFromItsOwnHost(t *testing.T) {
	b, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	page := string(b)
	if strings.Contains(page, "'.hive.kubestellar.io'") ||
		strings.Contains(page, "+ '.hive.kubestellar.io'") {
		t.Error("landing page still builds tenant URLs from a hardcoded hostname; " +
			"derive the parent domain from location.hostname so the links survive a domain move")
	}
	if !strings.Contains(page, "location.hostname") {
		t.Error("landing page does not derive the tenant parent domain from location.hostname")
	}
	// An unknown or non-public host must suppress the link rather than emit
	// "<id>.localhost" or a bare relative "/contribute" on the hub itself.
	if !strings.Contains(page, "if (!cBase) return '';") {
		t.Error("contributeButton does not guard on an underivable tenant base")
	}
	if !strings.Contains(page, "if (!cBase2) {") {
		t.Error("the access-status renderer does not guard on an underivable tenant base")
	}
}
