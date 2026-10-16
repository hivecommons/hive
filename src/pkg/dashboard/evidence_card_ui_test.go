package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestEvidenceCardInThroughputSection pins the Evidence sub-panel (#12064): it
// sits inside the Throughput section below the throughput card, reuses the
// prt-card chrome, follows the Throughput window/repo selector, and reads
// /api/evidence/summary for the outcome-by-author-kind (#12364), calibration,
// accuracy × model and shadow-gate (#12068) panels.
func TestEvidenceCardInThroughputSection(t *testing.T) {
	html := indexHTML(t)
	section := strings.Index(html, `<div id="pr-throughput-section"`)
	if section < 0 {
		t.Fatal("pr-throughput-section missing")
	}
	end := strings.Index(html[section:], `<div id="advisory-section"`)
	if end < 0 {
		t.Fatal("cannot find end of pr-throughput-section markup")
	}
	markup := html[section : section+end]
	prt := strings.Index(markup, `id="pr-throughput-card"`)
	ev := strings.Index(markup, `<div id="evidence-card" class="prt-card advisory-subsection-card evidence-card"`)
	if prt < 0 || ev < 0 || ev < prt {
		t.Fatalf("evidence-card must sit inside pr-throughput-section below pr-throughput-card (prt=%d ev=%d)", prt, ev)
	}

	for _, snippet := range []string{
		"/api/evidence/summary?hours=${encodeURIComponent(hours)}",
		"if (repo) url += `&repo=${encodeURIComponent(repo)}`;",
		"if (hours !== prtHours || repo !== prtRepo) return;",
		"async function fetchPRThroughput() {\n      fetchEvidenceSummary();",
		"dto.reviewer_calibration",
		"dto.reviewer_accuracy_by_model",
		"verdicts_model_unknown",
		`<details class="ev-panel" data-ev-panel="${key}"`,
		`n=${Number(n) || 0}`,
		"What this does not show:",
		"'Reviewer calibration'",
		"'Reviewer accuracy × model family'",
		"row.model === 'unknown' ? 'ev-unknown' : ''",
		`<svg class="ev-svg"`,
		".ev-svg .ev-unknown .ev-bar",
		"dto.shadow_gate || {}",
		"evPanel('shadow-gates', 'Shadow gates', shadow.n, evShadowGateSvg(shadow),",
		"no shadow-gate observations in this window",
		"[['held', `would have ${verb}`], ['passed', 'would have passed']]",
		".ev-svg .ev-bar.sr",
		// Panel 1 (#12364): outcome by author kind.
		"dto.outcomes_by_author_kind || {}",
		"evPanel('outcome-by-author', 'Outcome by author kind', outcomeN, evOutcomesSvg(outcomes),",
		"const EV_AUTHOR_KINDS = ['human', 'agent', 'bot'];",
		"reverted: Number(o.reverted_14d) || 0",
		"${bar('om', row.merged, 0)}${bar('oc', row.closed, 1)}${bar('or', row.reverted, 2)}",
		"no PRs with an evidence bundle in this window",
		"Schema v1 bundles record a merge but not a close-without-merge, so closed may read 0.",
		"Reverted means a reverted_by link to a revert PR within 14 d of merge",
		"Open PRs are excluded from the merged and reverted rates.",
		".ev-svg .ev-bar.or",
		".ev-legend i.om",
		"dto.complexity_by_repo",
		"evPanel('maintainability', 'Maintainability trend', complexityN",
		"complexity.filter((r) => r.months.length >= 2).length",
		"Report-only; Go-only cyclomatic complexity. No causal claim",
		"evMaintainabilitySvg(complexity, dto.acmm_changes)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence card missing snippet %q", snippet)
		}
	}
	// Panel order follows the RFC numbering: 1 (outcome) renders first.
	p1 := strings.Index(html, "evPanel('outcome-by-author'")
	p2 := strings.Index(html, "evPanel('calibration'")
	if p1 < 0 || p2 < 0 || p1 > p2 {
		t.Errorf("outcome-by-author panel must render before calibration (p1=%d p2=%d)", p1, p2)
	}
}

func TestEvidenceMaintainabilityRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: maintainability rendering not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	source.WriteString("const EV_PALETTE_SIZE = 6;\n")
	for _, name := range []string{"escapeHtml", "evRate", "evPanel", "evOutcomesSvg", "evCalibrationSvg", "evAccuracySvg", "evShadowGateSvg", "evMaintainabilityRows", "evMaintainabilitySvg", "renderEvidenceSummary"} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(`
const assert = require('node:assert/strict');
const snap = (month, median, over, tools) => ({month, median_cyclomatic: median, functions_over_threshold: over, threshold: 10, static_warnings_by_tool: tools});
const dto = {hours: 0, complexity_by_repo: [
  {repo: 'o/z', months: [snap('2020-03', 3, 4, {'go vet': 0}), snap('2020-01', 2, 1, {'go vet': 7})]},
  {repo: 'o/<b>', months: [snap('2020-02', 9, 2, {'<tool>': 5})]},
  {repo: 'o/empty', months: []}
]};
const rows = evMaintainabilityRows(dto);
assert.equal(rows.length, 2);
assert.equal(rows[0].repo, 'o/<b>');
assert.deepEqual(rows[1].months.map(m => m.month), ['2020-01', '2020-03']);
assert.equal(rows.filter(r => r.months.length >= 2).length, 1);
assert.equal(evMaintainabilityRows({...dto, repo: 'O/Z'}).length, 1);
assert.equal(evMaintainabilityRows({...dto, hours: 24}).length, 0);
assert.deepEqual(evMaintainabilityRows({}), []);
assert.match(evMaintainabilitySvg([], []), /no monthly complexity snapshots/);
const svg = evMaintainabilitySvg(rows, [
  {repo: 'O/Z', month: '2020-02', level: 3},
  {repo: 'o/z', month: '2019-12', level: 2},
  {repo: 'o/other', month: '2020-02', level: 6}
]);
assert.match(svg, /o\/z · 2020-02: ACMM L3 first observed/);
assert.doesNotMatch(svg, /ACMM L2|ACMM L6/);
assert.match(svg, /Median cyclomatic \(Go\): 3/);
assert.match(svg, /Functions over threshold: 4 \(threshold 10\)/);
assert.match(svg, /go vet: 0/);
assert.match(svg, /&lt;tool&gt;: 5/);
assert.match(svg, /o\/&lt;b&gt;/);
assert.doesNotMatch(svg, /<b>|<tool>|NaN|undefined/);
// Every repo has gaps: no continuous lines across an unmeasured month.
assert.doesNotMatch(svg, /<polyline/);
const contiguous = evMaintainabilitySvg(evMaintainabilityRows({hours: 0, complexity_by_repo: [
  {repo: 'o/r', months: [snap('2020-01', 2, 0, {'go vet': 0}), snap('2020-02', 4, 1, {'go vet': 3})]}
]}), []);
assert.equal((contiguous.match(/<polyline/g) || []).length, 3);
assert.match(contiguous, /stroke-dasharray="3 3"/);
const missingTools = evMaintainabilitySvg([{repo: 'o/r', months: [snap('2020-01', 1, 0)]}], []);
assert.match(missingTools, /No tool measurements/);
assert.doesNotMatch(missingTools, /NaN|undefined/);
const EV_AUTHOR_KINDS = ['human', 'agent', 'bot'];
const PRT_WINDOWS = [], prtHours = 0;
const card = {innerHTML: '', querySelector() { return null; }};
const document = {getElementById() { return card; }};
renderEvidenceSummary({...dto, all_time: true});
assert.match(card.innerHTML, /data-ev-panel="maintainability" open><summary>Maintainability trend<span[^>]*>n=1/);
assert.ok(card.innerHTML.indexOf('data-ev-panel="maintainability"') < card.innerHTML.indexOf('data-ev-panel="shadow-gates"'));
assert.match(card.innerHTML, /Report-only; Go-only cyclomatic complexity. No causal claim/);
renderEvidenceSummary(null);
assert.match(card.innerHTML, /data-ev-panel="maintainability" open><summary>Maintainability trend<span[^>]*>n=0/);
assert.match(card.innerHTML, /no monthly complexity snapshots/);
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("maintainability renderer failed: %v\n%s", err, out)
	}
}

// TestEvidenceCardUIHasNoUndefinedCallees is the ReferenceError guard for the
// Evidence card helpers.
func TestEvidenceCardUIHasNoUndefinedCallees(t *testing.T) {
	html := indexHTML(t)
	for _, fn := range []string{
		"fetchEvidenceSummary",
		"renderEvidenceSummary",
		"evPanel",
		"evRate",
		"evOutcomesSvg",
		"evCalibrationSvg",
		"evAccuracySvg",
		"evShadowGateSvg",
		"evMaintainabilityRows",
		"evMaintainabilitySvg",
		"escapeHtml",
	} {
		decl := regexp.MustCompile(`(?m)^\s*(async\s+)?function\s+` + fn + `\s*\(`)
		if !jsDeclaredNear(html, fn, decl) {
			t.Errorf("Evidence card calls %q but it is never declared", fn)
		}
	}
}
