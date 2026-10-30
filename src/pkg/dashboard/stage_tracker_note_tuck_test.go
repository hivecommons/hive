package dashboard

import (
	"strings"
	"testing"
)

// The hive note under a row's chevrons shows for 30 s when it is new, then
// tucks into a ⬡-i icon; a click re-opens it for another 30 s (owner,
// 2026-10-29). These tests lift the pure pieces and the in-place sync and
// drive them against a fake clock + DOM.

func stageTrackerNoteTuckJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		"function esc(s) { if (typeof s !== 'string') return ''; return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		jsConstLine(t, html, "STAGE_TRACKER_EVENT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_CAPTION_SHOW_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_NOTE_TUCK_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_NOTE_LABEL"),
		jsConstLine(t, html, "STAGE_TRACKER_NOTE_ICON"),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerPRKey"),
		jsFunc(t, html, "stageTrackerStateMap"),
		jsFunc(t, html, "stageTrackerStageFell"),
		jsConstBlock(t, html, "STAGE_TRACKER_WORK_WORDS"),
		jsConstBlock(t, html, "STAGE_TRACKER_EVENT_MOD_WORDS"),
		jsConstLine(t, html, "STAGE_TRACKER_EVENT_COVERED_BY_PR"),
		jsFunc(t, html, "stageTrackerEventText"),
		jsFunc(t, html, "stageTrackerEventsNext"),
		jsFunc(t, html, "stageTrackerNoteEvents"),
		jsFunc(t, html, "stageTrackerNoteOwnerEvent"),
		jsFunc(t, html, "stageTrackerEventHtml"),
		jsFunc(t, html, "stageTrackerNoteTuckSetting"),
		jsFunc(t, html, "stageTrackerSetNoteTuck"),
		jsFunc(t, html, "stageTrackerNoteOpen"),
		jsFunc(t, html, "stageTrackerNoteToggled"),
		jsFunc(t, html, "stageTrackerNoteNextDue"),
		jsFunc(t, html, "stageTrackerNoteShift"),
		jsFunc(t, html, "stageTrackerNoteTitle"),
		jsFunc(t, html, "stageTrackerNoteSync"),
		jsFunc(t, html, "stageTrackerNoteSchedule"),
		jsFunc(t, html, "stageTrackerNoteToggle"),
		jsFunc(t, html, "stageTrackerNoteVisibility"),
		jsFunc(t, html, "stageTrackerRowSrcKey"),
		"let _stageTrackerEvents = {}; let _stageTrackerStates = null; let _stageTrackerNoteTimer = 0; let _stageTrackerNoteHiddenAt = 0;",
	}, "\n") + "\n"
}

// A fake clock whose setTimeout fires from tick(ms), and the few DOM calls
// stageTrackerNoteSync makes: querySelectorAll('.cue-event[data-cue-key]'),
// querySelector('.cue-event-ico'), classList, title, get/setAttribute.
const stageTrackerNoteTuckFixture = `
const assert = require('assert');
let clock = Date.parse('2026-10-29T20:17:00Z');
Date.now = () => clock;
let timers = []; let timerSeq = 0;
global.setTimeout = (fn, ms) => { const id = ++timerSeq; timers.push({ id, at: clock + (ms || 0), fn }); return id; };
global.clearTimeout = id => { timers = timers.filter(x => x.id !== id); };
function tick(ms) { const until = clock + ms; while (true) { const due = timers.filter(x => x.at <= until).sort((a, b) => a.at - b.at)[0]; if (!due) break; timers = timers.filter(x => x !== due); clock = Math.max(clock, due.at); due.fn(); } clock = until; }
const store = {}; global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem(k, v) { store[k] = String(v); } };
function el(cls, attrs) {
  const e = { className: cls || '', title: '', attrs: Object.assign({}, attrs || {}), children: [],
    classList: { add(c) { if (!e.className.split(' ').includes(c)) e.className = (e.className + ' ' + c).trim(); }, remove(c) { e.className = e.className.split(' ').filter(x => x && x !== c).join(' '); }, contains(c) { return e.className.split(' ').includes(c); } },
    getAttribute(k) { return k in e.attrs ? e.attrs[k] : null; }, setAttribute(k, v) { e.attrs[k] = String(v); },
    querySelector(sel) { return e.querySelectorAll(sel)[0] || null; },
    querySelectorAll(sel) { const out = []; const walk = n => { for (const c of n.children) { if (sel === '.cue-event[data-cue-key]' ? (c.classList.contains('cue-event') && c.attrs['data-cue-key'] != null) : c.classList.contains(sel.slice(1))) out.push(c); walk(c); } }; walk(e); return out; } };
  return e;
}
const panel = el('panel');
global.document = { hidden: false, getElementById(id) { return id === 'activity-monitor-panel' ? panel : null; } };
// Build a line element from the rendered HTML, as the page's reconcile would.
function mount(key) {
  const html = stageTrackerEventHtml(key, { events: _stageTrackerEvents, nowMs: Date.now() });
  const p = el((html.match(/^<p class="([^"]*)"/) || [])[1], { 'data-cue-key': key });
  if (/class="cue-event-ico"/.test(html)) { const b = el('cue-event-ico', { 'aria-expanded': (html.match(/aria-expanded="(\w+)"/) || [])[1] }); b.title = (html.match(/ title="([^"]*)"/) || [])[1]; p.children.push(b); }
  panel.children.push(p); return p;
}
const tucked = p => p.classList.contains('cue-event-tucked');
`

func TestStageTrackerNoteTuckNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerNoteTuckJS(t, html)+stageTrackerNoteTuckFixture+`
assert.strictEqual(STAGE_TRACKER_CAPTION_SHOW_MS, 30000);
assert.strictEqual(stageTrackerNoteTuckSetting(), true, 'default on');

// A new line: open, with the ⬡-i button carrying the whole note as its tooltip.
stageTrackerNoteOwnerEvent('k', 'approved', Date.now());
let html = stageTrackerEventHtml('k', { events: _stageTrackerEvents, nowMs: Date.now() });
assert.ok(html.startsWith('<p class="cue-event cue-event-tuckable" data-cue-key="k"><button type="button" class="cue-event-ico" aria-label="Hive note" aria-expanded="true" title="You: approved · 0s ago" data-action="stageTrackerNoteToggle" data-arg0="k" data-stop="1">'), html);
assert.ok(html.includes('<span class="cue-event-body"><span class="cue-event-who">You:</span> <span class="cue-event-text">approved</span>'), html);
assert.ok(!html.includes('cue-event-fading'), 'no 2-minute fade while auto-collapse is on');
const p = mount('k');
stageTrackerNoteSync(panel, Date.now());
assert.ok(!tucked(p), 'open when new');
assert.strictEqual(timers.length, 1, 'one shared timer armed');

// 30 s later it tucks; the tooltip keeps the note and its age.
tick(29999); assert.ok(!tucked(p), 'still open at 29.999 s');
tick(1); assert.ok(tucked(p), 'tucked at 30 s');
assert.strictEqual(p.querySelector('.cue-event-ico').getAttribute('aria-expanded'), 'false');
assert.strictEqual(p.querySelector('.cue-event-ico').title, 'You: approved · 30s ago');
assert.strictEqual(timers.length, 0, 'nothing left to time');
// A fresh node rendered now arrives already tucked (no replayed slide).
assert.ok(/^<p class="cue-event cue-event-tuckable cue-event-tucked"/.test(stageTrackerEventHtml('k', { events: _stageTrackerEvents, nowMs: Date.now() })));

// Click re-opens for another 30 s, then it tucks again.
stageTrackerNoteToggle('k');
assert.ok(!tucked(p), 'click re-opens');
tick(29000); assert.ok(!tucked(p));
tick(1000); assert.ok(tucked(p), 'tucks again after another 30 s');
// Click while open tucks at once.
stageTrackerNoteToggle('k'); assert.ok(!tucked(p));
stageTrackerNoteToggle('k'); assert.ok(tucked(p), 'a click while open tucks at once');
assert.strictEqual(timers.length, 0);

// Hidden tab pauses the window: 20 s open, hidden for 60 s, visible → 10 s left.
stageTrackerNoteToggle('k'); tick(20000); assert.ok(!tucked(p));
stageTrackerNoteVisibility(true); document.hidden = true; assert.strictEqual(timers.length, 0, 'no timer while hidden');
tick(60000); stageTrackerNoteSync(panel, Date.now()); assert.ok(!tucked(p), 'not spent unseen');
document.hidden = false; stageTrackerNoteVisibility(false);
assert.ok(!tucked(p), 'still open on return');
tick(9999); assert.ok(!tucked(p)); tick(1); assert.ok(tucked(p), 'the remaining 10 s run after return');

// Rows have independent clocks, one timer.
stageTrackerNoteOwnerEvent('a', 'one', Date.now()); const pa = mount('a');
tick(10000);
stageTrackerNoteOwnerEvent('b', 'two', Date.now()); const pb = mount('b');
stageTrackerNoteSync(panel, Date.now());
assert.strictEqual(timers.length, 1, 'still one shared timer with two open lines');
tick(20000); assert.ok(tucked(pa) && !tucked(pb), 'a tucks first'); tick(10000); assert.ok(tucked(pb));

// Pure helpers: shift only moves open windows; a tucked line stays put.
assert.deepStrictEqual(stageTrackerNoteShift({ x: { at: 100, tucked: true }, y: { at: 100 } }, 50), { x: { at: 100, tucked: true }, y: { at: 100, openAt: 150 } });
assert.strictEqual(stageTrackerNoteNextDue({}, 0, true), Infinity);

// Payload diffs: auto-collapse keeps lines (no 2-minute expiry) until the
// row leaves the payload; off, the old expiry holds.
const stages = [{ number: 40, label: 'Claimed' }, { number: 50, label: 'CI / Review' }];
const issue = { repo: 'r', kind: 'pr', number: 1, stage: 40, caption: '' };
const prev = stageTrackerStateMap([issue]); const next = stageTrackerStateMap([Object.assign({}, issue, { stage: 50 })]);
let ev = stageTrackerEventsNext({}, prev, next, stages, 0, Infinity);
assert.deepStrictEqual(Object.keys(ev), ['r/pr#1']);
assert.deepStrictEqual(Object.keys(stageTrackerEventsNext(ev, next, next, stages, STAGE_TRACKER_EVENT_MS * 10, Infinity)), ['r/pr#1'], 'kept, tucked, well past 2 min');
assert.deepStrictEqual(stageTrackerEventsNext(ev, next, {}, stages, 1000, Infinity), {}, 'dropped when its row leaves');
assert.deepStrictEqual(stageTrackerEventsNext(ev, next, next, stages, STAGE_TRACKER_EVENT_MS + 1), {}, 'default ttl still expires');

// The tucked flag, aria-expanded and the ageing tooltip are not reasons to swap a row's node.
const a = stageTrackerEventHtml('k', { events: _stageTrackerEvents, nowMs: Date.now() });
_stageTrackerEvents.k = Object.assign({}, _stageTrackerEvents.k, { tucked: false, openAt: Date.now() });
const b = stageTrackerEventHtml('k', { events: _stageTrackerEvents, nowMs: Date.now() + 5000 });
assert.notStrictEqual(a, b); assert.strictEqual(stageTrackerRowSrcKey(a), stageTrackerRowSrcKey(b), 'same source key across a tuck / re-open / age');

// Preference off: the line stays open, no button, the old fade returns.
stageTrackerSetNoteTuck(false);
assert.strictEqual(stageTrackerNoteTuckSetting(), false);
assert.strictEqual(stageTrackerNoteOpen({ at: 0, tucked: true }, 10 * STAGE_TRACKER_CAPTION_SHOW_MS, false), true, 'off keeps every line open');
html = stageTrackerEventHtml('k', { events: _stageTrackerEvents, nowMs: Date.now() + STAGE_TRACKER_EVENT_MS * 0.8 });
assert.ok(html.startsWith('<p class="cue-event cue-event-fading" data-cue-key="k"><span class="cue-event-who">You:</span>'), html);
assert.ok(!html.includes('cue-event-ico'));
stageTrackerNoteSync(panel, Date.now());
assert.ok(!tucked(p) && !tucked(pa) && !tucked(pb), 'off un-tucks every line in place');
assert.strictEqual(timers.length, 0, 'nothing to time when off');
stageTrackerSetNoteTuck(true);
`)
}

// TestStageTrackerNoteTuckWiring: the page carries the markup, the CSS (tokens
// only, reduced motion → instant toggle), the ⚙ toggle, and the visibility
// hook; the window is the one the owner asked for.
func TestStageTrackerNoteTuckWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`const STAGE_TRACKER_CAPTION_SHOW_MS = 30000;`,
		`const STAGE_TRACKER_NOTE_TUCK_KEY = 'hive.tracker.notetuck';`,
		`aria-label="${STAGE_TRACKER_NOTE_LABEL}"`,
		`data-action="stageTrackerNoteToggle"`,
		`id="sts-notetuck-on" data-change-action="stageTrackerNoteTuckChange" data-arg-types="c"`,
		`Auto-collapse hive notes <span class="text-muted">(default on)</span>`,
		`stageTrackerNoteVisibility(document.visibilityState !== 'visible')`,
		`ctx.noteTuck = stageTrackerNoteTuckSetting();`,
		`if (typeof stageTrackerNoteSync === 'function') stageTrackerNoteSync(el, nowMs);`,
		`.cue-event.cue-event-tuckable { display: flex; align-items: center;`,
		`transition: clip-path 400ms ease-in-out, opacity 350ms ease-in-out, transform 400ms ease-in-out;`,
		`.cue-event-tucked .cue-event-body { opacity: 0; transform: translateX(-8px); clip-path: inset(0 100% 0 0); pointer-events: none; }`,
		`@media (prefers-reduced-motion: reduce) { .cue-event-body, .cue-event-ico { transition: none; } }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	// The line keeps one fixed height either way so the chevrons never shift.
	if !strings.Contains(html, `.cue-event.cue-event-tuckable { display: flex; align-items: center; gap: 0; height: 1.6em; line-height: 1.6em;`) {
		t.Errorf("tuckable line has no fixed height")
	}
}
