// Meetings 🗑 Delete, "Clear finished" and Calendar 🗑 Delete: who sees
// the action, which rows offer it, and the in-page confirm.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const ASYNC_PREFIX = 'async ';
const fnSrc = name => {
  let start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  if (html.slice(start - ASYNC_PREFIX.length, start) === ASYNC_PREFIX) start -= ASYNC_PREFIX.length;
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};
const constSrc = name => {
  const m = html.match(new RegExp('const ' + name + ' = \\{[^}]*\\};'));
  assert.ok(m, name + ' not found');
  return m[0];
};
const escapeHtml = s => String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const lib = new Function('escapeHtml', 'window',
  constSrc('MEETINGS_FINAL_STATES') + '\n' + constSrc('CALENDAR_FINISHED_STATUSES') + '\n' +
  ['meetingsCanDelete', 'meetingsDeleteButtonHtml', 'meetingsDeleteText', 'meetingsFinished', 'meetingsClearFinishedText', 'calendarCanDelete', 'calendarDeleteText']
    .map(fnSrc).join('\n') +
  '\nreturn { meetingsCanDelete, meetingsDeleteButtonHtml, meetingsDeleteText, meetingsFinished, meetingsClearFinishedText, calendarCanDelete, calendarDeleteText };');
const win = { _hiveRole: 'owner' };
const m = lib(escapeHtml, win);

test('finished meetings and pending join requests offer delete; running ones do not', () => {
  for (const state of ['ended', 'failed', 'blocked', 'handed_off', 'purged']) {
    assert.strictEqual(m.meetingsCanDelete({ id: 'x', state }, 'owner'), true, state);
  }
  assert.strictEqual(m.meetingsCanDelete({ id: 'x', state: 'requested', requested_by: { name: 'andy' } }, 'owner'), true, 'stale join request');
  for (const state of ['requested', 'joining', 'lobby', 'live']) {
    assert.strictEqual(m.meetingsCanDelete({ id: 'x', state }, 'owner'), false, state);
  }
  for (const role of ['read-write', 'read', undefined]) {
    assert.strictEqual(m.meetingsCanDelete({ id: 'x', state: 'ended' }, role), false, String(role));
  }
});

test('the row action is a 🗑 Delete button wired to meetingsDelete', () => {
  win._hiveRole = 'owner';
  const btn = m.meetingsDeleteButtonHtml({ id: 'mtg-1', state: 'blocked' });
  assert.match(btn, /data-action="meetingsDelete"/);
  assert.match(btn, /data-arg0="mtg-1"/);
  assert.match(btn, /🗑 Delete/);
  assert.match(btn, /class="hv-btn btn-secondary btn-sm"/);
  assert.strictEqual(m.meetingsDeleteButtonHtml({ id: 'mtg-2', state: 'live' }), '');
  win._hiveRole = 'read-write';
  assert.strictEqual(m.meetingsDeleteButtonHtml({ id: 'mtg-1', state: 'ended' }), '');
  win._hiveRole = 'owner';
});

test('the rows render the delete action', () => {
  const row = fnSrc('meetingsRowHtml');
  assert.strictEqual((row.match(/meetingsDeleteButtonHtml\(m\)/g) || []).length, 2, 'join-request rows and meeting rows');
});

test('the confirm says knowledge facts and drafts are not touched', () => {
  assert.match(m.meetingsDeleteText({ state: 'ended' }), /Knowledge facts and drafts already made from it are not touched/);
  assert.match(m.meetingsDeleteText({ state: 'requested' }), /declines the join request/);
  assert.match(m.meetingsClearFinishedText(3), /^This deletes 3 finished meetings/);
  assert.match(m.meetingsClearFinishedText(1), /not touched/);
  assert.match(m.calendarDeleteText({ status: 'ended', history: [{ meeting_id: 'mtg-1' }] }), /and the meeting it created/);
});

test('Clear finished takes final-state meetings older than now only', () => {
  const now = new Date('2026-11-16T12:00:00Z');
  const list = [
    { id: 'a', state: 'ended', ended_at: '2026-11-16T10:00:00Z' },
    { id: 'b', state: 'failed', updated_at: '2026-11-15T10:00:00Z' },
    { id: 'c', state: 'ended', ended_at: '2026-11-16T13:00:00Z' },
    { id: 'd', state: 'live', created_at: '2026-11-16T09:00:00Z' },
    { id: 'e', state: 'requested', requested_by: { name: 'andy' }, created_at: '2026-11-10T09:00:00Z' },
  ];
  assert.deepStrictEqual(m.meetingsFinished(list, now).map(x => x.id), ['a', 'b']);
  assert.deepStrictEqual(m.meetingsFinished(null, now), []);
});

test('Calendar: past entries delete, scheduled ones keep Skip/Cancel', () => {
  for (const status of ['ended', 'cancelled', 'declined', 'failed', 'blocked', 'skipped']) {
    assert.strictEqual(m.calendarCanDelete({ status }, 'owner'), true, status);
  }
  for (const status of ['scheduled', 'awaiting_approval', 'live']) {
    assert.strictEqual(m.calendarCanDelete({ status }, 'owner'), false, status);
  }
  assert.strictEqual(m.calendarCanDelete({ status: 'ended' }, 'read-write'), false);
});

test('deletes use the in-page confirm and the DELETE endpoints', () => {
  for (const name of ['meetingsDelete', 'meetingsClearFinished', 'calendarDelete']) {
    const src = fnSrc(name);
    assert.match(src, /hiveConfirm\(/, name);
    assert.doesNotMatch(src, /window\.confirm|\balert\(|[^.]confirm\(/, name);
  }
  assert.match(fnSrc('meetingsDelete'), /'\/api\/meetings\/' \+ encodeURIComponent\(id\), \{ method: 'DELETE' \}/);
  assert.match(fnSrc('meetingsClearFinished'), /'\/api\/meetings\/clear-finished'/);
  assert.match(fnSrc('calendarDelete'), /'\/api\/meetings\/schedule\/' \+ encodeURIComponent\(id\), \{ method: 'DELETE' \}/);
});
