package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSystemDiagnosticsQualityStatsCard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: System Diagnostics quality stats rendering was not executed")
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString(`
const window = {
  _healthData: { smoke: -1 },
  _tokensByAgent: {},
  _lastStatus: { repos: [] },
  _trendData: []
};
let currentAgentMetrics = { quality: { coverage: 92 } };
let historyData = [];
function getHistoryTimes() { return []; }
function sparkSvg() { return '<svg></svg>'; }
function prMergeable() { return true; }
function escapeHtml(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
const esc = escapeHtml;
const SPARK_COLORS = {};
const DEFAULT_SPARK_COLOR = 'var(--blue)';
`)
	for _, name := range []string{"resolveStatValue", "statTargetHref", "qualityDiagnosticsAgent", "renderQualityStatValue", "renderQualityStatsCard"} {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const agents = [{ name: 'quality', statsConfig: [
  { key: 'coverage', label: 'Coverage', source: 'agentMetrics', field: 'coverage', style: 'pct', target: 'https://example.invalid/coverage' },
  { key: 'smoke', label: 'Smoke', source: 'health', field: 'smoke', style: 'dot', icon: '🧪' }
]}];
const rendered = renderQualityStatsCard(agents);
const empty = renderQualityStatsCard([{ name: 'quality', statsConfig: [] }]);
process.stdout.write(JSON.stringify({ rendered, empty }));
`)
	out, err := exec.Command(node, "-e", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("node quality stats render failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	got := string(out)
	for _, want := range []string{
		`Quality stats`,
		`Coverage`,
		`href=\"https://example.invalid/coverage\"`,
		`92%`,
		`🧪 Smoke`,
		`⊘ skipped`,
		`No stats configured — add them under quality → Configuration → Stats.`,
		`data-action=\"openConfigDialog\"`,
		`data-agent=\"quality\"`,
		`data-tab=\"Stats\"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered quality stats card missing %q in %s", want, got)
		}
	}
}

func TestSystemDiagnosticsOldWorkflowNamesRemoved(t *testing.T) {
	html := indexHTML(t)
	for _, old := range []string{
		"Brew" + " Formula",
		"Nightly" + " Tests",
		"Nightly" + " Release",
		"Weekly" + " Rel",
		"vLLM" + "-d Deploy",
		"Pok" + "Prod Deploy",
		"Nightly" + " Rel",
		"deploy_" + "vllm_d",
		"deploy_" + "pok_prod",
	} {
		if strings.Contains(html, old) {
			t.Fatalf("index.html still contains old hard-coded workflow stat %q", old)
		}
	}
}
