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
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIONABLE_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_DELEGATE_POLL_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_DELEGATE_POLL_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_DELEGATE_ALL_MIN"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		"var _stageTrackerSel = new Set(); var _stageTrackerSelAnchor = ''; var _stageTrackerMenu = null; var _stageTracker = null; var _statusSeqFloor = 0; var _stageTrackerEtag = 'e'; var _stageTrackerSig = 's';",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "repoParts"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerRefOf"),
		jsFunc(t, html, "stageTrackerRowToolsHtml"),
		jsFunc(t, html, "stageTrackerLinkedPRItem"),
		jsFunc(t, html, "stageTrackerLinkedPROpen"),
		jsFunc(t, html, "stageTrackerNestedOpenPRs"),
		jsFunc(t, html, "stageTrackerSubNodes"),
		jsFunc(t, html, "stageTrackerSubKeyOf"),
		jsFunc(t, html, "stageTrackerSelectableKey"),
		jsFunc(t, html, "stageTrackerSelectionNext"),
		jsFunc(t, html, "stageTrackerSelectionPrune"),
		jsFunc(t, html, "stageTrackerApplySelection"),
		jsFunc(t, html, "stageTrackerRowKeys"),
		jsFunc(t, html, "stageTrackerItemByKey"),
		jsFunc(t, html, "stageTrackerItemsForKeys"),
		jsFunc(t, html, "stageTrackerPRParked"),
		jsFunc(t, html, "stageTrackerParkedNestedPRs"),
		jsFunc(t, html, "stageTrackerDecisionTarget"),
		jsFunc(t, html, "stageTrackerDecideOptions"),
		jsFunc(t, html, "stageTrackerSupersederOptions"),
		jsFunc(t, html, "stageTrackerSupersederValid"),
		jsFunc(t, html, "stageTrackerTriageNeeded"),
		jsFunc(t, html, "stageTrackerTriageAccepted"),
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
		jsFunc(t, html, "stageTrackerDelegateTally"),
		jsFunc(t, html, "stageTrackerDelegatedSubResults"),
		jsFunc(t, html, "stageTrackerDelegatedSummary"),
		jsFunc(t, html, "stageTrackerDelegatedDone"),
		jsFunc(t, html, "stageTrackerAwaitDelegated"),
		jsFunc(t, html, "stageTrackerPatchItem"),
		jsFunc(t, html, "stageTrackerRunActions"),
		// runRowAction: result toasts + acting rows (owner request).
		jsConstLine(t, html, "STAGE_TRACKER_ACTION_NOOP"),
		jsConstLine(t, html, "STAGE_TRACKER_RESULT_TOAST_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTING_MAX_MS"),
		jsConstBlock(t, html, "STAGE_TRACKER_ACTING_CAPTIONS"),
		jsConstBlock(t, html, "STAGE_TRACKER_ACTION_WORDS"),
		"var _stageTrackerResultToasts = []; var _stageTrackerPending = [];",
		jsFunc(t, html, "stageTrackerActingCaption"),
		jsFunc(t, html, "stageTrackerItemState"),
		jsFunc(t, html, "stageTrackerTargetKey"),
		jsFunc(t, html, "stageTrackerResultOf"),
		jsFunc(t, html, "stageTrackerErrorResult"),
		jsFunc(t, html, "stageTrackerResultText"),
		jsFunc(t, html, "stageTrackerResultToast"),
		jsFunc(t, html, "stageTrackerResultToastStale"),
		jsFunc(t, html, "stageTrackerResultToastsSync"),
		jsFunc(t, html, "stageTrackerPendingKeys"),
		jsFunc(t, html, "stageTrackerDecisionTargetKey"),
		jsFunc(t, html, "stageTrackerSetPending"),
		jsFunc(t, html, "stageTrackerArmPending"),
		jsFunc(t, html, "stageTrackerClearPending"),
		jsFunc(t, html, "stageTrackerPendingLive"),
		jsFunc(t, html, "stageTrackerPendingExpire"),
		jsFunc(t, html, "stageTrackerPendingSync"),
		jsFunc(t, html, "runRowAction"),
		jsFunc(t, html, "makeToastSticky"),
		stageTrackerDomStubJS,
	}, "\n")
}

// stageTrackerDomStubJS is the least DOM the result toast and the acting
// rows need under node: a toast container, elements with classList /
// children / attributes, and a panel whose rows tests build by hand.
const stageTrackerDomStubJS = `
function hamStubEl(tag) {
  const el = { tagName: tag, className: '', textContent: '', children: [], attrs: {}, parentNode: null, _listeners: {},
    classList: { add(c) { if (!el.className.split(' ').includes(c)) el.className = (el.className + ' ' + c).trim(); }, remove(c) { el.className = el.className.split(' ').filter(x => x && x !== c).join(' '); }, contains(c) { return el.className.split(' ').includes(c); } },
    appendChild(c) { c.parentNode = el; el.children.push(c); return c; }, prepend(c) { c.parentNode = el; el.children.unshift(c); },
    setAttribute(k, v) { el.attrs[k] = String(v); }, getAttribute(k) { return k in el.attrs ? el.attrs[k] : null; },
    addEventListener(n, fn) { el._listeners[n] = fn; }, remove() { if (el.parentNode) el.parentNode.children = el.parentNode.children.filter(c => c !== el); el.parentNode = null; },
    get lastChild() { return el.children[el.children.length - 1] || null; },
    querySelector(sel) { const m = sel.match(/^:scope > \.([\w-]+)$/); if (!m) return null; return el.children.find(c => c.classList.contains(m[1])) || null; },
    querySelectorAll(sel) { const out = []; const walk = n => { for (const c of (n.children || [])) { if (!c.attrs) continue; if ((sel.includes('[data-ham-key]') && c.attrs['data-ham-key'] != null) || (sel.includes('[data-ham-sub-key]') && c.attrs['data-ham-sub-key'] != null)) out.push(c); walk(c); } }; walk(el); return out; },
  };
  return el;
}
const hamToastContainer = hamStubEl('div');
const hamPanel = hamStubEl('div');
global.document = { getElementById(id) { return id === 'toast-container' ? hamToastContainer : (id === 'activity-monitor-panel' ? hamPanel : null); }, createElement: hamStubEl, createTextNode(v) { return { nodeValue: v, parentNode: null }; } };
// A tracker row (or nested PR sub-row) as stageTrackerPendingSync sees it.
function hamStubRow(key, sub) { const r = hamStubEl('div'); r.setAttribute(sub ? 'data-ham-sub-key' : 'data-ham-key', key); const strip = hamStubEl('div'); strip.classList.add('ham-strip-row'); r.appendChild(strip); return r; }
`

func runStageTrackerRowActionsScript(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — row-actions helpers were NOT executed by this run")
	}
	// The lifted fixture is well past Linux's per-argument limit, so the
	// script travels over stdin rather than as a -e argument.
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(script)
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
assert.deepStrictEqual(ids(as[0].submenu), ['decide:delegate', 'decide:approve', 'decide:direction', 'decide:close-superseded', 'decide:close-wontfix'], 'an issue gets let-hive-decide first, then approve / direction / close');
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
assert.deepStrictEqual(ids(as[0].submenu), ['decide:delegate', 'decide:approve', 'decide:close-superseded', 'decide:close-wontfix', 'decide:direction'], 'a PR at 085 gets the full decision set, Let hive decide first');
assert.strictEqual(as[0].submenu[0].label, 'Let hive decide');
assert.strictEqual(as[0].submenu[1].label, 'Approve (/lgtm + /approve)');
assert.strictEqual(as[0].submenu[2].label, 'Close as superseded by…');
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
// Two open nested PRs: the issue row first offers the explicit all-PRs
// delegate (hivecommons/hive#15009), then the one parked PR's Decide.
assert.strictEqual(as[0].id, 'decide:delegate-all'); assert.strictEqual(as[0].label, 'Let hive decide — all 2 PRs'); assert.deepStrictEqual(as[0].prs, [457, 458]);
assert.strictEqual(as[1].id, 'decide'); assert.strictEqual(as[1].label, 'Decide PR #458…');
assert.strictEqual(as[1].target.number, 458);
assert.deepStrictEqual(ids(as[1].submenu), ['decide:delegate', 'decide:approve', 'decide:close-superseded', 'decide:close-wontfix', 'decide:direction']);
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
assert.strictEqual((menu.match(/role="menuitem"/g) || []).length, 11, 'six top-level items + five decide options');
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

// TestStageTrackerLetHiveDecide: "Let hive decide" is the first Decide…
// entry on rows and in the selection bar, posts to the delegated-decision
// endpoint with the row's park words, polls the job until it ends, reports
// the verdict and flags a failed job as a failed row; the audit view marks a
// delegated decision apart from a person's.
func TestStageTrackerLetHiveDecide(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`'decide:delegate': 'Let hive decide'`,
		`/decide-delegated'`,
		`'Hive is deciding ' + stageTrackerRefOf(it)`,
		`function auditDelegatedDecision(e)`,
		`>delegated</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+jsFunc(t, html, "auditDelegatedDecision")+`
const ids = as => as.map(a => a.id);
const ctx = { user: true, canHold: true, canHuman: true };
const pr = { repo: 'o/r', kind: 'pr', number: 4, url: 'u', stage: 85, modifiers: ['needs-decision'], reason: 'requires_human', caption: 'Needs human decision', sentinel_reason: '' };
const issue = { repo: 'o/r', kind: 'issue', number: 5, url: 'u', stage: 45, modifiers: ['needs-decision'] };
assert.strictEqual(stageTrackerDecideOptions(pr, ctx)[0].id, 'decide:delegate');
assert.strictEqual(stageTrackerDecideOptions(issue, ctx)[0].id, 'decide:delegate');
assert.strictEqual(stageTrackerDecideOptions(issue, { user: '', canHold: false })[0].enabled, false, 'needs write access like its siblings');
const sel = stageTrackerSelectionActions([pr], () => ctx);
assert.strictEqual(sel[0].submenu[0].id, 'decide:delegate'); assert.strictEqual(sel[0].submenu[0].label, 'Let hive decide'); assert.ok(sel[0].submenu[0].enabled);
assert.deepStrictEqual(stageTrackerActionRequest(pr, 'decide:delegate'), { url: '/api/repos/o/r/items/4/decide-delegated', body: { type: 'pr', reason: 'requires_human', caption: 'Needs human decision', sentinel_reason: '' }, poll: true });
assert.strictEqual(stageTrackerActionLabel('decide:delegate'), 'Let hive decide');
assert.strictEqual(stageTrackerDelegatedDone({ status: 'running' }), false);
assert.strictEqual(stageTrackerDelegatedDone({ status: 'done' }), true);
assert.strictEqual(stageTrackerDelegatedDone(null), false);
assert.strictEqual(stageTrackerDelegatedSummary([{ item: pr, ok: true, data: { decision: { verdict: 'approve', rule: 'small-low-risk' } } }, { item: issue, ok: true, data: { decision: { verdict: 'recommend-only', rule: 'hive-policy', keeps_park: true } } }, { item: issue, ok: false }]),
  'Hive decided: o/r#4 approve (small-low-risk); o/r#5 recommend-only (hive-policy) — kept for the owner');
assert.strictEqual(stageTrackerDelegatedSummary([{ item: pr, ok: true, data: {} }]), '');
assert.ok(auditDelegatedDecision({ action: 'repo_item_decision_delegated' }));
assert.ok(auditDelegatedDecision({ action: 'repo_item_decision_delegated_failed' }));
assert.ok(!auditDelegatedDecision({ action: 'repo_item_decision_approve' }));
assert.ok(!auditDelegatedDecision(null));

// The run: POST answers 202 running; GET polls land done / failed.
const calls = []; const toasts = []; const progress = [];
const jobs = { '/api/repos/o/r/items/4/decide-delegated': [{ status: 'running' }, { status: 'running' }, { status: 'done', decision: { verdict: 'approve', rule: 'default-release' } }],
               '/api/repos/o/r/items/5/decide-delegated': [{ status: 'running' }, { status: 'failed', error: 'GitHub 502' }] };
global.fetch = async (url, init) => { calls.push({ url, method: (init && init.method) || 'GET' }); const q = jobs[url]; const j = q.length > 1 ? q.shift() : q[0]; return { ok: true, status: init ? 202 : 200, json: async () => j }; };
global.showToast = (msg, type) => { toasts.push(type + ':' + msg); return null; }; global.dismissToast = () => {};
global.setTimeout = fn => fn();
function stageTrackerProgressToast(label, n) { return { update() {}, done(msg, type) { progress.push(type + ':' + msg); } }; }
function stageTrackerAfterActions() {}
(async () => {
  const res = await stageTrackerRunActions([pr, issue], 'decide:delegate', {});
  assert.deepStrictEqual(res.map(r => r.ok), [true, false]);
  assert.strictEqual(res[1].error, 'GitHub 502');
  assert.strictEqual(res[0].data.decision.verdict, 'approve');
  assert.deepStrictEqual(calls.filter(c => c.method === 'POST').map(c => c.url), ['/api/repos/o/r/items/4/decide-delegated', '/api/repos/o/r/items/5/decide-delegated']);
  assert.strictEqual(calls.filter(c => c.method === 'GET' && c.url.endsWith('/4/decide-delegated')).length, 2, 'polls until the job ends');
  assert.ok(toasts.includes('info:Hive is deciding o/r#4…'));
  // The delegate summary is a tally (sent · decided · no-op · failed), not n/m done.
  assert.strictEqual(progress[0], 'error:Let hive decide: 2 sent · 1 decided · 0 no-op · 1 failed. Failed: o/r#5 — GitHub 502. Hive decided: o/r#4 approve (default-release)');
})().catch(e => { console.error(e); process.exit(1); });
`)
}

// TestStageTrackerPRScopedRowActions: an issue row with two nested PRs
// (hivecommons/hive#15009: #15017 "Ready for human merge · untrusted
// author" and #15020 "Needs human decision") used to offer one Decide that
// silently acted on one of them. Now each sub-row carries its own ⋯ menu and
// checkbox and its actions target THAT PR; the issue row offers "Let hive
// decide — all N PRs", whose request names the PRs and whose answer lists
// each PR's outcome; a selection of sub-rows gets one "Let hive decide (N
// selected)" chip and a tally toast.
func TestStageTrackerPRScopedRowActions(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`${typeof stageTrackerRowToolsHtml === 'function' ? stageTrackerRowToolsHtml(pr) : ''}`,
		`.ham-sub > .ham-row-tools {`,
		`.ham-sub.ham-selected {`,
		`const sub = btn && btn.closest ? btn.closest('[data-ham-sub-key]') : null;`,
		`'decide:delegate-all': 'Let hive decide — all PRs'`,
		`'decide:delegate-all': 'Hive is deciding each PR…'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const ids = as => as.map(a => a.id);
const ctx = { user: true, canHold: true, canHuman: true };
const issue = { repo: 'hivecommons/hive', kind: 'issue', number: 15009, title: 'Two PRs', url: 'https://github.com/hivecommons/hive/issues/15009', stage: 85, mirrored: true, modifiers: [],
  linked_prs: [
    { number: 15017, title: 'first', url: 'https://github.com/hivecommons/hive/pull/15017', stage: 90, modifiers: [], manual_merge: true, human_merge_reason: 'untrusted-author', human_merge_reason_text: 'untrusted author', head: 'aaaa', author: 'ext', author_kind: 'external' },
    { number: 15020, title: 'second', url: 'https://github.com/hivecommons/hive/pull/15020', stage: 85, modifiers: ['needs-decision', 'hold'], head: 'bbbb', decision_needed: { question: 'Which one?', options: [{ action: 'decide:delegate', label: 'Let hive decide', enabled: true }] } },
    { number: 15001, title: 'merged', url: 'pu', stage: 110, modifiers: ['merged'] },
  ] };
_stageTracker = { items: [issue] };

// Each sub-row is a row item of its own, with its number, repo and head.
const a = stageTrackerLinkedPRItem(issue, issue.linked_prs[0]), b = stageTrackerLinkedPRItem(issue, issue.linked_prs[1]);
assert.strictEqual(a.kind, 'pr'); assert.strictEqual(a.number, 15017); assert.strictEqual(a.repo, 'hivecommons/hive'); assert.strictEqual(a.head, 'aaaa');
assert.strictEqual(a.parent, 15009); assert.deepStrictEqual(a.siblings, [15020], 'the one open sibling (the merged PR is not one)');
assert.deepStrictEqual(b.siblings, [15017]);
assert.deepStrictEqual(stageTrackerNestedOpenPRs(issue).map(p => p.number), [15017, 15020], 'merged PRs are not open');
assert.strictEqual(stageTrackerItemByKey(_stageTracker, 'hivecommons/hive/pr#15020').head, 'bbbb', 'the popover / menu look the PR up by its sub-row key');

// The sub-row markup carries the PR's own gutter (checkbox + ⋯).
const tools = stageTrackerRowToolsHtml(b);
assert.ok(tools.includes('aria-label="Select hivecommons/hive#15020 (Shift+click for a range)"'));
assert.ok(tools.includes('aria-label="Actions for hivecommons/hive#15020"') && tools.includes('data-action="hamRowMenu"'));

// Each PR's menu targets THAT PR: "Ready for human merge" and "Needs human
// decision" both park a PR; Hold/Release reads the PR's own modifiers.
let as = stageTrackerRowActions(a, ctx);
assert.strictEqual(as[0].id, 'decide'); assert.strictEqual(as[0].label, 'Decide…'); assert.strictEqual(as[0].target, a);
assert.deepStrictEqual(ids(as[0].submenu), ['decide:delegate', 'decide:approve', 'decide:close-superseded', 'decide:close-wontfix', 'decide:direction']);
assert.ok(ids(as).includes('hold') && !ids(as).includes('decide:delegate-all'), 'a PR row never gets the all-PRs item');
as = stageTrackerRowActions(b, ctx);
assert.strictEqual(as[0].target.number, 15020); assert.ok(ids(as).includes('release'), 'the held PR offers Release');
assert.strictEqual(stageTrackerActionRequest(b, 'decide:delegate').url, '/api/repos/hivecommons/hive/items/15020/decide-delegated');
assert.deepStrictEqual(stageTrackerActionRequest(a, 'decide:approve', {}).body, { decision: 'approve', comment: '', type: 'pr' });
assert.strictEqual(stageTrackerActionRequest(a, 'decide:approve', {}).url, '/api/repos/hivecommons/hive/items/15017/decision');

// The issue row no longer picks one: no single Decide, an explicit all-PRs item.
assert.strictEqual(stageTrackerDecisionTarget(issue), null, 'two parked PRs is ambiguous');
as = stageTrackerRowActions(issue, ctx);
assert.deepStrictEqual(ids(as), ['decide:delegate-all', 'hold', 'retry', 'needs-human', 'open', 'copy']);
assert.strictEqual(as[0].label, 'Let hive decide — all 2 PRs'); assert.strictEqual(as[0].hint, 'per PR'); assert.deepStrictEqual(as[0].prs, [15017, 15020]);
assert.strictEqual(stageTrackerRowActions(issue, { user: false, canHold: false })[0].enabled, false);
const req = stageTrackerActionRequest(issue, 'decide:delegate-all');
assert.strictEqual(req.url, '/api/repos/hivecommons/hive/items/15009/decide-delegated'); assert.strictEqual(req.poll, true);
assert.deepStrictEqual(req.body.prs, [15017, 15020]); assert.strictEqual(req.body.type, 'issue');
assert.strictEqual(stageTrackerActionRequest(a, 'decide:delegate-all'), null, 'never on a PR');
const one = { ...issue, linked_prs: [issue.linked_prs[1]] };
assert.strictEqual(stageTrackerDecisionTarget(one).number, 15020, 'one parked PR: the issue row decides it, as before');
assert.ok(!ids(stageTrackerRowActions(one, ctx)).includes('decide:delegate-all'));
const menu = stageTrackerMenuHtml(stageTrackerRowActions(issue, ctx), 'x');
assert.ok(menu.includes('data-ham-act="decide:delegate-all"') && menu.includes('Let hive decide — all 2 PRs<span class="ham-menu-more">per PR</span>'));

// The selection model runs over sub-rows: keys in document order, painted
// on the sub nodes; a sub-row click selects the PR, not its parent.
const el = hamStubEl('div'); const row = hamStubRow('hivecommons/hive/issue#15009'); row.dataset = { hamKey: 'hivecommons/hive/issue#15009' };
const s1 = hamStubRow('hivecommons/hive/pr#15017', true), s2 = hamStubRow('hivecommons/hive/pr#15020', true); row.appendChild(s1); row.appendChild(s2); el.appendChild(row);
assert.deepStrictEqual(stageTrackerRowKeys(el), ['hivecommons/hive/issue#15009', 'hivecommons/hive/pr#15017', 'hivecommons/hive/pr#15020']);
const sel = stageTrackerSelectionNext(new Set(), '', stageTrackerRowKeys(el), 'hivecommons/hive/pr#15020', {}).sel;
assert.deepStrictEqual([...sel], ['hivecommons/hive/pr#15020']);
assert.strictEqual(stageTrackerApplySelection(el, sel), true);
assert.ok(s2.classList.contains('ham-selected') && !s1.classList.contains('ham-selected') && !row.classList.contains('ham-selected'));
const range = stageTrackerSelectionNext(sel, 'hivecommons/hive/pr#15020', stageTrackerRowKeys(el), 'hivecommons/hive/pr#15017', { shift: true }).sel;
assert.deepStrictEqual([...range].sort(), ['hivecommons/hive/pr#15017', 'hivecommons/hive/pr#15020']);
assert.deepStrictEqual(stageTrackerItemsForKeys([...range]).map(p => p.number), [15017, 15020]);

// The bulk bar: one "Let hive decide (N selected)" chip, judged per item.
const bulk = stageTrackerSelectionActions([a, b], () => ctx);
assert.strictEqual(bulk[1].id, 'decide:delegate'); assert.strictEqual(bulk[1].label, 'Let hive decide (2 selected)'); assert.ok(bulk[1].enabled);
assert.ok(stageTrackerSelBarHtml([a, b], bulk).includes('data-arg0="decide:delegate" data-arg-types="s,t">Let hive decide (2 selected)</button>'));
const plain = { repo: 'o/r', kind: 'issue', number: 2, url: 'u', stage: 20, modifiers: [] };
assert.strictEqual(stageTrackerSelectionActions([a, plain], () => ctx)[1].enabled, false, 'an item with nothing to decide disables the chip');

// The tally: "3 sent · 2 decided · 1 no-op", counting each PR of a per-PR job.
assert.strictEqual(stageTrackerDelegateTally([{ ok: true, result: { action: 'approved' } }, { ok: true, result: { action: 'approved' } }, { ok: true, result: { action: STAGE_TRACKER_ACTION_NOOP } }]), '3 sent · 2 decided · 1 no-op');
assert.strictEqual(stageTrackerDelegateTally([{ ok: false }, { ok: true, result: { action: 'recommended' } }]), '2 sent · 1 decided · 0 no-op · 1 failed');
const perPR = { status: 'done', result: { action: 'decided-each', target: { repo: 'hivecommons/hive', kind: 'issue', number: 15009 }, outcome: 'Decided each of the 2 nested pull requests: #15017 approved (small low risk); #15020 no change (already decided).', next: '2 sent · 1 decided · 1 no-op.' },
  items: [{ number: 15017, type: 'pr', status: 'done', decision: { verdict: 'approve', rule: 'small-low-risk' }, result: { action: 'approved', rule: 'small-low-risk', target: { repo: 'hivecommons/hive', kind: 'pr', number: 15017 }, outcome: 'ok', next: 'merge gate' } },
          { number: 15020, type: 'pr', status: 'done', result: { action: STAGE_TRACKER_ACTION_NOOP, target: { repo: 'hivecommons/hive', kind: 'pr', number: 15020 }, outcome: 'No change', next: '' } }] };
assert.strictEqual(stageTrackerDelegateTally([{ ok: true, data: perPR, result: perPR.result }]), '2 sent · 1 decided · 1 no-op');
const subs = stageTrackerDelegatedSubResults(issue, perPR);
assert.deepStrictEqual(subs.map(x => x.item.number), [15017, 15020]); assert.strictEqual(subs[0].item.head, 'aaaa', 'the PR item comes from the tracker');
assert.strictEqual(subs[0].result.action, 'approved'); assert.strictEqual(subs[1].result.action, STAGE_TRACKER_ACTION_NOOP);
assert.ok(stageTrackerDelegatedSummary([{ item: issue, ok: true, data: perPR }]).includes('hivecommons/hive#15017 approve (small-low-risk)'));

// The run of "Let hive decide — all N PRs": one POST on the issue naming the
// PRs; one toast for the issue summary and one per PR, each on its PR.
const calls = [];
global.fetch = async (url, init) => { calls.push({ url, body: init && init.body ? JSON.parse(init.body) : null }); return { ok: true, status: 200, json: async () => perPR }; };
global.showToast = () => null; global.dismissToast = () => {}; global.setTimeout = fn => fn();
function stageTrackerProgressToast() { return { update() {}, done() {} }; }
function stageTrackerAfterActions() {}
(async () => {
  const res = await runRowAction('decide:delegate-all', issue);
  assert.strictEqual(calls.length, 1); assert.strictEqual(calls[0].url, '/api/repos/hivecommons/hive/items/15009/decide-delegated'); assert.deepStrictEqual(calls[0].body.prs, [15017, 15020]);
  assert.strictEqual(res.toasts.length, 3, 'the issue summary + one per PR');
  assert.deepStrictEqual(res.toasts.map(x => x.key), ['hivecommons/hive/issue#15009', 'hivecommons/hive/pr#15017', 'hivecommons/hive/pr#15020']);
  assert.ok(res.toasts[0].text.head.includes('decided per PR'), res.toasts[0].text.head);
  assert.ok(res.toasts[0].text.outcome.includes('#15017 approved') && res.toasts[0].text.outcome.includes('#15020 no change'));
  assert.ok(res.toasts[1].text.head.includes('hivecommons/hive#15017') && res.toasts[1].text.head.includes('approved'));
  assert.strictEqual(res.toasts[2].text.type, 'info', 'the no-op PR reads as no change');
  assert.strictEqual(dialogs.length, 1, 'one confirm for the batch'); assert.ok(dialogs[0].cfg.message.includes('hivecommons/hive#15017, hivecommons/hive#15020'), dialogs[0].cfg.message);
  // A PR row never runs the all-PRs action.
  const none = await runRowAction('decide:delegate-all', a);
  assert.strictEqual(none.toasts.length, 0); assert.strictEqual(calls.length, 1);
})().catch(e => { console.error(e); process.exit(1); });
`)
}

// stageTrackerConfirmOptOutJS lifts the per-action confirm opt-out and the
// dialog payload builder, with a localStorage stub and recording
// hiveConfirm / hivePrompt stand-ins.
func stageTrackerConfirmOptOutJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_CONFIRM_KEY_PREFIX"),
		jsConstLine(t, html, "STAGE_TRACKER_CONFIRM_SKIP"),
		jsConstLine(t, html, "STAGE_TRACKER_CONFIRM_CHECK_LABEL"),
		jsConstLine(t, html, "STAGE_TRACKER_DELEGATE_EXPLANATION"),
		jsConstLine(t, html, "STAGE_TRACKER_SHORT_SHA"),
		jsConstLine(t, html, "STAGE_TRACKER_SENTINEL_TIP"),
		jsConstLine(t, html, "STAGE_TRACKER_SENTINEL_TIP_FALLBACK"),
		jsFunc(t, html, "stageTrackerSentinelTip"),
		jsFunc(t, html, "stageTrackerSentinelPreface"),
		jsFunc(t, html, "stageTrackerConfirmSkipped"),
		jsFunc(t, html, "stageTrackerSetConfirmSkip"),
		jsFunc(t, html, "stageTrackerSkippedConfirms"),
		jsFunc(t, html, "stageTrackerResetConfirms"),
		jsFunc(t, html, "stageTrackerAskConfig"),
		jsFunc(t, html, "stageTrackerAsk"),
		jsFunc(t, html, "stageTrackerDelegateConfirm"),
		jsFunc(t, html, "stageTrackerActionPayload"),
		`const store = {}; global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } };
global.window = { _hiveUser: 'clubanderson' };
const dialogs = []; let dialogAnswer = true;
global.hiveConfirm = async cfg => { dialogs.push({ kind: 'confirm', cfg }); return dialogAnswer; };
global.hivePrompt = async cfg => { dialogs.push({ kind: 'prompt', cfg }); return dialogAnswer; };`,
	}, "\n")
}

// TestStageTrackerRowActionConfirmOptOut (owner request, screenshot of the
// sentinel-flagged Approve dialog): every confirm a row action raises
// carries "Don't ask again for this action"; ticking it stores
// hive.tracker.confirm.<action>=skip, after which the action runs with no
// dialog and the sentinel ⚠ line the dialog would have shown rides the
// result toast instead; required inputs (a direction, the superseding PR)
// never get the box; Reset confirmations clears every key.
func TestStageTrackerRowActionConfirmOptOut(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="sts-confirms"`, `data-action="stageTrackerResetConfirms"`, `>Reset confirmations<`, `id="sts-confirms-skipped"`,
		`class="hive-dialog-check"`, `.hive-dialog-check {`, `.hive-dialog-check input {`,
		`stageTrackerConfirmSectionHtml(state.skippedConfirms)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing confirm opt-out wiring %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const toasts = []; global.showToast = (msg, type) => { toasts.push(type + ':' + msg); return null; }; global.dismissToast = () => {};
const flagged = { repo: 'o/crux', kind: 'pr', number: 30, url: 'u30', stage: 85, head: '40a959b1234', sentinel_alert: true, sentinel_reason: 'touches the auth path', modifiers: ['needs-decision'] };
const plain = { repo: 'o/r', kind: 'pr', number: 4, url: 'u4', stage: 85, modifiers: ['needs-decision'] };
(async () => {
  // The first Approve asks, with the ⚠ line and the opt-out box.
  assert.deepStrictEqual(await stageTrackerActionPayload([flagged], 'decide:approve'), {});
  assert.strictEqual(dialogs.length, 1);
  assert.strictEqual(dialogs[0].kind, 'confirm');
  assert.ok(dialogs[0].cfg.message.startsWith('⚠ o/crux#30 — Sentinel flagged security-relevant changes at 40a959b: touches the auth path'), dialogs[0].cfg.message);
  assert.deepStrictEqual(dialogs[0].cfg.check, { label: "Don't ask again for this action" });
  assert.ok(dialogs[0].cfg.danger);
  assert.strictEqual(stageTrackerConfirmSkipped('decide:approve'), false, 'answering without the box keeps asking');
  // Ticking the box persists the opt-out for that action only.
  dialogAnswer = { checked: true };
  assert.deepStrictEqual(await stageTrackerActionPayload([plain], 'decide:approve'), {});
  assert.strictEqual(store['hive.tracker.confirm.decide:approve'], 'skip');
  assert.deepStrictEqual(stageTrackerSkippedConfirms(), ['decide:approve']);
  assert.strictEqual(stageTrackerConfirmSkipped('decide:delegate'), false);
  // Skipped: no dialog, the action runs; the ⚠ line is in the toast.
  dialogs.length = 0;
  assert.deepStrictEqual(await stageTrackerActionPayload([flagged], 'decide:approve'), {});
  assert.strictEqual(dialogs.length, 0, 'no dialog once skipped');
  const text = stageTrackerResultText({ action: 'approved', target: { repo: 'o/crux', kind: 'pr', number: 30, url: 'u30' }, outcome: '/lgtm + /approve posted.', next: 'Merge gate.' }, flagged, { id: 'decide:approve' });
  assert.ok(text.warn.startsWith('⚠ o/crux#30 — Sentinel flagged security-relevant changes at 40a959b'), 'the skipped dialog\'s warning reaches the toast: ' + text.warn);
  const toast = stageTrackerResultToast({ action: 'approved', target: { repo: 'o/crux', kind: 'pr', number: 30, url: 'u30' }, outcome: '/lgtm + /approve posted.', next: '' }, flagged, { id: 'decide:approve' });
  assert.ok(toast.el.children.some(c => c.className === 'ham-result-warn' && c.textContent.includes('Sentinel flagged')), 'toast carries the ⚠ line');
  // Let hive decide: the explanation paragraph asks once, then is skipped.
  dialogAnswer = { checked: true };
  assert.deepStrictEqual(await stageTrackerActionPayload([plain], 'decide:delegate'), {});
  assert.strictEqual(dialogs[0].cfg.title, 'Let hive decide');
  assert.ok(dialogs[0].cfg.message.includes('Rails: it never merges'));
  dialogs.length = 0;
  assert.deepStrictEqual(await stageTrackerActionPayload([plain], 'decide:delegate'), {});
  assert.strictEqual(dialogs.length, 0);
  // Optional-comment prompts skip to an empty comment; required ones never carry the box.
  dialogAnswer = { value: 'because', checked: true };
  assert.deepStrictEqual(await stageTrackerActionPayload([plain], 'decide:close-wontfix'), { comment: 'because' });
  assert.deepStrictEqual(dialogs[0].cfg.check, { label: "Don't ask again for this action" });
  dialogs.length = 0;
  assert.deepStrictEqual(await stageTrackerActionPayload([plain], 'decide:close-wontfix'), { comment: '' });
  assert.strictEqual(dialogs.length, 0);
  dialogAnswer = 'drop the retry loop';
  assert.deepStrictEqual(await stageTrackerActionPayload([plain], 'decide:direction'), { comment: 'drop the retry loop' });
  assert.strictEqual(dialogs[0].cfg.check, undefined, 'a required direction always asks');
  assert.strictEqual(stageTrackerConfirmSkipped('decide:direction'), false);
  dialogs.length = 0; dialogAnswer = 'o/r#9';
  _stageTracker = { items: [plain] };
  assert.deepStrictEqual(await stageTrackerActionPayload([plain], 'decide:close-superseded'), { comment: 'o/r#9', supersededBy: 'o/r#9' });
  assert.strictEqual(dialogs[0].cfg.check, undefined, 'the superseding PR is always asked');
  assert.deepStrictEqual(dialogs[1].cfg.check, { label: "Don't ask again for this action" }, 'the optional comment is skippable');
  // Cancelling never persists anything.
  dialogs.length = 0; dialogAnswer = false;
  assert.strictEqual(await stageTrackerActionPayload([plain], 'needs-human'), null);
  assert.strictEqual(stageTrackerConfirmSkipped('needs-human'), false);
  // Reset turns every dialog back on.
  assert.deepStrictEqual(stageTrackerSkippedConfirms(), ['decide:delegate', 'decide:approve', 'decide:close-wontfix']);
  assert.strictEqual(stageTrackerResetConfirms(), 3);
  assert.deepStrictEqual(stageTrackerSkippedConfirms(), []);
  assert.deepStrictEqual(Object.keys(store), []);
  assert.ok(toasts.some(x => x.startsWith('success:Row actions ask again')));
  dialogs.length = 0; dialogAnswer = true;
  await stageTrackerActionPayload([plain], 'decide:approve');
  assert.strictEqual(dialogs.length, 1, 'asks again after reset');
})().catch(e => { console.error(e); process.exit(1); });
`)
}

// TestStageTrackerRowActionResults (owner request): every row action goes
// through runRowAction → a persistent result toast built from the server's
// shared result {action, target, outcome, next, rule} — headline naming the
// target when it differs from the clicked row, ✕, link — that leaves only
// when the tracker reports the target in a new state; the affected row and
// the nested PR sub-row read "acting…" from the click until that state
// change (or at once on a no-op or an error); no-ops say so plainly;
// older hives without a result still get a toast from the legacy fields.
func TestStageTrackerRowActionResults(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`.cue-acting > .ham-strip-row .ham-seg.active {`, `.cue-acting-cap {`, `.ham-result-toast {`, `.ham-result-warn {`, `.ham-result-link {`,
		`if (typeof stageTrackerPendingSync === 'function') stageTrackerPendingSync(el);`,
		`stageTrackerResultToastsSync(data); stageTrackerPendingExpire(data);`,
		`async function stageTrackerDoAction(items, id) { return runRowAction(id, items); }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing row-action result wiring %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const toasts = []; const dismissed = [];
global.showToast = (msg, type) => { toasts.push(type + ':' + msg); return null; };
global.dismissToast = (el, msg, type, force) => { dismissed.push({ el, msg, type, force }); if (el && el.remove) el.remove(); };
global.setTimeout = fn => fn();
global.fetch = null;
function stageTrackerProgressToast() { return { update() {}, done(msg, type) { if (msg) toasts.push('summary:' + type + ':' + msg); } }; }
function stageTrackerAfterActions() {}
const nested = { number: 14947, title: 'fix', url: 'u947', stage: 85, caption: 'Needs human decision', modifiers: ['needs-decision'] };
const issue = { repo: 'o/r', kind: 'issue', number: 14938, url: 'u938', stage: 85, caption: 'Needs human decision', modifiers: ['needs-decision'], linked_prs: [nested] };
const held = { repo: 'o/r', kind: 'issue', number: 7, url: 'u7', stage: 10, caption: 'Open', modifiers: [] };
_stageTracker = { items: [issue, held] };
hamPanel.appendChild(hamStubRow('o/r/issue#14938')); hamPanel.children[0].appendChild(hamStubRow('o/r/pr#14947', true)); hamPanel.appendChild(hamStubRow('o/r/issue#7'));
const row = k => hamPanel.querySelectorAll('[data-ham-key], [data-ham-sub-key]').find(n => n.getAttribute('data-ham-key') === k || n.getAttribute('data-ham-sub-key') === k);
const acting = k => row(k).classList.contains('cue-acting');
const caption = k => { const c = row(k).children[0].querySelector(':scope > .cue-acting-cap'); return c ? c.lastChild.nodeValue : ''; };

// Pure pieces first.
assert.strictEqual(stageTrackerActingCaption('decide:approve'), 'Approving…');
assert.strictEqual(stageTrackerActingCaption('whatever'), 'acting…');
assert.strictEqual(stageTrackerItemState({ stage: 85, caption: 'c', modifiers: ['b', 'a'] }), '85||c|a,b');
assert.strictEqual(stageTrackerTargetKey({ repo: 'o/r', kind: 'pr', number: 14947 }), 'o/r/pr#14947');
assert.strictEqual(stageTrackerTargetKey(null), '');
const target = { repo: 'o/r', kind: 'pr', number: 14947, url: 'u947' };
const decided = { action: 'adopted', rule: 'contributor-offline', target, outcome: 'Adopted o/r#14947 in supersede mode as o/r#14977.', next: 'The hive rebases and merges it unattended.' };
let text = stageTrackerResultText(decided, issue, { id: 'decide:delegate', rowRef: 'o/r#14938' });
assert.strictEqual(text.head, 'Hive decided on o/r#14938 → PR #14947: adopted (contributor offline)');
assert.strictEqual(text.outcome, decided.outcome); assert.strictEqual(text.next, decided.next); assert.strictEqual(text.type, 'success');
assert.strictEqual(text.url, 'u947'); assert.strictEqual(text.linkText, 'Open PR #14947');
text = stageTrackerResultText({ action: 'held', target: { repo: 'o/r', kind: 'issue', number: 7, url: 'u7' }, outcome: 'hold applied.', next: '' }, held, { id: 'hold' });
assert.strictEqual(text.head, 'Hold on o/r#7: held', 'the target is not repeated when it is the clicked row');
text = stageTrackerResultText({ action: 'noop-already-applied', target, outcome: 'No change — hive already recorded this decision at 12:01Z; still blocked by needs-human.', next: '' }, issue, { id: 'decide:delegate' });
assert.strictEqual(text.head, 'Let hive decide on o/r#14938 → PR #14947: no change'); assert.strictEqual(text.type, 'info');
assert.ok(text.outcome.startsWith('No change — hive already recorded this decision at'));
text = stageTrackerResultText(stageTrackerErrorResult('hold', held, 'write permission required'), held, { id: 'hold' });
assert.strictEqual(text.head, 'Hold failed on o/r#7'); assert.strictEqual(text.outcome, 'write permission required'); assert.strictEqual(text.type, 'error');
// Legacy answers (no result) still describe what happened.
assert.strictEqual(stageTrackerResultOf('retry', held, { status: 'debounced' }).action, 'noop-already-applied');
assert.strictEqual(stageTrackerResultOf('accept-triage', held, { alreadyAccepted: true }).action, 'noop-already-applied');
assert.strictEqual(stageTrackerResultOf('decide:delegate', issue, { decision: { verdict: 'approve', rule: 'small-low-risk' } }).rule, 'small-low-risk');
assert.strictEqual(stageTrackerResultOf('hold', held, {}).outcome, 'Hold done.');
assert.strictEqual(stageTrackerResultOf('hold', held, { result: { action: 'held', outcome: 'x' } }).target.number, 7, 'a result without a target takes the row');

// The toast persists across an unchanged payload and leaves on a state change.
const tst = stageTrackerResultToast(decided, issue, { id: 'decide:delegate' });
assert.strictEqual(hamToastContainer.children.length, 1);
assert.ok(tst.el.className.includes('toast success ham-result-toast sticky'), tst.el.className);
assert.ok(tst.el.children.some(c => c.className === 'toast-close'), 'has ✕');
const link = tst.el.children.find(c => c.tagName === 'a'); assert.strictEqual(link.href, 'u947'); assert.strictEqual(link.textContent, 'Open PR #14947 ↗');
assert.strictEqual(tst.key, 'o/r/pr#14947', 'keyed on the target, not the clicked row');
stageTrackerResultToastsSync({ items: [issue, held] });
assert.strictEqual(hamToastContainer.children.length, 1, 'same state → stays');
stageTrackerResultToastsSync({ items: [Object.assign({}, issue, { caption: 'other' }), held] });
assert.strictEqual(hamToastContainer.children.length, 1, 'the issue row changing is not the target changing');
stageTrackerResultToastsSync({ items: [Object.assign({}, issue, { linked_prs: [Object.assign({}, nested, { stage: 90, caption: 'Merge gate' })] }), held] });
assert.strictEqual(hamToastContainer.children.length, 0, 'the target moved → dismissed');
assert.ok(dismissed[0].force, 'dismissed through the sticky guard');
assert.deepStrictEqual(_stageTrackerResultToasts, []);
const gone = stageTrackerResultToast(decided, issue, { id: 'decide:delegate' });
stageTrackerResultToastsSync({ items: [held] });
assert.strictEqual(gone.el.parentNode, null, 'target gone from the tracker → dismissed');
// Error toasts are sticky but not tied to a row state.
const bad = stageTrackerResultToast(stageTrackerErrorResult('hold', held, 'boom'), held, { id: 'hold' });
assert.ok(bad.el.className.includes('error')); assert.deepStrictEqual(_stageTrackerResultToasts, []);
bad.el.remove();

// Acting state: the clicked issue row and its nested PR both act from the click.
const keys = stageTrackerPendingKeys({ repo: 'o/r', kind: 'pr', number: 14947 }, [issue]);
assert.deepStrictEqual(keys, ['o/r/pr#14947', 'o/r/issue#14938']);
const pend = stageTrackerSetPending(keys, 'Hive is deciding…', 1000);
assert.ok(acting('o/r/issue#14938') && acting('o/r/pr#14947')); assert.ok(!acting('o/r/issue#7'));
assert.strictEqual(caption('o/r/issue#14938'), 'Hive is deciding…'); assert.strictEqual(caption('o/r/pr#14947'), 'Hive is deciding…');
stageTrackerArmPending(pend, target);
assert.strictEqual(pend.key, 'o/r/pr#14947');
stageTrackerPendingExpire({ items: [issue, held] }, 2000); stageTrackerPendingSync(hamPanel);
assert.ok(acting('o/r/pr#14947'), 'still acting while the target reads the same state');
stageTrackerPendingExpire({ items: [Object.assign({}, issue, { linked_prs: [Object.assign({}, nested, { stage: 90 })] }), held] }, 3000); stageTrackerPendingSync(hamPanel);
assert.ok(!acting('o/r/issue#14938') && !acting('o/r/pr#14947'), 'clears once the target moves');
assert.strictEqual(caption('o/r/issue#14938'), '', 'the caption node is removed');
const p2 = stageTrackerSetPending(['o/r/issue#7'], 'Holding…', 1000);
stageTrackerArmPending(p2, { repo: 'o/r', kind: 'issue', number: 7 });
stageTrackerPendingExpire({ items: [issue, held] }, 1000 + STAGE_TRACKER_ACTING_MAX_MS + 1); stageTrackerPendingSync(hamPanel);
assert.ok(!acting('o/r/issue#7'), 'a row never acts forever');
const p3 = stageTrackerSetPending(['o/r/issue#7'], 'Holding…', 1000);
stageTrackerClearPending(p3);
assert.ok(!acting('o/r/issue#7'), 'a no-op or an error clears at once');

// The whole run: one request, pending set before the fetch, toast from the server's result.
(async () => {
  const seen = [];
  global.fetch = async (url, init) => { seen.push({ url, acting: acting('o/r/issue#7') }); return { ok: true, status: 200, json: async () => ({ result: { action: 'held', target: { repo: 'o/r', kind: 'issue', number: 7, url: 'u7' }, outcome: 'hold applied.', next: 'Agents leave it alone.' } }) }; };
  const out = await runRowAction('hold', held);
  assert.strictEqual(seen.length, 1); assert.ok(seen[0].acting, 'the row acts before the request leaves');
  assert.ok(out.toast && out.toast.el.children[0].textContent === 'Hold on o/r#7: held', out.toast && out.toast.el.children[0].textContent);
  assert.strictEqual(out.pending.length, 1); assert.strictEqual(out.pending[0].key, 'o/r/issue#7');
  assert.ok(acting('o/r/issue#7'), 'acts until the tracker reports the new state');
  assert.ok(!toasts.some(x => x.startsWith('summary:')), 'a single success needs no summary line beside its toast');
  stageTrackerPendingExpire({ items: [issue, Object.assign({}, held, { modifiers: ['hold'] })] }, Date.now()); stageTrackerPendingSync(hamPanel);
  assert.ok(!acting('o/r/issue#7'));
  // A no-op clears the acting state right away and says so plainly.
  global.fetch = async () => ({ ok: true, status: 200, json: async () => ({ result: { action: 'noop-already-applied', target: { repo: 'o/r', kind: 'issue', number: 7 }, outcome: 'No change — no hold label was on this item.', next: '' } }) });
  const noop = await runRowAction('release', held);
  assert.ok(!acting('o/r/issue#7')); assert.strictEqual(noop.toast.text.outcome, 'No change — no hold label was on this item.'); assert.strictEqual(noop.toast.text.type, 'info');
  // An error clears it and is shown plainly.
  global.fetch = async () => ({ ok: false, status: 403, json: async () => ({ error: 'write permission required' }) });
  const err = await runRowAction('hold', held);
  assert.ok(!acting('o/r/issue#7')); assert.strictEqual(err.toast.text.type, 'error'); assert.strictEqual(err.toast.text.outcome, 'write permission required');
  assert.ok(toasts.some(x => x.startsWith('summary:error:Hold: 0/1 done')), 'a failure keeps the summary line');
  // An issue row's delegated decision targets the nested PR: both act, the toast names the PR.
  global.fetch = async (url, init) => ({ ok: true, status: 200, json: async () => ({ status: 'done', result: decided }) });
  stageTrackerSetConfirmSkip('decide:delegate', true);
  const del = await runRowAction('decide:delegate', issue);
  assert.strictEqual(del.toast.text.head, 'Hive decided on o/r#14947 → PR #14947: adopted (contributor offline)'.replace('o/r#14947 → PR #14947', 'o/r#14947'), del.toast.text.head);
  assert.deepStrictEqual(del.pending[0].keys, ['o/r/pr#14947', 'o/r/issue#14938']);
  assert.ok(acting('o/r/issue#14938') && acting('o/r/pr#14947'));
})().catch(e => { console.error(e); process.exit(1); });
`)
}
