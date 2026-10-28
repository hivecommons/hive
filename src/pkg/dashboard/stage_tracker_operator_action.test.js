// Exercise the shipped pure helpers, without a browser or server ledger.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, 'static/index.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, 'static/components.css'), 'utf8');
function pick(name) {
  const match = html.match(new RegExp('function ' + name + '\\([^]*?\\n    \\}\\n'));
  assert.ok(match, name + ' missing');
  return match[0];
}
const names = ['stageTrackerOperatorStuck', 'stageTrackerDecisionMatches', 'stageTrackerOperatorStage',
  'stageTrackerOperatorTooltip', 'stageTrackerOperatorBadgeHtml', 'stageTrackerOperatorPRBadge',
  'stageTrackerHeadline', 'stageTrackerHoldLabels', 'stageTrackerSoakLeft', 'stageTrackerHumanMergeReasonWord'];
const esc = text => String(text).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const constant = html.match(/const STAGE_TRACKER_OPERATOR_STUCK_MS = [^;]+;/);
assert.ok(constant);
const helpers = new Function('esc', constant[0] + '\n' + names.map(pick).join('\n') + '\nreturn {' + names.join(',') + '};')(esc);
const { stageTrackerOperatorTooltip: tooltip, stageTrackerOperatorStuck: stuck,
  stageTrackerDecisionMatches: matches, stageTrackerOperatorBadgeHtml: badge,
  stageTrackerOperatorPRBadge: prBadge } = helpers;
const now = Date.parse('2026-10-28T16:00:00Z');
function item(minutes = 12, stage = 85, action = {}) {
  return { stage, caption: 'Waiting for an approval', last_operator_action: {
    action: 'decide:approve', at: new Date(now - minutes * 60000).toISOString(), stage: 85, outcome: 'ok', ...action,
  } };
}

test('decision trail orders waiting, choice/time, then hive result in human words', () => {
  assert.equal(tooltip(item(), now), 'Waiting on: Needs a human decision. You chose: Approve, 12 min ago. Hive: action recorded; waiting for progress.');
  assert.equal(tooltip({}, now), '');
  assert.equal(tooltip(null, now), '');
  assert.doesNotMatch(tooltip(item(), now), /stage\/|085|decide:/);
});

test('moved-on receipt persists, without an amber warning', () => {
  const moved = item(90, 110);
  assert.match(tooltip(moved, now), /Addressed — now at Merged/);
  assert.equal(stuck(moved, now), false);
  assert.match(badge(moved, now), />✓<\/span>/);
  assert.doesNotMatch(badge(moved, now), /ham-operator-stuck/);
});

test('amber uses a strict single threshold and human caption, not a reason token', () => {
  assert.equal(stuck(item(30), now), false);
  assert.equal(stuck(item(30), now + 1), true);
  const delayed = { ...item(42), caption: 'Approval recorded\nwaiting for review', reason: 'raw-stage-token' };
  assert.match(tooltip(delayed, now), /Still at Needs a human decision 42 min after you asked — hive says: Approval recorded waiting for review\./);
  assert.doesNotMatch(tooltip(delayed, now), /\n|raw-stage-token/);
  assert.match(badge(delayed, now), /ham-operator-stuck/);
  assert.match(tooltip({ ...item(42), caption: '' }, now), /no further progress reported/);
});

test('failed outcomes and malformed timestamps are honest, single-line copy', () => {
  assert.match(tooltip(item(42, 85, { outcome: 'Permission denied\nPlease sign in' }), now), /Hive: action failed: Permission denied Please sign in\./);
  const invalid = item(42, 85, { at: 'not-a-time' });
  assert.equal(stuck(invalid, now), false);
  assert.match(tooltip(invalid, now), /time unavailable/);
  assert.equal(stuck(item(42, 85, { stage: undefined }), now), false);
  assert.equal(stuck(item(-5), now), false);
  assert.equal(stuck({}, now), false);
});

test('all submitted actions receive plain labels', () => {
  for (const action of ['hold', 'release', 'retry', 'needs-human', 'accept-triage', 'decide:delegate',
    'decide:direction', 'decide:close-superseded', 'decide:close-wontfix']) {
    assert.doesNotMatch(tooltip(item(12, 85, { action }), now), /Submit an action|decide:/);
  }
});

test('decision buckets share the badge predicate and distinguish acted from awaiting', () => {
  for (const stage of [15, 17, 45, 85, 90]) assert.equal(matches({ stage }, 'awaiting', now), true);
  assert.equal(matches({ stage: 50, modifiers: ['needs-decision'] }, 'awaiting', now), true);
  assert.equal(matches({ stage: 50 }, 'awaiting', now), false);
  assert.equal(matches(item(), 'awaiting', now), false);
  assert.equal(matches({}, 'acted', now), false);
  assert.equal(matches({}, 'stuck', now), false);
  for (const row of [item(), item(42), item(42, 110)]) {
    assert.equal(matches(row, 'acted', now), true);
    assert.equal(matches(row, 'stuck', now), stuck(row, now));
  }
});

test('badge is focusable, escaped, and absent for legacy payloads', () => {
  assert.equal(badge({}, now), '');
  const row = item(42, 85, { outcome: '<denied> "try again"' });
  const rendered = badge(row, now);
  const tip = esc(tooltip(row, now));
  assert.ok(rendered.includes('title="' + tip + '" aria-label="' + tip + '"'));
  assert.match(rendered, /tabindex="0"/);
  assert.doesNotMatch(rendered, /<denied>|style=|animation|blink/);
  assert.match(css, /\.ham-badge\.ham-operator-stuck\s*\{ color: var\(--amber\); \}/);
});

test('nested PR receipts use the full tracker item, not an issue action', () => {
  const pr = item(42);
  const ctx = { operatorItems: new Map([['org/repo/pr#7', pr]]) };
  assert.equal(prBadge({ number: 7 }, { repo: 'org/repo' }, now, ctx), badge(pr, now));
  assert.equal(prBadge({ number: 8 }, { repo: 'org/repo', ...item() }, now, ctx), '');
  assert.ok(pick('stageTrackerSubRowHtml').includes('stageTrackerOperatorPRBadge'));
  const row = pick('stageTrackerRowHtml');
  assert.ok(row.includes('stageTrackerOperatorPRBadge'));
  assert.equal((row.match(/stageTrackerOperatorBadgeHtml\(item, nowMs\)/g) || []).length, 3);
});

test('quiet payloads re-render once a minute so amber and the stuck filter age without a new response', () => {
  let clock = now;
  let renders = 0;
  const tick = new Function('_stageTracker', 'Date', 'renderStageTracker', 'document',
    'let _stageTrackerOperatorMinute = -1;\n' + pick('stageTrackerTickClock') + '\nreturn stageTrackerTickClock;')(
    { items: [item()] }, { now: () => clock }, () => renders++, { querySelector: () => null });
  tick();
  tick();
  assert.equal(renders, 1);
  clock += 60000;
  tick();
  assert.equal(renders, 2);
});

test('decision filter is normalized, saved with other filters, synced and reset', () => {
  assert.match(pick('stageTrackerDefaultFilter'), /decision: ''/);
  assert.match(pick('stageTrackerNormalizeFilter'), /\['', 'awaiting', 'acted', 'stuck'\]/);
  assert.match(pick('stageTrackerFilterActive'), /!!f\.decision/);
  assert.match(pick('stageTrackerMatches'), /stageTrackerDecisionMatches\(item, f\.decision/);
  assert.match(pick('hamFilter'), /case 'decision': f\.decision = f\.decision === value \? '' : value/);
  assert.match(pick('hamFilter'), /stageTrackerSaveFilter\(\)/);
  assert.match(pick('stageTrackerSaveFilter'), /localStorage\.setItem/);
  assert.match(pick('stageTrackerSyncToolbar'), /b\.dataset\.arg1 === f\.decision/);
});
