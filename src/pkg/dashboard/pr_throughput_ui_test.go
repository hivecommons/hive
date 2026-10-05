package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestPRThroughputCardPinned pins the Change Throughput card (#9432): it fetches
// /api/pr-throughput for a selectable window (including all time), renders
// opened / merged / closed-without-merging counts, splits merges by path, and
// labels how far back the data goes the way the Lifecycle Timeline does.
func TestPRThroughputCardPinned(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`id="pr-throughput-section"`,
		`id="pr-throughput-card"`,
		`📊 Change Throughput`,
		`pull/merge requests across tracked forges`,
		`data-arg0="pr-throughput-section"`,
		"function renderPRThroughput",
		"function fetchPRThroughput",
		"/api/pr-throughput?hours=",
		"[[1, '1h'], [6, '6h'], [12, '12h'], [24, '24h'], [48, '48h'], [168, '7d'], [0, 'all']]",
		`data-action="setPRThroughputHours"`,
		"window.setPRThroughputHours = setPRThroughputHours",
		"window.setPRThroughputRole = setPRThroughputRole",
		"'pr-throughput-section': { title: '📊 Change Throughput', summary: '0 merged · 0 opened (24h)', subtitleHtml: 'pull/merge requests across tracked forges' }",
		`<div class="lbl">Opened</div>`,
		`<div class="lbl">Merged</div>`,
		`<div class="lbl">Closed without merging</div>`,
		"function prtActorMatrix",
		"Actor attribution",
		"hive · human · other",
		"function prtTrendSvg",
		`<svg class="prt-trend-svg"`,
		`class="area ${key}"`,
		"100% stacked area trend",
		"non-hive bots (dependabot, renovate, GitHub Actions, …)",
		"function prtTrendCaption",
		"No attribution data yet — counters start now.",
		"setSectionSummary('pr-throughput-section'",
		"dto.merged_by_path",
		"dto.by_actor",
		"dto.series",
		"function prtWindowLabel",
		"dto.recorded_since",
		"of recorded history",
		"fetchPRThroughput(); fetchApprovals();",
		"'governor','pr-throughput-section','repos-section',",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("Change Throughput card missing snippet %q", snippet)
		}
	}
}

func TestPRThroughputSectionIsTopLevel(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`<div id="pr-throughput-section" data-dashboard-section="pr-throughput-section" hidden>`,
		`data-section="pr-throughput-section" data-action="ocNavigate" data-arg0="pr-throughput-section"><span class="oc-nav-emoji">📊</span><span class="oc-nav-text">Change Throughput</span>`,
		`DASHBOARD_LAYOUT_TEMPLATE={main:['overview-section','governor','pr-throughput-section','repos-section'`,
		`h === 'throughput' || h.startsWith('section-')`,
		`if (h === 'throughput') return 'pr-throughput-section';`,
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("Change Throughput top-level wiring missing snippet %q", snippet)
		}
	}
	if strings.Contains(html, `id="pr-throughput-section" class="advisory-subsection"`) {
		t.Fatal("Change Throughput is still nested as an Advisory subsection")
	}
	gov := strings.Index(html, `<div class="governor" id="governor"></div>`)
	throughput := strings.Index(html, `<div id="pr-throughput-section" data-dashboard-section="pr-throughput-section" hidden>`)
	advisory := strings.Index(html, `<div id="advisory-section" data-dashboard-section="advisory-section"`)
	if gov < 0 || throughput < 0 || advisory < 0 {
		t.Fatalf("missing governor/throughput/advisory structural markers: gov=%d throughput=%d advisory=%d", gov, throughput, advisory)
	}
	if !(gov < throughput && throughput < advisory) {
		t.Fatalf("Change Throughput must be a top-level sibling after Governor and before Advisory: gov=%d throughput=%d advisory=%d", gov, throughput, advisory)
	}
}

func TestPRThroughputCollapsedHeadlineKeepsWindowPill(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Change Throughput headline window controls were not executed")
	}
	html := indexHTML(t)
	render := jsFunc(t, html, "renderPRThroughput")
	for _, snippet := range []string{
		`<div class="lc-fleet prt-headline sec-headline" data-collapsed-keep>`,
		"${prtWindowControls()}",
	} {
		if !strings.Contains(render, snippet) {
			t.Fatalf("Change Throughput render missing collapsed-headline window control snippet %q", snippet)
		}
	}
	script := `const assert = require('node:assert/strict');
const PRT_WINDOWS = [[1, '1h'], [6, '6h'], [12, '12h'], [24, '24h'], [48, '48h'], [168, '7d'], [0, 'all']];
let prtHours = 168;
` + jsFunc(t, html, "prtWindowControls") + `
const out = prtWindowControls();
assert.match(out, /class="gov-pr-models-toggle prt-window-controls"/);
assert.match(out, /data-stop="1"/);
assert.match(out, /data-action="setPRThroughputHours"/);
assert.match(out, /data-arg0="168"[^>]*>7d<\/button>/);
assert.match(out, /class="active"[^>]*data-action="setPRThroughputHours"[^>]*data-arg0="168"/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node Change Throughput window controls failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestPRThroughputTrendStackedAreaAndLegendTooltip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Change Throughput actor trend helpers were not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
let prtRole = 'merged';
function escapeHtml(v) { return String(v).replace(/[&<>"]/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[ch])); }
function prtTrendCaption() { return 'caption'; }
` + jsFunc(t, html, "prtSeriesAllZero") + "\n" + jsFunc(t, html, "prtTrendSvg") + "\n" + jsFunc(t, html, "prtActorTrend") + `
const series = [
  { hive: 4, human: 0, other: 6 },
  { hive: 5, human: 0, other: 5 },
  { hive: 7, human: 0, other: 3 },
];
const svg = prtTrendSvg(series);
assert.match(svg, /class="area hive"/);
assert.match(svg, /class="area other"/);
assert.doesNotMatch(svg, /class="area human"/);
assert.doesNotMatch(svg, /class="series /);
assert.doesNotMatch(svg, /class="baseline"/);
const paths = Array.from(svg.matchAll(/<path class="area ([^"]+)" d="([^"]+)"/g));
assert.equal(paths.length, 2, svg);
assert.notEqual(paths[0][2], paths[1][2], 'stacked bands must not render duplicate line paths');
const trend = prtActorTrend({ series });
assert.match(trend, /title="non-hive bots \(dependabot, renovate, GitHub Actions, …\)"/);
assert.match(trend, /human · 0/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node Change Throughput actor trend failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
