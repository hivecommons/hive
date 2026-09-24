package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dashboardtheme "github.com/hivecommons/hive/pkg/dashboard/theme"
)

func TestThemeCSSServesETagAndEffectiveTokens(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Dashboard.Theme = "hive"
	s.deps.Config.Dashboard.ThemeOverrides.Tokens = map[string]string{"--accent": "#e0a33a"}
	rec := doGet(s, "/api/theme.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/theme.css = %d: %s", rec.Code, rec.Body.String())
	}

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("Content-Type = %q", ct)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if body := rec.Body.String(); !strings.Contains(body, "#e0a33a") || !strings.Contains(body, "hive") {
		t.Fatalf("theme css missing expected content: %s", body)
	}
	rec304 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/theme.css", nil)
	req.Header.Set("If-None-Match", etag)
	s.mux.ServeHTTP(rec304, req)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", rec304.Code)
	}
}

func TestThemesListAndExplicitThemeCSS(t *testing.T) {
	s := govServer(t)
	rec := doGet(s, "/api/themes")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/themes = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Themes []struct {
			ID       string   `json:"id"`
			Name     string   `json:"name"`
			Swatches []string `json:"swatches"`
			Scopes   []string `json:"scopes"`
		} `json:"themes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /api/themes: %v", err)
	}
	if len(payload.Themes) < 10 {
		t.Fatalf("theme list has %d themes, want at least 10", len(payload.Themes))
	}
	found := false
	for _, th := range payload.Themes {
		if th.ID == "star-wars" {
			found = th.Name != "" && len(th.Swatches) >= 4
		}
		key := strings.Join(th.Swatches, "|")
		if key == "" {
			t.Fatalf("%s has no swatches", th.ID)
		}
	}
	if !found {
		t.Fatalf("/api/themes missing star-wars with swatches: %+v", payload.Themes)
	}
	seenSwatches := map[string]string{}
	for _, th := range payload.Themes {
		key := strings.Join(th.Swatches, "|")
		if other := seenSwatches[key]; other != "" {
			t.Fatalf("%s and %s expose identical swatches %v", other, th.ID, th.Swatches)
		}
		seenSwatches[key] = th.ID
	}
	contrib := doGet(s, "/api/themes?scope=contributor")
	if contrib.Code != http.StatusOK {
		t.Fatalf("GET /api/themes?scope=contributor = %d: %s", contrib.Code, contrib.Body.String())
	}
	var scoped struct {
		Themes []struct {
			ID string `json:"id"`
		} `json:"themes"`
	}
	if err := json.Unmarshal(contrib.Body.Bytes(), &scoped); err != nil {
		t.Fatalf("decode scoped themes: %v", err)
	}
	if len(scoped.Themes) <= len(payload.Themes)-7 {
		t.Fatalf("contributor scope did not include migrated profile skins: got %d of %d", len(scoped.Themes), len(payload.Themes))
	}
	css := doGet(s, "/api/theme.css?theme=terminal")
	if css.Code != http.StatusOK || !strings.Contains(css.Body.String(), "green-on-black") && !strings.Contains(css.Body.String(), "terminal") {
		t.Fatalf("explicit terminal css = %d: %s", css.Code, css.Body.String())
	}
	contribCSS := doGet(s, "/api/theme.css?scope=contributor&theme=contributor-violet-advisor")
	if contribCSS.Code != http.StatusOK || !strings.Contains(contribCSS.Body.String(), "--cc-bg") || !strings.Contains(contribCSS.Body.String(), "--me-accent") {
		t.Fatalf("contributor theme css = %d: %s", contribCSS.Code, contribCSS.Body.String())
	}
	if bad := doGet(s, "/api/theme.css?theme=missing"); bad.Code != http.StatusNotFound {
		t.Fatalf("missing theme css = %d, want 404", bad.Code)
	}
}

func TestThemeCSSAndPreviewExposeCurrentModePalettes(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Dashboard.Theme = "solarized-dark"

	served := doGet(s, "/api/theme.css")
	if served.Code != http.StatusOK {
		t.Fatalf("served theme css = %d: %s", served.Code, served.Body.String())
	}
	servedRoot := cssBlockInBody(t, served.Body.String(), ":root")
	servedLight := cssBlockInBody(t, served.Body.String(), "body.light-mode")
	for _, want := range []string{"--surface-0: #002b36;", "--accent: #2aa198;"} {
		if !strings.Contains(servedRoot, want) {
			t.Fatalf("served dark/root mode missing %q:\n%s", want, servedRoot)
		}
	}
	for _, forbidden := range []string{"--surface-0: #002b36;", "--text: #93a1a1;"} {
		if strings.Contains(servedLight, forbidden) {
			t.Fatalf("served light-mode block re-emits dark token %q:\n%s", forbidden, servedLight)
		}
	}
	if !strings.Contains(servedLight, "--accent: #2aa198;") {
		t.Fatalf("served light-mode block should keep Solarized accent:\n%s", servedLight)
	}

	preview := doGet(s, "/api/theme.css?theme=solarized-dark")
	if preview.Code != http.StatusOK {
		t.Fatalf("preview theme css = %d: %s", preview.Code, preview.Body.String())
	}
	previewRoot := cssBlockInBody(t, preview.Body.String(), ":root")
	previewLight := cssBlockInBody(t, preview.Body.String(), "body.light-mode")
	if previewRoot != servedRoot || previewLight != servedLight {
		t.Fatalf("hover preview must expose the same mode palettes as served CSS\nserved root:%s\npreview root:%s\nserved light:%s\npreview light:%s", servedRoot, previewRoot, servedLight, previewLight)
	}
}

func cssBlockInBody(t *testing.T, css, selector string) string {
	t.Helper()
	start := strings.Index(css, selector+"{")
	if start < 0 {
		t.Fatalf("CSS missing selector %s", selector)
	}
	start += len(selector) + 1
	end := strings.Index(css[start:], "}")
	if end < 0 {
		t.Fatalf("CSS selector %s has no closing brace", selector)
	}
	return css[start : start+end]
}

func TestDashboardThemeAPIRoundTripAndOwnerGate(t *testing.T) {
	s := govServer(t)
	if rec := doGet(s, "/api/config/dashboard/theme"); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated GET = %d, want 403", rec.Code)
	}
	put := doPut(s, "/api/config/dashboard/theme", map[string]any{
		"theme": "nord",
		"theme_overrides": map[string]any{
			"tokens":     map[string]string{"--accent": "#88c0d0"},
			"background": map[string]any{"image": dashboardtheme.HoneycombDataURI, "opacity": 0.1, "attachment": "fixed"},
		},
	})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", put.Code, put.Body.String())
	}
	if s.deps.Config.Dashboard.Theme != "nord" || s.deps.Config.Dashboard.ThemeOverrides.Tokens["--accent"] != "#88c0d0" {
		t.Fatalf("config not updated: %+v", s.deps.Config.Dashboard)
	}
	get := doOwnerGet(s, "/api/config/dashboard/theme")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", get.Code, get.Body.String())
	}
	var payload struct {
		Theme     string                 `json:"theme"`
		Catalog   []dashboardtheme.Theme `json:"catalog"`
		Effective dashboardtheme.Theme   `json:"effective"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Theme != "nord" || len(payload.Catalog) < 8 || payload.Effective.Tokens["--accent"] != "#88c0d0" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestDashboardThemeAPIRejectsUnsafeCSSAndTokens(t *testing.T) {
	s := govServer(t)
	for _, body := range []map[string]any{
		{"theme": "missing"},
		{"theme": "hive", "theme_overrides": map[string]any{"tokens": map[string]string{"--typo": "red"}}},
		{"theme": "hive", "theme_overrides": map[string]any{"custom_css": ".x{background:url(http://example.org/x.png)}"}},
	} {
		rec := doPut(s, "/api/config/dashboard/theme", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %#v => %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
}
