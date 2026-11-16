'use strict';

// Inbox item rendering: summary (collapsible), pending note, transcript
// disclosure and the ▶ Play recording control, extracted from the generated
// dashboard page so the test exercises the shipped code.
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
    if (depth === 0) return indexHtml.slice(start, i + 1);
  }
  throw new Error(`unterminated ${name}`);
}

function makeContext(role) {
  const ctx = {
    window: { _hiveRole: role },
    INBOX_SOURCE_ICONS: { plaud: '🎙️' },
    INBOX_DEST_LABELS: { spek_rfc: 'RFC', dibs: 'dibs' },
    INBOX_SUMMARY_COLLAPSE_CHARS: 280,
    _inboxSelected: new Set(),
    _inboxExpanded: new Set(),
    _inboxTranscriptOpen: new Set(),
    _inboxContent: {},
    _inboxTranscribing: new Set(),
    INBOX_TX_STATUS_LABELS: { queued: 'Transcription queued', running: 'Transcribing', done: 'Machine transcript', failed: 'Transcription failed', skipped: 'Not transcribed' },
    formatDashboardDateTime: (s) => String(s),
    escapeHtml: (s) => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
    viewerIsOwner: () => role === 'owner',
    relativeTimeShort: () => '2h ago',
    Number,
    Math,
    String,
    Object,
    Array,
  };
  vm.createContext(ctx);
  for (const fn of ['inboxSourceIcon', 'inboxItemKey', 'inboxViewerCanPlay', 'inboxApiPath', 'inboxFmtDuration', 'inboxStatePill', 'inboxSummaryHtml', 'inboxTranscriptHtml', 'inboxTxUnavailableText', 'inboxTxEtaText', 'inboxTxElapsedText', 'inboxTxProgressHtml', 'inboxTranscriptionHtml', 'inboxItemHtml']) {
    vm.runInContext(extractFunction(fn), ctx);
  }
  return ctx;
}

const pendingItem = { source: 'plaud', id: 'f_1', title: 'nate v2 - workflow', recorded_at: '2026-11-11T19:24:28Z', duration_ms: 307100, pending: true, fact_slug: 'plaud-plaud-f_1', layer: 'personal' };
const readyItem = { source: 'plaud', id: 'f_2', title: 'Standup', duration_ms: 125000, summary: 'Key points: ship it.', has_transcript: true };

test('pending item explains that Plaud has no text yet and never shows the MCP error line', () => {
  const ctx = makeContext('owner');
  const html = vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, pendingItem, { excerpt: 'Block "transaction" not available for this recording. Available blocks: mark_memo.' })) + ')', ctx);
  assert.match(html, /Not transcribed yet in Plaud/);
  assert.doesNotMatch(html, /not available for this recording/);
  assert.match(html, /recorded 2h ago · 5m 7s/);
  assert.match(html, /data-action="inboxPlay" data-arg0="plaud" data-arg1="f_1"/);
  assert.match(html, /▶ Play recording/);
  assert.match(html, /data-action="inboxToggleTranscript"/);
  assert.match(html, /data-inbox-media="plaud\/f_1"/);
  for (const action of ['inboxOpenRoute', 'inboxDismiss', 'inboxOpenFact']) assert.match(html, new RegExp('data-action="' + action + '"'), action + ' action kept');
  assert.doesNotMatch(html, /<audio/, 'no audio element (and no URL) before the owner clicks play');
});

test('the Knowledge fact is a status pill and a metadata link, never a row button', () => {
  const ctx = makeContext('owner');
  const html = vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, pendingItem, { fact_state: 'pending' })) + ')', ctx);
  assert.doesNotMatch(html, /📚 Fact/);
  assert.doesNotMatch(html, /<button[^>]*data-action="inboxOpenFact"/, 'no Fact button');
  assert.match(html, /<div class="muted-xs">[^]*<a href="#" class="inbox-fact-link" data-action="inboxOpenFact" data-prevent="1" data-arg0="plaud-plaud-f_1" data-arg1="personal"[^>]*>open in Knowledge<\/a><\/div>/);
  assert.match(html, /<span class="kc-pill" role="status" data-status="not loaded" data-fact-state="pending"[^>]*>Pending text<\/span>/);
  const machine = vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, pendingItem, { pending: false, fact_state: 'machine' })) + ')', ctx);
  assert.match(machine, /data-fact-state="machine"[^>]*>In knowledge \(machine text\)<\/span>/);
  const ready = vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, readyItem, { fact_slug: 'plaud-plaud-f_2', fact_state: 'ready' })) + ')', ctx);
  assert.match(ready, /data-fact-state="ready"[^>]*>In knowledge<\/span>/);
  const live = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  assert.doesNotMatch(live, /inbox-fact-link|data-fact-state/, 'no fact yet: no pill, no link');
});

test('summary renders inline and collapses past the threshold with a more toggle', () => {
  const ctx = makeContext('owner');
  let html = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  assert.match(html, /<div class="inbox-summary">Key points: ship it\.<\/div>/);
  assert.doesNotMatch(html, /inbox-more/);
  const long = Object.assign({}, readyItem, { summary: 'x'.repeat(300) });
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(long) + ')', ctx);
  assert.match(html, /inbox-summary is-collapsed/);
  assert.match(html, /data-action="inboxToggleSummary"[^>]*>more</);
  vm.runInContext('_inboxExpanded.add("plaud/f_2")', ctx);
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(long) + ')', ctx);
  assert.doesNotMatch(html, /is-collapsed/);
  assert.match(html, />less</);
});

test('transcript disclosure loads on demand and escapes the text', () => {
  const ctx = makeContext('read-only');
  let html = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  assert.doesNotMatch(html, /inbox-transcript/);
  vm.runInContext('_inboxTranscriptOpen.add("plaud/f_2")', ctx);
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  assert.match(html, /Loading transcript…/);
  vm.runInContext('_inboxContent["plaud/f_2"] = { transcript: "[00:01] A: <b>hi</b>", highlights: "- 05:03" }', ctx);
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  assert.match(html, /inbox-transcript-text">\[00:01\] A: &lt;b&gt;hi&lt;\/b&gt;</);
  assert.match(html, /Highlights/);
  assert.match(html, /aria-expanded="true"/);
});

test('play control follows the role: owner and read-write only', () => {
  for (const [role, want] of [['owner', true], ['read-write', true], ['read-only', false], [undefined, false]]) {
    const ctx = makeContext(role);
    const html = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
    assert.equal(/inboxPlay/.test(html), want, 'role ' + role);
  }
  const ctx = makeContext('owner');
  assert.equal(vm.runInContext('inboxApiPath("plaud", "a/b c", "audio")', ctx), '/api/inbox/plaud/a%2Fb%20c/audio');
});

test('File RFC route state: a failed hand-off shows its error inline, delegated names the agent', () => {
  const ctx = makeContext('owner');
  const failed = Object.assign({}, readyItem, { routes: { spek_rfc: { status: 'failed', repo: 'o/r', error: 'spek_rfc: handing the RFC to agent <builder> failed: busy' } } });
  let html = vm.runInContext('inboxItemHtml(' + JSON.stringify(failed) + ')', ctx);
  assert.match(html, /data-status="error"[^>]*>RFC failed</);
  assert.match(html, /class="kc-error muted-xs" role="alert">spek_rfc: handing the RFC to agent &lt;builder&gt; failed: busy</);
  assert.match(html, />Retry RFC…</);

  const delegated = Object.assign({}, readyItem, { routes: { spek_rfc: { status: 'delegated', repo: 'o/r', note: 'handed to agent builder to draft with spek and publish to o/r' } } });
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(delegated) + ')', ctx);
  assert.match(html, /data-status="ok"[^>]*>RFC handed to agent</);
  assert.match(html, /muted-xs">handed to agent builder to draft with spek and publish to o\/r</);
  assert.doesNotMatch(html, /role="alert"/);
});
