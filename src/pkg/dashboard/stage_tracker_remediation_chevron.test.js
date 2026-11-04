// The PR in progress chevron on a red PR (hivecommons/hive stage
// stage_tracker_remediation.go): "Changes requested · unassigned" bright
// and still, "Remediating · <actor>" pulsing on pickup evidence, and a
// done-style "Fix pushed <sha> · re-review pending" once a head lands after
// the red signal. Functions are lifted from static-src as the activity
// test does.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const root = path.join(__dirname, 'static-src', 'js');
const strip = fs.readFileSync(path.join(root, 'tracker-row-strip.js'), 'utf8');
const anchor = fs.readFileSync(path.join(root, 'tracker-row-anchor.js'), 'utf8');
const rowSource = fs.readFileSync(path.join(root, 'tracker-row.js'), 'utf8');
function fn(source, name) {
  const match = source.match(new RegExp('function ' + name + '\\([^]*?\\n    \\}'));
  assert.ok(match, name);
  return match[0];
}
const esc = text => String(text).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;');
const globals = `
const STAGE_TRACKER_PATH = [0, 10, 20, 30, 50, 80];
const STAGE_TRACKER_TERMINAL = 110;
const STAGE_TRACKER_PR_COLUMN = 30, STAGE_TRACKER_MERGE_COLUMN = 80;
const STAGE_TRACKER_NEEDS_DECISION = 85, STAGE_TRACKER_ISSUE_NEEDS_DECISION = 45;
const STAGE_TRACKER_ISSUE_DECISION_COLUMN = 10, STAGE_TRACKER_TRIAGE_NEEDED = 15, STAGE_TRACKER_POSSIBLY_EXISTS = 17;
const STAGE_TRACKER_PRE_MERGE_SOAK = 95;
`;
const track = new Function('esc', 'stageTrackerIcon', fn(strip, 'stageTrackerTrackLead') + fn(strip, 'stageTrackerHiveOwnsRepair') + fn(strip, 'stageTrackerTrackWord') + fn(strip, 'stageTrackerActivityTrackHtml') + ';return stageTrackerActivityTrackHtml;')(esc, (n, cls) => `<svg class="${cls}" data-icon="${n}"></svg>`);
const getAnchor = new Function(globals + anchor + ';return stageTrackerAnchor;')();
const segments = new Function('stageTrackerAnchor', globals + `
const STAGE_TRACKER_ACTIVE_WORD = {70: 'Fixing'};
const STAGE_TRACKER_HUMAN_GATED = 85, STAGE_TRACKER_READY_FOR_HUMAN_MERGE = 90, STAGE_TRACKER_ADOPTED_BY_HIVE = 74;
function stageTrackerSentinelInLane() { return false; }
function stageTrackerUnauthorizedWord() { return ''; }
function stageTrackerManualMergeWord() { return 'Ready for human merge'; }
function stageTrackerAdoptedWord() { return 'Adopted by hive'; }
` + fn(rowSource, 'stageTrackerSegments') + ';return stageTrackerSegments;')(getAnchor);
const renderStrip = new Function('esc', 'stageTrackerSegments', 'stageTrackerActivityTrackHtml', globals + `
const STAGE_TRACKER_MOTION_STATIC = 'static', STAGE_TRACKER_MOTION_ACTIVE = 'active';
const STAGE_TRACKER_READY_FOR_HUMAN_MERGE = 90;
function stageTrackerActorKind() { return ''; }
function stageTrackerActorTip() { return ''; }
function stageTrackerMotion() { return STAGE_TRACKER_MOTION_STATIC; }
function stageTrackerIcon(n) { return '<svg data-icon="' + n + '"></svg>'; }
` + fn(strip, 'stageTrackerStripItems') + ';return stageTrackerStripItems;')(esc, segments, track);

const stages = [{ number: 70, branch_from: 50 }, { number: 71, branch_from: 50 }, { number: 72, branch_from: 50 }];
const now = Date.parse('2026-11-04T02:45:00Z');
const red = '2026-11-04T02:05:00Z';
const cells = item => [...renderStrip(item, stages, null, now, {}).matchAll(/<li\b[^]*?<\/li>/g)].map(m => m[0]);
const redPR = extra => ({
  kind: 'pr', repo: 'org/repo', number: 7, head: 'abc1234def', stage: 70, modifiers: [],
  ci_activity: { head: 'abc1234def', state: 'passing', caption: 'CI 11/11 passed', source: 'checks', observed_at: '2026-11-04T02:44:00Z' },
  review_activity: { head: 'abc1234def', state: 'changes_requested', caption: 'Review 5/5 · changes requested', source: 'verified-review-bundle', observed_at: '2026-11-04T02:44:00Z', since: red },
  ...extra,
});

test('unassigned: PR in progress is lit, bright and still, with the pickup rule in its tooltip', () => {
  const item = redPR({ display_column: 30, code_activity: { head: 'abc1234def', state: 'unassigned', caption: 'Changes requested · unassigned · next: scanner', source: 'no-pickup-evidence', since: red, detail: 'Changes requested at ' + red + '; no fix kick, claim or new head seen since. It switches to Remediating when a hive fix kick for this head runs, a clanker claims it or scanner pushes.' } });
  const c = cells(item);
  const pr = c[3];
  assert.match(pr, /class="ham-seg active[^"]*ham-remediation-unassigned/);
  assert.match(pr, /data-remediation="unassigned"/);
  assert.match(pr, /Changes requested · unassigned · next: scanner/);
  assert.doesNotMatch(pr, /data-motion="active"/, 'nobody is on it: no pulse');
  assert.match(pr, /switches to Remediating when a hive fix kick/);
  assert.match(c[5], /data-activity-state="changes_requested"/, 'the Review track keeps its own red state');
});

test('remediating: the chevron pulses, names the actor and shows the evidence kind and time', () => {
  const item = redPR({ display_column: 30,
    remediation: { actor: 'scanner', kind: 'kick', since: '2026-11-04T02:41:00Z', source: 'fix-dispatch' },
    code_activity: { head: 'abc1234def', state: 'remediating', caption: 'Remediating · scanner · 4m', actor: 'scanner', active: true, source: 'fix-dispatch', since: '2026-11-04T02:41:00Z', observed_at: '2026-11-04T02:45:00Z', detail: 'Changes requested at ' + red + '; a hive fix kick is running (evidence: kick, fix-dispatch, since 2026-11-04T02:41:00Z).' } });
  const pr = cells(item)[3];
  assert.match(pr, /class="ham-seg active[^"]*ham-remediation-remediating/);
  assert.match(pr, /data-motion="active"/);
  assert.match(pr, /Remediating · scanner · 4m/);
  assert.match(pr, /evidence: kick by scanner since 2026-11-04T02:41:00Z \(fix-dispatch\)/);
});

test('fix pushed: PR in progress is done-style with its word while CI / Review is the current column', () => {
  const item = redPR({ display_column: 50, head: 'f269349abcdef',
    ci_activity: { head: 'f269349abcdef', state: 'running', caption: 'CI running', source: 'checks', observed_at: '2026-11-04T02:44:00Z' },
    review_activity: { head: 'f269349abcdef', state: 'unknown', caption: 'Review unknown', source: 'unavailable' },
    remediation: { actor: '@hive[bot]', kind: 'push', since: '2026-11-04T02:43:00Z', source: 'recorded-push', head: 'f269349abcdef' },
    code_activity: { head: 'f269349abcdef', state: 'fix_pushed', caption: 'Fix pushed f269349 · CI pending', source: 'recorded-push', since: '2026-11-04T02:43:00Z' } });
  const c = cells(item);
  const pr = c[3];
  assert.match(pr, /class="ham-seg done ham-fix-pushed/);
  assert.match(pr, /data-remediation="fix_pushed"/);
  assert.match(pr, /Fix pushed f269349 · CI pending/);
  assert.doesNotMatch(pr, /aria-current/, 'the pushed fix is history: CI / Review is the current column');
  assert.match(pr, /evidence: push by @hive\[bot\]/);
  assert.match(c[4], /ham-current/, 'CI track is current');
});

test('a red PR with a stale code head renders no remediation attributes', () => {
  const item = redPR({ display_column: 30, code_activity: { head: 'older', state: 'unassigned', caption: 'Changes requested · unassigned · next: scanner' } });
  const html = renderStrip(item, stages, null, now, {});
  assert.doesNotMatch(html, /data-remediation=/);
});

// hive#16509 row: back in PR in progress with the hive on it, a finished
// review is done-style — never the amber in-progress fill — and the word is
// the verdict, not "awaiting human decision".
test('adopted row: a complete review verdict is done-style, and a human-decision verdict reads as changes requested', () => {
  const adopted = { head: 'abc1234def', state: 'adopted', caption: 'Adopted by hive · activity', source: 'adoption', observed_at: '2026-11-04T02:44:00Z' };
  const decision = { head: 'abc1234def', state: 'human_decision', caption: 'Review findings · human decision', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: '2026-11-04T02:44:00Z', since: red };
  const review = cells(redPR({ stage: 74, display_column: 30, code_activity: adopted, review_activity: decision }))[5];
  assert.match(review, /class="ham-seg ham-activity-track done decided"/, 'done-style, muted: not the active fill');
  assert.doesNotMatch(review, /ham-activity-track active|data-motion="active"/);
  assert.match(review, /Review 5\/5 · changes requested</);
  assert.doesNotMatch(review, /awaiting human/);

  const parked = cells(redPR({ stage: 85, display_column: 85, needs_human: true, review_activity: decision }))[5];
  assert.match(parked, /class="ham-seg ham-activity-track done decided"/, 'parked on a person is settled, still not amber');
  assert.match(parked, /Review 5\/5 · awaiting human decision</, 'genuinely parked: the human word stands');

  const changes = cells(redPR({ stage: 74, display_column: 30, code_activity: adopted, review_activity: { ...decision, state: 'changes_requested' } }))[5];
  assert.match(changes, /class="ham-seg ham-activity-track failing"/);
  assert.match(changes, /Review 5\/5 · changes requested</);
  const approved = cells(redPR({ stage: 74, display_column: 30, code_activity: adopted, review_activity: { ...decision, state: 'approved' } }))[5];
  assert.match(approved, /class="ham-seg ham-activity-track done"/);
  assert.match(approved, /Review 5\/5 approved</);
  const ci = cells(redPR({ stage: 74, display_column: 30, code_activity: adopted, review_activity: decision }))[4];
  assert.match(ci, /class="ham-seg ham-activity-track done"/, '11/11 passed is done-style');
});

test('#16494 keeps: review perspectives still running stay amber beside running CI, with words', () => {
  const item = redPR({ display_column: 50,
    code_activity: { head: 'abc1234def', state: 'pushed', caption: 'New head pushed · CI and re-review', source: 'recorded-push', observed_at: '2026-11-04T02:44:00Z' },
    ci_activity: { head: 'abc1234def', state: 'running', active: true, caption: 'CI 4/11 running', checks_passed: 4, checks_total: 11, source: 'checks', observed_at: '2026-11-04T02:44:00Z' },
    review_activity: { head: 'abc1234def', state: 'reviewing', active: true, actor: 'reviewer-2', caption: 'Review 2/5 · reviewer-2 reviewing', perspectives_done: 2, perspectives_total: 5, source: 'dispatch', observed_at: '2026-11-04T02:44:00Z' } });
  const c = cells(item);
  assert.match(c[4], /ham-activity-track active ham-current/);
  assert.match(c[4], /CI 4\/11/);
  assert.match(c[5], /ham-activity-track active ham-current/);
  assert.match(c[5], /Review 2\/5 · reviewer-2 reviewing</);
});
