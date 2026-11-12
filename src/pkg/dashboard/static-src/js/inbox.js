    // ── Inbox: source-agnostic triage of voice recordings and dictations ──
    // Items come from knowledge connectors (plaud, wisprflow, …); each one is
    // already a Knowledge fact. Routing them to spek (RFC + child issues) or
    // dibs happens inside the hive via POST /api/inbox/route.
    const INBOX_POLL_MS = 60000;
    const INBOX_BUSY_POLL_MS = 5000;
    const INBOX_SOURCE_ICONS = { plaud: '🎙️', wisprflow: '🗣️', 'file-drop': '📎', webhook: '🔗' };
    const INBOX_DEST_LABELS = { spek_rfc: 'RFC', dibs: 'dibs' };
    let _inboxCache = null;
    let _inboxSelected = new Set();
    let _inboxShowProcessed = false;
    let _inboxBusyTimer = null;

    function inboxSourceIcon(src) { return INBOX_SOURCE_ICONS[src] || '📥'; }
    function inboxItemKey(it) { return it.source + '/' + it.id; }
    function inboxFmtDuration(ms) {
      const s = Math.round((Number(ms) || 0) / 1000);
      if (s <= 0) return '';
      const m = Math.floor(s / 60);
      return m >= 60 ? Math.floor(m / 60) + 'h ' + (m % 60) + 'm' : m + 'm ' + (s % 60) + 's';
    }
    function inboxRouteBusy(items) {
      return (items || []).some(it => Object.values(it.routes || {}).some(r => r && (r.status === 'queued' || r.status === 'running')));
    }

    async function fetchInbox() {
      try {
        const r = await fetch('/api/inbox');
        if (!r.ok) {
          // Never leave "Loading inbox…" up: a 404/500 becomes a visible error.
          const body = await r.json().catch(() => ({}));
          _inboxCache = { items: [], sources: [], settings: {}, error: body.error || ('HTTP ' + r.status) };
          renderInbox();
          return;
        }
        _inboxCache = await r.json();
        renderInbox();
        clearTimeout(_inboxBusyTimer);
        if (inboxRouteBusy(_inboxCache.items)) _inboxBusyTimer = setTimeout(fetchInbox, INBOX_BUSY_POLL_MS);
      } catch (e) { /* transient — leave last render in place */ }
    }
    dashboardScheduleWork('inbox', fetchInbox).catch(console.error);
    dashboardSetInterval('inbox', fetchInbox, INBOX_POLL_MS);

    function inboxStatePill(dest, st) {
      const label = INBOX_DEST_LABELS[dest] || dest;
      if (!st) return '';
      const status = st.status === 'filed' || st.status === 'delegated' ? 'ok' : (st.status === 'failed' ? 'error' : 'syncing');
      const text = st.status === 'filed' ? label + ' filed' : st.status === 'delegated' ? label + ' handed to agent' : label + ' ' + st.status;
      const title = st.error ? escapeHtml(st.error) : (st.repo ? escapeHtml(st.repo) : '');
      let html = '<span class="kc-pill" data-status="' + status + '" title="' + title + '">' + escapeHtml(text) + '</span>';
      if (st.url) html += ' <a class="inbox-link" href="' + escapeHtml(st.url) + '" target="_blank" rel="noopener">open ↗</a>';
      if (Array.isArray(st.children) && st.children.length) html += ' <span class="muted-xs">' + st.children.length + ' child issue' + (st.children.length === 1 ? '' : 's') + '</span>';
      return html;
    }

    function inboxItemHtml(it) {
      const key = inboxItemKey(it);
      const checked = _inboxSelected.has(key) ? ' checked' : '';
      const when = it.recorded_at && !String(it.recorded_at).startsWith('0001') ? relativeTimeShort(it.recorded_at) : '';
      const dur = inboxFmtDuration(it.duration_ms);
      const meta = [escapeHtml(it.source), when, dur].filter(Boolean).join(' · ');
      const routes = it.routes || {};
      let pills = '';
      for (const d of Object.keys(routes)) pills += inboxStatePill(d, routes[d]) + ' ';
      if (it.dismissed) pills += '<span class="kc-pill" title="Kept as a Knowledge fact only">knowledge only</span>';
      const owner = viewerIsOwner();
      const src = escapeHtml(it.source), id = escapeHtml(it.id);
      let actions = '';
      if (owner) {
        const retry = (d) => routes[d] && routes[d].status === 'failed' ? 'Retry ' : '→ ';
        actions += '<button type="button" class="hv-btn btn-primary btn-sm" data-action="inboxOpenRoute" data-arg0="' + src + '" data-arg1="' + id + '" data-arg2="spek_rfc">' + retry('spek_rfc') + 'RFC…</button>';
        actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxOpenRoute" data-arg0="' + src + '" data-arg1="' + id + '" data-arg2="dibs">' + retry('dibs') + 'dibs…</button>';
        if (!it.dismissed) actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxDismiss" data-arg0="' + src + '" data-arg1="' + id + '" title="Keep it as a Knowledge fact; no RFC, no dibs idea">Knowledge only</button>';
        else actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxUndismiss" data-arg0="' + src + '" data-arg1="' + id + '">Back to inbox</button>';
      }
      if (it.fact_slug) actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxOpenFact" data-arg0="' + escapeHtml(it.fact_slug) + '" data-arg1="' + escapeHtml(it.layer || '') + '" title="This item is already a Knowledge fact">📚 Fact</button>';
      return '<div class="kb-fact-item inbox-item" data-inbox-key="' + escapeHtml(key) + '">' +
        (owner ? '<label class="inbox-check"><input type="checkbox" data-change-action="inboxToggleSelect" data-arg0="' + src + '" data-arg1="' + id + '"' + checked + ' aria-label="Select ' + escapeHtml(it.title) + '"></label>' : '') +
        '<div class="inbox-icon" title="' + src + '">' + inboxSourceIcon(it.source) + '</div>' +
        '<div class="kb-fact-body"><div class="kb-fact-title">' + escapeHtml(it.title || it.id) + '</div>' +
        '<div class="muted-xs">' + meta + '</div>' +
        (it.excerpt ? '<div class="kb-fact-snippet">' + escapeHtml(it.excerpt) + '</div>' : '') +
        (pills ? '<div class="inbox-pills">' + pills + '</div>' : '') +
        '<div class="kc-actions mt-2">' + actions + '</div></div></div>';
    }

    function renderInbox() {
      refreshSectionCardShell('inbox-section');
      const panel = document.getElementById('inbox-panel');
      const badge = document.getElementById('inbox-sidebar-badge');
      const data = _inboxCache;
      if (!panel) return;
      if (!data) { panel.innerHTML = '<div class="muted-note">Loading inbox…</div>'; return; }
      const items = data.items || [];
      const unprocessed = Number(data.unprocessed) || 0;
      if (badge) setSidebarCountBadge(badge, unprocessed, unprocessed + ' unprocessed');
      setSectionSummary('inbox-section', unprocessed + ' to triage · ' + items.length + ' total');
      const owner = viewerIsOwner();
      const sources = (data.sources || []).map(s => {
        const status = s.connected ? 'ok' : (s.configured ? 'error' : '');
        const label = inboxSourceIcon(s.type) + ' ' + escapeHtml(s.type) + (s.connected ? (s.account ? ': ' + escapeHtml(s.account) : ': connected') : s.configured ? ': not connected' : ': not configured');
        return '<span class="kc-pill" data-status="' + status + '">' + label + '</span>';
      }).join(' ');
      const settings = data.settings || {};
      const rules = Array.isArray(settings.rules) ? settings.rules.length : 0;
      let html = '<div class="row-between inbox-toolbar">' +
        '<div class="inbox-sources">' + (sources || '<span class="muted-xs">No voice sources configured.</span>') +
        ' <button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbOpenIntegrations">Connect sources</button></div>' +
        '<div class="kc-actions">' +
        '<label class="muted-xs inbox-toggle"><input type="checkbox" data-change-action="inboxToggleProcessed"' + (_inboxShowProcessed ? ' checked' : '') + '> show processed</label>' +
        (owner ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxOpenSettings" title="Auto-routing rules, default RFC repository, dibs URL">⚙ Auto-routing (' + rules + ')</button>' : '') +
        '</div></div>';
      if (data.error) html += '<div class="kc-error muted-note">' + escapeHtml(data.error) + '</div>';
      if (!settings.spek_ready && owner) html += '<p class="muted-xs">RFC filing needs a <code>spektacular</code> binary on the hive host (<code>inbox.spek.binary</code>) or at least one enabled agent to hand the job to (<code>inbox.spek.agent</code> overrides the default).</p>';
      const shown = items.filter(it => _inboxShowProcessed || !it.processed);
      if (owner && shown.length) {
        const sel = _inboxSelected.size;
        html += '<div class="kc-actions mb-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxSelectAll">' + (sel === shown.length ? 'Clear selection' : 'Select all') + '</button>' +
          (sel ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxDismissSelected">Knowledge only (' + sel + ')</button>' : '') + '</div>';
      }
      if (!shown.length) html += '<div class="muted-note">' + (items.length ? 'Everything is triaged. Toggle “show processed” to see filed and dismissed items.' : 'Nothing yet. Connect Plaud or another voice source under Knowledge → Integrations; new recordings land here after the next sync (a newly added connector starts on the next hive restart).') + '</div>';
      else html += '<div class="kb-fact-list inbox-list">' + shown.map(inboxItemHtml).join('') + '</div>';
      panel.innerHTML = html;
    }

    function inboxToggleProcessed() { _inboxShowProcessed = !_inboxShowProcessed; renderInbox(); }
    function inboxToggleSelect(src, id) {
      const key = src + '/' + id;
      if (_inboxSelected.has(key)) _inboxSelected.delete(key); else _inboxSelected.add(key);
      renderInbox();
    }
    function inboxSelectAll() {
      const shown = ((_inboxCache || {}).items || []).filter(it => _inboxShowProcessed || !it.processed);
      if (_inboxSelected.size === shown.length) _inboxSelected.clear();
      else _inboxSelected = new Set(shown.map(inboxItemKey));
      renderInbox();
    }
    function inboxFind(src, id) { return ((_inboxCache || {}).items || []).find(it => it.source === src && it.id === id); }
    function inboxOpenFact(slug, layer) {
      ocNavigate('knowledge-section');
      kbReadFact(slug, layer || undefined);
    }

    async function inboxPostDismiss(items, undo) {
      const r = await fetch('/api/inbox/dismiss', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ items, undo: !!undo }) });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      return data;
    }
    async function inboxDismiss(src, id) {
      try { await inboxPostDismiss([{ source: src, item_id: id }], false); showToast('Kept as a Knowledge fact only', 'success'); await fetchInbox(); }
      catch (e) { showToast('Dismiss failed: ' + e.message, 'error'); }
    }
    async function inboxUndismiss(src, id) {
      try { await inboxPostDismiss([{ source: src, item_id: id }], true); await fetchInbox(); }
      catch (e) { showToast('Undo failed: ' + e.message, 'error'); }
    }
    async function inboxDismissSelected() {
      const items = Array.from(_inboxSelected).map(k => { const i = k.indexOf('/'); return { source: k.slice(0, i), item_id: k.slice(i + 1) }; });
      if (!items.length) return;
      if (!(await hiveConfirm('Mark ' + items.length + ' item' + (items.length === 1 ? '' : 's') + ' as Knowledge only? They stay searchable as facts; no RFC or dibs idea is created.', 'Knowledge only'))) return;
      try { await inboxPostDismiss(items, false); _inboxSelected.clear(); await fetchInbox(); }
      catch (e) { showToast('Dismiss failed: ' + e.message, 'error'); }
    }

    // Route dialog: destination-specific confirm with a repo picker for RFCs.
    function inboxOpenRoute(src, id, dest) {
      const it = inboxFind(src, id);
      if (!it) return;
      const settings = (_inboxCache || {}).settings || {};
      const repos = (_inboxCache || {}).repos || [];
      const def = settings.spek_repo || repos[0] || '';
      let body = '<p class="muted-note m-0"><strong>' + escapeHtml(it.title || it.id) + '</strong></p>';
      if (dest === 'spek_rfc') {
        const opts = repos.map(r => '<option value="' + escapeHtml(r) + '"' + (r === def ? ' selected' : '') + '>' + escapeHtml(r) + '</option>').join('');
        body += '<p class="muted-note">A hive-run <code>spektacular voice</code> turns the transcript into a spec, then files one RFC issue plus child issues on the chosen repository. The children are picked up by the hive like any other issue.</p>' +
          '<label class="muted-note inbox-field">Repository<select class="field-input field-input-block mt-2" id="inbox-route-repo">' + opts + '</select></label>' +
          (repos.length ? '' : '<p class="kc-error muted-xs">No repositories configured on this hive.</p>');
      } else {
        body += '<p class="muted-note">Posts the title, summary and transcript to dibs as a private draft idea' + (settings.dibs_url ? ' at <code>' + escapeHtml(settings.dibs_url) + '</code>' : '') + '. Provenance links back to <code>' + escapeHtml(src) + '://recordings/' + escapeHtml(id) + '</code>.</p>' +
          (!settings.dibs_url ? '<p class="kc-error muted-xs">dibs is not configured — set the URL under ⚙ Auto-routing and <code>inbox.dibs.token_env</code> in the hive config.</p>' : !settings.dibs_token_set ? '<p class="kc-error muted-xs">dibs token missing — set the environment variable named by <code>inbox.dibs.token_env</code>.</p>' : '');
      }
      body += '<div class="hive-dialog-error" role="alert" id="inbox-route-error"></div>' +
        '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-action="kbCloseModal">Cancel</button>' +
        '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxConfirmRoute" data-arg0="' + escapeHtml(src) + '" data-arg1="' + escapeHtml(id) + '" data-arg2="' + escapeHtml(dest) + '">' + (dest === 'spek_rfc' ? 'File RFC' : 'Send to dibs') + '</button></div>';
      kbShowModal(dest === 'spek_rfc' ? '→ RFC (spektacular)' : '→ dibs', body, { width: '520px' });
    }
    async function inboxConfirmRoute(src, id, dest) {
      const repoEl = document.getElementById('inbox-route-repo');
      const repo = repoEl ? repoEl.value : '';
      const errEl = document.getElementById('inbox-route-error');
      try {
        const r = await fetch('/api/inbox/route', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ source: src, item_id: id, destination: dest, repo }) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        kbCloseModal();
        showToast((INBOX_DEST_LABELS[dest] || dest) + ' queued — the pill updates when it finishes.', 'success');
        await fetchInbox();
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Route failed: ' + e.message, 'error');
      }
    }

    // Settings drawer: default RFC repo, dibs URL, and ordered auto-routing rules.
    let _inboxDraftRules = [];
    function inboxRuleRowHtml(rule, i) {
      const sources = ((_inboxCache || {}).settings || {}).sources || [];
      const srcOpts = ['<option value=""' + (!rule.source ? ' selected' : '') + '>any source</option>'].concat(sources.map(s => '<option value="' + escapeHtml(s) + '"' + (rule.source === s ? ' selected' : '') + '>' + escapeHtml(s) + '</option>')).join('');
      const destOpts = [['spek_rfc', '→ RFC'], ['dibs', '→ dibs'], ['dismiss', 'Knowledge only']].map(([v, l]) => '<option value="' + v + '"' + (rule.destination === v ? ' selected' : '') + '>' + l + '</option>').join('');
      return '<div class="inbox-rule" data-rule-index="' + i + '">' +
        '<select class="field-input" data-rule-field="source" aria-label="Source">' + srcOpts + '</select>' +
        '<input class="kb-search-box" data-rule-field="title_prefix" placeholder="title prefix (RFC:)" value="' + escapeHtml(rule.title_prefix || '') + '" aria-label="Title prefix">' +
        '<input class="kb-search-box" data-rule-field="tag" placeholder="or tag" value="' + escapeHtml(rule.tag || '') + '" aria-label="Tag">' +
        '<select class="field-input" data-rule-field="destination" aria-label="Destination">' + destOpts + '</select>' +
        '<input class="kb-search-box" data-rule-field="repo" placeholder="owner/repo (RFC only)" value="' + escapeHtml(rule.repo || '') + '" aria-label="Repository">' +
        '<button type="button" class="hv-btn btn-icon btn-sm" data-action="inboxRemoveRule" data-arg0="' + i + '" aria-label="Remove rule">✕</button></div>';
    }
    // The same form renders in the Inbox page modal and inline under
    // Settings → Inbox, and both can sit in the DOM at once (the settings
    // tab stays mounted; the closed modal keeps its last body). Every read
    // and write therefore goes through the active form root: the modal
    // while it is open, otherwise the inline section.
    function inboxSettingsRoot() {
      const overlay = document.getElementById('kb-modal-overlay');
      if (overlay && overlay.style.display !== 'none') {
        const modal = overlay.querySelector('.inbox-settings-form');
        if (modal) return modal;
      }
      return document.querySelector('#inbox-settings-inline .inbox-settings-form');
    }
    function inboxSettingsField(root, name) {
      return root ? root.querySelector('[data-inbox-field="' + name + '"]') : null;
    }
    function inboxReadRules(root) {
      root = root || inboxSettingsRoot();
      if (!root) return [];
      return Array.from(root.querySelectorAll('.inbox-settings-rules .inbox-rule')).map(row => {
        const v = f => { const el = row.querySelector('[data-rule-field="' + f + '"]'); return el ? el.value.trim() : ''; };
        return { source: v('source'), title_prefix: v('title_prefix'), tag: v('tag'), destination: v('destination'), repo: v('repo') };
      });
    }
    function inboxRenderRules(root) {
      root = root || inboxSettingsRoot();
      const host = root ? root.querySelector('.inbox-settings-rules') : null;
      if (!host) return;
      host.innerHTML = _inboxDraftRules.length ? _inboxDraftRules.map(inboxRuleRowHtml).join('') : '<p class="muted-xs">No rules — every item waits here for manual triage.</p>';
    }
    function inboxSettingsFormHtml(settings, inline) {
      const buttons = inline
        ? '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxSaveSettings">Save</button></div>'
        : '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-action="kbCloseModal">Cancel</button>' +
          '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxSaveSettings">Save</button></div>';
      return '<div class="inbox-settings-form" data-inbox-form="' + (inline ? 'inline' : 'modal') + '"><p class="muted-note m-0">New items are checked against these rules in order after every connector sync; the first match routes the item automatically. Items that arrived before a rule existed are never re-routed.</p>' +
        '<div class="inbox-settings-grid">' +
        '<label class="muted-note inbox-field">Default RFC repository<input class="kb-search-box mt-2" type="text" data-inbox-field="spek-repo" placeholder="owner/repo" autocomplete="off" value="' + escapeHtml(settings.spek_repo || '') + '"></label>' +
        '<label class="muted-note inbox-field">dibs URL<input class="kb-search-box mt-2" type="text" data-inbox-field="dibs-url" placeholder="https://dibs.example.org" autocomplete="off" value="' + escapeHtml(settings.dibs_url || '') + '"></label></div>' +
        '<p class="muted-xs">The dibs token is read from the environment variable named by <code>inbox.dibs.token_env</code>' + (settings.dibs_token_set ? ' (set)' : ' (not set)') + '.</p>' +
        '<div class="row-between mt-4 mb-2"><div class="section-label m-0">Rules</div><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxAddRule">Add rule</button></div>' +
        '<div class="inbox-settings-rules"></div>' +
        '<div class="hive-dialog-error" role="alert" data-inbox-field="error"></div>' + buttons + '</div>';
    }
    function inboxOpenSettings() {
      const settings = (_inboxCache || {}).settings || {};
      _inboxDraftRules = (settings.rules || []).map(r => Object.assign({}, r));
      kbShowModal('⚙ Inbox auto-routing', inboxSettingsFormHtml(settings, false), { width: '720px' });
      inboxRenderRules();
    }
    // Settings → Inbox: the same form inline, loaded from /api/inbox/settings.
    function renderGovInbox() {
      return '<div class="config-section" data-config-section="Inbox"><h3>Inbox</h3>' +
        '<p class="text-muted fs-base">Default RFC repository, dibs URL and auto-routing rules for voice recordings. ' + yamlHint('inbox.spek.repo') + ' ' + yamlHint('inbox.dibs.url') + ' ' + yamlHint('inbox.rules') + '</p>' +
        '<div id="inbox-settings-inline"><p class="muted-xs">Loading…</p></div></div>';
    }
    async function loadInboxSettingsInline() {
      const host = document.getElementById('inbox-settings-inline');
      if (!host) return;
      try {
        const r = await fetch('/api/inbox/settings');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _inboxCache = Object.assign({}, _inboxCache, { settings: data });
        _inboxDraftRules = (data.rules || []).map(x => Object.assign({}, x));
        host.innerHTML = inboxSettingsFormHtml(data, true);
        inboxRenderRules(host.querySelector('.inbox-settings-form'));
      } catch (e) {
        host.innerHTML = '<p class="kc-error muted-xs">Inbox settings unavailable: ' + escapeHtml(e.message) + '</p>';
      }
    }
    function inboxAddRule() {
      _inboxDraftRules = inboxReadRules();
      _inboxDraftRules.push({ source: '', title_prefix: 'RFC:', tag: '', destination: 'spek_rfc', repo: '' });
      inboxRenderRules();
    }
    function inboxRemoveRule(i) {
      _inboxDraftRules = inboxReadRules();
      _inboxDraftRules.splice(Number(i), 1);
      inboxRenderRules();
    }
    async function inboxSaveSettings() {
      const root = inboxSettingsRoot();
      if (!root) return;
      const errEl = inboxSettingsField(root, 'error');
      const rules = inboxReadRules(root).map(r => { const o = {}; for (const k of Object.keys(r)) if (r[k]) o[k] = r[k]; return o; });
      const payload = { rules, spek_repo: (inboxSettingsField(root, 'spek-repo') || {}).value || '', dibs_url: (inboxSettingsField(root, 'dibs-url') || {}).value || '' };
      try {
        const r = await fetch('/api/inbox/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        if (root.dataset.inboxForm !== 'inline') kbCloseModal();
        showToast('Inbox settings saved', 'success');
        await fetchInbox();
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Save failed: ' + e.message, 'error');
      }
    }

    // Hosted-hive Plaud sign-in completion: Plaud's OAuth client only redirects
    // to localhost:8199, so the operator pastes either the redirect URL the
    // browser landed on or the tokens-mcp.json contents; the hive finishes
    // the login in-pod.
    async function kbPlaudPasteComplete(name) {
      const el = document.getElementById('kb-plaud-paste-' + name);
      const raw = el ? el.value.trim() : '';
      if (!raw) { showToast('Paste the redirect URL or the tokens JSON first.', 'error'); return; }
      const body = raw.startsWith('{') ? { tokens: raw } : { redirect_url: raw };
      try {
        const r = await fetch('/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth/complete', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _kbPlaudAuth[name] = data;
        kbRenderIntegrationsModal();
        showToast(data.connected ? 'Plaud connected' + (data.account ? ' as ' + data.account : '') : 'Plaud sign-in submitted; click Check.', data.connected ? 'success' : 'info');
        fetchInbox();
      } catch (e) {
        showToast('Plaud sign-in failed: ' + e.message, 'error');
      }
    }
    function kbPlaudPasteHtml(name) {
      const n = escapeHtml(name);
      const a = _kbPlaudAuth[name] || {};
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs">Recordings are triaged in the Inbox, not here.</span></div>';
      return '<details class="kb-plaud-paste mt-2"><summary class="muted-xs">Hive runs on a server? Finish sign-in by pasting</summary>' +
        '<p class="muted-xs">Plaud only redirects to <code>localhost:8199</code>. Open the authorization link, approve, then paste the <code>localhost:8199/auth/callback?…</code> URL your browser lands on (within 10 minutes). Alternatively paste the contents of <code>~/.plaud/tokens-mcp.json</code> from a laptop where <code>plaud-mcp</code> is signed in.</p>' +
        '<textarea class="kb-search-box" id="kb-plaud-paste-' + n + '" rows="3" placeholder="http://localhost:8199/auth/callback?code=…  or  {&quot;access_token&quot;: …}"></textarea>' +
        '<div class="kc-actions mt-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbPlaudPasteComplete" data-arg0="' + n + '">Submit</button>' + openInbox + '</div></details>';
    }
