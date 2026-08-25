package dashboard

import (
	"strings"
	"testing"
)

// #4298 asked for a graph of token usage over time with a visual marker for
// the configured budget and reset times. #4320 landed the data layer
// (GET /api/budget/history); these tests pin the dashboard actually drawing
// it — and pin the wiring, because the embedded SPA has shipped calls to
// functions that were never defined (the renderAll/hiveToast class of bug).
func TestBudgetHistoryChartWiredIntoDashboard(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	// Every function the chart path calls must be DEFINED in the document —
	// a referenced-but-undefined name throws ReferenceError at render time.
	for _, def := range []string{
		"function fetchBudgetHistory(",
		"function budgetHistoryChart(",
		"function renderBudgetBar(",
		"function fmtSparkVal(",
		"function axisSparkSvg(",
	} {
		if !strings.Contains(html, def) {
			t.Errorf("index.html does not define %q", def)
		}
	}

	// The chart must read the durable per-window series #4320 added, not a
	// client-side reconstruction that would be empty for closed windows.
	if !strings.Contains(html, "fetch('/api/budget/history')") {
		t.Error("index.html never fetches /api/budget/history — the chart would have no closed-window data")
	}

	// The chart must actually be rendered where the budget is shown, fetched
	// at startup, and refetched after a manual reset (a reset closes a window,
	// which is exactly the event the chart exists to record).
	for _, snippet := range []string{
		"${budgetHistoryChart()}",
		"() => fetchBudgetHistory().then(() => { setInterval(fetchBudgetHistory, HISTORY_REFRESH_MS); })",
		"fetchBudgetHistory(); // a reset closes a window",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}

	// The issue's two named markers: the configured budget (per-window limit
	// line) and the reset times (x-axis labels). Pin the explanatory copy so
	// the axes stay labeled — unlabeled graphs are what #4298 called
	// "impossible to understand".
	for _, snippet := range []string{
		"dashed red line = the limit then in force",
		"x-axis labels = reset times",
		"no closed periods yet",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html budget history chart is missing %q", snippet)
		}
	}
}

// The cost-section trend graphs in the issue's screenshot "lack horizontal
// and vertical axes, making them impossible to understand". Pin that the
// 30-day fact/cost charts render through the axis-labeled helper and that
// the tiny inline sparklines carry a scale tooltip (they are too small for
// drawn axes).
func TestSparklinesCarryAxesOrScaleTooltips(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	for _, snippet := range []string{
		"axisSparkSvg(_factHistory",
		"axisSparkSvg(_costHistory",
		// sparkSvg + miniSparkSvg embed an SVG <title> stating the range.
		"<title>range ${fmtSparkVal(min)}",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
}
