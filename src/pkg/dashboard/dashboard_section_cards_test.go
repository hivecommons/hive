package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardSectionsUseSharedCardChrome(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function sectionCardHeader(opts)",
		"function sectionCardShell(opts, bodyHtml)",
		"function ensureSectionCard(sectionId)",
		"function refreshSectionCardShell(sectionId)",
		"function initializeDashboardSectionCards()",
		`data-section-card-header="1"`,
		"dash-card-summary",
		"refreshDashboardSectionSummaries(data)",
		"sectionHeaderKey",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("shared dashboard card chrome is missing %q", want)
		}
	}

	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	for _, sectionID := range dashboardLayoutTemplateIDs(t, html) {
		entry := dashboardSectionConfigEntry(t, config, sectionID)
		if !strings.Contains(entry, "summary:") {
			t.Fatalf("section %q card config lacks collapsed summary", sectionID)
		}
		if title := dashboardSectionConfigTitle(t, entry); !startsWithEmoji(title) {
			t.Fatalf("section %q title %q does not start with an emoji", sectionID, title)
		}
	}

	for _, fn := range []string{"function renderGovernor(gov, cadenceMatrix, data)", "function renderTokens(tokens)", "function renderCost(cost)"} {
		body := jsFunctionBody(t, html, fn)
		if !strings.Contains(body, "sectionCardShell({") {
			t.Fatalf("%s does not render through sectionCardShell", fn)
		}
		if !strings.Contains(body, "applySectionCollapse(") && !strings.Contains(body, "applyCostCollapse()") {
			t.Fatalf("%s does not re-apply persisted section collapse state", fn)
		}
	}
}

func TestDashboardSectionEmojisMatchSidebar(t *testing.T) {
	html := indexHTML(t)
	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	for sectionID, emoji := range sidebarSectionEmojis(html) {
		entry := dashboardSectionConfigEntry(t, config, sectionID)
		title := dashboardSectionConfigTitle(t, entry)
		if !strings.HasPrefix(title, emoji) {
			t.Fatalf("section %q title %q does not match sidebar emoji %q", sectionID, title, emoji)
		}
	}
}

func TestDashboardSectionTitlesMatchSidebarLabels(t *testing.T) {
	html := indexHTML(t)
	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	for sectionID, label := range sidebarSectionLabels(html) {
		entry := dashboardSectionConfigEntry(t, config, sectionID)
		title := dashboardSectionTitleLabel(dashboardSectionConfigTitle(t, entry))
		if !strings.EqualFold(title, label) {
			t.Fatalf("section %q title %q does not match sidebar label %q", sectionID, title, label)
		}
	}
}

func TestDashboardSectionCardActionsStopPropagationAndShareClass(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-action="openACMMDialog" data-stop="1"`,
		`id="acmm-refresh-btn" data-action="acmmForceRefresh" data-stop="1"`,
		`id="agents-compact-all-btn" data-action="toggleAllAgentsCompact" data-stop="1"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("header action is missing data-stop propagation guard: %s", want)
		}
	}
	settingsGearBody := jsFunctionBody(t, html, "function sectionSettingsGear(label, action, title, extraAttrs)")
	for _, want := range []string{
		`data-action="${esc(action)}"`,
		`data-stop="1"`,
		`aria-label="${esc(label)} settings"`,
	} {
		if !strings.Contains(settingsGearBody, want) {
			t.Fatalf("shared section settings gear is missing propagation/accessibility guard %q", want)
		}
	}
	for _, want := range []string{
		`id="repos-clear-pill-filter-btn" data-action="clearRepoPillFilter" data-stop="1"`,
		`id="repos-rescan-btn" data-action="reposForceRescan" data-stop="1"`,
		`id="repos-reset-layout-btn" data-action="resetRepoCardWidths" data-stop="1"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Projects toolbar action is missing data-stop propagation guard: %s", want)
		}
	}

	normalizeBody := jsFunctionBody(t, html, "function normalizeSectionCardChrome(sectionId)")
	for _, want := range []string{
		".dash-card-actions > *",
		"classList.add('dash-card-action')",
		".dash-card-badges > *",
		"classList.add('dash-card-badge')",
	} {
		if !strings.Contains(normalizeBody, want) {
			t.Fatalf("shared header action/badge normalization is missing %q", want)
		}
	}
	if !strings.Contains(html, ".dash-card.collapsed .dash-card-actions { display: none; }") {
		t.Fatal("collapsed dashboard cards must hide expanded-content header action buttons")
	}

	ensureBody := jsFunctionBody(t, html, "function ensureSectionCard(sectionId)")
	if !strings.Contains(ensureBody, `el.setAttribute('data-stop', '1')`) {
		t.Fatal("runtime-migrated header links/buttons are not forced to stop propagation")
	}
}

func TestDashboardSectionSettingsGearsRenderNextToHelp(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: dashboard section gear placement was not executed")
	}
	script := `const assert = require('node:assert/strict');
function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}
function textOrDash(v) { return String(v || '—'); }
function visualSectionSummary(_id, html) { return html; }
const DASHBOARD_HELP_PAGE = 'dashboard-sections';
const DASHBOARD_HELP_CLICK_LINE = 'Click (or press Enter) for the full explanation';
const DASHBOARD_SECTION_HELP = {
  'overview-section': { heading: 'Overview', help: 'Overview help.' },
  'governor': { heading: 'Governor', help: 'Governor help.' },
  'repos-section': { heading: 'Projects', help: 'Projects help.' },
  'nous-section': { heading: 'Strategy Lab', help: 'Strategy Lab help.' },
};
function dashboardDocsHref(page, anchor) { return '/docs/' + page + '.md' + (anchor ? '#' + anchor : ''); }
function dashboardSectionHelpAnchor(heading) { return String(heading || '').toLowerCase().replace(/[^\w\s-]/g, '').trim().replace(/\s/g, '-'); }
` + jsFunctionBody(t, html, "function dashboardSectionHelpMark(sectionId)") + "\n" +
		jsFunctionBody(t, html, "function sectionSettingsGear(label, action, title, extraAttrs)") + "\n" +
		jsFunctionBody(t, html, "function sectionCardHeader(opts)") + `
const cases = [
  ['overview-section', '🍩 Overview', sectionSettingsGear('Overview', 'toggleOverviewChartSettings', 'Configure Overview chart types, carousel timing, and transitions', ' aria-haspopup="true" aria-expanded="false" aria-controls="overview-chart-settings"')],
  ['governor', '📊 Governor', sectionSettingsGear('Governor', 'openConfigDialog', 'Open Governor configuration', ' data-arg0="governor"')],
  ['repos-section', '📦 Projects', sectionSettingsGear('Projects', 'gh1', 'Manage monitored projects')],
  ['nous-section', '🧪 Strategy Lab', sectionSettingsGear('Strategy Lab', 'openNousConfig', 'Strategy Lab Configuration')],
];
for (const [id, title, settingsHtml] of cases) {
  const header = sectionCardHeader({ id, title, settingsHtml });
  const m = header.match(/<span class="dash-card-title">([\s\S]*?)<\/span><span class="dash-card-badges">/);
  assert.ok(m, id + ' title cluster not found');
  const titleCluster = m[1];
  assert.ok(titleCluster.includes('section-help-mark'), id + ' lacks help mark');
  assert.ok(titleCluster.includes('section-settings-gear'), id + ' lacks settings gear in title cluster');
  assert.ok(titleCluster.indexOf('section-help-mark') < titleCluster.indexOf('section-settings-gear'), id + ' gear is not after help');
  assert.match(titleCluster, /<\/a><button type="button" class="config-info section-settings-gear"/, id + ' gear is not the next sibling after help');
  assert.doesNotMatch(header, /<span class="dash-card-actions">[\s\S]*(section-settings-gear|data-section-settings|⚙)/, id + ' gear rendered in far-right actions');
}
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node dashboard section gear placement failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestDashboardSectionSettingsGearsAvoidLegacyFarRightActions(t *testing.T) {
	html := indexHTML(t)
	for _, forbidden := range []string{
		".governor-card .dash-card-actions { margin-left: 0; }",
		`actionsHtml: '<button class="config-gear" data-action="openConfigDialog" data-arg0="governor"`,
		`class="hv-btn config-gear btn-sm" data-action="toggleOverviewChartSettings"`,
		`class="hv-btn config-gear btn-sm" data-action="gh1"`,
		`class="hv-btn btn-danger btn-sm" data-action="openNousConfig"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("legacy far-right section settings gear remains: %s", forbidden)
		}
	}
	for _, want := range []string{
		"function sectionSettingsGear(label, action, title, extraAttrs)",
		`settingsHtml: sectionSettingsGear('Overview', 'toggleOverviewChartSettings'`,
		`settingsHtml: sectionSettingsGear('Governor', 'openConfigDialog'`,
		`settingsHtml: sectionSettingsGear('Projects', 'gh1'`,
		`settingsHtml: sectionSettingsGear('Strategy Lab', 'openNousConfig'`,
		".section-settings-gear",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("shared section settings gear path missing %q", want)
		}
	}
}

func TestDashboardSectionSettingsGearRestoresCogInsideCircle(t *testing.T) {
	html := indexHTML(t)
	settingsGearBody := jsFunctionBody(t, html, "function sectionSettingsGear(label, action, title, extraAttrs)")
	for _, want := range []string{
		`<svg class="section-settings-gear-icon"`,
		`viewBox="0 0 24 24"`,
		`fill="currentColor"`,
		`aria-label="${esc(label)} settings"`,
	} {
		if !strings.Contains(settingsGearBody, want) {
			t.Fatalf("shared section settings gear is missing SVG icon contract %q", want)
		}
	}
	if strings.Contains(settingsGearBody, "⚙") {
		t.Fatal("shared section settings gear must use the inline SVG, not the emoji gear")
	}

	infoRule := cssRule(t, html, ".config-info")
	for _, want := range []string{
		"width: 15px",
		"height: 15px",
		"border-radius: 50%",
		"background: color-mix(in srgb, var(--blue) 18%, var(--panel))",
		"border: 1px solid color-mix(in srgb, var(--blue) 60%, transparent)",
	} {
		if !strings.Contains(infoRule, want) {
			t.Fatalf("section settings gear must keep the shared help-mark circle; .config-info missing %q in %s", want, infoRule)
		}
	}

	gearRule := cssRule(t, html, ".section-settings-gear")
	for _, want := range []string{
		"padding: var(--sp-0)",
		"margin-left: var(--sp-0)",
		"text-decoration: none",
	} {
		if !strings.Contains(gearRule, want) {
			t.Fatalf("section settings gear CSS is missing circle-compatible override %q in %s", want, gearRule)
		}
	}
	for _, forbidden := range []string{
		"width:",
		"height:",
		"background:",
		"border:",
	} {
		if strings.Contains(gearRule, forbidden) {
			t.Fatalf("section settings gear CSS must not override the shared help-mark circle with %q in %s", forbidden, gearRule)
		}
	}

	iconRule := cssRule(t, html, ".section-settings-gear-icon")
	for _, want := range []string{"width: 11px", "height: 11px", "color: var(--muted)"} {
		if !strings.Contains(iconRule, want) {
			t.Fatalf("section settings SVG icon CSS is missing %q in %s", want, iconRule)
		}
	}
	hoverIconRule := cssRule(t, html, ".section-settings-gear:hover .section-settings-gear-icon,\n    .section-settings-gear:focus-visible .section-settings-gear-icon")
	if !strings.Contains(hoverIconRule, "color: var(--bg)") {
		t.Fatalf("section settings SVG icon hover/focus CSS must follow the circular button contrast: %s", hoverIconRule)
	}
}

func TestDashboardSectionRenderersRefreshSharedShell(t *testing.T) {
	html := indexHTML(t)
	cases := map[string]string{
		"function renderRepos(repos)":               "repos-section",
		"function renderAgents(agents)":             "agents-section",
		"function acmmRenderCard()":                 "acmm-eval-section",
		"function renderApprovals(dto)":             "approvals-section",
		"function renderContributors(contributors)": "contributors-section",
		"function renderAuditTable(entries)":        "audit-section",
		"function renderReviewQueue(data)":          "review-queue-section",
		"function renderFAQ()":                      "faq-section",
		"function renderInception()":                "inception-section",
		"function renderKnowledge()":                "knowledge-section",
		"function renderDebugSection()":             "debug-section",
		"function renderLogs()":                     "logs-section",
	}
	for fn, sectionID := range cases {
		body := jsFunctionBody(t, html, fn)
		if !strings.Contains(body, "refreshSectionCardShell('"+sectionID+"')") {
			t.Fatalf("%s does not refresh shared shell for %s", fn, sectionID)
		}
	}
}

func TestDashboardSectionChromeHasNoSectionSpecificOverrides(t *testing.T) {
	html := indexHTML(t)
	for _, forbidden := range []string{
		".contributors-grip", ".agents-grip", ".contributors-title", ".agents-title",
		".contributors-chevron", ".agents-chevron",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("section-specific dashboard card chrome override remains: %s", forbidden)
		}
	}
	for _, want := range []string{
		".dash-card-header .dashboard-grip",
		".dash-card-header .section-chevron",
		".dash-card-title",
		".dash-card-action",
		".dash-card-badge",
		".dash-card.collapsed",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("shared dashboard card CSS is missing %q", want)
		}
	}
}

func TestDashboardCardPolishBatchStaticContracts(t *testing.T) {
	html := indexHTML(t)
	if strings.Contains(html, `'platform-section':`) || strings.Contains(html, `id="platform-section"`) || strings.Contains(strings.Join(dashboardLayoutTemplateIDs(t, html), ","), "platform-section") {
		t.Fatal("Platform must not be a top-level dashboard section")
	}
	for _, want := range []string{
		"function platformFactTiles(plat)",
		`class="platform-diagnostics"`,
		`#audit-panel.audit-resizable`,
		`resize: vertical`,
		`AUDIT_PANEL_HEIGHT_KEY = 'hive.audit.panel.height'`,
		`ResizeObserver`,
		`⚡ Powered by Spektacular`,
		`https://github.com/jumppad-labs/spektacular`,
		`Project inception runs on <a href="https://github.com/jumppad-labs/spektacular"`,
		`summary: '0 facts'`,
		"kbFactsLabel(kbTotalFactsFromStats",
		"No pull requests in the window yet.",
		"function lcJourneyHasContent(j)",
		"repo-header-badges",
		"repo-header-actions",
		`id="governor-pr-models-section"`,
		"PRS BY MODEL",
		"gov-pr-models-subtitle",
		"gov-pr-models-summary",
		"collapsedSummary",
		"applySectionCollapse('governor-pr-models-section')",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard polish contract missing %q", want)
		}
	}
	govCSS := html[strings.Index(html, "/* Governor uses the shared dash-card shell"):strings.Index(html, ".gov-title")]
	if strings.Contains(govCSS, "box-shadow") || strings.Contains(govCSS, "padding: var(--sp-6)") || strings.Contains(html, "body.light-mode .governor,") {
		t.Fatal("Governor outer container still draws card chrome instead of leaving it to .dash-card")
	}
	if regexp.MustCompile(`(?s)\.governor(?:\.[\w-]+)?\s*>\s*\.dash-card\.governor-card\s*\{[^}]*(?:border|outline|box-shadow)`).MatchString(html) {
		t.Fatal("Governor must not override shared dash-card border, outline, or shadow in any state")
	}
}

func TestGovernorPRModelsNestedSubsectionIsCollapsible(t *testing.T) {
	html := indexHTML(t)
	body := jsFunctionBody(t, html, "function renderGovernorPRModelsTile()")
	for _, want := range []string{
		`id="governor-pr-models-section"`,
		`class="section-label section-header-toggle gov-pr-models-subheader"`,
		`data-action="toggleSection"`,
		`data-keydown-action="sectionHeaderKey"`,
		`data-arg0="governor-pr-models-section"`,
		`PRS BY MODEL`,
		`gov-pr-models-subtitle`,
		`gov-pr-models-summary`,
		`#1 ${escapeHtml(topModel)} · ${modelCount} model`,
		`<span class="gov-pr-models-toggle" data-stop="1">${buttons}${sortButtons}</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Governor PRs-by-model subsection missing %q", want)
		}
	}
	renderGovernor := jsFunctionBody(t, html, "function renderGovernor(gov, cadenceMatrix, data)")
	if !strings.Contains(renderGovernor, "applySectionCollapse('governor-pr-models-section')") {
		t.Fatal("Governor render does not re-apply nested PRs-by-model collapse state")
	}
	apply := jsFunctionBody(t, html, "function applySectionCollapse(sectionId)")
	if !strings.Contains(apply, "section.classList.toggle('collapsed', collapsed)") {
		t.Fatal("nested subsection collapse state is not reflected on the section for collapsed summaries")
	}
}

func TestAdvisoryNestedSubsectionsUseSeparatedHeaderPattern(t *testing.T) {
	html := indexHTML(t)
	config := jsConstObject(t, html, "const DASHBOARD_SECTION_CARD_CONFIG")
	for _, id := range []string{"advisory-digest-section", "hive-advice-section", "fleet-report-section", "acmm-reco-section", "lifecycle-section", "pr-throughput-section"} {
		idx := strings.Index(html, `id="`+id+`"`)
		if idx < 0 {
			t.Fatalf("missing advisory subsection %s", id)
		}
		if !strings.Contains(config, "'"+id+"':") {
			t.Fatalf("%s is not registered for shared dashboard card chrome", id)
		}
		window := html[idx:]
		if len(window) > 700 {
			window = window[:700]
		}
		if !strings.Contains(window, "advisory-subsection") {
			t.Fatalf("%s does not use advisory-subsection spacing pattern", id)
		}
		if !strings.Contains(window, "advisory-subsection-card") {
			t.Fatalf("%s body card does not start below its header", id)
		}
	}
}

func TestDashboardNoticesContainReleaseAndPlanningAboveOverview(t *testing.T) {
	html := indexHTML(t)
	notices := strings.Index(html, `id="dash-notices"`)
	release := strings.Index(html, `id="release-status"`)
	planning := strings.Index(html, `id="planning-intro"`)
	overview := strings.Index(html, `data-dashboard-section="overview-section"`)
	if notices < 0 || release < notices || planning < notices || overview < 0 || release > overview || planning > overview {
		t.Fatalf("release status and planning notice must be inside pinned notices above Overview")
	}
}

func TestCostPanelUsesGenericSectionPersistence(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"if (sectionId === 'cost-panel' && localStorage.getItem('hive-cost-collapsed') === '1') return true;",
		"if (sectionId === 'cost-panel') {",
		"function isCostCollapsed() {\n      return isSectionCollapsed('cost-panel');",
		"function setCostCollapsed(collapsed) {\n      setSectionCollapsed('cost-panel', collapsed);",
		"action: 'toggleCostPanel'",
		"summaryClass: 'cost-collapsed-total'",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("cost panel did not converge on generic section persistence/component: missing %q", want)
		}
	}
}

func dashboardLayoutTemplateIDs(t *testing.T, html string) []string {
	t.Helper()
	re := regexp.MustCompile(`var DASHBOARD_LAYOUT_TEMPLATE=\{main:\[([^\]]+)\]\}`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("dashboard layout template not found")
	}
	idRe := regexp.MustCompile(`'([^']+)'`)
	matches := idRe.FindAllStringSubmatch(m[1], -1)
	if len(matches) == 0 {
		t.Fatal("dashboard layout template has no section ids")
	}
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		ids = append(ids, match[1])
	}
	return ids
}

func jsConstObject(t *testing.T, html, decl string) string {
	t.Helper()
	i := strings.Index(html, decl)
	if i < 0 {
		t.Fatalf("index.html has no %q", decl)
	}
	rest := html[i:]
	loc := regexp.MustCompile(`(?m)^    \}\);\n`).FindStringIndex(rest)
	if loc == nil {
		t.Fatalf("could not find the end of %q", decl)
	}
	return rest[:loc[1]]
}

func dashboardSectionConfigEntry(t *testing.T, config, sectionID string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta("'"+sectionID+"'") + `:\s*\{([^}]+)\}`)
	m := re.FindStringSubmatch(config)
	if m == nil {
		t.Fatalf("section %q is not registered for shared dashboard card chrome", sectionID)
	}
	return m[1]
}

func dashboardSectionConfigTitle(t *testing.T, entry string) string {
	t.Helper()
	re := regexp.MustCompile(`title:\s*'([^']+)'`)
	m := re.FindStringSubmatch(entry)
	if m == nil {
		t.Fatalf("section config entry has no title: %s", entry)
	}
	return m[1]
}

func sidebarSectionEmojis(html string) map[string]string {
	re := regexp.MustCompile(`data-section="([^"]+)"[^>]*>\s*<span class="oc-nav-emoji">([^<]+)</span>`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func sidebarSectionLabels(html string) map[string]string {
	re := regexp.MustCompile(`data-section="([^"]+)"[^>]*>\s*<span class="oc-nav-emoji">[^<]+</span><span class="oc-nav-text">([^<]+)</span>`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

func dashboardSectionTitleLabel(title string) string {
	title = strings.TrimSpace(title)
	for i, r := range title {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return strings.TrimSpace(title[i:])
		}
	}
	return ""
}

func startsWithEmoji(s string) bool {
	if s == "" {
		return false
	}
	r, _ := utf8DecodeRuneInString(s)
	return r > 0x2600
}

func utf8DecodeRuneInString(s string) (rune, int) {
	for i, r := range s {
		return r, i
	}
	return 0, 0
}

func TestSidebarDragHandlesSharePersistedOrder(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-nav-order-grip`,
		`oc-nav-hide-toggle`,
		`function dashboardSidebarLayoutFromDom()`,
		`function dashboardApplyOrderFromSidebar()`,
		`dashboardApplyLayout(layout);dashboardLayoutWrite()`,
		`function agentCardLayoutSetOrder(order, agents)`,
		`function agentSidebarSaveOrderFromDom()`,
		`agentSidebarSaveOrderFromDom();`,
		`ocUpdateSidebarAgents();`,
		`Alt+↑/↓`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sidebar/card order sharing is missing %q", want)
		}
	}

	dashboardApply := jsFunctionBody(t, html, "function dashboardApplyOrderFromSidebar()")
	for _, want := range []string{"dashboardSidebarLayoutFromDom()", "dashboardApplyLayout(layout)", "dashboardLayoutWrite()"} {
		if !strings.Contains(dashboardApply, want) {
			t.Fatalf("dashboard sidebar reorder does not update the persisted dashboard layout: missing %q", want)
		}
	}

	agentSidebar := jsFunctionBody(t, html, "function agentSidebarSaveOrderFromDom()")
	for _, want := range []string{".oc-nav-item[data-agent-nav]", "agentCardLayoutSetOrder(order, agents)", "renderAgents(window._lastAgents)"} {
		if !strings.Contains(agentSidebar, want) {
			t.Fatalf("agent sidebar reorder does not update the agent card layout store: missing %q", want)
		}
	}

	agentCard := jsFunctionBody(t, html, "function agentOrderSaveFromDom(grid)")
	if !strings.Contains(agentCard, "ocUpdateSidebarAgents()") {
		t.Fatal("agent card reorder does not refresh the sidebar from the same order")
	}
	applySnapshot := jsFunctionBody(t, html, "function dashboardApplySnapshotState(state)")
	if !strings.Contains(applySnapshot, "ocUpdateSidebarAgents()") {
		t.Fatal("restoring a saved layout does not refresh the agent sidebar order")
	}
}

func TestDashboardSidebarHideSectionsContract(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`var DASHBOARD_HIDDEN_SECTIONS_KEY='hive.dashboard.hiddenSections'`,
		`var DASHBOARD_HIDE_DENYLIST=['overview-section']`,
		`function dashboardCanHideSection(id)`,
		`function dashboardHiddenSectionsRead()`,
		`function dashboardSetSectionHidden(sectionId,hidden)`,
		`function toggleDashboardSectionHidden(sectionId)`,
		`.oc-nav-item.oc-nav-item-hidden`,
		`.dashboard-section-hidden { display: none !important; }`,
		`hide.setAttribute('data-action','toggleDashboardSectionHidden')`,
		`hide.setAttribute('aria-label','Hide '+label)`,
		`hide.setAttribute('aria-pressed','false')`,
		`hide.textContent='⊘'`,
		`btn.textContent=isHidden?'👁':'⊘'`,
		`item.setAttribute('aria-disabled','true')`,
		`btn.title=isHidden?'Unhide '+label:'Hide '+label`,
		`btn.setAttribute('aria-label',(isHidden?'Unhide ':'Hide ')+label)`,
		`btn.setAttribute('aria-pressed',isHidden?'true':'false')`,
		`if (dashboardSectionHidden(section)) dashboardSetSectionHidden(section,false);`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard hide sections contract missing %q", want)
		}
	}

	snapshot := jsFunctionBody(t, html, "function dashboardLayoutSnapshot()")
	if !strings.Contains(snapshot, "dashboardHiddenSectionsRead().forEach") || !strings.Contains(snapshot, "hidden:hidden") {
		t.Fatal("dashboard layout snapshots do not capture hidden sections")
	}
	normalize := jsFunctionBody(t, html, "function dashboardNormalizeSnapshot(value)")
	if !strings.Contains(normalize, "src.hidden") || !strings.Contains(normalize, "dashboardCanHideSection(id)") {
		t.Fatal("saved layout import/apply does not normalize hidden sections through the deny-list")
	}
	reset := jsFunctionBody(t, html, "async function resetDashboardLayout()")
	if !strings.Contains(reset, "localStorage.removeItem(DASHBOARD_HIDDEN_SECTIONS_KEY)") {
		t.Fatal("dashboard layout reset does not clear hidden sections")
	}

	applyHidden := jsFunctionBody(t, html, "function dashboardApplyHiddenSections()")
	for _, want := range []string{
		"item.classList.toggle('oc-nav-item-hidden',isHidden)",
		"item.setAttribute('aria-disabled','true')",
		"else item.removeAttribute('aria-disabled')",
		"btn.title=isHidden?'Unhide '+label:'Hide '+label",
		"btn.setAttribute('aria-label',(isHidden?'Unhide ':'Hide ')+label)",
		"btn.setAttribute('aria-pressed',isHidden?'true':'false')",
	} {
		if !strings.Contains(applyHidden, want) {
			t.Fatalf("dashboard hidden sidebar state does not cover both hide/unhide directions: missing %q", want)
		}
	}
	toggleHidden := jsFunctionBody(t, html, "function toggleDashboardSectionHidden(sectionId)")
	for _, want := range []string{
		"var wasHidden=dashboardSectionHidden(sectionId)",
		"dashboardSetSectionHidden(sectionId,!wasHidden)",
		"if(wasHidden)ocNavigate(sectionId)",
	} {
		if !strings.Contains(toggleHidden, want) {
			t.Fatalf("dashboard hide control is not wired as a two-way toggle: missing %q", want)
		}
	}

	if strings.Contains(html, "section.hidden=isHidden") {
		t.Fatal("dashboard hide state must not clear existing section-owned hidden attributes")
	}
}

func TestLayoutResetRestoresSidebarAndCardDefaults(t *testing.T) {
	html := indexHTML(t)
	reset := jsFunctionBody(t, html, "async function resetDashboardLayout()")
	for _, want := range []string{
		"localStorage.removeItem(DASHBOARD_LAYOUT_KEY)",
		"dashboardLayoutExtraKeys().forEach",
		"dashboardApplyLayout(dashboardLayoutNormalize(null))",
		"renderAgents(((window._lastStatus||{}).agents)||",
	} {
		if !strings.Contains(reset, want) {
			t.Fatalf("dashboard reset no longer restores shared layout defaults: missing %q", want)
		}
	}
	extraKeys := jsFunctionBody(t, html, "function dashboardLayoutExtraKeys()")
	if !strings.Contains(extraKeys, "hive-agent-card-layout:") {
		t.Fatal("dashboard reset does not clear the shared agent card/sidebar order key")
	}
	resetAgents := jsFunctionBody(t, html, "function resetAgentCardLayout()")
	for _, want := range []string{"localStorage.removeItem(agentLayoutHiveKey())", "renderAgents(window._lastAgents)", "ocUpdateSidebarAgents()"} {
		if !strings.Contains(resetAgents, want) {
			t.Fatalf("agent layout reset does not refresh both card and sidebar defaults: missing %q", want)
		}
	}
}
