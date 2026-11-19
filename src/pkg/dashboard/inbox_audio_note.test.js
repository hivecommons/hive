// Inbox player note: audio this hive stores (hive recordings, dropped audio
// files) says how long it is kept; third-party audio is streamed, not stored.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const fnSrc = name => {
  const start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};
const inboxAudioNoteText = new Function(fnSrc('inboxAudioNoteText') + '\nreturn inboxAudioNoteText;')();
const fmt = d => 'DAY(' + d + ')';

test('hive-stored audio names the day retention deletes it', () => {
  for (const source of ['hive-recording', 'upload']) {
    const out = inboxAudioNoteText({ source, audio_expires_at: '2026-11-21T10:00:00Z' }, fmt);
    assert.strictEqual(out, 'The audio is kept on this hive until DAY(2026-11-21T10:00:00Z), then deleted; the transcript stays. Click Play again to restart.');
    assert.doesNotMatch(out, /not stored/);
  }
  assert.strictEqual(inboxAudioNoteText({ source: 'upload' }, fmt), 'The audio is kept on this hive. Click Play again to restart.');
  assert.strictEqual(inboxAudioNoteText({ source: 'hive-recording', audio_expires_at: '0001-01-01T00:00:00Z' }, fmt), 'The audio is kept on this hive. Click Play again to restart.');
});

test('third-party audio keeps the streaming note', () => {
  for (const it of [{ source: 'plaud' }, { source: 'zoom', audio_expires_at: '2026-11-21T10:00:00Z' }, undefined]) {
    assert.strictEqual(inboxAudioNoteText(it, fmt), 'Streaming through this hive; the recording is not stored. Click Play again to restart.');
  }
});

test('live meetings keep no audio: no player, no play button, a quiet note', () => {
  const has = new Function(fnSrc('inboxItemHasAudio') + '\nreturn inboxItemHasAudio;')();
  assert.strictEqual(has({ source: 'meeting' }), false);
  assert.strictEqual(has({ source: 'plaud' }), true);
  assert.strictEqual(has({ source: 'upload', upload_kind: 'audio' }), true);
  const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'inbox.js'), 'utf8');
  assert.match(src, /Audio is not kept for live meetings/);
  assert.match(src, /if \(src === 'meeting'\) return;/);
});
