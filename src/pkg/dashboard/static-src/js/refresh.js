    // ── System resource gauge history for sparklines ──
    // NOTE: these mini-sparklines still read this in-memory array, which resets
    // on reload. The disk/mem/cpu percentages ARE now persisted server-side in
    // the trend history (/api/trend/history, entry.system) alongside the
    // governor/repo/beads trends, so the data survives restarts; wiring these
    // gauge sparklines to that persisted series is a follow-up (kept client-only
    // here to avoid reworking renderSystemGauges in this change).
    const SYS_GAUGE_HISTORY_MAX = 60;
    const _sysGaugeHistory = { disk: [], mem: [], cpu: [] };

    const SYS_GAUGE_NORMAL_MAX = 70;   // below this: muted gray
    const SYS_GAUGE_AMBER_MAX = 85;    // 70-85%: amber warning
    const SYS_GAUGE_ORANGE_MAX = 95;   // 85-95%: orange alert

    const SYS_GAUGE_SPARK_W = 30;      // sparkline width in px
    const SYS_GAUGE_SPARK_H = 8;       // sparkline height in px

    function sysGaugeColor(pct) {
      if (pct < SYS_GAUGE_NORMAL_MAX) return 'var(--muted)'; // muted gray — healthy
      if (pct < SYS_GAUGE_AMBER_MAX) return 'var(--yellow)'; // warning — elevated
      if (pct < SYS_GAUGE_ORANGE_MAX) return 'var(--orange)'; // orange — high
      return 'var(--red)';                                     // red — critical
    }

    function sysGaugeMiniSpark(values, color, name) {
      if (!values || values.length < 2) return '';
      const max = Math.max(...values, 1);
      return renderSparkline(null, values, {
        width: SYS_GAUGE_SPARK_W, height: SYS_GAUGE_SPARK_H, color, min: 0, max,
        strokeWidth: 1, dot: false, wrapper: false, lineAttrs: ' opacity="0.3"',
        title: `${name || 'system'} gauge sparkline · latest ${fmtSparkVal(values[values.length - 1])}%`,
        ariaLabel: `${name || 'system'} gauge sparkline latest ${fmtSparkVal(values[values.length - 1])}%`,
        label: `${name || 'system'} gauge sparkline`, id: `sys-${name || 'gauge'}`,
      });
    }

    function renderSystemGauges(sysRes) {
      if (!sysRes) return;

      // Push to history
      _sysGaugeHistory.disk.push(sysRes.diskPct || 0);
      _sysGaugeHistory.mem.push(sysRes.memPct || 0);
      _sysGaugeHistory.cpu.push(sysRes.cpuPct || 0);
      if (_sysGaugeHistory.disk.length > SYS_GAUGE_HISTORY_MAX) _sysGaugeHistory.disk.shift();
      if (_sysGaugeHistory.mem.length > SYS_GAUGE_HISTORY_MAX) _sysGaugeHistory.mem.shift();
      if (_sysGaugeHistory.cpu.length > SYS_GAUGE_HISTORY_MAX) _sysGaugeHistory.cpu.shift();

      // Disk
      const diskPct = sysRes.diskPct || 0;
      const diskColor = sysGaugeColor(diskPct);
      const diskBar = document.getElementById('sys-bar-disk');
      if (diskBar) {
        diskBar.style.width = diskPct + '%';
        diskBar.style.background = diskColor;
      }
      const diskVal = document.getElementById('sys-val-disk');
      if (diskVal) {
        diskVal.textContent = sysRes.diskTotalGB > 0
          ? `${sysRes.diskUsedGB}/${sysRes.diskTotalGB}`
          : `${diskPct.toFixed(0)}%`;
        diskVal.style.color = diskPct >= SYS_GAUGE_NORMAL_MAX ? diskColor : '';
      }
      const diskRow = document.getElementById('sys-gauge-disk');
      if (diskRow) {
        diskRow.title = sysRes.diskTotalGB > 0
          ? `Disk: ${sysRes.diskUsedGB} GB used of ${sysRes.diskTotalGB} GB (${diskPct.toFixed(0)}%) — /data volume`
          : `Disk: ${diskPct.toFixed(0)}% used`;
      }
      const diskSpark = document.getElementById('sys-spark-disk');
      if (diskSpark) diskSpark.innerHTML = sysGaugeMiniSpark(_sysGaugeHistory.disk, diskColor, 'disk');

      // Memory
      const memPct = sysRes.memPct || 0;
      const memColor = sysGaugeColor(memPct);
      const memBar = document.getElementById('sys-bar-mem');
      if (memBar) {
        memBar.style.width = memPct + '%';
        memBar.style.background = memColor;
      }
      const memVal = document.getElementById('sys-val-mem');
      const MEM_MB_PER_GB = 1024;
      if (memVal) {
        if (sysRes.memTotalMB > MEM_MB_PER_GB) {
          memVal.textContent = `${(sysRes.memUsedMB / MEM_MB_PER_GB).toFixed(1)}/${(sysRes.memTotalMB / MEM_MB_PER_GB).toFixed(1)}`;
        } else if (sysRes.memTotalMB > 0) {
          memVal.textContent = `${Math.round(sysRes.memUsedMB)}/${Math.round(sysRes.memTotalMB)}`;
        } else {
          memVal.textContent = `${memPct.toFixed(0)}%`;
        }
        memVal.style.color = memPct >= SYS_GAUGE_NORMAL_MAX ? memColor : '';
      }
      const memRow = document.getElementById('sys-gauge-mem');
      if (memRow) {
        memRow.title = sysRes.memTotalMB > 0
          ? `Memory: ${Math.round(sysRes.memUsedMB)} MB used of ${Math.round(sysRes.memTotalMB)} MB (${memPct.toFixed(0)}%)`
          : `Memory: ${memPct.toFixed(0)}% used`;
      }
      const memSpark = document.getElementById('sys-spark-mem');
      if (memSpark) memSpark.innerHTML = sysGaugeMiniSpark(_sysGaugeHistory.mem, memColor, 'memory');

      // CPU
      const cpuPct = sysRes.cpuPct || 0;
      const cpuColor = sysGaugeColor(cpuPct);
      const cpuBar = document.getElementById('sys-bar-cpu');
      if (cpuBar) {
        cpuBar.style.width = Math.min(cpuPct, 100) + '%';
        cpuBar.style.background = cpuColor;
      }
      const cpuVal = document.getElementById('sys-val-cpu');
      if (cpuVal) {
        cpuVal.textContent = `${cpuPct.toFixed(0)}%`;
        cpuVal.style.color = cpuPct >= SYS_GAUGE_NORMAL_MAX ? cpuColor : '';
      }
      const cpuRow = document.getElementById('sys-gauge-cpu');
      if (cpuRow) {
        const cpuCores = sysRes.cpuCores || 0;
        cpuRow.title = cpuCores > 0
          ? `CPU: ${cpuPct.toFixed(0)}% average (${cpuCores} cores)`
          : `CPU: ${cpuPct.toFixed(0)}% average`;
      }
      const cpuSpark = document.getElementById('sys-spark-cpu');
      if (cpuSpark) cpuSpark.innerHTML = sysGaugeMiniSpark(_sysGaugeHistory.cpu, cpuColor, 'cpu');
      const debugSection = document.getElementById('debug-section');
      if (debugSection) replaySparklinesIn(debugSection);
    }

    let _hashScrollPending = false;

    // ── Stale-snapshot guard state (#4348) ─────────────────────────────
    // _lastStatusSeq: highest statusSeq applied to the DOM. _statusSeqFloor:
    // snapshots below this were built before a mutation this client has
    // already confirmed (e.g. restart-count / budget-window reset) and must
    // never be rendered. See the guard at the top of render().
    let _lastStatusSeq = 0;
    let _statusSeqFloor = 0;
    let _statusInstance = '';
    // noteStatusMutation raises the floor from a mutation response's
    // minStatusSeq so in-flight pre-mutation polls/SSE frames are discarded.
    function noteStatusMutation(minSeq) {
      const n = Number(minSeq);
      if (Number.isFinite(n) && n > _statusSeqFloor) _statusSeqFloor = n;
    }
    var SCROLL_SETTLE_DELAY_MS = 500;
    var _deferredLoadPromise = null;
    function _afterPaint(callback) {
      _hashScrollPending = true;
      if (_deferredLoadPromise) {
        _deferredLoadPromise.then(function() {
          requestAnimationFrame(function() {
            requestAnimationFrame(function() {
              callback();
              setTimeout(function() { _hashScrollPending = false; }, 200);
            });
          });
        });
      } else {
        requestAnimationFrame(function() {
          requestAnimationFrame(function() {
            setTimeout(callback, SCROLL_SETTLE_DELAY_MS);
            setTimeout(function() { _hashScrollPending = false; }, 500);
          });
        });
      }
    }

    const NAVBAR_CLOCK_TICK_MS = 1000;
    const STATUS_EXPECTED_REFRESH_MS = 30000;
    const STATUS_STALE_AFTER_POLLS = 3;
    const STATUS_STALE_AFTER_MS = STATUS_EXPECTED_REFRESH_MS * STATUS_STALE_AFTER_POLLS;
    // ── SSE idle watchdog ──────────────────────────────────────────────
    // /api/events only carries status frames when the eval cycle publishes
    // (~every STATUS_EXPECTED_REFRESH_MS). A connection that a NAT/proxy idle
    // timeout or a laptop sleep silently dropped never fires EventSource
    // onerror, so without a watchdog the tab paints its last snapshot
    // forever while the "live" dot stays green — one tab shows counts from
    // hours ago while a fresh tab beside it is right. The server now sends a
    // `keepalive` event every SSE_KEEPALIVE_EXPECTED_MS; any frame (status,
    // agent-status, stage-tracker, keepalive) resets the idle clock, and
    // SSE_IDLE_RECONNECT_MS of silence (3 missed status publishes, the same
    // window the "status Xm old" badge uses) reconnects and refetches
    // /api/status so the page converges on the server's truth.
    const SSE_KEEPALIVE_EXPECTED_MS = 20000;
    const SSE_WATCHDOG_TICK_MS = 15000;
    const SSE_IDLE_RECONNECT_MS = STATUS_STALE_AFTER_MS;
    // sse watchdog helpers begin
    // sseStreamIdle: true when the stream has been silent for longer than
    // idleMs. lastFrameAtMs is the newest frame of any kind; openedAtMs is
    // when the current EventSource was created, so a connection that never
    // produced a frame (half-open from the start, or the server never got the
    // request) is also recycled instead of waiting on a frame that will not
    // come. Zero/absent timestamps mean "not connected yet" → not idle.
    function sseStreamIdle(nowMs, lastFrameAtMs, openedAtMs, idleMs) {
      const since = Math.max(Number(lastFrameAtMs) || 0, Number(openedAtMs) || 0);
      if (!since) return false;
      return (nowMs - since) > idleMs;
    }
    // sse watchdog helpers end
    const DASHBOARD_TIMEZONE_STORAGE_KEY = 'hive-dashboard-display-timezone';
    const DASHBOARD_TIMEZONE_BROWSER = 'browser';
    const DASHBOARD_TIMEZONE_SERVER = 'server';
    let _navbarClockTimeZone = '';
    let _dashboardServerTimeZone = '';
    let _lastStatusPayloadAtMs = 0;

    function validDashboardTimeZone(tz) {
      if (!tz || typeof tz !== 'string') return '';
      const cache = validDashboardTimeZone.cache || (validDashboardTimeZone.cache = new Map());
      if (cache.has(tz)) return cache.get(tz);
      try {
        new Intl.DateTimeFormat([], { timeZone: tz }).format(new Date());
        if (cache.size >= 64) cache.clear();
        cache.set(tz, tz);
        return tz;
      } catch (e) {
        return '';
      }
    }

    function browserDashboardTimeZone() {
      // Recheck occasionally so OS timezone changes still reach a long-lived tab.
      const now = Date.now();
      if (browserDashboardTimeZone.expires > now) return browserDashboardTimeZone.value;
      try {
        const tz = (Intl && Intl.DateTimeFormat && Intl.DateTimeFormat().resolvedOptions().timeZone) || '';
        browserDashboardTimeZone.value = tz;
        browserDashboardTimeZone.expires = now + 60000;
        return tz;
      } catch (e) {
        return '';
      }
    }

    function dashboardTimeZonePreference() {
      try { return localStorage.getItem(DASHBOARD_TIMEZONE_STORAGE_KEY) || DASHBOARD_TIMEZONE_BROWSER; } catch (e) { return DASHBOARD_TIMEZONE_BROWSER; }
    }

    function selectedDashboardTimeZone() {
      const pref = dashboardTimeZonePreference();
      if (pref === DASHBOARD_TIMEZONE_SERVER) return validDashboardTimeZone(_dashboardServerTimeZone) || browserDashboardTimeZone() || 'UTC';
      if (pref === 'UTC') return 'UTC';
      if (pref && pref !== DASHBOARD_TIMEZONE_BROWSER) return validDashboardTimeZone(pref) || browserDashboardTimeZone() || 'UTC';
      return browserDashboardTimeZone() || 'UTC';
    }

    function dashboardTimeZoneLabel(tz) {
      if (!tz) return 'Browser local';
      if (tz === 'UTC') return 'UTC';
      return tz;
    }

    function supportedDashboardTimeZones() {
      try {
        if (Intl && typeof Intl.supportedValuesOf === 'function') {
          const zones = Intl.supportedValuesOf('timeZone');
          if (Array.isArray(zones) && zones.length) return zones;
        }
      } catch (e) {}
      return ['UTC','America/New_York','America/Chicago','America/Denver','America/Los_Angeles','America/Phoenix','America/Anchorage','Pacific/Honolulu','Europe/London','Europe/Berlin','Asia/Kolkata','Asia/Tokyo','Australia/Sydney'];
    }

    const _dashboardDateFormatters = new Map();
    function dashboardDateFormatter(options) {
      const opts = Object.assign({}, options || {});
      const tz = selectedDashboardTimeZone();
      if (tz) opts.timeZone = tz;
      const key = JSON.stringify(Object.keys(opts).sort().map(k => [k, opts[k]]));
      if (_dashboardDateFormatters.has(key)) return _dashboardDateFormatters.get(key);
      const formatter = new Intl.DateTimeFormat([], opts);
      // Bound memory even when callers supply arbitrary option combinations.
      if (_dashboardDateFormatters.size >= 64) _dashboardDateFormatters.delete(_dashboardDateFormatters.keys().next().value);
      _dashboardDateFormatters.set(key, formatter);
      return formatter;
    }

    function formatDashboardDateTime(value, options, fallback) {
      const d = value instanceof Date ? value : new Date(value);
      if (isNaN(d.getTime())) return fallback !== undefined ? fallback : String(value || '');
      const opts = options || { month:'numeric', day:'numeric', hour:'numeric', minute:'2-digit', hour12:true, timeZoneName:'short' };
      try {
        const fmt = dashboardDateFormatter(opts);
        if (typeof fmt.formatToParts === 'function' && opts.month === 'numeric' && opts.day === 'numeric') {
          const parts = {};
          for (const p of fmt.formatToParts(d)) parts[p.type] = p.value;
          const datePart = parts.month && parts.day ? `${parts.month}/${parts.day}` : '';
          if (opts.hour || opts.minute) {
            const dayPeriod = parts.dayPeriod ? ' ' + parts.dayPeriod : '';
            const zone = parts.timeZoneName ? ' ' + parts.timeZoneName : '';
            return `${datePart} ${parts.hour || ''}:${parts.minute || ''}${dayPeriod}${zone}`.trim();
          }
          if (datePart) return datePart;
        }
        return fmt.format(d);
      } catch (e) { return d.toISOString(); }
    }

    function formatDashboardDate(value, options, fallback) {
      return formatDashboardDateTime(value, options || { month:'numeric', day:'numeric' }, fallback);
    }

    function formatDashboardTime(value, options, fallback) {
      return formatDashboardDateTime(value, options || { hour:'numeric', minute:'2-digit', hour12:true, timeZoneName:'short' }, fallback);
    }

    function formatNumber(value, locale, options) {
      try { return new Intl.NumberFormat(locale || undefined, options || {}).format(value); } catch (e) { return String(value); }
    }

    function updateNavbarClockTimeZone(data) {
      _dashboardServerTimeZone = validDashboardTimeZone(data && (data.timeZone || data.timezone || data.tz)) || '';
      _navbarClockTimeZone = (typeof selectedDashboardTimeZone === 'function')
        ? selectedDashboardTimeZone()
        : (_dashboardServerTimeZone || browserDashboardTimeZone());
    }

    function noteStatusPayloadTimestamp(data) {
      const payloadAt = new Date(data && data.timestamp).getTime();
      _lastStatusPayloadAtMs = Number.isFinite(payloadAt) ? payloadAt : Date.now();
    }

    function formatNavbarClock(now, timeZone) {
      const opts = { month:'numeric', day:'numeric', hour:'numeric', minute:'2-digit', hour12:true, timeZoneName:'short' };
      const fmt = dashboardDateFormatter(Object.assign({}, opts, timeZone ? { timeZone } : {}));
      if (typeof fmt.formatToParts === 'function') {
        const parts = {};
        for (const p of fmt.formatToParts(now)) parts[p.type] = p.value;
        const dayPeriod = parts.dayPeriod ? ' ' + parts.dayPeriod : '';
        return `${parts.month}/${parts.day} ${parts.hour}:${parts.minute}${dayPeriod} ${parts.timeZoneName || ''}`.trim();
      }
      return fmt.format(now);
    }

    function formatStatusPayloadAge(ms) {
      const minutes = Math.max(1, Math.floor(ms / 60000));
      if (minutes < 60) return `${minutes}m`;
      const hours = Math.floor(minutes / 60);
      const rem = minutes % 60;
      return rem ? `${hours}h ${rem}m` : `${hours}h`;
    }

    function formatGovernorNextRun(gov, nowMs = Date.now()) {
      if (!gov || !gov.nextKick) return '—';
      const nextMs = new Date(gov.nextKickAt || '').getTime();
      if (Number.isFinite(nextMs) && nextMs <= nowMs) {
        const overdueMs = nowMs - nextMs;
        if (overdueMs < 60000) return 'due now';
        return `${gov.active ? 'delayed' : 'overdue'} ${formatStatusPayloadAge(overdueMs)}`;
      }
      return formatDashboardDateTime(gov.nextKickAt || gov.nextKick, { month:'numeric', day:'numeric', hour:'numeric', minute:'2-digit', hour12:true, timeZoneName:'short' }, gov.nextKick);
    }

    function setNavbarClockText(text) {
      const ts = document.getElementById('ts');
      if (ts) ts.textContent = text;
      const ocTs = document.getElementById('oc-ts');
      if (ocTs) ocTs.textContent = text;
    }

    function updateStatusStaleness(nowMs) {
      const staleEls = [document.getElementById('status-stale'), document.getElementById('oc-status-stale')].filter(Boolean);
      if (!staleEls.length) return;
      const ageMs = _lastStatusPayloadAtMs ? Math.max(0, nowMs - _lastStatusPayloadAtMs) : 0;
      const stale = ageMs > STATUS_STALE_AFTER_MS;
      const label = stale ? `status ${formatStatusPayloadAge(ageMs)} old` : '';
      for (const el of staleEls) {
        el.hidden = !stale;
        el.textContent = label;
        el.title = stale ? 'The last /api/status payload is stale; live status-dependent values may be behind.' : '';
      }
    }

    function tickNavbarClock(now = new Date()) {
      if (!_navbarClockTimeZone) _navbarClockTimeZone = browserDashboardTimeZone();
      setNavbarClockText(formatNavbarClock(now, _navbarClockTimeZone));
      updateStatusStaleness(now.getTime());
      if (typeof refreshVersionProgressSurfaces === 'function') refreshVersionProgressSurfaces();
    }

    // ── Background-refresh stability (#11032) ──
    // Status pushes repaint every section; these helpers keep the reader on
    // the same content and keep refreshes away from fields being edited.
    function isEditableElement(el) {
      if (!el || el === document.body) return false;
      if (el.isContentEditable) return true;
      return el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT';
    }

    // True when focus sits in a form field inside el, or a field inside el
    // (other than those matching ignoreSelector) differs from its rendered default.
    function sectionHoldsUserInput(el, ignoreSelector) {
      if (!el) return false;
      const active = document.activeElement;
      if (isEditableElement(active) && el.contains(active)) return true;
      const fields = el.querySelectorAll('input, textarea, select');
      for (let i = 0; i < fields.length; i++) {
        const f = fields[i];
        if (ignoreSelector && f.matches(ignoreSelector)) continue;
        if (f.tagName === 'SELECT') {
          for (let j = 0; j < f.options.length; j++) {
            if (f.options[j].selected !== f.options[j].defaultSelected) return true;
          }
        } else if (f.type === 'checkbox' || f.type === 'radio') {
          if (f.checked !== f.defaultChecked) return true;
        } else if (f.type !== 'hidden' && f.value !== f.defaultValue) {
          return true;
        }
      }
      return false;
    }

    // Remember the topmost visible section and the reader's offset within it,
    // so a repaint that resizes content above doesn't move them elsewhere.
    function captureScrollAnchor() {
      const anchor = { el: null, top: 0, scrollY: window.scrollY, cardKey: '', cardTop: 0 };
      // A sidebar navigation is settling: pin the section the reader asked
      // for, not whichever section above it still peeks out under the
      // sticky navbar — otherwise a repaint that resizes that neighbour
      // drags the target away from the top of the viewport.
      const navEl = typeof _ocNavAnchorEl !== 'undefined' ? _ocNavAnchorEl : null;
      if (navEl && navEl.isConnected && navEl.getClientRects().length) {
        anchor.el = navEl; anchor.top = navEl.getBoundingClientRect().top;
        return anchor;
      }
      if (window.scrollY <= 0) return anchor;
      const sections = document.querySelectorAll('[data-dashboard-section]');
      for (let i = 0; i < sections.length; i++) {
        const el = sections[i];
        if (!el.getClientRects().length) continue;
        const rect = el.getBoundingClientRect();
        if (rect.bottom > 0) { anchor.el = el; anchor.top = rect.top; break; }
      }
      // A section's top is too coarse once the reader has scrolled INTO it:
      // the agents grid is several rows of cards whose terminal tails grow
      // with every refresh, so rows above the viewport change height while
      // the section's top edge stays put. Anchor on the first card still in
      // view instead, by key — the node may be rebuilt by the render.
      if (anchor.el && anchor.top < 0) {
        const cards = anchor.el.querySelectorAll('.agent-card[data-agent]');
        for (let i = 0; i < cards.length; i++) {
          const rect = cards[i].getBoundingClientRect();
          if (rect.bottom > 0 && rect.height > 0) { anchor.cardKey = cards[i].getAttribute('data-agent'); anchor.cardTop = rect.top; break; }
        }
      }
      return anchor;
    }

    function restoreScrollAnchor(anchor) {
      if (anchor.cardKey && anchor.el && anchor.el.isConnected) {
        const safeKey = (window.CSS && CSS.escape) ? CSS.escape(anchor.cardKey) : String(anchor.cardKey).replace(/["\\]/g, '\\$&');
        const card = anchor.el.querySelector(`.agent-card[data-agent="${safeKey}"]`);
        if (card && card.getClientRects().length) {
          const delta = card.getBoundingClientRect().top - anchor.cardTop;
          if (delta) window.scrollBy(0, delta);
          return;
        }
      }
      if (anchor.el && anchor.el.isConnected && anchor.el.getClientRects().length) {
        const delta = anchor.el.getBoundingClientRect().top - anchor.top;
        if (delta) window.scrollBy(0, delta);
        return;
      }
      window.scrollTo(0, anchor.scrollY);
    }

    function render(data) {
      if (!data || data.error || data.status === 'initializing') return;
      // ── Stale-snapshot guard (#4348) ──────────────────────────────────
      // Status payloads arrive from several racing sources (SSE pushes,
      // periodic refetches, post-action refreshStatus calls). Two drops:
      //  1. out-of-order: a response older than the newest one already
      //     rendered must not repaint the page backwards;
      //  2. pre-mutation: after a reset button confirms success, snapshots
      //     built before that mutation (statusSeq below the server-supplied
      //     minStatusSeq floor) would revert the optimistic 0 — discard them
      //     until a post-mutation snapshot arrives.
      // Payloads without statusSeq (older spoke server) apply as before.
      if (data.statusSeq != null) {
        if (data.statusInstance && data.statusInstance !== _statusInstance) {
          // New server process: seqs restarted at 1 — reset the guard so a
          // spoke restart doesn't freeze the page on pre-restart state.
          _statusInstance = data.statusInstance;
          _lastStatusSeq = 0;
          _statusSeqFloor = 0;
        }
        if (data.statusSeq < _statusSeqFloor || data.statusSeq < _lastStatusSeq) return;
        _lastStatusSeq = data.statusSeq;
      }
      if (document.body.style.opacity === '0') document.body.style.opacity = '1';
      const scrollAnchor = captureScrollAnchor();
      updateNavbarClockTimeZone(data);
      noteStatusPayloadTimestamp(data);
      noteAgentNowUpdate(data && data.timestamp);
      tickNavbarClock();
      currentAgentMetrics = data.agentMetrics || {};
      window._healthData = data.health || {};
      window._tokensByAgent = (data.tokens || {}).byAgent || {};
      window._lastAgents = data.agents || [];
      window._configuredAgents = data.configuredAgents || [];
      const prevWorkers = window._lastStatus && typeof stageTrackerWorkers === 'function' ? JSON.stringify(stageTrackerWorkers(window._lastStatus)) : '';
      window._lastStatus = data;
      // The tracker's hop cue reads which agent is on which item from this
      // payload; redraw the rows when that set changes (no extra fetch).
      if (typeof stageTrackerWorkers === 'function' && JSON.stringify(stageTrackerWorkers(data)) !== prevWorkers && typeof renderStageTracker === 'function') renderStageTracker();
      applyDashboardFeatureVisibility();
      if (typeof refreshThroughputSLA === 'function') refreshThroughputSLA();
      if (window._acmmOverride) {
        if (data.acmmLevel === window._acmmOverride.level) {
          window._acmmOverride = null;
        } else {
          window._lastStatus.acmmLevel = window._acmmOverride.level;
          window._lastStatus.acmmLevelConfigured = true;
          window._lastStatus.acmmPackAgents = window._acmmOverride.packAgents;
        }
      }
      window._lastBudget = data.budget || {};
      // Keep the fleet-breaker toggle in sync with server truth on every status
      // refresh (another operator may have engaged it, or a restart restored it).
      if (typeof refreshFleetBreakerState === 'function') refreshFleetBreakerState();
      updateInferenceModelsFromSSE(data.inferenceBackends);
      if (data.agents) injectAgentStyles(data.agents);
      // Display the Hive ID in header badges
      const hiveId = data.hiveId || '';
      const hiveIdBadge = document.getElementById('hive-id-badge');
      if (hiveIdBadge) hiveIdBadge.textContent = hiveId;
      const ocHiveId = document.getElementById('oc-hive-id');
      if (ocHiveId) ocHiveId.textContent = hiveId;
      if (typeof stageTrackerPopoutSyncTitle === 'function') stageTrackerPopoutSyncTitle(hiveId);
      // Update Light health badge with hover detail
      renderNavbarHealthBadge(document.getElementById('oc-health'), data);
      renderProviderStrip(document.getElementById('oc-providers'), data.providers);
      renderGhRateLimits(data.ghRateLimits || {});
      renderGitHubAppBanner(data);
      renderRepoTargetBanner(data);
      renderIssuesDisabledBanner(data);
      refreshWelcomeAutoChecks(data);
      renderHubBanner(data.hubBanner);
      maybePlanningIntro(data);
      renderSystemAlerts(data.systemAlerts || []);
      if (!_dropdownOpen && !_configModalOpen) {
        applyPinOverrides(data.agents || []);
        renderAgents(data.agents || []);
        renderGovernor(data.governor || {}, data.cadenceMatrix || [], data);
        // Keep the focused agent detail in sync with fresh payloads — its
        // fast path only rebuilds when the config fingerprint changes.
        if (_ocSelectedAgent) ocRenderAgentDetail();
      }
      renderTokens(data.tokens || {});
      renderAdvisoryDigest(data.advisoryDigest || null);
      renderRepos(data.repos || []);
      renderSwarm(data.swarm || {});
      var reposBadge = document.getElementById('repos-sidebar-badge');
      if (reposBadge) setSidebarCountBadge(reposBadge, (data.repos || []).length, (data.repos || []).length + ' repositories');
      updateContributorBadge(data.contributorPool || null);
      renderPlatform(data.platform || null);
      renderHiveAdvice(data.hiveAdvice || null);
      renderFleetReport(data.fleetReport || null);
      renderSystemGauges(data.systemResources || null);
      ocUpdateSidebarAgents();
      renderDebugSection();
      renderFAQ();
      initializeDashboardSectionCards();
      // Restore persisted collapsed state for all collapsible sections
      const COLLAPSIBLE_SECTION_IDS = [
        'advisory-section', 'advisory-digest-section', 'hive-advice-section', 'acmm-reco-section', 'lifecycle-section', 'pr-throughput-section',
        'overview-section', 'governor', 'token-panel', 'cost-panel', 'repos-section', 'acmm-eval-section',
        'fleet-report-section', 'audit-section', 'review-queue-section', 'nous-section', 'inception-section',
        'knowledge-section', 'contributors-section', 'debug-section', 'logs-section',
        'activity-monitor-section', 'agents-section', 'agent-activity-section', 'faq-section'
      ];
      COLLAPSIBLE_SECTION_IDS.forEach(applySectionCollapse);
      refreshDashboardSectionSummaries(data);
      // Update compact-all button text
      const compactBtn = document.getElementById('agents-compact-all-btn');
      if (compactBtn) compactBtn.textContent = localStorage.getItem(ALL_AGENTS_COMPACT_KEY) === '1' ? '⊞ Expand All' : '⊟ Compact All';
      updateACMMBadge(data.acmmLevel || 0, data.acmmPackAgents || null);
      if (!_ocViewRestored) {
        _ocViewRestored = true;
        if (_ocSelectedAgent) {
          const restoreName = _ocSelectedAgent;
          ocRenderAgentDetail(); ocUpdateFocusedState(); ocStartPanePoll();
          _afterPaint(function() {
            const detail = document.getElementById('oc-agent-detail');
            if (detail) {
              detail.classList.add('active');
              // Instant behavior (no animation on load), but still routed
              // through the settle loop: first-load layout shifts the most
              // (deferred sections, images, async fetches all land late).
              ocScrollToAgentDetail('instant');
            }
          });
        } else {
          const savedSection = _ocSectionFromHash();
          if (savedSection) {
            _afterPaint(function() { ocNavigate(savedSection); });
          }
        }
      } else if (_ocSelectedAgent) {
        ocRenderAgentDetail(); ocUpdateFocusedState(); ocStartPanePoll();
        if (!_hashScrollPending) restoreScrollAnchor(scrollAnchor);
      } else {
        if (!_hashScrollPending) restoreScrollAnchor(scrollAnchor);
      }
    }

