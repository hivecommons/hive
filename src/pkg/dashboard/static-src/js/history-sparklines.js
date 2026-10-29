    // ── Sparkline helper ──
    const SPARKLINE_STORAGE_KEY = 'hive_sparkline_history';
    const SPARKLINE_MAX_POINTS = 360;
    const DASHBOARD_HISTORY_CAP = SPARKLINE_MAX_POINTS;
    const CLIENT_HISTORY_MAX_SAMPLES = DASHBOARD_HISTORY_CAP;
    const DASHBOARD_SERVER_HISTORY_CAP = 10000;
    const DASHBOARD_LOG_CAP = 500;
    const DASHBOARD_INTERVALS = window.__dashboardIntervals = window.__dashboardIntervals || {};
    function capDashboardArray(arr, cap) {
      if (!Array.isArray(arr)) return [];
      return arr.length > cap ? arr.slice(-cap) : arr;
    }
    function pushDashboardRing(arr, item, cap) {
      arr.push(item);
      if (arr.length > cap) arr.splice(0, arr.length - cap);
      return arr;
    }
    function capHistory(arr, max) {
      return capDashboardArray(arr, max || CLIENT_HISTORY_MAX_SAMPLES);
    }
    function dashboardSetInterval(key, fn, ms) {
      dashboardClearInterval(key);
      const entry = { fn, ms, timer: null };
      DASHBOARD_INTERVALS[key] = entry;
      if (!document.hidden) dashboardStartInterval(entry);
      return entry;
    }
    function dashboardStartInterval(entry) {
      entry.timer = setInterval(entry.fn, entry.ms);
    }
    function dashboardClearInterval(key) {
      if (!DASHBOARD_INTERVALS[key]) return;
      clearInterval(DASHBOARD_INTERVALS[key].timer);
      delete DASHBOARD_INTERVALS[key];
    }
    document.addEventListener('visibilitychange', () => {
      for (const [key, entry] of Object.entries(DASHBOARD_INTERVALS)) {
        if (DASHBOARD_INTERVALS[key] !== entry) continue;
        clearInterval(entry.timer);
        entry.timer = null;
        if (document.hidden) continue;
        dashboardStartInterval(entry);
        // A refresh failure must not prevent the other panels from resuming.
        // Snapshot iteration also permits a callback to clear/replace itself.
        if (DASHBOARD_INTERVALS[key] === entry) {
          try { Promise.resolve(entry.fn()).catch(console.error); } catch (err) { console.error(err); }
        }
      }
    });
    let historyData = (() => {
      try {
        const stored = localStorage.getItem(SPARKLINE_STORAGE_KEY);
        return capHistory(stored ? JSON.parse(stored) : []);
      } catch { return []; }
    })();
    function persistSparklineHistory() {
      historyData = capHistory(historyData);
      try { localStorage.setItem(SPARKLINE_STORAGE_KEY, JSON.stringify(historyData)); } catch {}
    }
    const SPARK_W = 80, SPARK_H = 20;

    // Compact axis-value formatter for sparkline scale labels/tooltips. NOT
    // fmtTokens (which is billions-denominated and unreadable for small
    // counts like "5 issues") — this adapts to the magnitude of the series.
    function fmtSparkVal(v) {
      if (typeof v !== 'number' || !isFinite(v)) return '—';
      const a = Math.abs(v);
      if (a >= 1e9) return (v / 1e9).toFixed(1) + 'B';
      if (a >= 1e6) return (v / 1e6).toFixed(1) + 'M';
      if (a >= 1e3) return (v / 1e3).toFixed(1) + 'k';
      return Number.isInteger(v) ? String(v) : v.toFixed(2);
    }

    // Compact date+time for sparkline tooltips (#6768: "oldest → newest" says
    // nothing about the actual time span the graph covers).
    function fmtSparkTime(t) {
      const d = new Date(t);
      if (!(typeof t === 'number' && isFinite(t) && t > 0) || isNaN(d)) return '';
      return formatDashboardDateTime(d, { month:'numeric', day:'numeric', hour:'numeric', minute:'2-digit', hour12:true, timeZoneName:'short' });
    }
    // Horizontal-span label for a sparkline tooltip: the real first→last
    // timestamps when the caller has them (aligned with the value series),
    // otherwise the old direction-only hint.
    function sparkSpanLabel(times, n) {
      if (Array.isArray(times) && times.length === n) {
        const a = fmtSparkTime(times[0]), b = fmtSparkTime(times[times.length - 1]);
        if (a && b) return `${a} → ${b}`;
      }
      return 'oldest → newest';
    }

    const SPARKLINE_ANIMATION_MS = 620;
    const SPARKLINE_STAGGER_MS = 25;
    const SPARKLINE_MAX_STAGGER_MS = 325;
    // Bounded: some sparkline ids embed their latest value, so an unbounded
    // map would gain an entry per refresh for the life of the tab.
    const SPARKLINE_KEY_CACHE_MAX = 500;
    const _sparklineLastKeys = new Map();
    function rememberSparklineKey(id, key) {
      _sparklineLastKeys.delete(id);
      _sparklineLastKeys.set(id, key);
      while (_sparklineLastKeys.size > SPARKLINE_KEY_CACHE_MAX) {
        _sparklineLastKeys.delete(_sparklineLastKeys.keys().next().value);
      }
    }
    const _sparklineCleanupTimers = new WeakMap();
    const _sparklineStaggerTimers = new Set();

    function cancelSparklineWork(root, clearStaggers) {
      if (clearStaggers) {
        _sparklineStaggerTimers.forEach(timer => clearTimeout(timer));
        _sparklineStaggerTimers.clear();
      }
      const nodes = [];
      if (root && root.nodeType === 1) nodes.push(root);
      if (root && root.querySelectorAll) nodes.push(...root.querySelectorAll('*'));
      nodes.forEach(node => {
        const cleanup = _sparklineCleanupTimers.get(node);
        if (cleanup) { clearTimeout(cleanup); _sparklineCleanupTimers.delete(node); }
        if (node.getAnimations) node.getAnimations().forEach(animation => animation.cancel());
      });
    }

    function sparklineSeriesKey(series) {
      if (!Array.isArray(series)) return '';
      return series.map(v => {
        const n = Number(v);
        if (!Number.isFinite(n)) return 'x';
        return Number.isInteger(n) ? String(n) : n.toFixed(3).replace(/0+$/, '').replace(/\.$/, '');
      }).join('|');
    }
    function sparklineReducedMotion() {
      return !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
    }
    function sparklineValueSummary(values) {
      if (!values || !values.length) return 'no data';
      const first = values[0], latest = values[values.length - 1];
      const trend = latest > first ? 'up' : latest < first ? 'down' : 'flat';
      return `latest ${fmtSparkVal(latest)}, trend ${trend}`;
    }
    function renderSparkline(el, series, opts) {
      opts = opts || {};
      if (!Array.isArray(series) || series.length < 1) return '';
      const values = series.map(v => {
        const n = Number(v);
        return Number.isFinite(n) ? n : 0;
      });
      const W = opts.width || SPARK_W, H = opts.height || SPARK_H;
      const min = opts.min != null ? opts.min : Math.min(...values);
      const max = opts.max != null ? opts.max : Math.max(...values);
      const range = (max - min) || max || 1;
      const padX = opts.padX != null ? opts.padX : 0;
      const padY = opts.padY != null ? opts.padY : 1;
      const plotW = opts.plotW != null ? opts.plotW : (W - padX * 2);
      const plotH = opts.plotH != null ? opts.plotH : (H - padY * 2);
      const points = opts.points || (values.length === 1
        ? (() => {
            const y = padY + plotH - ((values[0] - min) / range) * plotH;
            return [`${padX.toFixed(1)},${y.toFixed(1)}`, `${(padX + plotW).toFixed(1)},${y.toFixed(1)}`];
          })()
        : values.map((v, i) => {
            const x = padX + (i / (values.length - 1)) * plotW;
            const y = padY + plotH - ((v - min) / range) * plotH;
            return `${x.toFixed(1)},${y.toFixed(1)}`;
          }));
      const lastPt = String(opts.lastPoint || points[points.length - 1]).split(',');
      const latest = values[values.length - 1];
      const scaleText = `range ${fmtSparkVal(min)}–${fmtSparkVal(max)} · latest ${fmtSparkVal(latest)}`;
      const detailText = opts.title || sparklineValueSummary(values);
      const title = detailText && !String(detailText).includes(scaleText) ? `${scaleText} · ${detailText}` : detailText || scaleText;
      const titleDetail = title.replace(scaleText + ' · ', '').replace(scaleText, '');
      const ariaDetail = opts.ariaLabel || `${opts.label || 'sparkline'}: ${sparklineValueSummary(values)}`;
      const aria = `${scaleText}; ${ariaDetail}`;
      const key = sparklineSeriesKey(values);
      const id = opts.id || `${opts.label || 'sparkline'}:${opts.color || ''}`;
      const lineAttrs = opts.lineAttrs || '';
      const dot = opts.dot === false ? '' : `<circle class="sparkline-dot" cx="${lastPt[0]}" cy="${lastPt[1]}" r="${opts.dotRadius || 2}" fill="${opts.dotColor || opts.color || 'currentColor'}"/>`;
      const area = opts.area ? `<polygon class="sparkline-area" points="${points[0].split(',')[0]},${H - padY} ${points.join(' ')} ${lastPt[0]},${H - padY}" fill="${opts.areaColor || opts.color || 'currentColor'}"/>` : '';
      const svgClass = opts.svgClass ? ` class="${escapeHtml(opts.svgClass)}"` : '';
      const titleAttr = opts.svgTitle ? ` title="${escapeHtml(opts.svgTitle)}"` : '';
      const svg = `<svg${svgClass} width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-label="${escapeHtml(aria)}" data-sparkline-id="${escapeHtml(id)}" data-sparkline-key="${escapeHtml(key)}"${titleAttr}>` +
        `<title>range ${fmtSparkVal(min)}–${fmtSparkVal(max)} · latest ${fmtSparkVal(latest)}${titleDetail ? ` · ${escapeHtml(titleDetail)}` : ''}</title>` +
        (opts.before || '') + area +
        `<polyline class="sparkline-line" points="${points.join(' ')}" fill="none" stroke="${opts.color || 'currentColor'}" stroke-width="${opts.strokeWidth || 1.5}" stroke-linecap="round" stroke-linejoin="round"${lineAttrs}/>` +
        dot + (opts.after || '') + `</svg>`;
      const html = opts.wrapper === false ? svg : `<span class="${['sparkline', opts.wrapperClass || ''].filter(Boolean).map(escapeHtml).join(' ')}"${opts.wrapperTitle ? ` title="${escapeHtml(opts.wrapperTitle)}"` : ''}>${svg}</span>`;
      if (el && el.nodeType === 1) {
        cancelSparklineWork(el, true);
        el.innerHTML = html;
        replaySparklinesIn(el, { force: true });
      }
      return html;
    }

    function sparklineSvgVisible(svg) {
      if (!svg || !svg.isConnected) return false;
      const inHeadline = !!(svg.closest && svg.closest('.sec-headline,[data-collapsed-keep]'));
      const shell = svg.closest('.dash-card, .section-body, [data-dashboard-section], section');
      if (shell && shell.classList && shell.classList.contains('collapsed') && !inHeadline) return false;
      const body = svg.closest('.dash-card-body, .section-body');
      if (body && body.classList && body.classList.contains('collapsed') && !(inHeadline && body.classList.contains('collapsed-with-headline'))) return false;
      if (svg.getClientRects && svg.getClientRects().length === 0) return false;
      return true;
    }
    function animateSparklinePart(element, frames, options) {
      const animation = element.animate(frames, options);
      animation.onfinish = () => {
        // Persist the final appearance before releasing the forwards-fill effect.
        const last = frames[frames.length - 1];
        for (const [property, value] of Object.entries(last)) element.style[property] = String(value);
        animation.cancel();
      };
      return animation;
    }
    function animateSparklineSvg(svg, length) {
      if (!svg || !svg.isConnected) return;
      cancelSparklineWork(svg, false);
      const line = svg && svg.querySelector('.sparkline-line');
      if (!line || sparklineReducedMotion()) return;
      const wrap = svg.closest('.sparkline') || svg;
      const area = svg.querySelector('.sparkline-area');
      const dot = svg.querySelector('.sparkline-dot');
      const len = length || (line.getTotalLength ? line.getTotalLength() : 0);
      if (!len) return;
      wrap.style.setProperty('--sparkline-glow', line.getAttribute('stroke') || 'currentColor');
      wrap.classList.add('is-redrawing', 'is-glowing');
      line.style.strokeDasharray = String(len);
      line.style.strokeDashoffset = String(len);
      animateSparklinePart(line, [{ strokeDashoffset: len }, { strokeDashoffset: 0 }], { duration: SPARKLINE_ANIMATION_MS, easing: 'cubic-bezier(.22,1,.36,1)', fill: 'forwards' });
      if (area) animateSparklinePart(area, [{ opacity: 0 }, { opacity: 0.14 }], { duration: SPARKLINE_ANIMATION_MS - 120, delay: 90, easing: 'ease-out', fill: 'forwards' });
      if (dot) animateSparklinePart(dot, [{ opacity: 0, transform: 'scale(0.35)' }, { opacity: 1, transform: 'scale(1.35)' }, { opacity: 1, transform: 'scale(1)' }], { duration: 220, delay: SPARKLINE_ANIMATION_MS - 140, easing: 'cubic-bezier(.2,1.8,.4,1)', fill: 'forwards' });
      const cleanupTimer = setTimeout(() => {
        _sparklineCleanupTimers.delete(wrap);
        if (!line.isConnected) return;
        line.style.strokeDasharray = '';
        line.style.strokeDashoffset = '';
        wrap.classList.remove('is-redrawing', 'is-glowing');
      }, SPARKLINE_ANIMATION_MS + 80);
      _sparklineCleanupTimers.set(wrap, cleanupTimer);
    }
    function replaySparklinesIn(root, opts) {
      opts = opts || {};
      if (!root || sparklineReducedMotion()) return;
      const svgs = Array.from(root.querySelectorAll ? root.querySelectorAll('svg[data-sparkline-key]') : []);
      const runnable = [];
      for (const svg of svgs) {
        if (!sparklineSvgVisible(svg)) continue;
        const id = svg.getAttribute('data-sparkline-id') || '';
        const key = svg.getAttribute('data-sparkline-key') || '';
        if (!opts.force && id && _sparklineLastKeys.get(id) === key) continue;
        rememberSparklineKey(id, key);
        const line = svg.querySelector('.sparkline-line');
        runnable.push({ svg, length: line && line.getTotalLength ? line.getTotalLength() : 0 });
      }
      runnable.forEach((item, i) => {
        const timer = setTimeout(() => {
          _sparklineStaggerTimers.delete(timer);
          if (item.svg && item.svg.isConnected) animateSparklineSvg(item.svg, item.length);
        }, Math.min(i * SPARKLINE_STAGGER_MS, SPARKLINE_MAX_STAGGER_MS));
        _sparklineStaggerTimers.add(timer);
      });
    }

    function sparkSvg(rawValues, color, times, id) {
      if (!rawValues || rawValues.length < 2) return '';
      // Fill zero gaps: carry forward last non-zero value through consecutive zeros
      const values = [...rawValues];
      let lastNonZero = 0;
      for (let i = 0; i < values.length; i++) {
        if (values[i] > 0) { lastNonZero = values[i]; }
        else if (lastNonZero > 0) { values[i] = lastNonZero; }
      }
      const max = Math.max(...values);
      const min = Math.min(...values);
      // <title> gives every tiny sparkline a hover scale (#4298: graphs with
      // no axes are impossible to read — these are too small for drawn axes).
      // renderSparkline emits that as <title>range ${fmtSparkVal(min)}…
      const title = `range ${fmtSparkVal(min)}–${fmtSparkVal(max)} · latest ${fmtSparkVal(values[values.length - 1])} · ${values.length} points, ${sparkSpanLabel(times, values.length)}`;
      return renderSparkline(null, values, { color, title, ariaLabel: title, label: id || 'sparkline', id: id || title });
    }

    function getHistorySeries(key) {
      return historyData.map(s => {
        const keys = key.split('.');
        let v = s;
        for (const k of keys) v = v?.[k];
        return v ?? 0;
      });
    }
    // Snapshot timestamps aligned index-for-index with getHistorySeries(),
    // so sparkSvg can label the real time span (#6768).
    function getHistoryTimes() {
      return historyData.map(s => s.t ?? s.timestamp);
    }

    // Default stroke colors for the named series historyData carries. Keeps the
    // sparkline() helper below a single source of truth for each series' color
    // so the scattered render fns don't each repeat the getHistorySeries +
    // sparkSvg + color triple.
    const SPARKLINE_COLORS = {
      govIssues: 'var(--blue)',
      govPrs: 'var(--purple)',
      govTotal: 'var(--green)',
      govHold: 'var(--muted)', // sparkline tint only — the count itself is neutral
      tokenTotal: 'var(--cyan)',
      tokenInput: 'var(--blue)',
      tokenOutput: 'var(--purple)',
      tokenCacheRead: 'var(--green)',
      tokenCacheCreate: 'var(--yellow)',
      tokenMessages: 'var(--muted)',
    };
    // sparkline(name, color?) renders the named historyData series as an SVG
    // sparkline. color defaults to SPARKLINE_COLORS[name] (falling back to
    // muted). One helper routes every historyData-backed sparkline through the
    // same getHistorySeries + sparkSvg path.
    function sparkline(name, color) {
      return sparkSvg(getHistorySeries(name), color || SPARKLINE_COLORS[name] || 'var(--muted)', getHistoryTimes(), `history:${name}`);
    }

    function restartSparkSvg(agentName) {
      const entries = historyData
        .map(s => ({ v: s.agents?.[agentName]?.restarts, t: s.t ?? s.timestamp }))
        .filter(e => e.v != null);
      const series = entries.map(e => e.v);
      if (series.length < 2 || Math.max(...series) === 0) return '';
      return sparkSvg(series, 'var(--red)', entries.map(e => e.t), `agent:${agentName}:restarts`);
    }

    // Conditional reads avoid reparsing unchanged histories. Keep one validator
    // per endpoint (not per since cursor), so this cache stays bounded.
    const _historyValidators = new Map();
    async function fetchDashboardHistory(url) {
      const key = url.split('?')[0];
      const previous = _historyValidators.get(key);
      const headers = previous && previous.url === url ? { 'If-None-Match': previous.etag } : {};
      const r = await fetch(url, { headers, cache: 'no-store' });
      if (r.status === 304) return null;
      if (!r.ok) throw new Error(`History request failed: ${r.status}`);
      const data = await r.json();
      const etag = r.headers.get('ETag');
      if (etag) _historyValidators.set(key, { url, etag });
      else _historyValidators.delete(key);
      return data;
    }

    // ── Fact count sparkline (30-day server-side history) ──
    let _factHistory = [];
    async function fetchFactHistory() {
      try {
        const data = await fetchDashboardHistory('/api/knowledge/fact-history');
        if (data !== null) _factHistory = capDashboardArray(data, DASHBOARD_SERVER_HISTORY_CAP);
      } catch { /* non-critical */ }
    }
    // axisSparkSvg renders a time series WITH axes (#4298: the dashboard's
    // trend graphs "lack horizontal and vertical axes, making them impossible
    // to understand"). Vertical axis: max/min value labels. Horizontal axis:
    // first/last timestamps. entries need a value (getV) and an epoch-ms
    // timestamp (getT); fmtV formats the axis value labels.
    const AXIS_SPARK_W = 170, AXIS_SPARK_H = 46;
    function axisSparkSvg(entries, getV, getT, fmtV, color, title) {
      if (!entries || entries.length < 2) return '';
      const W = AXIS_SPARK_W, H = AXIS_SPARK_H;
      const PAD_L = 36, PAD_R = 4, PAD_T = 5, PAD_B = 12;
      const plotW = W - PAD_L - PAD_R, plotH = H - PAD_T - PAD_B;
      const values = entries.map(getV);
      const max = Math.max(...values);
      const min = Math.min(...values);
      const range = (max - min) || max || 1;
      const pts = values.map((v, i) => {
        const x = PAD_L + (i / (values.length - 1)) * plotW;
        const y = PAD_T + plotH - ((v - min) / range) * plotH;
        return `${x.toFixed(1)},${y.toFixed(1)}`;
      });
      const fmtDate = t => { const d = new Date(t); return isNaN(d) ? '' : `${d.getMonth() + 1}/${d.getDate()}`; };
      const t0 = fmtDate(getT(entries[0])), t1 = fmtDate(getT(entries[entries.length - 1]));
      const AXIS_TXT = `font-size="7" fill="var(--muted)"`;
      const axes = `<line x1="${PAD_L}" y1="${PAD_T}" x2="${PAD_L}" y2="${PAD_T + plotH}" stroke="var(--border)" stroke-width="1"/>` +
        `<line x1="${PAD_L}" y1="${PAD_T + plotH}" x2="${W - PAD_R}" y2="${PAD_T + plotH}" stroke="var(--border)" stroke-width="1"/>` +
        `<text x="${PAD_L - 3}" y="${PAD_T + 6}" text-anchor="end" ${AXIS_TXT}>${fmtV(max)}</text>` +
        `<text x="${PAD_L - 3}" y="${PAD_T + plotH}" text-anchor="end" ${AXIS_TXT}>${fmtV(min)}</text>` +
        `<text x="${PAD_L}" y="${H - 3}" text-anchor="start" ${AXIS_TXT}>${t0}</text>` +
        `<text x="${W - PAD_R}" y="${H - 3}" text-anchor="end" ${AXIS_TXT}>${t1}</text>`;
      return renderSparkline(null, values, {
        width: W, height: H, color, points: pts, min, max, before: axes,
        title, wrapperTitle: title, wrapperClass: 'sparkline--flush', label: title,
      });
    }
    function factSparkSvg() {
      if (!_factHistory || _factHistory.length < 2) return '';
      const values = _factHistory.map(e => Number(e.count) || 0);
      const times = _factHistory.map(e => e.t);
      return '<div class="kb-stat-spark">' + sparkSvg(values, 'var(--blue)', times) + '</div>';
    }
    // fetchFactHistory() — deferred to _deferredInit

    // ── Estimated-cost sparkline (30-day server-side history) ──
    // Mirrors the fact sparkline: same axis treatment,
    // reads /api/cost/history which parallels /api/knowledge/fact-history.
    let _costHistory = [];
    let _costHistoryLoading = false;
    let _costHistoryInstance = null;
    // Spend calculations require every counter step, including resets. Bootstrap
    // the full retained ring once, then transfer only new full-resolution rows.
    const COST_HISTORY_POINTS = 8640;
    async function fetchCostHistory() {
      if (_costHistoryLoading) return;
      _costHistoryLoading = true;
      try {
        const instance = _statusInstance;
        const history = _costHistoryInstance === instance ? _costHistory : [];
        const since = history.length ? history[history.length - 1].t : 0;
        if (_costHistoryInstance !== instance) _historyValidators.delete('/api/cost/history');
        const data = await fetchDashboardHistory(`/api/cost/history?max_points=${COST_HISTORY_POINTS}${since ? '&since=' + since : ''}`);
        if (instance !== _statusInstance) return; // a restart raced this request
        if (data !== null) {
          _costHistory = capDashboardArray(history.concat(data), COST_HISTORY_POINTS);
          _costHistoryInstance = instance;
        }
      } catch { /* non-critical */ }
      finally { _costHistoryLoading = false; }
    }
    function costSparkSvg() {
      const total = costAllTimeEstimate(_lastCost && _lastCost.estimated);
      const hist = total.series;
      const title = hist.length
        ? `Estimated all-time cost over time — ${costRangeTitle(hist[0].timestamp, hist[hist.length - 1].timestamp, 0)}; ${total.resets.length ? `${total.resets.length} token-summary reset(s) noted; spend on both sides of each is counted once` : 'recorded history samples'}`
        : 'Estimated cost over time (last 30d) — vertical axis: cumulative USD, horizontal axis: date';
      return axisSparkSvg(hist, e => e.usd, e => e.timestamp, v => '$' + fmtSparkVal(v), 'var(--green)',
        title);
    }
    // fetchCostHistory() is invoked alongside fetchCost() (see below).

    // ── Governor / repo / system trend history (server-persisted) ──
    // These trends used to live only in the browser's localStorage
    // (hive_sparkline_history), so they were blank after a pod restart or for a
    // brand-new viewer. The server now persists ~30 days of snapshots to
    // /data/trend-history.json (same ring-buffer treatment as fact/cost). We
    // fetch them and merge into historyData by timestamp so the governor / repo
    // / system-gauge sparklines render immediately instead of blank.
    // Field names in the server entry mirror the client snapshot shape
    // (govIssues, govPrs, govTotal, govHold, repos[name].{issues,prs}) so no
    // per-field translation is needed.
    let _serverTrendHistory = [];
    const TREND_MERGE_DELTA_MS = 60000;
    async function fetchTrendHistory() {
      try {
        const r = await fetch('/api/trend/history');
        const data = await r.json();
        _serverTrendHistory = capDashboardArray(data, DASHBOARD_SERVER_HISTORY_CAP);
        mergeTrendHistory();
      } catch { /* non-critical */ }
    }
    // mergeTrendHistory folds cached server trend snapshots into historyData,
    // matching each to the nearest live snapshot within TREND_MERGE_DELTA_MS and
    // injecting any that have no live counterpart. Idempotent-ish: injected
    // entries carry _fromTrend so a re-merge (after fetchHistory reassigns
    // historyData) doesn't double-count against already-present live points.
    function mergeTrendHistory() {
      if (!_serverTrendHistory.length) return;
      const matched = new Set();
      for (const live of historyData) {
        const lt = live.t || 0;
        let bestIdx = -1, bestDelta = Infinity;
        for (let i = 0; i < _serverTrendHistory.length; i++) {
          const d = Math.abs((_serverTrendHistory[i].t || 0) - lt);
          if (d < bestDelta) { bestDelta = d; bestIdx = i; }
        }
        if (bestIdx >= 0 && bestDelta < TREND_MERGE_DELTA_MS) matched.add(bestIdx);
      }
      const injected = [];
      for (let i = 0; i < _serverTrendHistory.length; i++) {
        if (matched.has(i)) continue;
        const e = _serverTrendHistory[i];
        const repos = {};
        for (const [name, rs] of Object.entries(e.repos || {})) {
          repos[name] = { issues: rs.issues || 0, prs: rs.prs || 0 };
        }
        injected.push({
          t: e.t,
          govIssues: e.govIssues || 0,
          govPrs: e.govPrs || 0,
          govTotal: e.govTotal || 0,
          govHold: e.govHold || 0,
          repos,
          agents: {},
          _fromTrend: true,
        });
      }
      if (injected.length) {
        historyData = historyData.concat(injected);
        historyData.sort((a, b) => (a.t || 0) - (b.t || 0));
        historyData = capDashboardArray(historyData, DASHBOARD_HISTORY_CAP);
        capHistory(historyData);
      }
    }

