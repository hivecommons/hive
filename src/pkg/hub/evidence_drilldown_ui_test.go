package hub

import (
	"encoding/json"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The Fleet quadrant card → evidence drill-down (#12117) lives in the inline
// dashboardHTML JavaScript. Like upgrade_filter_agreement_test.go, these tests
// extract the SHIPPED functions and run them under node, so the markup checked
// here is the markup the hub serves.

// evidenceHarness is the shipped evidence renderers plus string-only stand-ins
// for the DOM-backed esc/escAttr helpers.
func evidenceHarness(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		"function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }",
		"function escAttr(s) { return esc(s).replace(/\"/g, '&quot;').replace(/'/g, '&#39;'); }",
		countryJSDecl(t, "EVIDENCE_NULL"),
		countryJSDecl(t, "EVIDENCE_FAMILY_MIXES"),
		countryJSDecl(t, "EVIDENCE_MIN_N"),
		countryJSDecl(t, "EVIDENCE_AUTHOR_KINDS"),
		countryJSDecl(t, "EVIDENCE_SHADOW_GATES"),
		countryJSDecl(t, "_evidenceLast"),
		countryJSDecl(t, "_evidenceFilter"),
		countryJSDecl(t, "EVIDENCE_COVERAGE_STRONG"),
		countryJSDecl(t, "EVIDENCE_COVERAGE_FAIR"),
		countryJSDecl(t, "EVIDENCE_ERR_STRONG"),
		countryJSDecl(t, "EVIDENCE_ERR_FAIR"),
		countryJSDecl(t, "EVIDENCE_REVERT_STRONG"),
		countryJSDecl(t, "EVIDENCE_REVERT_FAIR"),
		countryJSDecl(t, "EVIDENCE_SHADOW_FALSE_ALARM_STRONG"),
		countryJSDecl(t, "EVIDENCE_SHADOW_FALSE_ALARM_FAIR"),
		countryJSDecl(t, "EVIDENCE_REPORTING_STRONG"),
		countryJSDecl(t, "EVIDENCE_REPORTING_FAIR"),
		countryJSDecl(t, "EVIDENCE_DATA_SINCE"),
		countryJSDecl(t, "EVIDENCE_REVERTS_SINCE"),
		countryJSDecl(t, "EVIDENCE_GRADE_LABELS"),
		countryJSDecl(t, "EVIDENCE_CI_NOTE"),
		countryJSDecl(t, "EVIDENCE_UPGRADE_BUILD"),
		countryJSDecl(t, "EVIDENCE_UPGRADE_NOTE"),
		countryJSDecl(t, "EVIDENCE_CONFIDENCE_RANGES"),
		countryJSDecl(t, "EVIDENCE_MARGIN_TIP"),
		countryJSDecl(t, "EVIDENCE_GLOSS_FALSE_APPROVE"),
		countryJSDecl(t, "EVIDENCE_GLOSS_FALSE_BLOCK"),
		countryJSDecl(t, "EVIDENCE_CRITERION_STATUSES"),
		countryJSDecl(t, "EVIDENCE_FINDINGS_ORDER"),
		countryJSDecl(t, "EVIDENCE_WINDOWS"),
		countryJSDecl(t, "EVIDENCE_WINDOW_DEFAULT"),
		countryJSDecl(t, "EVIDENCE_WINDOW_STORAGE_KEY"),
		countryJSDecl(t, "EVIDENCE_WINDOW_MAX_DAYS"),
		countryJSDecl(t, "EVIDENCE_MONTHS"),
		countryJSDecl(t, "_evidenceWindow"),
		countryJSDecl(t, "_evidenceCustomOpen"),
		jsFunc(t, "evidenceSectionHead"),
		jsFunc(t, "evidenceGradeHTML"),
		jsFunc(t, "evidenceGradeLow"),
		jsFunc(t, "evidenceGradeHigh"),
		jsFunc(t, "evidenceRateText"),
		jsFunc(t, "evidenceWinShort"),
		jsFunc(t, "evidenceExplainer"),
		jsFunc(t, "evidenceExplainerHTML"),
		jsFunc(t, "evidenceCloseExplainers"),
		jsFunc(t, "evidenceToggleExplainer"),
		jsFunc(t, "evidenceWindowValid"),
		jsFunc(t, "evidenceLoadWindow"),
		jsFunc(t, "evidenceSaveWindow"),
		jsFunc(t, "evidenceWindowQuery"),
		jsFunc(t, "evidenceDateLabel"),
		jsFunc(t, "evidenceWindowLabel"),
		jsFunc(t, "evidenceWindowChip"),
		jsFunc(t, "evidenceWindowControlsHTML"),
		jsFunc(t, "evidenceSetWindow"),
		jsFunc(t, "evidenceValidateRange"),
		jsFunc(t, "evidenceApplyCustomRange"),
		jsFunc(t, "evidenceSetTitle"),
		jsFunc(t, "evidencePct"),
		jsFunc(t, "evidenceInterval"),
		jsFunc(t, "evidenceCount"),
		jsFunc(t, "evidenceRateCells"),
		jsFunc(t, "evidenceRateHeads"),
		jsFunc(t, "evidenceTable"),
		jsFunc(t, "evidenceFindingLine"),
		jsFunc(t, "evidenceConfidenceLabel"),
		jsFunc(t, "evidenceConfidenceRows"),
		jsFunc(t, "evidencePlural"),
		jsFunc(t, "evidenceRateCount"),
		jsFunc(t, "evidenceWindowText"),
		jsFunc(t, "evidenceHiveName"),
		jsFunc(t, "evidenceLevelName"),
		jsFunc(t, "evidenceFindingsByKey"),
		jsFunc(t, "evidenceFindings"),
		jsFunc(t, "evidenceWaiting"),
		jsFunc(t, "evidenceFindingsHTML"),
		jsFunc(t, "evidenceWaitingHTML"),
		jsFunc(t, "evidenceReportQuery"),
		jsFunc(t, "evidenceReportLinksHTML"),
		jsFunc(t, "evidenceHiveLabel"),
		jsFunc(t, "evidenceWilson"),
		jsFunc(t, "evidenceRateOf"),
		jsFunc(t, "evidenceFilterActive"),
		jsFunc(t, "evidenceFilterHives"),
		jsFunc(t, "evidenceOutcomeAdd"),
		jsFunc(t, "evidenceOutcomeZero"),
		jsFunc(t, "evidenceLevelTimeToMerge"),
		jsFunc(t, "evidenceDuration"),
		jsFunc(t, "evidenceTimeToMergeCell"),
		jsFunc(t, "evidenceRepool"),
		jsFunc(t, "evidenceChip"),
		jsFunc(t, "evidenceChipsHTML"),
		jsFunc(t, "evidenceToggleHive"),
		jsFunc(t, "evidenceClearHives"),
		jsFunc(t, "evidenceSetLevel"),
		jsFunc(t, "evidenceRerender"),
		jsFunc(t, "evidenceSvgLabel"),
		jsFunc(t, "evidenceOutcomeBarsSvg"),
		jsFunc(t, "evidenceOutcomesPanel"),
		jsFunc(t, "evidenceWriteCountersSvg"),
		jsFunc(t, "evidenceWriteCountersPanel"),
		jsFunc(t, "evidenceShadowPanel"),
		jsFunc(t, "evidenceCriterionText"),
		jsFunc(t, "evidenceCriteriaPanel"),
		jsFunc(t, "evidenceViewHTML"),
		jsFunc(t, "evidencePanelHTML"),
	}, "\n")
}

// runEvidenceJS runs src (which must assign `result`, possibly asynchronously
// via the `done` promise) and unmarshals the JSON it prints.
func runEvidenceJS(t *testing.T, src string, out interface{}) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	script := "var result;\nvar done = Promise.resolve();\n" + src +
		"\nPromise.resolve(done).then(function(){ process.stdout.write(JSON.stringify(result)); })" +
		".catch(function(e){ console.error(e); process.exit(1); });\n"
	stdout, err := exec.Command("node", "-e", script).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Fatalf("node failed: %v", err)
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		t.Fatalf("bad JSON from node: %v\ngot: %s", err, stdout)
	}
}

// TestFleetQuadrantCardDrillGatedOnAdmin: admins get a keyboard-reachable
// button into the evidence view; members get today's card with no affordance.
func TestFleetQuadrantCardDrillGatedOnAdmin(t *testing.T) {
	src := strings.Join([]string{
		"function esc(s) { return String(s == null ? '' : s); }",
		"var QUADRANT_AXES = ['trust', 'productivity', 'satisfaction', 'efficiency'];",
		"var QUADRANT_AXIS_LABELS = {trust:'Trust', productivity:'Productivity', satisfaction:'Satisfaction', efficiency:'Efficiency'};",
		"function quadrantAxis(q, name) { return {axis: name, scored: true, score: 50}; }",
		"function quadrantSVG() { return '<svg></svg>'; }",
		"function quadrantAxisTileHTML(a) { return '<div class=\"quadrant-axis\">' + a.axis + '</div>'; }",
		"function quadrantSummaryHTML() { return ''; }",
		"var _allDashHives = [{}, {}];",
		"var _fleetQuadrant = {scored_axes: 4};",
		"var _isAdmin = false;",
		"function activeFilterCount() { return 0; }",
		jsFunc(t, "fleetQuadrantScopeLabel"),
		jsFunc(t, "fleetQuadrantDrillAttrs"),
		jsFunc(t, "fleetQuadrantEvidenceButtonHTML"),
		jsFunc(t, "fleetQuadrantHintHTML"),
		jsFunc(t, "fleetQuadrantHeaderHTML"),
		"_isAdmin = true; var admin = fleetQuadrantHeaderHTML();",
		"_isAdmin = false; var member = fleetQuadrantHeaderHTML();",
		"result = {admin: admin, member: member};",
	}, "\n")
	var got struct{ Admin, Member string }
	runEvidenceJS(t, src, &got)

	for _, want := range []string{
		`role="button"`,
		`tabindex="0"`,
		`aria-haspopup="dialog"`,
		`aria-controls="evidence-modal"`,
		`onclick="openEvidenceModal()"`,
		"event.key==='Enter'",
		`class="fleet-quadrant-drill"`,
		// Explicit affordance beside the title (user feedback: the clickable
		// chart alone was not discoverable), plus a hint line under the axes.
		`<button type="button" class="fleet-quadrant-evidence-btn" aria-haspopup="dialog" aria-controls="evidence-modal"`,
		`onclick="event.stopPropagation();openEvidenceModal()">🔬 Evidence</button>`,
		`<div class="fleet-quadrant-hint">Click the chart or Evidence to open the fleet evidence details</div>`,
	} {
		if !strings.Contains(got.Admin, want) {
			t.Errorf("admin fleet card is missing %q:\n%s", want, got.Admin)
		}
	}
	if strings.Count(got.Admin, "openEvidenceModal()") != 3 {
		t.Errorf("admin fleet card should wire the drill click, its keydown and the button, got:\n%s", got.Admin)
	}
	for _, forbidden := range []string{"openEvidenceModal", `role="button"`, "tabindex", "fleet-quadrant-drill", "onkeydown", "fleet-quadrant-evidence-btn", "fleet-quadrant-hint"} {
		if strings.Contains(got.Member, forbidden) {
			t.Errorf("member fleet card must stay unchanged but contains %q:\n%s", forbidden, got.Member)
		}
	}
	if !strings.Contains(got.Member, "Fleet quadrant · all 2 hives") {
		t.Errorf("member fleet card lost its content:\n%s", got.Member)
	}
}

// TestFleetQuadrantFallbackLinkWhenNothingScored: the card is the only entry
// point to the evidence view, and it is not drawn until an axis is scored, so
// admins get a one-line link in its place; members still get nothing. The
// header keeps returning ” on its own (TestFleetQuadrantHeaderWiring pins
// that), so the link is mounted beside it and the pair is rendered here the
// way the hives container does.
func TestFleetQuadrantFallbackLinkWhenNothingScored(t *testing.T) {
	if !strings.Contains(dashboardHTML, "fleetQuadrantHeaderHTML() + fleetQuadrantFallbackHTML() +") {
		t.Fatal("fleetQuadrantFallbackHTML is not mounted right after the fleet quadrant header")
	}
	src := strings.Join([]string{
		"function esc(s) { return String(s == null ? '' : s); }",
		"var _fleetQuadrant = {scored_axes: 0};",
		"var _isAdmin = true;",
		jsFunc(t, "fleetQuadrantDrillAttrs"),
		jsFunc(t, "evidenceFallbackLinkHTML"),
		jsFunc(t, "fleetQuadrantFallbackHTML"),
		jsFunc(t, "fleetQuadrantHeaderHTML"),
		"function block() { return fleetQuadrantHeaderHTML() + fleetQuadrantFallbackHTML(); }",
		"var admin = block();",
		"_fleetQuadrant = null; var adminNull = block();",
		"_isAdmin = false; var member = block();",
		"result = {admin: admin, adminNull: adminNull, member: member};",
	}, "\n")
	var got struct{ Admin, AdminNull, Member string }
	runEvidenceJS(t, src, &got)

	for _, html := range []string{got.Admin, got.AdminNull} {
		for _, want := range []string{
			`class="evidence-fallback"`,
			`<button type="button" class="evidence-fallback-link"`,
			`aria-controls="evidence-modal"`,
			`onclick="openEvidenceModal()"`,
			"not enough data",
		} {
			if !strings.Contains(html, want) {
				t.Errorf("admin fallback link is missing %q:\n%s", want, html)
			}
		}
		if strings.Contains(html, "<svg") || strings.Contains(html, "fleet-quadrant-drill") {
			t.Errorf("fallback must not draw the quadrant card:\n%s", html)
		}
	}
	if got.Member != "" {
		t.Errorf("member must get nothing when no axis is scored, got:\n%s", got.Member)
	}
}

const evidenceFixtureJS = `{
  spokes: 3, reporting: 2,
  revert_rate_by_author_kind: {human: {n: 0, rate: null}, agent: {n: 20, rate: 0.05, lo: 0.0089, hi: 0.236}, bot: {n: 2, rate: null}},
  calibration: [
    {bucket: 0, n: 40, rate: 0.1, lo: 0.04, hi: 0.23},
    {bucket: 1, n: 4, rate: null},
    {bucket: 2, n: 0, rate: null}, {bucket: 3, n: 0, rate: null},
    {bucket: 4, n: 0, rate: null}, {bucket: 5, n: 0, rate: null}
  ],
  by_acmm_level: [{level: 4, hives: 2, merged_90d: 30, revert_rate: {n: 30, rate: 0.1, lo: 0.0346, hi: 0.2563}}],
  family_false_approve_rate: {
    same: {n: 25, rate: 0.12, lo: 0.0417, hi: 0.3},
    cross: {n: 3, rate: null},
    mixed: {n: 0, rate: null}
  },
  record_coverage: {n: 50, rate: 0.8, lo: 0.67, hi: 0.89},
  hives: [
    {id: 'alpha', name: 'Alpha <b>', reported: true, acmm_level: 4, family_mix: 'same',
     verdicts_90d: 25, merged_90d: 20,
     record_coverage: {n: 20, rate: 1, lo: 0.84, hi: 1},
     false_approve_rate: {n: 25, rate: 0.12, lo: 0.0417, hi: 0.3},
     false_block_rate: {n: 25, rate: 0.04, lo: 0.007, hi: 0.195},
     revert_rate: {n: 20, rate: 0.05, lo: 0.0089, hi: 0.236},
     calibration: [{n: 30, rate: 0.1, lo: 0.035, hi: 0.256}, {n: 2, rate: null}, {n: 0, rate: null}, {n: 0, rate: null}, {n: 0, rate: null}, {n: 0, rate: null}]},
    {id: 'beta', reported: true, acmm_level: 4, family_mix: 'cross',
     verdicts_90d: 3, merged_90d: 10,
     record_coverage: {n: 0, rate: null},
     false_approve_rate: {n: 3, rate: null}, false_block_rate: {n: 3, rate: null},
     revert_rate: {n: 10, rate: 0.1, lo: 0.0179, hi: 0.404},
     calibration: [{n: 10, rate: 0.1, lo: 0.0179, hi: 0.404}, {n: 2, rate: null}, {n: 0, rate: null}, {n: 0, rate: null}, {n: 0, rate: null}, {n: 0, rate: null}]},
    {id: 'gamma', reported: false, acmm_level: 2, record_coverage: {n: 0, rate: null}, calibration: []}
  ]
}`

// TestEvidencePanelRendersEveryTable drives the shipped renderer with an
// /api/hub/evidence body: every section renders, every rate shows n and its
// Wilson interval, and null rates print "—" rather than 0%.
func TestEvidencePanelRendersEveryTable(t *testing.T) {
	var html string
	runEvidenceJS(t, evidenceHarness(t)+"\nresult = evidencePanelHTML(200, "+evidenceFixtureJS+");", &html)

	for _, want := range []string{
		"2 of 3 hives report evidence",
		// Findings first (#12456): plain sentences above every table, then one
		// per section under its title.
		`<section class="evidence-findings" aria-labelledby="evidence-findings-title"><h3 id="evidence-findings-title">What this says</h3>`,
		"<li>2 of 3 selected hives report evidence for the last 90d.</li>",
		"<li>Of the 50 merged PRs in the last 90 days across the selected hives, 40 (80.0%) have an evidence record — recording began 2026-10-08, so this will rise.</li>",
		`<p class="evidence-finding">Of the 50 merged PRs in the last 90 days`,
		// Sections #12117 lists that spokes do not report yet are collapsed
		// into one "Waiting on hives" list at the bottom, with no issue numbers.
		"Waiting on hives",
		"<li>Median time-to-merge per autonomy level — no selected hive reports it yet (it needs a build that records when each merged PR was opened).</li>",
		// The autonomy table carries the column; a level without it says so, never 0.
		"<th title=\"Median of the per-hive median times from PR opened to merged. Not a pooled median: each hive counts once however many PRs it merged.\">Median time-to-merge (median of hive medians)</th>",
		`<span class="evidence-rate-main">10.0% of 30</span><span class="evidence-rate-margin" title="Margin (95%): the true value is very likely inside this range. Fewer cases, wider range.">margin 3.5% – 25.6%</span></td><td class="evidence-empty">not reported</td></tr>`,
		// The per-axis criteria table renders; hives without criteria read
		// "Not reported", never unmet.
		`<th class="evidence-sticky-col">Hive</th><th>Axis</th><th>Met</th><th>Unmet</th><th>Not reported</th>`,
		`<tr><td class="evidence-sticky-col">gamma</td><td colspan="4" class="evidence-empty">Not reported</td></tr>`,
		"Evidence coverage", "Reviewer confidence", "Autonomy level × outcome",
		"Same- vs cross-family review", "Per-hive evidence",
		// Plain labels replace the jargon.
		"<th>Scope</th><th>Merged PRs</th>", "With an evidence record", "<th>Reviewer confidence</th><th>Verdicts</th>",
		"All selected hives — went wrong", "<th>Autonomy level</th>",
		// Rate cells fold n into the cell with the margin as a muted second line.
		`<td class="evidence-rate"><span class="evidence-rate-main">5.0% of 20</span><span class="evidence-rate-margin" title="Margin (95%): the true value is very likely inside this range. Fewer cases, wider range.">margin 0.9% – 23.6%</span></td>`,
		// Six heartbeat buckets fold into four ranges: buckets 0–2 (40 + 4 verdicts) pool.
		`<tr><td>Not confident (0.0–0.5)</td><td>44</td><td class="evidence-rate"><span class="evidence-rate-main">9.1% of 44</span>`,
		// All-selected-hives coverage row.
		`<tr><td>All selected hives</td><td>50</td><td class="evidence-rate"><span class="evidence-rate-main">80.0% of 50</span>`,
		"Revert rate by author kind",
		`<tr><td>agent</td><td class="evidence-rate"><span class="evidence-rate-main">5.0% of 20</span>`,
		`<tr><td>bot</td><td class="evidence-rate"><span class="evidence-rate-main">— (2, below the minimum of 10)</span>`,
		// A below-minimum sample shows its n but a dash, never 0%.
		`<span class="evidence-rate-main">— (3, below the minimum of 10)</span>`,
		`<tr><td>different families</td><td>3</td><td class="evidence-rate"><span class="evidence-rate-main">— (3, below the minimum of 10)</span></td></tr>`,
		`<td>L4</td><td>2</td><td>30</td><td class="evidence-rate"><span class="evidence-rate-main">10.0% of 30</span>`,
		// Confidence ranges carry both the plain label and the numeric range.
		"<td>Not confident (0.0–0.5)</td>", "<td>Very confident (0.83–1.0)</td>",
		// Per-hive calibration overlay: one column per reporting hive.
		`<th title="This hive alone">Alpha &lt;b&gt;</th><th title="This hive alone">beta</th>`,
		// Per-hive table: 7 columns, sticky hive column, horizontally scrollable.
		`<div class="table-wrap evidence-table-scroll">`, `<th class="evidence-sticky-col">Hive</th><th>Autonomy</th><th>Family mix</th>`,
		// A hive that never reported is listed, not dropped.
		`<tr><td class="evidence-sticky-col">gamma</td><td>L2</td><td colspan="5" class="evidence-empty">No evidence reported</td></tr>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("evidence panel is missing %q", want)
		}
	}
	if strings.Contains(html, "Alpha <b>") {
		t.Error("hive names are not escaped in the evidence panel")
	}
	// Fleet panels (#12370) no selected hive reports are listed once under
	// "Waiting on hives" in user-facing words; the ⓘ keeps the pointer.
	for _, want := range []string{
		"<li>Outcome by author kind — hives report this once upgraded to the Oct 16 build or later. <button",
		"<li>Boundary enforcement (agent writes the guardrail proxy blocked) — hives report this once upgraded to the Oct 16 build or later. <button",
		"<li>Shadow gates — shadow mode is off on every selected hive, or none has reported it yet (hives report this once upgraded to the Oct 16 build or later). <button",
		"<li>CI gate (claim vs verdict) totals — not reported by any hive build yet.</li>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("evidence panel is missing %q", want)
		}
	}
	for _, reject := range []string{"<h3 class=\"evidence-title\"><span>Outcome by author kind</span>", "<h3 class=\"evidence-title\"><span>Boundary enforcement</span>", "<h3 class=\"evidence-title\"><span>Shadow gates"} {
		if strings.Contains(html, reject) {
			t.Errorf("absent panel still rendered as a section: %q", reject)
		}
	}
	if strings.Contains(html, "<svg") {
		t.Error("fleet panel SVG rendered although the API returned no panel data")
	}
	// Filter chips: hive multi-select and ACMM level, all selected by default.
	for _, want := range []string{
		`<div class="evidence-chips" role="group" aria-label="Filter by hive">`,
		`<button type="button" class="evidence-chip on" aria-pressed="true" onclick="evidenceClearHives()">All</button>`,
		`data-hive="alpha" onclick="evidenceToggleHive(this.dataset.hive)">Alpha &lt;b&gt;</button>`,
		`class="evidence-chip evidence-chip-dim" aria-pressed="false" data-hive="gamma"`,
		`<div class="evidence-chips" role="group" aria-label="Filter by ACMM level">`,
		`data-level="2" onclick="evidenceSetLevel(this.dataset.level)">L2</button>`,
		`data-level="4" onclick="evidenceSetLevel(this.dataset.level)">L4</button>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("evidence panel is missing %q", want)
		}
	}
	if strings.Contains(html, "NaN") || strings.Contains(html, "undefined") {
		t.Errorf("evidence panel leaked NaN/undefined:\n%s", html)
	}
}

// evidenceModalHarness wires the shipped openEvidenceModal to a fake DOM and
// fetch. The native dialogs throw, so any use of one fails the test.
func evidenceModalHarness(t *testing.T, fetchJS string) string {
	t.Helper()
	return evidenceHarness(t) + "\n" + `
function makeEl(id) { return {id: id, style: {}, dataset: {}, innerHTML: '', focus: function() {}}; }
var els = {'evidence-modal': makeEl('evidence-modal'), 'evidence-body': makeEl('evidence-body'), 'evidence-close': makeEl('evidence-close')};
global.document = { getElementById: function(id) { return els[id] || null; } };
var nativeDialog = function() { throw new Error('native browser dialog used'); };
global.alert = nativeDialog; global.confirm = nativeDialog; global.prompt = nativeDialog;
var _hiveHubModalSeq = 0;
var fetched = [];
global.fetch = ` + fetchJS + `;
` + extractHubJSFunction(t, dashboardHTML, "hiveHubRememberModalOpen") +
		"async " + extractHubJSFunction(t, dashboardHTML, "openEvidenceModal") +
		"async " + extractHubJSFunction(t, dashboardHTML, "evidenceFetch") +
		extractHubJSFunction(t, dashboardHTML, "closeEvidenceModal")
}

// TestEvidenceModalMember403 is the member path: the endpoint answers 403 and
// the panel explains the restriction in place, with no native dialog.
func TestEvidenceModalMember403(t *testing.T) {
	src := evidenceModalHarness(t, `function(url) { fetched.push(url); return Promise.resolve({ok: false, status: 403, json: function() { throw new Error('403 body must not be parsed'); }}); }`) + `
done = openEvidenceModal().then(function() {
  result = {fetched: fetched, display: els['evidence-modal'].style.display, body: els['evidence-body'].innerHTML};
});`
	var got struct {
		Fetched []string
		Display string
		Body    string
	}
	runEvidenceJS(t, src, &got)
	if len(got.Fetched) != 1 || got.Fetched[0] != "/api/hub/evidence" {
		t.Errorf("fetched %v, want [/api/hub/evidence]", got.Fetched)
	}
	if got.Display != "flex" {
		t.Errorf("modal display = %q, want flex", got.Display)
	}
	if !strings.Contains(got.Body, "limited to hub admins") || strings.Contains(got.Body, "<table") {
		t.Errorf("403 must render the restriction message and no tables, got %q", got.Body)
	}
}

// TestEvidenceModalFetchErrors covers a server error and a network failure:
// both are shown inside the panel.
func TestEvidenceModalFetchErrors(t *testing.T) {
	cases := []struct{ name, fetch, want string }{
		{"http 500", `function() { return Promise.resolve({ok: false, status: 500}); }`, "Could not load the evidence details (HTTP 500)."},
		{"network", `function() { return Promise.reject(new Error('offline')); }`, "Failed to load the evidence details: offline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := evidenceModalHarness(t, tc.fetch) + `
done = openEvidenceModal().then(function() { result = els['evidence-body'].innerHTML; });`
			var body string
			runEvidenceJS(t, src, &body)
			if !strings.Contains(body, tc.want) || !strings.Contains(body, `role="alert"`) {
				t.Errorf("panel body = %q, want it to contain %q", body, tc.want)
			}
		})
	}
}

// TestEvidenceModalSuccessAndClose renders a 200 into the panel and closes it.
func TestEvidenceModalSuccessAndClose(t *testing.T) {
	src := evidenceModalHarness(t, `function() { return Promise.resolve({ok: true, status: 200, json: function() { return Promise.resolve(`+evidenceFixtureJS+`); }}); }`) + `
done = openEvidenceModal().then(function() {
  var body = els['evidence-body'].innerHTML;
  closeEvidenceModal();
  result = {body: body, display: els['evidence-modal'].style.display};
});`
	var got struct{ Body, Display string }
	runEvidenceJS(t, src, &got)
	if !strings.Contains(got.Body, "Per-hive evidence") {
		t.Errorf("200 response did not render the evidence tables: %q", got.Body)
	}
	if got.Display != "none" {
		t.Errorf("closeEvidenceModal left display = %q", got.Display)
	}
}

// TestEvidenceModalMarkup guards the static wiring: the dialog exists with an
// accessible name and is part of the Escape-to-close registry.
func TestEvidenceModalMarkup(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		`<div id="evidence-modal" class="evidence-modal" role="dialog" aria-modal="true" aria-labelledby="evidence-modal-title"`,
		`.evidence-modal { display: none;`,
		`<div id="evidence-body">`,
		`onclick="closeEvidenceModal()"`,
		"fetch('/api/hub/evidence' + evidenceWindowQuery())",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboardHTML is missing %q", want)
		}
	}
	if !strings.Contains(extractHubJSFunction(t, html, "hiveHubDismissTopModal"), "evidence-modal") {
		t.Error("Escape does not close the evidence modal")
	}
}

// evidenceFleetFixtureJSON is a real GET /api/hub/evidence body built by the
// server's pooling, so the client-side re-pool (#12370) is checked against
// the same code that renders the unfiltered view.
func evidenceFleetFixtureJSON(t *testing.T) (string, EvidenceView) {
	t.Helper()
	view := buildEvidenceView(evidenceFleetFixtureRegistry())
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), view
}

// evidenceFleetFixtureRegistry is the registry behind evidenceFleetFixtureJSON,
// exposed so filtered views can be built server-side for parity checks.
func evidenceFleetFixtureRegistry() []RegistryEntry {
	merged := func(v int) *int { return &v }
	return []RegistryEntry{
		{ID: "alpha", Name: "Alpha <b>", ACMMLevel: 4, PRsMerged90d: merged(40), Evidence: &HeartbeatEvidence{
			Verdicts90d: 25, FalseApprove90d: 3, FalseBlock90d: 1, Merged90d: 30, Reverted14d90d: 3,
			ReviewerFamilies: []string{"claude"}, AuthorFamilies: []string{"claude"},
			Calibration: [EvidenceCalibrationBuckets]EvidenceBucket{{N: 12, Bad: 2}, {}, {}, {}, {}, {N: 13, Bad: 1}},
			OutcomesByAuthorKind: map[string]EvidenceOutcome{
				"agent": {N: 20, Merged: 16, Reverted: 2}, "human": {N: 12, Merged: 10, Reverted: 1},
			},
			WriteCounters: map[string]EvidenceWriteCount{"guarded": {Attempted: 40, Blocked: 4}, "sandbox": {Attempted: 3}},
			ShadowGate: &EvidenceShadow{ReviewGate: EvidenceShadowGate{
				Held: EvidenceOutcome{N: 10, Merged: 8, Reverted: 2}, Passed: EvidenceOutcome{N: 15, Merged: 12}}},
		}},
		{ID: "beta", ACMMLevel: 3, PRsMerged90d: merged(10), Evidence: &HeartbeatEvidence{
			Verdicts90d: 12, FalseApprove90d: 1, Merged90d: 9, Reverted14d90d: 1,
			ReviewerFamilies: []string{"gpt"}, AuthorFamilies: []string{"claude"},
			Calibration:          [EvidenceCalibrationBuckets]EvidenceBucket{{N: 12, Bad: 3}},
			OutcomesByAuthorKind: map[string]EvidenceOutcome{"agent": {N: 12, Merged: 9, Reverted: 1}},
			WriteCounters:        map[string]EvidenceWriteCount{"guarded": {Attempted: 10, Blocked: 1}},
		}},
		{ID: "old", ACMMLevel: 3, Evidence: &HeartbeatEvidence{Verdicts90d: 4, Merged90d: 2}},
		{ID: "gamma", ACMMLevel: 2},
	}
}

// evidencePooledJS extracts the pooled sections of a view body so the server
// result and the client re-pool can be compared as one JSON value.
const evidencePooledJS = `function pooled(v) { return {spokes: v.spokes, reporting: v.reporting, record_coverage: v.record_coverage, calibration: v.calibration,
  by_acmm_level: (v.by_acmm_level || []).map(function(a) { return {level: a.level, hives: a.hives, merged_90d: a.merged_90d, revert_rate: a.revert_rate}; }),
  family_false_approve_rate: v.family_false_approve_rate, outcomes_by_author_kind: v.outcomes_by_author_kind || null,
  write_counters: v.write_counters || null, shadow_gate: v.shadow_gate || null,
  revert_rate_by_author_kind: v.revert_rate_by_author_kind || null}; }`

// TestEvidenceRepoolMatchesServer: re-pooling every hive client-side must
// reproduce the server's pooled numbers exactly, so a filtered selection is
// computed by the same rules as the unfiltered fleet view.
func TestEvidenceRepoolMatchesServer(t *testing.T) {
	fixture, _ := evidenceFleetFixtureJSON(t)
	var got struct {
		Server, Client json.RawMessage
	}
	runEvidenceJS(t, evidenceHarness(t)+"\n"+evidencePooledJS+"\nvar data = "+fixture+";\n"+
		"result = {Server: pooled(data), Client: pooled(evidenceRepool(data, data.hives))};", &got)
	var server, client interface{}
	if err := json.Unmarshal(got.Server, &server); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got.Client, &client); err != nil {
		t.Fatal(err)
	}
	// Floats are compared after a JSON round trip on both sides, so the only
	// tolerance is the renderer's own 0.1% rounding.
	normalize := func(raw json.RawMessage) string {
		var v interface{}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(roundFloats(v))
		return string(out)
	}
	if s, c := normalize(got.Server), normalize(got.Client); s != c {
		t.Fatalf("client re-pool differs from server pooling\nserver: %s\nclient: %s", s, c)
	}
}

func roundFloats(v interface{}) interface{} {
	switch x := v.(type) {
	case float64:
		return float64(int64(x*1e6+0.5)) / 1e6
	case []interface{}:
		for i := range x {
			x[i] = roundFloats(x[i])
		}
	case map[string]interface{}:
		for k := range x {
			x[k] = roundFloats(x[k])
		}
	}
	return v
}

// TestEvidenceFleetPanelsRender: with reporting spokes the three fleet panels
// draw inline SVG plus a rate table, and the server-provided totals appear.
func TestEvidenceFleetPanelsRender(t *testing.T) {
	fixture, _ := evidenceFleetFixtureJSON(t)
	var html string
	runEvidenceJS(t, evidenceHarness(t)+"\nresult = evidencePanelHTML(200, "+fixture+");", &html)
	for _, want := range []string{
		"Outcome by author kind", `aria-label="Outcome by author kind: merged, reverted and open PRs for human, agent and bot authors"`,
		// agent: 32 PRs over two hives, 25 merged (78.1%), 3 reverted of 25 (12.0%).
		`<tr><td>agent</td><td>2</td><td>32</td><td class="evidence-rate"><span class="evidence-rate-main">78.1% of 32</span>`,
		`<span class="evidence-rate-main">12.0% of 25</span>`,
		// bot: zero row, never a missing kind.
		`<tr><td>bot</td><td>0</td><td>0</td><td class="evidence-rate"><span class="evidence-rate-main">— (0, below the minimum of 10)</span>`,
		"Boundary enforcement", `aria-label="Boundary enforcement: attempted and proxy-blocked agent writes per intent tier"`,
		`<tr><td>guarded</td><td>2</td><td>50</td><td class="evidence-rate"><span class="evidence-rate-main">10.0% of 50</span>`,
		`<tr><td>sandbox</td><td>1</td><td>3</td><td class="evidence-rate"><span class="evidence-rate-main">— (3, below the minimum of 10)</span>`,
		`class="evidence-bar blocked"`, `class="evidence-bar attempted"`,
		"Shadow gates × outcome", "1 hive reports shadow gates",
		"<tr><td>Review gate</td><td>would have held</td><td>10</td><td>8</td><td>2</td>",
		`<tr><td>Review gate</td><td>would have passed</td><td>15</td><td>12</td><td>3</td><td class="evidence-rate"><span class="evidence-rate-main">0.0% of 12</span>`,
		// Findings generated from the fleet fixture, one per panel with data.
		"<li>AI agents wrote 32 PRs: 25 merged (78.1%) and 3 of those were reverted within 14 days. Humans wrote 12 PRs: 10 merged (83.3%) and 1 of those was reverted within 14 days.</li>",
		`<p class="evidence-finding">Agents attempted 53 writes to GitHub through the guardrail proxy and 5 (9.4%) were blocked; a blocked write never reached GitHub.</p>`,
		`class="evidence-bar merged"`, `class="evidence-bar reverted"`, `class="evidence-bar open"`,
		"no merge was held or blocked anywhere in the fleet",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("fleet panels are missing %q", want)
		}
	}
	if strings.Contains(html, "Intent verification") {
		t.Error("intent gate rendered although no spoke evaluated it")
	}
	if strings.Contains(html, "NaN") || strings.Contains(html, "undefined") {
		t.Errorf("fleet panels leaked NaN/undefined:\n%s", html)
	}
	if strings.Contains(html, "Alpha <b>") {
		t.Error("hive names are not escaped in the chips")
	}
}

// TestEvidenceFilterChips: a hive selection and an ACMM level re-pool every
// panel from the selected hives only, the chips reflect the state, and the
// handlers mutate the shared filter and re-render the body.
func TestEvidenceFilterChips(t *testing.T) {
	fixture, _ := evidenceFleetFixtureJSON(t)
	var got struct{ Beta, Level4, Both, Cleared string }
	runEvidenceJS(t, evidenceHarness(t)+"\nvar data = "+fixture+";\n"+
		"var body = {innerHTML: ''}; global.document = {getElementById: function(id) { return id === 'evidence-body' ? body : null; }};\n"+
		"_evidenceLast = data;\n"+
		"evidenceToggleHive('beta'); var beta = body.innerHTML;\n"+
		"evidenceClearHives(); evidenceSetLevel('4'); var level4 = body.innerHTML;\n"+
		"evidenceToggleHive('beta'); var both = body.innerHTML;\n"+
		"evidenceClearHives(); evidenceSetLevel(null); var cleared = body.innerHTML;\n"+
		"result = {Beta: beta, Level4: level4, Both: both, Cleared: cleared};", &got)

	// beta alone: one reporting hive of one selected; only beta's numbers.
	for _, want := range []string{
		"1 of 1 hives report evidence in this selection (3 of 4 fleet-wide)",
		`class="evidence-chip on" aria-pressed="true" data-hive="beta"`,
		`class="evidence-chip" aria-pressed="false" onclick="evidenceClearHives()">All</button>`,
		`<tr><td>All selected hives</td><td>10</td><td class="evidence-rate"><span class="evidence-rate-main">90.0% of 10</span>`,
		// Exactly one hive selected: no duplicate per-hive coverage row.
		`<tr><td>beta</td><td colspan="2" class="evidence-same-hive">(same hive as above)</td></tr>`,
		`<tr><td>agent</td><td>1</td><td>12</td><td class="evidence-rate"><span class="evidence-rate-main">75.0% of 12</span>`,
		`<tr><td>guarded</td><td>1</td><td>10</td><td class="evidence-rate"><span class="evidence-rate-main">10.0% of 10</span>`,
		"<li>Shadow gates — shadow mode is off on every selected hive, or none has reported it yet",
		"<tr><td>L3</td><td>1</td><td>9</td>",
		// Findings are regenerated client-side for the selection.
		"<li>beta reports evidence for the last 90d.</li>",
		"<li>AI agents wrote 12 PRs: 9 merged (75.0%) and 1 of those was reverted within 14 days.</li>",
	} {
		if !strings.Contains(got.Beta, want) {
			t.Errorf("beta selection is missing %q", want)
		}
	}
	for _, reject := range []string{"<td>sandbox</td>", "<tr><td>L4</td>", "Alpha &lt;b&gt;</td>", "<td>old</td>"} {
		if strings.Contains(got.Beta, reject) {
			t.Errorf("beta selection still shows %q", reject)
		}
	}
	// ACMM L4 alone is alpha.
	for _, want := range []string{
		"1 of 1 hives report evidence in this selection",
		`class="evidence-chip on" aria-pressed="true" data-level="4"`,
		`<tr><td>All selected hives</td><td>40</td><td class="evidence-rate"><span class="evidence-rate-main">75.0% of 40</span>`,
		`<tr><td>human</td><td>1</td><td>12</td><td class="evidence-rate"><span class="evidence-rate-main">83.3% of 12</span>`,
		"<tr><td>Review gate</td><td>would have held</td><td>10</td>",
	} {
		if !strings.Contains(got.Level4, want) {
			t.Errorf("L4 selection is missing %q", want)
		}
	}
	// beta ∩ L4 is empty: the panels disclose, never NaN.
	for _, want := range []string{"0 of 0 hives report evidence in this selection", "<li>No hives are selected.</li>", "<li>Outcome by author kind — hives report this once upgraded to the Oct 16 build or later."} {
		if !strings.Contains(got.Both, want) {
			t.Errorf("empty selection is missing %q", want)
		}
	}
	if strings.Contains(got.Both, "NaN") || strings.Contains(got.Both, "undefined") {
		t.Errorf("empty selection leaked NaN/undefined:\n%s", got.Both)
	}
	// Clearing both filters returns to the server's fleet view.
	if !strings.Contains(got.Cleared, "3 of 4 hives report evidence for last 90d. Each percentage") || !strings.Contains(got.Cleared, "<tr><td>All selected hives</td><td>50</td>") {
		t.Errorf("cleared filter did not restore the fleet view:\n%s", got.Cleared[:400])
	}
}

// TestEvidenceWindowSelector: the modal offers 24h / 7d / 30d / 90d / custom
// (RFC #12062), persists the choice, builds the matching query and labels the
// header; a custom range is validated client-side with the server's rules.
func TestEvidenceWindowSelector(t *testing.T) {
	src := evidenceModalHarness(t, `function(url) { fetched.push(url); return Promise.resolve({ok: true, status: 200, json: function() { return Promise.resolve(`+evidenceFixtureJS+`); }}); }`) + `
var store = {};
global.window = { localStorage: { getItem: function(k) { return store[k] === undefined ? null : store[k]; }, setItem: function(k, v) { store[k] = v; } } };
els['evidence-modal-title'] = makeEl('evidence-modal-title');
els['evidence-modal-title'].textContent = '';
els['evidence-from'] = makeEl('evidence-from'); els['evidence-to'] = makeEl('evidence-to'); els['evidence-range-err'] = makeEl('evidence-range-err');
var out = {};
done = openEvidenceModal().then(function() {
  out.defaultBody = els['evidence-body'].innerHTML;
  out.defaultTitle = els['evidence-modal-title'].textContent;
  out.defaultQuery = evidenceWindowQuery();
  return evidenceSetWindow('7d');
}).then(function() {
  out.weekQuery = evidenceWindowQuery(); out.weekTitle = els['evidence-modal-title'].textContent; out.weekStored = store[EVIDENCE_WINDOW_STORAGE_KEY];
  out.weekBody = els['evidence-body'].innerHTML;
  evidenceSetWindow('custom');
  out.customBody = els['evidence-body'].innerHTML;
  els['evidence-from'].value = '2026-10-15'; els['evidence-to'].value = '2026-10-01';
  evidenceApplyCustomRange(); out.errInverted = els['evidence-range-err'].textContent;
  els['evidence-from'].value = '2025-01-01'; els['evidence-to'].value = '2026-10-01';
  evidenceApplyCustomRange(); out.errSpan = els['evidence-range-err'].textContent;
  els['evidence-from'].value = '2026-10-01'; els['evidence-to'].value = '2026-10-15';
  return evidenceApplyCustomRange();
}).then(function() {
  out.customQuery = evidenceWindowQuery(); out.customTitle = els['evidence-modal-title'].textContent; out.customLabel = evidenceWindowLabel();
  out.fetched = fetched;
  out.reloaded = JSON.stringify(evidenceLoadWindow());
  out.validate = [evidenceValidateRange('', '2026-01-01'), evidenceValidateRange('2026-01-01', ''), evidenceValidateRange('2026-01-01', '2026-01-01')];
  result = out;
});`
	var got struct {
		DefaultBody, DefaultTitle, DefaultQuery, WeekQuery, WeekTitle, WeekStored, WeekBody, CustomBody string
		ErrInverted, ErrSpan, CustomQuery, CustomTitle, CustomLabel, Reloaded                           string
		Fetched, Validate                                                                               []string
	}
	runEvidenceJS(t, src, &got)
	for _, want := range []string{
		`<div class="evidence-chips" role="group" aria-label="Time window"><span class="evidence-chips-label">Window</span>`,
		`data-window="24h" onclick="evidenceSetWindow(this.dataset.window)">24h</button>`,
		`data-window="7d" onclick="evidenceSetWindow(this.dataset.window)">7d</button>`,
		`data-window="30d" onclick="evidenceSetWindow(this.dataset.window)">30d</button>`,
		`class="evidence-chip on" aria-pressed="true" data-window="90d"`,
		`data-window="custom" onclick="evidenceSetWindow(this.dataset.window)">custom</button>`,
		"hives report evidence for last 90d",
	} {
		if !strings.Contains(got.DefaultBody, want) {
			t.Errorf("default body is missing %q", want)
		}
	}
	if got.DefaultQuery != "" || got.DefaultTitle != "Fleet evidence · last 90d" {
		t.Errorf("default query=%q title=%q", got.DefaultQuery, got.DefaultTitle)
	}
	if got.WeekQuery != "?hours=168" || got.WeekTitle != "Fleet evidence · last 7d" || got.WeekStored != `{"key":"7d"}` {
		t.Errorf("7d query=%q title=%q stored=%q", got.WeekQuery, got.WeekTitle, got.WeekStored)
	}
	if !strings.Contains(got.WeekBody, `class="evidence-chip on" aria-pressed="true" data-window="7d"`) {
		t.Error("7d pill is not marked selected after the switch")
	}
	for _, want := range []string{`<input type="date" id="evidence-from"`, `<input type="date" id="evidence-to"`, `onclick="evidenceApplyCustomRange()">Apply</button>`, `id="evidence-range-err" role="alert"`} {
		if !strings.Contains(got.CustomBody, want) {
			t.Errorf("custom pill did not reveal %q", want)
		}
	}
	if got.ErrInverted != "The end date must not be before the start date." || got.ErrSpan != "The range must not exceed 366 days." {
		t.Errorf("range errors = %q / %q", got.ErrInverted, got.ErrSpan)
	}
	if got.CustomQuery != "?from=2026-10-01&to=2026-10-15" || got.CustomLabel != "Oct 1 – Oct 15" || got.CustomTitle != "Fleet evidence · Oct 1 – Oct 15" {
		t.Errorf("custom query=%q label=%q title=%q", got.CustomQuery, got.CustomLabel, got.CustomTitle)
	}
	if got.Reloaded != `{"key":"custom","from":"2026-10-01","to":"2026-10-15"}` {
		t.Errorf("custom range not persisted: %s", got.Reloaded)
	}
	wantFetched := []string{"/api/hub/evidence", "/api/hub/evidence?hours=168", "/api/hub/evidence?from=2026-10-01&to=2026-10-15"}
	if strings.Join(got.Fetched, " ") != strings.Join(wantFetched, " ") {
		t.Errorf("fetched %v, want %v", got.Fetched, wantFetched)
	}
	if got.Validate[0] == "" || got.Validate[1] == "" || got.Validate[2] != "" {
		t.Errorf("validate = %v", got.Validate)
	}
}

// TestEvidenceWindowApproximateDisclosure: when the server maps a custom
// range onto a reported window it says so, and record coverage (a 90-day
// ratio) is withheld with an explanation rather than shown as 0%.
func TestEvidenceWindowApproximateDisclosure(t *testing.T) {
	src := evidenceHarness(t) + `
_evidenceWindow = {key: 'custom', from: '2026-10-01', to: '2026-10-15'};
var data = ` + evidenceFixtureJS + `;
data.window = {hours: 720, key: '30d', label: 'last 30d', from: '2026-10-01T00:00:00Z', to: '2026-10-16T00:00:00Z', approximate: true, coverage_available: false};
data.window_unreported = 1;
data.hives[2].window_unreported = true;
result = evidencePanelHTML(200, data);`
	var html string
	runEvidenceJS(t, src, &html)
	for _, want := range []string{
		"so Oct 1 – Oct 15 is served from the smallest window that covers it: last 30d.",
		"Record coverage is only computed for the 90-day window",
		"(1 report only the 90-day figures)",
		"Not reported for this window (older build)",
		"<th>Merged (range)</th>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("approximate view is missing %q", want)
		}
	}
	if strings.Contains(html, "<tr><td>All selected hives</td><td>50</td>") {
		t.Error("record coverage row rendered although coverage_available is false")
	}
}

// TestEvidenceExplainers: every section carries an accessible ⓘ button and a
// popover with the four hub headings; the last section is computed from the
// data on screen and graded with the named thresholds; Escape closes it.
func TestEvidenceExplainers(t *testing.T) {
	src := evidenceHarness(t) + `
var html = evidencePanelHTML(200, ` + evidenceFixtureJS + `);
var keys = ['coverage', 'calibration', 'outcomes', 'acmm', 'writes', 'family', 'perhive', 'shadow'];
var exp = {};
keys.forEach(function(k) { exp[k] = evidenceExplainer(k, ` + evidenceFixtureJS + `); });
// Fake DOM for the toggle: two explainers, one button each.
function fakeEl(id) { return {id: id, hidden: true, attrs: {}, setAttribute: function(k, v) { this.attrs[k] = v; }, getAttribute: function(k) { return this.attrs[k]; }}; }
var a = fakeEl('evidence-exp-coverage'), b = fakeEl('evidence-exp-shadow'), ba = fakeEl('btn-a'), bb = fakeEl('btn-b');
ba.attrs['aria-expanded'] = 'false'; bb.attrs['aria-expanded'] = 'false';
var body = { querySelectorAll: function(sel) {
  if (sel.indexOf('.evidence-explainer') === 0) return [a, b].filter(function(e) { return !e.hidden; });
  return [ba, bb].filter(function(e) { return e.attrs['aria-expanded'] === 'true'; });
} };
global.document = { getElementById: function(id) { return ({'evidence-body': body, 'evidence-exp-coverage': a, 'evidence-exp-shadow': b})[id] || null; } };
evidenceToggleExplainer(ba, 'evidence-exp-coverage');
var afterOpen = {a: a.hidden, b: b.hidden, ba: ba.attrs['aria-expanded']};
evidenceToggleExplainer(bb, 'evidence-exp-shadow');
var afterSecond = {a: a.hidden, b: b.hidden, ba: ba.attrs['aria-expanded'], bb: bb.attrs['aria-expanded']};
var closed = evidenceCloseExplainers();
var afterEsc = {a: a.hidden, b: b.hidden, bb: bb.attrs['aria-expanded'], closed: closed, again: evidenceCloseExplainers()};
result = {html: html, exp: exp, afterOpen: afterOpen, afterSecond: afterSecond, afterEsc: afterEsc,
  grades: [evidenceGradeLow({n: 20, rate: 0.05}, EVIDENCE_ERR_STRONG, EVIDENCE_ERR_FAIR), evidenceGradeLow({n: 20, rate: 0.2}, EVIDENCE_ERR_STRONG, EVIDENCE_ERR_FAIR),
    evidenceGradeLow({n: 20, rate: 0.5}, EVIDENCE_ERR_STRONG, EVIDENCE_ERR_FAIR), evidenceGradeLow({n: 5, rate: 0}, EVIDENCE_ERR_STRONG, EVIDENCE_ERR_FAIR),
    evidenceGradeHigh({n: 50, rate: 0.8}, EVIDENCE_COVERAGE_STRONG, EVIDENCE_COVERAGE_FAIR), evidenceGradeHigh({n: 50, rate: 0.3}, EVIDENCE_COVERAGE_STRONG, EVIDENCE_COVERAGE_FAIR)]};`
	var got struct {
		HTML string
		Exp  map[string]struct {
			Measured, Why, How, Ours, Grade string
		}
		AfterOpen struct {
			A, B bool
			Ba   string
		}
		AfterSecond struct {
			A, B   bool
			Ba, Bb string
		}
		AfterEsc struct {
			A, B, Closed, Again bool
			Bb                  string
		}
		Grades []string
	}
	runEvidenceJS(t, src, &got)

	keys := []string{"coverage", "calibration", "outcomes", "acmm", "writes", "family", "perhive", "shadow"}
	for _, k := range keys {
		id := "evidence-exp-" + k
		btn := `aria-expanded="false" aria-controls="` + id + `"`
		if !strings.Contains(got.HTML, btn) || !strings.Contains(got.HTML, `onclick="evidenceToggleExplainer(this, '`+id+`')">ⓘ</button>`) {
			t.Errorf("section %s lacks an accessible info button", k)
		}
		if !strings.Contains(got.HTML, `<div id="`+id+`" class="evidence-explainer" role="region" aria-label="About `) {
			t.Errorf("section %s lacks its explainer region", k)
		}
		ex, ok := got.Exp[k]
		if !ok || ex.Measured == "" || ex.Why == "" || ex.How == "" || ex.Ours == "" || ex.Grade == "" {
			t.Errorf("explainer %s incomplete: %+v", k, ex)
		}
	}
	for _, want := range []string{
		"<h4>What is measured</h4>", "<h4>Why it matters</h4>", "<h4>How it is measured</h4>", "<h4>How to read our numbers</h4>",
		// Sticky head markup: title, ⓘ, one-line note, explainer, then the table.
		`<div class="evidence-head"><h3 class="evidence-title"><span>Evidence coverage</span><button type="button" class="evidence-info"`,
		// Live text from the fixture: fleet coverage 80.0% of 50, 2 of 3 hives.
		"All selected hives: 80.0% of 50 (margin 67.0% – 89.0%) merged PRs (90d) have an evidence record. 2 of 3 hives report today.",
		`<span class="evidence-grade evidence-grade-strong">Strong</span>`,
		// Plain-language definitions on first use.
		"margin (95%)", "minimum sample (10)", "reviewer-confidence range", "A “wrong call” is a false approve",
		// Honest limits.
		"Merged is not the same as correct", "reverts are only recorded since 2026-10-15", "evidence records only exist since 2026-10-08",
	} {
		if strings.Count(got.HTML, want) == 0 {
			t.Errorf("explainers are missing %q", want)
		}
	}
	if n := strings.Count(got.HTML, "<h4>How to read our numbers</h4>"); n != len(keys) {
		t.Errorf("%d explainers rendered, want %d", n, len(keys))
	}
	if strings.Contains(got.HTML, "NaN") || strings.Contains(got.HTML, "undefined") {
		t.Error("explainers leaked NaN/undefined")
	}
	// Only bucket 0 is measurable in the fixture: nothing to compare, so no grade.
	if got.Exp["coverage"].Grade != "strong" || got.Exp["writes"].Grade != "na" || got.Exp["calibration"].Grade != "na" || got.Exp["perhive"].Grade != "fair" {
		t.Errorf("grades: coverage=%s writes=%s calibration=%s perhive=%s", got.Exp["coverage"].Grade, got.Exp["writes"].Grade, got.Exp["calibration"].Grade, got.Exp["perhive"].Grade)
	}
	if want := []string{"strong", "fair", "weak", "na", "strong", "weak"}; strings.Join(got.Grades, ",") != strings.Join(want, ",") {
		t.Errorf("grade thresholds = %v, want %v", got.Grades, want)
	}
	if got.AfterOpen.A || !got.AfterOpen.B || got.AfterOpen.Ba != "true" {
		t.Errorf("open: %+v", got.AfterOpen)
	}
	if !got.AfterSecond.A || got.AfterSecond.B || got.AfterSecond.Ba != "false" || got.AfterSecond.Bb != "true" {
		t.Errorf("second toggle must close the first: %+v", got.AfterSecond)
	}
	if !got.AfterEsc.A || !got.AfterEsc.B || got.AfterEsc.Bb != "false" || !got.AfterEsc.Closed || got.AfterEsc.Again {
		t.Errorf("escape: %+v", got.AfterEsc)
	}
	if !strings.Contains(dashboardHTML, "if (typeof evidenceCloseExplainers === 'function' && evidenceCloseExplainers())") {
		t.Error("document Escape handler does not close an open explainer first")
	}
}

// TestEvidenceStickyHeadings guards the CSS that keeps a section's title and
// column heads visible while its rows scroll inside the modal body.
func TestEvidenceStickyHeadings(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		// The scroll container is the modal body, an ancestor of every section.
		`.evidence-modal-scroll { flex: 1; overflow: auto;`,
		`<div class="evidence-modal-scroll">`,
		`.evidence-section { --evidence-head-h: 2.9rem; position: relative; }`,
		// table-wrap must not become its own scroll container or sticky breaks.
		`.evidence-section .table-wrap { overflow: visible; }`,
		`.evidence-head { position: sticky; top: 0; z-index: 3; background: var(--surface); height: var(--evidence-head-h);`,
		`.evidence-section thead th { position: sticky; top: calc(var(--evidence-head-h) - 1px); z-index: 2; background: var(--surface); }`,
		`.evidence-explainer[hidden] { display: none; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboardHTML is missing %q", want)
		}
	}
}

// TestEvidenceModalNoIssueNumbers (#12456): nothing a hub admin reads in the
// modal may cite an issue number or an internal flag name; those live in the
// ⓘ explainer's "Technically:" sentence or in the docs. Checked on the fleet
// fixture, the small fixture, a single-hive selection and an empty selection.
func TestEvidenceModalNoIssueNumbers(t *testing.T) {
	fixture, _ := evidenceFleetFixtureJSON(t)
	var got map[string]string
	runEvidenceJS(t, evidenceHarness(t)+"\nvar fleet = "+fixture+";\n"+
		"result = {fleet: evidencePanelHTML(200, fleet), small: evidencePanelHTML(200, "+evidenceFixtureJS+"),"+
		" beta: evidenceViewHTML(fleet, {hives: ['beta'], level: null}), none: evidenceViewHTML(fleet, {hives: ['beta'], level: 4})};", &got)
	issue := regexp.MustCompile(`#\d{4,}`)
	internal := []string{"review.shadow", "intent shadow mode", "spoke predates", "spokes yet", "#12"}
	for name, html := range got {
		// Strip the explainer regions: the technical pointer is allowed there.
		visible := regexp.MustCompile(`(?s)<div id="evidence-exp-[^"]*" class="evidence-explainer".*?</p></div>`).ReplaceAllString(html, "")
		if m := issue.FindString(visible); m != "" {
			t.Errorf("%s: visible modal copy cites an issue number %q", name, m)
		}
		for _, bad := range internal {
			if strings.Contains(visible, bad) {
				t.Errorf("%s: visible modal copy contains internal wording %q", name, bad)
			}
		}
		if strings.Contains(html, "NaN") || strings.Contains(html, "undefined") {
			t.Errorf("%s: leaked NaN/undefined", name)
		}
	}
}

// TestEvidenceFindingsParity: the client-side findings generator (used for a
// filtered selection) must reproduce the server's sentences byte for byte,
// for the unfiltered fleet and for a hive-filtered re-pool.
func TestEvidenceFindingsParity(t *testing.T) {
	fixture, view := evidenceFleetFixtureJSON(t)
	betaView := buildEvidenceView(filterEvidenceRegistry(evidenceFleetFixtureRegistry(), url.Values{"hive": {"beta"}}))
	l3View := buildEvidenceView(filterEvidenceRegistry(evidenceFleetFixtureRegistry(), url.Values{"level": {"3"}}))
	var got struct {
		Fleet, Beta, L3            []string
		FleetKeys, BetaKeys        map[string]string
		FleetWait, BetaWait, Small []string
	}
	runEvidenceJS(t, evidenceHarness(t)+"\nvar data = "+fixture+";\n"+
		"function sel(f) { var hs = evidenceFilterHives(data.hives, f); var v = evidenceRepool(data, hs); v.window = data.window; return v; }\n"+
		"var beta = sel({hives: ['beta'], level: null}), l3 = sel({hives: null, level: 3});\n"+
		"result = {Fleet: evidenceFindings(data), Beta: evidenceFindings(beta), L3: evidenceFindings(l3), FleetKeys: evidenceFindingsByKey(data), BetaKeys: evidenceFindingsByKey(beta),"+
		" FleetWait: evidenceWaiting(data), BetaWait: evidenceWaiting(beta), Small: evidenceFindings("+evidenceFixtureJS+")};", &got)
	same := func(name string, js, goSide []string) {
		t.Helper()
		if strings.Join(js, "\n") != strings.Join(goSide, "\n") {
			t.Errorf("%s findings differ\nJS: %q\nGo: %q", name, js, goSide)
		}
	}
	same("fleet", got.Fleet, view.Findings)
	same("beta", got.Beta, betaView.Findings)
	same("level 3", got.L3, l3View.Findings)
	same("fleet waiting", got.FleetWait, evidenceWaiting(view))
	same("beta waiting", got.BetaWait, evidenceWaiting(betaView))
	for k, v := range evidenceFindingsByKey(view) {
		if got.FleetKeys[k] != v {
			t.Errorf("fleet finding %s: JS %q, Go %q", k, got.FleetKeys[k], v)
		}
	}
	for k, v := range evidenceFindingsByKey(betaView) {
		if got.BetaKeys[k] != v {
			t.Errorf("beta finding %s: JS %q, Go %q", k, got.BetaKeys[k], v)
		}
	}
	if len(got.Fleet) < 3 || len(got.Fleet) > 6 {
		t.Errorf("fleet findings: %d sentences, want 3–6", len(got.Fleet))
	}
	if len(got.Small) < 3 {
		t.Errorf("small fixture findings: %d sentences, want at least 3 (with fallbacks)", len(got.Small))
	}
}

// TestEvidenceReportLinks: the 📄 Report / ⬇ Markdown links sit in the window
// row and carry the current window and filter so the download reconciles
// with what is on screen.
func TestEvidenceReportLinks(t *testing.T) {
	fixture, _ := evidenceFleetFixtureJSON(t)
	var got struct{ Fleet, Beta, Custom string }
	runEvidenceJS(t, evidenceHarness(t)+"\nvar data = "+fixture+";\n"+
		"var fleet = evidenceWindowControlsHTML(data);\n"+
		"_evidenceFilter = {hives: ['beta'], level: 4}; _evidenceWindow = {key: '7d'}; var beta = evidenceWindowControlsHTML(data);\n"+
		"_evidenceFilter = {hives: null, level: null}; _evidenceWindow = {key: 'custom', from: '2026-10-01', to: '2026-10-15'}; var custom = evidenceWindowControlsHTML(data);\n"+
		"result = {Fleet: fleet, Beta: beta, Custom: custom};", &got)
	for _, want := range []string{
		// The default 90d window is the server default, so no hours= is sent.
		`<a class="evidence-report-link" href="/api/hub/evidence/report?format=html" target="_blank" rel="noopener"`,
		`📄 Report</a>`,
		`<a class="evidence-report-link" href="/api/hub/evidence/report?format=md&amp;download=1" download`,
		`⬇ Markdown</a>`,
	} {
		if !strings.Contains(got.Fleet, want) {
			t.Errorf("fleet window row is missing %q\n%s", want, got.Fleet)
		}
	}
	if !strings.Contains(got.Beta, `href="/api/hub/evidence/report?hours=168&amp;hive=beta&amp;level=4&amp;format=html"`) {
		t.Errorf("filtered report link lacks hive/level:\n%s", got.Beta)
	}
	if !strings.Contains(got.Custom, `href="/api/hub/evidence/report?from=2026-10-01&amp;to=2026-10-15&amp;format=html"`) {
		t.Errorf("custom-window report link lacks from/to:\n%s", got.Custom)
	}
	// The window row must still carry the 24h/7d/30d/90d/custom pills.
	for _, want := range []string{`data-window="24h"`, `data-window="90d"`, `data-window="custom"`} {
		if !strings.Contains(got.Fleet, want) {
			t.Errorf("window pills missing %q", want)
		}
	}
}

// TestEvidenceLayoutClasses: the layout fix is class-based (TestStyleRatchet
// forbids inline styles): wide modal, scrollable per-hive table with a sticky
// hive column, and the muted margin line under each rate.
func TestEvidenceLayoutClasses(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		"max-width: min(96vw, 1600px)",
		".evidence-table-scroll { overflow-x: auto;",
		".evidence-table-scroll td.evidence-sticky-col { position: sticky; left: 0;",
		".evidence-rate-margin {",
		".evidence-findings {",
		".evidence-waiting {",
		".evidence-same-hive {",
		".evidence-report-link {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboardHTML CSS is missing %q", want)
		}
	}
}

// TestEvidenceCriteriaPanel drives the per-axis criteria table (#12463):
// criteria sort into met / unmet / not reported per axis, and a hive with no
// measured criterion is one "Not reported" row with the reason, never unmet.
func TestEvidenceCriteriaPanel(t *testing.T) {
	hives := `[
  {id: 'alpha', criteria: [
    {axis: 'trust', label: 'Trust', scored: true, criteria: [
      {name: 'revert_rate', label: 'Revert rate', status: 'met', display: '0.0%', score: 100, good: 'Under 2% reverted.'},
      {name: 'autonomy', label: 'Autonomy level', status: 'unmet', display: 'L1', score: 0},
      {name: 'pulse', label: 'Pulse', status: 'not_reported', reason: 'no direct feedback signal exists yet'}]},
    {axis: 'productivity', label: 'Productivity', scored: false, criteria: [
      {name: 'relay_7d', label: 'Relay tasks', status: 'not_reported', reason: 'contributor relay not reported'}]}]},
  {id: 'off', criteria: [
    {axis: 'trust', label: 'Trust', scored: false, criteria: [
      {name: 'autonomy', label: 'Autonomy level', status: 'not_reported', reason: 'hive is offline'}]}]}
]`
	var html string
	runEvidenceJS(t, evidenceHarness(t)+"\nresult = evidenceCriteriaPanel("+hives+");", &html)
	for _, want := range []string{
		"Per-axis criteria",
		`<tr><td class="evidence-sticky-col">alpha</td><td>Trust</td><td><span title="Good: Under 2% reverted.">Revert rate (0.0%)</span></td><td><span title="">Autonomy level (L1)</span></td><td><span title="no direct feedback signal exists yet">Pulse</span></td></tr>`,
		`<tr><td class="evidence-sticky-col"></td><td>Productivity</td><td>—</td><td>—</td><td><span title="contributor relay not reported">Relay tasks</span></td></tr>`,
		`<tr><td class="evidence-sticky-col">off</td><td colspan="4" class="evidence-empty">Not reported — hive is offline</td></tr>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("criteria panel missing %q\n%s", want, html)
		}
	}
	if strings.Contains(html, "undefined") || strings.Contains(html, "NaN") {
		t.Errorf("leaked undefined/NaN: %s", html)
	}

	var empty string
	runEvidenceJS(t, evidenceHarness(t)+"\nresult = evidenceCriteriaPanel([]);", &empty)
	if !strings.Contains(empty, "No data reported yet.") {
		t.Errorf("empty selection: %s", empty)
	}
}
