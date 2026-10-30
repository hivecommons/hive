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
    // Pure: label + title for the ⛶ button given the current state.
    function stageTrackerFullscreenLabel(isFull) {
      return isFull ? STAGE_TRACKER_FULLSCREEN_LABELS.exit : STAGE_TRACKER_FULLSCREEN_LABELS.enter;
    }
    function stageTrackerRenderFullscreenButton() {
      // Esc (or the browser) leaving full screen must drop the layout too.
      if (!document.fullscreenElement) document.body.classList.remove('ham-fullscreen');
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
        document.body.classList.remove('ham-fullscreen');
        p = document.exitFullscreen ? document.exitFullscreen() : Promise.resolve();
      } else if (target.requestFullscreen) {
        document.body.classList.add('ham-fullscreen');
        p = target.requestFullscreen();
      } else {
        if (typeof showToast === 'function') showToast('This browser does not support full screen here.', 'error');
        return Promise.resolve(false);
      }
      return Promise.resolve(p).then(() => { stageTrackerRenderFullscreenButton(); return true; }, (err) => {
        document.body.classList.remove('ham-fullscreen');
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
    document.addEventListener('DOMContentLoaded', function() {
      stageTrackerRenderFullscreenButton();
      stageTrackerPopoutSyncTitle();
    });
