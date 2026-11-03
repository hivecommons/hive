    /* ---- Waiting on you ---------------------------------------------------
       The stage tracker payload carries waiting_on_human (stage_tracker_waiting.go):
       every open item parked on the signed-in operator / maintainers, with a
       kind (needs-decision, ready-for-human-merge, approve-needed,
       returned-to-contributor-offline, other-human-wait) and who the wait
       is attributed to. Three surfaces read it so a human wait is never
       silent (hivecommons/hive#15638): a second, attention-coloured badge on
       the "Hive activity monitor" sidebar item, a "Waiting on you · N" pill
       in the topbar (hidden at 0) and a debounced toast when the count
       rises. All three land on the Activity Monitor with the Waiting on you
       filter (?filter=waiting-on-you deep-links to the same view). */
    const STAGE_TRACKER_WAITING_TOAST_GAP_MS = 10 * 60 * 1000;
    const STAGE_TRACKER_WAITING_TOOLTIP_MAX = 5;
    const STAGE_TRACKER_WAITING_TOAST_NUMBERS = 3;
    const STAGE_TRACKER_WAITING_TOAST_KEY = 'hive-waiting-on-you-toast-at';
    const STAGE_TRACKER_WAITING_FILTER_PARAM = 'waiting-on-you';
    const STAGE_TRACKER_WAIT_KIND_WORDS = {
      'needs-decision': 'needs your decision',
      'ready-for-human-merge': 'ready for you to merge',
      'approve-needed': 'needs your approval',
      'returned-to-contributor-offline': 'contributor offline — adopt or reroute',
      'other-human-wait': 'needs a person',
    };
    var _stageTrackerWaitingPrevCount = null;

    // The waiting_on_human block, rebuilt from the per-item flags when an
    // older spoke sends items without the header block.
    function stageTrackerWaitingOnYou(d) {
      const w = d && d.waiting_on_human;
      if (w && Array.isArray(w.items)) return { count: Number(w.count) || w.items.length, items: w.items };
      const items = (Array.isArray(d?.items) ? d.items : []).filter(it => it && it.waiting_on_you && !it.nested_under)
        .map(it => ({ repo: it.repo, number: it.number, type: it.kind, kind: it.wait_kind || 'other-human-wait', since: it.stage_since, who: 'operator' }));
      return { count: items.length, items };
    }

    function stageTrackerWaitKindWords(kind) { return STAGE_TRACKER_WAIT_KIND_WORDS[kind] || STAGE_TRACKER_WAIT_KIND_WORDS['other-human-wait']; }

    // Tooltip: "N waiting on you" then the five longest waits, one per line.
    function stageTrackerWaitingTitle(w, nowMs) {
      if (!w.count) return 'Nothing is waiting on you';
      const lines = w.items.slice(0, STAGE_TRACKER_WAITING_TOOLTIP_MAX).map(it => {
        const since = it.since ? Date.parse(it.since) : NaN;
        const ago = Number.isFinite(since) && nowMs ? ' · ' + stageTrackerActedAgo(nowMs - since) : '';
        const who = it.who && it.who !== 'operator' ? ' (@' + it.who + ')' : '';
        return it.repo.split('/').pop() + '#' + it.number + ' — ' + stageTrackerWaitKindWords(it.kind) + who + ago;
      });
      const more = w.count > lines.length ? '\n… and ' + (w.count - lines.length) + ' more' : '';
      return w.count + ' waiting on you — click to show\n' + lines.join('\n') + more;
    }

    // Sidebar badge + topbar pill. Called from renderStageTracker on every
    // payload; the pill hides at zero, the badge keeps data-zero.
    function stageTrackerRenderWaiting(d, nowMs) {
      const w = stageTrackerWaitingOnYou(d);
      const title = stageTrackerWaitingTitle(w, nowMs);
      const badge = document.getElementById('activity-monitor-waiting-badge');
      if (badge) {
        badge.textContent = String(w.count);
        badge.title = title;
        badge.setAttribute('aria-label', w.count + ' waiting on you');
        if (w.count) badge.removeAttribute('data-zero'); else badge.setAttribute('data-zero', '');
      }
      const pill = document.getElementById('oc-waiting-on-you');
      if (pill) {
        pill.hidden = !w.count;
        pill.title = title;
        pill.setAttribute('aria-label', w.count + ' waiting on you — show in the activity monitor');
        const label = pill.querySelector('.nav-chip__label');
        if (label) label.textContent = 'Waiting on you · ' + w.count;
      }
      const chip = document.getElementById('activity-monitor-waiting');
      if (chip) chip.textContent = w.count ? 'Waiting on you (' + w.count + ')' : 'Waiting on you';
      stageTrackerWaitingToast(w, nowMs);
    }

    // One toast per ten minutes at most, only when the count rose since the
    // previous payload, never in kiosk mode (a wall display cannot act).
    function stageTrackerWaitingToast(w, nowMs) {
      const prev = _stageTrackerWaitingPrevCount;
      _stageTrackerWaitingPrevCount = w.count;
      if (prev === null || w.count <= prev) return;
      if (typeof window.hiveKioskMode === 'function' && window.hiveKioskMode()) return;
      let last = 0;
      try { last = Number(localStorage.getItem(STAGE_TRACKER_WAITING_TOAST_KEY)) || 0; } catch {}
      if (nowMs - last < STAGE_TRACKER_WAITING_TOAST_GAP_MS) return;
      try { localStorage.setItem(STAGE_TRACKER_WAITING_TOAST_KEY, String(nowMs)); } catch {}
      if (typeof showToast !== 'function') return;
      showToast(stageTrackerWaitingToastText(w), 'warning', { actionLabel: 'Show', onAction: showWaitingOnYou, autoDismissMs: stageTrackerToastMs('info') });
    }

    function stageTrackerWaitingToastText(w) {
      const nums = w.items.slice(0, STAGE_TRACKER_WAITING_TOAST_NUMBERS).map(it => '#' + it.number).join(', ');
      const tail = w.count > STAGE_TRACKER_WAITING_TOAST_NUMBERS ? ' …' : '';
      return w.count + (w.count === 1 ? ' item needs' : ' items need') + ' your decision — ' + nums + tail;
    }

    // Land on the Activity Monitor with only the waiting-on-you rows.
    function showWaitingOnYou() {
      if (typeof ocNavigate === 'function') ocNavigate('activity-monitor-section');
      hamFilter('waitingOnYou', true);
    }

    // ?filter=waiting-on-you (from a notification link or the pill's URL)
    // applies the filter once, then drops the parameter so a reload does
    // not re-apply it over whatever the operator chose next.
    function stageTrackerApplyUrlFilter() {
      let url;
      try { url = new URL(window.location.href); } catch { return false; }
      if (url.searchParams.get('filter') !== STAGE_TRACKER_WAITING_FILTER_PARAM) return false;
      url.searchParams.delete('filter');
      url.hash = '#section-activity-monitor-section';
      try { history.replaceState(null, '', url.toString()); } catch {}
      stageTrackerLoadFilter();
      _stageTrackerFilter.waitingOnYou = true;
      stageTrackerSaveFilter();
      return true;
    }
