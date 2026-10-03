package webstatic

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// testIndexBody is intentionally repetitive so the gzipped form is measurably
// smaller than the raw form — a positive control that compression actually
// happened rather than the raw bytes being relabeled.
var testIndexBody = []byte("<!DOCTYPE html><html>" + strings.Repeat("<div>hive dashboard</div>", 200) + "</html>")

func newTestIndex(t *testing.T) *IndexDocument {
	t.Helper()
	d := NewIndexDocument(testIndexBody)
	d.Precompress()
	if d.gzipped == nil {
		t.Fatal("gzip precompression failed for test body")
	}
	if len(d.gzipped) >= len(d.raw) {
		t.Fatalf("gzipped form (%d bytes) not smaller than raw (%d bytes)", len(d.gzipped), len(d.raw))
	}
	return d
}

func TestIndexDocumentServesGzipWhenAccepted(t *testing.T) {
	d := newTestIndex(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompressing body: %v", err)
	}
	if string(decoded) != string(testIndexBody) {
		t.Fatal("decompressed body does not round-trip to the raw document")
	}
}

func TestIndexDocumentServesIdentityWithoutAcceptEncoding(t *testing.T) {
	d := newTestIndex(t)
	for _, ae := range []string{"", "identity", "gzip;q=0", "br"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if ae != "" {
			req.Header.Set("Accept-Encoding", ae)
		}
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Accept-Encoding=%q: status = %d, want 200", ae, rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Accept-Encoding=%q: unexpected Content-Encoding %q", ae, got)
		}
		if rec.Body.String() != string(testIndexBody) {
			t.Fatalf("Accept-Encoding=%q: body is not the raw document", ae)
		}
	}
}

func TestIndexDocumentRevalidation(t *testing.T) {
	d := newTestIndex(t)

	// First load discovers the ETag.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("ETag = %q, want a quoted validator", etag)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}

	// Revalidation with the same ETag — including a weak-prefixed or listed
	// form — must produce an empty-body 304.
	for _, inm := range []string{etag, "W/" + etag, `"stale-etag", ` + etag, "*"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("If-None-Match", inm)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match=%q: status = %d, want 304", inm, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("If-None-Match=%q: 304 carried a %d-byte body", inm, rec.Body.Len())
		}
	}

	// A mismatched validator serves the full document.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", `"something-else"`)
	rec = httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("mismatched If-None-Match: status = %d, body = %d bytes; want 200 with body", rec.Code, rec.Body.Len())
	}
}

func TestIndexDocumentHeadHasNoBody(t *testing.T) {
	d := newTestIndex(t)
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD carried a %d-byte body", rec.Body.Len())
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"br;q=1.0, gzip;q=0.8", true},
		{"*", true},
		{"GZIP", true},
		{"", false},
		{"identity", false},
		{"gzip;q=0", false},
		{"gzip;q=0.0", false},
		{"br, deflate", false},
	}
	for _, c := range cases {
		if got := acceptsGzip(c.header); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

func TestStaticTerminalLinksRenewAssertionBeforeOpening(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const TERMINAL_ASSERTION_RENEW_PATH = '/api/terminal/assertion/renew';",
		"const TERMINAL_HANDOFF_PATH = '/api/terminal/handoff';",
		"async function createTerminalHandoff()",
		"async function dashboardTokenConfigured()",
		"async function openAuthenticatedLink",
		"case 'openAuthenticatedLink': e.preventDefault();",
		"async function renewTerminalAssertion()",
		"credentials: 'same-origin'",
		"case 'openTerminal': e.preventDefault(); openTerminal(agent, el.href); break;",
		"data-action=\"openTerminal\"",
		"openTerminal(name);",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard terminal renewal wiring missing %q", want)
		}
	}
	if strings.Contains(html, "window.open(terminalUrl(name), '_blank', 'noopener');") {
		t.Fatal("welcome terminal action still opens /terminal directly without renewing the assertion")
	}
	if strings.Contains(html, "token=${encodeURIComponent(t)}") || strings.Contains(html, "tokenParam") {
		t.Fatal("static dashboard still appends the shared token to URLs")
	}
}

// TestStaticTerminalHostedApexWiring pins the two hosted-hive terminal defects
// that shipped alongside the proxy's single-apex /terminal gate.
//
//  1. isHostedHiveHost() suffix-matched ONE hardcoded apex, so on the rebranded
//     hive.hivecommons.dev fleet renewTerminalAssertion() returned early, the
//     15-minute hive_terminal_assertion expired with nothing to refresh it, and
//     ttyd's next reconnect was rejected.
//  2. dashboardTokenConfigured() mapped EVERY non-OK response to "a shared token
//     IS configured". A hosted hive has no shared token and answers
//     /api/auth/token with 404, so when the handoff failed openTerminal() took
//     the tab.close() branch instead of falling back to the plain terminal URL —
//     the operator sees a tab flash open and vanish.
func TestStaticTerminalHostedApexWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const HOSTED_HIVE_HOST_SUFFIXES = ['.hive.kubestellar.io', '.hive.hivecommons.dev'];",
		"HOSTED_HIVE_HOST_SUFFIXES.some(suffix => host.endsWith(suffix))",
		"if (r.status === 404) return false;",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard hosted-hive terminal wiring missing %q", want)
		}
	}
	if strings.Contains(html, "endsWith('.hive.kubestellar.io')") {
		t.Fatal("isHostedHiveHost still hardcodes a single hosted apex; assertion renewal no-ops on every other apex")
	}
}

func TestStaticCadenceTableTogglesUsePowerSwitchMarkup(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"function pwrSwitchHtml(on, attrs, ariaLabel, title, extraClass = '')",
		"return pwrSwitchHtml(on, action, `${who} enabled`, title, `agent-power-switch${canEdit ? '' : ' readonly'}`);",
		"`<td class=\"power-col\">${agentPowerSwitchHtml(agentData)}</td>`",
		"<th class=\"power-col\" title=\"Agent power: 1 enabled, 0 disabled\">Power</th>",
		"const rowClass = agentDisabled ? ' class=\"agent-disabled\"' : '';",
		".agent-power-switch[data-agent=",
		"${pwrSwitchHtml(_enabled, ' data-action=\"toggleConfigSwitch\" data-section=\"general\" data-key=\"enabled\"'",
		"${pwrSwitchHtml(_continuous, ' id=\"cfg-agent-continuous\" data-action=\"toggleContinuousAllActiveModes\" data-arg-types=\"t\"'",
		"Continuous in all active modes",
		"<span class=\"pwr-mark\">1</span><span class=\"pwr-lever\"></span><span class=\"pwr-mark\">0</span>",
		".gov-matrix .power-col { width: 52px; white-space: nowrap; }",
		"data-stop=\"1\"",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard cadence table switch markup missing %q", want)
		}
	}
	for _, old := range []string{
		"<span class=\"config-toggle-switch gov-continuous-switch",
		"<div class=\"config-toggle-switch ${_continuous ? 'on' : ''}\" id=\"cfg-agent-continuous\"",
		"setAgentContinuous",
		"<th>Continuous</th>",
		".continuous-col",
	} {
		if strings.Contains(html, old) {
			t.Fatalf("continuous toggle still uses old horizontal config switch markup %q", old)
		}
	}
}

func TestStaticDashboardPRAuthorUserMenuWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`id="oc-gh-menu-pr-author"`,
		`id="oc-gh-menu-pr-author-copy"`,
		`data-action="copyPRAuthorFromMenu"`,
		"function renderPRAuthorInMenu()",
		"async function copyPRAuthorFromMenu()",
		"window._aiAuthor = cfg.ai_author_effective || '';",
		"renderPRAuthorInMenu();",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("PR author user-menu wiring missing %q", want)
		}
	}
	if strings.Contains(html, `<div class="gov-stat" title="PR AUTHOR`) {
		t.Fatal("governor strip still renders the PR AUTHOR tile")
	}
}

func TestStaticAddAgentDiscoverabilityWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`id="agents-add-btn"`,
		`data-action="openAddAgentDialog"`,
		`title="Add a new agent from a template or from scratch"`,
		`＋ Add agent`,
		`＋ Add your first agent`,
		`applyAgentQuickStart`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard Add agent discoverability wiring missing %q", want)
		}
	}
	if strings.Contains(html, `>+ agent</a>`) {
		t.Fatal("sidebar still exposes the low-contrast '+ agent' link")
	}
}

func TestStaticDashboardLayoutUserMenuWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`id="oc-gh-menu-layout-save"`,
		`id="oc-gh-menu-layout-saved"`,
		`id="oc-gh-menu-layout-list"`,
		`id="oc-gh-menu-layout-reset"`,
		`id="oc-gh-menu-layout-export"`,
		`id="oc-gh-menu-layout-import"`,
		`id="oc-gh-menu-layout-status"`,
		"function dashboardLayoutSnapshot",
		"function saveDashboardLayoutPreset",
		"function exportDashboardLayoutJSON",
		"function importDashboardLayoutJSON",
		"Layout: default",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard layout user-menu wiring missing %q", want)
		}
	}
	for _, gone := range []string{
		`id="dashboard-layout-reset"`,
		`id="dashboard-layout-reset-main"`,
		`>↕️ Reset layout</button>`,
	} {
		if strings.Contains(html, gone) {
			t.Fatalf("dashboard layout reset control should only live in the user menu; found %q", gone)
		}
	}
}

func TestStaticDashboardFAQDefaultsAfterOperationalSections(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	d := NewIndexDocument(body)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	html := rec.Body.String()

	agentsMarker := `id="agents-section" data-dashboard-section="agents-section"`
	faqMarker := `id="faq-section" data-dashboard-section="faq-section"`
	agentsIdx := strings.Index(html, agentsMarker)
	if agentsIdx < 0 {
		t.Fatalf("served dashboard missing last operational section marker %q", agentsMarker)
	}
	faqIdx := strings.Index(html, faqMarker)
	if faqIdx < 0 {
		t.Fatalf("served dashboard missing FAQ section marker %q", faqMarker)
	}
	if faqIdx < agentsIdx {
		t.Fatalf("FAQ section renders before the last operational section by default: faq=%d agents=%d", faqIdx, agentsIdx)
	}

	defaultOrderTail := "'logs-section','agents-section','faq-section']"
	if !strings.Contains(html, defaultOrderTail) {
		t.Fatalf("dashboard default layout/reset order no longer leaves FAQ last; missing %q", defaultOrderTail)
	}
}

func TestStaticStrategyLabFeatureGateWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`id="nous-section" data-dashboard-section="nous-section" data-feature="strategy_lab" hidden`,
		`data-section="nous-section" data-feature="strategy_lab" hidden`,
		`function strategyLabEnabled()`,
		`function applyDashboardFeatureVisibility()`,
		`if (!strategyLabEnabled()) return;`,
		`() => ensureNousPolling()`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard Strategy Lab feature gate missing %q", want)
		}
	}
}

// TestStaticPlanReviewWiring pins the plan-review view the governor PLANNING
// tile promises (#7537).
//
// The bug this guards against is specifically a DANGLING REFERENCE, not a
// missing feature: index.html already called openPlanReview(epic_id) from the
// ⧉ Plan flow, but the function was never defined anywhere, so the tooltip's
// instruction to "approve them in the plan view" was impossible to follow. A
// typeof guard at that call site means the dangling reference fails SILENTLY —
// and .github/scripts/check-inline-js.js deliberately exempts typeof-guarded
// no-undef hits (it uses openPlanReview as its own example), so the inline-JS
// linter cannot catch a regression here. Nothing else in the suite reads this
// markup, so without this test the entire frontend half of the feature can be
// deleted with every gate still green.
func TestStaticPlanReviewWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		// The listing and the drill-in must both be DEFINED, not just called.
		"async function openPlanList()",
		"async function openPlanReview(",
		"const res = await fetch('/api/plans');",
		// Approve/reject/retag are what make the gate more than decorative.
		"async function planGateAction(epicID, verb)",
		"function planApprove(epicID)",
		"function planReject(epicID)",
		"function planChildRemove(epicID, childID)",
		"function planCloseModal()",
		// The PLANNING tile must actually navigate somewhere.
		`data-action="openPlanList"`,
		`data-action="openPlanReview"`,
		`data-action="planApprove"`,
		`data-action="planReject"`,
		`data-action="planCloseModal"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("plan-review wiring missing %q — the PLANNING tile promises a plan view that would not exist", want)
		}
	}
	// The pre-existing call site must keep working: it is guarded by typeof, so
	// if the definition disappears again the flow silently does nothing.
	if !strings.Contains(html, "openPlanReview(data.epic_id)") {
		t.Fatal("the ⧉ Plan flow no longer calls openPlanReview(data.epic_id)")
	}
}

func TestStaticRunDetailTranscriptRenderingWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"function renderRunStageInterview(st)",
		"function renderRunStageDocuments(st)",
		"doc.markdown || doc.content || ''",
		"st.agent_stdout_stderr",
		"Agent transcript",
		"Interview / Q&amp;A",
		"Raw receipt JSON",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("run detail transcript rendering missing %q", want)
		}
	}
}

func TestStaticCadenceMatrixNameOpensAgentCard(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`data-action="ocSelectAgent" data-agent="${esc(aname)}" title="Open ${esc(displayLabel)}'s agent card"`,
		`data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(aname)}" data-tab="Cadences"`,
		`.gov-matrix td[data-tab="Cadences"], .gov-matrix .gov-agent-name-link { cursor: pointer; }`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("cadence matrix name/cadence wiring missing %q", want)
		}
	}
	for _, banned := range []string{
		"return `<tr style=\"cursor:pointer\"",
		"return `<tr data-action=\"openConfigDialog\"",
		"return `<tr data-tab=\"Cadences\"",
	} {
		if strings.Contains(html, banned) {
			t.Fatalf("cadence matrix row still carries click-target wiring %q", banned)
		}
	}
}

func TestNotificationsTabRendersEventCheckboxesAndSlack(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"HIVE_NOTIFICATION_EVENTS",
		"notification-event-checkbox",
		"markNotificationEventsDirty()",
		"slackWebhook",
		"Slack Webhook",
		"sweep_completed",
		"stage_completed",
		"issue_claimed",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("notifications UI missing %q", want)
		}
	}
}

func TestStaticIndexRepoTileReorderHandle(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const REPO_ORDER_KEY = 'hive.repoOrder.v1';",
		`class="dashboard-grip repo-card-order-handle" draggable="true" data-repo-order-grip`,
		`aria-label="Drag to reorder" title="Drag to reorder"`,
		"function repoOrderApply(repos)",
		"repoOrderSaveFromDom(grid);",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard repo tile reorder wiring missing %q", want)
		}
	}
}

func TestStaticTopbarACMMAndSidebarVersionPlacement(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	topbarStart := strings.Index(html, `<header id="oc-topbar"`)
	if topbarStart < 0 {
		t.Fatal("static dashboard is missing the light topbar")
	}
	versionPos := strings.Index(html, `id="oc-git-version"`)
	if versionPos < 0 {
		t.Fatal("static dashboard is missing the sidebar version chip container")
	}
	if versionPos > topbarStart {
		t.Fatal("#oc-git-version moved back into/after the topbar; version details belong in the sidebar footer")
	}
	topbarEnd := strings.Index(html[topbarStart:], `</header>`)
	if topbarEnd < 0 {
		t.Fatal("static dashboard topbar is not closed")
	}
	topbar := html[topbarStart : topbarStart+topbarEnd]
	for _, want := range []string{
		`id="acmm-badge" class="nav-chip nav-chip--level nav-chip--gold" data-action="ocNavigate" data-arg0="acmm-eval-section"`,
		`id="oc-version-chip"`,
		`id="oc-version-menu"`,
		`aria-expanded="false" aria-controls="oc-version-menu"`,
	} {
		haystack := html
		if strings.Contains(want, "acmm-badge") {
			haystack = topbar
		}
		if !strings.Contains(haystack, want) {
			t.Fatalf("static dashboard placement missing %q", want)
		}
	}
	if strings.Contains(topbar, `id="oc-git-version"`) || strings.Contains(topbar, `id="spoke-upgrade-btn"`) {
		t.Fatal("topbar still contains the inline version/upgrade strip")
	}
}
