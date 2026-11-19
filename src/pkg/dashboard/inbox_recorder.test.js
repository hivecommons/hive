// Inbox ● Record: the recorder state machine (fake MediaRecorder and
// getUserMedia), the chunk upload queue and its resume after failures, the
// consent notice required before every recording, and who sees the control.
// Functions are extracted from the generated page and run in a sandbox.
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
// Every single-line `const INBOX_REC_… = …;` declaration of the recorder.
const consts = () => html.split('\n').filter(l => /^\s*const INBOX_REC_[A-Z_]+ = .*;\s*$/.test(l)).join('\n');
const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

const FNS = ['inboxRecSupport', 'inboxRecRoleCanRecord', 'inboxRecAvailable', 'inboxRecFmtClock', 'inboxRecLevel', 'inboxRecSplit',
  'inboxRecConsentText', 'inboxRecDefaultTitle', 'inboxRecConsentHtml', 'inboxRecMemoryQueue', 'inboxRecUploader', 'inboxRecorderMachine',
  'inboxRecResumePending', 'inboxRecUploadText', 'inboxRecMeterHtml', 'inboxRecPanelHtml', 'inboxRecTabSupport', 'inboxRecSourcesHtml', 'inboxRecCaptureTab',
  'inboxRecDoneOutcome', 'inboxRecDoneText'];
const lib = new Function('escapeHtml', consts() + '\n' + FNS.map(fnSrc).join('\n') + '\nreturn {' + FNS.join(',') + ', CONSENT: INBOX_REC_CONSENT_VERSION, ACK: INBOX_REC_CONSENT_ACK };')(escapeHtml);

const flush = async (n = 20) => { for (let i = 0; i < n; i++) await new Promise(r => setImmediate(r)); };

// FakeRecorder mimics MediaRecorder: emit() delivers a timeslice chunk,
// stop() delivers the last one and fires onstop.
class FakeRecorder {
  constructor(stream, opts) { this.stream = stream; this.opts = opts; this.state = 'inactive'; this.mimeType = (opts && opts.mimeType) || 'audio/webm'; FakeRecorder.last = this; }
  start(slice) { this.slice = slice; this.state = 'recording'; }
  pause() { this.state = 'paused'; }
  resume() { this.state = 'recording'; }
  emit(bytes) { this.ondataavailable({ data: blob(bytes) }); }
  stop() { this.state = 'inactive'; this.ondataavailable({ data: blob('last') }); setImmediate(() => this.onstop()); }
}
FakeRecorder.isTypeSupported = t => t === 'audio/webm;codecs=opus';
const blob = s => ({ size: s.length, text: s, slice: (a, b) => blob(s.slice(a, b)) });
const fakeTrack = () => { const t = { stopped: false, stop() { t.stopped = true; } }; return t; };
const fakeStream = () => { const track = fakeTrack(); return { track, getTracks: () => [track], getAudioTracks: () => [track] }; };

// fakeHive answers the recording API in memory; fail(n) makes the next n
// chunk uploads fail with a network error.
const fakeHive = () => {
  const hive = { calls: [], stored: [], finalized: null, failNext: 0, aborted: false };
  hive.fetch = async (url, opts = {}) => {
    hive.calls.push({ url, method: opts.method || 'GET' });
    const json = b => ({ ok: true, status: 200, json: async () => b });
    if (url === '/api/inbox/recordings' && opts.method === 'POST') {
      hive.start = JSON.parse(opts.body);
      return { ok: true, status: 201, json: async () => ({ id: 'r1', max_chunk_bytes: 524288, max_seconds: 10800, retention_days: 5, transcribes: true }) };
    }
    const chunk = /\/chunks\?seq=(\d+)$/.exec(url);
    if (chunk) {
      if (hive.failNext > 0) { hive.failNext--; throw new Error('network down'); }
      const seq = Number(chunk[1]);
      if (seq < hive.stored.length) return json({ duplicate: true, next_seq: hive.stored.length });
      if (seq !== hive.stored.length) return { ok: false, status: 409, json: async () => ({ error: 'gap', expected_seq: hive.stored.length }) };
      hive.stored.push(opts.body.text);
      return json({ next_seq: hive.stored.length });
    }
    if (/\/finalize$/.test(url)) { hive.finalized = JSON.parse(opts.body); return json({ state: 'finalized' }); }
    if (opts.method === 'DELETE') { hive.aborted = true; return json({ aborted: true }); }
    return { ok: false, status: 404, json: async () => ({ error: 'nope' }) };
  };
  return hive;
};

const machine = (over = {}) => {
  const hive = fakeHive();
  const timers = [];
  let clock = 1000;
  const mic = fakeStream();
  const env = Object.assign({
    navigator: { mediaDevices: { getUserMedia: async () => mic } },
    MediaRecorder: FakeRecorder, fetch: hive.fetch, queue: lib.inboxRecMemoryQueue(), now: () => clock,
    setTimeout: (f, ms) => { timers.push({ f, ms }); return timers.length; }, clearTimeout: () => {}, onChange: () => {},
  }, over);
  const m = lib.inboxRecorderMachine(env);
  return { m, hive, env, timers, mic, tick: ms => { clock += ms; } };
};

test('recorder state machine: consent, chunks in order, pause excluded from duration, finalize', async () => {
  const { m, hive, env, tick, mic } = machine();
  await assert.rejects(m.start({ title: 'Standup', mime: 'audio/webm;codecs=opus' }), /consent/);
  assert.strictEqual(m.state, 'idle');
  await m.start({ title: 'Standup', mime: 'audio/webm;codecs=opus', consentVersion: lib.CONSENT });
  assert.strictEqual(m.state, 'recording');
  assert.deepStrictEqual(hive.start, { title: 'Standup', mime: 'audio/webm;codecs=opus', consent_version: lib.CONSENT, includes_tab_audio: false });
  assert.strictEqual(FakeRecorder.last.slice, 10000, 'timeslice 10 s');
  assert.strictEqual(FakeRecorder.last.opts.audioBitsPerSecond, 32000);
  FakeRecorder.last.emit('c0');
  tick(10000);
  FakeRecorder.last.emit('c1');
  await flush();
  assert.deepStrictEqual(hive.stored, ['c0', 'c1']);
  m.pause();
  assert.strictEqual(m.state, 'paused');
  tick(60000);
  m.resume();
  tick(5000);
  m.stop();
  await flush();
  assert.strictEqual(m.state, 'done');
  assert.deepStrictEqual(hive.stored, ['c0', 'c1', 'last']);
  assert.deepStrictEqual(hive.finalized, { duration_ms: 15000 }, 'the paused minute is not counted');
  assert.ok(mic.track.stopped, 'the microphone is released');
  assert.deepStrictEqual(await env.queue.sessions(), [], 'nothing left queued');
});

test('consent is required for every recording, never remembered', async () => {
  const { m } = machine();
  await m.start({ title: 'a', consentVersion: lib.CONSENT });
  m.stop();
  await flush();
  assert.strictEqual(m.state, 'done');
  const next = machine().m;
  await assert.rejects(next.start({ title: 'b' }), /consent/, 'a second recording needs the notice again');
  await assert.rejects(next.start({ title: 'b', consentVersion: 'older' }), /consent/);
  const body = lib.inboxRecConsentHtml({ title: 'x', retentionDays: 5, transcribes: true });
  assert.match(body, /id="inbox-rec-ack" data-change-action="inboxRecConsentToggle">/, 'the checkbox starts unticked');
  assert.match(body, /id="inbox-rec-start" data-action="inboxRecConfirmStart" disabled aria-disabled="true"/, 'Start is disabled until ticked');
  assert.ok(body.includes(escapeHtml(lib.ACK)));
  const src = fnSrc('inboxRecConfirmStart') + fnSrc('inboxRecOpen');
  assert.doesNotMatch(src, /localStorage|sessionStorage/, 'consent is not stored');
});

test('consent text: responsibility, retention, transcription off hint, tab participants', () => {
  const mic = lib.inboxRecConsentText({ retentionDays: 5, transcribes: false }).join(' ');
  assert.match(mic, /your responsibility/);
  assert.match(mic, /kept on this hive for 5 days/);
  assert.match(mic, /Transcription is off on this hive/);
  assert.doesNotMatch(mic, /remote participants/);
  const tab = lib.inboxRecConsentText({ tab: true, retentionDays: 1, transcribes: true }).join(' ');
  assert.match(tab, /remote participants of that meeting too: they must have been told/);
  assert.match(tab, /1 day and/);
  assert.match(tab, /transcribes the recording automatically/);
});

test('upload queue resumes after a network failure and keeps order', async () => {
  const { m, hive, timers } = machine();
  await m.start({ title: 't', consentVersion: lib.CONSENT });
  hive.failNext = 2;
  FakeRecorder.last.emit('a');
  FakeRecorder.last.emit('b');
  await flush();
  assert.deepStrictEqual(hive.stored, []);
  assert.strictEqual(m.uploader.state.failing, true);
  assert.strictEqual(m.uploader.state.pending, 2);
  assert.match(lib.inboxRecUploadText(m.uploader.state), /2 chunks waiting to upload \(connection problem; retrying in 2 s\)/);
  timers.shift().f();
  await flush();
  assert.strictEqual(timers[0].ms, 4000, 'backoff doubles');
  timers.shift().f();
  await flush();
  assert.deepStrictEqual(hive.stored, ['a', 'b']);
  assert.strictEqual(lib.inboxRecUploadText(m.uploader.state), 'All chunks saved on the hive');
});

test('a chunk the hive already stored (lost ack) leaves the queue on 409 expected_seq', async () => {
  const hive = fakeHive();
  hive.stored = ['x0', 'x1'];
  const q = lib.inboxRecMemoryQueue();
  await q.putChunk('r1', 0, blob('x0'));
  await q.putChunk('r1', 1, blob('x1'));
  await q.putChunk('r1', 2, blob('x2'));
  const fetchSeen = [];
  const up = lib.inboxRecUploader({ id: 'r1', queue: q, fetch: async (u, o) => { fetchSeen.push(u); if (/seq=1$/.test(u)) return { ok: false, status: 409, json: async () => ({ expected_seq: 2 }) }; return hive.fetch(u, o); }, setTimeout: () => 0, clearTimeout: () => {} });
  await up.pump();
  assert.deepStrictEqual(hive.stored, ['x0', 'x1', 'x2']);
  assert.deepStrictEqual(await q.chunks('r1'), []);
  assert.strictEqual(up.state.fatal, '');
});

test('a refusal that is not retryable stops the upload with the reason', async () => {
  const q = lib.inboxRecMemoryQueue();
  await q.putChunk('r1', 0, blob('x'));
  const up = lib.inboxRecUploader({ id: 'r1', queue: q, fetch: async () => ({ ok: false, status: 507, json: async () => ({ error: 'storage is full' }) }), setTimeout: () => 0, clearTimeout: () => {} });
  await up.pump();
  assert.strictEqual(up.state.fatal, 'storage is full');
  assert.match(lib.inboxRecUploadText(up.state), /Upload stopped: storage is full/);
  assert.strictEqual((await q.chunks('r1')).length, 1, 'the chunk is kept');
});

test('a reload resumes queued chunks and finalizes the recording', async () => {
  const hive = fakeHive();
  const q = lib.inboxRecMemoryQueue();
  await q.putSession({ id: 'r1', title: 't', stopped: false, durationMs: 42000 });
  await q.putChunk('r1', 0, blob('p0'));
  await q.putChunk('r1', 1, blob('p1'));
  const done = await lib.inboxRecResumePending({ queue: q, fetch: hive.fetch, setTimeout: () => 0, clearTimeout: () => {} });
  assert.deepStrictEqual(done, ['r1']);
  assert.deepStrictEqual(hive.stored, ['p0', 'p1']);
  assert.deepStrictEqual(hive.finalized, { duration_ms: 42000 });
  assert.deepStrictEqual(await q.sessions(), []);
});

test('microphone refusal and a refused start end in a readable error', async () => {
  const denied = machine({ navigator: { mediaDevices: { getUserMedia: async () => { const e = new Error('no'); e.name = 'NotAllowedError'; throw e; } } } });
  await denied.m.start({ title: 't', consentVersion: lib.CONSENT });
  assert.strictEqual(denied.m.state, 'error');
  assert.match(denied.m.error, /Microphone permission was refused/);
  const { m, mic } = machine({ fetch: async () => ({ ok: false, status: 403, json: async () => ({ error: 'owner or read-write access required' }) }) });
  await m.start({ title: 't', consentVersion: lib.CONSENT });
  assert.strictEqual(m.state, 'error');
  assert.strictEqual(m.error, 'owner or read-write access required');
  assert.ok(mic.track.stopped, 'the microphone is released after a refused start');
});

test('discard aborts upstream and clears the queue', async () => {
  const { m, hive, env } = machine();
  await m.start({ title: 't', consentVersion: lib.CONSENT });
  FakeRecorder.last.emit('a');
  await flush();
  await m.abort();
  assert.strictEqual(m.state, 'idle');
  assert.ok(hive.aborted);
  assert.deepStrictEqual(await env.queue.sessions(), []);
});

test('the panel shows the timer, the mic meter, the controls and the upload status', async () => {
  const { m, tick } = machine();
  await m.start({ title: 'Design <review>', consentVersion: lib.CONSENT });
  tick(65000);
  let out = lib.inboxRecPanelHtml(m);
  assert.match(out, /Design &lt;review&gt;/);
  assert.match(out, /data-inbox-rec-time>1:05</);
  assert.match(out, /data-inbox-rec-meter="mic"/);
  assert.match(out, /aria-label="Microphone level"/);
  assert.match(out, /data-action="inboxRecPause"/);
  assert.match(out, /data-action="inboxRecStop"/);
  assert.match(out, /Keep this screen open/);
  m.pause();
  out = lib.inboxRecPanelHtml(m);
  assert.match(out, /data-action="inboxRecResume"/);
  assert.strictEqual(lib.inboxRecPanelHtml({ state: 'idle' }), '');
});

test('who sees ● Record: owner and read-write, when enabled and the browser can record', () => {
  const support = lib.inboxRecSupport({ MediaRecorder: FakeRecorder, navigator: { mediaDevices: { getUserMedia() {} } } });
  assert.deepStrictEqual(support, { record: true, reason: '', mime: 'audio/webm;codecs=opus' });
  const on = { recording: { enabled: true } };
  for (const [role, want] of [['owner', true], ['read-write', true], ['read', false], ['read-only', false], ['', false]]) {
    assert.strictEqual(lib.inboxRecAvailable(on, role, support), want, role);
  }
  assert.strictEqual(lib.inboxRecAvailable({ recording: { enabled: false } }, 'owner', support), false);
  assert.strictEqual(lib.inboxRecAvailable(on, 'owner', lib.inboxRecSupport({ navigator: {} })), false, 'no MediaRecorder');
  assert.match(lib.inboxRecSupport({ navigator: {} }).reason, /MediaRecorder/);
  assert.match(lib.inboxRecSupport({ MediaRecorder: FakeRecorder, navigator: {} }).reason, /microphone/);
  const safari = function () {};
  safari.isTypeSupported = t => t === 'audio/mp4';
  assert.strictEqual(lib.inboxRecSupport({ MediaRecorder: safari, navigator: { mediaDevices: { getUserMedia() {} } } }).mime, 'audio/mp4');
});

test('helpers: clock, level, chunk split', () => {
  assert.strictEqual(lib.inboxRecFmtClock(0), '0:00');
  assert.strictEqual(lib.inboxRecFmtClock(3725000), '1:02:05');
  assert.strictEqual(lib.inboxRecLevel(new Uint8Array(8).fill(128)), 0);
  assert.ok(lib.inboxRecLevel(Uint8Array.from([0, 255, 0, 255])) > 0.9);
  const parts = lib.inboxRecSplit(blob('abcdefghij'), 4);
  assert.deepStrictEqual(parts.map(p => p.text), ['abcd', 'efgh', 'ij']);
  assert.deepStrictEqual(lib.inboxRecSplit(blob(''), 4), []);
  assert.match(lib.inboxRecDefaultTitle(Date.UTC(2026, 10, 16, 9, 5)), /^Recording 2026-11-1\d \d\d:\d\d$/);
});

test('resume skips a recording another tab is still recording', async () => {
  const hive = fakeHive();
  const q = lib.inboxRecMemoryQueue();
  await q.putSession({ id: 'live', title: 't', stopped: false });
  await q.putChunk('live', 0, blob('l0'));
  const done = await lib.inboxRecResumePending({ queue: q, fetch: hive.fetch, setTimeout: () => 0, clearTimeout: () => {}, liveIds: async () => ['live'] });
  assert.deepStrictEqual(done, []);
  assert.deepStrictEqual(hive.stored, []);
  assert.strictEqual((await q.sessions()).length, 1, 'left for the tab that records it');
});

test('an earlier recording still open on the hive can be finished from the error', async () => {
  const { m } = machine({ fetch: async () => ({ ok: false, status: 409, json: async () => ({ error: 'you already have a recording in progress (r0)', active_id: 'r0' }) }) });
  await m.start({ title: 't', consentVersion: lib.CONSENT });
  assert.strictEqual(m.state, 'error');
  assert.match(lib.inboxRecPanelHtml(m), /data-action="inboxRecFinishEarlier" data-arg0="r0"/);
});

test('a live recorder holds a lock and releases it when done', async () => {
  const held = [];
  const { m } = machine({ holdLock: name => { held.push(name); return () => held.splice(held.indexOf(name), 1); } });
  await m.start({ title: 't', consentVersion: lib.CONSENT });
  assert.deepStrictEqual(held, ['hive-inbox-recording-r1']);
  m.stop();
  await flush();
  assert.strictEqual(m.state, 'done');
  assert.deepStrictEqual(held, []);
});

// ── Tab audio (mic + a meeting tab, Chromium only) ──
const CHROME_UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36';
const SAFARI_UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.1 Safari/605.1.15';
const FIREFOX_UA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 15.7; rv:145.0) Gecko/20100101 Firefox/145.0';
const ANDROID_UA = 'Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Mobile Safari/537.36';
const withDisplay = ua => ({ navigator: { userAgent: ua, mediaDevices: { getUserMedia() {}, getDisplayMedia() {} } } });

test('tab audio is offered on desktop Chromium only, and explained elsewhere', () => {
  assert.deepStrictEqual(lib.inboxRecTabSupport(withDisplay(CHROME_UA)), { tab: true, reason: '' });
  assert.strictEqual(lib.inboxRecTabSupport({ navigator: { userAgent: SAFARI_UA, userAgentData: undefined, mediaDevices: { getDisplayMedia() {} } } }).tab, false);
  for (const ua of [SAFARI_UA, FIREFOX_UA, ANDROID_UA]) {
    const s = lib.inboxRecTabSupport(withDisplay(ua));
    assert.strictEqual(s.tab, false, ua);
    assert.ok(s.reason.length > 0, 'a reason is given for ' + ua);
  }
  assert.match(lib.inboxRecTabSupport(withDisplay(FIREFOX_UA)).reason, /Chrome or Edge on a computer/);
  assert.match(lib.inboxRecTabSupport({ navigator: { userAgent: CHROME_UA, mediaDevices: {} } }).reason, /cannot share a tab/);
  const brands = { navigator: { userAgent: '', userAgentData: { brands: [{ brand: 'Microsoft Edge' }], mobile: false }, mediaDevices: { getDisplayMedia() {} } } };
  assert.strictEqual(lib.inboxRecTabSupport(brands).tab, true);

  const hidden = lib.inboxRecSourcesHtml({ tab: false, reason: 'needs Chrome' });
  assert.doesNotMatch(hidden, /value="tab"/, 'no tab option where unsupported');
  assert.match(hidden, /data-inbox-rec-tab-unsupported>needs Chrome</);
  const shown = lib.inboxRecSourcesHtml({ tab: true, reason: '' });
  assert.match(shown, /value="tab"/);
  assert.match(shown, /value="mic" data-change-action="inboxRecSourceChange" checked/);
});

// fakeDisplay returns a getDisplayMedia stream with video and (optionally)
// audio tracks, recording what happened to them.
const fakeDisplay = (withAudio) => {
  const video = Object.assign(fakeTrack(), { kind: 'video' });
  const audio = Object.assign(fakeTrack(), { kind: 'audio' });
  const tracks = withAudio ? [video, audio] : [video];
  const stream = {
    video, audio, asked: null,
    getTracks: () => tracks.slice(), getVideoTracks: () => tracks.filter(t => t.kind === 'video'), getAudioTracks: () => tracks.filter(t => t.kind === 'audio'),
    removeTrack: t => { const i = tracks.indexOf(t); if (i >= 0) tracks.splice(i, 1); },
  };
  return stream;
};
class FakeAudioContext {
  constructor() { this.connected = []; FakeAudioContext.last = this; this.mixed = { mixed: true, getTracks: () => [] }; }
  createMediaStreamDestination() { return { stream: this.mixed }; }
  createMediaStreamSource(s) { const ctx = this; return { connect: d => ctx.connected.push([s, d]) }; }
  close() { this.closed = true; }
}

test('mic + tab: the tab is requested with audio and video, video dropped, both mixed into the recorder', async () => {
  const display = fakeDisplay(true);
  let asked = null;
  const mic = fakeStream();
  let analysed = null;
  const { m, hive } = machine({
    navigator: { mediaDevices: { getUserMedia: async () => mic, getDisplayMedia: async (c) => { asked = c; return display; } } },
    AudioContext: FakeAudioContext, captureTab: lib.inboxRecCaptureTab, analyse: (mm, captured) => { analysed = captured; return { mic: {}, tab: {} }; },
  });
  await m.start({ title: 'Call', consentVersion: lib.CONSENT, tab: true });
  assert.strictEqual(m.state, 'recording');
  assert.deepStrictEqual(asked, { audio: true, video: true });
  assert.ok(display.video.stopped, 'the video track is stopped');
  assert.deepStrictEqual(display.getVideoTracks(), [], 'and removed');
  assert.strictEqual(FakeRecorder.last.stream, FakeAudioContext.last.mixed, 'the recorder records the mix');
  assert.deepStrictEqual(FakeAudioContext.last.connected.map(c => c[0]), [mic, display], 'mic and tab both feed the mix');
  assert.strictEqual(hive.start.includes_tab_audio, true);
  assert.strictEqual(analysed.tab, display);
  assert.match(lib.inboxRecPanelHtml(m), /data-inbox-rec-meter="mic"[\s\S]*data-inbox-rec-meter="tab"/, 'two level meters');
  m.stop();
  await flush();
  assert.ok(display.audio.stopped, 'the tab capture is released on stop');
});

test('a shared tab without audio warns and records the microphone only', async () => {
  const mic = fakeStream();
  const { m, hive } = machine({
    navigator: { mediaDevices: { getUserMedia: async () => mic, getDisplayMedia: async () => fakeDisplay(false) } },
    AudioContext: FakeAudioContext, captureTab: lib.inboxRecCaptureTab,
  });
  await m.start({ title: 'Call', consentVersion: lib.CONSENT, tab: true });
  assert.strictEqual(m.state, 'recording');
  assert.match(m.warning, /no audio: share a browser tab and tick “Share tab audio”/);
  assert.strictEqual(hive.start.includes_tab_audio, false);
  assert.strictEqual(FakeRecorder.last.stream, mic);
  assert.match(lib.inboxRecPanelHtml(m), /role="alert">The shared tab has no audio/);
});

test('cancelling the tab picker records the microphone only', async () => {
  const mic = fakeStream();
  const { m, hive } = machine({
    navigator: { mediaDevices: { getUserMedia: async () => mic, getDisplayMedia: async () => { const e = new Error('cancel'); e.name = 'NotAllowedError'; throw e; } } },
    AudioContext: FakeAudioContext, captureTab: lib.inboxRecCaptureTab,
  });
  await m.start({ title: 'Call', consentVersion: lib.CONSENT, tab: true });
  assert.strictEqual(m.state, 'recording');
  assert.match(m.warning, /No tab was shared/);
  assert.strictEqual(hive.start.includes_tab_audio, false);
});

test('the saved panel follows the item: transcribed, failed, off, stalled, waiting', () => {
  const o = (it, transcribes = true, why = '', waited = false) => lib.inboxRecDoneOutcome(it, transcribes, why, waited);
  assert.deepStrictEqual(o(null), { state: 'waiting', reason: '' });
  assert.deepStrictEqual(o(null, false), { state: 'off', reason: '' });
  assert.strictEqual(o({ transcription: { status: 'running' } }).state, 'waiting');
  assert.strictEqual(o({ transcription: { status: 'done' } }).state, 'transcribed');
  assert.strictEqual(o({ fact_state: 'machine' }).state, 'transcribed');
  assert.strictEqual(o({ has_transcript: true }, false).state, 'transcribed', 'text wins over transcription being off');
  assert.deepStrictEqual(o({ transcription: { status: 'failed', reason: 'whisper crashed' } }), { state: 'failed', reason: 'whisper crashed' });
  assert.deepStrictEqual(o({ transcribe_unavailable: 'budget' }, true, 'Monthly budget used'), { state: 'failed', reason: 'Monthly budget used' });
  assert.strictEqual(o({ transcribe_unavailable: 'disabled' }).state, 'off');
  assert.strictEqual(o({}, true, '', true).state, 'stalled');
  assert.strictEqual(o({ transcription: { status: 'queued' } }, true, '', true).state, 'waiting', 'a queued job is still followed');

  assert.strictEqual(lib.inboxRecDoneText({ state: 'transcribed' }), 'Transcribed. See the item below.');
  assert.match(lib.inboxRecDoneText({ state: 'failed', reason: 'whisper crashed' }), /^Transcription failed: whisper crashed\. The recording is saved/);
  assert.match(lib.inboxRecDoneText({ state: 'waiting' }), /transcribing it now/);
  assert.match(lib.inboxRecDoneText({ state: 'off' }), /Transcription is off on this hive/);

  const done = { state: 'done', title: 'Standup', levels: {}, elapsedMs: () => 4000, outcome: { state: 'transcribed', reason: '' } };
  let out = lib.inboxRecPanelHtml(done);
  assert.match(out, /data-inbox-rec-outcome="transcribed">Transcribed\. See the item below\.</);
  assert.doesNotMatch(out, /transcribing it now/, 'the stale line is gone');
  assert.match(out, /data-action="inboxRecClose"/, 'Close stays');
  out = lib.inboxRecPanelHtml(Object.assign({}, done, { outcome: { state: 'failed', reason: 'no audio' } }));
  assert.match(out, /class="kc-error muted-xs"[^>]*data-inbox-rec-outcome="failed">Transcription failed: no audio\./);
});

test('the saved panel auto-dismisses with the toast countdown, not its own timer', () => {
  const src = fnSrc('inboxRecArmDismiss');
  assert.match(src, /toastCountdownBar\(panel, TOAST_DURATION_MS,/);
  assert.match(fnSrc('inboxRecSyncDone'), /inboxRecArmDismiss\(m\)/);
  assert.match(fnSrc('fetchInbox'), /inboxRecSyncDone\(_inboxCache\.items\)/);
});
