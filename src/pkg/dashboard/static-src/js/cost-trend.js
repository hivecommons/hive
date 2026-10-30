    // ── Cost spend-over-time (hourly / daily / weekly / monthly / custom) ──
    // The server keeps 30 days of CUMULATIVE all-time estimated-$ snapshots at
    // ~5-min resolution. We derive per-bucket SPEND on the client by taking the
    // delta of the cumulative value across each bucket boundary — no backend
    // change needed. Selected range persists across re-renders and page loads.
    //
    // Every selector value defines ONE window and ONE bucket size, and the bar
    // chart, "spend in selected range" and the run-rate are all derived from
    // that same window (costWindow()). Each non-custom range has a COUNT of
    // buckets — a default, adjusted with the −/+ stepper beside the chips and
    // remembered per range — and the window is the newest `count` buckets,
    // the current (partial) one included. Boundaries are in the browser's
    // local clock, matching the axis labels:
    //   hourly  — the current clock hour plus the count−1 before it (24 by
    //             default ≈ last 24h); one bar per hour, labelled by hour.
    //   daily   — the last `count` calendar days, today included (14 by
    //             default); one bar per day, labelled by weekday when there
    //             are 7 or fewer bars and by M/D beyond that.
    //   weekly  — the current Mon → Sun week plus the count−1 before it (5 by
    //             default); one bar per week, labelled by its Monday (M/D).
    //             The part of the current week still ahead draws empty.
    //   monthly — the current calendar month plus the count−1 before it (5 by
    //             default); one bar per month, labelled by month name.
    //   custom  — the picked bounds; hourly bars when the span is at most
    //             COST_CUSTOM_HOUR_BUCKET_MAX_MS, daily bars otherwise.
    // The run-rate is spend-in-window ÷ ELAPSED window length (never the part
    // of a calendar week/month still in the future), shown per hour for hourly
    // bars and per day for every wider bucket.
    //
    // Data horizon: the server keeps ~30 days of 5-minute snapshots plus a
    // per-day rollup (/api/cost daily_rollup, UTC days, kept ~2 years). A
    // bucket is filled from the snapshots where they reach and from the
    // rollup for days entirely before the first snapshot; a bucket neither
    // covers is drawn hatched and the summary says when history starts.
    const COST_HOUR_MS = 3600e3;
    const COST_DAY_MS = 24 * COST_HOUR_MS;
    const COST_WEEK_DAYS = 7;
    const COST_WEEK_MS = COST_WEEK_DAYS * COST_DAY_MS;
    // Nominal month length, used only for the "bucket/step" tooltip text.
    const COST_MONTH_NOMINAL_MS = 30 * COST_DAY_MS;
    const COST_CUSTOM_HOUR_BUCKET_MAX_MS = 2 * COST_DAY_MS;
    const COST_CUSTOM_MIN_SPAN_MS = 60e3;
    // Charts with this many bars or fewer label every bar; wider charts label
    // COST_AXIS_LABEL_SPARSE_COUNT evenly spaced bars (first and last included).
    const COST_AXIS_LABEL_EVERY_BAR_MAX = 7;
    const COST_AXIS_LABEL_SPARSE_COUNT = 5;
    const COST_WEEKDAY_LABELS = ['Su', 'M', 'Tu', 'W', 'Th', 'F', 'Sa'];
    const COST_MONTH_LABELS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
    const COST_RANGES = {
      // key: { label, bucket } — bucket 'hour' | 'day' | 'week' | 'month' |
      // 'auto' (custom: by span)
      hourly:  { label: 'hourly',  bucket: 'hour' },
      daily:   { label: 'daily',   bucket: 'day' },
      weekly:  { label: 'weekly',  bucket: 'week' },
      monthly: { label: 'monthly', bucket: 'month' },
      custom:  { label: 'custom',  bucket: 'auto' }, // bounds come from _costCustom
    };
    // Per-range bucket counts: default and the stepper's bounds/step, with
    // the unit shown next to the count ("14 d", "5 wk", …).
    const COST_RANGE_COUNT = {
      hourly:  { def: 24, min: 6, max: 72, step: 6, unit: 'h' },
      daily:   { def: 14, min: 7, max: 60, step: 7, unit: 'd' },
      weekly:  { def: 5,  min: 2, max: 26, step: 1, unit: 'wk' },
      monthly: { def: 5,  min: 2, max: 24, step: 1, unit: 'mo' },
    };
    const COST_RANGE_STORAGE_KEY = 'hiveCostRange';
    const COST_RANGE_COUNTS_STORAGE_KEY = 'hiveCostRangeCounts';
    let _costRange = (() => {
      try { const s = localStorage.getItem(COST_RANGE_STORAGE_KEY); if (s && COST_RANGES[s]) return s; } catch {}
      return 'daily';
    })();
    // costClampCount: snap a requested count onto the range's min..max grid.
    function costClampCount(key, n) {
      const c = COST_RANGE_COUNT[key];
      if (!c) return null;
      if (!Number.isFinite(n)) return c.def;
      const snapped = c.min + Math.round((n - c.min) / c.step) * c.step;
      return Math.min(c.max, Math.max(c.min, snapped));
    }
    // Chosen bucket count per range, persisted together under one key.
    let _costCounts = (() => {
      const out = {};
      let saved = {};
      try { saved = JSON.parse(localStorage.getItem(COST_RANGE_COUNTS_STORAGE_KEY) || '{}') || {}; } catch {}
      for (const k in COST_RANGE_COUNT) out[k] = costClampCount(k, Number(saved[k]));
      return out;
    })();
    function costRangeCount(key) {
      key = key || _costRange;
      const c = COST_RANGE_COUNT[key];
      if (!c) return null;
      return _costCounts[key] ?? c.def;
    }
    // costStepCount: move the active range's count by ±1 step (clamped) and
    // persist. Returns true when the count changed.
    function costStepCount(dir, key) {
      key = key || _costRange;
      const c = COST_RANGE_COUNT[key];
      if (!c) return false;
      const next = costClampCount(key, costRangeCount(key) + Math.sign(dir) * c.step);
      if (next === costRangeCount(key)) return false;
      _costCounts[key] = next;
      try { localStorage.setItem(COST_RANGE_COUNTS_STORAGE_KEY, JSON.stringify(_costCounts)); } catch {}
      return true;
    }
    // Custom range bounds (ms since epoch), persisted like the range key.
    let _costCustom = (() => {
      try {
        const s = JSON.parse(localStorage.getItem('hiveCostCustom') || 'null');
        if (s && typeof s.start === 'number' && typeof s.end === 'number' && s.end > s.start) return s;
      } catch {}
      const now = Date.now();
      return { start: now - COST_DAY_MS, end: now };
    })();

    function costStartOfHour(ms) { const d = new Date(ms); d.setMinutes(0, 0, 0); return d.getTime(); }
    function costStartOfDay(ms) { const d = new Date(ms); d.setHours(0, 0, 0, 0); return d.getTime(); }
    // Calendar-day stepping (setDate) so DST transitions keep bucket edges on
    // local midnight instead of drifting an hour.
    function costAddDays(ms, n) { const d = new Date(ms); d.setDate(d.getDate() + n); return d.getTime(); }
    // Calendar-month stepping from a 1st-of-month instant.
    function costAddMonths(ms, n) { const d = new Date(ms); return new Date(d.getFullYear(), d.getMonth() + n, 1).getTime(); }
    function costStartOfWeek(ms) {
      const today = costStartOfDay(ms);
      const sinceMonday = (new Date(today).getDay() + 6) % 7;
      return costAddDays(today, -sinceMonday);
    }
    function costStartOfMonth(ms) { const d = new Date(ms); return new Date(d.getFullYear(), d.getMonth(), 1).getTime(); }
    // costBucketNext: the boundary one bucket after t.
    function costBucketNext(t, bucket) {
      if (bucket === 'day') return costAddDays(t, 1);
      if (bucket === 'week') return costAddDays(t, COST_WEEK_DAYS);
      if (bucket === 'month') return costAddMonths(t, 1);
      return t + COST_HOUR_MS;
    }
    function costBucketNominalMs(bucket) {
      if (bucket === 'day') return COST_DAY_MS;
      if (bucket === 'week') return COST_WEEK_MS;
      if (bucket === 'month') return COST_MONTH_NOMINAL_MS;
      return COST_HOUR_MS;
    }

    // costBucketEdges: ascending boundaries [start, …, end] stepping one hour,
    // calendar day, week or month from start; the final bucket is clipped at end.
    function costBucketEdges(start, end, bucket) {
      const edges = [start];
      let t = start;
      while (t < end) {
        t = Math.min(costBucketNext(t, bucket), end);
        edges.push(t);
      }
      return edges;
    }

    // costWindow: the single source of truth for the active range —
    // {key, start, end, bucket, bucketMs, edges, count, elapsedMs}. The chart
    // buckets, the "spend in selected range" sum and the run-rate all read it.
    // `now` is injectable for tests.
    function costWindow(now) {
      now = typeof now === 'number' ? now : Date.now();
      const key = _costRange;
      const n = costRangeCount(key);
      let start, end, bucket;
      if (key === 'custom') {
        start = _costCustom.start;
        end = Math.max(_costCustom.end, start + COST_CUSTOM_MIN_SPAN_MS);
        bucket = (end - start) <= COST_CUSTOM_HOUR_BUCKET_MAX_MS ? 'hour' : 'day';
      } else if (key === 'hourly') {
        start = costStartOfHour(now) - (n - 1) * COST_HOUR_MS;
        end = now;
        bucket = 'hour';
      } else if (key === 'daily') {
        start = costAddDays(costStartOfDay(now), -(n - 1));
        end = now;
        bucket = 'day';
      } else if (key === 'weekly') {
        const thisWeek = costStartOfWeek(now);
        start = costAddDays(thisWeek, -(n - 1) * COST_WEEK_DAYS);
        end = costAddDays(thisWeek, COST_WEEK_DAYS);
        bucket = 'week';
      } else { // monthly
        const thisMonth = costStartOfMonth(now);
        start = costAddMonths(thisMonth, -(n - 1));
        end = costAddMonths(thisMonth, 1);
        bucket = 'month';
      }
      const edges = costBucketEdges(start, end, bucket);
      return {
        key, start, end, bucket,
        bucketMs: costBucketNominalMs(bucket),
        edges,
        count: edges.length - 1,
        elapsedMs: Math.max(0, Math.min(now, end) - start),
      };
    }

    function fmtCostDateTime(ms) {
      return formatDashboardDateTime(ms, { month:'short', day:'numeric', hour:'numeric', minute:'2-digit', timeZoneName:'short' });
    }
    // Stale-recording notice (#13485): plain text, escaped by the caller.
    // Empty unless /api/cost reports the running agents' models recorded
    // nothing for longer than the stale window.
    function costRecordingNoticeText(recording) {
      if (!recording || !recording.stale) return '';
      const models = (recording.agent_models || []).slice(0, 4);
      const more = (recording.agent_models || []).length > models.length ? ', …' : '';
      const sinceMs = Number(recording.last_agent_model_growth_at) || Number(recording.last_priced_growth_at) || 0;
      const since = sinceMs > 0 ? `since ${fmtCostDateTime(sinceMs)}` : 'in the recorded history';
      const recent = recording.recent_models || [];
      const reflect = recent.length
        ? ` The figures below only reflect ${recent.join(', ')}.`
        : ' No model recorded any usage recently.';
      return `⚠ No usage recorded from the agents' configured models (${models.join(', ')}${more}) ${since}.${reflect} Live cost recording is likely broken — see`;
    }
    function fmtCostDuration(ms) {
      const min = Math.round(ms / 60000);
      if (min < 60) return `${Math.max(1, min)} min`;
      const hr = ms / 3600e3;
      if (hr < 48) return `${Number.isInteger(hr) ? hr : hr.toFixed(1)} hr`;
      const day = ms / 86400e3;
      return `${Number.isInteger(day) ? day : day.toFixed(1)} day`;
    }
    function costRangeTitle(start, end, bucketMs) {
      if (!start || !end) return 'time range unavailable';
      const bucket = bucketMs ? `; bucket/step ${fmtCostDuration(bucketMs)}` : '';
      return `${fmtCostDateTime(start)} → ${fmtCostDateTime(end)}${bucket}`;
    }
    function estimatedUsageWindowLabel(est) {
      if (est && est.window_start && est.window_end) {
        return costRangeTitle(est.window_start, est.window_end, 0);
      }
      return 'current token summary; session timestamps unavailable';
    }

    // Normalized history [{usd, timestamp, agents}]. The server serializes the
    // timestamp as `t` (CostHistoryEntry json:"t"); accept `timestamp` too so a
    // future rename can't zero the card again. `agents` (name → cumulative $)
    // is absent on entries recorded before per-agent snapshots existed.
    function costHist() {
      return (_costHistory || [])
        .map(e => e && { usd: e.usd, timestamp: e.t ?? e.timestamp, agents: e.agents, models: e.models })
        .filter(e => e && typeof e.usd === 'number' && typeof e.timestamp === 'number');
    }

    // Per-day rollups from /api/cost (daily_rollup): [{start, end, spend,
    // samples}] in ascending order, each UTC day placed on the LOCAL calendar
    // day of the same date so it lines up with the day/week/month buckets.
    // Only days with at least one sample count as data.
    function costDailyRollups() {
      const raw = (_lastCost && Array.isArray(_lastCost.daily_rollup)) ? _lastCost.daily_rollup : [];
      const out = [];
      for (const r of raw) {
        if (!r || typeof r.day !== 'string' || !(r.samples > 0)) continue;
        const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(r.day);
        if (!m) continue;
        const start = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])).getTime();
        out.push({ start, end: costAddDays(start, 1), spend: Number(r.cost_usd) || 0, samples: r.samples });
      }
      out.sort((a, b) => a.start - b.start);
      return out;
    }

    // costHistoryStart: the earliest instant any cost data covers — the first
    // rollup day or the first snapshot, whichever is older; null with no data.
    function costHistoryStart() {
      const hist = costHist();
      const roll = costDailyRollups();
      const a = hist.length ? hist[0].timestamp : null;
      const b = roll.length ? roll[0].start : null;
      if (a == null) return b;
      if (b == null) return a;
      return Math.min(a, b);
    }

    const COST_LINEAGE_RESET_FLOOR_USD = 100;
    const COST_LINEAGE_RESET_RATIO = 0.5;

    function costHistWithCurrent(est) {
      const hist = costHist();
      const currentUSD = est && Number(est.total_usd);
      if (Number.isFinite(currentUSD)) {
        if (hist.length) {
          const latest = hist[hist.length - 1];
          if (Math.abs(Number(latest.usd) - currentUSD) > 0.000001) {
            hist.push({ ...latest, timestamp: Date.now(), usd: currentUSD });
          }
        } else {
          hist.push({ timestamp: Date.now(), usd: currentUSD });
        }
      }
      return hist;
    }

    // costLineageResets: the points where the cumulative token-summary
    // estimate fell by more than COST_LINEAGE_RESET_FLOOR_USD AND to below half
    // its previous value — a pod roll, a cleared session store, a backend
    // change. Listed in the headline tooltip so a user can see why the live
    // token summary restarted; the headline itself no longer depends on them.
    function costLineageResets(hist) {
      const resets = [];
      let prev = null;
      for (const e of hist) {
        const cur = Number(e.usd);
        if (!Number.isFinite(cur)) continue;
        if (prev != null && prev - cur > COST_LINEAGE_RESET_FLOOR_USD && cur < prev * COST_LINEAGE_RESET_RATIO) {
          resets.push({ timestamp: e.timestamp, previous: prev, next: cur });
        }
        prev = cur;
      }
      return resets;
    }

    // costRollupsBeforeRing: the persisted daily rollups (UTC days, raw from
    // /api/cost) for days up to and including the UTC day of the first
    // snapshot still in the ring. Those days were folded while the ring still
    // covered them fully and are frozen server-side, so they are the
    // authoritative record of spend the ring has since evicted. Returns the
    // days ascending plus the instant from which the ring takes over.
    function costRollupsBeforeRing(hist) {
      const raw = (_lastCost && Array.isArray(_lastCost.daily_rollup)) ? _lastCost.daily_rollup : [];
      const utcDay = (ms) => new Date(ms).toISOString().slice(0, 10);
      const firstDay = hist.length ? utcDay(hist[0].timestamp) : null;
      const days = [];
      for (const r of raw) {
        if (!r || typeof r.day !== 'string' || !(r.samples > 0)) continue;
        if (firstDay != null && r.day > firstDay) continue;
        const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(r.day);
        if (!m) continue;
        const start = Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
        // localStart: the same calendar date at local midnight, for display
        // (the chart places UTC days on the local calendar day the same way).
        const localStart = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])).getTime();
        days.push({ day: r.day, start, localStart, end: start + COST_DAY_MS, spend: Number(r.cost_usd) || 0 });
      }
      days.sort((a, b) => a.start - b.start);
      // The ring takes over from the LAST snapshot that still falls on the
      // last rolled-up day, so the step into the next day is counted once (by
      // the ring) and the rolled-up day's own steps are not counted twice.
      let ringFrom = hist.length ? hist[0].timestamp : 0;
      if (days.length && firstDay != null) {
        const lastDay = days[days.length - 1].day;
        for (const e of hist) {
          if (utcDay(e.timestamp) > lastDay) break;
          ringFrom = e.timestamp;
        }
      }
      return { days, ringFrom, coversRingStart: days.some(d => firstDay != null && d.day < firstDay) };
    }

    // costAllTimeEstimate: the headline figure. ONE estimator shared with the
    // trend chart and the server's daily rollup: persisted rollup days the ring
    // no longer holds + the sum of positive step-to-step increments inside the
    // ring (priced per model, discontinuity-guarded, so a restored or re-seeded
    // counter is not billed as spend). When the ring IS the whole history the
    // first snapshot's value is the spend accumulated before sampling began
    // and is counted once as the opening balance.
    //
    // Replaces the old "sum of per-lineage peaks", which double counted the
    // residual that survives a partial reset (a counter falling 13,964 → 5,058
    // added the 5,058 again inside the next peak), missed spend inside a
    // lineage that dipped and regrew, and changed whenever the ring evicted a
    // sample — the "since …" date crept forward hour by hour.
    function costAllTimeEstimate(est) {
      const hist = costHistWithCurrent(est);
      const roll = costRollupsBeforeRing(hist);
      const series = [];
      let running = 0;
      for (const d of roll.days) {
        running += d.spend;
        series.push({ timestamp: d.end, usd: running, rollup: true });
      }
      const opening = (!roll.coversRingStart && hist.length && Number.isFinite(Number(hist[0].usd))) ? Number(hist[0].usd) : 0;
      running += opening;
      const first = roll.days.length ? roll.days[0].localStart : (hist.length ? hist[0].timestamp : 0);
      // Differenced on the recorded $ (what the server's daily rollup folds
      // and what "persisted history" means), not on re-priced tokens: the
      // headline is the one figure that must agree with the persisted record.
      const ring = hist.filter(e => e.timestamp >= roll.ringFrom && Number.isFinite(Number(e.usd)));
      const incs = [];
      for (let i = 1; i < ring.length; i++) {
        const d = Number(ring[i].usd) - Number(ring[i - 1].usd);
        if (d > 0) incs.push(d);
      }
      const ceiling = costHeadlineCeiling(incs);
      let peak = -Infinity;
      for (let i = 0; i < ring.length; i++) {
        const cur = Number(ring[i].usd);
        if (i > 0) {
          const prevVal = Number(ring[i - 1].usd);
          if (cur > prevVal && !costStepIsRestore(cur - prevVal, prevVal, cur, peak, ceiling)) running += cur - prevVal;
        }
        series.push({ timestamp: ring[i].timestamp, usd: running });
        if (cur > peak) peak = cur;
      }
      return {
        usd: running,
        firstTimestamp: first,
        currentUSD: Number.isFinite(est && Number(est.total_usd)) ? Number(est.total_usd) : null,
        currentStart: est && est.window_start ? est.window_start : 0,
        resets: costLineageResets(hist),
        rollupUSD: roll.days.reduce((s, d) => s + d.spend, 0),
        openingUSD: opening,
        series,
      };
    }

    // costHeadlineCeiling: the $ above which a single step is "abnormally
    // large" — the same rule as the chart's costDiscontinuityCeiling (20× the
    // median ordinary step, never below $500) applied to the recorded $ steps.
    // Infinity on thin data so nothing is dropped before a baseline exists.
    function costHeadlineCeiling(incs) {
      const normal = incs.filter(v => v < COST_DISCONTINUITY_FLOOR_USD).sort((a, b) => a - b);
      if (normal.length < 2) return Infinity;
      const mid = Math.floor(normal.length / 2);
      const median = normal.length % 2 ? normal[mid] : (normal[mid - 1] + normal[mid]) / 2;
      if (!(median > 0)) return Infinity;
      return Math.max(COST_DISCONTINUITY_MULT * median, COST_DISCONTINUITY_FLOOR_USD);
    }

    // A large step is a RESTORE (a re-seeded counter regaining ground that
    // was already counted) when it closes at least this fraction of the gap
    // between the previous value and the highest value the counter has held.
    const COST_RESTORE_GAP_FRACTION = 0.8;

    // costStepIsRestore: is one positive step a data correction rather than
    // spend? Only a step that is both abnormally large (above the chart's
    // discontinuity ceiling) AND lands at or below a level the counter had
    // already reached, closing most of the gap to it, is a restore — e.g. the
    // observed 394 → 18,350 jump an hour after a 20,800 → 394 reset. A large
    // step to a NEW high is spend however big (a day's session flush on an
    // Opus-class hive, or several days revealed after a sampling gap), and a
    // normal-sized step always counts.
    function costStepIsRestore(inc, prev, cur, peak, ceiling) {
      if (!(typeof ceiling === 'number') || !(inc > ceiling)) return false;
      return costStepRegainsGround(inc, prev, cur, peak);
    }

    // costStepRegainsGround: the size-independent half of the restore test —
    // the step lands at or below a level already reached and closes most of
    // the gap to it. A step to a new high is never a restore.
    function costStepRegainsGround(inc, prev, cur, peak) {
      if (!Number.isFinite(peak) || !(cur <= peak)) return false;
      return inc >= COST_RESTORE_GAP_FRACTION * (peak - prev);
    }

    function costCurrentLineageHist(currentUSD) {
      const hist = costHistWithCurrent({ total_usd: currentUSD });
      if (!hist.length) return [];
      let start = 0;
      for (let i = 1; i < hist.length; i++) {
        const prev = Number(hist[i - 1].usd);
        const cur = Number(hist[i].usd);
        if (Number.isFinite(prev) && Number.isFinite(cur) && prev - cur > COST_LINEAGE_RESET_FLOOR_USD && cur < prev * COST_LINEAGE_RESET_RATIO) start = i;
      }
      return hist.slice(start);
    }

    // Tiny inline sparkline for cost-table cells. Same min/max scaling as the
    // big sparkline, sized to sit beside a number.
    function miniSparkSvg(values, color, label) {
      if (!values || values.length < 2) return '';
      const W = 56, H = 14;
      const max = Math.max(...values), min = Math.min(...values);
      const win = costWindow();
      const title = `${label || 'sparkline'} · ${costRangeTitle(win.start, win.end, win.bucketMs)} · values ${fmtSparkVal(min)}–${fmtSparkVal(max)} · latest ${fmtSparkVal(values[values.length - 1])}`;
      return renderSparkline(null, values, {
        width: W, height: H, color, min, max, strokeWidth: 1.2, dot: false,
        title, ariaLabel: title, label: label || 'sparkline', id: `cell:${label || 'sparkline'}`, wrapper: false, svgClass: 'cell-spark',
      });
    }

    // Cumulative series for one model's field ('i' input, 'o' output, 'usd')
    // across the active cost window — the table sparklines follow the same
    // timeframe selector as everything else.
    function modelSeries(name, field) {
      const win = costWindow();
      const out = [];
      for (const e of costHist()) {
        if (e.timestamp < win.start || e.timestamp > win.end) continue;
        const m = e.models && e.models[name];
        if (m && typeof m[field] === 'number') out.push(m[field]);
      }
      return out;
    }

    // Last cumulative value at-or-before t via pick(e); null if history starts
    // after t (or pick never yields a number up to t).
    function costValueAt(hist, t, pick) {
      let v = null;
      for (const e of hist) {
        if (e.timestamp > t) break;
        const x = pick(e);
        if (typeof x === 'number') v = x;
      }
      return v;
    }

    // costBuckets: returns [{start, end, spend, hasData, rollup}] — one per
    // bucket of the active window (costWindow()). spend is the cumulative-$
    // delta over the bucket from the 5-minute snapshots, plus — for days that
    // lie entirely before the first snapshot — the per-day rollup. Buckets
    // neither source reaches stay hasData=false, as do calendar periods still
    // ahead in the weekly/monthly views. rollup=true marks a bucket that drew
    // any of its spend from the rollup.
    function costBuckets(win) {
      win = win || costWindow();
      const hist = costHist();
      const buckets = [];
      for (let i = 0; i + 1 < win.edges.length; i++) {
        buckets.push({ start: win.edges[i], end: win.edges[i + 1], spend: 0, hasData: false, rollup: false });
      }
      costApplyRollups(buckets, hist);
      if (hist.length < 2) return buckets;
      // Per-bucket spend = sum of POSITIVE per-model token increments inside the
      // bucket, priced at each model's current effective rate.
      //
      // The token "cumulative" counter is not truly monotonic: it's a sum over
      // the sessions currently visible to the scanner, so after a pod roll or as
      // old sessions age out it DROPS. A plain end−start delta of that counter
      // therefore goes negative and clamps to $0 even though agents kept running
      // and really did spend. Summing only the step-to-step INCREASES recovers
      // the actual spend from the noisy counter. (Also immune to price-table
      // shifts, which move the cumulative $ but not the token increments.)
      const rate = costPriceMap(hist);
      // Ceiling derived once from the full history so every bucket judges a
      // one-time re-seed jump against the same baseline (a bucket holding only
      // the spike has no local typical of its own).
      const ceiling = { ceiling: costDiscontinuityCeiling(hist, rate), ratePerMs: costDiscontinuityRate(hist, rate) };
      // Spread each increment across the interval it accrued over rather than
      // billing it to the instant it was observed — otherwise a bucket narrower
      // than the observation cadence (notably every hourly bucket) reads $0 even
      // while agents were working. See COST_ACCRUAL_* above.
      const spans = costAccrualSpans(hist, rate);
      for (const b of buckets) {
        // Coverage, not spend, decides hasData: a bucket the history actually
        // spans is a real $0 (nothing ran), whereas a bucket outside the
        // recorded range is genuinely unknown. Keeping these distinct is what
        // lets the empty state say which one it is.
        const covered = costSpendBetween(hist, rate, b.start, b.end, ceiling) != null
          || spans.some(s => s.to > b.start && s.from < b.end);
        if (!covered) continue;
        b.spend += costSpreadSpendBetween(spans, b.start, b.end, ceiling);
        b.hasData = true;
      }
      return buckets;
    }

    // costApplyRollups: add rollup days that fall inside a bucket AND end at or
    // before the first snapshot (so a day the snapshots already cover is never
    // counted twice).
    function costApplyRollups(buckets, hist) {
      const roll = costDailyRollups();
      if (!roll.length) return;
      const fineStart = hist.length ? hist[0].timestamp : Infinity;
      for (const b of buckets) {
        for (const r of roll) {
          if (r.start < b.start || r.end > b.end || r.end > fineStart) continue;
          b.spend += r.spend;
          b.hasData = true;
          b.rollup = true;
        }
      }
    }

    // agentWindowSpend: estimated $ spent by one agent inside the active cost
    // window, derived from per-agent cumulative snapshots. null = no per-agent
    // data covering this window (e.g. history predates the feature).
    function agentWindowSpend(name) {
      const win = costWindow();
      const hist = costHist().filter(e => e.agents && typeof e.agents[name] === 'number');
      if (hist.length < 2) return null;
      // Same non-monotonic counter problem as the model spend: the per-agent
      // cumulative $ drops after a pod roll / session-window shift, so an
      // end−start delta clamps to $0 and the chip vanishes even for a busy
      // agent. Sum only the positive step-to-step increases over the window.
      let spend = 0, prev = null, saw = false;
      for (const e of hist) {
        if (e.timestamp < win.start || e.timestamp > win.end) { if (e.timestamp > win.end) break; continue; }
        saw = true;
        const cur = e.agents[name];
        if (prev != null && cur > prev) spend += (cur - prev);
        prev = cur;
      }
      return saw ? spend : null;
    }

    // costBucketAxisLabel: the bar is labelled by the hour/day/week/month it
    // COVERS (its start), in the browser's local clock like the window
    // boundaries. Daily charts use weekday names only while every weekday is
    // unique (≤ 7 bars); wider daily charts label by M/D.
    function costBucketAxisLabel(b, win) {
      win = win || costWindow();
      const d = new Date(b.start);
      const hh = String(d.getHours()).padStart(2, '0');
      const md = `${d.getMonth() + 1}/${d.getDate()}`;
      if (win.key === 'hourly') return hh;
      if (win.key === 'daily') return win.count <= COST_WEEK_DAYS ? COST_WEEKDAY_LABELS[d.getDay()] : md;
      if (win.key === 'weekly') return md;
      if (win.key === 'monthly') return COST_MONTH_LABELS[d.getMonth()];
      return win.bucket === 'hour' ? `${md} ${hh}h` : md;
    }

    // costAxisLabelsHtml: every bar when there are few, else
    // COST_AXIS_LABEL_SPARSE_COUNT evenly spaced bars (first and last included)
    // to avoid crowding.
    function costAxisLabelsHtml(buckets, win) {
      if (!buckets.length) return '';
      let picks = buckets;
      if (buckets.length > COST_AXIS_LABEL_EVERY_BAR_MAX) {
        picks = [];
        const k = COST_AXIS_LABEL_SPARSE_COUNT, n = buckets.length;
        for (let i = 0; i < k; i++) picks.push(buckets[Math.round(i * (n - 1) / (k - 1))]);
      }
      return picks.map(b => `<span>${esc(costBucketAxisLabel(b, win))}</span>`).join('');
    }

    // Bar heights are a percentage of the chart; the tallest bar is 100 %.
    // A bar with ANY recorded spend is drawn at least COST_BAR_MIN_SPEND_PCT
    // tall: on a linear scale a $1 day beside a $1,000 day is 0.1 % — under a
    // pixel at the chart's height — and read as "the day vanished" (observed
    // the morning real-time Copilot recording started, #13492). A recorded $0
    // (data, nothing spent) draws the COST_BAR_ZERO_PCT baseline so an
    // all-zero window reads as "flat at zero" rather than missing.
    const COST_BAR_MIN_SPEND_PCT = 6;
    const COST_BAR_ZERO_PCT = 2;
    function costBarHeightPct(b, max) {
      if (!(b.spend > 0)) return b.hasData ? COST_BAR_ZERO_PCT : 0;
      if (!(max > 0)) return COST_BAR_MIN_SPEND_PCT;
      return Math.max(COST_BAR_MIN_SPEND_PCT, (b.spend / max) * 100);
    }

    // costWindowRunRate: $ per ms = spend inside the window ÷ the part of the
    // window that has elapsed so far. null when nothing was spent or no time
    // has passed (the caller then omits the run-rate).
    function costWindowRunRate(spend, win) {
      if (!(spend > 0) || !(win && win.elapsedMs > 0)) return null;
      return spend / win.elapsedMs;
    }
    // costRunRateUnit: the natural unit for the window's bucket size.
    function costRunRateUnit(win) {
      return win.bucket === 'hour' ? { ms: COST_HOUR_MS, label: '/hr' } : { ms: COST_DAY_MS, label: '/day' };
    }
    // priceMap: $/token per model, each taken from the NEWEST snapshot that
    // still carries that model. The newest snapshot alone is not enough: the
    // token summary only lists the models of the sessions currently on disk, so
    // after a backend or model change the latest snapshot may name only
    // today's cheap helper model, and every historical model would then be
    // priced at $0 and vanish from the chart (observed: a $17K history whose
    // chart summed to $15 because the last snapshot listed only gpt-4o-mini).
    function costPriceMap(hist) {
      const rate = {};
      for (let i = hist.length - 1; i >= 0; i--) {
        const models = hist[i].models;
        if (!models) continue;
        for (const k in models) {
          const m = models[k];
          const toks = (m.i || 0) + (m.o || 0);
          if (rate[k] == null && typeof m.usd === 'number' && toks > 0) rate[k] = m.usd / toks;
        }
      }
      return rate;
    }

    // Multiple of the median positive increment above which a single step is
    // treated as a data correction (re-seed / regression-fix restore), not real
    // spend. Chosen high enough that bursty-but-real hours (a few× the median)
    // are always kept, low enough to catch a restore that dwarfs every normal
    // step. The absolute floor below keeps a tiny median from nuking legit spend.
    const COST_DISCONTINUITY_MULT = 20;
    // Absolute-$ floor: an increment is only ever discarded as a discontinuity
    // if it also exceeds this many dollars in a single step. Real session
    // flushes DO exceed it (a day's flush on an Opus-class hive runs $600–$18k,
    // #13497), so the ceiling it anchors applies only to restore-shaped steps —
    // those that regain ground the counter had already reached
    // (costStepRegainsGround). A step to a new high is spend however large;
    // the ~$18k all-time restore it was built to catch lands back at old
    // ground and still trips it.
    const COST_DISCONTINUITY_FLOOR_USD = 500;

    // ── Accrual spreading ──────────────────────────────────────────────────
    // The cumulative token counter does NOT rise smoothly as agents work: the
    // scanner only observes a session's tokens once that session's transcript
    // is flushed, so the counter sits flat for hours and then jumps by the whole
    // session at once. Observed on a live spoke: 285 snapshots over 6.5 days
    // produced just 4 increases, spaced a median of ~8.5h apart.
    //
    // Attributing a jump to the single instant it was OBSERVED is wrong twice
    // over: the one bucket containing that instant gets the entire session's
    // spend, and every other bucket reads $0 even though agents were busy. For
    // an hourly view (24 one-hour buckets) that means the chart is blank unless
    // the viewer happens to load it within an hour of an observation — which is
    // exactly the reported "I see data there once in a while".
    //
    // Fix: treat an increment as spend that accrued EVENLY across the interval
    // between the previous observation and this one, and bill each bucket the
    // overlapping fraction. Totals are unchanged (every dollar is still counted
    // exactly once); they are simply placed where the work happened rather than
    // where it was noticed.
    //
    // How far back an increment may be spread is bounded by RECORDED LIVENESS,
    // not by a fixed wall-clock limit. Spreading should follow the interval the
    // hive was actually up and sampling: if snapshots kept arriving every ~5 min
    // while the counter sat flat, the hive was alive and agents plausibly
    // working the whole time, so the spend belongs across all of it. If instead
    // the snapshots themselves stop, the hive was down — no work happened, and
    // smearing spend across that dead air would invent activity.
    //
    // A gap between consecutive SNAPSHOTS longer than this means the sampler
    // wasn't running (it records every ~5 min, so a few missed cycles is normal
    // jitter but a long silence is an outage/restart). Spreading is cut at the
    // most recent such silence.
    const COST_ACCRUAL_LIVENESS_GAP_MS = 30 * 60e3; // 30 minutes
    // Absolute backstop so a hive that sampled continuously for weeks while the
    // counter never moved still can't smear one jump across the entire chart.
    const COST_ACCRUAL_MAX_SPREAD_MS = 24 * 3600e3; // 24 hours
    // An interval shorter than this is treated as instantaneous — no meaningful
    // spreading to do, and it keeps back-to-back snapshots cheap to compute.
    const COST_ACCRUAL_MIN_SPREAD_MS = 60e3; // 1 minute

    // costPositiveIncrements: the list of POSITIVE step-to-step increments over
    // [t0, t1], differenced only within a like measure (see costSpendBetween).
    // Shared by the spend sum and the discontinuity ceiling so both judge the
    // same series.
    function costPositiveIncrements(hist, rate, t0, t1) {
      const pricedTok = (models) => {
        let s = 0;
        for (const k in models) { const m = models[k]; s += ((m.i || 0) + (m.o || 0)) * (rate[k] || 0); }
        return s;
      };
      const incs = [], gaps = [], restores = [];
      // peak is tracked separately from prev/prevKind: pre-window entries
      // `continue` before prev is updated (they must not seed an increment),
      // yet they still raise the peak the restore test compares against, so
      // the peak needs its own kind marker to reset on a measure change.
      let prev = null, prevKind = null, prevTs = 0, sawWindow = false, peak = -Infinity, peakKind = null;
      for (const e of hist) {
        if (e.timestamp > t1) break;
        const kind = e.models ? 'm' : (typeof e.usd === 'number' ? 'u' : null);
        if (kind == null) continue;
        const cur = kind === 'm' ? pricedTok(e.models) : e.usd;
        if (kind !== peakKind) { peak = -Infinity; peakKind = kind; }
        if (e.timestamp < t0) { if (cur > peak) peak = cur; continue; }
        sawWindow = true;
        if (prev != null && kind === prevKind && cur > prev) {
          incs.push(cur - prev); gaps.push(e.timestamp - prevTs);
          restores.push(costStepRegainsGround(cur - prev, prev, cur, peak));
        }
        if (cur > peak) peak = cur;
        prev = cur; prevKind = kind; prevTs = e.timestamp;
      }
      return { incs, gaps, restores, sawWindow };
    }

    // costDiscontinuityRate: the plausible-spend rate ceiling in $/ms —
    // COST_DISCONTINUITY_MULT × the hive's average rate (ordinary, below-floor
    // spend ÷ the span the history covers). The fixed ceiling judges every
    // step as if it arrived at the 5-minute cadence, but the sampler is
    // view-driven (#13483): a hive nobody looked at for a week reveals a week
    // of real spend in one step. Such a step is spend, not a correction, when
    // elapsed time × a plausible rate explains it (#13482). The average, not
    // the median, is the baseline: spend arrives in lumps between long runs
    // of near-zero steps, so the median step rate says nothing about how fast
    // the hive really spends. Infinity (no rate cap) on thin data. Mirrors
    // costRollupDiscontinuityRate in cost_rollup.go.
    function costDiscontinuityRate(hist, rate) {
      if (!hist || hist.length < 2) return Infinity;
      const { incs, gaps } = costPositiveIncrements(hist, rate, -Infinity, Infinity);
      let sum = 0, n = 0;
      for (let i = 0; i < incs.length; i++) {
        if (incs[i] < COST_DISCONTINUITY_FLOOR_USD && gaps[i] > 0) { sum += incs[i]; n++; }
      }
      const span = hist[hist.length - 1].timestamp - hist[0].timestamp;
      if (n < 2 || !(span > 0) || !(sum > 0)) return Infinity;
      return COST_DISCONTINUITY_MULT * sum / span;
    }

    // costStepAllowed: a positive step observed over gapMs is spend when it is
    // under the fixed ceiling or within what the plausible rate could have
    // accrued over the gap. `cap` may be a number (fixed ceiling only, legacy
    // callers) or { ceiling, ratePerMs }. `restore` is the costStepRegainsGround
    // verdict for the step: false → always spend (a step to a new high is never
    // a correction, #13497); true → the size tests apply; undefined (legacy
    // callers without peak tracking) → the size tests apply to every step.
    // The Go mirror (costFoldDailyRollups) applies the same rule as a
    // precondition at the call site rather than inside the predicate.
    function costStepAllowed(amount, gapMs, cap, restore) {
      if (restore === false) return true;
      const obj = !!(cap && typeof cap === 'object');
      const ceiling = obj ? cap.ceiling : cap;
      const c = (typeof ceiling === 'number') ? ceiling : Infinity;
      if (amount <= c) return true;
      if (!obj) return false;
      const ratePerMs = cap.ratePerMs;
      if (ratePerMs === Infinity) return true;
      if (!(gapMs > 0)) return false;
      return amount <= ratePerMs * gapMs;
    }

    // costAccrualSpans: every POSITIVE increment in the WHOLE history, each
    // tagged with the interval [from, to] it is presumed to have accrued over.
    // See the COST_ACCRUAL_* notes above for why observation time ≠ spend time.
    //
    // The interval is the stretch over which the counter that rose had been
    // FLAT while the sampler stayed alive: from the snapshot at which that
    // same counter last rose (or the first dense snapshot, or 24 h, whichever
    // is nearest) to the snapshot that revealed the rise. Spend can only have
    // been hiding while its counter sat still — the stretch before the
    // previous rise was already accounted for by that rise. Without this
    // bound every 5-minute increment of a LIVE counter (the proxy records
    // Copilot usage as it happens, #13492) walked back over the whole dense
    // 24 h and the first day of real-time recording was billed ~80 % to the
    // day before it (observed: $860 on a day whose own record was $1.19).
    //
    // Flatness is judged per model, not on the summed total: a chatty cheap
    // helper that moves the total every 5 minutes must not stop an expensive
    // model's session flush — which arrives as one jump after hours of
    // silence on ITS counter — from being spread back across those hours.
    // Each aggregate step is therefore split across the models that rose,
    // in proportion to their rise, so the total billed is exactly the
    // aggregate step (what costSpendBetween and the server's rollup count)
    // and only its placement differs. A model's first appearance has no flat
    // stretch (it was absent, not still) and is billed to its observation
    // interval. Snapshots recorded before per-model data existed carry one
    // '$' counter.
    //
    // Deliberately walks the entire series rather than a window: an increment
    // observed just AFTER a bucket ends may still have accrued largely INSIDE
    // it, and one observed inside a bucket may have started well before it.
    // Windowing first would drop exactly the spans that need splitting.
    function costAccrualSpans(hist, rate) {
      const priceModel = (k, m) => ((m.i || 0) + (m.o || 0)) * (rate[k] || 0);
      const pricedTok = (models) => {
        let s = 0;
        for (const k in models) s += priceModel(k, models[k]);
        return s;
      };
      const spans = [];
      // Timestamps of every usable snapshot, in order, so an increment can walk
      // back over the stretch the sampler was demonstrably alive.
      const stamps = [];
      // lastRise: per counter (model name, or '$' for usd-only snapshots), the
      // snapshot at which it last rose — the start of its current flat stretch.
      let lastRise = {};
      let prev = null, prevModels = null, prevKind = null, peak = -Infinity;
      for (const e of (hist || [])) {
        const kind = e.models ? 'm' : (typeof e.usd === 'number' ? 'u' : null);
        if (kind == null) continue;
        const cur = kind === 'm' ? pricedTok(e.models) : e.usd;
        if (kind !== prevKind) { peak = -Infinity; lastRise = {}; }
        const prevStamp = stamps.length ? stamps[stamps.length - 1] : e.timestamp;
        // Per-counter rises at this snapshot, used both to place this step and
        // to advance each counter's flat-stretch start afterwards.
        const rises = [];
        if (prev != null && kind === prevKind) {
          if (kind === 'u') {
            if (cur > prev) rises.push({ k: '$', rise: cur - prev });
          } else {
            for (const k in e.models) {
              const now = priceModel(k, e.models[k]);
              const before = prevModels && prevModels[k] ? priceModel(k, prevModels[k]) : null;
              if (before == null) { if (now > 0) rises.push({ k, rise: now, first: true }); }
              else if (now > before) rises.push({ k, rise: now - before });
            }
          }
        }
        if (prev != null && kind === prevKind && cur > prev) {
          const step = cur - prev;
          // gap is the observation interval (previous snapshot → this one),
          // which the discontinuity guard judges the step against; `from` is
          // the (liveness-bounded) accrual start and may be later.
          const gap = stamps.length ? e.timestamp - prevStamp : 0;
          const restore = costStepRegainsGround(step, prev, cur, peak);
          // Walk back while snapshots stayed dense: that is the stretch the hive
          // was up and this spend could have been accruing. Stop at the first
          // long silence (outage/restart) so spend never smears across dead air.
          let dense = e.timestamp;
          for (let i = stamps.length - 1; i > 0; i--) {
            if (stamps[i] - stamps[i - 1] > COST_ACCRUAL_LIVENESS_GAP_MS) break;
            dense = stamps[i - 1];
            if (e.timestamp - dense >= COST_ACCRUAL_MAX_SPREAD_MS) break;
          }
          dense = Math.max(dense, e.timestamp - COST_ACCRUAL_MAX_SPREAD_MS);
          const riseTotal = rises.reduce((s, r) => s + r.rise, 0);
          const parts = riseTotal > 0 ? rises : [{ k: '$', rise: step, first: true }];
          const partTotal = riseTotal > 0 ? riseTotal : step;
          for (const r of parts) {
            const since = r.first ? prevStamp : (lastRise[r.k] != null ? lastRise[r.k] : dense);
            const from = Math.max(dense, since);
            spans.push({ amount: step * (r.rise / partTotal), step, from, to: e.timestamp, gap, restore });
          }
        }
        for (const r of rises) lastRise[r.k] = e.timestamp;
        if (cur > peak) peak = cur;
        prev = cur; prevModels = kind === 'm' ? e.models : null; prevKind = kind;
        stamps.push(e.timestamp);
      }
      return spans;
    }

    // costSpreadSpendBetween: estimated $ that accrued inside [t0, t1], billing
    // each increment the fraction of its accrual interval that overlaps the
    // bucket. An increment whose interval is shorter than
    // COST_ACCRUAL_MIN_SPREAD_MS is treated as a point event at its observation
    // time. Increments above `cap` are dropped as data corrections, exactly as
    // in costSpendBetween — judged on the whole aggregate step (`step`), not on
    // the per-model part of it this span carries.
    function costSpreadSpendBetween(spans, t0, t1, cap) {
      let spend = 0;
      for (const s of (spans || [])) {
        if (!costStepAllowed(s.step != null ? s.step : s.amount, s.gap, cap, s.restore)) continue;
        const span = s.to - s.from;
        if (span < COST_ACCRUAL_MIN_SPREAD_MS) {
          // Point event: bill it entirely to the bucket containing it.
          if (s.to > t0 && s.to <= t1) spend += s.amount;
          continue;
        }
        const lo = Math.max(s.from, t0), hi = Math.min(s.to, t1);
        if (hi <= lo) continue;
        spend += s.amount * ((hi - lo) / span);
      }
      return spend;
    }

    // costDiscontinuityCeiling: the $ threshold above which a single positive
    // increment is a data correction, not spend. Derived from the FULL history
    // (not one bucket) so it's stable: a per-hour window that happens to contain
    // ONLY the re-seed spike has no local "typical" to compare against, so the
    // ceiling must come from the whole series. Returns Infinity (no cap) when
    // there aren't enough normal increments to establish a median, so we never
    // drop real spend on thin data. The median is computed over increments BELOW
    // the absolute floor, so the outlier itself can't inflate the baseline.
    function costDiscontinuityCeiling(hist, rate) {
      if (!hist || hist.length < 2) return Infinity;
      const { incs } = costPositiveIncrements(hist, rate, -Infinity, Infinity);
      const normal = incs.filter(v => v < COST_DISCONTINUITY_FLOOR_USD).sort((a, b) => a - b);
      if (normal.length < 2) return Infinity;
      const mid = Math.floor(normal.length / 2);
      const median = normal.length % 2 ? normal[mid] : (normal[mid - 1] + normal[mid]) / 2;
      if (!(median > 0)) return Infinity;
      // Cap at whichever bound is LARGER so an increment must beat BOTH the
      // relative (20× median) and the absolute ($500) test to be discarded.
      return Math.max(COST_DISCONTINUITY_MULT * median, COST_DISCONTINUITY_FLOOR_USD);
    }

    // costSpendBetween: estimated $ spent in [t0, t1], summed from the POSITIVE
    // step-to-step token increments priced at the current rate. The token
    // "cumulative" counter is NOT truly monotonic — after a pod roll or when old
    // sessions age out of the scanner window it drops — so a plain end−start
    // delta goes negative and (wrongly) reads as $0. Summing only the positive
    // increments recovers the real spend from that noisy counter.
    //
    // Discontinuity guard: a positive counter can ALSO jump by a huge one-time
    // step when the counter is re-seeded — a pod restart re-seeding per-agent
    // totals, or a regression fix restoring an all-time count (observed: a lone
    // ~$394 → ~$18,350 step). That single jump is a data correction, not spend,
    // yet the positive-increment sum would bill it into one hourly bucket,
    // producing a bogus giant bar and an inflated window-spend + run-rate. So an
    // increment above the discontinuity ceiling (see above) is skipped — but
    // only when the step is restore-shaped, i.e. it regains ground the counter
    // had already reached (costStepRegainsGround); a step to a new high is
    // spend however large (#13497). `ceiling`
    // is derived once from the full history and passed in; omit it to disable the
    // guard (kept optional so callers without full history stay correct).
    function costSpendBetween(hist, rate, t0, t1, ceiling) {
      const { incs, gaps, restores, sawWindow } = costPositiveIncrements(hist, rate, t0, t1);
      let spend = 0;
      for (let i = 0; i < incs.length; i++) { if (costStepAllowed(incs[i], gaps[i], ceiling, restores[i])) spend += incs[i]; }
      return sawWindow ? spend : null;
    }

    // costRangeStepperHtml: the −/+ chips that grow or shrink the active
    // range's bucket count; hidden for custom (its bounds are explicit).
    function costRangeStepperHtml() {
      const c = COST_RANGE_COUNT[_costRange];
      if (!c) return '';
      const n = costRangeCount();
      const label = COST_RANGES[_costRange].label;
      const unitWord = { h: 'hours', d: 'days', wk: 'weeks', mo: 'months' }[c.unit] || c.unit;
      return `<div class="cost-range-step" role="group" aria-label="${esc(label)} window size">
        <button type="button" data-cost-step="-1" aria-label="Shrink ${esc(label)} window by ${c.step} ${esc(unitWord)}" title="−${c.step} ${esc(c.unit)}" ${n <= c.min ? 'disabled' : ''}>−</button>
        <span class="cost-range-step-val" aria-live="polite" title="Showing the last ${n} ${esc(unitWord)} (${c.min}–${c.max})">${n}&thinsp;${esc(c.unit)}</span>
        <button type="button" data-cost-step="1" aria-label="Grow ${esc(label)} window by ${c.step} ${esc(unitWord)}" title="+${c.step} ${esc(c.unit)}" ${n >= c.max ? 'disabled' : ''}>+</button>
      </div>`;
    }

    // `now` is injectable for tests; renders use the wall clock.
    function costTrendHtml(now) {
      now = typeof now === 'number' ? now : Date.now();
      const win = costWindow(now);
      const pills = Object.keys(COST_RANGES).map(k =>
        `<button data-cost-range="${k}" class="${k === _costRange ? 'active' : ''}" aria-pressed="${k === _costRange}">${COST_RANGES[k].label}</button>`
      ).join('');
      const stepper = costRangeStepperHtml();
      let customControls = '';
      if (_costRange === 'custom') {
        // datetime-local wants local wall-clock time without zone suffix.
        const toLocal = (ms) => new Date(ms - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 16);
        customControls = `<div class="cost-custom-range">
          <input type="datetime-local" id="cost-custom-start" value="${toLocal(_costCustom.start)}" data-change-action="costCustomChanged" title="Custom range start">
          <span>→</span>
          <input type="datetime-local" id="cost-custom-end" value="${toLocal(_costCustom.end)}" data-change-action="costCustomChanged" title="Custom range end">
        </div>`;
      }
      const buckets = costBuckets(win);
      const withData = buckets.filter(b => b.hasData);
      const total = withData.reduce((s, b) => s + b.spend, 0);
      const max = Math.max(...buckets.map(b => b.spend), 0);
      // Buckets that begin before the earliest recorded data point: the
      // window extends past the recorded history.
      const histStart = costHistoryStart();
      const beforeHistory = histStart == null ? [] : buckets.filter(b => !b.hasData && b.start < now && b.start < histStart);

      let body;
      if (withData.length === 0) {
        // Nothing recorded anywhere in this window — distinct from "recorded and
        // it was $0", which is handled by the overlay below.
        body = `<div class="cost-trend-empty">No cost history recorded for this range — data accrues as agents run.</div>`;
      } else {
        const bars = buckets.map(b => {
          const span = `${formatDashboardDateTime(b.start)} → ${formatDashboardDateTime(b.end)}`;
          if (!b.hasData && b.start < now) {
            // Hatched full-height placeholder: no history at all here, which is
            // different from a recorded $0.
            return `<div class="cost-bar nodata" title="${esc(`${span}: no cost history recorded`)}"></div>`;
          }
          const h = costBarHeightPct(b, max);
          const cls = b.spend > 0 ? 'cost-bar' : 'cost-bar zero';
          const tip = `${span}: ${fmtUSD(b.spend)}${b.rollup ? ' (includes daily rollup)' : ''}`;
          return `<div class="${cls}" style="height:${h.toFixed(1)}%" title="${esc(tip)}"></div>`;
        }).join('');
        // Bare axes with no visible series are indistinguishable from a broken
        // chart. When the range IS covered by history but every bucket is $0,
        // say so on top of the (flat) bars instead of leaving the reader to
        // guess whether the panel failed to render.
        const allZero = max <= 0;
        const overlay = allZero
          ? `<div class="cost-bars-note">no spend recorded in this range</div>`
          : '';
        body = `<div class="cost-bars-wrap"><div class="cost-bars">${bars}</div>${overlay}</div>
          <div class="cost-bars-axis">${costAxisLabelsHtml(buckets, win)}</div>`;
      }

      const totalLabel = 'spend in selected range';
      // Spend and run-rate come from the SAME window as the bars: the run-rate
      // is that spend over the elapsed part of the window, in the unit of the
      // bucket size (per hour for hourly bars, per day for daily bars).
      const spendStr = fmtUSD(total);
      const rate = costWindowRunRate(total, win); // $ per ms, or null
      let rateStr = '';
      if (rate != null) {
        const unit = costRunRateUnit(win);
        rateStr = ` &nbsp;·&nbsp; run-rate: <b>~${fmtUSD(rate * unit.ms)}${unit.label}</b>`;
        // The agents' models have recorded nothing lately (#13485): the
        // run-rate only reflects whatever is still being recorded.
        if (_lastCost && _lastCost.recording && _lastCost.recording.stale) rateStr += ' <span class="cost-recording-incomplete">(recording incomplete)</span>';
      }
      // When the window reaches past the recorded history, say where history
      // starts so the owner knows the earlier bars are blank, not $0.
      let horizonStr = '';
      if (beforeHistory.length && histStart != null) {
        const hd = new Date(histStart);
        horizonStr = ` &nbsp;·&nbsp; <span class="cost-history-start" title="${esc(`Earliest cost data point: ${fmtCostDateTime(histStart)}. Hatched bars have no recorded history.`)}">history starts ${hd.getMonth() + 1}/${hd.getDate()}${hd.getFullYear() !== new Date(now).getFullYear() ? `/${hd.getFullYear()}` : ''}</span>`;
      }
      const summaryTitle = `${costRangeTitle(win.start, win.end, win.bucketMs)} · elapsed ${fmtCostDuration(win.elapsedMs)}`;
      return `<div class="cost-trend">
        <div class="cost-trend-head">
          <div class="cost-range">${pills}</div>
          ${stepper}
          ${customControls}
          <div class="cost-trend-total" title="${esc(summaryTitle)}">${totalLabel}: <b>${spendStr}</b>${rateStr}${horizonStr}</div>
        </div>
        ${body}
      </div>`;
    }

    function costCustomChanged() {
      const s = document.getElementById('cost-custom-start');
      const e = document.getElementById('cost-custom-end');
      if (!s || !e || !s.value || !e.value) return;
      const start = new Date(s.value).getTime();
      const end = new Date(e.value).getTime();
      if (!(end > start)) { showToast('Custom range: end must be after start', 'error'); return; }
      _costCustom = { start, end };
      try { localStorage.setItem('hiveCostCustom', JSON.stringify(_costCustom)); } catch {}
      if (_lastCost) renderCost(_lastCost);
      if (window._lastAgents) renderAgents(window._lastAgents);
      fetchAdvisorSpend().then(() => { if (window._lastAgents) renderAgents(window._lastAgents); });
    }

