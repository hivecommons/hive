// Covered issues and the 👤 marker's reason (hivecommons/hive#15773).
// Owner, on #15503 ("under #15502", covered by PR #15759 parked at 085):
// why does the issue show "Let hive decide" at all? And: "when i see 'pr
// in progress' on the chevron - i have no idea when it needs a human at
// that stage". So: an issue whose covering PR waits on a decision draws a
// passive, muted "decision pending on #N" chip that scrolls to the PR row
// instead of the PR's actionable chip; and every chevron with the human
// marker names what the person is wanted for after a middot, with the
// full question in the tooltip. The functions are lifted out of
// index.html so this runs the shipped source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function fn(name) {
  const start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found in index.html');
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  assert.fail('unterminated ' + name);
}

const stubs = `
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
let leadAction = null;
function stageTrackerLeadAction(item, ctx) { return typeof leadAction === 'function' ? leadAction(item, ctx) : leadAction; }
function stageTrackerRefOf(item) { return '#' + item.number; }
function stageTrackerLinkedPROpen() { return true; }
const STAGE_TRACKER_ACTOR_NONE = 'none';
function stageTrackerStageActor(item) { return item.stage_actor || null; }
`;
const src = [
  fn('stageTrackerItemKey'),
  fn('stageTrackerActorKind'),
  fn('stageTrackerHumanReasonOf'),
  fn('stageTrackerHumanReasonHtml'),
  fn('stageTrackerLinkedPRItem'),
  fn('stageTrackerDecisionPendingOn'),
  fn('stageTrackerDecisionPendingChipHtml'),
  fn('stageTrackerLeadDrawnElsewhere'),
  fn('stageTrackerLeadActionOnRow'),
  fn('stageTrackerLeadActionHtml'),
].join('\n');
const T = new Function(stubs + src + `
return { setLead: f => { leadAction = f; }, onRow: stageTrackerLeadActionOnRow, html: stageTrackerLeadActionHtml, chip: stageTrackerDecisionPendingChipHtml,
  reasonOf: stageTrackerHumanReasonOf, reasonHtml: stageTrackerHumanReasonHtml, linked: stageTrackerLinkedPRItem };`)();

const decide = { id: 'decide:delegate', label: 'Let hive decide', enabled: true, hint: 'hive answers the reviewer', target: { kind: 'pr', repo: 'acme/docs', number: 15759 } };
const pr = { kind: 'pr', repo: 'acme/docs', number: 15759, stage: 85, stage_actor: { kind: 'human', why: 'a maintainer must decide' }, human_reason: "reviewer's question" };
const primary = { kind: 'issue', repo: 'acme/docs', number: 15502, stage: 40, decision_pending_on: 15759, stage_actor: { kind: 'human' }, human_reason: '#15759 needs a decision',
  linked_prs: [{ number: 15759, drawn_under: 15502, stage: 85, stage_actor: { kind: 'human' }, human_reason: "reviewer's question" }] };
const secondary = Object.assign({}, primary, { number: 15503 });

test('a covered issue draws the passive "decision pending on #N" chip, not the PR\'s actionable one', () => {
  T.setLead(() => decide);
  const ctx = { user: true };
  for (const issue of [primary, secondary]) {
    assert.strictEqual(T.onRow(issue, ctx, issue.linked_prs), null, '#' + issue.number + ' must not lead with the PR\'s decision');
    const out = T.html(issue, ctx, issue.linked_prs);
    assert.ok(!out.includes('ham-lead-act') && !out.includes('>Let hive decide<'), out);
    assert.ok(out.includes('class="ham-chip ham-chip-passive"'), out);
    assert.ok(out.includes('>decision pending on #15759<'), out);
    assert.ok(out.includes('data-action="hamBannerShowRow" data-arg0="acme/docs/pr#15759"'), 'the chip scrolls to the PR row: ' + out);
    assert.ok(/title="PR #15759 is parked on a decision/.test(out), out);
  }
  // The PR row itself keeps the button.
  const prOut = T.html(pr, ctx);
  assert.ok(prOut.includes('ham-lead-act') && prOut.includes('data-ham-act="decide:delegate"') && prOut.includes('>Let hive decide<'), prOut);
});

test('without the server flag a secondary issue still never offers the decision of a PR drawn under the primary', () => {
  T.setLead(() => decide);
  const issue = { kind: 'issue', repo: 'acme/docs', number: 15503, stage: 40, linked_prs: [{ number: 15759, drawn_under: 15502, stage: 85 }] };
  assert.strictEqual(T.onRow(issue, { user: true }, []), null);
  assert.strictEqual(T.html(issue, { user: true }, []), '', 'no flag, no chip: the sub-row under the primary issue carries the button');
  // The primary (the PR drawn under it, no sub-rows passed) keeps the existing contract.
  const prime = Object.assign({}, issue, { number: 15502 });
  assert.ok(T.onRow(prime, { user: true }), 'the issue the PR is drawn under keeps its lead when no sub-rows are rendered');
  // An issue's own decision (045, target is the issue) still leads.
  const own = { kind: 'issue', repo: 'acme/docs', number: 15600, stage: 45 };
  T.setLead(() => Object.assign({}, decide, { target: { kind: 'issue', number: 15600 } }));
  assert.ok(T.onRow(own, { user: true }), 'an issue decides its own 045 question');
  // A non-decision lead whose target is a PR (e.g. merge) is untouched.
  T.setLead(() => ({ id: 'merge', label: 'Merge', enabled: true, target: { kind: 'pr', number: 15759 } }));
  assert.ok(T.onRow(issue, { user: true }, []), 'only decide:* leads are suppressed');
  // No lead at all and no flag → nothing.
  T.setLead(() => null);
  assert.strictEqual(T.html(issue, { user: true }), '');
});

test('the human marker always carries its reason after a middot; the word is not repeated', () => {
  assert.strictEqual(T.reasonOf(primary), '#15759 needs a decision');
  assert.strictEqual(T.reasonOf(pr), "reviewer's question");
  assert.strictEqual(T.reasonOf({ kind: 'pr', stage: 70, stage_actor: { kind: 'hive' }, human_reason: 'stale' }), '', 'a hive actor carries no human reason');
  assert.strictEqual(T.reasonOf({ kind: 'pr', stage: 85, stage_actor: { kind: 'human' } }), '', 'no server reason, no clause');
  assert.strictEqual(T.reasonHtml('#15759 needs a decision', 'PR in progress'), '<span class="ham-human-reason"> · #15759 needs a decision</span>');
  assert.strictEqual(T.reasonHtml('read & close', 'Advisory · read & close'), '', 'a word that already says it adds nothing');
  assert.strictEqual(T.reasonHtml('<b>x</b>', 'w'), '<span class="ham-human-reason"> · &lt;b&gt;x&lt;/b&gt;</span>', 'escaped');
  assert.strictEqual(T.reasonHtml('', 'w'), '');
  // The nested PR sub-row carries the PR's own reason.
  const sub = T.linked(primary, primary.linked_prs[0]);
  assert.strictEqual(sub.human_reason, "reviewer's question");
  assert.strictEqual(T.reasonOf(sub), "reviewer's question");
});
