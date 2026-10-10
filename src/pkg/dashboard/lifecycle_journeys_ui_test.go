package dashboard

import (
	"os/exec"
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
//  2. Each row shows a non-zero SVG bar along the lifecycle time axis, with
//     the current state colored independently from collapsed row chrome.
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
		"lcBarHtml",
		"lcAxisHtml",
		"lcParseTime",
		"ResizeObserver",
		"lifecycleToggleRows",
		"LC_COLLAPSED_ROW_LIMIT",
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
		"No pull requests in the window yet.",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("lifecycle journeys panel missing snippet %q", snippet)
		}
	}
}

func TestLifecycleTimelineRendersNonZeroBars(t *testing.T) {
	html := indexHTML(t)
	if _, err := exec.LookPath("node"); err != nil {
		for _, snippet := range []string{
			"function lcParseTime(value)",
			"Date.parse(trimmed)",
			"if (n < 100000000000) return Math.round(n * 1000);",
			"if (n > 100000000000000) return Math.round(n / 1000000);",
			"Math.max(LC_BAR_MIN_WIDTH_PCT, right - left)",
			`<rect class="lc-bar ${cls}"`,
			`width="${width.toFixed(2)}"`,
			"lcTimelineBounds(visibleJourneys)",
		} {
			if !strings.Contains(html, snippet) {
				t.Fatalf("lifecycle timeline non-zero bar/timestamp guard missing %q", snippet)
			}
		}
		return
	}
	start := strings.Index(html, "// Format a UnixMilli timestamp")
	end := strings.Index(html, "    // Change throughput card")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("could not locate lifecycle script block")
	}
	block := html[start:end]
	script := `
	const elements = {};
	function mkEl(id) {
	  return elements[id] || (elements[id] = {
	    id, style: {}, innerHTML: '', scrollTop: 0, clientWidth: 900,
	    classList: { toggle() {} }, setAttribute() {}, getAttribute() { return null; }, removeAttribute() {}
	  });
	}
	global.document = {
	  getElementById: mkEl,
	  querySelector(sel) {
	    if (sel.includes('.lc-journeys')) return mkEl('journeys');
	    return mkEl(sel);
	  }
	};
	global.ResizeObserver = class { observe() {} };
	global.requestAnimationFrame = (fn) => { fn(); return 1; };
	function escapeHtml(v) { return String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
	function selectedDashboardTimeZone() { return 'UTC'; }
	function dashboardDateFormatter(options) { const opts = Object.assign({}, options || {}); opts.timeZone = selectedDashboardTimeZone(); return new Intl.DateTimeFormat([], opts); }
	function formatDashboardDateTime(value, options, fallback) { const d = value instanceof Date ? value : new Date(value); if (isNaN(d.getTime())) return fallback !== undefined ? fallback : String(value || ''); try { return dashboardDateFormatter(options || { month:'numeric', day:'numeric', hour:'numeric', minute:'2-digit', hour12:true, timeZoneName:'short' }).format(d); } catch (e) { return d.toISOString(); } }
	function formatDashboardTime(value, options, fallback) { return formatDashboardDateTime(value, options || { hour:'numeric', minute:'2-digit', hour12:true, timeZoneName:'short' }, fallback); }
	function refreshSectionCardShell() {}
	function setSectionSummary() {}
	function applySectionCollapse() {}
	` + block + `
	if (lcParseTime('2026-10-02T12:00:00Z') !== Date.parse('2026-10-02T12:00:00Z')) throw new Error('RFC3339 timestamp did not parse');
	if (lcParseTime(1700000000) !== 1700000000000) throw new Error('Unix seconds timestamp did not normalize');
	if (lcParseTime(1700000000000) !== 1700000000000) throw new Error('Unix milliseconds timestamp did not remain milliseconds');
	renderLifecycle({
	  fleet: { inFlight: 1, merged: 1, blocked: 1, windowMs: 21600000, coveredMs: 21600000 },
	  journeys: [
	    { ref: 'hivecommons/hive#1', current: 'kicked', firstAt: 1700000000000, lastAt: 1700000600000, agent: 'bob', stages: { kicked: { firstAt: 1700000000000, lastAt: 1700000600000, count: 1, attrs: { pr_number: '101', pr_title: 'Fix timeline' } } } },
	    { ref: 'hivecommons/hive#2', current: 'merged', firstAt: '2026-10-02T12:00:00Z', lastAt: '2026-10-02T13:00:00Z', stages: { kicked: { lastAt: '2026-10-02T12:00:00Z', count: 1 }, merged: { lastAt: '2026-10-02T13:00:00Z', count: 1, attrs: { title: 'Merged work' } } } },
	    { ref: 'hivecommons/hive#3', current: 'blocked', firstAt: 1700001200000, lastAt: 1700001800000, stages: { kicked: { lastAt: 1700001200000, count: 1 }, blocked: { lastAt: 1700001800000, count: 1, attrs: { issue_title: 'Needs input' } } } }
	  ]
	});
	const rendered = elements['lifecycle-card'].innerHTML;
	const bars = [...rendered.matchAll(/class="lc-bar [^"]+" x="[^"]+" y="6" width="([0-9.]+)"/g)].map(m => Number(m[1]));
	if (bars.length !== 3) throw new Error('got ' + bars.length + ' bars: ' + rendered);
	if (bars.some(w => !(w > 0))) throw new Error('zero-width bar: ' + bars.join(','));
	if (!rendered.includes('PR #101') || !rendered.includes('Fix timeline')) throw new Error('missing PR label/title: ' + rendered);
	if (!rendered.includes('lc-axis')) throw new Error('missing time axis: ' + rendered);
	`
	cmd := exec.Command("node", "--input-type=commonjs", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node lifecycle render fixture failed: %v\n%s", err, out)
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

// TestLifecycleTimelineAxisAndRowsKeepNaturalSize (#10956): axis labels must
// not live in the non-uniformly scaled SVG, and rows must not flex-shrink.
func TestLifecycleTimelineAxisAndRowsKeepNaturalSize(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`<span class="lc-axis-tick-label" style="left:${x}%">`,
		"flex-shrink: 0;",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("lifecycle timeline missing %q", snippet)
		}
	}
	if strings.Contains(html, `<text class="lc-axis-tick-label"`) {
		t.Fatalf("axis tick labels must not be SVG <text> inside the preserveAspectRatio=none SVG")
	}
	if !strings.Contains(html, ".lc-journey { font-size: 0.78rem;") || !strings.Contains(html, "overflow: hidden; flex-shrink: 0; }") {
		t.Fatalf(".lc-journey must set flex-shrink: 0 so rows do not collapse to stripes")
	}
}
