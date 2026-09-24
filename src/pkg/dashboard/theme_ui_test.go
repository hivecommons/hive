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
		"function toggleLayout()",
		"document.body.classList.toggle('light-mode', mode === 'light')",
		"/api/theme.css?theme=",
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

func TestLayoutToggleDoesNotPersistHiveWideTheme(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "function toggleLayout()")
	if start < 0 {
		t.Fatal("missing toggleLayout")
	}
	next := strings.Index(html[start+len("function toggleLayout()"):], "function ")
	if next < 0 {
		t.Fatal("toggleLayout is not followed by another function")
	}
	toggle := html[start : start+len("function toggleLayout()")+next]
	for _, forbidden := range []string{"putTheme", "/api/config/dashboard/theme", "fetch("} {
		if strings.Contains(toggle, forbidden) {
			t.Fatalf("toggleLayout must stay local-only; found %q in:\n%s", forbidden, toggle)
		}
	}
}

func TestAppearanceThemeHoverCannotResizeSettingsModal(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	html := string(b)
	start := strings.Index(html, ".config-modal {\n")
	if start < 0 {
		t.Fatal("missing .config-modal CSS block")
	}
	end := strings.Index(html[start:], "\n    } /* modal panel recipe */")
	if end < 0 {
		t.Fatal(".config-modal CSS block terminator changed")
	}
	block := html[start : start+end]
	for _, want := range []string{
		"width: 1200px; max-width: 96vw;",
		"height: 85vh;",
		"font-family: var(--font-ui); font-size: var(--fs-base);",
		"contain: layout;",
		"--font-ui: Inter, ui-sans-serif",
		"--font-mono: 'SF Mono'",
		"--fs-base: .82rem;",
		"--sp-7: 20px;",
		"--r: 6px;",
		"--radius: var(--r);",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("settings modal layout is not pinned; missing %q in:\n%s", want, block)
		}
	}
	for _, want := range []string{
		"data-mouseover-action=\"themePreview\"",
		"data-mouseout-action=\"themePreviewClear\"",
		"function themePreview()",
		"setThemeStylesheet(id)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("theme hover preview wiring missing %q", want)
		}
	}
}
