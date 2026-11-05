package dashboard

import (
	"strings"
	"testing"
)

// Owner report: a hard reload of the dashboard flashed the decision pills
// ("Let hive decide", "Approve and let hive merge", "Merge now as
// maintainer") grey and unclickable, then they came back a poll later. The
// tracker's first paint ran before the auth fetch had named the viewer, so
// stageTrackerActionCtx read the missing role as 'read' and every
// role-gated option rendered aria-disabled with a sign-in reason. Unknown is
// not read: while auth is pending the ctx is optimistic (like canHold ===
// null), and showGHAuthState repaints the tracker once the answer lands.
// The helpers run under node the way stage_tracker_row_actions_test.go does.

func stageTrackerAuthPendingPreamble(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		stageTrackerDecisionPopPreamble(t, html),
		jsFunc(t, html, "dashboardRoleAtLeast"),
		jsFunc(t, html, "stageTrackerAuthPending"),
		jsFunc(t, html, "stageTrackerActionCtx"),
		jsFunc(t, html, "hiveAuthResolved"),
		jsFunc(t, html, "showGHAuthState"),
		// showGHAuthState's header DOM, the profile URL helper and the
		// tracker re-render it schedules, as stubs that record calls.
		`const authEls = {};
global.document.getElementById = id => (authEls[id] = authEls[id] || { style: {}, hidden: false, textContent: '', src: '', alt: '', href: '', setAttribute() {}, removeAttribute() {} });
global.ghProfileURL = u => 'https://github.com/' + u;
let repaints = 0;
global.renderStageTracker = () => { repaints++; };
_stageTracker = { items: [] };
global.window = {};`,
	}, "\n")
}

func TestStageTrackerActionCtxWhileAuthPending(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerAuthPendingPreamble(t, html)+stageTrackerDecisionPopFixture+`
const mergeDecision = { question: 'Ready for human merge', asked_by: 'the hive', since: '2026-10-27T10:30:00Z', options: [
  { action: 'decide:queue-merge', label: 'Approve and let hive merge', enabled: true },
  { action: 'decide:merge-now', label: 'Merge now as maintainer', enabled: true },
  { action: 'decide:delegate', label: 'Let hive decide', enabled: true },
] };
const mergePR = Object.assign({}, pr, { number: 9, url: 'https://github.com/o/r/pull/9', manual_merge: true, decision_needed: mergeDecision });

// Before auth answers: nothing names the viewer, so the ctx is pending and
// optimistic — no option is greyed with a role or sign-in reason.
assert.strictEqual(stageTrackerAuthPending(), true);
let actx = stageTrackerActionCtx('o/r');
assert.strictEqual(actx.authPending, true);
assert.strictEqual(actx.user, true);
assert.strictEqual(actx.canHold, null);
assert.strictEqual(actx.canHuman, true);
let opts = stageTrackerDecisionOptions(mergePR, actx);
assert.deepStrictEqual(opts.map(o => o.id), ['decide:queue-merge', 'decide:merge-now', 'decide:delegate']);
assert.ok(opts.every(o => o.enabled), JSON.stringify(opts));
assert.ok(opts.every(o => o.why !== STAGE_TRACKER_WHY_ROLE && o.why !== STAGE_TRACKER_WHY_WRITE), JSON.stringify(opts));
// Options the server itself rules out stay disabled with the server's reason.
opts = stageTrackerDecisionOptions(pr, actx);
assert.strictEqual(opts.find(o => o.id === 'decide:approve').enabled, false);
assert.ok(opts.find(o => o.id === 'decide:approve').why.indexOf('requested changes') >= 0);
// The popover's lead button is not aria-disabled while pending.
const pop = stageTrackerDecisionPopHtml(mergePR, stageTrackerDecisionOptions(mergePR, actx), Date.parse('2026-10-27T12:30:00Z'), '');
assert.ok(pop.indexOf('aria-disabled') < 0, pop);
assert.ok(pop.indexOf('data-ham-act="decide:queue-merge"') >= 0, pop);
// The row pill and the selection bar follow the same actx.
const pill = stageTrackerLeadPillHtml(mergePR, stageTrackerFlatActions(stageTrackerRowActions(mergePR, actx))['decide:queue-merge']);
assert.ok(pill.indexOf('aria-disabled') < 0, pill);
// ("Release hold" is greyed as not applicable — the row is not held — which
// is the row's state, not the viewer's; the role-gated chips are live.)
const bar = stageTrackerSelBarHtml([mergePR], stageTrackerSelectionActions([mergePR], () => actx));
assert.ok(bar.indexOf(STAGE_TRACKER_WHY_ROLE) < 0 && bar.indexOf(STAGE_TRACKER_WHY_WRITE) < 0, bar);
for (const id of ['decide:delegate', 'needs-human', 'hold']) {
  const chip = bar.match(new RegExp('<button[^>]*data-arg0="' + id + '"[^>]*>'));
  assert.ok(chip && chip[0].indexOf('aria-disabled') < 0, id + ' chip: ' + (chip ? chip[0] : 'missing'));
}

// Auth lands as a merger: the actx is real, the pills stay enabled, and the
// tracker is repainted exactly once for the change.
showGHAuthState(true, 'alice', 'https://github.com/alice.png', 'merger');
assert.strictEqual(window._hiveAuthKnown, true);
assert.strictEqual(stageTrackerAuthPending(), false);
assert.strictEqual(repaints, 1);
actx = stageTrackerActionCtx('o/r');
assert.strictEqual(actx.authPending, undefined);
assert.strictEqual(actx.user, true);
assert.strictEqual(actx.canHuman, true);
assert.strictEqual(actx.login, 'alice');
opts = stageTrackerDecisionOptions(mergePR, actx);
assert.ok(opts.every(o => o.enabled), JSON.stringify(opts));
// A re-check that changes nothing does not repaint.
showGHAuthState(true, 'alice', 'https://github.com/alice.png', 'merger');
assert.strictEqual(repaints, 1);

// A viewer the server names as read-only: merge options carry the role reason.
global.window = { _hiveUser: 'bob', _hiveRole: 'read', _hiveAuthKnown: true };
actx = stageTrackerActionCtx('o/r');
assert.strictEqual(actx.canHuman, false);
opts = stageTrackerDecisionOptions(mergePR, actx);
assert.strictEqual(opts.find(o => o.id === 'decide:merge-now').enabled, false);
assert.strictEqual(opts.find(o => o.id === 'decide:merge-now').why, STAGE_TRACKER_WHY_ROLE);

// Logged out, once auth is known: every option greyed with the sign-in reason.
global.window = {};
showGHAuthState(false);
assert.strictEqual(window._hiveAuthKnown, true);
assert.strictEqual(stageTrackerAuthPending(), false);
assert.strictEqual(repaints, 2, 'pending → known-anonymous is a change worth one repaint');
actx = stageTrackerActionCtx('o/r');
assert.strictEqual(actx.user, false);
assert.strictEqual(actx.canHold, false);
opts = stageTrackerDecisionOptions(mergePR, actx);
assert.ok(opts.every(o => !o.enabled), JSON.stringify(opts));
assert.strictEqual(opts.find(o => o.id === 'decide:delegate').why, STAGE_TRACKER_WHY_WRITE);
assert.strictEqual(opts.find(o => o.id === 'decide:merge-now').why, STAGE_TRACKER_WHY_ROLE);
// Giving up on auth (both endpoints failed) also ends the pending state.
global.window = {};
hiveAuthResolved()();
assert.strictEqual(stageTrackerAuthPending(), false);
`)
}
