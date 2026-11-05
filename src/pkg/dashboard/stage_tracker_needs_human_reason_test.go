package dashboard

import (
	"strings"
	"testing"
)

// A row parked on a person states WHY on the row itself — not only in a
// tooltip. hivecommons/docs#612: an issue at 045 with a capability blocker,
// covered by a human-authored PR, read a bare muted "needs human" and "Let
// hive decide ▸" once the PR's strip took over. rationguard#480: a PR at 085
// read only "Needs human decision" though the server knew the reason
// ("workflow path"), and its issue #431 wore "blocked" purely because the PR
// waited on that decision.

const stageTrackerNeedsHumanFixture = `
const appURL = 'https://github.com/organizations/hivecommons/settings/installations/1';
const why612 = "Hive can't do this one: the GitHub App has no ` + "`workflows`" + ` permission, so it cannot push changes to .github/workflows/*. Options: (a) grant the App workflows: write, (b) apply the patch by hand, (c) close as partially addressed, (d) keep waiting.";
const decision612 = { question: 'Grant the App workflows permission, apply the patch by hand, or close as partially addressed?', asked_by: 'the hive', since: '2026-10-20T19:33:00Z',
  blocker: why612, blocker_reason: 'capability:workflows-permission',
  options: [
    { action: 'open:app-settings', label: 'Grant the App workflows: write', enabled: true, url: appURL },
    { action: 'blocker:recheck', label: 'Re-check permissions', enabled: true },
    { action: 'blocker:patch', label: 'Prepare the patch for a person to push', enabled: false, reason_if_disabled: 'No human-authorised push identity is configured.' },
    { action: 'blocker:close-partial', label: 'Mark partially addressed and close', enabled: true },
    { action: 'blocker:wait', label: 'Keep waiting', enabled: true },
    { action: 'decide:delegate', label: 'Let hive decide', enabled: false, reason_if_disabled: 'No decision grants the App the permission; a maintainer must act in GitHub.' },
    { action: 'hold', label: 'Hold', enabled: true },
  ] };
const pr620 = { repo: 'hivecommons/docs', kind: 'pr', number: 620, title: 'ci: opt docs refresh into hive', url: 'https://github.com/hivecommons/docs/pull/620',
  stage: 50, stage_since: '2026-10-21T09:00:00Z', state: 'open', head: 'h620', author: 'bob', author_kind: 'human', caption: 'In review',
  linked_issues: [{ number: 612, state: 'open', via: 'closes' }] };
const issue612 = { repo: 'hivecommons/docs', kind: 'issue', number: 612, title: 'Opt docs refresh into hive', url: 'https://github.com/hivecommons/docs/issues/612',
  stage: 45, stage_since: '2026-10-20T19:33:00Z', state: 'open', modifiers: ['needs-human'], needs_human: true,
  reason: 'blocked: App lacks workflows permission', caption: 'A human decision is owed — blocked: App lacks workflows permission',
  human_attention: { kind: 'issue', number: 612, reason: why612, asked_by: 'the hive' },
  blocker: { kind: 'capability:workflows-permission', short: 'App lacks workflows permission', permission: 'workflows', since: '2026-10-20T19:33:00Z',
    detail: 'needs-human: the .github/workflows/hive-docs-refresh.yml opt-in + add-paths change must be applied by a maintainer; App token lacks workflows permission' },
  decision_needed: decision612, linked_pr: 620, linked_prs: [pr620] };
const decision480 = { question: 'The hive never merges this by itself (it edits CI workflow files). Approve it for a maintainer to merge, give direction, or close it?', asked_by: 'the hive', since: '2026-10-22T08:00:00Z',
  blocker: 'The hive never merges this by itself. After approval, a maintainer merges.',
  options: [ { action: 'decide:delegate', label: 'Let hive decide', enabled: true }, { action: 'decide:approve', label: 'Approve', enabled: true } ] };
const pr480 = { repo: 'hivecommons/rationguard', kind: 'pr', number: 480, title: 'ci: pin workflow runners', url: 'https://github.com/hivecommons/rationguard/pull/480',
  stage: 85, stage_since: '2026-10-22T08:00:00Z', state: 'open', head: 'h480', author: 'hive-bot', author_kind: 'hive', base_ref: 'main', modifiers: ['needs-decision'],
  caption: 'A human decision is owed — workflow path', human_reason: 'workflow path',
  stage_actor: { kind: 'human', why: 'a human decision is owed — workflow path', human_reason: 'workflow path' },
  decision_needed: decision480, linked_issues: [{ number: 431, state: 'open', via: 'closes' }] };
const issue431 = { repo: 'hivecommons/rationguard', kind: 'issue', number: 431, title: 'Pin workflow runners', url: 'https://github.com/hivecommons/rationguard/issues/431',
  stage: 40, stage_since: '2026-10-21T08:00:00Z', state: 'open', modifiers: ['blocked'], caption: 'PR open', linked_pr: 480,
  // The live payload's linked entry carried no modifiers of its own, so the issue's "blocked" led the pill.
  linked_prs: [(({ modifiers, ...rest }) => rest)(pr480)] };
`

func TestStageTrackerNeedsHumanReasonOnRow(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerHumanAttentionOf")+"\n"+
		jsFunc(t, html, "stageTrackerNeedsHumanAsk")+"\n"+
		stageTrackerRowsFixtureJS+stageTrackerNeedsHumanFixture+`
// Short reasons: the known map first, then the clause after "—".
assert.strictEqual(stageTrackerNeedsHumanShort(issue612), 'App lacks workflows permission');
assert.strictEqual(stageTrackerNeedsHumanShort(pr480), 'edits CI workflows');
assert.strictEqual(stageTrackerNeedsHumanWord(pr480), 'Needs human · edits CI workflows');
assert.strictEqual(stageTrackerNeedsHumanShort({ stage: 85, stage_actor: { kind: 'human', why: 'a human decision is owed — the reviewer asked for a design note. Then more.' } }), 'the reviewer asked for a design note');
assert.strictEqual(stageTrackerNeedsHumanWord({ stage: 85, modifiers: ['needs-decision'] }), '');

// rationguard#480: the 085 chevron names the reason, not a bare "Needs human decision".
let row = stageTrackerRowHtml(pr480, stages, ctx);
assert.ok(text(strip(row)).includes('Needs human · edits CI workflows'), 'the 085 caption carries the short reason: ' + text(strip(row)));
const detour = (/<span class="ham-detour(?: ham-has-holder)?">([^<]*)</.exec(strip(row)) || [])[1];
assert.strictEqual(detour, 'Needs human · edits CI workflows', 'the visible chevron word, not "Needs human decision"');

// rationguard#431: "blocked" only because its PR waits at 085 reads "decision on #480".
row = stageTrackerRowHtml(issue431, stages, ctx);
const pill431 = /<[a-z]+ class="ham-badge ham-badge-mod ham-mod-blocked[^"]*"[^>]*>([\s\S]*?)<\/[a-z]+>/.exec(row);
assert.ok(pill431, row);
assert.strictEqual(text(pill431[1]), 'decision on #480');
assert.ok(pill431[0].includes('Blocked only because PR #480 waits on a human decision'), pill431[0]);
// A real blocked-on dependency keeps its own words.
assert.strictEqual(stageTrackerBlockedForDecisionPR(Object.assign({}, issue431, { blocked_on: ['#7'] })), null);
assert.strictEqual(stageTrackerBlockedForDecisionPR(Object.assign({}, issue431, { linked_prs: [Object.assign({}, pr480, { stage: 50, modifiers: [], decision_needed: null })] })), null);

// docs#612: the covering human PR drives the strip, yet the issue's decision
// column still names the blocker with the person glyph, and the pill is a
// button carrying the full reason that reveals the row's note line.
row = stageTrackerRowHtml(issue612, stages, ctx);
assert.ok(row.includes('ham-blocker-note'), 'the done decision column keeps the blocker: ' + row);
assert.ok(text(strip(row)).includes('Blocked · App lacks workflows permission'), text(strip(row)));
assert.ok(/<li class="ham-seg done[^"]*ham-blocker-note[^"]*"[\s\S]*?aria-label="Next move: human"/.test(strip(row)), 'the blocker note wears the person glyph');
const pill612 = /<button type="button" class="ham-badge ham-badge-mod ham-mod-needs-human ham-needs-human-pill"[^>]*>/.exec(row);
assert.ok(pill612, 'the needs-human pill is a button: ' + row);
assert.ok(pill612[0].includes('data-action="stageTrackerNeedsHumanReveal"') && pill612[0].includes('data-arg0="hivecommons/docs/issue#612"'), pill612[0]);
assert.ok(pill612[0].includes('title="Hive can&#39;t do this one: the GitHub App has no'), 'the hover is the full reason: ' + pill612[0]);
// Without a blocker there is no note, and a hive-owned row stays plain.
const plain = stageTrackerRowHtml(Object.assign({}, issue612, { blocker: null, needs_human: false, human_attention: null, decision_needed: null, modifiers: [] }), stages, ctx);
assert.ok(!plain.includes('ham-blocker-note') && !plain.includes('ham-needs-human-pill'), plain);
`)
}

func TestStageTrackerNeedsHumanLeadAction(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+"\n"+stageTrackerNeedsHumanFixture+`
const ctx = { user: true, canHold: true, canHuman: true, rows: [issue612] };
// The server ruled delegation out: the lead is the first enabled blocker
// option, and "Let hive decide" stands beside it muted with the reason.
const lead = stageTrackerLeadAction(issue612, ctx);
assert.ok(lead, 'a lead');
assert.strictEqual(lead.id, 'open:app-settings', JSON.stringify(lead));
assert.strictEqual(lead.label, 'Grant the App workflows: write');
assert.strictEqual(lead.enabled, true);
assert.strictEqual(lead.url, appURL);
const pills = stageTrackerLeadActionsOnRow(issue612, ctx, []);
assert.deepStrictEqual(pills.map(a => a.id), ['open:app-settings', 'decide:delegate'], JSON.stringify(pills));
assert.strictEqual(pills[1].enabled, false);
assert.ok(pills[1].why.includes('No decision grants the App the permission'), pills[1].why);
const htmlOut = stageTrackerLeadActionHtml(issue612, ctx, []);
assert.ok(/<button[^>]*class="ham-lead-act ham-act-primary"[^>]*data-ham-act="open:app-settings"[^>]*>Grant the App workflows: write<\/button>/.test(htmlOut), htmlOut);
assert.ok(/<button[^>]*class="ham-lead-act ham-act-secondary"[^>]*data-ham-act="decide:delegate"[^>]*aria-disabled="true"[^>]*title="No decision grants the App the permission[^"]*"[^>]*>Let hive decide<\/button>/.test(htmlOut), htmlOut);
// The Decide… submenu honours the server's reason too.
const del = stageTrackerDecideOptions(issue612, ctx).find(o => o.id === 'decide:delegate');
assert.strictEqual(del.enabled, false);
assert.ok(del.why.includes('No decision grants'), del.why);
// With the link gone, the next enabled, non-destructive option leads.
const noLink = Object.assign({}, issue612, { decision_needed: Object.assign({}, decision612, { options: decision612.options.filter(o => o.action !== 'open:app-settings') }) });
assert.strictEqual(stageTrackerLeadAction(noLink, ctx).id, 'blocker:recheck');
// A row whose server lets the hive decide keeps delegate as its lead.
assert.strictEqual(stageTrackerLeadAction(pr480, ctx).id, 'decide:delegate');
`)
}

func TestStageTrackerNeedsHumanCaptionSpacing(t *testing.T) {
	// derive_issue.go emits "blocked: <short>"; the generic replacer used to
	// double the space ("owed — blocked:  App lacks…").
	if got, want := humanizeStageReason("blocked: App lacks workflows permission"), "blocked: App lacks workflows permission"; got != want {
		t.Errorf("humanizeStageReason = %q, want %q", got, want)
	}
	if got := humanizeStageReason("blocked:  App lacks workflows permission"); strings.Contains(got, "  ") {
		t.Errorf("humanizeStageReason kept a double space: %q", got)
	}
}

func TestStageTrackerNeedsHumanWiredInPage(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`function stageTrackerNeedsHumanReveal(rowKey)`,
		`.ham-seg.done.ham-blocker-note { color: var(--amber); opacity: 1; }`,
		`.ham-needs-human-pill { cursor: pointer; font: inherit; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
