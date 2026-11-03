    // ── Debug section ──
    const DEBUG_REFRESH_MS = 10000;
    const GH_RATE_CRITICAL_REMAINING_PCT = 10;
    const GH_RATE_WARN_REMAINING_PCT = 25;
    function debugGhRateClassFromRemainingPct(pct) {
      const n = Number.isFinite(Number(pct)) ? Number(pct) : 100;
      if (n <= GH_RATE_CRITICAL_REMAINING_PCT) return 'debug-quota-bad';
      if (n <= GH_RATE_WARN_REMAINING_PCT) return 'debug-quota-warn';
      return 'debug-quota-ok';
    }
    function debugGhRateChipClassFromRemainingPct(pct) {
      const cls = debugGhRateClassFromRemainingPct(pct);
      if (cls === 'debug-quota-bad') return 'bad';
      if (cls === 'debug-quota-warn') return 'warn';
      return 'ok';
    }
    function debugQuotaClass(pct) {
      const n = Number(pct) || 0;
      if (n > 85) return 'debug-quota-bad';
      if (n > 60) return 'debug-quota-warn';
      return 'debug-quota-ok';
    }
    function debugQuotaChipClass(pct) {
      const n = Number(pct) || 0;
      if (n > 85) return 'bad';
      if (n > 60) return 'warn';
      return 'ok';
    }
    function debugCompactNum(n) {
      const v = Math.max(0, Number(n) || 0);
      const trim = s => s.replace(/\.0$/, '');
      if (v >= 1e9) return trim((v / 1e9).toFixed(v >= 10e9 ? 0 : 1)) + 'B';
      if (v >= 1e6) return trim((v / 1e6).toFixed(v >= 10e6 ? 0 : 1)) + 'M';
      if (v >= 1e3) return trim((v / 1e3).toFixed(v >= 10e3 ? 0 : 1)) + 'K';
      return String(Math.round(v));
    }
    function debugQuotaResetLabel(raw) {
      if (!raw) return '—';
      const d = new Date(typeof raw === 'number' ? raw * 1000 : raw);
      if (Number.isNaN(d.getTime())) return '—';
      return formatDashboardTime(d, { hour:'2-digit', minute:'2-digit', timeZoneName:'short' });
    }
    function debugGhRateLimitStatus(core, nowMs) {
      core = core || {};
      const now = Number.isFinite(Number(nowMs)) ? Number(nowMs) : Date.now();
      const limit = Math.max(0, Number(core.limit) || 0);
      const resetRaw = core.reset;
      const resetDate = resetRaw ? new Date(typeof resetRaw === 'number' ? resetRaw * 1000 : resetRaw) : null;
      const resetMs = resetDate && !Number.isNaN(resetDate.getTime()) ? resetDate.getTime() : 0;
      const resetPassed = !!(resetMs && resetMs <= now);
      const rawRemaining = Number(core.remaining);
      const clampedRemaining = limit > 0 && Number.isFinite(rawRemaining) ? Math.max(0, Math.min(limit, rawRemaining)) : 0;
      const remaining = limit > 0 && resetPassed ? limit : clampedRemaining;
      const used = limit > 0 ? Math.max(0, limit - remaining) : 0;
      const remainingPct = limit > 0 ? Math.round((remaining / limit) * 100) : 100;
      const resetAt = resetMs ? formatDashboardTime(resetDate, { hour:'2-digit', minute:'2-digit', timeZoneName:'short' }) : '—';
      const resetText = resetPassed ? 'reset passed; refreshing' : `resets ${resetAt}`;
      const headline = `GitHub API ${remaining}/${limit || '—'} remaining (${remainingPct}% free) · ${resetText}`;
      return {
        limit,
        remaining,
        used,
        remainingPct,
        resetAt,
        resetText,
        resetPassed,
        headline,
        headlineClass: debugGhRateClassFromRemainingPct(remainingPct),
        chipClass: debugGhRateChipClassFromRemainingPct(remainingPct),
      };
    }
    function debugQuotaNormalize(source, quota, fallbackLabel) {
      quota = quota || {};
      const limit = Number(quota.limit ?? quota.max ?? quota.quota ?? quota.total) || 0;
      const remaining = Number(quota.remaining ?? quota.left ?? quota.available);
      const used = Number(quota.used ?? quota.consumed ?? (limit > 0 && Number.isFinite(remaining) ? limit - remaining : 0)) || 0;
      if (limit <= 0) return null;
      const safeUsed = Math.max(0, Math.min(limit, used));
      const pct = Math.round((safeUsed / limit) * 100);
      const label = source || quota.source || quota.provider || fallbackLabel || 'Source';
      const bucket = quota.bucket || quota.name || quota.resource || quota.type || '';
      const reset = quota.reset ?? quota.reset_at ?? quota.resetAt;
      const resetAt = debugQuotaResetLabel(reset);
      return { label, bucket, limit, used: safeUsed, pct, resetAt };
    }
    function debugQuotaFromWorkSources(data) {
      const out = [];
      const containers = [
        data && data.quotaSources,
        data && data.workSources,
        data && data.diagnostics && data.diagnostics.quotaSources,
        data && data.diagnostics && data.diagnostics.workSources,
      ].filter(Array.isArray);
      containers.forEach(list => list.forEach(src => {
        const sourceLabel = src && (src.label || src.name || src.source || src.provider);
        const quotas = src && (src.quotas || src.limits || src.rateLimits || src.buckets);
        const items = Array.isArray(quotas) ? quotas : (quotas && typeof quotas === 'object' ? Object.keys(quotas).map(k => Object.assign({ bucket: k }, quotas[k])) : [src]);
        items.forEach(item => {
          const q = debugQuotaNormalize(sourceLabel, item, sourceLabel);
          if (q) out.push(q);
        });
      }));
      return out;
    }
    function debugGitHubQuotaSources(data) {
      const ghRate = (data && data.ghRateLimits) || {};
      const buckets = Object.keys(ghRate).filter(k => {
        const v = ghRate[k];
        return v && typeof v === 'object' && Number(v.limit || v.max || v.quota || 0) > 0;
      });
      return buckets.map(k => {
        const core = ghRate[k] || {};
        const status = debugGhRateLimitStatus(core);
        if (status.limit <= 0) return null;
        return {
          label: 'GitHub',
          bucket: buckets.length === 1 && k === 'core' ? 'API' : k,
          limit: status.limit,
          used: status.used,
          remaining: status.remaining,
          pct: status.remainingPct,
          resetAt: status.resetText,
          semantics: 'remaining',
          cls: status.chipClass,
        };
      }).filter(Boolean);
    }
    // Row-label help for the GitHub API budget panel. Same ⓘ + portal tooltip
    // the Overview and PRT metric labels use; copy mirrors pkg/github/etagcache.go
    // and pkg/github/apibudget.go.
    const GH_API_BUDGET_HELP = {
      core: 'Requests left in GitHub’s core REST rate limit for the current hour, out of the hourly limit for this credential (5,000 for a token; App installations may get more). GitHub resets the counter once an hour. The bar turns yellow at 25% free and red at 10%. Below the configured API reserve (default 800 requests) the governor enters conserve mode — it stretches the eval interval and skips optional steps such as recommendations and advisories; below the API critical floor (default 250) it stretches the interval further.',
      etag: 'Share of cacheable GitHub REST GETs answered 304 Not Modified because the hive re-sent a remembered ETag (If-None-Match). GitHub does not charge a 304 against the core rate limit, so a higher rate means more of the hive’s polling is free. Counted since the process started, not over the 1h window — a low rate right after a restart is normal while the cache warms. The 304 column below shows the same hits per caller.',
    };
    function ghApiBudgetHelpMark(key, label) {
      const id = 'gh-api-budget-' + key + '-tip';
      return `<span class="config-info overview-term-info" tabindex="0" data-action="gh3" data-stop="1" aria-describedby="${id}" aria-label="${esc(label)} details">ⓘ<span id="${id}" class="config-tooltip" role="tooltip">${esc(GH_API_BUDGET_HELP[key] || '')}</span></span>`;
    }
    function renderGitHubAPIBudgetPanel(ghRate) {

      ghRate = ghRate || {};
      const core = debugGhRateLimitStatus(ghRate.core || {});
      const etag = ghRate.etag_cache || ghRate.etagCache || {};
      const hits = Number(etag.hits) || 0;
      const misses = Number(etag.misses) || 0;
      const etagTotal = hits + misses;
      const etagPct = etagTotal > 0 ? Math.round((hits / etagTotal) * 100) : 0;
      const top = (ghRate.top_consumers || ghRate.topConsumers || []).slice(0, 8);
      const barWidth = Math.max(0, Math.min(100, core.remainingPct));
      const bar = `<div id="gh-api-budget-core-bar" class="gh-api-budget-bar"><svg viewBox="0 0 100 6" preserveAspectRatio="none" aria-hidden="true"><rect class="gh-api-budget-track" x="0" y="0" width="100" height="6" rx="3"/><rect class="gh-api-budget-fill ${core.headlineClass}" x="0" y="0" width="${barWidth}" height="6" rx="3"/></svg></div>`;
      const rows = top.map(c => {
        const caller = esc(c.caller || 'unknown');
        const endpoint = esc(c.endpoint || '');
        return `<tr><td class="gh-api-budget-col-caller" title="${caller}">${caller}</td><td class="gh-api-budget-col-endpoint" title="${endpoint}">${endpoint}</td><td class="gh-api-budget-col-num">${Number(c.charged) || 0}</td><td class="gh-api-budget-col-num">${Number(c.requests) || 0}</td><td class="gh-api-budget-col-num">${Number(c.not_modified) || 0}</td><td class="gh-api-budget-col-num">${Number(c.rate_limited) || 0}</td></tr>`;
      }).join('');
      return `<div id="gh-api-budget-panel" class="gh-api-budget-card">
        <div class="gh-api-budget-label">GitHub API budget (1h)</div>
        <div class="gh-api-budget-row"><span>Core remaining${ghApiBudgetHelpMark('core', 'Core remaining')}</span><span class="gh-api-budget-strong">${core.remaining} / ${core.limit || '—'} (${core.remainingPct}% free)</span></div>
        <div class="gh-api-budget-row"><span>ETag hit rate${ghApiBudgetHelpMark('etag', 'ETag hit rate')}</span><span class="gh-api-budget-strong">${etagPct}%</span></div>
        ${bar}
        <div class="gh-api-budget-table-wrap" tabindex="0" role="region" aria-label="GitHub API consumers table">
          <p class="mobile-table-hint">Scroll horizontally for more columns.</p>
          <table id="gh-api-budget-consumers" class="gh-api-budget-table">
            <thead><tr><th class="gh-api-budget-col-caller">Caller</th><th class="gh-api-budget-col-endpoint">Endpoint</th><th class="gh-api-budget-col-num" title="Rate-limit points charged">Charged</th><th class="gh-api-budget-col-num" title="Requests">Req</th><th class="gh-api-budget-col-num" title="304 Not Modified (ETag hits)">304</th><th class="gh-api-budget-col-num" title="429 / rate-limited responses">429</th></tr></thead>
            <tbody>${rows || '<tr><td colspan="6" class="gh-api-budget-empty">No recent GitHub API traffic</td></tr>'}</tbody>

          </table>
        </div>
      </div>`;
    }
    function debugQuotaWidgetHtml(data) {
      const quotas = debugQuotaFromWorkSources(data || {}).concat(debugGitHubQuotaSources(data || {}));
      if (!quotas.length) return '';
      const chips = quotas.map(q => {
        const cls = q.cls || debugQuotaChipClass(q.pct);
        const label = q.bucket ? `${q.label} ${q.bucket}` : q.label;
        const remainingMode = q.semantics === 'remaining';
        const frac = remainingMode ? `${q.remaining}/${q.limit}` : `${q.used}/${q.limit}`;
        const title = remainingMode
          ? `${label}: ${q.remaining}/${q.limit} remaining (${q.pct}% free). ${q.resetAt}`
          : `${label}: ${q.used}/${q.limit} used (${q.pct}%). Resets ${q.resetAt}`;
        const fill = Math.max(0, Math.min(50, Math.round(q.pct / 2)));
        return `<span class="debug-quota-chip ${cls}" title="${escapeHtml(title)}"><span class="debug-quota-label">${escapeHtml(label)}</span><svg class="debug-quota-gauge" width="54" height="10" viewBox="0 0 54 10" role="img" aria-label="${escapeHtml(title)}"><title>${escapeHtml(title)}</title><rect class="debug-quota-gauge-track" x="1" y="2" width="50" height="6" rx="3"/><rect class="debug-quota-gauge-fill" x="1" y="2" width="${fill}" height="6" rx="3"/></svg><span class="debug-quota-frac">${frac}</span></span>`;
      }).join('');
      const label = quotas.map(q => q.semantics === 'remaining' ? `${q.label} ${q.remaining}/${q.limit} remaining` : `${q.label} ${q.used}/${q.limit} used`).join(', ');
      return `<span class="mini-summary-strip debug-quota-strip" role="group" aria-label="${escapeHtml(label)}">${chips}</span>`;
    }
    function debugDiagnosticsHeadlineHtml(data, summaryText) {
      const status = data || {};
      const ghRate = status.ghRateLimits || {};
      const tokens = status.tokens || {};
      const core = ghRate.core || {};
      const rate = debugGhRateLimitStatus(core);
      const totals = tokens.totals || {};
      const apiBudget = status.api_budget || ghRate.api_budget || {};
      const skipped = Array.isArray(apiBudget.skipped_steps) ? apiBudget.skipped_steps : [];
      const budgetReset = apiBudget.reset ? new Date(apiBudget.reset) : null;
      const budgetResetText = budgetReset && !Number.isNaN(budgetReset.getTime()) ? 'resets ' + Math.max(0, Math.round((budgetReset.getTime() - Date.now()) / 60000)) + 'm' : 'reset unknown';
      const budgetMode = apiBudget.mode || 'normal';
      const budgetTitle = skipped.length ? 'Skipped for API budget: ' + skipped.join(', ') : 'No API-budget steps skipped';
      const budgetPill = `<span id="api-budget-pill" class="debug-quota-chip ${budgetMode === 'critical' ? 'bad' : budgetMode === 'conserve' ? 'warn' : 'ok'}" title="${escapeHtml(budgetTitle)}">API budget: ${escapeHtml(budgetMode)} (${formatNumber(Number(apiBudget.remaining || rate.remaining || 0))} left, ${escapeHtml(budgetResetText)})</span>`;
      const apiText = rate.headline;
      const tokenText = `${debugCompactNum(totals.input)} in · ${debugCompactNum(totals.output)} out · ${debugCompactNum(totals.cacheRead)} cache`;
      const mini = debugQuotaWidgetHtml(status);
      return `<div class="debug-headline sec-headline" data-collapsed-keep><div class="debug-headline-main"><span class="debug-headline-api ${rate.headlineClass}" title="${escapeHtml(apiText)}">${escapeHtml(apiText)}</span><span class="debug-headline-tokens" title="${escapeHtml(tokenText)}">${escapeHtml(tokenText)}</span>${budgetPill}</div><span class="debug-headline-mini">${mini}</span></div>`;
    }
    const HEALTH_LABEL_DEFAULTS = {
      ci: 'CI Pass Rate', helm: 'Helm'
    };
    function _buildHealthLabels(agents) {
      const labels = { ...HEALTH_LABEL_DEFAULTS };
      for (const a of agents || []) {
        for (const s of a.statsConfig || []) {
          if (s.source === 'health' && s.field && s.label) {
            labels[s.field] = s.label;
          }
        }
      }
      return labels;
    }

    const HEALTH_DESC_DEFAULTS = {
      ci: 'Pass rate over the last 10 completed workflow runs on the primary repo (success or skipped count as passing; failed runs that scheduled no jobs are dropped from the sample)',
      helm: 'Helm chart exists at deploy/helm/kubestellar-console/Chart.yaml in the primary repo',
    };
    function _buildHealthDescs(agents) {
      const descs = { ...HEALTH_DESC_DEFAULTS };
      for (const a of agents || []) {
        for (const s of a.statsConfig || []) {
          if (s.source === 'health' && s.field && s.desc) {
            descs[s.field] = s.desc;
          }
        }
      }
      return descs;
    }
    function renderDebugSection() {
      refreshSectionCardShell('debug-section');
      const grid = document.getElementById('debug-grid');
      if (!grid) return;
      const data = window._lastStatus || {};
      const health = data.health || {};
      const agents = data.agents || [];
      const budget = data.budget || {};
      const ghRate = data.ghRateLimits || {};
      const tokens = data.tokens || {};

      const CI_PASS_THRESHOLD = 70;
      const HEALTH_LABELS = _buildHealthLabels(agents);

      const cardStyle = 'background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:14px;';
      const labelStyle = 'font-size:0.65rem;text-transform:uppercase;letter-spacing:0.5px;color:var(--muted);margin-bottom:var(--sp-4);font-weight:600;';
      const rowStyle = 'display:flex;flex-wrap:wrap;gap:var(--sp-2);justify-content:space-between;align-items:center;padding:3px 0;font-size:0.8rem;';

      // Only show health checks that match ci-maintainer's stats config
      const ciAgent = agents.find(a => a.role === 'ci-maintainer' || a.name === 'ci-maintainer');
      const ciHealthFields = new Set(
        (ciAgent?.statsConfig || [])
          .filter(s => s.source === 'health')
          .map(s => s.field)
      );

      const HEALTH_DESCS = _buildHealthDescs(agents);
      let healthRows = '';
      for (const [k, v] of Object.entries(health)) {
        if (ciHealthFields.size > 0 && !ciHealthFields.has(k)) continue;
        const label = HEALTH_LABELS[k] || k;
        const desc = HEALTH_DESCS[k] || '';
        const labelHtml = desc
          ? `<span style="cursor:help;text-decoration:underline dotted;text-underline-offset:3px" title="${escapeHtml(desc)}">${label}</span>`
          : `<span>${label}</span>`;
        const ok = k === 'ci' ? v >= CI_PASS_THRESHOLD : (v === true || v === 1);
        const skip = v === -1;
        const dot = skip ? '⊘' : ok ? '✓' : '✗';
        const color = skip ? 'var(--muted)' : ok ? 'var(--green)' : 'var(--red)';
        const val = k === 'ci' ? `${v}%` : skip ? 'skipped' : ok ? 'ok' : 'fail';
        healthRows += `<div style="${rowStyle}">${labelHtml}<span style="color:${color};font-weight:600">${dot} ${val}</span></div>`;
      }
      const stalledRows = stalledRunHealthRows(dashboardRunsFromStatus(data));
      if (stalledRows) healthRows += stalledRows;

      const { sorted: sortedForStates, groups: stateGroups } = _sortAgentsBySidebar(agents);
      let agentRows = '';
      let _stateCurrentGroup = undefined;
      for (const a of sortedForStates) {
        const grp = stateGroups.find(g => (g.agents || []).includes(a.name));
        const grpName = grp ? grp.name : null;
        if (grpName !== _stateCurrentGroup) {
          _stateCurrentGroup = grpName;
          if (grpName) agentRows += `<div style="font-size:var(--fs-2xs);text-transform:uppercase;letter-spacing:0.5px;color:var(--muted);margin-top:var(--sp-4);margin-bottom:var(--sp-1);font-weight:600">${esc(grpName)}</div>`;
        }
        const stateColor = { running: 'var(--green)', idle: 'var(--green)', paused: 'var(--yellow)', stopped: 'var(--red)', off: 'var(--muted)' }[a.state] || 'var(--muted)';
        agentRows += `<div style="${rowStyle}"><span>${esc(a.name)}</span><span style="color:${stateColor};font-weight:600">${a.state}</span></div>`;
      }

      const ghCoreRate = debugGhRateLimitStatus(ghRate.core || {});
      const ghCoreLimit = ghCoreRate.limit;
      const ghCoreRemaining = ghCoreRate.remaining;
      const ghCorePct = ghCoreRate.remainingPct;
      const ghResetAt = ghCoreRate.resetText;
      // How old is this reading? (#5733) The card used to show only `reset`,
      // which moves independently of when the sample was taken — so a value
      // that had stopped updating looked exactly like a fresh one. A reading
      // held back by the monotone clamp keeps its original observed_at, so the
      // age below is what makes that visible.
      const GH_RATE_STALE_SECONDS = 300;
      const ghObservedRaw = ghRate.core?.observed_at;
      let ghObservedHtml = '';
      if (ghObservedRaw) {
        const observedMs = new Date(ghObservedRaw).getTime();
        if (!Number.isNaN(observedMs)) {
          const ageSec = Math.max(0, Math.floor((Date.now() - observedMs) / 1000));
          const ageStr = ageSec < 60
            ? ageSec + 's ago'
            : ageSec < 3600
              ? Math.floor(ageSec / 60) + 'm ago'
              : Math.floor(ageSec / 3600) + 'h ago';
          const stale = ageSec >= GH_RATE_STALE_SECONDS;
          const color = stale ? 'var(--yellow)' : 'var(--muted)';
          const title = stale
            ? 'This reading has not refreshed recently — treat the headroom above as a floor, not a live value'
            : 'When this rate-limit reading was taken';
          ghObservedHtml = `<div style="${rowStyle}"><span>Measured</span><span style="color:${color}" title="${escapeHtml(title)}">${ageStr}</span></div>`;
        }
      }
      const ghIdentity = ghRate.identity || {};
      const ghIdentityLabel = ghIdentity.label || 'unknown';
      const ghIdentityType = ghIdentity.type || 'unknown';
      const ghIdentityBadge = ghIdentityType === 'app'
        ? `<span style="display:inline-block;font-size:var(--fs-2xs);background:#dbeafe;color:#1d4ed8;padding:1px 6px;border-radius:var(--r-sm);font-weight:500">App</span>`
        : `<span style="display:inline-block;font-size:var(--fs-2xs);background:#fef3c7;color:#92400e;padding:1px 6px;border-radius:var(--r-sm);font-weight:500">Personal</span>`;

      const totals = tokens.totals || {};
      const tokenInput = formatNumber(totals.input || 0);
      const tokenOutput = formatNumber(totals.output || 0);
      const tokenCache = formatNumber(totals.cacheRead || 0);

      const budgetUsed = budget.used || 0;
      const budgetLimit = budget.limit || 0;
      const budgetPct = budgetLimit > 0 ? Math.round((budgetUsed / budgetLimit) * 100) : 0;

      const platform = _platformStatus || data.platform || null;
      const version = platform && (platform.version || platform.hiveVersion || platform.gitVersion || platform.commit);
      const warningCount = Object.entries(health).filter(([k, v]) => k === 'ci' ? Number(v) < CI_PASS_THRESHOLD && Number(v) !== -1 : !(v === true || v === 1 || v === -1)).length;
      const debugSummary = warningCount ? `${warningCount} warning${warningCount === 1 ? '' : 's'}` : `healthy${version ? ' · ' + version : ''}`;
      setSectionSummary('debug-section', debugSummary);
      // Patched in place (#12512): a 10s innerHTML rebuild re-inserted every
      // tile without its collapsed-hidden class and shifted the sections below.
      morphInnerHtml(grid, `
        ${debugDiagnosticsHeadlineHtml(data, debugSummary)}
        <div style="${cardStyle}" class="platform-diagnostics"><div style="${labelStyle}">Platform</div>${platformFactTiles(platform)}</div>
        <div style="${cardStyle}"><div style="${labelStyle}">Health Checks</div>${healthRows || '<div style="color:var(--muted);font-size:0.8rem">No data</div>'}</div>
        ${renderQualityStatsCard(agents)}
        <div style="${cardStyle}"><div style="${labelStyle}">Agent States</div>${agentRows || '<div style="color:var(--muted);font-size:0.8rem">No agents</div>'}</div>
        ${renderGitHubAPIBudgetPanel(ghRate)}

        <div style="${cardStyle}">
          <div style="${labelStyle}">GitHub API</div>
          <div style="${rowStyle}"><span>Identity</span><span>${ghIdentityBadge} ${esc(ghIdentityLabel)}</span></div>
          <div style="${rowStyle}"><span>Core API</span><span style="font-weight:600">${ghCoreRemaining} / ${ghCoreLimit} remaining (${ghCorePct}% free)</span></div>
          <div style="${rowStyle}"><span>Reset</span><span>${ghResetAt}</span></div>
          ${ghObservedHtml}
          <div style="margin-top:var(--sp-3);height:6px;background:var(--line);border-radius:3px;overflow:hidden">
            <div style="height:100%;width:${ghCorePct}%;background:${ghCorePct <= GH_RATE_CRITICAL_REMAINING_PCT ? 'var(--red)' : ghCorePct <= GH_RATE_WARN_REMAINING_PCT ? 'var(--yellow)' : 'var(--green)'};border-radius:3px;transition:width 0.3s"></div>
          </div>
        </div>
        <div style="${cardStyle}">
          <div style="${labelStyle}">Token Usage</div>
          <div style="${rowStyle}"><span>Input</span><span style="font-weight:600">${tokenInput}</span></div>
          <div style="${rowStyle}"><span>Output</span><span style="font-weight:600">${tokenOutput}</span></div>
          <div style="${rowStyle}"><span>Cache Read</span><span style="font-weight:600">${tokenCache}</span></div>
          ${budgetLimit > 0 ? `<div style="margin-top:var(--sp-4);${labelStyle}">Budget</div><div style="${rowStyle}"><span>Used</span><span style="font-weight:600">$${budgetUsed.toFixed(2)} / $${budgetLimit.toFixed(2)} (${budgetPct}%)</span></div>` : ''}
        </div>
      `);
      applySectionCollapse('debug-section');
    }
    dashboardSetInterval('debug-section', renderDebugSection, DEBUG_REFRESH_MS);

    // ── Logs section ──
    const LOGS_REFRESH_MS = 5000;
    // Lines shown per agent in the "all agents" view; the poll asks the
    // server for only that many instead of its 100-line default.
    const LOGS_ALL_VIEW_LINES = 15;
    let _logsData = {};
    // Set when a poll tick was skipped because nothing could see the result
    // (tab hidden, section collapsed or not displayed); the next time the
    // section becomes visible one fetch runs immediately instead of waiting
    // for the interval.
    let _logsPollSkipped = false;
    // Polling 16 agents' panes every 5s (~6.5 MB/min) while the Logs section
    // was collapsed, or the tab in the background, was pure waste.
    function agentLogsVisible() {
      if (document.hidden) return false;
      const section = document.getElementById('logs-section');
      if (!section || section.offsetParent === null) return false;
      return !isSectionCollapsed('logs-section');
    }
    function agentLogsResumeIfSkipped() {
      if (!_logsPollSkipped || !agentLogsVisible()) return;
      _logsPollSkipped = false;
      fetchAgentLogs();
    }
    // Agents whose /api/pane returned 404, keyed by name → the agent's state
    // at the time. A disabled or stopped agent has no tmux window, so polling
    // it again every 5s only produces console noise (#12512); the entry is
    // dropped as soon as the live agent list reports a different state.
    const _paneAbsentState = {};
    function agentPaneStateKey(a) {
      return (a && a.enabled === false ? 'disabled' : 'enabled') + ':' + String((a && a.state) || '');
    }
    // Agents that can have a pane right now: present in the live /api/status
    // list, not disabled, not stopped, and not known-absent in this state.
    function agentPaneCandidates(agents) {
      const out = [];
      for (const a of Array.isArray(agents) ? agents : []) {
        if (!a || !a.name || agentIsDisabled(a) || a.state === 'stopped') continue;
        if (_paneAbsentState[a.name] && _paneAbsentState[a.name] !== agentPaneStateKey(a)) delete _paneAbsentState[a.name];
        if (_paneAbsentState[a.name]) continue;
        out.push(a);
      }
      return out;
    }
    async function fetchAgentLogs() {
      if (!agentLogsVisible()) { _logsPollSkipped = true; return; }
      _logsPollSkipped = false;
      const allAgents = window._lastAgents || [];
      const agents = agentPaneCandidates(allAgents);
      const select = document.getElementById('logs-agent-select');
      if (!select) return;

      const current = select.value;
      const existingOpts = new Set(Array.from(select.options).map(o => o.value));
      for (const a of allAgents) {
        if (!existingOpts.has(a.name)) {
          const opt = document.createElement('option');
          opt.value = a.name;
          opt.textContent = a.name;
          select.appendChild(opt);
        }
      }

      const toFetch = current === 'all' ? agents : agents.filter(a => a.name === current);
      const lines = current === 'all' ? '?lines=' + LOGS_ALL_VIEW_LINES : '';
      for (const a of toFetch) {
        try {
          const res = await fetch(`/api/pane/${a.name}${lines}`);
          if (res.ok) {
            const d = await res.json();
            _logsData[a.name] = capDashboardArray(d.lines || [], DASHBOARD_LOG_CAP);
          } else if (res.status === 404) {
            _paneAbsentState[a.name] = agentPaneStateKey(a);
          }
        } catch (e) { /* skip */ }
      }
      renderLogs();
    }

    function renderFAQ() {
      refreshSectionCardShell('faq-section');
      const n = document.querySelectorAll('#faq-panel details.faq-q').length;
      setSectionSummary('faq-section', n ? `${n} questions` : '');
    }

    // Client-side filter for the FAQ panel: hides every <details class="faq-q">
    // whose question or answer does not contain the typed text, hides groups
    // that end up empty, and opens the matches so the hit is visible. Static
    // content only — no fetch.
    function faqFilter() {
      const input = document.getElementById('faq-filter');
      const panel = document.getElementById('faq-panel');
      if (!input || !panel) return;
      const q = (input.value || '').trim().toLowerCase();
      let shown = 0;
      const items = panel.querySelectorAll('details.faq-q');
      items.forEach(d => {
        const hit = !q || (d.textContent || '').toLowerCase().includes(q);
        d.hidden = !hit;
        if (hit && q) d.open = true;
        if (hit) shown++;
      });
      panel.querySelectorAll('.faq-group').forEach(g => {
        g.hidden = !!q && !g.querySelector('details.faq-q:not([hidden])');
      });
      const empty = document.getElementById('faq-empty');
      if (empty) empty.hidden = shown > 0;
      const count = document.getElementById('faq-filter-count');
      if (count) count.textContent = q ? `${shown} of ${items.length}` : '';
    }

    function renderLogs() {
      refreshSectionCardShell('logs-section');
      const logAgents = Object.keys(_logsData || {}).length || (window._lastAgents || []).length;
      setSectionSummary('logs-section', `${logAgents} agent${logAgents === 1 ? '' : 's'}`);
      const output = document.getElementById('logs-output');
      if (!output) return;
      const select = document.getElementById('logs-agent-select');
      const selected = select ? select.value : 'all';
      const follow = document.getElementById('logs-follow');

      let lines = [];
      if (selected === 'all') {
        for (const [name, data] of Object.entries(_logsData)) {
          if (data.length > 0) {
            lines.push(`\x1b[0m── ${name} ──`);
            lines.push(...data.slice(-LOGS_ALL_VIEW_LINES));
            lines.push('');
          }
        }
      } else {
        lines = _logsData[selected] || ['(no output)'];
      }

      while (lines.length > 0 && lines[lines.length - 1].trim() === '') lines.pop();
      output.textContent = lines.join('\n').replace(/\x1b\[[0-9;]*m/g, '');
      if (follow && follow.checked) {
        output.scrollTop = output.scrollHeight;
      }
    }

    document.getElementById('logs-agent-select')?.addEventListener('change', () => { _logsData = {}; fetchAgentLogs(); });
    // setInterval(fetchAgentLogs) — deferred to _deferredInit

    const MODE_EMOJIS = {'ADVISORY':'📝','ISSUES_ONLY':'🎫','ISSUES_AND_PRS':'🔧','ISSUES_PRS_MERGE':'🚀'};
    function modeEmoji(mode) { return MODE_EMOJIS[mode] || ''; }
    function modeLabel(a) {
      if (!a.mode) return '—';
      const emoji = modeEmoji(a.mode);
      const isCustom = a.isCustomMode ? ' *' : '';
      return `${emoji} ${a.mode}${isCustom}`;
    }

    function esc(s) {
      if (typeof s !== 'string') return '';
      return s
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;')
        .replace(/`/g, '&#96;')
        .replace(/\n/g, ' ')
        .replace(/\r/g, '');
    }
    function escBlock(s) {
      if (typeof s !== 'string') return '';
      return s
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;')
        .replace(/`/g, '&#96;')
        .replace(/\r\n/g, '\n')
        .replace(/\r/g, '')
        .replace(/\n/g, '<br>');
    }

    function escPreserveNewlines(s) {
      if (typeof s !== 'string') return '';
      return s
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;')
        .replace(/`/g, '&#96;')
        .replace(/\r\n/g, '\n')
        .replace(/\r/g, '');
    }

