package dashboard

import (
	"strings"
	"testing"
)

func TestAppearanceThemeUITabWired(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"'Appearance'",
		"function renderGovAppearance()",
		"/api/config/dashboard/theme",
		"applyThemePreset",
		"resetThemeToPreset",
		"saveThemeOverrides",
		"theme-bg-opacity",
		"theme-custom-css",
		"HONEYCOMB_WATERMARK",
		"/api/themes?scope=dashboard",
		"Contributor profile styles",
		"setThemeStylesheet(id)",
		"startsWith('contributor-')",
		"_themeBaseHref = '/api/theme.css?v=' + Date.now()",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Appearance UI missing %q", want)
		}
	}
	for _, gone := range []string{
		"switchThemeVariantForLayout",
		"&preview=",
		"data-action=\"themePreview\"",
	} {
		if strings.Contains(html, gone) {
			t.Fatalf("Appearance UI still contains %q", gone)
		}
	}
}
