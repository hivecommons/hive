    // Plan usage gauge. subscriptionGaugeModel is the single pure decision
    // point: it turns a /api/providers/headroom response into one row per
    // provider the hive's agents spend (used_by), each with its dials, plus
    // the needle and the no-reading reasons that the Overview gauge and the
    // header chip render. No DOM, no fetch, so node tests can run it. The
    // top-level fields mirror the row holding the needle, so a one-provider
    // hive reads exactly as before.
    function subscriptionGaugeModel(resp, nowMs) {
      const OUT_OF_DATE_MS = 15 * 60 * 1000;
      const NAMES = { anthropic: 'Claude', github: 'Copilot', openai: 'Codex', google: 'Gemini', 'aws-kiro': 'Kiro', deepseek: 'DeepSeek' };
      const ORDER = ['anthropic', 'github', 'openai', 'google', 'aws-kiro', 'deepseek'];
      const reasonFor = (cause, name) => ({
        not_installed: name === 'Claude' ? 'Claude Code is not installed on this hive.' : name + ' is not installed on this hive.',
        no_credentials: 'This hive is not signed in to ' + name + '.',
        rate_limited: name + ' rate-limited the usage check; hive will try again.',
        timeout: 'The usage check timed out; hive will try again.',
        unrecognized_schema: name + ' changed its usage format, so hive could not read it.',
        probe_failed: 'The usage check failed; hive will try again.',
      })[cause];
      const NOT_USED = ['not_installed', 'no_credentials'];
      const validTime = (v) => {
        const ms = Date.parse(v || '');
        // Go's zero time.Time serializes as 0001-01-01: treat it as unknown.
        return Number.isFinite(ms) && ms > 0 ? ms : null;
      };
      const statusFor = (pct) => (pct <= 10 ? 'error' : pct <= 25 ? 'warn' : 'ok');
      const describe = (kind, id, mins, scope) => {
        const k = String(kind || id || '').toLowerCase();
        const i = String(id || '').toLowerCase();
        const sc = scope || {};
        // Copilot's monthly quotas, named as Copilot names them.
        if (i === 'premium_interactions') return { label: 'Premium requests', shortName: 'premium' };
        if (i === 'chat') return { label: 'Chat', shortName: 'chat' };
        if (i === 'completions') return { label: 'Code completions', shortName: 'completions' };
        // A weekly limit that counts only some models: the builtin source
        // sends kind weekly_scoped; ccleft sends kind weekly with id
        // seven_day_<model> and scope.scope set (binding "false").
        const scoped = k === 'weekly_scoped' || ((k === 'weekly' || k === 'seven_day') &&
          (!!sc.scope || /^(seven_day|weekly)_(?!all$)./.test(i)));
        if (scoped) {
          let model = String(sc.model || sc.model_family || sc.scope || sc.name || '');
          if (/^scoped$/i.test(model)) model = '';
          // ccleft lower-cases the name ("fable"); /usage writes "Fable". A
          // model id ("claude-fable") is shown as sent.
          if (/^[a-z]+$/.test(model)) model = model.charAt(0).toUpperCase() + model.slice(1);
          return { label: model ? 'Weekly (' + model + ')' : 'Weekly (model-specific)', shortName: 'model' };
        }
        if (k === 'weekly' || k === 'weekly_all' || k === 'seven_day') return { label: 'Weekly', shortName: '' };
        if (k === 'session' || k === 'five_hour') return { label: '5-hour', shortName: '5-hour' };
        if (k === 'daily') return { label: 'Daily', shortName: 'daily' };
        if (k === 'monthly') return { label: 'Monthly', shortName: 'monthly' };
        if (mins > 0 && mins % 1440 === 0) return { label: (mins / 1440) + '-day', shortName: (mins / 1440) + '-day' };
        if (mins > 0) return { label: Math.round(mins / 60) + '-hour', shortName: Math.round(mins / 60) + '-hour' };
        return { label: 'Limit', shortName: 'limit' };
      };
      // Paid credit pools and prepaid balances are not plan limits.
      const NOT_LIMITS = ['credits', 'balance'];
      // Claude's extra usage (usage credits past the plan limits) as one
      // line: on/off and spent of the monthly cap. null hides the line: the
      // provider did not say, which is never shown as 0 or off. Display
      // only; the dashboard never turns credits on or spends them.
      const creditsFor = (x) => {
        if (!x || typeof x !== 'object' || Array.isArray(x)) return null;
        const enabled = typeof x.enabled === 'boolean' ? x.enabled : null;
        const reached = x.spend_limit_reached === true;
        const exp = Number.isInteger(x.exponent) && x.exponent >= 0 && x.exponent <= 6 ? x.exponent : 2;
        const amount = (v) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v / Math.pow(10, exp) : null);
        const used = amount(x.used_minor);
        const cap = amount(x.limit_minor);
        const currency = String(x.currency || '').toUpperCase();
        const money = (n) => (currency === 'USD' || currency === '' ? '$' + n.toFixed(exp) : n.toFixed(exp) + ' ' + currency);
        const parts = [];
        if (reached) parts.push('limit reached');
        else if (enabled !== null) parts.push(enabled ? 'on' : 'off');
        if (enabled !== false) {
          if (used !== null && cap !== null) parts.push(money(used) + ' of ' + money(cap) + ' used this month');
          else if (cap !== null) parts.push(money(cap) + ' monthly cap');
          else if (used !== null) parts.push(money(used) + ' used this month');
        }
        if (!parts.length) return null;
        return { text: 'Usage credits: ' + parts.join(' · '), status: reached ? 'warn' : 'ok', enabled: enabled, limitReached: reached, used: used, cap: cap, currency: currency };
      };
      // listed: the provider is in used_by, so agents spend it and a missing
      // or failed reading is worth saying. Unlisted (a server without
      // used_by) keeps the Claude-only rule: hidden when Claude is not used.
      const providerRow = (provider, reading, usedBy, listed) => {
        const name = NAMES[provider] || provider;
        const base = { provider: provider, name: name, usedBy: usedBy || [] };
        if (!reading) {
          return Object.assign(base, {
            state: 'no-reading', shown: listed, cause: 'absent',
            reason: listed ? 'This hive does not measure ' + name + ' usage.' : 'Usage measurement is not running on this hive.',
          });
        }
        const cause = String(reading.probe_error_cause || '');
        const limits = Array.isArray(reading.limits) ? reading.limits : [];
        const dials = [];
        limits.forEach((l, i) => {
          if (!l || typeof l.pct_remaining !== 'number' || !Number.isFinite(l.pct_remaining)) return;
          if (NOT_LIMITS.includes(String(l.kind || '').toLowerCase())) return;
          const mins = Number(l.duration_mins) || 0;
          const d = describe(l.kind, l.id, mins, l.scope);
          const pctLeft = Math.max(0, Math.min(100, Math.round(l.pct_remaining)));
          const reset = validTime(l.resets_at);
          dials.push({
            id: String(l.id || l.kind || ''),
            kind: String(l.kind || ''),
            label: d.label,
            shortName: d.shortName,
            pctLeft: pctLeft,
            resetsAt: reset === null ? '' : new Date(reset).toISOString(),
            durationMins: mins,
            status: statusFor(pctLeft),
            order: i,
          });
        });
        if (!dials.length) {
          return Object.assign(base, {
            state: 'no-reading',
            shown: listed || !NOT_USED.includes(cause),
            cause: cause || 'empty',
            reason: reasonFor(cause, name) || 'No usage reading yet.',
          });
        }
        // Longest window first; among equal lengths the all-models weekly
        // limit leads, then the provider's own order.
        const rank = (dial) => (dial.shortName === '' ? 0 : 1);
        dials.sort((a, b) => {
          const am = a.durationMins > 0 ? a.durationMins : -1;
          const bm = b.durationMins > 0 ? b.durationMins : -1;
          if (am !== bm) return bm - am;
          if (rank(a) !== rank(b)) return rank(a) - rank(b);
          return a.order - b.order;
        });
        // The needle follows the window with the least left; ties keep the
        // earlier (longer) window.
        let needle = dials[0];
        dials.forEach((dial) => { if (dial.pctLeft < needle.pctLeft) needle = dial; });
        const captured = validTime(reading.captured_at);
        const ageMs = captured === null || !Number.isFinite(nowMs) ? null : Math.max(0, nowMs - captured);
        return Object.assign(base, {
          state: 'reading',
          shown: true,
          cause: cause,
          dials: dials.map((dial) => ({
            id: dial.id, kind: dial.kind, label: dial.label, shortName: dial.shortName,
            pctLeft: dial.pctLeft, resetsAt: dial.resetsAt, durationMins: dial.durationMins, status: dial.status,
          })),
          needle: { id: needle.id, kind: needle.kind, label: needle.label, shortName: needle.shortName, pctLeft: needle.pctLeft, status: needle.status },
          worstStatus: needle.status,
          capturedAt: captured === null ? '' : new Date(captured).toISOString(),
          ageMs: ageMs,
          outOfDate: !!reading.stale || (ageMs !== null && ageMs > OUT_OF_DATE_MS),
          planName: String(reading.plan_type || ''),
          credits: creditsFor(reading.extra_usage),
          accountNote: 'Reflects the ' + name + ' account this hive is signed in with.',
        });
      };
      const providers = resp && Array.isArray(resp.providers) ? resp.providers : [];
      const readingFor = (id) => providers.find((p) => p && p.provider === id) || null;
      const usedBy = resp && resp.used_by && typeof resp.used_by === 'object' && !Array.isArray(resp.used_by) ? resp.used_by : null;
      const rank = (id) => (ORDER.includes(id) ? ORDER.indexOf(id) : ORDER.length);
      const ids = usedBy
        ? Object.keys(usedBy).filter((id) => Array.isArray(usedBy[id]) && usedBy[id].length > 0)
          .sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
        : ['anthropic'];
      // Gateways (OpenRouter, LiteLLM, vLLM, …) have no plan to measure, so
      // they get a spend row in dollars at the gateway's prices instead of a
      // dial. Models with no published or configured price are listed as
      // "price unknown", never counted as $0.
      const spend = (resp && Array.isArray(resp.gateway_spend) ? resp.gateway_spend : [])
        .filter((g) => g && g.gateway && Array.isArray(g.agents) && g.agents.length)
        .map((g) => {
          const unknown = Array.isArray(g.unknown_price_models) ? g.unknown_price_models.map(String) : [];
          const usd = Number(g.usd) || 0;
          return {
            gateway: String(g.gateway), kind: String(g.kind || ''), usedBy: g.agents.map(String),
            usd: usd, tokens: Number(g.tokens) || 0, unknownModels: unknown,
            state: unknown.length && usd === 0 ? 'unknown' : (unknown.length ? 'partial' : 'priced'),
          };
        });
      const rows = ids.map((id) => providerRow(id, readingFor(id), usedBy ? usedBy[id].map(String) : null, !!usedBy))
        .filter((row) => row.shown);
      if (!rows.length) {
        const legacy = !usedBy ? providerRow('anthropic', readingFor('anthropic'), null, false) : null;
        return legacy && legacy.state === 'no-reading' && legacy.cause !== 'absent'
          ? Object.assign(legacy, { rows: [], spend: spend })
          : { state: 'no-reading', shown: false, rows: [], spend: spend, cause: 'absent', reason: 'Usage measurement is not running on this hive.' };
      }
      // The needle row is the one with the least left; rows without a
      // reading only lead when no row has one.
      let lead = rows.find((row) => row.state === 'reading') || rows[0];
      rows.forEach((row) => { if (row.state === 'reading' && row.needle.pctLeft < lead.needle.pctLeft) lead = row; });
      return Object.assign({}, lead, { shown: true, rows: rows, spend: spend });
    }

    // subscriptionGaugeSpendText is a gateway spend row's one-line summary:
    // "$1.23 at gateway prices", "$1.23 + price unknown for qwen", or
    // "price unknown (qwen)" when nothing could be costed.
    function subscriptionGaugeSpendText(row) {
      const usd = '$' + (Number(row.usd) || 0).toFixed(2);
      const unknown = (row.unknownModels || []).join(', ');
      if (row.state === 'unknown') return 'Price unknown (' + unknown + '): this gateway publishes no price. Set prices: on the gateway to cost it.';
      if (row.state === 'partial') return usd + ' at gateway prices, plus price unknown for ' + unknown + ' (not counted).';
      return usd + ' at gateway prices (estimate from token counts).';
    }

    // Shared loader. The headroom endpoint is owner-only and the fetch
    // wrapper drops a stored token on any 401/403 from /api/, so nothing is
    // fetched until the viewer is known to be the owner. The endpoint serves
    // stored readings: polling it never triggers a measurement.
    let _subscriptionGaugeData = null;
    const SUBSCRIPTION_GAUGE_POLL_MS = 60000;
    function subscriptionGaugeOwner() {
      return typeof dashboardRoleAtLeast === 'function' && dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
    }
    function subscriptionGaugeOverviewEnabled() {
      try { return typeof overviewChartsState !== 'function' || overviewChartsState().showUsageGauge !== false; } catch (e) { return true; }
    }
    function subscriptionGaugeHeaderEnabled() {
      return typeof dashboardFeatureEnabled === 'function' && dashboardFeatureEnabled('subscription_gauge_header');
    }
    function subscriptionGaugeWanted() {
      return subscriptionGaugeOwner() && (subscriptionGaugeOverviewEnabled() || subscriptionGaugeHeaderEnabled());
    }
    function subscriptionGaugeCurrentModel() {
      return _subscriptionGaugeData ? subscriptionGaugeModel(_subscriptionGaugeData, Date.now()) : null;
    }
    function subscriptionGaugeRefresh() {
      if (!subscriptionGaugeWanted()) {
        dashboardClearInterval('subscription-gauge');
      } else if (!DASHBOARD_INTERVALS['subscription-gauge']) {
        dashboardSetInterval('subscription-gauge', subscriptionGaugeFetch, SUBSCRIPTION_GAUGE_POLL_MS);
        subscriptionGaugeFetch();
      }
      renderSubscriptionGaugeSurfaces();
    }
    async function subscriptionGaugeFetch() {
      if (!subscriptionGaugeWanted()) return;
      try {
        const res = await fetch('/api/providers/headroom');
        if (!res.ok) return;
        _subscriptionGaugeData = await res.json();
      } catch (e) {
        return;
      }
      renderSubscriptionGaugeSurfaces();
    }
    function renderSubscriptionGaugeSurfaces() {
      renderSubscriptionGaugeOverview();
      if (typeof renderSubscriptionGaugeChip === 'function') renderSubscriptionGaugeChip();
    }

    // An E-to-F fuel dial drawn with DOM calls: numbers only, colours from
    // CSS classes keyed on data-status, no inline styles.
    function subscriptionGaugeDialSvg(pctLeft, opts) {
      const o = opts || {};
      const NS = 'http://www.w3.org/2000/svg';
      const cx = 50, cy = 54, r = 42;
      const v = Math.max(0, Math.min(100, Number(pctLeft) || 0));
      const pt = (f, rr) => {
        const a = Math.PI * (1 - f);
        return (cx + rr * Math.cos(a)).toFixed(2) + ' ' + (cy - rr * Math.sin(a)).toFixed(2);
      };
      const arc = (a, b) => 'M ' + pt(a, r) + ' A ' + r + ' ' + r + ' 0 0 1 ' + pt(b, r);
      const node = (tag, attrs) => {
        const el = document.createElementNS(NS, tag);
        Object.keys(attrs).forEach((k) => el.setAttribute(k, String(attrs[k])));
        return el;
      };
      const svg = node('svg', {
        viewBox: '0 0 100 62',
        class: 'sg-dial' + (o.compact ? ' sg-dial--compact' : ''),
        'data-status': o.status || (v <= 10 ? 'error' : v <= 25 ? 'warn' : 'ok'),
        'data-stale': o.stale ? 'true' : 'false',
        role: 'img',
        'aria-label': o.label || (v + '% left'),
      });
      svg.appendChild(node('path', { class: 'sg-dial__track', d: arc(0, 1) }));
      svg.appendChild(node('path', { class: 'sg-dial__zone sg-dial__zone--red', d: arc(0, 0.10) }));
      svg.appendChild(node('path', { class: 'sg-dial__zone sg-dial__zone--amber', d: arc(0.10, 0.25) }));
      if (v > 0.5) svg.appendChild(node('path', { class: 'sg-dial__fill', d: arc(0, v / 100) }));
      if (!o.compact) {
        let ticks = '';
        [0, 0.25, 0.5, 0.75, 1].forEach((f) => { ticks += 'M ' + pt(f, r - 9) + ' L ' + pt(f, r - 14) + ' '; });
        svg.appendChild(node('path', { class: 'sg-dial__tick', d: ticks.trim() }));
        const e = node('text', { class: 'sg-dial__letter sg-dial__letter--e', x: 6, y: 60, 'font-size': 11 });
        e.textContent = 'E';
        const f = node('text', { class: 'sg-dial__letter', x: 87, y: 60, 'font-size': 11 });
        f.textContent = 'F';
        svg.appendChild(e);
        svg.appendChild(f);
      }
      const tip = pt(v / 100, r - 5).split(' ');
      svg.appendChild(node('line', { class: 'sg-dial__needle', x1: cx, y1: cy, x2: tip[0], y2: tip[1] }));
      svg.appendChild(node('circle', { class: 'sg-dial__hub', cx: cx, cy: cy, r: o.compact ? 7 : 4.5 }));
      return svg;
    }

    // "scanner, reviewer and 3 more": the agents that spend a provider.
    function subscriptionGaugeUsers(names) {
      const list = Array.isArray(names) ? names : [];
      const SHOWN = 6;
      if (list.length <= SHOWN) return list.join(', ');
      return list.slice(0, SHOWN).join(', ') + ' and ' + (list.length - SHOWN) + ' more';
    }

    // Overview block: on by default for the owner of a hive whose agents
    // spend a measured plan; hidden per browser from the Overview settings.
    // One row per provider; a one-provider hive gets the single block it
    // always had.
    function renderSubscriptionGaugeOverview() {
      const host = document.getElementById('overview-usage-gauge');
      if (!host) return;
      const m = subscriptionGaugeCurrentModel();
      const spend = (m && m.spend) || [];
      if (!subscriptionGaugeOwner() || !subscriptionGaugeOverviewEnabled() || !m || (!m.shown && !spend.length)) {
        host.hidden = true;
        host.replaceChildren();
        return;
      }
      const el = (tag, cls, text) => {
        const n = document.createElement(tag);
        if (cls) n.className = cls;
        if (text !== undefined) n.textContent = text;
        return n;
      };
      const rows = !m.shown ? [] : (m.rows && m.rows.length ? m.rows : [m]);
      const single = rows.length === 1 && !spend.length;
      const metaFor = (row) => {
        const meta = [];
        if (row.state === 'reading') {
          if (row.capturedAt) meta.push('Measured ' + relativeTimeShort(row.capturedAt));
          if (row.planName) meta.push('Plan: ' + row.planName);
        }
        return meta.join(' · ');
      };
      const head = el('div', 'overview-usage-gauge__head');
      head.appendChild(el('div', 'overview-usage-gauge__title', single ? rows[0].name + ' usage left' : (rows.length ? 'Usage left' : 'Gateway spend')));
      head.appendChild(el('div', 'overview-usage-gauge__meta', single ? metaFor(rows[0]) : ''));
      const parts = [head];
      rows.forEach((row) => {
        const box = el('div', 'overview-usage-gauge__row');
        box.dataset.provider = row.provider;
        if (!single) {
          const rowHead = el('div', 'overview-usage-gauge__head');
          rowHead.appendChild(el('div', 'overview-usage-gauge__provider', row.name));
          rowHead.appendChild(el('div', 'overview-usage-gauge__meta', metaFor(row)));
          box.appendChild(rowHead);
        }
        if (row.usedBy && row.usedBy.length) box.appendChild(el('div', 'overview-usage-gauge__users', 'Used by ' + subscriptionGaugeUsers(row.usedBy)));
        if (row.state !== 'reading') {
          box.appendChild(el('div', 'overview-usage-gauge__note', 'No reading: ' + row.reason));
        } else {
          if (row.outOfDate) box.appendChild(el('div', 'overview-usage-gauge__stale', 'This reading is out of date; treat it as a rough guide until hive measures again.'));
          const grid = el('div', 'overview-usage-gauge__dials');
          row.dials.forEach((d) => {
            const cell = el('div', 'overview-usage-gauge__dial');
            cell.appendChild(subscriptionGaugeDialSvg(d.pctLeft, { status: d.status, stale: row.outOfDate, label: row.name + ' ' + d.label + ': ' + d.pctLeft + '% left' }));
            const pct = el('div', 'overview-usage-gauge__pct', d.pctLeft + '% left');
            pct.setAttribute('data-status', row.outOfDate ? 'neutral' : d.status);
            cell.appendChild(pct);
            cell.appendChild(el('div', 'overview-usage-gauge__label', d.label));
            if (d.resetsAt) cell.appendChild(el('div', 'overview-usage-gauge__reset', 'Resets ' + formatDashboardDateTime(d.resetsAt)));
            grid.appendChild(cell);
          });
          box.appendChild(grid);
          if (row.credits) {
            const credits = el('div', 'overview-usage-gauge__credits', row.credits.text);
            credits.setAttribute('data-status', row.credits.status);
            box.appendChild(credits);
          }
        }
        parts.push(box);
      });
      spend.forEach((row) => {
        const box = el('div', 'overview-usage-gauge__row');
        box.dataset.gateway = row.gateway;
        const rowHead = el('div', 'overview-usage-gauge__head');
        rowHead.appendChild(el('div', 'overview-usage-gauge__provider', row.gateway + ' (gateway)'));
        box.appendChild(rowHead);
        if (row.usedBy.length) box.appendChild(el('div', 'overview-usage-gauge__users', 'Used by ' + subscriptionGaugeUsers(row.usedBy)));
        const line = el('div', row.state === 'priced' ? 'overview-usage-gauge__spend' : 'overview-usage-gauge__note', subscriptionGaugeSpendText(row));
        line.dataset.priceState = row.state;
        box.appendChild(line);
        parts.push(box);
      });
      if (!rows.length) {
        parts.push(el('div', 'overview-usage-gauge__note', 'Gateway spend is estimated from token counts at each gateway\'s published or configured prices.'));
        host.replaceChildren.apply(host, parts);
        host.hidden = false;
        return;
      }
      parts.push(el('div', 'overview-usage-gauge__note', single
        ? (rows[0].accountNote || 'Reflects the ' + rows[0].name + ' account this hive is signed in with.') + ' This is the ' + rows[0].name + ' plan, not the hive token budget.'
        : 'Each row reflects the account this hive is signed in with for that provider. These are the providers\' plans, not the hive token budget.'));
      host.replaceChildren.apply(host, parts);
      host.hidden = false;
    }

    // Opt-in header chip: a compact dial whose needle shows the limit closest
    // to empty across every provider row. It shares the Overview gauge's
    // readings; no separate fetch.
    function renderSubscriptionGaugeChip() {
      const el = document.getElementById('oc-subscription-gauge');
      if (!el) return;
      const dialHost = el.querySelector('.subscription-gauge-chip__dial');
      const labelEl = el.querySelector('.nav-chip__label');
      const m = subscriptionGaugeCurrentModel();
      if (!subscriptionGaugeOwner() || !subscriptionGaugeHeaderEnabled() || !m || (m.rows && !m.shown && m.cause === 'absent')) {
        el.hidden = true;
        if (dialHost) dialHost.replaceChildren();
        return;
      }
      const name = m.name || 'Claude';
      const rows = m.rows && m.rows.length ? m.rows : [m];
      const lines = [];
      if (m.state !== 'reading') {
        if (dialHost) dialHost.replaceChildren();
        if (labelEl) labelEl.textContent = name + ': no reading';
        el.dataset.status = 'neutral';
        el.dataset.outOfDate = 'false';
      } else {
        const n = m.needle;
        const label = name + ' ' + n.pctLeft + '%' + (n.shortName ? ' · ' + n.shortName : '');
        if (dialHost) dialHost.replaceChildren(subscriptionGaugeDialSvg(n.pctLeft, { compact: true, status: n.status, stale: m.outOfDate, label: name + ' ' + n.label + ': ' + n.pctLeft + '% left' }));
        if (labelEl) labelEl.textContent = label;
        el.dataset.status = m.outOfDate ? 'neutral' : m.worstStatus;
        el.dataset.outOfDate = m.outOfDate ? 'true' : 'false';
      }
      rows.forEach((row) => {
        const prefix = rows.length > 1 ? row.name + ' ' : '';
        if (row.state !== 'reading') {
          lines.push('No ' + row.name + ' usage reading: ' + row.reason);
          return;
        }
        row.dials.forEach((d) => {
          lines.push(prefix + d.label + ': ' + d.pctLeft + '% left' + (d.resetsAt ? ' · resets ' + formatDashboardDateTime(d.resetsAt) : ''));
        });
        if (row.capturedAt) lines.push(prefix + 'Measured ' + relativeTimeShort(row.capturedAt) + (row.outOfDate ? ' (out of date)' : ''));
      });
      lines.push('Click to open the usage gauge on the Overview.');
      el.title = lines.join('\n');
      el.setAttribute('aria-label', (m.state === 'reading' ? name + ' plan: ' + m.needle.pctLeft + '% left in the ' + m.needle.label + ' limit' : name + ' plan: no reading') + '. Open the usage gauge on the Overview.');
      el.hidden = false;
    }
    function openSubscriptionGaugeOverview() {
      if (typeof ocNavigate === 'function') ocNavigate('overview-section');
      const host = document.getElementById('overview-usage-gauge');
      if (host && !host.hidden) {
        host.scrollIntoView({ block: 'start', behavior: 'smooth' });
      } else if (typeof showToast === 'function') {
        showToast('The usage gauge is hidden on the Overview. Turn on "Show usage gauge" in Overview settings to see every limit.', 'info');
      }
    }
