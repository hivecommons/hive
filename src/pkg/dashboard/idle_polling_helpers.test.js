const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function fn(name) {
  const re = new RegExp('\\n    (?:async )?function ' + name + '\\([^)]*\\) \\{\\n[\\s\\S]*?\\n    \\}\\n');
  const m = html.match(re);
  assert.ok(m, name + ' not found');
  return m[0];
}
function constant(name) {
  const m = html.match(new RegExp('const ' + name + ' = [^;]+;'));
  assert.ok(m, name + ' not found');
  return m[0];
}

// Builds the stage-tracker nudge coalescer with a fake clock, a fake
// document and a counter in place of fetchStageTracker.
function nudgeHarness() {
  const state = { fetches: 0, timers: [], hidden: false };
  const src = [
    constant('STAGE_TRACKER_NUDGE_COALESCE_MS'),
    'let _stageTrackerNudgeTimer = null;',
    'let _stageTrackerNudgeAt = 0;',
    'let _stageTrackerNudgeSkipped = false;',
    'function fetchStageTracker() { state.fetches++; }',
    fn('stageTrackerNudge'),
    fn('stageTrackerResumeIfSkipped'),
    'return { nudge: stageTrackerNudge, resume: stageTrackerResumeIfSkipped, pending: () => !!_stageTrackerNudgeTimer, skipped: () => _stageTrackerNudgeSkipped };',
  ].join('\n');
  const document = { get hidden() { return state.hidden; } };
  const setTimeout = (cb, ms) => { state.timers.push({ cb, ms }); return state.timers.length; };
  const Date = { now: () => state.now };
  state.now = 100000;
  const h = new Function('state', 'document', 'setTimeout', 'Date', src)(state, document, setTimeout, Date);
  return Object.assign(h, { state, fire() { const t = state.timers.shift(); if (t) t.cb(); } });
}

test('the first nudge in a window fetches immediately', () => {
  const h = nudgeHarness();
  assert.strictEqual(h.nudge(h.state.now), true);
  assert.strictEqual(h.state.fetches, 1);
  assert.strictEqual(h.pending(), false);
});

test('a burst of nudges collapses into one trailing fetch', () => {
  const h = nudgeHarness();
  h.nudge(h.state.now);
  for (let i = 0; i < 10; i++) { h.state.now += 100; assert.strictEqual(h.nudge(h.state.now), false); }
  assert.strictEqual(h.state.fetches, 1);
  assert.strictEqual(h.pending(), true);
  assert.strictEqual(h.state.timers.length, 1, 'one trailing timer, not one per nudge');
  assert.strictEqual(h.state.timers[0].ms, 2000 - 100, 'scheduled by the first coalesced nudge for the end of the window');
  h.state.now += 1000;
  h.fire();
  assert.strictEqual(h.state.fetches, 2);
  assert.strictEqual(h.pending(), false);
});

test('a nudge after the window fetches again at once', () => {
  const h = nudgeHarness();
  h.nudge(h.state.now);
  h.state.now += 2000;
  assert.strictEqual(h.nudge(h.state.now), true);
  assert.strictEqual(h.state.fetches, 2);
});

test('nudges on a hidden tab are deferred to the next visibilitychange', () => {
  const h = nudgeHarness();
  h.state.hidden = true;
  h.nudge(h.state.now);
  h.nudge(h.state.now + 5000);
  assert.strictEqual(h.state.fetches, 0);
  assert.strictEqual(h.skipped(), true);
  h.state.hidden = false;
  h.resume();
  assert.strictEqual(h.state.fetches, 1);
  assert.strictEqual(h.skipped(), false);
  // Nothing was skipped: resuming again is a no-op.
  h.resume();
  assert.strictEqual(h.state.fetches, 1);
});

test('a trailing fetch that fires after the tab hid is deferred too', () => {
  const h = nudgeHarness();
  h.nudge(h.state.now);
  h.nudge(h.state.now + 100);
  h.state.hidden = true;
  h.fire();
  assert.strictEqual(h.state.fetches, 1);
  assert.strictEqual(h.skipped(), true);
});

// Builds the agent-log visibility gate against a fake document.
function logsHarness(opts) {
  const state = Object.assign({ hidden: false, section: true, displayed: true, collapsed: false, fetches: 0 }, opts);
  const src = [
    'let _logsPollSkipped = false;',
    'function isSectionCollapsed() { return state.collapsed; }',
    'function fetchAgentLogs() { state.fetches++; }',
    fn('agentLogsVisible'),
    fn('agentLogsResumeIfSkipped'),
    'return { visible: agentLogsVisible, resume: agentLogsResumeIfSkipped, skip: () => { _logsPollSkipped = true; } };',
  ].join('\n');
  const document = {
    get hidden() { return state.hidden; },
    getElementById(id) {
      if (id !== 'logs-section' || !state.section) return null;
      return { get offsetParent() { return state.displayed ? {} : null; } };
    },
  };
  return Object.assign(new Function('state', 'document', src)(state, document), { state });
}

test('agent logs poll only when the section can be seen', () => {
  assert.strictEqual(logsHarness().visible(), true);
  assert.strictEqual(logsHarness({ hidden: true }).visible(), false);
  assert.strictEqual(logsHarness({ section: false }).visible(), false);
  assert.strictEqual(logsHarness({ displayed: false }).visible(), false);
  assert.strictEqual(logsHarness({ collapsed: true }).visible(), false);
});

test('a skipped poll runs once when the section becomes visible', () => {
  const h = logsHarness({ collapsed: true });
  h.skip();
  h.resume();
  assert.strictEqual(h.state.fetches, 0, 'still collapsed: nothing to do');
  h.state.collapsed = false;
  h.resume();
  assert.strictEqual(h.state.fetches, 1);
  h.resume();
  assert.strictEqual(h.state.fetches, 1, 'nothing skipped since: no refetch');
});

test('fetchAgentLogs bails out before touching the network when hidden', () => {
  const body = fn('fetchAgentLogs');
  assert.ok(body.includes("if (!agentLogsVisible()) { _logsPollSkipped = true; return; }"));
  assert.ok(body.includes("const lines = current === 'all' ? '?lines=' + LOGS_ALL_VIEW_LINES : '';"), 'the all-agents view asks for only the lines it shows');
});
