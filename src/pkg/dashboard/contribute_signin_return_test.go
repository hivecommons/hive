package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// #7453, spoke half: the Operations tab's "Sign in with GitHub" link pointed
// at "/", so a hosted visitor who signed in landed on the dashboard instead of
// back on the tab they were reading. On a hub-proxied spoke it now goes
// through the /auth/return trampoline, which bounces back to the tab.

func TestSafeReturnPathAcceptsOnlySameOriginPaths(t *testing.T) {
	cases := map[string]string{
		"":                                  authReturnDefault,
		"/contribute/operations":            "/contribute/operations",
		"/contribute/operations?x=1#mine":   "/contribute/operations?x=1#mine",
		"/":                                 "/",
		"contribute":                        authReturnDefault, // relative
		"//evil.example/phish":              authReturnDefault, // protocol-relative
		"/\\evil.example":                   authReturnDefault, // backslash trick
		"https://evil.example/":             authReturnDefault,
		"javascript:alert(1)":               authReturnDefault,
		"/contribute\r\nSet-Cookie: a=b":    authReturnDefault, // header injection
		"/contribute\x00":                   authReturnDefault,
		"/contribute/operations?to=//x.com": "/contribute/operations?to=//x.com", // a query on a same-origin path is fine
	}
	for in, want := range cases {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthReturnRedirectsAndIsPublic(t *testing.T) {
	if !isPublicPath("/auth/return") {
		t.Fatal("/auth/return must be public so grant-less signed-in contributors can return to public /contribute tabs")
	}
	s, _ := apiServer(t)

	rec := doGet(s, "/auth/return?to=%2Fcontribute%2Foperations%23mine")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/contribute/operations#mine" {
		t.Errorf("Location = %q, want the tab the visitor came from", got)
	}
	rec = doGet(s, "/auth/return?to=https%3A%2F%2Fevil.example%2F")
	if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != authReturnDefault {
		t.Errorf("off-origin target: %d %q, want 302 to %s", rec.Code, got, authReturnDefault)
	}
}

func TestAuthReturnHubProxiedAnonymousStartsHubLogin(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.HubProxied = true
	deps.Config.Hub.URL = "https://hive.hivecommons.dev"
	req := httptest.NewRequest(http.MethodGet, "/auth/return?to=%2Fcontribute%2Fprofile", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "bluefin.example.test")
	rec := httptest.NewRecorder()

	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 to hub login", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://hive.hivecommons.dev/login?") {
		t.Fatalf("Location = %q, want hub login", loc)
	}
	if !strings.Contains(loc, url.QueryEscape("https://bluefin.example.test/auth/return?to=%2Fcontribute%2Fprofile")) {
		t.Fatalf("Location = %q, want redirect back to this auth-return URL", loc)
	}
}

func TestAuthReturnHubProxiedIdentityReturnsToContributorTab(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.HubProxied = true
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodGet, "/auth/return?to=%2Fcontribute%2Fprofile", nil)
	req.Header.Set("X-Hive-User", "kikaraage")
	req.Header.Set("X-Hive-Role", "read")
	req.Header.Set(proxyAuthHeader, s.authToken)
	rec := httptest.NewRecorder()

	s.mux.ServeHTTP(rec, req)

	if got := rec.Header().Get("Location"); rec.Code != http.StatusFound || got != "/contribute/profile" {
		t.Fatalf("identity return: status=%d Location=%q, want 302 to /contribute/profile", rec.Code, got)
	}
}

func TestContributeSignInLinkFollowsSpokeAuthShape(t *testing.T) {
	render := func(hubProxied bool) string {
		t.Helper()
		srv := newFullServer(t)
		srv.deps.Config.Dashboard.HubProxied = hubProxied
		req := httptest.NewRequest(http.MethodGet, "/contribute/operations", nil)
		rec := httptest.NewRecorder()
		srv.handleContributeLanding(rec, req)
		return rec.Body.String()
	}
	hosted := render(true)
	if !strings.Contains(hosted, "var ccHubProxied=true;") {
		t.Error("hub-proxied spoke did not tell the page so")
	}
	if strings.Contains(hosted, "{{HIVE_HUB_PROXIED}}") {
		t.Error("the hub-proxied sentinel leaked into the rendered page")
	}
	self := render(false)
	if !strings.Contains(self, "var ccHubProxied=false;") {
		t.Error("self-hosted spoke did not tell the page so")
	}
	for _, want := range []string{
		"function ccSignInHref()",
		"return '/auth/return?to='+encodeURIComponent(here);",
		`href="'+esc(ccSignInHref())+'"`,
	} {
		if !strings.Contains(hosted, want) {
			t.Errorf("landing page is missing %q", want)
		}
	}
	if strings.Contains(hosted, `class="cc-signin-cta" href="/">`) {
		t.Error("the sign-in CTA is still hard-wired to the dashboard root")
	}
}
