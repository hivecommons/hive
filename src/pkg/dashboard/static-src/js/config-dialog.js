    function openConfigDialog(type, agentName, initialTabOverride, initialSearch) {
      const isCreate = type === 'agent' && !agentName;
      _configModalOpen = true;
      // promptTemplateDirty/Value hold the Prompt Template tab's pending edit
      // (it has its own endpoint, so it is not a `dirty` section). Reset on every
      // open and on close, so Cancel/X/Escape discard it like any other field.
      _configState = { type, agent: agentName || null, tab: null, data: {}, dirty: {}, initialSearch: initialSearch || '', isCreate, initialTabOverride: initialTabOverride || null, promptTemplateDirty: false, promptTemplateValue: '' };
      document.getElementById('config-overlay').classList.remove('hidden');
      hiveRememberModalOpen(document.getElementById('config-overlay'));
      const title = type === 'governor' ? 'Settings' : isCreate ? 'Create Agent' : `${agentName || 'Agent'} Configuration`;
      document.querySelector('.config-title').textContent = title;
      setupSettingsSearchControls(type);
      configRailBind();
      const tabs = type === 'governor' ? GOVERNOR_CONFIG_TABS : (isCreate ? AGENT_CREATE_TABS : AGENT_CONFIG_TABS);
      const tabsEl = document.getElementById('config-tabs');
      tabsEl.innerHTML = tabs.map((t, i) =>
        `<div class="config-tab${i === 0 ? ' active' : ''}" data-tab="${t}" data-action="switchConfigTab">${t}</div>`
      ).join('');
      const saveBtn = document.querySelector('.config-save');
      if (saveBtn) saveBtn.textContent = isCreate ? 'Create & close' : 'Save & close';
      if (isCreate) {
        _configState.data = { general: { clearOnKick: true, restartStrategy: 'immediate', staleTimeout: 1200 }, cadences: {} };
        switchConfigTab(tabs[0]);
      } else {
        const configKey = type === 'governor' ? '__governor__' : agentName;
        const savedTab = _configTabMemory[configKey];
        let overrideTab = _configState.initialTabOverride;
        if (type === 'governor' && overrideTab === 'Access') overrideTab = 'Security';
        const rememberedTab = type === 'governor' && savedTab === 'Access' ? 'Security' : savedTab;
        const initialTab = (overrideTab && tabs.includes(overrideTab)) ? overrideTab : (rememberedTab && tabs.includes(rememberedTab) ? rememberedTab : tabs[0]);
        document.getElementById('config-body').innerHTML = '<div style="text-align:center;padding:40px;color:var(--muted)">Loading…</div>';
        loadConfigData(type, agentName).then((current) => {
          // A superseded or cancelled load must not paint this dialog.
          if (!_configModalOpen || !current) return;
          // Nothing loaded: say so, and never render a tab from the empty
          // placeholder (#7405).
          if (_configState.loadError) { renderConfigLoadError(); return; }
          const dn = (_configState.data.general || {}).displayName;
          if (dn && type !== 'governor') {
            document.querySelector('.config-title').textContent = `${dn} Configuration`;
          }
          try {
            switchConfigTab(initialTab);
            if (type === 'governor' && _configState.initialSearch) settingsSearchPrefill(_configState.initialSearch);
          } catch (err) {
            document.getElementById('config-body').innerHTML =
              '<div style="text-align:center;padding:40px;color:var(--red)">Failed to render tab: ' + err.message + '</div>';
            showToast('Config render error: ' + err.message, 'error');
          }
        });
      }
    }

    function closeConfigDialog() {
      _configModalOpen = false;
      // Invalidate any in-flight load so a late reply cannot repopulate a
      // dialog the operator has already dismissed.
      _configLoadSeq++;
      if (_configLoadAbort) { _configLoadAbort.abort(); _configLoadAbort = null; }
      _configState = { type: null, agent: null, tab: null, data: {}, dirty: {}, promptTemplateDirty: false, promptTemplateValue: '' };
      setupSettingsSearchControls(null);
      document.getElementById('config-overlay').classList.add('hidden');
      if (location.hash.startsWith('#config/')) history.replaceState(null, '', window.hiveURLWithHash ? window.hiveURLWithHash('') : location.pathname);
    }

    function handleConfigHash() {
      const hash = location.hash;
      if (!hash.startsWith('#config/')) return;
      const parts = hash.slice(8).split('/').map(decodeURIComponent);
      if (parts.length < 1) return;
      const target = parts[0];
      const tab = parts[1] || null;
      if (target === 'governor') {
        openConfigDialog('governor', null, tab, parts[2] || '');
      } else {
        openConfigDialog('agent', target, tab);
      }
    }
    window.addEventListener('hashchange', () => { if (location.hash.startsWith('#config/')) handleConfigHash(); });
    setTimeout(() => { if (location.hash.startsWith('#config/')) handleConfigHash(); }, 1000);
    // #plan=<epicId> deep-links into the plan review — it is the link the
    // mirrored issue checklist carries back to the dashboard (hivecommons/hive#8011).
    function handlePlanHash() {
      const hash = location.hash;
      if (!hash.startsWith('#plan=')) return;
      const epicID = decodeURIComponent(hash.slice(6));
      if (epicID && typeof openPlanReview === 'function') openPlanReview(epicID);
    }
    window.addEventListener('hashchange', () => { if (location.hash.startsWith('#plan=')) handlePlanHash(); });
    setTimeout(() => { if (location.hash.startsWith('#plan=')) handlePlanHash(); }, 1000);

    function hiveOverlayIsOpen(el) {
      if (!el) return false;
      if (el.hidden) return false;
      if (el.classList && el.classList.contains('hidden')) return false;
      if (el.classList && el.classList.contains('hive-chat-shortcuts') && !el.classList.contains('open')) return false;
      if (el.id === 'oc-drawer-backdrop') return el.classList && el.classList.contains('open');
      const disp = el.style.display || getComputedStyle(el).display;
      return disp !== 'none';
    }
    function hiveOverlayRank(el, fallback) {
      if (!el) return fallback || 0;
      const z = parseInt(getComputedStyle(el).zIndex || '', 10);
      return Number.isFinite(z) ? z : (fallback || 0);
    }
    let _hiveModalSequence = 0;
    function hiveRememberModalOpen(el) {
      if (!el) return el;
      // A modal covers every help-tooltip trigger; never leave a tooltip floating over it.
      if (typeof hideConfigTooltip === 'function') hideConfigTooltip();
      _hiveModalSequence++;
      if (!el.dataset) el.dataset = {};
      el.dataset.hiveModalSequence = String(_hiveModalSequence);
      return el;
    }
    function hiveModalOrder(el, fallback) {
      const seq = el && el.dataset ? parseInt(el.dataset.hiveModalSequence || '', 10) : NaN;
      return Number.isFinite(seq) ? seq : (fallback || 0);
    }
    function hiveCloseGHUserMenu() {
      const menu = document.getElementById('oc-gh-user-menu');
      if (menu) menu.hidden = true;
      if (typeof hiveSyncGHUserMenuTrigger === 'function') hiveSyncGHUserMenuTrigger();
    }
    function hiveDismissTopOverlay() {
      const candidates = [];
      function add(el, close, fallbackRank) {
        if (!hiveOverlayIsOpen(el)) return;
        candidates.push({ el: el, close: close, rank: hiveOverlayRank(el, fallbackRank), order: hiveModalOrder(el, candidates.length) });
      }
      if (typeof configTooltipIsOpen === 'function' && configTooltipIsOpen()) {
        candidates.push({ el: document.getElementById(CONFIG_TOOLTIP_PORTAL_ID), close: hideConfigTooltip, rank: 40000, order: candidates.length });
      }
      const installHintBtn = document.getElementById('gh-app-install-id-hint-btn');
      if (installHintBtn && installHintBtn.getAttribute('aria-expanded') === 'true') {
        candidates.push({ el: installHintBtn, close: function() { installHintBtn.setAttribute('aria-expanded', 'false'); installHintBtn.focus(); }, rank: 30000, order: candidates.length });
      }
      if (repoPillFilterActive() && document.activeElement && document.activeElement.closest && document.activeElement.closest('#repos-section')) {
        candidates.push({ el: document.activeElement, close: clearRepoPillFilter, rank: 30000, order: candidates.length });
      }
      const versionMenu = document.getElementById('oc-version-menu');
      add(versionMenu, function() { closeVersionMenu(); const chip = document.getElementById('oc-version-chip'); if (chip) chip.focus(); }, 30000);
      const avatarMenu = document.getElementById('oc-gh-user-menu');
      add(avatarMenu, hiveCloseGHUserMenu, 30000);
      add(document.getElementById('nps-survey-card'), dismissNPSSurvey, 30000);
      add(document.getElementById('hiveChatShortcuts'), function() { document.getElementById('hiveChatShortcuts').classList.remove('open'); }, 20001);
      document.querySelectorAll('.hive-dialog-overlay').forEach(function(el) { add(el, function() { if (typeof el.hiveDialogClose === 'function') el.hiveDialogClose(); else el.remove(); }, 20000); });
      add(document.getElementById('acmm-dialog-overlay'), acmmCloseDialog, 10000);
      add(document.getElementById('feedback-overlay'), closeFeedbackModal, 10000);
      add(document.getElementById('welcome-overlay'), closeWelcomeDialog, 10000);
      add(document.getElementById('config-overlay'), closeConfigDialog, 10000);
      add(document.getElementById('overview-chart-settings-overlay'), closeOverviewChartSettings, 10000);
      add(document.getElementById('activity-monitor-settings-overlay'), closeStageTrackerSettings, 10000);
      add(document.getElementById('activity-monitor-labels-overlay'), closeStageTrackerLabelDialog, 10000);
      add(document.getElementById('acmm-overlay'), closeACMMDialog, 10000);
      const nousOverlay = document.getElementById('nous-config-overlay');
      add(nousOverlay, function() { if (typeof closeNousConfig === 'function') closeNousConfig(); else nousOverlay.style.display = 'none'; }, 1000);
      const kbOverlay = document.getElementById('kb-modal-overlay');
      add(kbOverlay, function() { if (typeof kbCloseModal === 'function') kbCloseModal(); else kbOverlay.style.display = 'none'; }, 1000);
      const planOverlay = document.getElementById('plan-modal-overlay');
      add(planOverlay, function() { if (typeof planCloseModal === 'function') planCloseModal(); else planOverlay.style.display = 'none'; }, 1000);
      const ghAuth = document.getElementById('gh-auth-modal-overlay');
      add(ghAuth, function() { if (typeof cancelGHLogin === 'function') cancelGHLogin(); else ghAuth.style.display = 'none'; }, 9999);
      const ghSetup = document.getElementById('gh-setup-overlay');
      add(ghSetup, function() { ghSetup.style.display = 'none'; }, 9999);
      add(document.getElementById('oc-drawer-backdrop'), function() { if (typeof window.hiveCloseMobileDrawer === 'function') window.hiveCloseMobileDrawer(); }, 100);
      const chatPanel = document.getElementById('hiveChatPanel');
      if (chatPanel && chatPanel.classList.contains('open')) {
        candidates.push({ el: chatPanel, close: function() { chatPanel.classList.remove('open'); const fab = document.getElementById('hiveChatFab'); if (fab) fab.classList.remove('open'); }, rank: 50, order: candidates.length });
      }
      if (!candidates.length) return false;
      candidates.sort(function(a, b) { return (b.rank - a.rank) || (b.order - a.order); });
      candidates[0].close();
      return true;
    }
    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      if (e.isComposing) return;
      const tag = e.target && e.target.tagName ? e.target.tagName.toLowerCase() : '';
      if (tag === 'select') return;
      if (!hiveDismissTopOverlay()) return;
      e.preventDefault();
      e.stopPropagation();
      if (typeof e.stopImmediatePropagation === 'function') e.stopImmediatePropagation();
    }, true);

    function openAddAgentDialog() {
      openConfigDialog('agent', null);
    }

    // ── ACMM Level Selector ──
    let _acmmOpen = false;
    let _acmmPacks = null;

    async function openACMMDialog() {
      _acmmOpen = true;
      document.getElementById('acmm-overlay').classList.remove('hidden');
      hiveRememberModalOpen(document.getElementById('acmm-overlay'));
      document.getElementById('acmm-apply-error').style.display = 'none';

      // Restate the current level in the title so users arriving from any
      // entry point (badge, config dialog, eval section) see where they are.
      const currentEl = document.getElementById('acmm-dialog-current');
      if (currentEl) {
        const lvl = window._lastStatus ? (window._lastStatus.acmmLevel || 0) : 0;
        currentEl.textContent = '(current: ' + (lvl > 0 ? acmmLevelLabel(lvl) : 'not set') + ')';
      }

      if (!_acmmPacks) {
        try {
          const res = await fetch('/api/packs');
          _acmmPacks = await res.json();
        } catch (err) {
          document.getElementById('acmm-packs-list').innerHTML = '<div class="text-muted">Failed to load packs</div>';
          return;
        }
      }
      renderACMMPacks();
    }

    function closeACMMDialog() {
      _acmmOpen = false;
      document.getElementById('acmm-overlay').classList.add('hidden');
    }

    // Shared bee-to-hive animation markup (the .wb-* stage from the Getting
    // Started banner). Returned as a string so the welcome dialog and the ACMM
    // "Applying Level…" overlay render the identical animation and stay in sync.
    // All CSS lives in the .wb-* rules in <head>; this only emits the DOM.
    function beeStageHtml() {
      return '' +
        '<svg class="wb-hive" viewBox="0 0 80 92" width="42" height="48">' +
          '<path d="M40 2 v8" stroke="#8a5a30" stroke-width="3" fill="none"/>' +
          '<g class="wb-coils">' +
            '<rect x="30" y="10" width="20" height="9"  rx="4.5"/>' +
            '<rect x="24" y="18" width="32" height="10" rx="5"/>' +
            '<rect x="18" y="27" width="44" height="11" rx="5.5"/>' +
            '<rect x="13" y="37" width="54" height="12" rx="6"/>' +
            '<rect x="11" y="48" width="58" height="12" rx="6"/>' +
            '<rect x="13" y="59" width="54" height="12" rx="6"/>' +
            '<rect x="18" y="70" width="44" height="11" rx="5.5"/>' +
            '<rect x="26" y="80" width="28" height="10" rx="5"/>' +
          '</g>' +
          '<ellipse cx="40" cy="64" rx="7.5" ry="9" fill="#2a1a06"/>' +
          '<ellipse cx="40" cy="62" rx="7.5" ry="7" fill="#120c04"/>' +
        '</svg>' +
        '<div class="wb-flyer"><div class="wb-facing"><div class="wb-trail"><i></i><i></i><i></i></div><div class="wb-bob"><span class="wb-bee">&#x1F41D;</span></div></div></div>';
    }

    function showACMMLoading(message) {
      let el = document.getElementById('acmm-loading-overlay');
      if (!el) {
        el = document.createElement('div');
        el.id = 'acmm-loading-overlay';
        el.style.cssText = 'position:absolute;inset:0;z-index:100;background:rgba(0,0,0,0.7);display:flex;align-items:center;justify-content:center;border-radius:12px';
        const inner = document.createElement('div');
        inner.style.cssText = 'text-align:center;color:#fff';
        // Bee-to-hive animation (shared with the Getting Started banner) above
        // the level label. The .wb-stage sets width/height so the bee's flight
        // bounds are defined; slightly narrower than the welcome banner to suit
        // the compact overlay.
        inner.innerHTML = '<div class="wb-stage acmm-loading-stage" aria-hidden="true" style="width:220px;height:64px;margin:0 auto 12px">' + beeStageHtml() + '</div><div id="acmm-loading-text" style="font-size:0.9rem;font-weight:600"></div>';
        el.appendChild(inner);
        document.querySelector('#acmm-overlay .config-modal').style.position = 'relative';
        document.querySelector('#acmm-overlay .config-modal').appendChild(el);
      }
      document.getElementById('acmm-loading-text').textContent = message || 'Switching level…';
      el.style.display = 'flex';
    }

    function hideACMMLoading() {
      const el = document.getElementById('acmm-loading-overlay');
      if (el) el.style.display = 'none';
    }

    // Per-level ACMM colors, index = level (0 = unset, 1..6 = L1..L6). Each hue
    // is deliberately far from its neighbors so adjacent levels never read as the
    // same pill color: grey → purple → teal → green → orange → red → crimson.
    const ACMM_LEVEL_COLORS = ['var(--status-neutral)', 'var(--acmm-level-1)', 'var(--acmm-level-2)', 'var(--acmm-level-3)', 'var(--acmm-level-4)', 'var(--acmm-level-5)', 'var(--acmm-level-6)'];
    const acmmLevelColor = level => ACMM_LEVEL_COLORS[level] || 'var(--status-info)';

    function renderACMMPacks() {
      // Level 6 is the only level that merges unattended, so its card links the
      // operating guide (hivecommons/hive#10516). Declared locally because tests
      // evaluate this function in isolation.
      const guide = { level: 6, url: 'https://docs.hivecommons.dev/docs/hive/running-at-level-6' };
      const container = document.getElementById('acmm-packs-list');
      const currentLevel = window._lastStatus ? (window._lastStatus.acmmLevel || 0) : 0;

      container.innerHTML = (_acmmPacks || []).map(p => {
        const isCurrent = p.level === currentLevel;
        const color = acmmLevelColor(p.level);
        const agentList = (p.agents || []).map(a =>
          `<div style="display:flex;align-items:center;gap:var(--sp-3);padding:3px 0;font-size:var(--fs-sm)">
            <span style="font-size:1rem">${esc(a.emoji || '🤖')}</span>
            <span style="font-weight:600;color:${esc(a.color || '#999')}">${esc(a.displayName || a.name)}</span>
            <span style="color:var(--muted);flex:1">${esc(a.description || '').substring(0, 80)}${(a.description || '').length > 80 ? '…' : ''}</span>
          </div>`
        ).join('');

        const knowledgeNotes = (p.agents || []).filter(a => a.knowledgeUse).map(a =>
          `<div style="font-size:0.7rem;color:var(--muted);padding:var(--sp-1) var(--sp-0)">
            <span style="font-weight:600">${esc(a.emoji || '')} ${esc(a.displayName || a.name)}</span>: ${esc(a.knowledgeUse || '').substring(0, 120)}${(a.knowledgeUse || '').length > 120 ? '…' : ''}
          </div>`
        ).join('');

        const interactionNotes = (p.agents || []).filter(a => a.interactions).map(a =>
          `<div style="font-size:0.7rem;color:var(--muted);padding:var(--sp-1) var(--sp-0)">
            <span style="font-weight:600">${esc(a.emoji || '')} ${esc(a.displayName || a.name)}</span>: ${esc(a.interactions || '')}
          </div>`
        ).join('');

        return `<div style="border:2px solid ${isCurrent ? color : 'var(--line-strong)'};border-radius:10px;padding:var(--sp-6);${isCurrent ? 'background:var(--surface);box-shadow:0 0 12px ' + color + '40' : ''}">
          <div style="display:flex;align-items:center;gap:10px;margin-bottom:var(--sp-4)">
            <span style="font-size:var(--fs-2xl);font-weight:800;color:${color};min-width:32px">L${p.level}</span>
            <div style="flex:1">
              <div style="font-weight:700;font-size:0.9rem">${esc(p.name)} ${isCurrent ? '<span style="font-size:0.7rem;background:' + color + ';color:#fff;padding:var(--sp-1) var(--sp-4);border-radius:8px;margin-left:var(--sp-3)">CURRENT</span>' : ''}</div>
              <div style="font-size:0.78rem;color:var(--muted);margin-top:var(--sp-1)">${esc(p.description || '')}</div>
              ${p.level === guide.level ? '<div class="mt-2 fs-sm"><a class="text-accent" href="' + guide.url + '" target="_blank" rel="noopener">Read the Level 6 operating guide ↗</a> before switching: unattended merging needs real required checks and a tested merge stop.</div>' : ''}
            </div>
            <div style="text-align:right;min-width:80px">
              <div style="font-size:0.7rem;color:var(--muted)">${p.agentCount} agent${p.agentCount !== 1 ? 's' : ''}</div>
              ${!isCurrent && p.level > 0 ? '<button class="hv-btn btn-primary btn-sm" data-action="applyACMMPack" data-arg0="' + p.level + '" data-arg-types="j" style="margin-top:var(--sp-2);padding:6px 14px;background:' + color + ';color:#fff;border:none;border-radius:var(--r);font-size:var(--fs-sm);font-weight:600;cursor:pointer">Apply</button>' : ''}
            </div>
          </div>
          ${p.governor ? '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:var(--sp-3)"><span style="font-weight:600">Governor:</span> ' + esc(p.governor.modes || 'none') + ' · <span style="font-weight:600">Merge:</span> ' + esc(p.governor.mergePolicy || 'manual') + '</div>' : ''}
          ${agentList ? '<details class="mt-3"><summary style="font-size:var(--fs-sm);font-weight:600;cursor:pointer;color:var(--fg)">Agents (' + p.agentCount + ')</summary><div style="margin-top:var(--sp-3);padding-left:var(--sp-2)">' + agentList + '</div></details>' : ''}
          ${interactionNotes ? '<details class="mt-2"><summary style="font-size:0.72rem;font-weight:600;cursor:pointer;color:var(--muted)">Agent Interactions</summary><div style="margin-top:var(--sp-2);padding-left:var(--sp-2)">' + interactionNotes + '</div></details>' : ''}
          ${knowledgeNotes ? '<details class="mt-2"><summary style="font-size:0.72rem;font-weight:600;cursor:pointer;color:var(--muted)">Knowledge Base Usage</summary><div style="margin-top:var(--sp-2);padding-left:var(--sp-2)">' + knowledgeNotes + '</div></details>' : ''}
        </div>`;
      }).join('');
    }

    async function applyACMMPack(level) {
      const errEl = document.getElementById('acmm-apply-error');
      errEl.style.display = 'none';

      const pack = (_acmmPacks || []).find(p => p.level === level);
      if (!pack) return;

      if (!(await hiveConfirm(`Apply ACMM Level ${level} (${pack.name})?\n\nThis will:\n• Create ${pack.agentCount} agent(s) (existing agents with the same name are skipped)\n• Pause agents not in this level's pack\n• Resume agents that are in this level's pack${level === 6 ? '\n\nLevel 6 merges unattended. Read the operating guide first: https://docs.hivecommons.dev/docs/hive/running-at-level-6' : ''}`))) return;

      showACMMLoading('Applying Level ' + level + '…');
      try {
        const res = await fetch(`/api/packs/${level}/apply`, { method: 'POST' });
        const data = await res.json();
        hideACMMLoading();
        if (!res.ok) {
          errEl.textContent = data.error || 'Failed to apply pack';
          errEl.style.display = 'block';
          return;
        }
        const created = (data.created || []).length;
        const skipped = (data.skipped || []).length;
        const governorChanges = formatGovernorChanges(data.governor_changes);
        closeACMMDialog();
        // Render the level the SERVER confirmed, never the one we asked for
        // (#5492). We only reach here on a 2xx, so the write is committed; the
        // body is authoritative. Falling back to `level` keeps an older spoke
        // that omits the field working.
        const confirmedLevel = (data && Number.isFinite(Number(data.level))) ? Number(data.level) : level;
        // Drop any status snapshot built before this apply, so the async
        // rebuild's predecessor cannot repaint the previous level (#4348).
        noteStatusMutation(data.minStatusSeq);
        window._acmmOverride = { level: confirmedLevel, packAgents: data.packAgents || [] };
        if (window._lastStatus) {
          window._lastStatus.acmmLevel = confirmedLevel;
          window._lastStatus.acmmLevelConfigured = true;
          window._lastStatus.acmmPackAgents = data.packAgents || [];
          render(window._lastStatus);
        }
        showToast(`Level ${level} applied: ${created} created, ${skipped} skipped` + (governorChanges ? ' — governor settings changed' : ''), governorChanges ? 'info' : 'success');
        if (governorChanges) await hiveAlert(governorChanges, 'Governor settings changed');
        _acmmPacks = null;
      } catch (err) {
        hideACMMLoading();
        errEl.textContent = 'Network error: ' + err.message;
        errEl.style.display = 'block';
      }
    }


    // Packs reconcile scheduling as part of an explicit level apply. Show the
    // exact mutation instead of leaving an operator to discover it after restart.
    function formatGovernorChanges(changes) {
      if (!changes) return '';
      const lines = [];
      const interval = changes.eval_interval_s;
      if (interval && interval.from !== interval.to) {
        lines.push(`Evaluation interval: ${interval.from}s → ${interval.to}s`);
      }
      for (const cadence of changes.cadences || []) {
        lines.push(`${cadence.mode} / ${cadence.agent}: ${cadence.from || 'unset'} → ${cadence.to || 'unset'}`);
      }
      return lines.join('\n');
    }

    // Short "L3 Quality-Gated"-style label for an ACMM pack level. Shared by the
    // badges, the level dialog subtitle, and the Governor config General tab.
    function acmmLevelLabel(level) {
      const levelNames = ['', 'L1 Inception', 'L2 Advisory', 'L3 Quality-Gated', 'L4 Security-Aware', 'L5 Semi-Autonomous', 'L6 Autonomous'];
      return levelNames[level] || 'L' + level;
    }

    function updateACMMBadge(level, packAgents) {
      // Trailing ▾ signals the badge is a dropdown-style control, not a label.
      // The caret is wrapped in .acmm-caret so it renders slightly larger than
      // the small badge label, strengthening the "opens a picker" affordance.
      const label = 'ACMM ' + acmmLevelLabel(level) + ' ';
      const html = escapeHtml(label) + '<span class="acmm-caret">▾</span>';
      // Tint the badge with the level's own color so each level is visually
      // distinct at a glance; fall back to amber when the level is unset (0).
      const c = level > 0 ? acmmLevelColor(level) : 'var(--brand)';
      const tint = el => {
        if (!el) return;
        el.style.color = c;
        el.style.background = 'color-mix(in srgb, ' + c + ' 14%, transparent)';
        el.style.borderColor = 'color-mix(in srgb, ' + c + ' 45%, transparent)';
      };
      const topbarBadge = document.getElementById('acmm-badge');
      if (topbarBadge) {
        const [levelCode, ...levelName] = acmmLevelLabel(level).split(' ');
        topbarBadge.innerHTML = escapeHtml(levelCode) + (levelName.length ? `<span class="acmm-badge-name"> · ${escapeHtml(levelName.join(' '))}</span>` : '');
        topbarBadge.title = 'ACMM ' + acmmLevelLabel(level) + ' — click to change maturity level';
        tint(topbarBadge);
      }
      const badge = document.getElementById('acmm-main-badge');
      if (badge) { badge.innerHTML = html; tint(badge); }
      applyACMMVisibility(level, packAgents);
    }

    function applyACMMVisibility(level, packAgents) {
      const hide = el => { if (el) el.style.display = 'none'; };
      const show = (el, d) => { if (el) el.style.display = d || ''; };

      const governor = document.getElementById('governor');
      const advisorySection = document.getElementById('advisory-section');
      const tokenPanel = document.getElementById('token-panel');
      const reposSection = document.getElementById('repos-section');
      const contributorsSection = document.getElementById('contributors-section');
      const leaderboardSection = document.getElementById('leaderboard-section');
      const nousSection = document.getElementById('nous-section');
      const debugSection = document.getElementById('debug-section');
      const logsSection = document.getElementById('logs-section');

      const sidebarGroups = document.querySelectorAll('.oc-nav-group');
      const sidebarItems = document.querySelectorAll('.oc-nav-item');

      const agentCards = document.getElementById('agents');
      const agentDetail = document.getElementById('oc-agent-detail');

      const ACMM_GOVERNOR_MIN_LEVEL = 2;
      const ACMM_ADVISORY_MIN_LEVEL = 2;
      const ACMM_TOKENS_MIN_LEVEL = 3;
      const ACMM_REPOS_MIN_LEVEL = 2;
      const ACMM_NOUS_MIN_LEVEL = 3;
      const ACMM_CONTRIBUTORS_MIN_LEVEL = 2;
      const ACMM_LEADERBOARD_MIN_LEVEL = 2;
      const ACMM_DEBUG_MIN_LEVEL = 3;
      const ACMM_LOGS_MIN_LEVEL = 3;

        show(agentCards, '');
        show(agentDetail, '');
        sidebarGroups.forEach(g => show(g, ''));
        sidebarItems.forEach(item => show(item, ''));

        level >= ACMM_GOVERNOR_MIN_LEVEL ? show(governor, '') : hide(governor);
        level >= ACMM_ADVISORY_MIN_LEVEL ? show(advisorySection, '') : hide(advisorySection);
        level >= ACMM_TOKENS_MIN_LEVEL ? show(tokenPanel, '') : hide(tokenPanel);
        level >= ACMM_REPOS_MIN_LEVEL ? show(reposSection, '') : hide(reposSection);
        level >= ACMM_CONTRIBUTORS_MIN_LEVEL ? show(contributorsSection, '') : hide(contributorsSection);
        level >= ACMM_LEADERBOARD_MIN_LEVEL ? show(leaderboardSection, '') : hide(leaderboardSection);
        // #12076: the Experiment Lab (nous) section, its sidebar entry and the
        // sidebar scope/mode summary need BOTH the dashboard.experiment_lab
        // opt-in (applyDashboardFeatureVisibility) and ACMM L3+.
        const nousVisible = experimentLabEnabled() && level >= ACMM_NOUS_MIN_LEVEL;
        nousVisible ? show(nousSection, '') : hide(nousSection);
        const nousSidebarCfg = document.getElementById('nous-sidebar-config');
        nousVisible ? show(nousSidebarCfg, '') : hide(nousSidebarCfg);
        level >= ACMM_DEBUG_MIN_LEVEL ? show(debugSection, '') : hide(debugSection);
        hide(logsSection);

        if (level < ACMM_GOVERNOR_MIN_LEVEL) {
          sidebarItems.forEach(item => {
            if (item.getAttribute('data-section') === 'governor') hide(item);
          });
        }
        sidebarItems.forEach(item => {
          const section = item.getAttribute('data-section') || '';
          if (section === 'advisory-section' && level < ACMM_ADVISORY_MIN_LEVEL) hide(item);
          if (section === 'token-panel' && level < ACMM_TOKENS_MIN_LEVEL) hide(item);
          if (section === 'repos-section' && level < ACMM_REPOS_MIN_LEVEL) hide(item);
          if (section === 'contributors-section' && level < ACMM_CONTRIBUTORS_MIN_LEVEL) hide(item);
          if (section === 'leaderboard-section' && level < ACMM_LEADERBOARD_MIN_LEVEL) hide(item);
          if (section === 'nous-section' && !nousVisible) hide(item);
        });

        // #7099: the pack-roster gate is NOT re-applied here. The server already
        // filters agents through it in buildAgentsWithHidden (status_builder.go),
        // but WITH the documented "active outside pack" exception
        // (activeOutsidePack): a running or enabled-and-unpaused agent outside the
        // level's roster — a custom agent, or one explicitly enabled in YAML — is
        // deliberately published in status.agents and correctly absent from
        // hiddenAgents. The nav items and cards this function toggles are built
        // ONLY from status.agents (renderAgents / ocUpdateSidebarAgents), so
        // status.agents is already authoritative. Re-deriving the pack filter here
        // with no escape hatch could only ever DISAGREE with the server, hiding
        // the exact agent (and its card — terminal link, Login button, restart,
        // config gear) the server chose to surface, including one the watchdog is
        // actively raising a re-auth alert about. It also hid the card without
        // recording anything in hiddenAgents, reopening the diagnostic gap
        // #6581/#6652 closed. The server decision is the only pack gate; the
        // client no longer second-guesses it.
    }

    function focusAuthorizedUsersAccessList() {
      const el = document.getElementById('access-users-list') || document.getElementById('access-users-status');
      if (!el) return;
      if (typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'center', behavior: 'smooth' });
      if (typeof el.focus === 'function') el.focus({ preventScroll: true });
    }

    function openTrustedAuthorSecurityAccess() {
      switchConfigTab('Security');
      requestAnimationFrame(() => setTimeout(focusAuthorizedUsersAccessList, 0));
    }

    function switchConfigTab(tabId) {
      _configState.tab = tabId;
      const configKey = _configState.type === 'governor' ? '__governor__' : _configState.agent;
      if (configKey) _configTabMemory[configKey] = tabId;
      document.querySelectorAll('.config-tab').forEach(t => {
        t.classList.toggle('active', t.dataset.tab === tabId);
      });
      document.querySelector('.config-tab.active')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
      renderConfigTab(tabId);
      const hashTarget = _configState.type === 'governor' ? 'governor' : _configState.agent;
      if (hashTarget) history.replaceState(null, '', window.hiveURLWithHash ? window.hiveURLWithHash('#config/' + encodeURIComponent(hashTarget) + '/' + encodeURIComponent(tabId)) : '#config/' + encodeURIComponent(hashTarget) + '/' + encodeURIComponent(tabId));
    }

    /* loadConfigData fills _configState.data from the server, or records why
       it could not. It never fabricates a config: the old `data = {}`
       fallback rendered every field as 0/blank/off and armed Save to persist
       exactly those zeros (#7405). On failure loadError is 'session' (the
       session is gone — the shared handler already showed the sign-in toast)
       or 'error' with the reason in loadErrorDetail, and every render path
       checks it before touching data. */
    // Config reads can block server-side: GET /api/config/agent/{name} took the
    // global agent-manager lock, which a restart holds across tmux/subprocess
    // work for 10s+ (#7417). GetStatusFast removed that particular wait, but an
    // unbounded fetch turns ANY future server stall back into a permanent
    // "Loading…" with no diagnostics, so bound it and say so instead.
    const CONFIG_LOAD_TIMEOUT_MS = 12000;
    // Guards against a slow reply for a dialog the operator already closed or
    // replaced painting over the dialog that superseded it.
    let _configLoadSeq = 0;
    let _configLoadAbort = null;

    async function loadConfigData(type, agentName) {
      _configState.loadError = null;
      _configState.loadErrorDetail = '';
      const seq = ++_configLoadSeq;
      if (_configLoadAbort) _configLoadAbort.abort();
      const ctl = new AbortController();
      _configLoadAbort = ctl;
      let timedOut = false;
      const timer = setTimeout(() => { timedOut = true; ctl.abort(); }, CONFIG_LOAD_TIMEOUT_MS);
      try {
        const url = type === 'governor' ? '/api/config/governor' : `/api/config/agent/${agentName}`;
        const res = await fetch(url, { signal: ctl.signal });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data = await res.json();
        if (seq !== _configLoadSeq) return false;
        _configState.data = data;
        _settingsSearch.index = null;
      } catch (err) {
        if (seq !== _configLoadSeq) return false;
        if (timedOut) {
          // A timeout is not an expired session: do not send the operator to
          // sign in again when the server is simply slow.
          _configState.loadError = 'error';
          _configState.loadErrorDetail = `timed out after ${Math.round(CONFIG_LOAD_TIMEOUT_MS / 1000)}s — the server did not respond`;
        } else {
          const expired = await reportFetchFailure('Failed to load config', err);
          _configState.loadError = expired ? 'session' : 'error';
          _configState.loadErrorDetail = err && err.message ? err.message : String(err);
        }
      } finally {
        clearTimeout(timer);
        if (_configLoadAbort === ctl) _configLoadAbort = null;
      }
      return true;
    }

