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
		"switchThemeVariantForLayout(next)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Appearance UI missing %q", want)
		}
	}
}
