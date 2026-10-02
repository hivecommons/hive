package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDashboardSparklineSharedRendererAndAnimationHooks(t *testing.T) {
	html := indexHTML(t)
	if got := strings.Count(html, "function renderSparkline(el, series, opts)"); got != 1 {
		t.Fatalf("shared renderSparkline helper count = %d, want 1", got)
	}
	for _, fn := range []string{
		"function sparkSvg(rawValues, color, times, id)",
		"function axisSparkSvg(entries, getV, getT, fmtV, color, title)",
		"function miniSparkSvg(values, color, label)",
		"function sysGaugeMiniSpark(values, color, name)",
		"function prtSeriesSpark(dto, field, color)",
	} {
		body := jsFunctionBody(t, html, fn)
		if !strings.Contains(body, "renderSparkline(") {
			t.Fatalf("%s is not routed through renderSparkline", fn)
		}
	}
	for _, want := range []string{
		"data-sparkline-key",
		"role=\"img\"",
		"aria-label=",
		"SPARKLINE_ANIMATION_MS = 620",
		"SPARKLINE_STAGGER_MS = 25",
		"SPARKLINE_MAX_STAGGER_MS",
		"sparklineReducedMotion()",
		"prefers-reduced-motion: reduce",
		"strokeDasharray",
		"strokeDashoffset",
		"sparkline-dot",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("sparkline animation contract missing %q", want)
		}
	}
}

func TestDashboardSparklineReplayWiredToSectionExpansion(t *testing.T) {
	html := indexHTML(t)
	toggle := jsFunctionBody(t, html, "function toggleSection(sectionId, maybeSectionId)")
	if !strings.Contains(toggle, "replaySparklinesIn(section, { force: true })") {
		t.Fatal("section expand toggle does not force replay sparklines in the expanded card")
	}
	apply := jsFunctionBody(t, html, "function applySectionCollapse(sectionId)")
	if !strings.Contains(apply, "replaySparklinesIn(section)") {
		t.Fatal("section refresh path does not replay changed sparklines")
	}
	replay := jsFunctionBody(t, html, "function replaySparklinesIn(root, opts)")
	for _, want := range []string{"sparklineReducedMotion()", "sparklineSvgVisible(svg)", "_sparklineLastKeys.get(id) === key", "Math.min(i * SPARKLINE_STAGGER_MS, SPARKLINE_MAX_STAGGER_MS)"} {
		if !strings.Contains(replay, want) {
			t.Fatalf("sparkline replay missing %q", want)
		}
	}
}

func TestSparklineSeriesKeyNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: sparkline series key behavior was not executed")
	}
	body := jsFunctionBody(t, indexHTML(t), "function sparklineSeriesKey(series)")
	script := "const assert = require('node:assert/strict');\n" + body + `
assert.equal(sparklineSeriesKey([1, 2, 3]), '1|2|3');
assert.equal(sparklineSeriesKey([1, 2.5, 3.1250]), '1|2.5|3.125');
assert.equal(sparklineSeriesKey([1, NaN, Infinity, '4']), '1|x|x|4');
assert.equal(sparklineSeriesKey('nope'), '');
assert.equal(sparklineSeriesKey([1, 2, 3]), sparklineSeriesKey([1, 2, 3]));
assert.notEqual(sparklineSeriesKey([1, 2, 3]), sparklineSeriesKey([1, 2, 4]));
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node sparklineSeriesKey test failed: %v\n%s", err, out)
	}
}
