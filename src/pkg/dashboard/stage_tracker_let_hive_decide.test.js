// Owner, on hive's own tracker (hivecommons/hive#15773): "when i click
// 'let hive decide' - it takes a few seconds to open the dialog … can we
// have something - that is consistent with the rest of the activity
// tracker, that shows the button is working?"; "#15503 (a secondary issue
// 'under #15502', covered by PR #15759) shows the PR's 'Let hive decide'
// chip — why does the issue show the button at all?"; "when i see 'pr in
// progress' on the chevron - i have no idea when it needs a human". The
// helpers are lifted out of index.html so this runs against the shipped
// source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}
const fn = name => pick(new RegExp('function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n    \\}\\n'), name);
// A one-line function or declaration, e.g. "function f(x) { return x; }".
const line = (re, what) => pick(new RegExp('^ *' + re + '.*$', 'm'), what);

const src = [
  "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\"/g, '&quot;'); }",
  "const STAGE_TRACKER_TERMINAL = 110;",
  "const STAGE_TRACKER_ACTOR = { 15: 'human', 45: 'human', 85: 'human', 90: 'human', 50: 'hive' };",
  line('const STAGE_TRACKER_ACTOR_NONE = ', 'STAGE_TRACKER_ACTOR_NONE'),
  line('const STAGE_TRACKER_ASKING_CAPTION = ', 'STAGE_TRACKER_ASKING_CAPTION'),
  line('var _stageTrackerAsking = new Set\\(\\);', '_stageTrackerAsking'),
  line('function stageTrackerAsking\\(', 'stageTrackerAsking'),
  line('function stageTrackerItemKey\\(', 'stageTrackerItemKey'),
  line('function stageTrackerRefOf\\(', 'stageTrackerRefOf'),
  line('function stageTrackerDecisionTargetKey\\(', 'stageTrackerDecisionTargetKey'),
  line('const STAGE_TRACKER_HUMAN_REASON_MAX = ', 'STAGE_TRACKER_HUMAN_REASON_MAX'),
  line('const STAGE_TRACKER_HUMAN_REASON_FALLBACK = ', 'STAGE_TRACKER_HUMAN_REASON_FALLBACK'),
  line('const STAGE_TRACKER_HUMAN_WHY_SAID = ', 'STAGE_TRACKER_HUMAN_WHY_SAID'),
  fn('stageTrackerLeadBusyHtml'),
  fn('stageTrackerPendingKeys'),
  fn('stageTrackerAskingKeys'),
  fn('stageTrackerBeginAsking'),
  fn('stageTrackerEndAsking'),
  fn('stageTrackerDelegatePreview'),
  fn('stageTrackerLeadActionOnRow'),
  fn('stageTrackerLeadActionHtml'),
  fn('stageTrackerDecisionPendingChipHtml'),
  fn('stageTrackerLinkedDrawnHere'),
  fn('stageTrackerLinkedDrawnElsewhere'),
  fn('stageTrackerLinkedPROpen'),
  fn('stageTrackerCoveredDecisionPR'),
  fn('stageTrackerCoveredDecisionCaption'),
  fn('stageTrackerStageActor'),
  fn('stageTrackerActorKind'),
  fn('stageTrackerHumanReason'),
  fn('stageTrackerHumanReasonHtml'),
].join('\n');

// The browser pieces the lifted code reaches for, recorded.
const harness = `
const log = { pending: [], cleared: [], synced: 0, ran: [] };
var document = { getElementById: () => null };
function stageTrackerSetPending(keys, caption) { const p = { keys: keys.slice(), caption }; log.pending.push(p); return p; }
function stageTrackerClearPending(p) { log.cleared.push(p); }
function stageTrackerAskingSync() { log.synced++; }
function stageTrackerDecisionTarget(row) { return row && row.kind === 'issue' && row.target ? row.target : null; }
function stageTrackerSentinelPreface() { return ''; }
function showToast() {}
async function stageTrackerRunActions(items, id) { log.ran.push(id); }
let planGate = null;
function stageTrackerFetchDelegatePlan(item) { return new Promise((res, rej) => { planGate = err => (err ? rej(err) : res({ ref: stageTrackerRefOf(item), would: 'approve' })); }); }
let dialogSeen = null;
function stageTrackerDelegatePlanDialog(plans) { dialogSeen = { plans, asking: Array.from(_stageTrackerAsking) }; return Promise.resolve({ chosen: 'Let hive do that · previewed approve' }); }
let lead = null;
function stageTrackerLeadAction() { return lead; }
`;

const api = new Function(src + harness + `; return {
  get log() { return log; }, release: err => planGate && planGate(err), get dialogSeen() { return dialogSeen; }, setLead: a => { lead = a; },
  asking: stageTrackerAsking, askingSet: _stageTrackerAsking, caption: STAGE_TRACKER_ASKING_CAPTION,
  busyHtml: stageTrackerLeadBusyHtml, preview: stageTrackerDelegatePreview, leadOnRow: stageTrackerLeadActionOnRow, leadHtml: stageTrackerLeadActionHtml,
  coveredPR: stageTrackerCoveredDecisionPR, coveredCaption: stageTrackerCoveredDecisionCaption, chip: stageTrackerDecisionPendingChipHtml,
  humanReason: stageTrackerHumanReason, humanReasonHtml: stageTrackerHumanReasonHtml, max: STAGE_TRACKER_HUMAN_REASON_MAX,
};`)();

const repo = 'hivecommons/hive';
const pr15759 = { number: 15759, url: 'https://github.com/hivecommons/hive/pull/15759', title: 'Fix the tracker', author: 'hive', author_kind: 'hive', stage: 85, modifiers: ['needs-decision'],
  decision_needed: { question: 'Approve or send back?', options: [{ action: 'decide:delegate', label: 'Let hive decide', enabled: true }] },
  stage_actor: { kind: 'human', why: 'a human decision is owed' } };
const issue15502 = { repo, kind: 'issue', number: 15502, title: 'Primary', stage: 40, modifiers: [], linked_prs: [Object.assign({}, pr15759, { drawn_under: 15502 })], stage_actor: { kind: 'human', why: 'a human decision is owed' } };
const issue15503 = { repo, kind: 'issue', number: 15503, title: 'Secondary', stage: 85, mirrored: true, modifiers: [], linked_prs: [Object.assign({}, pr15759, { drawn_under: 15502 })], stage_actor: { kind: 'human', why: 'a human decision is owed' } };
const prItem = { repo, kind: 'pr', number: 15759, stage: 85, modifiers: ['needs-decision'] };
const delegateLead = { id: 'decide:delegate', label: 'Let hive decide', enabled: true, target: { repo, kind: 'pr', number: 15759 } };

test('Let hive decide reads "Asking the hive…" from the click until the preview opens', async () => {
  const row = Object.assign({}, issue15502, { target: prItem });
  const done = api.preview([prItem], [row]);
  // The plan is still being fetched: the PR and the clicked issue row ask.
  assert.ok(api.asking(prItem), 'the PR is marked asking at once');
  assert.ok(api.asking(row), 'the clicked row is marked asking at once');
  assert.strictEqual(api.log.pending.length, 1, 'the row gets the acting wash at once');
  assert.strictEqual(api.log.pending[0].caption, 'Asking the hive…');
  assert.deepStrictEqual(api.log.pending[0].keys.sort(), [repo + '/issue#15502', repo + '/pr#15759']);
  assert.ok(api.log.synced >= 1, 'the live buttons are swapped to busy in place');
  api.release();
  const got = await done;
  assert.deepStrictEqual(got, { chosen: 'Let hive do that · previewed approve' });
  assert.deepStrictEqual(api.dialogSeen.asking, [], 'busy ends before the preview opens');
  assert.strictEqual(api.log.cleared[0], api.log.pending[0], 'the acting wash is cleared');
  assert.strictEqual(api.askingSet.size, 0);
});

test('a failed plan fetch still clears the busy state', async () => {
  const before = api.log.cleared.length;
  const done = api.preview([prItem], [prItem]);
  assert.ok(api.asking(prItem));
  api.release(new Error('network error'));
  await assert.rejects(done, /network error/);
  assert.ok(!api.asking(prItem), 'the button comes back after a failure');
  assert.strictEqual(api.log.cleared.length, before + 1);
});

test('the busy button is disabled, aria-busy, and wears the acting glyph', () => {
  const h = api.busyHtml('decide:delegate', 'Let hive decide', 'hivecommons/hive#15759', false);
  assert.match(h, /^<button type="button" class="ham-lead-act ham-lead-busy" data-ham-act="decide:delegate"/);
  assert.match(h, / disabled /);
  assert.match(h, /aria-busy="true"/);
  assert.match(h, /aria-disabled="true"/);
  assert.match(h, /<span class="toast-spinner cue-acting-spin" aria-hidden="true"><\/span>Asking the hive…<\/button>$/);
  assert.match(h, /aria-label="Asking the hive… — hivecommons\/hive#15759"/);
  // A re-render while asking keeps the busy button; afterwards the plain one.
  api.setLead(Object.assign({}, delegateLead, { target: prItem }));
  api.askingSet.add(repo + '/pr#15759');
  assert.match(api.leadHtml(prItem, {}), /aria-busy="true"/);
  api.askingSet.clear();
  const plain = api.leadHtml(prItem, {});
  assert.ok(!plain.includes('aria-busy') && plain.includes('>Let hive decide</button>'), plain);
  // The client wires the click guard and the double-fire guard.
  assert.ok(html.includes("b.getAttribute('aria-busy') === 'true') return;"), 'the lead click ignores a busy button');
  assert.ok(html.includes("if (id === 'decide:delegate' && typeof stageTrackerAsking === 'function' && items.some(stageTrackerAsking)) return"), 'runRowAction never fires twice');
  assert.ok(html.includes('.ham-lead-act.ham-lead-busy[aria-busy="true"]'), 'the busy button is styled');
});

test('a covered issue drops its PR\'s button for a passive "decision pending on #N" chip', () => {
  api.setLead(delegateLead);
  // #15503 is covered by #15759, which is drawn under #15502.
  assert.strictEqual(api.leadOnRow(issue15503, {}, null), null, 'no Let hive decide on the covered issue');
  const chip = api.leadHtml(issue15503, {});
  assert.ok(!chip.includes('ham-lead-act'), chip);
  assert.match(chip, /^<button type="button" class="ham-badge ham-decision-pending" data-ham-goto="hivecommons\/hive\/pr#15759" data-ham-goto-row="hivecommons\/hive\/issue#15502"/);
  assert.match(chip, />decision pending on #15759<\/button>$/);
  assert.match(chip, /title="Covered by PR #15759 — waiting for a decision on the PR\. Its row \(under #15502\) carries the decision; click to go there\."/);
  assert.strictEqual(api.coveredCaption(issue15503), 'Covered by PR #15759 — waiting for a decision on the PR');
  // #15502 draws #15759 itself: its row keeps the button, no chip.
  assert.strictEqual(api.leadOnRow(issue15502, {}, null), delegateLead);
  assert.match(api.leadHtml(issue15502, {}), /class="ham-lead-act" data-ham-act="decide:delegate"/);
  assert.strictEqual(api.coveredPR(issue15502), null);
  assert.strictEqual(api.coveredCaption(issue15502), '');
  // An older payload without drawn_under draws the PR under every issue.
  const legacy = Object.assign({}, issue15503, { linked_prs: [Object.assign({}, pr15759)] });
  assert.strictEqual(api.leadOnRow(legacy, {}, null), delegateLead);
  // A covering PR drawn elsewhere that asks nothing: no chip.
  const quiet = Object.assign({}, issue15503, { linked_prs: [Object.assign({}, pr15759, { stage: 50, modifiers: [], decision_needed: null })] });
  assert.strictEqual(api.coveredPR(quiet), null);
  api.setLead(null);
  assert.strictEqual(api.leadHtml(quiet, {}), '');
  // The click scrolls to the PR's row; the caption is wired into the row.
  assert.ok(html.includes("e.target.closest('.ham-decision-pending[data-ham-goto]')"), 'the chip goes to the PR row');
  assert.ok(html.includes('stageTrackerCoveredDecisionCaption(item)'), 'the covered caption reaches the row');
});

test('every 👤 says why a person is wanted', () => {
  const fixtures = [
    issue15503,
    { repo, kind: 'pr', number: 1, stage: 90, human_merge_reason: 'workflow-files', human_merge_reason_text: "Hive won't merge it alone — it touches .github/workflows", stage_actor: { kind: 'human', why: 'a maintainer merges it' } },
    Object.assign({ repo, kind: 'pr' }, pr15759),
    { repo, kind: 'pr', number: 2, stage: 70, author: 'sam', stage_actor: { kind: 'human', login: 'sam', why: "@sam must push a fix; the hive does not edit a person's PR" } },
    { repo, kind: 'pr', number: 3, stage: 85, stage_actor: { kind: 'human', why: 'the hive is not permitted to merge into a protected release branch; a person must' } },
    { repo, kind: 'pr', number: 4, stage: 85, modifiers: ['needs-decision'] },
    { repo, kind: 'issue', number: 5, stage: 15, stage_actor: { kind: 'human', why: 'no maintainer has triaged it yet' } },
  ];
  for (const it of fixtures) {
    const r = api.humanReason(it);
    assert.ok(r, 'a human mover has a reason: ' + JSON.stringify(it));
    assert.ok(r.full && r.full.trim(), 'the hover is never empty: ' + JSON.stringify(it));
    assert.ok(r.short.length <= api.max, 'short reason fits: ' + r.short);
  }
  const [covered, merge, decision, fixing, long, bare, triage] = fixtures.map(api.humanReason);
  assert.deepStrictEqual(covered, { short: '#15759 needs a decision', full: 'Covered by PR #15759 — waiting for a decision on the PR' });
  assert.deepStrictEqual(merge, { short: 'workflow files', full: "Hive won't merge it alone — it touches .github/workflows" });
  // The chevron word already says it: no inline repeat, the question is the hover.
  assert.deepStrictEqual(decision, { short: '', full: 'Approve or send back?' });
  assert.deepStrictEqual(fixing, { short: '@sam must push a fix', full: "@sam must push a fix; the hive does not edit a person's PR" });
  assert.ok(long.short.endsWith('…') && long.short.length === api.max, long.short);
  assert.strictEqual(bare.full, 'a person must act');
  assert.strictEqual(triage.short, '');
  assert.strictEqual(api.humanReasonHtml(issue15503), '<span class="ham-human-why" title="Covered by PR #15759 — waiting for a decision on the PR"> · #15759 needs a decision</span>');
  assert.strictEqual(api.humanReasonHtml(fixtures[2]), '');
  // Not a person's move: no reason, no marker text.
  assert.strictEqual(api.humanReason({ repo, kind: 'pr', number: 6, stage: 50, stage_actor: { kind: 'hive', why: 'review swarm' } }), null);
  assert.strictEqual(api.humanReasonHtml({ repo, kind: 'pr', number: 6, stage: 50, stage_actor: { kind: 'hive', why: 'review swarm' } }), '');
  assert.ok(html.includes("if (active && !sg.cap && actorKind === 'human' && typeof stageTrackerHumanReasonHtml === 'function') inner += stageTrackerHumanReasonHtml(item);"), 'the lit chevron renders the reason after the 👤');
});
