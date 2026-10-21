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
	for _, tok := range []string{"--ham-move-ms:", "--ham-move-ease:", "--ham-move-tint:", "--ham-move-tint-soft:"} {
		if !strings.Contains(tokensCSS(t), tok) {
			t.Errorf("tokens.css lacks %s", tok)
		}
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
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
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
		jsFunc(t, html, "stageTrackerLinkedSorted"),
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
assert.strictEqual(col('pr', 50), 4); assert.strictEqual(col('pr', 60), 5); assert.strictEqual(col('pr', 70), 4); assert.strictEqual(col('pr', 71), 4);
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
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_CAP_COLUMN"),
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerHoldLabels"),
		jsFunc(t, html, "stageTrackerSoakLeft"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerPRRef"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerStripItems"),
		jsFunc(t, html, "stageTrackerTooltip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
		jsFunc(t, html, "stageTrackerHeadColsHtml"),
		jsFunc(t, html, "stageTrackerSinceHtml"),
		jsFunc(t, html, "stageTrackerSubRowHtml"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerColIndex"),
		jsFunc(t, html, "stageTrackerColMap"),
		jsFunc(t, html, "stageTrackerColMoves"),
		jsFunc(t, html, "stageTrackerMoveAttrs"),
		jsFunc(t, html, "stageTrackerRowHtml"),
		jsFunc(t, html, "stageTrackerPRIssueSlot"),
	}, "\n") + `
const now = Date.parse('2026-01-01T12:00:00Z');
const stages = [{ number: 10, label: 'Actionable', short: 'Actionable' }, { number: 20, label: 'Claimed', short: 'Claimed' }, { number: 30, label: 'PR in progress', short: 'PR in progress' }, { number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' }, { number: 110, label: 'Merged', short: 'Merged' }];
const STAGE_TRACKER_ICONS = {};
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
