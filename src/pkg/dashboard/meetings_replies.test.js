// The attendee's own lines are shown wherever a meeting's transcript is:
// in the live meeting view (a `reply` event among the participants' lines)
// and in the Inbox item's Transcript view (lines labelled "(assistant,
// ...)"), set apart, marked spoken or chat, with the ones that were never
// delivered. The functions are lifted straight out of index.html so this
// runs against the shipped source.
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

const src = [
  'function escapeHtml(s) { return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }',
  'function meetingsFormatTime(iso) { return iso ? "T" : ""; }',
  pick(/const MEETINGS_REPLY_SPOKEN_MARK = [^\n]*\n/, 'MEETINGS_REPLY_SPOKEN_MARK'),
  pick(/const MEETINGS_REPLY_CHAT_MARK = [^\n]*\n/, 'MEETINGS_REPLY_CHAT_MARK'),
  pick(/const MEETINGS_REPLY_UNDELIVERED_MARK = [^\n]*\n/, 'MEETINGS_REPLY_UNDELIVERED_MARK'),
  pick(/const MEETINGS_ASSISTANT_LINE = [^\n]*\n/, 'MEETINGS_ASSISTANT_LINE'),
  pick(/function meetingsReplyMark\(how\) \{[\s\S]*?\n    \}\n/, 'meetingsReplyMark'),
  pick(/function meetingsReplyHow\(r\) \{[\s\S]*?\n    \}\n/, 'meetingsReplyHow'),
  pick(/function meetingsReplyLineHtml\(ev\) \{[\s\S]*?\n    \}\n/, 'meetingsReplyLineHtml'),
  pick(/function meetingsReachHtml\(live\) \{[\s\S]*?\n    \}\n/, 'meetingsReachHtml'),
  pick(/function meetingsTranscriptTextHtml\(text\) \{[\s\S]*?\n    \}\n/, 'meetingsTranscriptTextHtml'),
  pick(/function meetingsLineHtml\(ev, partial\) \{[\s\S]*?\n    \}\n/, 'meetingsLineHtml'),
  'module.exports = { meetingsReplyLineHtml, meetingsReachHtml, meetingsTranscriptTextHtml, meetingsLineHtml };',
].join('\n');
const mod = { exports: {} };
new Function('module', src)(mod);
const { meetingsReplyLineHtml, meetingsReachHtml, meetingsTranscriptTextHtml, meetingsLineHtml } = mod.exports;

const spoken = {
  type: 'reply', author: { name: 'Betty' }, text: 'It ships <Friday>.',
  reply: { kind: 'voice', trigger: 'addressed', source: 'hive-status', delivered: true },
};

test('a reply line in the meeting view is set apart, with how it went out and its source', () => {
  const out = meetingsLineHtml(spoken, false);
  assert.strictEqual(out, meetingsReplyLineHtml(spoken));
  assert.match(out, /class="meetings-line is-reply"/);
  assert.match(out, /data-reply-kind="spoken"/);
  assert.match(out, /Betty/);
  assert.match(out, /spoken · source: hive-status · addressed/);
  assert.match(out, /It ships &lt;Friday&gt;\./);
});

test('a participant line is unchanged', () => {
  const out = meetingsLineHtml({ type: 'chat', author: { name: 'Bo' }, text: 'hi' }, false);
  assert.doesNotMatch(out, /is-reply/);
  assert.match(out, /Bo \(chat\)/);
});

test('a chat reply delivered in the other channel says so', () => {
  const out = meetingsReplyLineHtml({ type: 'reply', author: { name: 'Betty' }, text: 'x', reply: { kind: 'chat', trigger: 'intro', source: 'fixed', delivered: true, fallback: true } });
  assert.match(out, /data-reply-kind="chat"/);
  assert.match(out, /chat \(fallback\) · source: fixed · intro/);
});

test('an undelivered reply is marked with the reason', () => {
  const out = meetingsReplyLineHtml({ type: 'reply', author: { name: 'Betty' }, text: 'x', reply: { kind: 'chat', trigger: 'addressed', delivered: false, undelivered: 'chat_send_failed reason=not_landed' } });
  assert.match(out, /is-undelivered/);
  assert.match(out, /data-reply-kind="not delivered"/);
  assert.match(out, /chat_send_failed reason=not_landed/);
});

test('the reach notice is empty until a reply fails, then names the Leave button once unreachable', () => {
  assert.strictEqual(meetingsReachHtml({}), '');
  assert.strictEqual(meetingsReachHtml(null), '');
  const some = meetingsReachHtml({ undelivered: 1, undelivered_reason: 'chat down' });
  assert.match(some, /1 reply could not be delivered/);
  assert.doesNotMatch(some, /is-unreachable/);
  const gone = meetingsReachHtml({ undelivered: 3, unreachable: true, unreachable_since: '2026-11-17T15:52:30Z', undelivered_reason: 'chat <down>' });
  assert.match(gone, /is-unreachable/);
  assert.match(gone, /Can't reach participants: 3 replies/);
  assert.match(gone, /Use Leave on this meeting/);
  assert.match(gone, /chat &lt;down&gt;/);
});

test('the Inbox transcript sets her lines apart and marks spoken, chat and undelivered', () => {
  const text = [
    'Ana: Betty, when does it <ship>?',
    'Betty (assistant, spoken; source: hive-status): It ships Friday.',
    'Bo (chat): thanks',
    'Betty (assistant, chat; source: model): Notes are in chat.',
    'Betty (assistant, not delivered: chat down; source: model): Lost line.',
  ].join('\n');
  const lines = meetingsTranscriptTextHtml(text).split('\n');
  assert.strictEqual(lines.length, 5);
  assert.strictEqual(lines[0], 'Ana: Betty, when does it &lt;ship&gt;?');
  assert.match(lines[1], /^<span class="inbox-assistant-line" data-reply-kind="spoken">/);
  assert.match(lines[1], /It ships Friday\./);
  assert.strictEqual(lines[2], 'Bo (chat): thanks');
  assert.match(lines[3], /data-reply-kind="chat"/);
  assert.match(lines[4], /class="inbox-assistant-line is-undelivered" data-reply-kind="not delivered"/);
  // A participant who merely says "(assistant" mid-line is not her.
  assert.strictEqual(meetingsTranscriptTextHtml('Ana: my (assistant, chat) said: hi'), 'Ana: my (assistant, chat) said: hi');
  assert.strictEqual(meetingsTranscriptTextHtml(''), '');
});
