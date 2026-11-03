    // URL views (hivecommons/hive#15924): `am.*` query parameters describe a
    // filter and group so a display link reproduces the view in a fresh
    // browser profile. waitingOnYou is per-user and stays on the one-shot
    // `?filter=waiting-on-you`; it is never an `am.*` key.
    // tracker url view helpers begin
    const STAGE_TRACKER_URL_PREFIX = 'am.';
    const STAGE_TRACKER_URL_KEYS = ['kind', 'repo', 'q', 'age', 'author', 'acted', 'sort', 'dir', 'labels', 'needsHuman', 'onHold', 'attention'];
    const STAGE_TRACKER_URL_BOOLS = ['needsHuman', 'onHold', 'attention'];
    const STAGE_TRACKER_URL_CREDENTIAL_PARAMS = ['token', 'access_token'];
    // Pure: the { filter, group } an `am.*` URL describes, or null when it
    // carries no `am.*` parameter. Values go through
    // stageTrackerNormalizeFilter, so an unknown one falls back to its default.
    function stageTrackerFilterFromUrl(href) {
      let url;
      try { url = new URL(href); } catch { return null; }
      const p = url.searchParams;
      if (!Array.from(p.keys()).some(k => k.indexOf(STAGE_TRACKER_URL_PREFIX) === 0)) return null;
      const raw = {};
      for (const k of STAGE_TRACKER_URL_KEYS) {
        const v = p.get(STAGE_TRACKER_URL_PREFIX + k);
        if (v === null) continue;
        if (k === 'labels') raw.labels = v.split(',').map(l => l.trim()).filter(Boolean);
        else if (STAGE_TRACKER_URL_BOOLS.indexOf(k) >= 0) raw[k] = v === '1' || v === 'true';
        else raw[k] = v;
      }
      const g = p.get(STAGE_TRACKER_URL_PREFIX + 'group');
      return { filter: stageTrackerNormalizeFilter(raw), group: STAGE_TRACKER_GROUPS.indexOf(g) >= 0 ? g : STAGE_TRACKER_GROUP_DEFAULT };
    }
    // Pure: href with `am.*` set to describe filter + group. Defaults are
    // omitted; other query params and the hash are kept; credentials never
    // ride along.
    function stageTrackerViewUrl(href, filter, group) {
      let url;
      try { url = new URL(href); } catch { return ''; }
      for (const k of Array.from(url.searchParams.keys())) {
        if (k.indexOf(STAGE_TRACKER_URL_PREFIX) === 0 || STAGE_TRACKER_URL_CREDENTIAL_PARAMS.indexOf(k) >= 0) url.searchParams.delete(k);
      }
      url.username = '';
      url.password = '';
      const f = stageTrackerNormalizeFilter(filter);
      const d = stageTrackerDefaultFilter();
      for (const k of STAGE_TRACKER_URL_KEYS) {
        if (k === 'labels') { if (f.labels.length) url.searchParams.set(STAGE_TRACKER_URL_PREFIX + k, f.labels.join(',')); continue; }
        if (f[k] === d[k]) continue;
        url.searchParams.set(STAGE_TRACKER_URL_PREFIX + k, STAGE_TRACKER_URL_BOOLS.indexOf(k) >= 0 ? '1' : String(f[k]));
      }
      if (STAGE_TRACKER_GROUPS.indexOf(group) >= 0 && group !== STAGE_TRACKER_GROUP_DEFAULT) url.searchParams.set(STAGE_TRACKER_URL_PREFIX + 'group', group);
      return url.toString();
    }
    // tracker url view helpers end
    // Display playlists (hivecommons/hive#15932): a display-mode URL may
    // carry repeated `am.view` params, each a URL-encoded `am.*` query
    // string, plus `interval=Ns|Nm` (10 s–1 h, default 60 s). The playlist
    // lives entirely in the URL — there are no named presets anywhere. A
    // view is today an `am.*` tracker filter; another view kind would be one
    // more branch in stageTrackerPlaylistView.
    // tracker display playlist helpers begin
    const STAGE_TRACKER_PLAYLIST_PARAM = 'am.view';
    const STAGE_TRACKER_PLAYLIST_INTERVAL_PARAM = 'interval';
    const STAGE_TRACKER_PLAYLIST_INTERVAL_DEFAULT_MS = 60000;
    const STAGE_TRACKER_PLAYLIST_INTERVAL_MIN_MS = 10000;
    const STAGE_TRACKER_PLAYLIST_INTERVAL_MAX_MS = 3600000;
    // Pure: `interval` (`45s`, `5m`) in ms, clamped; anything else is the default.
    function stageTrackerPlaylistIntervalMs(raw) {
      const m = /^\s*(\d{1,7})\s*([sm])\s*$/i.exec(raw == null ? '' : String(raw));
      if (!m) return STAGE_TRACKER_PLAYLIST_INTERVAL_DEFAULT_MS;
      const ms = Number(m[1]) * (m[2].toLowerCase() === 'm' ? 60000 : 1000);
      return Math.min(STAGE_TRACKER_PLAYLIST_INTERVAL_MAX_MS, Math.max(STAGE_TRACKER_PLAYLIST_INTERVAL_MIN_MS, ms));
    }
    // Pure: one decoded `am.view` value as { filter, group }, or null when it
    // names no known `am.*` key (or nests another am.view) — such a view is
    // skipped.
    function stageTrackerPlaylistView(raw) {
      let p;
      try { p = new URLSearchParams(String(raw == null ? '' : raw).replace(/^\?/, '')); } catch { return null; }
      const keys = Array.from(p.keys());
      if (keys.indexOf(STAGE_TRACKER_PLAYLIST_PARAM) >= 0) return null;
      const known = STAGE_TRACKER_URL_KEYS.concat(['group']).map(k => STAGE_TRACKER_URL_PREFIX + k);
      if (!keys.some(k => known.indexOf(k) >= 0)) return null;
      return stageTrackerFilterFromUrl('https://hive.invalid/?' + p.toString());
    }
    // Pure: { views: [{ filter, group }…], intervalMs } for a URL. Fewer than
    // two views means the caller does not rotate.
    function stageTrackerParsePlaylist(href) {
      let url;
      try { url = new URL(href); } catch { return { views: [], intervalMs: STAGE_TRACKER_PLAYLIST_INTERVAL_DEFAULT_MS }; }
      const views = url.searchParams.getAll(STAGE_TRACKER_PLAYLIST_PARAM).map(stageTrackerPlaylistView).filter(Boolean);
      return { views, intervalMs: stageTrackerPlaylistIntervalMs(url.searchParams.get(STAGE_TRACKER_PLAYLIST_INTERVAL_PARAM)) };
    }
    // Pure: which of n views is showing. Derived from wall-clock time (pass
    // startMs 0), so a reload or an SSE reconnect lands on the same view.
    function stageTrackerPlaylistIndex(startMs, nowMs, intervalMs, n) {
      if (!(n >= 2) || !(intervalMs > 0)) return 0;
      const k = Math.floor((Number(nowMs) - Number(startMs)) / intervalMs);
      if (!Number.isFinite(k)) return 0;
      return ((k % n) + n) % n;
    }
    // Pure: ms left before the next view.
    function stageTrackerPlaylistRemainingMs(startMs, nowMs, intervalMs) {
      if (!(intervalMs > 0)) return 0;
      const e = (((Number(nowMs) - Number(startMs)) % intervalMs) + intervalMs) % intervalMs;
      return Number.isFinite(e) ? intervalMs - e : 0;
    }
    // Pure: the reduced-motion label.
    function stageTrackerPlaylistNextLabel(remainingMs) {
      return 'next in ' + Math.max(0, Math.ceil((Number(remainingMs) || 0) / 1000)) + 's';
    }
    // tracker display playlist helpers end
    // The tab's URL view, read once. While it is set the filter and group
    // are session-only: saves stay in this object and never reach
    // localStorage, so two screens on one profile cannot clobber each other.
    var _stageTrackerUrlView;
    function stageTrackerUrlView() {
      if (_stageTrackerUrlView === undefined) {
        _stageTrackerUrlView = null;
        try { _stageTrackerUrlView = stageTrackerFilterFromUrl(window.location.href); } catch {}
      }
      return _stageTrackerUrlView;
    }
    function stageTrackerLoadFilter() {
      const view = stageTrackerUrlView();
      if (view) _stageTrackerFilter = stageTrackerNormalizeFilter(view.filter);
      else try { _stageTrackerFilter = stageTrackerNormalizeFilter(JSON.parse(localStorage.getItem(STAGE_TRACKER_FILTER_KEY) || 'null')); } catch { _stageTrackerFilter = stageTrackerDefaultFilter(); }
      _stageTrackerFilter.page = 1;
    }
    function stageTrackerSaveFilter() {
      const view = stageTrackerUrlView();
      if (view) { view.filter = Object.assign({}, _stageTrackerFilter); return; }
      try { const f = Object.assign({}, _stageTrackerFilter); delete f.page; localStorage.setItem(STAGE_TRACKER_FILTER_KEY, JSON.stringify(f)); } catch {}
    }
    // Repo chip / group settings and the per-repo collapsed set (localStorage).
    function stageTrackerRepoChipSetting() {
      let v = '';
      try { v = localStorage.getItem(STAGE_TRACKER_REPO_CHIP_KEY) || ''; } catch { v = ''; }
      return STAGE_TRACKER_REPO_CHIP_MODES.some(m => m.id === v) ? v : STAGE_TRACKER_REPO_CHIP_DEFAULT;
    }
    function stageTrackerSetRepoChip(id) {
      const v = STAGE_TRACKER_REPO_CHIP_MODES.some(m => m.id === id) ? id : STAGE_TRACKER_REPO_CHIP_DEFAULT;
      try { localStorage.setItem(STAGE_TRACKER_REPO_CHIP_KEY, v); } catch {}
      return v;
    }
    function stageTrackerGroupSetting() {
      const view = stageTrackerUrlView();
      if (view) return view.group;
      let v = '';
      try { v = localStorage.getItem(STAGE_TRACKER_GROUP_KEY) || ''; } catch { v = ''; }
      return STAGE_TRACKER_GROUPS.indexOf(v) >= 0 ? v : STAGE_TRACKER_GROUP_DEFAULT;
    }
    function stageTrackerSetGroup(id) {
      const v = STAGE_TRACKER_GROUPS.indexOf(id) >= 0 ? id : STAGE_TRACKER_GROUP_DEFAULT;
      const view = stageTrackerUrlView();
      if (view) { view.group = v; return v; }
      try { localStorage.setItem(STAGE_TRACKER_GROUP_KEY, v); } catch {}
      return v;
    }
    function stageTrackerLoadCollapsed() {
      try {
        const o = JSON.parse(localStorage.getItem(STAGE_TRACKER_GROUP_COLLAPSED_KEY) || '{}');
        if (!o || typeof o !== 'object' || Array.isArray(o)) return {};
        const out = {};
        for (const k of Object.keys(o)) if (o[k]) out[k] = true;
        return out;
      } catch { return {}; }
    }
    function stageTrackerSaveCollapsed(c) {
      try { localStorage.setItem(STAGE_TRACKER_GROUP_COLLAPSED_KEY, JSON.stringify(c || {})); } catch {}
    }
    // Pure: the collapsed set with one repo toggled.
    function stageTrackerToggleCollapsed(collapsed, repo) {
      const out = Object.assign({}, collapsed || {});
      if (out[repo]) delete out[repo]; else out[repo] = true;
      return out;
    }
    function stageTrackerLoadGrouping() {
      _stageTrackerGroup = stageTrackerGroupSetting();
      _stageTrackerCollapsed = stageTrackerLoadCollapsed();
    }
    function stageTrackerPrimaryRepo() { return (typeof window !== 'undefined' && window._primaryRepo) || ''; }
    function hamGroup(value) {
      _stageTrackerGroup = stageTrackerSetGroup(value);
      _stageTrackerFilter.page = 1;
      renderStageTracker();
      stageTrackerRenderSettings();
    }
    function hamGroupToggle(repo) {
      _stageTrackerCollapsed = stageTrackerToggleCollapsed(_stageTrackerCollapsed, repo);
      stageTrackerSaveCollapsed(_stageTrackerCollapsed);
      renderStageTracker();
    }
    // A row's repo chip: filter to that repo, or back to all when it already is.
    function hamRepoChip(repo) { hamFilter('repo', _stageTrackerFilter.repo === repo ? '' : repo); }
    function stageTrackerRepoChipChange(value) {
      stageTrackerSetRepoChip(value);
      renderStageTracker();
    }

    function hamFilter(key, value) {
      const f = _stageTrackerFilter;
      switch (key) {
        case 'kind': f.kind = value || 'all'; break;
        case 'repo': f.repo = value || ''; break;
        case 'q': f.q = value || ''; break;
        case 'age': f.age = value || 'any'; break;
        case 'author': f.author = value || 'any'; break;
        case 'attention': f.attention = value === 'toggle' ? !f.attention : !!value; break;
        case 'acted': f.acted = value || 'any'; break;
        case 'sort': f.sort = value || 'furthest'; break;
        case 'dir': f.dir = value === 'toggle' ? (f.dir === 'asc' ? 'desc' : 'asc') : value; break;
        case 'size': f.size = Number(value) || STAGE_TRACKER_DEFAULT_PAGE_SIZE; break;
        case 'label': { const l = String(value || '').toLowerCase(); const i = f.labels.indexOf(l); if (i >= 0) f.labels.splice(i, 1); else if (l) f.labels.push(l); break; }
        case 'needsHuman': f.needsHuman = value === 'toggle' ? !f.needsHuman : !!value; break;
        case 'onHold': f.onHold = value === 'toggle' ? !f.onHold : !!value; break;
        case 'waitingOnYou': f.waitingOnYou = value === 'toggle' ? !f.waitingOnYou : !!value; break;
        case 'labelsClear': f.labels = []; f.needsHuman = false; f.onHold = false; break;
        case 'page': f.page = value === 'prev' ? f.page - 1 : value === 'next' ? f.page + 1 : Number(value) || 1; break;
        case 'reset': { const keep = { sort: f.sort, dir: f.dir, size: f.size }; _stageTrackerFilter = Object.assign(stageTrackerDefaultFilter(), keep); break; }
      }
      if (key !== 'page') _stageTrackerFilter.page = 1;
      _stageTrackerFilter = stageTrackerNormalizeFilter(_stageTrackerFilter);
      stageTrackerSaveFilter();
      renderStageTracker();
    }

    // Push the filter state into the toolbar controls (they may have been
    // restored from storage or reset).
    function stageTrackerSyncToolbar(f) {
      document.querySelectorAll('#activity-monitor-toolbar .ham-chip[data-arg0="kind"]').forEach(b => b.setAttribute('aria-pressed', b.dataset.arg1 === f.kind ? 'true' : 'false'));
      const setVal = (id, v) => { const el = document.getElementById(id); if (el && el.value !== String(v) && document.activeElement !== el) el.value = String(v); };
      setVal('activity-monitor-q', f.q);
      setVal('activity-monitor-age', f.age);
      setVal('activity-monitor-author', f.author);
      setVal('activity-monitor-acted', f.acted);
      setVal('activity-monitor-sort', f.sort);
      setVal('activity-monitor-group', _stageTrackerGroup);
      setVal('activity-monitor-size', f.size);
      const att = document.getElementById('activity-monitor-attention');
      if (att) att.setAttribute('aria-pressed', f.attention ? 'true' : 'false');
      const wait = document.getElementById('activity-monitor-waiting');
      if (wait) wait.setAttribute('aria-pressed', f.waitingOnYou ? 'true' : 'false');
      const dir = document.getElementById('activity-monitor-dir');
      if (dir) { dir.textContent = f.dir === 'asc' ? '↑' : '↓'; dir.setAttribute('title', (f.dir === 'asc' ? 'Ascending — click for descending' : 'Descending — click for ascending') + STAGE_TRACKER_DIR_TIER_NOTE); }
      const reset = document.getElementById('activity-monitor-reset');
      if (reset) reset.hidden = !stageTrackerFilterActive(f);
    }

    function stageTrackerLabelChipHtml(c) {
      return `<button type="button" class="ham-chip ham-label-chip" data-action="hamFilter" data-arg0="label" data-arg1="${esc(c.label)}" aria-pressed="${c.on ? 'true' : 'false'}" title="${esc((c.on ? 'Remove label filter ' : 'Filter by label ') + c.label)}">${esc(c.label)}<span class="ham-chip-n">${c.count}</span></button>`;
    }
    function stageTrackerNeedsHumanChipHtml(nh) {
      if (!nh) return '';
      const tip = (nh.on ? 'Remove the Needs human filter. ' : '') + STAGE_TRACKER_NEEDS_HUMAN_TIP;
      return `<button type="button" class="ham-chip ham-needs-human-chip" data-action="hamFilter" data-arg0="needsHuman" data-arg1="toggle" aria-pressed="${nh.on ? 'true' : 'false'}" title="${esc(tip)}">🙋 Needs human<span class="ham-chip-n">${nh.count}</span></button>`;
    }
    function stageTrackerOnHoldChipHtml(oh) {
      if (!oh) return '';
      const tip = (oh.on ? 'Remove the On hold filter. ' : '') + STAGE_TRACKER_ON_HOLD_TIP;
      return `<button type="button" class="ham-chip ham-on-hold-chip" data-action="hamFilter" data-arg0="onHold" data-arg1="toggle" aria-pressed="${oh.on ? 'true' : 'false'}" title="${esc(tip)}">⏸ On hold<span class="ham-chip-n">${oh.count}</span></button>`;
    }
    function stageTrackerLabelsHtml(facets) {
      if (!facets.chips.length && !facets.needsHuman && !facets.onHold) return '';
      return stageTrackerNeedsHumanChipHtml(facets.needsHuman)
        + stageTrackerOnHoldChipHtml(facets.onHold)
        + facets.chips.map(stageTrackerLabelChipHtml).join('')
        + (facets.hidden ? `<button type="button" class="ham-chip ham-labels-more" data-action="openStageTrackerLabelDialog" aria-haspopup="dialog" title="Show all ${facets.all.length} labels with counts; pick any to filter">+${facets.hidden} more</button>` : '');
    }

    // "All labels" dialog: every label with its count, same order as the
    // row, a type-to-filter box, the same toggle the chips use and Clear.
    // In-app modal (config-overlay recipe), never a native prompt.
    function stageTrackerLabelDialogHtml(data) {
      return `<div class="ham-label-dialog-tools"><input type="search" class="ham-input" id="activity-monitor-labels-q" placeholder="Type to filter labels…" autocomplete="off" spellcheck="false" aria-label="Filter the label list" value="${esc(data.query)}" data-input-action="stageTrackerLabelDialogQuery" data-arg-types="w"><span class="ham-count">${esc(stageTrackerLabelDialogCountText(data))}</span><button type="button" class="hv-btn btn-secondary btn-sm" data-action="hamFilter" data-arg0="labelsClear"${data.selected ? '' : ' disabled'} title="Clear every label filter">Clear</button></div>`
        + (data.needsHuman || data.onHold ? `<div class="ham-labels ham-label-dialog-needs-human">${stageTrackerNeedsHumanChipHtml(data.needsHuman)}${stageTrackerOnHoldChipHtml(data.onHold)}</div>` : '')
        + `<div class="ham-labels ham-label-dialog-list" role="group" aria-label="All labels">${stageTrackerLabelDialogListHtml(data)}</div>`;
    }
    function stageTrackerLabelDialogListHtml(data) {
      return data.rows.length ? data.rows.map(stageTrackerLabelChipHtml).join('') : `<div class="ham-empty">No label matches “${esc(data.query)}”</div>`;
    }
    function stageTrackerLabelDialogCountText(data) {
      return (data.rows.length === data.total ? data.total + ' labels' : data.rows.length + ' of ' + data.total + ' labels') + ' · ' + data.selected + ' selected';
    }
    // Redraw the open dialog. While the filter box has focus only the list,
    // the count, the Clear button and the Needs human chip are patched so a
    // poll-driven render never steals the caret.
    function stageTrackerRenderLabelDialog() {
      const body = document.getElementById('activity-monitor-labels-body');
      if (!body || !_stageTracker || !Array.isArray(_stageTracker.items)) return;
      const facets = stageTrackerLabelFacets(_stageTracker.items, _stageTrackerFilter);
      const data = stageTrackerLabelDialogData(facets, _stageTrackerLabelQuery);
      const q = document.getElementById('activity-monitor-labels-q');
      if (!q || document.activeElement !== q) { setIfChanged(body, stageTrackerLabelDialogHtml(data)); return; }
      const list = body.querySelector('.ham-label-dialog-list');
      if (list) setIfChanged(list, stageTrackerLabelDialogListHtml(data));
      const count = body.querySelector('.ham-label-dialog-tools .ham-count');
      if (count && count.textContent !== stageTrackerLabelDialogCountText(data)) count.textContent = stageTrackerLabelDialogCountText(data);
      const clear = body.querySelector('.ham-label-dialog-tools .hv-btn');
      if (clear) clear.disabled = !data.selected;
      const nh = body.querySelector('.ham-label-dialog-needs-human');
      if (nh) setIfChanged(nh, stageTrackerNeedsHumanChipHtml(data.needsHuman) + stageTrackerOnHoldChipHtml(data.onHold));
    }
    var _stageTrackerLabelDialogReturnFocus = null;
    function openStageTrackerLabelDialog() {
      if (document.getElementById('activity-monitor-labels-overlay')) return;
      _stageTrackerLabelDialogReturnFocus = document.activeElement;
      const overlay = document.createElement('div');
      overlay.id = 'activity-monitor-labels-overlay';
      overlay.className = 'config-overlay';
      hiveRememberModalOpen(overlay);
      overlay.innerHTML = `<div class="config-modal overview-settings-modal ham-label-dialog" role="dialog" aria-modal="true" aria-labelledby="activity-monitor-labels-title">
        <div class="config-header">
          <h2 class="config-title" id="activity-monitor-labels-title">All labels</h2>
          <button type="button" class="config-close" data-action="closeStageTrackerLabelDialog" aria-label="Close the label list">&times;</button>
        </div>
        <div class="config-body overview-settings-body modal-scroll" id="activity-monitor-labels-body"></div>
      </div>`;
      overlay.addEventListener('click', e => { if (e.target === overlay) closeStageTrackerLabelDialog(); });
      document.body.appendChild(overlay);
      stageTrackerRenderLabelDialog();
      const q = document.getElementById('activity-monitor-labels-q');
      if (q) setTimeout(() => q.focus(), 0);
    }
    function closeStageTrackerLabelDialog() {
      const overlay = document.getElementById('activity-monitor-labels-overlay');
      if (overlay) overlay.remove();
      _stageTrackerLabelQuery = '';
      const ret = _stageTrackerLabelDialogReturnFocus;
      _stageTrackerLabelDialogReturnFocus = null;
      if (ret && ret.focus && document.contains(ret)) setTimeout(() => ret.focus({ preventScroll: true }), 0);
    }
    function stageTrackerLabelDialogQuery(value) {
      _stageTrackerLabelQuery = String(value || '');
      stageTrackerRenderLabelDialog();
    }

