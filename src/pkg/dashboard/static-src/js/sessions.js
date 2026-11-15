    // ── Sessions section ──
    // Named, grouped, shared CLI-agent terminal sessions. When the spoke has
    // `GET /api/sessions` -> {enabled:true} this section replaces the Inception
    // wizard UI (the legacy section stays untouched when disabled). The server
    // is the source of truth; localStorage only remembers the selected tab and
    // collapsed groups as a convenience. Each session gets one lazily created
    // ttyd iframe that is kept alive (just hidden) while another tab is shown.
    const SESSIONS_API = '/api/sessions';
    const SESSIONS_POLL_MS = 5000;
    const SESSIONS_LOG_POLL_MS = 3000;
    const SESSIONS_LOG_LINES = 500;
    const SESSIONS_ASSERTION_RENEW_MS = 600000;
    const SESSIONS_TMUX_PREFIX = 'hive-sess-';
    const SESSIONS_CAMPAIGNS_TAB = 'campaigns';
    const SESSIONS_UNGROUPED = '';
    const SESSIONS_UNGROUPED_LABEL = 'Ungrouped';
    const SESSIONS_SELECTED_KEY = 'hive.sessions.selected';
    const SESSIONS_COLLAPSED_KEY = 'hive.sessions.collapsedGroups';
    const SESSIONS_WRITE_ROLES = Object.freeze(['owner', 'read-write']);
    const SESSIONS_STATUS_RUNNING = 'running';
    const SESSIONS_STATUS_ENDED = 'ended';
    // A session is `preparing` from the create (202) until the background
    // start (workspace clone on the first session, spek init, tmux) finishes;
    // then `running`, or `failed` with the error on the record.
    const SESSIONS_STATUS_PREPARING = 'preparing';
    const SESSIONS_STATUS_FAILED = 'failed';
    // While a session is preparing the list is polled faster than the regular
    // SESSIONS_POLL_MS so the tab flips to a terminal soon after it starts.
    const SESSIONS_PREPARING_POLL_MS = 2000;
    const SESSIONS_PREPARING_HINT = 'this can take a couple of minutes on the first session of a hive, while the project repository is cloned.';
    const SESSIONS_TYPING_NOTE = 'Number of browsers attached to this terminal. One person at a time holds the keyboard; everyone else watches read-only. Typing is attributed to the keyboard holder, Send to whoever queued it.';
    const SESSIONS_HEADER_CONTENT_TYPE = 'Content-Type';
    // Keyboard lease (POST /api/sessions/{id}/keyboard): the one writable
    // terminal. The creator and owners can always take it; another writer only
    // once the holder released it or has been idle for the server's grace.
    const SESSIONS_KEYBOARD_TAKE = 'take';
    const SESSIONS_KEYBOARD_RELEASE = 'release';

    let _sessionsData = { enabled: false, sessions: [], vendors: [], limits: {}, workspace: {} };
    let _sessionsRole = '';
    let _sessionsSelected = '';
    let _sessionsCollapsed = {};
    let _sessionsLoaded = false;
    let _sessionsNewOpen = false;
    let _sessionsLogTimerOn = false;
    let _sessionsAssertionTimerOn = false;
    let _sessionsPrepPollOn = false;
    const _sessionsFrames = {};
    const _sessionsFrameFailed = {};
    // Extension slot for the input bar: other features (for example future
    // "Seed from..." providers) register {id, label, title, run(ctx)} here and
    // get an entry in the bar's "More" menu. Empty registry renders no menu.
    const SESSIONS_INPUT_ACTIONS = [];

    // ── Pure helpers (unit-tested) ──
    function sessionsRoleCanWrite(role) {
      return SESSIONS_WRITE_ROLES.indexOf(String(role || '')) >= 0;
    }
    function sessionsGroupLabel(group) {
      return group ? String(group) : SESSIONS_UNGROUPED_LABEL;
    }
    function sessionsGroupSessions(sessions) {
      const byGroup = {};
      (sessions || []).forEach(function(s) {
        if (!s || !s.id) return;
        const g = s.group || SESSIONS_UNGROUPED;
        (byGroup[g] = byGroup[g] || []).push(s);
      });
      const names = Object.keys(byGroup).sort(function(a, b) {
        if (a === SESSIONS_UNGROUPED) return 1;
        if (b === SESSIONS_UNGROUPED) return -1;
        return a.localeCompare(b);
      });
      return names.map(function(g) { return { group: g, label: sessionsGroupLabel(g), sessions: byGroup[g] }; });
    }
    function sessionsExistingGroups(sessions) {
      return sessionsGroupSessions(sessions).map(function(g) { return g.group; }).filter(Boolean);
    }
    function sessionsPickSelected(sessions, wanted) {
      if (wanted === SESSIONS_CAMPAIGNS_TAB) return wanted;
      const list = sessions || [];
      if (wanted && list.some(function(s) { return s && s.id === wanted; })) return wanted;
      return list.length && list[0] ? list[0].id : SESSIONS_CAMPAIGNS_TAB;
    }
    // Flat tab order as rendered (group order, then session order), used for arrow keys.
    function sessionsTabOrder(sessions, collapsed, selected) {
      const out = [];
      sessionsGroupSessions(sessions).forEach(function(g) {
        g.sessions.forEach(function(s) {
          if (collapsed && collapsed[g.group] && s.id !== selected) return;
          out.push(s.id);
        });
      });
      out.push(SESSIONS_CAMPAIGNS_TAB);
      return out;
    }
    function sessionsNextTab(order, current, key) {
      if (!order || !order.length) return '';
      const i = Math.max(0, order.indexOf(current));
      if (key === 'ArrowRight' || key === 'ArrowDown') return order[(i + 1) % order.length];
      if (key === 'ArrowLeft' || key === 'ArrowUp') return order[(i + order.length - 1) % order.length];
      if (key === 'Home') return order[0];
      if (key === 'End') return order[order.length - 1];
      return '';
    }
    // Every role that can see a running session gets the live terminal: the
    // keyboard holder writable, everyone else read-only (decided by the
    // server, never here). Scrollback is the fallback when the iframe fails.
    function sessionsViewMode(role, session) {
      if (!session) return 'none';
      if (session.status === SESSIONS_STATUS_ENDED) return 'ended';
      if (session.status === SESSIONS_STATUS_PREPARING) return 'preparing';
      if (session.status === SESSIONS_STATUS_FAILED) return 'failed';
      return 'terminal';
    }
    // Send (and upload) need a write role; the read role only watches.
    function sessionsCanSend(role, session) {
      return sessionsRoleCanWrite(role) && !!session && session.status === SESSIONS_STATUS_RUNNING;
    }
    // Which keyboard action the pane header offers: release for the holder,
    // take for any other writer (the server answers 409 while the holder is
    // active), nothing for the read role or a session that is not running.
    function sessionsKeyboardAction(role, session) {
      if (!sessionsCanSend(role, session)) return '';
      return session.keyboard_mine ? SESSIONS_KEYBOARD_RELEASE : SESSIONS_KEYBOARD_TAKE;
    }
    // "keyboard: andy · watching: bob, carol" from the server's viewers list.
    function sessionsPresenceText(session) {
      const viewers = (session && Array.isArray(session.viewers)) ? session.viewers.filter(function(v) { return v && v.user; }) : [];
      const holder = (session && session.keyboard_holder) || '';
      const watching = viewers.filter(function(v) { return v.user !== holder; }).map(function(v) { return v.user; });
      const holderAttached = viewers.some(function(v) { return v.user === holder; });
      const parts = [];
      if (holder) parts.push('keyboard: ' + holder + (holderAttached ? '' : ' (away)'));
      else parts.push('keyboard: free');
      if (watching.length) parts.push('watching: ' + watching.join(', '));
      return parts.join(' · ');
    }
    // The Send button label: "Send" or "Send · queued behind N".
    function sessionsQueueLabel(base, queue) {
      const n = Number(queue && queue.length) || 0;
      return n > 0 ? base + ' · queued behind ' + n : base;
    }
    // The toast after a 202 from /input.
    function sessionsQueuedToast(name, queued) {
      const pos = Number(queued && queued.position) || 1;
      return pos <= 1 ? 'Sent to ' + name + ' (delivered when the prompt is idle).' : 'Queued for ' + name + ' at position ' + pos + '.';
    }
    // Running and preparing sessions both hold (or are about to hold) a slot.
    function sessionsIsLive(s) {
      return !!s && (s.status === SESSIONS_STATUS_RUNNING || s.status === SESSIONS_STATUS_PREPARING);
    }
    function sessionsHasPreparing(sessions) {
      return (sessions || []).some(function(s) { return s && s.status === SESSIONS_STATUS_PREPARING; });
    }
    // The line shown in a preparing or failed tab: the server's message
    // ("preparing workspace: cloning org/repo (first session on this hive)")
    // with a hint about how long it takes, or the stored start error.
    function sessionsStatusText(s) {
      if (!s) return '';
      if (s.status === SESSIONS_STATUS_PREPARING) {
        const msg = String(s.message || 'preparing the session').trim();
        return msg.charAt(0).toUpperCase() + msg.slice(1) + '… ' + SESSIONS_PREPARING_HINT;
      }
      if (s.status === SESSIONS_STATUS_FAILED) {
        return 'Could not start this session: ' + String(s.error || 'unknown error') + '.';
      }
      if (s.status === SESSIONS_STATUS_ENDED && s.error) return 'The last resume failed: ' + String(s.error) + '.';
      return '';
    }
    function sessionsByteLength(text) {
      const s = String(text == null ? '' : text);
      if (typeof TextEncoder === 'function') return new TextEncoder().encode(s).length;
      return unescape(encodeURIComponent(s)).length;
    }
    function sessionsFormatBytes(n) {
      const KIB = 1024;
      const MIB = KIB * KIB;
      if (n >= MIB) return (n / MIB).toFixed(1).replace(/\.0$/, '') + ' MiB';
      if (n >= KIB) return Math.round(n / KIB) + ' KiB';
      return n + ' bytes';
    }
    function sessionsCheckInput(text, limits) {
      const value = String(text == null ? '' : text);
      if (!value.trim()) return { ok: false, error: 'Type or paste something to send first.' };
      const max = Number(limits && limits.max_input_bytes) || 0;
      const size = sessionsByteLength(value);
      if (max > 0 && size > max) {
        return { ok: false, error: 'That is ' + sessionsFormatBytes(size) + '; a single send is limited to ' + sessionsFormatBytes(max) + '. Split it up or attach it as a file.' };
      }
      return { ok: true, bytes: size };
    }
    function sessionsCheckUpload(file, limits) {
      if (!file) return { ok: false, error: 'Choose a file to attach first.' };
      const max = Number(limits && limits.max_upload_bytes) || 0;
      const size = Number(file.size) || 0;
      if (!size) return { ok: false, error: (file.name || 'That file') + ' is empty.' };
      if (max > 0 && size > max) {
        return { ok: false, error: (file.name || 'That file') + ' is ' + sessionsFormatBytes(size) + '; uploads are limited to ' + sessionsFormatBytes(max) + '.' };
      }
      return { ok: true, bytes: size };
    }
    function sessionsCanCreate(role, data) {
      if (!sessionsRoleCanWrite(role)) return { ok: false, reason: 'Your role can watch sessions but cannot start them.' };
      const vendors = ((data && data.vendors) || []).filter(function(v) { return v && v.available; });
      if (!vendors.length) return { ok: false, reason: 'No CLI vendor is available on this hive.' };
      const max = Number(data && data.limits && data.limits.max_sessions) || 0;
      const running = ((data && data.sessions) || []).filter(sessionsIsLive).length;
      if (max > 0 && running >= max) return { ok: false, reason: 'This hive is at its limit of ' + max + ' running sessions. Close one first.' };
      return { ok: true, reason: '' };
    }
    // Where a session's CLI runs: the hive's shared checkout (repo + path) or,
    // without one, the session's own empty directory.
    function sessionsWorkspaceLabel(ws) {
      const repo = ws && ws.repo ? String(ws.repo) : '';
      const path = ws && ws.path ? String(ws.path) : '';
      if (!repo) return 'No workspace repo is configured; sessions start in an empty directory.';
      return 'Workspace: ' + repo + (path ? ' (' + path + ')' : '') + ', shared by every session on this hive.';
    }
    function sessionsSessionWorkspace(s) {
      return s ? { repo: s.workspace_repo || '', path: s.workspace || '' } : {};
    }
    function sessionsTmuxName(id) { return SESSIONS_TMUX_PREFIX + id; }
    function sessionsTerminalPath(id, code) {
      return '/terminal/?arg=' + encodeURIComponent(sessionsTmuxName(id)) + (code ? '&code=' + encodeURIComponent(code) : '');
    }
    function sessionsIsEnabled() { return window._sessionsEnabled === true; }

    // ── Storage conveniences (never required) ──
    function sessionsStoreGet(key) { try { return localStorage.getItem(key) || ''; } catch (e) { return ''; } }
    function sessionsStoreSet(key, value) { try { localStorage.setItem(key, value); } catch (e) { /* convenience only */ } }
    function sessionsReadCollapsed() {
      try { const v = JSON.parse(sessionsStoreGet(SESSIONS_COLLAPSED_KEY) || '{}'); return v && typeof v === 'object' ? v : {}; } catch (e) { return {}; }
    }

    // ── Networking ──
    function sessionsHeaders(json) {
      const h = _inceptionAuthHeaders();
      if (!json) delete h[SESSIONS_HEADER_CONTENT_TYPE];
      return h;
    }
    async function sessionsApi(path, method, body) {
      const opts = { method: method || 'GET', headers: sessionsHeaders(body !== undefined && !(body instanceof FormData)), credentials: 'same-origin', cache: 'no-store' };
      if (body instanceof FormData) opts.body = body;
      else if (body !== undefined) opts.body = JSON.stringify(body);
      const resp = await fetch(SESSIONS_API + path, opts);
      let data = {};
      try { data = await resp.json(); } catch (e) { /* empty body */ }
      if (!resp.ok || data.ok === false) {
        const err = new Error(data.error || ('HTTP ' + resp.status));
        err.status = resp.status;
        throw err;
      }
      return data;
    }
    async function sessionsLoadRole() {
      try {
        const resp = await fetch('/api/role');
        const data = await resp.json();
        _sessionsRole = data.role || '';
      } catch (e) { _sessionsRole = ''; }
    }
    async function loadSessions() {
      try {
        const resp = await fetch(SESSIONS_API, { headers: sessionsHeaders(false), credentials: 'same-origin', cache: 'no-store' });
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        const data = await resp.json();
        _sessionsData = {
          enabled: data.enabled === true,
          sessions: Array.isArray(data.sessions) ? data.sessions : [],
          vendors: Array.isArray(data.vendors) ? data.vendors : [],
          limits: data.limits || {},
          workspace: data.workspace || {},
        };
      } catch (e) {
        if (!_sessionsLoaded) _sessionsData = { enabled: false, sessions: [], vendors: [], limits: {}, workspace: {} };
        else return;
      }
      const wasEnabled = sessionsIsEnabled();
      window._sessionsEnabled = _sessionsData.enabled;
      _sessionsLoaded = true;
      if (wasEnabled !== _sessionsData.enabled) sessionsApplyVisibility();
      if (!_sessionsData.enabled) { dashboardClearInterval('sessions-poll'); return; }
      if (!_sessionsRole) await sessionsLoadRole();
      _sessionsSelected = sessionsPickSelected(_sessionsData.sessions, _sessionsSelected || sessionsStoreGet(SESSIONS_SELECTED_KEY));
      renderSessions();
    }
    function sessionsStartPolling() {
      dashboardSetInterval('sessions-poll', loadSessions, SESSIONS_POLL_MS);
    }

    // ── Visibility: Sessions replaces Inception when enabled ──
    function sessionsApplyVisibility() {
      const on = sessionsIsEnabled();
      [['sessions-section', on], ['inception-section', !on]].forEach(function(pair) {
        const el = document.getElementById(pair[0]);
        if (el) el.hidden = !pair[1];
        const nav = document.querySelector('.oc-nav-item[data-section="' + pair[0] + '"]');
        if (nav) nav.hidden = !pair[1];
      });
      if (typeof dashboardApplyLayout === 'function' && typeof dashboardLayoutNormalize === 'function') {
        dashboardApplyLayout(dashboardLayoutNormalize(dashboardLayoutRead()));
      }
      if (on) {
        if (typeof refreshSectionCardShell === 'function') refreshSectionCardShell('sessions-section');
        sessionsStartPolling();
      }
    }

    // ── Rendering ──
    function sessionsSelectedSession() {
      return (_sessionsData.sessions || []).find(function(s) { return s.id === _sessionsSelected; }) || null;
    }
    function sessionsTabHtml(s) {
      const selected = s.id === _sessionsSelected;
      const ended = s.status === SESSIONS_STATUS_ENDED;
      const preparing = s.status === SESSIONS_STATUS_PREPARING;
      const failed = s.status === SESSIONS_STATUS_FAILED;
      const attached = Number(s.attached) || 0;
      const badge = attached > 0 ? '<span class="sessions-attached" title="' + esc(SESSIONS_TYPING_NOTE) + '">' + attached + '</span>' : '';
      const title = preparing || failed ? sessionsStatusText(s)
        : (s.status === SESSIONS_STATUS_RUNNING ? sessionsPresenceText(s) + '\n' : '') + sessionsWorkspaceLabel(sessionsSessionWorkspace(s));
      const dot = preparing
        ? '<span class="sessions-spinner" role="img" aria-label="Preparing"></span>'
        : '<span class="sessions-tab-dot" data-status="' + esc(s.status || '') + '"></span>';
      return '<button type="button" role="tab" class="sessions-tab' + (selected ? ' active' : '') + (ended ? ' ended' : '') + (preparing ? ' preparing' : '') + (failed ? ' failed' : '') + '" id="sessions-tab-' + esc(s.id) + '"'
        + ' title="' + esc(title) + '"'
        + ' aria-selected="' + selected + '" aria-controls="sessions-pane-' + esc(s.id) + '" tabindex="' + (selected ? '0' : '-1') + '"'
        + ' data-action="sessionsSelect" data-arg0="' + esc(s.id) + '" data-keydown-action="sessionsTabKey" data-arg-types="s,e" data-keys="*" data-arg1="">'
        + dot
        + '<span class="sessions-tab-name">' + esc(s.name || s.id) + '</span>'
        + '<span class="sessions-tab-vendor">' + esc(s.vendor || '') + '</span>' + badge + '</button>';
    }
    function sessionsTabsHtml() {
      const campaignsSel = _sessionsSelected === SESSIONS_CAMPAIGNS_TAB;
      let html = sessionsGroupSessions(_sessionsData.sessions).map(function(g) {
        const collapsed = !!_sessionsCollapsed[g.group];
        const tabs = g.sessions.filter(function(s) { return !collapsed || s.id === _sessionsSelected; }).map(sessionsTabHtml).join('');
        return '<div class="sessions-group" role="presentation">'
          + '<button type="button" class="sessions-group-toggle" aria-expanded="' + !collapsed + '" data-action="sessionsToggleGroup" data-arg0="' + esc(g.group) + '" title="' + (collapsed ? 'Expand' : 'Collapse') + ' the ' + esc(g.label) + ' group">'
          + '<span class="sessions-group-chevron">' + (collapsed ? '▶' : '▼') + '</span>' + esc(g.label) + ' <span class="sessions-group-count">' + g.sessions.length + '</span></button>'
          + '<div class="sessions-group-tabs" role="presentation">' + tabs + '</div></div>';
      }).join('');
      html += '<div class="sessions-group" role="presentation"><div class="sessions-group-tabs" role="presentation">'
        + '<button type="button" role="tab" class="sessions-tab' + (campaignsSel ? ' active' : '') + '" id="sessions-tab-' + SESSIONS_CAMPAIGNS_TAB + '" aria-selected="' + campaignsSel + '" aria-controls="sessions-pane-' + SESSIONS_CAMPAIGNS_TAB + '" tabindex="' + (campaignsSel ? '0' : '-1') + '"'
        + ' data-action="sessionsSelect" data-arg0="' + SESSIONS_CAMPAIGNS_TAB + '" data-keydown-action="sessionsTabKey" data-arg-types="s,e" data-keys="*" data-arg1="">'
        + '<span class="sessions-tab-name">Campaigns</span></button></div></div>';
      return html;
    }
    function sessionsNewFormHtml() {
      const vendors = (_sessionsData.vendors || []).filter(function(v) { return v && v.available; });
      const groups = sessionsExistingGroups(_sessionsData.sessions);
      return '<div class="sessions-form-row"><label for="sessions-new-name">Name</label><input id="sessions-new-name" class="campaigns-input" maxlength="80" autocomplete="off" placeholder="e.g. fix flaky test"></div>'
        + '<div class="sessions-form-row"><label for="sessions-new-group">Group</label><input id="sessions-new-group" class="campaigns-input" maxlength="80" list="sessions-group-list" autocomplete="off" placeholder="optional">'
        + '<datalist id="sessions-group-list">' + groups.map(function(g) { return '<option value="' + esc(g) + '"></option>'; }).join('') + '</datalist></div>'
        + '<div class="sessions-form-row"><label for="sessions-new-vendor">CLI</label><select id="sessions-new-vendor" class="campaigns-input">'
        + vendors.map(function(v) { return '<option value="' + esc(v.id) + '">' + esc(v.label || v.id) + '</option>'; }).join('') + '</select></div>'
        + '<div class="sessions-form-row"><label for="sessions-new-shared">Share</label><input id="sessions-new-shared" type="checkbox"> <span>Let the team watch and type (read-write users)</span></div>'
        + '<div class="sessions-form-row"><label>Workspace</label><span class="sessions-workspace">' + esc(sessionsWorkspaceLabel(_sessionsData.workspace)) + '</span></div>'
        + '<div class="sessions-form-actions"><button type="button" class="hv-btn btn-primary btn-sm" data-action="sessionsCreate">Start session</button>'
        + '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="sessionsToggleNew">Cancel</button></div>'
        + '<div class="sessions-note">Sessions run the vendor CLI with permission prompts on, as their own unprivileged user, inside the workspace above (a checkout of the repo with its spek skills installed; every session shares it, like several people on one checkout). A session is private to you and the hive owners unless you share it.</div>';
    }
    function sessionsToolbarHtml() {
      const can = sessionsCanCreate(_sessionsRole, _sessionsData);
      return '<div class="sessions-toolbar"><div id="sessions-tabs" class="sessions-tabs" role="tablist" aria-label="Inception (spek) sessions">' + sessionsTabsHtml() + '</div>'
        + '<button type="button" class="hv-btn btn-primary btn-sm sessions-new-btn" data-action="sessionsToggleNew"' + (can.ok ? '' : ' disabled title="' + esc(can.reason) + '"') + '>+ New session</button></div>'
        + '<div id="sessions-new" class="sessions-new" role="group" aria-label="New session"' + (_sessionsNewOpen && can.ok ? '' : ' hidden') + '>' + (_sessionsNewOpen && can.ok ? sessionsNewFormHtml() : '') + '</div>';
    }
    // The keyboard button and the "watching" badge of a running session.
    function sessionsKeyboardHtml(s) {
      if (s.status !== SESSIONS_STATUS_RUNNING) return '';
      const action = sessionsKeyboardAction(_sessionsRole, s);
      if (action === SESSIONS_KEYBOARD_RELEASE) {
        return '<button type="button" class="hv-btn btn-secondary btn-sm sessions-keyboard-btn" data-action="sessionsKeyboard" data-arg0="' + esc(s.id) + '" data-arg1="' + SESSIONS_KEYBOARD_RELEASE + '" title="Let someone else type; you keep watching">Release keyboard</button>';
      }
      if (action === SESSIONS_KEYBOARD_TAKE) {
        const holder = s.keyboard_holder ? ' from ' + s.keyboard_holder : '';
        return '<button type="button" class="hv-btn btn-primary btn-sm sessions-keyboard-btn" data-action="sessionsKeyboard" data-arg0="' + esc(s.id) + '" data-arg1="' + SESSIONS_KEYBOARD_TAKE + '" title="Take the keyboard' + esc(holder) + '; their terminal becomes read-only">Take keyboard</button>';
      }
      return '<span class="sessions-watching" title="Your role can watch this session but not type into it">watching · you have the keyboard? no</span>';
    }
    function sessionsHeaderHtml(s) {
      const canManage = sessionsRoleCanWrite(_sessionsRole);
      const attached = Number(s.attached) || 0;
      const presence = s.status === SESSIONS_STATUS_RUNNING
        ? '<span class="sessions-presence' + (s.keyboard_mine ? ' mine' : '') + '" title="' + esc(SESSIONS_TYPING_NOTE) + '">' + esc(sessionsPresenceText(s)) + '</span>'
        : '';
      // A preparing session cannot be closed yet (the start is in flight); a
      // failed one is deleted (or retried from its pane) rather than closed.
      let closeItem = '<button type="button" class="danger" data-action="sessionsClose" data-arg0="' + esc(s.id) + '">Close session</button>';
      if (s.status === SESSIONS_STATUS_PREPARING) closeItem = '';
      else if (s.status === SESSIONS_STATUS_FAILED || s.status === SESSIONS_STATUS_ENDED) closeItem = '<button type="button" class="danger" data-action="sessionsClose" data-arg0="' + esc(s.id) + '">Delete session</button>';
      const menu = canManage
        ? '<details class="sessions-menu"><summary class="hv-btn btn-secondary btn-sm" aria-label="Session actions">⋯</summary><div class="sessions-menu-items">'
          + '<button type="button" data-action="sessionsRename" data-arg0="' + esc(s.id) + '">Rename</button>'
          + '<button type="button" data-action="sessionsRegroup" data-arg0="' + esc(s.id) + '">Change group</button>'
          + closeItem + '</div></details>'
        : '';
      return '<div class="sessions-pane-head"><span class="sessions-pane-title">' + esc(s.name || s.id) + '</span>'
        + '<span class="sessions-pane-meta" title="' + esc(sessionsWorkspaceLabel(sessionsSessionWorkspace(s))) + '">' + esc(s.vendor || '') + ' · started by ' + esc(s.creator || 'unknown') + (s.group ? ' · ' + esc(s.group) : '') + (s.workspace_repo ? ' · ' + esc(s.workspace_repo) : '') + '</span>'
        + presence
        + '<span class="sessions-attached sessions-pane-attached" title="' + esc(SESSIONS_TYPING_NOTE) + '">' + attached + ' attached</span>' + sessionsKeyboardHtml(s) + menu + '</div>';
    }
    // The pending Send queue under the input bar; Cancel on your own items.
    function sessionsQueueHtml(s) {
      const q = (s && s.queue) || {};
      const items = Array.isArray(q.items) ? q.items : [];
      if (!items.length) return '<div class="sessions-queue" id="sessions-queue-' + esc(s.id) + '" hidden></div>';
      return '<div class="sessions-queue" id="sessions-queue-' + esc(s.id) + '" aria-live="polite"><div class="sessions-queue-title">' + items.length + ' queued' + (q.position ? ' · yours is #' + q.position : '') + '</div>'
        + items.map(function(it, i) {
          const what = (it.submit ? 'send and run' : 'send') + ' · ' + sessionsFormatBytes(Number(it.bytes) || 0);
          const state = it.delivering ? '<span class="sessions-queue-state">delivering…</span>' : '';
          const cancel = it.mine && !it.delivering ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="sessionsCancelQueued" data-arg0="' + esc(s.id) + '" data-arg1="' + esc(it.id) + '">Cancel</button>' : '';
          return '<div class="sessions-queue-item' + (it.mine ? ' mine' : '') + '"><span class="sessions-queue-pos">#' + (i + 1) + '</span><span class="sessions-queue-user">' + esc(it.user || '') + '</span><span class="sessions-queue-what">' + esc(what) + '</span>' + state + cancel + '</div>';
        }).join('') + '</div>';
    }
    function sessionsInputBarHtml(s) {
      if (!sessionsCanSend(_sessionsRole, s)) return '';
      const max = Number(_sessionsData.limits && _sessionsData.limits.max_input_bytes) || 0;
      const more = SESSIONS_INPUT_ACTIONS.length
        ? '<details class="sessions-menu sessions-input-more"><summary class="hv-btn btn-secondary btn-sm">More</summary><div class="sessions-menu-items">'
          + SESSIONS_INPUT_ACTIONS.map(function(a) { return '<button type="button" data-action="sessionsInputAction" data-arg0="' + esc(a.id) + '" title="' + esc(a.title || '') + '">' + esc(a.label) + '</button>'; }).join('')
          + '</div></details>'
        : '';
      return '<div class="sessions-input" data-session="' + esc(s.id) + '">'
        + '<label class="sessions-input-label" for="sessions-input-text-' + esc(s.id) + '">Send text to this session</label>'
        + '<textarea id="sessions-input-text-' + esc(s.id) + '" class="sessions-input-text" rows="3" placeholder="Paste or type a prompt, then Send. Multiline is fine."'
        + ' data-input-action="sessionsInputChanged" data-arg-types="t"></textarea>'
        + '<div class="sessions-input-actions">'
        + '<button type="button" class="hv-btn btn-secondary btn-sm sessions-send-btn" data-action="sessionsSend" data-arg0="0" title="Queue the text; it is typed into the terminal, without Enter, once the prompt is idle">' + esc(sessionsQueueLabel('Send', s.queue)) + '</button>'
        + '<button type="button" class="hv-btn btn-primary btn-sm sessions-send-run-btn" data-action="sessionsSend" data-arg0="1" title="Queue the text; it is typed and run once the prompt is idle">' + esc(sessionsQueueLabel('Send and run', s.queue)) + '</button>'
        + '<label class="hv-btn btn-secondary btn-sm sessions-attach" title="Upload a file into the session workspace and type its path">Attach file'
        + '<input type="file" class="sessions-file" id="sessions-file-' + esc(s.id) + '" data-change-action="sessionsFilePicked" data-arg-types="t"></label>' + more
        + '<span class="sessions-input-count" id="sessions-input-count-' + esc(s.id) + '" aria-live="polite">' + (max ? '0 / ' + sessionsFormatBytes(max) : '') + '</span></div>'
        + sessionsQueueHtml(s) + '</div>';
    }
    // Refresh the queue-dependent bits of a live pane without rebuilding it
    // (the iframe and a half-typed Send must survive polling).
    function sessionsRefreshQueueUI(s) {
      const pane = document.getElementById('sessions-pane-' + s.id);
      if (!pane) return;
      const queue = pane.querySelector('#sessions-queue-' + s.id);
      if (queue) queue.outerHTML = sessionsQueueHtml(s);
      const send = pane.querySelector('.sessions-send-btn');
      if (send) send.textContent = sessionsQueueLabel('Send', s.queue);
      const run = pane.querySelector('.sessions-send-run-btn');
      if (run) run.textContent = sessionsQueueLabel('Send and run', s.queue);
    }
    function sessionsPaneBodyHtml(s, mode) {
      if (mode === 'terminal') return '<div class="sessions-terminal" data-terminal-host="' + esc(s.id) + '"></div>' + sessionsInputBarHtml(s);
      const can = sessionsRoleCanWrite(_sessionsRole);
      if (mode === 'preparing') {
        return '<div class="sessions-status sessions-status-preparing" role="status" aria-live="polite"><span class="sessions-spinner" aria-hidden="true"></span>'
          + '<span class="sessions-status-text">' + esc(sessionsStatusText(s)) + ' The terminal opens here by itself once the session is running.</span></div>';
      }
      if (mode === 'failed') {
        return '<div class="sessions-status sessions-status-failed" role="alert"><span class="sessions-status-text">' + esc(sessionsStatusText(s)) + '</span>'
          + (can ? '<div class="sessions-form-actions"><button type="button" class="hv-btn btn-primary btn-sm" data-action="sessionsRetry" data-arg0="' + esc(s.id) + '">Retry</button>'
            + '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="sessionsClose" data-arg0="' + esc(s.id) + '">Delete</button></div>' : '') + '</div>';
      }
      let note;
      if (mode === 'ended') {
        const failedResume = s.error ? ' ' + sessionsStatusText(s) : '';
        const idAttr = esc(s.id);
        const restartBtn = function(cls) { return ' <button type="button" class="hv-btn ' + cls + ' btn-sm" data-action="sessionsRestart" data-arg0="' + idAttr + '">Start again</button>'; };
        let actions = '';
        let lead = 'This session has ended (the hive restarted or the process exited). Scrollback below is the last output.';
        if (s.resumable) {
          actions = can ? ' <button type="button" class="hv-btn btn-primary btn-sm" data-action="sessionsResume" data-arg0="' + idAttr + '">Resume</button>' + restartBtn('btn-secondary') : '';
        } else {
          // The CLI left no conversation to continue, so Resume would only 409.
          lead = 'This session has ended and cannot be resumed (the CLI left no conversation to continue). Start again opens a fresh ' + (s.vendor || 'CLI') + ' session with the same name and group; the scrollback below stays.';
          actions = can ? restartBtn('btn-primary') : '';
        }
        note = '<div class="sessions-note">' + esc(lead) + esc(failedResume) + actions + '</div>';
      } else {
        note = '<div class="sessions-note">The live terminal could not be opened, so you see a read-only scrollback that refreshes every few seconds.</div>';
      }
      return note + '<pre class="sessions-log" id="sessions-log-' + esc(s.id) + '" tabindex="0" aria-label="Session scrollback">Loading scrollback…</pre>';
    }
    function sessionsEnsurePane(s) {
      const host = document.getElementById('sessions-panes');
      if (!host) return null;
      let pane = document.getElementById('sessions-pane-' + s.id);
      const mode = sessionsViewMode(_sessionsRole, s) === 'terminal' && _sessionsFrameFailed[s.id] ? 'scrollback' : sessionsViewMode(_sessionsRole, s);
      if (pane && pane.getAttribute('data-mode') !== mode) {
        // Mode changed (ended, resumed, role change): rebuild this pane only.
        delete _sessionsFrames[s.id];
        pane.remove();
        pane = null;
      }
      if (!pane) {
        pane = document.createElement('div');
        pane.id = 'sessions-pane-' + s.id;
        pane.className = 'sessions-pane';
        pane.setAttribute('role', 'tabpanel');
        pane.setAttribute('aria-labelledby', 'sessions-tab-' + s.id);
        pane.setAttribute('data-mode', mode);
        pane.innerHTML = '<div class="sessions-pane-header"></div>' + sessionsPaneBodyHtml(s, mode);
        host.appendChild(pane);
      }
      const head = pane.querySelector('.sessions-pane-header');
      if (head) head.innerHTML = sessionsHeaderHtml(s);
      if (mode === 'terminal') { sessionsRefreshQueueUI(s); sessionsSyncKeyboardFrame(s); }
      return pane;
    }
    // When the keyboard changes hands the attached iframe is stale: the old
    // holder's tmux client was detached by the server, the new holder's is
    // still read-only. Drop the frame and re-attach with a fresh handoff, in
    // whichever mode the server now grants this user.
    const _sessionsFrameKeyboard = {};
    function sessionsSyncKeyboardFrame(s) {
      const mine = !!s.keyboard_mine;
      const frame = _sessionsFrames[s.id];
      if (!frame || frame === 'pending') { _sessionsFrameKeyboard[s.id] = mine; return; }
      if (_sessionsFrameKeyboard[s.id] === mine) return;
      _sessionsFrameKeyboard[s.id] = mine;
      if (frame.remove) frame.remove();
      delete _sessionsFrames[s.id];
      if (s.id === _sessionsSelected) sessionsAttachTerminal(s);
    }
    function renderSessions() {
      const panel = document.getElementById('sessions-panel');
      if (!panel) return;
      refreshSectionCardShell('sessions-section');
      _sessionsCollapsed = Object.keys(_sessionsCollapsed).length ? _sessionsCollapsed : sessionsReadCollapsed();
      const sessions = _sessionsData.sessions || [];
      setSectionSummary('sessions-section', sessionsSummary(sessions));
      let root = document.getElementById('sessions-root');
      if (!root) {
        panel.innerHTML = '<div id="sessions-root" class="sessions-root"><div id="sessions-chrome"></div><div id="sessions-empty" class="sessions-note" hidden></div><div id="sessions-panes" class="sessions-panes"></div></div>';
        root = document.getElementById('sessions-root');
      }
      // Re-render only the chrome so live iframes and a half-typed input survive polling.
      const chrome = document.getElementById('sessions-chrome');
      const focusId = document.activeElement && document.activeElement.closest && document.activeElement.closest('#sessions-tabs') ? (document.activeElement.id || '') : '';
      const typing = document.activeElement && document.activeElement.closest && document.activeElement.closest('#sessions-new');
      if (!typing) chrome.innerHTML = sessionsToolbarHtml();
      if (focusId) { const f = document.getElementById(focusId); if (f) f.focus(); }
      const empty = document.getElementById('sessions-empty');
      empty.hidden = sessions.length > 0 || _sessionsSelected !== SESSIONS_CAMPAIGNS_TAB;
      empty.textContent = sessions.length ? '' : 'No sessions yet. Start one with “New session” to run a CLI agent here; everyone on this hive can watch or join it.';
      const liveIds = {};
      sessions.forEach(function(s) { liveIds[s.id] = true; sessionsEnsurePane(s); });
      Array.prototype.forEach.call(document.querySelectorAll('#sessions-panes .sessions-pane'), function(pane) {
        const id = pane.id.replace('sessions-pane-', '');
        if (id !== SESSIONS_CAMPAIGNS_TAB && !liveIds[id]) { delete _sessionsFrames[id]; pane.remove(); }
      });
      sessionsEnsureCampaignsPane();
      sessionsShowSelected();
      sessionsSyncPreparingPoll(sessions);
    }
    // Poll faster while any session is preparing, so the tab turns into a
    // terminal (or shows its failure) within seconds of the start finishing.
    function sessionsSyncPreparingPoll(sessions) {
      const want = sessionsHasPreparing(sessions);
      if (want && !_sessionsPrepPollOn) { _sessionsPrepPollOn = true; dashboardSetInterval('sessions-preparing-poll', loadSessions, SESSIONS_PREPARING_POLL_MS); }
      if (!want && _sessionsPrepPollOn) { _sessionsPrepPollOn = false; dashboardClearInterval('sessions-preparing-poll'); }
    }
    function sessionsSummary(sessions) {
      const running = (sessions || []).filter(function(s) { return s.status === SESSIONS_STATUS_RUNNING; }).length;
      const preparing = (sessions || []).filter(function(s) { return s.status === SESSIONS_STATUS_PREPARING; }).length;
      return running + ' running · ' + (preparing ? preparing + ' preparing · ' : '') + (sessions || []).length + ' total';
    }
    function sessionsEnsureCampaignsPane() {
      const host = document.getElementById('sessions-panes');
      if (!host || document.getElementById('sessions-pane-' + SESSIONS_CAMPAIGNS_TAB)) return;
      const pane = document.createElement('div');
      pane.id = 'sessions-pane-' + SESSIONS_CAMPAIGNS_TAB;
      pane.className = 'sessions-pane';
      pane.setAttribute('role', 'tabpanel');
      pane.setAttribute('aria-labelledby', 'sessions-tab-' + SESSIONS_CAMPAIGNS_TAB);
      pane.setAttribute('data-mode', 'campaigns');
      host.appendChild(pane);
    }
    function sessionsShowSelected() {
      Array.prototype.forEach.call(document.querySelectorAll('#sessions-panes .sessions-pane'), function(pane) {
        pane.hidden = pane.id !== 'sessions-pane-' + _sessionsSelected;
      });
      if (_sessionsSelected === SESSIONS_CAMPAIGNS_TAB) {
        sessionsStopLogPoll();
        const pane = document.getElementById('sessions-pane-' + SESSIONS_CAMPAIGNS_TAB);
        if (pane && !pane.firstChild) { pane.innerHTML = renderCampaignsPanel(); loadCampaigns(); }
        return;
      }
      const s = sessionsSelectedSession();
      if (!s) return;
      const mode = sessionsViewMode(_sessionsRole, s);
      if (mode === 'preparing' || mode === 'failed') { sessionsStopLogPoll(); return; }
      if (mode === 'terminal' && !_sessionsFrameFailed[s.id]) { sessionsStopLogPoll(); sessionsAttachTerminal(s); } else sessionsStartLogPoll();
    }

    // ── Terminal iframe (lazy, kept alive when hidden) ──
    async function sessionsHandoff(id) {
      if (typeof renewTerminalAssertion === 'function') await renewTerminalAssertion();
      const resp = await fetch('/api/terminal/handoff', { method: 'POST', headers: sessionsHeaders(true), credentials: 'same-origin', cache: 'no-store', body: JSON.stringify({ session: id }) });
      const data = await resp.json().catch(function() { return {}; });
      if (!resp.ok || !data.code) {
        const err = new Error(data.error || ('HTTP ' + resp.status));
        err.status = resp.status;
        throw err;
      }
      return data.code;
    }
    async function sessionsAttachTerminal(s) {
      if (_sessionsFrames[s.id]) return;
      const host = document.querySelector('[data-terminal-host="' + s.id + '"]');
      if (!host) return;
      _sessionsFrames[s.id] = 'pending';
      _sessionsFrameKeyboard[s.id] = !!s.keyboard_mine;
      try {
        const code = await sessionsHandoff(s.id);
        const frame = document.createElement('iframe');
        frame.className = 'sessions-frame';
        frame.title = 'Terminal for session ' + (s.name || s.id);
        frame.setAttribute('allow', 'clipboard-read; clipboard-write');
        frame.src = sessionsTerminalPath(s.id, code);
        host.appendChild(frame);
        _sessionsFrames[s.id] = frame;
        sessionsStartAssertionRenew();
      } catch (e) {
        delete _sessionsFrames[s.id];
        _sessionsFrameFailed[s.id] = true;
        showToast('Could not open the terminal for ' + (s.name || s.id) + ': ' + e.message + '. Showing read-only scrollback instead.', 'error');
        const pane = document.getElementById('sessions-pane-' + s.id);
        if (pane) { pane.remove(); }
        renderSessions();
      }
    }
    function sessionsHasFrames() {
      return Object.keys(_sessionsFrames).some(function(k) { return _sessionsFrames[k] && _sessionsFrames[k] !== 'pending'; });
    }
    function sessionsStartAssertionRenew() {
      if (_sessionsAssertionTimerOn) return;
      _sessionsAssertionTimerOn = true;
      dashboardSetInterval('sessions-assertion', async function() {
        if (!sessionsHasFrames() || !sessionsIsEnabled()) { dashboardClearInterval('sessions-assertion'); _sessionsAssertionTimerOn = false; return; }
        if (typeof renewTerminalAssertion === 'function') await renewTerminalAssertion();
      }, SESSIONS_ASSERTION_RENEW_MS);
    }

    // ── Scrollback view (read role / ended sessions) ──
    async function sessionsRefreshLog() {
      const s = sessionsSelectedSession();
      if (!s || document.hidden) return;
      const el = document.getElementById('sessions-log-' + s.id);
      if (!el) return;
      try {
        const data = await sessionsApi('/' + encodeURIComponent(s.id) + '/log?lines=' + SESSIONS_LOG_LINES, 'GET');
        const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < el.clientHeight;
        el.textContent = (data.lines || []).join('\n') || '(no output yet)';
        if (atBottom) el.scrollTop = el.scrollHeight;
      } catch (e) { el.textContent = 'Could not load scrollback: ' + e.message; }
    }
    function sessionsStartLogPoll() {
      sessionsRefreshLog();
      if (_sessionsLogTimerOn) return;
      _sessionsLogTimerOn = true;
      dashboardSetInterval('sessions-log', sessionsRefreshLog, SESSIONS_LOG_POLL_MS);
    }
    function sessionsStopLogPoll() {
      if (!_sessionsLogTimerOn) return;
      _sessionsLogTimerOn = false;
      dashboardClearInterval('sessions-log');
    }

    // ── Actions (data-action handlers) ──
    function sessionsSelect(id) {
      if (!id || id === _sessionsSelected) return;
      _sessionsSelected = id;
      sessionsStoreSet(SESSIONS_SELECTED_KEY, id);
      renderSessions();
      const tab = document.getElementById('sessions-tab-' + id);
      if (tab && document.activeElement && document.activeElement.closest && document.activeElement.closest('#sessions-tabs')) tab.focus();
    }
    function sessionsTabKey(id, ev) {
      if (!ev) return;
      const order = sessionsTabOrder(_sessionsData.sessions, _sessionsCollapsed, _sessionsSelected);
      const next = sessionsNextTab(order, id, ev.key);
      if (!next) return;
      ev.preventDefault();
      sessionsSelect(next);
      const tab = document.getElementById('sessions-tab-' + next);
      if (tab) tab.focus();
    }
    function sessionsToggleGroup(group) {
      _sessionsCollapsed[group] = !_sessionsCollapsed[group];
      sessionsStoreSet(SESSIONS_COLLAPSED_KEY, JSON.stringify(_sessionsCollapsed));
      renderSessions();
    }
    function sessionsToggleNew() {
      _sessionsNewOpen = !_sessionsNewOpen;
      renderSessions();
      const name = document.getElementById('sessions-new-name');
      if (name) name.focus();
    }
    async function sessionsCreate() {
      const val = function(id) { const el = document.getElementById(id); return el ? el.value.trim() : ''; };
      const sharedBox = document.getElementById('sessions-new-shared');
      const body = { name: val('sessions-new-name'), group: val('sessions-new-group'), vendor: val('sessions-new-vendor'), shared: !!(sharedBox && sharedBox.checked) };
      if (!body.name) { showToast('Give the session a name.', 'error'); return; }
      if (!body.vendor) { showToast('Pick a CLI vendor.', 'error'); return; }
      try {
        await sessionsSubmitCreate(body);
        _sessionsNewOpen = false;
        renderSessions();
      } catch (e) { showToast('Could not start the session: ' + e.message, 'error'); }
    }
    // POST the create; the server answers at once with the session in
    // `preparing` (the clone and start run in the background), so it is added
    // to the list right away and selected: its tab shows the progress line
    // until polling sees it running (terminal attaches) or failed.
    async function sessionsSubmitCreate(body) {
      const data = await sessionsApi('', 'POST', body);
      if (data.session) {
        _sessionsData.sessions = (_sessionsData.sessions || []).filter(function(x) { return x.id !== data.session.id; });
        _sessionsData.sessions.push(data.session);
        _sessionsSelected = data.session.id;
        sessionsStoreSet(SESSIONS_SELECTED_KEY, data.session.id);
      }
      // renderSessions leaves the toolbar alone while focus is inside the New
      // session form (so typing survives polling); the click that got us here
      // left focus on its button, so drop it or the new tab would wait for
      // the next poll.
      if (document.activeElement && document.activeElement.closest && document.activeElement.closest('#sessions-new') && document.activeElement.blur) document.activeElement.blur();
      renderSessions();
      showToast('Starting session ' + body.name + '…', 'success');
      loadSessions();
      return data.session;
    }
    // Retry a failed session: its record is deleted and a new one is created
    // with the same name, group, vendor and sharing.
    async function sessionsRetry(id) {
      const s = (_sessionsData.sessions || []).find(function(x) { return x.id === id; });
      if (!s) return;
      const body = { name: s.name || id, group: s.group || '', vendor: s.vendor, shared: !!s.shared };
      try {
        await sessionsApi('/' + encodeURIComponent(id), 'DELETE');
        delete _sessionsFrames[id];
        _sessionsData.sessions = (_sessionsData.sessions || []).filter(function(x) { return x.id !== id; });
        await sessionsSubmitCreate(body);
      } catch (e) { showToast('Could not retry the session: ' + e.message, 'error'); loadSessions(); }
    }
    // Start again: an ended session with nothing to resume. A fresh session is
    // created with the same name, group, vendor and sharing; the old record
    // (and its scrollback) is kept so the user can still read it.
    async function sessionsRestart(id) {
      const s = (_sessionsData.sessions || []).find(function(x) { return x.id === id; });
      if (!s) return;
      const body = { name: s.name || id, group: s.group || '', vendor: s.vendor, shared: !!s.shared };
      try { await sessionsSubmitCreate(body); }
      catch (e) { showToast('Could not start the session again: ' + e.message, 'error'); }
    }
    async function sessionsPatch(id, patch, okMsg) {
      try {
        await sessionsApi('/' + encodeURIComponent(id), 'PATCH', patch);
        showToast(okMsg, 'success');
        await loadSessions();
      } catch (e) { showToast('Could not update the session: ' + e.message, 'error'); }
    }
    async function sessionsRename(id) {
      const s = (_sessionsData.sessions || []).find(function(x) { return x.id === id; });
      if (!s) return;
      const name = await hivePrompt({ title: 'Rename session', label: 'New name', defaultValue: s.name || '', confirmLabel: 'Rename', validate: function(v) { return v ? true : 'Name cannot be empty.'; } });
      if (name && name !== s.name) sessionsPatch(id, { name: name }, 'Renamed to ' + name + '.');
    }
    async function sessionsRegroup(id) {
      const s = (_sessionsData.sessions || []).find(function(x) { return x.id === id; });
      if (!s) return;
      const options = sessionsExistingGroups(_sessionsData.sessions).map(function(g) { return { value: g, label: '' }; });
      const group = await hivePrompt({ title: 'Change group', label: 'Group (leave empty for none)', defaultValue: s.group || '', confirmLabel: 'Move', options: options });
      if (group !== null && group !== undefined && group !== false && group !== (s.group || '')) sessionsPatch(id, { group: group }, group ? 'Moved to ' + group + '.' : 'Removed from its group.');
    }
    async function sessionsClose(id) {
      const s = (_sessionsData.sessions || []).find(function(x) { return x.id === id; });
      if (!s) return;
      const gone = s.status === SESSIONS_STATUS_FAILED || s.status === SESSIONS_STATUS_ENDED;
      const ok = await hiveConfirm(gone
        ? { title: 'Delete this session?', message: 'Deleting "' + (s.name || s.id) + '" removes its record and files; an ended session can no longer be resumed.', confirmLabel: 'Delete session', danger: true }
        : { title: 'Close this session?', message: 'Closing "' + (s.name || s.id) + '" ends the CLI process for everyone attached and discards its scrollback.', confirmLabel: 'Close session', danger: true });
      if (!ok) return;
      try {
        await sessionsApi('/' + encodeURIComponent(id), 'DELETE');
        delete _sessionsFrames[id];
        _sessionsData.sessions = (_sessionsData.sessions || []).filter(function(x) { return x.id !== id; });
        if (_sessionsSelected === id) _sessionsSelected = sessionsPickSelected(_sessionsData.sessions, '');
        renderSessions();
        showToast('Session closed.', 'success');
      } catch (e) { showToast('Could not close the session: ' + e.message, 'error'); }
    }
    async function sessionsResume(id) {
      try {
        const data = await sessionsApi('/' + encodeURIComponent(id) + '/resume', 'POST', {});
        if (data.session) {
          _sessionsData.sessions = (_sessionsData.sessions || []).map(function(x) { return x.id === data.session.id ? data.session : x; });
          renderSessions();
        }
        showToast('Resuming session…', 'success');
        await loadSessions();
      } catch (e) { showToast('Could not resume the session: ' + e.message, 'error'); }
    }

    // ── Input bar ──
    function sessionsInputChanged(el) {
      const bar = el && el.closest ? el.closest('.sessions-input') : null;
      if (!bar) return;
      const max = Number(_sessionsData.limits && _sessionsData.limits.max_input_bytes) || 0;
      const counter = bar.querySelector('.sessions-input-count');
      if (!counter || !max) return;
      const size = sessionsByteLength(el.value);
      counter.textContent = sessionsFormatBytes(size) + ' / ' + sessionsFormatBytes(max);
      counter.classList.toggle('over', size > max);
    }
    // POST /input answers 202 with {queued: {id, position}, queue}; the text
    // is typed by the server once the CLI prompt is idle.
    async function sessionsPostInput(id, text, submit) {
      const data = await sessionsApi('/' + encodeURIComponent(id) + '/input', 'POST', { text: text, submit: !!submit });
      const s = (_sessionsData.sessions || []).find(function(x) { return x.id === id; });
      if (s && data.queue) { s.queue = data.queue; sessionsRefreshQueueUI(s); }
      return data;
    }
    async function sessionsSend(submit) {
      const s = sessionsSelectedSession();
      if (!s) return;
      const box = document.getElementById('sessions-input-text-' + s.id);
      if (!box) return;
      const check = sessionsCheckInput(box.value, _sessionsData.limits);
      if (!check.ok) { showToast(check.error, 'error'); return; }
      try {
        const data = await sessionsPostInput(s.id, box.value, String(submit) === '1' || submit === true);
        box.value = '';
        sessionsInputChanged(box);
        showToast(sessionsQueuedToast(s.name || s.id, data && data.queued), 'success');
      } catch (e) { showToast('Could not send: ' + e.message, 'error'); }
    }
    async function sessionsCancelQueued(id, queueId) {
      try {
        const data = await sessionsApi('/' + encodeURIComponent(id) + '/input/' + encodeURIComponent(queueId), 'DELETE');
        const s = (_sessionsData.sessions || []).find(function(x) { return x.id === id; });
        if (s && data.queue) { s.queue = data.queue; sessionsRefreshQueueUI(s); }
        showToast('Cancelled the queued send.', 'success');
      } catch (e) { showToast('Could not cancel: ' + e.message, 'error'); loadSessions(); }
    }
    // Take or release the keyboard; the list reload re-attaches the iframe in
    // the new mode (sessionsSyncKeyboardFrame).
    async function sessionsKeyboard(id, action) {
      const s = (_sessionsData.sessions || []).find(function(x) { return x.id === id; });
      try {
        await sessionsApi('/' + encodeURIComponent(id) + '/keyboard', 'POST', { action: action });
        showToast(action === SESSIONS_KEYBOARD_RELEASE ? 'Keyboard released.' : 'You have the keyboard' + (s && s.name ? ' for ' + s.name : '') + '.', 'success');
        await loadSessions();
      } catch (e) { showToast('Could not ' + action + ' the keyboard: ' + e.message, 'error'); }
    }
    async function sessionsFilePicked(el) {
      const s = sessionsSelectedSession();
      const file = el && el.files ? el.files[0] : null;
      if (!s || !file) return;
      const check = sessionsCheckUpload(file, _sessionsData.limits);
      if (!check.ok) { showToast(check.error, 'error'); el.value = ''; return; }
      try {
        const form = new FormData();
        form.append('file', file);
        const data = await sessionsApi('/' + encodeURIComponent(s.id) + '/upload', 'POST', form);
        if (!data.path) throw new Error('the server returned no file path');
        await sessionsPostInput(s.id, data.path, false);
        showToast('Uploaded ' + file.name + '; its path is queued to be typed into the session.', 'success');
      } catch (e) { showToast('Could not attach the file: ' + e.message, 'error'); }
      el.value = '';
    }
    // Registry slot: features add entries to the input bar's "More" menu.
    function sessionsRegisterInputAction(action) {
      if (!action || !action.id || !action.label || typeof action.run !== 'function') return false;
      if (SESSIONS_INPUT_ACTIONS.some(function(a) { return a.id === action.id; })) return false;
      SESSIONS_INPUT_ACTIONS.push(action);
      if (_sessionsLoaded) renderSessions();
      return true;
    }
    function sessionsInputAction(id) {
      const action = SESSIONS_INPUT_ACTIONS.find(function(a) { return a.id === id; });
      const s = sessionsSelectedSession();
      if (!action || !s) return;
      const box = document.getElementById('sessions-input-text-' + s.id);
      action.run({ session: s, role: _sessionsRole, textarea: box, send: function(text, submit) { return sessionsPostInput(s.id, text, submit); } });
    }
    document.addEventListener('DOMContentLoaded', function() {
      loadSessions();
    });
