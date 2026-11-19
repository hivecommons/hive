// Inbox playback and share: ▶ Play recording streams from the hive's own
// origin (the CSP refuses cross-origin media and the presigned link must
// stay off the page), and 🔗 Share hands out an Inbox deep link, never the
// media URL. Functions are extracted from the generated page and run in a
// sandbox. Run: node --test src/pkg/dashboard/*.test.js
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
  const m = new RegExp('const ' + name + " = '[^']*';").exec(html);
  assert.ok(m, name + ' not found');
  return m[0];
};

const fakeElement = tag => ({
  tag, children: [], listeners: {}, innerHTML: '', className: '', textContent: '',
  appendChild(c) { this.children.push(c); return c; },
  addEventListener(ev, fn) { this.listeners[ev] = fn; },
  canPlayType: () => 'probably',
});

const harness = ({ fetchReply, share, clipboardOK = true } = {}) => {
  const calls = { fetches: [], toasts: [], clipboard: [], shares: [] };
  const mount = fakeElement('div');
  const document = {
    querySelector: sel => (sel.includes('data-inbox-media') ? mount : null),
    createElement: fakeElement,
  };
  const fetch = async (url, opts) => {
    calls.fetches.push({ url, opts });
    return fetchReply || { ok: true, status: 206, headers: { get: () => 'audio/ogg' }, json: async () => ({}) };
  };
  const navigator = share ? { share: async d => { calls.shares.push(d); if (share === 'abort') { const e = new Error('x'); e.name = 'AbortError'; throw e; } } } : {};
  const location = { origin: 'https://hive.example', pathname: '/', search: '?style=a', hash: '' };
  const items = [{ source: 'plaud', id: 'f_01', title: 'cost considerations' }];
  const api = new Function('document', 'fetch', 'navigator', 'location', 'CSS', 'showToast', 'writeClipboardText', 'window', 'renderInbox', 'items',
    'function inboxFind(src, id) { return items.find(it => it.source === src && it.id === id); }\n' +
    'function formatDashboardDate(d) { return String(d); }\n' + fnSrc('inboxAudioNoteText') + '\n' +
    fnSrc('inboxApiPath') + '\n' + fnSrc('inboxStreamURL') + '\n' + fnSrc('inboxAudioErrorReason') + '\n' + fnSrc('inboxPlay') + '\n' +
    constSrc('INBOX_SHARE_HASH') + '\n' + fnSrc('inboxShareLink') + '\n' + fnSrc('inboxFocusKeyFromHash') + '\n' + fnSrc('inboxShareCopy') + '\n' + fnSrc('inboxShareNative') + '\n' +
    'return { inboxPlay, inboxStreamURL, inboxAudioErrorReason, inboxShareLink, inboxFocusKeyFromHash, inboxShareCopy, inboxShareNative };',
  )(document, fetch, navigator, location, { escape: s => s }, (msg, kind) => calls.toasts.push([msg, kind]),
    async text => { calls.clipboard.push(text); return clipboardOK; }, {}, () => {}, items);
  return { api, calls, mount };
};

test('Play puts the same-origin stream URL on the <audio> element, no link fetch', () => {
  const { api, calls, mount } = harness();
  api.inboxPlay('plaud', 'f_01/x');
  assert.strictEqual(calls.fetches.length, 0, 'no presigned-link request from the page');
  const audio = mount.children.find(c => c.tag === 'audio');
  assert.ok(audio, 'audio element mounted');
  assert.strictEqual(audio.src, '/api/inbox/plaud/f_01%2Fx/audio/stream');
  assert.ok(!/^https?:/.test(audio.src), 'relative, same-origin');
  assert.ok(audio.controls && audio.autoplay);
  assert.strictEqual(typeof audio.listeners.error, 'function', 'playback errors are reported');
  assert.doesNotMatch(mount.children[1].textContent, /valid until/);
});

test('a failed stream toasts the hive error, without any link', async () => {
  const { api } = harness({ fetchReply: { ok: false, status: 502, json: async () => ({ error: "the recording's storage refused the audio link (expired or revoked)" }) } });
  const reason = await api.inboxAudioErrorReason('plaud', 'f_01', fakeElement('audio'));
  assert.match(reason, /refused the audio link/);
  const ok = harness({ fetchReply: { ok: true, status: 206, headers: { get: () => 'audio/ogg; codecs=opus' }, json: async () => ({}) } });
  const audio = fakeElement('audio');
  audio.canPlayType = () => '';
  assert.match(await ok.api.inboxAudioErrorReason('plaud', 'f_01', audio), /cannot play audio\/ogg/);
  assert.strictEqual(ok.calls.fetches[0].url, '/api/inbox/plaud/f_01/audio/stream');
  assert.strictEqual(ok.calls.fetches[0].opts.headers.Range, 'bytes=0-0');
});

test('Share produces an Inbox deep link, never the media URL', async () => {
  const { api, calls } = harness();
  const link = api.inboxShareLink('plaud', 'f_01');
  assert.strictEqual(link, 'https://hive.example/?style=a#section-inbox-section?item=plaud%2Ff_01');
  assert.doesNotMatch(link, /audio|amazonaws|X-Amz/);
  assert.strictEqual(api.inboxFocusKeyFromHash('#section-inbox-section?item=plaud%2Ff_01'), 'plaud/f_01');
  assert.strictEqual(api.inboxFocusKeyFromHash(link), 'plaud/f_01');
  assert.strictEqual(api.inboxFocusKeyFromHash('#section-inbox-section'), '');
  assert.strictEqual(api.inboxFocusKeyFromHash('#section-inbox-section?item=%E0%A4%A'), '', 'malformed escape is ignored');

  await api.inboxShareCopy('plaud', 'f_01');
  assert.deepStrictEqual(calls.clipboard, [link], 'Copy link copies the deep link');
  assert.deepStrictEqual(calls.toasts[0], ['Inbox link copied', 'success']);
  assert.strictEqual(calls.fetches.length, 0);
});

test('Share uses the Web Share sheet when the browser has one', async () => {
  const { api, calls } = harness({ share: true });
  await api.inboxShareNative('plaud', 'f_01');
  assert.deepStrictEqual(calls.shares, [{ title: 'cost considerations', url: 'https://hive.example/?style=a#section-inbox-section?item=plaud%2Ff_01' }]);
  assert.strictEqual(calls.clipboard.length, 0);
  const cancelled = harness({ share: 'abort' });
  await cancelled.api.inboxShareNative('plaud', 'f_01');
  assert.strictEqual(cancelled.calls.clipboard.length, 0, 'a cancelled share sheet does not copy');
  const noClip = harness({ clipboardOK: false });
  await noClip.api.inboxShareCopy('plaud', 'f_01');
  assert.match(noClip.calls.toasts[0][0], /^Copy this Inbox link: https:\/\/hive\.example\//);
});
