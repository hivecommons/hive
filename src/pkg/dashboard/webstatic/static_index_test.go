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

func TestStaticAuditCollapsedSummaryUsesSharedSparklineAndRiskChip(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"function renderAuditCollapsedSummary(text, title)",
		"renderSparkline(null, sparkValues",
		"Audit entries per hour over the last 24h",
		"audit-entry-rate-spark",
		"audit-last-entry",
		"audit-today-count",
		"role=\"group\" aria-label=\"${escapeHtml(label)}\"",
		"audit-sensitive-chip ${chipClass}",
		"role=\"button\" tabindex=\"0\" data-action=\"showAuditSensitiveEntries\"",
		"aria-label=\"Show sensitive entries\"",
		"const chipClass = sensitive > 0 ? 'warning' : 'muted';",
		"window._auditSummary = _auditSummary;",
		"return renderAuditCollapsedSummary(text, title);",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard audit collapsed summary missing %q", want)
		}
	}
	for _, want := range []string{
		".audit-sensitive-chip.warning",
		".audit-sensitive-chip.muted",
		"id=\"audit-sensitive-filter\"",
		"function toggleAuditSensitiveFilter()",
		"function showAuditSensitiveEntries()",
		"fetch('/api/audit' + (_auditSensitiveOnly ? '?sensitive=1' : ''))",
		".audit-entry-sensitive-tag",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard audit sensitive chip CSS missing %q", want)
		}
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

func TestStaticPublicKnowledgeToggleWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`id="public-knowledge-header-control" data-public-knowledge-control="header"`,
		`id="public-knowledge-settings-control" data-public-knowledge-control="settings"`,
		`case 'Knowledge': return renderGovKnowledgeSharing();`,
		`fetch('/api/knowledge/public')`,
		`fetch('/api/knowledge/public', {`,
		`data-action="publicKnowledgeRequestToggle"`,
		`data-action="publicKnowledgeConfirmEnable"`,
		`data-action="publicKnowledgeCopyURL"`,
		`Anyone with the URL can read operational facts (types `,
		`Only the hive owner can change this`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("public knowledge toggle wiring missing %q", want)
		}
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

func TestStaticDashboardDefaultLayoutOrder(t *testing.T) {
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

	defaultOrder := "'overview-section','governor','runs-section','pr-throughput-section','repos-section','knowledge-section','review-queue-section','contributors-section','advisory-section','token-panel','cost-panel','inception-section','acmm-eval-section','debug-section','audit-section','approvals-section','nous-section','logs-section','agents-section','agent-activity-section','faq-section'"
	if !strings.Contains(html, defaultOrder) {
		t.Fatalf("dashboard default layout/reset order no longer matches sidebar nav; missing %q", defaultOrder)
	}
}

func TestStaticExperimentLabFeatureGateWiring(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`id="nous-section" data-dashboard-section="nous-section" data-feature="experiment_lab" hidden`,
		`data-section="nous-section" data-feature="experiment_lab" hidden`,
		`function experimentLabEnabled()`,
		`function applyDashboardFeatureVisibility()`,
		`if (!experimentLabEnabled()) return;`,
		`() => ensureNousPolling()`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard Experiment Lab feature gate missing %q", want)
		}
	}
}

// TestStaticNoStrategyLabCopy pins the Strategy Lab → Experiment Lab rename
// (RFC #11585 part 2, #11695): no user-facing copy, feature gate or helper in
// the static dashboard may still use the old name. The section id and the
// /api/nous/* routes keep their Nous names by decision.
func TestStaticNoStrategyLabCopy(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(body))
	for _, stale := range []string{"strategy lab", "strategy_lab", "strategylab", "strategy-lab"} {
		if idx := strings.Index(lower, stale); idx >= 0 {
			end := idx + 120
			if end > len(lower) {
				end = len(lower)
			}
			t.Fatalf("static dashboard still contains %q: %q", stale, lower[idx:end])
		}
	}
	for _, want := range []string{
		`<span class="oc-nav-text">Experiment Lab (nous)</span>`,
		`🧪 Experiment Lab (nous)`,
		`Experiment Lab Configuration`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("static dashboard missing Experiment Lab copy %q", want)
		}
	}
}

// TestStaticExperimentLabHeaderShowsResearchQuestionNotModePill pins #12089:
// the Experiment Lab header no longer renders the "OBSERVING · GOVERNOR"
// mode×scope pill left over from the first Nous integration (#330/#338). In
// its place the header carries the research question for the active scope
// (status.researchQuestion), escaped via textContent and truncated with the
// full text in a tooltip, and renders nothing at all when none is set.
func TestStaticExperimentLabHeaderShowsResearchQuestionNotModePill(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, stale := range []string{
		`nous-mode-badge`,
		"${modeLabels[mode] || mode} · ${scope}",
		"observe: 'Observing'",
		`badge.textContent = 'not configured'`,
	} {
		if strings.Contains(src, stale) {
			t.Fatalf("static dashboard still renders the retired Experiment Lab mode×scope pill: %q", stale)
		}
	}
	for _, want := range []string{
		`🧪 Experiment Lab (nous) <span id="nous-research-question" class="nous-research-question" hidden></span></h2>`,
		`#review-pipeline-count, #nous-research-question, #kb-status-badge`,
		`function renderNousResearchQuestion(value)`,
		`renderNousResearchQuestion(s.status === 'not_configured' ? '' : s.researchQuestion);`,
		`el.hidden = true;`,
		`el.textContent = q.length > NOUS_RESEARCH_QUESTION_MAX_CHARS`,
		`el.setAttribute('title', 'Research question: ' + q);`,
		`const nous = document.getElementById('nous-research-question');`,
		`.nous-research-question[hidden] { display: none !important; }`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("static dashboard missing Experiment Lab research-question header wiring %q", want)
		}
	}
	// The gear dialog must prefill the repo goal from where PUT /api/nous/config/goals
	// stores it (the lab is repo-only, #12207).
	for _, want := range []string{
		`const goals = cfg.goals || {};`,
		`goals.repoGoal || goals.repo || repoCamp.research_question`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("Experiment Lab config dialog does not read saved goals: %q", want)
		}
	}
}

// TestStaticExperimentLabTypeStatusChips pins the live ledger/principles
// rendering (RFC #11585 part 4, #11697): every principle and recent round
// shows the shared schema's type and status as status-badge chips keyed by
// the design system's data-status tones (no new inline styles, no native
// dialogs), and the history timeline is keyed by status rather than the
// old type/state mix.
func TestStaticExperimentLabTypeStatusChips(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`const NOUS_STATUS_TONES = {`,
		`pending: 'warn', active: 'info', completed: 'ok', aborted: 'error',`,
		`updated: 'info', pruned: 'neutral',`,
		`function nousChip(kind, value) {`,
		`<span class="status-badge nous-${kind}-chip" data-status="${tone}" data-nous-${kind}="${escapeHtml(value)}"`,
		`function nousEntryLabel(entry) {`,
		`html += nousChip('type', p.type) + nousChip('status', p.status);`,
		`html += nousChip('type', entry.type) + nousChip('status', entry.status);`,
		`<div class="nous-recent-rounds mt-4">`,
		`<div class="nous-principle nous-round">`,
		`data-nous-type="${escapeHtml(entry.type || '')}" data-nous-status="${escapeHtml(entry.status || '')}"`,
		`const color = typeColors[entry.type] || statusColors[entry.status] || 'var(--muted)';`,
		`const dryRuns = ledger.filter(e => e.type === 'dry_run')`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard Experiment Lab type/status rendering missing %q", want)
		}
	}
	// The pre-#11697 timeline keyed colours on entry.type alone, which
	// conflated kinds with states.
	if strings.Contains(html, `const color = colors[entry.type]`) {
		t.Fatal("static dashboard still keys the experiment timeline on entry.type alone")
	}
	// Chips must not open native dialogs or carry their own inline styles.
	start := strings.Index(html, `function nousChip(kind, value) {`)
	end := strings.Index(html[start:], "\n    }\n") + start
	chip := html[start:end]
	for _, forbidden := range []string{"style=", "alert(", "confirm(", "prompt("} {
		if strings.Contains(chip, forbidden) {
			t.Fatalf("nousChip must not use %q: %s", forbidden, chip)
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

func TestStaticVersionMenuEscapesSidebarClipping(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`.oc-version-menu { --oc-version-menu-width: min(640px, calc(100vw - 24px)); position: fixed;`,
		`.avatar-menu.oc-version-menu { position: fixed;`,
		`z-index: 10002`,
		`function positionVersionMenu()`,
		`const rect = chip.getBoundingClientRect`,
		`window.addEventListener('resize', positionVersionMenuIfOpen);`,
		`window.addEventListener('scroll', positionVersionMenuIfOpen, true);`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard version menu clipping fix missing %q", want)
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
		`id="acmm-badge" class="nav-chip nav-chip--level nav-chip--gold" data-action="openACMMDialog" aria-haspopup="dialog" aria-controls="acmm-overlay"`,
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

// TestStaticExperimentLabVerdictCard pins the verdict card and the manual
// Promote action (RFC #11585 part 6, #11699): the card renders the engine's
// recommendation as a status-badge chip keyed by the design system's tones,
// flags verdicts that were not measured against a baseline, shows recorded
// promotions, and offers two owner-only Promote buttons (Issue, Spektacular
// run) that go through the in-app confirm dialog and the audited
// POST /api/nous/promote. The old automatic output-mode strip is gone.
func TestStaticExperimentLabVerdictCard(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`const NOUS_VERDICT_TONES = { deploy: 'ok', deploy_with_caveats: 'warn', fall_back_to_baseline: 'error' };`,
		`const NOUS_PROMOTE_TARGETS = {`,
		`function renderNousVerdictCard(verdict, mode) {`,
		`html += renderNousVerdictCard(s.verdict, mode);`,
		`<div class="nous-card nous-verdict-card mt-5" data-nous-verdict="${escapeHtml(v.recommendation)}"`,
		`<span class="status-badge nous-verdict-chip" data-status="${tone}"`,
		`<span class="status-badge nous-verdict-unmeasured" data-status="warn"`,
		`not measured against baseline</span>`,
		`<ul class="nous-verdict-list ${cls}">`,
		`<div class="nous-verdict-promotion" data-nous-promoted="${escapeHtml(target)}">`,
		`data-action="nousPromote" data-arg0="${escapeHtml(target)}" data-nous-target="${escapeHtml(target)}"`,
		`${done ? ' disabled' : ''}`,
		`Promotion is manual, owner-only and audited.`,
		`async function nousPromote(target) {`,
		`const ok = await hiveConfirm({`,
		`await fetch('/api/nous/promote', {`,
		`body: JSON.stringify({ target }),`,
		`.nous-verdict-card { border-color: var(--purple); }`,
		`.nous-config-output-modes {`,
		`<div class="text-muted nous-config-output-note"`,
		`body: JSON.stringify({ autoHold, autoDoNotMerge: autoDnm }),`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard Experiment Lab verdict card missing %q", want)
		}
	}
	// The pre-#11699 automatic output modes (state-only / issue / issue+pr /
	// spektacular-run picked in config and acted on by Approve) are gone.
	for _, gone := range []string{
		`function selectNousOutputMode(`,
		`data-action="selectNousOutputMode"`,
		`{ id: 'state-only', label: 'State Only'`,
		`body: JSON.stringify({ mode: outputMode, autoHold`,
		`'Spektacular run already admitted: '`,
	} {
		if strings.Contains(html, gone) {
			t.Fatalf("static dashboard still carries the automatic output mode: %q", gone)
		}
	}
	// The card and the promote flow must not use native dialogs or inline styles.
	for _, fn := range []string{`function renderNousVerdictCard(verdict, mode) {`, `async function nousPromote(target) {`} {
		start := strings.Index(html, fn)
		end := strings.Index(html[start:], "\n    }\n") + start
		snippet := html[start:end]
		for _, forbidden := range []string{"style=", "alert(", "confirm(", "prompt("} {
			if forbidden == "confirm(" && strings.Contains(snippet, "hiveConfirm(") {
				snippet = strings.ReplaceAll(snippet, "hiveConfirm(", "hiveConfirm[")
			}
			if strings.Contains(snippet, forbidden) {
				t.Fatalf("%s must not use %q", fn, forbidden)
			}
		}
	}
}

// The Experiment Lab Configuration dialog ends with a Data section whose
// three resets go through POST /api/nous/reset with the in-app dialogs
// (RFC #11585, hivecommons/hive#12029).
func TestStaticExperimentLabResetSection(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		`const NOUS_RESET_SCOPES = {`,
		`function renderNousResetSection() {`,
		`h += renderNousResetSection();`,
		`<div class="nous-config-section nous-reset-section" data-nous-reset-section>`,
		`<strong data-nous-count="snapshots">`,
		`<strong data-nous-count="principles">`,
		`<strong data-nous-count="experiments">`,
		`data-action="nousReset" data-arg0="${escapeHtml(scope)}" data-nous-reset="${escapeHtml(scope)}"`,
		`${canReset ? '' : ' disabled'}`,
		`dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')`,
		`baseline: { label: 'Reset baseline'`,
		`knowledge: { label: 'Clear knowledge'`,
		`all: { label: 'Reset everything'`,
		`${meta.danger ? 'btn-danger' : 'btn-secondary'}`,
		`async function nousReset(scope) {`,
		`const typed = await hivePrompt({`,
		`validate: (v) => v === 'RESET' ? true : 'Type RESET (all capitals) to confirm.',`,
		`ok = await hiveConfirm({ title: meta.confirm, message: meta.message, confirmLabel: meta.ok, danger: true });`,
		`await fetch('/api/nous/reset', {`,
		`body: JSON.stringify({ scope, confirm: 'RESET' }),`,
		`} else if (r.status === 409) {`,
		`await fetchNousStatus();`,
		`const NOUS_RESET_SNAPSHOT_BLOAT_FACTOR = 10;`,
		`snapshots > target * NOUS_RESET_SNAPSHOT_BLOAT_FACTOR`,
		`data-nous-reset-hint`,
		`older snapshots are not pruned`,
		`https://github.com/hivecommons/hive/issues/12030`,
		`.nous-reset-section h3 { color: var(--red); }`,
		`.nous-reset-actions .hv-btn:disabled {`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard Experiment Lab reset section missing %q", want)
		}
	}
	// The section and the reset flow must not use native dialogs or inline styles.
	for _, fn := range []string{`function renderNousResetSection() {`, `async function nousReset(scope) {`} {
		start := strings.Index(html, fn)
		end := strings.Index(html[start:], "\n    }\n") + start
		snippet := html[start:end]
		snippet = strings.ReplaceAll(snippet, "hiveConfirm(", "hiveConfirm[")
		snippet = strings.ReplaceAll(snippet, "hivePrompt(", "hivePrompt[")
		for _, forbidden := range []string{"style=", "alert(", "confirm(", "prompt(", "window."} {
			if forbidden == "window." && strings.Contains(snippet, "window._hiveRole") {
				snippet = strings.ReplaceAll(snippet, "window._hiveRole", "hiveRole")
			}
			if strings.Contains(snippet, forbidden) {
				t.Fatalf("%s must not use %q", fn, forbidden)
			}
		}
	}
	// Every reset button carries a tooltip.
	start := strings.Index(html, `function renderNousResetSection() {`)
	end := strings.Index(html[start:], "\n    }\n") + start
	for _, line := range strings.Split(html[start:end], "\n") {
		if strings.Contains(line, "<button") && !strings.Contains(line, "title=") {
			t.Fatalf("reset button without tooltip: %s", strings.TrimSpace(line))
		}
	}
}
