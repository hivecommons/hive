package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func dashboardTopbarHTML(t *testing.T) string {
	t.Helper()
	html := indexHTML(t)
	start := strings.Index(html, `<header id="oc-topbar"`)
	if start < 0 {
		t.Fatal("static dashboard is missing #oc-topbar")
	}
	end := strings.Index(html[start:], `</header>`)
	if end < 0 {
		t.Fatal("static dashboard topbar is not closed")
	}
	return html[start : start+end]
}

func TestNavbarChipElementsUseSharedClass(t *testing.T) {
	topbar := dashboardTopbarHTML(t)
	for _, id := range []string{
		"oc-drawer-toggle",
		"acmm-badge",
		"fleet-breaker-btn2",
		"feedback-bug-btn",
		"oc-settings-btn",
		"dashboard-custom-style-note",
		"oc-health",
		"welcome-topbar-btn",
		"oc-gh-avatar",
		"oc-gh-login-btn",
	} {
		re := regexp.MustCompile(`id="` + regexp.QuoteMeta(id) + `"[^>]*class="([^"]*)"|class="([^"]*)"[^>]*id="` + regexp.QuoteMeta(id) + `"`)
		m := re.FindStringSubmatch(topbar)
		if m == nil {
			t.Fatalf("topbar element #%s is missing", id)
		}
		classes := m[1]
		if classes == "" {
			classes = m[2]
		}
		if !strings.Contains(" "+classes+" ", " nav-chip ") {
			t.Errorf("topbar element #%s does not use shared nav-chip class: %q", id, classes)
		}
	}
}

func TestNavbarIconOnlyChipsHaveAccessibleNames(t *testing.T) {
	topbar := dashboardTopbarHTML(t)
	for _, id := range []string{"oc-drawer-toggle", "feedback-bug-btn", "oc-gh-avatar"} {
		re := regexp.MustCompile(`<[^>]+id="` + regexp.QuoteMeta(id) + `"[^>]*>`)
		tag := re.FindString(topbar)
		if tag == "" {
			t.Fatalf("topbar icon chip #%s is missing", id)
		}
		if !strings.Contains(tag, `title="`) {
			t.Errorf("icon-only chip #%s is missing title: %s", id, tag)
		}
		if !strings.Contains(tag, `aria-label="`) {
			t.Errorf("icon-only chip #%s is missing aria-label: %s", id, tag)
		}
	}
}

func TestNavbarStickyThreeZoneLayout(t *testing.T) {
	html := indexHTML(t)
	topbar := dashboardTopbarHTML(t)
	for _, want := range []string{
		`.oc-topbar {`,
		`position: fixed; top: var(--sp-0); left: var(--sidebar-w); right: auto;`,
		`--topbar-grid-columns: auto minmax(0, 1fr) auto;`,
		`display: grid; grid-template-columns: var(--topbar-grid-columns); align-items: center;`,
		`width: 100%;`,
		`z-index: var(--navbar-sticky-z);`,
		`.oc-topbar-left { justify-self: start; max-width: 100%; font-size: var(--fs-base); color: var(--muted); display: flex; align-items: center; gap: var(--nav-chip-gap, 8px); min-width: 0; justify-content: flex-start; overflow: hidden; }`,
		`.oc-topbar-center { container: navbar-center / inline-size; position: relative; z-index: 1; justify-self: stretch; display: flex; align-items: center; justify-content: center; gap: var(--nav-chip-gap, 8px); min-width: 0; max-width: 100%; overflow: hidden; }`,
		`.oc-topbar-right { position: relative; z-index: 2; justify-self: end; max-width: 100%; display: flex; align-items: center; justify-content: flex-end; gap: var(--nav-chip-gap, 8px); min-width: 0; flex-wrap: nowrap; }`,
		`overflow-x: auto; overflow-y: hidden;`,
		`scroll-margin-top: calc(var(--navbar-sticky-height) + var(--sp-5));`,
		`#oc-drawer-backdrop {`,
		`.config-overlay { z-index: 10000; }`,
		`.dashboard-section-ghost { position:fixed; z-index:12000;`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sticky navbar contract missing %q", want)
		}
	}
	topbarRuleRE := regexp.MustCompile(`(?s)\.oc-topbar\s*\{[^}]*\}`)
	topbarRules := topbarRuleRE.FindAllString(html, -1)
	gridTemplateCount := 0
	displayCount := 0
	for _, rule := range topbarRules {
		if strings.Contains(rule, `grid-template-columns:`) {
			gridTemplateCount++
		}
		if strings.Contains(rule, `display:`) {
			displayCount++
			if !strings.Contains(rule, `display: grid;`) {
				t.Fatalf("topbar display override is not grid: %s", rule)
			}
		}
	}
	if gridTemplateCount != 1 {
		t.Fatalf("topbar must have exactly one grid-template-columns declaration, found %d in %v", gridTemplateCount, topbarRules)
	}
	if displayCount != 1 {
		t.Fatalf("topbar must have exactly one display declaration, found %d in %v", displayCount, topbarRules)
	}
	if strings.Contains(html, `topbar.style.display = 'flex'`) || strings.Contains(html, `topbar.style.display = "flex"`) {
		t.Fatal("layout script must not override #oc-topbar display:flex; CSS owns the grid display")
	}
	for _, want := range []string{`class="oc-topbar-left"`, `class="oc-topbar-center"`, `class="oc-topbar-right"`} {
		if !strings.Contains(topbar, want) {
			t.Fatalf("topbar three-zone container missing %q", want)
		}
	}
	centerStart := strings.Index(topbar, `class="oc-topbar-center"`)
	rightStart := strings.Index(topbar, `class="oc-topbar-right"`)
	if centerStart < 0 || rightStart < 0 || centerStart > rightStart {
		t.Fatalf("topbar center/right zones are not ordered as expected")
	}
	center := topbar[centerStart:rightStart]
	right := topbar[rightStart:]
	for _, want := range []string{`id="fleet-breaker-wrap"`, `id="agent-navbar-upnext"`} {
		if !strings.Contains(center, want) {
			t.Errorf("topbar center zone missing %q", want)
		}
	}
	for _, want := range []string{`id="feedback-bug-btn"`, `class="nav-chip nav-chip--icon nav-chip--action layout-toggle"`, `id="oc-settings-btn"`, `id="oc-health"`, `id="welcome-topbar-btn"`, `id="oc-gh-avatar-wrap"`} {
		if !strings.Contains(right, want) {
			t.Errorf("topbar right zone missing %q", want)
		}
	}
	rightOrder := []string{
		`id="feedback-bug-btn"`,
		`class="nav-chip nav-chip--icon nav-chip--action layout-toggle"`,
		`id="oc-settings-btn"`,
		`id="oc-health"`,
		`id="welcome-topbar-btn"`,
		`id="oc-gh-avatar-wrap"`,
	}
	last := -1
	for _, want := range rightOrder {
		pos := strings.Index(right, want)
		if pos < 0 {
			t.Fatalf("topbar right zone missing %q", want)
		}
		if pos <= last {
			t.Fatalf("topbar right zone control %q is out of order", want)
		}
		last = pos
	}
}

func TestNavbarRemovedChipSpecificMetricOverrides(t *testing.T) {
	html := indexHTML(t)
	for _, forbidden := range []string{
		`class="hv-btn btn-secondary btn-sm feedback-bug-btn"`,
		`class="hv-btn btn-icon btn-sm layout-toggle" data-action="toggleLayout"`,
		`class="hv-btn btn-secondary btn btn-primary" id="welcome-topbar-btn"`,
		`#fleet-breaker-btn2 {
      --component-status`,
		`.oc-health-badge { padding:`,
		`.toast-action { margin-left: 10px; padding:`,
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("static dashboard still contains navbar-specific metric override/legacy class %q", forbidden)
		}
	}
	cssBytes, err := staticFS.ReadFile("static/components.css")
	if err != nil {
		t.Fatalf("reading embedded static/components.css: %v", err)
	}
	componentsCSS := string(cssBytes)
	tokenBytes, err := staticFS.ReadFile("static/tokens.css")
	if err != nil {
		t.Fatalf("reading embedded static/tokens.css: %v", err)
	}
	tokenCSS := string(tokenBytes)
	for _, want := range []string{
		`--nav-chip-h: 28px;`,
		`--nav-chip-radius: 999px;`,
		`--nav-chip-border: 1px;`,
		`--nav-chip-font-size: var(--fs-sm);`,
	} {
		if !strings.Contains(tokenCSS, want) {
			t.Errorf("shared navbar chip tokens are missing %q", want)
		}
	}
	for _, want := range []string{
		`.nav-chip {`,
		`.nav-chip--status`,
		`.nav-chip--action`,
		`.nav-chip--icon`,
		`.nav-chip--primary`,
		`.nav-chip--level`,
	} {
		if !strings.Contains(componentsCSS, want) {
			t.Errorf("shared navbar chip CSS is missing %q", want)
		}
	}
}
