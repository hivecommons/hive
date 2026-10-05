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
		`📊 Throughput`,
		`pull/merge requests across tracked forges`,
		`data-arg0="pr-throughput-section"`,
		"function renderPRThroughput",
		"function fetchPRThroughput",
		"/api/pr-throughput?hours=",
		"[[1, '1h'], [6, '6h'], [12, '12h'], [24, '24h'], [48, '48h'], [168, '7d'], [0, 'all']]",
		`data-action="setPRThroughputHours"`,
		"window.setPRThroughputHours = setPRThroughputHours",
		"window.setPRThroughputRole = setPRThroughputRole",
		"'pr-throughput-section': { title: '📊 Throughput', summary: '0 merged · 0 opened (24h)', subtitleHtml: 'pull/merge requests across tracked forges' }",
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
		"function prtActorTrend",
		"Hive vs human trend",
		"non-hive bots (dependabot, renovate, GitHub Actions, …)",
		"function prtTrendCaption",
		"function prtHiveHumanTile",
		`<div class="lbl">Hive vs human</div>`,
		`data-change-action="setPRThroughputRole"`,
		"['created','reviewed','merged','closed'].includes(String(role))",
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
		`data-section="pr-throughput-section" data-action="ocNavigate" data-arg0="pr-throughput-section"><span class="oc-nav-emoji">📊</span><span class="oc-nav-text">Throughput</span>`,
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
	for _, snippet := range []string{
		`.window-pills-toolbar { grid-column: 1 / -1; justify-self: end; max-width: 100%; flex-wrap: wrap; justify-content: flex-end; }`,
		`.window-pills-toolbar { justify-self: start; justify-content: flex-start; }`,
		`.prt-share-tile .prt-role-select { position:absolute; right:var(--sp-3); bottom:var(--sp-3);`,
		`.prt-share-tile .prt-tile-spark { max-width: calc(100% - 6.4rem); overflow: hidden; }`,
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("Change Throughput window controls must remain in-flow and wrapping, missing CSS snippet %q", snippet)
		}
	}
	if strings.Contains(html, `.prt-window-controls { position: absolute;`) {
		t.Fatal("Change Throughput window controls must not be absolutely positioned over the metric tiles")
	}
	if strings.Contains(html, `.prt-headline { padding-top:`) {
		t.Fatal("Change Throughput headline must not reserve a fixed top pad for out-of-flow controls")
	}
	if strings.Contains(html, `.prt-share-tile .prt-role-select { position:absolute; top:`) {
		t.Fatal("Hive vs human role select must stay at the tile's lower-right corner")
	}
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
assert.match(out, /class="gov-pr-models-toggle window-pills-toolbar prt-window-controls"/);
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
function renderSparkline(_node, values, opts) { return '<svg data-label="' + opts.label + '">' + (opts.area ? '<polygon class="sparkline-area"></polygon>' : '') + values.join(',') + '</svg>'; }
` + jsFunc(t, html, "prtSparkTitle") + "\n" + jsFunc(t, html, "prtHiveShareSpark") + "\n" + jsFunc(t, html, "prtSeriesAllZero") + "\n" + jsFunc(t, html, "prtTrendSvg") + "\n" + jsFunc(t, html, "prtRoleNoun") + "\n" + jsFunc(t, html, "prtWindowPhrase") + "\n" + jsFunc(t, html, "prtActorDefinition") + "\n" + jsFunc(t, html, "prtTrendStats") + "\n" + jsFunc(t, html, "prtTrendCaption") + "\n" + jsFunc(t, html, "prtHiveHumanTile") + `
const series = [
  { hive: 4, human: 2, other: 4 },
  { hive: 5, human: 3, other: 2 },
  { hive: 7, human: 1, other: 2 },
];
const previous_actor = { hive: 2, human: 6, other: 2 };
const dto = { hours: 24, series, previous_actor };
const svg = prtTrendSvg(series);
assert.match(svg, /class="area hive"/);
assert.match(svg, /class="area other"/);
assert.match(svg, /class="area human"/);
assert.doesNotMatch(svg, /class="series /);
assert.doesNotMatch(svg, /class="baseline"/);
const paths = Array.from(svg.matchAll(/<path class="area ([^"]+)" d="([^"]+)"/g));
assert.equal(paths.length, 3, svg);
assert.notEqual(paths[0][2], paths[1][2], 'stacked bands must not render duplicate line paths');
const tile = prtHiveHumanTile(dto);
assert.match(tile, /class="lc-fleet-stat prt-share-tile"/);
assert.match(tile, /<div class="val merged">53%<\/div>/);
assert.match(tile, /<div class="lbl">Hive vs human<\/div>/);
assert.match(tile, /hive share of PR merges \+ issue closes · last 1d · prev 20%/);
assert.match(tile, /53% = hive share of PRs merged \+ issues closed in last 1d/);
assert.match(tile, /Previous compares with previous 1d/);
assert.match(tile, /data-change-action="setPRThroughputRole"/);
assert.match(tile, /miniature 100% stacked area trend/);
assert.match(tile, /class="area human"/);
assert.doesNotMatch(tile, /sparkline-area/);
` + jsFunc(t, html, "prtActorTrend") + `
const trend = prtActorTrend(dto);
assert.match(trend, /class="prt-trend"/);
assert.match(trend, /<strong>Hive vs human trend<\/strong>/);
assert.match(trend, /title="non-hive bots \(dependabot, renovate, GitHub Actions, …\)"/);
assert.doesNotMatch(trend, /human · 0/);
assert.match(trend, /class="prt-trend-svg"/);
assert.match(trend, /hive 53% of PRs merged \+ issues closed in last 1d, up from 20% in previous 1d/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node Change Throughput actor trend failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
