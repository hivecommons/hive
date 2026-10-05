package dashboard

import (
	"strings"
	"testing"
)

// Operator feedback on #9027: SVG bar labels ran into their bars at a
// different font size from the legend. Bars and histograms now render as one
// HTML grid (label | track | count) at the legend's font size.
func TestOverviewChartPolish(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function overviewHBarRow(label, tooltip, fills, value)",
		`<div class="overview-hbar" role="img"`,
		".overview-hbar { display: grid; grid-template-columns: max-content minmax(0, 1fr) auto; column-gap: var(--sp-2); row-gap: var(--sp-3); align-items: center; width: 100%; font-size: var(--fs-sm);",
		".overview-hbar-label { color: var(--text); font-weight: 600; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;",
		"function toggleOverviewCarousel()",
		"function overviewTransitionOptions(selected)",
		`data-change-action="setOverviewTransition" data-arg-types="v" title="Transition between charts"`,
		".overview-chart-controls .overview-chart-select { font-size: var(--fs-xs);",
		".overview-chart-card { --overview-chart-card-min-height: 16rem;",
		"grid-template-columns: minmax(200px, 260px) minmax(0, 1fr); align-items: center;",
		".overview-chart-download { position: absolute;",
		".overview-chart-subtitle { color: var(--muted); font-size: var(--fs-base);",
		".overview-chart-label { fill: var(--text); font-size: var(--fs-sm);",
		".overview-chart-total { fill: var(--text); font-size: var(--fs-2xl);",
		".overview-chart-total-caption { fill: var(--muted); font-size: var(--fs-sm);",
		".overview-chart-line { fill: none; stroke: var(--slice-c); stroke-width: 1.5; }",
		"OVERVIEW_STACK_W = 220, OVERVIEW_STACK_H = 98, OVERVIEW_STACK_X = 10, OVERVIEW_STACK_Y = 48, OVERVIEW_STACK_BAR_W = 200, OVERVIEW_STACK_BAR_H = 18, OVERVIEW_STACK_TOTAL_Y = 24",
		`y="${OVERVIEW_STACK_TOTAL_Y}">${total} total</text>`,
		".overview-chart-legend-row { display: grid; grid-template-columns: auto minmax(0, 1fr) auto auto; align-items: center; gap: var(--sp-2); color: var(--muted); font-size: var(--fs-sm);",
		".overview-chart-legend-count { font-size: var(--fs-sm); font-variant-numeric: tabular-nums;",
		"OVERVIEW_TRANSITION_DURATION_MS = { fast: 1000, normal: 2000, slow: 3600 }",
		"rootStyle.setProperty('--overview-transition-' + duration, overviewTransitionDurationMs({ duration }) + 'ms')",
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
		".overview-chart-card:has(.overview-hbar)",
	} {
		if strings.Contains(html, gone) {
			t.Errorf("SVG bar/histogram text should be gone, found %q", gone)
		}
	}
}
