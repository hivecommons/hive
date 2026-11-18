'use strict';

// The Inbox must not render with an unknown role (treated as read) and must
// re-render the moment /api/role resolves, instead of waiting for the 60 s
// refresh. Functions are extracted from the generated dashboard page.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

function extractFunction(name) {
  const start = indexHtml.indexOf('function ' + name + '(');
  assert.notEqual(start, -1, `${name} function exists`);
  const brace = indexHtml.indexOf('{', start);
  let depth = 0;
  for (let i = brace; i < indexHtml.length; i++) {
    const ch = indexHtml[i];
    if (ch === '{') depth++;
    if (ch === '}') depth--;
    if (depth === 0) return (indexHtml.slice(start - 6, start) === 'async ' ? 'async ' : '') + indexHtml.slice(start, i + 1);
  }
  throw new Error(`unterminated ${name}`);
}

const NAMES = ['inboxRoleKnown', 'inboxOnRoleReady', 'inboxCanShare', 'inboxViewerCanPlay', 'renderInbox', 'hiveFetchRoleOnce', 'hiveRoleSettled', 'hiveApplyRoleData', 'hiveRetryRole'];

function makeSandbox(opts) {
  const panel = { innerHTML: '' };
  const sb = {
    window: { _hiveRoleListeners: [] },
    document: { getElementById: (id) => (id === 'inbox-panel' ? panel : null) },
    panel,
    _inboxCache: { items: [{ source: 'plaud', id: 'a', processed: false }], sources: [], settings: {} },
    _inboxSelected: new Set(),
    _inboxShowProcessed: false,
    _inboxFocusKey: '',
    refreshSectionCardShell: () => {},
    setSidebarCountBadge: () => {},
    setSectionSummary: () => {},
    escapeHtml: (s) => String(s),
    viewerIsOwner: () => sb.window._hiveRole === 'owner',
    inboxSourceIcon: () => '',
    inboxSourcesHtml: () => '',
    inboxCanAdd: () => false,
    inboxItemHtml: () => '<div class="item"></div>',
    inboxPreserveAudio: (p, fn) => fn(),
    inboxApplyFocus: () => {},
    // Same effect as the real showGHAuthState: it records the role.
    showGHAuthState: (loggedIn, user, avatar, role) => { sb.window._hiveUser = user; if (role) sb.window._hiveRole = role; },
    fetch: opts.fetch,
    setTimeout: (fn) => { fn(); return 0; },
    console, Math, Number, Array, Error, Promise, String,
    HIVE_ROLE_FETCH_ATTEMPTS: 3,
    HIVE_ROLE_RETRY_BASE_MS: 500,
    HIVE_ROLE_SERVER_ERROR_STATUS: 500,
  };
  vm.createContext(sb);
  vm.runInContext(NAMES.map(extractFunction).join('\n'), sb);
  // Same registration inbox.js performs at load.
  sb.window._hiveRoleListeners.push(sb.inboxOnRoleReady);
  return sb;
}

test('inbox renders a loading state, not a read-only view, while the role is unknown', () => {
  const sb = makeSandbox({});
  sb.renderInbox();
  assert.match(sb.panel.innerHTML, /Loading inbox/);
  assert.doesNotMatch(sb.panel.innerHTML, /Select all/);
});

test('role arriving as owner re-renders the Inbox with the Hive actions', () => {
  const sb = makeSandbox({});
  sb.renderInbox();
  assert.match(sb.panel.innerHTML, /Loading inbox/);
  sb.window._hiveRole = 'owner';
  sb.hiveRoleSettled(false);
  assert.match(sb.panel.innerHTML, /Select all/);
  assert.equal(sb.inboxViewerCanPlay(), true);
});

test('role arriving as read keeps the Hive actions hidden', () => {
  const sb = makeSandbox({});
  sb.window._hiveRole = 'read';
  sb.hiveRoleSettled(false);
  assert.doesNotMatch(sb.panel.innerHTML, /Loading inbox/);
  assert.doesNotMatch(sb.panel.innerHTML, /Select all/);
  assert.equal(sb.inboxViewerCanPlay(), false);
});

test('an unchanged role does not re-render on later auth checks', () => {
  const sb = makeSandbox({});
  sb.window._hiveRole = 'owner';
  sb.hiveRoleSettled(false);
  sb.panel.innerHTML = 'sentinel';
  sb.hiveRoleSettled(false);
  assert.equal(sb.panel.innerHTML, 'sentinel');
});

test('role lookup that stays failed retries, then falls back to read-only and says so', async () => {
  let calls = 0;
  const sb = makeSandbox({ fetch: async () => { calls++; throw new Error('network'); } });
  sb.renderInbox();
  assert.match(sb.panel.innerHTML, /Loading inbox/);
  await sb.hiveRetryRole();
  assert.equal(calls, 2, 'retried after the first attempt (made by checkGHAuth)');
  assert.doesNotMatch(sb.panel.innerHTML, /Loading inbox/);
  assert.doesNotMatch(sb.panel.innerHTML, /Select all/);
  assert.match(sb.panel.innerHTML, /Could not determine your role/);
});

test('role lookup recovers when a retry succeeds', async () => {
  let calls = 0;
  const sb = makeSandbox({ fetch: async () => { calls++; return calls < 2 ? { status: 503, json: async () => ({}) } : { status: 200, json: async () => ({ user: 'andy', role: 'owner' }) }; } });
  await sb.hiveRetryRole();
  assert.equal(calls, 2);
  assert.match(sb.panel.innerHTML, /Select all/);
  assert.doesNotMatch(sb.panel.innerHTML, /Could not determine your role/);
});

test('a non-retryable answer is not retried', async () => {
  const sb = makeSandbox({ fetch: async () => ({ status: 404, json: async () => { throw new Error('not json'); } }) });
  await assert.rejects(sb.hiveFetchRoleOnce(), (e) => !e.retryable);
});
