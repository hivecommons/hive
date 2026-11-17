// The email-in address row shared by the Inbox, Calendar and Meetings
// sections (hivecommons/hive#18693): label, mailto link, Copy button and a
// one-line hint; a "not set up yet" line with the docs link otherwise.
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
// constSrc returns a top-level `const NAME = {...};` object literal.
const constSrc = name => {
  const start = html.indexOf('const ' + name + ' = {');
  assert.ok(start >= 0, name + ' not found');
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1) + ';';
  }
  throw new Error('unbalanced ' + name);
};
const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
const DOCS = 'https://github.com/hivecommons/hive/blob/v5/src/docs/meetings.md#is-the-address-set-up';

const harness = () => {
  const calls = { toasts: [], copied: [] };
  const navigator = { clipboard: { writeText: async t => { calls.copied.push(t); } } };
  const api = new Function('escapeHtml', 'navigator', 'showToast',
    "const EMAIL_IN_DOCS_URL = '" + DOCS + "';\n" + constSrc('EMAIL_IN_KINDS') + '\n' + constSrc('EMAIL_IN_REASONS') + '\n' +
    ['emailInValid', 'emailInMailto', 'emailInRowHtml', 'emailInCopy'].map(fnSrc).join('\n') +
    '\nreturn { emailInValid, emailInMailto, emailInRowHtml, emailInCopy };')(escapeHtml, navigator, (m, k) => calls.toasts.push([m, k]));
  return { api, calls, navigator };
};
const ready = { address: 'hive@acme.hive.example.dev', ready: true, files_enabled: true, invites_enabled: true, docs_url: DOCS };

test('the docs link constant matches the page', () => {
  assert.ok(html.includes("const EMAIL_IN_DOCS_URL = '" + DOCS + "';"));
});

test('each section shows the same address as a mailto link with a Copy button and its own hint', () => {
  const { api } = harness();
  const hints = {
    inbox: [/Send to this inbox/, /Email or forward a recording, document or note to this address and it lands here\./],
    calendar: [/Invite by email/, /Add this address to a calendar invite and hive joins the meeting\./],
    meetings: [/Invite by email/, /or paste a link below\./],
  };
  for (const kind of Object.keys(hints)) {
    const out = api.emailInRowHtml(kind, ready);
    assert.match(out, new RegExp('data-email-in="' + kind + '" data-email-in-state="ready"'));
    assert.match(out, /<a class="email-in-address" href="mailto:hive@acme\.hive\.example\.dev" data-email-in-address[^>]*>hive@acme\.hive\.example\.dev<\/a>/, kind + ': a plain mailto link, no subject');
    assert.doesNotMatch(out, /mailto:[^"]*\?/, 'no prefilled subject: the subject of a mail becomes the item title');
    assert.match(out, /data-action="emailInCopy" data-arg0="hive@acme\.hive\.example\.dev"[^>]*>Copy<\/button>/);
    for (const re of hints[kind]) assert.match(out, re, kind);
  }
});

test('the address is escaped in the text, the copy argument and the mailto link', () => {
  const { api } = harness();
  // A local part may legally carry characters that matter in HTML and URLs.
  const out = api.emailInRowHtml('inbox', Object.assign({}, ready, { address: 'a+b&c=d%e@xn--bcher-kva.example' }));
  assert.match(out, /href="mailto:a%2Bb%26c%3Dd%25e@xn--bcher-kva\.example"/, 'local part percent-encoded, the @ literal');
  assert.match(out, />a\+b&amp;c=d%e@xn--bcher-kva\.example<\/a>/);
  assert.match(out, /data-arg0="a\+b&amp;c=d%e@xn--bcher-kva\.example"/);
  assert.strictEqual(api.emailInMailto('hive@acme.example'), 'mailto:hive@acme.example');
  assert.strictEqual(api.emailInMailto('a?subject=x@acme.example'), 'mailto:a%3Fsubject%3Dx@acme.example', 'a "?" cannot start mailto headers');
});

test('an address that is not one address never becomes a link', () => {
  const { api } = harness();
  for (const bad of ['', 'hive', 'hive@', '@host', 'a b@host', 'a@b@c', 'x@host"><script>', 'a@host,b@host', 'hive@host?cc=x@y', "o'brien@host", null, 42]) {
    assert.strictEqual(api.emailInValid(bad), false, String(bad));
    const out = api.emailInRowHtml('inbox', Object.assign({}, ready, { address: bad }));
    assert.match(out, /data-email-in-state="off"/, String(bad));
    assert.doesNotMatch(out, /mailto:|emailInCopy|<script/, String(bad));
  }
  assert.strictEqual(api.emailInValid('hive@acme.hive.example.dev'), true);
});

test('not set up: a short line and the docs link, no address', () => {
  const { api } = harness();
  let out = api.emailInRowHtml('inbox', { ready: false, reason: 'no_relay', docs_url: DOCS });
  assert.match(out, /data-email-in-state="off"/);
  assert.match(out, /Not set up yet: this hive is not connected to a hub that relays mail to it\./);
  assert.ok(out.includes('<a class="inbox-link" href="' + DOCS + '" target="_blank" rel="noopener noreferrer">How to set it up ↗</a>'));
  assert.doesNotMatch(out, /mailto:|emailInCopy/);
  out = api.emailInRowHtml('calendar', { ready: false, reason: 'no_host' });
  assert.match(out, /public address is not known/);
  assert.ok(out.includes('href="' + DOCS + '"'), 'falls back to the built-in docs link');
  assert.match(api.emailInRowHtml('meetings', { ready: false, reason: 'whatever' }), /Not set up yet: mail cannot reach this hive\./);
  // A docs link that is not https is replaced by the built-in one.
  assert.ok(api.emailInRowHtml('inbox', { ready: false, docs_url: 'javascript:void(0)' }).includes('href="' + DOCS + '"'));
});

test('a switched-off kind says so instead of showing an address that would bounce', () => {
  const { api } = harness();
  let out = api.emailInRowHtml('inbox', Object.assign({}, ready, { files_enabled: false }));
  assert.match(out, /Mailed-in files are turned off on this hive \(Settings, Meeting attendee, Email\)\./);
  assert.doesNotMatch(out, /mailto:/);
  for (const kind of ['calendar', 'meetings']) {
    out = api.emailInRowHtml(kind, Object.assign({}, ready, { invites_enabled: false }));
    assert.match(out, /Email invites are turned off on this hive/);
    assert.doesNotMatch(out, /mailto:/);
  }
  // Files off does not hide the invite address, and the other way round.
  assert.match(api.emailInRowHtml('calendar', Object.assign({}, ready, { files_enabled: false })), /mailto:/);
  assert.match(api.emailInRowHtml('inbox', Object.assign({}, ready, { invites_enabled: false })), /mailto:/);
});

test('no email_in block (a read viewer, an older hive) renders nothing', () => {
  const { api } = harness();
  for (const view of [undefined, null, '', 'hive@x']) assert.strictEqual(api.emailInRowHtml('inbox', view), '');
  assert.strictEqual(api.emailInRowHtml('nope', ready), '');
});

test('Copy writes the address to the clipboard and reports a failure', async () => {
  const { api, calls, navigator } = harness();
  await api.emailInCopy('hive@acme.hive.example.dev');
  assert.deepStrictEqual(calls.copied, ['hive@acme.hive.example.dev']);
  assert.deepStrictEqual(calls.toasts[0], ['Copied hive@acme.hive.example.dev', 'success']);
  await api.emailInCopy('not an address');
  assert.strictEqual(calls.copied.length, 1);
  navigator.clipboard.writeText = async () => { throw new Error('denied'); };
  await api.emailInCopy('hive@acme.hive.example.dev');
  assert.deepStrictEqual(calls.toasts[1], ['Copy failed: denied', 'error']);
});

test('the three sections render the row from their own state', () => {
  assert.match(fnSrc('renderInbox'), /emailInRowHtml\('inbox', data\.email_in\)/);
  assert.match(fnSrc('renderCalendar'), /emailInRowHtml\('calendar', data\.email_in\)/);
  assert.match(fnSrc('fetchMeetings'), /_meetingsEmailIn = data\.email_in \|\| null/);
  assert.match(fnSrc('renderMeetings'), /emailInRowHtml\('meetings', _meetingsEmailIn\) : ''\) \+ meetingsJoinFormHtml\(\)/);
});
