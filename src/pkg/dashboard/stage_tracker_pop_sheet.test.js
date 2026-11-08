// Activity Monitor Merged / merge-queue lists (hivecommons/hive#17165): on
// phones they open as a .hive-dialog.modal-sheet wired through hiveWireDialog
// (Esc, ✕, backdrop, focus back to the chip); desktop keeps the anchored
// popovers. Asserts against the shipped index.html.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}

test('sheet vs popover uses the modal-sheet media tier', () => {
  assert.ok(html.includes("const STAGE_TRACKER_POP_SHEET_QUERY = '(max-width: 640px), ((pointer: coarse) and (max-width: 820px))';"));
  assert.ok(html.includes('window.matchMedia(STAGE_TRACKER_POP_SHEET_QUERY).matches'));
});

test('both toggles open a hiveWireDialog sheet on phones and leave the popover hidden', () => {
  const sheet = pick(/function stageTrackerPopSheetOpen\([\s\S]*?\n    \}\n/, 'stageTrackerPopSheetOpen');
  assert.ok(sheet.includes('hiveWireDialog(overlay'), 'wired through hiveWireDialog');
  assert.ok(sheet.includes('modal-scroll ham-pop-sheet-body'), 'scroll body so the ✕ is decorated');
  assert.ok(sheet.includes("chip.setAttribute('aria-expanded', 'false')") && sheet.includes('chip.focus()'), 'focus returns to the chip');
  const merged = pick(/function hamMergedToggle\(open\) \{[\s\S]*?\n    \}\n/, 'hamMergedToggle');
  const queue = pick(/function hamQueueToggle\(open\) \{[\s\S]*?\n    \}\n/, 'hamQueueToggle');
  assert.ok(merged.includes("stageTrackerPopSheetOpen('merged'") && merged.includes('pop.hidden = true;'));
  assert.ok(queue.includes("stageTrackerPopSheetOpen('queue'") && queue.includes('pop.hidden = true;'));
});

test('desktop popover path is unchanged', () => {
  assert.ok(html.includes("const want = typeof open === 'boolean' ? open : !!pop.hidden;\n      pop.hidden = !want;"));
  assert.ok(html.includes('<div class="ham-merged-pop" id="activity-monitor-merged-pop"'));
});

test('sheet CSS is phone-only with tap-target rows and a 2-line title clamp', () => {
  const m = html.match(/@media \(max-width: 640px\), \(\(pointer: coarse\) and \(max-width: 820px\)\) \{\n\s*\.ham-pop-sheet[\s\S]*?\n    \}\n/);
  assert.ok(m, 'phone-scoped .ham-pop-sheet block');
  assert.ok(m[0].includes('min-height: var(--tap-target)'));
  assert.ok(m[0].includes('-webkit-line-clamp: 2'));
  assert.ok(m[0].includes('--safe-area-bottom'));
  assert.ok(html.includes('.ham-merged-list .ham-merged-repo { display: none; }'));
});
