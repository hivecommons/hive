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
  tokens: { totals: { sessions: 2 } }
} };
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
  'beads-section': '4 beads',
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
  if (!/svg class="mini-[^"]+"/.test(html)) throw new Error(section + ' did not render a mini SVG: ' + html);
  if (!html.includes('aria-label="')) throw new Error(section + ' missing accessible label');
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
