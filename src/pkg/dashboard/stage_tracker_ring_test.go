package dashboard

import (
	"strings"
	"testing"
)

// hivecommons/hive#16599: the CI / Review glyph is a tiny ring of steps.
// Segmented up to 16 steps, a continuous arc beyond; filled clockwise
// done → failed → rest; the next step pulses only while running; the
// a11y label names the count; no raw colour anywhere in the SVG.
func TestStageTrackerRingSvg(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "esc"),
		jsConstLine(t, html, "STAGE_TRACKER_RING_SEGMENT_GAP_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_RING_RADIUS"),
		jsConstLine(t, html, "STAGE_TRACKER_RING_GAP_DEG"),
		jsFunc(t, html, "stageTrackerRingSvg"),
	}, "\n")+`
const count = (s, re) => (s.match(re) || []).length;
const noColour = (s) => assert.ok(!/#[0-9a-f]{3,8}\b|rgb\(|stroke="(?!none)|fill="(?!none)/i.test(s), 'raw colour in ring: ' + s);
// queued: 0/5 done, nothing pulses, five muted steps
let r = stageTrackerRingSvg(0, 5, 0, 'unassigned');
noColour(r);
assert.ok(r.startsWith('<svg class="ham-ico ham-ring ham-ring-pass" '), r);
assert.strictEqual(count(r, /ham-ring-seg/g), 5);
assert.strictEqual(count(r, /ham-ring-rest/g), 5);
assert.strictEqual(count(r, /ham-ring-pulse/g), 0, 'queued does not pulse');
assert.ok(r.includes('role="img" aria-label="0 of 5 done"') && r.includes('data-ring-total="5" data-ring-done="0" data-ring-failed="0"'), r);
// working: 7/11 passed, 2 failed, running → segments 0..6 done, 7..8 failed, 9 pulses
r = stageTrackerRingSvg(7, 11, 2, 'running', 'CI: CI 7/11 · running');
noColour(r);
assert.ok(r.includes('ham-ring-live ham-ring-running'), r);
assert.strictEqual(count(r, /ham-ring-done/g), 7);
assert.strictEqual(count(r, /ham-ring-failed/g), 2);
assert.strictEqual(count(r, /ham-ring-rest/g), 2);
assert.strictEqual(count(r, /ham-ring-pulse/g), 1);
assert.ok(/ham-ring-rest ham-ring-pulse/.test(r), 'the next (10th) step pulses: ' + r);
assert.ok(r.includes('aria-label="CI: CI 7/11 · running"'), r);
// pushed / settled: 11/11 passed → all done, no pulse; 5/5 changes requested wears the fail tone
r = stageTrackerRingSvg(11, 11, 0, 'passing');
assert.strictEqual(count(r, /ham-ring-done/g), 11);
assert.strictEqual(count(r, /ham-ring-pulse|ham-ring-rest/g), 0, r);
r = stageTrackerRingSvg(5, 5, 0, 'changes_requested');
assert.ok(r.includes('ham-ring-fail') && count(r, /ham-ring-done/g) === 5, r);
// a single step is a full circle; above 16 steps the ring is continuous arcs
r = stageTrackerRingSvg(1, 1, 0, 'approved');
assert.ok(/<circle class="ham-ring-seg ham-ring-done"/.test(r), r);
r = stageTrackerRingSvg(10, 40, 3, 'running');
assert.ok(count(r, /<path/g) <= 4 && r.includes('ham-ring-track') && r.includes('ham-ring-pulse') && r.includes('data-ring-done="10" data-ring-failed="3"'), r);
// out-of-range counts clamp; no total draws the empty track
r = stageTrackerRingSvg(9, 5, 4, 'running');
assert.ok(r.includes('data-ring-done="5" data-ring-failed="0"'), r);
assert.ok(stageTrackerRingSvg(0, 0, 0, 'unknown').includes('ham-ring-track') && stageTrackerRingSvg(0, 0, 0, 'unknown').includes('aria-label="no steps"'));
`)
}
