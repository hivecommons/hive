const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const root = path.join(__dirname, 'static-src', 'js');
const strip = fs.readFileSync(path.join(root, 'tracker-row-strip.js'), 'utf8');
const anchor = fs.readFileSync(path.join(root, 'tracker-row-anchor.js'), 'utf8');
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
const track = new Function('esc', fn(strip, 'stageTrackerActivityTrackHtml') + ';return stageTrackerActivityTrackHtml;')(esc);
const getAnchor = new Function(globals + anchor + ';return stageTrackerAnchor;')();
const header = new Function('esc', 'stageTrackerIcon', globals + fn(strip, 'stageTrackerHeaderHtml') + ';return stageTrackerHeaderHtml;')(esc, () => '');
const now = Date.parse('2026-11-02T21:00:00Z');
const activity = (state, caption, active = false) => ({
  head: 'head', state, caption, active, source: state === 'fixing' ? 'fix-dispatch+successful-edit+verified-git-worktree' : 'observed-test-source',
  observed_at: new Date(now).toISOString(), since: new Date(now - 60000).toISOString(),
});
const stages = [{ number: 70, branch_from: 50 }, { number: 71, branch_from: 50 }, { number: 72, branch_from: 50 }, { number: 74, branch_from: 50 }, { number: 85, branch_from: 80 }];

test('header has visibly separate aligned CI and Review columns without serial stages', () => {
  const html = header([]);
  assert.equal((html.match(/<li /g) || []).length, 8);
  assert.match(html, /ham-track-head"><span class="ham-seg-t">CI<\/span><\/li><li class="ham-seg ham-track-head"><span class="ham-seg-t">Review/);
});

test('both tracks can be active on one PR and preserve their actual actors and sources', () => {
  const item = { kind: 'pr', head: 'head', ci_activity: activity('running', 'CI running', true), review_activity: activity('reviewing', 'Review working · reviewer', true) };
  const html = ['ci', 'review'].map(kind => track(item, kind, {}, false, now)).join('');
  assert.equal((html.match(/data-motion="active"/g) || []).length, 2);
  assert.match(html, /data-track="ci"/);
  assert.match(html, /data-track="review"/);
  assert.match(html, /reviewer/);
  assert.match(html, /observed-test-source/);
  assert.match(html, /since 2026-11-02T20:59:00.000Z/);
});

test('CI green/review findings and review approved/CI red remain independent', () => {
  const item = { head: 'head', ci_activity: activity('passing', 'CI passed'), review_activity: activity('changes_requested', 'Review findings · awaiting author') };
  assert.match(track(item, 'ci', {}, false, now), /ham-activity-track done/);
  assert.match(track(item, 'review', {}, false, now), /data-activity-state="changes_requested"/);
  item.ci_activity = activity('failing', 'CI failing · build');
  item.review_activity = activity('approved', 'Review approved');
  assert.match(track(item, 'ci', {}, false, now), /data-activity-state="failing"/);
  assert.match(track(item, 'review', {}, false, now), /ham-activity-track done/);
});

test('unassigned, old-head, stale, cached unknown timestamps never fabricate working', () => {
  const item = { head: 'head', author: 'not-a-reviewer', review_activity: activity('unassigned', 'Review queued · unassigned') };
  assert.doesNotMatch(track(item, 'review', {}, false, now), /not-a-reviewer|data-motion/);
  item.review_activity = activity('reviewing', 'Review working · reviewer', true);
  item.head = 'new-head';
  assert.doesNotMatch(track(item, 'review', {}, false, now), /reviewer|data-motion/);
  item.head = 'head';
  assert.doesNotMatch(track(item, 'review', {}, false, now + 300000), /data-motion/);
  assert.match(track(item, 'review', {}, false, now + 300000), /stale/);
  delete item.review_activity.observed_at;
  assert.doesNotMatch(track(item, 'review', {}, false, now), /data-motion/);
  assert.match(track(item, 'review', {}, false, now), /observation time unknown/);
});

test('repair display projection returns to PR progress then current-head tracks; native gates stay intact', () => {
  const item = { kind: 'pr', head: 'head', stage: 70, display_column: 30, code_activity: activity('fixing', 'Fixing · fixer', true) };
  assert.equal(getAnchor(item.stage, stages, item.kind, item), 30);
  assert.equal(item.stage, 70);
  item.display_column = 50;
  assert.equal(getAnchor(item.stage, stages, item.kind, item), 50);
  for (const stage of [71, 72, 74]) assert.equal(getAnchor(stage, stages, 'pr', { head: 'head', display_column: 30, code_activity: activity('waiting_owner', 'Waiting owner') }), 30);
  assert.equal(getAnchor(70, stages, 'pr', { head: 'new-head', display_column: 30, code_activity: activity('fixing', 'Fixing · fixer', true) }), 50, 'an old-head projection cannot return a new head to coding');
  assert.equal(getAnchor(85, stages, 'pr'), 80);
  assert.equal(getAnchor(110, stages, 'pr', { display_column: 30 }), 110);
  assert.equal(getAnchor(50, stages, 'pr'), 50, 'CI retry does not return to coding');
  assert.equal(getAnchor(70, stages, 'pr'), 50, '070 without source observation does not manufacture coding');
});

test('linked PRs carry independent evidence and hidden issue columns stay aligned', () => {
  const layout = fs.readFileSync(path.join(root, 'tracker-row-layout.js'), 'utf8');
  const linked = new Function('stageTrackerLinkedPROpen', 'STAGE_TRACKER_TERMINAL', fn(layout, 'stageTrackerLinkedPRItem') + ';return stageTrackerLinkedPRItem;')(() => true, 110);
  const lp = { number: 7, head: 'head', stage: 70, display_column: 30, ci_activity: activity('running', 'CI running', true), review_activity: activity('reviewing', 'Review working · reviewer', true), code_activity: activity('fixing', 'Fixing · fixer', true) };
  const item = linked({ repo: 'org/repo', number: 1, linked_prs: [lp, { number: 8 }] }, lp);
  assert.equal(item.number, 7);
  assert.equal(item.display_column, 30);
  assert.equal(item.review_activity, lp.review_activity);
  assert.deepEqual(item.siblings, [8]);
  assert.equal(track(item, 'ci', {}, true, now), '<li class="ham-seg blank" data-track="ci" data-route-state="blank" aria-hidden="true"></li>');
  const render = fs.readFileSync(path.join(root, 'tracker-row-render.js'), 'utf8');
  assert.match(render, /ci_activity: lp.ci_activity \|\| null, review_activity: lp.review_activity \|\| null/);
});

test('real source repair can show three concurrent activities without naming a generic worker as reviewer', () => {
  const row = fs.readFileSync(path.join(root, 'tracker-row.js'), 'utf8');
  const definitions = globals + `
const STAGE_TRACKER_HUMAN_GATED = 85, STAGE_TRACKER_READY_FOR_HUMAN_MERGE = 90, STAGE_TRACKER_ADOPTED_BY_HIVE = 74;
const STAGE_TRACKER_ACTIVE_WORD = {70: 'Fixing'};
const STAGE_TRACKER_MOTION_ACTIVE = 'active', STAGE_TRACKER_MOTION_STATIC = 'static';
function stageTrackerSentinelInLane() { return false; }
function stageTrackerActorKind() { return ''; }
function stageTrackerActorTip() { return ''; }
function stageTrackerMotion() { return 'active'; }
function stageTrackerIcon(n, cls) { return '<svg class="' + cls + '"></svg>'; }
` + anchor + fn(row, 'stageTrackerSegments') + fn(strip, 'stageTrackerActivityTrackHtml') + fn(strip, 'stageTrackerStripItems');
  const render = new Function('esc', definitions + ';return stageTrackerStripItems;')(esc);
  const item = { kind: 'pr', stage: 70, head: 'head', display_column: 30,
    ci_activity: activity('running', 'CI running', true),
    review_activity: activity('reviewing', 'Review working · reviewer', true),
    code_activity: activity('fixing', 'Fixing · fixer', true),
  };
  const html = render(item, [{ number: 30, short: 'PR in progress' }, { number: 50, short: 'CI / Review' }, { number: 70, short: 'Fixing', branch_from: 50 }], null, now, {});
  assert.equal((html.match(/<li /g) || []).length, 8, 'one PR strip, seven visible columns plus history');
  assert.equal((html.match(/data-motion="active"/g) || []).length, 3);
  assert.match(html, /ham-detour[^>]*>Fixing · fixer/);
  assert.match(html, /data-track="review"[^]*Review working · reviewer/);
  item.code_activity.observed_at = new Date(now - 300000).toISOString();
  const stale = render(item, stages, null, now, {});
  assert.match(stale, /Fix task assigned · activity unknown/);
  assert.equal((stale.match(/data-motion="active"/g) || []).length, 2, 'stale coding cannot spin beside live CI and review');
  item.code_activity.observed_at = new Date(now).toISOString();
  item.code_activity.source = 'fix-dispatch+current-worker';
  const administration = render(item, stages, null, now, {});
  assert.match(administration, /Fix task assigned · activity unknown/);
  assert.equal((administration.match(/data-motion="active"/g) || []).length, 2, 'cached generic PR administration cannot animate coding');
});

test('stale activity is visibly unknown and old green observations cannot look live', () => {
  const item = { kind: 'pr', head: 'head', review_activity: activity('reviewing', 'Review working · reviewer', true) };
  const html = track(item, 'review', {}, false, now + 300000);
  assert.match(html, /activity unknown · last observed reviewing/);
  assert.doesNotMatch(html, /Review working|data-motion/);
  item.review_activity = activity('approved', 'Review approved');
  assert.doesNotMatch(track(item, 'review', {}, false, now + 300000), /ham-activity-track done/);
});

test('current-head evidence links are accessible and never retained across a new head', () => {
  const item = { kind: 'pr', head: 'head', ci_activity: {...activity('failing', 'CI failing · build'), url: 'https://github.com/org/repo/actions/runs/1'} };
  assert.match(track(item, 'ci', {state: 'active'}, false, now), /aria-label="Open current-head CI evidence"/);
  assert.match(track(item, 'ci', {state: 'active'}, false, now), /aria-current="step"/);
  item.head = 'new-head';
  assert.doesNotMatch(track(item, 'ci', {}, false, now), /href=|actions\/runs\/1/);
});

test('a fix request or CI-only wait is visible without manufacturing source work', () => {
  const item = { kind: 'pr', head: 'head', stage: 70, display_column: 50,
    code_activity: activity('waiting_fixer', 'Waiting for fixer · no code work observed') };
  const waiting = track(item, 'review', {state:'active'}, false, now);
  assert.match(waiting, /ham-track-code-note">Waiting for fixer/);
  assert.doesNotMatch(waiting, /data-motion/);
  item.code_activity = activity('waiting_ci', 'Fix task waiting on CI · not observed coding');
  assert.match(track(item, 'ci', {}, false, now), /waiting on CI · not observed coding/);
  assert.equal(getAnchor(70, stages, 'pr', item), 50);
});

test('linked movement uses current-head repair evidence while native sort stays unchanged', () => {
  const motion = fs.readFileSync(path.join(root, 'tracker-motion.js'), 'utf8');
  const map = new Function('stageTrackerLinkedSorted', 'stageTrackerItemKey', globals + 'const STAGE_TRACKER_CAP_COLUMN = 7;' +
    anchor + fn(motion, 'stageTrackerColIndex') + fn(motion, 'stageTrackerColMap') + ';return stageTrackerColMap;')(
      item => item.linked_prs || [], item => item.kind + '#' + item.number);
  const moves = new Function(fn(motion, 'stageTrackerColMoves') + ';return stageTrackerColMoves;')();
  const pr = {number:7, stage:70, head:'head', display_column:30, code_activity:activity('fixing','Fixing · fixer',true)};
  const items = [{number:1, kind:'issue', linked_prs:[pr]}];
  const fixed = map(items, stages);
  assert.equal(fixed['issue#1'], 3);
  assert.equal(fixed['pr#7'], 3);
  assert.equal(getAnchor(70, stages, 'pr', pr, false), 50, 'default native sorting is unchanged');
  assert.deepEqual(moves(null, fixed), {}, 'the first observation cannot manufacture a bounce');
  pr.head = 'new-head';
  pr.display_column = 50;
  pr.code_activity = {head:'new-head',state:'pushed'};
  const pushed = map(items, stages);
  assert.deepEqual(moves(fixed, pushed), {'issue#1':{from:3,to:4}, 'pr#7':{from:3,to:4}});
});
