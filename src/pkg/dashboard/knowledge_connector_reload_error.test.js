// kcSave surfaces a connector save the running syncer did not follow
// (hivecommons/hive#18091): the PUT answers 200 with `runtime_error` because
// the config is already persisted, and kcSave rejects with a message that
// says so, so every caller's catch handler toasts it instead of the plain
// "saved" success. A response without runtime_error resolves as before.
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

const HTTP_OK = 200;
const HTTP_BAD_REQUEST = 400;
const RELOAD_ERROR = 'plaud: scope.home must be an absolute path';
const SAVED_LIST = [{ name: 'voice', type: 'plaud', enabled: true }];

// Drive the real kcSave with a stubbed fetch; returns the state it mutated
// and how often it refreshed the list so the saved path is still observable.
const save = async (res, list = SAVED_LIST) => {
  const calls = { reloads: 0, puts: [] };
  const fetch = async (url, init) => { calls.puts.push({ url, init }); return res; };
  const state = { connectors: [] };
  const run = new Function('fetch', '_kcState', 'loadKnowledgeConnectors',
    fnSrc('kcSave') + '\nreturn kcSave;');
  const kcSave = run(fetch, state, async () => { calls.reloads++; });
  const outcome = await kcSave(list).then(() => null, e => e);
  return { outcome, state, calls };
};

test('runtime_error on a 200 rejects with the saved-but-not-reloaded message, after the list refreshed', async () => {
  const res = { ok: true, status: HTTP_OK, json: async () => ({ connectors: SAVED_LIST, runtime: true, runtime_error: RELOAD_ERROR }) };
  const { outcome, state, calls } = await save(res);
  assert.ok(outcome instanceof Error, 'kcSave must reject');
  assert.strictEqual(outcome.message, 'saved, but the running syncer did not reload: ' + RELOAD_ERROR);
  assert.deepStrictEqual(state.connectors, SAVED_LIST, 'the persisted list is still stored');
  assert.strictEqual(calls.reloads, 1, 'the connector list is still reloaded before the error surfaces');
});

test('the rejection reaches the caller toast with the reload error verbatim', async () => {
  const res = { ok: true, status: HTTP_OK, json: async () => ({ connectors: SAVED_LIST, runtime_error: RELOAD_ERROR }) };
  const { outcome } = await save(res);
  const toasts = [];
  const showToast = (msg, kind) => toasts.push({ msg, kind });
  // Mirrors kcToggleEnabled's handlers: the catch branch fires, not the success toast.
  await Promise.reject(outcome).then(() => showToast('Connector voice updated', 'success')).catch(e => showToast('Connector update failed: ' + e.message, 'error'));
  assert.deepStrictEqual(toasts, [{ msg: 'Connector update failed: saved, but the running syncer did not reload: ' + RELOAD_ERROR, kind: 'error' }]);
});

test('a clean save without runtime_error resolves', async () => {
  const res = { ok: true, status: HTTP_OK, json: async () => ({ connectors: SAVED_LIST, runtime: true }) };
  const { outcome, state, calls } = await save(res);
  assert.strictEqual(outcome, null);
  assert.deepStrictEqual(state.connectors, SAVED_LIST);
  assert.strictEqual(calls.reloads, 1);
});

test('an empty runtime_error is not an error', async () => {
  const res = { ok: true, status: HTTP_OK, json: async () => ({ connectors: SAVED_LIST, runtime_error: '' }) };
  const { outcome } = await save(res);
  assert.strictEqual(outcome, null);
});

test('a rejected save still reports the server error and does not refresh the list', async () => {
  const res = { ok: false, status: HTTP_BAD_REQUEST, json: async () => ({ error: 'name is required' }) };
  const { outcome, calls } = await save(res);
  assert.ok(outcome instanceof Error);
  assert.strictEqual(outcome.message, 'name is required');
  assert.strictEqual(calls.reloads, 0);
});
