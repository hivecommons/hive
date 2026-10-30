    async function fetchHistory() {
      try {
        const evalEntries = await fetchDashboardHistory('/api/history');
        if (evalEntries === null) return;
        // Ensure token history is loaded before merging (fixes race on initial page load)
        if (!window._serverTokenHistory) {
          try {
            const tr = await fetch('/api/trends?range=week');
            const td = await tr.json();
            window._serverTokenHistory = td && td.tokenHistory ? td.tokenHistory : [];
            window._trendData = td && td.evals ? td.evals : (Array.isArray(td) ? td : []);
          } catch { window._serverTokenHistory = []; }
        }
        const tokenHist = window._serverTokenHistory || [];
        const MAX_MERGE_DELTA_MS = 60000;
        const matchedTokenIndices = new Set();
        if (tokenHist.length > 0) {
          for (const entry of (evalEntries || [])) {
            const entryT = entry.t || 0;
            let best = null;
            let bestDelta = Infinity;
            let bestIdx = -1;
            for (let i = 0; i < tokenHist.length; i++) {
              const delta = Math.abs((tokenHist[i].t || 0) - entryT);
              if (delta < bestDelta) {
                bestDelta = delta;
                best = tokenHist[i];
                bestIdx = i;
              }
            }
            if (best && bestDelta < MAX_MERGE_DELTA_MS) {
              entry.tokenInput = best.tokenInput || 0;
              entry.tokenOutput = best.tokenOutput || 0;
              entry.tokenCacheRead = best.tokenCacheRead || 0;
              entry.tokenCacheCreate = best.tokenCacheCreate || 0;
              entry.tokenMessages = best.tokenMessages || 0;
              if (best.tokens) entry.tokens = best.tokens;
              if (best.tokenModels) entry.tokenModels = best.tokenModels;
              matchedTokenIndices.add(bestIdx);
            }
          }
          // Inject unmatched token entries so sparklines survive refresh
          for (let i = 0; i < tokenHist.length; i++) {
            if (!matchedTokenIndices.has(i)) {
              const th = tokenHist[i];
              evalEntries.push({
                t: th.t,
                tokenInput: th.tokenInput || 0,
                tokenOutput: th.tokenOutput || 0,
                tokenCacheRead: th.tokenCacheRead || 0,
                tokenCacheCreate: th.tokenCacheCreate || 0,
                tokenMessages: th.tokenMessages || 0,
                tokens: th.tokens || {},
                tokenModels: th.tokenModels || {},
                tokenTotal: Object.values(th.tokens || {}).reduce((a, b) => a + b, 0),
              });
            }
          }
          evalEntries.sort((a, b) => (a.t || 0) - (b.t || 0));
        }
        historyData = capHistory(evalEntries || []);
        // Re-fold persisted governor/repo/beads trends into the freshly
        // reassigned historyData so those sparklines survive the 30s refresh.
        mergeTrendHistory();
      } catch (e) { /* ignore */ }
    }
    // fetchHistory() — deferred to _deferredInit
    const HISTORY_REFRESH_MS = 30000;

    function formatAge(sec) {
      if (sec < 0) return '—';
      if (sec < 120) return sec + 's';
      if (sec < 3600) return Math.floor(sec / 60) + 'm';
      return Math.floor(sec / 3600) + 'h';
    }

    let currentAgentMetrics = {};
    function resolveStatValue(stat, name) {
      const src = stat.source;
      const field = stat.field;
      // Resolver-backed stats (source: resolver/env/static/script/http) are
      // computed server-side by the variable-resolution engine and delivered as
      // a pre-resolved `value`. Prefer it; the client-side sources below stay
      // exactly as they were for the dashboard-native sources.
      if (stat.value !== undefined) return stat.value;
      if (src === 'agentMetrics') {
        const m = currentAgentMetrics[name] || {};
        return m[field];
      }
      if (src === 'health') {
        const h = window._healthData || {};
        return h[field];
      }
      if (src === 'tokens') {
        const t = (window._tokensByAgent || {})[name] || {};
        return t[field];
      }
      if (src === 'status') {
        const repos = (window._lastStatus || {}).repos || [];
        if (field === 'actionableCount') return repos.reduce((n, r) => n + (r.actionableIssues || []).length, 0);
        if (field === 'openPrCount') return repos.reduce((n, r) => n + (r.openPrs || []).length, 0);
        if (field === 'mergeableCount') return repos.reduce((n, r) => n + (r.openPrs || []).filter(prMergeable).length, 0);
        return 0;
      }
      return undefined;
    }

    const SPARK_COLORS = { stars: '#e3b341', outreachOpen: 'var(--blue)', outreachMerged: 'var(--green)', actionable: 'var(--red)', openPrs: 'var(--purple)', mergeable: 'var(--green)' };
    const DEFAULT_SPARK_COLOR = 'var(--blue)';

    function statTargetHref(stat) {
      const target = stat && stat.target;
      if (typeof target !== 'string') return '';
      const href = target.trim();
      return /^(https?:\/\/|\/|#)/.test(href) ? href : '';
    }

    function renderStatHtml(stat, name) {
      const v = resolveStatValue(stat, name);
      const style = stat.style;
      const label = escapeHtml(stat.label || stat.key);
      const icon = stat.icon ? escapeHtml(stat.icon) + ' ' : '';

      const desc = stat.desc || '';
      const tipAttr = desc ? ` title="${escapeHtml(desc)}" style="cursor:help"` : '';

      if (style === 'dot') {
        const dotCls = v === 1 ? 'ok' : v === 0 ? 'fail' : 'unknown';
        return `<span class="ind-dot-item"${tipAttr}><span class="health-dot ${dotCls}"></span><span class="ind-dlabel">${label}</span></span>`;
      }

      /* A percentage with no underlying metric renders as "—", the same way
         LAST KICK / AVG/PASS do on the card. `v || 0` turned an absent value
         into a hard 0% — indistinguishable from a real, catastrophic result
         (#7411: a reviewer with no coverage at all showed "0% vs goal N%"). */
      const hasValue = v !== undefined && v !== null && v !== '';

      if (style === 'pct') {
        const PCT_GOOD = 90;
        const PCT_WARN = 70;
        const cls = !hasValue ? 'unknown' : v >= PCT_GOOD ? 'good' : v >= PCT_WARN ? 'warn' : 'bad';
        return `<span class="ind-dot-item"${tipAttr}><span class="health-ci ${cls}">${hasValue ? v + '%' : '—'}</span><span class="ind-dlabel">${label}</span></span>`;
      }

      if (style === 'pct-bar') {
        const pct = hasValue ? v : 0;
        // The configured goal (governor.coverage_target) rides in the agent's own
        // metrics; the layout's static target is only the fallback.
        const metricTarget = (currentAgentMetrics[name] || {}).coverageTarget;
        const target = (typeof metricTarget === 'number' && metricTarget > 0) ? metricTarget : (stat.target || 100);
        const TARGET_WARN_OFFSET = 10;
        const covCls = !hasValue ? '' : pct >= target ? 'ind-ok' : pct >= target - TARGET_WARN_OFFSET ? 'ind-warn' : 'ind-err';
        const barColor = pct >= target ? 'var(--green)' : pct >= target - TARGET_WARN_OFFSET ? 'var(--yellow)' : 'var(--red)';
        const MAX_BAR_WIDTH = 120;
        const BAR_HEIGHT = 6;
        return `<div class="ind-group"><span class="ind-group-label">${label.toUpperCase()}</span><div class="ind-dots">
          <span class="ind-dot-item"><span class="ind-num ${covCls}">${hasValue ? pct + '%' : '—'}</span><span class="ind-dlabel">current</span></span>
          <span class="ind-dot-item"><span class="ind-dlabel">goal: ${target}%</span></span>
          <span class="ind-dot-item" style="flex:1;max-width:${MAX_BAR_WIDTH}px">
            <div style="background:var(--border);border-radius:3px;height:${BAR_HEIGHT}px;width:100%;position:relative">
              <div style="background:${barColor};height:100%;border-radius:3px;width:${hasValue ? Math.min(pct / target * 100, 100) : 0}%"></div>
            </div>
          </span>
        </div></div>`;
      }

      if (style === 'spark') {
        const trendField = stat.trendField || stat.field;
        const statKey = stat.key || stat.field;
        const color = SPARK_COLORS[trendField] || DEFAULT_SPARK_COLOR;
        // Try eval history agentStats first (persisted), fall back to trend data
        const histSeries = historyData.map(s => s.agentStats?.[name]?.[statKey] ?? 0);
        const hasHistData = histSeries.some(v => v > 0);
        const trendSeries = hasHistData ? histSeries : (window._trendData || []).map(s => s[trendField] || 0);
        const trendTimes = hasHistData ? getHistoryTimes() : (window._trendData || []).map(s => s.t ?? s.timestamp);
        const spark = sparkSvg(trendSeries, color, trendTimes, `agent-stat:${name}:${statKey}`);
        return `<span class="ind-dot-item"><span class="ind-num">${icon}${v || 0}</span><span class="ind-dlabel">${label} ${spark}</span></span>`;
      }

      // style === 'number' (default)
      return `<span class="ind-dot-item"><span class="ind-num">${icon}${v || 0}</span><span class="ind-dlabel">${label}</span></span>`;
    }

    function renderStatsFromConfig(stats, name) {
      if (!stats || stats.length === 0) return '';
      const items = stats.map(s => renderStatHtml(s, name));
      return `<div class="agent-indicators"><div class="ind-group"><div class="ind-dots">${items.join('')}</div></div></div>`;
    }

