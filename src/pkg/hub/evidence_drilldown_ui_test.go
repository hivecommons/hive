package hub

import (
	"encoding/json"
	"os/exec"
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
		jsFunc(t, "evidencePct"),
		jsFunc(t, "evidenceInterval"),
		jsFunc(t, "evidenceCount"),
		jsFunc(t, "evidenceRateCells"),
		jsFunc(t, "evidenceRateHeads"),
		jsFunc(t, "evidenceTable"),
		jsFunc(t, "evidenceHiveLabel"),
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
		"function quadrantAxis(q, name) { return {scored: true, score: 50}; }",
		"function quadrantSVG() { return '<svg></svg>'; }",
		"var _allDashHives = [{}, {}];",
		"var _fleetQuadrant = {scored_axes: 4};",
		"var _isAdmin = false;",
		jsFunc(t, "fleetQuadrantDrillAttrs"),
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
	} {
		if !strings.Contains(got.Admin, want) {
			t.Errorf("admin fleet card is missing %q:\n%s", want, got.Admin)
		}
	}
	for _, forbidden := range []string{"openEvidenceModal", `role="button"`, "tabindex", "fleet-quadrant-drill", "onkeydown"} {
		if strings.Contains(got.Member, forbidden) {
			t.Errorf("member fleet card must stay unchanged but contains %q:\n%s", forbidden, got.Member)
		}
	}
	if !strings.Contains(got.Member, "Fleet quadrant · 2 hives in view") {
		t.Errorf("member fleet card lost its content:\n%s", got.Member)
	}
}

// TestFleetQuadrantFallbackLinkWhenNothingScored: the card is the only entry
// point to the evidence view, and it is not drawn until an axis is scored, so
// admins get a one-line link in its place; members still get nothing. The
// header keeps returning '' on its own (TestFleetQuadrantHeaderWiring pins
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
		// Sections #12117 lists that spokes do not report yet are disclosed in place.
		"Median time-to-merge is not reported by spokes yet.",
		"Revert rate by author kind is not reported by spokes yet.",
		"The per-axis criteria table is not reported by spokes yet (tracked in #12080).",
		"Evidence coverage", "Calibration curve", "ACMM level × outcome",
		"Same- vs cross-family false approve", "Per-hive evidence",
		// n, rate and Wilson interval for a measured cell.
		"<td>40</td><td>10.0%</td><td class=\"evidence-ci\">4.0% – 23.0%</td>",
		// Fleet coverage row.
		"<tr><td>Fleet</td><td>50</td><td>80.0%</td><td class=\"evidence-ci\">67.0% – 89.0%</td></tr>",
		// A below-minimum sample shows its n but a dash, never 0%.
		"<td>4</td><td>—</td><td class=\"evidence-ci\">—</td>",
		"<tr><td>cross</td><td>3</td><td>—</td><td class=\"evidence-ci\">—</td></tr>",
		"<td>L4</td><td>2</td><td>30</td><td>30</td><td>10.0%</td>",
		// Per-hive calibration overlay: one column per reporting hive.
		"<th>Alpha &lt;b&gt;</th><th>beta</th>",
		// A hive that never reported is listed, not dropped.
		"<tr><td>gamma</td><td>L2</td><td colspan=\"12\" class=\"evidence-empty\">No evidence reported</td></tr>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("evidence panel is missing %q", want)
		}
	}
	if strings.Contains(html, "Alpha <b>") {
		t.Error("hive names are not escaped in the evidence panel")
	}
	if strings.Contains(html, "Shadow gate") {
		t.Error("shadow-gate table rendered although the API did not return it")
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
		"fetch('/api/hub/evidence')",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboardHTML is missing %q", want)
		}
	}
	if !strings.Contains(extractHubJSFunction(t, html, "hiveHubDismissTopModal"), "evidence-modal") {
		t.Error("Escape does not close the evidence modal")
	}
}
