    // ── Vault Management ──

    function kbOpenVaults() {
      kbShowModal('📂 Connected Vaults', '<div id="kb-vaults-content"><div style="text-align:center;padding:var(--sp-7);color:var(--muted)">Loading...</div></div>');
      kbLoadVaults();
    }

    async function kbLoadVaults() {
      const el = document.getElementById('kb-vaults-content');
      if (!el) return;

      try {
        const r = await fetch('/api/knowledge/vaults');
        const vaults = await r.json();

        let html = '';
        if (vaults && vaults.length > 0) {
          html += '<div style="margin-bottom:14px">';
          for (const v of vaults) {
            const tagList = Object.entries(v.tag_counts || {}).sort((a,b) => b[1]-a[1]).slice(0,8).map(([t,c]) => `<span style="background:var(--surface);padding:var(--sp-1) var(--sp-3);border-radius:var(--r-sm);font-size:0.65rem;margin-right:var(--sp-2)">#${escapeHtml(t)} (${c})</span>`).join('');
            html += `<div style="padding:10px;border:1px solid var(--border);border-radius:var(--r);margin-bottom:var(--sp-4)">`;
            html += `<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:var(--sp-3)">`;
            html += `<div><div style="font-size:var(--fs-base);font-weight:600;color:var(--fg)">📂 ${escapeHtml(v.name)}</div>`;
            html += `<div class="muted-xs">${escapeHtml(v.root_dir)} · ${v.pages} pages</div></div>`;
            html += `<div style="display:flex;gap:var(--sp-2)">`;
            html += `<button class="hv-btn btn-secondary nous-btn btn-primary btn-sm" data-action="kbReindexVault" data-arg0="${escapeHtml(v.root_dir)}">↻ Reindex</button>`;
            html += `<button class="hv-btn btn-danger nous-btn btn-sm" data-action="kbDisconnectVault" data-arg0="${escapeHtml(v.root_dir)}">Disconnect</button>`;
            html += '</div></div>';
            if (tagList) html += `<div class="mt-2">${tagList}</div>`;
            html += `<div style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-2)">Last indexed: ${v.last_indexed ? formatDashboardTime(v.last_indexed) : 'never'}</div>`;
            html += '</div>';
          }
          html += '</div>';
        } else {
          html += '<div style="text-align:center;padding:var(--sp-6);color:var(--muted);font-size:0.78rem">No vaults connected. Connect an Obsidian vault or any directory of markdown files.</div>';
        }

        html += '<div style="border-top:1px solid var(--border);padding-top:var(--sp-5);margin-top:var(--sp-4)">';
        html += '<div style="font-size:var(--fs-base);font-weight:600;color:var(--fg);margin-bottom:var(--sp-4)">Connect Vault</div>';
        html += '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:var(--sp-4)">Mount an Obsidian vault or markdown directory into the container (e.g. <code>-v /path/to/vault:/data/vaults/my-vault</code>), then enter the container path below.</div>';
        html += '<div style="display:grid;grid-template-columns:2fr 1fr;gap:var(--sp-4);margin-bottom:var(--sp-4)">';
        html += '<input id="kb-vault-path" type="text" class="kb-search-box" placeholder="/data/vaults/my-obsidian-vault">';
        html += '<input id="kb-vault-name" type="text" class="kb-search-box" placeholder="Display name">';
        html += '</div>';
        html += '<div class="row-end">';
        html += '<button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbConnectVault">Connect</button>';
        html += '</div></div>';

        el.innerHTML = html;
      } catch (e) {
        el.innerHTML = '<div style="color:var(--red);padding:var(--sp-5)">Failed to load vaults: ' + escapeHtml(e.message) + '</div>';
      }
    }

    async function kbConnectVault() {
      const path = document.getElementById('kb-vault-path')?.value?.trim();
      const name = document.getElementById('kb-vault-name')?.value?.trim();
      if (!path) { await hiveAlert('Path is required'); return; }

      try {
        const r = await fetch('/api/knowledge/vaults', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ path, name: name || '' }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to connect vault'); return; }
        kbLoadVaults();
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function kbDisconnectVault(path) {
      if (!(await hiveConfirm('Disconnect vault at ' + path + '?'))) return;
      try {
        const r = await fetch('/api/knowledge/vaults', {
          method: 'DELETE', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ path }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to disconnect'); return; }
        kbLoadVaults();
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function kbReindexVault(path) {
      try {
        const r = await fetch('/api/knowledge/vaults/reindex', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ path }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to reindex'); return; }
        kbLoadVaults();
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function kbSearchVault(name) {
      const el = document.getElementById('kb-fact-list');
      if (!el) return;
      try {
        const r = await fetch('/api/knowledge/vaults/' + encodeURIComponent(name) + '/facts');
        if (!r.ok) { _kbFacts = []; renderKnowledgeFacts(); return; }
        _kbFacts = await r.json();
        _kbServerPaged = false;
        _kbPageOffset = 0;
        _kbHasMore = false;
        renderKnowledgeFacts();
      } catch (e) { _kbFacts = []; renderKnowledgeFacts(); }
    }

    // fetchKnowledgeStats() — deferred to _deferredInit

    // --- Audit Log ---
    const AUDIT_POLL_MS = 30000;
    const AUDIT_PANEL_HEIGHT_KEY = 'hive.audit.panel.height';
    const AUDIT_PANEL_DEFAULT_HEIGHT = 640;
    var _auditEntries = [];
    var _auditSummary = { histogram: [], sensitive_24h: 0, today: 0, last: null };
    var _auditResizeObserver = null;
    var _auditSensitiveOnly = false;
    var _auditRelayOnly = false;
    var _auditPendingSensitiveScroll = false;
    function auditPanelSavedHeight() {
      const n = Number(localStorage.getItem(AUDIT_PANEL_HEIGHT_KEY));
      return Number.isFinite(n) && n >= 320 ? Math.min(n, Math.round(window.innerHeight * 0.8)) : AUDIT_PANEL_DEFAULT_HEIGHT;
    }
    function initAuditPanelResize() {
      const panel = document.getElementById('audit-panel');
      if (!panel) return;
      panel.style.setProperty('--audit-panel-height', auditPanelSavedHeight() + 'px');
      if (_auditResizeObserver || typeof ResizeObserver === 'undefined') return;
      _auditResizeObserver = new ResizeObserver((entries) => {
        const h = Math.round((entries && entries[0] && entries[0].contentRect && entries[0].contentRect.height) || 0);
        if (h >= 320) localStorage.setItem(AUDIT_PANEL_HEIGHT_KEY, String(h));
      });
      _auditResizeObserver.observe(panel);
    }

    /* Rendered size of the audit log's actor face, in CSS pixels — the log is
       the densest table on the dashboard, so its faces are the smallest. */
    const AUDIT_AVATAR_PX = 16;

    // Pure: a decision the hive made as the owner's delegate ("Let hive
    // decide"), as opposed to a person's repo_item_decision_* entry.
    function auditDelegatedDecision(e) { return !!e && String(e.action || '').indexOf('repo_item_decision_delegated') === 0; }
    function renderAuditRow(e) {
      var t = e.ts ? new Date(e.ts) : null;
      var timeStr = t ? formatDashboardDateTime(t) : '—';
      var sensitive = e && e.sensitive === true;
      /* An entry with no actor is attributed to 'ghost', which is a real
         github.com account and not this person — so the placeholder face is
         rendered UNLINKED by passing no username, rather than sending the
         reader to a stranger's profile. */
      var actor = String(e.user || '');
      /* user_name is the server-resolved display name for an opaque OIDC key
         ("ibmid:5500…"); provider-prefixed keys get an unlinked initials tile
         via accessRowAvatar instead of a fabricated github.com avatar/link. */
      var actorLabel = String(e.user_name || e.user || '');
      var face;
      if (!actor) {
        face = avatarImg(ghProfileURL('ghost') + '.png', AUDIT_AVATAR_PX, 'margin-right:4px');
      } else if (actor.indexOf(':') !== -1 || identityKeyProvider(actor) !== 'github') {
        face = accessRowAvatar(actor, actorLabel, AUDIT_AVATAR_PX);
      } else {
        face = linkedAvatar(actor, AUDIT_AVATAR_PX, actor, '', 'margin-right:4px');
      }
      var sensitiveTag = sensitive ? '<span class="audit-entry-sensitive-tag" title="Server-classified sensitive audit action">sensitive</span>' : '';
      var delegatedTag = auditDelegatedDecision(e) ? '<span class="audit-entry-sensitive-tag audit-entry-delegated-tag" title="Decided by the hive as the owner\'s delegate (owner-delegate policy), not by a person">delegated</span>' : '';
      return '<tr class="audit-entry' + (sensitive ? ' audit-entry-sensitive' : '') + '" data-audit-sensitive="' + (sensitive ? '1' : '0') + '" style="border-bottom:1px solid var(--terminal-line)">' +
        '<td style="padding:var(--sp-2) var(--sp-4);white-space:nowrap;color:var(--terminal-muted)">' + timeStr + '</td>' +
        '<td class="py-2-px-4" title="' + esc(actor) + '">' + face + esc(actorLabel || '—') + '</td>' +
        '<td style="padding:var(--sp-2) var(--sp-4);font-weight:600">' + esc(e.action||'—') + sensitiveTag + delegatedTag + '</td>' +
        '<td class="py-2-px-4">' + esc(e.agent||'—') + '</td>' +
        '<td style="padding:var(--sp-2) var(--sp-4);color:var(--terminal-muted)">' + esc(e.detail||'') + '</td>' +
        '</tr>';
    }

    function renderAuditTable(entries) {
      refreshSectionCardShell('audit-section');
      var panel = document.getElementById('audit-panel');
      if (!panel) return;
      initAuditPanelResize();
      // Patched in place (#12512) so the 30s poll keeps the panel's own
      // scroll offset and the empty/loaded states swap without a rebuild; the
      // panel's height is fixed by --audit-panel-height either way.
      if (!entries.length) {
        morphInnerHtml(panel, '<div style="color:var(--terminal-muted);font-size:var(--fs-base);padding:var(--sp-4)">' + (_auditSensitiveOnly ? 'No sensitive audit entries in the last 24h' : (_auditRelayOnly ? 'No relay_ audit entries loaded' : 'No audit entries yet')) + '</div>');
        return;
      }
      morphInnerHtml(panel, '<table style="width:100%;border-collapse:collapse;font-size:0.78rem">' +
        '<thead><tr style="color:var(--terminal-muted);font-size:0.7rem;border-bottom:1px solid var(--terminal-line)"><th style="text-align:left;padding:var(--sp-2) var(--sp-4)">Time</th><th style="text-align:left;padding:var(--sp-2) var(--sp-4)">User</th><th style="text-align:left;padding:var(--sp-2) var(--sp-4)">Action</th><th style="text-align:left;padding:var(--sp-2) var(--sp-4)">Agent</th><th style="text-align:left;padding:var(--sp-2) var(--sp-4)">Details</th></tr></thead>' +
        '<tbody>' + entries.map(renderAuditRow).join('') + '</tbody></table>');
    }

    function parseAuditQuery(q) {
      q = q.trim();
      if (q.length > 2 && q[0] === '/' && q[q.length - 1] === '/') {
        try { return new RegExp(q.slice(1, -1), 'i'); } catch(e) { return null; }
      }
      if (q.length > 3 && q[0] === '/' && q.indexOf('/') > 0) {
        var lastSlash = q.lastIndexOf('/');
        var flags = q.slice(lastSlash + 1);
        try { return new RegExp(q.slice(1, lastSlash), flags); } catch(e) { return null; }
      }
      return q.toLowerCase();
    }

    function filterAuditLog() {
      var raw = (document.getElementById('audit-search').value || '').trim();
      var countEl = document.getElementById('audit-match-count');
      var chip = document.getElementById('audit-sensitive-filter');
      if (chip) {
        chip.setAttribute('aria-pressed', _auditSensitiveOnly ? 'true' : 'false');
        chip.textContent = _auditSensitiveOnly ? 'Sensitive only ×' : 'Sensitive only';
        chip.title = _auditSensitiveOnly ? 'Clear sensitive-only audit filter' : 'Show only sensitive audit entries from the last 24h';
      }
      var relayChip = document.getElementById('audit-relay-filter');
      if (relayChip) {
        relayChip.setAttribute('aria-pressed', _auditRelayOnly ? 'true' : 'false');
        relayChip.textContent = _auditRelayOnly ? 'relay_ ×' : 'relay_';
        relayChip.title = _auditRelayOnly ? 'Clear relay_ audit filter' : 'Show only contributor relay (relay_*) audit entries';
      }
      var base = _auditRelayOnly ? _auditEntries.filter(auditEntryIsRelay) : _auditEntries;
      if (!raw) {
        renderAuditTable(base);
        if (countEl) countEl.textContent = base.length ? (_auditSensitiveOnly ? base.length + ' sensitive / 24h' : base.length + (_auditRelayOnly ? ' relay_ entries' : ' entries')) : '';
        scrollPendingAuditSensitiveEntry();
        return;
      }
      var query = parseAuditQuery(raw);
      var terms = typeof query === 'string' ? query.split(/\s+/).filter(Boolean) : [];
      var filtered = base.filter(function(e) {
        var haystack = [e.ts || '', e.user || '', e.action || '', e.agent || '', e.detail || ''].join(' ');
        if (query instanceof RegExp) return query.test(haystack);
        var lower = haystack.toLowerCase();
        return terms.some(function(t) { return lower.includes(t); });
      });
      renderAuditTable(filtered);
      if (countEl) countEl.textContent = filtered.length + ' / ' + base.length + (_auditSensitiveOnly ? ' sensitive / 24h' : '') + (_auditRelayOnly ? ' relay_' : '');
      scrollPendingAuditSensitiveEntry();
    }

    function scrollPendingAuditSensitiveEntry() {
      if (!_auditPendingSensitiveScroll) return;
      requestAnimationFrame(function() {
        var panel = document.getElementById('audit-panel');
        var row = panel && panel.querySelector('.audit-entry-sensitive');
        if (!row) return;
        _auditPendingSensitiveScroll = false;
        row.scrollIntoView({ block: 'center', behavior: 'smooth' });
      });
    }

    // Contributor relay rows (relay_connected, relay_task_assigned, ...) share
    // the relay_ action prefix; the chip narrows the loaded page client-side.
    function auditEntryIsRelay(e) {
      return String((e && e.action) || '').indexOf('relay_') === 0;
    }

    function toggleAuditRelayFilter() {
      _auditRelayOnly = !_auditRelayOnly;
      filterAuditLog();
    }

    function toggleAuditSensitiveFilter() {
      _auditSensitiveOnly = !_auditSensitiveOnly;
      _auditPendingSensitiveScroll = _auditSensitiveOnly;
      fetchAuditLog();
    }

    function showAuditSensitiveEntries() {
      if (isSectionCollapsed('audit-section')) toggleSection('audit-section', { collapsed: false });
      _auditSensitiveOnly = true;
      _auditPendingSensitiveScroll = true;
      fetchAuditLog();
    }

    var AUDIT_SEARCHES_KEY = 'hive-audit-saved-searches';
    function loadAuditSearches() {
      try { return JSON.parse(localStorage.getItem(AUDIT_SEARCHES_KEY) || '[]'); } catch(e) { return []; }
    }
    function renderAuditSearchDatalist() {
      var dl = document.getElementById('audit-saved-searches');
      if (!dl) return;
      dl.innerHTML = loadAuditSearches().map(function(s) { return '<option value="' + esc(s) + '">'; }).join('');
    }
    function saveAuditSearch() {
      var q = (document.getElementById('audit-search').value || '').trim();
      if (!q) return;
      var saved = loadAuditSearches();
      if (saved.indexOf(q) !== -1) { showToast('Search already saved', 'info'); return; }
      saved.unshift(q);
      var MAX_SAVED_SEARCHES = 20;
      if (saved.length > MAX_SAVED_SEARCHES) saved = saved.slice(0, MAX_SAVED_SEARCHES);
      localStorage.setItem(AUDIT_SEARCHES_KEY, JSON.stringify(saved));
      renderAuditSearchDatalist();
      showToast('Search saved', 'success');
    }
    function clearAuditSearches() {
      localStorage.removeItem(AUDIT_SEARCHES_KEY);
      renderAuditSearchDatalist();
      showToast('Saved searches cleared', 'info');
    }
    renderAuditSearchDatalist();

    async function fetchAuditLog() {
      try {
        var resp = await fetch('/api/audit' + (_auditSensitiveOnly ? '?sensitive=1' : ''));
        if (!resp.ok) return;
        var data = await resp.json();
        _auditEntries = capDashboardArray(data.entries || [], DASHBOARD_LOG_CAP);
        _auditSummary = {
          histogram: Array.isArray(data.histogram) ? data.histogram : [],
          sensitive_24h: Number(data.sensitive_24h) || 0,
          today: Number(data.today) || 0,
          last: data.last || null
        };
        window._auditSummary = _auditSummary;
        filterAuditLog();
        setSectionSummary('audit-section', _auditSummary.today + ' events today', auditEntryTooltip(_auditSummary.last));
      } catch(e) {}
    }
    // fetchAuditLog() — deferred to _deferredInit

    // --- Review queue (GET /api/review/queue) — #9607 ---
    // UI-only view over the deterministic PR review queue backend (#9598,
    // #9590): every open PR across the governed repos in one ranked order with
    // the reasons for each position, paged like the API (limit/offset/has_more).
    const REVIEW_QUEUE_POLL_MS = 30000;
    const REVIEW_QUEUE_PAGE_SIZE = 25;
    var _reviewQueueOffset = 0;
    var _reviewQueueLimit = REVIEW_QUEUE_PAGE_SIZE;
    var _reviewQueueTotal = 0;
    var _reviewQueueHasMore = false;
    window._reviewQueueHistory = window._reviewQueueHistory || [];

