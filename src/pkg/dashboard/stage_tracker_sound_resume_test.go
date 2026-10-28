package dashboard

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestStageTrackerSoundResumeWiring pins the gesture/automatic split in the
// markup and constants: the ▶ Play buttons hand the dispatcher the button so
// it can show "Playing…", the delegated click dispatcher reaches
// window[action] for them, and a gesture waits longer for resume() than an
// automatic play does.
func TestStageTrackerSoundResumeWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`data-action="stageTrackerPreviewSound" data-arg0="${esc(kind)}" data-arg-types="s,t" data-stop="1">▶ Play</button>`,
		`data-action="stageTrackerPlayTest" data-arg-types="t" data-stop="1">▶ Play both</button>`,
		`default: hiveDispatchAction('action', e); break;`,
		`const fn = (typeof HIVE_GEN_HANDLERS !== 'undefined' && HIVE_GEN_HANDLERS[action]) || window[action];`,
		"\n    function stageTrackerPreviewSound(kind, btn) {",
		"\n    function stageTrackerPlayTest(btn) {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// The context creator must never resume on its own: a stale synchronous
	// state check after a fire-and-forget resume() is the bug this guards.
	ctxFn := jsFunc(t, html, "stageTrackerAudioContext")
	if strings.Contains(ctxFn, "resume(") {
		t.Errorf("stageTrackerAudioContext must not call resume(); callers go through stageTrackerResumeContext:\n%s", ctxFn)
	}
	for _, fn := range []string{"stageTrackerPlayVoices", "stageTrackerPlayOnGesture", "stageTrackerSoundPrime"} {
		if !strings.Contains(jsFunc(t, html, fn), "stageTrackerResumeContext(") {
			t.Errorf("%s must await stageTrackerResumeContext", fn)
		}
	}
	// A blocked automatic play must surface the autoplay affordance (which
	// re-arms priming) rather than only re-arming priming silently.
	if pv := jsFunc(t, html, "stageTrackerPlayVoices"); !strings.Contains(pv, "stageTrackerSoundHeldNotice(true)") {
		t.Errorf("stageTrackerPlayVoices must call stageTrackerSoundHeldNotice(true) when the context stays blocked:\n%s", pv)
	}
	// Gesture plays schedule inside the gesture (WebKit unlock) and discard a
	// context the browser refused so the queued voices never burst later.
	if pg := jsFunc(t, html, "stageTrackerPlayOnGesture"); !strings.Contains(pg, "const scheduled = !!schedule(ctx);") || !strings.Contains(pg, "stageTrackerDiscardContext(ctx)") {
		t.Errorf("stageTrackerPlayOnGesture must schedule synchronously inside the gesture and discard a refused context:\n%s", pg)
	}
	for _, want := range []string{"'pointerdown'", "'keydown'", "'click'", "'touchend'"} {
		if !strings.Contains(jsConstLine(t, html, "STAGE_TRACKER_PRIME_EVENTS"), want) {
			t.Errorf("STAGE_TRACKER_PRIME_EVENTS must arm %s", want)
		}
	}
	if !strings.Contains(html, `.ham-sound.ham-sound-blocked`) {
		t.Errorf("index.html missing the .ham-sound-blocked chip style")
	}
	if init := jsFunc(t, html, "stageTrackerSoundInit"); !strings.Contains(init, "stageTrackerHasUserActivation(window.navigator)") || !strings.Contains(init, "stageTrackerSoundHeldNotice(false)") {
		t.Errorf("stageTrackerSoundInit must show the held-back affordance on a document without user activation:\n%s", init)
	}
	num := func(name string) int {
		m := regexp.MustCompile(`const ` + name + ` = (\d+);`).FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("index.html does not define %s as an integer", name)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	gesture, auto := num("STAGE_TRACKER_GESTURE_RESUME_MS"), num("STAGE_TRACKER_AUTO_RESUME_MS")
	if auto > 250 {
		t.Errorf("STAGE_TRACKER_AUTO_RESUME_MS = %d; an automatic play must give up within 250 ms so voices never burst", auto)
	}
	if gesture <= auto || gesture > 2000 {
		t.Errorf("STAGE_TRACKER_GESTURE_RESUME_MS = %d must exceed the automatic window (%d) and stay around a second", gesture, auto)
	}
}

// TestStageTrackerSoundResumeAsync runs the real play paths under node
// against a fake AudioContext whose resume() is asynchronous, the way every
// browser's is (and Safari's reports 'interrupted'). Asserts: a preview on a
// suspended context schedules its voices after resume() resolves and shows
// "Playing…" meanwhile; an automatic play on a context that resumes quickly
// plays exactly once; one that never resumes plays nothing and re-arms
// priming; a rejected resume() toasts; 'interrupted' behaves like suspended;
// a context that is already running schedules synchronously. Also: a
// gesture play starts its voices inside the gesture (WebKit unlock), a
// resume() that resolves before the state flips waits for statechange, a
// refused context is discarded with its queued voices, and a blocked
// automatic play (or a reload with sound on) shows the one-time "Enable
// sounds" affordance that resumes the context from its own click.
func TestStageTrackerSoundResumeAsync(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH — async resume paths were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		"const chip = { textContent: '🔇', attrs: {}, classes: new Set(), setAttribute(k, v) { this.attrs[k] = v; }, getAttribute(k) { return this.attrs[k]; }, removeAttribute(k) { delete this.attrs[k]; }, classList: { add: c => chip.classes.add(c), remove: c => chip.classes.delete(c), contains: c => chip.classes.has(c) } };",
		"const window = { navigator: { userActivation: { hasBeenActive: false } } }; const document = { hidden: false, listeners: {}, addEventListener(n, f) { (this.listeners[n] = this.listeners[n] || []).push(f); }, removeEventListener(n, f) { this.listeners[n] = (this.listeners[n] || []).filter(g => g !== f); }, getElementById(id) { return id === 'activity-monitor-sound' ? chip : null; } };",
		"const localStorage = { store: {}, getItem(k) { return k in this.store ? this.store[k] : null; }, setItem(k, v) { this.store[k] = String(v); } };",
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_MAX_PLAYS"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_OFFSET_S"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_MASTER_GAIN"),
		jsConstLine(t, html, "STAGE_TRACKER_BUZZ_MASTER_GAIN"),
		jsConstLine(t, html, "STAGE_TRACKER_VOLUME_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_VOLUME_DEFAULT"),
		jsConstLine(t, html, "STAGE_TRACKER_CLIP_PREVIEW_GAP_S"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_BLOCKED_MSG"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_UNSUPPORTED_MSG"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_PLAYING_LABEL"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_HELD_MSG"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_HELD_SKIPPED_MSG"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_HELD_ACTION"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_HELD_TOAST_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_HELD_CLASS"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_HELD_TITLE"),
		jsConstLine(t, html, "STAGE_TRACKER_PRIME_EVENTS"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_VOLUME_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_ZAP_S"),
		jsConstLine(t, html, "STAGE_TRACKER_BUZZ_S"),
		// Short windows keep the fixture fast; the real values are pinned by
		// TestStageTrackerSoundResumeWiring.
		"const STAGE_TRACKER_GESTURE_RESUME_MS = 80; const STAGE_TRACKER_AUTO_RESUME_MS = 40;",
		jsFunc(t, html, "stageTrackerAudioContext"),
		jsFunc(t, html, "stageTrackerResumeContext"),
		jsFunc(t, html, "stageTrackerDiscardContext"),
		jsFunc(t, html, "stageTrackerSoundPrime"),
		jsFunc(t, html, "stageTrackerHasUserActivation"),
		jsFunc(t, html, "stageTrackerSoundHeldNotice"),
		jsFunc(t, html, "stageTrackerSoundHeldClear"),
		jsFunc(t, html, "stageTrackerSoundSyncButton"),
		jsFunc(t, html, "stageTrackerSoundInit"),
		jsFunc(t, html, "stageTrackerScheduleVoices"),
		jsFunc(t, html, "stageTrackerPlayVoices"),
		jsFunc(t, html, "stageTrackerPlayOnGesture"),
		jsFunc(t, html, "stageTrackerPlayingState"),
		jsFunc(t, html, "stageTrackerClipVoice"),
		jsFunc(t, html, "stageTrackerVoiceFor"),
		jsFunc(t, html, "stageTrackerGainFor"),
		jsFunc(t, html, "stageTrackerScheduleSound"),
		jsFunc(t, html, "stageTrackerPlaySound"),
		jsFunc(t, html, "stageTrackerPlayZap"),
		jsFunc(t, html, "stageTrackerPlayBuzz"),
		jsFunc(t, html, "stageTrackerSoundLengthS"),
		jsFunc(t, html, "stageTrackerPlayTest"),
		jsFunc(t, html, "stageTrackerPreviewSound"),
		"function stageTrackerZapVoice(ctx, out, at) { out.voices.push('zap@' + at); }",
		"function stageTrackerBuzzVoice(ctx, out, at) { out.voices.push('buzz@' + at); }",
		"function stageTrackerDecodeClips() { if (decodeThrows) throw new Error('decode exploded'); }",
		"let decodeThrows = false;",
		"const toasts = []; function showToast(m, type, opts) { toasts.push({ m, type, opts: opts || {} }); }",
		"function stageTrackerClipsLoad() { return Promise.resolve(); } function stageTrackerRenderSettings() {}",
		"const STAGE_TRACKER_SOUND_KINDS = { open: { label: 'Open sound' }, merge: { label: 'Merge sound' } };",
		"var _stageTrackerAudioCtx = null; var _stageTrackerSoundOn = true; var _stageTrackerSoundPrimed = false; var _stageTrackerSoundHeldShown = false;",
		"var _stageTrackerVolume = STAGE_TRACKER_VOLUME_DEFAULT; var _stageTrackerClips = { open: null, merge: null };",
	}, "\n") + `
// Fake AudioContext: resume() is asynchronous and the test decides when (or
// whether) it settles. 'mode' is 'later' (resolve after resumeDelayMs),
// 'stale' (resolve at once with the state still 'suspended', then flip to
// 'running' and fire statechange after resumeDelayMs — WebKit's ordering),
// 'never', or 'reject'. Voices land in the master gain it hands out.
const gains = [];
let created = 0, closed = 0;
let mode = 'later', resumeDelayMs = 5;
class FakeAudioContext {
  constructor() { created++; this.state = 'suspended'; this.currentTime = 0; this.sampleRate = 48000; this.destination = {}; this.resumes = 0; this.stateListeners = []; }
  addEventListener(n, f) { if (n === 'statechange') this.stateListeners.push(f); }
  removeEventListener(n, f) { this.stateListeners = this.stateListeners.filter(g => g !== f); }
  close() { closed++; this.state = 'closed'; return Promise.resolve(); }
  createGain() { const g = { gain: { value: 1 }, voices: [], ctx: this, connect() {} }; gains.push(g); return g; }
  createBufferSource() { return { buffer: null, connect(out) { this.out = out; }, start(at) { this.out.voices.push('clip@' + at); } }; }
  resume() {
    this.resumes++;
    if (mode === 'reject') return Promise.reject(new Error('NotAllowedError'));
    if (mode === 'never') return new Promise(() => {});
    if (mode === 'stale') { setTimeout(() => { this.state = 'running'; this.stateListeners.slice().forEach(f => f()); }, resumeDelayMs); return Promise.resolve(); }
    return new Promise(resolve => setTimeout(() => { this.state = 'running'; resolve(); }, resumeDelayMs));
  }
}
window.AudioContext = FakeAudioContext;
const sleep = ms => new Promise(r => setTimeout(r, ms));
// Voices on a context that is still open — a discarded (closed) context's
// voices can never sound, so they do not count.
const voices = () => gains.filter(g => g.ctx.state !== 'closed').flatMap(g => g.voices);
const fakeButton = () => ({ textContent: '▶ Play', attrs: {}, classes: new Set(), setAttribute(k, v) { this.attrs[k] = v; }, removeAttribute(k) { delete this.attrs[k]; }, classList: { add: c => {}, remove: c => {} } });
const reset = () => { gains.length = 0; toasts.length = 0; closed = 0; _stageTrackerAudioCtx = null; _stageTrackerSoundPrimed = false; _stageTrackerSoundHeldShown = false; _stageTrackerSoundOn = true; chip.classes.clear(); chip.attrs = {}; document.listeners = {}; };
const heldToasts = () => toasts.filter(t => t.m.indexOf(STAGE_TRACKER_SOUND_HELD_MSG) === 0);

(async () => {
  // (a) Preview inside a gesture on a suspended context: resume() is called
  // and the voice is scheduled synchronously inside the gesture (WebKit
  // unlocks the context only for a resume()/start() made while processing
  // the gesture), the button reads "Playing…" until the context runs, and
  // the preview resolves true once it does. The context is created inside
  // the handler.
  reset();
  const btn = fakeButton();
  const p = stageTrackerPreviewSound('merge', btn);
  assert.strictEqual(created, 1, 'context created inside the gesture handler');
  assert.strictEqual(_stageTrackerAudioCtx.state, 'suspended', 'state is still stale-suspended synchronously');
  assert.strictEqual(_stageTrackerAudioCtx.resumes, 1, 'resume() called synchronously inside the gesture');
  assert.deepStrictEqual(voices(), ['zap@0'], 'the voice is started synchronously inside the gesture');
  assert.strictEqual(btn.textContent, STAGE_TRACKER_SOUND_PLAYING_LABEL);
  assert.strictEqual(btn.attrs['aria-busy'], 'true');
  assert.strictEqual(await p, true, 'preview resolves true once the context runs');
  assert.deepStrictEqual(voices(), ['zap@0'], 'no second voice after resume() resolves');
  assert.deepStrictEqual(toasts, [], 'no toast on success');
  await sleep(Math.ceil(STAGE_TRACKER_ZAP_S * 1000) + 10);
  assert.strictEqual(btn.textContent, '▶ Play', 'label restored after the sound length');
  assert.strictEqual(btn.attrs['aria-busy'], undefined);
  // An already running context schedules synchronously (second click).
  const before = gains.length;
  stageTrackerPreviewSound('open', null);
  assert.strictEqual(gains.length, before + 1, 'running context: scheduled at once');
  assert.strictEqual(_stageTrackerAudioCtx.resumes, 1, 'no second resume() on a running context');

  // ▶ Play both: one resume(), merge then open at the preview gap.
  reset();
  const both = fakeButton();
  assert.strictEqual(await stageTrackerPlayTest(both), true);
  assert.deepStrictEqual(voices(), ['zap@0', 'buzz@' + (STAGE_TRACKER_ZAP_S + STAGE_TRACKER_CLIP_PREVIEW_GAP_S)]);
  assert.strictEqual(_stageTrackerAudioCtx.resumes, 1);

  // (b) Automatic play on a suspended context that resumes quickly: plays
  // once (the two-voice cap honoured), no burst, no priming re-armed.
  reset();
  assert.strictEqual(stageTrackerPlayZap(3), false, 'not scheduled synchronously');
  assert.deepStrictEqual(voices(), []);
  await sleep(resumeDelayMs + 10);
  assert.strictEqual(voices().length, STAGE_TRACKER_SOUND_MAX_PLAYS, 'scheduled once resume() resolved, capped');
  assert.strictEqual(gains.length, 1, 'exactly one play, not a burst');
  assert.strictEqual(_stageTrackerSoundPrimed, false, 'priming not re-armed when the resume succeeded');
  assert.deepStrictEqual(heldToasts(), [], 'no held-back toast when the resume succeeded');
  assert.ok(!chip.classes.has(STAGE_TRACKER_SOUND_HELD_CLASS), 'chip not marked held when the resume succeeded');
  await sleep(STAGE_TRACKER_AUTO_RESUME_MS + 10);
  assert.strictEqual(gains.length, 1, 'the timed-out branch did not fire after the resolved one');

  // (c) Automatic play on a context whose resume() never settles: plays
  // nothing, re-arms priming, and the primed gesture then resumes it.
  reset();
  mode = 'never';
  assert.strictEqual(stageTrackerPlayBuzz(1), false);
  await sleep(STAGE_TRACKER_AUTO_RESUME_MS + 10);
  assert.deepStrictEqual(voices(), [], 'nothing queued for a later burst');
  assert.strictEqual(_stageTrackerSoundPrimed, true, 'priming re-armed');
  for (const n of ['pointerdown', 'keydown', 'click', 'touchend']) assert.strictEqual((document.listeners[n] || []).length, 1, n + ' armed');
  // The dropped play is not silent: the chip goes amber and one toast with
  // an "Enable sounds" action explains the autoplay policy.
  assert.ok(chip.classes.has(STAGE_TRACKER_SOUND_HELD_CLASS), 'chip marked held');
  assert.strictEqual(chip.attrs.title, STAGE_TRACKER_SOUND_HELD_TITLE);
  assert.strictEqual(heldToasts().length, 1, 'one held-back toast');
  assert.strictEqual(heldToasts()[0].m, STAGE_TRACKER_SOUND_HELD_MSG + STAGE_TRACKER_SOUND_HELD_SKIPPED_MSG);
  assert.strictEqual(heldToasts()[0].type, 'warning');
  assert.strictEqual(heldToasts()[0].opts.actionLabel, STAGE_TRACKER_SOUND_HELD_ACTION);
  assert.strictEqual(typeof heldToasts()[0].opts.onAction, 'function');
  // A second dropped play re-marks the chip but never toasts again.
  assert.strictEqual(stageTrackerPlayZap(1), false);
  await sleep(STAGE_TRACKER_AUTO_RESUME_MS + 10);
  assert.strictEqual(heldToasts().length, 1, 'the toast is shown once per page load');
  // A gesture whose resume() does not settle keeps the listeners armed.
  document.listeners.pointerdown[0]();
  await sleep(STAGE_TRACKER_GESTURE_RESUME_MS + 10);
  assert.strictEqual(_stageTrackerSoundPrimed, true, 'still primed after a blocked gesture');
  assert.strictEqual(document.listeners.pointerdown.length, 1, 'listeners kept (not once:true) until a resume succeeds');
  assert.ok(chip.classes.has(STAGE_TRACKER_SOUND_HELD_CLASS), 'chip still held after a blocked gesture');
  // The next gesture (a click this time) succeeds: it resumes the context,
  // disarms and clears the affordance.
  mode = 'later';
  document.listeners.click[0]();
  await sleep(resumeDelayMs + 10);
  assert.strictEqual(_stageTrackerAudioCtx.state, 'running');
  assert.strictEqual(_stageTrackerSoundPrimed, false, 'disarmed after a successful resume');
  for (const n of ['pointerdown', 'keydown', 'click', 'touchend']) assert.strictEqual(document.listeners[n].length, 0, n + ' disarmed');
  assert.deepStrictEqual(voices(), [], 'priming itself never plays');
  assert.ok(!chip.classes.has(STAGE_TRACKER_SOUND_HELD_CLASS), 'chip cleared once the context runs');
  assert.strictEqual(chip.attrs.title, 'Sound on (merge zap + new-item buzz) — turn off', 'chip title restored');

  // The toast's "Enable sounds" action is itself the gesture: it resumes
  // the context and clears the affordance without playing anything.
  reset();
  mode = 'never';
  assert.strictEqual(stageTrackerPlayBuzz(1), false);
  await sleep(STAGE_TRACKER_AUTO_RESUME_MS + 10);
  assert.strictEqual(heldToasts().length, 1);
  mode = 'later';
  heldToasts()[0].opts.onAction();
  await sleep(resumeDelayMs + 10);
  assert.strictEqual(_stageTrackerAudioCtx.state, 'running');
  assert.ok(!chip.classes.has(STAGE_TRACKER_SOUND_HELD_CLASS), 'action clears the chip');
  assert.deepStrictEqual(voices(), [], 'the action plays nothing');
  assert.strictEqual(toasts.length, 1, 'no blocked toast from the action');

  // (d) resume() rejects: the preview toasts and resolves false; label
  // restored at once. An automatic play re-arms priming instead of toasting.
  reset();
  mode = 'reject';
  const rb = fakeButton();
  assert.strictEqual(await stageTrackerPreviewSound('open', rb), false);
  assert.strictEqual(toasts.length, 1);
  assert.strictEqual(toasts[0].m, STAGE_TRACKER_SOUND_BLOCKED_MSG);
  assert.strictEqual(toasts[0].type, 'warning');
  await sleep(1);
  assert.strictEqual(rb.textContent, '▶ Play', 'label restored after a blocked preview');
  assert.strictEqual(closed, 1, 'the refused context is closed');
  assert.strictEqual(_stageTrackerAudioCtx, null, 'and dropped, so the next play starts fresh');
  assert.deepStrictEqual(voices(), [], 'the voice queued inside the gesture went with it');
  assert.strictEqual(stageTrackerPlayZap(1), false);
  await sleep(5);
  assert.strictEqual(toasts.length, 2, 'an automatic play never shows the blocked toast, only the held-back one');
  assert.strictEqual(heldToasts().length, 1);
  assert.strictEqual(_stageTrackerSoundPrimed, true);

  // Gesture that times out (resume() pending past the gesture window) toasts too.
  reset();
  mode = 'never';
  assert.strictEqual(await stageTrackerPreviewSound('merge', null), false);
  assert.strictEqual(toasts.length, 1);
  assert.strictEqual(toasts[0].m, STAGE_TRACKER_SOUND_BLOCKED_MSG);

  // (e) Safari's 'interrupted' is handled like suspended: resumed, then played.
  reset();
  mode = 'later';
  stageTrackerAudioContext().state = 'interrupted';
  const ip = stageTrackerPreviewSound('merge', null);
  assert.deepStrictEqual(voices(), ['zap@0'], 'scheduled inside the gesture');
  assert.strictEqual(await ip, true);
  assert.deepStrictEqual(voices(), ['zap@0']);
  reset();
  stageTrackerAudioContext().state = 'interrupted';
  assert.strictEqual(stageTrackerPlayBuzz(1), false);
  await sleep(resumeDelayMs + 10);
  assert.deepStrictEqual(voices(), ['buzz@0'], 'automatic play after an interrupted context resumes');

  // (f) WebKit ordering: resume() resolves while the state still reads
  // 'suspended' and statechange flips it a beat later. That is not a
  // blocked context — the play waits for the statechange and succeeds.
  reset();
  mode = 'stale';
  assert.strictEqual(await stageTrackerPreviewSound('merge', null), true);
  assert.deepStrictEqual(toasts, [], 'a stale-but-resuming context never toasts');
  assert.deepStrictEqual(voices(), ['zap@0']);
  assert.strictEqual(_stageTrackerAudioCtx.stateListeners.length, 0, 'statechange listener removed once settled');
  reset();
  assert.strictEqual(stageTrackerPlayBuzz(1), false);
  await sleep(resumeDelayMs + 10);
  assert.deepStrictEqual(voices(), ['buzz@0'], 'automatic play on a stale-resolving context plays once it runs');
  assert.deepStrictEqual(heldToasts(), []);
  // A resume() that resolves and a state that never flips is still blocked,
  // on the deadline.
  reset();
  mode = 'later';
  const stuck = stageTrackerAudioContext();
  stuck.resume = () => Promise.resolve();
  assert.strictEqual(await stageTrackerPreviewSound('merge', null), false);
  assert.strictEqual(toasts.length, 1);
  assert.deepStrictEqual(voices(), []);

  // (g) Sound left on across a reload: on a document without user
  // activation the affordance shows at init, before any event is dropped,
  // and does not when the document has been interacted with.
  reset();
  localStorage.store[STAGE_TRACKER_SOUND_KEY] = '1';
  window.navigator.userActivation.hasBeenActive = false;
  stageTrackerSoundInit();
  assert.strictEqual(_stageTrackerSoundOn, true);
  assert.strictEqual(heldToasts().length, 1, 'held-back toast at init without activation');
  assert.strictEqual(heldToasts()[0].m, STAGE_TRACKER_SOUND_HELD_MSG, 'no "skipped" suffix before anything was dropped');
  assert.ok(chip.classes.has(STAGE_TRACKER_SOUND_HELD_CLASS));
  assert.strictEqual(_stageTrackerSoundPrimed, true, 'priming armed by the notice');
  reset();
  window.navigator.userActivation.hasBeenActive = true;
  stageTrackerSoundInit();
  assert.deepStrictEqual(heldToasts(), [], 'no toast when the document has user activation');
  assert.ok(!chip.classes.has(STAGE_TRACKER_SOUND_HELD_CLASS));
  assert.strictEqual(_stageTrackerSoundPrimed, true, 'priming still armed for a browser that wants a gesture anyway');
  reset();
  localStorage.store[STAGE_TRACKER_SOUND_KEY] = '0';
  window.navigator.userActivation.hasBeenActive = false;
  stageTrackerSoundInit();
  assert.strictEqual(_stageTrackerSoundOn, false);
  assert.deepStrictEqual(heldToasts(), [], 'sound off: nothing to hold back, no toast');
  assert.strictEqual(stageTrackerHasUserActivation({}), false, 'no userActivation API counts as no activation');
  assert.strictEqual(stageTrackerHasUserActivation(undefined), false);
  _stageTrackerSoundOn = true;

  // Clip decoding that throws cannot abort context creation.
  reset();
  decodeThrows = true;
  assert.ok(stageTrackerAudioContext(), 'context created even when decoding throws');
  decodeThrows = false;

  // No Web Audio at all: the preview explains instead of staying silent.
  reset();
  delete window.AudioContext;
  assert.strictEqual(await stageTrackerPreviewSound('merge', null), false);
  assert.strictEqual(toasts.length, 1);
  assert.strictEqual(toasts[0].m, STAGE_TRACKER_SOUND_UNSUPPORTED_MSG);
  assert.strictEqual(stageTrackerPlayZap(1), false);
  assert.strictEqual(toasts.length, 1);
  console.log('ok');
})().catch(e => { console.error(e); process.exit(1); });
`
	cmd := exec.Command("node", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node async resume fixture failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("node async resume fixture did not reach the end:\n%s", out)
	}
}
