package dashboard

import (
	"strings"
	"testing"
)

// Operator feedback on #9027: SVG bar labels ran into their bars at a
// different font size from the legend, and the carousel/transition controls
// were only reachable through the ⚙️ popover. Bars and histograms now render
// as one HTML grid (label | track | count) at the legend's font size, and
// each panel carries a visible auto-play toggle and transition picker.
func TestOverviewChartPolish(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function overviewHBarRow(label, tooltip, fills, value)",
		`<div class="overview-hbar" role="img"`,
		".overview-hbar { display: grid; grid-template-columns: max-content minmax(0, 1fr) auto;",
		".overview-hbar-label { color: var(--text); font-weight: 600; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;",
		".overview-chart-card:has(.overview-hbar)",
		`data-action="toggleOverviewCarousel"`,
		"function toggleOverviewCarousel()",
		"function overviewTransitionOptions(selected)",
		`data-change-action="setOverviewTransition" data-arg-types="v" title="Transition between charts"`,
		".overview-chart-controls .overview-chart-select { font-size: var(--fs-xs);",
		".overview-duration-normal { --overview-transition-duration: 500ms; }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, gone := range []string{
		"overview-chart-bar-label\" x=",
		"overview-chart-hist-cell\" x=",
		"OVERVIEW_BAR_LABEL_X",
		"OVERVIEW_HIST_LABEL_X",
	} {
		if strings.Contains(html, gone) {
			t.Errorf("SVG bar/histogram text should be gone, found %q", gone)
		}
	}
}
