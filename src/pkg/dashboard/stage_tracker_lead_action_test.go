package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Owner, 2026-10-29, on five "New · waiting for a maintainer's triage" rows:
// "i dont have any way to act on these." The answer existed — Accept triage
// led the ⋯ menu — but three dots are invisible. Every row whose next move
// is the viewer's now renders its lead action as a real button in the chip
// slot: the ⋯ menu's `lead` entry, else the first Decide… option, else
// Adopt; the same id, hint and enable/disable reason, run down the same
// runRowAction path. One button only; everything else stays in ⋯.

func TestStageTrackerLeadActionInline(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`class="ham-lead-act`,
		`e.target.closest('.ham-lead-act[data-ham-act]')`,
		`stageTrackerDecisionRun(item, b.dataset.hamAct)`,
		`stageTrackerChipsHtml(shown) + (typeof stageTrackerLeadButtonHtml === 'function' ? stageTrackerLeadButtonHtml(item, nowMs, subRows) : '')`,
		`stageTrackerChipsHtml(pr) + (typeof stageTrackerLeadButtonHtml === 'function' ? stageTrackerLeadButtonHtml(pr, nowMs) : '')`,
		`.ham-lead-act[aria-disabled="true"]`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const BT = String.fromCharCode(96);
const NOW = Date.parse('2026-10-29T18:00:00Z');
const ctx = { user: true, canHold: true, canHuman: true, login: 'octo', nowMs: NOW, governor: { nextKickAt: '2026-10-29T18:10:00Z' } };
const buttons = h => (h.match(/<button\b/g) || []).length;
const labelOf = h => { const m = h.match(/>([^<]*)<\/button>/); return m ? m[1] : ''; };

// The screenshot row: a 000-new issue the reporter-trust gate holds.
const fresh = { repo: 'o/r', kind: 'issue', number: 15437, title: 'Monitoring component', url: 'https://github.com/o/r/issues/15437', stage: 0, stage_slug: '000-new', actor: 'maintainer', modifiers: [], bucket: 'reporter_triage',
  caption: 'Triage needed — accept or close',
  new: { next_mover: 'maintainer', next_pass_at: '2026-10-29T18:10:00Z', gates: [{ name: 'triage', state: 'next' }, { name: 'actionable', state: 'later' }, { name: 'claimed', state: 'later' }] } };
let lead = stageTrackerLeadAction(fresh, ctx);
assert.strictEqual(lead.id, 'accept-triage');
let h = stageTrackerLeadActionHtml(fresh, ctx);
assert.strictEqual(buttons(h), 1, 'one inline button only: ' + h);
assert.strictEqual(labelOf(h), 'Accept triage');
assert.ok(h.startsWith('<button type="button" class="ham-lead-act" data-ham-act="accept-triage"'), h);
assert.ok(!h.includes('aria-disabled'), 'enabled for a writer');
assert.ok(h.includes('title="adds &#96;triage/accepted&#96; as @octo; the hive takes it on its next pass (≈10 min)"'), 'the menu entry\'s hint is the hover: ' + h);
assert.ok(h.includes('aria-label="Accept triage — o/r#15437"'), h);
// Same for 015 Triage needed.
assert.strictEqual(labelOf(stageTrackerLeadActionHtml({ ...fresh, stage: 15, stage_slug: '015-triage-needed', new: null }, ctx)), 'Accept triage');

// Needs human decision: an issue parked at 045 leads with Let hive decide,
// the first Decide… option, carrying the decide target.
const parked = { repo: 'o/r', kind: 'issue', number: 13, title: 'Agents stand down', url: 'https://github.com/o/r/issues/13', stage: 45, actor: 'maintainer', modifiers: ['needs-decision'] };
lead = stageTrackerLeadAction(parked, ctx);
assert.strictEqual(lead.id, 'decide:delegate');
assert.strictEqual(lead.target, parked);
h = stageTrackerLeadActionHtml(parked, ctx);
assert.strictEqual(buttons(h), 1);
assert.strictEqual(labelOf(h), 'Let hive decide');
// A PR at 085 with the server's answers leads with the server's lead option.
const human = { repo: 'o/r', kind: 'pr', number: 104, title: 'Hand-made', url: 'https://github.com/o/r/pull/104', stage: 85, actor: 'maintainer', modifiers: ['needs-decision'],
  decision_needed: { question: 'Merge it?', options: [{ action: 'decide:merge-now', label: 'Merge now', enabled: true, consequence: 'merges as you' }, { action: 'decide:delegate', label: 'Let hive decide', enabled: true }] } };
h = stageTrackerLeadActionHtml(human, ctx);
assert.strictEqual(buttons(h), 1);
assert.strictEqual(labelOf(h), 'Merge now', h);
assert.ok(h.includes('data-ham-act="decide:merge-now"'), h);
// Ready for human merge without a merger role: the merge option is greyed
// with its reason, never hidden.
h = stageTrackerLeadActionHtml(human, { ...ctx, canHuman: false });
assert.strictEqual(buttons(h), 1);
assert.ok(h.includes('aria-disabled="true"'), h);

// A returned contributor PR whose adoption is due leads with Adopt.
const returned = { repo: 'o/r', kind: 'pr', number: 101, title: 'Fix redirect', url: 'https://github.com/o/r/pull/101', stage: 72, actor: 'karibhou', modifiers: [], adoption: { due: true, action: 'decide:adopt' } };
if (typeof stageTrackerAdoptOffered === 'function') {
  lead = stageTrackerLeadAction(returned, ctx);
  assert.strictEqual(lead.id, 'decide:adopt');
  assert.strictEqual(labelOf(stageTrackerLeadActionHtml(returned, ctx)), 'Adopt…');
}

// One button per ask (owner: "there are 2 hive decide for each of these
// items... should only be 1"). An issue with several linked PRs draws one
// sub-row per PR; when its one parked PR asks (hive#15320 → #15647 at 085),
// the issue row's lead resolves to that PR too. The sub-row — the row with
// the Needs-human-decision chevron — carries the button; the parent renders
// none, while its ⋯ menu keeps "Decide PR #N…".
const asking = { number: 15647, url: 'https://github.com/o/r/pull/15647', title: 'Fix it', author: 'hive', author_kind: 'hive', stage: 85, modifiers: ['needs-decision'],
  decision_needed: { question: 'Approve or send back?', options: [{ action: 'decide:delegate', label: 'Let hive decide', enabled: true }, { action: 'decide:approve', label: 'Approve', enabled: true }, { action: 'hold', label: 'Hold', enabled: true }] } };
const closedPR = { number: 15600, url: 'https://github.com/o/r/pull/15600', title: 'First try', author: 'hive', author_kind: 'hive', stage: 50, modifiers: [], state: 'closed' };
const parent = { repo: 'o/r', kind: 'issue', number: 15320, title: 'Tracker lead chips', url: 'https://github.com/o/r/issues/15320', stage: 40, actor: 'maintainer', modifiers: [], linked_prs: [asking, closedPR] };
const sub = stageTrackerLinkedPRItem(parent, asking);
const parentLead = stageTrackerLeadAction(parent, ctx);
assert.strictEqual(parentLead.id, 'decide:delegate');
assert.strictEqual(parentLead.target.number, 15647, 'the parent row\'s lead acts on the nested PR');
assert.strictEqual(stageTrackerLeadAction(sub, ctx).id, 'decide:delegate');
// Without sub-rows (one covering PR, no nested line) the issue row keeps its button.
assert.strictEqual(buttons(stageTrackerLeadActionHtml(parent, ctx)), 1);
// With the PR drawn as a sub-row, the parent yields nothing and the pair renders exactly one.
assert.strictEqual(stageTrackerLeadActionOnRow(parent, ctx, parent.linked_prs), null, 'the sub-row carries the button');
const pairHtml = stageTrackerLeadActionHtml(parent, ctx, parent.linked_prs) + stageTrackerLeadActionHtml(sub, ctx);
assert.strictEqual(buttons(pairHtml), 1, 'one Let hive decide per item: ' + pairHtml);
assert.ok(pairHtml.includes('aria-label="Let hive decide — o/r#15647"'), pairHtml);
// The parent's ⋯ menu still offers Decide PR #N….
const parentMenu = stageTrackerRowActions(parent, ctx).find(a => a.id === 'decide');
assert.ok(parentMenu && parentMenu.label === 'Decide PR #15647…', JSON.stringify(parentMenu));
// A lead that is the issue's own (045 parked issue, sub-rows that ask nothing) stays on the parent.
const ownAsk = { ...parked, linked_prs: [{ ...closedPR, number: 15601 }, { ...closedPR, number: 15602 }] };
assert.strictEqual(stageTrackerLeadActionOnRow(ownAsk, ctx, ownAsk.linked_prs).id, 'decide:delegate');

// Rows that wait on nobody here render no button.
for (const it of [
  { repo: 'o/r', kind: 'issue', number: 8, title: 'Flaky test', url: 'https://github.com/o/r/issues/8', stage: 20, actor: 'hive', modifiers: [] },
  { repo: 'o/r', kind: 'pr', number: 102, title: 'Stabilise', url: 'https://github.com/o/r/pull/102', stage: 95, actor: 'hive', modifiers: [] },
  { ...fresh, new: { ...fresh.new, next_mover: 'hive' }, actor: 'hive' },
  { repo: 'o/r', kind: 'pr', number: 101, title: 'Fix redirect', url: 'https://github.com/o/r/pull/101', stage: 72, actor: 'karibhou', modifiers: [] },
]) {
  assert.strictEqual(stageTrackerLeadAction(it, ctx), null, JSON.stringify(it));
  assert.strictEqual(stageTrackerLeadActionHtml(it, ctx), '', JSON.stringify(it));
}

// Without write access the button stays, disabled, with the sign-in reason
// as its hover — the menu entry's rule.
h = stageTrackerLeadActionHtml(fresh, { user: false, canHold: false, canHuman: false, nowMs: NOW });
assert.strictEqual(buttons(h), 1);
assert.ok(h.includes('aria-disabled="true"'), h);
assert.ok(h.includes('title="' + STAGE_TRACKER_WHY_WRITE + '"'), h);
h = stageTrackerLeadActionHtml(parked, { user: false, canHold: false, canHuman: false, nowMs: NOW });
assert.ok(h.includes('aria-disabled="true"') && h.includes(STAGE_TRACKER_WHY_WRITE), h);
// A signed-in viewer whose hold permission is still loading (null) is not
// locked out: the server answers 403 either way.
assert.ok(!stageTrackerLeadActionHtml(fresh, { user: true, canHold: null, nowMs: NOW }).includes('aria-disabled'));

// Labels and hints are escaped.
h = stageTrackerLeadActionHtml({ ...human, decision_needed: { question: 'Merge it?', options: [{ action: 'decide:merge-now', label: 'Merge <b>now</b>', enabled: true, consequence: 'a "quoted" hint' }] } }, ctx);
assert.ok(h.includes('&lt;b&gt;') && h.includes('&quot;quoted&quot;'), h);

// The dev warning only speaks on a development host.
let warned = [];
global.console = { warn: m => warned.push(m) };
global.window = { location: { hostname: 'hive.example.org' } };
stageTrackerDevWarn('x');
assert.deepStrictEqual(warned, []);
global.window = { location: { hostname: 'localhost' } };
stageTrackerDevWarn('x');
assert.deepStrictEqual(warned, ['[tracker] x']);
`)
}

// Every row the server marks as the maintainer's move (actor "maintainer"
// or new.next_mover "maintainer") must yield an inline lead action: a row
// that tells the operator what is needed and offers no control is the bug
// this fixes. The rows come from the real pipeline (buildStageTracker over
// the shared fixture plus a 015 issue, a 090 PR and a reporter-trust hold),
// marshalled as the SPA receives them.
func TestStageTrackerMaintainerRowsHaveLeadAction(t *testing.T) {
	now := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	store.Observe(stagelabels.Key("acme/console", 15), stagelabels.TriageNeeded, "reporter-trust gate")
	store.Observe(stagelabels.Key("acme/console", 120), stagelabels.ReadyForHumanMerge, "untrusted-author-role")
	opened := now.Add(-28 * time.Minute)
	s.statusMu.Lock()
	s.status.Governor.NextKickAt = now.Add(10 * time.Minute).UTC().Format(time.RFC3339)
	s.status.Repos[0].ActionableIssues = append(s.status.Repos[0].ActionableIssues,
		github.Issue{Number: 15, Title: "Needs a maintainer's triage", Author: "onkar717", AuthorIsHuman: true, Labels: []string{"stage/015-triage-needed", "needs-triage"}})
	s.status.Repos[0].NonActionableIssues = append(s.status.Repos[0].NonActionableIssues,
		github.RepoWorkIssue{Repo: "acme/console", Number: 15437, Title: "Monitoring component", Author: "onkar717", AuthorIsHuman: true, CreatedAt: opened, UpdatedAt: opened, Bucket: "reporter_triage", Reason: "Awaiting maintainer triage label"})
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 120, Title: "Shard nightly", Author: "odttlnt", Labels: []string{"stage/090-ready-for-human-merge", "lgtm", "approved"}}})
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	var rows []StageTrackerItem
	for _, it := range resp.Items {
		if it.Actor == StageTrackerMoverMaintainer || (it.New != nil && it.New.NextMover == StageTrackerMoverMaintainer) {
			rows = append(rows, it)
		}
	}
	want := map[string]bool{"issue#13": false, "issue#15": false, "issue#15437": false, "pr#104": false, "pr#120": false}
	for _, it := range rows {
		want[it.Kind+"#"+itoa(it.Number)] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("fixture row %s is not marked as the maintainer's move; the test lost a case", k)
		}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const rows = `+string(raw)+`;
const ctx = { user: true, canHold: true, canHuman: true, login: 'octo', nowMs: `+itoa(int(now.UnixMilli()))+`, governor: null };
assert.ok(rows.length >= 5, 'fixture rows: ' + rows.length);
const missing = [];
for (const it of rows) {
  const lead = stageTrackerLeadAction(it, ctx);
  if (!lead || !lead.enabled) missing.push(it.kind + '#' + it.number + ' (' + it.caption + ') → ' + JSON.stringify(lead));
  const h = stageTrackerLeadActionHtml(it, ctx);
  assert.strictEqual((h.match(/<button\b/g) || []).length, 1, 'one button on ' + it.kind + '#' + it.number + ': ' + h);
}
assert.deepStrictEqual(missing, [], 'maintainer-moved rows without an enabled lead action');
`)
}
