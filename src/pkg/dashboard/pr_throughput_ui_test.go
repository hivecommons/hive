package dashboard

import (
	"strings"
	"testing"
)

// TestPRThroughputCardPinned pins the PR throughput card (#9432): it fetches
// /api/pr-throughput for a selectable window (including all time), renders
// opened / merged / closed-without-merging counts, splits merges by path, and
// labels how far back the data goes the way the Lifecycle Timeline does.
func TestPRThroughputCardPinned(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`id="pr-throughput-section"`,
		`id="pr-throughput-card"`,
		`data-arg0="pr-throughput-section"`,
		"function renderPRThroughput",
		"function fetchPRThroughput",
		"/api/pr-throughput?hours=",
		"[[1, '1h'], [6, '6h'], [12, '12h'], [24, '24h'], [48, '48h'], [168, '7d'], [0, 'all']]",
		`data-action="setPRThroughputHours"`,
		"window.setPRThroughputHours = setPRThroughputHours",
		`<div class="lbl">Opened</div>`,
		`<div class="lbl">Merged</div>`,
		`<div class="lbl">Closed without merging</div>`,
		"dto.merged_by_path",
		"function prtWindowLabel",
		"dto.recorded_since",
		"of recorded history",
		"fetchPRThroughput(); fetchApprovals();",
		"'lifecycle-section', 'pr-throughput-section',",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("PR throughput card missing snippet %q", snippet)
		}
	}
}
