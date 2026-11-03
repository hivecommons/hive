    function _ocAgentFromHash() {
      const h = location.hash.replace('#', '');
      if (!h || h === 'throughput' || h.startsWith('section-')) return null;
      return h;
    }
    function _ocSectionFromHash() {
      const h = location.hash.replace('#', '');
      if (h === 'throughput') return 'pr-throughput-section';
      if (h === 'review-pipeline' || h === 'section-review-pipeline-section' || h === 'review-pipeline-section') return 'review-queue-section';
      if (h === 'review-queue') return 'review-queue-section';
      const route = h.split('?')[0];
      if (route === 'projects') {
        const key = overviewProjectFilterKeyFromURL();
        if (key) _overviewProjectFilterKey = key;
        return 'repos-section';
      }
      return route && route.startsWith('section-') ? dashboardAliasSectionId(route.replace('section-', '')) : null;
    }
    let _ocSelectedAgent = _ocAgentFromHash();
    let _ocPaneTimer = null;
    let _ocViewRestored = false;
    const OC_PANE_POLL_MS = 3000;
    const OC_PANE_LINES = 2000;

    function ocStartPanePoll() {
      ocStopPanePoll();
      if (!_ocSelectedAgent) return;
      ocFetchPane();
      _ocPaneTimer = setInterval(ocFetchPane, OC_PANE_POLL_MS);
    }
    function ocStopPanePoll() {
      if (_ocPaneTimer) { clearInterval(_ocPaneTimer); _ocPaneTimer = null; }
    }
    async function ocFetchPane() {
      if (!_ocSelectedAgent) return;
      const selAgent = (window._lastStatus?.agents || []).find(a => a.name === _ocSelectedAgent);
      if (selAgent && selAgent.detailSummary) return;
      try {
        const r = await fetch('/api/pane/' + encodeURIComponent(_ocSelectedAgent) + '?lines=' + OC_PANE_LINES);
        if (!r.ok) return;
        const d = await r.json();
        const el = document.getElementById('oc-summary-scroll');
        if (!el || !d.lines) return;
        while (d.lines.length > 0 && d.lines[d.lines.length - 1].trim() === '') d.lines.pop();
        const txt = d.lines.join('\n');
        if (el.textContent === txt) return;
        const wasAtBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        el.textContent = txt;
        if (_ocSummaryTailing || wasAtBottom) el.scrollTop = el.scrollHeight;
      } catch {}
    }

    function ocSectionBody(sectionId) {
      return document.querySelector(`#${sectionId} .dash-card-body, #${sectionId} .section-body`);
    }
    function ocAfterNavLayoutSettles(sectionIds, callback) {
      const raf = typeof requestAnimationFrame === 'function' ? requestAnimationFrame : function(fn) { setTimeout(fn, 0); };
      const afterPaint = function(fn) { raf(function() { raf(fn); }); };
      const ids = Array.from(new Set((sectionIds || []).filter(Boolean)));
      const bodies = ids.map(ocSectionBody).filter(Boolean);
      if (!bodies.length) { afterPaint(callback); return; }
      let done = false;
      let quietTimer = null;
      let maxTimer = null;
      let lastResizeAt = 0;
      let observer = null;
      const pending = new Set(bodies);
      const cleanup = function() {
        bodies.forEach(function(body) { body.removeEventListener('transitionend', onTransitionEnd); });
        if (observer) observer.disconnect();
        if (quietTimer) clearTimeout(quietTimer);
        if (maxTimer) clearTimeout(maxTimer);
      };
      const finish = function() {
        if (done) return;
        done = true;
        cleanup();
        afterPaint(callback);
      };
      const scheduleQuietFinish = function() {
        if (quietTimer) clearTimeout(quietTimer);
        quietTimer = setTimeout(function() {
          if (!lastResizeAt || performance.now() - lastResizeAt >= NAV_JUMP_RESIZE_QUIET_MS) finish();
          else scheduleQuietFinish();
        }, NAV_JUMP_RESIZE_QUIET_MS);
      };
      var onTransitionEnd = function(event) {
        if (event.target !== event.currentTarget) return;
        if (event.propertyName && event.propertyName !== SECTION_MAX_HEIGHT_PROPERTY && event.propertyName !== 'opacity') return;
        pending.delete(event.currentTarget);
        if (!pending.size) finish();
      };
      bodies.forEach(function(body) { body.addEventListener('transitionend', onTransitionEnd); });
      if (typeof ResizeObserver !== 'undefined') {
        observer = new ResizeObserver(function() {
          lastResizeAt = performance.now();
          scheduleQuietFinish();
        });
        ids.forEach(function(id) {
          const el = document.getElementById(id);
          if (el) observer.observe(el);
        });
      }
      maxTimer = setTimeout(finish, NAV_JUMP_SETTLE_MS);
    }
    function ocScrollSectionIntoView(section) {
      const el = document.getElementById(section);
      if (!el) return;
      // scrollIntoView honours the section's scroll-margin-top (sticky
      // navbar offset); the settle loop only needs the raw Y to detect drift.
      const targetY = () => el.getBoundingClientRect().top + window.scrollY;
      ocSettleScroll(targetY, () => el.scrollIntoView({ block: 'start', behavior: 'smooth' }), OC_NAV_SETTLE_MS, el);
      el.classList.remove('anchor-highlight');
      void el.offsetWidth;
      el.classList.add('anchor-highlight');
    }
    function scrollToAgentCard(name, behavior) {
      const safeName = (window.CSS && CSS.escape) ? CSS.escape(name || '') : String(name || '').replace(/["\\]/g, '\\$&');
      const selector = `.agent-card[data-agent="${safeName}"]`;
      const card = document.querySelector(selector);
      if (!card) return false;
      card.scrollIntoView({ block: 'center', behavior: behavior || 'smooth' });
      card.classList.remove('anchor-highlight');
      void card.offsetWidth;
      card.classList.add('anchor-highlight', 'oc-focused');
      setTimeout(() => card.classList.remove('oc-focused'), 1800);
      return true;
    }
    function openAgentCollapsedTile(name) {
      if (!name) return;
      _ocSelectedAgent = name;
      history.replaceState(null, '', window.hiveURLWithHash ? window.hiveURLWithHash('#' + name) : '#' + name);
      ocStopPanePoll();
      document.querySelectorAll('.oc-nav-item').forEach(i => i.classList.remove('active'));
      const active = document.querySelector(`.oc-nav-item[data-agent-nav="${(window.CSS && CSS.escape) ? CSS.escape(name) : String(name).replace(/["\\]/g, '\\$&')}"]`);
      if (active) active.classList.add('active');
      navPeekRestore('agents-section', { instant: true });
      const expanded = navPeekExpand('agents-section');
      if (!expanded && isSectionCollapsed('agents-section')) toggleSection('agents-section', { collapsed: false, persist: false, navPeek: true });
      if (window._lastAgents) renderAgents(window._lastAgents);
      ocUpdateFocusedState();
      const after = () => scrollToAgentCard(name, 'smooth');
      ocAfterNavLayoutSettles(['agents-section'], after);
    }
    function scrollToRepoCard(repo, behavior) {
      const key = repoOrderName(repo || '');
      const safeKey = (window.CSS && CSS.escape) ? CSS.escape(key) : String(key).replace(/["\\]/g, '\\$&');
      const selector = `.repo-card[data-repo-order-key="${safeKey}"]`;
      const card = document.querySelector(selector);
      if (!card) return false;
      card.scrollIntoView({ block: 'center', behavior: behavior || 'smooth' });
      card.classList.remove('anchor-highlight');
      void card.offsetWidth;
      card.classList.add('anchor-highlight');
      setTimeout(() => card.classList.remove('anchor-highlight'), 1800);
      return true;
    }
    function openRepoCollapsedMore() {
      navPeekRestore('repos-section', { instant: true });
      const expanded = navPeekExpand('repos-section');
      if (!expanded && isSectionCollapsed('repos-section')) toggleSection('repos-section', { collapsed: false, persist: false, navPeek: true });
      ocAfterNavLayoutSettles(['repos-section'], function() { ocScrollSectionIntoView('repos-section'); });
    }
    function openRepoCollapsedTile(repo) {
      if (!repo) return;
      const state = repoPillFilterState();
      writeRepoPillFilterState({ mode: state.mode, kinds: [], repo });
      navPeekRestore('repos-section', { instant: true });
      const expanded = navPeekExpand('repos-section');
      if (!expanded && isSectionCollapsed('repos-section')) toggleSection('repos-section', { collapsed: false, persist: false, navPeek: true });
      repaintReposForWidth();
      ocAfterNavLayoutSettles(['repos-section'], function() { scrollToRepoCard(repo, 'smooth') || ocScrollSectionIntoView('repos-section'); });
    }
    function openAdvisoryAdvicePill(index) {
      const affected = [];
      ['advisory-section', 'hive-advice-section'].forEach(sectionId => {
        navPeekRestore(sectionId, { instant: true });
        if (navPeekExpand(sectionId)) affected.push(sectionId);
        if (isSectionCollapsed(sectionId)) {
          toggleSection(sectionId, { collapsed: false, persist: false, navPeek: true });
          affected.push(sectionId);
        }
      });
      const raw = String(index == null ? '' : index).trim();
      const targetId = raw === '' ? 'hive-advice-section' : 'hive-advice-rec-' + raw;
      const after = () => {
        const target = document.getElementById(targetId) || document.getElementById('hive-advice-section') || document.getElementById('advisory-section');
        if (!target) return;
        target.scrollIntoView({ block: 'center', behavior: 'smooth' });
        target.classList.remove('anchor-highlight');
        void target.offsetWidth;
        target.classList.add('anchor-highlight');
      };
      ocAfterNavLayoutSettles(affected, after);
    }
    function lifecycleEmptyStateHtml() {
      return '<div class="lc-empty">No pull requests in the window yet.</div>';
    }
    function lifecycleRenderNavEmptyState() {
      const card = document.getElementById('lifecycle-card');
      if (!card || card.innerHTML.trim()) return;
      refreshSectionCardShell('lifecycle-section');
      setSectionSummary('lifecycle-section', '0 in flight · 0 merged');
      card.innerHTML = `<div class="lc-head"><div class="lc-fleet">
        <div class="lc-fleet-stat"><div class="val inflight">0</div><div class="lbl">In flight</div></div>
        <div class="lc-fleet-stat"><div class="val merged">0</div><div class="lbl">Merged</div></div>
        <div class="lc-fleet-stat"><div class="val blocked">0</div><div class="lbl">Blocked</div></div>
      </div></div>${lifecycleEmptyStateHtml()}`;
      applySectionCollapse('lifecycle-section');
    }
    function ocEnsureSectionVisibleForNav(section) {
      if (section !== 'lifecycle-section') return [];
      const affected = [];
      const parent = document.getElementById('advisory-section');
      const target = document.getElementById('lifecycle-section');
      if (parent) {
        if (dashboardSectionHidden('advisory-section')) dashboardSetSectionHidden('advisory-section', false);
        parent.style.display = '';
        if (isSectionCollapsed('advisory-section')) {
          toggleSection('advisory-section', { collapsed: false, persist: false, navPeek: true });
          affected.push('advisory-section');
        }
      }
      if (target) {
        target.style.display = '';
        if (isSectionCollapsed('lifecycle-section')) {
          toggleSection('lifecycle-section', { collapsed: false, persist: false, navPeek: true });
          affected.push('lifecycle-section');
        }
      }
      lifecycleRenderNavEmptyState();
      if (typeof lcScheduleLayout === 'function') {
        lcScheduleLayout();
        if (typeof requestAnimationFrame === 'function') requestAnimationFrame(lcScheduleLayout);
      }
      return affected;
    }
    function ocNavigate(section, skipScroll) {
      section = dashboardAliasSectionId(section);
      if (dashboardFeatureSectionHidden(section)) return;
      if (dashboardSectionHidden(section)) dashboardSetSectionHidden(section,false);
      _ocSelectedAgent = null;
      history.replaceState(null, '', window.hiveURLWithHash ? window.hiveURLWithHash('#section-' + section) : '#section-' + section);
      ocStopPanePoll();
      const detail = document.getElementById('oc-agent-detail');
      if (detail) { detail.classList.remove('active'); detail.innerHTML = ''; }
      ocUpdateFocusedState();
      const previousPeekSection = navPeekSectionId();
      const restored = navPeekRestore(section, { instant: true });
      const expanded = navPeekExpand(section);
      const revealed = ocEnsureSectionVisibleForNav(section);
      if (!skipScroll) {
        const affectedSections = [];
        if (restored && previousPeekSection) affectedSections.push(previousPeekSection);
        if (expanded) affectedSections.push(section);
        revealed.forEach(id => { if (affectedSections.indexOf(id) < 0) affectedSections.push(id); });
        ocAfterNavLayoutSettles(affectedSections, function() { ocScrollSectionIntoView(section); });
      }
      document.querySelectorAll('.oc-nav-item').forEach(i => i.classList.remove('active'));
      const active = document.querySelector(`.oc-nav-item[data-section="${section}"]`);
      if (active) active.classList.add('active');
      if (typeof hfOnNavigate === 'function') hfOnNavigate(section);
    }

    function applyDashboardFeatureVisibility() {
      const experimentOn = experimentLabEnabled();
      document.querySelectorAll('[data-feature="experiment_lab"]').forEach(el => {
        if (experimentOn) {
          el.hidden = false;
          if (el.id !== 'nous-section' && el.id !== 'nous-sidebar-config') el.style.display = '';
        } else {
          el.hidden = true;
          el.style.display = 'none';
        }
      });
      if (!experimentOn) {
        if (_nousPollTimer) { dashboardClearInterval('nous-status'); _nousPollTimer = null; }
        _nousPollStarted = false;
        const panel = document.getElementById('nous-panel');
        if (panel) panel.innerHTML = '';
        _nousCache = null;
        if (location.hash === '#nous-section' || location.hash === '#section-nous-section') {
          history.replaceState(null, '', window.hiveURLWithHash ? window.hiveURLWithHash('#') : '#');
        }
      } else {
        ensureNousPolling();
      }
      dashboardApplyLayout(dashboardLayoutNormalize(dashboardLayoutRead()));
      dashboardApplyHiddenSections();
      dashboardSortSidebar();
    }

    function ocToggleAgentTree(el) {
      el.classList.toggle('collapsed');
      const children = el.nextElementSibling;
      if (children) children.classList.toggle('collapsed');
      localStorage.setItem('hive-agent-tree-collapsed', el.classList.contains('collapsed') ? '1' : '0');
    }
    (function() {
      if (localStorage.getItem('hive-agent-tree-collapsed') === '1') {
        const t = document.querySelector('.oc-tree-toggle');
        if (t) { t.classList.add('collapsed'); const c = t.nextElementSibling; if (c) c.classList.add('collapsed'); }
      }
    })();

    const STRATEGY_AGENT_NAMES = ['strategist', 'experimenter'];
    // Tooltip for the hollow-green (off-healthy) dot: the agent process is fine,
    // but its governor cadence for the current mode is paused/off, so it is not
    // being kicked. Shared by every renderer that can show the hollow-green dot.
    const OFF_HEALTHY_TITLE = "Agent is healthy but off — the governor's cadence for this mode is paused, so it won't be kicked.";
    /* Held by the governor mode (#7474). Two server flags land an alive agent
       in the card's "off" display state, and they are different gaps:
       offByCadence is an explicit pause/off ENTRY for the current mode (an
       operator choice); unscheduledInMode is NO entry for the current mode —
       or idle, which every mode inherits — while some OTHER mode does name
       the agent. The second is the surge-only reviewer: kicked while the
       backlog holds the fleet in surge, silent the moment its own work drops
       the fleet to busy, and green-idle on every other signal. Both mean
       "the governor will not kick this until the mode or the config
       changes", so both render as off-healthy with the ⚙ Cadences action;
       the title says which. */
    function agentHeldByMode(a) {
      return a.offByCadence === true || a.unscheduledInMode === true;
    }
    function agentCadenceModesLabel(a) {
      const modes = Array.isArray(a.cadenceModes) ? a.cadenceModes.map(m => String(m)) : [];
      if (modes.length === 0) return 'other modes';
      if (modes.length === 1) return modes[0];
      return modes.slice(0, -1).join(', ') + ' and ' + modes[modes.length - 1];
    }
    function agentCadenceDisplay(a) {
      if (a && a.continuous === true) {
        const mode = String(window._lastStatus?.governor?.mode || '').trim().toUpperCase();
        return `∞ continuous${mode ? ' (' + mode + ')' : ''}`;
      }
      return a && a.cadence ? a.cadence : '—';
    }
    function agentOffTitle(a) {
      if (a.unscheduledInMode === true && a.offByCadence !== true) {
        const mode = (window._lastStatus?.governor?.mode || '').toLowerCase();
        return `Agent is healthy but off — it has a cadence only in ${agentCadenceModesLabel(a)}, and none for the current ${mode || 'governor'} mode (or idle, which every mode inherits), so the governor won't kick it until the mode changes. Add a cadence for this mode (⚙️ → Cadences).`;
      }
      return OFF_HEALTHY_TITLE;
    }
    function ocUpdateFocusedState() {
      const isFocused = !!_ocSelectedAgent && getLayout() === 'light';
      document.body.classList.toggle('oc-agent-focused', isFocused);
      document.body.classList.toggle('oc-strategy-focused', isFocused && STRATEGY_AGENT_NAMES.includes(_ocSelectedAgent));
      document.querySelectorAll('.agent-card').forEach(card => {
        const selected = !!_ocSelectedAgent && card.dataset.agent === _ocSelectedAgent;
        card.classList.remove('oc-hidden');
        card.classList.toggle('oc-focused', selected);
      });
    }

    // ---- Scroll anchoring for in-page navigation ---------------------------
    // Clicking a sidebar item or an agent scrolls to the target, but content
    // that renders AFTER the click shifts the layout out from under the
    // in-flight scroll: the status poll / SSE push re-renders every section
    // above the target, lazy panels and tables fill in, collapsed sections
    // resize, and ocFetchPane lands async pane content. The scroll then
    // finishes at a stale Y and the reader has to click the item again.
    // ocSettleScroll() re-anchors for a short settle window: on each
    // animation frame it recomputes the target's document Y and, if layout
    // drift moved it beyond a small tolerance, retargets the scroll (a repeat
    // smooth scroll redirects the in-flight one — no visible restart). The
    // window is hard-capped, and ANY user scroll input (wheel, touch,
    // scrollbar mousedown, keys) cancels correction immediately so manual
    // scrolling right after a click is never hijacked. While a section hold
    // is active, render()'s own scroll anchor pins that section too (see
    // captureScrollAnchor), so a repaint corrects instantly instead of
    // waiting for the next frame.
    const OC_SCROLL_SETTLE_MS = 700;   // agent-detail correction window after a click; capped so late shifts never fight the user
    const OC_NAV_SETTLE_MS = 2500;     // sidebar section window: covers the 3s status tick plus lazy panel loads
    const OC_SCROLL_TOLERANCE_PX = 4;  // re-anchor only when the target drifted beyond this
    const OC_SCROLL_CANCEL_EVENTS = ['wheel', 'touchstart', 'mousedown', 'keydown'];
    let _ocScrollSettleCancel = null;
    let _ocNavAnchorEl = null;

    function ocMaxScrollY() {
      const doc = document.documentElement;
      const full = Math.max(doc ? doc.scrollHeight : 0, document.body ? document.body.scrollHeight : 0);
      return Math.max(0, full - window.innerHeight);
    }
    // targetY(): the document Y the scroll should rest at; apply(): issues the
    // scroll. settleMs caps the correction window; anchorEl (optional) is
    // exposed to captureScrollAnchor for the duration of the hold.
    function ocSettleScroll(targetY, apply, settleMs, anchorEl) {
      if (_ocScrollSettleCancel) _ocScrollSettleCancel();
      const clampY = () => Math.max(0, Math.min(targetY(), ocMaxScrollY()));
      let lastTarget = clampY();
      apply();
      let cancelled = false;
      const cancel = () => {
        if (cancelled) return;
        cancelled = true;
        OC_SCROLL_CANCEL_EVENTS.forEach(t => window.removeEventListener(t, cancel));
        if (_ocScrollSettleCancel === cancel) _ocScrollSettleCancel = null;
        if (_ocNavAnchorEl === anchorEl) _ocNavAnchorEl = null;
      };
      // Passive listeners: cancellation must never delay the user's own
      // scrolling. Programmatic scrolls fire none of these events, so the
      // settle loop cannot cancel itself.
      OC_SCROLL_CANCEL_EVENTS.forEach(t => window.addEventListener(t, cancel, { passive: true }));
      _ocScrollSettleCancel = cancel;
      _ocNavAnchorEl = anchorEl || null;
      const deadline = performance.now() + settleMs;
      (function settle() {
        if (cancelled) return;
        const want = clampY();
        if (Math.abs(want - lastTarget) > OC_SCROLL_TOLERANCE_PX) {
          lastTarget = want;
          apply();
        }
        if (performance.now() > deadline) { cancel(); return; }
        requestAnimationFrame(settle);
      })();
    }

    function ocScrollToAgentDetail(behavior) {
      const detail = document.getElementById('oc-agent-detail');
      if (!detail) return;
      const mode = behavior || 'smooth';
      const targetY = () => Math.max(0, detail.getBoundingClientRect().top + window.scrollY - OC_TOPBAR_HEIGHT);
      ocSettleScroll(targetY, () => window.scrollTo({ top: targetY(), behavior: mode }), OC_SCROLL_SETTLE_MS, null);
    }

    function ocSelectAgent(name) {
      _ocSelectedAgent = name;
      history.replaceState(null, '', window.hiveURLWithHash ? window.hiveURLWithHash(name ? '#' + name : '') : (name ? '#' + name : location.pathname));
      document.querySelectorAll('.oc-nav-item').forEach(i => i.classList.remove('active'));
      const active = document.querySelector(`.oc-nav-item[data-agent-nav="${name}"]`);
      if (active) active.classList.add('active');
      if (window._lastAgents) renderAgents(window._lastAgents);
      ocRenderAgentDetail();
      ocUpdateFocusedState();
      if (name) { ocStartPanePoll(); } else { ocStopPanePoll(); }
      ocScrollToAgentDetail('smooth');
    }

    function ocFormatSummary(raw) {
      const lines = (raw || '').split('\n');
      const out = [];
      let tableRows = [];
      let tableCols = [];
      let cmdBlock = [];

      function flushTable() {
        if (!tableRows.length) return;
        let html = '<div class="sum-table-wrap"><table class="sum-table">';
        if (tableCols.length) html += '<thead><tr>' + tableCols.map(c => '<th>' + esc(c) + '</th>').join('') + '</tr></thead>';
        html += '<tbody>' + tableRows.map(r => '<tr>' + r.map(c => '<td>' + esc(c) + '</td>').join('') + '</tr>').join('') + '</tbody></table></div>';
        out.push(html);
        tableRows = [];
        tableCols = [];
      }

      function flushCmd() {
        if (!cmdBlock.length) return;
        out.push('<div class="sum-cmd">' + cmdBlock.map(l => esc(l)).join('\n') + '</div>');
        cmdBlock = [];
      }

      const TOOL_RE = /^(.+?)\s*\((shell|Read|Edit|Write|Bash|WebSearch|WebFetch|Agent|NotebookEdit)\)\s*$/;
      const TABLE_ROW_RE = /^\|\s*(.+?)\s*\|$/;
      const TABLE_SEP_RE = /^[\|+][\-─━=+\|]+[\|+]$/;
      const PIPE_LINE_RE = /^[│|]\s*(.*)$/;
      const TREE_LINE_RE = /^[└├╰╭─\s]*[└├╰]\s*(.*)$/;

      for (let i = 0; i < lines.length; i++) {
        const line = lines[i];
        const trimmed = line.trim();
        if (!trimmed) { flushCmd(); flushTable(); continue; }

        const toolMatch = trimmed.match(TOOL_RE);
        if (toolMatch) {
          flushCmd();
          flushTable();
          out.push('<div class="sum-tool"><span class="sum-tool-type">' + esc(toolMatch[2]) + '</span>' + esc(toolMatch[1]) + '</div>');
          continue;
        }

        if (TABLE_SEP_RE.test(trimmed.replace(/\s/g, ''))) {
          continue;
        }

        const tableMatch = trimmed.match(TABLE_ROW_RE);
        if (tableMatch) {
          flushCmd();
          const cells = tableMatch[1].split('|').map(c => c.trim());
          if (!tableCols.length && !tableRows.length) {
            tableCols = cells;
          } else {
            tableRows.push(cells);
          }
          continue;
        }

        if (tableRows.length || tableCols.length) flushTable();

        const pipeMatch = trimmed.match(PIPE_LINE_RE);
        if (pipeMatch) {
          cmdBlock.push(pipeMatch[1] || '');
          continue;
        }

        const treeMatch = trimmed.match(TREE_LINE_RE);
        if (treeMatch) {
          flushCmd();
          out.push('<div class="sum-tree">↳ ' + esc(treeMatch[1]) + '</div>');
          continue;
        }

        flushCmd();
        out.push('<div class="sum-text">' + esc(trimmed) + '</div>');
      }
      flushCmd();
      flushTable();
      return out.join('');
    }

    let _ocSummaryTailing = true;
    let _ocSummaryScrollTop = null;

    // One human-readable sentence answering WHO paused this agent, WHY and
    // WHEN — e.g. "paused by bketelsen via dashboard, 3d ago" or
    // "login-detector: login required detected, 2h ago". #4041: without
    // this, a deliberately owner-quiesced fleet was indistinguishable from
    // a malfunctioning one days later.
    function pauseProvenanceLine(a) {
      const trigger = a.pausedTrigger || '';
      const ago = a.pausedAt ? pillAge(a.pausedAt) : '';
      let line;
      if (trigger === 'dashboard-api') {
        line = 'paused by ' + (a.pausedBy || 'an operator') + ' via dashboard';
      } else if (trigger) {
        line = trigger + ': ' + (a.pausedReason || 'paused');
      } else {
        line = a.pausedReason || 'paused';
      }
      return ago ? line + ', ' + ago : line;
    }

    function pauseInfoIcon(a) {
      // A manual/persisted pause carries its reason/trigger even when the
      // current governor mode's cadence is also pause/off — the operator
      // pause is the state that needs explaining, so never hide the icon
      // behind offByCadence.
      if (!a.paused) return '';
      const reason = a.pausedReason || 'unknown';
      const trigger = a.pausedTrigger || 'unknown';
      const by = a.pausedBy || '';
      const at = a.pausedAt ? formatDashboardDateTime(a.pausedAt) : 'unknown';
      const ago = a.pausedAt ? pillAge(a.pausedAt) : '';
      return `<span class="pause-info" title="${esc(pauseProvenanceLine(a))}">ℹ️<span class="pause-tooltip">${esc(pauseProvenanceLine(a))}<br>Reason: ${esc(reason)}<br>Trigger: ${esc(trigger)}${by ? '<br>By: ' + esc(by) : ''}<br>Since: ${esc(at)}${ago ? ' (' + esc(ago) + ')' : ''}</span></span>`;
    }

    function subAgentModelsHtml(a) {
      const rows = a.subAgentModels || [];
      if (!rows.length) return '';
      return `<div class="oc-subagent-models"><strong>Observed sub-agent models</strong><ul>${rows.map(row =>
        `<li>${esc(row.agentType)} → <code>${esc(row.model)}</code>${row.olderGeneration ? ' <span class="text-warn" title="Older model generation than the parent launch model">⚠ older generation</span>' : ''}</li>`
      ).join('')}</ul></div>`;
    }

    // Volatile stat fields of the focused agent-detail panel, factored out so
    // the fast path can refresh them on every status payload. Everything shown
    // here (pause/interval state, last/next run, restarts, tokens, auth) goes
    // permanently stale while an agent is focused otherwise.
    function ocDetailFieldsHtml(a, isPaused, isOff, isOnDemand) {
      const _tok = (window._tokensByAgent || {})[a.name] || {};
      const _tokTotal = (_tok.input || 0) + (_tok.output || 0) + (_tok.cacheRead || 0);
      const _tokAvg = _tok.avgPerSession || (_tok.sessions > 0 ? Math.floor(_tokTotal / _tok.sessions) : 0);
      return `
        <div class="oc-detail-fields">
          <div class="oc-detail-field"><div class="label" title="${isInferenceBackendName(a.cli) ? 'Inference backend (self-hosted model server)' : 'The CLI backend running this agent (e.g. claude, copilot, gemini)'}">${isInferenceBackendName(a.cli) ? 'Method' : 'CLI'}</div><div class="value">${isInferenceBackendName(a.cli) ? backendLabel(a.cli) : esc(a.cli || '—')}${(a.pinnedBoth || a.pinnedCli) ? ` <span data-action="togglePin" data-agent="${esc(a.name)}" data-pin-type="cli" data-unpin="true" style="cursor:pointer" title="Unpin CLI">📌</span>` : ` <span data-action="togglePin" data-agent="${esc(a.name)}" data-pin-type="cli" data-unpin="false" style="cursor:pointer;opacity:0.3" title="Pin CLI">📌</span>`}</div></div>
          <div class="oc-detail-field"><div class="label" title="The LLM model powering this agent. Governor may change it based on budget.">Model</div><div class="value">${esc(a.model || '—')}${(a.pinnedBoth || a.pinnedModel) ? ` <span data-action="togglePin" data-agent="${esc(a.name)}" data-pin-type="model" data-unpin="true" style="cursor:pointer" title="Unpin model">📌</span>` : ` <span data-action="togglePin" data-agent="${esc(a.name)}" data-pin-type="model" data-unpin="false" style="cursor:pointer;opacity:0.3" title="Pin model">📌</span>`}</div></div>
          <div class="oc-detail-field"><div class="label" title="Time between governor kicks in the current mode. Configured per mode (idle/quiet/busy/surge).">Interval</div><div class="value">${isOnDemand ? 'on demand' : isPaused ? 'paused' : isOff ? esc(agentCadenceOffLabel()) : esc(agentCadenceDisplay(a))}</div></div>
          <div class="oc-detail-field"><div class="label" title="When the governor last kicked (sent a work prompt to) this agent. A kicked work pass often keeps running long after this time — terminal activity between kicks is normally the previous pass still working, not an off-schedule run.">Last Kick</div><div class="value">${esc(agentKickDisplay(a.lastKick, a.lastKickAt))}</div></div>
          <div class="oc-detail-field"><div class="label" title="Which house style the last kick carried: repo (HIVE_STYLE.md in the managed repo), hive (project.style / style_file) or built-in.">Style</div><div class="value">${styleSourceHtml(a)}</div></div>
          <div class="oc-detail-field"><div class="label" title="When the governor will next kick this agent based on its cadence interval. If the previous pass is still running at that time, the kick waits for the CLI's input dialog.">Next Kick</div><div class="value">${isOnDemand ? 'on demand' : isPaused ? 'paused' : isOff ? esc(agentCadenceOffLabel()) : esc(agentKickDisplay(a.nextKick, a.nextKickAt))}</div></div>
          <div class="oc-detail-field"><div class="label" title="Total tokens consumed by this agent in the last 24 hours across all sessions.">Tokens (24h)</div><div class="value">${_tokTotal > 0 ? fmtTokens(_tokTotal) : (_tok.sessions > 0 ? _tok.sessions + ' sess' : '—')}</div></div>
          <div class="oc-detail-field"><div class="label" title="Number of times this agent has been restarted in the last 24 hours. High counts may indicate instability.">Restarts</div><div class="value${(a.restarts || 0) > 5 ? ' restart-high' : (a.restarts || 0) > 0 ? ' restart-warn' : ''}">${a.restarts ?? 0}<span class="restart-label">24h</span>${(a.restarts || 0) > 0 ? ` <button class="restart-reset" data-action="resetRestarts" data-agent="${esc(a.name)}" title="Reset the restart counter to zero — does not affect the agent">✕</button>` : ''}<span class="restart-spark">${restartSparkSvg(a.name)}</span></div></div>
          ${a.sessionRssMb ? `<div class="oc-detail-field"><div class="label" title="Resident memory of this agent's CLI process tree (MiB) and how long the current CLI session has been up. Idle sessions over the agents.session_recycle RSS or age budget are recycled through a normal restart.">Session RSS</div><div class="value">${a.sessionRssMb} MiB${a.sessionAge ? ` <span class="restart-label">${esc(a.sessionAge)}</span>` : ''}${a.sessionRecycles ? ` <span class="restart-label" title="Sessions recycled over the RSS/age budget since boot">↻${a.sessionRecycles}</span>` : ''}</div></div>` : ''}
          <div class="oc-detail-field"><div class="label" title="Average tokens consumed per work pass (kick session). Lower is more efficient.">Avg/Pass</div><div class="value">${_tokAvg > 0 ? fmtTokens(_tokAvg) : '—'}</div></div>
          <div class="oc-detail-field"><div class="label" title="Number of separate CLI sessions this agent has had in the last 24 hours.">Sessions</div><div class="value">${_tok.sessions ?? '—'}</div></div>
          <div class="oc-detail-field"><div class="label" title="Total number of messages (prompts + responses) across all sessions in the last 24 hours.">Messages</div><div class="value">${_tok.messages ?? '—'}</div></div>
          <div class="oc-detail-field"><div class="label" title="CLI authentication status — log in to authenticate the agent's CLI tool">Auth</div><div class="value">${(() => {
            const isClaude = a.cli === 'claude';
            const isCopilot = a.cli === 'copilot';
            const _needsLogin = _agentNeedsLogin(a);
            const claudeIcon = '<span class="cli-icon claude-icon"></span>';
            const copilotIcon = '<span class="cli-icon copilot-icon"></span>';
            if (_needsLogin) {
              if (isClaude) return '<button class="hv-btn btn-primary btn-sm cli-login-btn claude" data-action="loginAgent" data-arg0="' + esc(a.name) + '" data-arg1="claude" data-stop="1" title="Authenticate Claude Code CLI">' + claudeIcon + ' Login</button>';
              if (isCopilot) return '<button class="hv-btn btn-primary btn-sm cli-login-btn copilot" data-action="loginAgent" data-arg0="' + esc(a.name) + '" data-arg1="copilot" data-stop="1" title="Authenticate GitHub Copilot CLI">' + copilotIcon + ' Login</button>';
              return '<button class="hv-btn btn-secondary btn-sm cli-login-btn disabled" disabled title="This backend requires manual /login in the terminal">Login</button>';
            }
            if (isClaude) return '<button class="hv-btn btn-secondary btn-sm cli-login-btn logged-in" data-action="loginAgent" data-arg0="' + esc(a.name) + '" data-arg1="claude" data-stop="1" title="Re-authenticate Claude Code CLI">' + claudeIcon + ' ✓ logged in</button>';
            if (isCopilot) return '<button class="hv-btn btn-secondary btn-sm cli-login-btn logged-in" data-action="loginAgent" data-arg0="' + esc(a.name) + '" data-arg1="copilot" data-stop="1" title="Re-authenticate GitHub Copilot CLI">' + copilotIcon + ' ✓ logged in</button>';
            return '<button class="hv-btn btn-secondary btn-sm cli-login-btn disabled" disabled title="This backend requires manual /login in the terminal">✓ logged in</button>';
          })()}</div></div>
        </div>
      `;
    }

    function ocRenderAgentDetail() {
      const detail = document.getElementById('oc-agent-detail');
      if (!detail || !_ocSelectedAgent) return;
      const agents = window._lastAgents || [];
      const a = agents.find(ag => ag.name === _ocSelectedAgent);
      if (!a) { detail.classList.remove('active'); return; }
      detail.classList.add('active');

      const isOnDemand = a.onDemand === true;
      // Manual/persisted pause WINS over the governor's per-mode cadence.
      // The old `paused && !offByCadence` derivation let an operator-paused
      // agent whose current-mode cadence was also pause/off fall through to
      // the off-healthy (hollow GREEN) styling with a "▶ start" button —
      // the operator could not see it was paused, and the toggle then sent
      // a no-op re-pause instead of the intended resume.
      const isPaused = a.paused === true && !isOnDemand;
      const isOff = agentHeldByMode(a) && !isPaused && !isOnDemand;
      // Down = process not alive ('stopped', 'failed', …) — anything but
      // 'running'. Matching only 'stopped' let 'failed' agents render the
      // green idle styling here (#2465).
      // Post-restart boot stagger / launch in progress: the agent is not down,
      // it just hasn't reached its pane yet. Render as 'starting', not red.
      const isStarting = !!a.starting && a.state !== 'running' && !isPaused && !isOff;
      // Sandboxed agents: down = last run failed / cadence missed, never
      // "no session" (agentSessionDown).
      const isStopped = agentSessionDown(a) && !isPaused && !isOff && !isOnDemand && !isStarting;
      // Same formula as the Auth section's Login button below: a down agent
      // with an unauthenticated CLI gets the amber needs-login state instead
      // of plain down-red (#2465).
      /* needs-login applies to a STOPPED agent with any unauthenticated
         signal, and ALSO to a RUNNING agent whose pane shows a login prompt
         (a.needsLogin) — alive-but-wedged must not render as healthy. The
         weaker probe-based inference stays stopped-only so a stale probe
         cannot repaint a genuinely working agent. */
      const needsLoginDown = _agentNeedsLogin(a, isStopped);
      const isRunning = a.busy === 'working' && !isPaused && !isOff && !isStopped;
      const dotCls = needsLoginDown ? 'needs-login' : isStarting ? 'starting' : isStopped ? 'stopped' : isOnDemand ? 'on-demand' : isPaused ? 'paused' : isOff ? 'off' : isRunning ? 'running' : 'idle';
      // A healthy agent that is off *because of its governor cadence* (offByCadence,
      // i.e. cadence==pause/off for the current mode) reaches the isOff branch above
      // only after the stopped / needs-login / on-demand / manual-paused guards, so
      // it is provably alive. Render its DOT as hollow green (off-healthy) instead of
      // grey 'off' to show "alive but the governor won't kick it" — a solid green dot
      // misled operators into thinking a surge-paused agent was running. The border/
      // state class (dotCls) stays 'off' so the panel border rule still applies.
      const dotState = isOff ? 'off-healthy' : dotCls;
      const dotTitle = isOff ? agentOffTitle(a) : '';

      detail.className = 'oc-agent-detail active state-' + dotCls;
      detail.dataset.status = statusAttr(dotCls);
      /* 'disabled' outranks 'down': both reach here with state!=='running',
         but a switched-off agent is not a fault, and labelling it "down" sent
         operators looking for a crash that never happened. */
      const stateLabel = agentSessionUnavailable(a) ? 'disabled' : needsLoginDown ? 'needs login' : isStopped ? (agentSandboxDownLabel(a) || (a.operatorStopped ? 'stopped by operator' : 'down')) : isOnDemand ? 'on demand' : isPaused ? 'paused' : isOff ? agentCadenceOffLabel() : isRunning ? (a.awaitingCI ? 'Waiting on CI' : 'working') : 'idle';

      const kickId = `oc-kick-${a.name}`;
      const prevInput = document.getElementById(kickId);
      const prevVal = prevInput ? prevInput.value : '';

      const existingSummary = detail.querySelector('.oc-detail-summary');
      if (existingSummary && !_ocSummaryTailing) {
        _ocSummaryScrollTop = existingSummary.scrollTop;
      }

      // Fast path: if the detail panel already shows this agent, update only the
      // summary text + status dot to avoid full DOM rebuild (prevents scroll jumps).
      // Only update summary when detailSummary is available — never replace long
      // ring buffer content with short liveSummary fallback.
      // The fingerprint guards the fields only the full template renders
      // (method/model/pins): without it a CLI or model switch stayed stale
      // in the focused panel until a hard refresh.
      const cfgSig = [a.cli || '', a.model || '', a.pinnedCli ? 1 : 0, a.pinnedModel ? 1 : 0, a.pinnedBoth ? 1 : 0, a.displayName || '', a.emoji || '', a.color || '', JSON.stringify(a.subAgentModels || [])].join('|');
      if (existingSummary && detail.dataset.agent === a.name && detail.dataset.cfgSig === cfgSig) {
        if (a.detailSummary) {
          const newText = escBlock(a.detailSummary);
          if (existingSummary.innerHTML !== newText) {
            existingSummary.innerHTML = newText;
          }
        }
        if (_ocSummaryTailing) {
          existingSummary.scrollTop = existingSummary.scrollHeight;
        }
        const dot = detail.querySelector('.status-dot');
        if (dot) { dot.className = 'status-dot ' + dotState; dot.dataset.status = statusAttr(dotState); dot.title = dotTitle; }
        const stateSpan = detail.querySelector('.oc-agent-detail-header > span:last-child');
        if (stateSpan) { stateSpan.innerHTML = stateLabel + pauseInfoIcon(a); }
        // The pause/resume toggle must track state here too — the fast path
        // runs on every status poll while an agent is focused, so a stale
        // button would otherwise persist until the user navigates away.
        const fastToggle = detail.querySelector('.oc-detail-actions .btn-toggle');
        if (fastToggle && !isOnDemand) {
          if (isPaused) {
            // Paused gets the full combined-action rebuild — '▶ start &
            // resume' when the session is also down — so data-action tracks
            // the session state too, not just the label (#5594 follow-up).
            fastToggle.outerHTML = pausedAgentActionHtml(a);
          } else if (isOff) {
            // Cadence hold — same routing as the full render. This used to
            // rebuild a '▶ start' wired to toggleAgent, which PAUSED the agent.
            fastToggle.outerHTML = cadenceOffAgentActionHtml(a);
          } else {
            fastToggle.className = 'hv-btn btn-secondary btn-sm btn-toggle running';
            fastToggle.textContent = '⏸ pause';
            fastToggle.dataset.action = 'toggleAgent';
            fastToggle.dataset.paused = 'false';
            fastToggle.title = agentToggleTitle(a, false, false);
          }
        }
        // The master-power switch tracks `enabled` on every poll too, so an
        // enablement change made elsewhere (config dialog, another browser,
        // hive.yaml reload) is reflected without leaving the panel.
        const fastPwr = detail.querySelector('.pwr-switch');
        if (fastPwr) fastPwr.outerHTML = agentPowerSwitchHtml(a);
        // Same for the blockers line, the stat fields and the indicators — they
        // live outside the scroll region, so replacing them wholesale cannot
        // cause the scroll jumps the fast path exists to prevent.
        const nowEl = detail.querySelector('[data-agent-now]');
        if (nowEl) nowEl.outerHTML = agentNowHtml(a);
        const blockersEl = detail.querySelector('.oc-detail-blockers');
        if (blockersEl) {
          blockersEl.innerHTML = agentBlockerLineHtml(a) + neverScheduledChipHtml(a, a.name);
          blockersEl.insertAdjacentHTML('beforeend', repeatFailureLineHtml(a));
        }
        const fieldsEl = detail.querySelector('.oc-detail-fields');
        if (fieldsEl) fieldsEl.outerHTML = ocDetailFieldsHtml(a, isPaused, isOff, isOnDemand);
        const indEl = detail.querySelector('.oc-detail-indicators');
        if (indEl) indEl.innerHTML = agentIndicators(a.name);
        detail.className = 'oc-agent-detail active state-' + dotCls;
        detail.dataset.status = statusAttr(dotCls);
        return;
      }
      detail.dataset.agent = a.name;
      detail.dataset.cfgSig = cfgSig;

      const gov = window._lastStatus?.governor || {};
      const govMode = (gov.mode || 'unknown').toLowerCase();
      const govModeClass = 'mode-' + (['idle','quiet','busy','surge'].includes(govMode) ? govMode : 'quiet');
      const govActionable = (window._lastStatus?.repos || []).reduce((n, r) => n + (r.actionableIssues || []).length, 0);
      const govPrs = (window._lastStatus?.repos || []).reduce((n, r) => n + (r.openPrs || []).length, 0);

      const govOuter = document.getElementById('oc-gov-strip-outer');
      if (govOuter) {
        const itm = window._lastStatus?.issueToMerge || {};
        const itmMedian = itm.median_minutes || 0;
        const govThresh = gov.thresholds || {};
        const OC_THRESH_QUIET = govThresh.quiet ?? 2;
        const OC_THRESH_BUSY = govThresh.busy ?? 10;
        const OC_THRESH_SURGE = govThresh.surge ?? 20;
        // Mode pressure = actionable issues + open PRs, matching the
        // governor's computeMode input — an issues-only needle read "idle"
        // while a 20+ PR backlog sat unmerged.
        const govPressure = govActionable + govPrs;
        const OC_GAUGE_MAX = Math.max(OC_THRESH_SURGE + 10, govPressure + 5);
        const gaugePct = Math.min(govPressure / OC_GAUGE_MAX * 100, 100);
        govOuter.innerHTML = `
          <div class="gov-metric"><span class="gm-label">timer</span><span class="gm-val text-success">${gov.active !== false ? '● active' : '○ off'}</span></div>
          <div class="gov-metric"><span class="gm-label">mode</span><span class="gm-val ${govModeClass}">${esc(gov.mode || '—')}</span></div>
          <div class="gov-metric"><span class="gm-label">actionable</span><span class="gm-val">${govActionable}</span></div>
          <div class="gov-metric"><span class="gm-label">PRs</span><span class="gm-val">${govPrs}</span></div>
          <div class="gov-metric"><span class="gm-label">MTTR</span><span class="gm-val">${formatFixTime(itmMedian)}</span></div>
          ${fleetRunStripHtml()}
          <div class="gov-metric" title="${esc((window._lastStatus?.hold?.items || []).map(h => (h.type === 'pr' ? 'PR' : 'Issue') + ' ' + h.repo + '#' + h.number + (h.review_class ? ' [' + h.review_class + ']' : '') + ': ' + h.title).join('\n') || 'No items on hold')}"><span class="gm-label">on hold</span><span class="gm-val">${window._lastStatus?.hold?.total || 0}</span></div>
          ${standbyPauseFactsHtml(gov)}
          ${(() => {
            const plan = window._lastStatus?.planning || {};
            // Planning only runs at ACMM >= 5 (architect scheduled); hide the tile
            // entirely below that so it never shows a misleading empty state.
            if (!plan.available) return '';
            const active = plan.active_plans || 0;
            const awaiting = plan.awaiting_review || 0;
            const pending = plan.pending_decompose || 0;
            const stuck = plan.stuck_decompose || 0;
            const paused = !!plan.architect_paused;
            // Highlight when plans await human review — a human-action-required
            // state, surfaced amber like the "N Issues" / ON HOLD indicators.
            // Stuck plans (architect kicked its full budget, no children) are
            // human-action-required too (hivecommons/hive#8010).
            const planClass = (awaiting > 0 || paused || stuck > 0) ? 'gm-alert' : '';
            let planText = `${active}`;
            if (awaiting > 0) planText = `${active} (${awaiting} review)`;
            if (stuck > 0) planText = `${planText} ⚠${stuck}`;
            if (paused) planText = `⏸ ${planText}`;
            let planTitle = active > 0 ? `${active} active plan(s)` : '';
            if (awaiting > 0) planTitle = `${awaiting} plan(s) awaiting human review — decomposed epic children are withheld until approved`;
            if (paused) planTitle = `⏸ Architect is paused — ${pending} queued plan(s) won't be built until you resume the architect agent.`;
            else if (pending > 0) planTitle = `${pending} plan(s) queued for the architect to decompose`;
            if (stuck > 0) planTitle = `⚠ ${stuck} plan(s) stuck — the architect was asked ${stuck === 1 ? 'for it' : 'for them'} repeatedly and produced no task list. Open the plan view and click Plan on the issue again to retry.` + (planTitle ? ` ${planTitle}.` : '');
            // Empty-state discoverability hint when there is no planning activity.
            // Below ACMM L5 the architect that decomposes epics has no cadence, so
            // planning is unavailable — say so instead of pointing at a button we hide.
            if (active === 0 && pending === 0 && stuck === 0) {
              planTitle = ((window._lastStatus?.acmmLevel || 0) >= 5)
                ? 'Break a big issue into a plan → click ⧉ Plan on any issue, or add the `plan` label on GitHub.'
                : 'Planning needs ACMM level 5+ — the architect that decomposes plans is only scheduled at L5/L6. Raise the level to enable ⧉ Plan and the `plan` label.';
            }
            // Name WHICH plans are waiting on a person, not just how many:
            // the tooltip is the fastest answer to "what does the hive need
            // from me right now?" (hivecommons/hive#8011).
            const waiting = planWaitingLines(plan);
            if (waiting) planTitle = planTitle + '\n\nNeeds you:\n' + waiting;
            return `<div class="gov-metric" style="cursor:pointer" data-action="openPlanList" title="${esc(planTitle + ' — click to open the plan view')}"><span class="gm-label">planning</span><span class="gm-val ${planClass}">${esc(planText)}${pending > 0 ? ` <span style="opacity:0.7;font-size:0.85em">+${pending} queued</span>` : ''}</span></div>`;
          })()}
          <div class="oc-gov-gauge">
            <div class="temp-gauge-track" style="background:linear-gradient(to right, var(--green) 0%, var(--green) ${OC_THRESH_QUIET/OC_GAUGE_MAX*100}%, var(--blue) ${OC_THRESH_QUIET/OC_GAUGE_MAX*100}%, var(--blue) ${OC_THRESH_BUSY/OC_GAUGE_MAX*100}%, var(--yellow) ${OC_THRESH_BUSY/OC_GAUGE_MAX*100}%, var(--yellow) ${OC_THRESH_SURGE/OC_GAUGE_MAX*100}%, var(--red) ${OC_THRESH_SURGE/OC_GAUGE_MAX*100}%, var(--red) 100%)">
              <div class="temp-gauge-glow" style="width:100%"></div>
              <div class="temp-gauge-ticks"><span style="left:2%;-webkit-transform:translate(0,-50%);transform:translate(0,-50%)">0</span><span style="left:${OC_THRESH_QUIET/OC_GAUGE_MAX*100}%;-webkit-transform:translate(-50%,-50%);transform:translate(-50%,-50%)">${OC_THRESH_QUIET}</span><span style="left:${OC_THRESH_BUSY/OC_GAUGE_MAX*100}%;-webkit-transform:translate(-50%,-50%);transform:translate(-50%,-50%)">${OC_THRESH_BUSY}</span><span style="left:${OC_THRESH_SURGE/OC_GAUGE_MAX*100}%;-webkit-transform:translate(-50%,-50%);transform:translate(-50%,-50%)">${OC_THRESH_SURGE}</span><span style="left:98%;-webkit-transform:translate(-100%,-50%);transform:translate(-100%,-50%)">${OC_GAUGE_MAX}</span></div>
              <div class="temp-gauge-needle" style="left:${gaugePct}%;--gauge-position:${gaugePct}%" data-val="${govPressure}"></div>
            </div>
            <div class="temp-gauge-labels">
              <span class="tl-idle" style="position:absolute;left:${OC_THRESH_QUIET / OC_GAUGE_MAX * 50}%;transform:translateX(-50%)">idle</span>
              <span class="tl-quiet" style="position:absolute;left:${(OC_THRESH_QUIET + OC_THRESH_BUSY) / 2 / OC_GAUGE_MAX * 100}%;transform:translateX(-50%)">quiet</span>
              <span class="tl-busy" style="position:absolute;left:${(OC_THRESH_BUSY + OC_THRESH_SURGE) / 2 / OC_GAUGE_MAX * 100}%;transform:translateX(-50%)">busy</span>
              <span class="tl-surge" style="position:absolute;left:${(OC_THRESH_SURGE + OC_GAUGE_MAX) / 2 / OC_GAUGE_MAX * 100}%;transform:translateX(-50%)">surge</span>
            </div>
          </div>
        `;
      }

      const _ocSparkCounts = {
        actionable: govActionable,
        openPrs: govPrs,
        mergeable: (window._lastStatus?.repos || []).reduce((n, r) => n + (r.openPrs || []).filter(prMergeable).length, 0),
      };

      detail.innerHTML = `
        <div class="oc-agent-detail-header">
          <span class="status-dot ${dotState}" data-status="${statusAttr(dotState)}"${dotTitle ? ` title="${esc(dotTitle)}"` : ''}></span>
          <span class="agent-name-big">${esc(a.displayName || a.name)}</span>${a.sandboxed ? ' ' + statusBadgeHTML('ready', 'sandboxed', 'Sandboxed runner active', 'done') : ''}${needsLoginDown ? '<span title="CLI needs login" style="margin-left:var(--sp-3)">\uD83D\uDD11</span>' : ''}
          ${agentRunLineHtml(a)}
          <span style="font-size:0.78rem;color:var(--muted);margin-left:auto">${stateLabel}${pauseInfoIcon(a)}</span>
        </div>
        <div class="oc-detail-actions">
          ${agentIsDisabled(a)
            ? ''
            : isOnDemand
            ? '<button class="hv-btn btn-secondary btn-sm btn-toggle on-demand" title="On-demand agent — kicked manually, not by governor cadence">⚡ on demand</button>'
            : !dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')
            ? ''
            : isPaused
            ? pausedAgentActionHtml(a)
            : isOff
            ? cadenceOffAgentActionHtml(a)
            : `<button class="hv-btn btn-secondary btn-sm btn-toggle running" data-action="toggleAgent" data-agent="${esc(a.name)}" data-paused="false" title="${esc(agentToggleTitle(a, false, false))}">⏸ pause</button>`}
          ${agentPowerSwitchHtml(a)}
          ${sessionChip(a, '▶ terminal', `<a href="${terminalUrl(a.name, a.sandboxRuntime)}" target="_blank" rel="noopener" class="hv-btn btn-secondary btn-sm terminal-link" data-action="openTerminal" data-agent="${esc(a.name)}" title="${esc(terminalLinkTitle(a, 'Open the live tmux terminal session for this agent in a new tab'))}">▶ terminal</a>`)}
          ${sessionChip(a, '📄 full log', `<a href="${fullLogUrl(a.name, false)}" target="_blank" rel="noopener" class="hv-btn btn-secondary btn-sm terminal-link" data-action="openAuthenticatedLink" title="${esc(fullLogTitle(a, 'Open the full retained log of this agent\'s latest run as selectable, searchable text in a new tab'))}">📄 full log</a>`)}
          ${sessionChip(a, '📋 log text', `<a href="${fullLogUrl(a.name, false)}" class="hv-btn btn-secondary btn-sm terminal-link" data-action="openAuthenticatedLink" title="${esc(fullLogTitle(a, 'Open the full retained log in the in-app viewer for copying'))}">📋 log text</a>`)}
          <a href="${kickHistoryUrl(a.name)}" target="_blank" rel="noopener" class="hv-btn btn-secondary btn-sm terminal-link" data-action="openAuthenticatedLink" title="Browse archived logs of this agent's previous kicks — each run is archived when its turn ends; history survives restarts and hive upgrades">🕘 past kicks</a>
          <button class="hv-btn btn-danger btn-sm restart-btn" data-action="restartAgent" data-agent="${esc(a.name)}" title="Kill this agent's tmux session — supervisor will respawn it with fresh context">↻ restart</button>
          <button class="hv-btn config-gear btn-primary" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(a.name)}" title="Open configuration dialog for this agent (cadences, model, pipeline, hooks, etc.)">⚙️</button>
          <button class="hv-btn btn-secondary btn btn-primary" data-action="kickEmpty" data-agent="${esc(a.name)}" title="Kick this agent now with its default prompt, bypassing the governor cadence timer">kick</button>
          <select class="hv-btn btn-secondary btn backend-select" data-change-action="switchCli" data-agent="${esc(a.name)}" title="Switch this agent to a different method (CLI or inference backend)">
            <option value="" disabled selected>${a.cli ? esc(backendLabel(a.cli)) + ' ▾' : 'method ▾'}</option>
            ${backendOptionList().map(b => `<option value="${b}" ${a.cli === b ? 'disabled' : ''}>${backendLabel(b)}</option>`).join('')}
          </select>
          <select class="hv-btn btn-secondary btn backend-select" data-change-action="switchModel" data-agent="${esc(a.name)}" title="Switch this agent to a different LLM model">
            <option value="" disabled${a.model ? '' : ' selected'}>model ▾</option>
            ${modelOptionsHtml(a.cli, a.model)}
          </select>
          ${effortSelectHtml(a)}
        </div>
        ${agentNowHtml(a)}
        <div class="oc-detail-blockers">${agentBlockerLineHtml(a)}${neverScheduledChipHtml(a, a.name)}${repeatFailureLineHtml(a)}</div>
        ${terminalCopyHintHtml(needsLoginDown, agentSessionUnavailable(a), a.name, a.loginURL, a.backendAuthStatus, a.backendAuthLastError, a.statusEvidence, a.inferenceLastError)}
        ${ocDetailFieldsHtml(a, isPaused, isOff, isOnDemand)}
        ${subAgentModelsHtml(a)}
        <div class="oc-detail-indicators">${agentIndicators(a.name)}</div>
        <div class="oc-detail-summary-wrap">
          <div class="oc-detail-summary" id="oc-summary-scroll">${(a.detailSummary || a.liveSummary) ? escBlock(a.detailSummary || a.liveSummary) : '<span class="text-muted">No activity summary</span>'}</div>
          <button class="oc-summary-follow-btn" id="oc-summary-follow" title="Follow new output" data-action="ocSummaryResumeTail">⬇</button>
        </div>
        <div class="oc-chat-prompt">
          <input type="text" class="oc-chat-input" id="${kickId}" placeholder="Send a message to ${esc(a.name)}..." value="${prevVal.replace(/"/g, '&quot;')}" data-enter-action="ocSendKick" data-agent="${esc(a.name)}" />
          <button class="oc-chat-send" data-action="ocSendKick" data-agent="${esc(a.name)}">Send</button>
        </div>
      `;

      const summaryEl = document.getElementById('oc-summary-scroll');
      const followBtn = document.getElementById('oc-summary-follow');
      if (summaryEl) {
        const savedHeight = localStorage.getItem('hive-oc-summary-height');
        if (savedHeight) summaryEl.style.height = savedHeight;
        const resizeObs = new ResizeObserver(() => {
          localStorage.setItem('hive-oc-summary-height', summaryEl.style.height || summaryEl.offsetHeight + 'px');
        });
        resizeObs.observe(summaryEl);
        if (_ocSummaryTailing) {
          summaryEl.scrollTop = summaryEl.scrollHeight;
        } else if (_ocSummaryScrollTop !== null) {
          summaryEl.scrollTop = _ocSummaryScrollTop;
        }
        summaryEl.addEventListener('scroll', () => {
          const SCROLL_THRESHOLD = 40;
          const atBottom = summaryEl.scrollHeight - summaryEl.scrollTop - summaryEl.clientHeight < SCROLL_THRESHOLD;
          _ocSummaryTailing = atBottom;
          if (followBtn) followBtn.classList.toggle('visible', !atBottom);
        });
        const atBottom = summaryEl.scrollHeight - summaryEl.scrollTop - summaryEl.clientHeight < 40;
        if (followBtn && !atBottom && !_ocSummaryTailing) followBtn.classList.add('visible');
      }
    }

    function ocSummaryResumeTail() {
      _ocSummaryTailing = true;
      const el = document.getElementById('oc-summary-scroll');
      if (el) el.scrollTop = el.scrollHeight;
      const btn = document.getElementById('oc-summary-follow');
      if (btn) btn.classList.remove('visible');
    }

    async function ocSendKick(name) {
      const input = document.getElementById('oc-kick-' + name);
      if (!input) return;
      const prompt = input.value.trim();
      const opts = { method: 'POST', headers: { 'Content-Type': 'application/json' } };
      if (prompt) opts.body = JSON.stringify({ prompt });
      try {
        const data = await postJSON('/api/kick/' + name, opts);
        if (!data.ok) { showToast('Kick failed: ' + (data.error || 'unknown'), 'error'); return; }
        input.value = '';
        if (data.status === 'in-flight') {
          showToast('A kick is already being delivered to ' + name, 'info');
          return;
        }
        // 202 means queued, not delivered (#5325). Hold a persistent toast and
        // rewrite it once the poll settles, so the card never claims success
        // the server has not confirmed — nor failure it has not observed.
        const toast = showToast((prompt ? 'Sending to ' : 'Kicking ') + name + '...', 'info', true);
        await reportKickOutcome(name, toast, 'Queued for ' + name);
      } catch (e) { showToast('Kick failed: ' + e.message, 'error'); }
    }

    // Agent descriptions now come from the JSON status payload (agent.description field).
    // This empty map serves as a fallback for agents without a description in config.
    const AGENT_DESCRIPTIONS = {};

