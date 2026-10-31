    /* ---- Pop out & full screen -------------------------------------------
       Two header buttons next to the settings gear (hivecommons/hive#15829)
       for an operator who wants the activity monitor on another monitor
       while they do other work. ⧉ opens the SPA in a named popup window on
       `?popout=1#section-activity-monitor-section`; boot.js reads the flag
       before first paint and css/popout.css hides every piece of chrome and
       every other section, so the new window is the tracker and nothing
       else. The bearer token is in localStorage, so the popup is already
       signed in. ⛶ toggles the Fullscreen API on the section itself (and
       `f` does the same while focus is inside the tracker); Esc exits
       natively and the label flips through the fullscreenchange event.
       Nothing is persisted: the only state is the URL of the popup. */
    const STAGE_TRACKER_POPOUT_PARAM = 'popout';
    const STAGE_TRACKER_POPOUT_WINDOW_NAME = 'hive-activity-monitor';
    const STAGE_TRACKER_POPOUT_FEATURES = 'popup,width=1600,height=900';
    const STAGE_TRACKER_POPOUT_HASH = '#section-activity-monitor-section';
    const STAGE_TRACKER_POPOUT_TITLE = 'Hive · Activity Monitor';
    const STAGE_TRACKER_FULLSCREEN_KEY = 'f';
    const STAGE_TRACKER_FULLSCREEN_LABELS = {
      enter: { text: '⛶', title: 'Full screen — fill the screen with the activity monitor (f, Esc exits)' },
      exit: { text: '⛶ Exit full screen', title: 'Exit full screen (Esc or f)' },
    };
    var _stageTrackerPopoutHiveId = '';

    // Pure: the pop-out URL for the window the operator is on. Keeps the
    // origin, path and any other query (theme, style…), drops kiosk so the
    // attended layout wins, and lands on the activity monitor section.
    function stageTrackerPopoutUrl(href) {
      let url;
      try { url = new URL(href); } catch { return ''; }
      url.searchParams.set(STAGE_TRACKER_POPOUT_PARAM, '1');
      url.searchParams.delete('kiosk');
      url.searchParams.delete('filter');
      url.hash = STAGE_TRACKER_POPOUT_HASH;
      return url.toString();
    }
    // Pure: the main-dashboard URL a pop-out falls back to when the browser
    // refuses to close it (a tab the operator opened directly, not a popup).
    function stageTrackerDashboardUrl(href) {
      let url;
      try { url = new URL(href); } catch { return ''; }
      url.searchParams.delete(STAGE_TRACKER_POPOUT_PARAM);
      url.hash = STAGE_TRACKER_POPOUT_HASH;
      return url.toString();
    }
    // Pure: the pop-out window title; the hive id joins once /api/status
    // has delivered it.
    function stageTrackerPopoutTitle(hiveId) {
      const id = String(hiveId || '').trim();
      return id ? STAGE_TRACKER_POPOUT_TITLE + ' · ' + id : STAGE_TRACKER_POPOUT_TITLE;
    }
    function stageTrackerPopoutMode() {
      return !!(window.hivePopoutMode && window.hivePopoutMode());
    }
    function stageTrackerPopoutSyncTitle(hiveId) {
      if (hiveId !== undefined) _stageTrackerPopoutHiveId = hiveId || '';
      if (!stageTrackerPopoutMode()) return;
      document.title = stageTrackerPopoutTitle(_stageTrackerPopoutHiveId);
    }
    function openStageTrackerPopout() {
      const url = stageTrackerPopoutUrl(window.location.href);
      if (!url) return null;
      let win = null;
      try { win = window.open(url, STAGE_TRACKER_POPOUT_WINDOW_NAME, STAGE_TRACKER_POPOUT_FEATURES); } catch { win = null; }
      if (!win) {
        if (typeof showToast === 'function') showToast('The browser blocked the pop-out window — allow pop-ups for this site and try again.', 'error');
        return null;
      }
      try { win.focus(); } catch {}
      return win;
    }
    // ⤶ Return to dashboard: bring the opener forward and close this
    // pop-out. A window the operator navigated to by hand cannot be
    // closed by script, so it navigates back to the full dashboard instead.
    function stageTrackerReturnToDashboard() {
      try { if (window.opener && !window.opener.closed) window.opener.focus(); } catch {}
      try { window.close(); } catch {}
      if (!window.closed) {
        const url = stageTrackerDashboardUrl(window.location.href);
        if (url) window.location.assign(url);
      }
    }

    // The whole document goes full screen (not the section) so the
    // body-level toasts — waiting on you, merge celebrations — stay
    // visible; body.ham-fullscreen then applies the pop-out layout.
    function stageTrackerFullscreenTarget() {
      return document.documentElement;
    }
    function stageTrackerFullscreenButton() {
      return document.getElementById('activity-monitor-fullscreen');
    }
    function stageTrackerIsFullscreen() {
      return !!(document.fullscreenElement && document.fullscreenElement === stageTrackerFullscreenTarget()) && document.body.classList.contains('ham-fullscreen');
    }
    // body.ham-fullscreen marks browser full screen; body.ham-chromeless is
    // the shared chrome-less layout (css/popout.css). Leaving full screen
    // keeps the layout when the URL already asks for it (pop-out, display).
    function stageTrackerSetFullscreenLayout(on) {
      const cl = document.body.classList;
      cl.toggle('ham-fullscreen', !!on);
      cl.toggle('ham-chromeless', !!on || cl.contains('popout-mode') || cl.contains('display-mode'));
    }
    // Pure: label + title for the ⛶ button given the current state.
    function stageTrackerFullscreenLabel(isFull) {
      return isFull ? STAGE_TRACKER_FULLSCREEN_LABELS.exit : STAGE_TRACKER_FULLSCREEN_LABELS.enter;
    }
    function stageTrackerRenderFullscreenButton() {
      // Esc (or the browser) leaving full screen must drop the layout too.
      if (!document.fullscreenElement) stageTrackerSetFullscreenLayout(false);
      const full = stageTrackerIsFullscreen();
      const btn = stageTrackerFullscreenButton();
      if (!btn) return;
      const label = stageTrackerFullscreenLabel(full);
      btn.textContent = label.text;
      btn.title = label.title;
      btn.setAttribute('aria-label', label.title);
      btn.setAttribute('aria-pressed', full ? 'true' : 'false');
    }
    function toggleStageTrackerFullscreen() {
      const target = stageTrackerFullscreenTarget();
      if (!target) return Promise.resolve(false);
      let p;
      if (stageTrackerIsFullscreen()) {
        stageTrackerSetFullscreenLayout(false);
        p = document.exitFullscreen ? document.exitFullscreen() : Promise.resolve();
      } else if (target.requestFullscreen) {
        stageTrackerSetFullscreenLayout(true);
        p = target.requestFullscreen();
      } else {
        if (typeof showToast === 'function') showToast('This browser does not support full screen here.', 'error');
        return Promise.resolve(false);
      }
      return Promise.resolve(p).then(() => { stageTrackerRenderFullscreenButton(); return true; }, (err) => {
        stageTrackerSetFullscreenLayout(false);
        stageTrackerRenderFullscreenButton();
        if (typeof showToast === 'function') showToast('Full screen was refused: ' + ((err && err.message) || err), 'error');
        return false;
      });
    }
    // Pure: should this keydown toggle full screen? Only a bare `f` with
    // focus inside the tracker (or anywhere, in a pop-out — the whole page
    // is the tracker), never while typing in a field.
    function stageTrackerFullscreenHotkey(ev, inTracker, typing, popout) {
      if (!ev || ev.key !== STAGE_TRACKER_FULLSCREEN_KEY) return false;
      if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.shiftKey) return false;
      if (typing) return false;
      return !!(inTracker || popout);
    }
    document.addEventListener('keydown', function(e) {
      const t = e.target;
      const tag = ((t && t.tagName) || '').toLowerCase();
      const typing = tag === 'input' || tag === 'textarea' || tag === 'select' || !!(t && t.isContentEditable);
      const section = document.getElementById('activity-monitor-section');
      const inTracker = !!(section && t && t.nodeType === 1 && section.contains(t)) || stageTrackerIsFullscreen();
      if (!stageTrackerFullscreenHotkey(e, inTracker, typing, stageTrackerPopoutMode())) return;
      const overlay = document.getElementById('activity-monitor-settings-overlay');
      if (overlay && !overlay.classList.contains('hidden')) return;
      e.preventDefault();
      toggleStageTrackerFullscreen();
    });
    document.addEventListener('fullscreenchange', stageTrackerRenderFullscreenButton);

    /* ---- Display mode (hivecommons/hive#15925) ----------------------------
       📺 navigates to the current URL with `display=1`; boot.js sets
       body.display-mode before first paint and css/popout.css gives it the
       pop-out layout minus every row action. It keeps all other query
       params (am.* views, theme…). Esc (outside browser full screen) or `d`
       leaves by dropping the flag. Entering never requests full screen or a
       wake lock, and nothing is persisted: the mode lives in the URL. */
    const STAGE_TRACKER_DISPLAY_PARAM = 'display';
    const STAGE_TRACKER_DISPLAY_KEY = 'd';
    const STAGE_TRACKER_DISPLAY_LABELS = {
      enter: { text: '📺', title: 'Display — show the activity monitor as a wall view with no row actions (d; Esc or d leaves)' },
      exit: { text: '📺 Exit display', title: 'Leave display mode (Esc or d)' },
    };
    function stageTrackerDisplayMode() {
      return !!(window.hiveDisplayMode && window.hiveDisplayMode());
    }
    // Pure: href with display mode switched on or off. Entering drops kiosk
    // (it would win) and the one-shot filter, and lands on the tracker.
    function stageTrackerDisplayUrl(href, on) {
      let url;
      try { url = new URL(href); } catch { return ''; }
      if (on) {
        url.searchParams.set(STAGE_TRACKER_DISPLAY_PARAM, '1');
        url.searchParams.delete('kiosk');
        url.searchParams.delete('filter');
        url.hash = STAGE_TRACKER_POPOUT_HASH;
      } else {
        url.searchParams.delete(STAGE_TRACKER_DISPLAY_PARAM);
      }
      return url.toString();
    }
    // Pure: should this keydown toggle display mode? A bare `d` with focus in
    // the tracker (anywhere while displaying — the page is the tracker), or
    // Esc while displaying and not in browser full screen, which consumes
    // Esc itself. Never while typing.
    function stageTrackerDisplayHotkey(ev, inTracker, typing, display, fullscreen) {
      if (!ev || typing) return false;
      if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.shiftKey) return false;
      if (ev.key === 'Escape') return !!display && !fullscreen;
      if (ev.key !== STAGE_TRACKER_DISPLAY_KEY) return false;
      return !!(inTracker || display);
    }
    // Pure: is a popover or dialog open that Esc should close instead of
    // leaving display mode? Leaving reloads the page and drops session-only
    // am.* edits, so any open overlay wins — the tracker's own settings,
    // labels, merged and queue popovers, and every hive dialog — whatever
    // order their Esc handlers run in.
    const STAGE_TRACKER_DISPLAY_OVERLAY_SELECTOR = '#activity-monitor-settings-overlay, #activity-monitor-labels-overlay, #activity-monitor-merged-pop, #activity-monitor-queue-pop, .hive-dialog-overlay';
    function stageTrackerDisplayOverlayOpen(doc) {
      if (!doc || typeof doc.querySelectorAll !== 'function') return false;
      const els = doc.querySelectorAll(STAGE_TRACKER_DISPLAY_OVERLAY_SELECTOR);
      for (let i = 0; i < els.length; i++) {
        const el = els[i];
        if (el.hidden) continue;
        if (el.classList && el.classList.contains('hidden')) continue;
        if (el.style && el.style.display === 'none') continue;
        return true;
      }
      return false;
    }
    function toggleStageTrackerDisplay() {
      const url = stageTrackerDisplayUrl(window.location.href, !stageTrackerDisplayMode());
      if (url) window.location.assign(url);
      return url;
    }
    function stageTrackerRenderDisplayButton() {
      const btn = document.getElementById('activity-monitor-display');
      if (!btn) return;
      const on = stageTrackerDisplayMode();
      const label = on ? STAGE_TRACKER_DISPLAY_LABELS.exit : STAGE_TRACKER_DISPLAY_LABELS.enter;
      btn.textContent = label.text;
      btn.title = label.title;
      btn.setAttribute('aria-label', label.title);
      btn.setAttribute('aria-pressed', on ? 'true' : 'false');
    }
    // Pure: in display mode, must this event be kept from the row action
    // handlers — the context menu, Shift+F10 / ContextMenu key, and Space or
    // modifier-click selection? Presentation only; the server still
    // authorizes every action.
    function stageTrackerDisplayBlocksRowAction(ev, display) {
      if (!display || !ev) return false;
      if (ev.type === 'contextmenu') return true;
      if (ev.type === 'keydown') return ev.key === 'ContextMenu' || ev.key === ' ' || (!!ev.shiftKey && ev.key === 'F10');
      if (ev.type === 'click') return !!(ev.metaKey || ev.ctrlKey || ev.shiftKey);
      return false;
    }
    ['contextmenu', 'keydown', 'click'].forEach(function(type) {
      document.addEventListener(type, function(e) {
        if (!stageTrackerDisplayBlocksRowAction(e, stageTrackerDisplayMode())) return;
        const t = e.target;
        if (!(t && t.closest && t.closest('#activity-monitor-section .ham-row, #activity-monitor-section [data-ham-sub-key]'))) return;
        e.stopPropagation();
      }, true);
    });
    document.addEventListener('keydown', function(e) {
      if (e.defaultPrevented) return;
      const t = e.target;
      const tag = ((t && t.tagName) || '').toLowerCase();
      const typing = tag === 'input' || tag === 'textarea' || tag === 'select' || !!(t && t.isContentEditable);
      const section = document.getElementById('activity-monitor-section');
      const inTracker = !!(section && t && t.nodeType === 1 && section.contains(t));
      if (!stageTrackerDisplayHotkey(e, inTracker, typing, stageTrackerDisplayMode(), !!document.fullscreenElement)) return;
      if (stageTrackerDisplayOverlayOpen(document)) return;
      e.preventDefault();
      // Leaving goes through stageTrackerDisplayExit, like the overlay's
      // Exit display control, so a held wake lock is released the same way.
      if (stageTrackerDisplayMode()) stageTrackerDisplayExit(); else toggleStageTrackerDisplay();
    });

    /* ---- Display controls (hivecommons/hive#15929) ------------------------
       In display mode only, a small overlay (STAGE_TRACKER_DISPLAY_CONTROLS_HTML
       in sections.js) appears on pointer movement or keyboard focus and hides
       after 3 s idle. Each control is separate and explicit: Exit display,
       Copy display link, Full screen (the existing
       toggleStageTrackerFullscreen) and Keep screen awake. Full screen and
       the wake lock are requested only from a click on their own control —
       never on load or on entering display mode. Nothing is persisted: the
       wake lock lives in memory and a reload is a new consent boundary. */
    const STAGE_TRACKER_DISPLAY_IDLE_MS = 3000;
    // Overlay titles that the header buttons also use live in the *_LABELS
    // objects above; the rest are here so the overlay has one source of text.
    const STAGE_TRACKER_DISPLAY_CONTROL_LABELS = {
      fullscreenEnter: 'Fill the screen with the display (f, Esc exits)',
      fullscreenExit: STAGE_TRACKER_FULLSCREEN_LABELS.exit.title,
      fullscreenUnsupported: 'Full screen is not supported in this browser',
      wakeUnsupported: { text: '☀ Keep awake', title: 'Keep screen awake is not supported in this browser' },
      wakeOn: { text: '☀ Awake: on', title: 'The screen is kept awake — click to let it sleep' },
      wakeDenied: { text: '☀ Awake: denied', title: 'The browser refused to keep the screen awake (%s) — click to ask again' },
      wakeOff: { text: '☀ Keep awake', title: 'Keep the screen awake while this display is open (not remembered after a reload)' },
    };
    // tracker display controls helpers begin
    // Pure: is the overlay showing, given the last pointer/focus activity?
    function stageTrackerDisplayOverlayVisible(lastPointerMs, nowMs, idleMs) {
      if (!(lastPointerMs > 0)) return false;
      const d = nowMs - lastPointerMs;
      return d >= 0 && d < idleMs;
    }
    // Pure: label and state of the Keep screen awake control — unsupported,
    // denied (with the reason; never retried automatically), on or off.
    function stageTrackerWakeLockState(supported, sentinel, lastError) {
      const L = STAGE_TRACKER_DISPLAY_CONTROL_LABELS;
      if (!supported) return { state: 'unsupported', text: L.wakeUnsupported.text, title: L.wakeUnsupported.title, disabled: true, pressed: false };
      if (sentinel && !sentinel.released) return { state: 'on', text: L.wakeOn.text, title: L.wakeOn.title, disabled: false, pressed: true };
      if (lastError) {
        const reason = String((lastError && (lastError.message || lastError.name)) || lastError);
        return { state: 'denied', text: L.wakeDenied.text, title: L.wakeDenied.title.replace('%s', reason), disabled: false, pressed: false };
      }
      return { state: 'off', text: L.wakeOff.text, title: L.wakeOff.title, disabled: false, pressed: false };
    }
    // Pure: can this document go full screen?
    function stageTrackerFullscreenSupported(doc) {
      return !!(doc && doc.fullscreenEnabled !== false && doc.documentElement && typeof doc.documentElement.requestFullscreen === 'function');
    }
    // Pure: the `am.*` query (no leading `?`) describing one view. A view
    // that is all defaults still names `am.group`, so it stays a valid
    // playlist entry on reload.
    function stageTrackerViewQuery(filter, group) {
      const view = stageTrackerViewUrl('https://hive.invalid/', filter, group);
      const out = new URLSearchParams();
      if (view) for (const [k, v] of new URL(view).searchParams) if (k.indexOf(STAGE_TRACKER_URL_PREFIX) === 0) out.append(k, v);
      if (!Array.from(out.keys()).length) out.set(STAGE_TRACKER_URL_PREFIX + 'group', STAGE_TRACKER_GROUPS.indexOf(group) >= 0 ? group : STAGE_TRACKER_GROUP_DEFAULT);
      return out.toString();
    }
    // Pure: a playlist interval in ms as the `interval` value (`45s`, `5m`).
    function stageTrackerPlaylistIntervalValue(ms) {
      const n = Number(ms) || STAGE_TRACKER_PLAYLIST_INTERVAL_DEFAULT_MS;
      return n % 60000 === 0 ? (n / 60000) + 'm' : Math.round(n / 1000) + 's';
    }
    // Pure: the link Copy display link puts on the clipboard. Only view
    // parameters ride along — display=1, am.* (or, given a parsed playlist,
    // one am.view per accepted view plus interval) and theme — so no token,
    // cookie or other query param can leak. Playlist views are re-serialised
    // from the parsed { filter, group }, never echoed from the current URL.
    function stageTrackerDisplayLinkUrl(href, filter, group, playlist) {
      let src;
      try { src = new URL(href); } catch { return ''; }
      const out = new URL(src.origin + src.pathname);
      out.searchParams.set(STAGE_TRACKER_DISPLAY_PARAM, '1');
      if (playlist && Array.isArray(playlist.views) && playlist.views.length) {
        for (const v of playlist.views) out.searchParams.append(STAGE_TRACKER_PLAYLIST_PARAM, stageTrackerViewQuery(v.filter, v.group));
        out.searchParams.set(STAGE_TRACKER_PLAYLIST_INTERVAL_PARAM, stageTrackerPlaylistIntervalValue(playlist.intervalMs));
      } else {
        const view = stageTrackerViewUrl(href, filter, group);
        if (!view) return '';
        for (const [k, v] of new URL(view).searchParams) if (k.indexOf(STAGE_TRACKER_URL_PREFIX) === 0) out.searchParams.append(k, v);
      }
      const theme = src.searchParams.get('theme');
      if (theme !== null) out.searchParams.set('theme', theme);
      out.hash = STAGE_TRACKER_POPOUT_HASH;
      return out.toString();
    }
    // tracker display controls helpers end
    var _stageTrackerDisplayLastPointer = 0;
    var _stageTrackerDisplayHideTimer = null;
    var _stageTrackerWakeLock = null;
    var _stageTrackerWakeLockError = null;
    var _stageTrackerWakeLockWanted = false;
    function stageTrackerDisplayControlsEl() {
      return document.getElementById('activity-monitor-display-controls');
    }
    function stageTrackerDisplayControlsSync(nowMs) {
      const root = stageTrackerDisplayControlsEl();
      if (!root) return false;
      const link = document.getElementById('activity-monitor-display-link');
      const on = stageTrackerDisplayOverlayVisible(_stageTrackerDisplayLastPointer, nowMs === undefined ? Date.now() : nowMs, STAGE_TRACKER_DISPLAY_IDLE_MS) || !!(link && !link.hidden);
      root.classList.toggle('is-visible', on);
      return on;
    }
    function stageTrackerDisplayControlsPing() {
      _stageTrackerDisplayLastPointer = Date.now();
      stageTrackerDisplayControlsSync();
      clearTimeout(_stageTrackerDisplayHideTimer);
      _stageTrackerDisplayHideTimer = setTimeout(function() { stageTrackerDisplayControlsSync(); }, STAGE_TRACKER_DISPLAY_IDLE_MS + 50);
    }
    function stageTrackerWakeLockSupported() {
      return typeof navigator !== 'undefined' && !!navigator.wakeLock && typeof navigator.wakeLock.request === 'function';
    }
    function stageTrackerRenderDisplayControls() {
      const root = stageTrackerDisplayControlsEl();
      if (!root) return;
      const fs = root.querySelector('[data-display-control="fullscreen"]');
      if (fs) {
        const ok = stageTrackerFullscreenSupported(document);
        const full = !!document.fullscreenElement;
        fs.disabled = !ok;
        const L = STAGE_TRACKER_DISPLAY_CONTROL_LABELS;
        fs.title = ok ? (full ? L.fullscreenExit : L.fullscreenEnter) : L.fullscreenUnsupported;
        fs.setAttribute('aria-pressed', full ? 'true' : 'false');
      }
      const wl = root.querySelector('[data-display-control="wakelock"]');
      if (wl) {
        const s = stageTrackerWakeLockState(stageTrackerWakeLockSupported(), _stageTrackerWakeLock, _stageTrackerWakeLockError);
        wl.textContent = s.text;
        wl.title = s.title;
        wl.disabled = s.disabled;
        wl.setAttribute('aria-pressed', s.pressed ? 'true' : 'false');
        wl.setAttribute('data-state', s.state);
      }
    }
    function stageTrackerWakeLockAcquire() {
      let p;
      try { p = navigator.wakeLock.request('screen'); } catch (e) { p = Promise.reject(e); }
      return Promise.resolve(p).then(function(s) {
        _stageTrackerWakeLock = s || null;
        _stageTrackerWakeLockError = null;
        if (s && typeof s.addEventListener === 'function') s.addEventListener('release', stageTrackerRenderDisplayControls);
        stageTrackerRenderDisplayControls();
        return true;
      }, function(err) {
        _stageTrackerWakeLock = null;
        _stageTrackerWakeLockError = err || new Error('refused');
        _stageTrackerWakeLockWanted = false;
        stageTrackerRenderDisplayControls();
        if (typeof showToast === 'function') showToast('Keep screen awake was refused: ' + ((err && (err.message || err.name)) || err || 'refused'), 'error');
        return false;
      });
    }
    function stageTrackerWakeLockRelease() {
      _stageTrackerWakeLockWanted = false;
      const s = _stageTrackerWakeLock;
      _stageTrackerWakeLock = null;
      let p;
      try { p = s && typeof s.release === 'function' ? s.release() : undefined; } catch {}
      return Promise.resolve(p).catch(function() {}).then(function() { stageTrackerRenderDisplayControls(); return false; });
    }
    // ☀ Keep screen awake: a click toggles the sentinel; nothing else
    // requests one except re-acquiring after the tab was hidden, and only
    // while the user still wants it on.
    function stageTrackerToggleWakeLock() {
      if (!stageTrackerWakeLockSupported()) { stageTrackerRenderDisplayControls(); return Promise.resolve(false); }
      if (_stageTrackerWakeLock && !_stageTrackerWakeLock.released) return stageTrackerWakeLockRelease();
      _stageTrackerWakeLockWanted = true;
      return stageTrackerWakeLockAcquire();
    }
    function stageTrackerWakeLockOnVisible() {
      stageTrackerRenderDisplayControls();
      if (document.visibilityState !== 'visible' || !_stageTrackerWakeLockWanted || !stageTrackerWakeLockSupported()) return Promise.resolve(false);
      if (_stageTrackerWakeLock && !_stageTrackerWakeLock.released) return Promise.resolve(true);
      return stageTrackerWakeLockAcquire();
    }
    function stageTrackerDisplayExit() {
      if (_stageTrackerWakeLock || _stageTrackerWakeLockWanted) stageTrackerWakeLockRelease();
      return toggleStageTrackerDisplay();
    }
    function stageTrackerDisplayShowLink(url) {
      const field = document.getElementById('activity-monitor-display-link');
      if (!field) return;
      field.value = url;
      field.hidden = false;
      stageTrackerDisplayControlsPing();
      try { field.focus(); field.select(); } catch {}
    }
    // 🔗 Copy display link: the clipboard when it works, otherwise the link
    // in a selectable field with a toast.
    function stageTrackerCopyDisplayLink() {
      const url = stageTrackerDisplayLinkUrl(window.location.href, _stageTrackerFilter, _stageTrackerGroup, _stageTrackerPlaylist);
      if (!url) return Promise.resolve(false);
      const fallback = function() {
        stageTrackerDisplayShowLink(url);
        if (typeof showToast === 'function') showToast('Could not copy to the clipboard — the display link is selected in the controls; copy it from there.', 'error');
        return false;
      };
      const clip = typeof navigator !== 'undefined' ? navigator.clipboard : null;
      if (!clip || typeof clip.writeText !== 'function') return Promise.resolve(fallback());
      let p;
      try { p = clip.writeText(url); } catch { return Promise.resolve(fallback()); }
      return Promise.resolve(p).then(function() {
        if (typeof showToast === 'function') showToast('Display link copied.', 'success');
        return true;
      }, fallback);
    }
    function stageTrackerDisplayControlsInit() {
      if (!stageTrackerDisplayMode()) return null;
      let root = stageTrackerDisplayControlsEl();
      if (root) return root;
      root = document.createElement('div');
      root.id = 'activity-monitor-display-controls';
      root.className = 'ham-display-controls';
      root.setAttribute('role', 'toolbar');
      root.setAttribute('aria-label', 'Display controls');
      root.innerHTML = STAGE_TRACKER_DISPLAY_CONTROLS_HTML;
      document.body.appendChild(root);
      ['mousemove', 'pointerdown', 'focusin'].forEach(function(type) { document.addEventListener(type, stageTrackerDisplayControlsPing, true); });
      const link = document.getElementById('activity-monitor-display-link');
      if (link) link.addEventListener('blur', function() { link.hidden = true; stageTrackerDisplayControlsSync(); });
      document.addEventListener('visibilitychange', stageTrackerWakeLockOnVisible);
      document.addEventListener('fullscreenchange', stageTrackerRenderDisplayControls);
      stageTrackerRenderDisplayControls();
      return root;
    }

    /* ---- Display playlists (hivecommons/hive#15932) -----------------------
       With two or more valid `am.view` params, display mode rotates the
       in-memory tracker view (see the playlist helpers in
       tracker-filter-state.js). The view index comes from wall-clock time,
       so a reload or SSE reconnect resumes the right one; rotation never
       navigates, never refetches and never writes localStorage. A thin bar
       shows the time left — a static "next in Ns" label under
       prefers-reduced-motion. Kiosk is untouched: display mode is off there. */
    const STAGE_TRACKER_PLAYLIST_TICK_MS = 1000;
    var _stageTrackerPlaylist = null;
    var _stageTrackerPlaylistIndex = -1;
    function stageTrackerPlaylistApply(i) {
      const v = _stageTrackerPlaylist && _stageTrackerPlaylist.views[i];
      if (!v) return;
      _stageTrackerPlaylistIndex = i;
      // The URL view is what a first fetch (or a refetch after a reconnect)
      // reloads the filter from, so it follows the rotation too.
      const view = stageTrackerUrlView();
      if (view) { view.filter = Object.assign({}, v.filter); view.group = v.group; }
      _stageTrackerFilter = stageTrackerNormalizeFilter(v.filter);
      _stageTrackerFilter.page = 1;
      _stageTrackerGroup = v.group;
      if (_stageTracker) renderStageTracker();
    }
    function stageTrackerPlaylistRenderProgress(nowMs) {
      const el = document.getElementById('activity-monitor-playlist');
      const pl = _stageTrackerPlaylist;
      if (!el || !pl) return;
      const rem = stageTrackerPlaylistRemainingMs(0, nowMs, pl.intervalMs);
      const bar = el.querySelector('.ham-playlist-bar');
      if (bar) bar.style.setProperty('--ham-playlist-left', (rem / pl.intervalMs).toFixed(4));
      const next = el.querySelector('.ham-playlist-next');
      if (next) next.textContent = (_stageTrackerPlaylistIndex + 1) + '/' + pl.views.length + ' · ' + stageTrackerPlaylistNextLabel(rem);
    }
    function stageTrackerPlaylistTick(nowMs) {
      const pl = _stageTrackerPlaylist;
      if (!pl) return -1;
      const now = nowMs === undefined ? Date.now() : nowMs;
      const i = stageTrackerPlaylistIndex(0, now, pl.intervalMs, pl.views.length);
      if (i !== _stageTrackerPlaylistIndex) stageTrackerPlaylistApply(i);
      stageTrackerPlaylistRenderProgress(now);
      return i;
    }
    function stageTrackerPlaylistInit() {
      if (!stageTrackerDisplayMode()) return null;
      const pl = stageTrackerParsePlaylist(window.location.href);
      if (pl.views.length < 2) return null;
      _stageTrackerPlaylist = pl;
      _stageTrackerPlaylistIndex = -1;
      const el = document.createElement('div');
      el.id = 'activity-monitor-playlist';
      el.className = 'ham-playlist';
      el.setAttribute('aria-hidden', 'true');
      el.innerHTML = '<div class="ham-playlist-track"><span class="ham-playlist-bar"></span></div><span class="ham-playlist-next"></span>';
      document.body.appendChild(el);
      stageTrackerPlaylistTick();
      // The shared registry pauses the tick while the tab is hidden; the
      // index is wall-clock maths, so resuming lands on the right view.
      dashboardSetInterval('stage-tracker-playlist', stageTrackerPlaylistTick, STAGE_TRACKER_PLAYLIST_TICK_MS);
      return pl;
    }

    document.addEventListener('DOMContentLoaded', function() {
      stageTrackerRenderFullscreenButton();
      stageTrackerRenderDisplayButton();
      stageTrackerPopoutSyncTitle();
      stageTrackerDisplayControlsInit();
      stageTrackerPlaylistInit();
    });
