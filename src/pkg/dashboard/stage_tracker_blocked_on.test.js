// A `blocked` Activity Monitor row whose issue carries a hive:blocked-on
// marker names the PR it waits on (hivecommons/hive#14863 showed a bare grey
// `blocked` pill with "No stage recorded yet"). The helpers are lifted out of
// index.html so this runs against the shipped source; the fixture is the
// live /api/stage-tracker shape with the new `blocked_on_pr` field.
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
  pick(/const STAGE_TRACKER_BLOCKED_ON_PR_OPEN = '[^\n]*';/, 'STAGE_TRACKER_BLOCKED_ON_PR_OPEN'),
  pick(/const STAGE_TRACKER_BLOCKED_ON_PR_MERGED = '[^\n]*';/, 'STAGE_TRACKER_BLOCKED_ON_PR_MERGED'),
  pick(/const STAGE_TRACKER_BLOCKED_ON_PR_CLOSED = '[^\n]*';/, 'STAGE_TRACKER_BLOCKED_ON_PR_CLOSED'),
  pick(/function stageTrackerBlockedOnPR\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerBlockedOnPR'),
  pick(/function stageTrackerBlockedTip\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerBlockedTip'),
].join('\n');

const api = new Function(src + '; return { blockedOnPR: stageTrackerBlockedOnPR, blockedTip: stageTrackerBlockedTip };')();

// hivecommons/hive#14863 as /api/stage-tracker now emits it.
const live14863 = { repo: 'hivecommons/hive', number: 14863, kind: 'issue', stage: 0, labels: ['agent/reviewer', 'blocked'], modifiers: ['blocked'],
  bucket: 'filtered', bucket_reason: 'blocked on hivecommons/hive#14862', caption: 'Blocked on PR #14862 — lifts when it merges',
  blocked_on_pr: { repo: 'hivecommons/hive', number: 14862, url: 'https://github.com/hivecommons/hive/pull/14862', state: 'open' } };

test('blocked_on_pr names, shortens and links the blocking PR', () => {
  const pr = api.blockedOnPR(live14863);
  assert.deepStrictEqual(pr, { ref: 'hivecommons/hive#14862', short: '#14862', url: 'https://github.com/hivecommons/hive/pull/14862', state: 'open',
    tip: 'Blocked on PR hivecommons/hive#14862 (open) — lifts when it merges; re-triage if closed unmerged' });
  assert.strictEqual(api.blockedTip(live14863), 'blocked on hivecommons/hive#14862');
});

test('state words: merged and closed-unmerged explain the next sweep move; unknown state is omitted', () => {
  assert.strictEqual(api.blockedOnPR({ repo: 'a/b', blocked_on_pr: { repo: 'a/b', number: 1, state: 'merged' } }).tip,
    'Blocked on PR a/b#1 (merged) — merged; the blocked sweep lifts the label next');
  assert.strictEqual(api.blockedOnPR({ repo: 'a/b', blocked_on_pr: { repo: 'a/b', number: 1, state: 'closed' } }).tip,
    'Blocked on PR a/b#1 (closed) — closed unmerged; the blocked sweep re-triages it next');
  assert.strictEqual(api.blockedOnPR({ repo: 'a/b', blocked_on_pr: { repo: 'a/b', number: 1 } }).tip,
    'Blocked on PR a/b#1 — lifts when it merges; re-triage if closed unmerged');
});

test('a blocker in another repo keeps its owner/repo; the GitHub blocked-by list and the PR are merged in the chevron tip', () => {
  const foreign = api.blockedOnPR({ repo: 'hivecommons/hive', blocked_on_pr: { repo: 'hivecommons/pluk', number: 7 } });
  assert.strictEqual(foreign.short, 'hivecommons/pluk#7');
  assert.strictEqual(api.blockedTip({ blocked_on: ['acme/api#8'], blocked_on_pr: { repo: 'acme/api', number: 7 } }), 'blocked on acme/api#8, acme/api#7');
  assert.strictEqual(api.blockedTip({ blocked_on: ['acme/api#7'], blocked_on_pr: { repo: 'acme/api', number: 7 } }), 'blocked on acme/api#7', 'no duplicate');
});

test('rows without the field, or with a malformed one, render as before', () => {
  assert.strictEqual(api.blockedOnPR({ repo: 'a/b', modifiers: ['blocked'] }), null);
  assert.strictEqual(api.blockedOnPR({ repo: 'a/b', blocked_on_pr: {} }), null);
  assert.strictEqual(api.blockedOnPR({ repo: 'a/b', blocked_on_pr: { number: 'x' } }), null);
  assert.strictEqual(api.blockedOnPR(null), null);
  assert.strictEqual(api.blockedTip({ modifiers: ['blocked'] }), 'carries the blocked label');
});
