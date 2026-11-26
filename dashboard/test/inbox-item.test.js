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
    INBOX_INTENT_LABELS: { spek_rfc: 'RFC', dismiss: 'Knowledge', spek: 'spek' },
    INBOX_SUMMARY_COLLAPSE_CHARS: 280,
    INBOX_SUMMARY_CLAMP_LINES: 3,
    INBOX_SHARE_LABELS: { rfc: 'RFC', dibs: 'DIBS', knowledge: 'Knowledge', spek: 'spek' },
    _inboxHistoryOpen: new Set(),
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
  for (const fn of ['inboxIconSvg', 'inboxTitleParts', 'inboxMarkdownToText', 'inboxFactStateHtml', 'inboxFactLayer', 'inboxSharePillText', 'inboxShareTargetHref', 'inboxSharePillHtml', 'inboxSharePillsHtml', 'inboxCanShare', 'inboxIntentLabel', 'inboxIntentThen', 'inboxSourceIcon', 'inboxItemHasAudio', 'inboxItemKey', 'inboxViewerCanPlay', 'inboxApiPath', 'inboxFmtDuration', 'inboxStatePill', 'inboxSummaryHtml', 'inboxTranscriptHtml', 'inboxTxUnavailableText', 'inboxTxEtaText', 'inboxTxElapsedText', 'inboxTxProgressHtml', 'inboxTranscriptionHtml', 'inboxDeleteKind', 'inboxCanDelete', 'inboxDeleteBusy', 'inboxDeleteButtonHtml', 'inboxItemHtml']) {
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
  assert.doesNotMatch(html, /▶ Play recording|🔗 Share ▾/, 'icon buttons, no text labels');
  assert.match(html, /data-action="inboxToggleTranscript"/);
  assert.match(html, /data-inbox-media="plaud\/f_1"/);
  for (const action of ['inboxShare', 'inboxOpenFact']) assert.match(html, new RegExp('data-action="' + action + '"'), action + ' action kept');
  for (const action of ['inboxOpenRoute', 'inboxDismiss']) assert.doesNotMatch(html, new RegExp('data-action="' + action + '"'), action + ' moved into the Share menu');
  assert.doesNotMatch(html, /<audio/, 'no audio element (and no URL) before the owner clicks play');
});

test('the Knowledge fact is a status pill and a metadata link, never a row button', () => {
  const ctx = makeContext('owner');
  const fact = { ...pendingItem, fact_state: 'pending', fact_path: '/data/knowledge/connectors/personal/plaud-plaud-f_1.md', fact_source_layer: 'vault:sha256:recordings' };
  const html = ctx.inboxItemHtml(fact);
  assert.doesNotMatch(html, /📚 Fact/);
  assert.doesNotMatch(html, /<button[^>]*data-action="inboxOpenFact"/, 'no Fact button');
  assert.match(html, /<div class="muted-xs inbox-meta">[^]*<a href="#" class="inbox-fact-link" data-action="inboxOpenFact" data-prevent="1" data-arg0="plaud-plaud-f_1" data-arg1="vault:sha256:recordings"[^>]*>open in Knowledge<\/a><\/div>/);
  // The Knowledge state sits in the meta line, before the fact link, not as a floating pill.
  assert.match(html, /<div class="muted-xs inbox-meta">[^]*<span class="inbox-meta-state" role="status" data-status="not loaded" data-fact-state="pending"[^>]*>Pending text<\/span> · <a href="#" class="inbox-fact-link"/);
  assert.doesNotMatch(html, /kc-pill"[^>]*data-fact-state/, 'no floating fact pill');
  const machine = vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, pendingItem, { pending: false, fact_state: 'machine' })) + ')', ctx);
  assert.match(machine, /data-fact-state="machine"[^>]*>In knowledge \(machine text\)<\/span>/);
  const ready = vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, readyItem, { fact_slug: 'plaud-plaud-f_2', fact_state: 'ready' })) + ')', ctx);
  assert.match(ready, /data-fact-state="ready"[^>]*>In knowledge<\/span>/);
  const live = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  assert.doesNotMatch(live, /inbox-fact-link|data-fact-state/, 'no fact yet: no pill, no link');
});

test('Play and Share are icon buttons with an accessible name and a tooltip; Transcript is a compact toggle', () => {
  const ctx = makeContext('owner');
  const html = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  const play = /<button type="button" class="hv-btn btn-icon btn-sm inbox-action inbox-play" data-action="inboxPlay" data-arg0="plaud" data-arg1="f_2" aria-label="Play recording" title="[^"]+">(<svg class="inbox-ic"[^>]*aria-hidden="true"[^>]*>[^]*?<\/svg>)<\/button>/.exec(html);
  assert.ok(play, 'play icon button');
  const share = /<button type="button" class="hv-btn btn-icon btn-sm inbox-action inbox-share" data-action="inboxShare" data-arg0="plaud" data-arg1="f_2" aria-label="Share" title="[^"]+" data-arg-types="s,s,t" aria-haspopup="menu" aria-expanded="false"><svg class="inbox-ic"/.exec(html);
  assert.ok(share, 'share icon button keeps the menu attributes');
  assert.match(html, /<button type="button" class="hv-btn btn-ghost btn-sm inbox-action inbox-action-text" data-action="inboxToggleTranscript" data-arg0="plaud" data-arg1="f_2" aria-expanded="false" title="Show the transcript"><svg class="inbox-ic"[^]*?<\/svg>Transcript<\/button>/);
  assert.match(html, /<div class="inbox-actions">/);
  assert.doesNotMatch(html, /kc-actions mt-2/);
  // Every icon-only control names itself.
  for (const m of html.matchAll(/<(?:button|a) [^>]*btn-icon[^>]*>/g)) {
    assert.match(m[0], /aria-label="[^"]+"/, m[0]);
    assert.match(m[0], /title="[^"]+"/, m[0]);
  }
  // Delete (every kind, #18694) is the trash icon, last, after a separator.
  const own = vm.runInContext('inboxItemHtml(' + JSON.stringify({ source: 'hive-recording', id: 'r1', title: 'note', summary: 'x', has_transcript: true, recorded_by: 'ana' }) + ')', ctx);
  assert.match(own, /<span class="inbox-actions-sep" role="separator"><\/span><button type="button" class="hv-btn btn-icon btn-sm inbox-action inbox-action-danger inbox-delete" data-action="inboxDeleteItem" data-arg0="hive-recording" data-arg1="r1" aria-label="Delete" title="[^"]+"><svg class="inbox-ic"[^]*?<\/svg><\/button><\/div>/);
  assert.doesNotMatch(own, /🗑 Delete/);
  const plaudRow = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  assert.match(plaudRow, /data-action="inboxDeleteItem" data-arg0="plaud" data-arg1="f_2" aria-label="Delete"/, 'every item kind has the slot');
});

test('a trailing recording id leaves the title for the meta line, the full title is the tooltip', () => {
  const ctx = makeContext('owner');
  // Objects cross the vm boundary with another Object prototype: compare by value.
  const parts = s => JSON.parse(JSON.stringify(vm.runInContext('inboxTitleParts(' + JSON.stringify(s) + ')', ctx)));
  assert.deepEqual(parts('Meeting notes: google meet meeting 2026-11-17 (mtg-078395cc1f76)'), { title: 'Meeting notes: google meet meeting 2026-11-17', id: 'mtg-078395cc1f76', full: 'Meeting notes: google meet meeting 2026-11-17 (mtg-078395cc1f76)' });
  assert.deepEqual(parts('Standup (notes)'), { title: 'Standup (notes)', id: '', full: 'Standup (notes)' });
  const html = vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, readyItem, { source: 'hive-recording', title: 'Meeting notes: teams meeting 2026-11-17 (mtg-814d078a559b)', recorded_at: '2026-11-17T08:00:00Z' })) + ')', ctx);
  assert.match(html, /<div class="kb-fact-title inbox-title" title="Meeting notes: teams meeting 2026-11-17 \(mtg-814d078a559b\)">Meeting notes: teams meeting 2026-11-17<\/div>/);
  assert.match(html, /hive-recording · recorded 2h ago · 2m 5s · <span class="inbox-meta-id" title="Recording id">mtg-814d078a559b<\/span>/);
  assert.match(html, /aria-label="Select Meeting notes: teams meeting 2026-11-17"/);
});

test('summary renders inline and collapses past the threshold with a more toggle', () => {
  const ctx = makeContext('owner');
  let html = vm.runInContext('inboxItemHtml(' + JSON.stringify(readyItem) + ')', ctx);
  assert.match(html, /<div class="inbox-summary">Key points: ship it\.<\/div>/);
  assert.doesNotMatch(html, /inbox-more/);
  const long = Object.assign({}, readyItem, { summary: 'x'.repeat(300) });
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(long) + ')', ctx);
  assert.match(html, /inbox-summary is-collapsed/);
  assert.match(html, /data-action="inboxToggleSummary"[^>]*aria-expanded="false">Show more</);
  vm.runInContext('_inboxExpanded.add("plaud/f_2")', ctx);
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(long) + ')', ctx);
  assert.doesNotMatch(html, /is-collapsed/);
  assert.match(html, /aria-expanded="true">Show less</);
  // Four short lines are also more than the three-line clamp.
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, readyItem, { id: 'f_3', summary: 'a\nb\nc\nd' })) + ')', ctx);
  assert.match(html, /inbox-summary is-collapsed/);
});

test('an uploaded document\'s Markdown preview is plain text: no heading marks, ticks or link syntax, mentions stay plain', () => {
  const ctx = makeContext('owner');
  const md = 'Held RFC. It is labelled `hold` so no agent picks it up.\n\nReviewers requested: @ana @bo - read for direction first.\n\n## Summary\n\n- **Bold** point with [a link](https://example.org)\n> quoted\n```\ncode\n```';
  assert.equal(vm.runInContext('inboxMarkdownToText(' + JSON.stringify(md) + ')', ctx), 'Held RFC. It is labelled hold so no agent picks it up.\n\nReviewers requested: @ana @bo - read for direction first.\n\nSummary\n\nBold point with a link\nquoted\ncode');
  const doc = { source: 'upload', id: 'u1', title: 'rfc-v7-teams', origin: 'upload · rfc-v7-teams.md', upload_kind: 'markdown', summary: md };
  const html = vm.runInContext('inboxItemHtml(' + JSON.stringify(doc) + ')', ctx);
  assert.match(html, /<div class="inbox-summary is-collapsed">Held RFC\. It is labelled hold/);
  assert.doesNotMatch(html, /## Summary|`hold`|\*\*Bold\*\*|\]\(https/);
  assert.match(html, /@ana @bo/, 'mentions are plain text');
  // A recording's summary is shown as it came.
  assert.match(vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, readyItem, { summary: '## not markdown' })) + ')', ctx), /## not markdown/);
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
  assert.doesNotMatch(html, />Retry RFC…</, 'Retry RFC… lives in the Share menu');

  const delegated = Object.assign({}, readyItem, { routes: { spek_rfc: { status: 'delegated', repo: 'o/r', note: 'handed to agent builder to draft with spek and publish to o/r' } } });
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(delegated) + ')', ctx);
  assert.match(html, /data-status="ok"[^>]*>RFC handed to agent</);
  assert.match(html, /muted-xs">handed to agent builder to draft with spek and publish to o\/r</);
  assert.doesNotMatch(html, /role="alert"/);
});

test('transcribe-then-share: the progress bar names the share, a failed intent shows its reason', () => {
  const ctx = makeContext('read-write');
  const running = Object.assign({}, pendingItem, {
    transcription: { status: 'running', phase: 'transcribing', progress: 0.4, started_at: '2026-11-16T10:00:00Z', eta_seconds: 240 },
    share_intents: { spek_rfc: { status: 'waiting', actor: 'rw@example.com' } },
  });
  let html = vm.runInContext('inboxItemHtml(' + JSON.stringify(running) + ')', ctx);
  assert.match(html, /Transcribing, then → RFC · 40%/);
  const failed = Object.assign({}, pendingItem, { share_intents: { dismiss: { status: 'failed', error: 'transcription failed: provider down' } } });
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(failed) + ')', ctx);
  assert.match(html, /data-status="error"[^>]*>→ Knowledge failed</);
  assert.match(html, /role="alert">transcription failed: provider down</);
  assert.match(html, /inbox-check/, 'read-write can select items for the bulk Hive menu');
});

test('an uploaded document shows where it came from and who added it, its text, and no Play', () => {
  const ctx = makeContext('owner');
  const doc = { source: 'upload', id: '20261116t093000-0a1b2c3d', title: 'Launch plan', origin: 'upload · notes.md', added_by: 'ana@example.com', upload_kind: 'markdown', recorded_at: '2026-11-16T09:30:00Z', summary: 'Ship the drop zone first.', fact_slug: 'upload-20261116t093000-0a1b2c3d', fact_state: 'ready', layer: 'personal' };
  let html = vm.runInContext('inboxItemHtml(' + JSON.stringify(doc) + ')', ctx);
  assert.match(html, /upload · notes\.md · added 2h ago · by ana@example\.com/);
  assert.doesNotMatch(html, /data-action="inboxPlay"/, 'documents have no audio');
  assert.match(html, /data-action="inboxToggleTranscript"[^>]*title="Show the text"><svg[^]*?<\/svg>Text<\/button>/);
  assert.match(html, /· <span class="inbox-meta-state"[^>]*>In knowledge<\/span> · <a href="#" class="inbox-fact-link"/, 'state in the meta line');
  ctx._inboxTranscriptOpen.add('upload/' + doc.id);
  ctx._inboxContent['upload/' + doc.id] = { markdown: '# Launch plan\n\n<b>bold</b>' };
  html = vm.runInContext('inboxTranscriptHtml(' + JSON.stringify(doc) + ', ' + JSON.stringify('upload/' + doc.id) + ')', ctx);
  assert.match(html, /Extracted text/);
  assert.match(html, /&lt;b&gt;bold&lt;\/b&gt;/);
  // Uploaded audio keeps the player.
  assert.match(vm.runInContext('inboxItemHtml(' + JSON.stringify(Object.assign({}, doc, { upload_kind: 'audio' })) + ')', ctx), /data-action="inboxPlay"/);
});

test('share history pills sit with the other pills and read users get no link', () => {
  const withHistory = Object.assign({}, readyItem, { share_history: [{ id: 'sh-1', destination: 'rfc', actor: 'andy', status: 'done', started_at: '2026-11-16T10:00:00Z', ref: { number: 18412, url: 'https://github.com/o/r/issues/18412' } }] });
  let html = vm.runInContext('inboxItemHtml(' + JSON.stringify(withHistory) + ')', makeContext('owner'));
  assert.match(html, /<a class="kc-pill inbox-share-pill"[^>]*href="https:\/\/github\.com\/o\/r\/issues\/18412"[^>]*>→ RFC #18412 · andy · 2h ago</);
  html = vm.runInContext('inboxItemHtml(' + JSON.stringify(withHistory) + ')', makeContext('read-only'));
  assert.match(html, /<span class="kc-pill inbox-share-pill"[^>]*>→ RFC #18412/);
  assert.doesNotMatch(html, /href="https:\/\/github/);
});

test('an uploaded audio file is pending with its own note and keeps the player', () => {
  const ctx = makeContext('read-write');
  const audio = { source: 'upload', id: '20261116t093000-0a1b2c3e', title: 'standup', origin: 'upload · standup.m4a', added_by: 'ana', upload_kind: 'audio', pending: true, transcribable: true, fact_slug: 'upload-x', fact_state: 'pending' };
  const html = vm.runInContext('inboxItemHtml(' + JSON.stringify(audio) + ')', ctx);
  assert.match(html, /this audio was added to the Inbox without text/);
  assert.doesNotMatch(html, /Plaud/);
  assert.match(html, /data-action="inboxPlay"/);
  assert.match(html, /data-action="inboxToggleTranscript"[^>]*title="Show the transcript"><svg[^]*?<\/svg>Transcript<\/button>/);
});

test('every item kind gets the trash icon Delete for owner and read-write, and none for read', () => {
  const kinds = [pendingItem, { source: 'upload', id: 'u1', title: 'notes.md' }, { source: 'meeting', id: 'm1', title: 'Weekly' }];
  for (const role of ['owner', 'read-write']) {
    const ctx = makeContext(role);
    for (const it of kinds) {
      assert.match(ctx.inboxItemHtml(it), new RegExp('data-action="inboxDeleteItem" data-arg0="' + it.source + '" data-arg1="' + it.id + '" aria-label="Delete"[^>]*title="[^"]+"><svg class="inbox-ic"'), role + ' ' + it.source);
    }
  }
  const read = makeContext('read');
  for (const it of kinds) assert.doesNotMatch(read.inboxItemHtml(it), /inboxDeleteItem/, it.source);
});
