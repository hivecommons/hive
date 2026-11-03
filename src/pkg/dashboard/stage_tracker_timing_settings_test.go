package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerTimingSettingsNode runs the Activity Monitor settings
// modal's "Toasts & timing" section under node (hivecommons/hive#15813):
// the fields show the hive's values from the tracker payload, a change
// PUTs the knob to /api/config/dashboard (soak to /api/config/auto-merge)
// and the new duration applies to the next toast, a refused write falls
// back to this browser's localStorage and says so, and the merge banner
// follows the info duration.
func TestStageTrackerTimingSettingsNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — timing settings were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		"function esc(s) { if (typeof s !== 'string') return ''; return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		"const store = {}; global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem(k, v) { store[k] = String(v); }, removeItem(k) { delete store[k]; } };",
		"global.window = {}; const toasts = []; function showToast(msg, type) { toasts.push({ msg, type }); }",
		"let renders = 0; function stageTrackerRenderSettings() { renders++; }",
		"const puts = []; let nextOk = true; global.fetch = async (url, init) => { puts.push({ url, body: JSON.parse(init.body), method: init.method }); return { ok: nextOk, json: async () => ({ error: 'owner access required' }) }; };",
		"var _stageTracker = null;",
		jsConstLine(t, html, "STAGE_TRACKER_TOAST_DEFAULT_S"),
		jsConstLine(t, html, "STAGE_TRACKER_TOAST_MIN_S"),
		jsConstLine(t, html, "STAGE_TRACKER_TOAST_MAX_S"),
		jsConstLine(t, html, "STAGE_TRACKER_TOAST_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_BANNER_FACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_SOAK_DEFAULT_MIN"),
		jsFunc(t, html, "stageTrackerToastSecondsValid"),
		jsFunc(t, html, "stageTrackerToastSeconds"),
		jsFunc(t, html, "stageTrackerToastLocal"),
		jsFunc(t, html, "stageTrackerToastMs"),
		jsFunc(t, html, "stageTrackerMergeBannerMs"),
		jsFunc(t, html, "stageTrackerTimingSectionHtml"),
		jsFunc(t, html, "stageTrackerTimingState"),
		jsFunc(t, html, "stageTrackerTimingHeaders"),
		jsFunc(t, html, "stageTrackerSaveToastSeconds"),
		jsFunc(t, html, "stageTrackerToastInfoChange"),
		jsFunc(t, html, "stageTrackerToastErrorChange"),
		jsFunc(t, html, "stageTrackerSoakChange"),
		`
(async () => {
// Defaults with no payload: 15 s / 30 s, banner 30 s, soak 30 min.
assert.strictEqual(stageTrackerToastMs('info'), 15000); assert.strictEqual(stageTrackerToastMs('error'), 30000);
assert.strictEqual(stageTrackerMergeBannerMs(), 30000, 'the banner is twice the info toast');
let h = stageTrackerTimingSectionHtml(stageTrackerTimingState());
assert.ok(h.includes('id="sts-timing"') && h.includes('Toasts &amp; timing'), h);
assert.ok(h.includes('id="sts-toast-info" min="3" max="120" step="1" value="15"'), h);
assert.ok(h.includes('id="sts-toast-error" min="3" max="120" step="1" value="30"'), h);
assert.ok(h.includes('id="sts-soak" min="1"') && h.includes('value="30"'), h);
assert.ok(h.includes('data-change-action="stageTrackerToastInfoChange"') && h.includes('data-change-action="stageTrackerSoakChange"'), 'fields dispatch change actions');
assert.ok(h.includes('re-reads it on every pass'), 'the soak hint says how a running soak behaves');

// The payload's settings block drives the fields and every toast.
_stageTracker = { items: [], settings: { toast_info_s: 8, toast_error_s: 40, soak_minutes: 12 } };
assert.strictEqual(stageTrackerToastMs('info'), 8000); assert.strictEqual(stageTrackerToastMs('error'), 40000); assert.strictEqual(stageTrackerMergeBannerMs(), 16000);
h = stageTrackerTimingSectionHtml(stageTrackerTimingState());
assert.ok(h.includes('id="sts-toast-info" min="3" max="120" step="1" value="8"') && h.includes('value="40"') && h.includes('id="sts-soak" min="1" max="100000" step="1" value="12"'), h);

// A change writes the hive's knob and applies at once.
const info = { value: '25' };
assert.strictEqual(await stageTrackerToastInfoChange(info), true);
assert.deepStrictEqual(puts.pop(), { url: '/api/config/dashboard', method: 'PUT', body: { stage_tracker: { toast_info_s: 25 } } });
assert.strictEqual(stageTrackerToastMs('info'), 25000, 'applies to the next toast without waiting for a poll');
assert.strictEqual(stageTrackerMergeBannerMs(), 50000);
assert.strictEqual(renders, 1); assert.strictEqual(toasts.pop().type, 'success');
assert.strictEqual(store[STAGE_TRACKER_TOAST_KEY], undefined, 'a saved value leaves nothing in localStorage');
const bad = { value: 'abc' };
assert.strictEqual(await stageTrackerToastErrorChange(bad), false, 'not a number: not sent');
assert.strictEqual(bad.value, '40', 'the field snaps back to the live value'); assert.strictEqual(puts.length, 0);
const low = { value: '1' };
await stageTrackerToastErrorChange(low);
assert.strictEqual(low.value, '3', 'clamped to the minimum before the write'); assert.deepStrictEqual(puts.pop().body, { stage_tracker: { toast_error_s: 3 } });
const high = { value: '500' };
await stageTrackerToastErrorChange(high);
assert.strictEqual(high.value, '120', 'clamped to the maximum before the write'); assert.deepStrictEqual(puts.pop().body, { stage_tracker: { toast_error_s: 120 } });

// A refused write (viewer, hub proxy) keeps the value in this browser only.
nextOk = false;
assert.strictEqual(await stageTrackerToastInfoChange({ value: '5' }), false);
assert.deepStrictEqual(JSON.parse(store[STAGE_TRACKER_TOAST_KEY]), { info: 5, fallback: true });
assert.strictEqual(stageTrackerToastMs('info'), 5000, 'the fallback applies here');
assert.strictEqual(stageTrackerToastMs('error'), 120000, 'the other knob still reads the hive');
assert.strictEqual(toasts.pop().type, 'warning');
assert.ok(stageTrackerTimingSectionHtml(stageTrackerTimingState()).includes('Saved in this browser only'), 'the modal says the write was refused');
window.hiveKioskMode = () => true; delete store[STAGE_TRACKER_TOAST_KEY];
await stageTrackerToastInfoChange({ value: '6' });
assert.strictEqual(store[STAGE_TRACKER_TOAST_KEY], undefined, 'a kiosk never writes localStorage'); window.hiveKioskMode = () => false;
nextOk = true;
await stageTrackerToastInfoChange({ value: '9' });
assert.strictEqual(store[STAGE_TRACKER_TOAST_KEY], undefined, 'a later successful write clears the fallback'); assert.strictEqual(stageTrackerToastMs('info'), 9000);

// The soak writes through the auto-merge config endpoint; < 1 is refused.
puts.length = 0;
assert.strictEqual(await stageTrackerSoakChange({ value: '45' }), true);
assert.deepStrictEqual(puts.pop(), { url: '/api/config/auto-merge', method: 'PUT', body: { contributor_lane: { soak_minutes: 45 } } });
assert.strictEqual(_stageTracker.settings.soak_minutes, 45); assert.ok(toasts.pop().msg.includes('45 min'));
const zero = { value: '0' };
assert.strictEqual(await stageTrackerSoakChange(zero), false); assert.strictEqual(zero.value, '45'); assert.strictEqual(puts.length, 0);
nextOk = false;
assert.strictEqual(await stageTrackerSoakChange({ value: '50' }), false);
assert.strictEqual(_stageTracker.settings.soak_minutes, 45, 'a refused soak write changes nothing'); assert.ok(toasts.pop().msg.includes('owner access required'));
console.log('ok');
})().catch(e => { console.error(e); process.exit(1); });
`}, "\n")
	out, err := runNodeStdin(node, script)
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node timing settings fixture failed: %v\n%s", err, out)
	}
}
