package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/auth"
)

// ============================================================
// handleLogin
// ============================================================

func TestHandleLoginSingleProviderRedirects(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(&auth.Provider{
			Name:         "github",
			DisplayName:  "GitHub",
			AuthorizeURL: "https://github.com/login/oauth/authorize",
			ClientID:     "cid",
		}),
	}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "github.com/login/oauth/authorize") {
		t.Fatalf("location = %q, want github authorize URL", loc)
	}
}

func TestHandleLoginMultiProviderShowsPicker(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(
			&auth.Provider{Name: "github", DisplayName: "GitHub"},
			&auth.Provider{Name: "google", DisplayName: "Google", IsOIDC: true},
		),
	}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Continue with GitHub") {
		t.Fatal("picker missing GitHub button")
	}
	if !strings.Contains(body, "Continue with Google") {
		t.Fatal("picker missing Google button")
	}
}

func TestHandleLoginNoProvidersGitHubFallback(t *testing.T) {
	// Even with an empty registry, resolveProvider("github") synthesizes a
	// GitHub provider from env, so handleLogin redirects rather than 503.
	s := &HubServer{
		logger:        slog.Default(),
		authProviders: auth.NewRegistry(),
	}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	// GitHub fallback means we get a redirect, not 503.
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (GitHub fallback)", rec.Code, http.StatusTemporaryRedirect)
	}
}

func TestHandleLoginCrawlerGetsPreview(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(&auth.Provider{
			Name: "github", DisplayName: "GitHub",
			AuthorizeURL: "https://github.com/login/oauth/authorize",
			ClientID:     "cid",
		}),
	}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0")
	rec := httptest.NewRecorder()

	s.handleLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("crawler should not be redirected, got Location=%q", loc)
	}
}

// ============================================================
// loginRedirectTarget
// ============================================================

func TestLoginRedirectTargetRelativePath(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=%2Fmy-hives", nil)
	got := s.loginRedirectTarget(req)
	if got != "/my-hives" {
		t.Fatalf("redirect = %q, want /my-hives", got)
	}
}

func TestLoginRedirectTargetRdParam(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?rd=%2Fdash", nil)
	got := s.loginRedirectTarget(req)
	if got != "/dash" {
		t.Fatalf("redirect = %q, want /dash", got)
	}
}

func TestLoginRedirectTargetDoubleSlashBlocked(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=//evil.com", nil)
	got := s.loginRedirectTarget(req)
	if got != "/dashboard" {
		t.Fatalf("redirect = %q, want /dashboard (double-slash blocked)", got)
	}
}

func TestLoginRedirectTargetEmpty(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	got := s.loginRedirectTarget(req)
	if got != "" {
		t.Fatalf("redirect = %q, want empty", got)
	}
}

// ============================================================
// resolveProvider
// ============================================================

func TestResolveProviderFromRegistry(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(&auth.Provider{
			Name: "github", DisplayName: "GitHub",
		}),
	}
	p := s.resolveProvider("github")
	if p == nil || p.Name != "github" {
		t.Fatal("expected github provider from registry")
	}
}

func TestResolveProviderGitHubFallback(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(), // empty registry
	}
	p := s.resolveProvider("github")
	if p == nil {
		t.Fatal("expected synthesized github provider")
	}
	if p.Name != "github" {
		t.Fatalf("name = %q, want github", p.Name)
	}
}

func TestResolveProviderEmptyNameIsGitHub(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(),
	}
	p := s.resolveProvider("")
	if p == nil || p.Name != "github" {
		t.Fatal("expected github provider for empty name")
	}
}

func TestResolveProviderUnknownOIDCReturnsNil(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(),
	}
	p := s.resolveProvider("ibmid")
	if p != nil {
		t.Fatalf("expected nil for unknown OIDC provider, got %+v", p)
	}
}

// ============================================================
// parseCallbackState
// ============================================================

func TestParseCallbackStateNewFormat(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(
			&auth.Provider{Name: "google", DisplayName: "Google"},
		),
	}
	state := "abc123:google:/my-page"
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state="+state, nil)
	provider, redirect := s.parseCallbackState(req)
	if provider != "google" {
		t.Fatalf("provider = %q, want google", provider)
	}
	if redirect != "/my-page" {
		t.Fatalf("redirect = %q, want /my-page", redirect)
	}
}

func TestParseCallbackStateLegacyFormat(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(),
	}
	// Legacy two-part state: nonce:redirect (no provider in middle)
	state := "abc123:/dashboard"
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state="+state, nil)
	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Fatalf("provider = %q, want github (legacy default)", provider)
	}
	if redirect != "/dashboard" {
		t.Fatalf("redirect = %q, want /dashboard", redirect)
	}
}

func TestParseCallbackStateEmpty(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)
	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Fatalf("provider = %q, want github", provider)
	}
	if redirect != "/dashboard" {
		t.Fatalf("redirect = %q, want /dashboard", redirect)
	}
}

// ============================================================
// isLinkPreviewCrawler
// ============================================================

func TestIsLinkPreviewCrawlerKnownBots(t *testing.T) {
	bots := []string{
		"Slackbot-LinkExpanding 1.0",
		"Twitterbot/1.0",
		"facebookexternalhit/1.1",
		"LinkedInBot/1.0",
		"Discordbot/2.0",
		"TelegramBot (like TwitterBot)",
	}
	for _, ua := range bots {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("User-Agent", ua)
		if !isLinkPreviewCrawler(r) {
			t.Errorf("expected crawler=true for UA %q", ua)
		}
	}
}

func TestIsLinkPreviewCrawlerRegularBrowser(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")
	if isLinkPreviewCrawler(r) {
		t.Fatal("regular browser should not be detected as crawler")
	}
}

func TestIsLinkPreviewCrawlerEmptyUA(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if isLinkPreviewCrawler(r) {
		t.Fatal("empty UA should not be detected as crawler")
	}
}

// ============================================================
// handleAuthUser — unauthenticated (no cookie)
// ============================================================

func TestHandleAuthUserNoCookie_OAuth(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	rec := httptest.NewRecorder()

	s.handleAuthUser(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"authenticated":false`) {
		t.Fatalf("body = %q, want authenticated:false", body)
	}
}

func TestHandleAuthUserEmptyCookie_OAuth(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: ""})
	rec := httptest.NewRecorder()

	s.handleAuthUser(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"authenticated":false`) {
		t.Fatalf("body = %q, want authenticated:false", body)
	}
}

// ============================================================
// handleLogout — no cookie path
// ============================================================

func TestHandleLogoutNoCookie(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	rec := httptest.NewRecorder()

	s.handleLogout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body = %q, want ok:true", body)
	}
	// Verify the cookie is cleared
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.MaxAge == -1 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("expected hive_hub_user cookie to be cleared")
	}
}

func TestHandleLogoutInvalidCookie(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "forged-garbage"})
	rec := httptest.NewRecorder()

	s.handleLogout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body = %q, want ok:true", body)
	}
}

// ============================================================
// handleOGCard
// ============================================================

func TestHandleOGCardServesImage(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/og-card.png", nil)
	rec := httptest.NewRecorder()

	s.handleOGCard(rec, req)

	// If the staticFS embed has the card, we get 200 with image/png.
	// If not (e.g. test binary without embed), we get 404. Both are valid.
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 200 or 404", rec.Code)
	}
	if rec.Code == http.StatusOK {
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("content-type = %q, want image/png", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
			t.Fatalf("cache-control = %q, want public", cc)
		}
	}
}

// ============================================================
// writeLinkPreview
// ============================================================

func TestWriteLinkPreviewContainsOGTags(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLinkPreview(rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `og:title`) {
		t.Fatal("missing og:title meta tag")
	}
	if !strings.Contains(body, `og:image`) {
		t.Fatal("missing og:image meta tag")
	}
	if !strings.Contains(body, "hive.kubestellar.io") {
		t.Fatal("missing hive URL")
	}
}

// ============================================================
// verifyOAuthStateNonce
// ============================================================

func TestVerifyOAuthStateNonceMissingCookie(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=abc:rest", nil)
	if s.verifyOAuthStateNonce(req) {
		t.Fatal("expected false when no cookie")
	}
}

func TestVerifyOAuthStateNonceEmptyCookie(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=abc:rest", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: ""})
	if s.verifyOAuthStateNonce(req) {
		t.Fatal("expected false for empty cookie")
	}
}

func TestVerifyOAuthStateNonceMissingState(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "nonce123"})
	if s.verifyOAuthStateNonce(req) {
		t.Fatal("expected false for missing state param")
	}
}

func TestVerifyOAuthStateNonceMismatch(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=wrong:rest", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "nonce123"})
	if s.verifyOAuthStateNonce(req) {
		t.Fatal("expected false for mismatched nonce")
	}
}

func TestVerifyOAuthStateNonceMatch(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=nonce123:github:/dash", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "nonce123"})
	if !s.verifyOAuthStateNonce(req) {
		t.Fatal("expected true for matching nonce")
	}
}

// ============================================================
// oauthStateNonce — entropy
// ============================================================

func TestOAuthStateNonceUniqueness(t *testing.T) {
	a, err := oauthStateNonce()
	if err != nil {
		t.Fatal(err)
	}
	b, err := oauthStateNonce()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two nonces should not be equal")
	}
	if len(a) != oauthStateNonceBytes*2 {
		t.Fatalf("nonce length = %d, want %d hex chars", len(a), oauthStateNonceBytes*2)
	}
}
