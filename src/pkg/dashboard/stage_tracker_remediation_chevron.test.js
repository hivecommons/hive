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
function constLine(source, name) {
  const match = source.match(new RegExp('const ' + name + ' = [^\\n]*;'));
  assert.ok(match, name);
  return match[0];
}
const trackConsts = ['STAGE_TRACKER_WORKFLOW_APPROVAL_BLOCKER', 'STAGE_TRACKER_WORKFLOW_APPROVAL_TIP', 'STAGE_TRACKER_RING_SEGMENT_GAP_MAX', 'STAGE_TRACKER_RING_RADIUS', 'STAGE_TRACKER_RING_GAP_DEG'].map(name => constLine(strip, name)).join('\n');
const trackFns = ['stageTrackerTrackLead', 'stageTrackerWaitingOnPerson', 'stageTrackerTrackRunning', 'stageTrackerWorkflowApprovalPending', 'stageTrackerRingSvg', 'stageTrackerStalledTrack', 'stageTrackerQueueAheadWord', 'stageTrackerStageTrack', 'stageTrackerHiveOwnsRepair', 'stageTrackerTrackWord', 'stageTrackerActivityTrackHtml'].map(name => fn(strip, name)).join('\n');
const track = new Function('esc', 'stageTrackerIcon', trackConsts + '\n' + trackFns + ';return stageTrackerActivityTrackHtml;')(esc, (n, cls) => `<svg class="${cls}" data-icon="${n}"></svg>`);
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
` + fn(strip, 'stageTrackerWaitingOnPerson') + fn(strip, 'stageTrackerTrackRunning') + fn(strip, 'stageTrackerStageTrack') + fn(strip, 'stageTrackerStalledTrack') + fn(strip, 'stageTrackerStripItems') + ';return stageTrackerStripItems;')(esc, segments, track);

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
  // #16599: a settled verdict is the glyph alone; its words ride in the tooltip.
  assert.match(review, /Review: Review 5\/5 · changes requested · /);
  assert.doesNotMatch(review, /changes requested<|awaiting human/);

  const parked = cells(redPR({ stage: 85, display_column: 85, needs_human: true, review_activity: decision }))[5];
  assert.match(parked, /class="ham-seg ham-activity-track done decided"/, 'parked on a person is settled, still not amber');
  assert.match(parked, /Review: Review 5\/5 · needs owner sign-off · /, 'genuinely parked: the sign-off word stands (in the tooltip)');

  const changes = cells(redPR({ stage: 74, display_column: 30, code_activity: adopted, review_activity: { ...decision, state: 'changes_requested' } }))[5];
  // A settled verdict is the glyph alone in the pass / fail colour; its
  // count words ride in the tooltip.
  assert.match(changes, /class="ham-seg ham-activity-track failing"[^>]*data-verdict="fail"/);
  assert.match(changes, /Review: Review 5\/5 · changes requested · /);
  assert.match(changes, /ham-track-verdict"><svg [^]*?<\/svg><\/span>/);
  const approved = cells(redPR({ stage: 74, display_column: 30, code_activity: adopted, review_activity: { ...decision, state: 'approved' } }))[5];
  assert.match(approved, /class="ham-seg ham-activity-track done"[^>]*data-verdict="pass"/);
  assert.match(approved, /Review: Review 5\/5 approved · /);
  assert.match(approved, /ham-track-verdict"><svg [^]*?<\/svg><\/span>/);
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

// hivecommons/hive#16540: never three lit chevrons. Amber = the stage
// chevron's active fill (minus settled-current) plus any track in the
// active class.
const litCount = html => (html.match(/class="ham-seg active(?! [^"]*ham-settled-current)|class="ham-seg ham-activity-track active\b/g) || []).length;
const seen = '2026-11-04T02:44:00Z';

test('#16509 row: adopted, CI 7/11 unreported, review 5/5 decided → one amber chevron', () => {
  const item = redPR({ stage: 74, display_column: 30,
    code_activity: { head: 'abc1234def', state: 'adopted', caption: 'Adopted by hive · activity', source: 'adoption', observed_at: seen },
    ci_activity: { head: 'abc1234def', state: 'missing', caption: 'CI waiting · required check "test" hasn\'t reported for this head yet', checks_passed: 7, checks_total: 11, blocker: 'test', source: 'checks', observed_at: seen },
    review_activity: { head: 'abc1234def', state: 'human_decision', caption: 'Review findings · human decision', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen } });
  const c = cells(item);
  assert.match(c[3], /class="ham-seg active detoured"/, 'the stage chevron is the one lit chevron');
  // #16599: unreported checks are not motion and not the stage's track →
  // neutral glyph alone, count words in the tooltip.
  assert.match(c[4], /class="ham-seg ham-activity-track todo"/, 'unreported checks are not motion');
  assert.match(c[4], /title="CI: CI 7\/11 · test not reported · /);
  assert.doesNotMatch(c[4], /test not reported</);
  assert.match(c[5], /class="ham-seg ham-activity-track done decided"/);
  assert.equal(litCount(c.join('')), 1);
});

test('#16268 row: waiting for the contributor, CI 7/11 unreported, review approved → exactly one amber chevron', () => {
  const owner = { head: 'abc1234def', state: 'waiting_owner', caption: 'Waiting for @hanthor · not observed coding', actor: 'hanthor', source: 'native-author-wait', observed_at: seen };
  const ci = { head: 'abc1234def', state: 'missing', caption: 'CI waiting · required check "test" hasn\'t reported for this head yet', checks_passed: 7, checks_total: 11, blocker: 'test', source: 'checks', observed_at: seen };
  const approved = { head: 'abc1234def', state: 'approved', caption: 'Review approved', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen };
  const c = cells(redPR({ stage: 71, display_column: 30, author: 'hanthor', author_kind: 'human', code_activity: owner, ci_activity: ci, review_activity: approved }));
  assert.match(c[3], /class="ham-seg active detoured"/);
  assert.match(c[4], /class="ham-seg ham-activity-track todo"[^>]*title="CI: CI 7\/11 · test not reported · /);
  assert.doesNotMatch(c[4], /test not reported</, 'waiting on a person: CI is its glyph alone (#16599)');
  assert.match(c[5], /class="ham-seg ham-activity-track done"[^]*Review: Review 5\/5 approved · [^]*ham-track-verdict"><svg [^]*?<\/svg><\/span>/);
  assert.equal(litCount(c.join('')), 1);
  // Even a queued check does not light CI while the row waits on a person.
  const queued = cells(redPR({ stage: 71, display_column: 30, author: 'hanthor', author_kind: 'human', code_activity: owner, ci_activity: { ...ci, state: 'queued' }, review_activity: approved }));
  assert.match(queued[4], /class="ham-seg ham-activity-track todo"/);
  assert.equal(litCount(queued.join('')), 1);
});

test('three qualify: CI and review running on a lit PR in progress → tracks win, stage chevron settled-current', () => {
  const running = {
    code_activity: { head: 'abc1234def', state: 'remediating', caption: 'Remediating · scanner · 4m', actor: 'scanner', active: true, source: 'fix-dispatch', since: '2026-11-04T02:41:00Z', observed_at: seen },
    ci_activity: { head: 'abc1234def', state: 'running', active: true, caption: 'CI running', checks_passed: 4, checks_total: 11, source: 'checks', observed_at: seen },
    review_activity: { head: 'abc1234def', state: 'reviewing', active: true, actor: 'reviewer-2', caption: 'Review working', perspectives_done: 2, perspectives_total: 5, source: 'dispatch', observed_at: seen } };
  const both = cells(redPR({ display_column: 30, ...running }));
  assert.match(both[3], /class="ham-seg active ham-settled-current ham-remediation-remediating detoured"/);
  assert.doesNotMatch(both[3], /data-motion=/);
  assert.match(both[3], /Remediating · scanner · 4m/, 'the words stay');
  assert.match(both[3], /Waiting on CI and review, both running/);
  assert.match(both[4], /ham-activity-track active"[^]*data-motion="active"/);
  assert.match(both[5], /ham-activity-track active"[^]*data-motion="active"/);
  assert.equal(litCount(both.join('')), 2);
  // One track running: the stage chevron keeps its own fill and pulse.
  const one = cells(redPR({ display_column: 30, ...running, review_activity: { ...running.review_activity, state: 'changes_requested', active: false } }));
  assert.match(one[3], /class="ham-seg active ham-remediation-remediating detoured"[^>]*data-motion="active"/);
  assert.equal(litCount(one.join('')), 2);
});

test('#16456 row: PR in progress with changes requested → the stage chevron is the only lit one; review is a red-text finding, CI a green-text pass', () => {
  const c = cells(redPR({ display_column: 30,
    code_activity: { head: 'abc1234def', state: 'unassigned', caption: 'Changes requested · unassigned · next: scanner', source: 'no-pickup-evidence', since: red, observed_at: seen },
    ci_activity: { head: 'abc1234def', state: 'passing', caption: 'CI green', checks_passed: 11, checks_total: 11, source: 'checks', observed_at: seen },
    review_activity: { head: 'abc1234def', state: 'changes_requested', caption: 'Review findings', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen, since: red } }));
  assert.match(c[3], /class="ham-seg active ham-remediation-unassigned detoured"/);
  assert.match(c[4], /class="ham-seg ham-activity-track done"[^>]*data-verdict="pass"[^]*CI: CI 11\/11 passed · [^]*ham-track-verdict"><svg [^]*?<\/svg><\/span>/);
  assert.match(c[4], /ham-ring ham-ring-pass[^>]*data-ring-done="11"/, 'the pass ring is the icon');
  assert.match(c[5], /class="ham-seg ham-activity-track failing"[^>]*data-verdict="fail"[^]*Review: Review 5\/5 · changes requested · [^]*ham-track-verdict"><svg [^]*?<\/svg><\/span>/);
  assert.match(c[5], /ham-ring ham-ring-fail/, 'the finding ring is red');
  assert.equal(litCount(c.join('')), 1, 'done / failing tracks are text-colour variants, not fills');
});

test('#16539 shape: a queued swarm reads 0/5 and, as the stage\'s own track, is lit still (#16599)', () => {
  const c = cells(redPR({ display_column: 50, stage: 50,
    review_activity: { head: 'abc1234def', state: 'unassigned', caption: 'Review queued · unassigned', blocker: 'no-free-reviewer', perspectives_done: 0, perspectives_total: 5, source: 'review-dispatch-wait', observed_at: seen } }));
  assert.match(c[5], /class="ham-seg ham-activity-track active ham-current"[^]*Review 0\/5 · queued</);
  assert.doesNotMatch(c[5], /data-motion=/, 'queued is lit but still');
  assert.equal(litCount(c.join('')), 1, 'the Review column is where the item is: the one lit chevron');
});
