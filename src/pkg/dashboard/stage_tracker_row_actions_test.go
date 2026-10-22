package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The activity monitor's row context menu and multi-select (owner request:
// right-click / ⋯ / Shift+F10 on a row; checkbox, Shift+click range,
// Cmd/Ctrl+click toggle; a selection bar with bulk actions). The pure
// helpers are executed under node the way stage_tracker_motion_test.go does.

func stageTrackerRowActionsPreamble(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "STAGE_TRACKER_HEADER_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_GROUP_KEY_PREFIX"),
		jsConstLine(t, html, "STAGE_TRACKER_DECISION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_SEL_ACTIONS"),
		jsConstLine(t, html, "STAGE_TRACKER_MENU_MARGIN"),
		jsConstLine(t, html, "STAGE_TRACKER_MENU_FAIL_LINES"),
		jsConstBlock(t, html, "STAGE_TRACKER_ACTION_LABELS"),
		jsConstLine(t, html, "STAGE_TRACKER_WHY_WRITE"),
		jsConstLine(t, html, "STAGE_TRACKER_WHY_ROLE"),
		"var _stageTrackerSel = new Set(); var _stageTrackerSelAnchor = ''; var _stageTrackerMenu = null; var _stageTracker = null; var _statusSeqFloor = 0; var _stageTrackerEtag = 'e'; var _stageTrackerSig = 's';",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "repoParts"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerRefOf"),
		jsFunc(t, html, "stageTrackerRowToolsHtml"),
		jsFunc(t, html, "stageTrackerSelectableKey"),
		jsFunc(t, html, "stageTrackerSelectionNext"),
		jsFunc(t, html, "stageTrackerSelectionPrune"),
		jsFunc(t, html, "stageTrackerApplySelection"),
		jsFunc(t, html, "stageTrackerRowKeys"),
		jsFunc(t, html, "stageTrackerItemByKey"),
		jsFunc(t, html, "stageTrackerItemsForKeys"),
		jsFunc(t, html, "stageTrackerDecisionTarget"),
		jsFunc(t, html, "stageTrackerDecideOptions"),
		jsFunc(t, html, "stageTrackerSupersederOptions"),
		jsFunc(t, html, "stageTrackerSupersederValid"),
		jsFunc(t, html, "stageTrackerRowActions"),
		jsFunc(t, html, "stageTrackerFlatActions"),
		jsFunc(t, html, "stageTrackerSelectionActions"),
		jsFunc(t, html, "stageTrackerMenuItemHtml"),
		jsFunc(t, html, "stageTrackerMenuHtml"),
		jsFunc(t, html, "stageTrackerMenuPosition"),
		jsFunc(t, html, "stageTrackerSelBarHtml"),
		jsFunc(t, html, "stageTrackerActionLabel"),
		jsFunc(t, html, "stageTrackerActionRequest"),
		jsFunc(t, html, "stageTrackerActionSummary"),
		jsFunc(t, html, "stageTrackerRunActions"),
	}, "\n")
}

func runStageTrackerRowActionsScript(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — row-actions helpers were NOT executed by this run")
	}
	cmd := exec.Command(node, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
}

// TestStageTrackerRowActionsWiring: the markup, the CSS and the handlers the
// menu and the selection bar need are in the page, and the CSS is tokens only.
func TestStageTrackerRowActionsWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<div class="ham-selbar" id="activity-monitor-selbar" role="toolbar"`,
		`${stageTrackerRowToolsHtml(item)}`,
		`data-ham-sub-key="${esc(stageTrackerItemKey(pr))}"`,
		`stageTrackerSelectionSync(el, rows.map(r => r.key));`,
		`data-action="hamSelectClick" data-arg-types="e,t"`,
		`data-action="hamRowMenu" data-arg-types="e,t"`,
		`function hamSelectClick(`, `function hamRowMenu(`, `function hamSelClear(`, `function hamSelAction(`,
		`el.addEventListener('contextmenu'`, `e.key === 'F10'`, `e.key === 'ContextMenu'`,
		`role="menu" aria-label="Row actions"`, `role="menuitem"`,
		`.ham-menu { position: fixed;`, `.ham-row.ham-selected {`, `.ham-selbar {`,
		`'/items/' + n + '/hold'`, `'/items/' + n + '/decision'`, `'/send-to-human'`, `'/api/repos/rescan'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	start := strings.Index(html, "/* ---- Row actions: context menu + multi-select")
	end := strings.Index(html, "/* ---- Activity monitor sounds (#13275)")
	if start < 0 || end < start {
		t.Fatal("row actions JS block not found")
	}
	js := html[start:end]
	for _, banned := range []string{"window.confirm(", "window.prompt(", "window.alert(", " confirm(", " prompt(", " alert("} {
		if strings.Contains(js, banned) {
			t.Errorf("row actions must use the in-app dialogs, found %q", banned)
		}
	}
	cssStart := strings.Index(html, "/* Row actions: a left gutter holds the select checkbox")
	cssEnd := strings.Index(html, "@media (prefers-reduced-motion: reduce) { .ham-seg.active")
	if cssStart < 0 || cssEnd < cssStart {
		t.Fatal("row actions CSS block not found")
	}
	if css := html[cssStart:cssEnd]; strings.Contains(css, "rgb(") || strings.Contains(css, "rgba(") || strings.Contains(css, "#f") || strings.Contains(css, "#0") || strings.Contains(css, "#1") {
		t.Errorf("row actions CSS must use tokens only, found a literal colour:\n%s", css)
	}
	tokens := tokensCSS(t)
	for _, tok := range []string{"--ham-gutter:", "--ham-tool-size:", "--ham-tool-fade-ms:"} {
		if !strings.Contains(tokens, tok) {
			t.Errorf("tokens.css lacks %s", tok)
		}
	}
}

// TestStageTrackerRowActionsApplicability: the menu lists only the actions
// that fit the row's state (Decide for needs-decision rows, Hold vs Release
// by the hold modifier, Mark needs-human hidden once set), disabled entries
// carry their reason, the menu markup has menu/menuitem roles, and the
// position clamps to the viewport.
func TestStageTrackerRowActionsApplicability(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const ids = as => as.map(a => a.id);
const ctx = { user: true, canHold: true, canHuman: true };
const parked = { repo: 'o/r', kind: 'issue', number: 1, title: 'Parked', url: 'https://github.com/o/r/issues/1', stage: 45, modifiers: ['needs-decision'] };
let as = stageTrackerRowActions(parked, ctx);
assert.deepStrictEqual(ids(as), ['decide', 'hold', 'retry', 'needs-human', 'open', 'copy']);
assert.deepStrictEqual(ids(as[0].submenu), ['decide:approve', 'decide:direction', 'decide:close-superseded', 'decide:close-wontfix'], 'an issue gets approve / direction / close');
assert.ok(as.every(a => a.enabled), 'everything is enabled for a writer');

const plain = { repo: 'o/r', kind: 'issue', number: 2, url: 'u', stage: 20, modifiers: [] };
assert.deepStrictEqual(ids(stageTrackerRowActions(plain, ctx)), ['hold', 'retry', 'needs-human', 'open', 'copy'], 'no Decide off 045/085');

const held = { repo: 'o/r', kind: 'pr', number: 3, url: 'u', stage: 60, modifiers: ['hold', 'needs-human'] };
as = stageTrackerRowActions(held, ctx);
assert.deepStrictEqual(ids(as), ['release', 'retry', 'open', 'copy'], 'held row offers Release, needs-human already set is hidden');

// The primary case: a PR parked at 085 ("Needs human decision" on the
// Merge gate column) — Approve posts /lgtm + /approve, Close as superseded
// by… names the winner, Won't fix, Give direction.
const pr85 = { repo: 'o/r', kind: 'pr', number: 4, url: 'u', stage: 85, modifiers: ['needs-decision'] };
as = stageTrackerRowActions(pr85, ctx);
assert.strictEqual(as[0].id, 'decide'); assert.strictEqual(as[0].label, 'Decide…');
assert.deepStrictEqual(ids(as[0].submenu), ['decide:approve', 'decide:close-superseded', 'decide:close-wontfix', 'decide:direction'], 'a PR at 085 gets the full decision set');
assert.strictEqual(as[0].submenu[0].label, 'Approve (/lgtm + /approve)');
assert.strictEqual(as[0].submenu[1].label, 'Close as superseded by…');
assert.ok(as[0].submenu.every(o => o.enabled));
// The pill alone (no stage) is enough; a PR at 085 without the pill too.
const pilled = { ...pr85, stage: 70 };
assert.strictEqual(stageTrackerDecisionTarget(pilled), pilled, 'the needs-decision pill alone parks a PR');
assert.ok(stageTrackerDecisionTarget({ ...pr85, modifiers: [] }), '085 alone parks a PR');
assert.strictEqual(stageTrackerDecisionTarget({ ...pr85, stage: 70, modifiers: [] }), null);

// An issue row whose nested PR is parked at 085 decides THAT PR (its own
// stage is only mirrored), not itself; a mirrored issue at 85 with no such
// PR is not a decision target at all.
const issueWithPR = { repo: 'o/r', kind: 'issue', number: 5, url: 'u', stage: 85, mirrored: true, modifiers: [],
  linked_prs: [{ number: 457, title: 'dibs A', url: 'pu', stage: 60, modifiers: [] }, { number: 458, title: 'dibs B', url: 'pu2', stage: 85, modifiers: ['needs-decision'] }] };
const tgt = stageTrackerDecisionTarget(issueWithPR);
assert.strictEqual(tgt.kind, 'pr'); assert.strictEqual(tgt.number, 458);
as = stageTrackerRowActions(issueWithPR, ctx);
assert.strictEqual(as[0].id, 'decide'); assert.strictEqual(as[0].label, 'Decide PR #458…');
assert.strictEqual(as[0].target.number, 458);
assert.deepStrictEqual(ids(as[0].submenu), ['decide:approve', 'decide:close-superseded', 'decide:close-wontfix', 'decide:direction']);
assert.strictEqual(stageTrackerDecisionTarget({ ...issueWithPR, linked_prs: [issueWithPR.linked_prs[0]] }), null, 'mirrored 85 with no parked PR is not decidable');
assert.strictEqual(stageTrackerDecisionTarget(parked), parked, 'a 045 issue decides itself');

// The decision request carries type pr and the superseding PR.
const req = stageTrackerActionRequest(tgt, 'decide:close-superseded', { comment: 'B won', supersededBy: 'o/r#457' });
assert.strictEqual(req.url, '/api/repos/o/r/items/458/decision');
assert.deepStrictEqual(req.body, { decision: 'close-superseded', comment: 'B won', type: 'pr', superseded_by: 'o/r#457' });
assert.deepStrictEqual(stageTrackerActionRequest(tgt, 'decide:approve', {}).body, { decision: 'approve', comment: '', type: 'pr' });

// Superseder picker: the other PRs in the same repo, newest first, never
// the PR being closed; validation accepts #N / owner/repo#N / URL.
const opts = stageTrackerSupersederOptions({ items: [issueWithPR, pr85, { repo: 'x/y', kind: 'pr', number: 9, title: 'other repo' }] }, [tgt]);
assert.deepStrictEqual(opts.map(o => o.value), ['o/r#457', 'o/r#4']);
assert.strictEqual(opts[0].label, 'dibs A');
assert.strictEqual(stageTrackerSupersederValid('', [tgt]), true);
assert.strictEqual(stageTrackerSupersederValid('#457', [tgt]), true);
assert.strictEqual(stageTrackerSupersederValid('o/r#457', [tgt]), true);
assert.strictEqual(stageTrackerSupersederValid('https://github.com/o/r/pull/457', [tgt]), true);
assert.strictEqual(typeof stageTrackerSupersederValid('458', [tgt]), 'string', 'self-supersession is refused');
assert.strictEqual(typeof stageTrackerSupersederValid('nope', [tgt]), 'string');

// A reader: hold/decide disabled with the reason; needs-human needs merger.
as = stageTrackerRowActions(parked, { user: false, canHold: false, canHuman: false });
const by = Object.fromEntries(as.map(a => [a.id, a]));
assert.strictEqual(by.hold.enabled, false); assert.strictEqual(by.hold.why, STAGE_TRACKER_WHY_WRITE);
assert.strictEqual(by.decide.enabled, false);
assert.strictEqual(by['needs-human'].enabled, false); assert.strictEqual(by['needs-human'].why, STAGE_TRACKER_WHY_ROLE);
assert.strictEqual(by.retry.enabled, true, 'the rescan is open to every viewer');
assert.strictEqual(by.open.enabled, true);

// Menu markup.
const menu = stageTrackerMenuHtml(as, 'o/r#1 · Parked');
assert.ok(menu.startsWith('<div class="ham-menu" role="menu"'));
assert.ok(menu.includes('<div class="ham-menu-head">o/r#1 · Parked</div>'));
assert.strictEqual((menu.match(/role="menuitem"/g) || []).length, 10, 'six top-level items + four decide options');
assert.ok(menu.includes('data-ham-act="hold" tabindex="-1" aria-disabled="true" title="' + STAGE_TRACKER_WHY_WRITE + '"'), 'disabled items carry the reason');
assert.ok(menu.includes('aria-haspopup="menu" aria-expanded="false"'), 'Decide is a collapsed submenu');
assert.ok(menu.includes('<div class="ham-submenu" role="menu" aria-label="Decide…">'));
assert.ok(menu.includes('<div class="ham-menu-sep" role="separator"></div><button type="button" role="menuitem" class="ham-menu-item" data-ham-act="open"'), 'Open on GitHub starts the link group');
assert.ok(!menu.includes('onclick='), 'no inline handlers');

// Escaping: a title with markup never reaches the DOM raw.
assert.ok(stageTrackerMenuHtml([], '<img src=x onerror=alert(1)>').includes('&lt;img'));
assert.ok(stageTrackerRowToolsHtml({ repo: 'o/r', number: 7 }).includes('aria-label="Select o/r#7 (Shift+click for a range)"'));
assert.ok(!stageTrackerRowToolsHtml({ repo: 'o/r', number: 7 }).includes('aria-checked="true"'), 'selection state is not baked into the row HTML');

// Position: at the pointer when it fits, flipped/clamped otherwise.
assert.deepStrictEqual(stageTrackerMenuPosition(100, 100, 200, 300, 1000, 800), { left: 100, top: 100 });
assert.deepStrictEqual(stageTrackerMenuPosition(900, 700, 200, 300, 1000, 800), { left: 700, top: 400 }, 'flips to the other side of the pointer');
assert.deepStrictEqual(stageTrackerMenuPosition(50, 50, 2000, 3000, 1000, 800), { left: STAGE_TRACKER_MENU_MARGIN, top: STAGE_TRACKER_MENU_MARGIN }, 'a menu larger than the viewport pins to the margin');

// Selection actions: enabled only when valid for every row, with the reason.
const sel = stageTrackerSelectionActions([plain, held], () => ctx);
const sby = Object.fromEntries(sel.map(a => [a.id, a]));
assert.strictEqual(sby.hold.enabled, false); assert.strictEqual(sby.hold.why, 'Not applicable to o/r#3');
assert.strictEqual(sby.release.enabled, false); assert.strictEqual(sby.release.why, 'Not applicable to o/r#2');
assert.strictEqual(sby.retry.enabled, true);
assert.strictEqual(sby.copy.enabled, true);
assert.strictEqual(sby['needs-human'].enabled, false, 'one row already has needs-human');
assert.strictEqual(sby.decide.enabled, false, 'neither row needs a decision');
const sel2 = stageTrackerSelectionActions([parked, { ...parked, number: 9 }], () => ctx);
assert.strictEqual(sel2[0].enabled, true); assert.ok(sel2[0].submenu.find(s => s.id === 'decide:approve').enabled);
const sel3 = stageTrackerSelectionActions([pr85, issueWithPR], () => ctx);
assert.strictEqual(sel3[0].enabled, true, 'two rows that both decide PRs share the PR decision set');
assert.ok(sel3[0].submenu.every(s => s.enabled));
const sel4 = stageTrackerSelectionActions([pr85, parked], () => ctx);
assert.strictEqual(sel4[0].enabled, false, 'mixing a parked issue and a parked PR disables Decide');
assert.ok(sel4[0].why.includes('mixes'));
const bar = stageTrackerSelBarHtml([plain, held], sel);
assert.ok(bar.startsWith('<span class="ham-sel-count">2 selected</span><button type="button" class="ham-chip" data-action="hamSelClear">Clear</button>'));
assert.ok(bar.includes('data-arg0="hold" data-arg-types="s,t" aria-disabled="true" title="Not applicable to o/r#3"'), 'disabled bar buttons keep a tooltip');
assert.strictEqual(stageTrackerSelBarHtml([], []), '');
`)
}

// TestStageTrackerSelectionModel: Shift+click selects the range from the
// anchor, Cmd/Ctrl+click toggles, a plain checkbox click toggles and moves
// the anchor; pruning drops rows that left.
func TestStageTrackerSelectionModel(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const keys = ['a', 'b', 'c', 'd', 'e'];
const S = (...k) => new Set(k);
const eq = (got, want, msg) => assert.deepStrictEqual([...got].sort(), [...want].sort(), msg);
let r = stageTrackerSelectionNext(S(), '', keys, 'b', {});
eq(r.sel, S('b')); assert.strictEqual(r.anchor, 'b');
r = stageTrackerSelectionNext(r.sel, r.anchor, keys, 'd', { shift: true });
eq(r.sel, S('b', 'c', 'd'), 'shift extends from the anchor'); assert.strictEqual(r.anchor, 'b', 'the anchor stays');
r = stageTrackerSelectionNext(r.sel, r.anchor, keys, 'a', { shift: true });
eq(r.sel, S('a', 'b'), 'shift the other way replaces the range');
r = stageTrackerSelectionNext(r.sel, r.anchor, keys, 'e', { meta: true });
eq(r.sel, S('a', 'b', 'e'), 'cmd/ctrl toggles on'); assert.strictEqual(r.anchor, 'e');
r = stageTrackerSelectionNext(r.sel, r.anchor, keys, 'c', { shift: true, meta: true });
eq(r.sel, S('a', 'b', 'c', 'd', 'e'), 'cmd/ctrl+shift adds the range to what was selected');
r = stageTrackerSelectionNext(r.sel, r.anchor, keys, 'a', {});
eq(r.sel, S('b', 'c', 'd', 'e'), 'a plain click toggles off');
r = stageTrackerSelectionNext(S('a'), 'zz', keys, 'c', { shift: true });
eq(r.sel, S('a', 'c'), 'a shift-click with no anchor in the list toggles'); assert.strictEqual(r.anchor, 'c');
eq(stageTrackerSelectionPrune(S('a', 'b', 'x'), keys), S('a', 'b'), 'keys whose rows left are dropped');
// Group-by-repo header rows share the keyed list but are never selectable.
const grouped = ['group:o/r1', 'a', 'b', 'group:o/r2', 'c'];
eq(stageTrackerSelectionPrune(S('a', 'group:o/r1', 'c', '__cols'), grouped), S('a', 'c'), 'group and column headers never survive a prune');
assert.strictEqual(stageTrackerSelectableKey('group:o/r1'), false);
assert.strictEqual(stageTrackerSelectableKey('__cols'), false);
assert.strictEqual(stageTrackerSelectableKey(''), false);
assert.strictEqual(stageTrackerSelectableKey('o/r#1:issue'), true);
const rows = grouped.map(k => ({ dataset: { hamKey: k }, classList: { add() {}, remove() {} }, querySelector() { return null; } }));
assert.deepStrictEqual(stageTrackerRowKeys({ children: rows }), ['a', 'b', 'c'], 'row keys skip group headers so a shift range cannot land on one');
`)
}

// TestStageTrackerSelectionSurvivesReconcile: the selection lives on the
// row key, not in the row HTML, so stageTrackerReconcile keeps a selected
// node across the live re-render (and re-applies the class to a swapped
// node); a row that leaves the list leaves the selection.
func TestStageTrackerSelectionSurvivesReconcile(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_MAX_ROWS"),
		jsConstLine(t, html, "STAGE_TRACKER_EXIT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ENTER_MS"),
		"var _stageTrackerRowSrc = new WeakMap(); var _stageTrackerSettleTimer = 0;",
		jsFunc(t, html, "stageTrackerRowSrcKey"),
		jsFunc(t, html, "stageTrackerRowHasMove"),
		jsFunc(t, html, "stageTrackerRowAnimating"),
		jsFunc(t, html, "stageTrackerRowNode"),
		jsFunc(t, html, "stageTrackerRowSrcOf"),
		jsFunc(t, html, "stageTrackerRowMotionOn"),
		jsFunc(t, html, "stageTrackerRowExit"),
		jsFunc(t, html, "stageTrackerRowEnter"),
		jsFunc(t, html, "stageTrackerAfter"),
		jsFunc(t, html, "stageTrackerReconcile"),
	}, "\n")+`
global.setTimeout = () => 0;
function classes() { const set = new Set(); return { set, contains: c => set.has(c), add(...cs) { cs.forEach(c => set.add(c)); }, remove(...cs) { cs.forEach(c => set.delete(c)); } }; }
function base() {
  const n = { parentNode: null, children: [], dataset: {}, classList: classes(), offsetHeight: 40, attrs: {},
    style: { setProperty() {} }, addEventListener() {},
    setAttribute(k, v) { n.attrs[k] = v; }, getAttribute(k) { return n.attrs[k]; },
    remove() { if (n.parentNode) { const c = n.parentNode.children; c.splice(c.indexOf(n), 1); n.parentNode = null; } },
    insertBefore(x, ref) { x.remove(); const i = ref ? n.children.indexOf(ref) : n.children.length; n.children.splice(i, 0, x); x.parentNode = n; },
    replaceChild(x, old) { const i = n.children.indexOf(old); x.remove(); n.children[i] = x; x.parentNode = n; old.parentNode = null; },
    querySelector(sel) { return sel === '.ham-sel' ? n.sel : null; } };
  n.sel = base0(); return n;
}
function base0() { const a = {}; return { attrs: a, setAttribute(k, v) { a[k] = v; }, getAttribute(k) { return a[k]; } }; }
global.document = { createElement() { const t = { content: {} }; Object.defineProperty(t, 'innerHTML', { set(v) { const r = base(); r.html = v; t.content.firstElementChild = r; } }); return t; } };
const el = base();
const H = { key: STAGE_TRACKER_HEADER_KEY, html: '<div class="ham-cols"></div>' };
const R = (k, since) => ({ key: k, html: '<div class="ham-row" tabindex="0">' + k + ' since=' + (since || '1m') + '</div>' });
const opts = { effect: '', hidden: false };
stageTrackerReconcile(el, [H, R('o/r/issue#1'), R('o/r/issue#2'), R('o/r/pr#3')], opts);
let sel = new Set(['o/r/issue#2', 'o/r/pr#3']);
assert.strictEqual(stageTrackerApplySelection(el, sel), true);
const n2 = el.children[2], n3 = el.children[3];
assert.ok(n2.classList.contains('ham-selected') && n3.classList.contains('ham-selected') && !el.children[1].classList.contains('ham-selected'));
assert.strictEqual(n2.sel.getAttribute('aria-checked'), 'true'); assert.strictEqual(el.children[1].sel.getAttribute('aria-checked'), 'false');
assert.ok(el.classList.contains('ham-has-sel'));

// The ~1 s refresh with the same rows keeps the nodes — and their class.
let r = stageTrackerReconcile(el, [H, R('o/r/issue#1'), R('o/r/issue#2'), R('o/r/pr#3')], opts);
assert.strictEqual(r.kept, 4);
sel = stageTrackerSelectionPrune(sel, stageTrackerRowKeys(el)); stageTrackerApplySelection(el, sel);
assert.strictEqual(el.children[2], n2); assert.ok(n2.classList.contains('ham-selected'), 'a kept node stays selected');

// A refresh that changes #2's text swaps its node; the key keeps it selected.
r = stageTrackerReconcile(el, [H, R('o/r/issue#1'), R('o/r/issue#2', '2m'), R('o/r/pr#3')], opts);
assert.strictEqual(r.replaced, 1);
sel = stageTrackerSelectionPrune(sel, stageTrackerRowKeys(el)); stageTrackerApplySelection(el, sel);
assert.notStrictEqual(el.children[2], n2); assert.ok(el.children[2].classList.contains('ham-selected'), 'a swapped node is re-selected by key');

// #3 leaves (merged): it leaves the selection too; #2 stays.
r = stageTrackerReconcile(el, [H, R('o/r/issue#1'), R('o/r/issue#2', '2m')], opts);
assert.strictEqual(r.exited, 1);
sel = stageTrackerSelectionPrune(sel, stageTrackerRowKeys(el)); stageTrackerApplySelection(el, sel);
assert.deepStrictEqual([...sel], ['o/r/issue#2']);
assert.ok(el.classList.contains('ham-has-sel'));
assert.strictEqual(stageTrackerApplySelection(el, new Set()), false); assert.ok(!el.classList.contains('ham-has-sel'));
`)
}

// TestStageTrackerBulkActionsCallEndpointPerRow: a bulk action posts to the
// existing per-item endpoint once per row, in order, carries on past a
// failure and reports it; the hive-wide rescan is posted once per batch;
// the action → endpoint mapping is the one the Overview/Governor panes use.
func TestStageTrackerBulkActionsCallEndpointPerRow(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const i1 = { repo: 'o/r', kind: 'issue', number: 1, url: 'u1' }, p2 = { repo: 'o/r', kind: 'pr', number: 2, url: 'u2' }, i3 = { repo: 'o/x', kind: 'issue', number: 3, url: 'u3' };
// Mapping.
assert.deepStrictEqual(stageTrackerActionRequest(i1, 'hold'), { url: '/api/repos/o/r/items/1/hold', body: { held: true, type: 'issue' } });
assert.deepStrictEqual(stageTrackerActionRequest(p2, 'release'), { url: '/api/repos/o/r/items/2/hold', body: { held: false, type: 'pr' } });
assert.deepStrictEqual(stageTrackerActionRequest(p2, 'retry'), { url: '/api/repos/rescan', body: {}, once: true });
assert.deepStrictEqual(stageTrackerActionRequest(p2, 'needs-human', { comment: 'loop' }), { url: '/api/review/pipeline/o/r/2/send-to-human', body: { reason: 'loop' } });
assert.deepStrictEqual(stageTrackerActionRequest(i1, 'decide:approve'), { url: '/api/repos/o/r/items/1/decision', body: { decision: 'approve', comment: '', type: 'issue' } });
assert.deepStrictEqual(stageTrackerActionRequest(p2, 'decide:close-superseded', { comment: 'dup' }), { url: '/api/repos/o/r/items/2/decision', body: { decision: 'close-superseded', comment: 'dup', type: 'pr' } });
assert.strictEqual(stageTrackerActionRequest(i1, 'open'), null, 'client-only actions have no endpoint');
assert.strictEqual(stageTrackerActionRequest({ repo: 'bad', number: 1 }, 'hold'), null);

// Fake fetch + toasts.
const calls = [];
const progress = [];
global.fetch = async (url, init) => { calls.push({ url, init }); const fail = url.includes('/items/2/'); return { ok: !fail, status: fail ? 403 : 200, json: async () => fail ? { error: 'write permission required' } : { ok: true, minStatusSeq: calls.length } }; };
global.showToast = () => null; global.dismissToast = () => {};
function stageTrackerProgressToast(label, n) { return { update(i, it) { progress.push(label + ' ' + (i + 1) + '/' + n + ' ' + stageTrackerRefOf(it)); }, done(msg, type) { progress.push('done:' + type + ':' + msg); } }; }
let refreshed = 0; function stageTrackerAfterActions() { refreshed++; }
(async () => {
  const res = await stageTrackerRunActions([i1, p2, i3], 'hold', {});
  assert.deepStrictEqual(calls.map(c => c.url), ['/api/repos/o/r/items/1/hold', '/api/repos/o/r/items/2/hold', '/api/repos/o/x/items/3/hold'], 'one POST per row, in order');
  assert.ok(calls.every(c => c.init.method === 'POST' && c.init.headers['Content-Type'] === 'application/json'));
  assert.deepStrictEqual(JSON.parse(calls[1].init.body), { held: true, type: 'pr' });
  assert.deepStrictEqual(res.map(r => r.ok), [true, false, true], 'a failure does not stop the batch');
  assert.strictEqual(res[1].error, 'write permission required');
  assert.strictEqual(_statusSeqFloor, 3, 'minStatusSeq raises the status floor like the hold button does');
  assert.strictEqual(refreshed, 1, 'the tracker re-fetches once after the batch');
  assert.deepStrictEqual(progress.slice(0, 3), ['Hold 1/3 o/r#1', 'Hold 2/3 o/r#2', 'Hold 3/3 o/x#3']);
  assert.strictEqual(progress[3], 'done:error:Hold: 2/3 done. Failed: o/r#2 — write permission required');

  calls.length = 0;
  const rr = await stageTrackerRunActions([i1, p2, i3], 'retry', {});
  assert.deepStrictEqual(calls.map(c => c.url), ['/api/repos/rescan'], 'the hive-wide rescan is posted once for the whole selection');
  assert.deepStrictEqual(rr.map(r => r.ok), [true, true, true]);

  const sum = stageTrackerActionSummary('Hold', [{ item: i1, ok: true }, { item: p2, ok: false, error: 'a' }, { item: i3, ok: false, error: 'b' }, { item: { repo: 'o/r', number: 4 }, ok: false, error: 'c' }, { item: { repo: 'o/r', number: 5 }, ok: false, error: 'd' }]);
  assert.strictEqual(sum.msg, 'Hold: 1/5 done. Failed: o/r#2 — a; o/x#3 — b; o/r#4 — c; +1 more');
  assert.strictEqual(stageTrackerActionSummary('Release hold', [{ item: i1, ok: true }]).type, 'success');
  assert.ok(stageTrackerActionSummary('Re-evaluate now', [{ item: i1, ok: true, data: { status: 'debounced' } }]).msg.includes('already running'));
})().catch(e => { console.error(e); process.exit(1); });
`)
}
