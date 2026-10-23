// stageTrackerMotion(): which lit chevrons in the Hive activity monitor
// animate. Only the hive acting ('active') or a step waiting on a person
// ('needs-human') moves; Open, Open · hold, Claimed and everything else is
// 'static'. The function is lifted straight out of index.html so this test
// runs against the shipped source.
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
  pick(/const STAGE_TRACKER_ACTOR = \{[^\n]*\n/, 'STAGE_TRACKER_ACTOR'),
  pick(/const STAGE_TRACKER_TERMINAL = \d+;/, 'STAGE_TRACKER_TERMINAL'),
  pick(/const STAGE_TRACKER_MOTION_STATIC = [^\n]*\n/, 'STAGE_TRACKER_MOTION_STATIC'),
  pick(/const STAGE_TRACKER_MOTION_ACTIVE = [^\n]*\n/, 'STAGE_TRACKER_MOTION_ACTIVE'),
  pick(/const STAGE_TRACKER_MOTION_HUMAN = [^\n]*\n/, 'STAGE_TRACKER_MOTION_HUMAN'),
  pick(/const STAGE_TRACKER_MOTION_BY_STAGE = \{[\s\S]*?\n    \};\n/, 'STAGE_TRACKER_MOTION_BY_STAGE'),
  pick(/const STAGE_TRACKER_MOTION_AUTHOR_STAGES = [^\n]*\n/, 'STAGE_TRACKER_MOTION_AUTHOR_STAGES'),
  pick(/const STAGE_TRACKER_MOTION_HUMAN_MODS = [^\n]*\n/, 'STAGE_TRACKER_MOTION_HUMAN_MODS'),
  pick(/function stageTrackerActorKind\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActorKind'),
  pick(/function stageTrackerMotion\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerMotion'),
].join('\n');
const motion = new Function(src + '; return stageTrackerMotion;')();

// Every stage the dashboard knows about (stage_tracker.go / pkg/stagelabels)
// plus 000 Open and the 110 ✓ cap, each with its expected bucket.
const EXPECTED = {
  0: 'static',        // Open
  10: 'static',       // Actionable — waiting for a worker
  15: 'needs-human',  // Triage needed — a maintainer's look
  20: 'static',       // Claimed
  30: 'static',       // PR in progress — human author (see author tests below)
  40: 'static',       // Covered by PR — human author
  45: 'needs-human',  // Issue parked on a decision
  50: 'active',       // CI / Review pending — hive reviewing
  60: 'active',       // Reviewed — hive admits to the merge gate
  70: 'active',       // Fixing review findings — hive
  71: 'needs-human',  // Needs author
  72: 'needs-human',  // Returned to contributor
  74: 'active',       // Adopted by hive
  80: 'active',       // Merge gate — hive deciding admission
  85: 'needs-human',  // PR parked on a decision
  90: 'needs-human',  // Ready for a human merge
  95: 'active',       // Pre-merge soak — hive's timer
  100: 'active',      // Merge queue — hive merging
  110: 'static',      // Merged (✓ cap)
};

test('every known stage maps to its bucket', () => {
  for (const [stage, want] of Object.entries(EXPECTED)) {
    assert.strictEqual(motion({ stage: Number(stage), kind: 'pr', author_kind: 'human', modifiers: [] }), want, 'stage ' + stage);
  }
});

test('open, open · hold and claimed never move', () => {
  assert.strictEqual(motion({ stage: 0, modifiers: [] }), 'static');
  assert.strictEqual(motion({ stage: 0, modifiers: ['hold'] }), 'static');
  assert.strictEqual(motion({ stage: 20, kind: 'issue', claimant_kind: 'hive', modifiers: [] }), 'static');
  assert.strictEqual(motion({ stage: 20, kind: 'issue', claimant_kind: 'human', modifiers: [] }), 'static');
});

test('a PR in progress pulses only when an agent is working it', () => {
  for (const stage of [30, 40]) {
    assert.strictEqual(motion({ stage, kind: 'pr', author_kind: 'hive' }), 'active', stage + ' hive author');
    assert.strictEqual(motion({ stage, kind: 'pr', author_kind: 'clanker' }), 'active', stage + ' clanker author');
    assert.strictEqual(motion({ stage, kind: 'pr', author_kind: 'human' }), 'static', stage + ' human author');
    assert.strictEqual(motion({ stage, kind: 'pr' }), 'static', stage + ' unknown author');
  }
  // An issue at 030 (kick running, no PR yet) reads the linked PR's author kind.
  assert.strictEqual(motion({ stage: 30, kind: 'issue', linked_prs: [{ author_kind: 'hive' }] }), 'active');
  assert.strictEqual(motion({ stage: 30, kind: 'issue', linked_prs: [] }), 'static');
});

test('hold wins over every other modifier and stage', () => {
  assert.strictEqual(motion({ stage: 0, modifiers: ['hold', 'needs-human'] }), 'static');
  assert.strictEqual(motion({ stage: 50, kind: 'pr', modifiers: ['hold'] }), 'static');
  assert.strictEqual(motion({ stage: 85, kind: 'pr', modifiers: ['hold', 'needs-decision'] }), 'static');
});

test('human-attention modifiers breathe regardless of stage', () => {
  for (const mod of ['stalled', 'blocked', 'needs-human', 'needs-decision', 'needs-triage']) {
    assert.strictEqual(motion({ stage: 50, kind: 'pr', modifiers: [mod] }), 'needs-human', mod + ' on an active stage');
    assert.strictEqual(motion({ stage: 10, kind: 'issue', modifiers: [mod] }), 'needs-human', mod + ' on a static stage');
  }
});

test('modifiers that are not a human call leave the stage bucket alone', () => {
  for (const mod of ['regressed', 'needs-rebase', 'draft']) {
    assert.strictEqual(motion({ stage: 50, kind: 'pr', modifiers: [mod] }), 'active', mod);
    assert.strictEqual(motion({ stage: 0, modifiers: [mod] }), 'static', mod);
  }
});

test('unknown stages and malformed items default to static', () => {
  assert.strictEqual(motion({ stage: 999 }), 'static');
  assert.strictEqual(motion({ stage: 55 }), 'static');
  assert.strictEqual(motion({ stage: 'nope' }), 'static');
  assert.strictEqual(motion({}), 'static');
  assert.strictEqual(motion(null), 'static');
  assert.strictEqual(motion({ stage: 50, modifiers: 'not-an-array' }), 'active');
});

test('CSS animates exactly the two moving buckets and respects reduced motion', () => {
  assert.ok(html.includes('.ham-seg.active { background: var(--amber); color: var(--surface-0); }'), 'bare lit rule has no animation');
  assert.ok(html.includes('.ham-seg.active[data-motion="active"] { animation: ham-pulse'), 'active bucket pulses');
  assert.ok(html.includes('.ham-seg.active[data-motion="needs-human"] { animation: ham-breathe-human'), 'needs-human bucket breathes');
  assert.ok(/@media \(prefers-reduced-motion: reduce\) \{[^\n]*\.ham-seg\.active\[data-motion\] \{ animation: none; \}/.test(html), 'reduced motion silences both buckets');
});
