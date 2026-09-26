package dashboard

import (
	"strings"
	"testing"
)

// #9036: ◀ ▶ step through every chart type (the same domain as the <select>),
// auto-play rotates only through the ticked types, and donut edge labels stay
// inside the SVG box instead of spilling into the legend or past the card edge.
func TestOverviewCarouselArrowsStepAllTypes(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function overviewCarouselStep(kind, delta, types) {",
		"const domain = Array.isArray(types) && types.length ? types : OVERVIEW_CHART_TYPES;",
		"function overviewCarouselPrev(kind) { overviewCarouselStep(kind, -1, OVERVIEW_CHART_TYPES); }",
		"function overviewCarouselNext(kind) { overviewCarouselStep(kind, 1, OVERVIEW_CHART_TYPES); }",
		"function overviewCarouselAutoNext(kind) { overviewCarouselStep(kind, 1, overviewEnabledTypes(overviewChartsState())); }",
		"overviewCarouselAutoNext(kind);",
		"function renderOverviewDots(enabled, active) {",
		"OVERVIEW_CHART_TYPES.map(type => {",
		"' — in auto-play rotation'",
		".overview-carousel-dot.ticked { border-color: var(--accent); }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, "const enabled = overviewEnabledTypes(state);\n      const current = enabled.indexOf(state.active[kind]);") {
		t.Error("overviewCarouselStep still restricts manual arrows to the ticked chart types")
	}
	if !strings.Contains(html, "if (!state.carousel || overviewEnabledTypes(state).length < 2) return;") {
		t.Error("auto-play timer must still require at least two ticked types")
	}
}

func TestOverviewDonutLabelsStayInsideSVG(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const OVERVIEW_CHART_VIEWBOX = 300;",
		"const OVERVIEW_CHART_LABEL_POLE_DEGREES = 12;",
		"anchor: nearPole ? 'middle' : (cos > 0 ? 'start' : 'end')",
		`<text class="overview-chart-label" text-anchor="${point.anchor}"`,
		".overview-chart-svg { width: 100%; max-width: 240px; height: auto; overflow: hidden; }",
		"grid-template-columns: minmax(200px, 240px) minmax(0, 1fr);",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, stale := range []string{
		"const OVERVIEW_CHART_VIEWBOX = 240;",
		".overview-chart-svg { width: 100%; max-width: 220px; height: auto; overflow: visible; }",
	} {
		if strings.Contains(html, stale) {
			t.Errorf("index.html still contains stale %q", stale)
		}
	}
}
