package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerMotionWiring pins the movement-animation surface: the card
// settings modal (gear → #activity-monitor-settings with #sts-sounds and
// #sts-motion sections), the setting key and default, the tokenised effect CSS, and
// the render path that diffs columns and plays the effect after the panel is
// replaced.
func TestStageTrackerMotionWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="activity-monitor-settings-overlay"`,
		`id="activity-monitor-settings-body"`,
		`sectionSettingsGear('Hive activity monitor', 'toggleStageTrackerSettings'`,
		`add(document.getElementById('activity-monitor-settings-overlay'), closeStageTrackerSettings, 10000);`,
		`function stageTrackerMotionSectionHtml(motion, stages)`,
		`+ stageTrackerMotionSectionHtml(state.motion, state.stages || []);`,
		`motion: stageTrackerMotionSetting(), stages: (_stageTracker && _stageTracker.stages) || []`,
		`const STAGE_TRACKER_MOTION_KEY = 'hiveStageTrackerMotion';`,
		`const STAGE_TRACKER_MOTION_DEFAULT = 'wash';`,
		`const STAGE_TRACKER_MOTION_NONE = 'none';`,
		`const STAGE_TRACKER_MOVE_MS = 450;`,
		`localStorage.getItem(STAGE_TRACKER_MOTION_KEY)`,
		`localStorage.setItem(STAGE_TRACKER_MOTION_KEY, v)`,
		`id="sts-sounds"`,
		`id="sts-motion"`,
		`data-action="stageTrackerPreviewMotion"`,
		`data-change-action="stageTrackerMotionChange"`,
		`function stageTrackerColIndex(item, stages)`,
		`function stageTrackerColMap(items, stages)`,
		`function stageTrackerColMoves(prev, next)`,
		`function stageTrackerMoveAttrs(key, ctx)`,
		`function stageTrackerNoteMoves(d)`,
		`function stageTrackerAnimateMoves(root)`,
		`function stageTrackerMotionEffect(setting, reduced)`,
		`stageTrackerNoteMerges(data); stageTrackerNoteOpened(data); stageTrackerNoteMoves(data); }`,
		`const ctx = { nowMs, stages, moves: _stageTrackerMoves };`,
		`if (Object.keys(_stageTrackerMoves).length) { stageTrackerAnimateMoves(el); _stageTrackerMoves = {}; }`,
		`window.matchMedia('(prefers-reduced-motion: reduce)').matches`,
		`@media (prefers-reduced-motion: reduce) { .ham-anim-layer { display: none; } }`,
		`.ham-seg.ham-rm-fade {`,
		`.ham-puck {`, `.ham-seg.ham-ripple::after {`, `.ham-ghost-old {`, `.ham-comet {`, `.ham-seg.active.ham-bump {`, `.ham-seg.ham-wash::after {`,
		`--ham-move-dur: calc(var(--ham-move-ms) * var(--ham-anim-scale));`,
		`--ham-flash-dur: calc((var(--ham-move-flash-ms) + var(--ham-move-flash-gap-ms)) * var(--ham-anim-scale));`,
		`.ham-seg.ham-flash::after {`,
		`var(--ham-flash-count, var(--ham-move-flash-count))`,
		`.ham-seg.ham-flash.soft::after {`,
		`.ham-seg.active.ham-pending {`,
		`function stageTrackerMovePlan(effect, scale)`,
		`const STAGE_TRACKER_FLASH_MS = 140;`,
		`const STAGE_TRACKER_FLASH_GAP_MS = 90;`,
		`const STAGE_TRACKER_FLASH_COUNT = 2;`,
		`strip.dataset.hamPhase = 'flash';`,
		`strip.dataset.hamPhase = 'move';`,
		`strip.dataset.hamPhase = 'land';`,
		// Keyed reconcile: the live refresh keeps row nodes instead of
		// rebuilding the panel's innerHTML, so a move survives the next poll.
		`const STAGE_TRACKER_EXIT_MS = 300;`,
		`const STAGE_TRACKER_ENTER_MS = 220;`,
		`const STAGE_TRACKER_SETTLE_MS = 1200;`,
		`function stageTrackerReconcile(el, rows, opts)`,
		`function stageTrackerRowExit(node, animate)`,
		`function stageTrackerRowEnter(node)`,
		`function stageTrackerRowSrcKey(html)`,
		`function stageTrackerScheduleSettle()`,
		`const rec = stageTrackerReconcile(el, rows, { effect, hidden: document.hidden });`,
		`if (rec.deferred) stageTrackerScheduleSettle();`,
		`.ham-row.ham-leaving {`,
		`.ham-row.ham-entering {`,
		`@keyframes ham-row-exit {`,
		`@keyframes ham-row-enter {`,
		`@media (prefers-reduced-motion: reduce) { .ham-row.ham-leaving { animation-name: ham-row-exit-rm; } .ham-row.ham-entering { animation: none; } }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing movement animation wiring %q", want)
		}
	}
	// Every effect id the settings list offers has an implementation, and
	// the colours ride tokens: the effect CSS names no literal colour.
	for _, id := range []string{"puck", "ripple", "ghost", "comet", "bump", "wash"} {
		if !strings.Contains(html, "{ id: '"+id+"'") {
			t.Errorf("STAGE_TRACKER_MOTIONS lacks %q", id)
		}
	}
	fns := jsConstLine(t, html, "STAGE_TRACKER_MOTION_FNS")
	for _, id := range []string{"'rm-fade': stageTrackerAnimRmFade", "puck: stageTrackerAnimPuck", "ripple: stageTrackerAnimRipple", "ghost: stageTrackerAnimGhost", "comet: stageTrackerAnimComet", "bump: stageTrackerAnimBump", "wash: stageTrackerAnimWash"} {
		if !strings.Contains(fns, id) {
			t.Errorf("STAGE_TRACKER_MOTION_FNS lacks %q", id)
		}
	}
	start := strings.Index(html, "/* Movement animation: when an item's lit chevron")
	end := strings.Index(html, "/* Settings modal (gear on the card header)")
	if start < 0 || end < start {
		t.Fatal("movement animation CSS block not found")
	}
	if css := html[start:end]; strings.Contains(css, "#") || strings.Contains(css, "rgb(") || strings.Contains(css, "rgba(") {
		t.Errorf("movement animation CSS must use tokens only, found a literal colour:\n%s", css)
	}
	// The JS mirrors of the timing tokens must agree with tokens.css.
	for _, tok := range []string{"--ham-move-ms: 450ms;", "--ham-move-ease:", "--ham-move-tint:", "--ham-move-tint-soft:", "--ham-move-flash-ms: 140ms;", "--ham-move-flash-gap-ms: 90ms;", "--ham-move-flash-count: 2;", "--ham-move-exit-ms: 300ms;", "--ham-move-enter-ms: 220ms;"} {
		if !strings.Contains(tokensCSS(t), tok) {
			t.Errorf("tokens.css lacks %s", tok)
		}
	}
}

// TestStageTrackerMotionSequence drives stageTrackerAnimateMove with fake
// timers and a stub DOM: every move is flash (origin pulses, destination
// held dim) → move (the effect runs) → land (destination lit); the flash
// is two pulses normally and one soft pulse under reduced motion; the
// whole plan stays under 1.2 s at 1×; None never starts a sequence.
func TestStageTrackerMotionSequence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — movement sequence was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "STAGE_TRACKER_MOVE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_FLASH_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_FLASH_GAP_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_FLASH_COUNT"),
		jsFunc(t, html, "stageTrackerMovePlan"),
		jsFunc(t, html, "stageTrackerAnimScale"),
		jsFunc(t, html, "stageTrackerAnimLayer"),
		jsFunc(t, html, "stageTrackerAfter"),
		jsFunc(t, html, "stageTrackerAnimateMove"),
	}, "\n") + `
// -- fake timers: callbacks run in due order when flush() is called --
let now = 0; const timers = [];
global.setTimeout = (fn, ms) => { timers.push({ at: now + (ms || 0), fn }); return timers.length; };
function flush() { timers.sort((a, b) => a.at - b.at); while (timers.length) { const t = timers.shift(); now = t.at; t.fn(); } }
global.getComputedStyle = () => ({ getPropertyValue: () => '1' });
global.document = { createElement() { return { className: '', setAttribute() {}, innerHTML: '' }; } };
// -- stub DOM: a strip of 7 chevrons recording every class change --
const trace = [];
function seg(i) {
  const set = new Set(i === 4 ? ['ham-seg', 'active'] : ['ham-seg', 'done']);
  return { i, style: { props: {}, setProperty(k, v) { this.props[k] = v; }, removeProperty(k) { delete this.props[k]; } },
    classList: { contains: c => set.has(c), add(...cs) { for (const c of cs) { set.add(c); trace.push('seg' + i + '+' + c); } }, remove(...cs) { for (const c of cs) { if (set.delete(c)) trace.push('seg' + i + '-' + c); } } },
    addEventListener() {}, removeEventListener() {} };
}
function makeStrip() {
  const row = { layer: null, querySelector() { return this.layer; }, appendChild(l) { this.layer = l; } };
  return { children: Array.from({ length: 7 }, (_, i) => seg(i)), parentElement: row, dataset: {} };
}
const STAGE_TRACKER_MOTION_FNS = {};
for (const id of ['rm-fade', 'puck', 'ripple', 'ghost', 'comet', 'bump', 'wash']) STAGE_TRACKER_MOTION_FNS[id] = (s, f, t) => trace.push('move:' + id + ':' + f + '>' + t);

// plan: two pulses, one soft under reduced motion, under 1.2 s at 1×
for (const id of ['puck', 'ripple', 'ghost', 'comet', 'bump', 'wash']) {
  const p = stageTrackerMovePlan(id, 1);
  assert.strictEqual(p.flashes, 2, id + ' flashes twice');
  assert.strictEqual(p.soft, false);
  assert.strictEqual(p.flashMs, (140 + 90) * 2);
  assert.strictEqual(p.moveMs, 450);
  assert.ok(p.totalMs <= 1200, id + ' total ' + p.totalMs + 'ms exceeds 1.2 s');
}
const rm = stageTrackerMovePlan('rm-fade', 1);
assert.deepStrictEqual([rm.flashes, rm.soft, rm.flashMs, rm.revealAtLand], [1, true, 230, false]);
assert.strictEqual(stageTrackerMovePlan('wash', 2).flashMs, 920, 'preview slow-down scales the flash');
assert.deepStrictEqual(['ripple', 'comet', 'wash'].map(id => stageTrackerMovePlan(id, 1).revealAtLand), [true, true, true]);
assert.deepStrictEqual(['puck', 'ghost', 'bump'].map(id => stageTrackerMovePlan(id, 1).revealAtLand), [false, false, false]);

// advance 2 → 4 with the wash: flash ×2 on the origin, destination held, move, land
let strip = makeStrip();
assert.strictEqual(stageTrackerAnimateMove(strip, 2, 4, 'wash'), true);
assert.strictEqual(strip.dataset.hamPhase, 'flash');
assert.strictEqual(strip.children[2].style.props['--ham-flash-count'], '2', 'origin pulses twice');
assert.deepStrictEqual(trace, ['seg4+ham-pending', 'seg2+ham-flash'], 'flash first: destination dimmed, origin flashing');
assert.strictEqual(timers.length, 1, 'only the flash timer is armed; nothing moves yet');
const phases = [];
// run timers one at a time to watch the phase change
function step() { timers.sort((a, b) => a.at - b.at); const t = timers.shift(); now = t.at; t.fn(); phases.push(strip.dataset.hamPhase); }
step();
assert.strictEqual(strip.dataset.hamPhase, 'move');
assert.deepStrictEqual(trace.slice(2), ['seg2-ham-flash', 'move:wash:2>4'], 'then the effect runs; the wash reveals the destination only on landing');
assert.ok(strip.children[4].classList.contains('ham-pending'), 'destination still held during the wash');
step();
assert.strictEqual(strip.dataset.hamPhase, 'land');
assert.deepStrictEqual(trace.slice(4), ['seg4-ham-pending'], 'land: destination lit');
assert.ok(now >= (140 + 90) * 2 + 450, 'landing is not before flash + move elapsed (' + now + 'ms)');
step();
assert.strictEqual(strip.dataset.hamPhase, undefined, 'phase marker cleared after landing');
assert.deepStrictEqual(phases, ['move', 'land', undefined]);

// regress 5 → 1 with the puck: same order; the puck reveals as the travel starts
trace.length = 0; timers.length = 0; now = 0;
strip = makeStrip();
assert.strictEqual(stageTrackerAnimateMove(strip, 5, 1, 'puck'), true);
assert.deepStrictEqual(trace, ['seg1+ham-pending', 'seg5+ham-flash']);
flush();
assert.deepStrictEqual(trace.slice(2), ['seg5-ham-flash', 'seg1-ham-pending', 'move:puck:5>1'], 'regress: flash → reveal → move');

// reduced motion: one soft pulse, then the crossfade
trace.length = 0; timers.length = 0; now = 0;
strip = makeStrip();
assert.strictEqual(stageTrackerAnimateMove(strip, 2, 4, 'rm-fade'), true);
assert.strictEqual(strip.children[2].style.props['--ham-flash-count'], '1', 'one pulse under reduced motion');
assert.deepStrictEqual(trace, ['seg4+ham-pending', 'seg2+ham-flash', 'seg2+soft']);
flush();
assert.deepStrictEqual(trace.slice(3), ['seg2-ham-flash', 'seg2-soft', 'seg4-ham-pending', 'move:rm-fade:2>4'], 'soft flash → crossfade');

// None / unknown effect / no move: nothing starts
trace.length = 0; timers.length = 0;
strip = makeStrip();
assert.strictEqual(stageTrackerAnimateMove(strip, 2, 4, ''), false, 'None never animates');
assert.strictEqual(stageTrackerAnimateMove(strip, 4, 4, 'wash'), false, 'no column change, no sequence');
assert.deepStrictEqual(trace, []);
assert.strictEqual(timers.length, 0);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node movement sequence fixture failed: %v\n%s", err, out)
	}
}

func tokensCSS(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestStageTrackerMotionDetector runs the pure column diff under node: an
// item's column follows stageTrackerAnchor (detours and shared columns are
// not moves; merged is the cap), the first payload never moves anything, a
// key that appears is not a move, advance and regress both are, an issue
// with one covering PR rides the PR's column and nested PRs are keyed on
// their own, the row builder stamps the moved strip only, and the stateful
// wrapper keeps the moves for exactly one render.
func TestStageTrackerMotionDetector(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — movement detector was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		"const window = {}; const document = { hidden: false };",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		stageTrackerManualMergeJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		stageTrackerSentinelJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_CAP_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_DEFAULT"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_NONE"),
		jsConstBlock(t, html, "STAGE_TRACKER_MOTIONS"),
		"var _stageTrackerCols = null; var _stageTrackerMoves = {};",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerPRKey"),
		jsFunc(t, html, "stageTrackerLinkedDrawnHere"),
		jsFunc(t, html, "stageTrackerLinkedSuperseder"),
		jsFunc(t, html, "stageTrackerLinkedSupersededGone"),
		jsFunc(t, html, "stageTrackerLinkedSupersededTip"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		stageTrackerTierJS(t, html),
		jsFunc(t, html, "stageTrackerColIndex"),
		jsFunc(t, html, "stageTrackerColMap"),
		jsFunc(t, html, "stageTrackerColMoves"),
		jsFunc(t, html, "stageTrackerMoveAttrs"),
		jsFunc(t, html, "stageTrackerNoteMoves"),
		jsFunc(t, html, "stageTrackerMotionEffect"),
		jsFunc(t, html, "stageTrackerMotionSetting"),
		jsFunc(t, html, "stageTrackerSetMotion"),
	}, "\n") + `
const stages = [
  { number: 10 }, { number: 15 }, { number: 20 }, { number: 30 }, { number: 40 }, { number: 45 }, { number: 50 }, { number: 60 },
  { number: 70, branch_from: 50 }, { number: 71, branch_from: 50 }, { number: 80 }, { number: 85, branch_from: 80 }, { number: 90, branch_from: 80 },
  { number: 95 }, { number: 100 }, { number: 110 },
];
const col = (kind, stage) => stageTrackerColIndex({ kind, stage }, stages);
// Column index follows the drawn anchor: shared columns and detours collapse.
assert.strictEqual(col('issue', 0), 0); assert.strictEqual(col('issue', 10), 1); assert.strictEqual(col('issue', 15), 1);
assert.strictEqual(col('issue', 20), 2); assert.strictEqual(col('pr', 30), 3); assert.strictEqual(col('pr', 40), 3);
assert.strictEqual(col('pr', 50), 4); assert.strictEqual(col('pr', 60), 4, '060 reviewed is pre-admission: CI / Review, not the merge gate'); assert.strictEqual(col('pr', 70), 4); assert.strictEqual(col('pr', 71), 4);
assert.strictEqual(col('pr', 80), 5); assert.strictEqual(col('pr', 85), 5); assert.strictEqual(col('pr', 90), 5); assert.strictEqual(col('pr', 95), 5); assert.strictEqual(col('pr', 100), 5);
assert.strictEqual(col('issue', 45), 1, 'an issue parked on a person lights Actionable');
assert.strictEqual(col('issue', 110), STAGE_TRACKER_CAP_COLUMN, 'merged is the cap column');
assert.strictEqual(STAGE_TRACKER_CAP_COLUMN, STAGE_TRACKER_PATH.length);

const P = (n, stage) => ({ kind: 'pr', repo: 'a/b', number: n, stage });
const I = (n, stage, linked) => ({ kind: 'issue', repo: 'a/b', number: n, stage, linked_prs: linked });
// Column map: plain items, an issue riding its single covering PR, nested PRs keyed on their own.
assert.deepStrictEqual(stageTrackerColMap([P(1, 50), I(2, 20), I(3, 20, [{ number: 9, stage: 30 }]), I(4, 20, [{ number: 7, stage: 50 }, { number: 8, stage: 80 }])], stages),
  { 'a/b/pr#1': 4, 'a/b/issue#2': 2, 'a/b/issue#3': 3, 'a/b/pr#9': 3, 'a/b/issue#4': 2, 'a/b/pr#7': 4, 'a/b/pr#8': 5 });
assert.deepStrictEqual(stageTrackerColMap(null, stages), {});
assert.deepStrictEqual(stageTrackerColMap([{ kind: 'pr', repo: 'a/b', number: 0 }, null], stages), {}, 'items without a number are skipped');

// Moves: none on the first payload, none for the same payload, advance and
// regress both count, a new key does not, a detour within a column does not.
const first = stageTrackerColMap([P(1, 50), P(2, 80), I(3, 10), I(5, 20, [{ number: 6, stage: 30 }])], stages);
assert.deepStrictEqual(stageTrackerColMoves(null, first), {}, 'no previous payload → nothing');
assert.deepStrictEqual(stageTrackerColMoves(first, first), {}, 'same payload → nothing');
const second = stageTrackerColMap([P(1, 80), P(2, 50), I(3, 10), P(4, 30), I(5, 20, [{ number: 6, stage: 50 }])], stages);
assert.deepStrictEqual(stageTrackerColMoves(first, second), {
  'a/b/pr#1': { from: 4, to: 5 }, 'a/b/pr#2': { from: 5, to: 4 },
  'a/b/issue#5': { from: 3, to: 4 }, 'a/b/pr#6': { from: 3, to: 4 },
}, 'advance, regress, covered issue + its PR; #3 unchanged and new #4 are not moves');
assert.deepStrictEqual(stageTrackerColMoves(stageTrackerColMap([P(1, 50)], stages), stageTrackerColMap([P(1, 70)], stages)), {}, '050 → 070 shares the Review column');
assert.deepStrictEqual(stageTrackerColMoves(stageTrackerColMap([P(1, 100)], stages), stageTrackerColMap([P(1, 110)], stages)), { 'a/b/pr#1': { from: 5, to: 6 } }, 'merge queue → merged moves to the cap');
assert.deepStrictEqual(stageTrackerColMoves({ 'a/b/pr#1': 4 }, { 'c/d/pr#1': 5 }), {}, 'same number in another repo is another PR');

// Row builder attribute: only the moved key gets the stamp.
const ctx = { moves: stageTrackerColMoves(first, second) };
assert.strictEqual(stageTrackerMoveAttrs('a/b/pr#1', ctx), ' data-ham-prev-col="4" data-ham-col="5"');
assert.strictEqual(stageTrackerMoveAttrs('a/b/pr#2', ctx), ' data-ham-prev-col="5" data-ham-col="4"');
assert.strictEqual(stageTrackerMoveAttrs('a/b/issue#3', ctx), '');
assert.strictEqual(stageTrackerMoveAttrs('a/b/pr#1', {}), '');
assert.strictEqual(stageTrackerMoveAttrs('a/b/pr#1', null), '');

// Stateful wrapper: first payload is silent, then the diff is kept once.
stageTrackerNoteMoves({ items: [P(1, 50)], stages });
assert.deepStrictEqual(_stageTrackerMoves, {});
stageTrackerNoteMoves({ items: [P(1, 80)], stages });
assert.deepStrictEqual(_stageTrackerMoves, { 'a/b/pr#1': { from: 4, to: 5 } });
stageTrackerNoteMoves({ items: [P(1, 80)], stages });
assert.deepStrictEqual(_stageTrackerMoves, {}, 'an unchanged payload clears the pending moves');
stageTrackerNoteMoves({ items: [P(1, 50)], stages });
assert.deepStrictEqual(_stageTrackerMoves, { 'a/b/pr#1': { from: 5, to: 4 } }, 'regression is a move too');

// Setting: default F (wash), persisted, unknown values fall back, None disables,
// reduced motion turns every other choice into the crossfade.
const store = {}; global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); } };
assert.strictEqual(stageTrackerMotionSetting(), 'wash', 'default is F colour wash');
assert.strictEqual(stageTrackerSetMotion('comet'), 'comet');
assert.strictEqual(store[STAGE_TRACKER_MOTION_KEY], 'comet');
assert.strictEqual(stageTrackerMotionSetting(), 'comet');
assert.strictEqual(stageTrackerSetMotion('bogus'), 'wash', 'unknown id → default');
store[STAGE_TRACKER_MOTION_KEY] = 'nonsense';
assert.strictEqual(stageTrackerMotionSetting(), 'wash');
assert.strictEqual(stageTrackerSetMotion('none'), 'none');
assert.strictEqual(stageTrackerMotionSetting(), 'none');
global.localStorage = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); } };
assert.strictEqual(stageTrackerMotionSetting(), 'wash', 'storage errors fall back to the default');
assert.strictEqual(stageTrackerSetMotion('puck'), 'puck', 'and setting still reports the chosen value');
for (const id of ['puck', 'ripple', 'ghost', 'comet', 'bump', 'wash']) {
  assert.strictEqual(stageTrackerMotionEffect(id, false), id);
  assert.strictEqual(stageTrackerMotionEffect(id, true), 'rm-fade', id + ' under reduced motion → crossfade');
}
assert.strictEqual(stageTrackerMotionEffect('none', false), '', 'None disables');
assert.strictEqual(stageTrackerMotionEffect('none', true), '', 'None disables under reduced motion too');
assert.strictEqual(stageTrackerMotionEffect('bogus', false), 'wash');
assert.deepStrictEqual(STAGE_TRACKER_MOTIONS.map(m => m.id), ['none', 'puck', 'ripple', 'ghost', 'comet', 'bump', 'wash']);
assert.deepStrictEqual(STAGE_TRACKER_MOTIONS.map(m => m.key), ['', 'A', 'B', 'C', 'D', 'E', 'F']);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node movement detector fixture failed: %v\n%s", err, out)
	}
}

// TestStageTrackerMotionRowStamp runs the real row builder: the moved strip's
// <ol> carries data-ham-prev-col/data-ham-col, an unmoved one carries
// nothing, and a nested PR sub-row is stamped on its own key.
func TestStageTrackerMotionRowStamp(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — row stamp was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		stageTrackerManualMergeJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		stageTrackerSentinelJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_DRAFT_TIP"),
		jsConstLine(t, html, "STAGE_TRACKER_CAP_COLUMN"),
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerHoldLabels"),
		jsFunc(t, html, "stageTrackerSoakLeft"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsConstLine(t, html, "STAGE_TRACKER_OPEN_STALE_DAYS"),
		jsFunc(t, html, "stageTrackerOpenSince"),
		jsFunc(t, html, "stageTrackerBlockedTip"),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerPRRef"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR_NONE"),
		jsConstLine(t, html, "STAGE_TRACKER_NOBODY_LEAD"),
		jsFunc(t, html, "stageTrackerStageActor"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerActorTip"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_STATIC"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_ACTIVE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN"),
		jsConstBlock(t, html, "STAGE_TRACKER_MOTION_BY_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_AUTHOR_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN_MODS"),
		jsFunc(t, html, "stageTrackerMotion"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerStripItems"),
		jsFunc(t, html, "stageTrackerCurrentStageHtml"),
		jsFunc(t, html, "stageTrackerStageWord"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_NOTE"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerStaleLabelsText"),
		jsFunc(t, html, "stageTrackerHopName"),
		jsFunc(t, html, "stageTrackerTrailText"),
		jsFunc(t, html, "stageTrackerTooltip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
		jsFunc(t, html, "stageTrackerHeadColsHtml"),
		jsFunc(t, html, "stageTrackerSinceHtml"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		stageTrackerActedJS(t, html),
		jsFunc(t, html, "stageTrackerSubRowHtml"),
		jsFunc(t, html, "stageTrackerPRPillsHtml"),
		jsFunc(t, html, "stageTrackerLinkedPRItem"),
		jsFunc(t, html, "stageTrackerLinkedPROpen"),
		jsFunc(t, html, "stageTrackerNestedOpenPRs"),
		jsConstLine(t, html, "STAGE_TRACKER_LINKED_TIP"),
		jsFunc(t, html, "stageTrackerSharedPRs"),
		jsFunc(t, html, "stageTrackerLinkedTip"),
		jsConstLine(t, html, "STAGE_TRACKER_LINKED_GLYPH"),
		jsFunc(t, html, "stageTrackerLinkedDrawnHere"),
		jsFunc(t, html, "stageTrackerLinkedSuperseder"),
		jsFunc(t, html, "stageTrackerLinkedSupersededGone"),
		jsFunc(t, html, "stageTrackerLinkedSupersededTip"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		stageTrackerTierJS(t, html),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerColIndex"),
		jsFunc(t, html, "stageTrackerColMap"),
		jsFunc(t, html, "stageTrackerColMoves"),
		jsFunc(t, html, "stageTrackerMoveAttrs"),
		jsFunc(t, html, "stageTrackerRefOf"),
		jsFunc(t, html, "stageTrackerRowToolsHtml"),
		jsFunc(t, html, "stageTrackerRowHtml"),
		jsFunc(t, html, "stageTrackerPRIssueSlot"),
		jsFunc(t, html, "stageTrackerClosedChipHtml"),
		jsFunc(t, html, "stageTrackerItemURL"),
	}, "\n") + `
const now = Date.parse('2026-01-01T12:00:00Z');
const stages = [{ number: 10, label: 'Actionable', short: 'Actionable' }, { number: 20, label: 'Claimed', short: 'Claimed' }, { number: 30, label: 'PR in progress', short: 'PR in progress' }, { number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' }, { number: 110, label: 'Merged', short: 'Merged' }];
const prev = [{ kind: 'issue', repo: 'a/b', number: 1, stage: 10 }, { kind: 'issue', repo: 'a/b', number: 2, stage: 20, linked_prs: [{ number: 7, stage: 30 }, { number: 8, stage: 50 }] }, { kind: 'pr', repo: 'a/b', number: 3, stage: 50 }];
const next = [{ kind: 'issue', repo: 'a/b', number: 1, stage: 20 }, { kind: 'issue', repo: 'a/b', number: 2, stage: 20, linked_prs: [{ number: 7, stage: 30 }, { number: 8, stage: 80 }] }, { kind: 'pr', repo: 'a/b', number: 3, stage: 50 }];
const ctx = { nowMs: now, stages, moves: stageTrackerColMoves(stageTrackerColMap(prev, stages), stageTrackerColMap(next, stages)) };
const ols = html => Array.from(html.matchAll(/<ol class="ham-strip" aria-label="Pipeline progress"([^>]*)>/g)).map(m => m[1]);
assert.deepStrictEqual(ols(stageTrackerRowHtml(next[0], stages, ctx)), [' data-ham-prev-col="1" data-ham-col="2"'], 'the moved issue strip is stamped');
assert.deepStrictEqual(ols(stageTrackerRowHtml(next[2], stages, ctx)), [''], 'an unmoved strip carries nothing');
assert.deepStrictEqual(ols(stageTrackerRowHtml(next[1], stages, ctx)), ['', ' data-ham-prev-col="4" data-ham-col="5"', ''], 'only the nested PR that moved is stamped (sub-rows sort furthest first)');
assert.deepStrictEqual(ols(stageTrackerRowHtml(next[0], stages, { nowMs: now })), [''], 'no moves in ctx → no stamp');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node row stamp fixture failed: %v\n%s", err, out)
	}
}

// The settings-modal preview is one real tracker row, rebuilt in place on
// every Preview click and on every settings re-render; it must never grow a
// second chevron strip (hivecommons/hive: the preview once wrote chevrons
// into the row's title-column strip, stacking two strips with different
// geometry).
func TestStageTrackerMotionPreviewSample(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — motion preview sample was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		"const window = {};",
		"function formatDashboardTime(d){return new Date(d).toISOString().slice(11,16)+' UTC';}",
		"function formatDashboardDate(d){return new Date(d).toISOString().slice(0,10);}",
		"function formatDashboardDateTime(d){return new Date(d).toISOString().slice(0,16);}",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		stageTrackerManualMergeJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_DRAFT_TIP"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		stageTrackerSentinelJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		jsConstLine(t, html, "STAGE_TRACKER_LABEL_CHIP_LIMIT"),
		jsConstLine(t, html, "STAGE_TRACKER_AGE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_LABEL_FAMILIES"),
		jsConstLine(t, html, "STAGE_TRACKER_CAP_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_DEFAULT"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_NONE"),
		jsConstBlock(t, html, "STAGE_TRACKER_MOTIONS"),
		jsConstLine(t, html, "STAGE_TRACKER_PREVIEW_STAGES"),
		"var _stageTrackerPreviewAt = 0; var _stageTrackerCols = null; var _stageTrackerMoves = {};",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "stageTrackerWaitSince"),
		jsFunc(t, html, "stageTrackerLabelFamily"),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerPRRef"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR_NONE"),
		jsConstLine(t, html, "STAGE_TRACKER_NOBODY_LEAD"),
		jsFunc(t, html, "stageTrackerStageActor"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerActorTip"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_STATIC"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_ACTIVE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN"),
		jsConstBlock(t, html, "STAGE_TRACKER_MOTION_BY_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_AUTHOR_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN_MODS"),
		jsFunc(t, html, "stageTrackerMotion"),
		jsFunc(t, html, "stageTrackerHeaderHtml"),
		jsFunc(t, html, "stageTrackerHoldLabels"),
		jsFunc(t, html, "stageTrackerSoakLeft"),
		jsFunc(t, html, "stageTrackerStripItems"),
		jsFunc(t, html, "stageTrackerCurrentStageHtml"),
		jsFunc(t, html, "stageTrackerStageWord"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_NOTE"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerStaleLabelsText"),
		jsFunc(t, html, "stageTrackerHopName"),
		jsFunc(t, html, "stageTrackerTrailText"),
		jsFunc(t, html, "stageTrackerTooltip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
		jsFunc(t, html, "stageTrackerHeadColsHtml"),
		jsFunc(t, html, "stageTrackerSinceHtml"),
		stageTrackerActedJS(t, html),
		jsFunc(t, html, "stageTrackerSubRowHtml"),
		jsFunc(t, html, "stageTrackerPRPillsHtml"),
		jsFunc(t, html, "stageTrackerLinkedPRItem"),
		jsFunc(t, html, "stageTrackerLinkedPROpen"),
		jsFunc(t, html, "stageTrackerNestedOpenPRs"),
		jsConstLine(t, html, "STAGE_TRACKER_LINKED_TIP"),
		jsFunc(t, html, "stageTrackerSharedPRs"),
		jsFunc(t, html, "stageTrackerLinkedTip"),
		jsConstLine(t, html, "STAGE_TRACKER_LINKED_GLYPH"),
		jsFunc(t, html, "stageTrackerLinkedDrawnHere"),
		jsFunc(t, html, "stageTrackerLinkedSuperseder"),
		jsFunc(t, html, "stageTrackerLinkedSupersededGone"),
		jsFunc(t, html, "stageTrackerLinkedSupersededTip"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		stageTrackerTierJS(t, html),
		jsFunc(t, html, "stageTrackerSortKey"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsConstLine(t, html, "STAGE_TRACKER_OPEN_STALE_DAYS"),
		jsFunc(t, html, "stageTrackerOpenSince"),
		jsFunc(t, html, "stageTrackerBlockedTip"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerRefOf"),
		jsFunc(t, html, "stageTrackerRowToolsHtml"),
		jsFunc(t, html, "stageTrackerRowHtml"),
		jsFunc(t, html, "stageTrackerPRIssueSlot"),
		jsFunc(t, html, "stageTrackerClosedChipHtml"),
		jsFunc(t, html, "stageTrackerItemURL"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerMoveAttrs"),
		jsFunc(t, html, "stageTrackerColIndex"),
		jsFunc(t, html, "stageTrackerSampleRowHtml"),
		jsFunc(t, html, "stageTrackerSampleStrip"),
		jsFunc(t, html, "stageTrackerMotionSectionHtml"),
		jsFunc(t, html, "stageTrackerPreviewMotion"),
	}, "\n") + `
const stages = [
  { number: 10, label: 'Actionable', short: 'Actionable' }, { number: 20, label: 'Claimed', short: 'Claimed' },
  { number: 30, label: 'PR in progress', short: 'PR in progress' }, { number: 50, label: 'Review pending', short: 'CI / Review' },
  { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' }, { number: 110, label: 'Merged', short: 'Merged' },
];
const _stageTracker = { stages };
const PIPE = 'aria-label="Pipeline progress"';
const count = (s, needle) => s.split(needle).length - 1;
const chevrons = (s) => count(s.slice(s.indexOf(PIPE)).split('</ol>')[0], '<li class="ham-seg');
const activeShort = (s) => { const m = s.slice(s.indexOf(PIPE)).split('</ol>')[0].match(/<span class="ham-seg-t">([^<]+)<\/span>/); return m && m[1].trim(); };

// A live row for comparison: the sample must have the same chevron count.
const liveRow = stageTrackerRowHtml({ repo: 'hive/x', number: 7, kind: 'issue', title: 'Live', url: '#', stage: 30, stage_since: '' }, stages, { nowMs: Date.now() });
const LIVE_CHEVRONS = chevrons(liveRow);
assert.ok(LIVE_CHEVRONS >= stages.length, 'live row draws a chevron per column');

// One sample row, inside #sts-motion, with exactly one pipeline strip, drawn
// by the real row builder (same .ham-row/.ham-strip markup as live rows).
function render() {
  const section = stageTrackerMotionSectionHtml('wash', stages);
  assert.ok(section.indexOf('id="sts-motion"') < section.indexOf('id="sts-sample"'), 'sample lives under #sts-motion');
  const sample = section.slice(section.indexOf('id="sts-sample"'));
  assert.strictEqual(count(sample, '<div class="ham-row"'), 1, 'exactly one sample row');
  assert.strictEqual(count(sample, PIPE), 1, 'exactly one chevron strip');
  assert.strictEqual(chevrons(sample), LIVE_CHEVRONS, 'same chevron count as a live row');
  assert.strictEqual(count(sample, 'ham-seg active'), 1, 'one lit chevron');
  return sample;
}
let first = render();
assert.strictEqual(activeShort(first), 'Claimed', 'sample starts at Claimed');
// settings re-render (setIfChanged) yields the same single row
assert.strictEqual(render(), first, 'settings re-render is idempotent');

// -- fake DOM: #sts-sample holds a string; querySelector only answers the
// pipeline-strip selector, and only when that strip is unique --
const animated = [];
const sampleEl = {
  _html: first,
  get innerHTML() { return this._html; }, set innerHTML(v) { this._html = v; },
  querySelector(sel) {
    assert.ok(sel.includes('.ham-row') && sel.includes(PIPE), 'preview targets the pipeline strip, not the title columns: ' + sel);
    return count(this._html, PIPE) === 1 ? { sel } : null;
  },
};
global.document = { getElementById: id => (id === 'sts-sample' ? sampleEl : null) };
function stageTrackerAnimateMove(strip, from, to, effect) { animated.push([from, to, effect]); return true; }
function stageTrackerMotionEffect(id) { return id; }
function stageTrackerReducedMotion() { return false; }

// N Preview clicks: still one row, one strip, lit chevron alternating
const want = ['CI / Review', 'Claimed', 'CI / Review', 'Claimed', 'CI / Review'];
for (let i = 0; i < want.length; i++) {
  stageTrackerPreviewMotion(i % 2 ? 'comet' : 'wash');
  const s = sampleEl.innerHTML;
  assert.strictEqual(count(s, '<div class="ham-row"'), 1, 'click ' + (i + 1) + ': one row');
  assert.strictEqual(count(s, PIPE), 1, 'click ' + (i + 1) + ': one chevron strip');
  assert.strictEqual(chevrons(s), LIVE_CHEVRONS, 'click ' + (i + 1) + ': chevron count');
  assert.strictEqual(count(s, 'ham-seg active'), 1, 'click ' + (i + 1) + ': one lit chevron');
  assert.strictEqual(activeShort(s), want[i], 'click ' + (i + 1) + ': destination lit');
}
const claimed = stageTrackerColIndex({ kind: 'issue', stage: 20 }, stages), review = stageTrackerColIndex({ kind: 'issue', stage: 50 }, stages);
assert.deepStrictEqual(animated, [[claimed, review, 'wash'], [review, claimed, 'comet'], [claimed, review, 'wash'], [review, claimed, 'comet'], [claimed, review, 'wash']], 'each click animates origin → destination on the single strip');
// a re-render after clicks picks up where the preview left off
assert.strictEqual(activeShort(stageTrackerMotionSectionHtml('wash', stages)), 'CI / Review');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node motion preview sample fixture failed: %v\n%s", err, out)
	}
}

// TestStageTrackerMotionReconcile drives the keyed row reconcile with fake
// timers and a stub DOM the way the live poll does (hivecommons/hive: the
// hosted hive refreshes /api/stage-tracker about once a second and the old
// innerHTML rebuild wiped every move mid-flight, leaving only its opening
// flash). A poll whose payload moves one row swaps that row for a stamped
// node and schedules the move plan; the next poll keeps the node and the
// plan runs to its end; a row that leaves the payload gets the exit class
// and stays in place until its collapse ends; a new row fades in; None, a
// hidden tab and a sweep over the row cap skip the row motion.
func TestStageTrackerMotionReconcile(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — reconcile was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "STAGE_TRACKER_MOVE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_FLASH_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_FLASH_GAP_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_FLASH_COUNT"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_MAX_ROWS"),
		jsConstLine(t, html, "STAGE_TRACKER_EXIT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ENTER_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_SETTLE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_HEADER_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_DEFAULT"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_NONE"),
		jsConstBlock(t, html, "STAGE_TRACKER_MOTIONS"),
		"var _stageTrackerRowSrc = new WeakMap(); var _stageTrackerSettleTimer = 0; var renders = 0; function renderStageTracker() { renders++; }",
		jsFunc(t, html, "stageTrackerMotionSetting"),
		jsFunc(t, html, "stageTrackerReducedMotion"),
		jsFunc(t, html, "stageTrackerMotionEffect"),
		jsFunc(t, html, "stageTrackerMovePlan"),
		jsFunc(t, html, "stageTrackerAnimScale"),
		jsFunc(t, html, "stageTrackerAnimLayer"),
		jsFunc(t, html, "stageTrackerAfter"),
		jsFunc(t, html, "stageTrackerAnimateMove"),
		jsFunc(t, html, "stageTrackerAnimateMoves"),
		jsFunc(t, html, "stageTrackerRowSrcKey"),
		jsFunc(t, html, "stageTrackerRowHasMove"),
		jsFunc(t, html, "stageTrackerRowAnimating"),
		jsFunc(t, html, "stageTrackerRowNode"),
		jsFunc(t, html, "stageTrackerRowSrcOf"),
		jsFunc(t, html, "stageTrackerRowMotionOn"),
		jsFunc(t, html, "stageTrackerRowExit"),
		jsFunc(t, html, "stageTrackerRowEnter"),
		jsFunc(t, html, "stageTrackerReconcile"),
		jsFunc(t, html, "stageTrackerScheduleSettle"),
	}, "\n") + `
// -- fake timers -------------------------------------------------------
let now = 0; const timers = [];
global.setTimeout = (fn, ms) => { timers.push({ at: now + (ms || 0), fn }); return timers.length; };
function flush() { timers.sort((a, b) => a.at - b.at); while (timers.length) { const t = timers.shift(); now = t.at; t.fn(); } }
global.getComputedStyle = () => ({ getPropertyValue: () => '1' });
global.localStorage = { getItem: () => null, setItem() {} };
global.window = {};
const moved = [];
const STAGE_TRACKER_MOTION_FNS = { wash: (s, f, t) => moved.push(f + '>' + t) };

// -- stub DOM ------------------------------------------------------------
function classes() { const set = new Set(); return { set, contains: c => set.has(c), add(...cs) { cs.forEach(c => set.add(c)); }, remove(...cs) { cs.forEach(c => set.delete(c)); } }; }
function base(tag) {
  return { tag, parentNode: null, children: [], dataset: {}, classList: classes(), offsetHeight: 40,
    style: { props: {}, setProperty(k, v) { this.props[k] = v; }, removeProperty(k) { delete this.props[k]; }, getPropertyValue(k) { return this.props[k] || ''; } },
    addEventListener() {}, removeEventListener() {},
    remove() { if (this.parentNode) { const c = this.parentNode.children; c.splice(c.indexOf(this), 1); this.parentNode = null; } },
    insertBefore(n, ref) { n.remove(); const i = ref ? this.children.indexOf(ref) : this.children.length; this.children.splice(i, 0, n); n.parentNode = this; },
    replaceChild(n, old) { const i = this.children.indexOf(old); n.remove(); this.children[i] = n; n.parentNode = this; old.parentNode = null; },
    appendChild(n) { this.insertBefore(n, null); },
    querySelector() { return null; }, querySelectorAll() { return []; } };
}
function seg(i, active) { const s = base('li'); s.classList.add('ham-seg', active ? 'active' : 'done'); s.i = i; return s; }
// A row parsed from the builder's string: the strip's lit column and the
// one-render move stamp are what the reconcile and the move plan read.
function rowFrom(html) {
  const row = base('div');
  const stripRow = base('div'); stripRow.layer = null;
  stripRow.querySelector = () => stripRow.layer; stripRow.appendChild = l => { stripRow.layer = l; };
  const active = Number((/active=(\d)/.exec(html) || [])[1]);
  const strip = base('ol'); strip.children = Array.from({ length: 7 }, (_, i) => seg(i, i === active)); strip.parentElement = stripRow;
  const st = / data-ham-prev-col="(\d+)" data-ham-col="(\d+)"/.exec(html);
  if (st) { strip.dataset.hamPrevCol = st[1]; strip.dataset.hamCol = st[2]; }
  row.strip = strip; row.html = html;
  row.querySelector = sel => (sel === '.ham-strip[data-ham-phase]' && strip.dataset.hamPhase ? strip : null);
  return row;
}
global.document = { hidden: false, body: { classList: classes() },
  createElement(tag) { if (tag === 'template') { const t = { content: {} }; Object.defineProperty(t, 'innerHTML', { set(v) { t.content.firstElementChild = rowFrom(v); } }); return t; } return { className: '', setAttribute() {}, innerHTML: '' }; } };
const el = base('div');
el.querySelectorAll = sel => { assert.strictEqual(sel, '.ham-strip[data-ham-prev-col]'); return el.children.map(r => r.strip).filter(s => s && s.dataset.hamPrevCol != null); };
el.querySelector = sel => el.children.find(c => sel === '[data-ham-key]' ? !!c.dataset.hamKey : sel === '.ham-leaving' ? c.classList.contains('ham-leaving') : false) || null;

const H = { key: STAGE_TRACKER_HEADER_KEY, html: '<div class="ham-cols">cols</div>' };
const R = (n, active, since, stamp) => ({ key: 'a/b/pr#' + n, html: '<div class="ham-row"><ol class="ham-strip" aria-label="Pipeline progress"' + (stamp ? ' data-ham-prev-col="' + stamp[0] + '" data-ham-col="' + stamp[1] + '"' : '') + '>active=' + active + '</ol>since=' + (since || '1m') + '</div>' });
const keys = () => el.children.map(c => (c.classList.contains('ham-leaving') ? '-' : '') + c.dataset.hamKey);
const opts = () => ({ effect: stageTrackerMotionEffect(stageTrackerMotionSetting(), stageTrackerReducedMotion()), hidden: document.hidden });
assert.strictEqual(opts().effect, 'wash', 'default effect is the colour wash');

// First payload: every row enters, nothing animates (no previous list).
let r = stageTrackerReconcile(el, [H, R(1, 2), R(2, 4), R(3, 3)], opts());
assert.deepStrictEqual(r, { kept: 0, replaced: 0, entered: 4, exited: 0, deferred: 0 });
assert.deepStrictEqual(keys(), [STAGE_TRACKER_HEADER_KEY, 'a/b/pr#1', 'a/b/pr#2', 'a/b/pr#3']);
assert.ok(el.children.every(c => !c.classList.contains('ham-entering')), 'the first render does not fade rows in');
const n1 = el.children[1], n2 = el.children[2], n3 = el.children[3];

// Same payload again (a filter/paging re-render, or a poll whose rows did
// not change): every node is kept.
r = stageTrackerReconcile(el, [H, R(1, 2), R(2, 4), R(3, 3)], opts());
assert.deepStrictEqual(r, { kept: 4, replaced: 0, entered: 0, exited: 0, deferred: 0 });
assert.ok(el.children[1] === n1 && el.children[2] === n2 && el.children[3] === n3, 'unchanged rows keep their nodes');

// A poll moves #1 from column 2 to 4 (the row builder stamps it) and the
// sort puts it ahead of #2: the row is swapped for the stamped node, the
// others are kept, and the move plan starts on the new strip.
r = stageTrackerReconcile(el, [H, R(1, 4, '1m', [2, 4]), R(2, 4), R(3, 3)], opts());
assert.deepStrictEqual(r, { kept: 3, replaced: 1, entered: 0, exited: 0, deferred: 0 });
const m1 = el.children[1];
assert.notStrictEqual(m1, n1, 'the moved row is a fresh node');
assert.strictEqual(m1.strip.dataset.hamPrevCol, '2');
assert.strictEqual(stageTrackerAnimateMoves(el), 1, 'the stamped strip plays');
assert.strictEqual(m1.strip.dataset.hamPhase, 'flash', 'the plan starts with the origin flash');
assert.strictEqual(m1.strip.dataset.hamPrevCol, undefined, 'the stamp is consumed so a kept node never replays it');
assert.ok(m1.strip.children[4].classList.contains('ham-pending') && m1.strip.children[2].classList.contains('ham-flash'));
assert.strictEqual(timers.length, 1, 'the flash timer is armed');

// The next poll (about a second later on the hosted hive) carries the same
// rows unstamped: the animating node is kept, the plan is not interrupted.
r = stageTrackerReconcile(el, [H, R(1, 4), R(2, 4), R(3, 3)], opts());
assert.deepStrictEqual(r, { kept: 4, replaced: 0, entered: 0, exited: 0, deferred: 0 });
assert.strictEqual(el.children[1], m1, 'the mid-move node survives the refresh');
assert.strictEqual(m1.strip.dataset.hamPhase, 'flash');
assert.strictEqual(stageTrackerAnimateMoves(el), 0, 'nothing is stamped, nothing replays');

// A poll that only ticks the row's "since" text while it is mid-move keeps
// the node too and asks for one settle re-render.
r = stageTrackerReconcile(el, [H, R(1, 4, '2m'), R(2, 4), R(3, 3)], opts());
assert.deepStrictEqual(r, { kept: 3, replaced: 0, entered: 0, exited: 0, deferred: 1 });
assert.strictEqual(el.children[1], m1);
stageTrackerScheduleSettle(); stageTrackerScheduleSettle();
assert.strictEqual(timers.filter(t => t.at === now + STAGE_TRACKER_SETTLE_MS).length, 1, 'one settle timer however many refreshes deferred');
flush();
assert.strictEqual(m1.strip.dataset.hamPhase, undefined, 'the plan ran to its end: flash → move → land');
assert.deepStrictEqual(moved, ['2>4'], 'the wash travelled once, from the old column to the new');
assert.ok(!m1.strip.children[4].classList.contains('ham-pending'));
assert.strictEqual(renders, 1, 'the settle re-render ran once');
assert.strictEqual(_stageTrackerSettleTimer, 0);
// ... and that re-render, with the move landed, swaps in the ticked row.
r = stageTrackerReconcile(el, [H, R(1, 4, '2m'), R(2, 4), R(3, 3)], opts());
assert.deepStrictEqual(r, { kept: 3, replaced: 1, entered: 0, exited: 0, deferred: 0 });

// #2 leaves the payload (merged) and #4 arrives: #2 gets the exit class
// and holds its place while it collapses, #4 fades in; the collapse ends
// and only then is #2 gone.
const gone = el.children[2];
r = stageTrackerReconcile(el, [H, R(1, 4, '2m'), R(4, 1), R(3, 3)], opts());
assert.deepStrictEqual(r, { kept: 3, replaced: 0, entered: 1, exited: 1, deferred: 0 });
assert.ok(gone.classList.contains('ham-leaving'), 'the departed row carries the exit class');
assert.strictEqual(gone.style.props['--ham-row-h'], '40px', 'the collapse starts from the measured height');
assert.strictEqual(gone.parentNode, el, 'still in the list while it collapses');
assert.deepStrictEqual(keys(), [STAGE_TRACKER_HEADER_KEY, 'a/b/pr#1', '-a/b/pr#2', 'a/b/pr#4', 'a/b/pr#3'], 'the collapsing row keeps its slot; the new row is laid out around it');
const fresh = el.children[3];
assert.ok(fresh.classList.contains('ham-entering'), 'the new row fades in');
flush();
assert.strictEqual(gone.parentNode, null, 'removed once the collapse ended');
assert.ok(!fresh.classList.contains('ham-entering'));
assert.deepStrictEqual(keys(), [STAGE_TRACKER_HEADER_KEY, 'a/b/pr#1', 'a/b/pr#4', 'a/b/pr#3']);

// Row motion is off for None, a hidden tab, or a sweep over the row cap.
assert.strictEqual(stageTrackerRowMotionOn('wash', false, 3), true);
assert.strictEqual(stageTrackerRowMotionOn('', false, 3), false, 'None');
assert.strictEqual(stageTrackerRowMotionOn('wash', true, 3), false, 'hidden tab');
assert.strictEqual(stageTrackerRowMotionOn('wash', false, STAGE_TRACKER_MOTION_MAX_ROWS), true);
assert.strictEqual(stageTrackerRowMotionOn('wash', false, STAGE_TRACKER_MOTION_MAX_ROWS + 1), false, 'a page change is not movement');
const gone2 = el.children[1];
r = stageTrackerReconcile(el, [H, R(4, 1), R(3, 3)], { effect: '', hidden: false });
assert.deepStrictEqual(r, { kept: 3, replaced: 0, entered: 0, exited: 1, deferred: 0 });
assert.strictEqual(gone2.parentNode, null, 'None removes the row at once');
assert.ok(!gone2.classList.contains('ham-leaving'));
assert.strictEqual(timers.length, 0, 'no exit timer under None');

// Emptying the list: every row leaves; the header goes at once.
r = stageTrackerReconcile(el, [], opts());
assert.deepStrictEqual(r, { kept: 0, replaced: 0, entered: 0, exited: 3, deferred: 0 });
assert.deepStrictEqual(keys(), ['-a/b/pr#4', '-a/b/pr#3'], 'the header never collapses; the rows do');
flush();
assert.deepStrictEqual(keys(), []);

// The source key ignores the one-render stamp and nothing else.
assert.strictEqual(stageTrackerRowSrcKey(R(1, 4, '1m', [2, 4]).html), R(1, 4).html);
assert.notStrictEqual(stageTrackerRowSrcKey(R(1, 4, '2m').html), R(1, 4).html);
assert.strictEqual(stageTrackerRowHasMove(R(1, 4, '1m', [2, 4]).html), true);
assert.strictEqual(stageTrackerRowHasMove(R(1, 4).html), false);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node reconcile fixture failed: %v\n%s", err, out)
	}
}
