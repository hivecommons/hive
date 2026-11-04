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
const track = new Function('esc', 'stageTrackerIcon', fn(strip, 'stageTrackerActivityTrackHtml') + ';return stageTrackerActivityTrackHtml;')(esc, (n, cls) => `<svg class="${cls}" data-icon="${n}"></svg>`);
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
` + fn(strip, 'stageTrackerStripItems') + ';return stageTrackerStripItems;')(esc, segments, track, ...Object.values(helpers));
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
        assert.match(cells[4], /todo ham-current/);
        assert.match(cells[5], /done ham-current/);
      }
      if (item.code_activity?.state === 'waiting_owner') {
        assert.ok(cells[3].includes(item.code_activity.caption));
        assert.doesNotMatch(rendered, /data-motion="active"/, 'waiting for an author is not observed coding');
      }
      for (const cell of cells.filter(cell => /data-display-state="unknown"/.test(cell))) {
        const content = cell.match(/aria-describedby="[^"]+">([^]*?)<span id=/)[1];
        assert.equal(content.replace(/<[^>]*>/g, '').trim(), '', 'missing evidence remains icon-only at screenshot density');
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
  assert.match(cells.ci, /ham-activity-track todo ham-current/);
  assert.match(cells.review, /ham-activity-track done ham-current/);
  for (const cell of [cells.ci, cells.review]) {
    assert.match(cell, /aria-current="step"/);
    assert.match(cell, /aria-label="Current pipeline phase/);
    assert.doesNotMatch(cell, /data-motion=|ham-activity-track active/);
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
    assert.match(cell, /todo ham-current/);
    assert.match(cell, /data-activity-state="unknown"/);
    assert.match(cell, /data-display-state="unknown"/);
    assert.doesNotMatch(cell, /data-motion=|ham-activity-track done|ham-activity-track active/);
  }
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
  assert.match(cells.ci, /todo ham-current/);
  assert.match(cells.review, /done ham-current/);
  assert.match(cells.review, /Waiting for fixer · no code work observed/);
  assert.match(cells.review, /Held by @hanthor · Maintainer hold/);
  assert.doesNotMatch(cells.rendered, /data-motion=|>(?:Fixing|Remediating) ·/);
  assert.equal(item.stage, 70);
  assert.deepEqual(item.modifiers, ['hold']);
});

test('current phase, running activity, failure and successful completion are independent dimensions', () => {
  for (const [state, caption, running, cls] of [
    ['failing', 'CI failing · "build" red', false, 'todo'],
    ['running', 'CI running · 1/2 checks done', true, 'active'],
    ['passing', 'CI green · all 2 required checks passed', false, 'done'],
    ['skipped', 'CI skipped · check "build" skipped', false, 'todo'],
  ]) {
    const cells = physicalTracks(livePR({ ci_activity: activity(state, caption, running) }));
    assert.ok(cells.ci.includes('ham-activity-track ' + cls + ' ham-current'));
    assert.equal(cells.ci.includes('data-motion="active"'), running);
    assert.match(cells.review, /todo ham-current[^]*data-activity-state="unknown"/);
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
      assert.match(cell, /todo ham-current/);
      assert.doesNotMatch(cell, /data-motion=|ham-activity-track done|ham-activity-track active/);
    }
    if (mode === 'old-head') assert.match(cells.review, /data-activity-state="unknown"/);
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
  assert.match(css, /\.ham-activity-track\.ham-current:not\(\.active\) \{ background: color-mix/);
  // Lanes are chevrons like every other stage (#16384's boxed card is
  // gone): no clip-path opt-out, no border/ring; state is fill and colour.
  assert.doesNotMatch(css, /\.ham-activity-track \{/, 'no activity-track rule overrides the .ham-seg chevron shape');
  for (const block of css.match(/\.ham-(?:seg\.)?ham-activity-track[^{]*\{[^}]*\}/g)) {
    assert.doesNotMatch(block, /clip-path|border(?!-radius)|box-shadow: inset/, block);
  }
  assert.match(css, /\.ham-seg\.ham-activity-track\[data-activity-state="failing"\], \.ham-seg\.ham-activity-track\[data-activity-state="changes_requested"\] \{ background: color-mix\(in srgb, var\(--amber\) \d+%, var\(--panel\)\); color: var\(--amber\);/);
  const current = css.match(/\.ham-activity-track\.ham-current \{([^}]*)\}/)[1];
  assert.doesNotMatch(current, /animation|data-motion/);
  assert.ok(css.indexOf('.ham-activity-track.ham-current {') > css.indexOf('.ham-seg.todo {'), 'current occupancy overrides generic upcoming-track dimming');
  assert.doesNotMatch(css, /\.ham-track-position/);
});

test('unknown tracks show only the canonical column glyph, preserving full focus/hover details and safe evidence links', () => {
  for (const kind of ['ci', 'review']) {
    const item = livePR({
      number: 16434, repo: 'hivecommons/hive',
      [kind + '_activity']: { head: 'head', state: 'unknown', caption: (kind === 'ci' ? 'CI' : 'Review') + ' unknown · full precise reason', source: 'unavailable', url: 'https://github.com/org/repo/actions/runs/1' },
    });
    const rendered = track(item, kind, { state: 'active' }, false, now);
    const content = rendered.match(/aria-describedby="[^"]+">([^]*?)<span id="[^"]+" class="config-tooltip/)[1];
    assert.match(content, kind === 'ci' ? /<svg [^]*data-icon="ci"/ : /<svg [^]*data-icon="50"/, 'CI and Review wear distinct glyphs');
    assert.equal(content.replace(/<[^>]*>/g, '').trim(), '', 'no CI/Review/unknown/reason/question-mark panel text');
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
  assert.match(css, /\.ham-chevron-copy \{[^}]*-webkit-line-clamp: var\(--ham-chevron-text-lines\);[^}]*white-space: normal; overflow-wrap: anywhere;/);
  assert.doesNotMatch(css, /\.ham-(?:seg-t|detour)\.ham-has-holder[^}]*white-space: nowrap/);
  assert.match(css, /padding: var\(--sp-1\) calc\(var\(--ham-notch\) \+ var\(--sp-2\)\)/);
  assert.match(css, /\.config-tooltip\.ham-chevron-tip \{[^}]*pointer-events: auto/);
  assert.match(css, /#activity-monitor-panel \{ --ham-compact-min-width: 60em; \}/);
  assert.match(css, /\.ham-row > \.ham-strip-row, \.ham-sub > \.ham-strip-row, \.ham-cols > \.ham-strip-row \{ overflow-x: auto; \}/);
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
    ci_activity: activity('running', 'CI running · 1/2 checks done', true),
    review_activity: activity('reviewing', 'Review working · reviewer', true),
    code_activity: activity('fixing', 'Fixing · fixer', true),
  };
  const html = render(item, [{ number: 30, short: 'PR in progress' }, { number: 50, short: 'CI / Review' }, { number: 70, short: 'Fixing', branch_from: 50 }], null, now, {});
  assert.equal((html.match(/<li /g) || []).length, 8, 'one PR strip, seven visible columns plus history');
  assert.equal((html.match(/data-motion="active"/g) || []).length, 3);
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

test('stale activity is visibly unknown and old green observations cannot look live', () => {
  const item = { kind: 'pr', head: 'head', review_activity: activity('reviewing', 'Review working · reviewer', true) };
  const html = track(item, 'review', {}, false, now + 300000);
  assert.match(html, /activity unknown · last observed reviewing/);
  assert.doesNotMatch(html, /data-motion/);
  assert.match(html, /data-display-state="unknown"/);
  assert.doesNotMatch(html, /ham-chevron-copy/);
  item.review_activity = activity('approved', 'Review approved');
  assert.doesNotMatch(track(item, 'review', {}, false, now + 300000), /ham-activity-track done/);
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
