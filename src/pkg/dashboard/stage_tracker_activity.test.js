const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { pathToFileURL } = require('node:url');

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
function constLine(source, name) {
  const match = source.match(new RegExp('const ' + name + ' = [^\\n]*;'));
  assert.ok(match, name);
  return match[0];
}
const trackConsts = ['STAGE_TRACKER_WORKFLOW_APPROVAL_BLOCKER', 'STAGE_TRACKER_WORKFLOW_APPROVAL_TIP', 'STAGE_TRACKER_RING_SEGMENT_GAP_MAX', 'STAGE_TRACKER_RING_RADIUS', 'STAGE_TRACKER_RING_GAP_DEG'].map(name => constLine(strip, name)).join('\n');
const trackFns = ['stageTrackerHiveOwesReReview', 'stageTrackerTrackLead', 'stageTrackerWaitingOnPerson', 'stageTrackerTrackRunning', 'stageTrackerWorkflowApprovalPending', 'stageTrackerRingSvg', 'stageTrackerStalledTrack', 'stageTrackerQueueAheadWord', 'stageTrackerStageTrack', 'stageTrackerHiveOwnsRepair', 'stageTrackerTrackWord', 'stageTrackerActivityTrackHtml'].map(name => fn(strip, name)).join('\n');
const track = new Function('esc', 'stageTrackerIcon', trackConsts + '\n' + trackFns + ';return stageTrackerActivityTrackHtml;')(esc, (n, cls) => `<svg class="${cls}" data-icon="${n}"></svg>`);
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
  const item = { kind: 'pr', head: 'head', ci_activity: activity('running', 'CI running · 1/2 checks done', true), review_activity: activity('reviewing', 'Review working · reviewer', true) };
  const html = ['ci', 'review'].map(kind => track(item, kind, {}, false, now)).join('');
  assert.equal((html.match(/data-motion="active"/g) || []).length, 2);
  assert.match(html, /data-track="ci"/);
  assert.match(html, /data-track="review"/);
  assert.match(html, /reviewer/);
  assert.match(html, /observed-test-source/);
  assert.match(html, /since 2026-11-02T20:59:00.000Z/);
});

test('CI green/review findings and review approved/CI red remain independent', () => {
  const item = { head: 'head', ci_activity: activity('passing', 'CI green · all 2 required checks passed'), review_activity: activity('changes_requested', 'Review findings · awaiting author') };
  assert.match(track(item, 'ci', {}, false, now), /ham-activity-track done/);
  assert.match(track(item, 'review', {}, false, now), /data-activity-state="changes_requested"/);
  item.ci_activity = activity('failing', 'CI failing · "build" red');
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
  const lp = { number: 7, head: 'head', stage: 70, display_column: 30, ci_activity: activity('running', 'CI running · 1/2 checks done', true), review_activity: activity('reviewing', 'Review working · reviewer', true), code_activity: activity('fixing', 'Fixing · fixer', true) };
  const item = linked({ repo: 'org/repo', number: 1, linked_prs: [lp, { number: 8 }] }, lp);
  assert.equal(item.number, 7);
  assert.equal(item.display_column, 30);
  assert.equal(item.review_activity, lp.review_activity);
  assert.deepEqual(item.siblings, [8]);
  assert.equal(track(item, 'ci', {}, true, now), '<li class="ham-seg blank" data-track="ci" data-route-state="blank" aria-hidden="true"></li>');
  const render = fs.readFileSync(path.join(root, 'tracker-row-render.js'), 'utf8');
  assert.match(render, /ci_activity: lp.ci_activity \|\| null, review_activity: lp.review_activity \|\| null/);
});

const rowSource = fs.readFileSync(path.join(root, 'tracker-row.js'), 'utf8');
const segments = new Function('stageTrackerAnchor', globals + `
const STAGE_TRACKER_ACTIVE_WORD = {70: 'Fixing'};
const STAGE_TRACKER_HUMAN_GATED = 85, STAGE_TRACKER_READY_FOR_HUMAN_MERGE = 90, STAGE_TRACKER_ADOPTED_BY_HIVE = 74;
function stageTrackerSentinelInLane() { return false; }
function stageTrackerUnauthorizedWord() { return ''; }
function stageTrackerManualMergeWord() { return 'Ready for human merge'; }
` + fn(rowSource, 'stageTrackerSegments') + ';return stageTrackerSegments;')(getAnchor);
const makeStripRenderer = (helpers = {}) => new Function('esc', 'stageTrackerSegments', 'stageTrackerActivityTrackHtml', ...Object.keys(helpers), globals + `
const STAGE_TRACKER_MOTION_STATIC = 'static', STAGE_TRACKER_MOTION_ACTIVE = 'active';
const STAGE_TRACKER_READY_FOR_HUMAN_MERGE = 90;
function stageTrackerActorKind() { return ''; }
function stageTrackerActorTip() { return ''; }
function stageTrackerMotion() { return STAGE_TRACKER_MOTION_STATIC; }
function stageTrackerIcon(n) { return '<svg data-icon="' + n + '"></svg>'; }
` + fn(strip, 'stageTrackerWaitingOnPerson') + fn(strip, 'stageTrackerTrackRunning') + fn(strip, 'stageTrackerStageTrack') + fn(strip, 'stageTrackerStalledTrack') + fn(strip, 'stageTrackerStripItems') + ';return stageTrackerStripItems;')(esc, segments, track, ...Object.values(helpers));
const renderStrip = makeStripRenderer();

function physicalTracks(item) {
  const rendered = renderStrip(item, stages, null, now, {});
  const cells = [...rendered.matchAll(/<li\b[^]*?<\/li>/g)].map(match => match[0]);
  assert.equal(cells.length, 8, 'actual strip expands native 050 into two physical tracks plus history');
  assert.match(cells[4], /data-track="ci"/);
  assert.match(cells[5], /data-track="review"/);
  return { rendered, cells, ci: cells[4], review: cells[5] };
}
const livePR = extra => ({ kind: 'pr', head: 'head', stage: 50, modifiers: [], ...extra });

const actions = fs.readFileSync(path.join(root, 'tracker-actions.js'), 'utf8');

test('remediation wording preserves native identifiers, source names and waiting semantics', () => {
  for (const caption of ['Fixing · FixingBot', 'Remediating · FixingBot']) {
    const code = activity('fixing', caption, true);
    const item = livePR({ stage: 70, display_column: 30, title: 'Fixing a widget — public PR title', code_activity: code });
    const current = physicalTracks(item).cells[3];
    assert.match(current, /Remediating · FixingBot/);
    assert.match(current, /data-motion="active"/);
    assert.equal(code.state, 'fixing', 'internal state stays stable');
    assert.equal(code.caption, caption, 'rendering does not rewrite recorded source evidence');
    assert.equal(item.title, 'Fixing a widget — public PR title');
    code.observed_at = new Date(now - 300000).toISOString();
    const stale = physicalTracks(item).cells[3];
    assert.doesNotMatch(stale, /data-motion="active"/);
    assert.match(stale, /Fix task assigned · activity unknown/);
    assert.doesNotMatch(stale, /ham-detour[^>]*>Remediating/);
  }
});

test('12:01 screenshot density preserves physical tracks for issue-only, one-to-one, multi-PR and PR-only rows', () => {
  const fixture = JSON.parse(fs.readFileSync(path.join(__dirname, 'webstatic/testdata/tracker-activity-density.json'), 'utf8'));
  const clock = Date.parse(fixture.clock);
  const fixtureStages = [...stages, { number: 90, short: 'Ready for human merge', branch_from: 80 }];
  for (const row of fixture.rows) {
    const linked = row.linked_prs || [];
    const items = linked.length === 1 ? [{ ...linked[0], kind: 'pr', repo: row.repo }] :
      [row, ...linked.map(pr => ({ ...pr, kind: 'pr', repo: row.repo }))];
    for (const item of items) {
      const range = item === row && linked.length > 1 ? { to: 20 } : item !== row && linked.length > 1 ? { from: 30 } : null;
      const rendered = renderStrip(item, fixtureStages, range, clock, {});
      const cells = [...rendered.matchAll(/<li\b[^]*?<\/li>/g)].map(match => match[0]);
      assert.equal(cells.length, 8, 'all row structures retain seven lanes plus history');
      if (range?.from) {
        assert.ok(cells.slice(0, 3).every(cell => /class="ham-seg blank"/.test(cell)), 'nested PRs reserve the physical Open–Claimed slots');
      }
      if (range?.to) {
        assert.ok(cells.slice(3).every(cell => /class="ham-seg blank/.test(cell)), 'multi-PR issue owns no PR-side activity');
      }
      if (item.number === 344) {
        assert.match(cells[4], /active ham-current[^]*CI \?\/\? · queued</);
        assert.match(cells[5], /done ham-current/);
      }
      if (item.code_activity?.state === 'waiting_owner') {
        assert.ok(cells[3].includes(item.code_activity.caption));
        assert.doesNotMatch(rendered, /data-motion="active"/, 'waiting for an author is not observed coding');
      }
      for (const cell of cells.filter(cell => /data-display-state="unknown"/.test(cell))) {
        const content = cell.match(/aria-describedby="[^"]+">([^]*?)<span id=/)[1];
        assert.match(content, /<svg /, 'missing evidence keeps the track glyph');
        const words = content.replace(/<[^>]*>/g, '').trim();
        if (/ham-current/.test(cell)) assert.match(words, /^(CI|Review)\b/, 'a track in the current phase always carries count words (#16484)');
        else assert.equal(words, '', 'a track not started on this head is the glyph alone');
      }
    }
  }
});

test('merge-gate claim decisions wrap the complete gate caption and use only their existing decision dialog', () => {
  const gate = { word: 'Claim conflict · @' + 'unbrokenclaimanthandle'.repeat(12), detail: 'needs a pick', tip: 'Two claimants need a human decision' };
  const decisionOf = new Function('stageTrackerItemKey', fn(actions, 'stageTrackerDecisionOf') + ';return stageTrackerDecisionOf;')(item => item.repo + '/' + item.kind + '#' + item.number);
  const trigger = new Function('esc', fn(actions, 'stageTrackerDecisionTriggerHtml') + ';return stageTrackerDecisionTriggerHtml;')(esc);
  const gateSource = fs.readFileSync(path.join(root, 'tracker-words-gate.js'), 'utf8');
  const gateWord = new Function('esc', 'stageTrackerMergeGateInfo', fn(gateSource, 'stageTrackerMergeGateWordHtml') + ';return stageTrackerMergeGateWordHtml;')(esc, () => gate);
  const render = makeStripRenderer({
    stageTrackerDecisionOf: decisionOf, stageTrackerDecisionTriggerHtml: trigger,
    stageTrackerMergeGateInfo: () => gate, stageTrackerMergeGateWordHtml: gateWord,
  });
  const item = livePR({
    repo: 'hivecommons/hive', number: 7, stage: 80, reason: 'Unabridged claim conflict reason',
    decision_needed: { question: 'Which claimant should continue?', blocker: 'Do not merge until the claim conflict is resolved' },
  });
  const cells = [...render(item, stages, null, now, {}).matchAll(/<li\b[^]*?<\/li>/g)].map(match => match[0]);
  const cell = cells[6];
  assert.match(cell, /^<li class="ham-seg active"[^>]*><span class="ham-chevron-info ham-chevron-decision">/);
  assert.match(cell, /ham-chevron-copy"><button type="button" class=" ham-decision-trigger"/);
  assert.match(cell, /data-ham-decision="hivecommons\/hive\/pr#7"/);
  assert.match(cell, /aria-haspopup="dialog" aria-expanded="false"><span class="ham-seg-t">/);
  assert.ok(cell.includes(gate.word) && cell.includes('· needs a pick</span>'), 'gate words and detail remain inside the decision button');
  assert.equal((cell.match(/<button /g) || []).length, 1, 'one native decision trigger, not duplicate actions');
  assert.doesNotMatch(cell, /config-info|config-tooltip|role="tooltip"|tabindex=| title=/, 'no competing tooltip, native title, or extra focus stop');
  const tip = cell.match(/data-ham-tip="([^"]+)"/)[1];
  for (const full of [gate.word, gate.detail, gate.tip, item.reason, 'head head']) assert.ok(tip.includes(full), full + ' remains in overflow details');
  const pop = new Function('esc', 'stageTrackerRefOf', 'agentNowAgo', fn(actions, 'stageTrackerDecisionPopHtml') + ';return stageTrackerDecisionPopHtml;')(esc, it => it.repo + '#' + it.number, () => 'just now');
  const dialog = pop(item, [{ id: 'decide:direction', label: 'Give direction', enabled: true }, { id: 'hold', label: 'Hold', enabled: true }], now, tip);
  assert.match(dialog, /role="dialog"/);
  assert.match(dialog, /data-ham-act="decide:direction"/);
  assert.match(dialog, /data-ham-more="1"/);
  const details = dialog.match(/<details class="ham-decision-details"><summary>Details<\/summary>([^]*?)<\/details>/)[1];
  for (const full of [gate.word, gate.detail, item.reason, item.decision_needed.blocker, 'head head']) assert.ok(details.includes(full), full + ' is keyboard-accessible without a second popup');
  const css = fs.readFileSync(path.join(__dirname, 'static-src/css/tracker.css'), 'utf8');
  assert.match(css, /\.ham-chevron-copy > \.ham-decision-trigger \{[^}]*-webkit-line-clamp: var\(--ham-chevron-text-lines\);[^}]*white-space: normal; overflow-wrap: anywhere;/);
});

test('decision pointer, focus and keyboard delegation retain the same accessible action path', () => {
  const opened = [];
  const wire = new Function('stageTrackerOpenDecisionPop', 'stageTrackerDecisionPopLeave', 'stageTrackerCloseDecisionPop', 'const _stageTrackerDecisionPop = null;\n' + fn(actions, 'stageTrackerWireDecisionPop') + ';return stageTrackerWireDecisionPop;')((button, pin) => opened.push({ button, pin }), () => {}, () => {});
  const listeners = {};
  const button = { dataset: { hamDecision: 'hivecommons/hive/pr#7' }, closest: selector => selector === '.ham-decision-trigger' ? button : null };
  const host = { addEventListener: (type, handler) => { listeners[type] = handler; }, contains: target => target === button };
  wire(host);
  const event = extra => ({ target: button, pointerType: 'mouse', preventDefault() { this.prevented = true; }, stopPropagation() {}, ...extra });
  listeners.pointerover(event({ pointerType: 'touch' }));
  assert.equal(opened.length, 0, 'touch does not open a hover-only dialog');
  listeners.pointerover(event());
  assert.deepEqual(opened.pop(), { button, pin: false }, 'pointer opens the existing hover dialog');
  listeners.focusin(event());
  assert.deepEqual(opened.pop(), { button, pin: true }, 'keyboard focus pins the same dialog');
  for (const key of ['ArrowDown', 'Enter', ' ']) {
    const e = event({ key });
    listeners.keydown(e);
    assert.ok(e.prevented);
    assert.deepEqual(opened.pop(), { button, pin: { focus: true } }, key + ' moves focus into the existing dialog actions');
  }
  listeners.click(event());
  assert.deepEqual(opened.pop(), { button, pin: { focus: true } }, 'pointer activation preserves the native decision action path');
  let hidden = 0;
  let actionFocused = 0;
  const action = { focus(options) {
    assert.deepEqual(options, { preventScroll: true }, 'keyboard entry cannot generate the scroll event that closes its own decision dialog');
    actionFocused++;
  } };
  const dialog = { querySelector() { return action; } };
  const openExisting = new Function('hideConfigTooltip', 'dialog', 'let _stageTrackerDecisionPop = { key: "hivecommons/hive/pr#7", timer: 0, el: dialog };\n' + fn(actions, 'stageTrackerOpenDecisionPop') + ';return stageTrackerOpenDecisionPop;')(() => { hidden++; }, dialog);
  openExisting(button, false);
  openExisting(button, true);
  assert.equal(hidden, 2, 'opening or focusing even an existing decision cancels stale/pending shared tooltips');
  for (const key of ['ArrowDown', 'Enter', ' ']) {
    openExisting(button, { focus: true });
    assert.ok(actionFocused > 0, key + ' enters an already-open hover/focus dialog');
  }
  assert.equal(actionFocused, 3, 'existing-dialog fast path honors each keyboard focus request');
});

test('canonical activity browser wiring is retained (not browser/visual proof)', () => {
  const harness = fs.readFileSync(path.join(__dirname, '../../scripts/dashboard-mobile-shots.mjs'), 'utf8');
  const fixture = fs.readFileSync(path.join(__dirname, '../../scripts/fixtures/dashboard-activity-density.mjs'), 'utf8');
  assert.match(harness, /ACTIVITY_DESKTOP_WIDTHS = \[2400, 1440, 1110, 1024\]/);
  for (const name of ['tracker-header-geometry', 'tracker-decision-interaction', 'native-CDP-pointer-keyboard']) {
    assert.ok(harness.includes(name), name + ' stays wired into the existing canonical browser run');
  }
  assert.match(harness, /Input\.dispatchMouseEvent/);
  assert.match(harness, /Input\.dispatchKeyEvent/);
  assert.match(harness, /Emulation\.setFocusEmulationEnabled', \{ enabled: true \}/);
  assert.match(harness, /documentFocused: document\.hasFocus\(\), helpFocused: document\.activeElement === help/);
  assert.match(harness, /captureBeyondViewport: !viewportOnly/);
  assert.match(harness, /x: scrollX, y: scrollY, width: innerWidth, height: innerHeight/);
  assert.match(harness, /getImageData\(0, 0, width, height\)/);
  assert.match(harness, /probe: 'viewport-paint', ok: capture\.painted/);
  assert.match(harness, /if \(!capture\.painted\) activityFailures\.push/);
  assert.match(harness, /failures\.push\(\.\.\.pointerCapture\.failures\)/);
  assert.match(harness, /failures\.push\(\.\.\.keyboardCapture\.failures\)/);
  assert.match(harness, /if \(!point\.triggerReachable\) failures\.push/);
  assert.match(harness, /resetDashboardFixtureOverlays\.toString\(\)/);
  assert.match(harness, /const pageReadiness = await renderReady\(\)/);
  assert.match(harness, /const readiness = await renderReady\(\)/);
  assert.match(harness, /provenance, pageReadiness, fixtureCleanup, fixture, results/);
  assert.match(harness, /screenshot\(prefix \+ '-decision-pointer\.png', true\)/);
  assert.match(harness, /screenshot\(prefix \+ '-decision-keyboard\.png', true\)/);
  assert.match(harness, /every native decision action must be reachable and unobscured/);
  assert.match(harness, /elementFromPoint/);
  assert.match(harness, /activity-density-viewport\.png`, true\)/);
  assert.match(harness, /preparationRailLayoutFixture\.toString/);
  assert.match(harness, /if \(activityFailures\.length\) throw new Error/);
  assert.match(harness, /activity-probes\.json/);
  assert.match(harness, /generatedAssetSHA256/);
  assert.match(fixture, /stageTrackerRowHtml\(item, stages, ctx\)/);
  assert.match(fixture, /stageTrackerRowNode\(row\.key, row\.html\)/);
  assert.match(fixture, /stageTrackerPreparationVisualSync/);
  assert.match(fixture, /stageTrackerPreparationInlineRows/);
  assert.match(fixture, /stageTrackerWireRowActions\(panel\)/);
  assert.match(fixture, /fix-dispatch\+successful-edit\+verified-git-worktree/);
  assert.match(fixture, /__activityDensityCleanup/);
});

function simulatedBrowser() {
  let time = 0, sequence = 0, locked = false;
  const frames = new Map(), timers = new Map(), overlays = [];
  const style = { opacity: '0', visibility: 'visible', display: 'block' };
  const classList = { contains: name => name === 'modal-open' && locked };
  const body = { classList, getBoundingClientRect: () => ({ width: 1440, height: 2000 }) };
  const context = {
    document: { body, documentElement: { classList }, readyState: 'complete',
      visibilityState: 'visible', querySelectorAll: () => overlays },
    getComputedStyle: node => node === body ? style : { opacity: '1', visibility: 'visible', display: 'block' },
    performance: { now: () => time },
    requestAnimationFrame: callback => { const id = ++sequence; frames.set(id, callback); return id; },
    cancelAnimationFrame: id => frames.delete(id),
    setTimeout: callback => { const id = ++sequence; timers.set(id, callback); return id; },
    clearTimeout: id => timers.delete(id),
    hiveOverlayIsOpen: overlay => overlay.open,
    hiveDismissTopOverlay: () => {
      const overlay = overlays.find(candidate => candidate.open);
      if (!overlay) return false;
      overlay.open = false;
      return true;
    },
  };
  return { context, style, overlays, frames, timers,
    lock(value) { locked = value; },
    frame() {
      time += 16;
      const callbacks = [...frames.values()]; frames.clear();
      callbacks.forEach(callback => callback(time));
    },
    expire() {
      time += 10000;
      const callbacks = [...timers.values()]; timers.clear();
      callbacks.forEach(callback => callback());
    },
  };
}

test('canonical paint readiness waits for real opacity and stable frames without changing page styles', async () => {
  const helpers = await import(pathToFileURL(path.join(__dirname, '../../scripts/fixtures/dashboard-browser-readiness.mjs')).href);
  assert.throws(() => helpers.waitForDashboardPaint(), /dedicated preview/);
  const browser = simulatedBrowser();
  const ready = vm.runInNewContext('(' + helpers.waitForDashboardPaint.toString() + ')', browser.context);
  const pending = ready({ dedicatedPreview: true });
  browser.frame();
  assert.equal(browser.frames.size, 1, 'body opacity zero cannot pass');
  browser.style.opacity = '0.5';
  browser.frame();
  assert.equal(browser.frames.size, 1, 'the boot fade-in must finish, not merely start');
  browser.style.opacity = '1';
  browser.frame();
  assert.equal(browser.frames.size, 1, 'one visible frame cannot pass');
  browser.style.visibility = 'hidden';
  browser.frame();
  browser.style.visibility = 'visible';
  browser.frame();
  assert.equal(browser.frames.size, 1, 'hidden frames reset consecutive readiness');
  browser.frame();
  const result = await pending;
  assert.equal(result.ready, true);
  assert.equal(result.stableFrames, 2);
  assert.equal(result.state.opacity, '1');
  assert.equal(browser.frames.size, 0);
  assert.equal(browser.timers.size, 0);
  assert.equal(browser.context.document.body.style, undefined, 'readiness never forces body styles');
});

test('render readiness has a real deadline even when animation frames never arrive', async () => {
  const helpers = await import(pathToFileURL(path.join(__dirname, '../../scripts/fixtures/dashboard-browser-readiness.mjs')).href);
  for (const sample of [false, true]) {
    const browser = simulatedBrowser();
    const ready = vm.runInNewContext('(' + helpers.waitForDashboardPaint.toString() + ')', browser.context);
    const pending = ready({ dedicatedPreview: true });
    if (sample) browser.frame();
    browser.expire();
    await assert.rejects(pending, error => /render readiness timed out/.test(error.message) &&
      (!sample || /"opacity":"0"/.test(error.message)));
    assert.equal(browser.frames.size, 0, 'timeout cancels a pending animation frame');
    assert.equal(browser.timers.size, 0);
    assert.equal(browser.style.opacity, '0', 'timeout must not turn invisible content into a pass');
  }
});

test('native fixture reset closes every legacy modal and waits for the production scroll-lock observer', async () => {
  const helpers = await import(pathToFileURL(path.join(__dirname, '../../scripts/fixtures/dashboard-browser-readiness.mjs')).href);
  await assert.rejects(helpers.resetDashboardFixtureOverlays(), /dedicated preview/);
  const browser = simulatedBrowser();
  browser.overlays.push({ id: 'terminal-export', open: true }, { id: 'kb-modal-overlay', open: true }, { id: 'config-overlay', open: true });
  browser.lock(true);
  const reset = vm.runInNewContext('(' + helpers.resetDashboardFixtureOverlays.toString() + ')', browser.context);
  const pending = reset({ dedicatedPreview: true });
  assert.ok(browser.overlays.every(overlay => !overlay.open), 'all three legacy producers use native dismissal');
  browser.lock(false);
  browser.frame();
  const result = await pending;
  assert.equal(result.dismissals, 3);
  assert.equal(result.remaining.length, 0);
  assert.equal(result.scrollLocked, false);
  assert.equal(browser.timers.size, 0);
  const source = helpers.resetDashboardFixtureOverlays.toString();
  assert.doesNotMatch(source, /\.remove\(|classList\.(?:add|remove)|style\.\w+\s*=/, 'no raw modal deletion, hiding, or manual scroll-lock override');
});

test('native cleanup rejects undispatched overlays, stuck dismissal loops, scroll locks and stalled observer frames', async () => {
  const helpers = await import(pathToFileURL(path.join(__dirname, '../../scripts/fixtures/dashboard-browser-readiness.mjs')).href);
  for (const failure of ['overlay', 'bound', 'lock', 'frame']) {
    const browser = simulatedBrowser();
    let calls = 0;
    browser.context.hiveDismissTopOverlay = () => { calls++; return failure === 'bound'; };
    if (failure === 'overlay') browser.overlays.push({ id: 'kb-modal-overlay', open: true });
    if (failure === 'lock') browser.lock(true);
    const reset = vm.runInNewContext('(' + helpers.resetDashboardFixtureOverlays.toString() + ')', browser.context);
    const pending = reset({ dedicatedPreview: true, maxDismissals: 3 });
    if (failure === 'frame') browser.expire(); else browser.frame();
    await assert.rejects(pending, error => {
      const expected = { overlay: 'kb-modal-overlay', bound: 'dismissal bound', lock: '"scrollLocked":true', frame: 'observer did not settle' };
      return error.message.includes(expected[failure]);
    });
    assert.ok(calls <= 3, 'native cleanup is bounded');
    assert.equal(browser.frames.size, 0);
    assert.equal(browser.timers.size, 0);
  }
});

test('browser probes require a dedicated preview and cannot count offscreen header controls as success', async () => {
  for (const name of ['tracker-header-geometry.js', 'tracker-decision-interaction.js']) {
    const source = fs.readFileSync(path.join(__dirname, 'webstatic/testdata', name), 'utf8');
    const probe = new Function('return (\n' + source + '\n);')();
    await assert.rejects(probe(), /dedicated published-preview tab/, 'refuse to change scroll/focus without an explicit dedicated-preview opt-in');
    if (name === 'tracker-header-geometry.js') {
      assert.match(source, /entire header control stays horizontally inside the toolbar and viewport/);
      assert.match(source, /control\.scrollIntoView/);
      assert.match(source, /coverage\.controlTargets === coverage\.hitTested/);
      assert.match(source, /await settleScroll\(control, name\)/);
      assert.match(source, /control\/visual viewport must settle before its mandatory hit-test/);
      assert.doesNotMatch(source, /if \(x >= 0|if \(!visible\(control\)\) continue/);
    } else {
      assert.match(source, /new PointerEvent\('pointerover'/);
      assert.match(source, /new MouseEvent\('mouseover'/);
      assert.match(source, /trigger\.focus/);
      assert.match(source, /\['ArrowDown', 'Enter', ' '\]/);
      assert.match(source, /pointer decision opening immediately cancels/);
    }
  }
});

test('Hotshot #344 shape: queued CI and approved review retain current phase without pretending work runs', () => {
  const item = livePR({ stage: 60, ci_activity: activity('queued', 'CI queued · "build" waiting for a runner'), review_activity: activity('approved', 'Review approved') });
  const before = JSON.stringify(item);
  const cells = physicalTracks(item);
  assert.match(cells.ci, /ham-activity-track active ham-current/, 'queued CI is in progress: the full active fill, no pulse');
  assert.match(cells.ci, /ham-track-state"><svg [^>]*><\/svg>CI \?\/\? · queued</);
  assert.match(cells.review, /ham-activity-track done ham-current/);
  assert.match(cells.review, /data-verdict="pass"[^]*ham-track-verdict"><svg [^>]*><\/svg><\/span>/, 'an approved review is the glyph alone');
  assert.match(cells.review, /Review: Review approved · /, 'the words move to the tooltip');
  for (const cell of [cells.ci, cells.review]) {
    assert.match(cell, /aria-current="step"/);
    assert.match(cell, /aria-label="Current pipeline phase/);
    assert.doesNotMatch(cell, /data-motion=/);
  }
  assert.equal(JSON.stringify(item), before, 'rendering does not advance native stage or mutate evidence');
});

test('Hive #16434 shape: missing current-head CI and review stay unknown but visibly occupy the current phase', () => {
  const cells = physicalTracks(livePR({
    display_column: 50,
    code_activity: { head: 'head', state: 'pushed', caption: 'New head pushed · CI and re-review', source: 'recorded-push' },
    ci_activity: { head: 'head', state: 'unknown', caption: 'CI unknown · no check results observed for this head yet', source: 'unavailable' },
    review_activity: { head: 'head', state: 'unknown', caption: 'Review unknown · current-head evidence unavailable', source: 'unavailable' },
  }));
  for (const cell of [cells.ci, cells.review]) {
    assert.match(cell, /data-activity-state="unknown"/);
    assert.match(cell, /data-display-state="unknown"/);
    assert.doesNotMatch(cell, /data-motion=|ham-activity-track done/);
  }
  // #16599: at 050 the Review column is the stage's own track — active
  // with its words even without evidence; CI is the neutral icon alone,
  // its words in the tooltip.
  assert.match(cells.ci, /ham-activity-track todo ham-current/);
  assert.match(cells.ci, /title="Current pipeline phase · CI: CI \?\/\? · no checks reported yet · /);
  assert.doesNotMatch(cells.ci, /no checks reported yet</);
  assert.match(cells.review, /ham-activity-track active ham-current/);
  assert.match(cells.review, /Review · re-review of head</);
  assert.doesNotMatch(cells.cells[3], /aria-current|data-motion/);
});

test('Hive #16268 shape: held native Fixing waits for a fixer in the CI/Review phase, not observed coding', () => {
  const item = livePR({
    stage: 70, modifiers: ['hold'], hold: { by: 'hanthor', reason: 'Maintainer hold' }, display_column: 50,
    code_activity: { head: 'head', state: 'waiting_fixer', caption: 'Waiting for fixer · no code work observed', source: 'native-fix-request' },
    ci_activity: activity('missing', 'CI waiting · required check "test" hasn\'t reported for this head yet'),
    review_activity: activity('approved', 'Review approved'),
  });
  const cells = physicalTracks(item);
  // #16599: nothing moves and the stage's own track (review) has settled,
  // so the unreported CI is the stalled active track: amber-muted, still,
  // with its words — the row's one filled chevron.
  assert.match(cells.ci, /ham-activity-track active ham-stalled ham-current/, 'held: unreported checks are the stalled blocker, not motion');
  assert.doesNotMatch(cells.ci, /data-motion=/);
  assert.match(cells.ci, /CI \?\/\? · no checks reported yet</);
  assert.match(cells.review, /done ham-current/);
  assert.match(cells.review, /data-verdict="pass"[^]*ham-track-verdict"><svg [^>]*><\/svg><\/span>/);
  assert.match(cells.review, /Review: Review approved · /);
  assert.match(cells.review, /Waiting for fixer · no code work observed/);
  assert.match(cells.review, /Held by @hanthor · Maintainer hold/);
  assert.doesNotMatch(cells.rendered, /data-motion=|>(?:Fixing|Remediating) ·/);
  assert.equal(item.stage, 70);
  assert.deepEqual(item.modifiers, ['hold']);
});

test('current phase, running activity, failure and successful completion are independent dimensions', () => {
  for (const [state, caption, running, cls] of [
    ['failing', 'CI failing · "build" red', false, 'failing'],
    ['running', 'CI running · 1/2 checks done', true, 'active'],
    ['passing', 'CI green · all 2 required checks passed', false, 'done'],
    ['skipped', 'CI skipped · check "build" skipped', false, 'todo'],
  ]) {
    const cells = physicalTracks(livePR({ ci_activity: activity(state, caption, running) }));
    assert.ok(cells.ci.includes('ham-activity-track ' + cls + ' ham-current'), state + ' → ' + cls + ': ' + cells.ci);
    assert.equal(cells.ci.includes('data-motion="active"'), running);
    // The stage's own track (review, 050) is active with words (#16599).
    assert.match(cells.review, /active ham-current[^]*data-activity-state="unknown"/);
  }
  const admitted = physicalTracks(livePR({ stage: 80, ci_activity: activity('passing', 'CI green · all 2 required checks passed'), review_activity: activity('approved', 'Review approved') }));
  assert.doesNotMatch(admitted.ci + admitted.review, /ham-current|aria-current/);
  assert.match(admitted.cells[6], /aria-current="step"/, 'merge gate keeps its own current position');
});

test('old-head or stale observations cannot fabricate running/completion, but do not erase current position', () => {
  for (const mode of ['old-head', 'stale', 'missing-time']) {
    const ci = activity('running', 'CI running · 1/2 checks done', true);
    const review = activity('approved', 'Review approved');
    for (const observation of [ci, review]) {
      if (mode === 'old-head') observation.head = 'old-head';
      if (mode === 'stale') observation.observed_at = new Date(now - 300000).toISOString();
      if (mode === 'missing-time') delete observation.observed_at;
    }
    const cells = physicalTracks(livePR({ ci_activity: ci, review_activity: review }));
    for (const cell of [cells.ci, cells.review]) {
      assert.match(cell, /ham-current/);
      assert.doesNotMatch(cell, /data-motion=/, mode + ': no pulse without a fresh observation');
    }
    if (mode === 'old-head') {
      assert.match(cells.review, /data-activity-state="unknown"/);
      assert.match(cells.ci, /todo ham-current[^]*title="Current pipeline phase · CI: CI \?\/\? · no checks reported yet · /, 'old-head evidence counts for nothing on this head');
      assert.doesNotMatch(cells.ci, /no checks reported yet</, 'inactive CI is its icon alone (#16599)');
      assert.doesNotMatch(cells.review, /ham-activity-track done/);
    } else {
      assert.match(cells.ci, /active ham-current[^]*CI \?\/\? · running</, mode + ': the last observed state still reads in words');
      assert.match(cells.review, /done ham-current[^]*data-verdict="pass"[^]*Review: Review approved · /, mode + ': an approval of this head stays done, freshness only in the tooltip');
      assert.match(cells.review, mode === 'stale' ? /stale · / : /observation time unknown/);
    }
  }
});

test('genuine current-head source work still lights PR in progress; fix assignment alone does not', () => {
  const code = activity('fixing', 'Fixing · observed-fixer', true);
  const coding = physicalTracks(livePR({ stage: 70, display_column: 30, code_activity: code }));
  assert.match(coding.cells[3], /class="ham-seg active detoured"[^]*data-motion="active"/);
  assert.match(coding.cells[3], /Remediating · observed-fixer/);
  assert.doesNotMatch(coding.ci + coding.review, /ham-current|aria-current/);
  const assigned = physicalTracks(livePR({
    stage: 70, display_column: 50,
    code_activity: { ...code, state: 'assigned', caption: 'Fix task assigned · activity unknown', source: 'fix-dispatch', active: false },
  }));
  assert.doesNotMatch(assigned.cells[3], /aria-current|data-motion/);
  assert.match(assigned.review, /ham-current/);
});

test('current-phase styling is steady and undimmed, including compact/reduced-motion layouts', () => {
  const css = fs.readFileSync(path.join(__dirname, 'static-src/css/tracker.css'), 'utf8');
  assert.match(css, /\.ham-activity-track\.ham-current \{[^}]*opacity: 1;[^}]*box-shadow: none/);
  assert.doesNotMatch(css, /\.ham-activity-track\.ham-current:not\(\.active\)/, 'no muted current-phase tint: a lit track is .active, a finished one .done (#16484)');
  // Lanes are chevrons like every other stage (#16384's boxed card is
  // gone): no clip-path opt-out, no border/ring; state is fill and colour.
  assert.doesNotMatch(css, /\.ham-activity-track \{/, 'no activity-track rule overrides the .ham-seg chevron shape');
  for (const block of css.match(/\.ham-(?:seg\.)?ham-activity-track[^{]*\{[^}]*\}/g)) {
    assert.doesNotMatch(block, /clip-path|border(?!-radius)|box-shadow: inset/, block);
  }
  // Only a running track is filled (#16540): done / failing / pending /
  // decided are words in their colour on the muted field.
  assert.match(css, /\.ham-seg\.ham-activity-track\.failing \{ background: var\(--panel-strong\); color: var\(--status-error\);/, 'a finding is red words and glyph, not a red fill');
  assert.match(css, /\.ham-seg\.ham-activity-track\.done \{ background: var\(--panel-strong\); color: var\(--green\);/, 'a pass is green words, not a fill');
  for (const state of ['pending', 'done.decided']) assert.match(css, new RegExp('\\.ham-seg\\.ham-activity-track\\.' + state.replace('.', '\\.') + ' \\{ background: var\\(--panel-strong\\); color: var\\(--muted\\);'), state + ' is muted words on the muted field');
  assert.doesNotMatch(css, /\.ham-seg\.ham-activity-track\.(?:done|failing|pending)[^{]*\{[^}]*background: var\(--(?:amber|status-error|green|red)\)/, 'no fill colour on a non-running track');
  assert.doesNotMatch(css, /\.ham-track-state \{[^}]*white-space: nowrap/, 'count words wrap to the strip-wide two-line budget like every other chevron, never ellipsised mid-count');
  const current = css.match(/\.ham-activity-track\.ham-current \{([^}]*)\}/)[1];
  assert.doesNotMatch(current, /animation|data-motion/);
  assert.ok(css.indexOf('.ham-activity-track.ham-current {') > css.indexOf('.ham-seg.todo {'), 'current occupancy overrides generic upcoming-track dimming');
  assert.doesNotMatch(css, /\.ham-track-position/);
});

test('unknown tracks wear the canonical column glyph beside their caption, preserving full focus/hover details and safe evidence links', () => {
  for (const kind of ['ci', 'review']) {
    const item = livePR({
      number: 16434, repo: 'hivecommons/hive',
      [kind + '_activity']: { head: 'head', state: 'unknown', caption: (kind === 'ci' ? 'CI' : 'Review') + ' unknown · full precise reason', source: 'unavailable', url: 'https://github.com/org/repo/actions/runs/1' },
    });
    const rendered = track(item, kind, { state: 'active' }, false, now);
    const content = rendered.match(/aria-describedby="[^"]+">([^]*?)<span id="[^"]+" class="config-tooltip/)[1];
    assert.match(content, kind === 'ci' ? /<svg [^]*data-icon="ci"/ : /<svg [^]*data-icon="50"/, 'CI and Review wear distinct glyphs');
    // #16599: at 050 the Review column is the stage's own track and carries
    // its words; the inactive CI column is its glyph alone, words in the tooltip.
    assert.equal(content.replace(/<[^>]*>/g, ''), kind === 'ci' ? '' : 'Review · queued', 'words ride only on the active chevron (#16599)');
    if (kind === 'ci') assert.match(rendered, /aria-label="[^"]*CI: CI \?\/\? · no checks reported yet · /, 'the inactive count words move to the tooltip');
    assert.match(rendered, /role="tooltip">[^]*full precise reason/, 'the precise backend caption stays in the tooltip');
    assert.match(content, /aria-label="Open current-head/);
    assert.match(rendered, /class="ham-chevron-info config-info" tabindex="0"/);
    assert.match(rendered, /role="tooltip">[^]*full precise reason[^]*head head/);
    assert.match(rendered, /ham-current/);
    assert.doesNotMatch(rendered, /data-motion=|ham-track-freshness|ham-track-name|ham-track-position/);
  }
});

test('known and original chevrons keep full long text inside a shared one-line clamped copy with complete hover/focus metadata', () => {
  const handle = '@' + 'verylongunbrokenhandle'.repeat(12);
  const caption = 'Waiting for ' + handle + ' · not observed coding';
  const owner = physicalTracks(livePR({
    number: 1, stage: 71, display_column: 30,
    code_activity: { head: 'head', state: 'waiting_owner', caption, source: 'native-author-wait' },
  }));
  assert.match(owner.cells[3], /ham-chevron-info config-info" tabindex="0"/);
  assert.match(owner.cells[3], /ham-chevron-copy"><span class="ham-detour"/);
  assert.ok(owner.cells[3].includes(caption), 'full original chevron words remain in markup');
  assert.ok(owner.cells[3].match(/role="tooltip">([^]*?)<\/span>/)[1].includes(caption), 'full owner text remains in the keyboard/hover tooltip');
  const known = track(livePR({ ci_activity: { ...activity('queued', 'CI queued · ' + handle), url: 'https://github.com/org/repo/actions/runs/1' } }), 'ci', { state: 'active' }, false, now);
  assert.match(known, /ham-chevron-copy ham-track-state"><a href=/);
  assert.ok(known.match(/role="tooltip">([^]*?)<\/span>/)[1].includes(handle));
  const css = fs.readFileSync(path.join(__dirname, 'static-src/css/tracker.css'), 'utf8');
  assert.match(css, /--ham-chevron-text-lines: 1;/);
  // responsive.css gives every .config-info a 40px --tap-target floor on
  // phones; the chevron's info wrapper must undo it or each chevron becomes
  // a 44px box (hivecommons/hive#16551).
  assert.match(css, /\.ham-chevron-info\.config-info, \.ham-chevron-info\.ham-chevron-decision \{[^}]*min-height: 0;/);
  assert.match(css, /\.ham-chevron-info\.config-info::before \{ content: none; \}/);
  assert.match(css, /\.ham-chevron-copy \{[^}]*-webkit-line-clamp: var\(--ham-chevron-text-lines\);[^}]*white-space: normal; overflow-wrap: anywhere;/);
  assert.doesNotMatch(css, /\.ham-(?:seg-t|detour)\.ham-has-holder[^}]*white-space: nowrap/);
  assert.match(css, /padding: var\(--sp-1\) calc\(var\(--ham-notch\) \+ var\(--sp-2\)\)/);
  assert.match(css, /\.config-tooltip\.ham-chevron-tip \{[^}]*pointer-events: auto/);
  assert.match(css, /#activity-monitor-panel \{ --ham-compact-min-width: 60em; \}/);
  assert.match(css, /\.ham-row > \.ham-strip-row, \.ham-sub > \.ham-strip-row, \.ham-cols > \.ham-strip-row \{ overflow-x: auto; \}/);
});

test('a 071/072 row with a pushed or checking head keeps its running CI track lit (#16552)', () => {
  for (const stage of [71, 72]) {
    for (const state of ['pushed', 'checking']) {
      const cells = physicalTracks(livePR({ stage, display_column: 50, push: { state }, ci_activity: activity('running', 'CI running · 1/2 checks done', true) }));
      assert.match(cells.ci, /ham-activity-track active/, stage + '/' + state + ': checks run on the pushed head');
    }
    const parked = physicalTracks(livePR({ stage, display_column: 50, ci_activity: activity('running', 'CI running · 1/2 checks done', true) }));
    assert.doesNotMatch(parked.ci, /ham-activity-track active/, stage + ': no push in flight stays settled, waiting on the contributor');
  }
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
` + trackConsts + '\n' + fn(strip, 'stageTrackerWaitingOnPerson') + fn(strip, 'stageTrackerTrackRunning') + fn(strip, 'stageTrackerWorkflowApprovalPending') + fn(strip, 'stageTrackerRingSvg') + fn(strip, 'stageTrackerStageTrack') + fn(strip, 'stageTrackerStalledTrack') + fn(strip, 'stageTrackerQueueAheadWord') + `
function stageTrackerMotion() { return 'active'; }
function stageTrackerIcon(n, cls) { return '<svg class="' + cls + '"></svg>'; }
` + anchor + fn(row, 'stageTrackerSegments') + fn(strip, 'stageTrackerActivityTrackHtml') + fn(strip, 'stageTrackerStripItems');
  const render = new Function('esc', definitions + ';return stageTrackerStripItems;')(esc);
  const item = { kind: 'pr', stage: 70, head: 'head', display_column: 30,
    ci_activity: activity('running', 'CI running · 1/2 checks done', true),
    review_activity: activity('reviewing', 'Review working · reviewer', true),
    code_activity: activity('fixing', 'Fixing · fixer', true),
  };
  const html = render(item, [{ number: 30, short: 'PR in progress' }, { number: 50, short: 'CI / Review' }, { number: 70, short: 'Fixing', branch_from: 50 }], null, now, {});
  assert.equal((html.match(/<li /g) || []).length, 8, 'one PR strip, seven visible columns plus history');
  // Three qualify, two may light (#16540): CI and review running win; the
  // stage chevron is settled-current — words kept, no pulse.
  assert.equal((html.match(/data-motion="active"/g) || []).length, 2);
  assert.match(html, /class="ham-seg active ham-settled-current detoured"/);
  assert.doesNotMatch(html, /ham-settled-current[^>]*data-motion/);
  assert.match(html, /Waiting on CI and review, both running — /);
  assert.match(html, /ham-detour[^>]*>Remediating · fixer/);
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

test('stale activity is visibly unknown in the tooltip, never pulses, and still reads in words', () => {
  const item = { kind: 'pr', head: 'head', review_activity: { ...activity('reviewing', 'Review working · reviewer', true), actor: 'reviewer', perspectives_done: 1, perspectives_total: 5 } };
  const html = track(item, 'review', {}, false, now + 300000);
  assert.match(html, /activity unknown · last observed reviewing/);
  assert.doesNotMatch(html, /data-motion/);
  assert.match(html, /data-display-state="unknown"/);
  assert.match(html, /ham-activity-track active"[^]*ham-chevron-copy ham-track-state"><svg [^]*?<\/svg>Review 1\/5 · reviewer reviewing</, 'stale in-progress evidence keeps the lit fill and its count words');
  item.review_activity = { ...activity('approved', 'Review approved'), perspectives_done: 5, perspectives_total: 5 };
  assert.match(track(item, 'review', {}, false, now + 300000), /ham-activity-track done"[^]*Review: Review 5\/5 approved · [^]*ham-track-verdict"><svg [^]*?<\/svg><\/span>/, 'an approval of this head stays done when stale: glyph alone, count words in the tooltip');
});

test('current-head evidence links are accessible and never retained across a new head', () => {
  const item = { kind: 'pr', head: 'head', ci_activity: {...activity('failing', 'CI failing · "build" red'), url: 'https://github.com/org/repo/actions/runs/1'} };
  assert.match(track(item, 'ci', {state: 'active'}, false, now), /aria-label="Open current-head CI evidence"/);
  assert.match(track(item, 'ci', {state: 'active'}, false, now), /aria-current="step"/);
  item.head = 'new-head';
  assert.doesNotMatch(track(item, 'ci', {}, false, now), /href=|actions\/runs\/1/);
});

test('a fix request or CI-only wait is visible without manufacturing source work', () => {
  const item = { kind: 'pr', head: 'head', stage: 70, display_column: 50,
    code_activity: activity('waiting_fixer', 'Waiting for fixer · no code work observed') };
  const waiting = track(item, 'review', {state:'active'}, false, now);
  assert.match(waiting, /role="tooltip">[^]*Waiting for fixer/);
  assert.doesNotMatch(waiting, /data-motion/);
  item.code_activity = activity('waiting_ci', 'Fix task idle · agent waiting for CI before coding');
  assert.match(track(item, 'ci', {}, false, now), /Fix task idle · agent waiting for CI before coding/);
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

test('the backend detail sentence rides in the track tooltip, not on the chevron', () => {
  const detail = 'Branch protection requires a check named "test", but no check run or commit status with that name exists for the PR\'s current commit.';
  const item = livePR({ ci_activity: { ...activity('missing', 'CI waiting · required check "test" hasn\'t reported for this head yet'), detail } });
  const html = track(item, 'ci', { state: 'active' }, false, now);
  const panel = html.match(/aria-describedby="[^"]+">([^]*?)<span id="[^"]+" class="config-tooltip/)[1];
  assert.ok(!panel.includes('Branch protection'), 'the chevron shows only the compact word');
  assert.match(html, /role="tooltip">[^]*hasn't reported for this head yet · Branch protection requires a check named &quot;test&quot;/);
  assert.match(html, /title="[^"]*Branch protection requires a check named &quot;test&quot;, but no check run/);
});

test('#16484: count grammar — CI passed/total and Review done/total across every state', () => {
  const word = new Function(trackConsts + '\n' + fn(strip, 'stageTrackerWorkflowApprovalPending') + fn(strip, 'stageTrackerQueueAheadWord') + fn(strip, 'stageTrackerHiveOwnsRepair') + fn(strip, 'stageTrackerTrackWord') + ';return stageTrackerTrackWord;')();
  const ci = (state, extra = {}) => word({ head: 'h', ci_activity: { head: 'h', state, ...extra } }, 'ci', true);
  assert.equal(ci('passing', { checks_passed: 14, checks_total: 14 }), 'CI 14/14 passed');
  assert.equal(ci('failing', { checks_passed: 11, checks_total: 14, checks_failed: 1, checks_failing: ['lint'] }), 'CI 11/14 · 1 failing (lint)');
  assert.equal(ci('failing', { checks_passed: 10, checks_total: 14, checks_failed: 3, checks_failing: ['lint', 'test', 'e2e'] }), 'CI 10/14 · 3 failing (lint, test, …)');
  assert.equal(ci('failing', { blocker: 'native-queue-policy' }), 'CI ?/? · failing (native-queue-policy)', 'older servers without counts still name the red check');
  assert.equal(ci('running', { checks_passed: 3, checks_total: 14 }), 'CI 3/14 · running');
  assert.equal(ci('running', { checks_passed: 3, checks_total: 14, checks_failed: 1, checks_failing: ['lint'] }), 'CI 3/14 · running · 1 failing (lint)');
  assert.equal(ci('queued', { checks_passed: 0, checks_total: 14 }), 'CI 0/14 · queued');
  assert.equal(ci('missing', { checks_passed: 0, checks_total: 14, blocker: 'test' }), 'CI 0/14 · no checks reported yet');
  assert.equal(ci('missing', { checks_passed: 13, checks_total: 14, blocker: 'test' }), 'CI 13/14 · test not reported');
  assert.equal(ci('skipped', { checks_passed: 13, checks_total: 14, blocker: 'build' }), 'CI 13/14 · build skipped');
  assert.equal(ci('unknown'), 'CI ?/? · no checks reported yet');
  assert.equal(ci('unknown', { checks_passed: 51, checks_total: 68 }), 'CI 51/68 · no verdict yet', 'unprotected base: head-wide counts without a required verdict');
  assert.equal(word({ head: 'h' }, 'ci', true), 'CI ?/? · no checks reported yet', 'no evidence at all in the current phase');
  assert.equal(word({ head: 'h' }, 'ci', false), '', 'no evidence outside the current phase: not started, glyph alone');
  assert.equal(word({ head: 'h', ci_activity: { head: 'old', state: 'passing', checks_passed: 14, checks_total: 14 } }, 'ci', true), 'CI ?/? · no checks reported yet', 'old-head evidence counts for nothing');
  const review = (state, extra = {}, item = {}) => word({ head: 'h', ...item, review_activity: { head: 'h', state, perspectives_done: 3, perspectives_total: 5, ...extra } }, 'review', true);
  assert.equal(review('approved', { perspectives_done: 5 }), 'Review 5/5 approved');
  assert.equal(review('changes_requested'), 'Review 3/5 · changes requested');
  assert.equal(review('human_decision'), 'Review 3/5 · needs owner sign-off', 'reviews done; the sign-off is what is left, never "awaiting human"');
  assert.equal(review('human_decision', {}, { decision_needed: { question: '@clubanderson should decide whether to accept this workflow change from a fork.' } }), 'Review 3/5 · needs owner sign-off', 'no viewer known: the owner');
  globalThis.window = { _hiveUser: 'ClubAnderson' };
  try {
    assert.equal(review('human_decision', {}, { decision_needed: { question: '@clubanderson should decide whether to accept this workflow change from a fork.' } }), 'Review 3/5 · needs your sign-off', 'the ask names the viewer');
    assert.equal(review('human_decision', {}, { decision_needed: { question: '@someone-else should decide.' } }), 'Review 3/5 · needs owner sign-off');
    for (const q of ['@clubanderson-bot should decide.', '@clubandersonx should decide.']) assert.equal(review('human_decision', {}, { decision_needed: { question: q } }), 'Review 3/5 · needs owner sign-off', 'a longer login is not the viewer: ' + q);
    for (const q of ['@clubanderson', 'ping @clubanderson, please decide.', 'ok @ClubAnderson?']) assert.equal(review('human_decision', {}, { decision_needed: { question: q } }), 'Review 3/5 · needs your sign-off', 'the viewer handle ends at punctuation or the end: ' + q);
  } finally { delete globalThis.window; }
  assert.equal(review('reviewing', { actor: 'reviewer-2' }), 'Review 3/5 · reviewer-2 reviewing');
  assert.equal(review('adjudicating', { actor: 'adjudicator' }), 'Review 3/5 · adjudicator adjudicating');
  assert.equal(review('assigned', { actor: 'reviewer' }), 'Review 3/5 · reviewer queued');
  assert.equal(review('assigned'), 'Review 3/5 · queued');
  assert.equal(review('unassigned', { perspectives_done: 0 }), 'Review 0/5 · queued');
  assert.equal(review('unassigned', { perspectives_done: 0, queue_ahead: 3 }), 'Review 0/5 · queued, 3 ahead');
  assert.equal(review('unknown', { perspectives_done: 0 }), 'Review 0/5 · queued');
  assert.equal(review('unknown', { perspectives_done: 0 }, { push: { state: 'pushed', sha: 'h', next: 're-review' } }), 'Review 0/5 · re-review of h');
  assert.equal(review('unknown'), 'Review 3/5 · perspectives', 'partial verdicts with no aggregate verdict yet');
  assert.equal(word({ head: 'h', review_activity: { head: 'h', state: 'approved' } }, 'review', true), 'Review approved', 'older servers without counts');
  assert.equal(word({ head: 'h' }, 'review', false), '');
});

test('#16484: the three screenshot rows — both tracks lit with counts, swarm requested, red CI beside a done review', () => {
  const head = 'd3c0824304d4c97b8ee76a267fb1e91f6d4c79a4';
  const fresh = (state, extra = {}) => ({ head, state, caption: state, source: 'cache', observed_at: new Date(now).toISOString(), ...extra });
  const text = cell => cell.match(/aria-describedby="[^"]+">([^]*?)<span id=/)[1].replace(/<[^>]*>/g, '');
  const cls = cell => cell.match(/class="ham-seg ham-activity-track ([a-z]+)/)[1];
  // #16456: CI running on the new head while the re-review is pending → BOTH lit, counts disambiguate.
  let cells = physicalTracks(livePR({ head, stage: 70, display_column: 50, push: { state: 'pushed', sha: head, next: 're-review' },
    code_activity: { head, state: 'pushed', caption: 'New head pushed · CI and re-review', source: 'recorded-push' },
    ci_activity: fresh('running', { checks_passed: 9, checks_total: 14, active: true }), review_activity: fresh('unknown', { perspectives_done: 0, perspectives_total: 5 }) }));
  assert.equal(cls(cells.ci), 'active'); assert.equal(text(cells.ci), 'CI 9/14 · running');
  assert.match(cells.ci, /data-motion="active"/);
  // #16599: the pending re-review is not in motion and not the stage's own
  // track (070) → glyph alone, its words in the tooltip.
  assert.equal(cls(cells.review), 'todo', 'a pending re-review is an inactive glyph (#16599)'); assert.equal(text(cells.review), '');
  assert.match(cells.review, /aria-label="[^"]*Review: Review 0\/5 · re-review of d3c0824 · /);
  assert.doesNotMatch(cells.review, /data-motion/);
  assert.match(cells.review, /hive is waiting on CI for new head d3c0824/, 'the tooltip still says which track leads');
  // #16491: review swarm requested, no bundle yet; CI shows its count.
  cells = physicalTracks(livePR({ head, stage: 50, display_column: 50,
    ci_activity: fresh('running', { checks_passed: 2, checks_total: 14 }), review_activity: fresh('unassigned', { perspectives_done: 0, perspectives_total: 5, blocker: 'no reviewer free' }) }));
  // #16599: at 050 the Review column is the stage's own track → active with
  // words even while only queued; CI runs beside it (two lit).
  assert.equal(cls(cells.review), 'active'); assert.equal(text(cells.review), 'Review 0/5 · queued');
  assert.doesNotMatch(cells.review, /data-motion/, 'queued is lit but still');
  assert.equal(cls(cells.ci), 'active'); assert.equal(text(cells.ci), 'CI 2/14 · running');
  // #16268: CI failed native-queue-policy, review approved, held.
  cells = physicalTracks(livePR({ head, stage: 70, display_column: 50, modifiers: ['hold'], hold: { by: 'hanthor', pill: 'Held by @hanthor' },
    ci_activity: fresh('failing', { checks_passed: 13, checks_total: 14, checks_failed: 1, checks_failing: ['native-queue-policy'], blocker: 'native-queue-policy' }),
    review_activity: fresh('approved', { perspectives_done: 5, perspectives_total: 5 }) }));
  // Nothing runs and the row sits in the CI/Review column, so the failing
  // CI is the stalled blocker: lit still (amber-muted), words on (#16599);
  // the settled approval is the glyph alone, its count words in the tooltip.
  const tip = cell => cell.match(/ aria-label="([^"]*)"/)[1];
  assert.equal(cls(cells.ci), 'active'); assert.match(cells.ci, /ham-activity-track active ham-stalled/); assert.doesNotMatch(cells.ci, /data-motion/);
  assert.equal(text(cells.ci), 'CI 13/14 · 1 failing (native-queue-policy)'); assert.match(cells.ci, /data-verdict="fail"/);
  assert.match(tip(cells.ci), /^(?:Current pipeline phase · )?CI: CI 13\/14 · 1 failing \(native-queue-policy\) · /);
  assert.equal(cls(cells.review), 'done'); assert.equal(text(cells.review), ''); assert.match(cells.review, /data-verdict="pass"/);
  assert.match(tip(cells.review), /^(?:Current pipeline phase · )?Review: Review 5\/5 approved · /);
  assert.match(cells.review, /Held by @hanthor/);
  // Not started: a PR still being written (column 30) with no evidence shows the glyph alone, never lit.
  cells = physicalTracks(livePR({ head, stage: 30, display_column: 30 }));
  for (const cell of [cells.ci, cells.review]) {
    assert.equal(cls(cell), 'todo');
    assert.equal(text(cell), '');
    assert.match(cell, /<svg /);
  }
});

test('#16615: CI green on the pushed head + re-review owed by the hive → Review lit with the head, CI a green ring', () => {
  const head = 'd88928e16f21ab6e653e5d06f137926bc0db1c0d';
  const fresh = (state, extra = {}) => ({ head, state, caption: state, source: 'github-check-runs', observed_at: new Date(now).toISOString(), ...extra });
  const text = cell => cell.match(/aria-describedby="[^"]+">([^]*?)<span id=/)[1].replace(/<[^>]*>/g, '');
  const cls = cell => cell.match(/class="ham-seg ham-activity-track ([a-z -]+?)(?: ham-current)?"/)[1];
  const item = livePR({ head, stage: 72, display_column: 50, author: 'thatguywob', author_kind: 'clanker',
    push: { state: 'waiting', login: 'thatguywob' },
    code_activity: { head, state: 'fix_pushed', caption: 'Fix pushed d88928e · re-review pending', actor: '@thatguywob', source: 'rework-commit' },
    rework: { state: 'pushed', actor: 'thatguywob', evidence: 'commit', head, owed_by: 'hive-reviewer' },
    ci_activity: fresh('passing', { checks_passed: 11, checks_total: 11, caption: 'CI green' }),
    review_activity: fresh('unknown', { perspectives_done: 0, perspectives_total: 5 }) });
  const cells = physicalTracks(item);
  assert.equal(cls(cells.ci), 'done', 'CI complete: icon only'); assert.equal(text(cells.ci), '');
  assert.match(cells.ci, /data-verdict="pass"/, 'green ring');
  assert.match(cls(cells.review), /^active/, 'the owed re-review is the lit chevron');
  assert.equal(text(cells.review), 'Review 0/5 · re-review of d88928e');
  assert.match(cells.review, /hive is waiting on review for new head d88928e/);
  assert.equal((cells.ci + cells.review).match(/ham-activity-track active/g).length, 1, 'exactly one active track');
  // While CI still runs on the pushed head, CI is the lit one and Review waits as a glyph.
  const running = physicalTracks({ ...item, ci_activity: fresh('running', { checks_passed: 7, checks_total: 11, active: true }) });
  assert.equal(cls(running.ci), 'active'); assert.equal(text(running.ci), 'CI 7/11 · running');
  assert.equal(cls(running.review), 'todo'); assert.equal(text(running.review), '');
});
