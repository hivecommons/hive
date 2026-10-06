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
