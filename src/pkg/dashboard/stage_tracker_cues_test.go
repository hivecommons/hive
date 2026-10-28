package dashboard

import (
	"strings"
	"testing"
)

// stageTrackerCuesJS lifts the tracker-cues helpers (working / waiting
// lead, cue classes, event line, merged counter) so fixtures that render
// rows can carry them (STAGE_TRACKER_TERMINAL, stageTrackerItemKey and
// stageTrackerActorKind are the fixture's own). The row renderers guard
// every call with typeof, so a fixture without this block still renders —
// without the cues.
func stageTrackerCuesJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_EVENT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGED_TIP_MAX"),
		jsConstBlock(t, html, "STAGE_TRACKER_WORK_WORDS"),
		jsConstBlock(t, html, "STAGE_TRACKER_EVENT_MOD_WORDS"),
		jsFunc(t, html, "stageTrackerWorkers"),
		jsFunc(t, html, "stageTrackerWorkerKey"),
		jsFunc(t, html, "stageTrackerWorkingOn"),
		jsFunc(t, html, "stageTrackerCueLead"),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsFunc(t, html, "stageTrackerCueClasses"),
		jsFunc(t, html, "stageTrackerStateMap"),
		jsFunc(t, html, "stageTrackerEventText"),
		jsFunc(t, html, "stageTrackerEventsNext"),
		jsFunc(t, html, "stageTrackerNoteOwnerEvent"),
		jsFunc(t, html, "stageTrackerEventHtml"),
		jsFunc(t, html, "stageTrackerLocalMidnight"),
		jsFunc(t, html, "stageTrackerMergedTodayNext"),
		jsFunc(t, html, "stageTrackerMergedTodayText"),
		jsFunc(t, html, "stageTrackerPRKey"),
		"let _stageTrackerEvents = {};",
	}, "\n") + "\n"
}

// TestStageTrackerCuesWiring: the shared cue vocabulary the three Activity
// Monitor PRs compose on (owner, 2026-10-28) is in the page — class names,
// the one spin keyframe, the merged counter chip — and the CSS is tokens
// only, with the reduced-motion fallback stopping every animation.
func TestStageTrackerCuesWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`/* ---- tracker-cues: the Activity Monitor's shared cue vocabulary`,
		`.cue-working .ham-seg.active:not(.cap) .ham-active-ico, .cue-spin { animation: cue-spin`,
		`.cue-acting > .ham-strip-row .ham-seg.active {`,
		`.cue-event {`, `.cue-setback .cue-event-who {`, `.cue-merged .cue-event-who {`,
		`.cue-merged .ham-seg.cap {`, `.cue-token {`, `.ham-merged-count {`, `.ham-merged-count.cue-tick {`,
		`@media (prefers-reduced-motion: reduce) { .cue-working .ham-seg.active .ham-active-ico, .cue-spin, .cue-acting .ham-seg.active, .cue-event { animation: none; } }`,
		`@media (prefers-reduced-motion: reduce) { .cue-token { display: none; }`,
		`id="activity-monitor-merged"`, `class="ham-merged-n"`,
		`ctx.workers = stageTrackerWorkers(window._lastStatus);`,
		`ctx.events = _stageTrackerEvents;`,
		`stageTrackerNoteEvents(data); stageTrackerResultToastsSync(data); stageTrackerPendingExpire(data);`,
		`stageTrackerCueAgeSync(el, nowMs);`,
		`function celebrateRow(rowEl, opts) {`,
		`const rows = stageTrackerPinnedRows(el, [{ key: STAGE_TRACKER_HEADER_KEY, html: stageTrackerHeaderHtml(stages) }]`,
		`if (!visible.length && !Object.keys(_stageTrackerCelebrating).length) {`,
		`data-change-action="stageTrackerCelebrateChange"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing tracker-cues wiring %q", want)
		}
	}
	start := strings.Index(html, `/* ---- tracker-cues: the Activity Monitor's shared cue vocabulary`)
	end := strings.Index(html, `.ham-rm-preview .cue-token { display: none; }`)
	if start < 0 || end < start {
		t.Fatal("tracker-cues CSS block not found")
	}
	css := html[start:end]
	for _, bad := range []string{"#0", "#1", "#2", "#3", "#4", "#5", "#6", "#7", "#8", "#9", "#a", "#b", "#c", "#d", "#e", "#f", "rgb(", "rgba(", "hsl("} {
		if strings.Contains(strings.ToLower(css), bad) {
			t.Errorf("tracker-cues CSS must use tokens only, found %q", bad)
		}
	}
	// Enterprise tone: no game words reach the page's operator-facing CSS/JS.
	for _, bad := range []string{"frog", "Frogger", "cue-hop", "cue-hit", "cue-home"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html must not carry game vocabulary %q", bad)
		}
	}
}

// TestStageTrackerCuesNode: the pure pieces — which rows an agent is on,
// the working / waiting tooltip lead, the row's cue classes, the event
// line's plain words and its expiry, and the merged-today counter.
func TestStageTrackerCuesNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL")+"\n"+
		jsFunc(t, html, "stageTrackerItemKey")+"\n"+
		jsFunc(t, html, "stageTrackerActorKind")+"\n"+
		stageTrackerCuesJS(t, html)+`
const assert = require('assert');
const now = Date.parse('2026-10-28T16:30:00Z');
const status = { agents: [
  { name: 'scout', busy: 'working', lastAction: { repo: 'hivecommons/hive', number: 14947, action: 'agent_comment_created', at: new Date(now - 90000).toISOString(), current: true } },
  { name: 'idle', busy: 'idle', lastAction: { repo: 'hivecommons/hive', number: 1, action: 'agent_pr_created', at: '', current: true } },
  { name: 'stale', busy: 'working', lastAction: { repo: 'hivecommons/hive', number: 2, action: 'agent_pr_created', at: '', current: false } },
  { name: 'short', busy: 'working', lastAction: { repo: 'crux', number: 30, action: 'agent_pr_reviewed', at: '', current: true } },
] };
const workers = stageTrackerWorkers(status);
assert.deepStrictEqual(Object.keys(workers).sort(), ['crux#30', 'hivecommons/hive#14947'], 'only agents working now on a current item count');
const pr = { repo: 'hivecommons/hive', kind: 'pr', number: 14947, stage: 50, caption: '' };
const issue = { repo: 'hivecommons/hive', kind: 'issue', number: 14938, stage: 40, linked_prs: [{ number: 14947, stage: 50 }] };
const other = { repo: 'hivecommons/hive', kind: 'issue', number: 5, stage: 40, caption: 'Waiting on a reviewer' };
assert.strictEqual(stageTrackerWorkingOn(pr, workers).agent, 'scout', 'own row');
assert.strictEqual(stageTrackerWorkingOn(issue, workers).agent, 'scout', 'through the nested PR');
assert.strictEqual(stageTrackerWorkingOn({ repo: 'hivecommons/crux', kind: 'pr', number: 30 }, workers).agent, 'short', 'a bare repo name in the audit matches on its tail');
assert.strictEqual(stageTrackerWorkingOn(other, workers), null);
assert.strictEqual(stageTrackerCueLead(pr, workers['hivecommons/hive#14947'], now), 'Working now: scout commented · 1m ago — ');
assert.strictEqual(stageTrackerCueLead(other, null, now), '', 'nobody working → no lead; the tooltip already says where it stands and whose move it is');
assert.strictEqual(stageTrackerCueClasses(pr, { workers }), ' cue-working');
assert.strictEqual(stageTrackerCueClasses(Object.assign({}, other, { modifiers: ['regressed'] }), { workers }), ' cue-setback');
assert.strictEqual(stageTrackerCueClasses({ stage: STAGE_TRACKER_TERMINAL }, { workers }), '', 'an already-merged PR does not glow on every render');
assert.strictEqual(stageTrackerCueClasses(other, { workers }), '');

// Event line: plain words, stage first, then what it waits on.
const stages = [{ number: 40, label: 'Claimed' }, { number: 50, label: 'CI / Review' }, { number: 85, label: 'Needs decision' }];
const st = (stage, mods, caption) => ({ stage, label: '', caption: caption || '', mods: (mods || []).slice().sort() });
assert.strictEqual(stageTrackerEventText(st(40), st(50), stages), 'moved to CI / Review');
assert.strictEqual(stageTrackerEventText(st(50), st(40, [], 'a reviewer requested changes'), stages), 'fell back to Claimed — a reviewer requested changes');
assert.strictEqual(stageTrackerEventText(st(50), st(STAGE_TRACKER_TERMINAL), stages), 'merged');
assert.strictEqual(stageTrackerEventText(st(50), st(50, ['needs-human']), stages), 'waiting on a person');
assert.strictEqual(stageTrackerEventText(st(50, ['hold']), st(50), stages), 'hold released');
assert.strictEqual(stageTrackerEventText(st(50, [], 'a'), st(50, [], 'CI running'), stages), 'CI running');
assert.strictEqual(stageTrackerEventText(st(50), st(50), stages), '');
const prev = stageTrackerStateMap([issue]);
const next = stageTrackerStateMap([Object.assign({}, issue, { linked_prs: [{ number: 14947, stage: 85, caption: 'Needs human decision' }] })]);
let events = stageTrackerEventsNext({}, prev, next, stages, now);
assert.deepStrictEqual(Object.keys(events), ['hivecommons/hive/pr#14947']);
assert.strictEqual(events['hivecommons/hive/pr#14947'].text, 'moved to Needs decision');
assert.deepStrictEqual(stageTrackerEventsNext({}, null, next, stages, now), {}, 'the first payload never writes a line for everything');
events = stageTrackerEventsNext(events, next, next, stages, now + STAGE_TRACKER_EVENT_MS + 1);
assert.deepStrictEqual(events, {}, 'lines expire');
_stageTrackerEvents = {};
stageTrackerNoteOwnerEvent('k', 'approved', now);
let line = stageTrackerEventHtml('k', { events: _stageTrackerEvents, nowMs: now });
assert.ok(line.startsWith('<p class="cue-event" data-cue-key="k"><span class="cue-event-who">You:</span> <span class="cue-event-text">approved</span>'), line);
assert.ok(/<span class="cue-event-age" data-at="\d+"><\/span>/.test(line), 'the age is filled in place, not baked in');
line = stageTrackerEventHtml('k', { events: _stageTrackerEvents, nowMs: now + STAGE_TRACKER_EVENT_MS * 0.8 });
assert.ok(line.includes('cue-event cue-event-fading'), 'dims for its last third');
assert.strictEqual(stageTrackerEventHtml('none', { events: _stageTrackerEvents }), '');

// Merged today: seeded from merged_24h since local midnight, grown live, newest first in the tooltip.
const midnight = stageTrackerLocalMidnight(now);
const d = { items: [{ repo: 'hivecommons/hive', kind: 'issue', number: 1, stage: 10 }], merged_24h: [
  { repo: 'hive', number: 100, merged_at: new Date(midnight + 3600000).toISOString() },
  { repo: 'hivecommons/hive', number: 99, merged_at: new Date(midnight - 60000).toISOString() },
  { repo: 'hivecommons/hive', number: 101, merged_at: new Date(midnight + 7200000).toISOString() },
] };
let today = stageTrackerMergedTodayNext({}, d, [], now);
assert.deepStrictEqual(Object.keys(today).sort(), ['hivecommons/hive/pr#100', 'hivecommons/hive/pr#101'], 'yesterday is out; a short repo name resolves');
today = stageTrackerMergedTodayNext(today, { items: [] }, ['hivecommons/hive/pr#102'], now + 1000);
assert.strictEqual(Object.keys(today).length, 3, 'a live merge adds');
let txt = stageTrackerMergedTodayText(today, now + 1000);
assert.strictEqual(txt.count, 3);
assert.ok(txt.title.startsWith('Merged today: 3\n' + 'hivecommons/hive#102 · '), txt.title);
assert.strictEqual(txt.title.split('\n').length, 4);
today = stageTrackerMergedTodayNext(today, { items: [] }, [], midnight + 86400000 + 1);
assert.deepStrictEqual(today, {}, 'resets at local midnight');
assert.strictEqual(stageTrackerMergedTodayText({}, now).title, 'Merged today: 0 — resets at midnight');
console.log('ok');
`)
}

// TestStageTrackerCelebrateNode (owner, 12:15 ET): on a merge the row is
// pinned past the payload that dropped it, its ✓ cell glows, a token flies
// to the "Merged today" count which ticks +1 (zap on arrival, sound on),
// then the row takes its normal exit; several merges stagger 200 ms apart
// under a flight cap; reduced motion flies nothing and ticks at once; the
// toggle off keeps the old immediate zap and no pin.
func TestStageTrackerCelebrateNode(t *testing.T) {
	html := indexHTML(t)
	lifted := strings.Join([]string{
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		jsConstLine(t, html, "STAGE_TRACKER_EVENT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGED_TIP_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_PIN_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_STAGGER_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_FLY_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS"),
		jsConstLine(t, html, "STAGE_TRACKER_TOKEN_SVG"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_MAX_PLAYS"),
		jsFunc(t, html, "stageTrackerCelebrateSetting"),
		jsFunc(t, html, "stageTrackerSetCelebrate"),
		jsFunc(t, html, "stageTrackerLocalMidnight"),
		jsFunc(t, html, "stageTrackerMergedTodayNext"),
		jsFunc(t, html, "stageTrackerMergedTodayText"),
		jsFunc(t, html, "stageTrackerMergedTodaySync"),
		jsFunc(t, html, "stageTrackerMergedCountDraw"),
		jsFunc(t, html, "stageTrackerCelebrateTick"),
		jsFunc(t, html, "stageTrackerCelebrateRowKey"),
		jsFunc(t, html, "stageTrackerCelebrateMerges"),
		jsFunc(t, html, "stageTrackerPinnedRows"),
		jsFunc(t, html, "stageTrackerScheduleRelease"),
		jsFunc(t, html, "celebrateRow"),
		jsFunc(t, html, "stageTrackerEventLineInject"),
		jsFunc(t, html, "stageTrackerEventHtml"),
		jsFunc(t, html, "stageTrackerNoteOwnerEvent"),
		jsFunc(t, html, "stageTrackerCueAgeSync"),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsFunc(t, html, "stageTrackerAfter"),
		jsFunc(t, html, "stageTrackerTransient"),
		jsFunc(t, html, "stageTrackerNoteMerges"),
		jsFunc(t, html, "stageTrackerPRKey"),
		jsFunc(t, html, "stageTrackerMergeState"),
		jsFunc(t, html, "stageTrackerCarryOpen"),
		jsFunc(t, html, "stageTrackerNewlyMerged"),
	}, "\n")
	runStageTrackerRowActionsScript(t, lifted+`
const assert = require('assert');
// ---- fake clock + DOM ----
let clock = Date.parse('2026-10-28T16:30:00Z');
Date.now = () => clock;
let timers = [];
global.setTimeout = (fn, ms) => { const id = timers.length + 1; timers.push({ id, at: clock + (ms || 0), fn }); return id; };
function tick(ms) { const until = clock + ms; while (true) { const due = timers.filter(x => x.at <= until).sort((a, b) => a.at - b.at)[0]; if (!due) break; timers = timers.filter(x => x !== due); clock = Math.max(clock, due.at); due.fn(); } clock = until; }
let idSeq = 0;
function el(tag, attrs) {
  const e = { tag, id: ++idSeq, className: '', textContent: '', title: '', innerHTML: '', children: [], attrs: Object.assign({}, attrs || {}), parentNode: null, style: { props: {}, setProperty(k, v) { this.props[k] = v; } }, _ev: {}, offsetWidth: 10,
    get dataset() { const d = {}; for (const k of Object.keys(e.attrs)) if (k.startsWith('data-')) d[k.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = e.attrs[k]; return d; },
    classList: { add(c) { if (!e.className.split(' ').includes(c)) e.className = (e.className + ' ' + c).trim(); }, remove(c) { e.className = e.className.split(' ').filter(x => x && x !== c).join(' '); }, contains(c) { return e.className.split(' ').includes(c); } },
    appendChild(c) { if (c.parentNode) c.parentNode.children = c.parentNode.children.filter(x => x !== c); c.parentNode = e; e.children.push(c); return c; },
    insertBefore(c, ref) { c.parentNode = e; const i = ref ? e.children.indexOf(ref) : -1; if (i < 0) e.children.push(c); else e.children.splice(i, 0, c); return c; },
    replaceWith(n) { if (!e.parentNode) return; const p = e.parentNode; p.children.splice(p.children.indexOf(e), 1, n); n.parentNode = p; e.parentNode = null; },
    remove() { if (e.parentNode) e.parentNode.children = e.parentNode.children.filter(c => c !== e); e.parentNode = null; },
    getAttribute(k) { return k in e.attrs ? e.attrs[k] : null; }, setAttribute(k, v) { e.attrs[k] = String(v); },
    addEventListener(n, fn) { e._ev[n] = fn; }, removeEventListener(n) { delete e._ev[n]; },
    getBoundingClientRect() { return e.rect || { left: 0, top: 0, width: 10, height: 10 }; },
    get outerHTML() { return '<div class="' + e.className + '"></div>'; },
    querySelector(sel) { return e.querySelectorAll(sel)[0] || null; },
    querySelectorAll(sel) { const out = []; const m = sel.match(/^\[([\w-]+)="(.*)"\]$/); const cls = sel.match(/^\.([\w-]+)/); const walk = n => { for (const c of n.children) { if (m && c.attrs[m[1]] === m[2].replace(/\\(.)/g, '$1')) out.push(c); else if (cls && c.classList.contains(cls[1]) && (!sel.includes('[data-at]') || c.attrs['data-at'] != null)) out.push(c); walk(c); } }; walk(e); return out; },
    closest(sel) { let n = e; while (n) { if (n.attrs && n.attrs['data-ham-key'] != null) return n; n = n.parentNode; } return null; },
  };
  return e;
}
global.CSS = { escape: s => String(s).replace(/([^\w-])/g, '\\$1') };
const body = el('body'); const panel = el('div'); const chip = el('span', { id: 'activity-monitor-merged' }); const n = el('span'); n.classList.add('ham-merged-n'); n.textContent = '0'; chip.appendChild(n); chip.rect = { left: 500, top: 10, width: 60, height: 20 };
global.document = { body, hidden: false, getElementById(id) { return id === 'activity-monitor-panel' ? panel : id === 'activity-monitor-merged' ? chip : null; }, createElement(tag) { if (tag === 'template') { const tpl = el('template'); Object.defineProperty(tpl, 'innerHTML', { set(v) { const c = el('p'); c.classList.add('cue-event'); c.attrs['data-cue-key'] = (v.match(/data-cue-key="([^"]*)"/) || [])[1]; c.textContent = v.replace(/<[^>]+>/g, ''); const age = el('span'); age.classList.add('cue-event-age'); age.attrs['data-at'] = (v.match(/data-at="(\d+)"/) || [])[1]; c.appendChild(age); tpl.content = { firstElementChild: c }; } }); return tpl; } return el(tag); } };
const store = {}; global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem(k, v) { store[k] = String(v); } };
let reduced = false; function stageTrackerReducedMotion() { return reduced; }
let soundOn = true; function stageTrackerSoundGate() { return soundOn; }
const zaps = []; function stageTrackerPlayZap(c) { zaps.push(c); }
let renders = 0; function renderStageTracker() { renders++; }
const srcs = new Map(); function stageTrackerRowSrcOf(node) { return srcs.get(node); }
let _stageTrackerEvents = {};
const STAGE_TRACKER_TERMINAL = 110;
var _stageTrackerMergeState = null, _stageTrackerZapped = {}, _stageTrackerMergedToday = {}, _stageTrackerMergedTodayCount = -1, _stageTrackerCelebrating = {}, _stageTrackerCelebratePending = {}, _stageTrackerFlights = 0, _stageTrackerReleaseTimer = 0;
function row(key) { const r = el('div', { 'data-ham-key': key }); const strip = el('div'); strip.classList.add('ham-strip-row'); const cap = el('li'); cap.classList.add('ham-seg'); cap.classList.add('cap'); cap.rect = { left: 300, top: 400, width: 20, height: 20 }; strip.appendChild(cap); r.appendChild(strip); srcs.set(r, '<row ' + key + '>'); return r; }
const header = { key: '__header__', html: 'h' };
const items = (...nums) => ({ items: nums.map(x => ({ repo: 'o/r', kind: 'pr', number: x, stage: 50 })) });
const payloadMerged = (...nums) => ({ items: [], recently_merged: nums.map(x => ({ repo: 'o/r', number: x, merged_at: new Date(clock).toISOString() })) });

// 1. A merge: pinned ≥ 2 s, glow, token to the counter, count ticks, zap on arrival, then released.
panel.appendChild(row('o/r/pr#1')); panel.appendChild(row('o/r/pr#2'));
stageTrackerNoteMerges(items(1, 2));
stageTrackerNoteMerges(payloadMerged(1));
assert.deepStrictEqual(zaps, [], 'with the celebration on the zap waits for the token to land');
let rows = stageTrackerPinnedRows(panel, [header, { key: 'o/r/pr#2', html: 'x' }], clock);
assert.deepStrictEqual(rows.map(r => r.key), ['__header__', 'o/r/pr#2', 'o/r/pr#1'], 'the merged row is pinned although the payload dropped it');
assert.strictEqual(rows[2].html, '<row o/r/pr#1>', 'replayed as the node it already has, so the reconcile keeps it');
assert.ok(rows[2].pinned);
tick(0);
const r1 = panel.children[0];
assert.ok(r1.classList.contains('cue-merged'), 'row glows: ' + r1.className);
assert.ok(r1.children.some(c => c.classList.contains('cue-event') && /merged #1 · by hive/.test(c.textContent)), 'the event line says what happened');
assert.strictEqual(body.children.length, 1, 'one token in flight'); assert.ok(body.children[0].classList.contains('cue-token'));
assert.strictEqual(body.children[0].style.props['--cue-dx'], (530 - 310) + 'px'); assert.strictEqual(body.children[0].style.props['--cue-dy'], (20 - 410) + 'px');
assert.strictEqual(n.textContent, '0', 'the count waits for the token');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(body.children.length, 0, 'token gone'); assert.strictEqual(n.textContent, '1', 'count ticked'); assert.ok(chip.classList.contains('cue-tick'), 'pop'); assert.deepStrictEqual(zaps, [1], 'zap on arrival');
assert.ok(chip.title.startsWith('Merged today: 1\no/r#1 · '), chip.title);
rows = stageTrackerPinnedRows(panel, [header], clock);
assert.ok(rows.some(r => r.key === 'o/r/pr#1'), 'still pinned before 2.5 s (' + (clock) + ')');
tick(STAGE_TRACKER_CELEBRATE_PIN_MS);
assert.ok(renders >= 1, 'a release render was scheduled');
rows = stageTrackerPinnedRows(panel, [header], clock);
assert.ok(!rows.some(r => r.key === 'o/r/pr#1'), 'released after the pin window: the row takes its normal exit');

// 2. Several merges stagger 200 ms apart; the flight cap ticks the rest at once; sound off → no zap.
soundOn = false; chip.classList.remove('cue-tick'); zaps.length = 0;
for (const k of [3, 4, 5, 6, 7]) panel.appendChild(row('o/r/pr#' + k));
stageTrackerNoteMerges(items(3, 4, 5, 6, 7));
stageTrackerNoteMerges(payloadMerged(3, 4, 5, 6, 7));
tick(0); assert.strictEqual(body.children.length, 1, 'first token at once');
tick(STAGE_TRACKER_CELEBRATE_STAGGER_MS); assert.strictEqual(body.children.length, 2, 'second 200 ms later');
tick(STAGE_TRACKER_CELEBRATE_STAGGER_MS); assert.strictEqual(body.children.length, 3);
tick(STAGE_TRACKER_CELEBRATE_STAGGER_MS); assert.strictEqual(body.children.length, 3, 'capped at ' + STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS + ' flights');
assert.strictEqual(n.textContent, '2', 'the capped one ticked without flying');
tick(2000);
assert.strictEqual(body.children.length, 0); assert.strictEqual(n.textContent, '6'); assert.deepStrictEqual(zaps, [], 'muted');

// 3. Reduced motion: nothing flies, the count ticks and the row glows.
reduced = true; panel.appendChild(row('o/r/pr#8'));
stageTrackerNoteMerges(items(8)); stageTrackerNoteMerges(payloadMerged(8)); tick(0);
assert.strictEqual(body.children.length, 0, 'no token'); assert.strictEqual(n.textContent, '7');
assert.ok(panel.querySelector('[data-ham-key="o/r/pr#8"]').classList.contains('cue-merged'));
reduced = false;

// 4. A merged PR not on the board still counts.
stageTrackerNoteMerges(items(9)); stageTrackerNoteMerges(payloadMerged(9)); tick(0);
assert.strictEqual(n.textContent, '8'); assert.strictEqual(body.children.length, 0);

// 5. Toggle off: immediate zap as before, no pin.
tick(STAGE_TRACKER_CELEBRATE_PIN_MS + 1000);
stageTrackerSetCelebrate(false); soundOn = true; zaps.length = 0; panel.appendChild(row('o/r/pr#10'));
stageTrackerNoteMerges(items(10)); stageTrackerNoteMerges(payloadMerged(10));
assert.deepStrictEqual(zaps, [1], 'zap at once');
assert.deepStrictEqual(stageTrackerPinnedRows(panel, [header], clock).map(r => r.key), ['__header__'], 'nothing pinned');
assert.strictEqual(n.textContent, '9', 'the counter still counts');
// celebrateRow is reusable on its own.
stageTrackerSetCelebrate(true);
const r11 = row('o/r/issue#11'); panel.appendChild(r11);
const out = celebrateRow(r11, { kind: 'closed-by-merge', number: 77 });
assert.deepStrictEqual(out, { flew: false }); assert.ok(r11.classList.contains('cue-merged'));
assert.ok(r11.children.some(c => c.classList.contains('cue-event') && /closed by merge of #77/.test(c.textContent)));
console.log('ok');
`)
}
