package dashboard

import (
	"regexp"
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
		"theme-bg-scope",
		"Watermark on",
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

func TestModalOverlaysAreNotInsideWatermarkedCards(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	cardClasses := map[string]bool{
		"agent-card":     true,
		"repo-card":      true,
		"card-inset":     true,
		"card-tile":      true,
		"row-card":       true,
		"campaigns-card": true,
		"nous-card":      true,
		"kb-setup-card":  true,
	}
	overlayIDs := map[string]bool{
		"gh-auth-modal-overlay": true,
		"gh-setup-overlay":      true,
		"config-overlay":        true,
		"acmm-overlay":          true,
		"welcome-overlay":       true,
		"nous-config-overlay":   true,
	}
	attrsRe := regexp.MustCompile(`\s([a-zA-Z0-9_-]+)="([^"]*)"`)
	tagRe := regexp.MustCompile(`(?s)<(/?)([a-zA-Z0-9_-]+)([^>]*)>`)
	type node struct {
		tag     string
		classes []string
	}
	stack := []node{}
	seenOverlays := map[string]bool{}
	for _, match := range tagRe.FindAllStringSubmatch(string(b), -1) {
		tag := strings.ToLower(match[2])
		if strings.HasPrefix(match[3], "!--") || strings.HasPrefix(match[0], "<!") {
			continue
		}
		if match[1] == "/" {
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].tag == tag {
					stack = stack[:i]
					break
				}
			}
			continue
		}
		attrs := map[string]string{}
		for _, attr := range attrsRe.FindAllStringSubmatch(match[3], -1) {
			attrs[strings.ToLower(attr[1])] = attr[2]
		}
		if overlayIDs[attrs["id"]] {
			seenOverlays[attrs["id"]] = true
			for _, ancestor := range stack {
				for _, class := range ancestor.classes {
					if cardClasses[class] {
						t.Fatalf("overlay #%s is nested inside card class .%s", attrs["id"], class)
					}
				}
			}
		}
		selfClosing := strings.HasSuffix(strings.TrimSpace(match[3]), "/")
		if !selfClosing && tag != "input" && tag != "link" && tag != "meta" && tag != "br" {
			stack = append(stack, node{tag: tag, classes: strings.Fields(attrs["class"])})
		}
	}
	for id := range overlayIDs {
		if !seenOverlays[id] {
			t.Fatalf("overlay #%s was not found in index.html", id)
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
