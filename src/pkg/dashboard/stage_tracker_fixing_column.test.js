// The chevron column follows the CURRENT state, never the highest stage
// a PR once reached. Hive's own #15574 (its adopted copy of #15526 under
// issue #15511) had been queued, hit a conflict on its merge attempt and
// fell back to 070 Fixing — its caption said "fell back to Fixing —
// conflicting", its chevron said "Fixing", and yet it sat in the Merge
// queue column: the stale merge-failed gate (and, before #15616, the
// planner's merge_train) still promoted the column. The maintainer: "why
// is this in merge queue if it is being fixed?"
//
// stageTrackerUnderRepair is the one predicate (mirroring Go's
// stageTrackerTrainUnderRepair, which seats or drops train members) that
// stageTrackerAnchor consults before any gate / train promotion, so the
// column and train membership can never disagree. The functions are
// lifted out of index.html so this runs against the shipped source.
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

const src = [
  pick(/const STAGE_TRACKER_PATH = [^\n]*\n/, 'STAGE_TRACKER_PATH'),
  pick(/const STAGE_TRACKER_PR_COLUMN = \d+;/, 'STAGE_TRACKER_PR_COLUMN'),
  pick(/const STAGE_TRACKER_MERGE_COLUMN = \d+;/, 'STAGE_TRACKER_MERGE_COLUMN'),
  pick(/const STAGE_TRACKER_NEEDS_DECISION = \d+;/, 'STAGE_TRACKER_NEEDS_DECISION'),
  pick(/const STAGE_TRACKER_ISSUE_NEEDS_DECISION = \d+;/, 'STAGE_TRACKER_ISSUE_NEEDS_DECISION'),
  pick(/const STAGE_TRACKER_ISSUE_DECISION_COLUMN = \d+;/, 'STAGE_TRACKER_ISSUE_DECISION_COLUMN'),
  pick(/const STAGE_TRACKER_TRIAGE_NEEDED = \d+;/, 'STAGE_TRACKER_TRIAGE_NEEDED'),
  pick(/const STAGE_TRACKER_POSSIBLY_EXISTS = \d+;/, 'STAGE_TRACKER_POSSIBLY_EXISTS'),
  pick(/const STAGE_TRACKER_PRE_MERGE_SOAK = \d+;/, 'STAGE_TRACKER_PRE_MERGE_SOAK'),
  pick(/const STAGE_TRACKER_TERMINAL = \d+;/, 'STAGE_TRACKER_TERMINAL'),
  pick(/const STAGE_TRACKER_REPAIR_STAGE_WHY = [^\n]*\n/, 'STAGE_TRACKER_REPAIR_STAGE_WHY'),
  pick(/function stageTrackerUnderRepair\(stage, reason\) \{[\s\S]*?\n    \}\n/, 'stageTrackerUnderRepair'),
  pick(/function stageTrackerAnchor\(stage, stages, kind, item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAnchor'),
].join('\n');
const lifted = new Function(src + '; return { anchor: stageTrackerAnchor, underRepair: stageTrackerUnderRepair };')();

const STAGES = [
  { number: 10 }, { number: 15 }, { number: 17 }, { number: 20 }, { number: 30 }, { number: 40 }, { number: 45 },
  { number: 50 }, { number: 60 },
  { number: 70, branch_from: 50 }, { number: 71, branch_from: 50 }, { number: 72, branch_from: 50 }, { number: 74, branch_from: 50 },
  { number: 80 }, { number: 85, branch_from: 80 }, { number: 90, branch_from: 80 }, { number: 95 }, { number: 100 },
];
const COLUMN = { OPEN: 0, ACTIONABLE: 10, CLAIMED: 20, PR: 30, REVIEW: 50, MERGE_GATE: 80, DONE: 110 };

// Hive's own #15574 as the roll-9 spoke served it: adopted copy, once
// queued (gate still "merge-failed" from the conflicting attempt, train
// still on the record from the planner), now 070 Fixing · conflicting.
const pr15574 = (extra) => Object.assign({
  repo: 'hivecommons/hive', number: 15574, kind: 'pr', stage: 70, reason: 'conflicting', modifiers: [],
  caption: 'fell back to Fixing — Red or changes requested; a hive agent is fixing it — conflicting',
  adopted_from: { number: 15526, author: 'odttlnt' },
  merge_gate: { state: 'merge-failed', reason: 'conflicting', merge_failed: 'PUT …/merge: 405 Head branch is out of date' },
  merge_train: { id: 'v5#15574', state: 'queued-behind', front: 15560, position: 2, size: 2 },
}, extra || {});

test('#15574: a Fixing PR with a stale merge-failed gate and a planner train lights CI / Review', () => {
  assert.strictEqual(lifted.anchor(70, STAGES, 'pr', pr15574()), COLUMN.REVIEW, 'gate + train');
  assert.strictEqual(lifted.anchor(70, STAGES, 'pr', pr15574({ merge_train: null })), COLUMN.REVIEW, 'merge-failed gate alone');
  assert.strictEqual(lifted.anchor(70, STAGES, 'pr', pr15574({ merge_gate: { state: 'merging' } })), COLUMN.REVIEW, 'merging gate alone');
  assert.strictEqual(lifted.anchor(70, STAGES, 'pr', pr15574({ merge_gate: null })), COLUMN.REVIEW, 'planner train alone (pre-#15616 server)');
  assert.strictEqual(lifted.anchor(70, STAGES, 'pr', pr15574({ merge_gate: null, merge_train: null, left_train: { why: 'fixing' } })), COLUMN.REVIEW, 'post-#15616 server');
});

test('stageTrackerUnderRepair mirrors the Go train rule', () => {
  assert.strictEqual(lifted.underRepair(70, ''), 'fixing');
  assert.strictEqual(lifted.underRepair(71, ''), 'needs-author');
  assert.strictEqual(lifted.underRepair(72, ''), 'returned-to-contributor');
  assert.strictEqual(lifted.underRepair(74, ''), 'adopted-by-hive');
  assert.strictEqual(lifted.underRepair(85, ''), 'needs-decision');
  assert.strictEqual(lifted.underRepair(50, 'required-check-failing:build-and-test'), 'checks-failing');
  assert.strictEqual(lifted.underRepair(60, 'check-failing:lint'), 'checks-failing');
  assert.strictEqual(lifted.underRepair(60, 'CI-RED'), 'checks-failing');
  for (const [stage, reason] of [[50, ''], [50, 'required-check-pending:build-and-test'], [60, 'conflicting'], [80, ''], [100, ''], [110, '']]) {
    assert.strictEqual(lifted.underRepair(stage, reason), '', 'stage ' + stage + ' reason ' + JSON.stringify(reason));
  }
  assert.strictEqual(lifted.underRepair(undefined, undefined), '');
});

// Ratchet: every PR stage × every gate state × every train state → the
// column the current state earns. Under repair → CI / Review always;
// a live merge attempt or a seated train past 030 → Merge queue; merged
// → Merged; otherwise the stage's own column.
const GATE_STATES = [null, 'gate-pending', 'ready', 'soaking', 'merging', 'merge-failed', 'duplicate-of:15560'];
const TRAIN_STATES = [null, 'queued-behind', 'rebasing', 'rebase-exhausted', 'front'];
const STAGE_COLUMN = {
  30: COLUMN.PR, 40: COLUMN.PR,
  50: COLUMN.REVIEW, 60: COLUMN.REVIEW, 70: COLUMN.REVIEW, 71: COLUMN.REVIEW, 72: COLUMN.REVIEW, 74: COLUMN.REVIEW,
  80: COLUMN.MERGE_GATE, 85: COLUMN.MERGE_GATE, 90: COLUMN.MERGE_GATE, 95: COLUMN.MERGE_GATE, 100: COLUMN.MERGE_GATE,
  110: COLUMN.DONE, 120: COLUMN.DONE,
};
const REPAIR_STAGES = [70, 71, 72, 74];

function expectedColumn(stage, gate, train, reason) {
  if (stage >= COLUMN.DONE) return COLUMN.DONE;
  // 085 is the decision park: it rides no train but the person decides at the gate.
  if (stage > COLUMN.PR && stage !== 85 && (REPAIR_STAGES.indexOf(stage) >= 0 || /^(required-)?check-failing:|^ci-red$/.test(reason))) return COLUMN.REVIEW;
  if (stage > COLUMN.PR && (gate === 'merging' || gate === 'merge-failed')) return COLUMN.MERGE_GATE;
  if (stage > COLUMN.PR && train) return COLUMN.MERGE_GATE;
  return STAGE_COLUMN[stage];
}

test('ratchet: every state lands in the column its current state earns', () => {
  let cases = 0;
  for (const stage of Object.keys(STAGE_COLUMN).map(Number)) for (const gate of GATE_STATES) for (const train of TRAIN_STATES) for (const reason of ['', 'conflicting', 'required-check-failing:build-and-test']) {
    const item = { repo: 'hivecommons/hive', number: 1, kind: 'pr', stage, reason, modifiers: [],
      merge_gate: gate ? { state: gate } : null,
      merge_train: train ? { id: 'v5#2', state: train, front: 2, position: 2, size: 2 } : null };
    const want = expectedColumn(stage, gate, train, reason);
    const label = 'stage ' + stage + ' gate ' + gate + ' train ' + train + ' reason ' + JSON.stringify(reason);
    assert.strictEqual(lifted.anchor(stage, STAGES, 'pr', item), want, label);
    // A row under repair never lands past CI / Review, whatever else it carries.
    if (lifted.underRepair(stage, reason) && stage > COLUMN.PR && stage !== 85 && stage < COLUMN.DONE) assert.ok(lifted.anchor(stage, STAGES, 'pr', item) <= COLUMN.REVIEW, label + ' promoted while under repair');
    cases++;
  }
  assert.ok(cases >= 15 * GATE_STATES.length * TRAIN_STATES.length * 3, 'ratchet covered ' + cases + ' cases');
});

test('hivecommons/docs#512 keeps its column: a merge outcome at 050/060 with nothing to repair is a Merge queue matter', () => {
  const row = (stage) => ({ repo: 'hivecommons/docs', number: 512, kind: 'pr', stage, reason: 'check-pending', modifiers: [], merge_gate: { state: 'merge-failed', reason: 'check-pending' } });
  assert.strictEqual(lifted.anchor(60, STAGES, 'pr', row(60)), COLUMN.MERGE_GATE);
  assert.strictEqual(lifted.anchor(50, STAGES, 'pr', row(50)), COLUMN.MERGE_GATE);
  assert.strictEqual(lifted.anchor(30, STAGES, 'pr', row(30)), COLUMN.PR, 'a stale gate never jumps a PR back in progress forward');
});

// hive's own #15654: an issue filed seconds ago (000 New, no label yet)
// whose nested PR #15655 is already open. The strip draws the row via
// the PR ("PR in progress · @clubanderson"), so the list order must ride
// the same live work — not the parent's unlabelled stage, which sank the
// row to the bottom among the New rows. stageTrackerLead is the one rule
// the strip (stageTrackerLeadStage) and the sort key share.
const leadSrc = [
  pick(/const STAGE_TRACKER_COVERED_BY_PR = \d+;/, 'STAGE_TRACKER_COVERED_BY_PR'),
  pick(/function stageTrackerLinkedDrawnHere\(lp, item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerLinkedDrawnHere'),
  // One covering PR per case: the tie-breakers never run, so stubs do.
  'function stageTrackerLinkedSupersededGone(lp) { return String(lp && lp.state || "") === "closed"; }',
  'function stageTrackerAttentionTier() { return 0; }',
  pick(/function stageTrackerLinkedSorted\(item, stages\) \{[\s\S]*?\n    \}\n/, 'stageTrackerLinkedSorted'),
  pick(/function stageTrackerLeadStage\(lp\) \{[\s\S]*?\n    \}\n/, 'stageTrackerLeadStage'),
  pick(/function stageTrackerLead\(item, stages\) \{[\s\S]*?\n    \}\n/, 'stageTrackerLead'),
  pick(/function stageTrackerSortKey\(item, stages\) \{[\s\S]*?\n    \}\n/, 'stageTrackerSortKey'),
].join('\n');

test('#15654: a New issue whose nested PR is open sorts and anchors by the PR, not the unlabelled parent', () => {
  let l;
  try {
    l = new Function(src + '\n' + leadSrc + '; return { lead: stageTrackerLead, leadStage: stageTrackerLeadStage, sortKey: stageTrackerSortKey, anchor: stageTrackerAnchor };')();
  } catch (e) {
    assert.fail('lifting the lead helpers: ' + e.message);
  }
  const issue = { repo: 'hivecommons/hive', number: 15654, kind: 'issue', stage: 0, stage_since: '', created_at: '2026-10-30T23:59:50Z',
    caption: 'Covered by PR #15655 — waiting for the stage-label pass', modifiers: [],
    linked_prs: [{ number: 15655, title: 'Fix', author: 'clubanderson', author_kind: 'human', stage: 0, stage_since: '' }] };
  const lead = l.lead(issue, STAGES);
  assert.strictEqual(lead.kind, 'pr');
  assert.strictEqual(lead.lead.number, 15655);
  assert.strictEqual(lead.stage, 40, 'an open PR with no stage yet drives the strip at 040 covered-by-PR');
  assert.strictEqual(l.leadStage(issue.linked_prs[0]), 40);
  assert.strictEqual(l.leadStage({ stage: 50 }), 50, 'a staged PR keeps its own stage');
  const key = l.sortKey(issue, STAGES);
  assert.strictEqual(key.anchor, COLUMN.PR, 'list place = the PR column, the same the strip lights');
  assert.ok(key.anchor > l.sortKey({ kind: 'issue', stage: 0, linked_prs: [] }, STAGES).anchor, 'sorts above a New issue with no PR');
  // The strip and the sort key never disagree: whatever the nested PR's stage,
  // the sort anchor equals the anchor of the stage the strip draws.
  for (const stage of [0, 30, 40, 50, 60, 70, 80, 90, 110]) {
    const it = Object.assign({}, issue, { linked_prs: [Object.assign({}, issue.linked_prs[0], { stage })] });
    const ld = l.lead(it, STAGES);
    assert.strictEqual(l.sortKey(it, STAGES).anchor, l.anchor(ld.stage, STAGES, 'pr', ld.lead), 'nested PR stage ' + stage);
  }
  // No covering PR: the issue is its own lead.
  const bare = l.lead({ kind: 'issue', stage: 10, stage_since: '2026-10-30T00:00:00Z' }, STAGES);
  assert.strictEqual(bare.kind, 'issue');
  assert.strictEqual(bare.stage, 10);
});
