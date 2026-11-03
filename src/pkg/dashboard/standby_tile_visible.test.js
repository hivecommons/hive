const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

// #15720: the standby tile only rendered into #oc-gov-strip-outer, whose
// .oc-gov-strip rule is display:none, so a paused lane was never visible.
const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
function grab(name) {
  const m = html.match(new RegExp('function ' + name + '\\([\\s\\S]*?\\n    }\\n'));
  assert.ok(m, name + ' not found');
  return m[0];
}
const src = ['standbyPauseFactsHtml', 'renderGovernor'].map(grab).join('\n');
const id = s => String(s ?? '');
const stubs = {
  window: {},
  sparkline: () => '',
  setSidebarPairBadge: () => {},
  overviewKPIGovernorFallbackEquations: () => ({}),
  renderActionableEquationSubline: () => '',
  governorPressureBarBoundaries: () => ({ max: 30, pressurePct: 0, quietPct: 10, busyPct: 40, surgePct: 70 }),
  GOVERNOR_MODE_COLORS: {},
  formatDashboardDateTime: () => '',
  sparkSvg: () => '',
  formatFixTime: () => '—',
  escapeHtml: id,
  esc: id,
  loadGovernorPRModels: () => {},
  govPRModelsWindow: '7d',
  loadReviewerAccuracy: () => {},
  fleetRunStatHtml: () => '',
  formatGovernorNextRun: () => '',
  governorModeGradient: () => '',
  renderBudgetBar: () => '',
  renderGovernorPRModelsTile: () => '',
  renderReviewerAccuracyTile: () => '',
  renderCadenceMatrix: () => '',
  sectionCardShell: (opts, body) => body,
  DASHBOARD_SECTION_CARD_CONFIG: { governor: { settingsHtml: '' } },
  applySectionCollapse: () => {},
};

function renderGovernorSection(gov) {
  const el = { className: '', innerHTML: '' };
  const doc = { getElementById: x => (x === 'governor' ? el : null) };
  const names = Object.keys(stubs);
  const fn = new Function('document', ...names, src + '; return renderGovernor;')(doc, ...names.map(n => stubs[n]));
  fn(gov, [], { governor: gov });
  return el;
}

// Rules from the page's <style> blocks that hide an element outright.
const css = [...html.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)].map(m => m[1]).join('\n').replace(/\/\*[\s\S]*?\*\//g, '');
function classIsHidden(cls) {
  for (const chunk of css.split('}')) {
    const [selectors, body] = chunk.split('{');
    if (body === undefined) continue;
    if (!selectors.split(',').some(s => s.trim() === '.' + cls)) continue;
    if (/(^|;)\s*display\s*:\s*none\b/.test(body)) return true;
  }
  return false;
}

// Open elements (tag + attributes) enclosing the first match of marker.
const VOID = new Set(['br', 'img', 'input', 'hr', 'meta', 'link']);
function ancestorsOf(markup, marker) {
  const stack = [];
  for (const m of markup.matchAll(/<(\/?)([a-zA-Z][\w-]*)([^>]*)>/g)) {
    if (m[0].includes(marker)) return stack;
    const tag = m[2].toLowerCase();
    if (m[1]) { stack.pop(); continue; }
    if (VOID.has(tag) || m[3].endsWith('/')) continue;
    stack.push(m[3]);
  }
  return null;
}
function hiddenBy(containerClass, attrsList) {
  const classes = [containerClass];
  for (const attrs of attrsList) {
    if (/\shidden(\s|=|$)/.test(attrs) || /display\s*:\s*none/.test(attrs)) return attrs;
    const c = attrs.match(/class="([^"]*)"/);
    if (c) classes.push(...c[1].split(/\s+/).filter(Boolean));
  }
  return classes.find(classIsHidden) || '';
}

const pausedGov = {
  active: true,
  mode: 'quiet',
  suppressed_lanes: ['scanner'],
  lane_pause_reasons: { scanner: 'budget_exhausted' },
  lane_queue_depths: { scanner: 1 },
  qualified_standbys: { scanner: 1 },
};

test('the visibility check flags the legacy hidden governor strip', () => {
  assert.strictEqual(classIsHidden('oc-gov-strip'), true);
  assert.strictEqual(hiddenBy('oc-gov-strip', []), 'oc-gov-strip');
});

test('a paused lane renders a visible standby tile in the Governor section', () => {
  const el = renderGovernorSection(pausedGov);
  assert.match(el.innerHTML, /scanner paused, out of budget, 1 waiting, 1 qualify/);
  const ancestors = ancestorsOf(el.innerHTML, 'id="gov-standby-tile"');
  assert.ok(ancestors, 'standby tile is not rendered in #governor');
  assert.strictEqual(hiddenBy('governor', ancestors), '', 'standby tile sits in a display:none container');
  const tile = el.innerHTML.match(/<div class="gov-stat" id="gov-standby-tile"[\s\S]*?<\/div><\/div>/);
  assert.ok(tile, 'standby tile markup not found');
  assert.match(tile[0], /data-action="standbyDispatch"[^>]*data-standby-lane="scanner"/);
});

test('no paused lane renders no standby tile', () => {
  const el = renderGovernorSection({ active: true, mode: 'idle' });
  assert.doesNotMatch(el.innerHTML, /gov-standby-tile/);
});
