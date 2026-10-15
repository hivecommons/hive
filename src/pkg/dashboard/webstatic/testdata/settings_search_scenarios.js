// Scenarios for settings_search_harness.js; each returns a JSON-serialisable
// result the Go test asserts on.
'use strict';

function openSettings(h) {
  const d = h.document;
  h.define('_configModalOpen', true);
  const state = h.resolveName('_configState');
  state.type = 'governor';
  // Enough config for conditionally rendered sections (watchdog, otel
  // fields) to appear, as they do against a live hive.
  state.data = {
    dashboard: { strategy_lab: false, port: 3001 },
    health: { watchdog: { mode: 'observe', probe_interval_s: 30 } },
    features: { tracingEnabled: true, rotationEnabled: true },
  };
  // Mirror openConfigDialog: build the tab strip and show the search panel.
  const tabs = h.resolveName('GOVERNOR_CONFIG_TABS');
  d.getElementById('config-tabs').innerHTML = tabs.map((t, i) => '<div class="config-tab' + (i === 0 ? ' active' : '') + '" data-tab="' + t + '" data-action="switchConfigTab">' + t + '</div>').join('');
  h.call('setupSettingsSearchControls', 'governor');
  h.call('switchConfigTab', tabs[0]);
  return state;
}

function activeTab(h) {
  const t = h.document.querySelector('.config-tab.active');
  return t ? t.getAttribute('data-tab') : '';
}

// Clicks a [data-action] element the way the document-level dispatcher does.
function clickAction(h, el) {
  const action = el.getAttribute('data-action');
  const args = h.call('hiveResolveActionArgs', el, { type: 'click', target: el, preventDefault() {}, stopPropagation() {} });
  return h.resolveName(action)(...args);
}

module.exports = {
  // Search → "Open tab" lands on the matching row.
  async openTab(h) {
    openSettings(h);
    const d = h.document;
    const input = d.getElementById('settings-search-input');
    input.value = 'strategy lab';
    h.call('applySettingsSearch', input.value);
    const groups = Array.from(d.querySelectorAll('.settings-search-group')).map(g => g.getAttribute('data-settings-search-tab'));
    const hits = Array.from(d.querySelectorAll('.settings-search-hit')).map(x => ({ tab: x.getAttribute('data-arg0'), key: x.getAttribute('data-arg1'), label: x.getAttribute('data-arg2'), action: x.getAttribute('data-action') }));
    const btn = d.querySelector('.settings-search-group[data-settings-search-tab="Dashboard"] button[data-action="settingsSearchOpenTab"]');
    if (!btn) return { error: 'no Open tab button for Dashboard', groups, hits };
    clickAction(h, btn);
    await h.flush();
    const body = d.getElementById('config-body');
    const row = body.querySelector('[data-setting-key="dashboard.strategy_lab"]');
    const scrolled = h.scrollCalls.map(c => ({ key: c.el.getAttribute('data-setting-key'), block: c.opts && c.opts.block }));
    return {
      groups, hits,
      activeTab: activeTab(h),
      rowFound: !!row,
      rowClass: row ? row.className : '',
      scrolled,
      focused: d.activeElement ? d.activeElement.localName + (d.activeElement.getAttribute('data-setting-key') ? '[' + d.activeElement.getAttribute('data-setting-key') + ']' : '') : '',
      searchCleared: input.value === '' && d.getElementById('settings-search-clear').hidden === true,
      missing: h.report.missing(),
    };
  },

  // Clicking the result row itself and pressing Enter both land on the row.
  async openHitAndEnter(h) {
    openSettings(h);
    const d = h.document;
    const input = d.getElementById('settings-search-input');
    h.call('applySettingsSearch', 'kick log source');
    const hit = d.querySelector('.settings-search-hit');
    if (!hit) return { error: 'no hit' };
    clickAction(h, hit);
    await h.flush();
    const body = d.getElementById('config-body');
    const row1 = body.querySelector('.cfg-row-flash');
    const first = { tab: activeTab(h), key: row1 ? row1.getAttribute('data-setting-key') : '', scrolled: h.scrollCalls.length };
    h.scrollCalls.length = 0;
    h.call('switchConfigTab', 'General');
    h.call('applySettingsSearch', 'strategy lab');
    input.listeners.keydown.forEach(fn => fn({ key: 'Enter', preventDefault() {} }));
    await h.flush();
    const row2 = d.getElementById('config-body').querySelector('.cfg-row-flash');
    return { first, second: { tab: activeTab(h), key: row2 ? row2.getAttribute('data-setting-key') : '', scrolled: h.scrollCalls.length } };
  },

  // A result row inside a closed <details> is expanded before it is revealed.
  async revealExpandsDetails(h) {
    openSettings(h);
    const d = h.document;
    const body = d.getElementById('config-body');
    body.innerHTML = '<details class="settings-subsection-details"><summary>Advanced</summary><div class="config-field"><label>Thing ' + h.call('yamlHint', 'x.y') + '</label><input data-change-action="markDirty" data-arg0="x" data-arg1="y"></div></details>';
    h.call('settingsAnnotateRows', body);
    const row = h.call('settingsFindRow', body, 'x.y', '');
    h.call('settingsRevealRow', row);
    const details = body.querySelector('details');
    return { rowKey: row ? row.getAttribute('data-setting-key') : '', detailsOpen: details.open, flashed: row.classList.contains('cfg-row-flash'), focused: d.activeElement && d.activeElement.localName };
  },

  // Every index entry must resolve to a rendered control: a hand-written
  // entry whose tab or key drifts from the renderer is reported here.
  // A static entry whose `tab` hint is wrong still lists under, and opens,
  // the tab where its key actually renders.
  async driftedEntry(h) {
    openSettings(h);
    const d = h.document;
    const statics = h.resolveName('SETTINGS_SEARCH_STATIC_HITS');
    statics.push({ tab: 'Features', section: 'Dashboard', label: 'Drifted Lab', key: 'dashboard.strategy_lab', text: 'zzdrifted' });
    h.resolveName('_settingsSearch').index = null;
    h.call('applySettingsSearch', 'zzdrifted');
    const groups = Array.from(d.querySelectorAll('.settings-search-group')).map(g => g.getAttribute('data-settings-search-tab'));
    const resolved = h.call('settingsSearchResolveTab', 'Features', 'dashboard.strategy_lab');
    const hit = d.querySelector('.settings-search-hit');
    if (!hit) return { error: 'no hit', groups };
    clickAction(h, hit);
    await h.flush();
    const row = d.getElementById('config-body').querySelector('.cfg-row-flash');
    return { groups, resolved, activeTab: activeTab(h), key: row ? row.getAttribute('data-setting-key') : '' };
  },

  // A control the tab fills in after its own fetch is still found by the
  // short retry loop.
  async lazyRow(h) {
    openSettings(h);
    const d = h.document;
    h.call('settingsSearchOpenHit', 'General', 'lazy.key', 'Lazy thing');
    await h.flush();
    const before = d.getElementById('config-body').querySelector('.cfg-row-flash');
    const body = d.getElementById('config-body');
    body.innerHTML += '<div class="config-field" data-setting-key="lazy.key"><label>Lazy thing</label><input></div>';
    await new Promise(r => setTimeout(r, h.resolveName('SETTINGS_REVEAL_RETRY_MS') * 3));
    const row = body.querySelector('.cfg-row-flash');
    return { flashedBefore: !!before, key: row ? row.getAttribute('data-setting-key') : '', scrolled: h.scrollCalls.length };
  },

  audit(h) {
    openSettings(h);
    const index = h.call('settingsSearchBuildIndex');
    const statics = h.resolveName('SETTINGS_SEARCH_STATIC_HITS');
    const keyTabs = h.resolveName('_settingsSearch').keyTabs;
    const tabs = h.resolveName('GOVERNOR_CONFIG_TABS');
    const rendered = {};
    tabs.forEach(tab => {
      const wrap = h.document.createElement('div');
      wrap.innerHTML = h.call('renderGovernorTab', tab) || '';
      h.call('settingsAnnotateRows', wrap);
      rendered[tab] = wrap;
    });
    const problems = [];
    statics.forEach(item => {
      if (item.lazy) return; // filled in by the tab's own fetch; not renderable here
      const resolvedTab = h.call('settingsSearchResolveTab', item.tab, item.key);
      if (!item.key) {
        const row = h.call('settingsFindRow', rendered[item.tab], '', item.label);
        if (!row) problems.push({ entry: item.label, tab: item.tab, key: item.key, problem: 'keyless entry has no row labelled "' + item.label + '" in its tab' });
        return;
      }
      const tabsForKey = h.call('settingsSearchTabsForKey', item.key);
      if (!tabsForKey.length) { problems.push({ entry: item.label, tab: item.tab, key: item.key, problem: 'no rendered control carries this key' }); return; }
      if (resolvedTab !== item.tab) problems.push({ entry: item.label, tab: item.tab, key: item.key, problem: 'control renders in ' + tabsForKey.join('/') + ', not ' + item.tab });
      // An exact key must land on one row; a group key (governor.modes) may
      // cover several, and "Open tab" scrolls to the first.
      const rows = h.call('settingsRowsForKey', rendered[resolvedTab], item.key);
      const exact = rows.some(r => r.getAttribute('data-setting-key') === item.key);
      if (rows.length === 0 || (exact && rows.length !== 1)) problems.push({ entry: item.label, tab: item.tab, key: item.key, problem: rows.length + ' innermost rows match in ' + resolvedTab });
    });
    // Rendered (non-static) entries: each key must resolve to one innermost row in its own tab.
    const seen = new Set();
    index.forEach(item => {
      if (!item.key || statics.some(s => s.key === item.key && s.label === item.label)) return;
      const id = item.tab + '\n' + item.key;
      if (seen.has(id)) return;
      seen.add(id);
      const rows = h.call('settingsRowsForKey', rendered[item.tab], item.key);
      if (rows.length !== 1) problems.push({ entry: item.label, tab: item.tab, key: item.key, problem: rows.length + ' innermost rows match' });
    });
    return { entries: index.length, statics: statics.length, keys: Object.keys(keyTabs).length, problems, missing: h.report.missing() };
  },
};
