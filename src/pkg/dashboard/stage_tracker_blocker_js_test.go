package dashboard

import (
	"strings"
	"testing"
)

// hivecommons/hive#15099: an issue parked on a capability gap reads
// "Blocked · App lacks workflows permission", its popover carries the
// plain-words blocker and the server's options (Let hive decide disabled
// with its reason, the App settings link, Re-check permissions, close as
// partial, wait), and ⚙ → Security names the missing permission.

const stageTrackerBlockerFixture = `
const appURL = 'https://github.com/organizations/o/settings/installations/1';
const why = "Hive can't do this one: the GitHub App has no ` + "`workflows`" + ` permission, so it cannot push changes to .github/workflows/*.";
const decision = { question: 'Grant the App workflows permission, or close as partially addressed?', asked_by: 'the scanner', since: '2026-10-20T19:33:00Z',
  blocker: why, blocker_reason: 'capability:workflows-permission',
  options: [
    { action: 'decide:delegate', label: 'Let hive decide', enabled: false, reason_if_disabled: why + ' A decision cannot grant a permission.' },
    { action: 'open:app-settings', label: 'Grant the App workflows permission', enabled: true, url: appURL },
    { action: 'blocker:recheck', label: 'Re-check permissions', enabled: true },
    { action: 'blocker:patch', label: 'Prepare the patch for a person to push', enabled: false, reason_if_disabled: 'No human-authorised push identity is configured.' },
    { action: 'blocker:close-partial', label: 'Mark partially addressed and close', enabled: true },
    { action: 'blocker:wait', label: 'Keep waiting', enabled: true },
  ] };
const issue = { repo: 'hivecommons/hive', kind: 'issue', number: 14976, title: '[shared-ci] Go module proxy stream resets', url: 'https://github.com/hivecommons/hive/issues/14976',
  stage: 45, stage_since: '2026-10-20T19:33:00Z', modifiers: ['needs-human'], decision_needed: decision,
  blocker: { kind: 'capability:workflows-permission', short: 'App lacks workflows permission', permission: 'workflows', since: '2026-10-20T19:33:00Z' } };
const ctx = { user: true, canHold: true, canHuman: true };
`

func TestStageTrackerBlockerPillAndOptions(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerDecisionPopPreamble(t, html)+
		jsConstLine(t, html, "STAGE_TRACKER_BLOCKED_WORD")+"\n"+
		jsFunc(t, html, "stageTrackerBlockerWord")+"\n"+
		jsFunc(t, html, "stageTrackerBlockerOptionUrl")+"\n"+
		stageTrackerBlockerFixture+`
// The pill word: "Blocked · <short>", never "Needs human decision"; a row without a blocker has none.
assert.strictEqual(stageTrackerBlockerWord(issue), 'Blocked · App lacks workflows permission');
assert.strictEqual(stageTrackerBlockerWord({ stage: 45, modifiers: ['needs-human'] }), '');
assert.strictEqual(stageTrackerBlockerWord(null), '');

// Row actions carry the server's blocker options in order with their labels, links and reasons.
const acts = stageTrackerBlockerActions(issue, ctx);
assert.deepStrictEqual(acts.map(a => a.id), ['open:app-settings', 'blocker:recheck', 'blocker:patch', 'blocker:close-partial', 'blocker:wait']);
assert.strictEqual(acts[0].url, appURL);
assert.strictEqual(acts[0].enabled, true);
assert.strictEqual(acts[2].enabled, false);
assert.ok(acts[2].why.indexOf('No human-authorised push identity') >= 0, acts[2].why);
assert.strictEqual(acts[3].danger, true);
assert.ok(stageTrackerRowActions(issue, ctx).some(a => a.id === 'blocker:recheck'));
assert.deepStrictEqual(stageTrackerBlockerActions({ stage: 45, decision_needed: decision }, ctx), []);
// A read-only viewer may still open the settings link and wait; the rest need write access.
const ro = stageTrackerBlockerActions(issue, { user: false, canHold: false, canHuman: false });
assert.strictEqual(ro.find(a => a.id === 'open:app-settings').enabled, true);
assert.strictEqual(ro.find(a => a.id === 'blocker:wait').enabled, true);
assert.strictEqual(ro.find(a => a.id === 'blocker:recheck').enabled, false);
assert.strictEqual(ro.find(a => a.id === 'blocker:recheck').why, STAGE_TRACKER_WHY_WRITE);
assert.strictEqual(stageTrackerBlockerOptionUrl(issue, 'open:app-settings'), appURL);
assert.strictEqual(stageTrackerBlockerOptionUrl(issue, 'blocker:recheck'), '');

// The popover options: "Let hive decide" is shown but disabled with the reason (not greyed silently).
const opts = stageTrackerDecisionOptions(issue, ctx);
const del = opts.find(o => o.id === 'decide:delegate');
assert.ok(del, JSON.stringify(opts));
assert.strictEqual(del.enabled, false);
assert.ok(del.why.indexOf('cannot grant a permission') >= 0, del.why);
const link = opts.find(o => o.id === 'open:app-settings');
assert.ok(link && link.url === appURL, JSON.stringify(link));
assert.ok(opts.some(o => o.id === 'blocker:close-partial' && o.enabled));

// The popover text: the blocker in plain words, the App settings link, the disabled delegate with its title.
const nowMs = Date.parse('2026-10-20T21:10:00Z');
const pop = stageTrackerDecisionPopHtml(issue, opts, nowMs, 'Needs decision — tip');
assert.ok(pop.indexOf('has no <code>workflows</code> permission') >= 0 || pop.indexOf('has no `+"`workflows`"+` permission') >= 0, pop);
assert.ok(pop.indexOf('.github/workflows/*') >= 0, pop);
// One primary action — the first the viewer may take, since "Let hive decide" cannot
// grant a permission — and "More ⋯" for the rest (the disabled delegate keeps its
// reason in the row menu).
const buttons = pop.match(/<button[^>]*data-ham-act="[^"]+"[^>]*>[^<]*<\/button>/g) || [];
assert.strictEqual(buttons.length, 1, buttons.join('\n'));
const first = opts.find(o => o.enabled);
assert.ok(buttons[0].indexOf('data-ham-act="' + first.id + '"') >= 0 && buttons[0].indexOf('ham-decision-lead') >= 0 && !/aria-disabled/.test(buttons[0]), buttons[0]);
assert.ok(pop.indexOf('data-ham-more="1"') >= 0, pop);
// No slugs reach the operator.
for (const bad of ['capability:workflows-permission', 'requires_human']) assert.ok(pop.indexOf(bad) < 0, bad + ' in ' + pop);
`)
}

func TestStageTrackerBlockerRunsActions(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerBlockerOptionUrl")+"\n"+
		stageTrackerBlockerFixture+`
// Requests: re-check probes the installation; close-partial posts the comment.
let req = stageTrackerActionRequest(issue, 'blocker:recheck', {});
assert.ok(req.url.endsWith('/blocker/recheck'), req.url);
assert.deepStrictEqual(req.body, {});
req = stageTrackerActionRequest(issue, 'blocker:close-partial', { comment: 'Closing as partially addressed.' });
assert.ok(req.url.endsWith('/blocker/close-partial'), req.url);
assert.strictEqual(req.body.comment, 'Closing as partially addressed.');
assert.strictEqual(stageTrackerActionRequest(Object.assign({}, issue, { kind: 'pr' }), 'blocker:recheck', {}), null);
`)
}

func TestCapabilityGapsText(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "capabilityGapsText"),
		jsFunc(t, html, "securityCapabilitiesHtml"),
	}, "\n")+`
assert.strictEqual(capabilityGapsText(null), '');
assert.strictEqual(capabilityGapsText({ missing: [], blocked_items: 0 }), '');
assert.strictEqual(capabilityGapsText({ missing: ['workflows'], blocked_items: 1 }), 'GitHub App permissions: missing `+"`workflows`"+` · 1 item blocked');
assert.strictEqual(capabilityGapsText({ missing: ['workflows', 'secrets'], blocked_items: 3, by_permission: { workflows: 2, secrets: 1 } }),
  'GitHub App permissions: missing `+"`workflows` (2), `secrets` (1)"+` · 3 items blocked');
const ok = securityCapabilitiesHtml({}, '');
assert.ok(ok.indexOf('no item is blocked') >= 0, ok);
const gap = securityCapabilitiesHtml({ missing: ['workflows'], blocked_items: 1 }, 'https://github.com/settings/installations/1');
assert.ok(gap.indexOf('ham-capability-gap') >= 0 && gap.indexOf('role="status"') >= 0, gap);
assert.ok(gap.indexOf('href="https://github.com/settings/installations/1"') >= 0, gap);
assert.ok(gap.indexOf('missing `+"`workflows`"+`') >= 0, gap);
`)
}

func TestStageTrackerBlockerWiredInRow(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`(typeof stageTrackerBlockerWord === 'function' && cur === STAGE_TRACKER_ISSUE_NEEDS_DECISION && stageTrackerBlockerWord(item))`,
		`if (typeof stageTrackerBlockerActions === 'function') for (const a of stageTrackerBlockerActions(item, ctx)) out.push(a);`,
		`renderSecuritySection('GitHub App permissions'`,
		`.ham-capability-gap { color: var(--amber); }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
