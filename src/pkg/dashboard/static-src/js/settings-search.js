    function settingsSearchControls() {
      return {
        panel: document.getElementById('settings-search-panel'),
        input: document.getElementById('settings-search-input'),
        clear: document.getElementById('settings-search-clear'),
        count: document.getElementById('settings-search-count'),
      };
    }

    function setupSettingsSearchControls(type) {
      const c = settingsSearchControls();
      if (!c.panel || !c.input) return;
      const on = type === 'governor';
      c.panel.hidden = !on;
      c.panel.classList.toggle('active', on);
      if (!on) {
        _settingsSearch = { active: false, query: '', previousTab: '', index: null, bound: _settingsSearch.bound };
        c.input.value = '';
        if (c.clear) c.clear.hidden = true;
        if (c.count) c.count.textContent = 'Search settings';
        return;
      }
      if (!_settingsSearch.bound) {
        c.input.addEventListener('input', () => applySettingsSearch(c.input.value));
        c.input.addEventListener('keydown', (e) => {
          if (e.key === 'Escape') { e.preventDefault(); clearSettingsSearch(); }
          else if (e.key === 'Enter') { e.preventDefault(); settingsSearchOpenFirst(); }
        });
        if (c.clear) c.clear.addEventListener('click', clearSettingsSearch);
        const overlay = document.getElementById('config-overlay');
        if (overlay) overlay.addEventListener('keydown', settingsSearchShortcutHandler);
        _settingsSearch.bound = true;
      }
      if (c.input.value) applySettingsSearch(c.input.value);
    }

    // #config/governor/<tab>/<query> opens Settings with the search box
    // already holding <query> — the link triage comments carry (#13575).
    function settingsSearchPrefill(query) {
      const c = settingsSearchControls();
      if (!c.input) return;
      c.input.value = query;
      c.input.dispatchEvent(new Event('input'));
    }

    function settingsSearchShortcutHandler(e) {
      if (!_configModalOpen || _configState.type !== 'governor') return;
      const c = settingsSearchControls();
      if (!c.input) return;
      const tag = ((e.target && e.target.tagName) || '').toLowerCase();
      const typing = tag === 'input' || tag === 'textarea' || tag === 'select' || (e.target && e.target.isContentEditable);
      if ((e.key === '/' && !typing) || ((e.ctrlKey || e.metaKey) && e.key && e.key.toLowerCase() === 'k')) {
        e.preventDefault();
        c.input.focus();
        c.input.select();
      } else if (e.key === 'Escape' && _settingsSearch.active) {
        clearSettingsSearch();
      }
    }

    function clearSettingsSearch() {
      const c = settingsSearchControls();
      if (c.input) c.input.value = '';
      if (c.clear) c.clear.hidden = true;
      if (c.count) c.count.textContent = 'Search settings';
      const previous = _settingsSearch.previousTab;
      _settingsSearch.active = false;
      _settingsSearch.query = '';
      if (_configState.type === 'governor') switchConfigTab(previous || _configState.tab || GOVERNOR_CONFIG_TABS[0]);
    }

    function settingsSearchText(node) {
      return (node && (node.textContent || node.innerText) || '').replace(/\s+/g, ' ').trim();
    }

    function settingsSearchSectionName(node, fallback) {
      let cur = node;
      while (cur) {
        if (cur.matches && (cur.matches('.config-section') || cur.matches('details'))) {
          const h = cur.querySelector('h3, summary, .var-heading, .overview-settings-card-title');
          if (h) return settingsSearchText(h);
        }
        cur = cur.parentElement;
      }
      return fallback || '';
    }

    function settingsSearchKeyFromField(field) {
      const ctrl = field.querySelector('[data-arg0][data-arg1], [data-section][data-key], [data-setting], [data-setting-key]');
      if (!ctrl) return '';
      const explicit = ctrl.getAttribute('data-setting-key') || ctrl.getAttribute('data-setting');
      if (explicit) return explicit;
      const section = ctrl.getAttribute('data-section') || ctrl.getAttribute('data-arg0') || '';
      const key = ctrl.getAttribute('data-key') || ctrl.getAttribute('data-arg1') || '';
      if (!section || !key || /^(s|v|N|c|t|w)$/.test(section)) return '';
      const snake = key.replace(/([a-z0-9])([A-Z])/g, '$1_$2').replace(/-/g, '_').toLowerCase();
      return section.replace(/-/g, '_') + '.' + snake;
    }

    function settingsSearchLabel(field) {
      const label = field.querySelector('label, .config-toggle-label, .var-heading, th, td:first-child, summary');
      if (label) {
        let txt = settingsSearchText(label);
        label.querySelectorAll('.settings-yaml-hint, .config-info').forEach(extra => { txt = txt.replace(settingsSearchText(extra), ''); });
        return txt.replace(/\s+/g, ' ').replace(/\?\s*$/, '').trim();
      }
      const txt = settingsSearchText(field);
      return txt.length > 100 ? txt.slice(0, 100) + '…' : txt;
    }

    // Every settings row carries a stable anchor, data-setting-key="<yaml.key>",
    // so a search hit maps to exactly one rendered control. Rows that do not
    // declare one get it from the YAML key yamlHint() prints in their label,
    // else from the control they wrap (data-section/data-key or
    // data-arg0/data-arg1). The annotation runs on every rendered tab and on the detached
    // render the index is built from, so both see the same keys.
    const SETTINGS_ROW_SELECTOR = '.config-field, .config-toggle, .config-list-item, .var-section, details, tr';
    function settingsRowKey(row) {
      if (!row || !row.getAttribute) return '';
      const own = row.getAttribute('data-setting-key');
      if (own) return own;
      // The printed YAML key is authoritative; a control's data-section/data-key
      // pair is a config-dialog section name that may omit the top-level
      // prefix (replan.enabled vs governor.replan.enabled).
      const hint = row.querySelector('.settings-yaml-hint code');
      if (hint) return settingsSearchText(hint);
      return settingsSearchKeyFromField(row);
    }

    function settingsAnnotateRows(root) {
      if (!root || !root.querySelectorAll) return;
      root.querySelectorAll(SETTINGS_ROW_SELECTOR).forEach(row => {
        if (row.hasAttribute('data-setting-key')) return;
        const key = settingsRowKey(row);
        if (key) row.setAttribute('data-setting-key', key);
      });
    }

    // Which of `keys` a search hit for `key` should land on: the exact key;
    // else, for an entry naming a group (governor.watchdog), keys under it;
    // else keys sharing its parent group, so a field that is only rendered
    // conditionally (governor.rotation.threshold_pct) lands on the group's
    // toggle rather than nowhere.
    function settingsMatchKeys(keys, key) {
      if (!key) return [];
      if (keys.indexOf(key) >= 0) return [key];
      const under = prefix => keys.filter(k => k.indexOf(prefix + '.') === 0 || k.indexOf(prefix + '[') === 0);
      const group = under(key);
      if (group.length) return group;
      const parent = key.replace(/\[\]$/, '').replace(/\.[^.]+$/, '');
      // Only fall back to a real group (governor.rotation), never to a whole
      // top-level section (governor), which would match half the dialog.
      return parent !== key && parent.indexOf('.') > 0 ? under(parent) : [];
    }

    // Rows whose anchor settingsMatchKeys picks for `key`. Outer rows (a
    // <details> or <tr> wrapping the field) are dropped in favour of the
    // innermost match so the flash lands on the control, not its container.
    function settingsRowsForKey(root, key) {
      if (!root || !key) return [];
      const rows = Array.from(root.querySelectorAll(SETTINGS_ROW_SELECTOR)).filter(r => r.getAttribute('data-setting-key'));
      const want = settingsMatchKeys(rows.map(r => r.getAttribute('data-setting-key')), key);
      const picked = rows.filter(r => want.indexOf(r.getAttribute('data-setting-key')) >= 0);
      return picked.filter(r => !picked.some(other => other !== r && r.contains(other)));
    }

    function settingsFindRow(root, key, label) {
      const byKey = settingsRowsForKey(root, key);
      if (byKey.length) return byKey[0];
      const want = (label || '').trim().toLowerCase();
      if (!want || !root) return null;
      const rows = Array.from(root.querySelectorAll(SETTINGS_ROW_SELECTOR));
      return rows.find(r => settingsSearchLabel(r).toLowerCase() === want) || null;
    }

    // The tabs each key is rendered in, computed from the live renderers so a
    // hand-written index entry whose `tab` has drifted cannot send the
    // operator to a tab without that control.
    function settingsSearchTabsForKey(key) {
      settingsSearchBuildIndex();
      return settingsSearchTabsForKeyIn(_settingsSearch.keyTabs || {}, key);
    }

    function settingsSearchTabsForKeyIn(keyTabs, key) {
      const out = [];
      settingsMatchKeys(Object.keys(keyTabs), key).forEach(k => {
        keyTabs[k].forEach(t => { if (out.indexOf(t) < 0) out.push(t); });
      });
      return out;
    }

    function settingsSearchResolveTab(tab, key) {
      const tabs = settingsSearchTabsForKey(key);
      if (!tabs.length) return tab;
      return tabs.indexOf(tab) >= 0 ? tab : tabs[0];
    }

    function settingsSearchBuildIndex() {
      if (_settingsSearch.index) return _settingsSearch.index;
      const out = [];
      const keyTabs = {};
      const previousTab = _configState.tab;
      GOVERNOR_CONFIG_TABS.forEach(tab => {
        let html = '';
        try { html = renderGovernorTab(tab) || ''; } catch (e) { html = ''; }
        const wrap = document.createElement('div');
        wrap.innerHTML = html;
        settingsAnnotateRows(wrap);
        const fields = wrap.querySelectorAll(SETTINGS_ROW_SELECTOR);
        fields.forEach(field => {
          const key = settingsRowKey(field);
          if (key) {
            if (!keyTabs[key]) keyTabs[key] = [];
            if (keyTabs[key].indexOf(tab) < 0) keyTabs[key].push(tab);
          }
          const label = settingsSearchLabel(field);
          const text = settingsSearchText(field);
          if (!label || !text) return;
          const section = settingsSearchSectionName(field, tab);
          out.push({ tab, section, label, key, text });
        });
      });
      _configState.tab = previousTab;
      _settingsSearch.keyTabs = keyTabs;
      // Static entries are hints that describe a group of controls; their tab
      // is re-derived from where the key actually rendered.
      const statics = SETTINGS_SEARCH_STATIC_HITS.map(item => {
        const tabs = settingsSearchTabsForKeyIn(keyTabs, item.key);
        const tab = !tabs.length || tabs.indexOf(item.tab) >= 0 ? item.tab : tabs[0];
        return Object.assign({}, item, { tab });
      });
      // One entry per tab+key: a static hint and the rendered row it points
      // at (or a wrapper and the field inside it) merge, keeping both texts
      // searchable, so the result list shows each control once.
      const seen = {};
      _settingsSearch.index = statics.concat(out).filter(item => {
        const id = (item.key ? [item.tab, item.key] : [item.tab, item.section, item.label]).join('\n').toLowerCase();
        if (seen[id]) {
          if (seen[id].text.indexOf(item.text) < 0) seen[id].text += ' ' + item.text;
          return false;
        }
        seen[id] = item;
        return true;
      });
      return _settingsSearch.index;
    }

    function settingsSearchMatches(item, query) {
      const hay = [item.tab, item.section, item.label, item.key, item.text].join(' ').toLowerCase();
      return hay.indexOf(query.toLowerCase()) >= 0;
    }

    function settingsSearchHighlight(text, query) {
      const safe = esc(text || '');
      if (!query) return safe;
      const idx = String(text || '').toLowerCase().indexOf(query.toLowerCase());
      if (idx < 0) return safe;
      return esc(text.slice(0, idx)) + '<span class="settings-search-mark">' + esc(text.slice(idx, idx + query.length)) + '</span>' + esc(text.slice(idx + query.length));
    }

    function settingsSearchSnippet(item, query) {
      const text = [item.section, item.text].filter(Boolean).join(' · ').replace(/\s+/g, ' ').trim();
      const low = text.toLowerCase();
      const q = query.toLowerCase();
      let start = low.indexOf(q);
      if (start < 0) start = 0;
      start = Math.max(0, start - 55);
      let snip = text.slice(start, start + 170);
      if (start > 0) snip = '…' + snip;
      if (start + 170 < text.length) snip += '…';
      return settingsSearchHighlight(snip, query);
    }

    function renderSettingsSearchResults(query) {
      const body = document.getElementById('config-body');
      const c = settingsSearchControls();
      if (!body) return;
      const q = (query || '').trim();
      const matches = q ? settingsSearchBuildIndex().filter(item => settingsSearchMatches(item, q)) : [];
      if (c.count) c.count.textContent = q ? (matches.length + ' setting' + (matches.length === 1 ? '' : 's') + ' match') : 'Search settings';
      if (c.clear) c.clear.hidden = !q;
      if (!q) return;
      if (!matches.length) {
        body.innerHTML = '<div class="settings-search-empty">No settings match <strong>' + esc(q) + '</strong></div>';
        return;
      }
      const byTab = {};
      matches.forEach(item => { if (!byTab[item.tab]) byTab[item.tab] = []; byTab[item.tab].push(item); });
      _settingsSearch.matches = Object.keys(byTab).reduce((acc, tab) => acc.concat(byTab[tab]), []);
      body.innerHTML = '<div class="settings-search-results">' + Object.keys(byTab).map(tab => {
        const hits = byTab[tab];
        return '<section class="settings-search-group" data-settings-search-tab="' + esc(tab) + '">' +
          '<div class="settings-search-group-title"><span>' + settingsSearchHighlight(tab, q) + ' <span class="text-muted">(' + hits.length + ')</span></span><button type="button" class="hv-btn btn-secondary btn-sm" data-action="settingsSearchOpenTab" data-arg0="' + esc(tab) + '">Open tab</button></div>' +
          hits.map(item => '<div class="settings-search-hit" role="button" tabindex="0" data-action="settingsSearchOpenHit" data-keydown-action="settingsSearchOpenHit" data-keys="Enter, " data-prevent="1" data-arg0="' + esc(tab) + '" data-arg1="' + esc(item.key || '') + '" data-arg2="' + esc(item.label || '') + '">' +
            '<div class="settings-search-hit-label">' + settingsSearchHighlight(item.label, q) + '</div>' +
            '<div class="settings-search-hit-meta">' + settingsSearchSnippet(item, q) + '</div>' +
            (item.key ? '<code class="settings-search-key">' + settingsSearchHighlight(item.key, q) + '</code>' : '') +
          '</div>').join('') + '</section>';
      }).join('') + '</div>';
    }

    function applySettingsSearch(value) {
      const q = (value || '').trim();
      if (!_configModalOpen || _configState.type !== 'governor') return;
      if (!_settingsSearch.active && q) _settingsSearch.previousTab = _configState.tab;
      _settingsSearch.active = !!q;
      _settingsSearch.query = q;
      if (!q) { clearSettingsSearch(); return; }
      renderSettingsSearchResults(q);
    }

    // "Open tab" on a result group lands on that group's first hit, so the
    // operator is taken to the control rather than the top of a long tab.
    function settingsSearchOpenTab(tab) {
      const first = (_settingsSearch.matches || []).find(item => item.tab === tab) || null;
      settingsSearchOpenHit(tab, first ? first.key : '', first ? first.label : '');
    }

    function settingsSearchOpenFirst() {
      const first = (_settingsSearch.matches || [])[0];
      if (first) settingsSearchOpenHit(first.tab, first.key, first.label);
    }

    function settingsSearchOpenHit(tab, key, label) {
      const c = settingsSearchControls();
      const target = settingsSearchResolveTab(tab, key);
      if (c.input) c.input.value = '';
      if (c.clear) c.clear.hidden = true;
      if (c.count) c.count.textContent = 'Search settings';
      _settingsSearch.active = false;
      _settingsSearch.query = '';
      switchConfigTab(target);
      // The tab body renders synchronously but layout has not run yet, so wait
      // a frame before scrolling; panels that fill in after their own fetch
      // (Health, Knowledge connectors) get a few short retries.
      let attempts = SETTINGS_REVEAL_ATTEMPTS;
      const reveal = () => {
        const body = document.getElementById('config-body');
        if (!body || _configState.tab !== target || _settingsSearch.active) return;
        settingsAnnotateRows(body);
        if (settingsRevealRow(settingsFindRow(body, key, label))) return;
        if (--attempts > 0) setTimeout(reveal, SETTINGS_REVEAL_RETRY_MS);
      };
      if (typeof requestAnimationFrame === 'function') requestAnimationFrame(reveal); else reveal();
    }

    const SETTINGS_REVEAL_ATTEMPTS = 12;
    const SETTINGS_REVEAL_RETRY_MS = 125;

    const SETTINGS_ROW_FLASH_MS = 1600;
    // Expands any disclosure hiding the row, scrolls it to the middle of the
    // dialog body (the scroll container — the overlay is fixed, so the page
    // behind does not move), flashes it, and moves focus to its control.
    const SETTINGS_REVEAL_SETTLE_MS = 400;

    // Instant (not smooth) scroll: the dialog body can be ~10k px tall and a
    // smooth scroll over that distance lands short when content above the
    // row changes height mid-way. The flash animation is the motion cue.
    function settingsScrollRowIntoView(row) {
      const body = document.getElementById('config-body');
      if (typeof row.scrollIntoView === 'function') {
        row.scrollIntoView({ block: 'center', inline: 'nearest', behavior: 'auto' });
      } else if (body) {
        body.scrollTop = Math.max(0, row.offsetTop - body.clientHeight / 2);
      }
    }

    function settingsRowInView(row) {
      const body = document.getElementById('config-body');
      if (!body || typeof row.getBoundingClientRect !== 'function') return true;
      const r = row.getBoundingClientRect(), b = body.getBoundingClientRect();
      return r.top >= b.top && r.bottom <= b.bottom;
    }

    function settingsRevealRow(row) {
      if (!row) return false;
      for (let cur = row.parentElement; cur; cur = cur.parentElement) {
        if (cur.tagName === 'DETAILS' && !cur.open) cur.open = true;
        if (cur.classList && cur.classList.contains('collapsed')) cur.classList.remove('collapsed');
      }
      settingsScrollRowIntoView(row);
      // Tabs like Features keep filling in from their own fetches for a
      // moment after rendering, which shifts the row; re-centre once settled.
      setTimeout(() => { if (row.isConnected !== false && !settingsRowInView(row)) settingsScrollRowIntoView(row); }, SETTINGS_REVEAL_SETTLE_MS);
      row.classList.remove('cfg-row-flash');
      void row.offsetWidth; // restart the animation when the same row is hit twice
      row.classList.add('cfg-row-flash');
      if (row._cfgFlashTimer) clearTimeout(row._cfgFlashTimer);
      row._cfgFlashTimer = setTimeout(() => { row.classList.remove('cfg-row-flash'); row._cfgFlashTimer = null; }, SETTINGS_ROW_FLASH_MS);
      let ctrl = row.querySelector('input:not([type="hidden"]), select, textarea') || row.querySelector('button, [role="switch"][tabindex], .config-toggle-switch[tabindex]');
      if (!ctrl) { row.setAttribute('tabindex', '-1'); ctrl = row; }
      if (typeof ctrl.focus === 'function') { try { ctrl.focus({ preventScroll: true }); } catch (e) { ctrl.focus(); } }
      return true;
    }

