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
