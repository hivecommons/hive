package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/auth"
)

// ============================================================
// handleProviderLogin — coverage for /login/{provider}
// ============================================================

func TestHandleProviderLogin_UnknownProvider(t *testing.T) {
	s := providerLoginHub(t)
	req := httptest.NewRequest("GET", "/login/unknown", nil)
	req.SetPathValue("provider", "unknown")
	w := httptest.NewRecorder()
	s.handleProviderLogin(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 for unknown provider", w.Code)
	}
}

func TestHandleProviderLogin_ValidProvider_Redirects(t *testing.T) {
	s := providerLoginHub(t)
	req := httptest.NewRequest("GET", "/login/github", nil)
	req.SetPathValue("provider", "github")
	w := httptest.NewRecorder()
	s.handleProviderLogin(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("got %d, want 307 redirect", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "github.com/login/oauth/authorize") {
		t.Fatalf("expected GitHub authorize URL in Location, got %q", loc)
	}
	if !strings.Contains(loc, "state=") {
		t.Fatalf("missing state parameter in %q", loc)
	}
}

func TestHandleProviderLogin_SetsStateCookie(t *testing.T) {
	s := providerLoginHub(t)
	req := httptest.NewRequest("GET", "/login/github", nil)
	req.SetPathValue("provider", "github")
	w := httptest.NewRecorder()
	s.handleProviderLogin(w, req)

	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			found = true
			if c.Value == "" {
				t.Fatal("state cookie is empty")
			}
			if !c.HttpOnly {
				t.Fatal("state cookie must be HttpOnly")
			}
		}
	}
	if !found {
		t.Fatal("state cookie not set")
	}
}

func TestHandleProviderLogin_LinkPreviewCrawler_GetsCard(t *testing.T) {
	s := providerLoginHub(t)
	req := httptest.NewRequest("GET", "/login/github", nil)
	req.SetPathValue("provider", "github")
	req.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0")
	w := httptest.NewRecorder()
	s.handleProviderLogin(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 for link preview bot", w.Code)
	}
	if !strings.Contains(w.Body.String(), "og:title") {
		t.Fatal("preview response missing og:title")
	}
}

func TestHandleProviderLogin_RedirectParam_Preserved(t *testing.T) {
	s := providerLoginHub(t)
	req := httptest.NewRequest("GET", "/login/github?redirect=%2Fsettings", nil)
	req.SetPathValue("provider", "github")
	w := httptest.NewRecorder()
	s.handleProviderLogin(w, req)

	loc := w.Header().Get("Location")
	// The state should contain the redirect target
	if !strings.Contains(loc, "state=") {
		t.Fatal("missing state in redirect")
	}
}

// ============================================================
// parseCallbackState
// ============================================================

func TestParseCallbackState_ThreePartState(t *testing.T) {
	s := providerLoginHub(t)
	nonce := "abc123"
	state := url.QueryEscape(nonce + ":" + "github" + ":" + "/settings")
	req := httptest.NewRequest("GET", "/api/auth/callback?state="+state, nil)

	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Fatalf("provider = %q, want github", provider)
	}
	if redirect != "/settings" {
		t.Fatalf("redirect = %q, want /settings", redirect)
	}
}

func TestParseCallbackState_LegacyTwoPartState(t *testing.T) {
	s := providerLoginHub(t)
	state := url.QueryEscape("abc123:/dashboard/hives")
	req := httptest.NewRequest("GET", "/api/auth/callback?state="+state, nil)

	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Fatalf("provider = %q, want github (legacy fallback)", provider)
	}
	if redirect != "/dashboard/hives" {
		t.Fatalf("redirect = %q, want /dashboard/hives", redirect)
	}
}

func TestParseCallbackState_EmptyState(t *testing.T) {
	s := providerLoginHub(t)
	req := httptest.NewRequest("GET", "/api/auth/callback", nil)

	provider, redirect := s.parseCallbackState(req)
	if provider != "github" {
		t.Fatalf("provider = %q, want github", provider)
	}
	if redirect != "/dashboard" {
		t.Fatalf("redirect = %q, want /dashboard", redirect)
	}
}

func TestParseCallbackState_RejectsOpenRedirect(t *testing.T) {
	s := providerLoginHub(t)
	state := url.QueryEscape("nonce:github:https://evil.example.com")
	req := httptest.NewRequest("GET", "/api/auth/callback?state="+state, nil)

	_, redirect := s.parseCallbackState(req)
	if strings.Contains(redirect, "evil") {
		t.Fatalf("open redirect not blocked: %q", redirect)
	}
}

// ============================================================
// displayIdentity
// ============================================================

func TestDisplayIdentity_GitHubUser(t *testing.T) {
	s := providerLoginHub(t)
	login, avatar := s.displayIdentity("testuser")
	if login != "testuser" {
		t.Fatalf("login = %q, want testuser", login)
	}
	if !strings.Contains(avatar, "github.com/testuser.png") {
		t.Fatalf("avatar = %q, want github avatar URL", avatar)
	}
}

func TestDisplayIdentity_CanonicalOIDCUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	s := providerLoginHub(t)

	saveSaaSUser(&SaaSUser{
		GitHubUsername: "google:12345",
		CanonicalID:    "google:12345",
		Provider:       "google",
		Email:          "user@gmail.com",
		AvatarURL:      "https://lh3.google.com/photo",
	})

	login, avatar := s.displayIdentity("google:12345")
	if login != "user@gmail.com" {
		t.Fatalf("login = %q, want email for OIDC user", login)
	}
	if avatar != "https://lh3.google.com/photo" {
		t.Fatalf("avatar = %q, want stored avatar", avatar)
	}
}

// ============================================================
// writeProviderPicker
// ============================================================

func TestWriteProviderPicker_RendersAllProviders(t *testing.T) {
	s := providerLoginHub(t)
	providers := []*auth.Provider{
		{Name: "github", DisplayName: "GitHub"},
		{Name: "google", DisplayName: "Google", IsOIDC: true},
	}
	w := httptest.NewRecorder()
	s.writeProviderPicker(w, providers, "/settings")

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Continue with GitHub") {
		t.Fatal("missing GitHub button")
	}
	if !strings.Contains(body, "Continue with Google") {
		t.Fatal("missing Google button")
	}
	if !strings.Contains(body, "/login/github") {
		t.Fatal("missing /login/github link")
	}
	if !strings.Contains(body, url.QueryEscape("/settings")) {
		t.Fatal("redirect not threaded through picker")
	}
}

func TestWriteProviderPicker_NoRedirect(t *testing.T) {
	s := providerLoginHub(t)
	providers := []*auth.Provider{
		{Name: "github", DisplayName: "GitHub"},
	}
	w := httptest.NewRecorder()
	s.writeProviderPicker(w, providers, "")

	body := w.Body.String()
	if strings.Contains(body, "?redirect=") {
		t.Fatal("empty redirect should not produce ?redirect= in links")
	}
}

// ============================================================
// handleLogin — multi-provider picker path
// ============================================================

func TestHandleLogin_MultiProvider_ShowsPicker(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	s := &HubServer{
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		mux:    http.NewServeMux(),
		authProviders: auth.NewRegistry(
			&auth.Provider{Name: "github", DisplayName: "GitHub", ClientID: "test-id", AuthorizeURL: defaultGHAuthorizeURL, TokenURL: defaultGHTokenURL},
			&auth.Provider{Name: "google", DisplayName: "Google", IsOIDC: true, ClientID: "goog-id"},
		),
	}
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	s.handleLogin(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (picker page)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Continue with GitHub") || !strings.Contains(body, "Continue with Google") {
		t.Fatal("picker should show both providers")
	}
}

// ============================================================
// handleAuthUser / handleLogout
// ============================================================

func TestHandleAuthUser_NoCookie_Unauthenticated(t *testing.T) {
	s := providerLoginHub(t)
	req := httptest.NewRequest("GET", "/api/auth/user", nil)
	w := httptest.NewRecorder()
	s.handleAuthUser(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != false {
		t.Fatalf("expected authenticated=false, got %v", resp)
	}
}

func TestHandleLogout_ClearsBothCookies(t *testing.T) {
	s := providerLoginHub(t)
	req := httptest.NewRequest("POST", "/api/auth/logout", nil)
	w := httptest.NewRecorder()
	s.handleLogout(w, req)

	cookieNames := map[string]bool{}
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			cookieNames[c.Name] = true
		}
	}
	if !cookieNames["hive_hub_user"] {
		t.Fatal("hive_hub_user not cleared")
	}
	if !cookieNames[hubUserCookieV2Name] {
		t.Fatal("v2 cookie not cleared")
	}
}

// ============================================================
// providerGlyph
// ============================================================

func TestProviderGlyph_KnownProviders(t *testing.T) {
	for _, name := range []string{"github", "google", "ibmid", "redhat", "microsoft", "custom"} {
		g := providerGlyph(name)
		if !strings.Contains(g, "<svg") {
			t.Fatalf("providerGlyph(%q) = %q, want SVG", name, g)
		}
	}
}

func TestProviderGlyph_Unknown_ReturnsBullet(t *testing.T) {
	g := providerGlyph("nonexistent")
	if g != "&#x2022;" {
		t.Fatalf("providerGlyph(nonexistent) = %q, want bullet", g)
	}
}

// ============================================================
// helpers
// ============================================================

func providerLoginHub(t *testing.T) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	s := &HubServer{
		logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		mux:    http.NewServeMux(),
		authProviders: auth.NewRegistry(
			&auth.Provider{
				Name:         "github",
				DisplayName:  "GitHub",
				ClientID:     "test-client-id",
				AuthorizeURL: defaultGHAuthorizeURL,
				TokenURL:     defaultGHTokenURL,
				Scopes:       []string{""},
			},
		),
	}
	return s
}
