    // ── Config-driven agent color/sort helpers ──────────────────────────────
    const _DEFAULT_AGENT_COLOR = 'var(--muted)';
    function _getAgentColor(name) {
      const agents = window._lastStatus?.agents || [];
      const agent = agents.find(a => a.name === name);
      return agent?.color || _DEFAULT_AGENT_COLOR;
    }
    function _getAgentSortOrder(name) {
      const agents = window._lastStatus?.agents || [];
      const agent = agents.find(a => a.name === name);
      return agent?.sortOrder ?? 100;
    }
    let _injectedAgentStyleEl = null;
    function injectAgentStyles(agents) {
      if (!_injectedAgentStyleEl) {
        _injectedAgentStyleEl = document.createElement('style');
        _injectedAgentStyleEl.id = 'agent-dynamic-styles';
        document.head.appendChild(_injectedAgentStyleEl);
      }
      const rules = agents
        .filter(a => a.color)
        .map(a => `.token-session-row .sagent.a-${CSS.escape(a.name)} { color: ${a.color}; }`)
        .join('\n');
      _injectedAgentStyleEl.textContent = rules;
    }

    // ── Sidebar layout: drag-and-drop + groups ──────────────────────────────
    let _sidebarLayout = null;
    let _sidebarLayoutLoaded = false;
    let _sidebarDragAgent = null;
    let _sidebarDragGroup = null;

    function _loadSidebarLayout() {
      if (_sidebarLayoutLoaded) return;
      _sidebarLayoutLoaded = true;
      fetch('/api/config/sidebar').then(r => r.json()).then(d => {
        if (d.sidebar && d.sidebar.groups) _sidebarLayout = d.sidebar;
        ocUpdateSidebarAgents();
      }).catch(() => {});
    }
    _loadSidebarLayout();

    function _saveSidebarLayout(groups) {
      _sidebarLayout = { groups };
      fetch('/api/config/sidebar', {
        method: 'PUT', headers: _inceptionAuthHeaders(),
        body: JSON.stringify({ groups })
      }).catch(() => {});
      const lastData = window._lastStatus || {};
      if (lastData.agents) {
        applyPinOverrides(lastData.agents);
        renderAgents(lastData.agents);
      }
      if (lastData.governor) {
        renderGovernor(lastData.governor, lastData.cadenceMatrix || [], lastData);
      }
      renderAdvisoryDigest(lastData.advisoryDigest || null);
      renderDebugSection();
      const _lvl2 = (lastData.acmmLevel != null) ? lastData.acmmLevel : 0;
      applyACMMVisibility(_lvl2, lastData.acmmPackAgents || null);
    }

    function _sidebarRenderAgent(a) {
      if (a.configDisabled === true) {
        const label = a.displayName || a.name;
        const ownerBits = [a.backendOwner && `backend: ${a.backendOwner}`, a.modelOwner && `model: ${a.modelOwner}`].filter(Boolean);
        const title = `Configured but disabled${ownerBits.length ? ' — ' + ownerBits.join(', ') : ''}`;
        const emojiHtml = a.emoji ? `<span class="oc-nav-emoji">${a.emoji}</span>` : '';
        // Mode badge on the disabled row too: the mode an agent WOULD run in is
        // exactly what you want to check before turning it on, and it is the one
        // field the enable dialog cannot show you from the sidebar.
        const modeHtml = (a.modeEmoji || a.mode) ? `<span class="oc-nav-mode" title="${esc(a.mode || '')}">${a.modeEmoji || modeEmoji(a.mode)}</span>` : '';
        // The gear matches the ⚙ on enabled rows on purpose. This button opens the
        // SAME agent config dialog they do — enabling is one field inside it, not a
        // one-click flip — and a bare "Enable…" read as a state toggle, so nobody
        // discovered that a disabled agent is fully configurable before it runs.
        return `<div class="oc-nav-item disabled-agent" data-agent-nav="${esc(a.name)}" title="${esc(title)}"><button type="button" class="oc-nav-drag-handle" draggable="true" data-nav-order-grip="agent" data-agent="${esc(a.name)}" data-stop="1" aria-label="Move ${esc(label)} agent" aria-pressed="false" title="Drag or focus and press Alt+↑/↓ to reorder">⠿</button><span class="oc-agent-dot off"></span>${emojiHtml}<span class="oc-nav-text">${esc(label)}</span>${modeHtml}<button class="oc-enable-agent" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(a.name)}" data-tab="General" title="Configure ${esc(label)} — review its settings and enable it from the General tab">⚙ Enable…</button><span class="oc-nav-actions"><button class="oc-nav-delete" data-action="deleteAgentFromConfig" data-agent="${esc(a.name)}" title="Delete">✕</button></span></div>`;
      }
      const isOnDemand = a.onDemand === true;
      // Manual/persisted pause wins over offByCadence — see the agent-detail
      // renderer for the incident this ordering prevents.
      const isPaused = a.paused === true && !isOnDemand;
      const isOff = agentHeldByMode(a) && !isPaused && !isOnDemand;
      // Down = the PROCESS is not alive ('stopped', 'failed', or never
      // launched — anything but 'running'). The dot used to read only the
      // 'stopped' state, so a failed/crash-looping agent fell through to the
      // green 'idle' class while the deep "agents" health check reported it
      // down (#2465). Match the deep check: non-running + non-paused = down,
      // with paused/off/on-demand keeping their own styling.
      const isStarting = !!a.starting && a.state !== 'running' && !isPaused && !isOff;
      const isDown = agentSessionDown(a) && !isPaused && !isOff && !isOnDemand && !isStarting;
      // A down agent whose CLI is unauthenticated is not merely down — it can
      // never do work until someone logs it in. Same formula as the agent
      // panel's Login button; rendered amber with a "needs login" note. Only
      // applies when the agent is not running: a running authenticated agent
      // is green, a non-running authenticated agent is red (#2465).
      const needsLoginDown = _agentNeedsLogin(a, isDown);
      const isRunning = a.busy === 'working' && !isPaused && !isOff && !isDown;
      const isOnDemandIdle = isOnDemand && !isRunning;
      const dotCls = needsLoginDown ? 'needs-login' : isStarting ? 'starting' : isDown ? 'stopped' : isOnDemandIdle ? 'on-demand' : isPaused ? 'paused' : isOff ? 'off' : isRunning ? 'running' : 'idle';
      // Healthy-but-governor-paused → hollow green dot (see detail-header renderer).
      // isOff is only reached past the down / needs-login / on-demand / manual-paused
      // guards, so the agent is provably alive; render off-healthy instead of grey off.
      const dotState = isOff ? 'off-healthy' : dotCls;
      const isActive = _ocSelectedAgent === a.name ? ' active' : '';
      const baseDesc = a.description || AGENT_DESCRIPTIONS[a.name] || '';
      const agentDesc = needsLoginDown ? (baseDesc ? baseDesc + '\n' : '') + 'needs login — CLI unauthenticated, the agent cannot work until logged in' : baseDesc;
      const label = a.displayName || a.name;
      const emojiHtml = a.emoji ? `<span class="oc-nav-emoji">${a.emoji}</span>` : '';
      const modeHtml = (a.modeEmoji || a.mode) ? `<span class="oc-nav-mode" title="${esc(a.mode || '')}">${a.modeEmoji || modeEmoji(a.mode)}</span>` : '';
      return `<a class="oc-nav-item${isActive}" data-agent-nav="${esc(a.name)}" data-action="ocSelectAgent" data-agent="${esc(a.name)}" title="${esc(agentDesc)}"><button type="button" class="oc-nav-drag-handle" draggable="true" data-nav-order-grip="agent" data-agent="${esc(a.name)}" data-stop="1" aria-label="Move ${esc(label)} agent" aria-pressed="false" title="Drag or focus and press Alt+↑/↓ to reorder">⠿</button><span class="oc-agent-dot ${dotState}" data-status="${statusAttr(dotState)}"${isOff ? ` title="${esc(agentOffTitle(a))}"` : ''}></span>${emojiHtml}<span class="oc-nav-text">${esc(label)}</span>${needsLoginDown ? '<span class="oc-nav-key" title="CLI needs login">\uD83D\uDD11</span>' : ''}${a.sandboxed ? statusBadgeHTML('ready', 'S', 'Sandboxed', 'done') : ''}${modeHtml}<span class="oc-nav-actions"><button class="hv-btn btn-secondary" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(a.name)}" title="Configure">⚙</button><button class="oc-nav-delete" data-action="deleteAgentFromConfig" data-agent="${esc(a.name)}" title="Delete">✕</button></span></a>`;
    }

    // variant 'stat' renders the tile as a Governor-section .gov-stat; the
    // default keeps the legacy .oc-gov-strip markup (that strip is hidden).
    function standbyPauseFactsHtml(gov, variant) {
      const lanes = Array.isArray(gov?.suppressed_lanes) ? gov.suppressed_lanes : [];
      if (lanes.length === 0) return '';
      const depths = gov.lane_queue_depths || {};
      const reasons = gov.lane_pause_reasons || {};
      const qualified = gov.qualified_standbys || {};
      const suspended = gov.suspended_standbys || {};
      const parts = lanes.map(lane => {
        const reason = reasons[lane] === 'budget_exhausted' ? 'out of budget' : (reasons[lane] || 'paused');
        const q = qualified[lane] || 0;
        const sus = suspended[lane] || 0;
        const suffix = sus > 0 ? `, ${sus} suspended (owner clear via standby-clear)` : '';
        const text = `${lane} paused, ${reason}, ${depths[lane] || 0} waiting, ${q} qualify${suffix}`;
        const btn = q > 0 ? ` <button class="pill" data-action="standbyDispatch" data-arg-types="t" data-standby-lane="${esc(lane)}" data-stop="1" data-prevent="1" title="Dispatch the next ready ${esc(lane)} item to standby">dispatch</button>` : '';
        return `${esc(text)}${btn}`;
      });
      const title = lanes.map(lane => `${lane}: ${depths[lane] || 0} waiting, ${qualified[lane] || 0} qualify, ${suspended[lane] || 0} suspended`).join('; ');
      if (variant === 'stat') return `<div class="gov-stat" id="gov-standby-tile" title="${esc(title)}"><div class="label">standby</div><div class="val starting">${parts.join('; ')}</div></div>`;
      return `<div class="gov-metric gm-alert" title="${esc(title)}"><span class="gm-label">standby</span><span class="gm-val">${parts.join('; ')}</span></div>`;
    }

    async function standbyDispatch(btn) {
      const lane = btn?.dataset?.standbyLane || '';
      const key = btn?.dataset?.standbyKey || '';
      if (!lane && !key) return;
      btn.disabled = true;
      try {
        const resp = await fetch('/api/contribute/standby/dispatch', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ lane, key })
        });
        if (!resp.ok) throw new Error(await resp.text());
        showToast('Standby dispatch queued', 'success');
        if (typeof fetchStatus === 'function') fetchStatus();
      } catch (e) {
        showToast('Standby dispatch failed: ' + (e && e.message ? e.message : e), 'error');
      } finally {
        btn.disabled = false;
      }
    }

    function _sidebarBuildGroups(agents) {
      let groups;
      if (!_sidebarLayout || !_sidebarLayout.groups) {
        const sorted = [...agents].sort((a, b) => {
          const disabled = (agentIsDisabled(a) ? 1 : 0) - (agentIsDisabled(b) ? 1 : 0);
          if (disabled !== 0) return disabled;
          return (a.sortOrder || 100) - (b.sortOrder || 100);
        });
        groups = [{ name: null, agents: sorted.map(a => a.name) }];
      } else {
        groups = _sidebarLayout.groups.map(g => ({ ...g }));
      }
      const assigned = new Set();
      groups.forEach(g => (g.agents || []).forEach(n => assigned.add(n)));
      const unassigned = agents.filter(a => !assigned.has(a.name)).map(a => a.name);
      if (unassigned.length > 0) {
        const ungrouped = groups.find(g => g.name === null || g.name === '');
        if (ungrouped) ungrouped.agents = [...(ungrouped.agents || []), ...unassigned];
        else groups.push({ name: null, agents: unassigned });
      }
      if (typeof agentCardLayoutNormalize === 'function') {
        const rank = {};
        agentCardLayoutNormalize(agents).order.forEach((n, i) => { rank[n] = i; });
        groups.forEach(g => { g.agents = [...(g.agents || [])].sort((a, b) => (rank[a] ?? 9999) - (rank[b] ?? 9999)); });
      }
      return groups;
    }

    function _sortAgentsBySidebar(agents) {
      const groups = _sidebarBuildGroups(agents);
      const order = [];
      for (const g of groups) {
        for (const n of (g.agents || [])) order.push(n);
      }
      const agentMap = {};
      agents.forEach(a => { agentMap[a.name] = a; });
      const sorted = [];
      for (const n of order) { if (agentMap[n]) sorted.push(agentMap[n]); }
      for (const a of agents) { if (!order.includes(a.name)) sorted.push(a); }
      return { sorted, groups };
    }

    function agentsFocusedFirst(agents) {
      const ordered = Array.isArray(agents) ? agents.slice() : [];
      if (!_ocSelectedAgent) return ordered;
      const idx = ordered.findIndex(a => a && a.name === _ocSelectedAgent);
      if (idx <= 0) return ordered;
      const focused = ordered.splice(idx, 1)[0];
      ordered.unshift(focused);
      return ordered;
    }


    const AGENT_CARD_LAYOUT_KEY_PREFIX = 'hive-agent-card-layout:';
    const AGENT_CARD_LAYOUT_VERSION = 1;
    const AGENT_CARD_MIN_H = 192;
    const AGENT_CARD_MAX_H = 900;
    let _agentOrderDrag = null;
    let _agentSpanDrag = null;

    function agentLayoutHiveKey() {
      return AGENT_CARD_LAYOUT_KEY_PREFIX + (((window._lastStatus || {}).hiveId) || 'default');
    }

    function agentLayoutName(name) {
      return String(name || '').trim();
    }
    function agentLayoutDefaultOrder(agents) {
      return (agents || []).map(a => agentLayoutName(a && a.name)).filter(Boolean);
    }

    function agentCardLayoutRead() {
      let raw = null;
      try { raw = localStorage.getItem(agentLayoutHiveKey()); } catch { return { v: AGENT_CARD_LAYOUT_VERSION, order: [], sizes: {} }; }
      const parsed = safeJsonParse(raw || '{}', {});
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return { v: AGENT_CARD_LAYOUT_VERSION, order: [], sizes: {} };
      return { v: AGENT_CARD_LAYOUT_VERSION, order: Array.isArray(parsed.order) ? parsed.order : [], sizes: (parsed.sizes && typeof parsed.sizes === 'object' && !Array.isArray(parsed.sizes)) ? parsed.sizes : {} };
    }

    function clampAgentCardSpan(span) {
      const n = Math.round(Number(span));
      if (!isFinite(n) || n <= 1) return 1;
      return Math.max(1, Math.min(3, n));
    }

    function clampAgentCardHeight(h) {
      const n = Math.round(Number(h));
      if (!isFinite(n) || n <= 0) return 0;
      return Math.max(AGENT_CARD_MIN_H, Math.min(AGENT_CARD_MAX_H, n));
    }

    function agentCardLayoutWrite(layout, defaultOrder) {
      const def = defaultOrder || [];
      const seen = new Set();
      const order = [];
      (layout.order || []).forEach(name => { const key = agentLayoutName(name); if (key && !seen.has(key)) { seen.add(key); order.push(key); } });
      const sizes = {};
      Object.keys(layout.sizes || {}).forEach(name => {
        const key = agentLayoutName(name);
        if (!key) return;
        const src = layout.sizes[name] || {};
        const span = clampAgentCardSpan(src.span);
        const height = clampAgentCardHeight(src.height);
        if (span > 1 || height) sizes[key] = { span: span, height: height };
      });
      let custom = Object.keys(sizes).length > 0 || order.length !== def.length;
      for (let i = 0; !custom && i < order.length; i++) custom = order[i] !== def[i];
      // Normalizing runs on every render; only touch storage when the
      // persisted layout actually changes.
      try {
        const key = agentLayoutHiveKey();
        const prev = localStorage.getItem(key);
        if (custom) {
          const next = JSON.stringify({ v: AGENT_CARD_LAYOUT_VERSION, order, sizes });
          if (prev !== next) localStorage.setItem(key, next);
        } else if (prev !== null) {
          localStorage.removeItem(key);
        }
      } catch {}
    }

    function agentCardLayoutSetOrder(order, agents) {
      agents = agents || ((window._lastStatus || {}).agents) || window._lastAgents || [];
      const def = agentLayoutDefaultOrder(agents);
      const valid = new Set(def);
      const seen = new Set();
      const next = [];
      (order || []).forEach(name => { const key = agentLayoutName(name); if (valid.has(key) && !seen.has(key)) { seen.add(key); next.push(key); } });
      def.forEach(name => { if (!seen.has(name)) next.push(name); });
      const layout = agentCardLayoutNormalize(agents);
      layout.order = next;
      agentCardLayoutWrite(layout, def);
      agentCardLayoutUpdateResetButton(agents);
    }

    function agentSidebarSaveOrderFromDom() {
      const tree = document.getElementById('oc-agent-tree');
      if (!tree) return;
      const order = Array.from(tree.querySelectorAll('.oc-nav-item[data-agent-nav]')).map(el => el.dataset.agentNav).filter(Boolean);
      const agents = ((window._lastStatus || {}).agents) || window._lastAgents || [];
      agentCardLayoutSetOrder(order, agents);
      if (window._lastAgents) renderAgents(window._lastAgents);
    }

    function agentCardLayoutNormalize(agents) {
      const def = agentLayoutDefaultOrder(agents);
      const valid = new Set(def);
      const stored = agentCardLayoutRead();
      const order = [];
      const seen = new Set();
      stored.order.forEach(name => { const key = agentLayoutName(name); if (valid.has(key) && !seen.has(key)) { seen.add(key); order.push(key); } });
      def.forEach(name => { if (!seen.has(name)) { seen.add(name); order.push(name); } });
      const sizes = {};
      Object.keys(stored.sizes || {}).forEach(name => {
        const key = agentLayoutName(name);
        if (!valid.has(key)) return;
        const src = stored.sizes[name] || {};
        const span = clampAgentCardSpan(src.span);
        const height = clampAgentCardHeight(src.height);
        if (span > 1 || height) sizes[key] = { span, height };
      });
      const normalized = { v: AGENT_CARD_LAYOUT_VERSION, order, sizes };
      agentCardLayoutWrite(normalized, def);
      return normalized;
    }

    function agentLayoutApplyOrder(agents) {
      agents = agents || [];
      const byName = new Map();
      agents.forEach(a => { const key = agentLayoutName(a && a.name); if (key && !byName.has(key)) byName.set(key, a); });
      const out = [];
      agentCardLayoutNormalize(agents).order.forEach(name => { if (byName.has(name)) out.push(byName.get(name)); });
      return out;
    }

    function agentCardLayoutSize(name, agents) {
      const layout = agentCardLayoutNormalize(agents || ((window._lastStatus || {}).agents) || []);
      const size = layout.sizes[agentLayoutName(name)] || {};
      return { span: clampAgentCardSpan(size.span), height: clampAgentCardHeight(size.height) };
    }

    function agentCardLayoutSetSize(name, patch) {
      const agents = ((window._lastStatus || {}).agents) || window._lastAgents || [];
      const layout = agentCardLayoutNormalize(agents);
      const key = agentLayoutName(name);
      if (!key) return;
      const prev = layout.sizes[key] || {};
      const next = { span: patch.span == null ? clampAgentCardSpan(prev.span) : clampAgentCardSpan(patch.span), height: patch.height == null ? clampAgentCardHeight(prev.height) : clampAgentCardHeight(patch.height) };
      if (next.span > 1 || next.height) layout.sizes[key] = next;
      else delete layout.sizes[key];
      agentCardLayoutWrite(layout, agentLayoutDefaultOrder(agents));
      agentCardLayoutUpdateResetButton(agents);
    }

    function agentCardLayoutHasCustom(agents) {
      const def = agentLayoutDefaultOrder(agents || []);
      const layout = agentCardLayoutNormalize(agents || []);
      if (Object.keys(layout.sizes || {}).length) return true;
      if (layout.order.length !== def.length) return true;
      for (let i = 0; i < layout.order.length; i++) if (layout.order[i] !== def[i]) return true;
      return false;
    }

    function agentCardLayoutUpdateResetButton(agents) {
      const btn = document.getElementById('agents-reset-layout-btn');
      if (btn) btn.hidden = !agentCardLayoutHasCustom(agents || window._lastAgents || []);
    }

    function resetAgentCardLayout() {
      try { localStorage.removeItem(agentLayoutHiveKey()); } catch {}
      if (window._lastAgents) renderAgents(window._lastAgents);
      ocUpdateSidebarAgents();
      dashboardUpdateLayoutMenuState();
      showToast('Agent card layout reset to the default', 'info');
    }

    function _sidebarReadLayout() {
      const tree = document.getElementById('oc-agent-tree');
      if (!tree) return null;
      const groups = [];
      tree.querySelectorAll('.oc-sidebar-group').forEach(g => {
        const name = g.dataset.groupName || null;
        const agentNames = [];
        const container = g.querySelector('.oc-sidebar-group-children') || g;
        container.querySelectorAll('.oc-nav-item[data-agent-nav]').forEach(el => {
          agentNames.push(el.dataset.agentNav);
        });
        groups.push({ name: name || null, agents: agentNames });
      });
      return groups;
    }

    function _sidebarAttachDragHandlers() {
      const tree = document.getElementById('oc-agent-tree');
      if (!tree) return;

      // --- Agent-level drag handlers ---
      tree.querySelectorAll('[data-nav-order-grip="agent"]').forEach(handle => {
        const el = handle.closest('.oc-nav-item[data-agent-nav]');
        if (!el) return;
        handle.addEventListener('click', e => { e.preventDefault(); e.stopPropagation(); });
        handle.addEventListener('dragstart', e => {
          _sidebarDragAgent = el.dataset.agentNav;
          _sidebarDragGroup = null;
          el.classList.add('dragging');
          handle.setAttribute('aria-pressed', 'true');
          e.dataTransfer.effectAllowed = 'move';
          e.dataTransfer.setData('text/plain', _sidebarDragAgent);
        });
        handle.addEventListener('dragend', () => {
          el.classList.remove('dragging');
          handle.setAttribute('aria-pressed', 'false');
          _sidebarDragAgent = null;
          _clearDragIndicators(tree);
        });
        handle.addEventListener('keydown', e => {
          if (!e.altKey || (e.key !== 'ArrowUp' && e.key !== 'ArrowDown')) return;
          e.preventDefault();
          if (e.key === 'ArrowUp') {
            const prev = el.previousElementSibling && el.previousElementSibling.matches('.oc-nav-item[data-agent-nav]') ? el.previousElementSibling : null;
            if (prev) prev.before(el);
          } else {
            const next = el.nextElementSibling && el.nextElementSibling.matches('.oc-nav-item[data-agent-nav]') ? el.nextElementSibling : null;
            if (next) next.after(el);
          }
          agentSidebarSaveOrderFromDom();
          handle.focus();
        });
      });

      // --- Group-level drag handlers ---
      tree.querySelectorAll('.oc-sidebar-group-header[draggable="true"]').forEach(header => {
        header.addEventListener('dragstart', e => {
          const group = header.closest('.oc-sidebar-group');
          if (!group) return;
          _sidebarDragGroup = group.dataset.groupName;
          _sidebarDragAgent = null;
          group.classList.add('dragging-group');
          e.dataTransfer.effectAllowed = 'move';
          e.dataTransfer.setData('text/plain', 'group:' + _sidebarDragGroup);
          e.stopPropagation();
        });
        header.addEventListener('dragend', () => {
          const group = header.closest('.oc-sidebar-group');
          if (group) group.classList.remove('dragging-group');
          _sidebarDragGroup = null;
          _clearDragIndicators(tree);
        });
      });

      function _clearDragIndicators(container) {
        container.querySelectorAll('.oc-drop-indicator, .oc-group-drop-indicator').forEach(d => d.remove());
        container.querySelectorAll('.oc-sidebar-group.drag-over').forEach(g => g.classList.remove('drag-over'));
      }

      const getAgentDropTarget = (e) => {
        const items = [...tree.querySelectorAll('.oc-nav-item[data-agent-nav]')];
        let closest = null, closestDist = Infinity;
        for (const item of items) {
          if (item.dataset.agentNav === _sidebarDragAgent) continue;
          const rect = item.getBoundingClientRect();
          const mid = rect.top + rect.height / 2;
          const dist = Math.abs(e.clientY - mid);
          if (dist < closestDist) { closestDist = dist; closest = { el: item, after: e.clientY > mid }; }
        }
        tree.querySelectorAll('.oc-sidebar-group[data-group-name]').forEach(g => {
          const groupName = g.dataset.groupName;
          if (!groupName) return;
          const header = g.querySelector('.oc-sidebar-group-header');
          const children = g.querySelector('.oc-sidebar-group-children');
          if (!header || !children) return;
          const headerRect = header.getBoundingClientRect();
          const childRect = children.getBoundingClientRect();
          const inHeader = e.clientY >= headerRect.top && e.clientY <= headerRect.bottom;
          const inEmptyChildren = children.querySelectorAll('.oc-nav-item[data-agent-nav]').length === 0
            && e.clientY >= childRect.top && e.clientY <= childRect.bottom;
          if (inHeader || inEmptyChildren) {
            closest = { el: children, appendTo: true, group: g };
          }
        });
        return closest;
      };

      const getGroupDropTarget = (e) => {
        const groups = [...tree.querySelectorAll('.oc-sidebar-group[data-group-name]')];
        let closest = null, closestDist = Infinity;
        for (const g of groups) {
          if (g.dataset.groupName === _sidebarDragGroup) continue;
          const rect = g.getBoundingClientRect();
          const mid = rect.top + rect.height / 2;
          const dist = Math.abs(e.clientY - mid);
          if (dist < closestDist) { closestDist = dist; closest = { el: g, after: e.clientY > mid }; }
        }
        return closest;
      };

      tree.addEventListener('dragover', e => {
        if (!_sidebarDragAgent && !_sidebarDragGroup) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';
        _clearDragIndicators(tree);

        if (_sidebarDragGroup) {
          const target = getGroupDropTarget(e);
          if (target) {
            const indicator = document.createElement('div');
            indicator.className = 'oc-group-drop-indicator';
            if (target.after) target.el.after(indicator);
            else target.el.before(indicator);
          }
        } else if (_sidebarDragAgent) {
          const target = getAgentDropTarget(e);
          if (target && target.appendTo && target.group) {
            target.group.classList.add('drag-over');
          } else if (target && !target.appendTo) {
            const indicator = document.createElement('div');
            indicator.className = 'oc-drop-indicator';
            if (target.after) target.el.after(indicator);
            else target.el.before(indicator);
          }
        }
      });

      tree.addEventListener('drop', e => {
        if (!_sidebarDragAgent && !_sidebarDragGroup) return;
        e.preventDefault();
        _clearDragIndicators(tree);

        if (_sidebarDragGroup) {
          const target = getGroupDropTarget(e);
          const dragEl = tree.querySelector(`.oc-sidebar-group[data-group-name="${_sidebarDragGroup}"]`);
          if (target && dragEl && target.el !== dragEl) {
            if (target.after) target.el.after(dragEl);
            else target.el.before(dragEl);
          }
          const groups = _sidebarReadLayout();
          if (groups) _saveSidebarLayout(groups);
          _sidebarDragGroup = null;
        } else if (_sidebarDragAgent) {
          const target = getAgentDropTarget(e);
          const dragEl = tree.querySelector(`.oc-nav-item[data-agent-nav="${_sidebarDragAgent}"]`);
          if (!target || !dragEl) return;
          if (target.appendTo) {
            target.el.appendChild(dragEl);
          } else if (target.el !== dragEl) {
            if (target.after) target.el.after(dragEl);
            else target.el.before(dragEl);
          }
          const groups = _sidebarReadLayout();
          if (groups) _saveSidebarLayout(groups);
          agentSidebarSaveOrderFromDom();
          _sidebarDragAgent = null;
        }
      });
    }

    async function ocAddSidebarGroup() {
      const name = await hivePrompt({title: 'Group name', label: 'Group name:'});
      if (!name || !name.trim()) return;
      const groups = _sidebarReadLayout() || [];
      groups.push({ name: name.trim(), agents: [] });
      _saveSidebarLayout(groups);
      ocUpdateSidebarAgents();
    }

    function ocRemoveSidebarGroup(groupName) {
      const groups = _sidebarReadLayout() || [];
      const group = groups.find(g => g.name === groupName);
      if (!group) return;
      const ungrouped = groups.find(g => g.name === null || g.name === '');
      if (ungrouped) ungrouped.agents = [...ungrouped.agents, ...(group.agents || [])];
      else groups.push({ name: null, agents: group.agents || [] });
      const filtered = groups.filter(g => g.name !== groupName);
      _saveSidebarLayout(filtered);
      ocUpdateSidebarAgents();
    }

    async function ocRenameSidebarGroup(oldName) {
      const newName = await hivePrompt({title: 'Rename group', label: 'Rename group:', defaultValue: oldName});
      if (!newName || !newName.trim() || newName.trim() === oldName) return;
      const groups = _sidebarReadLayout() || [];
      const group = groups.find(g => g.name === oldName);
      if (group) group.name = newName.trim();
      _saveSidebarLayout(groups);
      ocUpdateSidebarAgents();
    }

    function ocToggleSidebarGroup(header) {
      header.classList.toggle('collapsed');
      const children = header.nextElementSibling;
      if (children) children.classList.toggle('collapsed');
      const group = header.closest('.oc-sidebar-group');
      const name = group?.dataset?.groupName;
      if (name) {
        const collapsed = safeJsonParse(localStorage.getItem('hive-sidebar-collapsed') || '{}', {});
        collapsed[name] = header.classList.contains('collapsed');
        localStorage.setItem('hive-sidebar-collapsed', JSON.stringify(collapsed));
      }
    }

    function ocUpdateSidebarAgents() {
      const tree = document.getElementById('oc-agent-tree');
      if (!tree) return;
      const agents = _sidebarAgents();
      const agentMap = {};
      agents.forEach(a => { agentMap[a.name] = a; });
      const groups = _sidebarBuildGroups(agents);

      let html = '';
      for (const g of groups) {
        const validAgents = (g.agents || []).filter(n => agentMap[n]);
        if (g.name) {
          const esc = escapeHtml(g.name);
          html += `<div class="oc-sidebar-group" data-group-name="${esc}">`;
          html += `<div class="oc-sidebar-group-header" draggable="true" data-action="ocToggleSidebarGroup">`;
          html += `<span class="oc-group-arrow">▼</span> ${esc}`;
          html += `<span class="oc-group-actions" data-action="gh3">`;
          html += `<button class="hv-btn btn-secondary" data-action="ocRenameSidebarGroup" data-group="${esc}" title="Rename">✏</button>`;
          html += `<button class="hv-btn btn-danger" data-action="ocRemoveSidebarGroup" data-group="${esc}" title="Remove group">✕</button>`;
          html += `</span></div>`;
          html += `<div class="oc-sidebar-group-children">`;
          html += validAgents.map(n => _sidebarRenderAgent(agentMap[n])).join('');
          html += `</div></div>`;
        } else {
          html += `<div class="oc-sidebar-group" data-group-name="">`;
          html += validAgents.map(n => _sidebarRenderAgent(agentMap[n])).join('');
          html += `</div>`;
        }
      }

      tree.innerHTML = html;
      const collapsed = safeJsonParse(localStorage.getItem('hive-sidebar-collapsed') || '{}', {});
      tree.querySelectorAll('.oc-sidebar-group[data-group-name]').forEach(g => {
        const name = g.dataset.groupName;
        if (name && collapsed[name]) {
          const header = g.querySelector('.oc-sidebar-group-header');
          const children = g.querySelector('.oc-sidebar-group-children');
          if (header) header.classList.add('collapsed');
          if (children) children.classList.add('collapsed');
        }
      });
      _sidebarAttachDragHandlers();
    }

    function _sidebarAgents() {
      const runtime = [...(window._lastAgents || [])];
      const runtimeNames = new Set(runtime.map(a => a.name));
      for (const configured of (window._configuredAgents || [])) {
        if (!configured.enabled && !runtimeNames.has(configured.name)) {
          runtime.push({ ...configured, enabled: false, configDisabled: true, state: 'stopped', busy: 'idle' });
        }
      }
      return runtime;
    }

    applyLayout(getLayout());

