package dashboard

import (
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
		`stroke-width="1.5"`,
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
		"'governor','pr-throughput-section','advisory-section',",
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
		`DASHBOARD_LAYOUT_TEMPLATE={main:['runs-section','overview-section','governor','pr-throughput-section','advisory-section'`,
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
