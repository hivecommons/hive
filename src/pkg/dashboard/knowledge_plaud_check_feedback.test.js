// Knowledge Integrations "Check" (hivecommons/hive#18092): the per-connector
// Check button must always report an outcome. A failed status call surfaces
// the server's `error` string verbatim in an error toast; a successful one
// surfaces the sign-in state in a success/info toast and re-renders the card.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
// Like the other dashboard tests' fnSrc, but keeps a leading `async` so the
// extracted handlers can still await.
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

const NOT_CONFIGURED = 'no knowledge connectors are configured';
const HTTP_NOT_FOUND = 404;
const HTTP_OK = 200;

const setup = ({ status, body, cardPresent = true }) => {
  const toasts = [];
  const calls = { urls: [], renders: 0 };
  const fetch = async url => {
    calls.urls.push(url);
    return { ok: status === HTTP_OK, status, json: async () => body };
  };
  const document = { querySelector: () => (cardPresent ? {} : null) };
  const CSS = { escape: s => s };
  const h = new Function('fetch', 'document', 'CSS', 'showToast', 'kbRenderIntegrationsModal',
    'const _kbPlaudAuth = {};\n' + fnSrc('kbPlaudAuthLoad') + '\n' + fnSrc('kbPlaudAuthSummary') + '\n' + fnSrc('kbPlaudRefreshAuth') +
    '\nreturn { kbPlaudRefreshAuth, _kbPlaudAuth };')(fetch, document, CSS, (m, k) => toasts.push([m, k]), () => { calls.renders++; });
  return { h, toasts, calls };
};

test('a 404 from the auth endpoint shows an error toast with the server error verbatim', async () => {
  const { h, toasts, calls } = setup({ status: HTTP_NOT_FOUND, body: { error: NOT_CONFIGURED } });
  await h.kbPlaudRefreshAuth('plaud-voice');
  assert.deepStrictEqual(calls.urls, ['/api/knowledge/connectors/plaud-voice/auth?refresh=1']);
  assert.strictEqual(toasts[0][1], 'info');
  const last = toasts[toasts.length - 1];
  assert.strictEqual(last[1], 'error');
  assert.ok(last[0].includes(NOT_CONFIGURED), 'toast must carry the server error: ' + last[0]);
  assert.strictEqual(calls.renders, 1, 'card re-rendered with the error state');
  assert.strictEqual(h._kbPlaudAuth['plaud-voice'].error, NOT_CONFIGURED);
});

test('a non-JSON failure falls back to the HTTP status in the error toast', async () => {
  const { h, toasts } = setup({ status: HTTP_NOT_FOUND, body: {} });
  await h.kbPlaudRefreshAuth('p');
  const last = toasts[toasts.length - 1];
  assert.strictEqual(last[1], 'error');
  assert.ok(last[0].includes('HTTP ' + HTTP_NOT_FOUND), last[0]);
});

test('a connected state shows a success toast naming the account and refreshes the card', async () => {
  const { h, toasts, calls } = setup({ status: HTTP_OK, body: { connected: true, account: 'andy' } });
  await h.kbPlaudRefreshAuth('plaud-voice');
  const last = toasts[toasts.length - 1];
  assert.strictEqual(last[1], 'success');
  assert.ok(last[0].includes('plaud-voice') && last[0].includes('connected as andy'), last[0]);
  assert.strictEqual(calls.renders, 1);
});

test('a not-connected state is reported as info, with the server message', async () => {
  const { h, toasts } = setup({ status: HTTP_OK, body: { connected: false, message: 'token expired' } });
  await h.kbPlaudRefreshAuth('p');
  const last = toasts[toasts.length - 1];
  assert.strictEqual(last[1], 'info');
  assert.ok(last[0].includes('not connected') && last[0].includes('token expired'), last[0]);
});

test('a pending state tells the user where the authorization link is', async () => {
  const { h, toasts } = setup({ status: HTTP_OK, body: { pending: true, auth_url: 'https://example.invalid/auth' } });
  await h.kbPlaudRefreshAuth('p');
  const last = toasts[toasts.length - 1];
  assert.strictEqual(last[1], 'info');
  assert.ok(last[0].includes('waiting for authorization'), last[0]);
});

test('the toast is still shown when the card is no longer in the DOM', async () => {
  const { h, toasts, calls } = setup({ status: HTTP_NOT_FOUND, body: { error: NOT_CONFIGURED }, cardPresent: false });
  await h.kbPlaudRefreshAuth('p');
  assert.strictEqual(calls.renders, 0);
  assert.strictEqual(toasts[toasts.length - 1][1], 'error');
});
