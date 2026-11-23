    // Dashboard boot hooks that used to share this file with the removed
    // Inception wizard (the section is now Inception (spek), sessions.js).
    document.addEventListener('DOMContentLoaded', () => {
      applySectionCollapse('advisory-digest-section');
      /* OpenRouter scan-to-fund return: show the result and clear the flag so a
         reload doesn't re-toast. */
      try {
        const p = new URLSearchParams(window.location.search);
        const or = p.get('openrouter');
        if (or === 'connected') {
          showToast('OpenRouter connected — the "openrouter" gateway is now configured', 'success');
        } else if (or === 'error') {
          showToast('OpenRouter connection failed — please try funding again', 'error');
        }
        if (or) {
          p.delete('openrouter');
          const qs = p.toString();
          history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : ''));
        }
      } catch (e) { /* non-fatal */ }
    });

    function navbarHealthGroups(data) {
      const failing = [];
      const warning = [];
      const passing = [];
      const deepChecks = Array.isArray((data.deepHealth || {}).checks) ? data.deepHealth.checks : [];
      const deepFailNames = [];
      for (const c of deepChecks) {
        let line = c.detail ? `${c.name}: ${c.detail}` : c.name;
        if (c.docs_url) line += ' — what this means: ' + c.docs_url;
        if (c.onboarding) line += ' — new hive? open 🚀 Getting started (top bar ? Help menu) → step ‘' + (ONBOARDING_GUIDE_STEP_TITLES[c.guideStep] || ONBOARDING_GUIDE_STEP_TITLES['github-app']) + '’';
        if (c.status === 'fail') { deepFailNames.push(c.name); failing.push(line); }
        else if (c.status === 'warn') warning.push(line);
        else passing.push(line);
      }
      return { failing, warning, passing, deepFailNames };
    }

    function navbarHealthTitle(data) {
      const groups = navbarHealthGroups(data || {});
      const hoverLines = [];
      if (groups.failing.length) hoverLines.push('FAILING:\n' + groups.failing.map(f => '  ✗ ' + f).join('\n'));
      if (groups.warning.length) hoverLines.push('WARNINGS:\n' + groups.warning.map(wl => '  ⚠ ' + wl).join('\n'));
      if (groups.passing.length) hoverLines.push('PASSING:\n' + groups.passing.map(p => '  ✓ ' + p).join('\n'));
      return hoverLines.join('\n\n') || 'No health data';
    }

    function renderNavbarHealthBadge(ocHealth, data) {
      if (!ocHealth) return;
      const groups = navbarHealthGroups(data || {});
      ocHealth.title = navbarHealthTitle(data || {});
      if (groups.deepFailNames.length > 0) {
        // Any failing deep check ⇒ the spoke itself is degraded — same
        // verdict the hub renders for this hive's row. Repo workflow stats are
        // intentionally excluded: they are not this hive's runtime health.
        ocHealth.dataset.status = 'error';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Degraded</span>';
      } else if (groups.failing.length > 0) {
        ocHealth.dataset.status = 'error';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">' + groups.failing.length + ' Issue' + (groups.failing.length > 1 ? 's' : '') + '</span>';
      } else {
        ocHealth.dataset.status = 'ok';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Health OK</span>';
      }
      ocHealth.style.color = '';
      ocHealth.style.background = '';
      ocHealth.style.borderColor = '';
    }

    // Providers strip (ADR-0024): per-provider auth vs availability, two
    // separate dimensions so "needs login" never reads as "provider down".
    // Hidden while every provider is usable and available.
    function renderProviderStrip(el, providers) {
      if (!el) return;
      const bad = (providers || []).filter(p => p && (p.auth_state !== 'usable' || p.availability_state !== 'available'));
      if (!bad.length) {
        el.hidden = true;
        el.textContent = '';
        el.title = 'AI providers: all usable';
        el.setAttribute('aria-label', 'AI providers: all usable');
        return;
      }
      const severe = bad.some(p => p.auth_state === 'needs-login' || p.auth_state === 'store-inaccessible' ||
        p.availability_state === 'unavailable' || p.availability_state === 'quota-exhausted');
      el.dataset.status = severe ? 'error' : 'warn';
      el.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Providers: ' +
        escapeHtml(bad.map(p => p.provider).join(', ')) + '</span>';
      el.title = bad.map(p => p.provider + ': auth ' + p.auth_state + ', availability ' + p.availability_state +
        (p.next_action ? ' — ' + p.next_action : '')).join('\n');
      el.setAttribute('aria-label', 'AI providers needing attention: ' + bad.map(p => p.provider).join(', '));
      el.hidden = false;
    }

    function qualityDiagnosticsAgent(agents) {
      return (agents || []).find(a => a && a.name === 'quality') || null;
    }

    function renderQualityStatValue(stat, agentName) {
      const v = resolveStatValue(stat, agentName);
      const style = stat.style || 'number';
      const hasValue = v !== undefined && v !== null && v !== '';
      const icon = stat.icon ? escapeHtml(stat.icon) + ' ' : '';

      if (style === 'dot') {
        const skip = v === -1;
        const ok = v === true || v === 1;
        const dot = skip ? '⊘' : ok ? '✓' : hasValue ? '✗' : '—';
        const label = skip ? 'skipped' : ok ? 'ok' : hasValue ? 'fail' : 'unknown';
        const cls = skip ? 'skip' : ok ? 'ok' : hasValue ? 'fail' : 'unknown';
        return `<span class="quality-stat-value ${cls}">${dot} ${label}</span>`;
      }

      if (style === 'pct') {
        const PCT_GOOD = 90;
        const PCT_WARN = 70;
        const cls = !hasValue ? 'unknown' : Number(v) >= PCT_GOOD ? 'good' : Number(v) >= PCT_WARN ? 'warn' : 'bad';
        return `<span class="health-ci ${cls}">${hasValue ? escapeHtml(String(v)) + '%' : '—'}</span>`;
      }

      if (style === 'pct-bar') {
        const numericTarget = typeof stat.target === 'number' ? stat.target : 100;
        const pct = hasValue ? Number(v) : 0;
        const TARGET_WARN_OFFSET = 10;
        const covCls = !hasValue ? '' : pct >= numericTarget ? 'ind-ok' : pct >= numericTarget - TARGET_WARN_OFFSET ? 'ind-warn' : 'ind-err';
        return `<span class="ind-num ${covCls}">${hasValue ? escapeHtml(String(v)) + '%' : '—'}</span> <span class="ind-dlabel">goal: ${numericTarget}%</span>`;
      }

      if (style === 'spark') {
        const statKey = stat.key || stat.field;
        const trendField = stat.trendField || stat.field;
        const color = SPARK_COLORS[trendField] || DEFAULT_SPARK_COLOR;
        const histSeries = historyData.map(s => s.agentStats?.[agentName]?.[statKey] ?? 0);
        const hasHistData = histSeries.some(v => v > 0);
        const trendSeries = hasHistData ? histSeries : (window._trendData || []).map(s => s[trendField] || 0);
        const trendTimes = hasHistData ? getHistoryTimes() : (window._trendData || []).map(s => s.t ?? s.timestamp);
        return `<span class="ind-num">${icon}${hasValue ? escapeHtml(String(v)) : '0'}</span> ${sparkSvg(trendSeries, color, trendTimes, `agent-stat:${agentName}:${statKey}`)}`;
      }

      return `<span class="ind-num">${icon}${hasValue ? escapeHtml(String(v)) : '0'}</span>`;
    }

    function renderQualityStatsCard(agents) {
      const quality = qualityDiagnosticsAgent(agents);
      const stats = (quality && Array.isArray(quality.statsConfig)) ? quality.statsConfig : [];
      const emptyAction = '<button class="hv-btn btn-secondary btn-sm" data-action="openConfigDialog" data-config-type="agent" data-agent="quality" data-tab="Stats">Configure quality stats</button>';
      if (!quality || stats.length === 0) {
        return `<div class="quality-stat-card"><div class="quality-stat-title">Quality stats</div><div class="quality-stat-empty">No stats configured — add them under quality → Configuration → Stats.</div>${emptyAction}</div>`;
      }
      const rows = stats.map(stat => {
        const labelText = stat.label || stat.key || stat.field || 'Stat';
        const icon = stat.icon ? `${escapeHtml(stat.icon)} ` : '';
        const desc = stat.desc ? ` title="${escapeHtml(stat.desc)}"` : '';
        const href = statTargetHref(stat);
        const labelHtml = href
          ? `<a href="${escapeHtml(href)}" target="_blank" rel="noopener"${desc}>${icon}${escapeHtml(labelText)}</a>`
          : `<span${desc}>${icon}${escapeHtml(labelText)}</span>`;
        return `<div class="quality-stat-row">${labelHtml}<span>${renderQualityStatValue(stat, quality.name)}</span></div>`;
      }).join('');
      return `<div class="quality-stat-card"><div class="quality-stat-title">Quality stats</div>${rows}</div>`;
    }

