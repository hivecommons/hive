    // What each kick came to (hivecommons/hive#18916).
    //
    // GET /api/kick-outcomes carries, per agent, the latest kick as a one-line
    // summary ("opened PR #12"); GET /api/kick-outcomes?agent=<name> carries
    // the last ten kicks with their detail for owner and read-write sessions.
    // A read-only session gets the one-line summary only, and the server
    // enforces that: this file never decides who may see detail.
    //
    // The quoted text on a stood_down or refused kick is UNTRUSTED agent text.
    // It is redacted and capped on the server and escaped here (esc) before it
    // reaches markup; it is never put in an attribute, a handler or a link.
    // The fetch is paced by a timestamp, not a timer: no new setInterval.
    const KICK_OUTCOME_REFETCH_MS = 30000;
    const KICK_TRIGGER_LABELS = { dashboard: 'dashboard', governor: 'governor', workflow_dispatch: 'workflow hand-off', other: 'other' };
    const _kickOutcomes = { latest: {}, detail: {}, kicks: {}, open: {}, loadedAt: 0, loading: false };

    // "last kick: opened PR #12 6m ago".
    function kickOutcomeLastText(v) {
      if (!v || !v.summary) return '';
      const ago = typeof pillAge === 'function' && v.at ? pillAge(v.at) : '';
      return 'last kick: ' + String(v.summary) + (ago ? ' ' + ago : '');
    }

    function kickOutcomeTriggerText(d) {
      const base = KICK_TRIGGER_LABELS[d.trigger_kind] || 'other';
      return d.trigger_actor && d.trigger_kind === 'dashboard' ? base + ' (' + String(d.trigger_actor) + ')' : base;
    }

    // One row of the expanded history. Pure; the caller never trusts d.
    function kickOutcomeRowHtml(d) {
      const when = d.started_at && typeof formatDashboardDateTime === 'function' ? formatDashboardDateTime(d.started_at) : String(d.started_at || '');
      const quote = d.quote
        ? ' <span class="ko-quote-label">' + (d.kind === 'refused' ? 'reason' : 'agent said') + ':</span> <q class="ko-quote">' + esc(String(d.quote)) + '</q>'
        : '';
      const code = d.refusal_code ? ' <span class="ko-code">' + esc(String(d.refusal_code)) + '</span>' : '';
      return '<li class="kick-outcome-row" data-kind="' + esc(String(d.kind || '')) + '"><span class="ko-time">' + esc(when) + '</span> <span class="ko-trigger">' + esc(kickOutcomeTriggerText(d)) + '</span> <span class="ko-summary">' + esc(String(d.summary || '')) + '</span>' + code + quote + '</li>';
    }

    // The slot an agent's card or Activity Monitor row carries. Pure: reads the
    // cache only. Empty until the first load has an answer for this agent.
    function kickOutcomeSlotHtml(name) {
      const v = _kickOutcomes.latest[name];
      const line = kickOutcomeLastText(v);
      if (!line) return '';
      let html = '<span class="ko-last" data-kind="' + esc(String(v.kind || '')) + '">' + esc(line) + '</span>';
      if (_kickOutcomes.detail) {
        const open = !!_kickOutcomes.open[name];
        html += ' <button type="button" class="hv-btn btn-secondary btn-sm ko-toggle" data-action="toggleKickOutcomes" data-arg0="' + esc(name) + '" aria-expanded="' + (open ? 'true' : 'false') + '">' + (open ? 'hide kicks' : 'last 10 kicks') + '</button>';
        if (open) {
          const kicks = _kickOutcomes.kicks[name];
          html += kicks && kicks.length
            ? '<ul class="kick-outcome-list">' + kicks.map(kickOutcomeRowHtml).join('') + '</ul>'
            : '<div class="ko-empty">' + (kicks ? 'No kicks recorded.' : 'Loading…') + '</div>';
        }
      }
      return '<div class="kick-outcome-line" data-kick-outcome="' + esc(name) + '">' + html + '</div>';
    }

    function kickOutcomeRefreshSlots() {
      if (typeof document === 'undefined') return;
      document.querySelectorAll('[data-kick-outcome]').forEach(el => {
        const html = kickOutcomeSlotHtml(el.dataset.kickOutcome);
        if (html) el.outerHTML = html;
      });
    }

    async function kickOutcomesJson(path) {
      const r = await fetch(path, { cache: 'no-store' });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      return r.json();
    }

    // Loads every agent's latest kick, at most once per KICK_OUTCOME_REFETCH_MS.
    // Called from the renders that already run on each status push.
    async function loadKickOutcomes() {
      if (_kickOutcomes.loading || Date.now() - _kickOutcomes.loadedAt < KICK_OUTCOME_REFETCH_MS) return;
      _kickOutcomes.loading = true;
      try {
        const data = await kickOutcomesJson('/api/kick-outcomes');
        _kickOutcomes.latest = data && data.agents ? data.agents : {};
        _kickOutcomes.detail = !!(data && data.detail);
        _kickOutcomes.loadedAt = Date.now();
        kickOutcomeRefreshSlots();
      } catch (e) {
        _kickOutcomes.loadedAt = Date.now();
      } finally {
        _kickOutcomes.loading = false;
      }
    }

    // Expands or collapses an agent's last ten kicks, loading them on first open.
    async function toggleKickOutcomes(name) {
      _kickOutcomes.open[name] = !_kickOutcomes.open[name];
      kickOutcomeRefreshSlots();
      if (!_kickOutcomes.open[name]) return;
      try {
        const data = await kickOutcomesJson('/api/kick-outcomes?agent=' + encodeURIComponent(name));
        _kickOutcomes.kicks[name] = data && Array.isArray(data.kicks) ? data.kicks : [];
      } catch (e) {
        _kickOutcomes.kicks[name] = [];
      }
      kickOutcomeRefreshSlots();
    }
