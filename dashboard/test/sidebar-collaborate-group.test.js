'use strict';

// The Collaborate sidebar group (Inbox, Calendar, Meetings, Inception (spek))
// must survive the dashboard layout sidebar sort. Those sections are in
// DASHBOARD_LAYOUT_TEMPLATE (so their cards get layout padding, collapse
// defaults and ordering), and dashboardSortSidebar used to re-home every
// layout-listed nav link into #dashboard-section-nav-group, which emptied and
// hid Collaborate. This test renders the real sidebar markup and the real
// layout functions from the generated page, applies a saved layout that puts
// the Collaborate sections first, and checks the group keeps its links, in
// order, right after Dashboard and before Agents.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

const DASHBOARD_GROUP_ID = 'dashboard-section-nav-group';
const AGENT_GROUP_ID = 'oc-agent-nav-group';
const COLLABORATE_LABEL = 'Collaborate';
const COLLABORATE_LINKS = ['Inbox', 'Calendar', 'Meetings', 'Customers', 'Inception (spek)'];
const COLLABORATE_SECTIONS = ['inbox-section', 'calendar-section', 'meetings-section', 'customers-section', 'sessions-section'];
const LAYOUT_KEY = 'hive.dashboard.layout';

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

function extractVar(name) {
  const m = new RegExp('var ' + name + '=[^;]+;').exec(indexHtml);
  assert.ok(m, `${name} var exists`);
  return m[0];
}

// Minimal element model: enough DOM for the sidebar sort/drag helpers.
class El {
  constructor(attrs) {
    this.attrs = Object.assign({}, attrs);
    this.children = [];
    this.parentElement = null;
    this.hidden = 'hidden' in this.attrs;
    this.textContent = this.attrs._text || '';
    this.dataset = {};
  }
  get id() { return this.attrs.id || ''; }
  hasClass(c) { return (this.attrs.class || '').split(/\s+/).includes(c); }
  getAttribute(n) { return n in this.attrs ? this.attrs[n] : null; }
  setAttribute(n, v) { this.attrs[n] = String(v); }
  removeAttribute(n) { delete this.attrs[n]; }
  appendChild(ch) {
    if (ch.parentElement) ch.parentElement.children = ch.parentElement.children.filter((x) => x !== ch);
    ch.parentElement = this;
    this.children.push(ch);
    return ch;
  }
  descendants() { return this.children.flatMap((c) => [c, ...c.descendants()]); }
  closest(sel) {
    assert.equal(sel, '.oc-nav-group', `unexpected closest(${sel})`);
    for (let el = this; el; el = el.parentElement) if (el.hasClass('oc-nav-group')) return el;
    return null;
  }
  querySelectorAll(sel) {
    if (sel === '.oc-nav-item') return this.descendants().filter((e) => e.hasClass('oc-nav-item'));
    if (sel === ':scope > [data-dashboard-section]') return this.children.filter((c) => c.attrs['data-dashboard-section']);
    throw new Error(`unexpected querySelectorAll(${sel})`);
  }
  querySelector() { return null; }
}

function parseAttrs(s) {
  const out = {};
  for (const m of s.matchAll(/([\w-]+)(?:="([^"]*)")?/g)) out[m[1]] = m[2] === undefined ? '' : m[2];
  return out;
}

// Build the sidebar tree from the generated page: every .oc-nav-group in
// source order, its heading, and its .oc-nav-item links.
function buildSidebar() {
  const navStart = indexHtml.indexOf('<nav id="oc-sidebar"');
  assert.notEqual(navStart, -1, 'sidebar nav exists');
  const nav = indexHtml.slice(navStart, indexHtml.indexOf('</nav>', navStart));
  const sidebar = new El({ id: 'oc-sidebar' });
  const starts = [...nav.matchAll(/<div class="oc-nav-group"([^>]*)>/g)];
  assert.ok(starts.length > 0, 'sidebar has nav groups');
  starts.forEach((m, i) => {
    const seg = nav.slice(m.index, i + 1 < starts.length ? starts[i + 1].index : nav.length);
    const group = sidebar.appendChild(new El(Object.assign({ class: 'oc-nav-group' }, parseAttrs(m[1]))));
    const label = /<div class="oc-nav-label">([^<]+)</.exec(seg) || /oc-tree-arrow">[^<]*<\/span>\s*<span>([^<]+)</.exec(seg);
    group.label = label ? label[1].trim() : '';
    for (const a of seg.matchAll(/<a ([^>]*class="oc-nav-item[^"]*"[^>]*)>([\s\S]*?)<\/a>/g)) {
      const text = /<span class="oc-nav-text">([^<]*)</.exec(a[2]);
      group.appendChild(new El(Object.assign(parseAttrs(a[1]), { _text: text ? text[1] : '' })));
    }
  });
  return sidebar;
}

function makeContext({ saved, sessionsEnabled }) {
  const sidebar = buildSidebar();
  const store = {};
  if (saved) store[LAYOUT_KEY] = JSON.stringify(saved);
  const root = new El({ id: 'hive-dashboard-root' });
  const all = () => sidebar.descendants();
  const ctx = {
    window: { _lastStatus: { features: { experiment_lab: true } }, _sessionsEnabled: sessionsEnabled, _customersEnabled: true },
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    document: {
      getElementById: (id) => (id === root.id ? root : all().find((e) => e.id === id) || null),
      querySelectorAll: (sel) => {
        if (sel === '.oc-nav-item[data-section]') return all().filter((e) => e.hasClass('oc-nav-item') && e.attrs['data-section']);
        if (sel === '.oc-nav-group') return sidebar.children;
        throw new Error(`unexpected document.querySelectorAll(${sel})`);
      },
    },
    dashboardRegionEl: () => root,
    dashboardEnsureSidebarHandles: () => {},
    sidebar,
    root,
  };
  vm.createContext(ctx);
  const src = [
    extractVar('DASHBOARD_LAYOUT_VERSION'),
    extractVar('DASHBOARD_LAYOUT_TEMPLATE'),
    extractVar('DASHBOARD_LAYOUT_CARD_SELECTOR'),
    extractVar('DASHBOARD_LAYOUT_KEY'),
    extractVar('DASHBOARD_SIDEBAR_HOME_ID'),
    ...['dashboardAliasSectionId', 'dashboardFeatureEnabled', 'experimentLabEnabled', 'sessionsFeatureEnabled', 'customersFeatureEnabled',
      'dashboardFeatureSectionHidden', 'dashboardLayoutAllIds', 'dashboardLayoutNormalize', 'dashboardLayoutRead',
      'dashboardLayoutCurrent', 'dashboardSidebarLayoutItems', 'dashboardSidebarItemInOwnGroup',
      'dashboardSidebarNavItems', 'dashboardSidebarLayoutFromDom', 'dashboardSortSidebar'].map(extractFunction),
  ].join('\n');
  vm.runInContext(src, ctx);
  // Lay the main-page cards out the way dashboardApplyLayout would for the saved layout.
  const order = vm.runInContext('dashboardLayoutNormalize(dashboardLayoutRead()).main', ctx);
  order.forEach((id) => root.appendChild(new El({ id, 'data-dashboard-section': id })));
  return ctx;
}

function groupByLabel(ctx, label) { return ctx.sidebar.children.find((g) => g.label === label); }
function linkTexts(group) { return group.querySelectorAll('.oc-nav-item').map((e) => e.textContent); }

const SAVED_COLLABORATE_FIRST = { v: 1, main: [...COLLABORATE_SECTIONS, 'overview-section', 'governor', 'pr-throughput-section'] };

for (const sessionsEnabled of [true, false]) {
  test(`Collaborate keeps Inbox, Calendar, Meetings, Inception (spek) after the sidebar sort (sessions ${sessionsEnabled ? 'on' : 'off'})`, () => {
    const ctx = makeContext({ saved: SAVED_COLLABORATE_FIRST, sessionsEnabled });
    const layout = vm.runInContext('DASHBOARD_LAYOUT_TEMPLATE.main', ctx);
    for (const id of COLLABORATE_SECTIONS) assert.ok(layout.includes(id), `${id} is still a layout section`);

    vm.runInContext('dashboardSortSidebar()', ctx);

    const collaborate = groupByLabel(ctx, COLLABORATE_LABEL);
    assert.ok(collaborate, 'Collaborate heading exists');
    assert.equal(collaborate.hidden, false, 'Collaborate group is visible');
    assert.deepEqual(linkTexts(collaborate), COLLABORATE_LINKS);

    const dashboard = ctx.document.getElementById(DASHBOARD_GROUP_ID);
    const inDashboard = dashboard.querySelectorAll('.oc-nav-item').map((e) => e.getAttribute('data-section'));
    for (const id of COLLABORATE_SECTIONS) assert.ok(!inDashboard.includes(id), `${id} is not in the Dashboard group`);
    // Links authored in the Dashboard group are still sorted by the saved layout.
    assert.deepEqual(inDashboard, ['overview-section', 'governor', 'pr-throughput-section', 'activity-monitor-section']);
  });
}

test('sidebar group order is Dashboard, Collaborate, Agents, Resources, Intelligence, Admin, Help', () => {
  const ctx = makeContext({ saved: null, sessionsEnabled: true });
  vm.runInContext('dashboardSortSidebar()', ctx);
  const groups = ctx.sidebar.children;
  const dash = groups.findIndex((g) => g.id === DASHBOARD_GROUP_ID);
  const collab = groups.findIndex((g) => g.label === COLLABORATE_LABEL);
  const agents = groups.findIndex((g) => g.id === AGENT_GROUP_ID);
  assert.equal(dash, 0, 'Dashboard is the first group');
  assert.equal(collab, dash + 1, 'Collaborate comes right after Dashboard');
  assert.equal(agents, collab + 1, 'Agents comes right after Collaborate');
  assert.deepEqual(groups.map((g) => g.label).filter((l) => l !== 'Filters'),
    ['Dashboard', 'Collaborate', 'Agents', 'Resources', 'Intelligence', 'Admin', 'Help']);
});

test('reordering Dashboard links from the sidebar keeps Collaborate sections where the layout had them', () => {
  const ctx = makeContext({ saved: SAVED_COLLABORATE_FIRST, sessionsEnabled: true });
  vm.runInContext('dashboardSortSidebar()', ctx);
  const dashboard = ctx.document.getElementById(DASHBOARD_GROUP_ID);
  // Move Governor to the top of the Dashboard group, as a drag would.
  const governor = dashboard.children.find((e) => e.getAttribute('data-section') === 'governor');
  dashboard.children = [governor, ...dashboard.children.filter((e) => e !== governor)];
  const before = [...vm.runInContext('dashboardLayoutNormalize(dashboardLayoutCurrent()).main', ctx)];
  const main = [...vm.runInContext('dashboardSidebarLayoutFromDom().main', ctx)];
  // Governor and Overview trade slots; every other section, Collaborate included, keeps its place.
  const want = before.map((id) => (id === 'governor' ? 'overview-section' : id === 'overview-section' ? 'governor' : id));
  assert.deepEqual(main, want);
  for (const id of COLLABORATE_SECTIONS) assert.equal(main.indexOf(id), before.indexOf(id), `${id} keeps its layout position`);
  assert.equal(new Set(main).size, main.length, 'no duplicate sections');
});
