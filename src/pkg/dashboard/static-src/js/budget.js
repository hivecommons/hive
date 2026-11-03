    // ── Budget window history chart (#4298) ──────────────────────────────
    // "Did the budget stop the hive while I was gone?" — a bar per budget
    // window from GET /api/budget/history (#4320's durable series), with the
    // per-window limit drawn as a marker and reset times on the x-axis.
    let _budgetHistory = null; // null until first fetch; {windows:[closed, newest first], current:{...}}
    async function fetchBudgetHistory() {
      try {
        const r = await fetch('/api/budget/history');
        if (!r.ok) return;
        _budgetHistory = await r.json();
      } catch { /* non-critical; chart shows its empty state until the next fetch */ }
    }

    function budgetHistoryChart() {
      const h = _budgetHistory;
      if (!h) return '';
      // Closed windows arrive newest-first (epoch-ms bounds); chart reads oldest → newest.
      const wins = (Array.isArray(h.windows) ? h.windows.slice().reverse() : [])
        .map(w => ({ start: w.windowStart, end: w.windowEnd, limit: w.limit || 0, used: w.used || 0, pctUsed: w.pctUsed || 0, exhausted: !!w.exhausted, open: false }));
      const cur = h.current;
      if (cur && cur.limit > 0) {
        // The open window bounds itself with RFC3339 strings (live status), not epoch ms.
        wins.push({ start: cur.windowStart ? Date.parse(cur.windowStart) : NaN, end: cur.windowEnd ? Date.parse(cur.windowEnd) : NaN,
          limit: cur.limit, used: cur.used || 0, pctUsed: cur.pctUsed || 0, exhausted: !!cur.exhausted, open: true });
      }
      if (!wins.length) return '';
      const hasClosed = wins.some(w => !w.open);
      if (!hasClosed) {
        return `<div style="font-size:0.66rem;color:var(--muted);margin-top:5px" title="One bar per budget period will appear here after the first reset — see GET /api/budget/history">Budget history: no closed periods yet — the chart fills in after the first reset.</div>`;
      }
      const PCT_BUDGET_WARN = 90; // mirrors governor.BudgetWarnPct
      const BAR_SLOT = 30, BAR_W = 20, PAD_L = 40, PAD_R = 4, PAD_T = 6, PAD_B = 14;
      const plotH = 52;
      const W = PAD_L + wins.length * BAR_SLOT + PAD_R, H = PAD_T + plotH + PAD_B;
      const yMax = Math.max(...wins.map(w => Math.max(w.limit, w.used)), 1);
      const yPx = v => PAD_T + plotH - (Math.min(v, yMax) / yMax) * plotH;
      const fmtDate = t => { const d = new Date(t); return isNaN(d) ? '?' : `${d.getMonth() + 1}/${d.getDate()}`; };
      const AXIS_TXT = `font-size="7" fill="var(--muted)"`;
      const bars = wins.map((w, i) => {
        const x = PAD_L + i * BAR_SLOT + (BAR_SLOT - BAR_W) / 2;
        const yUsed = yPx(w.used);
        const fill = w.exhausted ? 'var(--red)' : w.pctUsed >= PCT_BUDGET_WARN ? 'var(--yellow)' : 'var(--green)';
        const tip = `${w.open ? 'OPEN budget period' : 'budget period'} ${formatDashboardDateTime(w.start)} → ${formatDashboardDateTime(w.end)}\nused ${fmtTokens(w.used)} of ${fmtTokens(w.limit)} (${Math.round(w.pctUsed)}%)${w.exhausted ? '\nEXHAUSTED — agent kicks were suspended' : ''}`;
        // Per-window limit marker: the limit in force FOR THAT window, so a
        // later limit change doesn't redraw history.
        const yLim = yPx(w.limit);
        return `<g><title>${esc(tip)}</title>` +
          `<rect x="${x.toFixed(1)}" y="${yUsed.toFixed(1)}" width="${BAR_W}" height="${(PAD_T + plotH - yUsed).toFixed(1)}" fill="${fill}" opacity="${w.open ? '0.45' : '0.85'}" rx="1"/>` +
          (w.limit > 0 ? `<line x1="${(x - 2).toFixed(1)}" y1="${yLim.toFixed(1)}" x2="${(x + BAR_W + 2).toFixed(1)}" y2="${yLim.toFixed(1)}" stroke="var(--red)" stroke-width="1" stroke-dasharray="3,2"/>` : '') +
          `<text x="${(x + BAR_W / 2).toFixed(1)}" y="${H - 4}" text-anchor="middle" ${AXIS_TXT}>${w.open ? 'open' : fmtDate(w.end)}</text>` +
          `</g>`;
      }).join('');
      return `<div class="mt-3">
        <div style="font-size:0.66rem;color:var(--muted);margin-bottom:var(--sp-1)" title="One bar per budget period (from /api/budget/history). Bar height = tokens used during that period; dashed red line = the limit then in force; x-axis labels = reset times. Red bar = budget exhausted (kicks were suspended); the translucent bar is the current period.">Budget history — tokens used vs. limit per reset period</div>
        <div style="overflow-x:auto"><svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}">
          <line x1="${PAD_L}" y1="${PAD_T}" x2="${PAD_L}" y2="${PAD_T + plotH}" stroke="var(--border)" stroke-width="1"/>
          <line x1="${PAD_L}" y1="${PAD_T + plotH}" x2="${W - PAD_R}" y2="${PAD_T + plotH}" stroke="var(--border)" stroke-width="1"/>
          <text x="${PAD_L - 3}" y="${PAD_T + 6}" text-anchor="end" ${AXIS_TXT}>${fmtTokens(yMax)}</text>
          <text x="${PAD_L - 3}" y="${PAD_T + plotH}" text-anchor="end" ${AXIS_TXT}>0</text>
          ${bars}
        </svg></div>
      </div>`;
    }

    // ── Budget window PHASE (#4686) ─────────────────────────────────────────
    // "37h until reset" says how long is left but never says WHEN — and an
    // operator who configured "1B tokens every 2 days" is asking exactly that:
    // which 2-day window am I in, and when does the next one start? A countdown
    // cannot be lined up against a calendar, so the absolute bounds are stated
    // and the countdown kept as the at-a-glance number.
    //
    // WINDOW_STARTS_AT / WINDOW_ENDS_AT are RFC3339 from the live status and
    // are both optional — they are empty until a limit is set and a window has
    // opened. Every caller must survive that, so these degrade to the bare
    // countdown rather than rendering "Invalid Date".
    function budgetWindowBounds(budget) {
      const parse = (s) => {
        if (!s) return null;
        const d = new Date(s);
        return isNaN(d.getTime()) ? null : d;
      };
      return { start: parse(budget && budget.WINDOW_STARTS_AT), end: parse(budget && budget.WINDOW_ENDS_AT) };
    }

    // fmtWindowStamp keeps the inline label short — a full date/time string on
    // both ends does not fit the budget bar. The tooltip carries the full,
    // unambiguous timestamps.
    function fmtWindowStamp(d) {
      return formatDashboardDateTime(d, { month:'short', day:'numeric', hour:'2-digit', minute:'2-digit', timeZoneName:'short' });
    }

    function budgetWindowPhaseText(budget, hoursLeft) {
      const { start, end } = budgetWindowBounds(budget);
      if (!start && !end) return `${hoursLeft}h until reset`;
      if (start && end) return `Budget period ${fmtWindowStamp(start)} → ${fmtWindowStamp(end)} · ${hoursLeft}h until reset`;
      return `Resets ${fmtWindowStamp(end || start)} · ${hoursLeft}h left`;
    }

    function budgetWindowPhaseTitle(budget) {
      const { start, end } = budgetWindowBounds(budget);
      if (!start && !end) return 'The budget period has not opened yet — it opens when a token limit is set and the governor first evaluates.';
      const parts = [];
      if (start) parts.push(`This budget period opened ${formatDashboardDateTime(start)}`);
      if (end) parts.push(`spend resets to 0 at ${formatDashboardDateTime(end)}`);
      return parts.join('; ') + '. Period length is governor.budget.period_days.';
    }

    function renderBudgetBar(budget) {
      if (!budget || (!budget.BUDGET_WEEKLY && !budget.COIN_BUDGET && !budget.USD_BUDGET)) return '';
      const PCT_SAFE = 50;
      const PCT_WARN = 85;
      const coinBudget = Number(budget.COIN_BUDGET) || 0;
      const coinUsed = Number(budget.COINS_USED) || 0;
      const coinLabel = budget.COIN_LABEL || 'coins';
      const usdBudget = Number(budget.USD_BUDGET) || 0;
      const usdUsed = Number(budget.USD_USED) || 0;
      const used = budget.BUDGET_USED || 0;
      const weekly = budget.BUDGET_WEEKLY;
      const tokenPct = weekly > 0 ? (budget.BUDGET_PCT_USED || 0) : 0;
      const coinPct = coinBudget > 0 ? (coinUsed / coinBudget * 100) : 0;
      const usdPct = usdBudget > 0 ? (usdUsed / usdBudget * 100) : 0;
      const choices = [
        weekly > 0 ? { pct: tokenPct, label: `${fmtTokens(used)} / ${fmtTokens(weekly)}`, name: 'tokens' } : null,
        coinBudget > 0 ? { pct: coinPct, label: `${fmtCoins(coinUsed, coinLabel)} / ${fmtCoins(coinBudget, coinLabel)}`, name: coinLabel } : null,
        usdBudget > 0 ? { pct: usdPct, label: `${fmtUSD(usdUsed)} / ${fmtUSD(usdBudget)}`, name: 'USD' } : null,
      ].filter(Boolean);
      const primary = choices.slice().sort((a, b) => b.pct - a.pct)[0] || choices[0];
      const pctUsed = primary ? primary.pct : 0;
      const usedText = primary ? primary.label : '';
      const secondaryBudgetText = choices.length > 1 ? ' · ' + choices.filter(c => c !== primary).map(c => `${c.name} ${c.label}`).join(' · ') : '';
      const projPct = budget.PROJECTED_PCT || 0;
      const burnRate = budget.BURN_RATE_HOURLY || 0;
      const hoursLeft = Math.max(0, Math.ceil(budget.WINDOW_HOURS_REMAINING || 0));
      const HOURS_PER_DAY = 24;
      const dailyRate = burnRate * HOURS_PER_DAY;
      const PCT_BUDGET_WARN = 90; // mirrors governor.BudgetWarnPct: soft-warning threshold
      const exhausted = !!budget.BUDGET_EXHAUSTED;
      const useCoinPrimary = !!primary && primary.name !== 'tokens'; // projection is token-based
      // Red is reserved for exhausted (kicks suspended); >=90% shows amber.
      const usedFillCls = budgetIgnored ? 'safe' : exhausted ? 'danger' : pctUsed >= PCT_SAFE ? 'warning' : 'safe';

      let govAction = '';
      if (budgetIgnored) {
        govAction = '<span class="text-muted">Budget enforcement disabled by operator</span>';
      } else if (exhausted) {
        const windowEnds = budget.WINDOW_ENDS_AT ? formatDashboardDateTime(budget.WINDOW_ENDS_AT) : 'the next window';
        govAction = `<span class="text-danger" title="Agents on the budget exempt list keep running">budget exhausted — agent kicks suspended until ${windowEnds}</span> <button class="hv-btn btn-danger" data-action="resetBudgetWindow" title="Open a fresh budget window now: spend restarts at 0 and suspended kicks resume. Budget enforcement stays on — raise Total Tokens in Settings first if the limit itself was too small.">reset window now</button>`;
      } else if (pctUsed >= PCT_BUDGET_WARN) {
        govAction = `<span class="text-warn">Budget warning — ${pctUsed}% of period limit used; kicks suspend at 100%</span>`;
      } else if (projPct > PCT_WARN) {
        govAction = '<span class="text-danger">Governor downgrading models to stay within budget</span>';
      } else if (projPct > PCT_SAFE) {
        govAction = '<span class="text-warn">Governor may downgrade non-priority agents if burn increases</span>';
      } else {
        govAction = '<span class="text-success">Budget healthy — governor using preferred models</span>';
      }

      const ignoreCheck = `<label style="font-size:var(--fs-xs);color:var(--muted);cursor:pointer;margin-left:var(--sp-5)" title="When checked, the governor ignores budget limits entirely. No model downgrading, no kick skipping. Use with caution — spending will be uncapped until unchecked.">
        <input type="checkbox" ${budgetIgnored ? 'checked' : ''} data-change-action="toggleBudgetIgnore" style="cursor:pointer;vertical-align:middle"> ignore budget
      </label><button class="hv-btn btn-danger" data-action="resetBudgetWindow" title="Open a fresh budget window now: spend re-anchors at 0 and the window timer restarts. Budget enforcement stays on.">reset window</button>`;

      return `<div class="budget-bar">
        <div class="budget-bar-label">
          <span>Used: ${usedText} (${Math.round(pctUsed)}%)${secondaryBudgetText}${ignoreCheck}</span>
          <span title="${esc(budgetWindowPhaseTitle(budget))}">${budgetWindowPhaseText(budget, hoursLeft)}</span>
        </div>
        <div class="budget-bar-track"><div class="budget-bar-fill ${usedFillCls}" style="width:${Math.min(pctUsed, 100)}%"></div></div>
        <div style="display:flex;justify-content:space-between;font-size:var(--fs-xs);color:var(--muted);margin-top:var(--sp-2)">
          <span>Burn: ${fmtTokens(burnRate)}/hr · ${fmtTokens(dailyRate)}/day</span>
          <span>Projected: ${useCoinPrimary ? '—' : fmtTokens(used + burnRate * hoursLeft)} (${useCoinPrimary ? Math.round(pctUsed) : projPct}%)</span>
        </div>
        <div style="font-size:var(--fs-xs);margin-top:3px">${govAction}</div>
        ${budgetHistoryChart()}
      </div>`;
    }

