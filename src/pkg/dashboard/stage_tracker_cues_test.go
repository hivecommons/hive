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
		jsConstLine(t, html, "STAGE_TRACKER_MERGED_LIST_MAX"),
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
		jsFunc(t, html, "stageTrackerMergedIdent"),
		jsFunc(t, html, "stageTrackerMergedNext"),
		jsFunc(t, html, "stageTrackerPRTitles"),
		jsFunc(t, html, "stageTrackerMergedText"),
		jsFunc(t, html, "stageTrackerMergedByText"),
		jsFunc(t, html, "stageTrackerMergedPopHtml"),
		jsFunc(t, html, "stageTrackerPRKey"),
		"let _stageTrackerEvents = {}; var _stageTrackerPRTitle = {};",
	}, "\n") + "\n"
}

// TestStageTrackerCuesWiring: the shared cue vocabulary the three Activity
// Monitor PRs compose on (owner, 2026-10-28) is in the page — class names,
// the merged counter chip — and the CSS is tokens only, with the
// reduced-motion fallback stopping every animation. No chevron glyph
// rotates: the owner removed the spinning status symbols
// (hivecommons/hive#15187).
func TestStageTrackerCuesWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`/* ---- tracker-cues: the Activity Monitor's shared cue vocabulary`,
		`.cue-working .ham-seg.active:not(.cap) .ham-active-ico, .cue-spin { animation: none; }`,
		`.cue-acting-spin { color: var(--cue-work); animation: none; border-color: currentColor; }`,
		`.cue-acting > .ham-strip-row .ham-seg.active {`,
		`.cue-event {`, `.cue-setback .cue-event-who {`, `.cue-merged .cue-event-who {`,
		`.cue-merged .ham-seg.cap {`, `.cue-flight {`, `.cue-flight.cue-flight-fade {`, `.ham-merged-count {`, `.ham-merged-count.cue-tick {`,
		`.ham-merged-pop {`, `.merge-banners {`, `.merge-banner {`,
		`@media (prefers-reduced-motion: reduce) { .cue-working .ham-seg.active .ham-active-ico, .cue-spin, .cue-acting .ham-seg.active, .cue-event { animation: none; } }`,
		`@media (prefers-reduced-motion: reduce) { .cue-flight { animation-name: cue-flight-fade; }`,
		`id="activity-monitor-merged"`, `class="ham-merged-n"`, `class="ham-merged-sub"`,
		// The chip and the column header strip stick under the navbar
		// while the list scrolls (hivecommons/hive#15096: the chip sat
		// 2,300 px above the operator's viewport when the merge played).
		`<div class="ham-sticky" id="activity-monitor-sticky">`,
		`.ham-sticky { position: sticky; top: var(--navbar-sticky-height);`,
		`.ham-cols { position: sticky; top: calc(var(--navbar-sticky-height) + var(--ham-sticky-bar-h));`,
		`id="activity-monitor-merged-pop" role="dialog" aria-label="Recent merges" hidden`,
		`data-action="hamMergedToggle"`, `data-change-action="stageTrackerCelebrateGlyphChange"`,
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
	end := strings.Index(html, `.ham-rm-preview .cue-flight { animation-name: cue-flight-fade; }`)
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
	// The chip is no longer a per-tab "Merged today" tally.
	if strings.Contains(html, "Merged today") {
		t.Error("index.html still says \"Merged today\"; the chip is the server's all-time Merged count")
	}
	for _, bad := range []string{`setInterval('merge`, `class="merge-banner" style=`} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html must not carry %q", bad)
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
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR_NONE")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_NOBODY_LEAD")+"\n"+
		jsFunc(t, html, "stageTrackerStageActor")+"\n"+
		jsFunc(t, html, "stageTrackerActorKind")+"\n"+
		jsFunc(t, html, "stageTrackerActorTip")+"\n"+
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

// Merged chip: the server's ledger is the number; this tab's unlisted live merges ride on top.
const midnight = stageTrackerLocalMidnight(now);
const merged = { today: 4, total_7d: 9, total_30d: 20, all_time: 33, tz: 'EDT', day_start: new Date(midnight).toISOString(), recent: [
  { repo: 'hivecommons/hive', number: 101, title: 'fix: b', merged_at: new Date(midnight + 7200000).toISOString(), merged_by: 'hive', lane: 'hive' },
  { repo: 'hivecommons/hive', number: 100, title: 'fix: a', merged_at: new Date(midnight + 3600000).toISOString(), merged_by: 'clubanderson' },
] };
const d = { items: [{ repo: 'hivecommons/hive', kind: 'issue', number: 1, stage: 10, linked_prs: [{ number: 102, title: 'feat: c' }] }], merged };
Object.assign(_stageTrackerPRTitle, stageTrackerPRTitles(d)); // what stageTrackerMergedSync does before folding a payload
let ms = stageTrackerMergedNext({}, d, [], now);
assert.strictEqual(ms.server.all_time, 33); assert.strictEqual(ms.server.recent.length, 2); assert.deepStrictEqual(ms.local, {});
let txt = stageTrackerMergedText(ms, now, 0);
assert.strictEqual(txt.count, 33, 'first render shows the server number');
assert.strictEqual(txt.sub, '4 today · 9 this week');
assert.ok(txt.title.startsWith('Merged: 33 all time\n4 today · 9 this week (EDT)\nhivecommons/hive#101 fix: b · '), txt.title);
ms = stageTrackerMergedNext(ms, { items: [] }, ['hivecommons/hive/pr#102'], now + 1000);
assert.deepStrictEqual(Object.keys(ms.local), ['hivecommons/hive/pr#102'], 'a live merge the server has not listed yet is added');
assert.strictEqual(ms.local['hivecommons/hive/pr#102'].title, 'feat: c', 'titled from the last payload that drew it');
txt = stageTrackerMergedText(ms, now + 1000, 0);
assert.strictEqual(txt.count, 34); assert.strictEqual(txt.sub, '5 today · 10 this week');
assert.strictEqual(txt.recent[0].number, 102, 'this tab\'s merge leads the list'); assert.strictEqual(txt.recent.length, 3);
ms = stageTrackerMergedNext(ms, { items: [], merged: Object.assign({}, merged, { all_time: 34, today: 5, total_7d: 10, recent: [{ repo: 'hive', number: 102, merged_at: new Date(now).toISOString() }].concat(merged.recent) }) }, [], now + 2000);
assert.deepStrictEqual(ms.local, {}, 'once the server lists it (even by short repo name) the local copy goes');
assert.strictEqual(stageTrackerMergedText(ms, now + 2000, 0).count, 34, 'and the number is unchanged');
ms = stageTrackerMergedNext(ms, { items: [] }, ['hivecommons/hive/pr#103'], now + 3000, { 'hivecommons/hive/pr#103': true });
assert.deepStrictEqual(ms.local, {}, 'a merge whose glyph is still in flight is not counted yet');
assert.strictEqual(stageTrackerMergedText(ms, now + 3000, 1).count, 33, 'holdback subtracts');
assert.strictEqual(stageTrackerMergedText({}, now, 0).count, 0);
assert.strictEqual(stageTrackerMergedText({}, now, 0).sub, '0 today · 0 this week');
assert.strictEqual(stageTrackerMergedByText('hive'), 'by hive'); assert.strictEqual(stageTrackerMergedByText('bob'), 'by @bob'); assert.strictEqual(stageTrackerMergedByText(''), '');
const pop = stageTrackerMergedPopHtml(stageTrackerMergedText(ms, now, 0), now);
assert.ok(pop.includes('<span>Recent merges</span>'), pop);
assert.ok(pop.includes('#102</span>') && pop.includes('>fix: b<') && pop.includes('by hive · hive · ') && pop.includes('by @clubanderson'), pop);
assert.ok(pop.includes('34 all time · days roll at midnight EDT'), pop);
assert.ok(stageTrackerMergedPopHtml(stageTrackerMergedText({}, now, 0), now).includes('No merges recorded yet'));
console.log('ok');
`)
}

// TestStageTrackerCelebrateNode (owner, 12:15 ET): on a merge the row is
// pinned past the payload that dropped it, its ✓ cell glows, a token flies
// to the "Merged today" count which ticks +1 (zap on arrival, sound on),
// then the row takes its normal exit; several merges stagger 200 ms apart
// under a flight cap; reduced motion flies nothing and ticks at once; the
// toggle off keeps the old immediate zap and no pin.
// stageTrackerCelebrateJS lifts the merge detector and the celebration
// module (counter, token flight, pinned rows) out of index.html.
func stageTrackerCelebrateJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		jsConstLine(t, html, "STAGE_TRACKER_EVENT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGED_TIP_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGED_LIST_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_GLYPH_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_GLYPHS"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_GLYPH_DEFAULT"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_PIN_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_STAGGER_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_FLY_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_EXIT_PAD"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_BANNER_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_BANNER_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_SOUND_MAX_PLAYS"),
		jsFunc(t, html, "stageTrackerCelebrateSetting"),
		jsFunc(t, html, "stageTrackerSetCelebrate"),
		jsFunc(t, html, "stageTrackerCelebrateGlyphId"),
		jsFunc(t, html, "stageTrackerCelebrateGlyph"),
		jsFunc(t, html, "stageTrackerSetCelebrateGlyph"),
		jsFunc(t, html, "stageTrackerLocalMidnight"),
		jsFunc(t, html, "stageTrackerMergedChip"),
		jsFunc(t, html, "stageTrackerMergedIdent"),
		jsFunc(t, html, "stageTrackerMergedNext"),
		jsFunc(t, html, "stageTrackerPRTitles"),
		jsFunc(t, html, "stageTrackerMergedText"),
		jsFunc(t, html, "stageTrackerMergedByText"),
		jsFunc(t, html, "stageTrackerMergedPopHtml"),
		jsFunc(t, html, "stageTrackerMergedSync"),
		jsFunc(t, html, "stageTrackerMergedHoldback"),
		jsFunc(t, html, "stageTrackerMergedCountDraw"),
		jsFunc(t, html, "stageTrackerMergedPopDraw"),
		jsFunc(t, html, "hamMergedToggle"),
		jsFunc(t, html, "stageTrackerCelebrateTick"),
		jsFunc(t, html, "stageTrackerCelebrateRowKey"),
		jsFunc(t, html, "stageTrackerCelebrateMerges"),
		jsFunc(t, html, "stageTrackerPinnedRows"),
		jsFunc(t, html, "stageTrackerScheduleRelease"),
		jsFunc(t, html, "stageTrackerRectInView"),
		jsFunc(t, html, "stageTrackerFlightTarget"),
		jsFunc(t, html, "celebrateRow"),
		jsFunc(t, html, "stageTrackerMergeBannerText"),
		jsFunc(t, html, "stageTrackerMergeBanner"),
		jsFunc(t, html, "stageTrackerMergeBannerDismiss"),
		jsFunc(t, html, "hamBannerDismiss"),
		jsFunc(t, html, "hamBannerShowRow"),
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
		jsFunc(t, html, "stageTrackerRowPRKeys"),
		jsFunc(t, html, "stageTrackerPRHosts"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerVisibleRow"),
		jsFunc(t, html, "stageTrackerTestCelebration"),
		jsConstLine(t, html, "STAGE_TRACKER_CELEBRATE_TEST_KEY"),
	}, "\n")
}

// stageTrackerCelebrateFixture is the fake clock + DOM the celebration
// tests run against: timers advance with tick(ms); elements support the
// few selectors the module uses (`[a="v"]`, `[a~="v"][data-ham-key]`,
// `.cls`, `.ham-row[data-ham-key] .ham-seg.cap`).
const stageTrackerCelebrateFixture = `
const assert = require('assert');
// ---- fake clock + DOM ----
let clock = Date.parse('2026-10-28T16:30:00Z');
Date.now = () => clock;
let timers = [];
let timerSeq = 0;
global.setTimeout = (fn, ms) => { const id = ++timerSeq; timers.push({ id, at: clock + (ms || 0), fn }); return id; };
global.clearTimeout = id => { timers = timers.filter(x => x.id !== id); };
global.window = { innerWidth: 1280, innerHeight: 800 };
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
    querySelectorAll(sel) { const out = []; const unesc = v => v.replace(/\\(.)/g, '$1'); const m = sel.match(/^\[([\w-]+)="(.*)"\]$/); const word = sel.match(/^\[([\w-]+)~="(.*)"\]\[data-ham-key\]$/); const cls = sel.match(/^\.([\w-]+)/); const capOfRow = sel === '.ham-row[data-ham-key] .ham-seg.cap'; const walk = n => { for (const c of n.children) { if (capOfRow) { if (c.classList.contains('cap') && c.closest('.ham-row') && c.closest('.ham-row').classList.contains('ham-row')) out.push(c); } else if (m && c.attrs[m[1]] === unesc(m[2])) out.push(c); else if (word && c.attrs['data-ham-key'] != null && String(c.attrs[word[1]] || '').split(' ').includes(unesc(word[2]))) out.push(c); else if (!m && !word && cls && c.classList.contains(cls[1]) && (!sel.includes('[data-at]') || c.attrs['data-at'] != null)) out.push(c); walk(c); } }; walk(e); return out; },
    closest(sel) { let n = e; while (n) { if (n.attrs && n.attrs['data-ham-key'] != null) return n; n = n.parentNode; } return null; },
  };
  return e;
}
global.CSS = { escape: s => String(s).replace(/([^\w-])/g, '\\$1') };
const body = el('body'); const panel = el('div'); const chip = el('button', { id: 'activity-monitor-merged' }); const n = el('span'); n.classList.add('ham-merged-n'); n.textContent = '0'; chip.appendChild(n); const sub = el('span'); sub.classList.add('ham-merged-sub'); chip.appendChild(sub); chip.rect = { left: 500, top: 10, width: 60, height: 20 };
const pop = el('div', { id: 'activity-monitor-merged-pop' }); pop.hidden = true; const banners = el('div'); const live = el('span');
const byId = { 'activity-monitor-panel': panel, 'activity-monitor-merged': chip, 'activity-monitor-merged-pop': pop, 'merge-banners': banners, 'activity-monitor-merged-live': live };
global.document = { body, hidden: false, getElementById(id) { return byId[id] || null; }, createElement(tag) { if (tag === 'template') { const tpl = el('template'); Object.defineProperty(tpl, 'innerHTML', { set(v) { const c = el('p'); c.classList.add('cue-event'); c.attrs['data-cue-key'] = (v.match(/data-cue-key="([^"]*)"/) || [])[1]; c.textContent = v.replace(/<[^>]+>/g, ''); const age = el('span'); age.classList.add('cue-event-age'); age.attrs['data-at'] = (v.match(/data-at="(\d+)"/) || [])[1]; c.appendChild(age); tpl.content = { firstElementChild: c }; } }); return tpl; } return el(tag); } };
const store = {}; global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem(k, v) { store[k] = String(v); } };
let reduced = false; function stageTrackerReducedMotion() { return reduced; }
let soundOn = true; function stageTrackerSoundGate() { return soundOn; }
const zaps = []; function stageTrackerPlayZap(c) { zaps.push(c); }
const toasts = []; function showToast(msg, type) { toasts.push([type, msg]); }
let renders = 0; function renderStageTracker() { renders++; }
const srcs = new Map(); function stageTrackerRowSrcOf(node) { return srcs.get(node); }
let _stageTrackerEvents = {};
const STAGE_TRACKER_TERMINAL = 110;
var _stageTrackerPRHost = {}; var _stageTrackerPRTitle = {}; var _stageTrackerMergeState = null, _stageTrackerZapped = {}, _stageTrackerMerged = { server: null, local: {} }, _stageTrackerMergedShown = 0, _stageTrackerCelebrating = {}, _stageTrackerCelebratePending = {}, _stageTrackerFlights = 0, _stageTrackerReleaseTimer = 0;
let settingsClosed = 0; function closeStageTrackerSettings() { settingsClosed++; }
const flights = () => body.children.filter(c => c.classList.contains('cue-flight'));
function row(key, prs) { const r = el('div', { 'data-ham-key': key }); r.classList.add('ham-row'); if (prs) r.attrs['data-ham-prs'] = prs; const strip = el('div'); strip.classList.add('ham-strip-row'); const cap = el('li'); cap.classList.add('ham-seg'); cap.classList.add('cap'); cap.rect = { left: 300, top: 400, width: 20, height: 20 }; strip.appendChild(cap); r.appendChild(strip); srcs.set(r, '<row ' + key + '>'); return r; }
const header = { key: '__header__', html: 'h' };
const items = (...nums) => ({ items: nums.map(x => ({ repo: 'o/r', kind: 'pr', number: x, stage: 50 })) });
const payloadMerged = (...nums) => ({ items: [], recently_merged: nums.map(x => ({ repo: 'o/r', number: x, merged_at: new Date(clock).toISOString() })) });

`

func TestStageTrackerCelebrateNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerCelebrateJS(t, html)+stageTrackerCelebrateFixture+`
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
assert.strictEqual(flights().length, 1, 'one glyph in flight'); assert.strictEqual(flights()[0].textContent, '🐝', 'the default glyph is the bee');
assert.strictEqual(flights()[0].style.props['--cue-dx'], (530 - 310) + 'px'); assert.strictEqual(flights()[0].style.props['--cue-dy'], (20 - 410) + 'px', 'the chip is in view: the flight ends on it');
assert.strictEqual(flights()[0].style.props['--cue-end-opacity'], '0.25');
assert.strictEqual(n.textContent, '0', 'the count waits for the glyph');
assert.strictEqual(banners.children.length, 1, 'a banner is up at once'); assert.ok(/#1<\/span> merged/.test(banners.children[0].innerHTML), banners.children[0].innerHTML); assert.ok(banners.children[0].innerHTML.includes('data-action="hamBannerShowRow" data-arg0="o/r/pr#1"'), 'show row link');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(flights().length, 0, 'glyph gone'); assert.strictEqual(n.textContent, '1', 'count bumped'); assert.ok(chip.classList.contains('cue-tick'), 'pop'); assert.deepStrictEqual(zaps, [1], 'zap on arrival');
assert.ok(chip.title.startsWith('Merged: 1 all time\n1 today · 1 this week\no/r#1 · '), chip.title);
assert.strictEqual(sub.textContent, '1 today · 1 this week');
assert.strictEqual(live.textContent, 'Merged: 1 all time, 1 today · 1 this week', 'announced for screen readers');
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
tick(0); assert.strictEqual(flights().length, 1, 'first glyph at once');
tick(STAGE_TRACKER_CELEBRATE_STAGGER_MS); assert.strictEqual(flights().length, 2, 'second 200 ms later');
tick(STAGE_TRACKER_CELEBRATE_STAGGER_MS); assert.strictEqual(flights().length, 3);
tick(STAGE_TRACKER_CELEBRATE_STAGGER_MS); assert.strictEqual(flights().length, 3, 'capped at ' + STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS + ' flights');
assert.strictEqual(n.textContent, '2', 'the capped one bumped without flying');
assert.strictEqual(banners.children.length, STAGE_TRACKER_MERGE_BANNER_MAX, 'banners stack, oldest dropped past the cap');
tick(2000);
assert.strictEqual(flights().length, 0); assert.strictEqual(n.textContent, '6'); assert.deepStrictEqual(zaps, [], 'muted');
tick(STAGE_TRACKER_MERGE_BANNER_MS);
assert.strictEqual(banners.children.length, 0, 'banners leave on their own after ' + STAGE_TRACKER_MERGE_BANNER_MS + ' ms');

// 3. Reduced motion: the glyph fades in place, the count bumps on the fade's end and the row glows.
reduced = true; panel.appendChild(row('o/r/pr#8'));
stageTrackerNoteMerges(items(8)); stageTrackerNoteMerges(payloadMerged(8)); tick(0);
assert.strictEqual(flights().length, 1, 'a fading glyph'); assert.ok(flights()[0].classList.contains('cue-flight-fade')); assert.strictEqual(flights()[0].style.props['--cue-dy'], '0px', 'no travel');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(flights().length, 0); assert.strictEqual(n.textContent, '7');
assert.ok(panel.querySelector('[data-ham-key="o/r/pr#8"]').classList.contains('cue-merged'));
reduced = false;

// 4. A merged PR not on the board still counts.
stageTrackerNoteMerges(items(9)); stageTrackerNoteMerges(payloadMerged(9)); tick(0);
assert.strictEqual(n.textContent, '8'); assert.strictEqual(flights().length, 0);
assert.ok(banners.children.some(b => /#9<\/span> merged/.test(b.innerHTML) && !b.innerHTML.includes('hamBannerShowRow')), 'its banner has no "show row" (no row)');

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

// TestStageTrackerCelebrateNestedPRRow replays the payload shape hive's hive
// serves (captured 2026-10-28 from /api/stage-tracker): a PR nested under
// its issue is drawn inline on the issue row — no sub-row, no
// data-ham-sub-key — and on merge the payload drops both the PR and the
// issue while recently_merged lists the PR. The flight must leave from the
// issue row; before the fix only the counter popped.
func TestStageTrackerCelebrateNestedPRRow(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerCelebrateJS(t, html)+stageTrackerCelebrateFixture+`
const before = { items: [
  { repo: 'hivecommons/hive', number: 14816, kind: 'issue', stage: 85, linked_prs: [{ number: 14822, stage: 85, url: 'https://github.com/hivecommons/hive/pull/14822' }] },
  { repo: 'hivecommons/hive', number: 14822, kind: 'pr', stage: 85, nested_under: 14816 },
  { repo: 'hivecommons/hive', number: 15009, kind: 'issue', stage: 40, linked_prs: [{ number: 15017, stage: 60 }, { number: 15020, stage: 85 }] },
  { repo: 'hivecommons/hive', number: 15017, kind: 'pr', stage: 60, nested_under: 15009 },
  { repo: 'hivecommons/hive', number: 15020, kind: 'pr', stage: 85, nested_under: 15009 },
  { repo: 'hivecommons/docs', number: 500, kind: 'pr', stage: 70 },
], recently_merged: [{ repo: 'hivecommons/hive', number: 14992, merged_at: '2026-10-28T18:47:47Z' }], merged_24h: [{ repo: 'hivecommons/hive', number: 14992, merged_at: '2026-10-28T18:47:47Z' }],
  merged: { today: 1, total_7d: 1, total_30d: 1, all_time: 1, recent: [{ repo: 'hivecommons/hive', number: 14992, merged_at: '2026-10-28T18:47:47Z', merged_by: 'hive' }] } };
const after = { items: before.items.filter(it => it.number !== 14816 && it.number !== 14822), recently_merged: [{ repo: 'hivecommons/hive', number: 14822, merged_at: new Date(clock).toISOString() }].concat(before.recently_merged), merged_24h: [{ repo: 'hivecommons/hive', number: 14822, merged_at: new Date(clock).toISOString() }].concat(before.merged_24h),
  merged: { today: 2, total_7d: 2, total_30d: 2, all_time: 2, recent: [{ repo: 'hivecommons/hive', number: 14822, merged_at: new Date(clock).toISOString(), merged_by: 'hive' }].concat(before.merged.recent) } };

// The row stamps the PR keys it draws (the real renderer does this via stageTrackerRowPRKeys).
assert.strictEqual(stageTrackerRowPRKeys(before.items[0]), 'hivecommons/hive/pr#14822');
assert.strictEqual(stageTrackerRowPRKeys(before.items[2]), 'hivecommons/hive/pr#15017 hivecommons/hive/pr#15020');
assert.strictEqual(stageTrackerRowPRKeys(before.items[5]), 'hivecommons/docs/pr#500');
assert.deepStrictEqual(stageTrackerPRHosts(before), {
  'hivecommons/hive/pr#14822': 'hivecommons/hive/issue#14816',
  'hivecommons/hive/pr#15017': 'hivecommons/hive/issue#15009',
  'hivecommons/hive/pr#15020': 'hivecommons/hive/issue#15009',
  'hivecommons/docs/pr#500': 'hivecommons/docs/pr#500',
});

// Board as drawn: the issue rows (inline PR, no sub-row) and the standalone PR row.
panel.appendChild(row('hivecommons/hive/issue#14816', 'hivecommons/hive/pr#14822'));
panel.appendChild(row('hivecommons/hive/issue#15009', 'hivecommons/hive/pr#15017 hivecommons/hive/pr#15020'));
panel.appendChild(row('hivecommons/docs/pr#500', 'hivecommons/docs/pr#500'));
stageTrackerNoteMerges(before);
assert.strictEqual(n.textContent, '1', 'hydrated from the server ledger, no flourish');
assert.strictEqual(stageTrackerCelebrateRowKey('hivecommons/hive/pr#14822', panel), 'hivecommons/hive/issue#14816', 'inline PR resolves to its issue row');
assert.strictEqual(stageTrackerCelebrateRowKey('hivecommons/hive/pr#15020', panel), 'hivecommons/hive/issue#15009');
assert.strictEqual(stageTrackerCelebrateRowKey('hivecommons/docs/pr#500', panel), 'hivecommons/docs/pr#500');

// The merge: the issue row is pinned and celebrated, a token flies, the count ticks on landing.
stageTrackerNoteMerges(after);
assert.deepStrictEqual(Object.keys(_stageTrackerCelebrating), ['hivecommons/hive/issue#14816'], 'the issue row is the celebrating row');
let rows = stageTrackerPinnedRows(panel, [{ key: '__header__', html: 'h' }, { key: 'hivecommons/hive/issue#15009', html: 'x' }, { key: 'hivecommons/docs/pr#500', html: 'y' }], clock);
assert.ok(rows.some(r => r.key === 'hivecommons/hive/issue#14816' && r.pinned), 'the dropped issue row is pinned');
assert.strictEqual(n.textContent, '1', 'the server already counts it: held back until the glyph lands');
tick(0);
const issueRow = panel.children[0];
assert.ok(issueRow.classList.contains('cue-merged'), 'issue row glows: ' + issueRow.className);
assert.ok(issueRow.children.some(c => c.classList.contains('cue-event') && /merged #14822 · by hive/.test(c.textContent)), 'event line names the merged PR');
assert.strictEqual(flights().length, 1, 'a glyph is in flight');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(flights().length, 0); assert.strictEqual(n.textContent, '2', 'count bumped on landing'); assert.deepStrictEqual(zaps, [1]);

// Host memory: a PR whose row left the DOM between renders (no stamp to find) still resolves through the last payload.
panel.children.length = 0;
panel.appendChild(row('hivecommons/hive/issue#15009'));
assert.strictEqual(stageTrackerCelebrateRowKey('hivecommons/hive/pr#15017', panel), 'hivecommons/hive/issue#15009', 'resolved via nested_under from the last payload');
assert.strictEqual(stageTrackerCelebrateRowKey('hivecommons/hive/pr#99999', panel), '', 'unknown PR: counter only');

// ⚙ → Merges → Test celebration: one flight from the first row in view, pop + zap, the count unchanged.
zaps.length = 0; chip.classList.remove('cue-tick');
const r = stageTrackerTestCelebration();
assert.deepStrictEqual(r, { flew: true, lands: true, faded: false }, JSON.stringify(toasts));
assert.strictEqual(flights().length, 1, 'test glyph in flight');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(n.textContent, '2', 'the test does not count'); assert.ok(chip.classList.contains('cue-tick'), 'but pops'); assert.deepStrictEqual(zaps, [1], 'and zaps');
assert.ok(!panel.children[0].children.some(c => c.classList.contains('cue-event')), 'no event line is written for a test');
tick(STAGE_TRACKER_CELEBRATE_PIN_MS + 1);
assert.ok(!panel.children[0].classList.contains('cue-merged'), 'test glow clears');
panel.children.length = 0;
assert.deepStrictEqual(stageTrackerTestCelebration(), { flew: false });
assert.ok(toasts.some(x => x[0] === 'warning' && /No row to celebrate on/.test(x[1])), JSON.stringify(toasts));
`)
}

// TestStageTrackerMergedChipHydrates: the chip shows the server's ledger
// number on the first render (no flourish), never goes backwards within a
// tab, holds a merge whose glyph is in flight back by one when the server
// already counts it, and the popover lists the recent merges.
func TestStageTrackerMergedChipHydrates(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerCelebrateJS(t, html)+stageTrackerCelebrateFixture+`
const merged = (allTime, recent, extra) => Object.assign({ today: 4, total_7d: 9, total_30d: 20, all_time: allTime, tz: 'UTC', day_start: new Date(stageTrackerLocalMidnight(clock)).toISOString(), recent }, extra || {});
const rec = (num, by, title) => ({ repo: 'o/r', number: num, title: title || ('fix ' + num), url: 'https://github.com/o/r/pull/' + num, merged_at: new Date(clock - 60000).toISOString(), merged_by: by || 'hive', lane: 'hive' });
// First render: the server number, no pop, no zap.
panel.appendChild(row('o/r/pr#1'));
stageTrackerNoteMerges({ items: [{ repo: 'o/r', kind: 'pr', number: 1, stage: 50, title: 'feat: one' }], merged: merged(33, [rec(31, 'clubanderson'), rec(30)]) });
assert.strictEqual(n.textContent, '33', 'hydrated from the server on first render');
assert.strictEqual(sub.textContent, '4 today · 9 this week');
assert.ok(!chip.classList.contains('cue-tick'), 'no pop on hydration'); assert.deepStrictEqual(zaps, []);
assert.ok(chip.title.startsWith('Merged: 33 all time\n4 today · 9 this week (UTC)\no/r#31 fix 31 · '), chip.title);
// Popover: click opens it with the recent list.
assert.strictEqual(hamMergedToggle(), true); assert.strictEqual(pop.hidden, false); assert.strictEqual(chip.attrs['aria-expanded'], 'true');
assert.ok(pop.innerHTML.includes('<span>Recent merges</span>'), pop.innerHTML);
assert.ok(pop.innerHTML.includes('href="https://github.com/o/r/pull/31"') && pop.innerHTML.includes('>fix 31<') && pop.innerHTML.includes('by @clubanderson') && pop.innerHTML.includes('by hive'), pop.innerHTML);
assert.ok(pop.innerHTML.includes('33 all time'), pop.innerHTML);
assert.strictEqual(hamMergedToggle(), false); assert.strictEqual(pop.hidden, true); assert.strictEqual(chip.attrs['aria-expanded'], 'false');
// A lower server number later (a roll before the ledger caught up) does not pull the chip down.
stageTrackerNoteMerges({ items: [{ repo: 'o/r', kind: 'pr', number: 1, stage: 50, title: 'feat: one' }], merged: merged(30, [rec(30)]) });
assert.strictEqual(n.textContent, '33', 'never backwards');
// The server already counts the merge (webhook beat the poll; the ledger's recent list is what settles it): the chip holds back by one until the glyph lands.
stageTrackerNoteMerges({ items: [], merged: merged(34, [rec(1, 'hive', 'feat: one'), rec(31, 'clubanderson'), rec(30)]) });
assert.strictEqual(n.textContent, '33', 'held back while the glyph flies');
tick(0); assert.strictEqual(flights().length, 1);
assert.ok(banners.children[0].innerHTML.includes('feat: one (hive, by hive, '), 'the banner carries the ledger title and merger: ' + banners.children[0].innerHTML);
assert.ok(banners.children[0].innerHTML.includes('href="https://github.com/o/r/pull/1"'), 'and the link');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(n.textContent, '34', 'bumps on landing'); assert.ok(chip.classList.contains('cue-tick'));
stageTrackerNoteMerges({ items: [], merged: merged(34, [rec(1, 'hive', 'feat: one'), rec(31, 'clubanderson'), rec(30)]) });
assert.strictEqual(n.textContent, '34', 'steady once the server lists it');
// A merge the server has not recorded yet still counts in this tab, and reconciles when it arrives.
panel.appendChild(row('o/r/pr#2'));
stageTrackerNoteMerges({ items: [{ repo: 'o/r', kind: 'pr', number: 2, stage: 50, title: 'fix: two' }], merged: merged(34, [rec(1), rec(31), rec(30)]) });
stageTrackerNoteMerges({ items: [], recently_merged: [{ repo: 'o/r', number: 2 }], merged: merged(34, [rec(1), rec(31), rec(30)]) });
tick(0);
assert.ok(banners.children[banners.children.length - 1].innerHTML.includes('href="https://github.com/o/r/pull/2"'), 'a banner the ledger has not enriched yet still links to the PR: ' + banners.children[banners.children.length - 1].innerHTML);
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(n.textContent, '35', 'local merge on top of the server number');
assert.strictEqual(sub.textContent, '5 today · 10 this week');
stageTrackerNoteMerges({ items: [], merged: merged(35, [rec(2), rec(1), rec(31), rec(30)], { today: 5, total_7d: 10 }) });
assert.strictEqual(n.textContent, '35', 'no double count when the server catches up');
assert.strictEqual(sub.textContent, '5 today · 10 this week');
// "Show row" from a banner scrolls to the row while it is on the board, and says so once it has gone.
const r2 = panel.querySelector('[data-ham-key="o/r/pr#2"]'); let scrolled = 0; r2.scrollIntoView = () => { scrolled++; };
assert.strictEqual(hamBannerShowRow('o/r/pr#2'), true); assert.strictEqual(scrolled, 1); assert.ok(r2.classList.contains('cue-merged'));
r2.remove();
assert.strictEqual(hamBannerShowRow('o/r/pr#2'), false); assert.ok(toasts.some(x => /left the board/.test(x[1]))); assert.strictEqual(pop.hidden, false, 'the list opens instead');
// Dismiss: the × removes its banner.
const before = banners.children.length; assert.ok(before > 0);
const closeBtn = el('button'); banners.children[0].appendChild(closeBtn); closeBtn.closest = () => banners.children[0];
hamBannerDismiss(closeBtn); tick(180 * 1.5 + 1);
assert.strictEqual(banners.children.length, before - 1, 'dismissed');
console.log('ok');
`)
}

// TestStageTrackerFlightTargetNode: the operator's spec for the flight —
// from the ✓ cell to the Merged chip when the chip is inside the viewport,
// otherwise up and out through the top edge; the glyph is configurable;
// the test celebration closes the dialog and plays on a row in view.
func TestStageTrackerFlightTargetNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerCelebrateJS(t, html)+stageTrackerCelebrateFixture+`
const from = { left: 300, top: 400, width: 20, height: 20 };
assert.deepStrictEqual(stageTrackerFlightTarget(from, { left: 500, top: 10, width: 60, height: 20 }, 1280, 800), { lands: true, dx: 220, dy: -390 }, 'chip in view: land on it');
assert.deepStrictEqual(stageTrackerFlightTarget(from, { left: 500, top: -30, width: 60, height: 20 }, 1280, 800), { lands: false, dx: 0, dy: -(410 + STAGE_TRACKER_CELEBRATE_EXIT_PAD) }, 'chip scrolled above: out through the top');
assert.strictEqual(stageTrackerFlightTarget(from, { left: 500, top: 900, width: 60, height: 20 }, 1280, 800).lands, false, 'chip below the fold');
assert.strictEqual(stageTrackerFlightTarget(from, null, 1280, 800).lands, false, 'no chip at all');
assert.strictEqual(stageTrackerFlightTarget(from, { left: 500, top: 10, width: 0, height: 0 }, 1280, 800).lands, false, 'a hidden chip has no box');
assert.ok(stageTrackerRectInView({ left: 0, top: 0, width: 10, height: 10 }, 100, 100)); assert.ok(!stageTrackerRectInView({ left: 95, top: 0, width: 10, height: 10 }, 100, 100));

// Glyph setting: bee by default, "+1" when chosen, unknown values fall back.
assert.strictEqual(stageTrackerCelebrateGlyph(), '🐝');
assert.strictEqual(stageTrackerSetCelebrateGlyph('+1'), 'plus1'); assert.strictEqual(stageTrackerCelebrateGlyph(), '+1');
assert.strictEqual(stageTrackerSetCelebrateGlyph('confetti'), 'bee'); assert.strictEqual(stageTrackerCelebrateGlyph(), '🐝');
stageTrackerSetCelebrateGlyph('plus1');

// The chip scrolled off (2,300 px above): the glyph exits the top edge at full opacity and the count still bumps.
chip.rect = { left: 500, top: -2300, width: 60, height: 20 };
panel.appendChild(row('o/r/pr#1'));
stageTrackerNoteMerges({ items: [{ repo: 'o/r', kind: 'pr', number: 1, stage: 50 }] });
stageTrackerNoteMerges({ items: [], recently_merged: [{ repo: 'o/r', number: 1, merged_at: new Date(clock).toISOString() }] });
tick(0);
assert.strictEqual(flights().length, 1); const g = flights()[0];
assert.strictEqual(g.textContent, '+1', 'the chosen glyph');
assert.strictEqual(g.style.props['--cue-dx'], '0px'); assert.strictEqual(g.style.props['--cue-dy'], -(410 + STAGE_TRACKER_CELEBRATE_EXIT_PAD) + 'px'); assert.strictEqual(g.style.props['--cue-end-opacity'], '1');
assert.strictEqual(g.style.props['left'], '300px'); assert.strictEqual(g.style.props['top'], '400px', 'spawned at the ✓ cell');
assert.strictEqual(g.attrs['aria-hidden'], 'true');
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(n.textContent, '1', 'the count bumps although the glyph left the screen');
chip.rect = { left: 500, top: 10, width: 60, height: 20 };

// Test celebration: closes the settings dialog, picks the first row in view (not the first row), flies, shows a "(test)" banner, counts nothing.
const above = row('o/r/pr#5'); above.rect = { left: 0, top: -900, width: 800, height: 30 }; above.children[0].children[0].rect = { left: 300, top: -890, width: 20, height: 20 };
const inView = row('o/r/pr#6'); inView.rect = { left: 0, top: 300, width: 800, height: 30 };
panel.children.length = 0; panel.appendChild(above); panel.appendChild(inView);
zaps.length = 0; chip.classList.remove('cue-tick'); banners.children.length = 0;
const r = stageTrackerTestCelebration();
assert.strictEqual(settingsClosed, 1, 'the dialog closes first');
assert.deepStrictEqual(r, { flew: true, lands: true, faded: false }, JSON.stringify(toasts));
assert.ok(inView.classList.contains('cue-merged') && !above.classList.contains('cue-merged'), 'the row in view is the one that glows');
assert.strictEqual(flights().length, 1); assert.strictEqual(flights()[0].style.props['top'], (410 - 10) + 'px', 'from the visible row\'s ✓ cell');
assert.strictEqual(banners.children.length, 1); assert.ok(banners.children[0].innerHTML.includes('(test)'), banners.children[0].innerHTML);
tick(STAGE_TRACKER_CELEBRATE_FLY_MS * 1.5 + 1);
assert.strictEqual(n.textContent, '1', 'the test does not count'); assert.ok(chip.classList.contains('cue-tick'), 'but pops'); assert.deepStrictEqual(zaps, [1], 'and zaps');
assert.ok(!inView.children.some(c => c.classList.contains('cue-event')), 'no event line is written for a test');
// Reduced motion: the test fades instead and says so.
reduced = true; const r2 = stageTrackerTestCelebration(); assert.strictEqual(r2.faded, true); assert.ok(toasts.some(x => /Reduced motion/.test(x[1])));
reduced = false;
panel.children.length = 0;
assert.deepStrictEqual(stageTrackerTestCelebration(), { flew: false });
assert.ok(toasts.some(x => x[0] === 'warning' && /No row to celebrate on/.test(x[1])), JSON.stringify(toasts));
console.log('ok');
`)
}
