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
		"What this does not show",
		// #13204: server-computed letter grade pill and plain-English line on
		// every panel; dense detail text lives in the ⓘ popover, not inline.
		"function evServerCard(dto, key) {",
		"const evDetails = {};",
		"Object.keys(evDetails).forEach((k) => { delete evDetails[k]; });",
		`<span class="ev-letter ev-band-${band}" title="${escapeHtml(why)}" aria-label="Grade ${escapeHtml(letter)}: ${escapeHtml(why)}">${escapeHtml(letter)}</span>`,
		"const plain = sc && sc.plain_summary ? `<div class=\"ev-plain\">${escapeHtml(String(sc.plain_summary))}</div>` : '';",
		"${section('Details', evDetails[key])}${section('What this does not show', note)}${gradeHtml}",
		".ev-letter.ev-band-a { color:var(--green);",
		".ev-letter.ev-band-b { color:var(--amber);",
		".ev-letter.ev-band-c { color:var(--red);",
		".ev-letter.ev-band-none { color:var(--muted);",
		".ev-plain { margin-top:var(--sp-2); font-size:var(--fs-xs);",
		".ev-disclosed { margin:var(--sp-2) 0 var(--sp-1); font-size:var(--fs-2xs); color:var(--muted); }",
		".ev-explainer { position:absolute;",
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
		"const reverted = Number(o.reverted_14d) || 0, reopened = Number(o.issue_reopened_14d) || 0;",
		"reverted, reopened, failed: reverted + reopened };",
		"${bar('om', row.merged, 0)}${bar('oc', row.closed, 1)}${bar('or', row.failed, 2)}${bar('ou', row.unknown, 3)}",
		"no PRs with an evidence bundle in this window",
		"A live bundle records a merge only when this hive merged, and never a close-without-merge; PRs reviewed here and then merged by someone else, closed by hand or left open are resolved from the governor's review-outcome ledger instead",
		"${outcomeElsewhere} merged elsewhere · ${outcomeClosed} closed without merge · ${outcomeOpen} open.",
		"prs_outcome_from_ledger",
		"merged_elsewhere",
		"is shown as unknown and excluded from every rate, not counted as open or as not merged.",
		// #13241: both change-failure signals, with their detection method and
		// the 14-day window spelled out.
		"Undone counts two change-failure signals, each within 14 d of merge: reverted means a reverted_by link from a later merged PR whose title or body named this PR or its commit",
		"re-opened means an issue this PR closed was re-opened by a person after the merge, as seen by the issues webhook or the issue listing",
		"Change-failure rate = (reverted + re-opened) within 14 days ÷ merged.",
		"const EV_REOPENS_SINCE = '2026-10-20';",
		// Unknown-outcome policy (#12514 follow-up): never a denominator, always a count.
		"prs_outcome_unknown",
		"unknown: Number(o.unknown) || 0",
		"outcome unknown ${row.unknown} (not in any rate)",
		".ev-svg .ev-bar.ou",
		"const EV_CALIB_MIN_BUCKET_N = 5;",
		"thin: known < EV_CALIB_MIN_BUCKET_N, merge: evRate(k && k.merged, known), block: evRate(k && k.blocks, n)",
		"const solid = pts.filter((q) => !q.thin && q.merge != null);",
		".ev-svg .ev-pt.ev-thin",
		"fa: evRate(s.approved_not_merged, approvalsKnown), fb: evRate(s.merged_after_enforced_block, blocksKnown)",
		// #12595: only enforced blocks are false blocks; advisory blocks are
		// dissent before merge, a count and a share, never an error rate.
		"const blocksKnown = (Number(s.enforced_blocks) || 0) - (Number(s.enforced_blocked_unknown) || 0);",
		"advisory += Number(st.advisory_blocks) || 0; dissent += Number(st.dissent_before_merge) || 0;",
		"Perspective verdicts were advisory on this hive (review.require_approval off); ${dissent} blocks preceded a merge",
		"Perspective verdicts were advisory on this hive (review.require_approval off); ",
		"false-block (enforced block, merged anyway)",
		// #12592: the review gate's shadow observations split by lane.
		"const lanes = sg.review_gate_by_lane || {};",
		"[lanes.merge_eligible, 'Review · eligible', 'held'], [lanes.self_merge, 'Review · self-merge', 'held']",
		"approved_unknown",
		"blocked_unknown",
		"false-approve (approved, closed without merge)",
		"records_ci_pending",
		"CI still running when captured ${Number(row.ci_pending) || 0}",
		"CI not green at claim",
		"${seg('sm', row.merged)}${seg('sr', row.reverted)}${seg('so', row.open)}${seg('su', row.unknown)}",
		".ev-svg .ev-bar.or",
		".ev-legend i.om",
		"dto.complexity_by_repo",
		"evPanel('maintainability', 'Maintainability trend', complexityN",
		"complexity.filter((r) => r.months.length >= 2).length",
		"Report-only; Go-only cyclomatic complexity. No causal claim",
		"evMaintainabilitySvg(complexity, dto.acmm_changes)",
		"dto.claim_vs_verdict",
		"evPanel('claim-vs-verdict', 'CI gate — integrity + convergence', claimN, evCIGateHtml(rec) + evConvergenceSvg(rec.convergence) + evCIRecoveryHtml(rec) + evClaimVsVerdictSvg(claims),",
		// CI gate: grade A integrity over merged PRs with a known merge-time
		// state (unknown outside the rate), grade B convergence over settled
		// agent PRs; red-at-claim is disclosed, not graded.
		"dto.ci_recovery",
		"const integrity = evRate(withChecks - breaches, withChecks);",
		"const abandon = evRate(never, settled);",
		"const firstPass = evRate(passes['1'], settled - never);",
		"const grade = evLowerGrade(gradeA, gradeB);",
		"with no state on the merged head; all outside the rate",
		"never a breach until GitHub confirms it",
		"Breach = a REQUIRED check GitHub reports red on the merged head, as the hive’s own merge gate judges it",
		"a breach is a bypass by the merger, not a fault of the author",
		"breach = a required check red on the merged head per GitHub, as the merge gate judges",
		"in flight, ${Number(conv.unknown) || 0} unknown, outside the rates",
		"['1', 'cp-1', 'green on pass 1'], ['2', 'cp-2', 'pass 2'], ['3', 'cp-3', 'pass 3'], ['4+', 'cp-4', 'pass 4+'], ['never', 'cp-never', 'never (closed unmerged)']",
		"share green by pass 1 (right axis)",
		"const EV_GATE_BREACH_GOOD = 0.01;",
		"const EV_GATE_BREACH_FAIR = 0.03;",
		"const EV_CONV_PASSES_EXCELLENT = 1.5, EV_CONV_ABANDON_EXCELLENT = 0.05;",
		"const EV_CONV_PASSES_GOOD = 2.5, EV_CONV_ABANDON_GOOD = 0.10;",
		"const EV_CONV_PASSES_FAIR = 4, EV_CONV_ABANDON_FAIR = 0.20;",
		"Number(breaches) === 0 ? 'excellent'",
		"Disclosed, not graded: at claim",
		"CI red at claim is by design and is disclosed, not graded",
		".ev-svg .ev-bar.cp-never",
		"['recovered_merged', 'cr-rec', 'recovered and merged'], ['green_open', 'cr-green', 'green, still open'], ['abandoned', 'cr-aband', 'abandoned (closed unmerged)'], ['pending', 'cr-pend', 'still red or pending'], ['outcome_unknown', 'cr-unk', 'outcome unknown']",
		"pol.self_authored_automerge === false",
		".ev-svg .ev-bar.cr-unk",
		"no agent-authored PRs with evidence in this window",
		"['ci_not_green', 'cv-ci', 'CI not green at claim'], ['verdict_not_approve', 'cv-swarm', 'swarm not approve'], ['both', 'cv-both', 'both']",
		"records_empty_ci",
		"Convergence covers agent-authored PRs only",
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
		"evPanel('work-balance', 'Work balance', balanceN, evWorkBalanceSvg(balance, balanceSrc),",
		"no issue or PR opens, closes or merges in this window",
		// Work balance reads "is the queue keeping up?": GitHub-truth issue
		// opens/closes, PR opens/merges from bundles, week-end queue depth,
		// throughput ratio graded per series with the lower winning.
		"dto.work_balance_sources",
		"Queue at end of ${escapeHtml(String(last.week || ''))}",
		"open at week end (right axis)",
		"closed without merge",
		"issues_github_since",
		"audit_only_weeks",
		"Issue opens and closes are GitHub-truth (all authors) from",
		"audit-log only",
		"const EV_THROUGHPUT_EXCELLENT = 0.95;",
		"const EV_THROUGHPUT_GOOD = 0.85;",
		"const EV_THROUGHPUT_FAIR = 0.70;",
		"const EV_QUEUE_GROWTH_MAX = 0.10;",
		"evThroughputGrade(issueRatio, openedTotal, issueGrowth)",
		"evThroughputGrade(prRatio, prsTotal, prGrowth)",
		"evLowerGrade(issueGrade, prGrade)",
		".ev-grade.good",
		".ev-svg .wb-queue, .ev-legend .wb-queue",
		"[['human', 'wb-human', 'opened by humans'], ['agent', 'wb-agent', 'opened by agents'], ['bot', 'wb-bot', 'opened by bots']]",
		"row.throttled ?",
		"throttle_windows_unavailable",
		"Throttle windows not reported by this hive",
		"PR opens, merges and closes come from evidence bundles",
		".ev-svg .ev-bar.wb-throttle",
		"'work-balance': (dto) => {",
		// Panel 4 (#12368): boundary enforcement. Headline denominator is every
		// agent write (hive-mediated + direct), not just proxied ones.
		"const activity = dto.write_activity && typeof dto.write_activity === 'object' ? dto.write_activity : null;",
		"const sentinel = dto.sentinel_hits_per_week || {};",
		"evPanel('boundary-enforcement', 'Boundary enforcement', writesN, evWriteActivitySvg(activity) + evSentinelSparkline(sentinel),",
		"const EV_WRITE_TIERS = ['ADVISORY', 'ISSUES_ONLY', 'ISSUES_AND_PRS', 'ISSUES_PRS_MERGE'];",
		"Object.entries(direct.blocked_by_reason || {})",
		"reasons.slice(0, EV_TOP_REASONS)",
		"no agent writes recorded in this window",
		"no sentinel hits recorded in this window",
		"agent writes blocked</b>",
		"${bar(row.kind === 'direct' ? 'wa' : 'wm', row.writes, 0, formatNumber(row.writes))}${bar('wb', row.blocked, 1,",
		`aria-label="Sentinel hits per ISO week"`,
		"direct-write counters recorded since",
		"would-block counts from intent verification are shown separately in the explainer and never counted as blocked",
		"Writes that bypass both the hive and the proxy are invisible here",
		"const EV_BLOCKED_STRONG = 0.01;",
		"const EV_BLOCKED_FAIR = 0.05;",
		".ev-legend i.wm",
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
	source.WriteString("const EV_PALETTE_SIZE = 6, EV_SVG_W = 320, EV_TOP_REASONS = 3;\nconst EV_WRITE_TIERS = ['ADVISORY', 'ISSUES_ONLY', 'ISSUES_AND_PRS', 'ISSUES_PRS_MERGE'];\nlet evLast = null;\nconst evWindowLabel = () => 'all time';\nconst evWindowControls = () => '';\nconst evExplainerHtml = () => ({ button: '', body: '' });\nconst EV_CALIB_MIN_BUCKET_N = 5, EV_GRADE_MIN_N = 20;\nconst evDetails = {};\n")
	for _, name := range []string{"escapeHtml", "evRate", "evPanel", "evServerCard", "evOutcomesSvg", "evCalibrationSvg", "evAccuracySvg", "evShadowGateSvg", "evClaimVsVerdictSvg", "evWriteActivitySvg", "evCIGateHtml", "evCIRecoveryHtml", "evGateGrade", "evConvergenceGrade", "evConvergenceSvg", "evPct", "evWindowHint", "evSentinelSparkline", "evWorkBalanceSvg", "evMaintainabilityRows", "evMaintainabilitySvg", "evGradeLine", "renderEvidenceSummary"} {
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
// Report-only panels end in a "Not graded" badge, never "Rated: Not enough data".
const EV_GRADES = { strong: 'Strong', fair: 'Fair', weak: 'Weak', none: 'Not enough data', ungraded: 'Not graded' };
const ungraded = evGradeLine('ungraded', 'report-only; no threshold to grade against');
assert.match(ungraded, /<span class="ev-grade ungraded">Not graded<\/span> <span class="ev-grade-why">report-only; no threshold to grade against<\/span>/);
assert.doesNotMatch(ungraded, /Rated:|Not enough data/);
assert.match(evGradeLine('none', 'needs more'), /<span class="ev-grade none">Rated: Not enough data<\/span>/);
assert.match(evGradeLine('strong'), /Rated: Strong<\/span>$/);
const EV_AUTHOR_KINDS = ['human', 'agent', 'bot'];
const PRT_WINDOWS = [], prtHours = 0;
const card = {innerHTML: '', querySelector() { return null; }};
const document = {getElementById() { return card; }};
renderEvidenceSummary({...dto, all_time: true});
assert.match(card.innerHTML, /data-ev-panel="maintainability" open><summary>Maintainability trend<span[^>]*>n=1/);
assert.ok(card.innerHTML.indexOf('data-ev-panel="maintainability"') < card.innerHTML.indexOf('data-ev-panel="shadow-gates"'));
// The "what this does not show" note is handed to the ⓘ explainer (stubbed here), never rendered inline.
assert.doesNotMatch(card.innerHTML, /Report-only; Go-only cyclomatic complexity. No causal claim/);
renderEvidenceSummary(null);
assert.match(card.innerHTML, /data-ev-panel="maintainability" open><summary>Maintainability trend<span[^>]*>n=0/);
assert.match(card.innerHTML, /no monthly complexity snapshots/);
// No server card: a dashed — pill and no plain line; nothing inline says "What this does not show".
assert.match(card.innerHTML, /<span class="ev-letter ev-band-none" title="Grade not reported by this hive"[^>]*>—<\/span><\/summary>/);
assert.doesNotMatch(card.innerHTML, /ev-plain|What this does not show/);
// Server cards (#13204): letter pill coloured by band, reason in the title, plain summary right under the header.
renderEvidenceSummary({...dto, all_time: true, cards: {
  maintainability: {grade: {letter: 'A', band: 'a', reason: 'median flat & fewer complex <fns>', enough_data: true, rubric: 'r'}, plain_summary: 'Code complexity across 1 repo: median flat, 4 complex functions (was 1).'},
  'shadow-gates': {grade: {letter: '—', band: 'none', reason: 'needs 20 observed PRs', enough_data: false, rubric: 'r'}, plain_summary: 'No PRs observed by the shadow gates in this window.'}
}});
assert.match(card.innerHTML, /<summary>Maintainability trend<span[^>]*>n=1<\/span><span class="ev-letter ev-band-a" title="median flat &amp; fewer complex &lt;fns&gt;" aria-label="Grade A: median flat &amp; fewer complex &lt;fns&gt;">A<\/span><\/summary><div class="ev-plain">Code complexity across 1 repo: median flat, 4 complex functions \(was 1\).<\/div>/);
assert.match(card.innerHTML, /<span class="ev-letter ev-band-none" title="Not enough data yet: needs 20 observed PRs"[^>]*>—<\/span><\/summary><div class="ev-plain">No PRs observed by the shadow gates in this window.<\/div>/);
assert.match(card.innerHTML, /<span class="ev-letter ev-band-none" title="Grade not reported by this hive"[^>]*>—<\/span><\/summary><details|<span class="ev-letter ev-band-none" title="Grade not reported by this hive"[^>]*>—<\/span><\/summary><div class="ev-(empty|headline)/);
// A band the client does not know falls back to the neutral pill.
renderEvidenceSummary({cards: {'work-balance': {grade: {letter: 'Z', band: 'z', reason: 'x', enough_data: true}}}});
assert.match(card.innerHTML, /<span class="ev-letter ev-band-none" title="x" aria-label="Grade Z: x">Z<\/span>/);
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
		"evWriteActivitySvg",
		"evCIRecoveryHtml",
		"evFlowSvg",
		"evThroughputGrade",
		"evLowerGrade",
		"evQueueGrowth",
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
		"evServerCard",
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
		"const EV_CUSTOM_MAX_DAYS = 3 * 366;",
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
		"const EV_BLOCKED_STRONG = 0.01;",
		"const EV_BLOCKED_FAIR = 0.05;",
		"const EV_DATA_SINCE = '2026-10-08';",
		"const EV_REVERTS_SINCE = '2026-10-15';",
		"const EV_GRADES = { excellent: 'Excellent', good: 'Good', strong: 'Strong', fair: 'Fair', weak: 'Weak', none: 'Not enough data', ungraded: 'Not graded' };",
		"const label = grade === 'ungraded' ? EV_GRADES.ungraded : `Rated: ${EV_GRADES[grade]}`;",
		"grade: 'ungraded', reason: 'report-only; no threshold to grade against',",
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
		"is reported as unknown and left out of every rate, never counted as \"not merged\". Live evidence (verdicts, CI, sentinel) has only been recorded since ${EV_DATA_SINCE}, reverts only since ${EV_REVERTS_SINCE} and issue re-opens only since ${EV_REOPENS_SINCE}; backfilled records reach further back but carry only author, outcome and dates.",
		"false-approve rate = approvals on PRs known not to have merged ÷ approvals on PRs with a known outcome",
		"merged ÷ verdicts on PRs with a known outcome",
		"Gate integrity = (with_checks − breaches) ÷ with_checks",
		"Abandoned share = never ÷ settled",
		"First-pass share = green on pass 1 ÷ PRs that went green: a neutral line (the RFC’s C1 number), not graded",
		"Denominator = hive-mediated writes",
		"Ratios: issues closed ÷ issues opened and PRs merged ÷ PRs opened over the whole window",
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

// TestEvidenceCardWindowTooShortHint pins that a panel with too few samples
// for a grade points at a longer preset window when the summary's
// available_windows says one would hold more PRs, instead of a bare "Not
// enough data"; and that the custom range accepts backfilled history.
func TestEvidenceCardWindowTooShortHint(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const EV_AVAILABLE_KEYS = [['all', 'all time'], ['720', '30d'], ['168', '7d']];",
		"function evWindowHint(dto) {",
		"dto.available_windows",
		"return ` — window too short: ${counts} PRs; switch to ${presets.length ? presets.join(' or ') : 'a custom range'}`;",
		"`needs at least ${EV_GRADE_MIN_N} merged PRs${evWindowHint(dto)}`",
		"`needs at least ${EV_GRADE_MIN_N} known-outcome verdicts in the top bucket${evWindowHint(dto)}`",
		"`needs at least ${EV_GRADE_MIN_N} known-outcome verdicts${evWindowHint(dto)}`",
		"`needs at least ${EV_GRADE_MIN_N} stopped-and-merged PRs${evWindowHint(dto)}`",
		"`needs at least ${EV_GRADE_MIN_N} merged PRs with a known merge-time check state and ${EV_GRADE_MIN_N} settled agent PRs${evWindowHint(dto)}`",
		`<div class="ev-note ev-window-hint">No PRs opened in this window${escapeHtml(windowHint)}.</div>`,
		"by the PR\\u2019s open time, not when its record was written",
		"PRs are placed in the window by their open time, never by when a record was written.",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("Evidence window hint missing snippet %q", snippet)
		}
	}
	for _, stale := range []string{
		"Closed-without-merge is not recorded yet, so a closed PR still shows as open.",
		"so closed may read 0",
		"a PR that is still open counts as not merged",
		"an approval on a still-open PR counts as a false approve",
		"reads as open",
	} {
		if strings.Contains(html, stale) {
			t.Errorf("stale closed-state copy still present: %q", stale)
		}
	}
}
