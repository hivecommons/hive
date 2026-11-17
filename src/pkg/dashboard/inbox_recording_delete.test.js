// Inbox 🗑 Delete on hive recordings: who sees it, and the in-page confirm
// text (shared copies stay).
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
const lib = new Function(fnSrc('inboxCanDeleteRecording') + '\n' + fnSrc('inboxDeleteRecordingText') + '\nreturn { inboxCanDeleteRecording, inboxDeleteRecordingText };')();

test('owners delete any hive recording, read-write only their own, read never', () => {
  const it = { source: 'hive-recording', id: 'r1', recorded_by: 'ana@example.com' };
  const can = lib.inboxCanDeleteRecording;
  assert.strictEqual(can(it, 'owner', 'bob@example.com'), true);
  assert.strictEqual(can(it, 'read-write', 'ana@example.com'), true);
  assert.strictEqual(can(it, 'read-write', 'ANA@example.com'), true, 'logins compare case-insensitively');
  assert.strictEqual(can(it, 'read-write', 'bob@example.com'), false);
  assert.strictEqual(can(it, 'read-write', ''), false);
  assert.strictEqual(can(it, 'read', 'ana@example.com'), false);
  assert.strictEqual(can(Object.assign({}, it, { source: 'plaud' }), 'owner', 'bob@example.com'), false, 'only hive recordings');
});

test('the confirm says shared copies stay only when there is share history', () => {
  const plain = lib.inboxDeleteRecordingText({ source: 'hive-recording' });
  assert.strictEqual(plain, 'This deletes the audio stored on this hive, this Inbox item and its transcript in Knowledge. It cannot be undone.');
  const shared = lib.inboxDeleteRecordingText({ source: 'hive-recording', share_history: [{ destination: 'dibs' }, { destination: 'spek_rfc' }] });
  assert.match(shared, /What was already shared from it \(2 shares: RFCs, issues, DIBS ideas, spek seeds\) stays where it is\.$/);
});

test('delete uses the in-page confirm and the DELETE endpoint', () => {
  const src = fnSrc('inboxDeleteRecording');
  assert.match(src, /hiveConfirm\(/);
  assert.doesNotMatch(src, /window\.confirm|\balert\(|[^.]confirm\(/);
  assert.match(src, /'\/api\/inbox\/hive-recording\/' \+ encodeURIComponent\(id\), \{ method: 'DELETE' \}/);
});
