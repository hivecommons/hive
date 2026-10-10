package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestQualityCoverageMetrics_KnownAndUnknown pins #11150: the quality agent's
// metrics carry the measured coverage percentage, and omit it (rendering "—")
// when no badge reading is available rather than reporting a fabricated 0%.
func TestQualityCoverageMetrics_KnownAndUnknown(t *testing.T) {
	known := qualityCoverageMetrics(87, true)
	if known["coverage"] != 87 {
		t.Errorf("coverage = %v, want 87", known["coverage"])
	}
	if known["coverageTarget"] != coverageTarget {
		t.Errorf("coverageTarget = %v, want %d", known["coverageTarget"], coverageTarget)
	}
	if known["coverageSource"] != qualityCoverageSource {
		t.Errorf("coverageSource = %v, want %q", known["coverageSource"], qualityCoverageSource)
	}

	unknown := qualityCoverageMetrics(0, false)
	if _, present := unknown["coverage"]; present {
		t.Errorf("unknown coverage must be omitted, got %v", unknown["coverage"])
	}
	if unknown["coverageSource"] != qualityCoverageSource {
		t.Errorf("coverageSource = %v, want %q", unknown["coverageSource"], qualityCoverageSource)
	}
}

func TestMetricsCollector_CollectPublishesQualityCoverage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "88%"})
	}))
	defer srv.Close()

	mc := &MetricsCollector{badgeURL: srv.URL, metrics: make(map[string]any), logger: covBLogger()}
	mc.collect(context.Background())

	got := mc.Get()
	quality, ok := got[qualityAgentName].(map[string]any)
	if !ok {
		t.Fatalf("collect did not publish %q metrics: %v", qualityAgentName, got)
	}
	if quality["coverage"] != 88 {
		t.Errorf("quality coverage = %v, want 88", quality["coverage"])
	}
	ci, _ := got["ci-maintainer"].(map[string]any)
	if ci["coverage"] != 88 {
		t.Errorf("ci-maintainer coverage = %v, want 88", ci["coverage"])
	}
}

func TestMetricsCollector_CollectQualityCoverageUnknownWithoutBadge(t *testing.T) {
	mc := &MetricsCollector{metrics: make(map[string]any), logger: covBLogger()}
	mc.collect(context.Background())

	quality, ok := mc.Get()[qualityAgentName].(map[string]any)
	if !ok {
		t.Fatalf("collect did not publish %q metrics", qualityAgentName)
	}
	if _, present := quality["coverage"]; present {
		t.Errorf("no badge configured: quality coverage must be absent, got %v", quality["coverage"])
	}
}

func TestBuildOverviewCoverageReusesQualityMetrics(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "hivecommons"
	cfg.Project.PrimaryRepo = "hive"
	got := buildOverviewCoverage(map[string]any{
		qualityAgentName: map[string]any{
			"coverage":       87,
			"coverageTarget": coverageTarget,
			"coverageSource": qualityCoverageSource,
		},
	}, cfg)
	if got == nil {
		t.Fatal("overview coverage payload missing")
	}
	if got.Coverage != 87 || got.Target != coverageTarget || got.Source != qualityCoverageSource || got.Repo != "hivecommons/hive" {
		t.Fatalf("overview coverage = %+v", got)
	}
	if missing := buildOverviewCoverage(map[string]any{qualityAgentName: map[string]any{}}, cfg); missing != nil {
		t.Fatalf("missing coverage should hide overview tile, got %+v", missing)
	}
}

func TestMeasureCoverage(t *testing.T) {
	if _, ok := (&MetricsCollector{}).measureCoverage(context.Background()); ok {
		t.Error("measureCoverage with no badge URL reported ok")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a badge"))
	}))
	defer srv.Close()
	mc := &MetricsCollector{badgeURL: srv.URL, metrics: make(map[string]any), logger: covBLogger()}
	if _, ok := mc.measureCoverage(context.Background()); ok {
		t.Error("measureCoverage with a malformed badge reported ok")
	}
}

// TestDefaultStatsConfig_QualityCoverage pins the default quality stat strip:
// a coverage pct-bar fed from agentMetrics, keyed so it is never mistaken for
// ci-maintainer's cloned strip (#7411).
func TestDefaultStatsConfig_QualityCoverage(t *testing.T) {
	stats := defaultStatsConfig(qualityAgentName)
	if len(stats) != 1 {
		t.Fatalf("quality default stats = %d entries, want 1", len(stats))
	}
	m, _ := stats[0].(map[string]any)
	for k, want := range map[string]any{
		"key": "testCoverage", "source": "agentMetrics", "field": "coverage", "style": "pct-bar", "target": coverageTarget,
	} {
		if m[k] != want {
			t.Errorf("quality stat %s = %v, want %v", k, m[k], want)
		}
	}
	desc, _ := m["desc"].(string)
	if !strings.Contains(desc, qualityCoverageSource) {
		t.Errorf("quality stat desc %q does not name its data source", desc)
	}
	if isClonedCIMaintainerStrip(qualityAgentName, stats) {
		t.Error("quality default strip is recognised as a cloned ci-maintainer strip and would be pruned")
	}
}

// TestQualityStatsCardRendersCoverageOrDash renders the default quality strip
// through the real dashboard JS: a measured value shows as "NN%", an unknown
// one as "—" (never "0%").
func TestQualityStatsCardRendersCoverageOrDash(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: quality coverage rendering was not executed")
	}
	statsJSON, err := json.Marshal(defaultStatsConfig(qualityAgentName))
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString(`
const window = { _healthData: {}, _tokensByAgent: {}, _lastStatus: { repos: [] }, _trendData: [] };
let currentAgentMetrics = {};
let historyData = [];
function getHistoryTimes() { return []; }
function sparkSvg() { return '<svg></svg>'; }
function prMergeable() { return true; }
function escapeHtml(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
const SPARK_COLORS = {};
const DEFAULT_SPARK_COLOR = 'var(--blue)';
`)
	for _, name := range []string{"resolveStatValue", "statTargetHref", "qualityDiagnosticsAgent", "renderQualityStatValue", "renderQualityStatsCard"} {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const agents = [{ name: 'quality', statsConfig: ` + string(statsJSON) + ` }];
currentAgentMetrics = { quality: { coverage: 87 } };
const known = renderQualityStatsCard(agents);
currentAgentMetrics = { quality: {} };
const unknown = renderQualityStatsCard(agents);
process.stdout.write(JSON.stringify({ known, unknown }));
`)
	out, err := exec.Command(node, "-e", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("node render failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	var got struct{ Known, Unknown string }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode render output: %v\n%s", err, out)
	}
	for _, want := range []string{"Test coverage", "87%", "goal: 91%", "HIVE_COVERAGE_BADGE_URL"} {
		if !strings.Contains(got.Known, want) {
			t.Errorf("known render missing %q: %s", want, got.Known)
		}
	}
	if !strings.Contains(got.Unknown, `<span class="ind-num ">—</span>`) || strings.Contains(got.Unknown, ">0%</span>") {
		t.Errorf("unknown coverage must render as —, got: %s", got.Unknown)
	}
}
