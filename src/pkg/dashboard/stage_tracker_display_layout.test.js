// Display-mode wall layout (hivecommons/hive#15931): `density=tv` caps the
// rows to what fits the viewport with a "+N more" footer instead of the
// pager, `shift=1` is a burn-in shift, and reduced motion turns row movement
// off on a display. The helpers are lifted by marker out of index.html so
// this runs against the shipped source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const m = html.match(/\/\/ tracker display layout helpers begin\n([\s\S]*?)\/\/ tracker display layout helpers end/);
assert.ok(m, 'tracker display layout helpers not found');
const h = new Function(m[1] + '; return { stageTrackerDisplayRowCap, stageTrackerDisplayCapPage, stageTrackerDisplayMoreHtml, stageTrackerDisplayMotionEffect };')();

test('row cap: the rows that fit below the header', () => {
  assert.strictEqual(h.stageTrackerDisplayRowCap(1080, 200, 80), 11);
  assert.strictEqual(h.stageTrackerDisplayRowCap(2160, 200, 80), 24);
  assert.strictEqual(h.stageTrackerDisplayRowCap(1080, 0, 100), 10);
  assert.strictEqual(h.stageTrackerDisplayRowCap(1079, 0, 100), 10, 'a partial row does not count');
});

test('row cap: never below one row, and one when sizes are unknown', () => {
  assert.strictEqual(h.stageTrackerDisplayRowCap(300, 400, 80), 1, 'header taller than the viewport');
  assert.strictEqual(h.stageTrackerDisplayRowCap(1080, 200, 0), 1);
  assert.strictEqual(h.stageTrackerDisplayRowCap(0, 0, 80), 1);
  assert.strictEqual(h.stageTrackerDisplayRowCap(NaN, 0, 80), 1);
  assert.strictEqual(h.stageTrackerDisplayRowCap(1080, -50, 100), 10, 'a negative header is ignored');
});

test('capped page: first rows, no pager, a correct "+N more" count', () => {
  const rows = Array.from({ length: 23 }, (_, i) => i);
  const p = h.stageTrackerDisplayCapPage(rows, 10);
  assert.deepStrictEqual(p.rows, rows.slice(0, 10));
  assert.strictEqual(p.pages, 1, 'no pager');
  assert.strictEqual(p.total, 23);
  assert.strictEqual(p.more, 13);
  assert.strictEqual(p.from, 1);
  assert.strictEqual(p.to, 10);
  const all = h.stageTrackerDisplayCapPage(rows.slice(0, 4), 10);
  assert.strictEqual(all.more, 0);
  assert.strictEqual(all.rows.length, 4);
  const none = h.stageTrackerDisplayCapPage([], 10);
  assert.strictEqual(none.from, 0);
  assert.strictEqual(none.more, 0);
});

test('"+N more" footer only when rows were left out', () => {
  assert.strictEqual(h.stageTrackerDisplayMoreHtml(13), '<span class="ham-display-more" role="status">+13 more</span>');
  assert.strictEqual(h.stageTrackerDisplayMoreHtml(0), '');
  assert.strictEqual(h.stageTrackerDisplayMoreHtml(-2), '');
});

test('reduced motion turns row movement off on a display only', () => {
  assert.strictEqual(h.stageTrackerDisplayMotionEffect('rm-fade', true, true), '');
  assert.strictEqual(h.stageTrackerDisplayMotionEffect('wash', true, false), 'wash');
  assert.strictEqual(h.stageTrackerDisplayMotionEffect('rm-fade', false, true), 'rm-fade', 'the ordinary monitor keeps its crossfade');
  assert.strictEqual(h.stageTrackerDisplayMotionEffect('wash', false, false), 'wash');
});

test('wiring: density and shift are honoured only with display=1; TV replaces the pager; shift is inert under reduced motion', () => {
  assert.ok(html.includes("density=u.searchParams.get('density')==='tv'?'tv':'';"), 'density accepts only tv');
  assert.ok(html.includes("window.HIVE_DISPLAY_DENSITY=window.HIVE_DISPLAY?density:'';"));
  assert.ok(html.includes('window.HIVE_DISPLAY_SHIFT=window.HIVE_DISPLAY&&shift;'));
  assert.ok(html.includes("if(window.HIVE_DISPLAY_DENSITY==='tv') document.body.classList.add('display-density-tv');"));
  assert.ok(html.includes("if(window.HIVE_DISPLAY_SHIFT) document.body.classList.add('display-shift');"));
  assert.ok(html.includes("const grouping = _stageTrackerGroup === 'repo' && !tv;"));
  assert.ok(html.includes('? stageTrackerDisplayCapPage(ordered, stageTrackerDisplayTvCap())'));
  assert.ok(html.includes('if (foot) setIfChanged(foot, tv ? stageTrackerDisplayMoreHtml(page.more) : '));
  assert.ok(html.includes("if (typeof stageTrackerDisplayEffect === 'function') effect = stageTrackerDisplayEffect(effect);"));
  assert.ok(html.includes('body.display-shift #activity-monitor-section { animation: ham-display-shift 16min steps(1, end) infinite; }'));
  assert.match(html, /@media \(prefers-reduced-motion: reduce\) \{\n\s*body\.display-shift #activity-monitor-section \{ animation: none; \}/);
  assert.ok(html.includes('body.display-density-tv #activity-monitor-section .ham-head-pr, body.display-density-tv #activity-monitor-section .ham-badges,'), 'TV drops the PR column and chips');
});
