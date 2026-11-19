// A blocked Discord join is the bot's server permissions on the voice
// channel, not a guest-bot refusal (Discord has no lobby), and a /hive join
// request whose approval failed shows why (hivecommons/hive#18598). The
// functions are lifted straight out of index.html so this runs against the
// shipped source.
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
  pick(/const MEETINGS_ERROR_INLINE_CHARS = [^\n]*\n/, 'MEETINGS_ERROR_INLINE_CHARS'),
  pick(/function meetingsFriendlyReason\(m\) \{[\s\S]*?\n    \}\n/, 'meetingsFriendlyReason'),
  pick(/function meetingsReasonHtml\(m\) \{[\s\S]*?\n    \}\n/, 'meetingsReasonHtml'),
  'module.exports = { meetingsFriendlyReason, meetingsReasonHtml };',
].join('\n');
const mod = { exports: {} };
new Function('module', src)(mod);
const { meetingsFriendlyReason, meetingsReasonHtml } = mod.exports;

const DISCORD_BLOCKED = 'quorum meeting.join: blocked: the bot has no permission to connect to that voice channel';

test('a blocked Discord join names the bot permissions, not a guest refusal', () => {
  const line = meetingsFriendlyReason({ platform: 'discord', state: 'blocked', state_reason: DISCORD_BLOCKED });
  assert.match(line, /Connect/);
  assert.doesNotMatch(line, /guest bot/);
});

test('a blocked Meet or Teams join is still a guest refusal', () => {
  assert.strictEqual(
    meetingsFriendlyReason({ platform: 'google_meet', state: 'blocked', state_reason: 'quorum meeting.join: blocked: denied' }),
    'The meeting refused a guest bot.');
});

test('a pending request whose approval failed shows the reason', () => {
  const html = meetingsReasonHtml({ platform: 'discord', state: 'requested', state_reason: DISCORD_BLOCKED });
  assert.match(html, /Discord bot cannot join/);
  assert.match(html, /no permission to connect/);
});
