    // Only explicitly wired navigation counts participate. Keep raw-number
    // baselines outside the DOM: filters/routes can redraw without live data.
    const NAV_COUNT_CUE_MS = 1400;
    const _navCountDeltas = new Map();
    function navCountDelta(badge, count, label, live = true) {
      if (!badge) return;
      let state = _navCountDeltas.get(badge.id);
      const replaced = state && state.badge !== badge;
      if (replaced) state.clear();
      if (!state || replaced) {
        state = { badge, count: null, timer: null, phase: false, delta: '', label: '' };
        state.clear = () => {
          if (state.timer !== null) clearTimeout(state.timer);
          state.timer = null;
          state.delta = '';
          badge.removeAttribute('data-count-delta');
          badge.removeAttribute('data-count-direction');
          badge.removeAttribute('data-count-phase');
          if (state.label) badge.setAttribute('aria-label', state.label);
        };
        _navCountDeltas.set(badge.id, state);
      }
      if (!Number.isSafeInteger(count) || count < 0) {
        if (label) state.label = label;
        state.clear();
        state.count = null;
        return;
      }
      state.label = label;
      const previous = state.count;
      state.count = count;
      if (previous === null || previous === count || replaced) {
        badge.setAttribute('aria-label', label + state.delta);
        return;
      }
      state.clear();
      // Re-rendering cached rows is not a new observation. A reconnect is
      // simply a net count change; never claim it was a merge/open event.
      if (!live || document.hidden) return;
      const delta = count - previous;
      state.delta = ', ' + (delta > 0 ? 'increased' : 'decreased') + ' by ' + Math.abs(delta);
      badge.setAttribute('aria-label', label + state.delta);
      badge.setAttribute('data-count-delta', (delta > 0 ? '↑ +' : '↓ ') + delta);
      badge.setAttribute('data-count-direction', delta > 0 ? 'up' : 'down');
      state.phase = !state.phase;
      badge.setAttribute('data-count-phase', state.phase ? 'a' : 'b');
      state.timer = setTimeout(state.clear, NAV_COUNT_CUE_MS);
    }

    function stageTrackerSidebarCountsUnknown() {
      for (const id of ['activity-monitor-sidebar-badge', 'activity-monitor-waiting-badge']) {
        navCountDelta(document.getElementById(id), null);
      }
    }
