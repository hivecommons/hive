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
// a context that is already running schedules synchronously.
func TestStageTrackerSoundResumeAsync(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH — async resume paths were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		"const window = {}; const document = { hidden: false, listeners: {}, addEventListener(n, f) { (this.listeners[n] = this.listeners[n] || []).push(f); }, removeEventListener(n, f) { this.listeners[n] = (this.listeners[n] || []).filter(g => g !== f); } };",
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
		jsConstLine(t, html, "STAGE_TRACKER_ZAP_S"),
		jsConstLine(t, html, "STAGE_TRACKER_BUZZ_S"),
		// Short windows keep the fixture fast; the real values are pinned by
		// TestStageTrackerSoundResumeWiring.
		"const STAGE_TRACKER_GESTURE_RESUME_MS = 80; const STAGE_TRACKER_AUTO_RESUME_MS = 40;",
		jsFunc(t, html, "stageTrackerAudioContext"),
		jsFunc(t, html, "stageTrackerResumeContext"),
		jsFunc(t, html, "stageTrackerSoundPrime"),
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
		"const toasts = []; function showToast(m, type) { toasts.push({ m, type }); }",
		"const STAGE_TRACKER_SOUND_KINDS = { open: { label: 'Open sound' }, merge: { label: 'Merge sound' } };",
		"var _stageTrackerAudioCtx = null; var _stageTrackerSoundOn = true; var _stageTrackerSoundPrimed = false;",
		"var _stageTrackerVolume = STAGE_TRACKER_VOLUME_DEFAULT; var _stageTrackerClips = { open: null, merge: null };",
	}, "\n") + `
// Fake AudioContext: resume() is asynchronous and the test decides when (or
// whether) it settles. 'mode' is 'later' (resolve after resumeDelayMs),
// 'never', or 'reject'. Voices land in the master gain it hands out.
const gains = [];
let created = 0;
let mode = 'later', resumeDelayMs = 5;
class FakeAudioContext {
  constructor() { created++; this.state = 'suspended'; this.currentTime = 0; this.sampleRate = 48000; this.destination = {}; this.resumes = 0; }
  createGain() { const g = { gain: { value: 1 }, voices: [], connect() {} }; gains.push(g); return g; }
  createBufferSource() { return { buffer: null, connect(out) { this.out = out; }, start(at) { this.out.voices.push('clip@' + at); } }; }
  resume() {
    this.resumes++;
    if (mode === 'reject') return Promise.reject(new Error('NotAllowedError'));
    if (mode === 'never') return new Promise(() => {});
    return new Promise(resolve => setTimeout(() => { this.state = 'running'; resolve(); }, resumeDelayMs));
  }
}
window.AudioContext = FakeAudioContext;
const sleep = ms => new Promise(r => setTimeout(r, ms));
const voices = () => gains.flatMap(g => g.voices);
const fakeButton = () => ({ textContent: '▶ Play', attrs: {}, classes: new Set(), setAttribute(k, v) { this.attrs[k] = v; }, removeAttribute(k) { delete this.attrs[k]; }, classList: { add: c => {}, remove: c => {} } });
const reset = () => { gains.length = 0; toasts.length = 0; _stageTrackerAudioCtx = null; _stageTrackerSoundPrimed = false; document.listeners = {}; };

(async () => {
  // (a) Preview inside a gesture on a suspended context: nothing plays
  // synchronously, the button reads "Playing…", and the voice is scheduled
  // once resume() resolves. The context is created inside the handler.
  reset();
  const btn = fakeButton();
  const p = stageTrackerPreviewSound('merge', btn);
  assert.strictEqual(created, 1, 'context created inside the gesture handler');
  assert.strictEqual(_stageTrackerAudioCtx.state, 'suspended', 'state is still stale-suspended synchronously');
  assert.deepStrictEqual(voices(), [], 'nothing scheduled before resume() settles');
  assert.strictEqual(btn.textContent, STAGE_TRACKER_SOUND_PLAYING_LABEL);
  assert.strictEqual(btn.attrs['aria-busy'], 'true');
  assert.strictEqual(await p, true, 'preview resolves true once scheduled');
  assert.deepStrictEqual(voices(), ['zap@0'], 'the preview voice plays after resume() resolves');
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
  assert.strictEqual((document.listeners.pointerdown || []).length, 1);
  assert.strictEqual((document.listeners.keydown || []).length, 1);
  // A gesture whose resume() does not settle keeps the listeners armed.
  document.listeners.pointerdown[0]();
  await sleep(STAGE_TRACKER_GESTURE_RESUME_MS + 10);
  assert.strictEqual(_stageTrackerSoundPrimed, true, 'still primed after a blocked gesture');
  assert.strictEqual(document.listeners.pointerdown.length, 1, 'listeners kept (not once:true) until a resume succeeds');
  // The next gesture succeeds: it resumes the context and disarms.
  mode = 'later';
  document.listeners.pointerdown[0]();
  await sleep(resumeDelayMs + 10);
  assert.strictEqual(_stageTrackerAudioCtx.state, 'running');
  assert.strictEqual(_stageTrackerSoundPrimed, false, 'disarmed after a successful resume');
  assert.strictEqual(document.listeners.pointerdown.length, 0);
  assert.strictEqual(document.listeners.keydown.length, 0);
  assert.deepStrictEqual(voices(), [], 'priming itself never plays');

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
  assert.deepStrictEqual(voices(), []);
  assert.strictEqual(stageTrackerPlayZap(1), false);
  await sleep(5);
  assert.strictEqual(toasts.length, 1, 'automatic plays never toast');
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
  assert.deepStrictEqual(voices(), []);
  assert.strictEqual(await ip, true);
  assert.deepStrictEqual(voices(), ['zap@0']);
  reset();
  stageTrackerAudioContext().state = 'interrupted';
  assert.strictEqual(stageTrackerPlayBuzz(1), false);
  await sleep(resumeDelayMs + 10);
  assert.deepStrictEqual(voices(), ['buzz@0'], 'automatic play after an interrupted context resumes');

  // A resume() that resolves while the state stays blocked counts as blocked.
  reset();
  const stuck = stageTrackerAudioContext();
  stuck.resume = () => Promise.resolve();
  assert.strictEqual(await stageTrackerPreviewSound('merge', null), false);
  assert.strictEqual(toasts.length, 1);
  assert.deepStrictEqual(voices(), []);

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
