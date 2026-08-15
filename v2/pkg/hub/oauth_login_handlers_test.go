package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/auth"
)

// ============================================================
// isLinkPreviewCrawler
// ============================================================

func TestIsLinkPreviewCrawlerMatch(t *testing.T) {
	crawlers := []string{
		"Slackbot-LinkExpanding 1.0 (+https://api.slack.com)",
		"Twitterbot/1.0",
		"facebookexternalhit/1.1",
		"LinkedInBot/1.0",
		"Discordbot/2.0",
		"TelegramBot (like TwitterBot)",
		"WhatsApp/2.21.10.16",
		"SkypeUriPreview",
		"Mozilla/5.0 (compatible; redditbot)",
		"Mattermost/1.0",
		"Googlebot/2.1",
		"bingbot/2.0",
		"Embedly/0.2",
	}
	for _, ua := range crawlers {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("User-Agent", ua)
		if !isLinkPreviewCrawler(req) {
			t.Errorf("isLinkPreviewCrawler(%q) = false, want true", ua)
		}
	}
}

func TestIsLinkPreviewCrawlerNoMatch(t *testing.T) {
	nonCrawlers := []string{
		"",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/120",
		"curl/8.1.0",
	}
	for _, ua := range nonCrawlers {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if isLinkPreviewCrawler(req) {
			t.Errorf("isLinkPreviewCrawler(%q) = true, want false", ua)
		}
	}
}

// ============================================================
// loginRedirectTarget
// ============================================================

func TestLoginRedirectTargetValidCases(t *testing.T) {
	s := &HubServer{}
	tests := []struct {
		query string
		want  string
	}{
		{"", ""},                     // no redirect param
		{"redirect=/dashboard", "/dashboard"},
		{"rd=/my-hives", "/my-hives"},
		{"redirect=//evil.com", "/dashboard"}, // protocol-relative rejected
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)
		got := s.loginRedirectTarget(req)
		if got != tt.want {
			t.Errorf("loginRedirectTarget(%q) = %q, want %q", tt.query, got, tt.want)
		}
	}
}

// ============================================================
// handleLogin
// ============================================================

func TestHandleLoginSingleProviderRedirectsDirectly(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(&auth.Provider{
			Name:         "github",
			DisplayName:  "GitHub",
			AuthorizeURL: "https://github.com/login/oauth/authorize",
			ClientID:     "cid",
		}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=/dash", nil)
	s.handleLogin(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "github.com/login/oauth/authorize") {
		t.Fatalf("single provider should redirect to GitHub, got %q", loc)
	}
}

func TestHandleLoginMultiProviderShowsPicker(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(
			&auth.Provider{Name: "github", DisplayName: "GitHub", AuthorizeURL: "https://github.com/login/oauth/authorize", ClientID: "cid"},
			&auth.Provider{Name: "google", DisplayName: "Google", IsOIDC: true, AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth", ClientID: "gid"},
		),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=/hives", nil)
	s.handleLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d for picker", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Continue with GitHub") {
		t.Fatal("picker missing GitHub button")
	}
	if !strings.Contains(body, "Continue with Google") {
		t.Fatal("picker missing Google button")
	}
}

func TestHandleLoginCrawlerGetsPreview(t *testing.T) {
	s := &HubServer{
		logger: slog.Default(),
		authProviders: auth.NewRegistry(&auth.Provider{
			Name: "github", DisplayName: "GitHub",
			AuthorizeURL: "https://github.com/login/oauth/authorize", ClientID: "cid",
		}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0")
	s.handleLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("crawler status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "og:title") {
		t.Fatal("crawler response missing Open Graph tags")
	}
}

func TestHandleLoginNoProvidersUsesGitHubFallback(t *testing.T) {
	// When the registry is empty but HIVE_HUB_OAUTH_CLIENT_ID is set,
	// handleLogin falls back to resolveProvider which synthesizes GitHub.
	t.Setenv("HIVE_HUB_OAUTH_CLIENT_ID", "fallback-id")
	s := &HubServer{
		logger:        slog.Default(),
		authProviders: auth.NewRegistry(),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	s.handleLogin(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("fallback status = %d, want 307", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "client_id=fallback-id") {
		t.Fatalf("expected fallback GitHub redirect, got %q", loc)
	}
}

// ============================================================
// verifyOAuthStateNonce
// ============================================================

func TestVerifyOAuthStateNonceMissingCookie(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/?state="+url.QueryEscape("nonce:github:/dash"), nil)
	if s.verifyOAuthStateNonce(req) {
		t.Fatal("should reject when cookie is missing")
	}
}

func TestVerifyOAuthStateNonceMissingState(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "abc"})
	if s.verifyOAuthStateNonce(req) {
		t.Fatal("should reject when state param is missing")
	}
}

func TestVerifyOAuthStateNonceMatch(t *testing.T) {
	s := &HubServer{}
	nonce := "abcdef1234567890"
	req := httptest.NewRequest(http.MethodGet, "/?state="+url.QueryEscape(nonce+":github:/dash"), nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})
	if !s.verifyOAuthStateNonce(req) {
		t.Fatal("should accept matching nonce")
	}
}

func TestVerifyOAuthStateNonceMismatch(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/?state="+url.QueryEscape("attacker-nonce:github:/dash"), nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "victim-nonce"})
	if s.verifyOAuthStateNonce(req) {
		t.Fatal("should reject mismatched nonce")
	}
}

// ============================================================
// handleAuthUser
// ============================================================

func TestHandleAuthUserNoCookie_OAuth(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	s.handleAuthUser(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("expected unauthenticated response, got %q", rec.Body.String())
	}
}

func TestHandleAuthUserEmptyCookie_OAuth(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: ""})
	s.handleAuthUser(rec, req)

	if !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("expected unauthenticated for empty cookie, got %q", rec.Body.String())
	}
}

func TestHandleAuthUserInvalidSignature(t *testing.T) {
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/user", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "forged-value"})
	s.handleAuthUser(rec, req)

	if !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("expected unauthenticated for forged cookie, got %q", rec.Body.String())
	}
}

// ============================================================
// handleLogout
// ============================================================

func TestHandleLogoutNoCookie(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	s.handleLogout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok response, got %q", rec.Body.String())
	}
	// Should clear the cookie regardless.
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the session cookie")
	}
}

func TestHandleLogoutWithForgedCookie(t *testing.T) {
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "forged-by-attacker"})
	s.handleLogout(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The forged cookie should NOT be revoked (no store write) but the
	// browser's copy should still be cleared.
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear cookie for forged value")
	}
}

// ============================================================
// handleOGCard
// ============================================================

func TestHandleOGCard(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/og-card.png", nil)
	s.handleOGCard(rec, req)

	// If og-card.png is embedded in staticFS, expect 200; if missing, 404 is acceptable.
	if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
		t.Fatalf("og-card status = %d, want 200 or 404", rec.Code)
	}
	if rec.Code == http.StatusOK {
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("content-type = %q, want image/png", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
			t.Errorf("cache-control = %q, want public", cc)
		}
	}
}

// ============================================================
// parseCallbackState
// ============================================================

func TestParseCallbackStateNewFormat(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(&auth.Provider{Name: "github", DisplayName: "GitHub"}),
	}
	req := httptest.NewRequest(http.MethodGet,
		"/?state="+url.QueryEscape("nonce123:github:/my-hives"), nil)
	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Errorf("provider = %q, want github", provider)
	}
	if redirect != "/my-hives" {
		t.Errorf("redirect = %q, want /my-hives", redirect)
	}
}

func TestParseCallbackStateLegacyFormat(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	req := httptest.NewRequest(http.MethodGet,
		"/?state="+url.QueryEscape("nonce123:/dashboard"), nil)
	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Errorf("legacy state provider = %q, want github", provider)
	}
	if redirect != "/dashboard" {
		t.Errorf("legacy state redirect = %q, want /dashboard", redirect)
	}
}

func TestParseCallbackStateEmpty(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry()}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	provider, redirect := s.parseCallbackState(req)
	if provider != "github" || redirect != "/dashboard" {
		t.Errorf("empty state = (%q, %q), want (github, /dashboard)", provider, redirect)
	}
}
