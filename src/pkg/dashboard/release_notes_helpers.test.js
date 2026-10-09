const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const m = html.match(/\/\/ release-notes helpers begin\n([\s\S]*?)\/\/ release-notes helpers end/);
assert.ok(m, 'release-notes helpers not found');
const h = new Function(m[1] + '; return { releaseNotesGroupSection, releaseNotesRefParts, releaseNotesBannerDecision };')();

test('groups categories in canonical order and drops empty ones', () => {
  const g = h.releaseNotesGroupSection({ categories: { Fixed: ['a'], Added: ['b'], Changed: [], Misc: ['c'] } });
  assert.deepStrictEqual(g.map(x => x.name), ['Added', 'Fixed', 'Misc']);
});

test('splits #NNNN references out of plain text', () => {
  const parts = h.releaseNotesRefParts('fix (#123) and #45');
  assert.deepStrictEqual(parts.filter(p => p.ref).map(p => p.ref), ['123', '45']);
  assert.strictEqual(parts.map(p => p.text).join(''), 'fix (#123) and #45');
});

test('banner never shows on first visit and shows once per SHA transition', () => {
  let d = h.releaseNotesBannerDecision({}, 'aaaaaaa1111');
  assert.strictEqual(d.show, false);
  d = h.releaseNotesBannerDecision(d.state, 'aaaaaaa');
  assert.strictEqual(d.show, false);
  d = h.releaseNotesBannerDecision(d.state, 'bbbbbbb2222');
  assert.strictEqual(d.show, true);
  assert.strictEqual(d.from, 'aaaaaaa1111');
  d = h.releaseNotesBannerDecision(d.state, 'bbbbbbb2222');
  assert.strictEqual(d.show, false);
});
