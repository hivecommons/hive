const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const m = html.match(/\/\/ your-turn critical path helpers begin\n([\s\S]*?)\/\/ your-turn critical path helpers end/);
assert.ok(m, 'your-turn critical path helpers not found');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const h = new Function('esc', m[1] + '; return { cpCollapse, cpCollapsedSummary, cpDependentsHTML, cpChipsHTML, cpActionText, cpByUnblocks };')(esc);

const item = (repo, kind, number, cp) => ({ repo, kind, row: Object.assign({ number, waits_on: [{ kind: 'merge' }], unblocks: 0 }, cp || {}) });

test('dependents fold under their blocker as one expandable row', () => {
  const blocker = item('acme/dakota-iso', 'pr', 255, { unblocks: 8 });
  const deps = [];
  for (let n = 260; n < 268; n++) deps.push(item('acme/dakota-iso', 'pr', n, { blocker: '#255', waits_on: [{ kind: 'base-check', ref: 'Python Unit Tests' }, { kind: 'pr', ref: '#255' }] }));
  const other = item('acme/dakota-iso', 'issue', 9);
  const entries = h.cpCollapse([blocker, ...deps, other]);
  assert.strictEqual(entries.length, 2);
  assert.strictEqual(entries[0].item, blocker);
  assert.strictEqual(entries[0].dependents.length, 8);
  assert.strictEqual(entries[1].dependents.length, 0);
  assert.strictEqual(h.cpCollapsedSummary(entries[0].item, entries[0].dependents), '8 PRs waiting on #255 (dakota-iso)');
  const rendered = h.cpDependentsHTML(entries[0], d => '<i>#' + d.row.number + '</i>');
  assert.match(rendered, /^<details class="cp-dependents"><summary>8 PRs waiting on #255 \(dakota-iso\)<\/summary><i>#260<\/i>/);
  assert.strictEqual(h.cpDependentsHTML(entries[1], () => 'x'), '');
});

test('a dependent whose blocker is not shown stays a row of its own', () => {
  const entries = h.cpCollapse([item('o/r', 'pr', 2, { blocker: '#1' }), item('o/r', 'issue', 3, { blocker: 'o/other#1' })]);
  assert.deepStrictEqual(entries.map(e => e.item.row.number), [2, 3]);
});

test('cross-repository blockers match by owner/repo', () => {
  const lib = item('o/lib', 'pr', 7, { unblocks: 1 });
  const entries = h.cpCollapse([item('o/app', 'issue', 4, { blocker: 'o/lib#7' }), lib]);
  assert.strictEqual(entries.length, 1);
  assert.strictEqual(entries[0].item, lib);
  assert.strictEqual(h.cpCollapsedSummary(lib, entries[0].dependents), '1 issue waiting on #7 (lib)');
});

test('chips: unblocks, waits on, and a soft may-conflict', () => {
  const chips = h.cpChipsHTML({ number: 5, unblocks: 3, waits_on: [{ kind: 'approval' }], may_conflict_with: [6] });
  assert.match(chips, /class="cp-chip cp-unblocks"[^>]*>unblocks 3</);
  assert.match(chips, /class="cp-chip cp-waits"[^>]*>waits on approval</);
  assert.match(chips, /class="cp-chip soft"[^>]*>may conflict with #6</);
  assert.strictEqual(h.cpChipsHTML({ number: 5 }), '', 'rows that are not your turn carry no chips');
  assert.strictEqual(h.cpChipsHTML({ number: 5, unblocks: 0, waits_on: [{ kind: 'merge' }], may_conflict_with: [] }), '');
});

test('action names and ordering', () => {
  assert.strictEqual(h.cpActionText({ action: 'rebase', action_ref: '#11' }), 'rebase after #11');
  assert.strictEqual(h.cpActionText({ action: 'release-hold' }), 'release hold');
  assert.strictEqual(h.cpActionText({ action: 'merge', action_ref: '#10' }), 'merge #10');
  assert.strictEqual(h.cpActionText({}), '');
  const rows = [{ n: 1, unblocks: 0 }, { n: 2, unblocks: 4 }, { n: 3 }, { n: 4, unblocks: 1 }];
  assert.deepStrictEqual(rows.sort(h.cpByUnblocks).map(r => r.n), [2, 4, 1, 3]);
});
