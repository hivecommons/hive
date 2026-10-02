package dashboard

import (
	"strings"
	"testing"
)

func TestDashboardNoticesPinnedAboveReorderableSections(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<div id="dashboard-notices">`,
		`<div id="release-status"`,
		`id="gh-app-install-banner"`,
		`id="repo-target-banner"`,
		`id="hub-banner"`,
		`id="planning-intro"`,
		`id="system-alerts-banner"`,
		`var DASHBOARD_LAYOUT_ANCHOR_ID='dashboard-notices';`,
		`function dashboardPinNoticeAnchor(region)`,
		`function dashboardTopAnchor(region)`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard notices pinning is missing %q", want)
		}
	}

	notices := strings.Index(html, `id="dashboard-notices"`)
	firstSection := strings.Index(html, `data-dashboard-section="overview-section"`)
	if notices < 0 || firstSection < 0 || notices > firstSection {
		t.Fatalf("dashboard notices wrapper is not before the first reorderable section")
	}

	for _, fn := range []string{
		"function dashboardFirstSection(region)",
		"function dashboardInsertSection(region,card,before)",
		"function dashboardApplyLayout(layout)",
		"function dashboardMoveCardByKeyboard(card,key)",
	} {
		body := jsFunctionBody(t, html, fn)
		if !strings.Contains(body, "dashboardPinNoticeAnchor(region)") && !strings.Contains(body, "dashboardInsertSection(") {
			t.Fatalf("%s does not route section placement through the notices anchor guard", fn)
		}
	}
	if strings.Contains(html, "region.insertBefore(card,dashboardFirstSection(region))") {
		t.Fatal("dashboardApplyLayout still bypasses dashboardInsertSection and can place sections before notices")
	}
}

func TestDashboardFocusedAgentRenderedFirstWithoutPersistingOrder(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function agentsFocusedFirst(agents)",
		"const ordered = Array.isArray(agents) ? agents.slice() : [];",
		"const idx = ordered.findIndex(a => a && a.name === _ocSelectedAgent);",
		"ordered.unshift(focused);",
		"const cards = agentsFocusedFirst(orderedAgents).map(a => {",
		"if (window._lastAgents) renderAgents(window._lastAgents);",
		"card.classList.toggle('oc-focused', selected);",
		"card.classList.remove('oc-hidden');",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("focused-agent first-card wiring is missing %q", want)
		}
	}

	focus := jsFunctionBody(t, html, "function agentsFocusedFirst(agents)")
	if strings.Contains(focus, "localStorage") || strings.Contains(focus, "_saveSidebarLayout") || strings.Contains(focus, "fetch('/api/config/sidebar'") {
		t.Fatal("focused agent card ordering must be a view-only reorder, not persisted")
	}
	render := jsFunctionBody(t, html, "function renderAgents(agents)")
	if !strings.Contains(render, "agentsFocusedFirst(orderedAgents).map") {
		t.Fatal("renderAgents does not keep the selected agent first across status re-renders")
	}
}
