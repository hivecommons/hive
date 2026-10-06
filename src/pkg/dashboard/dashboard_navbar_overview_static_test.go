package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func TestDashboardNavbarStaticMarkupAndReferences(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		`<header id="oc-topbar" class="oc-topbar">`,
		`<div class="oc-topbar-left">`,
		`<div class="oc-topbar-center">`,
		`<div class="oc-topbar-right">`,
		`id="oc-project-name"`,
		`id="acmm-badge"`,
		`id="fleet-breaker-btn2"`,
		`id="agent-navbar-upnext"`,
		`id="feedback-bug-btn"`,
		`id="oc-settings-btn"`,
		`id="oc-health"`,
		`id="welcome-topbar-btn"`,
		`id="oc-gh-avatar-wrap"`,
		`position: fixed; top: var(--sp-0); left: var(--sidebar-w); right: var(--sp-0);`,
		`padding: calc(var(--navbar-sticky-height) + var(--sp-5)) var(--sp-7) 84px calc(var(--sidebar-w) + var(--sp-7));`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static/index.html missing navbar invariant %q", want)
		}
	}

	ids := map[string]bool{}
	for _, match := range regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		ids[match[1]] = true
	}
	for _, fn := range []string{"function renderAgentNavbarUpNext", "function refreshAgentNavbarUpNextCountdowns"} {
		start := strings.Index(html, fn)
		if start < 0 {
			t.Fatalf("static/index.html missing %s", fn)
		}
		end := strings.Index(html[start:], "\n    function ")
		if end < 0 {
			t.Fatalf("could not isolate %s body", fn)
		}
		block := html[start : start+end]
		for _, match := range regexp.MustCompile(`getElementById\('([^']+)'\)`).FindAllStringSubmatch(block, -1) {
			if !ids[match[1]] {
				t.Fatalf("%s references missing id %q", fn, match[1])
			}
		}
	}
}

func TestDashboardOverviewKPIGridAndTooltipMarkup(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		`<div class="overview-kpis">`,
		`{ key: 'held', field: 'overviewHeld'`,
		`{ key: 'blocked-needs-human', field: 'overviewBlockedHuman'`,
		`<div class="overview-kpi" role="button" tabindex="0"`,
		`data-keydown-action="ocNavigate"`,
		`<span class="overview-kpi-subline">${card.subline}</span>`,
		`class="config-tooltip overview-partition-tooltip" role="tooltip"`,
		`<span class="config-info overview-term-info"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static/index.html missing Overview invariant %q", want)
		}
	}
	if strings.Contains(html, `<button type="button" class="overview-kpi"`) {
		t.Fatal("Overview KPI cards must not be buttons because their tooltip breakdown contains controls")
	}
}
