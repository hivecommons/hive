package hub

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/auth"
)

// --- handleLogout tests ---

func TestHandleLogoutClearsCookiesAndReturnsOK(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	rec := httptest.NewRecorder()

	s.handleLogout(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("body = %q, want ok:true", rec.Body.String())
	}
	var foundHubUser, foundHubUserV2 bool
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case "hive_hub_user":
			foundHubUser = true
			if c.MaxAge != -1 {
				t.Fatalf("hive_hub_user MaxAge = %d, want -1", c.MaxAge)
			}
			if c.Value != "" {
				t.Fatalf("hive_hub_user Value = %q, want empty", c.Value)
			}
		case hubUserCookieV2Name:
			foundHubUserV2 = true
			if c.MaxAge != -1 {
				t.Fatalf("%s MaxAge = %d, want -1", hubUserCookieV2Name, c.MaxAge)
			}
		}
	}
	if !foundHubUser {
		t.Fatal("missing hive_hub_user cookie clear")
	}
	if !foundHubUserV2 {
		t.Fatalf("missing %s cookie clear", hubUserCookieV2Name)
	}
}

// --- handleAuthUser tests ---

func TestHandleAuthUserNoCookieReturnsUnauthenticated(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	rec := httptest.NewRecorder()

	s.handleAuthUser(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("body = %q, want authenticated:false", rec.Body.String())
	}
}

func TestHandleAuthUserEmptyCookieReturnsUnauthenticated(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: ""})
	rec := httptest.NewRecorder()

	s.handleAuthUser(rec, req)

	if !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("body = %q, want authenticated:false", rec.Body.String())
	}
}

// --- loginRedirectTarget tests ---

func TestLoginRedirectTargetValidPath(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=%2Fdashboard%2Fhives", nil)
	got := s.loginRedirectTarget(req)
	if got != "/dashboard/hives" {
		t.Fatalf("loginRedirectTarget = %q, want /dashboard/hives", got)
	}
}

func TestLoginRedirectTargetRdParam(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?rd=%2Fmy-hives", nil)
	got := s.loginRedirectTarget(req)
	if got != "/my-hives" {
		t.Fatalf("loginRedirectTarget = %q, want /my-hives", got)
	}
}

func TestLoginRedirectTargetDoubleSlashFallsBackToDashboard(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=//evil.com", nil)
	got := s.loginRedirectTarget(req)
	if got != "/dashboard" {
		t.Fatalf("loginRedirectTarget = %q, want /dashboard for open-redirect attempt", got)
	}
}

func TestLoginRedirectTargetEmpty(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	got := s.loginRedirectTarget(req)
	if got != "" {
		t.Fatalf("loginRedirectTarget = %q, want empty", got)
	}
}

// --- resolveProvider tests ---

func TestResolveProviderKnownProvider(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry(&auth.Provider{
		Name:        "google",
		DisplayName: "Google",
	})}
	p := s.resolveProvider("google")
	if p == nil || p.Name != "google" {
		t.Fatalf("resolveProvider(google) = %v, want google provider", p)
	}
}

func TestResolveProviderFallbackGitHub(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	p := s.resolveProvider("github")
	if p == nil {
		t.Fatal("resolveProvider(github) = nil, want synthesized GitHub provider")
	}
	if p.Name != "github" {
		t.Fatalf("p.Name = %q, want github", p.Name)
	}
	if p.AuthorizeURL != defaultGHAuthorizeURL {
		t.Fatalf("p.AuthorizeURL = %q, want %q", p.AuthorizeURL, defaultGHAuthorizeURL)
	}
}

func TestResolveProviderEmptyNameFallbackGitHub(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	p := s.resolveProvider("")
	if p == nil || p.Name != "github" {
		t.Fatalf("resolveProvider('') = %v, want synthesized GitHub", p)
	}
}

func TestResolveProviderUnknownReturnsNil(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	p := s.resolveProvider("unknown-oidc")
	if p != nil {
		t.Fatalf("resolveProvider(unknown-oidc) = %v, want nil", p)
	}
}

// --- parseCallbackState tests ---

func TestParseCallbackStateNewFormat(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry(&auth.Provider{Name: "google"})}
	state := url.QueryEscape("abcdef123" + oauthStateSeparator + "google" + oauthStateSeparator + "/my-hives")
	req := httptest.NewRequest(http.MethodGet, "/callback?state="+state, nil)

	provider, redirect := s.parseCallbackState(req)
	if provider != "google" {
		t.Fatalf("provider = %q, want google", provider)
	}
	if redirect != "/my-hives" {
		t.Fatalf("redirect = %q, want /my-hives", redirect)
	}
}

func TestParseCallbackStateLegacyFormat(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	state := url.QueryEscape("abcdef123" + oauthStateSeparator + "/dashboard")
	req := httptest.NewRequest(http.MethodGet, "/callback?state="+state, nil)

	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Fatalf("provider = %q, want github (legacy default)", provider)
	}
	if redirect != "/dashboard" {
		t.Fatalf("redirect = %q, want /dashboard", redirect)
	}
}

func TestParseCallbackStateEmptyStateUsesDefaults(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	req := httptest.NewRequest(http.MethodGet, "/callback", nil)

	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Fatalf("provider = %q, want github", provider)
	}
	if redirect != "/dashboard" {
		t.Fatalf("redirect = %q, want /dashboard", redirect)
	}
}

func TestParseCallbackStateRejectsOpenRedirect(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	state := url.QueryEscape("nonce" + oauthStateSeparator + "//evil.com/steal")
	req := httptest.NewRequest(http.MethodGet, "/callback?state="+state, nil)

	_, redirect := s.parseCallbackState(req)
	if redirect == "//evil.com/steal" {
		t.Fatal("parseCallbackState allowed open redirect //evil.com/steal")
	}
}

// --- displayIdentity tests ---

func TestDisplayIdentityGitHubLogin(t *testing.T) {
	s := &HubServer{}
	login, avatar := s.displayIdentity("github:testuser")
	if login != "testuser" {
		t.Fatalf("login = %q, want testuser", login)
	}
	if avatar != "https://github.com/testuser.png" {
		t.Fatalf("avatar = %q, want github avatar URL", avatar)
	}
}

func TestDisplayIdentityBareStringCanonicalizesToGitHub(t *testing.T) {
	s := &HubServer{}
	// A bare string (no "provider:" prefix) is treated as a legacy GitHub login.
	login, avatar := s.displayIdentity("testuser")
	if login != "testuser" {
		t.Fatalf("login = %q, want testuser", login)
	}
	if avatar != "https://github.com/testuser.png" {
		t.Fatalf("avatar = %q, want github avatar (legacy canonicalization)", avatar)
	}
}
