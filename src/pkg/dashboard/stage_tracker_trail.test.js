// The Activity Monitor hover's "Trail:" clause names the item's recent hops
// by stage word (hivecommons/hive#14787 showed "undefined → undefined → …"
// because the renderer read a field the payload never carried). The helpers
// are lifted straight out of index.html so this test runs against the
// shipped source; the fixture is the live /api/stage-tracker shape.
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
  pick(/const STAGE_TRACKER_TRAIL_HOPS = \d+;/, 'STAGE_TRACKER_TRAIL_HOPS'),
  pick(/function stageTrackerHopName\(h\) \{[\s\S]*?\n    \}\n/, 'stageTrackerHopName'),
  pick(/function stageTrackerTrailText\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerTrailText'),
].join('\n');

const stages = [
  { number: 10, short: 'Actionable', label: 'stage/010-actionable' },
  { number: 50, short: 'CI / Review', label: 'stage/050-review-pending' },
  { number: 71, short: 'Needs author', label: 'stage/071-needs-author' },
  { number: 85, short: 'Needs human decision', label: 'stage/085-needs-decision' },
  { number: 90, label: 'stage/090-ready-for-human-merge' },
];

function load(tracker) {
  return new Function('_stageTracker', src + '; return { hopName: stageTrackerHopName, trail: stageTrackerTrailText };')(tracker);
}

// The hops /api/stage-tracker emitted for hivecommons/hive#14787 (StageTrackerHop: number, slug, at, reason, from).
const live14787 = { kind: 'pr', number: 14787, stage: 85, modifiers: ['needs-human'], history: [
  { number: 50, slug: 'review-pending', at: '2026-10-27T21:27:53-04:00', reason: 'required-check-pending:Sign-off survives the squash' },
  { number: 85, slug: 'needs-decision', at: '2026-10-27T21:38:16-04:00', reason: 'parked on a person: needs-human', from: 50 },
  { number: 50, slug: 'review-pending', at: '2026-10-27T21:38:50-04:00', reason: 'required-check-pending:build-and-test', from: 85 },
  { number: 71, slug: 'needs-author', at: '2026-10-27T21:53:36-04:00', reason: 'check-failing:coverage', from: 50 },
  { number: 50, slug: 'review-pending', at: '2026-10-27T21:57:26-04:00', reason: 'label:needs-human', from: 71 },
  { number: 71, slug: 'needs-author', at: '2026-10-27T22:19:45-04:00', reason: 'reviewer-verdict:changes_requested', from: 50 },
  { number: 50, slug: 'review-pending', at: '2026-10-27T23:59:38-04:00', reason: 'label:needs-human', from: 71 },
  { number: 85, slug: 'needs-decision', at: '2026-10-28T00:19:25-04:00', reason: 'label:needs-human', from: 50 },
] };

test('the trail names the last hops of the live payload by stage word', () => {
  const { trail } = load({ stages });
  assert.strictEqual(trail(live14787), 'CI / Review → Needs author → CI / Review → Needs human decision');
  assert.ok(!trail(live14787).includes('undefined'));
});

test('a hop that cannot be named is left out, never printed as undefined', () => {
  const { hopName, trail } = load({ stages });
  assert.strictEqual(hopName({ number: 999 }), '');
  assert.strictEqual(hopName({}), '');
  assert.strictEqual(hopName(null), '');
  assert.strictEqual(hopName({ number: 0 }), 'Open');
  assert.strictEqual(hopName({ number: 90 }), 'stage/090-ready-for-human-merge', 'label stands in when there is no short word');
  assert.strictEqual(hopName({ number: 999, slug: 'brand-new' }), 'brand-new', 'the slug stands in for a stage the table does not know');
  assert.strictEqual(trail({ history: [{ number: 50 }, { number: 999 }, { number: 85 }] }), 'CI / Review → Needs human decision');
});

test('the clause is omitted when fewer than two hops can be named', () => {
  const { trail } = load({ stages });
  assert.strictEqual(trail({ history: [{ number: 50 }] }), '');
  assert.strictEqual(trail({ history: [{ number: 50 }, { number: 999 }] }), '');
  assert.strictEqual(trail({ history: [] }), '');
  assert.strictEqual(trail({}), '');
  assert.strictEqual(trail(null), '');
});

test('before the stage table loads the slug carries the trail', () => {
  const { trail } = load(null);
  assert.strictEqual(trail(live14787), 'review-pending → needs-author → review-pending → needs-decision');
  assert.strictEqual(trail({ history: [{ number: 50 }, { number: 85 }] }), '', 'bare numbers with no table name nothing');
});
