    // ---- Unified cost ($) view ----
    // Fetches /api/cost on an interval and renders both ESTIMATED $ (token
    // counts × list prices, for Claude/Copilot/Codex/vLLM) and NATIVE $ (real
    // spend reported by OpenRouter / LiteLLM gateways). Estimates are always
    // labeled "est." so they never masquerade as actual billing.
    const COST_POLL_MS = 60000;
    // How many leading characters of a session id to show before eliding — full
    // id stays available on hover (title). Session ids are UUID-length, so a
    // short prefix is enough to disambiguate visually.
    const COST_SESSION_ID_CHARS = 8;
    // A session whose latest event is newer than this is presumed still running,
    // so the cost-per-session table shows "active" instead of a misleading end
    // time (the token scanner only sees events, not process exit).
    const COST_SESSION_ACTIVE_MS = 5 * 60 * 1000;
    // Format a dollar amount with thousands separators (commas). Small amounts
    // keep 2-decimal cents; amounts >= $100 are shown as whole dollars but still
    // comma-grouped (e.g. $20,849). Sentinels for null / sub-cent are preserved.
    function fmtUSD(n) {
      if (n == null || isNaN(n)) return '—';
      if (n === 0) return '$0.00';
      if (n > 0 && n < 0.01) {
        let s = Number(n).toPrecision(3);
        if (s.includes('e')) s = Number(s).toFixed(Math.ceil(-Math.log10(Number(n))) + 3);
        s = s.replace(/(\.\d*?[1-9])0+$/, '$1').replace(/\.0+$/, '');
        return '$' + s;
      }
      const frac = n < 100 ? 2 : 0;
      return '$' + formatNumber(n, 'en-US', { minimumFractionDigits: frac, maximumFractionDigits: frac });
    }
    // fmtUSDWhole rounds to the nearest dollar, comma-grouped, never cents:
    // "$1,010", "$10", "$0". Anything that would round to $0 but is non-zero
    // shows "<$1" so a small, real spend is not mistaken for none. Used by
    // the Governor RUNNING tile's today / week / month windows (#15614);
    // the cost panel keeps the precise fmtUSD.
    function fmtUSDWhole(n) {
      if (n == null || isNaN(n)) return '—';
      if (n > 0 && n < 0.5) return '<$1';
      return '$' + formatNumber(Math.round(n), 'en-US', { maximumFractionDigits: 0 });
    }

    // Headline wording (#13484): one plainly-named figure. "lineage" and
    // "persisted history" were implementation terms; the card now says what
    // the number is (an estimate at list price), from when, and that it is
    // not an invoice. The live token summary is shown as a subset of the
    // headline, dated from its last reset rather than from the oldest
    // surviving session file.
    const COST_HEADLINE_LABEL = 'Estimated spend at list price';
    const COST_NOT_A_BILL = 'tokens × public per-token price · not a bill';

    function costTotalLabel(total) {
      if (total && total.firstTimestamp) {
        return `${COST_HEADLINE_LABEL} — since ${fmtCostDateTime(total.firstTimestamp)} · ${COST_NOT_A_BILL}`;
      }
      return `${COST_HEADLINE_LABEL} — no recorded history yet`;
    }

    // costLiveSummaryLabel describes the live token summary (what /api/cost
    // estimated.total_usd holds right now) relative to the headline.
    function costLiveSummaryLabel(total) {
      if (!total || total.currentUSD == null) return '';
      const lastReset = total.resets.length ? total.resets[total.resets.length - 1].timestamp : 0;
      let since = '';
      if (lastReset) since = ` since the last reset on ${fmtCostDateTime(lastReset)}`;
      else if (total.currentStart) since = ` (sessions since ${fmtCostDateTime(total.currentStart)})`;
      return `of which ${fmtUSD(total.currentUSD)} is in the live token summary${since}`;
    }

    function costTotalTooltip(total, cost) {
      if (!total || !total.series.length) return 'no cost records yet';
      const priceDate = cost && cost.price_table_date ? ` (price table ${cost.price_table_date})` : '';
      const parts = [
        `Estimate = recorded tokens × each model's public list price${priceDate}. Not an invoice: Copilot / Claude subscription seats and self-hosted inference are not billed per token, so real spend can be far lower.`,
        `${fmtUSD(total.usd)} recorded since ${fmtCostDateTime(total.firstTimestamp)}.`,
      ];
      if (total.currentUSD != null) {
        const since = total.currentStart ? ` (sessions since ${fmtCostDateTime(total.currentStart)})` : '';
        parts.push(`The live token summary currently holds ${fmtUSD(total.currentUSD)}${since}.`);
      }
      if (total.resets.length) {
        parts.push(`That summary was reset ${total.resets.length} time(s) — ${total.resets.map(r => `${fmtCostDateTime(r.timestamp)}: ${fmtUSD(r.previous)} → ${fmtUSD(r.next)}`).join('; ')} — and the spend before each reset is kept in the headline.`);
      }
      return parts.join(' ');
    }

    // costCountAsOf renders when the merged / closed counts were last confirmed
    // (count_updated_at), and — when GitHub has since refused the refresh — how
    // long they have been stale, so a cached denominator is never presented as
    // live (#13484).
    function costCountAsOf(cost) {
      if (!cost) return '';
      // count_error names why the attributed refresh failed or was partial
      // so a stale / partial / fallback divisor is never a bare "—".
      const err = cost.count_error ? ` — ${cost.count_error}` : '';
      if (cost.count_stale) {
        const since = cost.count_stale_since || cost.count_updated_at;
        const when = since ? ` since ${fmtOverviewStaleSince(since)}` : '';
        const why = cost.count_status && !/^stale/.test(cost.count_status) ? ` (${cost.count_status})` : (cost.count_status && /rate-limited/.test(cost.count_status) ? ' (GitHub rate-limited)' : '');
        return `counts stale${when}${why}${err}`;
      }
      if (cost.count_partial) {
        const asOf = cost.count_updated_at ? ` as of ${fmtOverviewStaleSince(cost.count_updated_at)}` : '';
        return `${cost.count_status || 'counts partial'}${asOf}${err}`;
      }
      return cost.count_updated_at ? `counts as of ${fmtOverviewStaleSince(cost.count_updated_at)}` : '';
    }

    function costCountLabel(cost, count, unitLabel, outcomeWindow) {
      if (!cost || !cost.count_available) return cost && cost.count_status ? cost.count_status : 'counts unavailable (GitHub rate-limited)';
      const n = Number(count) || 0;
      const window = outcomeWindow ? ` ${outcomeWindow}` : '';
      const asOf = costCountAsOf(cost);
      return `total ÷ ${formatNumber(n, 'en-US')} ${unitLabel}${window}${asOf ? ` · ${asOf}` : ''}`;
    }

    function costPerUnitDisplay(totalUSD, count, unitLabel, countsAvailable = true, countStatus = '', countError = '') {
      const total = Number(totalUSD) || 0;
      const n = Number(count) || 0;
      if (!(total > 0)) return { value: '—', title: 'no cost records yet' };
      if (!countsAvailable) return { value: '—', title: countStatus || 'counts unavailable (GitHub rate-limited)' };
      if (!(n > 0)) return { value: '—', title: `no ${unitLabel} yet` };
      const value = total / n;
      const status = countStatus ? ` (${countStatus}${countError ? ' — ' + countError : ''})` : (countError ? ` (${countError})` : '');
      return {
        value: fmtUSD(value),
        title: `${fmtUSD(total)} ÷ ${formatNumber(n, 'en-US')} ${unitLabel} = ${fmtUSD(value)}${status}. The headline total divided by this count — cost / PR and cost / issue share one total and are not additive.`,
      };
    }

    function costOutcomeWindowLabel(cost, fallbackTotal) {
      const start = Number(cost && cost.count_window_start) || 0;
      return start > 0 ? `since ${fmtCostDateTime(start)}` : 'window pending refresh';
    }

    // Last cost payload, so the range pills can re-render the trend chart
    // instantly (client-only bucketing) without waiting for the next poll.
    let _lastCost = null;
    // ── Collapsible cost panel (mirrors the toggleSection idiom, #1533) ──
    // The cost panel is fully rebuilt via innerHTML on every /api/cost poll, so
    // the collapsed state cannot live only in the DOM — it is persisted in
    // localStorage and re-applied after each render so a poll never re-expands it.
    const COST_COLLAPSED_LS_KEY = 'hive-cost-collapsed';
    function isCostCollapsed() {
      return isSectionCollapsed('cost-panel');
    }
    function setCostCollapsed(collapsed) {
      setSectionCollapsed('cost-panel', collapsed);
    }
    /** Reflect the persisted collapsed state onto the current cost-panel DOM. */
    function applyCostCollapse() {
      applySectionCollapse('cost-panel');
    }
    /** Toggle collapsed state on click / keyboard, persist, and animate. */
    function toggleCostPanel() {
      setCostCollapsed(!isCostCollapsed());
      applyCostCollapse();
    }
    /** Enter/Space activate the header like a button. */
    function costHeaderKey(ev) {
      if (ev.target && ev.target.closest && ev.target.closest('.section-help-mark')) return;
      if (ev.key === 'Enter' || ev.key === ' ' || ev.key === 'Spacebar') {
        ev.preventDefault();
        toggleCostPanel();
      }
    }

    // ── Collapsible cost subsections (usage by sandbox / cost per session) ──
    // Same localStorage + re-apply-after-render idiom as the cost panel above:
    // the tables are always rendered into the DOM on every poll (collapse is
    // CSS-only via max-height), so a collapsed subsection keeps receiving fresh
    // data and shows current numbers the moment it is re-expanded. Defaults on
    // first visit: "cost per session" collapsed, "usage by sandbox" expanded.
    // "cost by repo" starts collapsed because it is an INFERRED attribution
    // whose caveats need reading before its numbers are acted on (#4836).
    const COST_SUB_LS_PREFIX = 'hive-cost-sub-';
    const COST_SUB_DEFAULT_COLLAPSED = { session: true, sandbox: false, repo: true };
    function isCostSubCollapsed(key) {
      const v = localStorage.getItem(COST_SUB_LS_PREFIX + key);
      if (v === null) return !!COST_SUB_DEFAULT_COLLAPSED[key];
      return v === '1';
    }
    // Markup-time form of applyCostSubCollapse: the sub-table is emitted
    // already collapsed, so a re-render never inserts it open and animates it
    // shut 200ms later (#12512).
    function costSubCollapsedCls(key) {
      return isCostSubCollapsed(key) ? ' collapsed' : '';
    }
    function costSubAriaExpanded(key) {
      return isCostSubCollapsed(key) ? 'false' : 'true';
    }
    function setCostSubCollapsed(key, collapsed) {
      // Store explicitly (never remove) so a user choice that matches the
      // default's opposite survives reloads for both subsections.
      localStorage.setItem(COST_SUB_LS_PREFIX + key, collapsed ? '1' : '0');
    }
    /** Reflect each subsection's persisted collapsed state onto the current DOM. */
    function applyCostSubCollapse() {
      for (const sec of document.querySelectorAll('#cost-panel [data-cost-sub]')) {
        const key = sec.dataset.costSub;
        const collapsed = isCostSubCollapsed(key);
        const body = sec.querySelector('.cost-sub-body');
        const chevron = sec.querySelector('.cost-chevron');
        const header = sec.querySelector('.cost-sub-toggle');
        if (body) body.classList.toggle('collapsed', collapsed);
        if (chevron) chevron.classList.toggle('collapsed', collapsed);
        if (header) header.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      }
    }
    /** Toggle one subsection's collapsed state and persist. */
    function toggleCostSub(key) {
      setCostSubCollapsed(key, !isCostSubCollapsed(key));
      applyCostSubCollapse();
    }
    /** Single click + keyboard entry point (toggleWelcomeCheck idiom): Enter/Space or click. */
    function costSubToggle(ev, key) {
      if (ev && ev.type === 'keydown') {
        if (ev.key !== 'Enter' && ev.key !== ' ' && ev.key !== 'Spacebar') return;
        ev.preventDefault();
      }
      toggleCostSub(key);
    }

    const COST_SORTS = {
      model: { key: 'usd', dir: 'desc' },
      sandbox: { key: 'usd', dir: 'desc' },
      session: { key: 'started', dir: 'desc' },
    };
    function costSortDir(table, key) {
      const s = COST_SORTS[table];
      return s && s.key === key ? s.dir : '';
    }
    function costSortTh(table, key, label, opts = {}) {
      const dir = costSortDir(table, key);
      const title = opts.title ? ` title="${esc(opts.title)}"` : '';
      const style = opts.align ? ` style="text-align:${opts.align}"` : '';
      const aria = dir ? ` aria-sort="${dir === 'asc' ? 'ascending' : 'descending'}"` : ' aria-sort="none"';
      const indicator = dir ? `<span class="cost-sort-ind" aria-hidden="true">${dir === 'asc' ? '▲' : '▼'}</span>` : '';
      return `<th class="cost-sortable"${style}${title}${aria} role="button" tabindex="0" data-action="costSortTable" data-keydown-action="costSortTable" data-arg-types="e,s,s" data-arg1="${table}" data-arg2="${key}" data-keys="*">${label}${indicator}</th>`;
    }
    function costSortTable(ev, table, key) {
      if (ev && ev.type === 'keydown') {
        if (ev.key !== 'Enter' && ev.key !== ' ' && ev.key !== 'Spacebar') return;
        ev.preventDefault();
      }
      const s = COST_SORTS[table];
      if (!s) return;
      s.dir = s.key === key && s.dir === 'asc' ? 'desc' : 'asc';
      s.key = key;
      renderCost(_lastCost);
    }
    function costCmp(a, b, key, dir, fallback) {
      const av = a[key], bv = b[key];
      const na = typeof av === 'number', nb = typeof bv === 'number';
      let c;
      if (na || nb) c = (Number(av) || 0) - (Number(bv) || 0);
      else c = String(av ?? '').localeCompare(String(bv ?? ''), undefined, { sensitivity: 'base', numeric: true });
      if (c !== 0) return dir === 'desc' ? -c : c;
      return fallback ? fallback(a, b) : 0;
    }
    function sortCostRows(rows, table, fallback) {
      const s = COST_SORTS[table] || { key: 'usd', dir: 'desc' };
      return rows.slice().sort((a, b) => costCmp(a, b, s.key, s.dir, fallback));
    }

    // costTokenBudgetHtml renders the TOKEN budget inside the Cost panel (#4686).
    //
    // The budget bar and the per-window history already existed, but only under
    // Governor. An operator asking "how close is this hive to my token budget?"
    // opens 💵 Cost — and found all-time dollar estimates and cumulative token
    // tables, none of which answer it. Worse, the answer is denominated in the
    // wrong unit: a hive with a token budget does not regulate on dollars and
    // does not know the true price, so the dollar figures are explicitly not
    // what the limit is enforced against.
    //
    // This reuses the same /api/budget/history payload the Governor chart reads
    // (fetched globally on an interval, so it is populated regardless of which
    // section is open) rather than adding a second source of truth.
    function costTokenBudgetHtml() {
      // _lastBudget is the same object renderBudgetBar reads, refreshed on every
      // status poll — so the Cost panel cannot disagree with the Governor bar.
      const budget = window._lastBudget || null;
      const limit = budget && budget.BUDGET_WEEKLY;
      const coinLimit = Number(budget && budget.COIN_BUDGET) || 0;
      const coinLabel = (budget && budget.COIN_LABEL) || 'coins';
      const usdLimit = Number(budget && budget.USD_BUDGET) || 0;
      if (!limit && !coinLimit && !usdLimit) {
        return `<div class="cost-subtitle" style="margin-top:10px">No token budget configured (and no dollar or coin budget) — set <b>Total Tokens</b>, <b>USD Budget</b>, or a backend coin budget and <b>Period Days</b> under Settings → Governor to track spend against a limit. Token budgets are enforced in tokens, not dollars; Bob dollar/coin budgets use the configured backend conversion.</div>`;
      }
      const used = budget.BUDGET_USED || 0;
      const coinUsed = Number(budget.COINS_USED) || 0;
      const usdUsed = Number(budget.USD_USED) || 0;
      const tokenPct = limit > 0 ? (budget.BUDGET_PCT_USED || 0) : 0;
      const coinPct = coinLimit > 0 ? (coinUsed / coinLimit * 100) : 0;
      const usdPct = usdLimit > 0 ? (usdUsed / usdLimit * 100) : 0;
      const choices = [
        limit > 0 ? { unit: 'Token', pct: tokenPct, label: `${fmtTokens(used)} / ${fmtTokens(limit)}` } : null,
        coinLimit > 0 ? { unit: 'Coin', pct: coinPct, label: `${fmtCoins(coinUsed, coinLabel)} / ${fmtCoins(coinLimit, coinLabel)}` } : null,
        usdLimit > 0 ? { unit: 'Dollar', pct: usdPct, label: `${fmtUSD(usdUsed)} / ${fmtUSD(usdLimit)}` } : null,
      ].filter(Boolean);
      const primary = choices.slice().sort((a, b) => b.pct - a.pct)[0] || choices[0];
      const pctUsed = primary.pct || 0;
      const usedLabel = primary.label;
      const secondaryBudgetText = choices.length > 1 ? ' · ' + choices.filter(c => c !== primary).map(c => `${c.unit.toLowerCase()}s ${c.label}`).join(' · ') : '';
      const hoursLeft = Math.max(0, Math.ceil(budget.WINDOW_HOURS_REMAINING || 0));
      const exhausted = !!budget.BUDGET_EXHAUSTED;
      const PCT_BUDGET_WARN = 90;
      const fill = exhausted ? 'danger' : pctUsed >= PCT_BUDGET_WARN ? 'warning' : 'safe';
      return `<div class="cost-token-budget" style="margin-top:14px;padding-top:var(--sp-5);border-top:1px solid var(--border)">
        <div class="cost-subtitle mb-3"><b>${esc(primary.unit)} budget</b> — this is the most-used enforced limit (token limits are in tokens, not dollars). Used amounts cover one reset period; the token tables above are all-time totals and will normally be larger.</div>
        <div class="budget-bar">
          <div class="budget-bar-label">
            <span>Used this budget period: ${usedLabel} (${Math.round(pctUsed)}%)${secondaryBudgetText}</span>
            <span title="${esc(budgetWindowPhaseTitle(budget))}">${budgetWindowPhaseText(budget, hoursLeft)}</span>
          </div>
          <div class="budget-bar-track"><div class="budget-bar-fill ${fill}" style="width:${Math.min(pctUsed, 100)}%"></div></div>
          ${exhausted ? '<div style="font-size:var(--fs-xs);margin-top:3px;color:var(--red)">Budget exhausted — agent kicks are suspended until the period resets.</div>' : ''}
          ${budgetHistoryChart()}
        </div>
      </div>`;
    }

    function fmtCoins(n, label) {
      const v = Number(n) || 0;
      const digits = Math.abs(v) >= 100 ? 0 : Math.abs(v) >= 10 ? 1 : 2;
      return `${formatNumber(v, 'en-US', { maximumFractionDigits: digits })} ${esc(label || 'coins')}`;
    }

    function renderCost(cost) {
      const el = document.getElementById('cost-panel');
      if (!el) return;
      _lastCost = cost;
      if (!cost || (!cost.estimated && (!cost.gateways || cost.gateways.length === 0))) {
        morphInnerHtml(el, sectionCardShell({ id: 'cost-panel', title: '💵 Cost', gripLabel: 'Cost', panelClass: 'cost-panel', bodyClass: 'cost-body', headerClass: 'cost-title cost-toggle', chevronClass: 'cost-chevron', action: 'toggleCostPanel', keyAction: 'costHeaderKey', badgeHtml: '<span class="cost-badge est" title="Estimated from token counts × list prices; subscription/self-hosted billing may differ">est.</span>', summaryClass: 'cost-collapsed-total', summaryHtml: '$0', summaryTitle: COST_HEADLINE_LABEL, bodyId: 'cost-panel-body', collapsed: isCostCollapsed() }, '<div class="text-muted">No cost data yet.</div>'));
        applyCostCollapse();
        return;
      }

      const est = cost.estimated || { total_usd: 0, by_model: [], by_agent: [], unpriced_models: [] };
      const gateways = cost.gateways || [];
      const isTierEstimated = (row) => row && (row.source === 'unpriced' || row.source === 'estimated_tier');
      const usageWindow = estimatedUsageWindowLabel(est);

      // Native gateway cards (OpenRouter / LiteLLM real spend).
      const gwCards = gateways.map(g => {
        const spent = g.spent_usd;
        const limit = g.limit_usd;
        const remain = g.remaining_usd;
        let body;
        if (g.error) {
          body = `<div class="cgerr">native spend unavailable (${esc(g.error)}) — using estimate</div>`;
        } else if (spent != null) {
          let line = `<span class="cgspend">${fmtUSD(spent)}</span> spent`;
          if (limit != null) line += ` of ${fmtUSD(limit)}`;
          if (remain != null) line += ` · ${fmtUSD(remain)} remaining`;
          let meter = '';
          if (limit != null && limit > 0) {
            const pct = Math.min(100, Math.max(0, (spent / limit) * 100));
            meter = `<div class="cgmeter"><span style="width:${pct.toFixed(1)}%"></span></div>`;
          }
          body = `<div>${line}</div>${meter}`;
        } else {
          body = `<div class="cgerr">no native figure reported — using estimate</div>`;
        }
        return `<div class="cost-gateway">
          <div><span class="cgname">${esc(g.name)}</span><span class="cgkind">${esc(g.kind)}</span><span class="cost-badge actual">actual</span></div>
          ${body}
        </div>`;
      }).join('');

      // Live count of running agents currently on each model. Model ids are
      // normalized (lowercase, dots→dashes) so a table row "claude-opus-4.7"
      // matches an agent whose model reads "claude-opus-4-7". Paused/off agents
      // are excluded — this is "using now", not "assigned".
      const normModel = (s) => (s || '').toLowerCase().replace(/\./g, '-').replace(/^.*\//, '');
      // Routing aliases: a config model that only names a concrete model at
      // runtime. An agent set to one of these accrues its tokens under the
      // RESOLVED model (e.g. copilot "auto" → gpt-5.3-codex), so its live badge
      // must be keyed on the resolved model, not the alias, or it lands on no
      // visible row. The backend supplies the resolution via tokens.byAgentModel.
      const aliasModels = new Set(['auto', 'default', '']);
      const byAgentModel = (window._lastStatus?.tokens?.byAgentModel) || {};
      const liveAgentCounts = (() => {
        const counts = {};
        for (const a of (window._lastAgents || [])) {
          if (!a || !a.model) continue;
          // Exclude agents that aren't actually running (same predicate the
          // agent cards use): paused, off-by-cadence, or stopped.
          if (a.paused === true || a.offByCadence === true || a.stopped === true) continue;
          let model = a.model;
          // When the config model is a routing alias, prefer the resolved model
          // parsed from this agent's session logs so the badge lands on the row
          // where the tokens actually accrued. Falls back to the alias if no
          // resolved model is known yet (no sessions scanned).
          if (aliasModels.has(normModel(a.model))) {
            const resolved = byAgentModel[a.name];
            if (resolved) model = resolved;
          }
          const k = normModel(model);
          if (!counts[k]) counts[k] = [];
          counts[k].push(a.displayName || a.name);
        }
        return counts;
      })();

      // Per-model estimated cost table. Every model shows a $ estimate now —
      // exact list price where known, a coarse tier estimate (~) otherwise.
      const models = sortCostRows((est.by_model || []).map(m => {
        const users = liveAgentCounts[normModel(m.name)] || [];
        return { ...m, running: users.length, _users: users };
      }), 'model', (a, b) => String(a.name || '').localeCompare(String(b.name || ''), undefined, { sensitivity: 'base', numeric: true }));
      const modelRows = models.map(m => {
        const approx = isTierEstimated(m);
        const usdCell = approx
          ? `<span title="unknown exact price — tier estimate, not billed spend">~${fmtUSD(m.usd)}</span>`
          : fmtUSD(m.usd);
        const users = m._users || [];
        const agentsCell = users.length
          ? `<span class="cagents-badge" title="Agents using this model now: ${esc(users.join(', '))}">${users.length}</span>`
          : `<span class="cagents-zero" title="No running agent on this model right now">—</span>`;
        return `<tr>
          <td class="cmodel">${esc(m.name || 'unknown')}</td>
          <td class="cnum" style="text-align:center">${agentsCell}</td>
          <td class="cnum">${fmtTokens(m.input)}${miniSparkSvg(modelSeries(m.name, 'i'), 'var(--blue)', `${m.name || 'model'} input tokens`)}</td>
          <td class="cnum">${fmtTokens(m.output)}${miniSparkSvg(modelSeries(m.name, 'o'), 'var(--purple)', `${m.name || 'model'} output tokens`)}</td>
          <td class="cnum">${usdCell}${miniSparkSvg(modelSeries(m.name, 'usd'), 'var(--green)', `${m.name || 'model'} estimated dollars`)}</td>
        </tr>`;
      }).join('');

      const unpricedNote = (est.unpriced_models && est.unpriced_models.length)
        ? `<div class="cost-disclaimer">Tier-estimated (no exact list price, marked ~ and included in the total): ${est.unpriced_models.map(esc).join(', ')}.</div>`
        : '';

      // ── Per-sandbox (per-agent) estimated cost ──
      // In hive each agent runs in its own sandbox, so "usage by sandbox" is the
      // per-agent breakdown. Default sort remains most-expensive first.
      const bySandbox = sortCostRows(est.by_agent || [], 'sandbox', (a, b) => String(a.name || '').localeCompare(String(b.name || ''), undefined, { sensitivity: 'base', numeric: true }));
      const sandboxRows = bySandbox.map(a => {
        const approx = isTierEstimated(a);
        const usdCell = approx
          ? `<span title="unknown exact price — tier estimate for this sandbox's model">~${fmtUSD(a.usd)}</span>`
          : fmtUSD(a.usd);
        return `<tr>
          <td class="cmodel">${esc(a.name || 'unknown')}</td>
          <td class="cnum">${fmtTokens(a.input)}</td>
          <td class="cnum">${fmtTokens(a.output)}</td>
          <td class="cnum">${usdCell}</td>
        </tr>`;
      }).join('');
      const sandboxTable = `<div class="cost-subsection" data-cost-sub="sandbox">
        <div class="cost-subtable-title cost-sub-toggle" role="button" tabindex="0" aria-expanded="${costSubAriaExpanded('sandbox')}" aria-controls="cost-sub-sandbox-body" data-action="costSubToggle" data-keydown-action="costSubToggle" data-arg-types="e,s" data-arg1="sandbox" data-keys="*" title="Estimated cost grouped by sandbox — one agent per sandbox (token × list price). Window: ${esc(usageWindow)}. Click to collapse / expand."><span class="cost-chevron${costSubCollapsedCls('sandbox')}" aria-hidden="true">▼</span>usage by sandbox <span class="cost-window-tag">(${esc(usageWindow)})</span></div>
        <div class="cost-sub-body${costSubCollapsedCls('sandbox')}" id="cost-sub-sandbox-body">
        <div class="cost-table-scroll"><table class="cost-table table-compact">
          <thead><tr>${costSortTh('sandbox', 'name', 'sandbox (agent)')}${costSortTh('sandbox', 'input', 'input', { align: 'right' })}${costSortTh('sandbox', 'output', 'output', { align: 'right' })}${costSortTh('sandbox', 'usd', 'est. $', { align: 'right' })}</tr></thead>
          <tbody>${sandboxRows || '<tr><td class="text-muted" colspan="4">no usage yet</td></tr>'}</tbody>
        </table></div>
        </div>
      </div>`;

      // ── Cost per session ──
      // Each row is one agent run (session) inside its sandbox, priced from its
      // own token splits. The client owns the visible sort state; default is
      // newest session start first, then sandbox name ascending.
      const bySession = sortCostRows(est.by_session || [], 'session', (a, b) =>
        String(a.agent || '').localeCompare(String(b.agent || ''), undefined, { sensitivity: 'base', numeric: true }) ||
        String(a.session_id || '').localeCompare(String(b.session_id || ''), undefined, { sensitivity: 'base', numeric: true }));
      const shortId = (id) => {
        const s = String(id || '');
        return s.length > COST_SESSION_ID_CHARS ? s.slice(0, COST_SESSION_ID_CHARS) + '…' : (s || '—');
      };
      const sessionRows = bySession.map(sn => {
        const approx = isTierEstimated(sn);
        const usdCell = approx
          ? `<span title="unknown exact price — tier estimate for this model">~${fmtUSD(sn.usd)}</span>`
          : fmtUSD(sn.usd);
        // "started → ended" in local time; a still-active session (latest event
        // within COST_SESSION_ACTIVE_MS) shows "active" instead of a fake end.
        const fmtSessTs = (ms) => formatDashboardDateTime(ms, {month:'numeric',day:'numeric',hour:'numeric',minute:'2-digit',hour12:true,timeZoneName:'short'});
        const isoOr = (ms, alt) => ms ? new Date(ms).toISOString() : alt;
        const stillActive = sn.last_active && (Date.now() - sn.last_active) < COST_SESSION_ACTIVE_MS;
        const startTxt = sn.started ? fmtSessTs(sn.started) : '—';
        const endTxt = stillActive
          ? '<span class="cost-session-active">active</span>'
          : (sn.last_active ? fmtSessTs(sn.last_active) : '—');
        const timeCell = (sn.started || sn.last_active)
          ? `<td class="ctime" title="${esc(isoOr(sn.started, 'start unknown'))} → ${esc(isoOr(sn.last_active, 'end unknown'))}${stillActive ? ' (still active)' : ''}">${startTxt} → ${endTxt}</td>`
          : '<td class="ctime">—</td>';
        return `<tr>
          <td class="csession" title="${esc(sn.session_id || '')}">${esc(shortId(sn.session_id))}</td>
          <td class="cmodel">${esc(sn.agent || 'unknown')}</td>
          <td class="cmodel">${esc(sn.model || '—')}</td>
          ${timeCell}
          <td class="cnum">${fmtTokens(sn.input)}</td>
          <td class="cnum">${fmtTokens(sn.output)}</td>
          <td class="cnum">${usdCell}</td>
        </tr>`;
      }).join('');
      const sessionTable = `<div class="cost-subsection" data-cost-sub="session">
        <div class="cost-subtable-title cost-sub-toggle" role="button" tabindex="0" aria-expanded="${costSubAriaExpanded('session')}" aria-controls="cost-sub-session-body" data-action="costSubToggle" data-keydown-action="costSubToggle" data-arg-types="e,s" data-arg1="session" data-keys="*" title="Estimated cost for each individual agent session (sandbox run), sorted by session start newest first by default (token × list price). Click to collapse / expand."><span class="cost-chevron${costSubCollapsedCls('session')}" aria-hidden="true">▼</span>cost per session</div>
        <div class="cost-sub-body${costSubCollapsedCls('session')}" id="cost-sub-session-body">
        <div class="cost-table-scroll"><table class="cost-table table-compact">
          <thead><tr>${costSortTh('session', 'session_id', 'session')}${costSortTh('session', 'agent', 'sandbox')}${costSortTh('session', 'model', 'model')}${costSortTh('session', 'started', 'started → ended', { title: "Session start → end, local time (earliest / latest usage event). Hover a row for full timestamps; 'active' means the session had events in the last few minutes." })}${costSortTh('session', 'input', 'input', { align: 'right' })}${costSortTh('session', 'output', 'output', { align: 'right' })}${costSortTh('session', 'usd', 'est. $', { align: 'right' })}</tr></thead>
          <tbody>${sessionRows || '<tr><td class="text-muted" colspan="7">no sessions yet</td></tr>'}</tbody>
        </table></div>
        </div>
      </div>`;

      // ── Estimated cost by repo (#4836 phase 3) ──
      // An INFERRED attribution, not a measurement: tokens spent between two
      // audited repo= events are billed to the repo named by the later event.
      // Two buckets are mandatory and are rendered as real rows, never folded
      // into the repos or hidden: `unattributed` (spend that cannot honestly be
      // placed) and `backend_unsupported` (Copilot/bob — real spend with no
      // intra-session timing to join against). A repo with no attributable cost
      // shows "—", never "$0.00": those are different facts.
      const repoTable = costRepoTableHtml(lastRepoCost);
      const recordingText = costRecordingNoticeText(cost.recording);
      const recordingNotice = recordingText
        ? `<div class="cost-recording-notice" id="cost-recording-notice" role="alert">${esc(recordingText)} <a href="https://github.com/hivecommons/hive/issues/13487" target="_blank" rel="noopener">#13487</a>.</div>`
        : '';
      // ── Estimated cost by work item (#12128) ──
      // Spend placed against the issue / PR / head SHA each kick was launched
      // for. Attribution only — never a gate; the governor window budget is
      // the single enforced budget.
      const launchTable = costLaunchTableHtml(lastLaunchCost);
      const allTimeCost = costAllTimeEstimate(est);
      const prUnit = costPerUnitDisplay(allTimeCost.usd, cost.merged_prs, 'PRs merged by hive', cost.count_available, cost.count_status, cost.count_error);
      const issueUnit = costPerUnitDisplay(allTimeCost.usd, cost.closed_issues, 'issues closed by hive', cost.count_available, cost.count_status, cost.count_error);
      const outcomeWindow = costOutcomeWindowLabel(cost, allTimeCost);
      const prCountLabel = costCountLabel(cost, cost.merged_prs, 'PRs merged by hive', outcomeWindow);
      const issueCountLabel = costCountLabel(cost, cost.closed_issues, 'issues closed by hive', outcomeWindow);
      const countAuthorNote = cost.count_author ? ` Counted for author ${cost.count_author} in the configured repos.` : '';
      const coinLabel = est.coin_label || '';
      const coinEquiv = Number(est.coin_usd) > 0 ? ` ≈ ${fmtUSD(est.coin_usd)}` : '';
      const coinBudgetEquiv = Number(est.coin_budget_usd) > 0 ? ` ≈ ${fmtUSD(est.coin_budget_usd)}` : '';
      const usdBudgetEquiv = Number(est.usd_budget_coins) > 0 ? ` (${fmtCoins(est.usd_budget_coins, coinLabel)})` : '';
      const coinStat = coinLabel && Number(est.coins) > 0
        ? `<div class="cost-stat"><div class="cval" title="Bob backend coin usage estimated from configured tokens-per-coin conversion">${fmtCoins(est.coins, coinLabel)}${coinEquiv}</div><div class="clabel">Bob coins${est.coin_budget ? ` — ${fmtCoins(est.coins, coinLabel)} of ${fmtCoins(est.coin_budget, coinLabel)}${coinBudgetEquiv}` : ''}${est.usd_budget ? ` · USD cap ${fmtUSD(est.usd_budget)}${usdBudgetEquiv}` : ''}</div></div>`
        : '';
      const headlineTooltip = costTotalTooltip(allTimeCost, cost);
      const currentLine = allTimeCost.currentUSD != null && allTimeCost.currentUSD !== allTimeCost.usd
        ? `<div class="clabel" title="${esc(headlineTooltip)}">${esc(costLiveSummaryLabel(allTimeCost))}</div>`
        : '';

      const costBody = `
          <div class="cost-subtitle">Estimated from token counts × list prices (as of ${esc(cost.price_table_date || '')}); models without an exact price use a coarse tier estimate (~). Where a metered gateway reports actual spend it refines the estimate — but this is not a bill: subscription plans (Claude, Copilot), self-hosted inference (vLLM) and negotiated rates differ from list price.</div>
          ${recordingNotice}
          <div class="cost-grid sec-headline" data-collapsed-keep>
            <div class="cost-stat"><div class="cval" title="${esc(headlineTooltip)}">${fmtUSD(allTimeCost.usd)}<span class="cost-badge est" title="${esc(headlineTooltip)}">est.</span><span class="cost-help" role="img" aria-label="What this estimate means" title="${esc(headlineTooltip)}">?</span></div><div class="clabel" title="${esc(headlineTooltip)}">${esc(costTotalLabel(allTimeCost))}</div>${currentLine}${costSparkSvg()}</div>
            ${coinStat}
            <div class="cost-stat"><div class="cval" title="${esc(issueUnit.title + countAuthorNote)}">${issueUnit.value}</div><div class="clabel">cost / closed issue — ${esc(issueCountLabel)}</div></div>
            <div class="cost-stat"><div class="cval" title="${esc(prUnit.title + countAuthorNote)}">${prUnit.value}</div><div class="clabel">cost / merged PR — ${esc(prCountLabel)}</div></div>
          </div>
          <div class="cost-disclaimer cost-sub-disclaimer">cost / closed issue and cost / merged PR each divide the same headline total — two views of one figure, not two costs to add.</div>
          ${costTrendHtml()}
          ${gwCards ? `<div class="cost-gateways">${gwCards}</div>` : ''}
          <div class="cost-subtable-title" title="Live token summary only — the sessions currently on disk, priced at list price. Spend recorded before a reset of that summary is in the headline but not in these rows.">usage by model <span class="cost-window-tag">(live token summary · ${esc(usageWindow)})</span></div>
          <div class="cost-table-scroll"><table class="cost-table table-compact">
            <thead><tr>${costSortTh('model', 'name', 'model')}${costSortTh('model', 'running', 'running agents', { align: 'center', title: 'Running agents on this model right now — hover the count for names' })}${costSortTh('model', 'input', 'input', { align: 'right' })}${costSortTh('model', 'output', 'output', { align: 'right' })}${costSortTh('model', 'usd', 'est. $', { align: 'right' })}</tr></thead>
            <tbody>${modelRows || '<tr><td class="text-muted" colspan="5">no usage yet</td></tr>'}</tbody>
          </table></div>
          ${sandboxTable}
          ${repoTable}
          ${launchTable}
          ${sessionTable}
          ${unpricedNote}
          ${costTokenBudgetHtml()}`;
      // Patched in place with the collapsed state already in the markup: this
      // runs on every SSE agent-status tick, and an innerHTML rebuild that
      // inserted the sub-tables open and collapsed them after a forced layout
      // moved the page by their height every tick (#12512).
      morphInnerHtml(el, sectionCardShell({ id: 'cost-panel', title: '💵 Cost', gripLabel: 'Cost', panelClass: 'cost-panel', bodyClass: 'cost-body', headerClass: 'cost-title cost-toggle', chevronClass: 'cost-chevron', action: 'toggleCostPanel', keyAction: 'costHeaderKey', badgeHtml: '<span class="cost-badge est" title="Estimated from token counts × list prices; subscription/self-hosted billing may differ">est.</span>', summaryClass: 'cost-collapsed-total', summaryHtml: fmtUSD(allTimeCost.usd), summaryTitle: headlineTooltip, bodyId: 'cost-panel-body', collapsed: isCostCollapsed() }, costBody));
      // Re-apply the persisted collapsed state so a poll re-render never resets
      // it. Sub-tables first: applyCostCollapse replays sparklines, which
      // forces a layout, and that layout must never see them open (#12512).
      applyCostSubCollapse();
      applyCostCollapse();
    }

    // ── Per-repo estimated cost (#4836 phase 3) ──
    // Held outside renderCost so a transient /api/repo-cost failure leaves the
    // last good table in place rather than blanking the section.
    let lastRepoCost = null;
    let lastLaunchCost = null;

    // COST_LAUNCH_MAX_ROWS bounds the by-work-item table; the API carries the
    // full list for anyone who wants it.
    const COST_LAUNCH_MAX_ROWS = 15;

    function costLaunchRowHtml(r) {
      const usd = (r.launch_usd === null || r.launch_usd === undefined)
        ? `<span class="cagents-zero" title="Launched, but no usage could be attributed to this work item yet. Not $0.00 — unobserved.">—</span>`
        : (r.source === 'estimated_tier' ? `<span title="tier estimate">~${fmtUSD(r.launch_usd)}</span>` : fmtUSD(r.launch_usd));
      const share = (r.share_usd === null || r.share_usd === undefined) ? '—' : fmtUSD(r.share_usd);
      const heads = (r.head_shas || []).length
        ? `<span class="cagents-badge" title="${esc((r.head_shas || []).map(h => h.slice(0, 7)).join(', '))}">${(r.head_shas || []).length}</span>`
        : `<span class="cagents-zero">—</span>`;
      const kind = r.kind === 'pr' ? 'PR' : (r.kind === 'issue' ? 'issue' : 'work');
      return `<tr>
        <td class="cmodel" title="${esc((r.agents || []).join(', '))}">${esc(r.ref || '?')} <span class="text-muted">${kind}</span></td>
        <td class="cnum ccenter" title="launches that named this work item (repeat spend)">${Number(r.launches) || 0}</td>
        <td class="cnum ccenter">${heads}</td>
        <td class="cnum">${fmtTokens(r.launch_tokens)}</td>
        <td class="cnum" title="even split of each launch's spend across the refs it named: ${esc(share)}">${usd}</td>
      </tr>`;
    }

    function costLaunchTableHtml(lc) {
      if (!lc || !lc.ready) return '';
      const refs = (lc.by_ref || []).slice(0, COST_LAUNCH_MAX_ROWS);
      const rows = refs.map(costLaunchRowHtml).join('');
      const total = Number(lc.total_tokens) || 0;
      const rate = total > 0 ? Math.round((Number(lc.attributed_tokens) || 0) / total * 100) : null;
      const refTotal = lc.by_ref_total ?? (lc.by_ref || []).length;
      const more = refTotal > refs.length ? ` · showing top ${refs.length} of ${refTotal}` : '';
      return `<div class="cost-subsection" data-cost-sub="launch">
        <div class="cost-subtable-title cost-sub-toggle" role="button" tabindex="0" aria-expanded="${costSubAriaExpanded('launch')}" aria-controls="cost-sub-launch-body" data-action="costSubToggle" data-keydown-action="costSubToggle" data-arg-types="e,s" data-arg1="launch" data-keys="*" title="Estimated cost per issue / PR, from the work each kick was launched for joined against per-message token timing. Inferred, not measured. Click to collapse / expand."><span class="cost-chevron${costSubCollapsedCls('launch')}" aria-hidden="true">▼</span>cost by work item <span class="cost-badge est">inferred</span></div>
        <div class="cost-sub-body${costSubCollapsedCls('launch')}" id="cost-sub-launch-body">
        <div class="cost-subtitle">Tokens are owned by the most recent recorded kick for the same agent (up to ${Number(lc.ownership_hours) || '?'}h). ${rate === null ? '' : `<strong>${rate}%</strong> of tokens attributable`} · ${lc.window_hours ? Math.round(lc.window_hours / 24) + 'd window' : 'window unknown'} · ${Number(lc.launches_retained) || 0} launches${more}.</div>
        <div class="cost-disclaimer cost-sub-disclaimer">A governor kick names many issues at once: "est. $" is the whole spend of every launch that named the item (an upper bound); hover for the even-split share. Only governor and review-swarm kicks are recorded. Not a bill, not a budget.</div>
        <table class="cost-table table-compact">
          <thead><tr><th>work item</th><th class="ccenter">launches</th><th class="ccenter">heads</th><th class="cright">tokens</th><th class="cright">est. $</th></tr></thead>
          <tbody>${rows || '<tr><td class="text-muted" colspan="5">no launches recorded yet</td></tr>'}</tbody>
        </table>
        </div>
      </div>`;
    }

    async function fetchLaunchCost() {
      try {
        const data = await fetchDashboardHistory('/api/launch-cost');
        if (data !== null) lastLaunchCost = data;
      } catch (e) { /* transient — keep the last good attribution */ }
    }

    // Render a single row of the by-repo table. usd is a POINTER server-side:
    // null means "no attributable cost", which is NOT $0.00 — a repo can be
    // busy and still show — when its spend could not be placed in time. Render
    // the two states differently or the operator will read absence as free.
    function costRepoRowHtml(r, opts) {
      const o = opts || {};
      const usd = (r.usd === null || r.usd === undefined)
        ? `<span class="cagents-zero" title="No cost could be attributed to this row. This is NOT $0.00 — it means unobserved, not free.">—</span>`
        : (r.source === 'estimated_tier'
            ? `<span title="tier estimate — at least one contributing model has no exact list price">~${fmtUSD(r.usd)}</span>`
            : fmtUSD(r.usd));
      const label = o.label
        ? `<span class="text-muted" title="${esc(o.title || '')}">${esc(o.label)}</span>`
        : esc(r.repo || 'unknown');
      const agents = (r.agents || []).length
        ? `<span class="cagents-badge" title="${esc((r.agents || []).join(', '))}">${(r.agents || []).length}</span>`
        : `<span class="cagents-zero">—</span>`;
      return `<tr${o.muted ? ' style="opacity:.85"' : ''}>
        <td class="cmodel">${label}</td>
        <td class="cnum" style="text-align:center">${agents}</td>
        <td class="cnum">${fmtTokens(r.tokens)}</td>
        <td class="cnum">${usd}</td>
      </tr>`;
    }

    function costRepoTableHtml(rc) {
      if (!rc || !rc.ready) return '';
      const repos = rc.by_repo || [];
      const rows = repos.map(r => costRepoRowHtml(r)).join('');
      // The two mandatory buckets ALWAYS render, even at zero, so the table can
      // never be mistaken for a complete picture of where the money went.
      const unattr = costRepoRowHtml(rc.unattributed || { repo: 'unattributed', usd: null }, {
        muted: true, label: 'unattributed',
        title: "Tokens spent before an agent's first repo event, after its last, or by agents with no repo events at all. Never spread across repos."
      });
      const unsup = costRepoRowHtml(rc.backend_unsupported || { repo: 'backend_unsupported', usd: null }, {
        muted: true, label: 'backend unsupported',
        title: 'Copilot and bob record no per-message timing, so their spend cannot be placed against a repo. This spend is REAL — it is not missing, it is not attributable.'
      });
      const total = Number(rc.total_tokens) || 0;
      const rate = total > 0 ? Math.round((Number(rc.attributed_tokens) || 0) / total * 100) : null;
      const windowTxt = rc.window_hours
        ? `${Math.round(rc.window_hours / 24)}d window`
        : 'window unknown';
      const oldest = rc.oldest_event_at
        ? ` · oldest audited event ${esc(String(rc.oldest_event_at).slice(0, 10))}`
        : '';
      return `<div class="cost-subsection" data-cost-sub="repo">
        <div class="cost-subtable-title cost-sub-toggle" role="button" tabindex="0" aria-expanded="${costSubAriaExpanded('repo')}" aria-controls="cost-sub-repo-body" data-action="costSubToggle" data-keydown-action="costSubToggle" data-arg-types="e,s" data-arg1="repo" data-keys="*" title="Estimated cost attributed to each repo by joining audited repo events against per-message token timing. Inferred, not measured. Click to collapse / expand."><span class="cost-chevron${costSubCollapsedCls('repo')}" aria-hidden="true">▼</span>cost by repo <span class="cost-badge est">inferred</span></div>
        <div class="cost-sub-body${costSubCollapsedCls('repo')}" id="cost-sub-repo-body">
        <div class="cost-subtitle">Claude sessions only. Cost is attributed by TIMING: tokens spent between two audited repo events are billed to the repo named by the later event. ${rate === null ? '' : `<strong>${rate}%</strong> of tokens attributable`} · ${windowTxt}${oldest}.</div>
        <div class="cost-disclaimer" style="margin:var(--sp-2) var(--sp-0) var(--sp-4)">Known bias — an agent that investigates repo A, finds nothing to file, then files on repo B bills <em>all</em> of A's investigation to B. That is this method's failure mode, not a rounding error: treat a repo's figure as an upper bound when a neighbouring repo shows activity but little cost. A row showing "—" means unobserved, not free. Not a bill.</div>
        <table class="cost-table table-compact">
          <thead><tr><th>repo</th><th style="text-align:center">sandboxes</th><th style="text-align:right">tokens</th><th style="text-align:right">est. $</th></tr></thead>
          <tbody>${rows || '<tr><td class="text-muted" colspan="4">no attributable repo cost yet</td></tr>'}${unattr}${unsup}</tbody>
        </table>
        </div>
      </div>`;
    }

    async function fetchRepoCost() {
      try {
        const r = await fetch('/api/repo-cost');
        if (!r.ok) return;
        lastRepoCost = await r.json();
      } catch (e) { /* transient — keep the last good attribution */ }
    }

    async function fetchCost() {
      try {
        const [resp] = await Promise.all([fetch('/api/cost'), fetchCostHistory(), fetchRepoCost(), fetchAdvisorSpend(), fetchLaunchCost()]);
        if (!resp.ok) return;
        const cost = await resp.json();
        renderCost(cost);
      } catch (e) { /* transient — leave last render in place */ }
    }
    fetchCost();
    dashboardSetInterval('cost', fetchCost, COST_POLL_MS);

