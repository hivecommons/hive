// A needs-triage left on a claimed issue is stale, not a signal
// (hivecommons/hive#14586): the tracker payload carries it as stale_labels,
// the row reads its stage word, the hover shows one muted operator note, and
// no "Accept triage" action is offered. The helpers are lifted straight out
// of index.html so this test runs against the shipped source.
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
  pick(/const STAGE_TRACKER_TRIAGE_STAGE = \d+;/, 'STAGE_TRACKER_TRIAGE_STAGE'),
  pick(/const STAGE_TRACKER_ACTIONABLE_STAGE = \d+;/, 'STAGE_TRACKER_ACTIONABLE_STAGE'),
  pick(/const STAGE_TRACKER_NEW_MOVER_MAINTAINER = '[a-z]+';/, 'STAGE_TRACKER_NEW_MOVER_MAINTAINER'),
  pick(/function stageTrackerStaleLabelsText\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerStaleLabelsText'),
  pick(/function stageTrackerTriageNeeded\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerTriageNeeded'),
  pick(/function stageTrackerTriageNewState\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerTriageNewState'),
].join('\n');
const { staleText, triageNeeded } = new Function(src + '; return { staleText: stageTrackerStaleLabelsText, triageNeeded: stageTrackerTriageNeeded };')();

// The payload the Go side emits for hivecommons/hive#14586 once claimed.
const claimedStale = { kind: 'issue', number: 14586, stage: 20, stage_slug: 'claimed', modifiers: [], stale_labels: ['needs-triage'] };

test('a claimed issue with a stale needs-triage gets one muted operator note', () => {
  assert.strictEqual(staleText(claimedStale), 'Stale label: needs-triage (stage has moved on; the hive removes it on its next pass)');
  assert.strictEqual(staleText({ kind: 'issue', stage: 20, modifiers: [] }), '');
  assert.strictEqual(staleText({ kind: 'issue', stage: 20, stale_labels: [] }), '');
  assert.strictEqual(staleText(null), '');
  assert.strictEqual(staleText({ stale_labels: ['needs-triage', 'x'] }), 'Stale labels: needs-triage, x (stage has moved on; the hive removes it on its next pass)');
});

test('a stale needs-triage is not "triage needed" and offers no Accept triage', () => {
  assert.strictEqual(triageNeeded(claimedStale), false);
  assert.strictEqual(triageNeeded({ kind: 'issue', stage: 15, modifiers: [] }), true, '015 is triage needed by its stage word');
  assert.strictEqual(triageNeeded({ kind: 'issue', stage: 10, modifiers: ['needs-triage'] }), true, 'the pill before the claim still counts');
  assert.strictEqual(triageNeeded({ kind: 'pr', stage: 50, modifiers: ['needs-triage'] }), false);
  // The label-less 000-new row the reporter-trust gate holds (hivecommons/hive#15215).
  assert.strictEqual(triageNeeded({ kind: 'issue', stage: 0, modifiers: [], new: { next_mover: 'maintainer' } }), true, 'a 000-new reporter-trust hold is triage needed');
  assert.strictEqual(triageNeeded({ kind: 'issue', stage: 0, modifiers: [], new: { next_mover: 'hive' } }), false, 'an admitted reporter\'s new issue is not');
  assert.strictEqual(triageNeeded({ kind: 'issue', stage: 0, modifiers: [] }), false);
});
