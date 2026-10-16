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
// accuracy × model, boundary enforcement (#12368) and shadow-gate (#12068) panels.
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
		"/api/evidence/summary?${query}",
		"if (repo) url += `&repo=${encodeURIComponent(repo)}`;",
		"if (query !== evWindowQuery() || repo !== prtRepo) return;",
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
		"dto.claim_vs_verdict",
		"evPanel('claim-vs-verdict', 'Claim vs verdict', claimN, evClaimVsVerdictSvg(claims),",
		"no agent-authored PRs with evidence in this window",
		"['ci_not_green', 'cv-ci', 'CI not green'], ['verdict_not_approve', 'cv-swarm', 'swarm not approve'], ['both', 'cv-both', 'both']",
		"records_empty_ci",
		"Only agent-authored PRs",
		".ev-svg .ev-bar.cv-both",
		// Typography / sizing (#evidence-card font bug): the chart is pinned to
		// its viewBox width so SVG text renders 1:1 at the dashboard's font
		// tokens, and panels tile in a grid instead of each spanning the card.
		"const EV_SVG_W = 320;",
		".ev-svg { display:block; width:320px; max-width:100%; height:auto;",
		".ev-svg text { fill:var(--muted); font-family:var(--font-ui); font-size:var(--fs-2xs); }",
		".ev-svg text.ev-lbl { fill:var(--text); font-size:var(--fs-xs); }",
		".evidence-card { margin-top: var(--sp-4); display:grid; grid-template-columns:repeat(auto-fit, minmax(min(380px, 100%), 1fr));",
		".evidence-card > .prt-head { grid-column:1 / -1;",
		"dto.work_balance",
		"evPanel('work-balance', 'Work balance', balanceN, evWorkBalanceSvg(balance),",
		"no audited issue opens or closes in this window",
		"[['human', 'wb-human', 'opened by humans'], ['agent', 'wb-agent', 'opened by agents'], ['bot', 'wb-bot', 'opened by bots']]",
		"row.throttled ?",
		"throttle_windows_unavailable",
		"Throttle windows not reported by this hive",
		"Issues only, from this hive's audit log",
		".ev-svg .ev-bar.wb-throttle",
		"'work-balance': (dto) => {",
		"const EV_BALANCE_STRONG = 0.8;",
		"const EV_BALANCE_FAIR = 0.5;",
		"evGrade(ratio, openedTotal, EV_BALANCE_STRONG, EV_BALANCE_FAIR, true)",
		// Panel 4 (#12368): boundary enforcement.
		"const counters = dto.write_counters && typeof dto.write_counters === 'object' ? dto.write_counters : null;",
		"const sentinel = dto.sentinel_hits_per_week || {};",
		"evPanel('boundary-enforcement', 'Boundary enforcement', attemptedN, evWriteCountersSvg(counters) + evSentinelSparkline(sentinel),",
		"const EV_WRITE_TIERS = ['ADVISORY', 'ISSUES_ONLY', 'ISSUES_AND_PRS', 'ISSUES_PRS_MERGE'];",
		"Object.entries(c.blocked_by_reason || {})",
		"row.reasons.slice(0, EV_TOP_REASONS)",
		"no write counters yet (the proxy has not recorded an agent write, or the counter file is absent)",
		"no sentinel hits recorded in this window",
		"${bar('wa', row.attempted, 0, row.attempted)}${bar('wb', row.blocked, 1,",
		`aria-label="Sentinel hits per ISO week"`,
		"Write counters are proxy-side observations",
		"Would-block counts from intent verification are not included in these bars",
		"direct writes that bypass the proxy are invisible here",
		".ev-svg .ev-bar.wb",
		".ev-legend i.wa",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence card missing snippet %q", snippet)
		}
	}
	// Every SVG helper must draw at the shared width; a hard-coded 320 would
	// silently drift from the CSS pin if the constant ever changes.
	if m := regexp.MustCompile(`const w = 320\b`).FindString(html); m != "" {
		t.Errorf("ev*Svg helpers must size from EV_SVG_W, found %q", m)
	}
	// Panel order follows the RFC numbering: 1 (outcome) renders first.
	p1 := strings.Index(html, "evPanel('outcome-by-author'")
	p2 := strings.Index(html, "evPanel('calibration'")
	if p1 < 0 || p2 < 0 || p1 > p2 {
		t.Errorf("outcome-by-author panel must render before calibration (p1=%d p2=%d)", p1, p2)
	}
	// Panel order follows the RFC numbering: 4 (boundary) before 5 (claim) before 8 (shadow).
	p4 := strings.Index(html, "evPanel('boundary-enforcement'")
	p5 := strings.Index(html, "evPanel('claim-vs-verdict'")
	p8 := strings.Index(html, "evPanel('shadow-gates'")
	if p4 < 0 || p5 < 0 || p8 < 0 || p4 > p5 || p5 > p8 {
		t.Errorf("boundary-enforcement panel must render before claim-vs-verdict and shadow-gates (p4=%d p5=%d p8=%d)", p4, p5, p8)
	}
}

func TestEvidenceMaintainabilityRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: maintainability rendering not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	source.WriteString("const EV_PALETTE_SIZE = 6, EV_SVG_W = 320, EV_TOP_REASONS = 3;\nconst EV_WRITE_TIERS = ['ADVISORY', 'ISSUES_ONLY', 'ISSUES_AND_PRS', 'ISSUES_PRS_MERGE'];\nlet evLast = null;\nconst evWindowLabel = () => 'all time';\nconst evWindowControls = () => '';\nconst evExplainerHtml = () => ({ button: '', body: '' });\n")
	for _, name := range []string{"escapeHtml", "evRate", "evPanel", "evOutcomesSvg", "evCalibrationSvg", "evAccuracySvg", "evShadowGateSvg", "evClaimVsVerdictSvg", "evWriteCountersSvg", "evSentinelSparkline", "evWorkBalanceSvg", "evMaintainabilityRows", "evMaintainabilitySvg", "renderEvidenceSummary"} {
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
// Custom range: from/to win over hours; To is exclusive so 2020-03 is out.
const custom = evMaintainabilityRows({...dto, hours: 1440, from: '2020-01-15T00:00:00Z', to: '2020-03-01T00:00:00Z'});
assert.deepEqual(custom.map(r => r.repo), ['o/<b>', 'o/z']);
assert.deepEqual(custom[1].months.map(m => m.month), ['2020-01']);
assert.deepEqual(evMaintainabilityRows({...dto, hours: 1440, from: '2019-01-01T00:00:00Z', to: '2019-12-31T00:00:00Z'}), []);
assert.equal(evMaintainabilityRows({...dto, from: 'garbage', to: ''}).length, 2);
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
		"evWriteCountersSvg",
		"evSentinelSparkline",
		"evShadowGateSvg",
		"evMaintainabilityRows",
		"evMaintainabilitySvg",
		"evClaimVsVerdictSvg",
		"evWorkBalanceSvg",
		"escapeHtml",
		"evWindowControls",
		"evWindowLabel",
		"evWindowQuery",
		"evExplainerHtml",
		"toggleEvidenceExplainer",
		"evGrade",
		"evLimitsSentence",
	} {
		decl := regexp.MustCompile(`(?m)^\s*(async\s+)?function\s+` + fn + `\s*\(`)
		if !jsDeclaredNear(html, fn, decl) {
			t.Errorf("Evidence card calls %q but it is never declared", fn)
		}
	}
}

// TestEvidenceCardWindowSelector pins the Evidence card's own window pills
// (24h · 7d · 30d · custom from/to), their localStorage persistence and the
// header scope text that follows the selection (RFC #12062).
func TestEvidenceCardWindowSelector(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const EV_WINDOWS = [[24, '24h'], [168, '7d'], [720, '30d']];",
		"const EV_WINDOW_STORAGE_KEY = 'hive.evidence.window';",
		"const EV_CUSTOM_MAX_DAYS = 366;",
		"localStorage.setItem(EV_WINDOW_STORAGE_KEY, JSON.stringify(evWindow))",
		"return evWindow.from ? `from=${encodeURIComponent(evWindow.from)}&to=${encodeURIComponent(evWindow.to)}` : `hours=${encodeURIComponent(evWindow.hours)}`;",
		`data-action="setEvidenceWindow" data-arg0="${h}" data-stop="1"`,
		`data-action="toggleEvidenceCustomRange" data-stop="1" aria-expanded="${evCustomOpen ? 'true' : 'false'}" aria-controls="ev-custom-range"`,
		`<div id="ev-custom-range" class="ev-custom-range" role="group"`,
		`<input type="date" name="from"`,
		`<input type="date" name="to"`,
		`data-action="applyEvidenceCustomRange" data-arg-types="t" data-stop="1">Apply</button>`,
		`'"To" must not be before "from".'`,
		"if (evWindow.from) return `${evShortDate(evWindow.from)} – ${evShortDate(evWindow.to)}`;",
		"return `last ${w ? w[1] : evWindow.hours + 'h'}`;",
		"🔬 Evidence · ${escapeHtml(scope)}",
		`class="gov-pr-models-toggle window-pills-toolbar ev-window-controls"`,
		".ev-custom-range input[type=\"date\"]",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence window selector missing snippet %q", snippet)
		}
	}
	// The card no longer follows the throughput pills.
	if strings.Contains(html, "const windowName = PRT_WINDOWS.find(([h]) => h === prtHours);\n      const scope = (dto.all_time") {
		t.Error("Evidence card scope must come from evWindowLabel(), not the throughput window")
	}
}

// TestEvidenceCardExplainers pins the plain-language ⓘ explainer per panel:
// keyboard-accessible toggle (aria-expanded/aria-controls, Esc closes), the
// four fixed headings plus "Why it matters", a live "What ours shows" section
// and the named grade thresholds documented in docs/review-evidence.md.
func TestEvidenceCardExplainers(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const EV_GRADE_MIN_N = 20;",
		"const EV_SHORT_WINDOW_HOURS = 48;",
		"const EV_CALIB_STRONG = 0.8;",
		"const EV_CALIB_FAIR = 0.6;",
		"const EV_ERR_STRONG = 0.10;",
		"const EV_ERR_FAIR = 0.25;",
		"const EV_REVERT_STRONG = 0.05;",
		"const EV_REVERT_FAIR = 0.15;",
		"const EV_SHADOW_FALSE_ALARM_STRONG = 0.25;",
		"const EV_SHADOW_FALSE_ALARM_FAIR = 0.5;",
		"const EV_BLOCKED_STRONG = 0.05;",
		"const EV_BLOCKED_FAIR = 0.15;",
		"const EV_DATA_SINCE = '2026-10-08';",
		"const EV_REVERTS_SINCE = '2026-10-15';",
		"const EV_GRADES = { strong: 'Strong', fair: 'Fair', weak: 'Weak', none: 'Not enough data' };",
		"'outcome-by-author': (dto) => {",
		"calibration: (dto) => {",
		"'accuracy-by-model': (dto) => {",
		"'shadow-gates': (dto) => {",
		"'boundary-enforcement': (dto) => {",
		`<button type="button" class="ev-info" data-action="toggleEvidenceExplainer" data-arg0="${key}" data-stop="1" data-prevent="1" aria-expanded="${open ? 'true' : 'false'}" aria-controls="${id}" aria-label="Explain ${escapeHtml(title)}">ⓘ</button>`,
		`class="ev-explainer" role="region"`,
		"section('What is measured', x.measured)",
		"section('Why it matters', x.matters)",
		"section('How it is measured', x.how)",
		"section('What good looks like', x.good)",
		"section('What ours shows', x.ours)",
		"evGradeLine(x.grade, x.reason)",
		"if (e.key !== 'Escape' || !evExplainOpen.size) return;",
		"Merged is not the same as correct, and a PR that is still open counts as not merged. Evidence has only been recorded since ${EV_DATA_SINCE} and reverts only since ${EV_REVERTS_SINCE}.",
		"A well-calibrated reviewer would be near 90–100% in the top bucket.",
		".ev-explainer[hidden] { display:none; }",
		".ev-grade.weak",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence explainer missing snippet %q", snippet)
		}
	}
	// Every panel rendered by renderEvidenceSummary has an explainer entry.
	for _, m := range regexp.MustCompile(`evPanel\('([a-z0-9-]+)'`).FindAllStringSubmatch(html, -1) {
		key := m[1]
		if !strings.Contains(html, "'"+key+"': (dto) => {") && !strings.Contains(html, "\n      "+key+": (dto) => {") {
			t.Errorf("panel %q has no EV_EXPLAINERS entry", key)
		}
	}
}
