/* ---- Display mode: honest liveness and wall layout ---------------------
       Rendering-only additions for `?display=1` (body.display-mode, see
       boot.js and tracker-popout.js); nothing here runs, or changes a pixel,
       on the ordinary Activity Monitor or a headless `?kiosk=1` stream.
       Liveness (hivecommons/hive#15930): a 1 s tick dims the board and says
       "Stale: last update N ago" once the event stream or the payload goes
       quiet, and a display that cannot reconnect reloads itself on a capped
       backoff. Layout (hivecommons/hive#15931): `density=tv` keeps the stage,
       title and actor at a larger type and caps the rows to the viewport
       with a "+N more" footer; `shift=1` is a CSS burn-in shift; reduced
       motion turns off row movement. No endpoint, credential or URL knob
       beyond `density` and `shift`. */

    // tracker display liveness helpers begin
    const STAGE_TRACKER_DISPLAY_RELOAD_AFTER_FAILS = 5;
    const STAGE_TRACKER_DISPLAY_RELOAD_BASE_MS = 30000;
    const STAGE_TRACKER_DISPLAY_RELOAD_MAX_MS = 600000;
    // Pure: is a display board's data stale? Stale when the event stream has
    // been silent for more than two poll intervals, or the payload's
    // generated_at is older than two cache TTLs. A zero/absent timestamp is
    // "not known yet" and judges nothing. sinceMs is the last update of the
    // signal that went quiet (the newer one when both did); reason is
    // 'sse', 'ttl' or 'both', '' when fresh.
    function stageTrackerDisplayStale(nowMs, sseLastFrameMs, pollMs, generatedAtMs, cacheTtlMs) {
      const now = Number(nowMs) || 0;
      const sseAt = Number(sseLastFrameMs) || 0;
      const genAt = Number(generatedAtMs) || 0;
      const poll = Number(pollMs) || 0;
      const ttl = Number(cacheTtlMs) || 0;
      const sse = sseAt > 0 && poll > 0 && now - sseAt > 2 * poll;
      const old = genAt > 0 && ttl > 0 && now - genAt > 2 * ttl;
      if (!sse && !old) return { stale: false, sinceMs: 0, reason: '' };
      if (sse && old) return { stale: true, sinceMs: Math.max(sseAt, genAt), reason: 'both' };
      return sse ? { stale: true, sinceMs: sseAt, reason: 'sse' } : { stale: true, sinceMs: genAt, reason: 'ttl' };
    }
    // Pure: how long to wait before reloading a display whose stream keeps
    // failing — 0 until STAGE_TRACKER_DISPLAY_RELOAD_AFTER_FAILS consecutive
    // failed reconnects, then 30 s doubling per reload already tried this
    // session (attempt), capped at 10 min.
    function stageTrackerDisplayReloadDelayMs(failures, attempt) {
      if ((Number(failures) || 0) < STAGE_TRACKER_DISPLAY_RELOAD_AFTER_FAILS) return 0;
      const n = Math.min(30, Math.max(0, Math.floor(Number(attempt) || 0)));
      return Math.min(STAGE_TRACKER_DISPLAY_RELOAD_MAX_MS, STAGE_TRACKER_DISPLAY_RELOAD_BASE_MS * Math.pow(2, n));
    }
    // Pure: should a display reload for this roll record (#15804's
    // versionReadUpgradeProgress)? Only for a roll that finished after the
    // page loaded, so the reload itself never triggers another.
    function stageTrackerDisplayRollReload(roll, loadedAtMs) {
      if (!roll || roll.source !== 'roll') return false;
      const done = Number(roll.completedAt) || 0;
      return done > 0 && done > (Number(loadedAtMs) || 0);
    }
    // Pure: "45 s", "4 m", "2 h", "3 d" for the stale banner.
    function stageTrackerDisplayAgo(ms) {
      const s = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
      if (s < 60) return s + ' s';
      if (s < 3600) return Math.floor(s / 60) + ' m';
      if (s < 86400) return Math.floor(s / 3600) + ' h';
      return Math.floor(s / 86400) + ' d';
    }
    // Pure: the banner line for a stageTrackerDisplayStale result.
    function stageTrackerDisplayStaleText(st, nowMs) {
      if (!st || !st.stale) return '';
      return st.sinceMs ? 'Stale: last update ' + stageTrackerDisplayAgo(nowMs - st.sinceMs) + ' ago' : 'Stale: no update yet';
    }
    // tracker display liveness helpers end

    // tracker display layout helpers begin
    const STAGE_TRACKER_DISPLAY_TV = 'tv';
    const STAGE_TRACKER_DISPLAY_TV_ROW_PX = 96;
    const STAGE_TRACKER_DISPLAY_TV_FOOT_PX = 48;
    // Pure: how many rows fit a TV display — the viewport less everything
    // above the rows (and the footer), over one row's height. At least one
    // row, and one when the sizes are not known yet.
    function stageTrackerDisplayRowCap(viewportPx, headerPx, rowPx) {
      const v = Number(viewportPx), r = Number(rowPx);
      const h = Math.max(0, Number(headerPx) || 0);
      if (!(v > 0) || !(r > 0)) return 1;
      return Math.max(1, Math.floor((v - h) / r));
    }
    // Pure: a page-shaped slice of the first `cap` rows with no pager
    // (pages 1) and `more` = the rows that did not fit.
    function stageTrackerDisplayCapPage(rows, cap) {
      const all = Array.isArray(rows) ? rows : [];
      const n = Math.max(1, Math.floor(Number(cap) || 1));
      const shown = all.slice(0, n);
      return { rows: shown, page: 1, pages: 1, from: shown.length ? 1 : 0, to: shown.length, total: all.length, size: n, more: all.length - shown.length };
    }
    // Pure: the "+N more" footer that replaces the pager on a TV display.
    function stageTrackerDisplayMoreHtml(more) {
      const n = Math.max(0, Math.floor(Number(more) || 0));
      return n ? `<span class="ham-display-more" role="status">+${n} more</span>` : '';
    }
    // Pure: the row-motion effect for a render — none at all on a display
    // under prefers-reduced-motion, unchanged everywhere else.
    function stageTrackerDisplayMotionEffect(effect, display, reduced) {
      return display && reduced ? '' : effect;
    }
    // tracker display layout helpers end

    function stageTrackerDisplayTv() {
      return stageTrackerDisplayMode() && window.HIVE_DISPLAY_DENSITY === STAGE_TRACKER_DISPLAY_TV;
    }
    function stageTrackerDisplayEffect(effect) {
      const reduced = typeof stageTrackerReducedMotion === 'function' && stageTrackerReducedMotion();
      return stageTrackerDisplayMotionEffect(effect, stageTrackerDisplayMode(), reduced);
    }

    // -- TV density: rows capped to the viewport ---------------------------
    var _stageTrackerDisplayRowPx = 0;
    var _stageTrackerDisplayCap = 0;
    function stageTrackerDisplayTvCap() {
      const panel = document.getElementById('activity-monitor-panel');
      const cols = panel && panel.querySelector('.ham-cols');
      const top = panel ? panel.getBoundingClientRect().top + (window.scrollY || 0) : 0;
      const head = cols ? cols.getBoundingClientRect().height : 0;
      _stageTrackerDisplayCap = stageTrackerDisplayRowCap(window.innerHeight, top + head + STAGE_TRACKER_DISPLAY_TV_FOOT_PX, _stageTrackerDisplayRowPx || STAGE_TRACKER_DISPLAY_TV_ROW_PX);
      return _stageTrackerDisplayCap;
    }
    // After a TV render: measure the real row pitch and render again once if
    // the estimate fitted a different number of rows.
    function stageTrackerDisplayMeasureRows(el) {
      if (!el || !stageTrackerDisplayTv()) return;
      const rows = el.querySelectorAll('.ham-row');
      if (!rows.length) return;
      const a = rows[0].getBoundingClientRect();
      const px = rows.length > 1 ? rows[1].getBoundingClientRect().top - a.top : a.height;
      if (!(px > 0) || Math.abs(px - _stageTrackerDisplayRowPx) < 1) return;
      _stageTrackerDisplayRowPx = px;
      const before = _stageTrackerDisplayCap;
      if (stageTrackerDisplayTvCap() !== before) requestAnimationFrame(function() { renderStageTracker(); });
    }
    var _stageTrackerDisplayResizeTimer = null;
    window.addEventListener('resize', function() {
      if (!stageTrackerDisplayTv()) return;
      clearTimeout(_stageTrackerDisplayResizeTimer);
      _stageTrackerDisplayResizeTimer = setTimeout(function() { renderStageTracker(); }, 150);
    });

    // -- Liveness: stale dimming, header and self-recovery ------------------
    const STAGE_TRACKER_DISPLAY_TICK_MS = 1000;
    const STAGE_TRACKER_DISPLAY_RELOAD_KEY = 'hive-display-reload-attempt';
    var _stageTrackerDisplayLoadedAtMs = Date.now();
    var _stageTrackerDisplayFails = 0;
    var _stageTrackerDisplayReloadTimer = null;
    var _stageTrackerDisplayAttemptCleared = false;
    var _stageTrackerDisplayRollReloaded = false;
    function stageTrackerDisplaySessionGet(key) {
      try { return sessionStorage.getItem(key) || ''; } catch { return ''; }
    }
    function stageTrackerDisplaySessionSet(key, value) {
      try { if (value === null) sessionStorage.removeItem(key); else sessionStorage.setItem(key, value); } catch {}
    }
    // sse-bootstrap.js: the stream's onerror. Consecutive failures past the
    // threshold schedule one reload; the URL (and so the view) survives it.
    function stageTrackerDisplayNoteReconnectFailed() {
      if (!stageTrackerDisplayMode()) return;
      _stageTrackerDisplayFails++;
      if (_stageTrackerDisplayReloadTimer) return;
      const attempt = Number(stageTrackerDisplaySessionGet(STAGE_TRACKER_DISPLAY_RELOAD_KEY)) || 0;
      const delay = stageTrackerDisplayReloadDelayMs(_stageTrackerDisplayFails, attempt);
      if (!delay) return;
      _stageTrackerDisplayReloadTimer = setTimeout(function() {
        stageTrackerDisplaySessionSet(STAGE_TRACKER_DISPLAY_RELOAD_KEY, String(attempt + 1));
        window.location.reload();
      }, delay);
    }
    // sse-bootstrap.js: any frame. A live stream cancels a pending reload
    // and resets the backoff.
    function stageTrackerDisplayNoteFrame() {
      if (!_stageTrackerDisplayFails && !_stageTrackerDisplayReloadTimer && _stageTrackerDisplayAttemptCleared) return;
      _stageTrackerDisplayFails = 0;
      if (_stageTrackerDisplayReloadTimer) { clearTimeout(_stageTrackerDisplayReloadTimer); _stageTrackerDisplayReloadTimer = null; }
      if (!stageTrackerDisplayMode()) return;
      _stageTrackerDisplayAttemptCleared = true;
      stageTrackerDisplaySessionSet(STAGE_TRACKER_DISPLAY_RELOAD_KEY, null);
    }
    function stageTrackerDisplayMountHead() {
      const body = document.querySelector('#activity-monitor-section .section-body');
      if (!body || document.getElementById('activity-monitor-display-head')) return;
      const head = document.createElement('div');
      head.className = 'ham-display-head';
      head.id = 'activity-monitor-display-head';
      head.innerHTML = '<span class="ham-display-dot" aria-hidden="true">●</span><span class="ham-display-hive"></span><span class="ham-display-clock"></span>';
      const banner = document.createElement('div');
      banner.className = 'ham-display-stale';
      banner.id = 'activity-monitor-display-stale';
      banner.setAttribute('role', 'status');
      banner.setAttribute('aria-live', 'polite');
      banner.hidden = true;
      body.insertBefore(banner, body.firstChild);
      body.insertBefore(head, banner);
    }
    function stageTrackerDisplayTick(nowMs) {
      if (!stageTrackerDisplayMode()) return;
      const now = typeof nowMs === 'number' ? nowMs : Date.now();
      const d = typeof _stageTracker !== 'undefined' ? _stageTracker : null;
      const genAt = Date.parse((d && d.generated_at) || '') || 0;
      const ttlMs = (Number(d && d.cache_ttl_s) || 0) * 1000;
      // Until the first frame the stream is judged from page load, so a
      // display that never connects still goes stale.
      const sseAt = (typeof _sseEverFrameAtMs === 'number' && _sseEverFrameAtMs) || _stageTrackerDisplayLoadedAtMs;
      const st = stageTrackerDisplayStale(now, sseAt, STAGE_TRACKER_POLL_MS, genAt, ttlMs);
      document.body.classList.toggle('display-stale', st.stale);
      const banner = document.getElementById('activity-monitor-display-stale');
      if (banner) {
        const text = stageTrackerDisplayStaleText(st, now);
        if (banner.textContent !== text) banner.textContent = text;
        banner.hidden = !st.stale;
      }
      const head = document.getElementById('activity-monitor-display-head');
      if (head) {
        const hive = head.querySelector('.ham-display-hive');
        const id = typeof _stageTrackerPopoutHiveId === 'string' ? _stageTrackerPopoutHiveId : '';
        if (hive && hive.textContent !== id) hive.textContent = id;
        const clock = head.querySelector('.ham-display-clock');
        const time = typeof formatDashboardTime === 'function'
          ? formatDashboardTime(now, { hour: '2-digit', minute: '2-digit' }, '')
          : '';
        if (clock && clock.textContent !== time) clock.textContent = time;
        const dot = head.querySelector('.ham-display-dot');
        if (dot) dot.title = st.stale ? stageTrackerDisplayStaleText(st, now) : 'Live';
      }
      const roll = typeof versionReadUpgradeProgress === 'function' ? versionReadUpgradeProgress(now) : null;
      if (!_stageTrackerDisplayRollReloaded && stageTrackerDisplayRollReload(roll, _stageTrackerDisplayLoadedAtMs)) {
        _stageTrackerDisplayRollReloaded = true;
        window.location.reload();
      }
    }
    document.addEventListener('DOMContentLoaded', function() {
      if (!stageTrackerDisplayMode()) return;
      stageTrackerDisplayMountHead();
      stageTrackerDisplayTick();
      dashboardSetInterval('display-liveness', stageTrackerDisplayTick, STAGE_TRACKER_DISPLAY_TICK_MS);
    });
