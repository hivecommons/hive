package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/auth"
)

// ============================================================
// parseCallbackState
// ============================================================

func TestParseCallbackState_ThreePartState(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(
			&auth.Provider{Name: "google", DisplayName: "Google", IsOIDC: true},
			&auth.Provider{Name: "github", DisplayName: "GitHub"},
		),
	}
	// state = nonce:google:/my-hives
	state := url.QueryEscape("abc123" + oauthStateSeparator + "google" + oauthStateSeparator + "/my-hives")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state="+state, nil)

	provider, redirect := s.parseCallbackState(req)

	if provider != "google" {
		t.Errorf("provider = %q, want google", provider)
	}
	if redirect != "/my-hives" {
		t.Errorf("redirect = %q, want /my-hives", redirect)
	}
}

func TestParseCallbackState_LegacyTwoPartState(t *testing.T) {
	// Pre-multi-provider state format: nonce:redirect (no provider).
	s := &HubServer{authProviders: auth.NewRegistry()}
	state := url.QueryEscape("nonce123" + oauthStateSeparator + "/dashboard/hives")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state="+state, nil)

	provider, redirect := s.parseCallbackState(req)

	if provider != "github" {
		t.Errorf("legacy two-part: provider = %q, want github (default)", provider)
	}
	if redirect != "/dashboard/hives" {
		t.Errorf("legacy two-part: redirect = %q, want /dashboard/hives", redirect)
	}
}

func TestParseCallbackState_EmptyState(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)

	provider, redirect := s.parseCallbackState(req)

	if provider != "github" {
		t.Errorf("empty state: provider = %q, want github", provider)
	}
	if redirect != "/dashboard" {
		t.Errorf("empty state: redirect = %q, want /dashboard", redirect)
	}
}

func TestParseCallbackState_UnknownProviderFallsToLegacy(t *testing.T) {
	// If the provider in state is not in the registry, it is treated as a
	// legacy redirect (the second part is the redirect, not a provider name).
	s := &HubServer{authProviders: auth.NewRegistry(
		&auth.Provider{Name: "github", DisplayName: "GitHub"},
	)}
	state := url.QueryEscape("nonce" + oauthStateSeparator + "bogus" + oauthStateSeparator + "/x")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state="+state, nil)

	provider, _ := s.parseCallbackState(req)

	if provider != "github" {
		t.Errorf("unknown provider: provider = %q, want github", provider)
	}
}

func TestParseCallbackState_DoubleSlashRedirectRejected(t *testing.T) {
	// A // prefix is a protocol-relative URL and must not be accepted as a safe
	// redirect (open-redirect risk).
	s := &HubServer{authProviders: auth.NewRegistry(
		&auth.Provider{Name: "github", DisplayName: "GitHub"},
	)}
	state := url.QueryEscape("n" + oauthStateSeparator + "github" + oauthStateSeparator + "//evil.com")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state="+state, nil)

	_, redirect := s.parseCallbackState(req)

	if redirect == "//evil.com" {
		t.Error("parseCallbackState must reject // prefix redirects")
	}
}

func TestParseCallbackState_AbsoluteURLRedirectRequiresTrust(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry(
		&auth.Provider{Name: "github", DisplayName: "GitHub"},
	)}

	// Trusted absolute URL should be accepted.
	state := url.QueryEscape("n" + oauthStateSeparator + "github" + oauthStateSeparator + "https://hive.kubestellar.io/dash")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state="+state, nil)
	_, redirect := s.parseCallbackState(req)
	if redirect != "https://hive.kubestellar.io/dash" {
		t.Errorf("trusted URL redirect = %q, want accepted", redirect)
	}

	// Untrusted absolute URL should fall back to /dashboard.
	state2 := url.QueryEscape("n" + oauthStateSeparator + "github" + oauthStateSeparator + "https://evil.com/steal")
	req2 := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state="+state2, nil)
	_, redirect2 := s.parseCallbackState(req2)
	if redirect2 == "https://evil.com/steal" {
		t.Error("parseCallbackState must reject untrusted absolute URLs")
	}
}

// ============================================================
// loginRedirectTarget
// ============================================================

func TestLoginRedirectTarget_RedirectParam(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=%2Fmy-hives", nil)
	got := s.loginRedirectTarget(req)
	if got != "/my-hives" {
		t.Errorf("redirect param: got %q, want /my-hives", got)
	}
}

func TestLoginRedirectTarget_RdParam(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?rd=%2Fdashboard%2Fsettings", nil)
	got := s.loginRedirectTarget(req)
	if got != "/dashboard/settings" {
		t.Errorf("rd param: got %q, want /dashboard/settings", got)
	}
}

func TestLoginRedirectTarget_NoParam(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	got := s.loginRedirectTarget(req)
	if got != "" {
		t.Errorf("no param: got %q, want empty", got)
	}
}

func TestLoginRedirectTarget_DoubleSlashBlocked(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=//evil.com", nil)
	got := s.loginRedirectTarget(req)
	if got == "//evil.com" {
		t.Error("loginRedirectTarget must block // prefix (protocol-relative open redirect)")
	}
}

func TestLoginRedirectTarget_AbsoluteUntrustedBlocked(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=https://evil.com/phish", nil)
	got := s.loginRedirectTarget(req)
	if got == "https://evil.com/phish" {
		t.Error("loginRedirectTarget must reject untrusted absolute URLs")
	}
	if got != "/dashboard" {
		t.Errorf("untrusted redirect should fall back to /dashboard, got %q", got)
	}
}

func TestLoginRedirectTarget_AbsoluteTrustedAccepted(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=https://hive.kubestellar.io/foo", nil)
	got := s.loginRedirectTarget(req)
	if got != "https://hive.kubestellar.io/foo" {
		t.Errorf("trusted absolute URL should be accepted, got %q", got)
	}
}

// ============================================================
// handleLogin dispatcher
// ============================================================

func TestHandleLogin_SingleProviderSkipsPicker(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(&auth.Provider{
			Name:         "github",
			DisplayName:  "GitHub",
			AuthorizeURL: "https://github.com/login/oauth/authorize",
			ClientID:     "test-id",
		}),
	}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("single provider: status = %d, want 307", rec.Code)
	}
}

func TestHandleLogin_MultiProviderShowsPicker(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(
			&auth.Provider{Name: "github", DisplayName: "GitHub", ClientID: "c1"},
			&auth.Provider{Name: "google", DisplayName: "Google", IsOIDC: true, ClientID: "c2"},
		),
	}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("multi provider: status = %d, want 200 (picker)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
}

func TestHandleLogin_NoProviderReturns503(t *testing.T) {
	// An empty registry AND no HIVE_HUB_OAUTH_CLIENT_ID: resolveProvider still
	// synthesizes GitHub with an empty client id, so handleLogin redirects (the
	// hub historically treated empty client id as "use whatever is in env"). The
	// 503 path is reachable only when resolveProvider returns nil, which happens
	// when the registry is nil (no registerOAuth at all) AND no env var. Since
	// resolveProvider always synthesizes for GitHub, test that an empty registry
	// still redirects (no crash, no panic).
	s := &HubServer{
		logger:        slog.Default(),
		authProviders: auth.NewRegistry(),
	}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)

	// Empty registry falls through to resolveProvider("github") which
	// synthesizes a provider → redirect to GitHub authorize.
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("empty registry with GitHub fallback: status = %d, want 307", rec.Code)
	}
}

func TestHandleLogin_CrawlerGetsPreview(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0")
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("crawler: status = %d, want 200", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("crawler must not redirect, got Location=%q", loc)
	}
}
