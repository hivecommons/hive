// Stage → column mapping of the Hive activity monitor and the detour word
// for an adopted original (hivecommons/hive#14975 → #14980, #14947 →
// #14977). The owner's rule: a PR that needed adoption goes back to the
// CI/Review column as adopted; a merge-gate decision whose outcome is
// rework leaves the merge gate; close-superseded / wontfix is the closed
// column. The functions are lifted straight out of index.html so this test
// runs against the shipped source; the branch_from table mirrors
// stageTrackerBranchFrom in stage_tracker.go.
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
  pick(/const STAGE_TRACKER_ADOPTED_BY_HIVE = \d+;/, 'STAGE_TRACKER_ADOPTED_BY_HIVE'),
  pick(/function stageTrackerAdoptedWord\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAdoptedWord'),
  pick(/function stageTrackerAnchor\(stage, stages, kind, item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAnchor'),
].join('\n');
const lifted = new Function(src + '; return { anchor: stageTrackerAnchor, adoptedWord: stageTrackerAdoptedWord, ADOPTED: STAGE_TRACKER_ADOPTED_BY_HIVE };')();

// The /api/stage-tracker stages payload: number + branch_from, as
// stageTrackerBranchFrom renders it (07x → 050, 085/090 → 080).
const STAGES = [
  { number: 10 }, { number: 15 }, { number: 17 }, { number: 20 }, { number: 30 }, { number: 40 }, { number: 45 },
  { number: 50 }, { number: 60 },
  { number: 70, branch_from: 50 }, { number: 71, branch_from: 50 }, { number: 72, branch_from: 50 }, { number: 74, branch_from: 50 },
  { number: 80 }, { number: 85, branch_from: 80 }, { number: 90, branch_from: 80 }, { number: 95 }, { number: 100 },
];

const COLUMN = { OPEN: 0, ACTIONABLE: 10, CLAIMED: 20, PR: 30, REVIEW: 50, MERGE_GATE: 80, DONE: 110 };

test('074 adopted-by-hive renders in the CI/Review column, not the merge gate', () => {
  assert.strictEqual(lifted.ADOPTED, 74);
  assert.strictEqual(lifted.anchor(74, STAGES, 'pr'), COLUMN.REVIEW);
});

test('every PR stage lands in its owner-rule column', () => {
  const want = {
    30: COLUMN.PR, 40: COLUMN.PR,                 // in progress / covered
    50: COLUMN.REVIEW, 60: COLUMN.REVIEW, 70: COLUMN.REVIEW, 71: COLUMN.REVIEW, 72: COLUMN.REVIEW, 74: COLUMN.REVIEW, // 060 reviewed waits on CI/soak, not the gate (#15185)
    80: COLUMN.MERGE_GATE, 85: COLUMN.MERGE_GATE, 90: COLUMN.MERGE_GATE, 95: COLUMN.MERGE_GATE, 100: COLUMN.MERGE_GATE,
    110: COLUMN.DONE, 120: COLUMN.DONE,           // merged / closed (superseded, wontfix)
  };
  for (const [stage, col] of Object.entries(want)) {
    assert.strictEqual(lifted.anchor(Number(stage), STAGES, 'pr'), col, 'PR stage ' + stage);
  }
});

test('issue parks light the actionable column, a PR park the merge gate', () => {
  assert.strictEqual(lifted.anchor(45, STAGES, 'issue'), COLUMN.ACTIONABLE);
  assert.strictEqual(lifted.anchor(85, STAGES, 'issue'), COLUMN.ACTIONABLE);
  assert.strictEqual(lifted.anchor(85, STAGES, 'pr'), COLUMN.MERGE_GATE);
  assert.strictEqual(lifted.anchor(15, STAGES, 'issue'), COLUMN.ACTIONABLE);
  assert.strictEqual(lifted.anchor(17, STAGES, 'issue'), COLUMN.ACTIONABLE);
});

// Rework outcomes of a merge-gate decision re-derive to an earlier stage;
// the column follows the stage, so none of them stays in the merge gate.
test('rework re-derivations leave the merge gate; close goes to the closed column', () => {
  const reworkOutcomes = {
    'adopt-supersede → 074': 74,
    'send-back / changes requested → 070 fixing': 70,
    'give-direction (new head needed) → 050 review-pending': 50,
    'returned to contributor → 072': 72,
    'needs author → 071': 71,
  };
  for (const [name, stage] of Object.entries(reworkOutcomes)) {
    assert.notStrictEqual(lifted.anchor(stage, STAGES, 'pr'), COLUMN.MERGE_GATE, name);
  }
  assert.strictEqual(lifted.anchor(110, STAGES, 'pr'), COLUMN.DONE, 'close-superseded / wontfix');
});

test('adopted word names the superseding PR and is empty otherwise', () => {
  assert.strictEqual(lifted.adoptedWord({ stage: 74, adopted_by: 14980 }), 'Adopted → #14980');
  assert.strictEqual(lifted.adoptedWord({ stage: 74, adopted_by: '14977' }), 'Adopted → #14977');
  assert.strictEqual(lifted.adoptedWord({ stage: 74 }), '');
  assert.strictEqual(lifted.adoptedWord({ stage: 74, adopted_by: 0 }), '');
  assert.strictEqual(lifted.adoptedWord(null), '');
});

// hivecommons/hive#15052: a copy closed unmerged says so and that the hive
// is re-adopting; the loop guard names the park. The 085 row carrying
// either state uses the adopted word, not "Needs human decision".
test('adopted word qualifies a closed copy and the loop-guard park', () => {
  assert.strictEqual(lifted.adoptedWord({ stage: 74, adopted_by: 14977, adopted_by_state: 'closed' }), 'Adopted → #14977 (closed) · recovering');
  assert.strictEqual(lifted.adoptedWord({ stage: 85, adopted_by: 14977, adopted_by_state: 'closed' }), 'Adopted → #14977 (closed) · recovering');
  assert.strictEqual(lifted.adoptedWord({ stage: 85, adopted_by: 15060, adopted_by_state: 'closed-twice' }), 'Adopted → #15060 (closed twice) · needs a person');
  assert.strictEqual(lifted.adoptedWord({ stage: 74, adopted_by: 15060, adopted_by_state: '' }), 'Adopted → #15060');
  assert.strictEqual(lifted.adoptedWord({ stage: 85, adopted_by_state: 'closed' }), '');
});

test('the detour word for 074 (and a qualified 085) uses the adopted pill in the shipped renderer', () => {
  assert.match(html, /\(cur === STAGE_TRACKER_ADOPTED_BY_HIVE \|\| \(cur === STAGE_TRACKER_HUMAN_GATED && item\.adopted_by_state\)\) && stageTrackerAdoptedWord\(item\)/);
});
