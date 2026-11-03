// Adopt now's acted state on the Activity Monitor row (hive#15312: the
// maintainer pressed Adopt now on PRs they had approved earlier and the rows
// kept reading "not adopted: a maintainer approved this head" and offering
// Adopt now again until the sweep landed minutes later). Once the payload
// carries `adoption.queued_by` / `takeover.forced_by` the chip reads
// "adopting… (queued)" and the ⋯ menu no longer offers Adopt now. The
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

const src = [
  pick(/const STAGE_TRACKER_ACTION_LABELS = \{[\s\S]*?\n    \};\n/, 'STAGE_TRACKER_ACTION_LABELS'),
  pick(/const STAGE_TRACKER_WHY_WRITE = [^\n]*\n/, 'STAGE_TRACKER_WHY_WRITE'),
  pick(/function stageTrackerAdoptOffered\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAdoptOffered'),
  pick(/const STAGE_TRACKER_ADOPTION_QUEUED_CHIP = [^\n]*\n/, 'STAGE_TRACKER_ADOPTION_QUEUED_CHIP'),
  pick(/function stageTrackerAdoptionQueued\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAdoptionQueued'),
  pick(/function stageTrackerAdoptionChipText\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAdoptionChipText'),
  pick(/const STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE = [^\n]*\n/, 'STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE'),
  pick(/const STAGE_TRACKER_REROUTE_ANY = [^\n]*\n/, 'STAGE_TRACKER_REROUTE_ANY'),
  pick(/const STAGE_TRACKER_REROUTE_PREFIX = [^\n]*\n/, 'STAGE_TRACKER_REROUTE_PREFIX'),
  pick(/function stageTrackerTakeoverOf\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerTakeoverOf'),
  pick(/function stageTrackerClankerLabel\(c, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerClankerLabel'),
  pick(/function stageTrackerRerouteOptions\(tk, enabled, why, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerRerouteOptions'),
  pick(/function stageTrackerTakeoverActions\(item, ctx, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerTakeoverActions'),
].join('\n');
const api = new Function(src + '; return { chipText: stageTrackerAdoptionChipText, queued: stageTrackerAdoptionQueued, actions: stageTrackerTakeoverActions };')();

const ctx = { user: 'clubanderson', canHold: true };
function row(over) {
  return Object.assign({
    repo: 'hivecommons/hive', number: 15312, kind: 'pr', stage: 72,
    adoption: { author: 'odttlnt', due: true, blocked_by: 'operator-admission', nudges: 3, author_online: false, text: 'waiting on @odttlnt (offline 18m) — not adopted: a maintainer approved this head; the hive merges it as-is' },
    takeover: { author: 'odttlnt', author_online: false, adopt_now: true, reroute: false, reroute_reason: 'no other clanker is online' },
  }, over || {});
}

test('before Adopt now: the approved-head blocker shows and Adopt now is offered', () => {
  const it = row();
  assert.equal(api.queued(it), false);
  assert.equal(api.chipText(it), 'not adopted');
  const adopt = api.actions(it, ctx).find(a => a.id === 'decide:adopt-now');
  assert.ok(adopt && adopt.enabled, 'Adopt now offered');
});

test('after Adopt now: acted state — "adopting… (queued)", no Adopt now action, Reroute still listed', () => {
  const it = row({
    adoption: { author: 'odttlnt', due: true, queued_by: 'clubanderson', nudges: 3, author_online: false, text: 'Adopting… (queued by @clubanderson; the hive opens its copy on the next stage pass)' },
    takeover: { author: 'odttlnt', author_online: false, adopt_now: false, adopt_now_reason: 'adoption already queued', forced_by: 'clubanderson', forced_at: '2026-10-30T03:16:58Z', reroute: false, reroute_reason: 'no other clanker is online' },
  });
  assert.equal(api.queued(it), true);
  assert.equal(api.chipText(it), 'adopting… (queued)');
  const acts = api.actions(it, ctx);
  assert.ok(!acts.some(a => a.id === 'decide:adopt-now'), 'Adopt now must not be offered again: ' + JSON.stringify(acts));
  assert.ok(acts.some(a => a.id === 'decide:reroute'), 'Reroute stays listed');
});

test('forced_by alone (older payload without adoption.queued_by) is enough for the acted state', () => {
  const it = row({ takeover: { author: 'odttlnt', adopt_now: true, forced_by: 'clubanderson', reroute: false } });
  assert.equal(api.chipText(it), 'adopting… (queued)');
  assert.ok(!api.actions(it, ctx).some(a => a.id === 'decide:adopt-now'));
});
