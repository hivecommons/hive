package dashboard

import (
	"strings"
	"testing"
)

// TestLifecycleJourneysPanelPinned pins Panel B's journey rendering (#5656).
// Before this, the panel listed raw events and only ever showed the latest
// re-stamped enumeration sweep with "0 merged / 0 blocked" forever. The
// invariants:
//
//  1. The panel renders JOURNEYS (one row per work item) from the DTO's
//     `journeys` array, not raw events.
//  2. Each row shows stage chips along the fixed lifecycle axis, with the
//     current stage colored via the shared v4KindClass mapping and earlier
//     stages dimmed.
//  3. The fleet counters carry an honest coverage label derived from
//     fleet.coveredMs — never claiming a 6h window over minutes of history.
//  4. The fetch path and empty state stay wired.
func TestLifecycleJourneysPanelPinned(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// Journey rendering, not raw events.
		"renderLifecycle",
		"dto.journeys",
		`<li class="lc-journey">`,
		`<ul class="lc-journeys">`,
		`aria-expanded=`,
		`data-keydown-action="toggleLifecycleJourney"`,
		"lcExpandedJourneys",
		"lifecycleExpandAll",
		"lifecycleCollapseAll",
		"lcRenderDetails",
		`target="_blank" rel="noopener"`,
		// Fixed stage axis + chips through the shared kind→color mapping.
		"LC_STAGE_AXIS",
		"['enumerated', 'classified', 'kicked', 'pr_opened', 'merged', 'blocked']",
		"v4KindClass(k)",
		"lc-stage past",
		`<span class="lc-stage-arrow">→</span>`,
		// Honest window labeling from real coverage.
		"lcWindowLabel",
		"fleet.coveredMs",
		"of recorded history",
		// Counters remain, fed by the journeys roll-up.
		`<div class="val inflight">`,
		`<div class="val merged">`,
		`<div class="val blocked">`,
		// Fetch path + calm empty state.
		"'/api/lifecycle-timeline?limit=50'",
		"No lifecycle journeys yet",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("lifecycle journeys panel missing snippet %q", snippet)
		}
	}
}

// TestLifecycleJourneysPanelDropsRawEventList: the flooding raw-event list
// must not come back alongside the journey view.
func TestLifecycleJourneysPanelDropsRawEventList(t *testing.T) {
	html := indexHTML(t)
	for _, gone := range []string{
		`<ul class="lc-events">`,
		"No lifecycle events yet",
	} {
		if strings.Contains(html, gone) {
			t.Fatalf("raw-event lifecycle markup %q should be gone", gone)
		}
	}
}
