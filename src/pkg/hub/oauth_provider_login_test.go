package hub

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/auth"
)

func TestHandleProviderLoginUnknownProvider(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login/nope", nil)
	req.SetPathValue("provider", "nope")
	rec := httptest.NewRecorder()

	s.handleProviderLogin(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(rec.Body.String(), "unknown login provider") {
		t.Fatalf("body = %q, want unknown provider error", rec.Body.String())
	}
}

func TestHandleProviderLoginStartsProviderFlow(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(&auth.Provider{
			Name:         "github",
			DisplayName:  "GitHub",
			AuthorizeURL: "https://github.com/login/oauth/authorize",
			ClientID:     "test-client-id",
		}),
	}
	req := httptest.NewRequest(http.MethodGet, "/login/github?redirect=%2Fmy-hives", nil)
	req.SetPathValue("provider", "github")
	rec := httptest.NewRecorder()

	s.handleProviderLogin(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, http.StatusTemporaryRedirect, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize?") {
		t.Fatalf("location = %q, want github authorize URL", loc)
	}
	if !strings.Contains(loc, "client_id=test-client-id") {
		t.Fatalf("location %q missing client_id", loc)
	}
	if !strings.Contains(loc, "state=") {
		t.Fatalf("location %q missing state", loc)
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if got := parsed.Query().Get("redirect_uri"); got != oauthRedirectURI() {
		t.Fatalf("redirect_uri = %q, want %q", got, oauthRedirectURI())
	}
	var stateCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			stateCookie = c
			break
		}
	}
	if stateCookie == nil {
		t.Fatal("expected oauth state cookie")
	}
}

func TestHandleProviderLoginCrawlerGetsPreview(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/login/github", nil)
	req.SetPathValue("provider", "github")
	req.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0")
	rec := httptest.NewRecorder()

	s.handleProviderLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("content-type = %q, want text/html", got)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("crawler path must not redirect, got Location=%q", loc)
	}
}

func TestWriteProviderPickerRendersProviderButtons(t *testing.T) {
	s := &HubServer{}
	providers := []*auth.Provider{
		{Name: "github", DisplayName: "GitHub"},
		{Name: "custom", DisplayName: "Custom OIDC"},
	}
	rec := httptest.NewRecorder()

	s.writeProviderPicker(rec, providers, "/dashboard/hives")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/html; charset=utf-8", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/login/github?redirect=%2Fdashboard%2Fhives"`) {
		t.Fatalf("picker missing github link with redirect, body=%q", body)
	}
	if !strings.Contains(body, "Continue with GitHub") || !strings.Contains(body, "Continue with Custom OIDC") {
		t.Fatalf("picker missing provider labels, body=%q", body)
	}
}

func TestProviderGlyphKnownAndDefault(t *testing.T) {
	if g := providerGlyph("github"); !strings.Contains(g, "<svg") {
		t.Fatalf("github glyph must be svg, got %q", g)
	}
	if g := providerGlyph("ibmid"); !strings.Contains(g, "viewBox=\"0 0 512 205\"") {
		t.Fatalf("ibmid glyph missing expected logo viewBox, got %q", g)
	}
	if g := providerGlyph("unknown-provider"); g != "&#x2022;" {
		t.Fatalf("unknown provider glyph = %q, want bullet", g)
	}
}
