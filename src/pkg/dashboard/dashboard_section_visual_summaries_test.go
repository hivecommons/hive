package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDashboardVisualSummaryWidgets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: visual summary widget behavior was not executed")
	}
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	start := strings.Index(html, "const MINI_SUMMARY_HISTORY_PREFIX =")
	if start < 0 {
		t.Fatal("mini summary helper block not found")
	}
	end := strings.Index(html[start:], "    function relativeTimeShort")
	if end < 0 {
		t.Fatal("mini summary helper block end not found")
	}
	block := html[start : start+end]

	script := `
const store = new Map();
var _factHistory = [{t: 1000, count: 1500}, {t: 2000, count: 1526}];
var lastSparkSeries;
function sparkSpanLabel(times) { return 'recorded time span'; }
var window = { _lastStatus: {
  hiveId: 'hive-visual-test',
  governor: { mode: 'busy', prs: 4, issues: 3, thresholds: { surge: 12 } },
  repos: [{ name: 'repo', openPrs: [1,2,3], actionableIssues: [1,2] }],
  agents: [{ name: 'a' }, { name: 'b', paused: true }],
  tokens: { totals: { sessions: 2 } },
  hiveAdvice: { recommendations: [
    { id: 'reduce-blocked-prs', title: 'Reduce the blocked PR queue', rationale: '4 blocked PRs need fixes.', score: 95, signals: [{ name: 'blocked', value: '4' }], items: [{}, {}, {}, {}] },
    { id: 'unblock-human-issues', title: 'Unblock the human queue', rationale: '2 issues need decisions.', score: 82, signals: [{ name: 'needs_human', value: '2' }], items: [{}, {}] }
  ] }
} };
var _acmmEvalData = { overall_level: 6, operational_level: 6, codebase_level: 6, criteria_passed: 7, criteria_total: 7, criteria_results: [
  { category: 'Build', passed: true }, { category: 'Tests', passed: true }, { category: 'Docs', passed: true },
  { category: 'Security', passed: true }, { category: 'Release', passed: true }, { category: 'Ops', passed: true }, { category: 'Policy', passed: true }
] };
var _acmmSelectedRepo = null;
var _cachedContributors = [{ github_username: 'octo' }, { login: 'bee' }];
var localStorage = { getItem: k => store.has(k) ? store.get(k) : null, setItem: (k, v) => store.set(k, String(v)), removeItem: k => store.delete(k) };
var document = { getElementById: id => ({ textContent: id === 'repos-needs-human' ? '1' : '' }) };
var prtLast = { buckets: [{ opened: 1, merged: 0 }, { opened: 2, merged: 1 }, { opened: 3, merged: 2 }] };
function escapeHtml(v) { return String(v == null ? '' : v).replace(/[&<>\"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','\"':'&quot;'}[c])); }
function formatNumber(value, locale, options) { try { return new Intl.NumberFormat(locale || undefined, options || {}).format(value); } catch (e) { return String(value); } }
function fmtSparkVal(v) { return Number.isFinite(Number(v)) ? String(Number(v)) : '—'; }
function renderSparkline(_el, series, opts) { lastSparkSeries = series; return '<span class="sparkline"><svg class="' + opts.svgClass + '" role="img"><title>' + escapeHtml(opts.title || '') + '</title><polyline class="sparkline-line" points="0,0 1,1"/></svg></span>'; }
function _getAgentColor() { return 'var(--cyan)'; }
function costHist() { return [{ usd: 1 }, { usd: 2 }]; }
function computeHourlyBurnRates() { return [{ rate: 4 }, { rate: 8 }]; }
` + block + `
const fixtures = {
  'overview-section': 'busy · 1 repos',
  'governor': 'busy · 7 pressure',
  'advisory-section': '3 advisories',
  'hive-advice-section': '2 recommendations',
  'pr-throughput-section': '2 merged · 3 opened (24h)',
  'token-panel': '8 tok/hr · 2 sessions (24h)',
  'cost-panel': '$2.00',
  'repos-section': '1 repos · 3 open PRs · 2 issues',
  'acmm-eval-section': 'L3',
  'audit-section': '5 events today',
  'review-queue-section': '6 PRs',
  'inception-section': '2 proposals · 1 in progress',
  'knowledge-section': '9 facts',
  'contributors-section': '2 contributors · 1 active',
  'debug-section': 'healthy',
  'agents-section': '2 agents · 1 on · 1 off',
  'lifecycle-section': '1 in flight · 2 merged · 1 blocked',
  'faq-section': '8 questions'
};
for (const [section, text] of Object.entries(fixtures)) {
  const html = visualSectionSummary(section, text);
  if (section === 'advisory-section') {
    if (!html.includes('advisory-collapsed-advice') || !html.includes('Fix 4 blocked PRs') || !html.includes('−4 queue')) throw new Error(section + ' did not render advice pills: ' + html);
  } else if (section === 'acmm-eval-section') {
    if (!html.includes('acmm-collapsed-pill') || !html.includes('L6') || !html.includes('7/7 dims')) throw new Error(section + ' did not render ACMM pill: ' + html);
  } else if (section === 'faq-section') {
    if (html !== '') throw new Error(section + ' should not render a collapsed mini-stat: ' + html);
  } else if (!/svg class="mini-[^"]+"/.test(html)) throw new Error(section + ' did not render a mini SVG: ' + html);
  if (section !== 'faq-section' && !html.includes('aria-label="')) throw new Error(section + ' missing accessible label');
}
_acmmEvalData = { overall_level: 0, operational_level: 6, levels: [
  { level: 0, name: 'Prerequisites not met', passed: false, matched: 0, total: 8 },
  { level: 1, name: 'Inception', passed: true, matched: 3, total: 3 },
  { level: 6, name: 'Fully Autonomous', passed: false, matched: 8, total: 10 }
] };
window._lastStatus.acmmLevel = 6;
let acmm = visualSectionSummary('acmm-eval-section', 'L6 · last eval now');
if (!acmm.includes('L6 · Fully Autonomous') || acmm.includes('>L0<') || acmm.includes('L0 Prerequisites')) throw new Error('ACMM collapsed pill must show the current level, not the lowest evaluated level: ' + acmm);
if (!acmm.includes('2 gaps')) throw new Error('ACMM collapsed pill should report current-level gaps when no next level exists: ' + acmm);
if (!acmm.includes('current level L6 · Fully Autonomous')) throw new Error('ACMM collapsed tooltip should identify current level: ' + acmm);
_acmmEvalData = null;
acmm = visualSectionSummary('acmm-eval-section', 'L6 · last eval pending');
if (!acmm.includes('L6 · Fully Autonomous') || !acmm.includes('not yet evaluated')) throw new Error('ACMM collapsed pill should prefer navbar level and show unevaluated placeholder: ' + acmm);
window._lastStatus.acmmLevel = 5;
_acmmEvalData = { overall_level: 0, operational_level: 6, levels: [{ level: 5, name: 'Semi-Autonomous', passed: true, matched: 4, total: 4 }, { level: 6, name: 'Fully Autonomous', passed: false, matched: 7, total: 10 }] };
acmm = visualSectionSummary('acmm-eval-section', 'L5 · last eval now');
if (!acmm.includes('L5 · Semi-Autonomous') || acmm.includes('>L6<')) throw new Error('ACMM collapsed pill should agree with navbar level instead of eval overall/next level: ' + acmm);
if (!acmm.includes('3 gaps to L6')) throw new Error('ACMM collapsed pill should count gaps toward next level when present: ' + acmm);
window._lastStatus.acmmLevel = 6;
_acmmEvalData = { overall_level: 6, operational_level: 6, codebase_level: 6, criteria_passed: 7, criteria_total: 7, criteria_results: [
  { category: 'Build', passed: true }, { category: 'Tests', passed: true }, { category: 'Docs', passed: true },
  { category: 'Security', passed: true }, { category: 'Release', passed: true }, { category: 'Ops', passed: true }, { category: 'Policy', passed: true }
] };
const knowledge = visualSectionSummary('knowledge-section', '1,526 facts');
if (!knowledge.includes('1,526 facts')) throw new Error('knowledge summary should preserve spaced fact count: ' + knowledge);
if (knowledge.includes('1,526facts')) throw new Error('knowledge summary collapsed number and label: ' + knowledge);
if (!knowledge.includes('mini-knowledge-facts') || !knowledge.includes('Fact count') || !knowledge.includes('recorded time span')) throw new Error('knowledge chart must identify its metric and time span');
if (JSON.stringify(lastSparkSeries) !== '[1500,1526]') throw new Error('knowledge must use server history');
_factHistory = [{t: 1000, count: 12}, {t: 2000, count: 0}, {t: 3000, count: 4}];
visualSectionSummary('knowledge-section', '4 facts');
if (JSON.stringify(lastSparkSeries) !== '[12,0,4]') throw new Error('zero fact counts must not be filled forward');
_factHistory = [{t: 1000, count: 7}];
visualSectionSummary('knowledge-section', '7 facts');
if (JSON.stringify(lastSparkSeries) !== '[7]') throw new Error('single sample must not fabricate growth from zero');
_factHistory = [];
const emptyKnowledge = visualSectionSummary('knowledge-section', '0 facts');
if (!emptyKnowledge.includes('0 facts') || !emptyKnowledge.includes('no history yet') || emptyKnowledge.includes('mini-knowledge-facts')) throw new Error('empty history must retain count without invented trend');
_factHistory = [{count: null}, {count: -1}, {count: 'bad'}];
if (!visualSectionSummary('knowledge-section', '0 facts').includes('no history yet')) throw new Error('invalid samples must be ignored');
window._auditSummary = { histogram: [], sensitive_24h: 0, today: 0, last: null };
const audit = visualSectionSummary('audit-section', '99 events today');
if (!audit.includes('>0 events today<')) throw new Error('audit summary should use live today count and spaced label: ' + audit);
if (audit.includes('0eventstod') || audit.includes('entries today')) throw new Error('audit summary retained malformed/old label: ' + audit);
const oneCadence = { id: 'add-agent-cadence', title: 'Add cadence', signals: [{ name: 'no_cadence_agents', value: '1' }], items: [{}] };
const twoCadence = { id: 'add-agent-cadence', title: 'Add cadence', signals: [{ name: 'no_cadence_agents', value: '2' }], items: [{}, {}] };
const twoDisabled = { id: 'enable-disabled-agent', title: 'Enable disabled agents', signals: [{ name: 'disabled_agents', value: '2' }], items: [{}, {}] };
if (advisoryCollapsedHeadline(oneCadence, advisoryCollapsedImpact(oneCadence)) !== 'Set cadence on 1 agent') throw new Error('singular cadence headline failed');
if (advisoryCollapsedHeadline(twoCadence, advisoryCollapsedImpact(twoCadence)) !== 'Set cadence on 2 agents') throw new Error('plural cadence headline failed');
if (advisoryCollapsedImpact(oneCadence).label !== '+1 kick') throw new Error('singular kick impact failed');
if (advisoryCollapsedImpact(twoCadence).label !== '+2 kicks') throw new Error('plural kick impact failed');
if (advisoryCollapsedImpact(twoDisabled).label !== '+2 lanes') throw new Error('plural lane impact failed');
for (const section of ['knowledge-section', 'audit-section', 'overview-section', 'pr-throughput-section', 'token-panel']) {
  const html = visualSectionSummary(section, fixtures[section]);
  if (/mini-spark/.test(html)) throw new Error(section + ' rendered an unlabelled collapsed-header sparkline: ' + html);
}
miniHistorySeries('persist-section', 'count', 10);
miniHistorySeries('persist-section', 'count', 12);
const saved = JSON.parse(store.get('hive_visual_summary_history:hive-visual-test'));
if (!saved['persist-section:count'] || saved['persist-section:count'].length !== 2 || saved['persist-section:count'][1].v !== 12) throw new Error('history did not persist');
const restored = miniHistorySeries('persist-section', 'count');
if (restored.length !== 2 || restored[0] !== 10 || restored[1] !== 12) throw new Error('history did not restore');
`

	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("visual summary JS failed: %v\n%s", err, out)
	}
}

func TestDashboardCostTileMathFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: cost tile math behavior was not executed")
	}
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	start := strings.Index(html, "    function fmtUSD(n) {")
	if start < 0 {
		t.Fatal("cost formatting helper block not found")
	}
	end := strings.Index(html[start:], "    // Last cost payload")
	if end < 0 {
		t.Fatal("cost formatting helper block end not found")
	}
	block := html[start : start+end]
	tokStart := strings.Index(html, "    function fmtTokens(n) {")
	if tokStart < 0 {
		t.Fatal("fmtTokens helper not found")
	}
	tokEnd := strings.Index(html[tokStart:], "    function pillAge(")
	if tokEnd < 0 {
		t.Fatal("fmtTokens helper end not found")
	}
	block += html[tokStart : tokStart+tokEnd]

	script := `
const assert = require('node:assert/strict');
function fmtCostDateTime(ms) { return new Date(ms).toISOString(); }
function fmtOverviewStaleSince(ts) { return new Date(ts).toISOString(); }
function formatNumber(value, locale, options) { try { return new Intl.NumberFormat(locale || undefined, options || {}).format(value); } catch (e) { return String(value); } }
` + block + `
assert.equal(fmtUSD(0), '$0.00');
assert.equal(fmtUSD(0.004321), '$0.00432');
assert.equal(fmtUSD(0.00999), '$0.00999');
assert.equal(fmtUSD(5.48), '$5.48');

let pr = costPerUnitDisplay(5.48, 937, 'PRs merged by hive');
assert.equal(pr.value, '$0.00585');
assert.ok(pr.title.startsWith('$5.48 ÷ 937 PRs merged by hive = $0.00585'), pr.title);
assert.ok(/not additive/.test(pr.title), 'per-unit tooltip must say the tiles share one total');

pr = costPerUnitDisplay(14005.48, 937, 'PRs merged by hive');
assert.equal(pr.value, '$14.95');
assert.ok(pr.title.startsWith('$14,005 ÷ 937 PRs merged by hive = $14.95'), pr.title);

let issue = costPerUnitDisplay(14005.48, 1182, 'hive-attributed closed issues');
assert.equal(issue.value, '$11.85');

let none = costPerUnitDisplay(0, 937, 'hive-attributed merged PRs');
assert.equal(none.value, '—');
assert.equal(none.title, 'no cost records yet');

let div0 = costPerUnitDisplay(5.48, 0, 'hive-attributed closed issues');
assert.equal(div0.value, '—');
assert.equal(div0.title, 'no hive-attributed closed issues yet');

let unavailable = costPerUnitDisplay(5.48, 0, 'hive-attributed closed issues', false, 'counts unavailable (GitHub rate-limited)');
assert.equal(unavailable.value, '—');
assert.equal(unavailable.title, 'counts unavailable (GitHub rate-limited)');
assert.equal(costCountLabel({count_available:false}, 0, 'PRs merged by hive', 'since Jul 24'), 'counts unavailable (GitHub rate-limited)');
// Stale denominators carry their as-of time instead of a bare "stale" (#13484).
assert.equal(costCountLabel({count_available:true,count_stale:true,count_status:'persisted',count_stale_since:'2026-08-18T00:00:00Z'}, 937, 'PRs merged by hive', 'since Jul 24'), 'total ÷ 937 PRs merged by hive since Jul 24 · counts stale since 2026-08-18T00:00:00.000Z (persisted)');
assert.equal(costCountLabel({count_available:true,count_stale:true,count_status:'stale: GitHub rate-limited',count_stale_since:'2026-08-18T00:00:00Z'}, 283, 'PRs merged by hive', 'since Sep 13'), 'total ÷ 283 PRs merged by hive since Sep 13 · counts stale since 2026-08-18T00:00:00.000Z (GitHub rate-limited)');
assert.equal(costCountLabel({count_available:true,count_updated_at:'2026-08-18T01:00:00Z'}, 937, 'PRs merged by hive', 'since Jul 24'), 'total ÷ 937 PRs merged by hive since Jul 24 · counts as of 2026-08-18T01:00:00.000Z');

// Headline wording: plain label, no "lineage" / "persisted history" jargon, says it is not a bill.
const total = { usd: 14005.48, firstTimestamp: 1, series: [{usd: 1}], currentUSD: 5.48, currentStart: 3, resets: [{ timestamp: 3, previous: 14000, next: 5.4 }] };
const label = costTotalLabel(total);
assert.ok(label.startsWith('Estimated spend at list price — since '), label);
assert.ok(/not a bill/.test(label), label);
assert.ok(!/lineage|persisted/i.test(label), label);
const live = costLiveSummaryLabel(total);
assert.ok(live.startsWith('of which $5.48 is in the live token summary since the last reset on '), live);
assert.ok(!/lineage/i.test(live), live);
const tip = costTotalTooltip(total, { price_table_date: '2026-10-01' });
assert.ok(/public list price \(price table 2026-10-01\)/.test(tip), tip);
assert.ok(/Not an invoice/.test(tip), tip);
assert.ok(/reset 1 time/.test(tip), tip);
assert.equal(costTotalLabel({ firstTimestamp: 0, series: [] }), 'Estimated spend at list price — no recorded history yet');

// Token formatting keeps ≥2 significant digits (no "0.000B" for sub-million counts).
assert.equal(fmtTokens(0), '0');
assert.equal(fmtTokens(950), '950');
assert.equal(fmtTokens(32963), '33.0K');
assert.equal(fmtTokens(176372), '176K');
assert.equal(fmtTokens(2500000), '2.50M');
assert.equal(fmtTokens(61e6), '61.0M');
assert.equal(fmtTokens(1.234e9), '1.23B');
assert.equal(fmtTokens(325e9), '325B');
`

	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cost tile JS failed: %v\n%s", err, out)
	}
}

func TestDashboardCostAllTimePreservesLineageResets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: cost lineage behavior was not executed")
	}
	html := indexHTML(t)

	// The headline is ONE estimator shared with the chart: persisted rollup
	// days the ring no longer holds + positive, discontinuity-guarded
	// increments inside the ring; the first snapshot's value is the opening
	// balance only when the ring is the whole history. It must not double
	// count the residual that survives a partial reset, must not change when
	// the ring evicts a sample, and must take its "since" from the oldest
	// rollup day.
	script := "const assert = require('node:assert/strict');\n" +
		"process.env.TZ = 'UTC';\n" +
		"let _costHistory = [];\n" +
		"let _lastCost = null;\n" +
		"const localStorage = { _m: {}, getItem(k){ return k in this._m ? this._m[k] : null; }, setItem(k, v){ this._m[k] = String(v); } };\n" +
		"function esc(s){ return String(s); }\n" +
		"function fmtUSD(n){ return '$' + Number(n).toFixed(2); }\n" +
		"function formatDashboardDateTime(v){ return new Date(v).toISOString(); }\n" +
		"function renderSparkline(){ return ''; }\n" +
		"function fmtSparkVal(v){ return String(v); }\n" +
		"function capDashboardArray(a){ return a; }\n" +
		costRangeScript(t, html) + "\n" +
		`
const H = 3600e3, D = 24 * H;
// Ring is the whole history: opening balance 1000, then +13000, then a reset
// to 5.40 (residual that must NOT be counted again), then +0.08.
_costHistory = [
  { t: 1, usd: 1000 },
  { t: 2, usd: 14000 },
  { t: 3, usd: 5.40 },
  { t: 4, usd: 5.48 }
];
const currentLineage = costCurrentLineageHist(5.48);
assert.deepEqual(currentLineage.map(e => e.usd), [5.40, 5.48]);

let allTime = costAllTimeEstimate({ total_usd: 5.48, window_start: 3 });
assert.ok(Math.abs(allTime.usd - 14000.08) < 1e-9, 'opening + increments, residual counted once: ' + allTime.usd);
assert.equal(allTime.firstTimestamp, 1);
assert.equal(allTime.openingUSD, 1000);
assert.deepEqual(allTime.series.map(e => Math.round(e.usd * 100) / 100), [1000, 14000, 14000, 14000.08]);
assert.equal(allTime.resets.length, 1);
assert.equal(allTime.resets[0].timestamp, 3);
assert.equal(allTime.resets[0].previous, 14000);
assert.equal(allTime.resets[0].next, 5.4);

// Partial reset: 13,964 → 5,058 (the old peak-sum added the 5,058 again).
_costHistory = [ { t: 1, usd: 0 }, { t: 2, usd: 13964 }, { t: 3, usd: 5058 }, { t: 4, usd: 5100 } ];
allTime = costAllTimeEstimate({ total_usd: 5100, window_start: 3 });
assert.ok(Math.abs(allTime.usd - 14006) < 1e-9, 'partial reset residual not double counted: ' + allTime.usd);

// Dip-and-regrow inside one lineage (console's shape): every regrowth is spend.
_costHistory = [ { t: 1, usd: 0 }, { t: 2, usd: 100 }, { t: 3, usd: 60 }, { t: 4, usd: 100 }, { t: 5, usd: 160 } ];
allTime = costAllTimeEstimate({ total_usd: 160, window_start: 1 });
assert.ok(Math.abs(allTime.usd - 200) < 1e-9, 'regrowth after a dip counts: ' + allTime.usd);

// Rollup days before the ring: the ring starts mid Oct 3 at 50; rollups for
// Oct 1, 2 and 3 are frozen server-side. Oct 3's own in-ring steps are NOT
// counted again (the ring takes over from the last Oct 3 snapshot) and the
// first snapshot's 50 is NOT an opening balance (Oct 1–2 precede it).
const oct1 = Date.UTC(2026, 9, 1), oct3 = Date.UTC(2026, 9, 3), oct4 = Date.UTC(2026, 9, 4);
_lastCost = { daily_rollup: [
  { day: '2026-10-01', cost_usd: 10, samples: 288 },
  { day: '2026-10-02', cost_usd: 20, samples: 288 },
  { day: '2026-10-03', cost_usd: 30, samples: 288 },
] };
_costHistory = [
  { t: oct3 + 12 * H, usd: 50 },
  { t: oct3 + 13 * H, usd: 70 },   // inside Oct 3: already in the rollup
  { t: oct4 + 1 * H,  usd: 75 },   // Oct 4: ring
  { t: oct4 + 2 * H,  usd: 80 },
];
allTime = costAllTimeEstimate({ total_usd: 80, window_start: oct3 });
assert.ok(Math.abs(allTime.usd - 70) < 1e-9, 'rollups 60 + ring 10: ' + allTime.usd);
assert.equal(allTime.firstTimestamp, oct1, 'since = oldest rollup day');
assert.equal(allTime.rollupUSD, 60);
assert.equal(allTime.openingUSD, 0);
assert.equal(allTime.series[0].timestamp, oct1 + D);
assert.equal(allTime.series[0].rollup, true);

// Evicting the oldest ring sample does not move the figure or the date.
_costHistory = _costHistory.slice(1);
const after = costAllTimeEstimate({ total_usd: 80, window_start: oct3 });
assert.ok(Math.abs(after.usd - 70) < 1e-9, 'stable under ring eviction: ' + after.usd);
assert.equal(after.firstTimestamp, oct1);

// A re-seed jump that RESTORES ground already counted (20,800 → 394 reset,
// then 394 → 18,389 in one step) is a correction, not spend.
_lastCost = null;
_costHistory = [ { t: 1, usd: 0 }, { t: 2, usd: 20800 }, { t: 3, usd: 394 } ];
const t0 = Date.UTC(2026, 9, 10);
for (let i = 0; i < 40; i++) _costHistory.push({ t: t0 + i * 5 * 60e3, usd: 394 + i });
_costHistory.push({ t: t0 + 40 * 5 * 60e3, usd: 18350 + 39 });
allTime = costAllTimeEstimate({ total_usd: 18389, window_start: t0 });
assert.ok(Math.abs(allTime.usd - (20800 + 39)) < 1e-9, 'restore step discarded: ' + allTime.usd);

// The same-sized step to a NEW high is spend (a day's session flush on an
// Opus-class hive, or days revealed after a sampling gap).
_costHistory = [ { t: 1, usd: 0 } ];
for (let i = 0; i < 40; i++) _costHistory.push({ t: t0 + i * 5 * 60e3, usd: 394 + i });
_costHistory.push({ t: t0 + 40 * 5 * 60e3, usd: 18350 + 39 });
allTime = costAllTimeEstimate({ total_usd: 18389, window_start: t0 });
assert.ok(Math.abs(allTime.usd - 18389) < 1e-9, 'large step to a new high is spend: ' + allTime.usd);

// Direct rule checks.
assert.equal(costStepIsRestore(18000, 394, 18394, 20800, 500), true);
assert.equal(costStepIsRestore(18000, 394, 18394, 10000, 500), false, 'lands above the old peak');
assert.equal(costStepIsRestore(300, 394, 694, 20800, 500), false, 'below the ceiling is never a restore');
assert.equal(costStepIsRestore(1320, 10984, 12304, 13346, 500), false, 'closes less than 80% of the gap');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cost lineage JS failed: %v\n%s", err, out)
	}
}
