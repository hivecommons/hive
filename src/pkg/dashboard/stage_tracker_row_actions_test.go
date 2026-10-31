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
		jsConstBlock(t, html, "STAGE_TRACKER_DECISION_GLOSSARY"),
		jsFunc(t, html, "stageTrackerPlainWords"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsConstBlock(t, html, "STAGE_TRACKER_ACTION_LABELS"),
		jsConstLine(t, html, "STAGE_TRACKER_WHY_WRITE"),
		jsConstLine(t, html, "STAGE_TRACKER_WHY_ROLE"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIONABLE_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_LABEL"),
		jsConstLine(t, html, "STAGE_TRACKER_NEW_MOVER_MAINTAINER"),
		jsConstLine(t, html, "STAGE_TRACKER_NEW_MOVER_HIVE"),
		jsConstLine(t, html, "STAGE_TRACKER_MINUTE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_DELEGATE_POLL_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_DELEGATE_POLL_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_DELEGATE_ALL_MIN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_ACTIONS"),
		jsConstLine(t, html, "STAGE_TRACKER_GENERIC_ACTIONS"),
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
		jsFunc(t, html, "stageTrackerDelegateTargets"),
		jsFunc(t, html, "stageTrackerDecideOptions"),
		jsFunc(t, html, "stageTrackerSupersederOptions"),
		jsFunc(t, html, "stageTrackerSupersederDefault"),
		jsFunc(t, html, "stageTrackerSupersederValid"),
		jsFunc(t, html, "stageTrackerTriageNeeded"),
		jsFunc(t, html, "stageTrackerTriageNewState"),
		jsFunc(t, html, "stageTrackerTriageLabel"),
		jsFunc(t, html, "stageTrackerNextPassLeftMs"),
		jsFunc(t, html, "stageTrackerNextPassWord"),
		jsFunc(t, html, "stageTrackerTriageHint"),
		jsFunc(t, html, "stageTrackerTriageAccepted"),
		jsFunc(t, html, "stageTrackerDecisionOf"),
		jsFunc(t, html, "stageTrackerActionAllowed"),
		jsFunc(t, html, "stageTrackerLeadActions"),
		// Takeover: Adopt now / Reroute (hivecommons/hive#15038).
		jsConstLine(t, html, "STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE"),
		jsConstLine(t, html, "STAGE_TRACKER_REROUTE_ANY"),
		jsConstLine(t, html, "STAGE_TRACKER_REROUTE_PREFIX"),
		jsFunc(t, html, "stageTrackerTakeoverOf"),
		jsFunc(t, html, "stageTrackerActionBase"),
		jsFunc(t, html, "stageTrackerRerouteTargetOf"),
		jsFunc(t, html, "stageTrackerClankerLabel"),
		jsFunc(t, html, "stageTrackerRerouteOptions"),
		jsFunc(t, html, "stageTrackerTakeoverActions"),
		jsFunc(t, html, "stageTrackerReroutePillHtml"),
		jsFunc(t, html, "stageTrackerTakeoverTarget"),
		jsFunc(t, html, "stageTrackerAdoptNowConfirm"),
		jsFunc(t, html, "stageTrackerRerouteConfirm"),
		jsFunc(t, html, "stageTrackerRerouteTargetValid"),
		jsConstLine(t, html, "STAGE_TRACKER_BLOCKER_ACTIONS"),
		jsFunc(t, html, "stageTrackerBlockerActions"),
		jsFunc(t, html, "stageTrackerRowActions"),
		jsFunc(t, html, "stageTrackerLeadAction"),
		jsFunc(t, html, "stageTrackerMaintainerMoved"),
		jsFunc(t, html, "stageTrackerDevWarn"),
		jsFunc(t, html, "stageTrackerPRListedOwnRow"),
		jsFunc(t, html, "stageTrackerPRDrawnUnderOther"),
		jsFunc(t, html, "stageTrackerLeadActionOnRow"),
		jsFunc(t, html, "stageTrackerLeadActionHtml"),
		jsFunc(t, html, "stageTrackerMenuTextHtml"),
		jsFunc(t, html, "stageTrackerFlatActions"),
		jsFunc(t, html, "stageTrackerSelectionActions"),
		jsFunc(t, html, "stageTrackerSelectionTriage"),
		jsFunc(t, html, "stageTrackerMenuItemHtml"),
		jsFunc(t, html, "stageTrackerMenuHasHints"),
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
		jsConstLine(t, html, "STAGE_TRACKER_ACTION_NO_EFFECT"),
		jsConstLine(t, html, "STAGE_TRACKER_RESULT_TOAST_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_TOAST_DEFAULT_S"),
		jsConstLine(t, html, "STAGE_TRACKER_TOAST_MIN_S"),
		jsConstLine(t, html, "STAGE_TRACKER_TOAST_MAX_S"),
		jsConstLine(t, html, "STAGE_TRACKER_TOAST_KEY"),
		jsFunc(t, html, "stageTrackerToastSecondsValid"),
		jsFunc(t, html, "stageTrackerToastSeconds"),
		jsFunc(t, html, "stageTrackerToastLocal"),
		jsFunc(t, html, "stageTrackerToastMs"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTING_MAX_MS"),
		jsConstBlock(t, html, "STAGE_TRACKER_ACTING_CAPTIONS"),
		jsConstLine(t, html, "STAGE_TRACKER_REBASE_MERGE_LABEL"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTION_REBASE_QUEUED"),
		jsFunc(t, html, "stageTrackerNeedsRebase"),
		jsFunc(t, html, "stageTrackerRebaseMergeLive"),
		jsConstBlock(t, html, "STAGE_TRACKER_ACTION_WORDS"),
		"var _stageTrackerResultToasts = []; var _stageTrackerPending = [];",
		jsFunc(t, html, "stageTrackerActingCaption"),
		jsFunc(t, html, "stageTrackerItemState"),
		jsFunc(t, html, "stageTrackerTargetKey"),
		jsFunc(t, html, "stageTrackerResultOf"),
		jsFunc(t, html, "stageTrackerErrorResult"),
		jsFunc(t, html, "stageTrackerResultWarn"),
		jsConstLine(t, html, "STAGE_TRACKER_RFC3339_RE"),
		// Local clock words are tested in stage_tracker_decided_ui_test.go;
		// here they are marked so the sentence shape can be asserted.
		"function agentNowClockTime(ms, nowMs) { return new Date(ms).toISOString().slice(11, 16); } function formatDashboardDate(v) { return new Date(v).toISOString().slice(0, 10); }",
		jsFunc(t, html, "stageTrackerLocalStamps"),
		jsFunc(t, html, "stageTrackerResultText"),
		jsFunc(t, html, "stageTrackerResultToast"),
		jsFunc(t, html, "stageTrackerResultToastTimed"),
		jsFunc(t, html, "stageTrackerResultToastStale"),
		jsFunc(t, html, "stageTrackerResultToastsSync"),
		jsFunc(t, html, "stageTrackerPendingKeys"),
		jsFunc(t, html, "stageTrackerDecisionTargetKey"),
		jsFunc(t, html, "stageTrackerSetPending"),
		jsFunc(t, html, "stageTrackerArmPending"),
		jsFunc(t, html, "stageTrackerClearPending"),
		jsFunc(t, html, "stageTrackerPendingLive"),
		jsFunc(t, html, "stageTrackerPendingExpire"),
		jsFunc(t, html, "stageTrackerLeadBusySync"),
		jsFunc(t, html, "stageTrackerPendingSync"),
		// Progress + failure surfacing for every mutating action (hivecommons/hive#15436).
		jsConstBlock(t, html, "STAGE_TRACKER_TERMINAL_CAPTIONS"),
		jsFunc(t, html, "stageTrackerTerminalCaption"),
		jsConstBlock(t, html, "STAGE_TRACKER_ACTION_LEDGER_NAME"),
		"var _stageTrackerActionFailures = {};",
		jsFunc(t, html, "stageTrackerNoteActionFailure"),
		jsFunc(t, html, "stageTrackerClearActionFailure"),
		jsFunc(t, html, "stageTrackerActionFailureOf"),
		jsFunc(t, html, "stageTrackerAdvisoryOf"),
		jsFunc(t, html, "stageTrackerAdvisoryCloseHint"),
		jsFunc(t, html, "stageTrackerAdvisoryCloseMessage"),
		jsFunc(t, html, "stageTrackerNothingToActOn"),
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
    setAttribute(k, v) { el.attrs[k] = String(v); }, getAttribute(k) { return k in el.attrs ? el.attrs[k] : null; }, removeAttribute(k) { delete el.attrs[k]; },
    addEventListener(n, fn) { el._listeners[n] = fn; }, remove() { if (el.parentNode) el.parentNode.children = el.parentNode.children.filter(c => c !== el); el.parentNode = null; },
    get lastChild() { return el.children[el.children.length - 1] || null; },
    querySelector(sel) {
      // ":scope > .child .descendant[attr]" — the lead button under the row head.
      const d = sel.match(/^:scope > \.([\w-]+) \.([\w-]+)(?:\[([\w-]+)\])?$/);
      if (d) { const head = el.children.find(c => c.classList && c.classList.contains(d[1])); if (!head) return null; const walk = n => { for (const c of (n.children || [])) { if (c.classList && c.classList.contains(d[2]) && (!d[3] || c.attrs[d[3]] != null)) return c; const r = walk(c); if (r) return r; } return null; }; return walk(head); }
      const m = sel.match(/^:scope > \.([\w-]+)$/); if (!m) return null; return el.children.find(c => c.classList && c.classList.contains(m[1])) || null; },
    querySelectorAll(sel) { const out = []; const walk = n => { for (const c of (n.children || [])) { if (!c.attrs) continue; if ((sel.includes('[data-ham-key]') && c.attrs['data-ham-key'] != null) || (sel.includes('[data-ham-sub-key]') && c.attrs['data-ham-sub-key'] != null)) out.push(c); walk(c); } }; walk(el); return out; },
  };
  return el;
}
const hamToastContainer = hamStubEl('div');
// The shared countdown (toast_countdown_bar_test.go covers the real one):
// record what a timed result toast arms so tests can fire it by hand.
global.toastCountdownBar = (el, ms, onExpire) => { el._countdown = { ms, onExpire }; };
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
// The prompt prefills the sweep's finding (hive's hive #15303 → #15304), the
// lone sibling when there is no finding, and nothing otherwise.
assert.strictEqual(stageTrackerSupersederDefault([{ repo: 'o/r', number: 458, superseded: { reason: 'superseded', closer_pr: 457, closer_repo: 'r' } }]), 'o/r#457');
assert.strictEqual(stageTrackerSupersederDefault([{ repo: 'o/r', number: 458, superseded: { closer_pr: 457, closer_repo: 'o/other' } }]), 'o/other#457');
assert.strictEqual(stageTrackerSupersederDefault([{ repo: 'o/r', number: 458, superseded: true, siblings: [457] }]), 'o/r#457');
assert.strictEqual(stageTrackerSupersederDefault([{ repo: 'o/r', number: 458, siblings: [1, 2] }]), '');
assert.strictEqual(stageTrackerSupersederDefault([{ repo: 'o/r', number: 458, superseded: { closer_pr: 457 } }, { repo: 'o/r', number: 459 }]), '', 'bulk selections are not prefilled');

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
assert.ok(menu.startsWith('<div class="ham-menu ham-menu-hinted" role="menu"'), 'the rescan hint makes it a hinted menu: ' + menu.slice(0, 60));
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
		`'decide:delegate-all': 'Hive deciding each PR…'`,
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
assert.ok(menu.includes('data-ham-act="decide:delegate-all"') && menu.includes('Let hive decide — all 2 PRs<span class="ham-menu-hint">per PR</span>'));

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

// A parent selected alone or with its PR sub-rows offers bulk delegation.
for (const selected of [[issue], [issue, a], [issue, a, b], [b, issue, a]]) {
  const actions = stageTrackerSelectionActions(selected, () => ctx);
  assert.strictEqual(actions[1].enabled, true, 'multi-PR parent must not grey out delegation');
  assert.strictEqual(actions[0].submenu.find(x => x.id === 'decide:delegate').enabled, true);
  assert.strictEqual(actions[0].submenu.find(x => x.id === 'decide:approve').enabled, false, 'no ambiguous bulk approval');
}
for (const denied of [{ ...ctx, user: false }, { ...ctx, canHold: false }]) {
  const action = stageTrackerSelectionActions([issue, a, b], () => denied)[1];
  assert.strictEqual(action.enabled, false, 'parent delegation retains write gates');
  assert.ok(action.why.includes(STAGE_TRACKER_WHY_WRITE));
}
assert.strictEqual(stageTrackerSelectionActions([issue, plain], () => ctx)[1].enabled, false);
// Bulk delegation on a parent expands to its open PRs only when no single
// PR is its decision target; with one parked PR the row keeps that PR.
assert.deepStrictEqual(stageTrackerDelegateTargets(issue).map(p => p.number), [15017, 15020]);
const oneParked = { ...issue, linked_prs: [{ ...issue.linked_prs[0], stage: 60, manual_merge: false, human_merge_reason: '' }, issue.linked_prs[1], issue.linked_prs[2]] };
assert.strictEqual(stageTrackerDecisionTarget(oneParked).number, 15020);
assert.deepStrictEqual(stageTrackerNestedOpenPRs(oneParked).map(p => p.number), [15017, 15020], 'still a two-PR parent for delegate-all');
assert.deepStrictEqual(stageTrackerDelegateTargets(oneParked).map(p => p.number), [15020], 'single parked PR: only that PR');
assert.deepStrictEqual(stageTrackerDelegateTargets(a).map(p => p.number), [15017], 'a parked PR row is its own target');
assert.deepStrictEqual(stageTrackerDelegateTargets(plain), []);
// A parent's open PRs count as PR targets in the issue/PR mix check: a
// multi-PR parent plus a 045 issue stays refused, as before.
const parked045 = { repo: 'o/r', kind: 'issue', number: 1, url: 'u', stage: 45, modifiers: ['needs-decision'] };
const mixed = stageTrackerSelectionActions([issue, parked045], () => ctx);
assert.strictEqual(mixed[1].enabled, false, 'parent + parked issue mixes kinds');
assert.ok(mixed[1].why.includes('mixes'), mixed[1].why);
assert.strictEqual(stageTrackerSelectionActions([issue, a, parked045], () => ctx)[1].enabled, false);
assert.strictEqual(stageTrackerSelectionActions([parked045, { ...parked045, number: 2 }], () => ctx)[1].enabled, true, 'two parked issues still delegate');

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
  assert.ok(res.toasts[1].text.outcome.includes('hivecommons/hive#15017') && res.toasts[1].text.outcome.includes('approved'), res.toasts[1].text.outcome);
  assert.strictEqual(res.toasts[2].text.type, 'info', 'the no-op PR reads as no change');
  assert.strictEqual(dialogs.length, 1, 'one confirm for the batch'); assert.ok(dialogs[0].cfg.message.includes('hivecommons/hive#15017, hivecommons/hive#15020'), dialogs[0].cfg.message);
  // A PR row never runs the all-PRs action.
  const none = await runRowAction('decide:delegate-all', a);
  assert.strictEqual(none.toasts.length, 0); assert.strictEqual(calls.length, 1);

  // Selection delegation expands parents like delegate-all, but dedupes
  // selected sub-rows before confirmation and before sending any requests.
  global.fetch = async (url, init) => {
    calls.push({ url, body: JSON.parse(init.body) });
    const number = Number(url.split('/').slice(-2)[0]);
    return { ok: true, status: 200, json: async () => ({ status: 'done', result: { action: STAGE_TRACKER_ACTION_NOOP, target: { repo: issue.repo, kind: 'pr', number }, outcome: 'No change' } }) };
  };
  for (const selected of [[issue], [issue, a, b], [b, issue, a]]) {
    calls.length = 0; dialogs.length = 0;
    await runRowAction('decide:delegate', selected);
    // Let hive decide previews first: one plan call per PR, then one
    // decide call per PR after the operator picks (#15126).
    assert.deepStrictEqual(calls.filter(c => c.url.endsWith('/plan')).map(c => c.url).sort(), [
      '/api/repos/hivecommons/hive/items/15017/decide-delegated/plan',
      '/api/repos/hivecommons/hive/items/15020/decide-delegated/plan',
    ]);
    assert.deepStrictEqual(calls.filter(c => !c.url.endsWith('/plan')).map(c => c.url).sort(), [
      '/api/repos/hivecommons/hive/items/15017/decide-delegated',
      '/api/repos/hivecommons/hive/items/15020/decide-delegated',
    ]);
    assert.ok(calls.every(c => c.body.type === 'pr'), 'never delegate the ambiguous issue itself');
    assert.ok(calls.filter(c => !c.url.endsWith('/plan')).every(c => /^Let hive do that · previewed /.test(c.body.chosen)), 'the pressed button rides the request');
    assert.strictEqual(dialogs.length, 1); assert.strictEqual(dialogs[0].kind, 'plan');
    for (const number of [15017, 15020]) {
      assert.strictEqual(dialogs[0].cfg.message.split('hivecommons/hive#' + number).length - 1, 1, 'confirm each PR exactly once');
    }
    assert.ok(!dialogs[0].cfg.message.includes('hivecommons/hive#15009'));
  }
  // The row menu's "Decide PR #15020 › Let hive decide" on a parent with
  // one parked PR (of two open) delegates that PR alone — never the other
  // in-flight PR, which only the explicit delegate-all touches.
  for (const selected of [oneParked, [oneParked], [oneParked, b]]) {
    calls.length = 0; dialogs.length = 0;
    await runRowAction('decide:delegate', selected);
    assert.deepStrictEqual(calls.filter(c => !c.url.endsWith('/plan')).map(c => c.url), ['/api/repos/hivecommons/hive/items/15020/decide-delegated']);
    assert.strictEqual(dialogs.length, 1);
    assert.ok(!dialogs[0].cfg.message.includes('hivecommons/hive#15017'), dialogs[0].cfg.message);
  }
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
		jsFunc(t, html, "stageTrackerDCOCaveat"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_BUTTON_DELEGATE"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_BUTTON_MERGE"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_MERGE_ACTIONS"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_TITLE"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_EYEBROW"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_NEXT_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_TITLE_MAX"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_RAILS"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_MERGE_WHY"),
		jsFunc(t, html, "hiveDialogEscape"),
		jsFunc(t, html, "stageTrackerPlanSentence"),
		jsFunc(t, html, "stageTrackerPlanTruncate"),
		jsFunc(t, html, "stageTrackerDelegatePlanView"),
		jsFunc(t, html, "stageTrackerDelegatePlanHtml"),
		jsFunc(t, html, "stageTrackerDelegatePlanVisibleText"),
		jsFunc(t, html, "stageTrackerDelegatePlanMergeAction"),
		jsFunc(t, html, "stageTrackerDelegatePlanOffersMerge"),
		jsFunc(t, html, "stageTrackerDelegatePlanMergeLabel"),
		jsFunc(t, html, "stageTrackerDelegatePlanMergeable"),
		jsFunc(t, html, "stageTrackerDelegatePlanMergeReason"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_PREFETCH_MS"),
		jsConstLine(t, html, "_stageTrackerPlanPrefetch"),
		jsFunc(t, html, "stageTrackerPlanRequestKey"),
		jsFunc(t, html, "stageTrackerPostDelegatePlan"),
		jsFunc(t, html, "stageTrackerPrefetchDelegatePlan"),
		jsFunc(t, html, "stageTrackerFetchDelegatePlan"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_SKELETON_LINES"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_RETRY"),
		jsConstLine(t, html, "STAGE_TRACKER_PLAN_BUTTONS_OPEN"),
		jsFunc(t, html, "stageTrackerDelegatePlanSkeletonContent"),
		jsFunc(t, html, "stageTrackerDelegatePlanSkeletonHtml"),
		jsFunc(t, html, "stageTrackerDelegatePlanErrorContent"),
		jsFunc(t, html, "stageTrackerDelegatePlanSubject"),
		jsConstLine(t, html, "_stageTrackerDelegatePreviewing"),
		jsFunc(t, html, "stageTrackerDelegatePreview"),
		jsFunc(t, html, "stageTrackerActionPayload"),
		jsFunc(t, html, "stageTrackerDirectionCarrier"),
		// The preview dialog is recorded like the confirms: it opens as the
		// skeleton (planGate, when a test sets it, holds the plan back), then
		// shows the plan or the inline error; its answer is the "Let hive do
		// that" button unless a test sets planAnswer.
		`let planAnswer = null; let planGate = null;
function stageTrackerDelegatePlanDialog(load, sentinel, opts) {
  const skeleton = { kind: 'plan-skeleton', cfg: { html: stageTrackerDelegatePlanSkeletonHtml((opts && opts.subject) || '', sentinel, opts && opts.caption), opts } };
  dialogs.push(skeleton);
  return Promise.resolve(planGate).then(() => load()).then(plans => {
    if (opts && typeof opts.onSettled === 'function') opts.onSettled();
    dialogs.splice(dialogs.indexOf(skeleton), 1);
    const failed = plans.filter(p => p.error);
    if (failed.length) { dialogs.push({ kind: 'plan-error', cfg: { html: stageTrackerDelegatePlanErrorContent((opts && opts.subject) || '', failed), failed } }); return null; }
    const view = stageTrackerDelegatePlanView(plans, sentinel); const html = stageTrackerDelegatePlanHtml(view);
    dialogs.push({ kind: 'plan', cfg: { title: view.title, message: stageTrackerDelegatePlanVisibleText(html), html, view, plans } });
    return planAnswer === null ? { chosen: 'Let hive do that · previewed ' + plans.map(p => p.would || '?').join('/') } : planAnswer;
  });
}`,
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
  // Let hive decide always previews what hive would do (#15126): the
  // plan dialog has no opt-out, and nothing is written until a button is
  // pressed. The delegate-all confirm keeps the opt-out key.
  const planCalls = []; global.fetch = async (url, init) => { planCalls.push(url); return { ok: true, status: 200, json: async () => ({ would: 'adopt-supersede', summary: 're-author o/r#4 as a hive PR with the same commits and merge it; o/r#4 will be closed as superseded', why: 'Author @k is untrusted and their clanker is offline.', consequences: ['o/r#4 is closed as superseded'], alternatives: [{ action: 'decide:queue-merge', label: 'Approve and let hive merge', enabled: false, reason_if_disabled: 'Needs merge rights' }] }) }; };
  assert.deepStrictEqual(await stageTrackerActionPayload([plain], 'decide:delegate'), { chosen: 'Let hive do that · previewed adopt-supersede' });
  assert.deepStrictEqual(planCalls, ['/api/repos/o/r/items/4/decide-delegated/plan'], 'the preview is the plan route, never the decide route');
  assert.strictEqual(dialogs[0].kind, 'plan'); assert.strictEqual(dialogs[0].cfg.title, 'Let hive decide');
  assert.ok(dialogs[0].cfg.message.includes('Hive would Re-author o/r#4 as a hive PR') && dialogs[0].cfg.message.includes('Why Author @k is untrusted and their clanker is offline.'), dialogs[0].cfg.message);
  assert.strictEqual(stageTrackerDelegatePlanMergeable(dialogs[0].cfg.plans), false); assert.strictEqual(stageTrackerDelegatePlanMergeReason(dialogs[0].cfg.plans), 'Needs merge rights');
  assert.ok(!dialogs[0].cfg.html.includes('ham-plan-merge'), 'no merge button when the plan does not say mergeable_as_is');
  // Cancel: no payload, so runRowAction never sends the decide request.
  planAnswer = null; dialogs.length = 0;
  const cancelled = await (async () => { planAnswer = undefined; const r = await stageTrackerActionPayload([plain], 'decide:delegate'); planAnswer = null; return r; })();
  assert.strictEqual(cancelled, null, 'Cancel runs nothing');
  stageTrackerSetConfirmSkip('decide:delegate', true); dialogs.length = 0;
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

// TestStageTrackerApproveToastSentinelWording (owner, hivecommons/dibs#510:
// "approved `/lgtm` + `/approve` posted (released: approval-recorded,
// sentinel-label). ⚠ … a maintainer must review before merge. The merge
// gate takes it from here; the dashboard merges nothing" — "that's a
// confusing response"): the toast leads with the server's plain-words
// outcome, repeats the row's ⚠ sentinel paragraph only while the alert is
// still open after the action (sentinel_cleared drops it), renders Next
// as the concrete step, and lists what was posted on a muted line.
func TestStageTrackerApproveToastSentinelWording(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, `.ham-result-posted {`) {
		t.Fatalf("index.html missing the muted ham-result-posted rule")
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const flagged = { repo: 'hivecommons/dibs', kind: 'pr', number: 510, url: 'u510', stage: 85, head: '30e94a9ffff', sentinel_alert: true, sentinel_reason: 'possible security override', modifiers: ['needs-decision'] };
const target = { repo: 'hivecommons/dibs', kind: 'pr', number: 510, url: 'u510' };
const posted = ['\u0060/lgtm\u0060 posted', '\u0060/approve\u0060 posted', '\u0060sentinel-alert\u0060 label removed'];
// Released: the alert the row carried is gone, so no ⚠ paragraph.
const cleared = { action: 'approved', target, outcome: 'Approved #510 as @clubanderson · sentinel alert cleared by your review.', next: "Hive's merge sweep merges it on its next pass once CI is green and the head has soaked (≈30 min).", posted, sentinel_cleared: true };
let text = stageTrackerResultText(cleared, flagged, { id: 'decide:approve', rowRef: 'hivecommons/dibs#510' });
assert.strictEqual(text.head, 'Approve on hivecommons/dibs#510: approved');
assert.strictEqual(text.outcome, cleared.outcome);
assert.strictEqual(text.warn, '', 'a released alert is not re-warned: ' + text.warn);
assert.strictEqual(text.next, cleared.next);
assert.strictEqual(text.posted, 'Posted: /lgtm posted · /approve posted · sentinel-alert label removed');
assert.ok(!text.next.includes('merges nothing') && !text.outcome.includes('released:'), 'no "released:" / "merges nothing"');
let toast = stageTrackerResultToast(cleared, flagged, { id: 'decide:approve' });
let classes = toast.el.children.map(c => c.className).filter(c => c !== 'toast-close');
assert.deepStrictEqual(classes, ['ham-result-head', 'ham-result-outcome', 'ham-result-next', 'ham-result-posted', 'ham-result-link'], classes.join(','));
assert.ok(classes.indexOf('ham-result-next') < classes.indexOf('ham-result-posted'), 'the posted list is the muted trailer, under Next');
// Still open: the ⚠ paragraph stays and Next says what the person must do.
const open = { action: 'approved', target, outcome: 'Approved #510 as @clubanderson · sentinel alert still open.', next: 'The \u0060sentinel-alert\u0060 label is still on the PR: a maintainer must review the flagged head and take the label off on GitHub before hive\'s merge sweep will touch it.', posted: posted.slice(0, 2), sentinel_open: true };
text = stageTrackerResultText(open, flagged, { id: 'decide:approve', rowRef: 'hivecommons/dibs#510' });
assert.ok(text.warn.startsWith('⚠ hivecommons/dibs#510 — Sentinel flagged security-relevant changes at 30e94a9: possible security override'), text.warn);
assert.ok(text.next.includes('take the label off on GitHub'), text.next);
toast = stageTrackerResultToast(open, flagged, { id: 'decide:approve' });
classes = toast.el.children.map(c => c.className).filter(c => c !== 'toast-close');
assert.deepStrictEqual(classes, ['ham-result-head', 'ham-result-outcome', 'ham-result-warn', 'ham-result-next', 'ham-result-posted', 'ham-result-link'], classes.join(','));
// An older hive's result (no flags) keeps the legacy behaviour: warn from the row.
text = stageTrackerResultText({ action: 'approved', target, outcome: 'x', next: '' }, flagged, { id: 'decide:approve' });
assert.ok(text.warn.startsWith('⚠ hivecommons/dibs#510'), text.warn);
assert.strictEqual(text.posted, '');
// A human-merge lane: Next names who merges.
text = stageTrackerResultText({ action: 'approved', target, outcome: 'Approved #510 as @clubanderson.', next: 'Hive cannot merge this one (trust or role blocks it): merge it on GitHub yourself, or use Merge now as maintainer.', posted: posted.slice(0, 2) }, { repo: 'hivecommons/dibs', kind: 'pr', number: 510, url: 'u510', stage: 90 }, { id: 'decide:approve' });
assert.strictEqual(text.warn, '');
assert.ok(text.next.startsWith('Hive cannot merge this one'), text.next);
// A Prow-managed repo: the headline says recorded-in-hive, Next says who still approves.
text = stageTrackerResultText({ action: 'approved', target, outcome: 'Approval of #510 recorded in hive as @clubanderson.', next: 'Prow approval still needs an OWNERS reviewer: the relayed \u0060/approve\u0060 came from hive\'s account, which Prow does not count. Hive\'s merge sweep merges it on its next pass once CI is green and the head has soaked (≈3 min).', posted: posted.slice(0, 2) }, { repo: 'hivecommons/dibs', kind: 'pr', number: 510, url: 'u510', stage: 85 }, { id: 'decide:approve' });
assert.ok(text.outcome.startsWith('Approval of #510 recorded in hive'), text.outcome);
assert.ok(text.next.startsWith('Prow approval still needs an OWNERS reviewer'), text.next);
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
// The decision toast leads with the outcome; the rule in plain words
// (never the bare slug) is the second line (#15126).
assert.strictEqual(text.head, 'Adopted o/r#14947 in supersede mode as o/r#14977.', 'the outcome leads');
assert.strictEqual(text.outcome, "Hive decided on o/r#14938 → PR #14947: adopted (the author hasn't been reachable since the nudge)", 'the rule in plain words, never the bare slug');
assert.strictEqual(text.next, decided.next); assert.strictEqual(text.type, 'success');
assert.strictEqual(text.url, 'u947'); assert.strictEqual(text.linkText, 'Open PR #14947');
text = stageTrackerResultText({ action: 'held', target: { repo: 'o/r', kind: 'issue', number: 7, url: 'u7' }, outcome: 'hold applied.', next: '' }, held, { id: 'hold' });
assert.strictEqual(text.head, 'Hold on o/r#7: held', 'the target is not repeated when it is the clicked row');
text = stageTrackerResultText({ action: 'noop-already-applied', target, outcome: 'No change — hive already recorded this decision at 12:01Z; still blocked by needs-human.', next: '' }, issue, { id: 'decide:delegate' });
assert.strictEqual(text.head, 'Let hive decide on o/r#14938 → PR #14947: no change'); assert.strictEqual(text.type, 'info');
assert.ok(text.outcome.startsWith('No change — hive already recorded this decision at'));
// hivecommons/hive#14822: a recorded decision that never took effect is a
// warning that says why in plain words and carries the next actions.
const stuck = { action: 'no-effect', rule: 'contributor-offline', why: 'self-decision-read-as-changes', target, applied_at: '2026-10-28T13:01:29Z',
  outcome: "Close this PR and open a fresh hive-authored PR with the same commits at 2026-10-28T13:01:29Z, but it hasn't taken effect: hive's own decision comment was read as a reviewer asking for changes.",
  next: 'Options: Merge as maintainer · Re-run decision · Close as superseded · Send back for a new commit.',
  options: [{ action: 'decide:merge-now', label: 'Merge as maintainer', enabled: true }, { action: 'decide:delegate:rerun', label: 'Re-run decision', enabled: true }, { action: 'decide:close-superseded', label: 'Close as superseded', enabled: false, reason_if_disabled: 'write required' }, { action: 'decide:direction', label: 'Send back for a new commit', enabled: true }, { label: 'no action' }] };
const stuckNow = Date.parse('2026-10-28T15:01:29Z');
text = stageTrackerResultText(stuck, issue, { id: 'decide:delegate', nowMs: stuckNow });
assert.strictEqual(text.head, "Hive decided on o/r#14938 → PR #14947: no effect yet (the author hasn't been reachable since the nudge)", 'the headline does not say "decided" twice');
assert.strictEqual(text.type, 'warning'); assert.strictEqual(text.options.length, 4, 'options without an action are dropped');
// The raw RFC3339 stamp never reaches the operator (#15126).
assert.ok(!/2026-10-28T13:01:29Z/.test(text.outcome) && / at 13:01( today)?, but/.test(text.outcome), text.outcome);
assert.ok(text.why.startsWith('The merge queue read the hive'), text.why);
assert.ok(!/contributor-offline|self-decision/.test(text.head + text.outcome + text.why), 'no bare slug reaches the operator');
// hivecommons/hive#15021: an ISSUE's approve is never "approved for merge",
// and a stuck 045 park label is named with the lift as the first option.
const issueTarget = { repo: 'o/r', kind: 'issue', number: 15021, url: 'u15021' };
const stuckIssue = { action: 'no-effect', rule: 'default-approve', why: 'park-label-stuck', target: issueTarget, applied_at: '2026-10-28T13:01:29Z',
  outcome: "Approved to proceed — an agent may claim it at 2026-10-28T13:01:29Z, but it hasn't taken effect: stage/045-needs-decision is still on the issue; the scanner skips it until that label clears.",
  next: 'Expected: the issue leaves Needs decision and an agent picks it up. Options: Lift the park (remove stage/045-needs-decision) · Re-evaluate now · Re-run decision · Give direction.',
  options: [{ action: 'decide:approve', label: 'Lift the park (remove stage/045-needs-decision)', enabled: true }, { action: 'retry', label: 'Re-evaluate now', enabled: true }, { action: 'decide:delegate:rerun', label: 'Re-run decision', enabled: true }, { action: 'decide:direction', label: 'Give direction', enabled: true }] };
text = stageTrackerResultText(stuckIssue, Object.assign({}, issue, { kind: 'issue', number: 15021 }), { id: 'decide:delegate', rowRef: 'o/r#15021', nowMs: stuckNow });
assert.strictEqual(text.head, 'Hive decided on o/r#15021: no effect yet (no special rule applies; the hive took its own recommendation)');
assert.ok(text.outcome.indexOf('approved for merge') < 0, text.outcome);
assert.ok(text.why.startsWith('The hive approved the issue and applied approved-direction'), text.why);
assert.strictEqual(text.options[0].action, 'decide:approve', 'the lift is the first option');
assert.strictEqual(stageTrackerPlainWords('approve', false, 'issue'), 'approved to proceed — an agent may claim it');
assert.strictEqual(stageTrackerPlainWords('approve', false, 'pr'), 'approved for merge');
assert.strictEqual(stageTrackerPlainWords('approve'), 'approved for merge', 'no kind reads as a PR');
assert.strictEqual(stageTrackerPlainWords('close-superseded', true, 'issue'), 'Closed because another issue already tracks the same work; the discussion continues there.');
assert.strictEqual(stageTrackerPlainWords('close-duplicate', false, 'issue'), 'closed as a duplicate', 'an entry without issue words keeps its plain words');
assert.strictEqual(stageTrackerPlainWords('supersede'), 'close this PR and open a fresh hive-authored PR with the same commits');
assert.strictEqual(stageTrackerPlainWords('adoption-stalled:contributor-offline'), "the author hasn't been reachable since the nudge");
assert.strictEqual(stageTrackerPlainWords('contributor-lane:changes-requested'), stageTrackerPlainWords('changes_requested'));
assert.strictEqual(stageTrackerPlainWords('some-new-rule'), 'some new rule', 'unknown slugs are humanised, never raw');
assert.strictEqual(stageTrackerPlainWords(''), '');
assert.strictEqual(stageTrackerResultOf('decide:delegate', issue, { decision: { verdict: 'supersede', rule: 'contributor-offline' } }).outcome, "close this PR and open a fresh hive-authored PR with the same commits (the author hasn't been reachable since the nudge)");
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
// An informational result closes itself after the info toast duration
// (15 s unless the hive's settings say otherwise — hivecommons/hive#15813)
// through the sticky guard; × and a state change still close it earlier.
assert.strictEqual(stageTrackerToastMs('info'), 15000);
assert.strictEqual(tst.el._countdown.ms, 15000, 'the countdown runs for the result TTL');
{
  const timed = stageTrackerResultToast(decided, issue, { id: 'decide:delegate' });
  assert.strictEqual(hamToastContainer.children.length, 2);
  timed.el._countdown.onExpire();
  assert.strictEqual(hamToastContainer.children.length, 1, 'expiry removes the toast');
  assert.ok(dismissed.pop().force, 'expiry dismisses through the sticky guard');
  _stageTrackerResultToasts = _stageTrackerResultToasts.filter(e => e !== timed);
}
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
// The stuck toast renders its options as buttons; disabled ones say why;
// "Re-run decision" runs the row action on the toast's target with force.
{
  const ran = [];
  const origRun = runRowAction;
  runRowAction = async (action, rows) => { ran.push({ action, rows }); return { toasts: [], pending: [] }; };
  const st = stageTrackerResultToast(stuck, issue, { id: 'decide:delegate' });
  assert.ok(st.el.className.includes('toast warning ham-result-toast'), st.el.className);
  assert.strictEqual(st.el._countdown, undefined, 'a toast offering next actions is a prompt: no countdown');
  const opts = st.el.children.find(c => c.className === 'ham-result-options');
  assert.ok(opts, 'options row rendered'); assert.strictEqual(opts.children.length, 4);
  assert.deepStrictEqual(opts.children.map(b => b.textContent), ['Merge as maintainer', 'Re-run decision', 'Close as superseded', 'Send back for a new commit']);
  assert.strictEqual(opts.children[2].getAttribute('aria-disabled'), 'true'); assert.strictEqual(opts.children[2].title, 'write required');
  opts.children[2]._listeners.click({ stopPropagation() {} });
  assert.strictEqual(ran.length, 0, 'a disabled option does nothing');
  opts.children[1]._listeners.click({ stopPropagation() {} });
  assert.strictEqual(ran.length, 1); assert.strictEqual(ran[0].action, 'decide:delegate:rerun');
  assert.strictEqual(ran[0].rows[0].number, 14947, 'acts on the toast target (the PR), not the clicked issue row');
  assert.strictEqual(st.el.parentNode, null, 'the toast leaves once an option is taken');
  runRowAction = origRun;
  _stageTrackerResultToasts = [];
}
const prRow = { repo: 'o/r', kind: 'pr', number: 14947, url: 'u947', reason: 'contributor-lane:changes-requested' };
const rerunReq = stageTrackerActionRequest(prRow, 'decide:delegate:rerun', {});
assert.strictEqual(rerunReq.body.force, true); assert.strictEqual(rerunReq.body.reason, 'contributor-lane:changes-requested'); assert.ok(rerunReq.poll); assert.ok(rerunReq.url.endsWith('/items/14947/decide-delegated'));
assert.strictEqual(stageTrackerActionRequest(prRow, 'decide:delegate', {}).body.force, undefined, 'a first click never forces');
assert.strictEqual(stageTrackerActionLabel('decide:delegate:rerun'), 'Re-run decision');
// Error toasts are sticky (×) but not tied to a row state; they close
// after the error toast duration, 30 s by default, and both durations
// follow the tracker payload's settings block live.
const bad = stageTrackerResultToast(stageTrackerErrorResult('hold', held, 'boom'), held, { id: 'hold' });
assert.ok(bad.el.className.includes('error')); assert.deepStrictEqual(_stageTrackerResultToasts, []);
assert.strictEqual(bad.el._countdown.ms, 30000, 'an error closes after the error toast duration');
bad.el.remove();
_stageTracker = { items: [issue, held], settings: { toast_info_s: 7, toast_error_s: 45 } };
{
  const tuned = stageTrackerResultToast(decided, issue, { id: 'decide:delegate' });
  assert.strictEqual(tuned.el._countdown.ms, 7000, 'info duration from the hive');
  tuned.el.remove(); _stageTrackerResultToasts = _stageTrackerResultToasts.filter(e => e !== tuned);
  const tunedErr = stageTrackerResultToast(stageTrackerErrorResult('hold', held, 'boom'), held, { id: 'hold' });
  assert.strictEqual(tunedErr.el._countdown.ms, 45000, 'error duration from the hive');
  tunedErr.el.remove();
  assert.strictEqual(stageTrackerToastSeconds('info', { toast_info_s: 500 }, null), 120, 'server values clamp to the 3..120 s range');
  assert.strictEqual(stageTrackerToastSeconds('info', { toast_info_s: 7 }, { info: 20, fallback: true }), 20, 'the browser fallback (write refused) wins over the payload');
  assert.strictEqual(stageTrackerToastSeconds('error', { toast_error_s: 'x' }, { info: 20 }), 30, 'garbage falls back to the default');
}
_stageTracker = { items: [issue, held] };

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
  assert.strictEqual(del.toast.text.outcome, "Hive decided on o/r#14947: adopted (the author hasn't been reachable since the nudge)", del.toast.text.outcome);
  assert.deepStrictEqual(del.pending[0].keys, ['o/r/pr#14947', 'o/r/issue#14938']);
  assert.ok(acting('o/r/issue#14938') && acting('o/r/pr#14947'));
  // An approval is recorded when the server answers: "Approving…" clears
  // at once instead of lingering beside the ✓ (hivecommons/hive#15103).
  stageTrackerClearPending(del.pending[0]);
  global.fetch = async () => ({ ok: true, status: 200, json: async () => ({ result: { action: 'approved', target: { repo: 'o/r', kind: 'pr', number: 14947, url: 'u947' }, outcome: 'Approved — #14947 conflicts with v5; hive will rebase it next pass (attempt 1/2).', next: 'n', options: [{ action: 'decide:merge-now', label: 'Rebase, then merge as maintainer', enabled: true }] } }) });
  stageTrackerSetConfirmSkip('decide:approve', true);
  const ap = await runRowAction('decide:approve', Object.assign({ repo: 'o/r', kind: 'pr' }, nested));
  assert.ok(!acting('o/r/pr#14947'), 'approved: the acting caption clears once the decision records');
  assert.strictEqual(ap.toast.text.options.length, 1, 'the approve toast carries the rebase-then-merge button');
})().catch(e => { console.error(e); process.exit(1); });
`)
}

// TestStageTrackerRowActionsLeadWithDecisionOptions: a row the hive parked
// for a person carries the matching actions at the top of its ⋯ menu, each
// with the server's consequence line, on the issue row (one nested asking
// PR) and on the PR sub-row alike (hivecommons/hive#15078: hotshot#317 at
// 090 "Ready for human merge · untrusted author" offered only Hold /
// Re-evaluate / Mark needs-human / Open / Copy). The merge actions need the
// merger role; a server-disabled option keeps its plain-words reason; the
// options the Decide… submenu already offers are not repeated.
func TestStageTrackerRowActionsLeadWithDecisionOptions(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const ids = as => as.map(a => a.id);
const merger = { user: true, canHold: true, canHuman: true };
const writer = { user: true, canHold: true, canHuman: false };
const decision090 = { question: 'This PR is ready to merge, but the hive will not merge it by itself (untrusted author): @Karibhou is not a collaborator…', blocker: 'The hive never merges this unattended.', asked_by: 'the merge lane', since: '2026-10-28T12:00:00Z',
  options: [
    { action: 'decide:queue-merge', label: 'Approve and let hive merge', enabled: true, consequence: 'records your approval; the hive\'s merge sweep merges #317 on its next pass once CI is green and the head has soaked' },
    { action: 'decide:merge-now', label: 'Merge now as maintainer', enabled: true, consequence: 'merges #317 into main now via hive\'s identity, attributed to you' },
    { action: 'decide:delegate', label: 'Let hive decide', enabled: true, consequence: 'the hive answers at once why it will not merge this itself and lists these options; it never merges it' },
    { action: 'decide:direction', label: 'Give direction', enabled: false, reason_if_disabled: 'The hive did not open this PR, so it cannot carry a direction to its author; leave a changes-requested review on GitHub' },
    { action: 'decide:close-superseded', label: 'Close as superseded', enabled: true, consequence: 'closes #317 unmerged, naming the PR that replaces it' },
    { action: 'decide:close-wontfix', label: "Close as won't fix", enabled: true, consequence: 'closes #317 unmerged' },
    { action: 'hold', label: 'Hold', enabled: true },
  ] };
const pr317 = { repo: 'hivecommons/hotshot', kind: 'pr', number: 317, title: 'docs: prepare v0.2.0 release notes', url: 'https://github.com/hivecommons/hotshot/pull/317', author: 'Karibhou', author_kind: 'human', base_ref: 'main',
  stage: 90, modifiers: [], human_merge_reason: 'untrusted-author', decision_needed: decision090 };
const issue316 = { repo: 'hivecommons/hotshot', kind: 'issue', number: 316, title: 'Release v0.2.0', url: 'https://github.com/hivecommons/hotshot/issues/316', stage: 90, mirrored: true, modifiers: [],
  linked_prs: [{ number: 317, title: pr317.title, url: pr317.url, author: 'Karibhou', author_kind: 'human', base_ref: 'main', stage: 90, modifiers: [], human_merge_reason: 'untrusted-author', decision_needed: decision090 }] };

// The PR sub-row: the two merge answers lead, then Decide… with the rest,
// then the generic items.
let as = stageTrackerRowActions(pr317, merger);
assert.deepStrictEqual(ids(as), ['decide:queue-merge', 'decide:merge-now', 'decide', 'hold', 'retry', 'needs-human', 'open', 'copy']);
assert.strictEqual(as[0].label, 'Approve and let hive merge');
assert.strictEqual(as[1].label, 'Merge now as maintainer');
assert.strictEqual(as[1].hint, 'merges #317 into main now via hive\'s identity, attributed to you', 'the consequence rides as the hint');
assert.ok(as[0].enabled && as[1].enabled, 'a merger may merge or queue');
assert.ok(as[0].lead && as[1].lead);
assert.strictEqual(as[2].sep, true, 'the Decide… group is separated from the lead answers');
assert.deepStrictEqual(ids(as[2].submenu), ['decide:delegate', 'decide:approve', 'decide:close-superseded', 'decide:close-wontfix', 'decide:direction'], 'options the submenu already offers are not repeated at the top');
// The menu markup lists the lead answers above everything else, with the
// consequence under the label.
const menu = stageTrackerMenuHtml(as, 'hivecommons/hotshot#317');
const at = s => menu.indexOf(s);
assert.ok(at('data-ham-act="decide:queue-merge"') > 0 && at('data-ham-act="decide:queue-merge"') < at('data-ham-act="decide:merge-now"'));
assert.ok(at('data-ham-act="decide:merge-now"') < at('data-ham-act="decide"'), 'merge answers above Decide…');
assert.ok(at('data-ham-act="decide"') < at('data-ham-act="hold"') && at('data-ham-act="hold"') < at('data-ham-act="open"'), 'generic items last');
assert.ok(menu.includes('<span class="ham-menu-hint">merges #317 into main now via hive&#39;s identity, attributed to you</span>') || menu.includes('<span class="ham-menu-hint">merges #317 into main now via hive\'s identity, attributed to you</span>'), 'consequence line rendered: ' + menu);
// Hints stack under their label and the menu grows for them (#15175).
assert.ok(menu.startsWith('<div class="ham-menu ham-menu-hinted"'), 'a menu with hints is ham-menu-hinted: ' + menu.slice(0, 80));
assert.ok(/class="ham-menu-item ham-menu-item-hinted" data-ham-act="decide:merge-now"/.test(menu), 'the hinted item stacks label over hint: ' + menu);
assert.ok(!/class="ham-menu-item[^"]*ham-menu-item-hinted"[^>]*data-ham-act="open"/.test(menu), 'items without a hint keep the inline layout');
assert.ok(!stageTrackerMenuHtml([{ id: 'open', label: 'Open', enabled: true }], 'x').includes('ham-menu-hinted'), 'no hints: plain menu width');
assert.ok(stageTrackerMenuHasHints([{ id: 'decide', label: 'Decide…', enabled: true, submenu: [{ id: 'x', label: 'x', hint: 'per PR' }] }]), 'a submenu hint counts');

// A writer without the merger role sees the merge answers, greyed with the
// role reason — never silently missing.
as = stageTrackerRowActions(pr317, writer);
assert.deepStrictEqual(ids(as).slice(0, 2), ['decide:queue-merge', 'decide:merge-now']);
assert.strictEqual(as[0].enabled, false); assert.strictEqual(as[0].why, STAGE_TRACKER_WHY_ROLE);
assert.strictEqual(as[1].enabled, false); assert.strictEqual(as[1].why, STAGE_TRACKER_WHY_ROLE);

// The issue row over its one asking PR: the same lead answers, acting on
// PR #317 (runRowAction resolves the decision target to the PR).
as = stageTrackerRowActions(issue316, merger);
assert.deepStrictEqual(ids(as), ['decide:queue-merge', 'decide:merge-now', 'decide', 'hold', 'retry', 'needs-human', 'open', 'copy']);
assert.strictEqual(as[0].target.number, 317); assert.strictEqual(as[0].target.kind, 'pr');
assert.strictEqual(as[2].label, 'Decide PR #317…');
assert.strictEqual(stageTrackerDecisionTarget(issue316).number, 317, 'a human-merge reason parks the PR for the issue row');
const req = stageTrackerActionRequest(stageTrackerDecisionTarget(issue316), 'decide:merge-now', {});
assert.strictEqual(req.url, '/api/repos/hivecommons/hotshot/items/317/decision');
assert.deepStrictEqual(req.body, { decision: 'merge-now', comment: '', type: 'pr' });
assert.deepStrictEqual(stageTrackerActionRequest(pr317, 'decide:queue-merge', {}).body, { decision: 'queue-merge', comment: '', type: 'pr' });
assert.strictEqual(STAGE_TRACKER_ACTING_CAPTIONS['decide:merge-now'], 'Merging…');
assert.strictEqual(STAGE_TRACKER_ACTION_WORDS.merged, 'merged');
assert.strictEqual(STAGE_TRACKER_ACTION_WORDS['queued-for-merge'], 'approved — hive will merge on the next tick once CI is green');
assert.strictEqual(STAGE_TRACKER_ACTING_CAPTIONS['decide:queue-merge'], 'Recording approval…');
assert.strictEqual(STAGE_TRACKER_ACTION_LABELS['decide:queue-merge'], 'Approve and let hive merge');
assert.strictEqual(STAGE_TRACKER_ACTION_LABELS['decide:merge-now'], 'Merge now as maintainer');
assert.deepStrictEqual(STAGE_TRACKER_MERGE_ACTIONS, ['decide:queue-merge', 'decide:merge-now'], 'queue-merge is the lead merge answer');

// A human-authored PR waiting on its author (072): no Decide… submenu. The
// server's close answers stay in the ⋯ menu with their consequences, but a
// destructive option never leads the row on its own — the row shows no
// inline button rather than "Close as superseded".
const waiting = { repo: 'o/r', kind: 'pr', number: 9, url: 'u', author: 'someone', author_kind: 'human', stage: 72, modifiers: [],
  decision_needed: { question: 'This PR waits on @someone to push a new commit.', options: [
    { action: 'decide:close-superseded', label: 'Close as superseded', enabled: true, consequence: 'closes #9 unmerged, naming the PR that replaces it' },
    { action: 'decide:close-wontfix', label: "Close as won't fix", enabled: true, consequence: 'closes #9 unmerged' },
    { action: 'hold', label: 'Hold', enabled: true } ] } };
as = stageTrackerRowActions(waiting, writer);
assert.deepStrictEqual(ids(as), ['decide:close-superseded', 'decide:close-wontfix', 'hold', 'retry', 'needs-human', 'open', 'copy']);
assert.ok(as[0].danger && as[1].danger);
assert.ok(!as[0].lead && !as[1].lead, 'a close is never the inline lead by default');
assert.strictEqual(as[0].hint, 'closes #9 unmerged, naming the PR that replaces it');
assert.strictEqual(stageTrackerLeadAction(waiting, writer), null, 'no inline button for a PR waiting on its author');
assert.strictEqual(stageTrackerLeadActionHtml(waiting, writer), '');

// The live shape (hivecommons/hive#15626, #15641): a human's PR parked at
// 085 after the review swarm sent it back — delegate and hold sit in the
// Decide… submenu, so the closes were the first options left over and
// "Close as superseded" became the red lead. Now the lead is the
// non-destructive submenu answer.
const sentBack = { repo: 'o/r', kind: 'pr', number: 15626, url: 'u', author: 'clubanderson', author_kind: 'human', stage: 85, modifiers: ['needs-decision'],
  rework: { state: 'acknowledged', actor: 'clubanderson' },
  decision_needed: { question: 'This PR waits on @clubanderson to push a new commit.', options: [
    { action: 'decide:delegate', label: 'Let hive decide', enabled: true },
    { action: 'decide:close-superseded', label: 'Close as superseded', enabled: true, consequence: 'Closes #15626, naming the PR that replaces it' },
    { action: 'decide:close-wontfix', label: "Close as won't fix", enabled: true, consequence: 'Closes #15626 without merging' },
    { action: 'hold', label: 'Hold', enabled: true } ] } };
let leadSB = stageTrackerLeadAction(sentBack, writer);
assert.ok(leadSB && !/^decide:close-/.test(leadSB.id), 'sent-back PR never leads with a close: ' + JSON.stringify(leadSB));
assert.ok(!leadSB.danger);
const sbDecide = stageTrackerRowActions(sentBack, writer).find(a => a.id === 'decide');
assert.ok(sbDecide && sbDecide.submenu.some(o => o.id === 'decide:close-superseded' && o.danger), 'the Decide… submenu still offers the close');

// When the server says the close IS the answer (lead: the reviewer named
// the PR that supersedes this one — RedundantOf), Close as superseded
// still leads.
const redundant = { ...sentBack, number: 15499, decision_needed: { question: 'q', redundant_of: 15400, options: [
    { action: 'decide:close-superseded', label: 'Close as superseded', enabled: true, lead: true, consequence: 'Closes this PR as superseded by #15400, which already carries its change.' },
    { action: 'decide:delegate', label: 'Let hive decide', enabled: true },
    { action: 'decide:close-wontfix', label: "Close as won't fix", enabled: true },
    { action: 'hold', label: 'Hold', enabled: true } ] } };
leadSB = stageTrackerLeadAction(redundant, writer);
assert.strictEqual(leadSB.id, 'decide:close-superseded');
assert.ok(leadSB.danger && leadSB.lead);
assert.strictEqual(leadSB.hint, 'Closes this PR as superseded by #15400, which already carries its change.');

// A server-disabled lead keeps the server's plain-words reason.
const refused = { ...pr317, decision_needed: { ...decision090, options: [{ action: 'decide:queue-merge', label: 'Approve and let hive merge', enabled: false, reason_if_disabled: 'Release the hold first' }] } };
as = stageTrackerRowActions(refused, merger);
assert.strictEqual(as[0].id, 'decide:queue-merge'); assert.strictEqual(as[0].enabled, false); assert.strictEqual(as[0].why, 'Release the hold first');

// Generic options the server names (hold / needs-human / retry) are never
// doubled; a 085 PR's menu is unchanged.
const pr85 = { repo: 'o/r', kind: 'pr', number: 4, url: 'u', stage: 85, modifiers: ['needs-decision'],
  decision_needed: { question: 'q', options: [{ action: 'decide:delegate', label: 'Let hive decide', enabled: true }, { action: 'decide:approve', label: 'Approve', enabled: true }, { action: 'hold', label: 'Hold', enabled: true }, { action: 'needs-human', label: 'Mark needs-human', enabled: true }] } };
assert.deepStrictEqual(ids(stageTrackerRowActions(pr85, merger)), ['decide', 'hold', 'retry', 'needs-human', 'open', 'copy']);
`)
}

// The live case (hivecommons/hive#15078, operator screenshot 2026-10-28):
// hotshot#316 with its one nested PR #317 at 090. The ISSUE row's ⋯ menu
// must carry all three — Approve and let hive merge, Merge now as
// maintainer, Let hive decide — and the popover the same, merge answers first.
func TestStageTrackerIssueRowWithNested090PRCarriesAllThree(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerDecisionPopPreamble(t, html)+`
const ids = as => as.map(a => a.id);
const merger = { user: true, canHold: true, canHuman: true };
const decision090 = { question: 'This PR is ready to merge, but the hive will not merge it by itself (untrusted author).', blocker: 'The hive never merges this unattended.', asked_by: 'the merge lane', since: '2026-10-28T12:00:00Z',
  options: [
    { action: 'decide:queue-merge', label: 'Approve and let hive merge', enabled: true, consequence: 'records your approval; the hive\'s merge sweep merges #317 on its next pass' },
    { action: 'decide:merge-now', label: 'Merge now as maintainer', enabled: true, consequence: 'merges #317 into main now via hive\'s identity, attributed to you' },
    { action: 'decide:delegate', label: 'Let hive decide', enabled: true, consequence: 'the hive answers at once why it will not merge this itself and lists these options; it never merges it' },
    { action: 'decide:close-superseded', label: 'Close as superseded', enabled: true, consequence: 'closes #317 unmerged, naming the PR that replaces it' },
    { action: 'decide:close-wontfix', label: "Close as won't fix", enabled: true, consequence: 'closes #317 unmerged' },
    { action: 'hold', label: 'Hold', enabled: true },
  ] };
const lp317 = { number: 317, title: 'docs: prepare v0.2.0 release notes', url: 'https://github.com/hivecommons/hotshot/pull/317', author: 'Karibhou', author_kind: 'human', base_ref: 'main', stage: 90, modifiers: [], human_merge_reason: 'untrusted-author', decision_needed: decision090 };
const issue316 = { repo: 'hivecommons/hotshot', kind: 'issue', number: 316, title: 'Release v0.2.0', url: 'https://github.com/hivecommons/hotshot/issues/316', stage: 90, mirrored: true, modifiers: [], linked_prs: [lp317] };

const as = stageTrackerRowActions(issue316, merger);
const flat = stageTrackerFlatActions(as);
assert.deepStrictEqual(ids(as).slice(0, 3), ['decide:queue-merge', 'decide:merge-now', 'decide'], 'issue row leads with the two merge answers, let-hive-merge first: ' + ids(as));
assert.ok(flat['decide:queue-merge'] && flat['decide:queue-merge'].enabled, 'Approve and let hive merge present and enabled');
assert.ok(flat['decide:merge-now'] && flat['decide:merge-now'].enabled, 'Merge now as maintainer present and enabled');
assert.ok(flat['decide:delegate'] && flat['decide:delegate'].enabled, 'Let hive decide present (in Decide PR #317…) and enabled');
assert.strictEqual(as[2].label, 'Decide PR #317…');
assert.strictEqual(as[2].submenu[0].id, 'decide:delegate', 'Let hive decide leads the Decide… group');
for (const id of ['decide:merge-now', 'decide:queue-merge']) assert.strictEqual(flat[id].target.number, 317, id + ' acts on PR #317');
const menu = stageTrackerMenuHtml(as, 'hivecommons/hotshot#316');
for (const id of ['decide:merge-now', 'decide:queue-merge', 'decide:delegate']) assert.ok(menu.includes('data-ham-act="' + id + '"'), id + ' rendered in the issue-row menu');
assert.ok(menu.indexOf('data-ham-act="decide:merge-now"') < menu.indexOf('data-ham-act="hold"'), 'merge answers above the generic items');

// The needs-human popover on the issue row resolves to PR #317 and lists
// the same three, merge answers first, Let hive decide right after.
const dec = stageTrackerDecisionOf(issue316);
assert.strictEqual(dec.item.number, 317);
const pop = stageTrackerDecisionOptions(dec.item, merger);
assert.deepStrictEqual(ids(pop).slice(0, 3), ['decide:queue-merge', 'decide:merge-now', 'decide:delegate'], 'popover order: ' + ids(pop));
assert.ok(pop[0].enabled && pop[1].enabled && pop[2].enabled);
const html = stageTrackerDecisionPopHtml(dec.item, pop, Date.now(), '');
// The popover shows the one recommended action (approve and let hive merge) and hands the rest to the row menu.
assert.ok(html.includes('data-ham-act="decide:queue-merge"') && html.includes('>Let hive merge<') && html.includes('data-ham-more="1"'), html);
assert.ok(!html.includes('data-ham-act="decide:merge-now"') && !html.includes('data-ham-act="decide:delegate"'), html);

// A 085 popover still leads with Let hive decide.
const pr85 = { repo: 'o/r', kind: 'pr', number: 4, url: 'u', stage: 85, modifiers: ['needs-decision'],
  decision_needed: { question: 'q', options: [{ action: 'decide:approve', label: 'Approve', enabled: true }, { action: 'decide:delegate', label: 'Let hive decide', enabled: true }] } };
assert.strictEqual(stageTrackerDecisionOptions(pr85, merger)[0].id, 'decide:delegate');
`)
}

// hivecommons/hive#15136: an owner's Approve is the trust override for the
// head, and the row must say so BEFORE acting. A 090 PR whose stage record
// counted unsigned commits gets the DCO caveat in both Approve confirms
// and a separate "Re-author as hive PR" lead action with its own confirm;
// the request, caption and ledger id are wired; a signed head says nothing
// about DCO.
func TestStageTrackerReauthorAndApprovalCaveats(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+jsConstLine(t, html, "STAGE_TRACKER_ACTED_OPTION_ID")+`
const ids = as => as.map(a => a.id);
const merger = { user: true, canHold: true, canHuman: true };
const options = [
  { action: 'decide:queue-merge', label: 'Approve and let hive merge', enabled: true, consequence: 'records your approval as the trust override for this head; the hive\'s merge sweep merges #317 as-is on its next pass once CI is green and the head has soaked, and does not adopt it' },
  { action: 'decide:merge-now', label: 'Merge now as maintainer', enabled: true, consequence: 'merges #317 into main now via hive\'s identity, attributed to you; 2 of 3 commits have no Signed-off-by, so GitHub refuses the merge where DCO is required' },
  { action: 'decide:re-author', label: 'Re-author as hive PR', enabled: true, consequence: 'the hive re-homes #317\'s commits on a hive-owned, signed-off branch and opens a superseding PR; #317 is closed when that one merges; nothing is merged as-is' },
  { action: 'decide:delegate', label: 'Let hive decide', enabled: true, consequence: 'the hive answers at once why it will not merge this itself and lists these options; it never merges it' },
  { action: 'decide:close-wontfix', label: "Close as won't fix", enabled: true, consequence: 'closes #317 unmerged' },
  { action: 'hold', label: 'Hold', enabled: true },
];
const pr317 = { repo: 'hivecommons/hotshot', kind: 'pr', number: 317, url: 'https://github.com/hivecommons/hotshot/pull/317', author: 'hive[bot]', author_kind: 'hive', base_ref: 'main',
  stage: 90, modifiers: [], human_merge_reason: 'untrusted-author', dco_unsigned: 2, dco_total: 3, decision_needed: { question: 'q', options } };
const issue316 = { repo: 'hivecommons/hotshot', kind: 'issue', number: 316, url: 'u', stage: 90, mirrored: true, modifiers: [],
  linked_prs: [{ number: 317, url: pr317.url, author: 'hive[bot]', author_kind: 'hive', base_ref: 'main', stage: 90, modifiers: [], human_merge_reason: 'untrusted-author', dco_unsigned: 2, dco_total: 3, decision_needed: { question: 'q', options } }] };

// The re-author choice leads right after the two Approve answers, on the
// PR sub-row and on the issue row over it.
let as = stageTrackerRowActions(pr317, merger);
assert.deepStrictEqual(ids(as).slice(0, 4), ['decide:queue-merge', 'decide:merge-now', 'decide:re-author', 'decide'], ids(as));
assert.strictEqual(as[2].label, 'Re-author as hive PR');
assert.ok(as[2].enabled && as[2].lead);
assert.ok(as[2].hint.includes('nothing is merged as-is'));
as = stageTrackerRowActions(issue316, merger);
assert.deepStrictEqual(ids(as).slice(0, 3), ['decide:queue-merge', 'decide:merge-now', 'decide:re-author'], ids(as));
assert.strictEqual(as[2].target.number, 317);
const menu = stageTrackerMenuHtml(as, 'hivecommons/hotshot#316');
assert.ok(menu.indexOf('data-ham-act="decide:re-author"') > 0 && menu.indexOf('data-ham-act="decide:re-author"') < menu.indexOf('data-ham-act="hold"'));
// Wiring: request body, acting caption, ledger id, label.
assert.deepStrictEqual(stageTrackerActionRequest(pr317, 'decide:re-author', {}).body, { decision: 're-author', comment: '', type: 'pr' });
assert.strictEqual(STAGE_TRACKER_ACTION_LABELS['decide:re-author'], 'Re-author as hive PR');
assert.strictEqual(STAGE_TRACKER_ACTING_CAPTIONS['decide:re-author'], 'Queuing re-authoring…');
assert.strictEqual(STAGE_TRACKER_ACTED_OPTION_ID['re-author'], 'decide:re-author');
// The DCO caveat, in plain words, only when the record counted unsigned commits.
assert.ok(stageTrackerDCOCaveat([pr317]).includes('2 of 3 commits on this head lack a Signed-off-by'), stageTrackerDCOCaveat([pr317]));
assert.ok(stageTrackerDCOCaveat([pr317]).includes('"Re-author as hive PR" is the separate choice'));
assert.strictEqual(stageTrackerDCOCaveat([{ ...pr317, dco_unsigned: 0 }]), '');
assert.strictEqual(stageTrackerDCOCaveat([]), '');
(async () => {
  // Both Approve confirms warn BEFORE acting and say what approval means.
  await stageTrackerActionPayload([pr317], 'decide:merge-now');
  assert.strictEqual(dialogs[0].cfg.title, 'Merge now as maintainer');
  assert.ok(dialogs[0].cfg.message.includes('lack a Signed-off-by'), dialogs[0].cfg.message);
  await stageTrackerActionPayload([pr317], 'decide:queue-merge');
  assert.strictEqual(dialogs[1].cfg.title, 'Approve and let hive merge');
  assert.ok(dialogs[1].cfg.message.includes("stands in for the author's trust on this version"), dialogs[1].cfg.message);
  assert.ok(dialogs[1].cfg.message.includes('will not adopt or re-author it'), dialogs[1].cfg.message);
  assert.ok(dialogs[1].cfg.message.includes('lack a Signed-off-by'), dialogs[1].cfg.message);
  // Re-author has its own confirm, danger-styled, with the don't-ask box.
  assert.deepStrictEqual(await stageTrackerActionPayload([pr317], 'decide:re-author'), {});
  assert.strictEqual(dialogs[2].cfg.title, 'Re-author as hive PR');
  assert.ok(dialogs[2].cfg.danger);
  assert.ok(dialogs[2].cfg.message.includes('superseding PR') && dialogs[2].cfg.message.includes('Nothing is merged as-is'), dialogs[2].cfg.message);
  assert.deepStrictEqual(dialogs[2].cfg.check, { label: "Don't ask again for this action" });
  dialogAnswer = false;
  assert.strictEqual(await stageTrackerActionPayload([pr317], 'decide:re-author'), null, 'cancel acts on nothing');
  // A signed head: no DCO sentence in the Approve confirm.
  dialogAnswer = true; dialogs.length = 0;
  await stageTrackerActionPayload([{ ...pr317, dco_unsigned: 0 }], 'decide:merge-now');
  assert.ok(!dialogs[0].cfg.message.includes('Signed-off-by'), dialogs[0].cfg.message);
})().catch(e => { console.error(e); process.exit(1); });
`)
}

// TestStageTrackerRebaseThenMergeRow: a PR that needs a rebase offers
// "Rebase, then merge as maintainer" in place of (090) or next to (085)
// the plain maintainer merge; the acting caption, the 202 rebase-queued
// toast, the cue-working glow while the follow-through runs, and the
// ledger trail's step lines all read as the operator was promised
// (hivecommons/hive#14822).
func TestStageTrackerRebaseThenMergeRow(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+"\n"+stageTrackerCuesJS(t, html)+jsConstBlock(t, html, "STAGE_TRACKER_ICONS")+"\n"+stageTrackerActedJS(t, html)+"\n"+jsFunc(t, html, "agentNowClockTime")+`
global.window = { _hiveUser: 'clubanderson' };
const ids = as => as.map(a => a.id);
const merger = { user: true, canHold: true, canHuman: true };
const rebaseOpt = { action: 'decide:merge-now', label: STAGE_TRACKER_REBASE_MERGE_LABEL, enabled: true,
  consequence: "hive rebases #14822 onto v5, waits for CI, then merges it with your approval on record; you'll get a toast when it lands or if the rebase conflicts" };
const pr = { repo: 'hivecommons/hive', kind: 'pr', number: 14822, title: 'fix: preserve owner intent through PR decision paths', url: 'u', author: 'thatguywob', author_kind: 'clanker', base_ref: 'v5',
  stage: 85, caption: 'Needs human decision', modifiers: ['needs-decision', 'needs-rebase'],
  decision_needed: { question: 'q', options: [
    { action: 'decide:delegate', label: 'Let hive decide', enabled: true },
    { action: 'decide:approve', label: 'Approve', enabled: true },
    rebaseOpt,
    { action: 'decide:close-superseded', label: 'Close as superseded', enabled: true },
    { action: 'hold', label: 'Hold', enabled: true } ] } };

assert.strictEqual(stageTrackerNeedsRebase(pr), true);
assert.strictEqual(stageTrackerNeedsRebase({ ...pr, modifiers: ['needs-decision'] }), false);
assert.strictEqual(stageTrackerNeedsRebase({ ...pr, modifiers: [], labels: ['Needs-Rebase'] }), true, 'the label counts too');
assert.strictEqual(stageTrackerNeedsRebase({ ...pr, modifiers: [], rebase_merge: { state: 'rebasing' } }), true, 'a live follow-through counts');
assert.strictEqual(stageTrackerNeedsRebase({ ...pr, modifiers: [], rebase_merge: { state: 'merged' } }), false);

// 085 menu: the relabelled merge leads; the 085 menu without needs-rebase is untouched.
let as = stageTrackerRowActions(pr, merger);
assert.strictEqual(as[0].id, 'decide:merge-now'); assert.strictEqual(as[0].label, STAGE_TRACKER_REBASE_MERGE_LABEL);
assert.ok(as[0].hint.startsWith('hive rebases #14822 onto v5'), 'consequence rides as the hint: ' + as[0].hint);
assert.ok(as[0].enabled && as[0].lead);
const menu = stageTrackerMenuHtml(as, 'hivecommons/hive#14822');
assert.ok(menu.includes('Rebase, then merge as maintainer'), menu);
assert.ok(menu.indexOf('data-ham-act="decide:merge-now"') < menu.indexOf('data-ham-act="decide"'), 'the rebase-merge leads the Decide… group');
const req = stageTrackerActionRequest(pr, 'decide:merge-now', {});
assert.strictEqual(req.url, '/api/repos/hivecommons/hive/items/14822/decision');
assert.deepStrictEqual(req.body, { decision: 'merge-now', comment: '', type: 'pr' }, 'the same decision wire: the server folds the rebase in');

// Acting caption and the action word for the 202.
assert.strictEqual(stageTrackerActingCaption('decide:merge-now', pr), 'Rebasing, then merging…');
assert.strictEqual(stageTrackerActingCaption('decide:merge-now', { ...pr, modifiers: [] }), 'Merging…');
assert.strictEqual(STAGE_TRACKER_ACTION_WORDS[STAGE_TRACKER_ACTION_REBASE_QUEUED], 'rebase queued, merge to follow');

// The toast for the 202 names the rebase-merge, not a plain merge.
const result = { action: STAGE_TRACKER_ACTION_REBASE_QUEUED, target: { repo: 'hivecommons/hive', kind: 'pr', number: 14822, url: 'u' },
  outcome: 'Your approval is on record; the hive is rebasing #14822 onto v5.', next: 'Once CI is green on the rebased head the hive merges it with your approval attributed; you\'ll get a toast when it lands or if the rebase conflicts.' };
const text = stageTrackerResultText(result, pr, { id: 'decide:merge-now' });
const flat = typeof text === 'string' ? text : JSON.stringify(text);
assert.ok(flat.includes('Rebase, then merge as maintainer on hivecommons/hive#14822'), flat);
assert.ok(flat.includes('rebasing #14822 onto v5'), flat);

// A refused follow-through (conflict) carries its two ways out.
const failed = { action: 'refused', why: 'rebase-conflict', target: result.target, outcome: 'Rebase conflicts in .github/workflows/v2-ci.yml.',
  options: [{ action: 'decide:adopt', label: 'Adopt into fresh branch', enabled: true }, { action: 'decide:close-superseded', label: 'Close as superseded', enabled: true }] };
const failedText = stageTrackerResultText(failed, pr, { id: 'decide:merge-now' });
const failedFlat = typeof failedText === 'string' ? failedText : JSON.stringify(failedText);
assert.ok(failedFlat.includes('v2-ci.yml'), failedFlat);

// The row while it runs: cue-working, caption from the server, state
// signature changes with the follow-through state.
const running = { ...pr, caption: 'Rebasing → merge queued (approved by @clubanderson)', rebase_merge: { state: 'rebasing', actor: 'clubanderson', base: 'v5', text: 'Rebasing → merge queued (approved by @clubanderson)' } };
assert.strictEqual(stageTrackerRebaseMergeLive(running), true);
assert.strictEqual(stageTrackerRebaseMergeLive(pr), false);
assert.ok(stageTrackerCueClasses(running, { workers: {} }).includes('cue-working'), 'the working cue while the hive rebases');
assert.ok(!stageTrackerCueClasses(pr, { workers: {} }).includes('cue-working'));
assert.notStrictEqual(stageTrackerItemState(running), stageTrackerItemState({ ...running, rebase_merge: { state: 'ci-pending' } }), 'the acting row sees the state move');

// The ledger trail: step hops without a stage number read as plain steps.
const now = Date.parse('2026-10-29T10:00:00Z');
const at = m => new Date(now - m * 60000).toISOString();
const acted = { ...running, last_operator_action: { action: 'merge-now', actor: 'clubanderson', at: at(5), outcome: 'pending', detail: 'rebase-queued', stage_at: 85,
  followed: [{ number: 0, slug: 'approval-recorded', at: at(5) }, { number: 0, slug: 'rebase-kicked', at: at(4), reason: 'fixer' }] } };
const tip = stageTrackerActedTip(acted, now);
assert.ok(tip.includes('rebasing, then merging…'), tip);
assert.ok(tip.includes('Then the hive recorded your approval'), tip);
assert.ok(tip.includes('Then the hive started the rebase — fixer'), tip);

// Approve on a dirty PR (hivecommons/hive#15103): the toast tells the
// truth (the gate skips a conflicting PR), carries the one-click upgrade
// to rebase-then-merge, and the row's state signature moves with the
// fixer's attempt so "Approving…" does not wait on the gate.
const approvedDirty = { action: 'approved', target: result.target,
  outcome: 'Approved — #14822 conflicts with v5; hive will rebase it next pass (attempt 1/2).',
  next: 'The merge queue skips a conflicting PR, so your approval alone would not land it. Once the rebased head is green the gate takes it — or have hive merge it for you right after the rebase:',
  options: [{ action: 'decide:merge-now', label: STAGE_TRACKER_REBASE_MERGE_LABEL, enabled: true, consequence: 'hive rebases #14822 onto v5, waits for CI, then merges it with your approval on record' }] };
const apText = stageTrackerResultText(approvedDirty, pr, { id: 'decide:approve' });
assert.strictEqual(apText.type, 'success');
assert.ok(apText.outcome.includes('hive will rebase it next pass (attempt 1/2)'), apText.outcome);
assert.strictEqual(apText.options.length, 1, 'the approve toast offers the rebase-then-merge button');
assert.strictEqual(apText.options[0].label, 'Rebase, then merge as maintainer');
const plainApproved = stageTrackerResultText({ action: 'approved', target: result.target, outcome: '/lgtm + /approve posted.', next: 'The merge queue takes it from here; the dashboard merges nothing.' }, pr, { id: 'decide:approve' });
assert.strictEqual(plainApproved.options.length, 0, 'a plain approval has no buttons');
assert.notStrictEqual(stageTrackerItemState(pr), stageTrackerItemState({ ...pr, rebase: { attempt: 1, max: 2, text: 'Rebasing · attempt 1/2' } }), 'the fixer attempt moves the row state');
`)
}

// TestStageTrackerDelegatePlanDialogIsOneDecision (operator feedback on
// hivecommons/hive#15332's "Let hive decide — preview": "yikes - this is a
// mess … this needs to be exceedingly simple to understand and act on").
// The renderer takes one view model for PR and issue decisions alike:
// title + one subject line, ONE verb-first sentence (no doubled "Hive
// would:"), ONE short why, at most two "then" items; the sentinel
// finding, the rails and the ruled-out rungs sit under a collapsed
// Details. The visible text for the #15332 case is ≤ 60 words and free of
// hive vocabulary; the merge button — "Approve and let hive merge", or
// "Merge now as maintainer" when the hive would approve anyway — appears
// only when the plan says the head is mergeable as it is.
func TestStageTrackerDelegatePlanDialogIsOneDecision(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`.ham-plan-sentence {`, `.ham-plan-details {`, `.ham-plan-chip {`, `.ham-plan-eyebrow, .ham-plan-label {`,
		`stageTrackerDelegatePlanHtml(view)`, `<details class="ham-plan-details"><summary>Details</summary>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing preview wiring %q", want)
		}
	}
	if strings.Contains(html, "Let hive decide — preview") {
		t.Error("the preview title still carries the “— preview” suffix")
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const JARGON = ['hive would:', 'park', 'audit marker', 'stage', 'ladder', '/hive decision', 'recommendation'];
const words = text => text.split(/\s+/).filter(Boolean).length;
const buttons = html => (html.match(/<button[^>]*>([^<]*)<\/button>/g) || []).map(b => b.replace(/<[^>]+>/g, '').replace(/&amp;/g, '&'));
const visibleOf = html => stageTrackerDelegatePlanVisibleText(html);
const detailsOf = html => (html.match(/<details[\s\S]*?<\/details>/) || [''])[0];

// 1. hivecommons/hive#15332 as the operator saw it: a direction on a red
//    head, sentinel-flagged, head not mergeable.
const pr15332 = { ref: 'hivecommons/hive#15332', number: 15332, type: 'pr', title: "ci: run crux's Go file-size ratchet against src/crux on hive PRs", would: 'direction', rule: 'fix-then-merge',
  summary: 'Send #15332 back to its author with one instruction: make CI green, nothing else.', why: 'CI is red.',
  consequences: ['Hive posts that instruction on #15332.', 'The author pushes a fix; the row updates when it lands.'],
  ladder: { decision: 'return-to-contributor', reason: 'the author is connected; returned with the fix list', ruled_out: ['already-fixed: no merged PR has closed this PR\'s issue', 'mergeable-as-is: CI not green'] },
  mergeable_as_is: false, alternatives: [] };
const sentinel = '⚠ hivecommons/hive#15332 — Sentinel flagged security-relevant changes at 7af26e9: changes 1 sensitive path(s)';
let view = stageTrackerDelegatePlanView([pr15332], sentinel);
let html = stageTrackerDelegatePlanHtml(view);
let visible = visibleOf(html);
assert.strictEqual(view.title, 'Let hive decide');
assert.strictEqual(view.subject, "hivecommons/hive#15332 · ci: run crux's Go file-size ratchet against…");
assert.strictEqual((visible.match(/Hive would/g) || []).length, 1, 'the eyebrow says Hive would exactly once: ' + visible);
assert.ok(!/Hive would:\s*Hive would:/i.test(html), 'no doubled prefix');
assert.strictEqual((visible.match(/hivecommons\/hive#15332/g) || []).length, 1, 'repo#number appears once: ' + visible);
assert.ok(visible.includes('Send #15332 back to its author with one instruction: make CI green, nothing else.'), visible);
assert.ok(visible.includes('Why CI is red.'), visible);
assert.ok(html.includes('<ol class="ham-plan-next"><li>Hive posts that instruction on #15332.</li><li>The author pushes a fix; the row updates when it lands.</li></ol>'), html);
assert.ok(words(visible) <= 60, 'visible text is ' + words(visible) + ' words: ' + visible);
for (const j of JARGON) assert.ok(!visible.toLowerCase().includes(j), 'visible text leaks ' + JSON.stringify(j) + ': ' + visible);
const details = detailsOf(html);
assert.ok(details.includes('Sentinel flagged security-relevant changes at 7af26e9') && details.includes('mergeable-as-is: CI not green') && details.includes('Rails: never merges'), 'finding, ruled-out rungs and rails live under Details: ' + details);
assert.ok(!visible.includes('Rails'), 'the rails are not visible by default');
assert.ok(html.includes('<span class="ham-plan-chip" title="' + hiveDialogEscape(sentinel) + '">⚠ Sentinel</span>'), 'a compact chip flags the sentinel finding');
assert.deepStrictEqual(buttons(html), ['Cancel', 'Let hive do that'], 'exactly two buttons when the head is not mergeable');
assert.ok(html.includes('btn-danger hive-dialog-btn primary" data-initial-focus'), 'the sentinel case keeps the danger primary');
assert.ok(html.indexOf('hive-dialog-btn cancel') < html.indexOf('hive-dialog-btn primary'), 'Cancel first, primary last');

// 2. A stale "Hive would:" prefix in the payload is still rendered once.
view = stageTrackerDelegatePlanView([Object.assign({}, pr15332, { summary: 'Hive would: Hive would: send #15332 back' })], '');
assert.strictEqual(view.decisions[0].sentence, 'Send #15332 back.');

// 3. Mergeable as-is: the ladder's approve rung offers the merge button —
//    the hive would approve anyway, so the server leaves merge-now as the
//    one alternative and the button carries that label.
const green = { ref: 'o/r#8', number: 8, type: 'pr', title: 'fix typo', would: 'approve', rule: 'mergeable-as-is',
  summary: 'Approve #8 as it is and let the merge queue merge it.', why: 'Mergeable and green.', consequences: ['Hive approves the current commit.', '#8 merges once CI is green and it has soaked.'],
  ladder: { decision: 'mergeable-as-is', reason: 'mergeable and green; handed to the merge queue', ruled_out: ['already-fixed: no merged PR has closed this PR\'s issue'] },
  mergeable_as_is: true, alternatives: [{ action: 'decide:merge-now', label: 'Merge now as maintainer', enabled: true }] };
view = stageTrackerDelegatePlanView([green], '');
html = stageTrackerDelegatePlanHtml(view);
assert.deepStrictEqual(buttons(html), ['Cancel', 'Merge now as maintainer', 'Let hive do that']);
assert.strictEqual(view.mergeAction, 'decide:merge-now');
assert.ok(!html.includes('ham-plan-chip') && html.includes('btn-primary hive-dialog-btn primary'), 'no sentinel: plain primary, no chip');
assert.ok(!html.includes('aria-disabled'), 'enabled with merge rights');
// …greyed with the reason without merge rights…
html = stageTrackerDelegatePlanHtml(stageTrackerDelegatePlanView([Object.assign({}, green, { alternatives: [{ action: 'decide:merge-now', enabled: false, reason_if_disabled: 'Needs merge rights' }] })], ''));
assert.ok(html.includes('ham-plan-merge" aria-disabled="true" disabled title="Needs merge rights"'), html);
// Any other verdict on a mergeable head leads with "Approve and let hive
// merge" (queue-merge), even when merge-now is listed too; the click runs
// queue-merge.
const recommend = Object.assign({}, green, { would: 'recommend-only', summary: 'Recommend only: a maintainer merges #8.',
  alternatives: [{ action: 'decide:queue-merge', label: 'Approve and let hive merge', enabled: true }, { action: 'decide:merge-now', label: 'Merge now as maintainer', enabled: true }] });
view = stageTrackerDelegatePlanView([recommend], '');
html = stageTrackerDelegatePlanHtml(view);
assert.deepStrictEqual(buttons(html), ['Cancel', 'Approve and let hive merge', 'Let hive do that']);
assert.strictEqual(view.mergeAction, 'decide:queue-merge');
assert.strictEqual(view.mergeLabel, STAGE_TRACKER_PLAN_BUTTON_MERGE);
assert.ok(view.mergeable && !html.includes('aria-disabled'));
// …greyed on the queue-merge alternative's own reason…
view = stageTrackerDelegatePlanView([Object.assign({}, recommend, { alternatives: [{ action: 'decide:queue-merge', enabled: false, reason_if_disabled: 'merger role required' }, { action: 'decide:merge-now', enabled: true }] })], '');
assert.strictEqual(view.mergeable, false); assert.strictEqual(view.mergeWhy, 'merger role required');
// …a batch sharing merge-now falls back to it; one with no shared merge
// alternative offers no merge button at all.
assert.strictEqual(stageTrackerDelegatePlanMergeAction([green, recommend]), 'decide:merge-now');
assert.strictEqual(stageTrackerDelegatePlanOffersMerge([green, Object.assign({}, recommend, { alternatives: [{ action: 'decide:queue-merge', enabled: true }] })]), false);
// …and absent when the plan says the head is not mergeable, whatever the alternatives list.
html = stageTrackerDelegatePlanHtml(stageTrackerDelegatePlanView([Object.assign({}, green, { mergeable_as_is: false })], ''));
assert.deepStrictEqual(buttons(html), ['Cancel', 'Let hive do that']);

// 4. An issue decision renders through the same model: no merge button,
//    no ladder, the direction option is not a button.
const issue = { ref: 'o/r#42', number: 42, type: 'issue', title: 'Flaky test in CI', would: 'recommend-only', summary: 'Recommend only: close #42 as already fixed.', why: 'The question is hive\'s own policy.', consequences: ['Hive posts a comment and nothing else.', '#42 waits for you to decide.'], alternatives: [{ action: 'decide:direction', label: 'Give direction', enabled: true }] };
view = stageTrackerDelegatePlanView([issue], '');
html = stageTrackerDelegatePlanHtml(view);
visible = visibleOf(html);
assert.strictEqual(view.subject, 'o/r#42 · Flaky test in CI');
assert.deepStrictEqual(buttons(html), ['Cancel', 'Let hive do that']);
assert.ok(visible.includes('Recommend only: close #42 as already fixed.') && !html.includes('ham-plan-ruled-out'), visible);
assert.ok(words(visible) <= 60, visible);

// 5. A batch names each item on its own block, once.
html = stageTrackerDelegatePlanHtml(stageTrackerDelegatePlanView([green, Object.assign({}, green, { ref: 'o/r#9', number: 9 })], ''));
visible = visibleOf(html);
assert.strictEqual((visible.match(/o\/r#8/g) || []).length, 1, visible);
assert.ok(visible.startsWith('Let hive decide 2 items'), visible);
// Three "then" items are cut to two.
assert.strictEqual(stageTrackerDelegatePlanView([Object.assign({}, green, { consequences: ['a', 'b', 'c'] })], '').decisions[0].next.length, 2);
`)
}

// TestStageTrackerAdvisoryCloseReadProgress (hivecommons/hive#15436, owner:
// "i clicked close and it still shows in list. I should see some progress
// … until the item is closed and is out of the list"): Close (read) on an
// advisory row acts on the row itself (it is not a decision park, so the
// decision-target mapping used to come up empty and the click did
// nothing), the chevron is busy with "Closing…" from the first byte sent,
// reads "Closed · leaving list" once the server answers, and the pending
// state lifts when the next tracker payload no longer lists the row. A
// refused or failed close restores the chevron, toasts the reason and
// puts it on the row's history badge; an empty target list is never silent.
func TestStageTrackerAdvisoryCloseReadProgress(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+"\n"+jsConstBlock(t, html, "STAGE_TRACKER_ICONS")+"\n"+stageTrackerActedJS(t, html)+`
const toasts = [];
global.showToast = (msg, type) => { toasts.push(type + ':' + msg); return null; };
global.dismissToast = () => {};
global.setTimeout = fn => fn();
function stageTrackerProgressToast() { return { update() {}, done(msg, type) { if (msg) toasts.push('summary:' + type + ':' + msg); } }; }
function stageTrackerAfterActions() {}
function stageTrackerDecidePending(pend) { pend.decided = true; }
// hivecommons/hive#15381 as the tracker lists it: stage 0, no modifiers, advisory block.
const adv = { repo: 'hivecommons/hive', kind: 'issue', number: 15381, url: 'u15381', title: 'triage: duplicate and overlapping open issues/PRs to consolidate', stage: 0, caption: 'Advisory for maintainers · nothing found · safe to close', modifiers: [], labels: ['help wanted', 'agent/dedupe', 'triage/advisory'],
  advisory: { kind: 'advisory', source: 'label', empty: true, chip: 'Advisory · nothing found · safe to close', caption: 'Advisory for maintainers · nothing found · safe to close' } };
const plain = { repo: 'hivecommons/hive', kind: 'issue', number: 2, url: 'u2', stage: 10, caption: 'Open', modifiers: [] };
_stageTracker = { items: [adv, plain] };
hamPanel.appendChild(hamStubRow('hivecommons/hive/issue#15381')); hamPanel.appendChild(hamStubRow('hivecommons/hive/issue#2'));
const row = k => hamPanel.querySelectorAll('[data-ham-key], [data-ham-sub-key]').find(n => n.getAttribute('data-ham-key') === k);
const acting = k => row(k).classList.contains('cue-acting');
const caption = k => { const c = row(k).children[0].querySelector(':scope > .cue-acting-cap'); return c ? c.lastChild.nodeValue : ''; };
const K = 'hivecommons/hive/issue#15381';

assert.strictEqual(stageTrackerDecisionTarget(adv), null, 'an advisory is not a decision park');
assert.deepStrictEqual(stageTrackerActionRequest(adv, 'decide:close-read', {}), { url: '/api/repos/hivecommons/hive/items/15381/decision', body: { decision: 'close-read', comment: '', type: 'issue' } });
assert.strictEqual(stageTrackerTerminalCaption({ action: 'closed' }), 'Closed · leaving list');
assert.strictEqual(stageTrackerTerminalCaption({ action: 'held' }), '');

const calls = []; let answer = null; let seenBusy = '';
global.fetch = async (url, init) => {
  calls.push({ url, body: JSON.parse(init.body) });
  seenBusy = acting(K) ? caption(K) : '';
  if (answer instanceof Error) throw answer;
  return { ok: answer.ok !== false, status: answer.status || 200, json: async () => answer.data };
};
(async () => {
  // 1. Happy path: confirm → request → busy → terminal caption → gone on the next payload.
  answer = { data: { ok: true, status: 'closed', decision: 'close-read', result: { action: 'closed', target: { repo: 'hivecommons/hive', kind: 'issue', number: 15381, url: 'u15381' }, outcome: 'Closed as read (advisory acknowledged).', next: 'Nothing further to do; the row leaves the tracker on its next refresh.' } } };
  let out = await runRowAction('decide:close-read', [adv]);
  assert.strictEqual(dialogs.length, 1, 'Close (read) asks once'); assert.strictEqual(dialogs[0].cfg.title, 'Close advisory');
  assert.strictEqual(calls.length, 1, 'the request is sent'); assert.strictEqual(calls[0].url, '/api/repos/hivecommons/hive/items/15381/decision'); assert.strictEqual(calls[0].body.decision, 'close-read');
  assert.strictEqual(seenBusy, 'Closing…', 'the chevron is busy with the verb from the first byte sent');
  assert.strictEqual(out.results[0].ok, true);
  assert.strictEqual(acting(K), true, 'still acting after the answer');
  assert.strictEqual(caption(K), 'Closed · leaving list', 'the terminal caption until the row leaves');
  assert.strictEqual(out.pending.length, 1); assert.strictEqual(out.pending[0].decided, true, 'the badge may show again');
  assert.strictEqual(out.toasts.length, 1); assert.strictEqual(out.toasts[0].text.type, 'success');
  // The same payload again (GitHub list lagging) keeps the terminal caption.
  stageTrackerPendingExpire({ items: [adv, plain] }, Date.now());
  assert.strictEqual(_stageTrackerPending.length, 1, 'the pending state survives an unchanged payload');
  // The server filtered the closed row out: pending lifts.
  stageTrackerPendingExpire({ items: [plain] }, Date.now());
  assert.strictEqual(_stageTrackerPending.length, 0, 'pending lifts once the row is gone');
  assert.strictEqual(stageTrackerActionFailureOf(adv), null, 'no failure noted on success');

  // 2. Refused (paused repo): 200 with a refused result — chevron restored, toast + badge say why.
  calls.length = 0; toasts.length = 0; dialogs.length = 0;
  answer = { data: { ok: true, status: 'refused', decision: 'close-read', result: { action: 'refused', target: { repo: 'hivecommons/hive', kind: 'issue', number: 15381 }, outcome: 'Close refused: repo paused — hivecommons/hive is paused by the operator (by clubanderson: freeze).', next: 'Resume the repository to let the dashboard close it, or close it on GitHub yourself.' } } };
  out = await runRowAction('decide:close-read', [adv]);
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(acting(K), false, 'a refused close does not leave the chevron busy');
  assert.strictEqual(out.toasts[0].text.type, 'warning');
  assert.ok(out.toasts[0].text.outcome.startsWith('Close refused: repo paused'), out.toasts[0].text.outcome);
  let f = stageTrackerActionFailureOf(adv);
  assert.ok(f && f.state === 'failed' && f.action === 'close-read', JSON.stringify(f));
  let src = stageTrackerActedSource(adv);
  assert.strictEqual(src.action.detail, answer.data.result.outcome, 'the badge reads the client-noted failure');
  let tip = stageTrackerActedTip(adv, Date.now());
  assert.ok(tip.split('\n')[0].includes('Close refused: repo paused'), 'hover first line: ' + tip);
  assert.ok(tip.includes('Closed') && tip.includes('failed'), tip);

  // 3. GitHub 403: the error toast names it; the badge too.
  calls.length = 0; toasts.length = 0;
  answer = { ok: false, status: 403, data: { error: 'GitHub 403 Resource not accessible by integration' } };
  out = await runRowAction('decide:close-read', [adv]);
  assert.strictEqual(acting(K), false);
  assert.strictEqual(out.results[0].ok, false); assert.strictEqual(out.toasts[0].text.type, 'error');
  assert.strictEqual(out.toasts[0].text.head, 'Close now failed on hivecommons/hive#15381');
  assert.ok(out.toasts[0].text.outcome.includes('GitHub 403'), out.toasts[0].text.outcome);
  tip = stageTrackerActedTip(adv, Date.now());
  assert.ok(tip.split('\n')[0].includes('Close now failed: GitHub 403'), tip);
  // A newer server record outranks the client note.
  const later = { ...adv, last_operator_action: { action: 'close-read', actor: 'clubanderson', at: new Date(Date.now() + 1000).toISOString(), outcome: 'applied', detail: 'closed', state: 'addressed' } };
  assert.strictEqual(stageTrackerActionFailureOf(later), null);
  assert.strictEqual(stageTrackerActedSource(later).action.outcome, 'applied');

  // 4. Network error.
  calls.length = 0; toasts.length = 0;
  answer = new Error('Failed to fetch');
  out = await runRowAction('decide:close-read', [adv]);
  assert.strictEqual(acting(K), false); assert.strictEqual(out.results[0].error, 'Failed to fetch');
  assert.ok(stageTrackerActedTip(adv, Date.now()).includes('Failed to fetch'));

  // 5. Not an advisory / no decision target: never silent.
  calls.length = 0; toasts.length = 0; dialogs.length = 0;
  out = await runRowAction('decide:close-read', [plain]);
  assert.strictEqual(calls.length, 0); assert.strictEqual(dialogs.length, 0);
  assert.strictEqual(toasts.length, 1); assert.ok(toasts[0].startsWith('warning:Close now: nothing to act on'), toasts[0]);
  toasts.length = 0;
  out = await runRowAction('decide:approve', [plain]);
  assert.strictEqual(calls.length, 0);
  assert.ok(toasts[0].startsWith('warning:Approve: nothing to act on'), toasts[0]);

  // 6. Generic: close-superseded on a parked PR gets the same terminal caption.
  const pr = { repo: 'hivecommons/hive', kind: 'pr', number: 9, url: 'u9', stage: 85, caption: 'Needs human decision', modifiers: ['needs-decision'] };
  _stageTracker = { items: [pr] }; hamPanel.appendChild(hamStubRow('hivecommons/hive/pr#9'));
  calls.length = 0; dialogAnswer = '';
  answer = { data: { ok: true, status: 'closed', result: { action: 'closed', target: { repo: 'hivecommons/hive', kind: 'pr', number: 9 }, outcome: 'Closed as superseded.', next: '' } } };
  out = await runRowAction('decide:close-wontfix', [pr]);
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(caption('hivecommons/hive/pr#9'), 'Closed · leaving list');
})().catch(e => { console.error(e); process.exit(1); });
`)
}

// TestStageTrackerBulkAcceptTriage: the selection bar and the bulk ⋯ menu
// offer Accept triage for every selected issue waiting on triage at once
// (owner report: four "Triage needed" rows multi-selected, no Accept
// triage in the menu). It leads the bar when the whole selection is
// triage ("Accept triage (4 selected)"), counts out the rows it skips
// otherwise ("Accept triage (2 of 4)"), is greyed with the first reason
// when a viewer may not write, posts the per-item accept-triage endpoint
// once per eligible row in order, re-renders each as Actionable and sums
// up as one toast "Accepted N issues".
func TestStageTrackerBulkAcceptTriage(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const ctx = { user: true, canHold: true, canHuman: true };
const tri = n => ({ repo: 'o/r', kind: 'issue', number: n, url: 'u' + n, stage: 15, caption: 'Triage needed', modifiers: ['needs-triage'] });
const t1 = tri(15437), t2 = tri(15438), t3 = tri(15439), t4 = tri(15440);
const plain = { repo: 'o/r', kind: 'issue', number: 7, url: 'u7', stage: 10, caption: 'Actionable', modifiers: [] };
const pr = { repo: 'o/r', kind: 'pr', number: 8, url: 'u8', stage: 20, caption: 'Claimed', modifiers: [] };

// All four waiting on triage: Accept triage leads the selection.
const all = stageTrackerSelectionActions([t1, t2, t3, t4], () => ctx);
assert.strictEqual(all[0].id, 'accept-triage');
assert.strictEqual(all[0].label, 'Accept triage (4 selected)');
assert.strictEqual(all[0].enabled, true);
assert.strictEqual(all[0].lead, true, 'leads when every selected row waits on triage');
assert.strictEqual(all[0].count, 4);
const bar = stageTrackerSelBarHtml([t1, t2, t3, t4], all);
assert.ok(bar.includes('class="ham-lead-act" data-action="hamSelAction" data-arg0="accept-triage"'), 'the bar renders it as the lead button: ' + bar);
assert.ok(bar.includes('>Accept triage (4 selected)<'));
const menu = stageTrackerMenuHtml(all, '4 selected');
assert.ok(menu.indexOf('data-ham-act="accept-triage"') < menu.indexOf('data-ham-act="decide"'), 'the ⋯ menu offers it first');

// Mixed: a PR and an actionable issue in the selection are skipped and counted out.
const mixed = stageTrackerSelectionActions([t1, plain, t2, pr], () => ctx);
const mt = mixed.find(a => a.id === 'accept-triage');
assert.ok(mt, 'offered when at least one selected row waits on triage');
assert.strictEqual(mt.label, 'Accept triage (2 of 4)');
assert.strictEqual(mt.hint, 'skips the 2 not waiting on triage');
assert.strictEqual(mt.enabled, true);
assert.strictEqual(mt.lead, false, 'not the lead when some rows are skipped');
assert.ok(mixed.indexOf(mt) > mixed.findIndex(a => a.id === 'decide'), 'sits after Decide… when it does not lead');
assert.ok(stageTrackerSelBarHtml([t1, plain], mixed).includes('class="ham-chip" data-action="hamSelAction" data-arg0="accept-triage"'), 'a chip, not the lead button, when mixed');

// Nothing waiting on triage: not offered at all.
assert.ok(!stageTrackerSelectionActions([plain, pr], () => ctx).some(a => a.id === 'accept-triage'));

// No write access: greyed with the reason, never hidden.
const denied = stageTrackerSelectionActions([t1, t2], () => ({ user: true, canHold: false, canHuman: false }))[0];
assert.strictEqual(denied.id, 'accept-triage');
assert.strictEqual(denied.enabled, false);
assert.ok(denied.why.includes('o/r#15437'), denied.why);

// The run: one POST per eligible row, in order; skipped rows are not posted.
const calls = [];
global.fetch = async (url, init) => { calls.push(url); return { ok: true, status: 200, json: async () => ({ ok: true, result: { action: 'triage-accepted', target: { repo: 'o/r', kind: 'issue', number: Number(url.match(/items\/(\d+)/)[1]) }, outcome: 'accepted' } }) }; };
const toasts = [];
global.showToast = (msg, type) => { toasts.push(type + ':' + msg); return null; }; global.dismissToast = () => {};
function stageTrackerProgressToast() { return { update() {}, done(msg, type) { if (msg) toasts.push('summary:' + type + ':' + msg); } }; }
function stageTrackerAfterActions() {}
function stageTrackerNoteOwnerEvent() {}
// Accept triage asks nothing first (no dialog, native or in-app).
async function stageTrackerActionPayload(items, id) { assert.strictEqual(id, 'accept-triage'); return {}; }
_stageTracker = { items: [t1, plain, t2, pr] };
(async () => {
  const res = await runRowAction('accept-triage', [t1, plain, t2, pr]);
  assert.deepStrictEqual(calls, ['/api/repos/o/r/items/15437/accept-triage', '/api/repos/o/r/items/15438/accept-triage'], 'only the rows waiting on triage are posted, each to the per-item endpoint');
  assert.strictEqual(res.results.length, 2);
  assert.ok(res.results.every(r => r.ok));
  assert.strictEqual(_stageTracker.items[0].stage, 10, 'each accepted row re-reads Actionable at once');
  assert.strictEqual(_stageTracker.items[2].stage, 10);
  assert.ok(toasts.includes('summary:success:Accepted 2 issues'), 'one summary toast: ' + JSON.stringify(toasts));

  // Nothing eligible: a warning toast, no request.
  calls.length = 0; toasts.length = 0;
  await runRowAction('accept-triage', [plain, pr]);
  assert.deepStrictEqual(calls, []);
  assert.ok(toasts.some(x => x.startsWith('warning:Accept triage: nothing to act on')), JSON.stringify(toasts));

  // The summary counts failures.
  assert.strictEqual(stageTrackerActionSummary('Accept triage', [{ item: t1, ok: true }, { item: t2, ok: false, error: 'HTTP 403' }], 'accept-triage').msg, 'Accepted 1 of 2 issues. Failed: o/r#15438 — HTTP 403');
  assert.strictEqual(stageTrackerActionSummary('Accept triage', [{ item: t1, ok: true }, { item: t2, ok: true }, { item: t3, ok: true }, { item: t4, ok: true }], 'accept-triage').msg, 'Accepted 4 issues');
})().catch(e => { console.error(e); process.exit(1); });
`)
}
