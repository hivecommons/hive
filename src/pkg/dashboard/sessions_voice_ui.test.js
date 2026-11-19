// Sessions voice notes: the browser Web Speech API dictates into the Send
// textarea as an editable preview. These tests drive the shipped code (cut out
// of the generated dashboard) with a fake SpeechRecognition: start/stop,
// interim vs final text, error toasts, role gating, the unsupported-browser
// fallback, and that a transcript is never sent on its own.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const helpersStart = html.indexOf('const SESSIONS_API');
const helpersEnd = html.indexOf('// ── Storage conveniences');
assert.ok(helpersStart >= 0 && helpersEnd > helpersStart, 'sessions helper block not found in index.html');
const voiceStart = html.indexOf('// ── Voice notes (browser Web Speech API) ──');
const voiceEnd = html.indexOf('// Registry slot: features add entries');
assert.ok(voiceStart > helpersEnd && voiceEnd > voiceStart, 'voice notes block not found in index.html');

const esc = (v) => String(v == null ? '' : v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');

class FakeRecognition {
  constructor() { FakeRecognition.instances.push(this); this.started = 0; this.stopped = 0; }
  start() { if (FakeRecognition.throwOnStart) throw new Error('busy'); this.started++; }
  stop() { this.stopped++; if (this.onend) this.onend(); }
  emit(results, resultIndex) {
    const list = results.map(([transcript, isFinal]) => Object.assign([{ transcript }], { isFinal }));
    this.onresult({ resultIndex: resultIndex || 0, results: list });
  }
  fail(code) { this.onerror({ error: code }); this.onend(); }
}
FakeRecognition.instances = [];
FakeRecognition.throwOnStart = false;

class FakeEl {
  constructor(id) { this.id = id; this.value = ''; this.textContent = ''; this.title = ''; this.attrs = {}; this.focused = 0; this.isConnected = true; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k]; }
  focus() { this.focused++; }
}

// Load the helper block plus the voice block into one sandbox with fakes for
// the dashboard globals those functions call.
function load(win) {
  const els = {};
  const document = { getElementById: (id) => els[id] || null };
  const toasts = [];
  const posts = [];
  const store = {};
  const changed = [];
  const body = html.slice(helpersStart, helpersEnd) + '\n' + html.slice(voiceStart, voiceEnd)
    + '\nreturn { sessionsVoiceCtor, sessionsCanVoice, sessionsVoiceErrorText, sessionsVoiceJoin, sessionsVoiceResults, sessionsVoiceRecognizer, sessionsVoiceHtml, sessionsVoiceToggle, sessionsVoiceStop,'
    + ' setData: (d) => { _sessionsData = d; }, setRole: (r) => { _sessionsRole = r; }, current: () => _sessionsVoice };';
  const fn = new Function('window', 'document', 'navigator', 'esc', 'showToast', 'sessionsInputChanged', 'sessionsStoreSet', 'sessionsPostInput', 'sessionsApi', 'fetch', body);
  const h = fn(win, document, { language: 'en-GB' }, esc,
    (msg, type) => toasts.push({ msg, type }),
    (el) => changed.push(el.value),
    (k, v) => { store[k] = v; },
    (...args) => { posts.push(args); return Promise.resolve({}); },
    (...args) => { posts.push(args); return Promise.resolve({}); },
    (...args) => { posts.push(args); return Promise.resolve({}); });
  const id = 'abc';
  for (const name of ['sessions-input-text-', 'sessions-voice-btn-', 'sessions-voice-interim-']) els[name + id] = new FakeEl(name + id);
  h.setData({ sessions: [{ id, name: 'demo', status: 'running' }], limits: {} });
  h.setRole('owner');
  return { h, els, id, toasts, posts, store, changed, box: els['sessions-input-text-' + id], btn: els['sessions-voice-btn-' + id], interim: els['sessions-voice-interim-' + id] };
}

const supported = () => ({ webkitSpeechRecognition: FakeRecognition });

test('the constructor is detected from either the standard or the webkit name', () => {
  const { h } = load({});
  assert.strictEqual(h.sessionsVoiceCtor({ SpeechRecognition: FakeRecognition }), FakeRecognition);
  assert.strictEqual(h.sessionsVoiceCtor({ webkitSpeechRecognition: FakeRecognition }), FakeRecognition);
  assert.strictEqual(h.sessionsVoiceCtor({}), null);
  assert.strictEqual(h.sessionsVoiceCtor(null), null);
});

test('only the roles that can Send get the mic, and only on a running session in a supporting browser', () => {
  const { h } = load({});
  const run = { id: 'a', status: 'running' };
  for (const role of ['owner', 'read-write']) assert.strictEqual(h.sessionsCanVoice(role, run, supported()), true, role);
  for (const role of ['read', 'merger', '', undefined]) assert.strictEqual(h.sessionsCanVoice(role, run, supported()), false, String(role));
  assert.strictEqual(h.sessionsCanVoice('owner', { id: 'a', status: 'ended' }, supported()), false);
  assert.strictEqual(h.sessionsCanVoice('owner', run, {}), false, 'no Web Speech API, no mic');
  // The mic is rendered inside the input bar, which is itself gated on sessionsCanSend.
  const bar = html.slice(html.indexOf('function sessionsInputBarHtml('), html.indexOf('function sessionsRefreshQueueUI('));
  assert.match(bar, /if \(!sessionsCanSend\(_sessionsRole, s\)\) return '';/);
  assert.match(bar, /sessionsVoiceHtml\(s, window,/);
});

test('mic button is accessible, shows the vendor notice the first time, and is hidden with a reason when unsupported', () => {
  const { h } = load({});
  const s = { id: 'a"b', status: 'running' };
  const on = h.sessionsVoiceHtml(s, supported(), false);
  assert.match(on, /<button type="button"[^>]*class="[^"]*sessions-voice-btn[^"]*"[^>]*data-action="sessionsVoiceToggle"[^>]*aria-pressed="false"/);
  assert.doesNotMatch(on, /\shidden[\s>]/);
  assert.match(on, /class="sessions-voice-interim"[^>]*aria-live="polite"/);
  assert.match(on, /class="sessions-voice-notice"[^>]*>[^<]*browser vendor&#39;s speech service[^<]*not by Hive/);
  assert.match(on, /id="sessions-voice-btn-a&quot;b"/, 'the session id is escaped');
  const seen = h.sessionsVoiceHtml(s, supported(), true);
  assert.doesNotMatch(seen, /sessions-voice-notice/, 'the notice is shown only until the first dictation');
  const off = h.sessionsVoiceHtml(s, {}, false);
  assert.match(off, /<button[^>]*title="Voice notes are unavailable: this browser has no Web Speech API[^"]*"[^>]*hidden>/);
  assert.doesNotMatch(off, /sessions-voice-interim|sessions-voice-notice/);
});

test('results split into new final text and the live interim tail', () => {
  const { h } = load({});
  const mk = (items, resultIndex) => ({ resultIndex, results: items.map(([t, f]) => Object.assign([{ transcript: t }], { isFinal: f })) });
  assert.deepStrictEqual(h.sessionsVoiceResults(mk([['fix the', true], [' flaky test', false]], 0)), { final: 'fix the', interim: 'flaky test' });
  assert.deepStrictEqual(h.sessionsVoiceResults(mk([['old', true], ['new', true]], 1)), { final: 'new', interim: '' }, 'already-delivered results are skipped');
  assert.deepStrictEqual(h.sessionsVoiceResults(null), { final: '', interim: '' });
  assert.strictEqual(h.sessionsVoiceJoin('hello', 'world'), 'hello world');
  assert.strictEqual(h.sessionsVoiceJoin('hello\n', ' world '), 'hello\nworld');
  assert.strictEqual(h.sessionsVoiceJoin('', 'x'), 'x');
  assert.strictEqual(h.sessionsVoiceJoin('keep', '  '), 'keep');
});

test('the recognizer follows navigator.language, shows interim text live and never runs continuously', () => {
  FakeRecognition.instances.length = 0;
  const t = load(supported());
  t.h.sessionsVoiceToggle(t.id);
  const rec = FakeRecognition.instances[0];
  assert.ok(rec, 'a recognizer was created');
  assert.strictEqual(rec.started, 1);
  assert.strictEqual(rec.lang, 'en-GB');
  assert.strictEqual(rec.interimResults, true);
  assert.strictEqual(rec.continuous, false, 'silence ends the note');
  assert.strictEqual(t.btn.getAttribute('aria-pressed'), 'true');
  assert.strictEqual(t.btn.textContent, 'Stop');
  rec.emit([['open a', false]]);
  assert.strictEqual(t.interim.textContent, 'open a');
  assert.strictEqual(t.box.value, '', 'interim text is not written into the textarea');
  rec.stop();
});

test('final transcript lands in the textarea as an editable preview and is never auto-sent', async () => {
  FakeRecognition.instances.length = 0;
  const t = load(supported());
  t.box.value = 'Context:';
  t.h.sessionsVoiceToggle(t.id);
  const rec = FakeRecognition.instances[0];
  rec.emit([['open a pull request', true]]);
  assert.strictEqual(t.box.value, 'Context: open a pull request');
  assert.deepStrictEqual(t.changed, ['Context: open a pull request'], 'the byte counter is refreshed');
  rec.stop();
  await new Promise((r) => setImmediate(r));
  assert.strictEqual(t.posts.length, 0, 'nothing was posted to /input or anywhere else');
  assert.strictEqual(t.btn.getAttribute('aria-pressed'), 'false');
  assert.strictEqual(t.btn.textContent, 'Voice');
  assert.strictEqual(t.interim.textContent, '');
  assert.ok(t.box.focused > 0, 'focus returns to the textarea for editing');
  assert.strictEqual(t.h.current(), null);
  assert.strictEqual(t.store['hive.sessions.voiceNoticeSeen'], '1', 'the first-use notice is acknowledged');
});

test('clicking the mic again stops the note', () => {
  FakeRecognition.instances.length = 0;
  const t = load(supported());
  t.h.sessionsVoiceToggle(t.id);
  const rec = FakeRecognition.instances[0];
  t.h.sessionsVoiceToggle(t.id);
  assert.strictEqual(rec.stopped, 1);
  assert.strictEqual(FakeRecognition.instances.length, 1, 'the second click does not start a new note');
  assert.strictEqual(t.btn.getAttribute('aria-pressed'), 'false');
  assert.strictEqual(t.h.current(), null);
});

test('recognition errors surface as error toasts', () => {
  for (const [code, re] of [['not-allowed', /permission was denied/], ['no-speech', /No speech was heard/], ['network', /could not be reached/], ['aborted', /stopped/]]) {
    FakeRecognition.instances.length = 0;
    const t = load(supported());
    t.h.sessionsVoiceToggle(t.id);
    FakeRecognition.instances[0].fail(code);
    assert.strictEqual(t.toasts.length, 1, code);
    assert.strictEqual(t.toasts[0].type, 'error', code);
    assert.match(t.toasts[0].msg, re, code);
    assert.strictEqual(t.btn.getAttribute('aria-pressed'), 'false', code);
    assert.strictEqual(t.box.value, '', code);
  }
  const { h } = load({});
  assert.match(h.sessionsVoiceErrorText('weird'), /Voice note failed: weird/);
});

test('a start failure, a read-only role and an unsupported browser never leave a note running', () => {
  FakeRecognition.instances.length = 0;
  FakeRecognition.throwOnStart = true;
  let t = load(supported());
  t.h.sessionsVoiceToggle(t.id);
  FakeRecognition.throwOnStart = false;
  assert.match(t.toasts[0].msg, /Could not start the voice note: busy/);
  assert.strictEqual(t.h.current(), null);
  assert.strictEqual(t.btn.getAttribute('aria-pressed'), undefined);

  FakeRecognition.instances.length = 0;
  t = load(supported());
  t.h.setRole('read');
  t.h.sessionsVoiceToggle(t.id);
  assert.strictEqual(FakeRecognition.instances.length, 0, 'the read role cannot dictate');

  t = load({});
  t.h.setRole('owner');
  t.h.sessionsVoiceToggle(t.id);
  assert.strictEqual(FakeRecognition.instances.length, 0);
  assert.match(t.toasts[0].msg, /no Web Speech API/);
});

test('switching tabs stops a running note', () => {
  const sel = html.slice(html.indexOf('function sessionsSelect('), html.indexOf('function sessionsTabKey('));
  assert.match(sel, /sessionsVoiceStop\(\);/);
});
