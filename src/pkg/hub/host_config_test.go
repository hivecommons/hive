package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHubPublicHostDefaultsPreserveCanonicalValues(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "")
	t.Setenv("HIVE_HUB_SPOKE_DOMAIN", "")
	t.Setenv("HIVE_HUB_LEGACY_COOKIE_DOMAIN", "")

	if got := hubPublicURL(); got != "https://hive.kubestellar.io" {
		t.Fatalf("hubPublicURL() = %q, want historical default", got)
	}
	if got := oauthRedirectURI(); got != "https://hive.kubestellar.io/api/auth/callback" {
		t.Fatalf("oauthRedirectURI() = %q, want historical default", got)
	}
	if got := hubCanonicalHost(); got != "hive.kubestellar.io" {
		t.Fatalf("hubCanonicalHost() = %q, want historical default", got)
	}
	if got := sessionCookieDomain("hive.kubestellar.io"); got != ".kubestellar.io" {
		t.Fatalf("sessionCookieDomain(default hub) = %q, want .kubestellar.io", got)
	}
	if got := defaultClusterRegistry()[defaultClusterID].Domain; got != "hive.kubestellar.io" {
		t.Fatalf("default cluster domain = %q, want historical default", got)
	}
}

func TestHubPublicURLConfiguresCallbackCookieDomainAndOrigin(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hive.hivecommons.dev/")
	t.Setenv("HIVE_HUB_SPOKE_DOMAIN", "")

	if got := hubPublicURL(); got != "https://hive.hivecommons.dev" {
		t.Fatalf("hubPublicURL() = %q, want trimmed configured URL", got)
	}
	if got := oauthRedirectURI(); got != "https://hive.hivecommons.dev/api/auth/callback" {
		t.Fatalf("oauthRedirectURI() = %q, want configured callback", got)
	}
	if got := sessionCookieDomain("hive.hivecommons.dev"); got != ".hivecommons.dev" {
		t.Fatalf("sessionCookieDomain(configured hub) = %q, want .hivecommons.dev", got)
	}
	if !isSameOriginAsHub("https://hive.hivecommons.dev/dashboard") {
		t.Fatal("configured hub origin was not trusted")
	}
	if isSameOriginAsHub("https://hive.kubestellar.io/dashboard") {
		t.Fatal("old hub origin stayed trusted after HIVE_HUB_PUBLIC_URL override")
	}
}

func TestHubSpokeDomainConfiguresDefaultClusterRegistry(t *testing.T) {
	t.Setenv("HIVE_HUB_SPOKE_DOMAIN", "spokes.hivecommons.dev")

	if got := defaultClusterRegistry()[defaultClusterID].Domain; got != "spokes.hivecommons.dev" {
		t.Fatalf("default cluster domain = %q, want configured spoke domain", got)
	}
}

func TestLegacyCookieDomainReadRemintsNewDomainAndClearsLegacy(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hive.hivecommons.dev")
	t.Setenv("HIVE_HUB_LEGACY_COOKIE_DOMAIN", "kubestellar.io")
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "octocat")

	req := httptest.NewRequest(http.MethodGet, "https://hive.hivecommons.dev/api/auth/user", nil)
	presented := testAuthCookie("octocat")
	req.AddCookie(presented)
	rec := httptest.NewRecorder()
	s.handleAuthUser(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated /api/auth/user = %d, want 200", rec.Code)
	}
	var live, legacyClear *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name != "hive_hub_user" {
			continue
		}
		switch {
		case c.Value == presented.Value:
			live = c
		case c.Value == "" && c.Domain == "kubestellar.io":
			legacyClear = c
		}
	}
	if live == nil || live.Domain != "hivecommons.dev" {
		t.Fatalf("live cookie = %#v, want session on hivecommons.dev", live)
	}
	if legacyClear == nil || legacyClear.MaxAge >= 0 {
		t.Fatalf("legacy clear cookie = %#v, want expired kubestellar.io cookie", legacyClear)
	}
}
