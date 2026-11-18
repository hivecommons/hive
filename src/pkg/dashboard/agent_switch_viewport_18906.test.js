// #18906: switching an agent's method/model/effort must not scroll the page away
// from that agent, drop focus from the control just used, or re-sort the list.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const js = n => fs.readFileSync(path.join(__dirname, 'static-src', 'js', n), 'utf8');
const cards = js('agent-cards.js');
const controls = js('plan-agent-controls.js');

// Pull a top-level `function name(...) {...}` out of a source by brace matching.
function fn(src, name) {
  const start = src.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let i = src.indexOf('{', src.indexOf(')', start));
  let depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}' && --depth === 0) return src.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
}

const NAMES = ['agentCssEscape', 'agentFocusSnapshot', 'agentFocusRestore', 'agentViewportCapture', 'agentViewportRestore', 'withAgentViewportPreserved'];
const body = [
  'const AGENT_VIEWPORT_MIN_DELTA_PX = 1;',
  ...NAMES.map(n => fn(cards, n)),
  'return { ' + NAMES.join(', ') + ' };',
].join('\n');

// A minimal fake page: a window with a scroll offset and a grid of cards laid
// out top to bottom. `shift` adds height above the anchor card after a render,
// which is what a resized neighbour or a rebuilt select does to the real page.
function makePage(order, heights) {
  const state = { scrollY: 300, scrollX: 0, active: null, focusLog: [], order: order.slice(), heights: Object.assign({}, heights), extraAbove: 0 };
  const controlsByKey = new Map();
  const cardTop = name => {
    let y = state.extraAbove - state.scrollY;
    for (const n of state.order) { if (n === name) return y; y += state.heights[n]; }
    return null;
  };
  const makeCard = name => ({
    getAttribute: a => (a === 'data-agent' ? name : null),
    getClientRects: () => [{}],
    getBoundingClientRect: () => { const top = cardTop(name); return { top, bottom: top + state.heights[name], height: state.heights[name] }; },
  });
  const makeControl = (agent, action, disabled) => {
    const attrs = { 'data-agent': agent, 'data-change-action': action };
    const el = { id: '', disabled: !!disabled, getAttribute: a => attrs[a] || null, focus: opts => { state.focusLog.push(opts); state.active = el; } };
    controlsByKey.set(agent + '|' + action, el);
    return el;
  };
  const window_ = {
    get scrollY() { return state.scrollY; }, get scrollX() { return state.scrollX; }, innerHeight: 800,
    scrollBy: (x, y) => { state.scrollY += y; },
    scrollTo: (x, y) => { state.scrollX = x; state.scrollY = y; },
  };
  const grid = {
    querySelectorAll: sel => (sel === '.agent-card[data-agent]' ? state.order.map(makeCard) : []),
    querySelector: sel => {
      const m = sel.match(/data-agent="([^"]+)"\]$/);
      return m && state.order.includes(m[1]) ? makeCard(m[1]) : null;
    },
  };
  const document_ = {
    body: { tag: 'body' },
    get activeElement() { return state.active || document_.body; },
    getElementById: id => (id === 'agents' ? grid : null),
    querySelector: sel => {
      const m = sel.match(/data-agent="([^"]+)"\]\[data-change-action="([^"]+)"\]$/);
      return m ? controlsByKey.get(m[1] + '|' + m[2]) || null : null;
    },
  };
  const api = new Function('window', 'document', 'requestAnimationFrame', body)(window_, document_, undefined);
  return { state, api, makeControl, window: window_ };
}

const ORDER = ['guide', 'scanner', 'supervisor', 'reviewer'];
const HEIGHTS = { guide: 400, scanner: 400, supervisor: 400, reviewer: 400 };

test('scroll position is re-pinned to the switched agent after a re-render that resizes content above it', () => {
  const p = makePage(ORDER, HEIGHTS);
  const before = p.state.scrollY;
  p.api.withAgentViewportPreserved('scanner', () => {
    // Re-render: the card above grows by 120px (new log lines, rebuilt select).
    p.state.extraAbove = 120;
  });
  // The card's viewport offset is unchanged, so the window moved by the shift.
  assert.strictEqual(p.state.scrollY, before + 120);
});

test('no correction when nothing shifted', () => {
  const p = makePage(ORDER, HEIGHTS);
  p.api.withAgentViewportPreserved('scanner', () => {});
  assert.strictEqual(p.state.scrollY, 300);
});

test('the list order is never changed by the guard or by a setting change', () => {
  const p = makePage(ORDER, HEIGHTS);
  const keysBefore = p.state.order.slice();
  p.api.withAgentViewportPreserved('supervisor', () => { /* a.cli/a.model updated in place */ });
  assert.deepStrictEqual(p.state.order, keysBefore);
  assert.ok(!/\.sort\(|\.reverse\(|splice\(|unshift\(/.test(fn(cards, 'withAgentViewportPreserved') + fn(cards, 'agentViewportCapture') + fn(cards, 'agentViewportRestore')),
    'viewport helpers must not reorder cards');
});

test('focus returns to the select that was used, without scrolling', () => {
  const p = makePage(ORDER, HEIGHTS);
  const sel = p.makeControl('scanner', 'switchModel');
  p.state.active = sel;
  const snap = p.api.agentFocusSnapshot(sel);
  assert.deepStrictEqual(snap, { agent: 'scanner', action: 'switchModel', change: true });
  // Re-render replaces the select: focus drops to <body> and a new node exists.
  p.state.active = null;
  const replacement = p.makeControl('scanner', 'switchModel');
  p.api.withAgentViewportPreserved('scanner', () => {}, snap);
  assert.strictEqual(p.state.active, replacement);
  assert.deepStrictEqual(p.state.focusLog, [{ preventScroll: true }]);
});

test('a disabled control is not focused (loading-models state) and focus is restored once re-enabled', () => {
  const p = makePage(ORDER, HEIGHTS);
  const sel = p.makeControl('scanner', 'switchModel', true);
  const snap = { agent: 'scanner', action: 'switchModel', change: true };
  p.api.withAgentViewportPreserved('scanner', () => {}, snap);
  assert.strictEqual(p.state.active, null);
  sel.disabled = false;
  p.api.withAgentViewportPreserved('scanner', () => {}, snap);
  assert.strictEqual(p.state.active, sel);
});

test('focus the operator moved elsewhere during the switch is not stolen back', () => {
  const p = makePage(ORDER, HEIGHTS);
  const other = p.makeControl('guide', 'switchCli');
  p.makeControl('scanner', 'switchModel');
  p.state.active = other;
  p.api.withAgentViewportPreserved('scanner', () => {}, { agent: 'scanner', action: 'switchModel', change: true });
  assert.strictEqual(p.state.active, other);
});

test('page scroll is restored when the anchor card is gone', () => {
  const p = makePage(ORDER, HEIGHTS);
  p.api.withAgentViewportPreserved('scanner', () => { p.state.order = []; p.state.scrollY = 0; });
  assert.strictEqual(p.state.scrollY, 300);
});

test('all three switch handlers re-render inside the viewport guard and keep the control focus', () => {
  for (const name of ['switchCli', 'switchModel', 'switchEffort']) {
    const src = fn(controls, name);
    assert.ok(src.includes('agentFocusSnapshot(document.activeElement)'), name + ' must snapshot focus before the request');
    assert.ok(/withAgentViewportPreserved\(agent, \(\) => \{[\s\S]*renderAgents\(window\._lastAgents\)/.test(src), name + ' must render inside the guard');
  }
  const dd = fn(controls, 'updateModelDropdown');
  assert.strictEqual((dd.match(/withAgentViewportPreserved\(/g) || []).length, 3, 'every model-dropdown rewrite is guarded');
});

test('renderAgents restores focus for id-less controls by agent and action', () => {
  const src = fn(cards, 'renderAgents');
  assert.ok(src.includes('agentFocusSnapshot(document.activeElement)'));
  assert.ok(src.includes('agentFocusRestore(focusSnap)'));
});

test('the server\'s saved-not-running message reaches the toast', () => {
  assert.ok(fn(controls, 'switchModel').includes('data.applied === false && data.message'));
  assert.ok(fn(controls, 'switchCli').includes('data.applied === false && data.message'));
});
