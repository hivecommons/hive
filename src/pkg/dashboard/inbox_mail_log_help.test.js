// The line under a refused or held mail in the Inbox's Recent mail list
// (hivecommons/hive#18744): how to try again, the docs link, a prefilled
// problem report, and Approve and Decline for a mail that awaits approval.
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
const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');

function harness(role) {
  const window = { _hiveRole: role };
  const factory = new Function('window', 'escapeHtml',
    fnSrc('inboxMailLogLink') + '\n' + fnSrc('inboxMailLogHelpHtml') + '\nreturn { inboxMailLogHelpHtml, inboxMailLogLink };');
  return factory(window, escapeHtml);
}

const HELP = 'https://github.com/hivecommons/hive/blob/v5/src/docs/inbox-voice-sources.md#unsupported-attachment';
const REPORT = 'https://github.com/hivecommons/hive/issues/new?body=Reason+code&labels=kind%2Fbug&title=Mail-in';
const refused = { outcome: 'rejected', reason: 'unsupported_attachment', how_to_fix: 'send the file as an accepted type', help_url: HELP, report_url: REPORT };
const held = { outcome: 'awaiting_approval', approval_reason: 'sender_not_on_allowlist', how_to_fix: 'add the address', help_url: HELP, report_url: REPORT, pending_items: ['0a1b2c3d', 'ffee0011'] };

test('a refused mail says how to try again and links the docs and a problem report', () => {
  const out = harness('owner').inboxMailLogHelpHtml(refused);
  assert.match(out, /<strong>How to try again:<\/strong> send the file as an accepted type\./);
  assert.ok(out.includes('href="' + escapeHtml(HELP) + '"'), out);
  assert.match(out, />What this means<\/a>/);
  assert.ok(out.includes('href="' + escapeHtml(REPORT) + '"'), out);
  assert.match(out, />Report a problem<\/a>/);
  assert.match(out, /target="_blank" rel="noopener noreferrer"/);
  assert.doesNotMatch(out, /inboxMailLogApprove|inboxMailLogDecline/, 'nothing to approve on a refusal');
});

test('a received mail has no help line', () => {
  assert.strictEqual(harness('owner').inboxMailLogHelpHtml({ outcome: 'accepted', help_url: HELP, report_url: REPORT }), '');
});

test('a held mail gives the owner Approve and Decline for every item it brought', () => {
  const out = harness('owner').inboxMailLogHelpHtml(held);
  assert.match(out, /data-action="inboxMailLogApprove" data-arg0="0a1b2c3d,ffee0011"[^>]*>✓ Approve<\/button>/);
  assert.match(out, /data-action="inboxMailLogDecline" data-arg0="0a1b2c3d,ffee0011"[^>]*>✕ Decline<\/button>/);
  assert.match(out, />Report a problem<\/a>/);
});

test('a read-write user sees who can approve, and no buttons', () => {
  const out = harness('read-write').inboxMailLogHelpHtml(held);
  assert.doesNotMatch(out, /<button/);
  assert.match(out, /owner can approve or decline it/);
});

test('a held mail with nothing left pending says so', () => {
  const out = harness('owner').inboxMailLogHelpHtml(Object.assign({}, held, { pending_items: [] }));
  assert.doesNotMatch(out, /<button/);
  assert.match(out, /Already approved or declined/);
});

test('only https links and well-formed item ids are rendered', () => {
  const api = harness('owner');
  for (const bad of ['javascript:alert(1)', 'http://example.com/', '//example.com', '', null, 42]) {
    assert.strictEqual(api.inboxMailLogLink(bad, 'x', 'y'), '', String(bad));
  }
  const out = api.inboxMailLogHelpHtml(Object.assign({}, held, { help_url: 'javascript:alert(1)', pending_items: ['"><script>', '0a1b'], how_to_fix: '<b>x</b>' }));
  assert.doesNotMatch(out, /javascript:|<script|<b>/);
  assert.match(out, /data-arg0="0a1b"/);
  assert.match(out, />How to try again<\/a>|How to try again:/);
});
