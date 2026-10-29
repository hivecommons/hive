    // ---- Approvals desk (RFC #4000) -------------------------------------
    // Pending operator-lane approvals. The panel stays hidden entirely unless
    // the desk is enabled, so a hive that has not opted in sees no new chrome.
    //
    // Filter chips are rule names: an operator narrows the list to "everything
    // the large-diff rule would hold" and bulk-resolves just those. Each row
    // shows WHICH rule would resolve it, re-evaluated server-side against the
    // current rule set, so editing policy is visible immediately in the queue.
    let _approvalsSelected = new Set();
    let _approvalsRuleFilter = null;
    let _approvalsLast = null;

    async function fetchApprovals() {
      try {
        const r = await fetch('/api/approvals');
        if (!r.ok) return;               // 403 for non-owners: leave hidden
        renderApprovals(await r.json());
      } catch (e) { /* transient — leave last render in place */ }
    }

    function renderApprovals(dto) {
      refreshSectionCardShell('approvals-section');
      _approvalsLast = dto;
      const section = document.getElementById('approvals-section');
      if (!section) return;
      if (!dto || !dto.enabled) { section.style.display = 'none'; return; }
      section.style.display = '';

      const items = dto.items || [];

      // Badge: pending count, hidden at zero so a quiet desk is not noise.
      const badge = document.getElementById('approvals-badge');
      if (badge) {
        badge.textContent = String(dto.count || 0);
        badge.style.display = (dto.count > 0) ? '' : 'none';
      }

      // Requirement 4: items queued at L6 (Autonomous) are probable policy bugs.
      const bug = document.getElementById('approvals-policy-bug');
      if (bug) {
        if (dto.policy_bug_count > 0) {
          bug.style.display = '';
          bug.innerHTML = '⚠️ <strong>' + dto.policy_bug_count + '</strong> item(s) are waiting on a hive at ACMM L6 (Autonomous). ' +
            'An inbox that accumulates at L6 usually means a policy rule is over-restrictive, not that a human is needed.';
        } else {
          bug.style.display = 'none';
        }
      }

      // Rule chips as filters.
      const chips = document.getElementById('approvals-chips');
      if (chips) {
        const names = dto.rule_chips || [];
        let html = renderApprovalChip(null, 'All', _approvalsRuleFilter === null);
        for (const n of names) html += renderApprovalChip(n, n, _approvalsRuleFilter === n);
        chips.innerHTML = html;
      }

      const shown = _approvalsRuleFilter
        ? items.filter(i => i.rule === _approvalsRuleFilter)
        : items;

      // Drop selections for rows that no longer exist (resolved elsewhere).
      const live = new Set(items.map(i => i.id));
      for (const id of Array.from(_approvalsSelected)) if (!live.has(id)) _approvalsSelected.delete(id);

      const list = document.getElementById('approvals-list');
      const empty = document.getElementById('approvals-empty');
      if (empty) empty.style.display = shown.length ? 'none' : '';
      if (list) list.innerHTML = shown.map(renderApprovalRow).join('');

      updateApprovalsActions();
    }

    function renderApprovalChip(value, label, active) {
      const bg = active ? 'color-mix(in srgb, var(--accent) 25%, transparent)' : 'transparent';
      return '<button class="repo-chip-btn" style="background:' + bg + '" ' +
        'data-action="approvalsSetFilter" data-arg-types="j" data-arg0="' + escapeHtml(value === null ? 'null' : JSON.stringify(value)) + '">' +
        escapeHtml(label) + '</button>';
    }

    function renderApprovalRow(it) {
      const checked = _approvalsSelected.has(it.id) ? ' checked' : '';
      const target = it.repo ? (it.repo + (it.number ? '#' + it.number : '')) : (it.tool || '');
      const ruleChip = it.rule
        ? '<span style="padding:1px 6px;border-radius:8px;font-size:0.66rem;background:color-mix(in srgb, var(--accent) 18%, transparent)">' +
            escapeHtml(it.rule) + ' → ' + escapeHtml(it.rule_action || '') + '</span>'
        : '<span class="muted" style="font-size:0.66rem">base policy</span>';
      const bugMark = it.policy_bug ? ' <span title="Queued at ACMM L6 — probable policy bug">⚠️</span>' : '';

      return '<div style="display:flex;gap:var(--sp-4);align-items:flex-start;padding:7px 0;border-top:1px solid var(--border);font-size:var(--fs-sm)">' +
        '<input type="checkbox"' + checked + ' data-change-action="approvalsToggle" data-arg0="' + escapeHtml(it.id) + '">' +
        '<div style="flex:1;min-width:0">' +
          '<div><strong>' + escapeHtml(target) + '</strong>' + bugMark +
            ' <span class="muted">' + escapeHtml(it.kind || '') + '</span></div>' +
          '<div class="muted" style="font-size:0.7rem;margin-top:var(--sp-1)">' + escapeHtml(it.rationale || '') + '</div>' +
          '<div style="margin-top:3px">' + ruleChip + '</div>' +
        '</div>' +
        '<div style="display:flex;gap:5px">' +
          '<button class="hv-btn btn-secondary btn" data-action="approvalsResolveOne" data-arg-types="s,j" data-arg0="' + escapeHtml(it.id) + '" data-arg1="true">Approve</button>' +
          '<button class="hv-btn btn-secondary btn" data-action="approvalsResolveOne" data-arg-types="s,j" data-arg0="' + escapeHtml(it.id) + '" data-arg1="false">Reject</button>' +
        '</div>' +
      '</div>';
    }

    function updateApprovalsActions() {
      const actions = document.getElementById('approvals-actions');
      const count = document.getElementById('approvals-selected-count');
      if (!actions) return;
      actions.style.display = _approvalsSelected.size ? 'flex' : 'none';
      if (count) count.textContent = _approvalsSelected.size + ' selected';
    }

    function approvalsSetFilter(rule) {
      _approvalsRuleFilter = rule;
      if (_approvalsLast) renderApprovals(_approvalsLast);
    }

    function approvalsToggle(id) {
      if (_approvalsSelected.has(id)) _approvalsSelected.delete(id);
      else _approvalsSelected.add(id);
      updateApprovalsActions();
    }

    async function approvalsResolveOne(id, approved) {
      try {
        await fetch('/api/approvals/resolve', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ id: id, approved: approved })
        });
      } catch (e) { /* surfaced by the refetch below */ }
      _approvalsSelected.delete(id);
      fetchApprovals();
    }

    // Bulk resolve. The server decomposes this into N individual resolutions
    // through the same path the single-item button uses — this is a UI
    // convenience, never a separate decision path.
    async function approvalsResolveSelected(approved) {
      const ids = Array.from(_approvalsSelected);
      if (!ids.length) return;
      try {
        await fetch('/api/approvals/bulk', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ids: ids, approved: approved })
        });
      } catch (e) { /* surfaced by the refetch below */ }
      _approvalsSelected.clear();
      fetchApprovals();
    }

    // Prime both panels and poll them on a modest independent interval. Do not
    // block initial paint: these run after the main poll is already wired.
    function v4RefreshPanels() { fetchHiveAdvice(); fetchACMMReco(); fetchLifecycle(); fetchPRThroughput(); fetchApprovals(); }
    v4RefreshPanels();
    dashboardSetInterval('v4-panels', v4RefreshPanels, V4_PANELS_POLL_MS);

    // Cost range pills: switch hourly/daily/weekly/monthly and re-render the
    // trend chart from the already-fetched history (no network round-trip).
    document.addEventListener('click', (e) => {
      const btn = e.target.closest('[data-cost-range]');
      if (!btn) return;
      const r = btn.getAttribute('data-cost-range');
      if (!COST_RANGES[r] || r === _costRange) return;
      _costRange = r;
      try { localStorage.setItem(COST_RANGE_STORAGE_KEY, r); } catch {}
      if (_lastCost) renderCost(_lastCost);
      // Agent cards' spend chips follow this selector as their global filter.
      if (window._lastAgents) renderAgents(window._lastAgents);
    });
    // Cost range −/+ stepper: grow or shrink the active range's bucket count.
    document.addEventListener('click', (e) => {
      const btn = e.target.closest('[data-cost-step]');
      if (!btn || btn.disabled) return;
      if (!costStepCount(Number(btn.getAttribute('data-cost-step')))) return;
      if (_lastCost) renderCost(_lastCost);
      if (window._lastAgents) renderAgents(window._lastAgents);
    });

    // GitHub auth health — sticky banner when 401
    const GH_AUTH_POLL_MS = 30000;
    async function checkGhAuth() {
      try {
        const res = await fetch('/api/gh-auth');
        const data = await res.json();
        const el = document.getElementById('gh-auth-alert');
        if (data.ok) {
          el.className = 'gh-auth-alert';
        } else {
          el.className = 'gh-auth-alert active';
        }
      } catch (_) { /* dashboard unreachable — different problem */ }
    }
    checkGhAuth();
    dashboardSetInterval('gh-auth-banner', checkGhAuth, GH_AUTH_POLL_MS);

    function renderGhRateLimits(ghRateLimits) {
      const el = document.getElementById('gh-rate-alert');
      const alerts = (ghRateLimits && ghRateLimits.alerts) || [];
      const pullbacks = (ghRateLimits && ghRateLimits.pullbacks) || [];
      const core = (ghRateLimits && ghRateLimits.core) || {};
      const now = Math.floor(Date.now() / 1000);

      maybeRefreshGhRateLimits(ghRateLimits);
      const coreStatus = debugGhRateLimitStatus(core);
      if (coreStatus.limit > 0 && !coreStatus.resetPassed && coreStatus.remainingPct <= GH_RATE_CRITICAL_REMAINING_PCT && core.reset) {
        const resetDate = new Date(core.reset);
        if (!Number.isNaN(resetDate.getTime()) && resetDate.getTime() > Date.now()) {
          const coreAlert = {
            agent: 'github-api',
            message: 'Core API: ' + coreStatus.remaining + '/' + coreStatus.limit + ' remaining (' + coreStatus.remainingPct + '% free) — ' + coreStatus.resetText,
            detected_epoch: now,
            api_reset_epoch: Math.floor(resetDate.getTime() / 1000),
          };
          const hasCoreAlert = alerts.some(a => a.agent === 'github-api');
          if (!hasCoreAlert) alerts.unshift(coreAlert);
        }
      }
      const GH_RATE_DEFAULT_TTL = 3600;
      const SECONDS_PER_MINUTE = 60;
      const MINUTES_PER_HOUR = 60;
      const active = alerts.filter(a => {
        if (a.api_reset_epoch && a.api_reset_epoch > 0) return now < a.api_reset_epoch;
        const ttl = a.ttl_seconds || GH_RATE_DEFAULT_TTL;
        return now - (a.detected_epoch || 0) < ttl;
      });
      const activePullbacks = pullbacks.filter(p => now < (p.expiry_epoch || 0));
      if (active.length === 0 && activePullbacks.length === 0) {
        el.className = 'gh-rate-alert';
        setIfChanged(el, '');
        return;
      }
      el.className = 'gh-rate-alert active';
      const items = active.map(a => {
        const ageSec = now - (a.detected_epoch || now);
        let ageStr;
        if (ageSec < SECONDS_PER_MINUTE) ageStr = ageSec + 's ago';
        else if (ageSec < SECONDS_PER_MINUTE * MINUTES_PER_HOUR) ageStr = Math.floor(ageSec / SECONDS_PER_MINUTE) + 'm ago';
        else ageStr = Math.floor(ageSec / (SECONDS_PER_MINUTE * MINUTES_PER_HOUR)) + 'h ago';
        const cliTag = a.cli ? ` <span style="opacity:0.6;font-size:0.65rem">(${esc(a.cli)})</span>` : '';
        return `<div class="gh-rate-alert-item">
          <span class="gh-rate-alert-agent">${esc(a.agent)}${cliTag}</span>
          <span class="gh-rate-alert-age">${ageStr}</span>
          <span class="gh-rate-alert-msg">${linkifyGitHubRefs(a.message || 'GitHub API rate limited', window._githubBaseUrl)}</span>
        </div>`;
      }).join('');
      const pullbackHtml = activePullbacks.map(p => {
        const remainSec = (p.expiry_epoch || 0) - now;
        const remainMin = Math.max(0, Math.ceil(remainSec / SECONDS_PER_MINUTE));
        const paused = (p.paused_agents || []).join(', ') || 'none';
        let resetStr = '';
        if (p.api_reset_epoch && p.api_reset_epoch > now) {
          const resetDate = new Date(p.api_reset_epoch * 1000);
          resetStr = ` · API quota resets ${formatDashboardTime(resetDate, {hour:'numeric',minute:'2-digit',timeZoneName:'short'})}`;
        } else if (p.api_reset_epoch && p.api_reset_epoch <= now) {
          resetStr = ' · API quota restored';
        }
        return `<div style="font-size:0.72rem;color:var(--yellow);margin-top:var(--sp-3);padding-top:var(--sp-3);border-top:1px solid rgba(210,153,34,0.25)">
          ⏸ Pullback (${esc(p.cli || '?')}): paused <strong>${esc(paused)}</strong> — resumes in ${remainMin}m${resetStr}
        </div>`;
      }).join('');
      setIfChanged(el, `<div class="gh-rate-alert-title">GitHub API Rate Limit (${active.length} agent${active.length > 1 ? 's' : ''})</div>${items}${pullbackHtml}`);
      stopSystemAlertLinkClicks(el);
    }

    const GH_RATE_LIMIT_POLL_MS = 5 * 60 * 1000;
    let _ghRateLimitPollInFlight = false;
    let _ghRateLimitLastPollMs = 0;
    async function maybeRefreshGhRateLimits(ghRateLimits) {
      const core = (ghRateLimits && ghRateLimits.core) || {};
      const status = debugGhRateLimitStatus(core);
      if (!status.limit || _ghRateLimitPollInFlight) return;
      const nowMs = Date.now();
      const dueForPeriodicRefresh = nowMs - _ghRateLimitLastPollMs >= GH_RATE_LIMIT_POLL_MS;
      if (!status.resetPassed && !dueForPeriodicRefresh) return;
      _ghRateLimitPollInFlight = true;
      _ghRateLimitLastPollMs = nowMs;
      try {
        const res = await fetch('/api/gh-rate-limits', { cache: 'no-store' });
        if (!res.ok) return;
        const fresh = await res.json();
        if (!fresh || !fresh.core || !window._lastStatus) return;
        window._lastStatus.ghRateLimits = Object.assign({}, window._lastStatus.ghRateLimits || {}, { core: fresh.core, etag_cache: fresh.etag_cache, top_consumers: fresh.top_consumers, api_budget: fresh.api_budget || fresh.apiBudget });
        window._lastStatus.api_budget = fresh.api_budget || fresh.apiBudget || window._lastStatus.api_budget;
        renderGhRateLimits(window._lastStatus.ghRateLimits);
        renderDebugSection();
      } catch (_) {
        // The cached /api/status payload will continue rendering safely.
      } finally {
        _ghRateLimitPollInFlight = false;
      }
    }

    // Guard: skip agent re-render while a dropdown is focused/open
    let _dropdownOpen = false;
    let _configModalOpen = false;
    document.addEventListener('focusin', (e) => { if (e.target.matches('select.backend-select')) _dropdownOpen = true; });
    document.addEventListener('focusout', (e) => { if (e.target.matches('select.backend-select')) _dropdownOpen = false; });
    document.addEventListener('change', (e) => { if (e.target.matches('select.backend-select')) _dropdownOpen = false; });

    // Skip innerHTML assignment when content hasn't changed — prevents
    // DOM reflow that causes scroll position to jump on SSE updates.
    function setIfChanged(el, html) {
      if (el.innerHTML !== html) el.innerHTML = html;
    }

