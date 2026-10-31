'use strict';

// Behavioural contract for the shared `.config-info` help-tooltip helper in
// src/pkg/dashboard/static/index.html: hover opens after a delay, focus opens
// at once, hovering into the portal keeps it open, leaving trigger and portal
// hides after a grace period, Escape / outside click / scroll / resize / modal
// open hide it, and only one tooltip is open at a time. The helper is run in a
// vm context against a minimal fake DOM so no browser is needed.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

function extractFunction(name) {
  const start = indexHtml.indexOf('function ' + name + '(');
  assert.notEqual(start, -1, `${name} function exists`);
  const brace = indexHtml.indexOf('{', start);
  let depth = 0;
  for (let i = brace; i < indexHtml.length; i++) {
    const ch = indexHtml[i];
    if (ch === '{') depth++;
    if (ch === '}') depth--;
    if (depth === 0) return indexHtml.slice(start, i + 1);
  }
  throw new Error(`unterminated ${name}`);
}

function extractConst(name) {
  const m = indexHtml.match(new RegExp(`const ${name} = [^;]+;`));
  assert.ok(m, `${name} constant exists`);
  return m[0];
}

function extractListeners() {
  const start = indexHtml.indexOf("document.addEventListener('mouseover', (e) => configTooltipPointerOver(e.target));");
  assert.notEqual(start, -1, 'tooltip listeners exist');
  const end = indexHtml.indexOf("window.addEventListener('resize', () => { if (configTooltipIsOpen()) hideConfigTooltip(); });", start);
  assert.notEqual(end, -1, 'resize listener exists');
  return indexHtml.slice(start, indexHtml.indexOf('\n', end));
}

const HELPER_FUNCTIONS = [
  'configTooltipPortal', 'configTooltipIsOpen', 'configTooltipOwns', 'configTooltipHovered',
  'cancelConfigTooltipOpen', 'cancelConfigTooltipClose', 'positionConfigTooltip',
  'scheduleConfigTooltipOpen', 'hideConfigTooltip', 'scheduleConfigTooltipHide',
  'configTooltipPointerOver', 'configTooltipPointerOut', 'configTooltipFocusIn', 'configTooltipFocusOut',
];

class FakeElement {
  constructor(tag, className) {
    this.tagName = tag;
    this.nodeType = 1;
    this.className = className || '';
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.style = {};
    this.hidden = false;
    this.innerHTML = '';
    this.id = '';
    this.offsetWidth = 200;
    this.offsetHeight = 40;
    this.hovered = false;
    this.rect = { left: 100, top: 100, width: 15, height: 15, right: 115, bottom: 115 };
  }
  get isConnected() {
    let n = this;
    while (n.parentNode) n = n.parentNode;
    return n.isRoot === true;
  }
  get textContent() { return this._textContent || ''; }
  set textContent(value) {
    this._textContent = String(value);
    for (const child of this.children) child.parentNode = null;
    this.children = [];
  }
  get classList() {
    const self = this;
    return {
      [Symbol.iterator]: function* () { yield* self.className.split(/\s+/).filter(Boolean); },
      contains: (c) => self.className.split(/\s+/).includes(c),
    };
  }
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; }
  removeChild(child) { this.children = this.children.filter(c => c !== child); child.parentNode = null; }
  contains(node) {
    for (let n = node; n; n = n.parentNode) if (n === this) return true;
    return false;
  }
  closest(selector) {
    const classes = selector.split(',').map(s => s.trim().replace(/^\./, ''));
    for (let n = this; n; n = n.parentNode) if (n.classList && classes.some(cls => n.classList.contains(cls))) return n;
    return null;
  }
  querySelector(selector) {
    const cls = selector.replace(/^\./, '');
    for (const c of this.children) {
      if (c.classList.contains(cls)) return c;
      const deep = c.querySelector(selector);
      if (deep) return deep;
    }
    return null;
  }
  matches(selector) {
    if (selector === ':hover') return this.hovered;
    return false;
  }
  getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attrs, name) ? this.attrs[name] : null; }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  removeAttribute(name) { delete this.attrs[name]; }
  getBoundingClientRect() { return this.rect; }
}

function makeTrigger(root, label) {
  const info = new FakeElement('span', 'config-info');
  const tip = new FakeElement('span', 'config-tooltip overview-partition-tooltip');
  tip.innerHTML = label + ' body';
  info.appendChild(tip);
  info.setAttribute('aria-describedby', label + '-tip');
  root.appendChild(info);
  return info;
}

function harness() {
  const root = new FakeElement('body');
  root.isRoot = true;
  const byId = {};
  const listeners = { document: {}, window: {} };
  let now = 0;
  const timers = [];
  const document = {
    body: root,
    getElementById: (id) => byId[id] || null,
    createElement: (tag) => new FakeElement(tag),
    addEventListener: (type, fn) => { (listeners.document[type] = listeners.document[type] || []).push(fn); },
  };
  const window = {
    innerWidth: 1440,
    innerHeight: 900,
    addEventListener: (type, fn) => { (listeners.window[type] = listeners.window[type] || []).push(fn); },
  };
  const origAppend = root.appendChild.bind(root);
  root.appendChild = (child) => { origAppend(child); if (child.id) byId[child.id] = child; return child; };
  const context = {
    document, window, exports: {},
    setTimeout: (fn, ms) => { const t = { fn, at: now + ms, id: timers.length + 1 }; timers.push(t); return t.id; },
    clearTimeout: (id) => { const i = timers.findIndex(t => t.id === id); if (i >= 0) timers.splice(i, 1); },
    Array,
  };
  const source = [
    extractConst('CONFIG_TOOLTIP_EDGE_PAD_PX'),
    extractConst('CONFIG_TOOLTIP_GAP_PX'),
    extractConst('CONFIG_TOOLTIP_DEFAULT_WIDTH_PX'),
    extractConst('CONFIG_TOOLTIP_OPEN_DELAY_MS'),
    extractConst('CONFIG_TOOLTIP_CLOSE_GRACE_MS'),
    extractConst('CONFIG_TOOLTIP_PORTAL_ID'),
    'let _configTooltipTrigger = null;',
    "let _configTooltipOriginalDescribedBy = '';",
    'let _configTooltipOpenTimer = 0;',
    'let _configTooltipCloseTimer = 0;',
    ...HELPER_FUNCTIONS.map(extractFunction),
    ...['esc', 'sidebarPairBadgeText', 'setSidebarPairBadge', 'contributorsOnlineTitle', 'contributorsOnlineTooltip', 'updateContributorBadge'].map(extractFunction),
    extractConst('NAV_COUNT_CUE_MS'),
    'const _navCountDeltas = new Map();',
    extractFunction('navCountDelta'),
    'let _cachedContributors = [];',
    'function refreshSectionCardShell() {} function setSectionSummary() {} function contributorsSectionSummary() {}',
    extractListeners(),
    ...HELPER_FUNCTIONS.map(n => `exports.${n} = ${n};`),
    'exports.updateContributorBadge = updateContributorBadge;',
    'exports.openDelay = CONFIG_TOOLTIP_OPEN_DELAY_MS; exports.closeGrace = CONFIG_TOOLTIP_CLOSE_GRACE_MS;',
  ].join('\n');
  vm.runInNewContext(source, context);
  // The portal is created lazily by the helper; register it for getElementById once it exists.
  const h = {
    api: context.exports,
    root,
    portal: () => byId['config-tooltip-portal'] || null,
    fire(where, type, target, relatedTarget) {
      for (const fn of listeners[where][type] || []) fn({ target, relatedTarget, key: type === 'keydown' ? 'Escape' : undefined });
    },
    advance(ms) {
      const until = now + ms;
      while (true) {
        const due = timers.filter(t => t.at <= until).sort((a, b) => a.at - b.at)[0];
        if (!due) break;
        timers.splice(timers.indexOf(due), 1);
        now = due.at;
        due.fn();
      }
      now = until;
    },
    pendingTimers: () => timers.length,
    isOpen: () => context.exports.configTooltipIsOpen(),
  };
  return h;
}

test('hover opens after the short delay, not synchronously', () => {
  const h = harness();
  const info = makeTrigger(h.root, 'held');
  h.fire('document', 'mouseover', info);
  assert.equal(h.isOpen(), false, 'not open before the hover delay');
  h.advance(h.api.openDelay - 1);
  assert.equal(h.isOpen(), false);
  h.advance(1);
  assert.equal(h.isOpen(), true, 'open once the hover delay elapses');
  assert.equal(h.portal().hidden, false);
  assert.equal(h.portal().innerHTML, 'held body');
  assert.equal(info.getAttribute('aria-describedby'), 'config-tooltip-portal');
});

test('focus opens immediately and blur hides after the grace period', () => {
  const h = harness();
  const info = makeTrigger(h.root, 'held');
  h.fire('document', 'focusin', info);
  assert.equal(h.isOpen(), true);
  h.fire('document', 'focusout', info, h.root);
  assert.equal(h.isOpen(), true, 'still open during the grace period');
  h.advance(h.api.closeGrace);
  assert.equal(h.isOpen(), false);
  assert.equal(info.getAttribute('aria-describedby'), 'held-tip', 'original aria-describedby restored');
});

test('leaving the trigger hides after the grace period', () => {
  const h = harness();
  const info = makeTrigger(h.root, 'held');
  const elsewhere = h.root.appendChild(new FakeElement('div'));
  h.fire('document', 'mouseover', info);
  h.advance(h.api.openDelay);
  assert.equal(h.isOpen(), true);
  h.fire('document', 'mouseout', info, elsewhere);
  h.fire('document', 'mouseover', elsewhere);
  assert.equal(h.isOpen(), true, 'grace period keeps it open briefly');
  h.advance(h.api.closeGrace - 1);
  assert.equal(h.isOpen(), true);
  h.advance(1);
  assert.equal(h.isOpen(), false);
  assert.equal(h.portal().hidden, true);
});

test('hovering into the popover keeps it open; leaving the popover hides it', () => {
  const h = harness();
  const info = makeTrigger(h.root, 'held');
  const elsewhere = h.root.appendChild(new FakeElement('div'));
  h.fire('document', 'mouseover', info);
  h.advance(h.api.openDelay);
  const portal = h.portal();
  const button = portal.appendChild(new FakeElement('span', 'hv-btn'));
  h.fire('document', 'mouseout', info, portal);
  h.fire('document', 'mouseover', button);
  h.advance(h.api.closeGrace * 4);
  assert.equal(h.isOpen(), true, 'popover hover cancels the pending hide');
  h.fire('document', 'mouseout', button, elsewhere);
  h.fire('document', 'mouseover', elsewhere);
  h.advance(h.api.closeGrace);
  assert.equal(h.isOpen(), false);
});

test('a quick pass over the trigger never opens the tooltip', () => {
  const h = harness();
  const info = makeTrigger(h.root, 'held');
  const elsewhere = h.root.appendChild(new FakeElement('div'));
  h.fire('document', 'mouseover', info);
  h.advance(h.api.openDelay / 2);
  h.fire('document', 'mouseout', info, elsewhere);
  h.advance(h.api.openDelay * 2);
  assert.equal(h.isOpen(), false);
  assert.equal(h.pendingTimers(), 0, 'no stray timers');
});

test('only one tooltip is open at a time', () => {
  const h = harness();
  const a = makeTrigger(h.root, 'held');
  const b = makeTrigger(h.root, 'blocked');
  h.fire('document', 'focusin', a);
  assert.equal(h.portal().innerHTML, 'held body');
  h.fire('document', 'mouseout', a, b);
  h.fire('document', 'mouseover', b);
  h.advance(h.api.openDelay);
  assert.equal(h.isOpen(), true);
  assert.equal(h.portal().innerHTML, 'blocked body');
  assert.equal(a.getAttribute('aria-describedby'), 'held-tip', 'previous trigger released');
  assert.equal(b.getAttribute('aria-describedby'), 'config-tooltip-portal');
});

test('hide does not depend on trigger identity: a re-rendered strip still closes', () => {
  const h = harness();
  const strip = h.root.appendChild(new FakeElement('div', 'strip'));
  const oldInfo = makeTrigger(strip, 'held');
  const elsewhere = h.root.appendChild(new FakeElement('div'));
  h.fire('document', 'mouseover', oldInfo);
  h.advance(h.api.openDelay);
  assert.equal(h.isOpen(), true);
  // Status poll re-renders the strip: the hovered trigger is replaced by a new node.
  strip.removeChild(oldInfo);
  const newInfo = makeTrigger(strip, 'held');
  h.fire('document', 'mouseout', newInfo, elsewhere);
  h.fire('document', 'mouseover', elsewhere);
  h.advance(h.api.closeGrace);
  assert.equal(h.isOpen(), false, 'tooltip opened by a detached trigger still hides');
});

test('Escape, outside click, scroll and resize hide the tooltip', () => {
  for (const how of ['click', 'scroll', 'resize', 'hide']) {
    const h = harness();
    const info = makeTrigger(h.root, 'held');
    const elsewhere = h.root.appendChild(new FakeElement('div'));
    h.fire('document', 'focusin', info);
    assert.equal(h.isOpen(), true, how);
    if (how === 'click') h.fire('document', 'click', elsewhere);
    else if (how === 'scroll') h.fire('window', 'scroll');
    else if (how === 'resize') h.fire('window', 'resize');
    else assert.equal(h.api.hideConfigTooltip(), true, 'Escape path (hiveDismissTopOverlay) reports it closed something');
    assert.equal(h.isOpen(), false, how + ' hides');
  }
});

test('clicking inside the popover does not hide it', () => {
  const h = harness();
  const info = makeTrigger(h.root, 'held');
  h.fire('document', 'focusin', info);
  const button = h.portal().appendChild(new FakeElement('span', 'hv-btn'));
  h.fire('document', 'click', button);
  assert.equal(h.isOpen(), true);
});

test('scrolling a long online ID list keeps the tooltip open, page scrolling still dismisses it', () => {
  const h = harness();
  const pill = makeTrigger(h.root, 'contributors');
  pill.className = 'contributor-online-trigger';
  h.fire('document', 'focusin', pill);
  h.fire('window', 'scroll', h.portal());
  assert.equal(h.isOpen(), true);
  h.fire('window', 'scroll', h.root);
  assert.equal(h.isOpen(), false);
});

test('Escape dismissal and modal-open hooks are wired to the shared helper', () => {
  const dismiss = extractFunction('hiveDismissTopOverlay');
  assert.match(dismiss, /configTooltipIsOpen\(\)/);
  assert.match(dismiss, /close: hideConfigTooltip, rank: 40000/);
  const ranks = [...dismiss.matchAll(/rank: (\d+)/g)].map(m => Number(m[1]));
  assert.equal(Math.max(...ranks), 40000, 'tooltip is the first thing Escape dismisses');
  const remember = extractFunction('hiveRememberModalOpen');
  assert.match(remember, /hideConfigTooltip\(\)/);
});

test('portal stylesheet hides the portal when the hidden attribute is set', () => {
  assert.match(indexHtml, /\.config-tooltip-portal\[hidden\] \{ display: none; \}/);
});

test('Contributors tooltip targets the count pill and supports focus without changing navigation or pill styling', () => {
  const badge = indexHtml.match(/<span id="contributors-sidebar-badge"[^>]+>/)[0];
  assert.match(badge, /class="badge-count oc-nav-count contributor-online-trigger"/);
  assert.match(badge, /tabindex="0" role="link"/);
  assert.match(badge, /data-keydown-action="ocNavigate" data-arg0="contributors-section"/);
  assert.match(indexHtml, /data-action="ocNavigate" data-arg0="contributors-section"><span class="oc-nav-emoji">👥/);
  assert.match(indexHtml, /\.contributor-online-ids li \{ display: block; \}/, 'one ID per line');

  const h = harness();
  const pill = new FakeElement('span', 'badge-count oc-nav-count contributor-online-trigger');
  pill.id = 'contributors-sidebar-badge';
  h.root.appendChild(pill);
  h.api.updateContributorBadge({ online: 2, registered: 4, presence_window: '15m', online_ids: ['clanker-a', 'clanker-b'] });
  assert.equal(pill.textContent, '2/4');
  assert.equal(pill.getAttribute('title'), null, 'no duplicate browser-native title');
  assert.match(pill.getAttribute('aria-label'), /2 online · 4 registered/);
  h.fire('document', 'focusin', pill);
  assert.equal(h.isOpen(), true);
  assert.match(h.portal().innerHTML, /<li>clanker-a<\/li><li>clanker-b<\/li>/);

  h.api.updateContributorBadge({ online: 1, registered: 4, online_ids: ['clanker-reconnected'] });
  assert.equal(pill.textContent, '1/4');
  assert.match(h.portal().innerHTML, /<li>clanker-reconnected<\/li>/);
  assert.doesNotMatch(h.portal().innerHTML, /clanker-a|clanker-b/);

  h.api.updateContributorBadge({ online: 0, registered: 4, online_ids: [] });
  assert.equal(pill.textContent, '0/4');
  assert.match(h.portal().innerHTML, /No ClankeRs online/);
  h.api.hideConfigTooltip();
  h.fire('document', 'mouseover', pill);
  h.advance(h.api.openDelay);
  assert.equal(h.isOpen(), true, 'same pill also opens on hover');
});

test('Contributors tooltip escapes IDs and distinguishes unavailable IDs from no online ClankeRs', () => {
  const context = {};
  vm.runInNewContext(['esc', 'contributorsOnlineTitle', 'contributorsOnlineTooltip'].map(extractFunction).join('\n'), context);
  const html = context.contributorsOnlineTooltip({ online: 1, registered: 2, online_ids: ['<img src=x onerror="bad">&'] });
  assert.match(html, /<li>&lt;img src=x onerror=&quot;bad&quot;&gt;&amp;<\/li>/);
  assert.doesNotMatch(html, /<img/);
  for (const pool of [{ online: 1, registered: 2 }, { online: 1, online_ids: [] }, { online: 1, online_ids: [''] }]) {
    const tip = context.contributorsOnlineTooltip(pool);
    assert.match(tip, /Online ClankeR IDs unavailable/);
    assert.doesNotMatch(tip, /No ClankeRs online/);
  }
});
