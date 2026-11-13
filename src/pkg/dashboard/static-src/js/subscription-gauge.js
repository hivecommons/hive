    // Claude subscription usage gauge. subscriptionGaugeModel is the single
    // pure decision point: it turns a /api/providers/headroom response into
    // the dials, the needle and the no-reading reason that the Overview gauge
    // and the header chip render. No DOM, no fetch, so node tests can run it.
    function subscriptionGaugeModel(resp, nowMs) {
      const OUT_OF_DATE_MS = 15 * 60 * 1000;
      const REASONS = {
        not_installed: 'Claude Code is not installed on this hive.',
        no_credentials: 'This hive is not signed in to Claude.',
        rate_limited: 'Claude rate-limited the usage check; hive will try again.',
        timeout: 'The usage check timed out; hive will try again.',
        unrecognized_schema: 'Claude changed its usage format, so hive could not read it.',
        probe_failed: 'The usage check failed; hive will try again.',
      };
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
        // A weekly limit that counts only some models: the builtin source
        // sends kind weekly_scoped; ccleft sends kind weekly with id
        // seven_day_<model> and scope.scope set (binding "false").
        const scoped = k === 'weekly_scoped' || ((k === 'weekly' || k === 'seven_day') &&
          (!!sc.scope || /^(seven_day|weekly)_(?!all$)./.test(i)));
        if (scoped) {
          let model = sc.model || sc.model_family || sc.scope || sc.name || '';
          if (/^scoped$/i.test(String(model))) model = '';
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
      const providers = resp && Array.isArray(resp.providers) ? resp.providers : [];
      const reading = providers.find((p) => p && p.provider === 'anthropic');
      if (!reading) {
        return { state: 'no-reading', usesClaude: false, cause: 'absent', reason: 'Usage measurement is not running on this hive.' };
      }
      const cause = String(reading.probe_error_cause || '');
      const limits = Array.isArray(reading.limits) ? reading.limits : [];
      const dials = [];
      // Paid credit pools and prepaid balances are not plan limits.
      const NOT_LIMITS = ['credits', 'balance'];
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
        return {
          state: 'no-reading',
          usesClaude: !NOT_USED.includes(cause),
          cause: cause || 'empty',
          reason: REASONS[cause] || 'No usage reading yet.',
        };
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
      return {
        state: 'reading',
        usesClaude: true,
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
        accountNote: 'Reflects the Claude account this hive is signed in with.',
      };
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

    // Overview block: on by default for the owner of a hive that uses
    // Claude; hidden per browser from the Overview settings.
    function renderSubscriptionGaugeOverview() {
      const host = document.getElementById('overview-usage-gauge');
      if (!host) return;
      const m = subscriptionGaugeCurrentModel();
      if (!subscriptionGaugeOwner() || !subscriptionGaugeOverviewEnabled() || !m || !m.usesClaude) {
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
      const head = el('div', 'overview-usage-gauge__head');
      head.appendChild(el('div', 'overview-usage-gauge__title', 'Claude usage left'));
      const meta = [];
      if (m.state === 'reading') {
        if (m.capturedAt) meta.push('Measured ' + relativeTimeShort(m.capturedAt));
        if (m.planName) meta.push('Plan: ' + m.planName);
      }
      head.appendChild(el('div', 'overview-usage-gauge__meta', meta.join(' · ')));
      const parts = [head];
      if (m.state !== 'reading') {
        parts.push(el('div', 'overview-usage-gauge__note', 'No reading: ' + m.reason));
      } else {
        if (m.outOfDate) parts.push(el('div', 'overview-usage-gauge__stale', 'This reading is out of date; treat it as a rough guide until hive measures again.'));
        const grid = el('div', 'overview-usage-gauge__dials');
        m.dials.forEach((d) => {
          const cell = el('div', 'overview-usage-gauge__dial');
          cell.appendChild(subscriptionGaugeDialSvg(d.pctLeft, { status: d.status, stale: m.outOfDate, label: d.label + ': ' + d.pctLeft + '% left' }));
          const pct = el('div', 'overview-usage-gauge__pct', d.pctLeft + '% left');
          pct.setAttribute('data-status', m.outOfDate ? 'neutral' : d.status);
          cell.appendChild(pct);
          cell.appendChild(el('div', 'overview-usage-gauge__label', d.label));
          if (d.resetsAt) cell.appendChild(el('div', 'overview-usage-gauge__reset', 'Resets ' + formatDashboardDateTime(d.resetsAt)));
          grid.appendChild(cell);
        });
        parts.push(grid);
      }
      parts.push(el('div', 'overview-usage-gauge__note', (m.accountNote || 'Reflects the Claude account this hive is signed in with.') + ' This is the Claude plan, not the hive token budget.'));
      host.replaceChildren.apply(host, parts);
      host.hidden = false;
    }
