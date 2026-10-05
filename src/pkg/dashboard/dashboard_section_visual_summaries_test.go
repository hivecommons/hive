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
function renderSparkline(_el, series, opts) { return '<span class="sparkline"><svg class="' + opts.svgClass + '" role="img"><title>' + escapeHtml(opts.title || '') + '</title><polyline class="sparkline-line" points="0,0 1,1"/></svg></span>'; }
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
  } else if (!/svg class="mini-[^"]+"/.test(html)) throw new Error(section + ' did not render a mini SVG: ' + html);
  if (!html.includes('aria-label="')) throw new Error(section + ' missing accessible label');
}
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
