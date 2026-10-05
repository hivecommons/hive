package dashboard

import (
	"os/exec"
	"regexp"
	"strconv"
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
		"id=\"overview-chart-settings-overlay\"",
		"class=\"config-modal overview-settings-modal\" id=\"overview-chart-settings\" role=\"dialog\" aria-modal=\"true\"",
		"aria-labelledby=\"overview-chart-settings-title\"",
		"data-action=\"closeOverviewChartSettings\"",
		"id=\"overview-repo-filter\"",
		"OVERVIEW_REPOS_KEY = 'hive.overview.repos'",
		"function toggleOverviewChartSettings()",
		"function openOverviewChartSettings()",
		"openOverviewChartSettings();",
		"function closeOverviewChartSettings()",
		"add(document.getElementById('overview-chart-settings-overlay'), closeOverviewChartSettings, 10000)",
		"data-change-action=\"setOverviewChartRotation\"",
		"data-change-action=\"setOverviewCarouselInterval\"",
		"data-change-action=\"setOverviewChartType\"",
		"data-change-action=\"setOverviewTransition\"",
		"data-change-action=\"setOverviewDuration\"",
		"data-action=\"overviewCarouselPrev\"",
		"data-action=\"overviewCarouselNext\"",
		"data-action=\"toggleOverviewCarousel\"",
		"overview-carousel-dot",
		"overview-settings-section",
		"aria-label=\"${esc(label)} chart controls\"",
		"data-mouseover-action=\"overviewPanelHover\"",
		"data-mouseout-action=\"overviewPanelUnhover\"",
		"document.visibilityState !== 'visible'",
		"document.addEventListener('visibilitychange', overviewCarouselVisibilityChanged)",
		"_overviewCarouselTimers.splice(0).forEach(id => clearInterval(id))",
		"setInterval(() => {",
		"OVERVIEW_CAROUSEL_MIN_MS = 5000",
		"OVERVIEW_CAROUSEL_MAX_MS = 300000",
		"OVERVIEW_CAROUSEL_DEFAULT_MS = 30000",
		"OVERVIEW_TRANSITION_DURATION_MS = { fast: 1000, normal: 2000, slow: 3600 }",
		"function overviewTransitionDurationMs(state)",
		"function overviewCarouselDwellMs(state)",
		"`--overview-transition-duration:${overviewTransitionDurationMs(state)}ms`",
		"}, overviewCarouselDwellMs(state)));",
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
	if !strings.Contains(html, "overview-duration-${state.duration}") {
		t.Errorf("index.html missing selected duration class wiring")
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

func TestOverviewCarouselControlsMovedToSettings9025(t *testing.T) {
	html := indexHTML(t)
	panel := jsFunc(t, html, "renderOverviewPanel")
	settings := jsFunc(t, html, "renderOverviewSettingsPopover")
	for _, gone := range []string{"overview-chart-controls", "data-action=\"overviewCarouselPrev\"", "data-action=\"toggleOverviewCarousel\"", "Export CSV"} {
		if strings.Contains(panel, gone) {
			t.Errorf("renderOverviewPanel should not include moved control %q", gone)
		}
	}
	for _, want := range []string{
		"overview-chart-download",
		"title=\"Download ${format}\"",
		"aria-label=\"${esc(csvTitle)}\"",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("renderOverviewPanel missing card download affordance %q", want)
		}
	}
	for _, want := range []string{
		"cardControls('issue', 'Issues')",
		"cardControls('pr', 'PRs')",
		"overview-chart-controls",
		"data-action=\"overviewCarouselPrev\"",
		"data-action=\"overviewCarouselNext\"",
		"data-action=\"toggleOverviewCarousel\"",
		"data-change-action=\"setOverviewChartType\"",
		"data-change-action=\"setOverviewTransition\"",
	} {
		if !strings.Contains(settings, want) {
			t.Errorf("renderOverviewSettingsPopover missing moved control %q", want)
		}
	}
}

func TestOverviewCarouselDwellCoversTransition9025(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview carousel timing rule was not executed")
	}
	html := indexHTML(t)
	durationRe := regexp.MustCompile(`const OVERVIEW_TRANSITION_DURATION_MS = \{ fast: ([0-9]+), normal: ([0-9]+), slow: ([0-9]+) \};`)
	minRe := regexp.MustCompile(`const OVERVIEW_CAROUSEL_MIN_MS = ([0-9]+);`)
	durations := durationRe.FindStringSubmatch(html)
	min := minRe.FindStringSubmatch(html)
	if len(durations) != 4 || len(min) != 2 {
		t.Fatalf("overview timing constants missing from index.html")
	}
	slow, err := strconv.Atoi(durations[3])
	if err != nil {
		t.Fatalf("parse slow duration: %v", err)
	}
	minMS, err := strconv.Atoi(min[1])
	if err != nil {
		t.Fatalf("parse carousel min: %v", err)
	}
	script := `const assert = require('node:assert/strict');
const OVERVIEW_TRANSITION_DURATION_MS = { fast: ` + durations[1] + `, normal: ` + durations[2] + `, slow: ` + durations[3] + ` };
const OVERVIEW_CAROUSEL_DEFAULT_MS = 30000;
` + jsFunc(t, html, "overviewTransitionDurationMs") + `
` + jsFunc(t, html, "overviewCarouselDwellMs") + `
assert.equal(overviewTransitionDurationMs({ duration: 'fast' }), 1000);
assert.equal(overviewTransitionDurationMs({ duration: 'normal' }), 2000);
assert.equal(overviewTransitionDurationMs({ duration: 'slow' }), 3600);
assert.ok(overviewCarouselDwellMs({ intervalMs: 500, duration: 'slow' }) >= overviewTransitionDurationMs({ duration: 'slow' }));
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node overview carousel timing failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	if minMS < slow {
		t.Fatalf("carousel minimum %dms must cover slow transition %dms", minMS, slow)
	}
}
