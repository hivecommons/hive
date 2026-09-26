package dashboard

import (
	"strings"
	"testing"
)

func TestOverviewChartCarouselStaticWiring9025(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const OVERVIEW_CHARTS_KEY = 'hive-overview-charts'",
		"const OVERVIEW_CHART_TYPES = ['donut', 'pie', 'bar', 'stacked', 'line', 'histogram']",
		"function renderOverviewPie(title, subtitle, slices)",
		"function renderOverviewBar(title, subtitle, slices)",
		"function renderOverviewStacked(title, subtitle, slices)",
		"function renderOverviewLine(title, subtitle, slices, history)",
		"function renderOverviewHistogram(title, subtitle, slices)",
		"function overviewSampleHistory(kind, slices, state)",
		"OVERVIEW_HISTORY_MAX_SAMPLES = 288",
		"OVERVIEW_HISTORY_MIN_SAMPLE_MS = 60000",
		"state.history[kind] = history.slice(-OVERVIEW_HISTORY_MAX_SAMPLES)",
		"function overviewItemAgeMinutes(item)",
		"item.updated_at || item.created_at",
		"item.age_minutes ?? item.ageMinutes",
		"const overviewChartRenderers = {",
		"donut: renderOverviewDonutSVG",
		"pie: renderOverviewPie",
		"bar: renderOverviewBar",
		"stacked: renderOverviewStacked",
		"line: renderOverviewLine",
		"histogram: renderOverviewHistogram",
		"className: 'overview-issue-' + band",
		"className: 'overview-pr-' + band",
		"<title>${esc(s.label)}: ${s.count}${s.rule ? ' — ' + esc(s.rule) : ''}</title>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestOverviewCarouselControlsAndTransitions9025(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"data-action=\"toggleOverviewChartSettings\"",
		"id=\"overview-chart-settings\"",
		"function toggleOverviewChartSettings()",
		"data-change-action=\"setOverviewChartRotation\"",
		"data-change-action=\"setOverviewCarouselEnabled\"",
		"data-change-action=\"setOverviewCarouselInterval\"",
		"data-change-action=\"setOverviewTransition\"",
		"data-change-action=\"setOverviewDuration\"",
		"data-action=\"overviewCarouselPrev\"",
		"data-action=\"overviewCarouselNext\"",
		"overview-carousel-dot",
		"data-mouseover-action=\"overviewPanelHover\"",
		"data-mouseout-action=\"overviewPanelUnhover\"",
		"document.visibilityState !== 'visible'",
		"document.addEventListener('visibilitychange', overviewCarouselVisibilityChanged)",
		"_overviewCarouselTimers.splice(0).forEach(id => clearInterval(id))",
		"setInterval(() => {",
		"OVERVIEW_CAROUSEL_MIN_MS = 5000",
		"OVERVIEW_CAROUSEL_MAX_MS = 300000",
		"OVERVIEW_CAROUSEL_DEFAULT_MS = 30000",
		"transition: 'fade', duration: 'normal'",
		"carousel: false",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}

	for _, transition := range []string{"fade", "rise-from-bottom", "lower-from-top", "slide-from-left", "slide-from-right", "zoom", "flip", "blur", "random"} {
		if !strings.Contains(html, transition) {
			t.Errorf("index.html missing transition %q", transition)
		}
	}
	for _, cls := range []string{"overview-duration-fast", "overview-duration-normal", "overview-duration-slow"} {
		if !strings.Contains(html, cls) {
			t.Errorf("index.html missing duration class %q", cls)
		}
	}
	if !strings.Contains(html, "@media (prefers-reduced-motion: reduce) { .overview-panel-in, .overview-panel-out { animation: none; } }") {
		t.Errorf("index.html missing reduced-motion instant swap rule")
	}
	for _, forbidden := range []string{"window.prompt(", "window.alert(", "window.confirm(", "prompt(", "alert(", "confirm("} {
		if strings.Contains(html, forbidden) {
			t.Errorf("index.html must not use native dialog %q", forbidden)
		}
	}
	if strings.Contains(html, "time.Sleep") {
		t.Errorf("static wiring must not add sleep-based tests or UI timing")
	}
}
