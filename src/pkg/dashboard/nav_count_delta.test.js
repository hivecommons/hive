// Execute the shipped helper and real badge writers, with a tiny DOM and
// deterministic clock. No browser, polling loop or Go process is started.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = name => fs.readFileSync(path.join(__dirname, 'static-src/js', name + '.js'), 'utf8');
const fn = (file, name) => {
  const found = source(file).match(new RegExp('(?:async )?function ' + name + '\\([^\\n]*\\) \\{[\\s\\S]*?\\n    \\}'));
  assert.ok(found, name);
  return found[0];
};
const IDS = ['activity-monitor-sidebar-badge', 'activity-monitor-waiting-badge', 'contributors-sidebar-badge', 'knowledge-sidebar-badge'];

function load() {
  let now = 0, nextTimer = 0;
  const timers = new Map(), elements = new Map();
  function element(id) {
    const attrs = {};
    const el = { id, attrs, textContent: '', hidden: false, title: '',
      setAttribute(k, v) { attrs[k] = String(v); },
      removeAttribute(k) { delete attrs[k]; },
      children: [],
      querySelector(sel) { return this.children.find(c => sel === '.' + c.className.split(' ').pop()) || null; },
      appendChild(child) { this.children.push(child); return child; },
      classList: { toggle() {} },
    };
    elements.set(id, el);
    return el;
  }
  IDS.concat(['activity-monitor-panel']).forEach(element);
  const document = { hidden: false, getElementById: id => elements.get(id) || null,
    createElement: tag => ({ tag, className: '', hidden: false, innerHTML: '' }) };
  const ctx = vm.createContext({ document, window: {}, console, Number,
    setTimeout(cb, ms) { const id = ++nextTimer; timers.set(id, { cb, at: now + ms }); return id; },
    clearTimeout(id) { timers.delete(id); },
  });
  vm.runInContext(source('nav-count-delta'), ctx);
  vm.runInContext(`
    var _kbCache = null, _kbInitialLoad = true, _kbRendered = false, _kbSetupView = 'none';
    var _cachedContributors = [], _stageTracker = null, _stageTrackerMoves = {}, _stageTrackerEvents = [];
    var _stageTrackerFilter = {repo: 'filtered-out'}, _stageTrackerGroup = 'none', _stageTrackerCelebrating = {};
    function refreshSectionCardShell() {}
    function setSectionSummary() {}
    function contributorsSectionSummary() { return ''; }
    function formatNumber(n) { return Number(n).toLocaleString('en-US'); }
    var knowledgeRenders = 0;
    function renderKnowledge() { knowledgeRenders++; }
    function fetchBeadSynthStatus() {}
    function fetchFactHistory() {}
    function stageTrackerNoteMerges() {}
    function stageTrackerReconcileMerges() {}
    function stageTrackerNoteOpened() {}
    function stageTrackerNoteMoves() {}
    function stageTrackerSyncToolbar() {}
    function stageTrackerWorkers() { return []; }
    function stageTrackerNoteTuckSetting() { return false; }
    function stageTrackerSharedPRs() { return {}; }
    function stageTrackerRepoChipOn() { return false; }
    function stageTrackerRepoChipSetting() { return ''; }
    function stageTrackerTrainContiguous(rows) { return rows; }
    function stageTrackerRegressionHandled() { return false; }
    function stageTrackerOpenCounts() { return null; }
    function stageTrackerRenderLabelDialog() {}
    function stageTrackerMotionEffect() { return ''; }
    function stageTrackerMotionSetting() { return ''; }
    function stageTrackerReducedMotion() { return false; }
    function stageTrackerSelectionSync() {}
    function stageTrackerWaitingToast() {}
    function stageTrackerActedAgo() { return ''; }
    function setIfChanged(el, text) { el.innerHTML = text; }
    var _configTooltipTrigger = null;
    function positionConfigTooltip() {}
    function esc(s) { return String(s); }
  `, ctx);
  for (const [file, names] of [
    ['knowledge', ['sidebarPairBadgeText', 'setSidebarPairBadge', 'setSidebarCountBadge', 'kbTotalFactsFromStats', 'kbFactsLabel', 'kbSidebarFactCount', 'fetchKnowledgeStats']],
    ['contributors', ['contributorsOnlineTitle', 'contributorsOnlineTooltip', 'updateContributorBadge']],
    ['tracker-waiting', ['stageTrackerWaitingOnYou', 'stageTrackerWaitingTitle', 'stageTrackerWaitKindWords', 'stageTrackerRenderWaiting']],
    ['tracker-words-stage', ['stageTrackerDefaultFilter']],
    ['tracker-filter-render', ['renderStageTracker']],
    ['tracker-merged', ['stageTrackerMergingHoldRelease']],
  ]) {
    for (const name of names) vm.runInContext(fn(file, name), ctx);
  }
  for (const name of ['STAGE_TRACKER_AGE_MS', 'STAGE_TRACKER_SORTS', 'STAGE_TRACKER_ACTED_FILTERS', 'STAGE_TRACKER_PAGE_SIZES', 'STAGE_TRACKER_DEFAULT_PAGE_SIZE', 'STAGE_TRACKER_ATTENTION_STAGES']) {
    const declaration = source(name === 'STAGE_TRACKER_ACTED_FILTERS' ? 'tracker-row-acted' : 'tracker-words-stage').match(new RegExp('const ' + name + ' = [^\\n]+'));
    assert.ok(declaration, name);
    vm.runInContext(declaration[0], ctx);
  }
  vm.runInContext(source('tracker-filter'), ctx);
  vm.runInContext(`const STAGE_TRACKER_WAITING_TOOLTIP_MAX = 5; const STAGE_TRACKER_WAIT_KIND_WORDS = {};`, ctx);
  return { ctx, document, timers, element, badge: id => elements.get(id),
    tick(ms = 1400) {
      now += ms;
      for (const [id, timer] of [...timers]) if (timer.at <= now) { timers.delete(id); timer.cb(); }
    },
  };
}
const delta = badge => badge.attrs['data-count-delta'];
const item = (number, kind = 'issue', extra = {}) => ({ repo: 'o/r', number, kind, ...extra });
const stats = (...counts) => ({ enabled: true, layers: counts.map(total_pages => ({ type: 'project', total_pages })) });
const knowledge = async (api, payload) => {
  api.ctx.fetch = async url => ({ ok: true, json: async () => url === '/api/knowledge/stats' ? payload : [] });
  await api.ctx.fetchKnowledgeStats();
};

for (const id of IDS) {
  test(id + ': initial/unchanged/unknown/zero, rapid changes, cleanup and rerender', () => {
    const api = load(), badge = api.badge(id), write = (n, live = true) => api.ctx.navCountDelta(badge, n, 'Count ' + n, live);
    write(0);
    assert.equal(delta(badge), undefined);
    write(0);
    assert.equal(api.timers.size, 0);
    write(3);
    assert.equal(delta(badge), '↑ +3');
    assert.match(badge.attrs['aria-label'], /increased by 3/);
    const phase = badge.attrs['data-count-phase'];
    api.tick(500);
    write(5);
    assert.equal(delta(badge), '↑ +2');
    assert.notEqual(badge.attrs['data-count-phase'], phase);
    assert.equal(api.timers.size, 1);
    write(3);
    assert.equal(delta(badge), '↓ -2');
    write(3);
    assert.equal(api.timers.size, 1);
    api.tick();
    assert.equal(delta(badge), undefined);
    assert.equal(api.timers.size, 0);
    assert.equal(badge.attrs['aria-label'], 'Count 3');
    write(0);
    assert.equal(delta(badge), '↓ -3', 'zero is a known value');
    for (const invalid of [null, undefined, NaN, -1, '8', Infinity, 1.5]) {
      write(invalid);
      write(8);
      assert.equal(delta(badge), undefined, 'unknown -> known seeds, not a synthetic increase');
    }
    write(9, false);
    assert.equal(delta(badge), undefined, 'cached/route redraw does not animate');
    write(9);
    assert.equal(delta(badge), undefined, 'redraw advanced the baseline');
    write(10);
    const replacement = api.element(id);
    api.ctx.navCountDelta(replacement, 10, 'Count 10');
    assert.equal(delta(badge), undefined);
    assert.equal(delta(replacement), undefined);
    assert.equal(api.timers.size, 0, 'replacement cancels the old element timer');
  });
}

test('real tracker renderer: new issues, PR removal, batching, filters, invalid payloads', () => {
  const api = load(), badge = api.badge(IDS[0]), render = items => api.ctx.renderStageTracker({ items });
  render([item(1), item(2, 'pr')]);
  assert.equal(badge.textContent, '2');
  assert.equal(delta(badge), undefined);
  render([item(1), item(2, 'pr'), item(3)]);
  assert.equal(delta(badge), '↑ +1');
  render([item(1), item(3)]);
  assert.equal(delta(badge), '↓ -1');
  render([item(1), item(3), item(4), item(5), item(6)]);
  assert.equal(delta(badge), '↑ +3');
  api.tick();
  api.ctx.renderStageTracker();
  assert.equal(delta(badge), undefined);
  // A PR under a listed issue shares its row, not a new count.
  render([item(1), item(3), item(4), item(5), item(6), item(7, 'pr', { nested_under: 1 })]);
  assert.equal(delta(badge), undefined);
  api.ctx.renderStageTracker({ status: 'initializing' });
  render([]);
  assert.equal(badge.textContent, '0');
  assert.equal(delta(badge), undefined);
});

test('real waiting badge writer: independent count, explicit zero and invalid header', () => {
  const api = load(), badge = api.badge(IDS[1]);
  const write = count => api.ctx.stageTrackerRenderWaiting({ waiting_on_human: { count, items: [] } }, 0);
  write(2);
  write(5);
  assert.equal(delta(badge), '↑ +3');
  write(0);
  assert.equal(badge.textContent, '0');
  assert.equal(delta(badge), '↓ -5');
  write(undefined);
  write(3);
  assert.equal(delta(badge), undefined);
  assert.equal(api.timers.size, 0);
  assert.equal(delta(api.badge(IDS[0])), undefined);
});

test('a merged PR count delayed by the existing hold animates when that hold releases', () => {
  const api = load(), badge = api.badge(IDS[0]);
  api.ctx._stageTrackerMergingHold = {};
  api.ctx.holding = true;
  api.ctx.stageTrackerMergingHoldView = d => api.ctx.holding ? { ...d, items: d.items.concat(item(2, 'pr')) } : d;
  api.ctx.renderStageTracker({ items: [item(1)] });
  assert.equal(badge.textContent, '2');
  api.ctx.holding = false;
  api.ctx.stageTrackerMergingHoldRelease();
  assert.equal(badge.textContent, '1');
  assert.equal(delta(badge), '↓ -1');
  api.tick();
  api.ctx.renderStageTracker();
  assert.equal(delta(badge), undefined, 'later cached redraw does not replay the release');
});

test('contributors use online only, not active sockets or denominator changes', () => {
  const api = load(), badge = api.badge(IDS[2]);
  const write = (online, registered = 12, active = 99) => api.ctx.updateContributorBadge({ online, registered, active, presence_window: '15m' });
  write(2);
  assert.equal(badge.textContent, '2/12');
  write(2, 20, 1);
  assert.equal(delta(badge), undefined);
  write(5, 20);
  assert.equal(delta(badge), '↑ +3');
  write(3, 21);
  assert.equal(delta(badge), '↓ -2');
  write(0);
  assert.equal(badge.textContent, '0/12');
  assert.equal(delta(badge), '↓ -3');
  write(undefined);
  assert.equal(badge.textContent, '0/12', 'unknown does not replace display with a fabricated zero');
  write(4);
  assert.equal(delta(badge), undefined);
});

test('knowledge uses numeric layer fact totals, independent of formatting and disabled priming', async () => {
  const api = load(), badge = api.badge(IDS[3]);
  await knowledge(api, stats(1000, 20));
  assert.equal(badge.textContent, 1020);
  assert.equal(delta(badge), undefined);
  await knowledge(api, stats(1000, 20));
  assert.equal(delta(badge), undefined);
  await knowledge(api, stats(1001, 22));
  assert.equal(delta(badge), '↑ +3');
  await knowledge(api, stats(1000, 21));
  assert.equal(delta(badge), '↓ -2');
  await knowledge(api, { enabled: true });
  assert.equal(badge.textContent, 1021);
  assert.equal(badge.attrs['aria-label'], 'Knowledge fact count unavailable');
  assert.equal(api.ctx._kbInitialLoad, false);
  await knowledge(api, stats());
  assert.equal(badge.textContent, 0);
  assert.equal(delta(badge), undefined);
  await knowledge(api, stats(2));
  assert.equal(delta(badge), '↑ +2');
  await knowledge(api, { enabled: false, layers: [] });
  assert.equal(delta(badge), undefined, 'disabling priming is not fact deletion');
  assert.equal(api.timers.size, 0);
});

test('an unhealthy Knowledge layer still loads and refreshes known facts without a synthetic delta', async () => {
  const api = load(), badge = api.badge(IDS[3]);
  const partial = { enabled: true, layers: [
    { type: 'project', total_pages: 5, healthy: true },
    { type: 'global', healthy: false },
  ] };
  await knowledge(api, partial);
  assert.equal(badge.textContent, 5);
  assert.equal(delta(badge), undefined);
  assert.equal(badge.attrs['aria-label'], '5 facts known; some layer counts unavailable');
  assert.equal(api.ctx.knowledgeRenders, 1, 'first load renders despite the unhealthy remote');
  assert.equal(api.ctx._kbInitialLoad, false);
  assert.equal(api.ctx._kbCache.layers[1].healthy, false);
  assert.ok(Array.isArray(api.ctx._kbCache._documents), 'document fetch still reaches the cache');
  await knowledge(api, stats(7));
  assert.equal(badge.textContent, 7);
  assert.equal(delta(badge), undefined, 'partial -> complete seeds a new baseline');
  await knowledge(api, stats(10));
  assert.equal(delta(badge), '↑ +3');
  await knowledge(api, partial);
  assert.equal(badge.textContent, 5, 'known facts refresh when a layer later becomes unhealthy');
  assert.equal(delta(badge), undefined, 'missing remote counts are not fact deletion');
  assert.equal(api.timers.size, 0);
  assert.equal(api.ctx._kbCache.layers[1].healthy, false);
  assert.equal(api.ctx.knowledgeRenders, 3, 'changed layer sets keep rendering');
});

test('reduced motion keeps static signed feedback; hidden tabs advance without animation', () => {
  const api = load(), badge = api.badge(IDS[2]);
  api.ctx.window.matchMedia = () => ({ matches: true });
  api.ctx.navCountDelta(badge, 1, '1 online');
  api.ctx.navCountDelta(badge, 4, '4 online');
  assert.equal(delta(badge), '↑ +3', 'feedback remains readable with reduced motion');
  // CSS, not a JS listener, disables motion (including preference changes
  // during a cue). Keep that shipped rule covered alongside behavior.
  const css = fs.readFileSync(path.join(__dirname, 'static-src/css/design-system.css'), 'utf8');
  assert.match(css, /@media \(prefers-reduced-motion: reduce\) \{\s*\[data-count-phase\]::after \{ animation: none; \}/);
  api.document.hidden = true;
  api.ctx.navCountDelta(badge, 7, '7 online');
  assert.equal(delta(badge), undefined);
  api.document.hidden = false;
  api.ctx.navCountDelta(badge, 7, '7 online');
  assert.equal(delta(badge), undefined);
  assert.equal(api.timers.size, 0);
});
