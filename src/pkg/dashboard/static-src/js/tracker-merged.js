    function stageTrackerPRKey(repo, number) { return (repo || '') + '/pr#' + (Number(number) || 0); }
    function stageTrackerItemKey(it) { return (it.repo || '') + '/' + (it.kind || '') + '#' + (Number(it.number) || 0); }

    // Pure: PR key → merged? for one payload. Issues are not tracked. A
    // recently_merged repo given by its short name is resolved against the
    // owner/name of the items (any kind, any repo) when exactly one matches.
    function stageTrackerMergeState(d) {
      const out = {};
      if (!d || typeof d !== 'object') return out;
      const byShort = {};
      for (const it of (Array.isArray(d.items) ? d.items : [])) {
        if (!it) continue;
        // Every item of every repo (issues too) teaches the short → full
        // map, so a repo whose last open PR just merged still resolves.
        const repo = String(it.repo || '');
        const slash = repo.lastIndexOf('/');
        if (slash > 0) { const short = repo.slice(slash + 1); byShort[short] = (short in byShort && byShort[short] !== repo) ? '' : repo; }
        if (it.kind !== 'pr') continue;
        out[stageTrackerPRKey(it.repo, it.number)] = (Number(it.stage) || 0) >= STAGE_TRACKER_TERMINAL;
      }
      // The ledger's recent list (merged.recent) settles a merge the same
      // way: it records merges by anyone, not only the hive's automerge.
      const ledger = d.merged && typeof d.merged === 'object' && Array.isArray(d.merged.recent) ? d.merged.recent : [];
      for (const m of (Array.isArray(d.recently_merged) ? d.recently_merged : []).concat(ledger)) {
        if (!m || !m.repo || !m.number) continue;
        const repo = String(m.repo);
        const full = repo.indexOf('/') < 0 && byShort[repo] ? byShort[repo] : repo;
        out[stageTrackerPRKey(full, m.number)] = true;
      }
      return out;
    }

    // Pure: next plus every PR prev listed open that next no longer lists at
    // all, kept as open. A PR the snapshot dropped before the audit log
    // recorded its merge stays comparable until recently_merged settles it
    // (or, when it was closed unmerged, for the life of the tab — a few keys).
    function stageTrackerCarryOpen(prev, next) {
      const out = Object.assign({}, next || {});
      if (!prev) return out;
      for (const k of Object.keys(prev)) if (prev[k] === false && !(k in out)) out[k] = false;
      return out;
    }

    // Pure: PR keys open in prev and merged in next, sorted. No prev → none.
    function stageTrackerNewlyMerged(prev, next) {
      if (!prev || !next) return [];
      return Object.keys(next).filter(k => next[k] === true && prev[k] === false).sort();
    }

    function stageTrackerNoteMerges(d) {
      const nowMs = Date.now();
      const raw = stageTrackerCarryOpen(_stageTrackerMergeState, stageTrackerMergeState(d));
      // The merge gate holds "Merging" for at least the merge toast's
      // lifetime before the row advances to ✓ (hivecommons/hive#15245): a
      // merge that lands inside the hold is masked as still open until it
      // ends, then re-read as fresh.
      _stageTrackerMergingHold = stageTrackerMergingHoldNext(_stageTrackerMergingHold, d, _stageTrackerMergeState, raw, _stageTrackerPRItems, nowMs);
      const masked = stageTrackerMergingHoldMask(raw, _stageTrackerMergingHold, nowMs);
      if (masked.until) stageTrackerMergingHoldSchedule(masked.until - nowMs);
      _stageTrackerPRItems = stageTrackerPRItemsOf(d);
      const next = masked.state;
      const fresh = stageTrackerNewlyMerged(_stageTrackerMergeState, next).filter(k => !_stageTrackerZapped[k]);
      _stageTrackerMergeState = next;
      // A merged PR is no longer in this payload, so its last host survives.
      if (typeof stageTrackerPRHosts === 'function') Object.assign(_stageTrackerPRHost, stageTrackerPRHosts(d));
      // With the celebration on, each fresh merge is counted (and the zap
      // plays) when its token lands on the counter; off, both happen now.
      const celebrate = fresh.length && typeof stageTrackerCelebrateSetting === 'function' && stageTrackerCelebrateSetting();
      if (celebrate) fresh.forEach(k => { _stageTrackerCelebratePending[k] = true; });
      if (typeof stageTrackerMergedSync === 'function') stageTrackerMergedSync(d, fresh, Date.now());
      if (typeof stageTrackerQueueSync === 'function') stageTrackerQueueSync(d && d.items, Date.now());
      if (!fresh.length) return;
      fresh.forEach(k => { _stageTrackerZapped[k] = true; });
      if (celebrate) { stageTrackerCelebrateMerges(fresh); return; }
      if (stageTrackerSoundGate()) stageTrackerPlayZap(Math.min(fresh.length, STAGE_TRACKER_SOUND_MAX_PLAYS));
    }

    /* ---- Merge gate: "Merging" held for the toast's lifetime (#15245) ----
       Operator: "I have seen some items go straight to merge without
       showing in the Merge gate column as 'merging' for at least as long
       as the merge toast is open." The sweep merges inside one pass, so a
       PR can go from 060/090/095 to merged between two polls and the
       intermediate state is never on screen. The payload's merge_gate now
       says merging (with merging_since) while the merge call is in flight;
       the SPA keeps a hold per PR from the first moment it saw merging —
       or, for a merge it never saw coming, from the poll that reported it
       merged — and until STAGE_TRACKER_MERGING_HOLD_MS (the merge
       banner's lifetime) has passed the row stays on the board at 100
       "Merging" (its last item, synthesised when the payload dropped it)
       and the merge is not counted as fresh. When the hold ends the merge
       is re-read: the row advances to ✓, the banner opens, the chip bumps.
       A refused merge (merge_gate.state merge-failed) drops the hold at
       once and the pill says why. All pure except the schedule/release
       pair; run under node by stage_tracker_merging_hold_test.go. */
    const STAGE_TRACKER_MERGE_QUEUE = 100;
    var _stageTrackerMergingHold = {};
    var _stageTrackerPRItems = {};
    var _stageTrackerMergingHoldTimer = 0;
    // Pure: PR key → item for one payload (the last open item a hold
    // synthesises its row from).
    function stageTrackerPRItemsOf(d) {
      const out = {};
      for (const it of ((d && Array.isArray(d.items)) ? d.items : [])) if (it && it.kind === 'pr') out[stageTrackerPRKey(it.repo, it.number)] = it;
      return out;
    }
    // Pure: whether a hold is still inside its minimum window at nowMs.
    function stageTrackerMergingHoldLive(hold, nowMs) { return !!hold && nowMs < hold.since + STAGE_TRACKER_MERGING_HOLD_MS; }
    // Pure: the holds after one payload. prevState / rawState are the
    // merge-state maps (PR key → merged?) before and after it; prevItems the
    // PR items of the previous payload.
    //   • a PR the payload shows at merge_gate.state 'merging' opens (or
    //     keeps) a hold from merging_since (else from now, else the one it had)
    //   • a PR shown at 'merge-failed' — or open at any other state after a
    //     hold the payload no longer backs — drops its hold at once
    //   • a PR open before and merged now with no hold opens one from now,
    //     from its last open item, so "Merging" shows before ✓
    //   • a hold whose window has passed is dropped (its merge, if any, is
    //     then read as fresh)
    function stageTrackerMergingHoldNext(prev, d, prevState, rawState, prevItems, nowMs) {
      prev = prev || {}; prevState = prevState || {}; rawState = rawState || {}; prevItems = prevItems || {};
      const out = {};
      const items = stageTrackerPRItemsOf(d);
      for (const key of Object.keys(items)) {
        const it = items[key];
        const mg = it.merge_gate || {};
        if (mg.state === 'merging') {
          const since = (mg.merging_since && Date.parse(mg.merging_since)) || (prev[key] && prev[key].since) || nowMs;
          out[key] = { since, item: it, path: mg.merge_path || (prev[key] && prev[key].path) || '' };
        }
        // 'merge-failed' or any other open state: no hold.
      }
      for (const key of Object.keys(prev)) {
        if (out[key] || items[key]) continue;
        if (stageTrackerMergingHoldLive(prev[key], nowMs)) out[key] = prev[key];
      }
      for (const key of Object.keys(rawState)) {
        // prev[key]: it was held and the window already passed — no second hold.
        if (rawState[key] !== true || prevState[key] !== false || out[key] || prev[key] || items[key] || !prevItems[key]) continue;
        out[key] = { since: nowMs, item: prevItems[key], path: '' };
      }
      return out;
    }
    // Pure: rawState with every PR under a live hold read as still open;
    // until is the earliest ms at which a masked hold ends (0 when none).
    function stageTrackerMergingHoldMask(rawState, holds, nowMs) {
      const state = Object.assign({}, rawState || {});
      let until = 0;
      const held = [];
      for (const key of Object.keys(holds || {})) {
        const h = holds[key];
        if (state[key] !== true || !stageTrackerMergingHoldLive(h, nowMs)) continue;
        state[key] = false;
        held.push(key);
        const end = h.since + STAGE_TRACKER_MERGING_HOLD_MS;
        if (!until || end < until) until = end;
      }
      return { state, held: held.sort(), until };
    }
    // Pure: the row a held PR shows while the payload no longer lists it —
    // its last open item at 100 "Merging", the gate saying the merge landed.
    function stageTrackerMergingHoldItem(hold, key) {
      const base = hold && hold.item ? hold.item : { kind: 'pr' };
      const mg = Object.assign({}, base.merge_gate || {}, { state: 'merging', merging_since: new Date(hold.since).toISOString(), held: true, sweep_running: false, next_sweep_at: '', soak_until: '', soak_remaining_seconds: 0 });
      if (hold.path) mg.merge_path = hold.path;
      return Object.assign({}, base, { stage: STAGE_TRACKER_MERGE_QUEUE, stage_slug: 'merge-queue', stage_label: 'stage/100-merge-queue', merge_gate: mg, caption: 'Merging now — the merge landed; ✓ follows', actor: 'hive', stage_actor: Object.assign({}, base.stage_actor || {}, { kind: 'hive', login: '', why: 'hive merged it; the row advances to ✓ in a moment' }), soak_remaining_seconds: 0, modifiers: (Array.isArray(base.modifiers) ? base.modifiers : []).filter(m => m !== 'stalled' && m !== 'blocked'), held_merging: true });
    }
    // Pure: the payload as rendered — its items plus a synthesised row for
    // every live hold the payload dropped; d itself when there is none.
    function stageTrackerMergingHoldView(d, holds, nowMs) {
      if (!d || !Array.isArray(d.items)) return d;
      const have = stageTrackerPRItemsOf(d);
      const extra = [];
      for (const key of Object.keys(holds || {}).sort()) {
        if (have[key] || !stageTrackerMergingHoldLive(holds[key], nowMs)) continue;
        extra.push(stageTrackerMergingHoldItem(holds[key], key));
      }
      if (!extra.length) return d;
      return Object.assign({}, d, { items: d.items.concat(extra) });
    }
    // Stateful: when the earliest live hold ends, re-read the last payload so
    // the held merges come through as fresh and the rows advance to ✓.
    function stageTrackerMergingHoldSchedule(inMs) {
      if (_stageTrackerMergingHoldTimer) return;
      _stageTrackerMergingHoldTimer = setTimeout(stageTrackerMergingHoldRelease, Math.max(50, inMs + 50));
    }
    function stageTrackerMergingHoldRelease() {
      _stageTrackerMergingHoldTimer = 0;
      if (!_stageTracker || typeof _stageTracker !== 'object') return;
      stageTrackerNoteMerges(_stageTracker);
      renderStageTracker();
    }

    /* ---- tracker-cues: merged — pinned row, glyph flight, "Merged" chip ---
       Owner: "subtle but noticeable … a merge count that rises, and the
       celebration is something that rises out of the column and into the
       merge count"; then (hivecommons/hive#15096): "an animation that
       originates from the item's row where the check mark is and flies up
       to the top of the browser border. If the Merged pill is visible, the
       animation stops there. If not, we just see it go off the screen. It
       can be a bee or a +1." A merged PR's row does not vanish on the next
       refresh: it is pinned for STAGE_TRACKER_CELEBRATE_PIN_MS, its ✓ cell
       glows, a glyph (🐝 by default, "+1" via hive.tracker.celebrate_glyph)
       rises out of that cell for STAGE_TRACKER_CELEBRATE_FLY_MS — to the
       sticky "Merged" chip when its box is inside the viewport, otherwise
       out through the top edge — the count bumps with a soft pop (the zap
       plays on arrival when sound is on), a banner at the bottom right
       says what merged, then the row leaves by the usual exit. Several
       merges in one refresh stagger 200 ms apart and at most
       STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS glyphs fly at once; the rest
       just bump the count. Reduced motion (or the preview toggle): the
       glyph fades in place, the count pops and the row glows.
       The number on the chip is the server's: /api/stage-tracker carries
       `merged` from the durable ledger (all_time, today, total_7d, recent),
       so a reload, a new tab or a roll shows the same count. A merge this
       tab just watched is added on top until the server's recent list
       carries it; while its glyph is in flight the count holds back by one
       so the landing is the moment it bumps. The shown number never goes
       down within a tab.
       Owner (2026-10-28): the flight takes 2 s (was 0.9 s) on an ease-out
       — the pin grew to 3.5 s so the row outlasts the landing — and the
       chip's numbers roll like a mileage odometer when they rise
       (stageTrackerOdometerSet): per digit, a strip of the digits it passes
       slides up for STAGE_TRACKER_ODOMETER_MS. The first draw, a fall
       (today → 0 at midnight), reduced motion and text that does not line
       up just set the value.
       Operator (2026-10-29, hivecommons/hive#15398): "the bee … is so
       small i can't see what it is" and "i dont see the mileage counter
       animation" — the roll was there but a +1 moved one 12 px digit a
       single cell, mostly inside the chip's 320 ms pop, and a reload never
       rolled. Now the rightmost wheel, and every wheel right of the first
       changed one, spins a full turn (STAGE_TRACKER_ODOMETER_TURN) before
       settling, so even a +1 is a visible roll; the day/week line uses the
       same mechanism, shorter (STAGE_TRACKER_ODOMETER_SUB_MS); and the
       values last shown are kept in sessionStorage
       (STAGE_TRACKER_MERGED_LAST_KEY) so a reload rolls up from them. The
       glyph is drawn at --cue-flight-size (two type steps up) with a
       theme-aware halo; STAGE_TRACKER_CELEBRATE_GLYPH_HALF centres it.
       Owner (2026-10-29, again): "where is my odometer look and feel?" —
       that PR sized the flying bee, not the pill's, and the roll was an
       overlay that existed for 900 ms over plain text. The counter is
       now a drum at rest, always (stageTrackerOdometerRest /
       stageTrackerOdometerDraw): a wheel per digit, the day/week line the
       same columns drawn plain; the pill's bee is --ham-merged-bee.
       Owner (2026-10-29, hivecommons/hive issue 15568): "the odometer
       should roll only after the bee animation finishes so it looks
       correlated" — the day/week line rolled on arrival, a total the
       server had already raised rolled at once, and merges over the
       flight cap bumped under other bees. Now a glyph in the air holds
       the whole drum (stageTrackerMergedInFlight); the last landing draws
       once, to the final numbers, and only then the roll (and the zap). */
    const STAGE_TRACKER_CELEBRATE_KEY = 'hive.tracker.celebrate';
    const STAGE_TRACKER_MERGED_LAST_KEY = 'hive.tracker.merged.last';
    const STAGE_TRACKER_CELEBRATE_GLYPH_KEY = 'hive.tracker.celebrate_glyph';
    const STAGE_TRACKER_CELEBRATE_GLYPHS = { bee: '🐝', plus1: '+1' };
    const STAGE_TRACKER_CELEBRATE_GLYPH_DEFAULT = 'bee';
    const STAGE_TRACKER_CELEBRATE_PIN_MS = 3500;
    const STAGE_TRACKER_CELEBRATE_STAGGER_MS = 200;
    const STAGE_TRACKER_CELEBRATE_FLY_MS = 2000;
    const STAGE_TRACKER_ODOMETER_MS = 900;
    const STAGE_TRACKER_ODOMETER_SUB_MS = 600;
    // Cells a spinning wheel passes before it may settle: one full turn.
    const STAGE_TRACKER_ODOMETER_TURN = 10;
    // Half the glyph's box (--cue-flight-size, 2rem) in px, to centre it on the ✓ cell.
    const STAGE_TRACKER_CELEBRATE_GLYPH_HALF = 16;
    const STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS = 3;
    const STAGE_TRACKER_CELEBRATE_EXIT_PAD = 40;
    const STAGE_TRACKER_MERGED_TIP_MAX = 5;
    const STAGE_TRACKER_MERGED_LIST_MAX = 20;
    const STAGE_TRACKER_MERGE_BANNER_MS = 30000;
    const STAGE_TRACKER_MERGE_BANNER_MAX = 5;
    // The merge gate's minimum "Merging" dwell is the merge banner's lifetime (#15245).
    const STAGE_TRACKER_MERGING_HOLD_MS = STAGE_TRACKER_MERGE_BANNER_MS;
    function stageTrackerCelebrateSetting() {
      try { return localStorage.getItem(STAGE_TRACKER_CELEBRATE_KEY) !== '0'; } catch { return true; }
    }
    function stageTrackerSetCelebrate(on) {
      try { localStorage.setItem(STAGE_TRACKER_CELEBRATE_KEY, on ? '1' : '0'); } catch {}
      return !!on;
    }
    // Pure: the stored glyph id, defaulting; unknown ids fall back to the bee.
    function stageTrackerCelebrateGlyphId(raw) {
      const v = String(raw == null ? '' : raw).trim().toLowerCase();
      if (v in STAGE_TRACKER_CELEBRATE_GLYPHS) return v;
      if (v === '+1') return 'plus1';
      return STAGE_TRACKER_CELEBRATE_GLYPH_DEFAULT;
    }
    function stageTrackerCelebrateGlyph() {
      let raw = null;
      try { raw = localStorage.getItem(STAGE_TRACKER_CELEBRATE_GLYPH_KEY); } catch {}
      return STAGE_TRACKER_CELEBRATE_GLYPHS[stageTrackerCelebrateGlyphId(raw)];
    }
    function stageTrackerSetCelebrateGlyph(id) {
      const v = stageTrackerCelebrateGlyphId(id);
      try { localStorage.setItem(STAGE_TRACKER_CELEBRATE_GLYPH_KEY, v); } catch {}
      return v;
    }
    // Pure: local midnight (ms) for the given instant.
    function stageTrackerLocalMidnight(nowMs) { const d = new Date(nowMs); d.setHours(0, 0, 0, 0); return d.getTime(); }
    // Pure: a merge's identity for matching the server's list against a key
    // this tab made — the repo's short name and the number, so
    // "hive#15067" and "hivecommons/hive#15067" are one PR.
    function stageTrackerMergedIdent(repo, number) { const r = String(repo || ''); const i = r.lastIndexOf('/'); return (i >= 0 ? r.slice(i + 1) : r).toLowerCase() + '#' + (Number(number) || 0); }
    // Pure: the chip's state after one payload — { server, local }. server
    // is the payload's `merged` block (kept from before when a payload has
    // none); local maps PR key → { repo, number, at, title } for the merges
    // this tab saw land that the server's recent list does not carry yet.
    // fresh keys are added (stamped now) unless excluded (a flight still in
    // the air adds its key when it lands).
    function stageTrackerMergedNext(prev, d, fresh, nowMs, exclude) {
      prev = prev || {};
      exclude = exclude || {};
      const m = d && typeof d === 'object' && d.merged && typeof d.merged === 'object' && Number.isFinite(Number(d.merged.all_time)) ? d.merged : null;
      const server = m ? {
        today: Number(m.today) || 0, total_7d: Number(m.total_7d) || 0, total_30d: Number(m.total_30d) || 0, all_time: Number(m.all_time) || 0,
        recent: (Array.isArray(m.recent) ? m.recent : []).filter(e => e && e.repo && Number(e.number) > 0).map(e => ({ repo: String(e.repo), number: Number(e.number), title: String(e.title || ''), url: String(e.url || ''), merged_by: String(e.merged_by || ''), lane: String(e.lane || ''), issue: Number(e.issue) || 0, at: Date.parse(e.merged_at || '') || 0 })),
        tz: String(m.tz || ''), day_start: Date.parse(m.day_start || '') || 0,
      } : (prev.server || null);
      const known = {};
      for (const e of (server ? server.recent : [])) known[stageTrackerMergedIdent(e.repo, e.number)] = true;
      const local = {};
      for (const k of Object.keys(prev.local || {})) { const e = prev.local[k]; if (e && !known[stageTrackerMergedIdent(e.repo, e.number)]) local[k] = e; }
      const titles = stageTrackerPRTitles(d);
      for (const key of (Array.isArray(fresh) ? fresh : [])) {
        if (local[key] || exclude[key]) continue;
        const i = key.indexOf('/pr#');
        if (i < 0) continue;
        const repo = key.slice(0, i), number = Number(key.slice(i + 4)) || 0;
        if (known[stageTrackerMergedIdent(repo, number)]) continue;
        local[key] = { repo, number, at: nowMs, title: titles[key] || _stageTrackerPRTitle[key] || '' };
      }
      return { server, local };
    }
    // Pure: PR key → title for every PR one payload draws (own rows and
    // the linked PRs on issue rows), for the banner and the recent list.
    function stageTrackerPRTitles(d) {
      const out = {};
      for (const it of (d && Array.isArray(d.items) ? d.items : [])) {
        if (!it) continue;
        if (it.kind === 'pr' && it.title) out[stageTrackerPRKey(it.repo, it.number)] = String(it.title);
        for (const lp of (Array.isArray(it.linked_prs) ? it.linked_prs : [])) if (lp && Number(lp.number) > 0 && lp.title) out[stageTrackerPRKey(lp.repo || it.repo, lp.number)] = String(lp.title);
      }
      return out;
    }
    var _stageTrackerPRTitle = {};
    // Pure: what the chip shows for a state — the all-time number (server
    // + this tab's unlisted merges − flights still holding back), the
    // "N today · N this week" sub-text, the tooltip, and the recent list
    // (this tab's first, then the server's, newest first).
    function stageTrackerMergedText(state, nowMs, holdback) {
      state = state || {};
      const server = state.server || { today: 0, total_7d: 0, total_30d: 0, all_time: 0, recent: [], tz: '', day_start: 0 };
      const local = Object.keys(state.local || {}).map(k => state.local[k]).filter(Boolean).sort((a, b) => b.at - a.at);
      const dayStart = server.day_start || stageTrackerLocalMidnight(nowMs);
      const localToday = local.filter(e => e.at >= dayStart).length;
      const count = Math.max(0, server.all_time + local.length - (Number(holdback) || 0));
      const today = server.today + localToday;
      const week = server.total_7d + local.length;
      const sub = `${today} today · ${week} this week`;
      const recent = local.map(e => ({ repo: e.repo, number: e.number, title: e.title || '', url: '', merged_by: '', lane: '', at: e.at, live: true }))
        .concat(server.recent.map(e => Object.assign({}, e, { live: false }))).slice(0, STAGE_TRACKER_MERGED_LIST_MAX);
      const fmt = at => { const d = new Date(at); return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`; };
      const lines = recent.slice(0, STAGE_TRACKER_MERGED_TIP_MAX).map(e => `${e.repo}#${e.number}${e.title ? ' ' + e.title : ''} · ${e.at ? fmt(e.at) : ''}`.trim());
      const head = `Merged: ${count} all time\n${sub}${server.tz ? ' (' + server.tz + ')' : ''}`;
      const title = (lines.length ? head + '\n' + lines.join('\n') : head) + '\nClick for the recent list';
      return { count, today, week, sub, title, recent, allTime: server.all_time, total30d: server.total_30d + local.length, tz: server.tz };
    }
    // Pure: who merged, for a list line — the hive, a login, or nothing known.
    function stageTrackerMergedByText(by) {
      by = String(by || '').trim();
      if (!by) return '';
      return by === 'hive' ? 'by hive' : 'by @' + by;
    }
    // Pure: the popover's inner HTML.
    function stageTrackerMergedPopHtml(txt, nowMs) {
      const fmtAt = at => { if (!at) return ''; const d = new Date(at); const sameDay = stageTrackerLocalMidnight(at) === stageTrackerLocalMidnight(nowMs); const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`; return sameDay ? hm : `${typeof formatDashboardDate === 'function' ? formatDashboardDate(d, { month: 'short', day: 'numeric' }) : d.toISOString().slice(5, 10)} ${hm}`; };
      const rows = (txt.recent || []).map(e => {
        const meta = [esc(stageTrackerMergedByText(e.merged_by)), e.lane ? esc(e.lane) : '', e.at ? fmtAt(e.at) : ''].filter(Boolean).join(' · ');
        // Numbers and counts go through String(): esc() drops non-strings.
        const n = esc(String(e.number || ''));
        const num = e.url ? `<a href="${esc(e.url)}" target="_blank" rel="noopener">#${n}</a>` : `#${n}`;
        return `<li><span class="ham-merged-num" title="${esc(String(e.repo || ''))}#${n}">${num}</span><span class="ham-merged-title" title="${esc(e.title || '')}">${esc(e.title || e.repo)}</span><span class="ham-merged-meta">${meta}</span></li>`;
      }).join('');
      const head = `<div class="ham-merged-pop-head"><span>Recent merges</span><span class="ham-merged-pop-sum">${esc(String(txt.today))} today · ${esc(String(txt.week))} this week · ${esc(String(txt.total30d))} in 30 days</span></div>`;
      const body = rows ? `<ol class="ham-merged-list">${rows}</ol>` : `<div class="ham-empty">No merges recorded yet.</div>`;
      const foot = `<div class="ham-merged-pop-foot">${esc(String(txt.count))} all time${txt.tz ? ' · days roll at midnight ' + esc(txt.tz) : ''}. Every pull request the hive tracked that reached merged — by the hive, a person or another bot.</div>`;
      return head + body + foot;
    }
    var _stageTrackerMerged = { server: null, local: {} };
    var _stageTrackerMergedShown = 0;
    // Merges whose glyph is still on its way: the count holds them back
    // until stageTrackerCelebrateTick lands them.
    var _stageTrackerCelebratePending = {};
    // Stateful: fold one payload into the state and redraw the chip. The
    // count only bumps (pop + zap) through stageTrackerCelebrateTick; a
    // plain reload draws the server's number without a flourish.
    function stageTrackerMergedSync(d, fresh, nowMs) {
      Object.assign(_stageTrackerPRTitle, stageTrackerPRTitles(d));
      _stageTrackerMerged = stageTrackerMergedNext(_stageTrackerMerged, d, fresh, nowMs, _stageTrackerCelebratePending);
      stageTrackerMergedCountDraw(false);
    }
    // The node is cached on the function itself (not a module var) so a
    // lifted copy of it (tests) is self-contained; a detached chip stays
    // cached until a re-mount reattaches it.
    function stageTrackerMergedChip() {
      const have = stageTrackerMergedChip.node;
      if (have && have.isConnected !== false) return have;
      stageTrackerMergedChip.node = document.getElementById('activity-monitor-merged') || have || null;
      return stageTrackerMergedChip.node;
    }
    // Pure: how many in-flight merges the server already counts — the
    // chip holds those back so the landing is when it bumps.
    function stageTrackerMergedHoldback(state, pending) {
      const known = {};
      for (const e of ((state && state.server && state.server.recent) || [])) known[stageTrackerMergedIdent(e.repo, e.number)] = true;
      let n = 0;
      for (const key of Object.keys(pending || {})) { const i = key.indexOf('/pr#'); if (i > 0 && known[stageTrackerMergedIdent(key.slice(0, i), key.slice(i + 4))]) n++; }
      return n;
    }
    // Pure: the odometer columns for a change of text from `from` to `to`,
    // or null when nothing should roll — the texts split into different
    // static runs ("201 today · 730 this week" keeps its words), no number
    // rose, or every rise is a fall elsewhere. A column is { text } (a
    // static run of non-digits), a wheel that sits still — { steps: ['4'] },
    // an unchanged digit or one of a number that fell or held, drawn as a
    // drum cell like the rest — or a wheel that rolls, { steps: ['9', '0']
    // } — the digits the column passes, old to new. In
    // a number that rose, the first changed wheel rolls just the digits
    // between old and new (wrapping past 9 the way a carry does); every
    // wheel to its right — and the rightmost wheel always — spins through
    // at least one full turn (STAGE_TRACKER_ODOMETER_TURN cells) before it
    // settles, so a +1 is a visible roll and a carry's zeros whirr past.
    // A number that grew a digit rolls its new leading column in from a
    // blank.
    function stageTrackerOdometerPlan(from, to) {
      const a = String(from == null ? '' : from).match(/\d+|\D+/g) || [];
      const b = String(to == null ? '' : to).match(/\d+|\D+/g) || [];
      if (!b.length || a.length !== b.length) return null;
      const cols = [];
      let rose = false;
      for (let i = 0; i < b.length; i++) {
        const num = /^\d+$/.test(b[i]);
        if (num !== /^\d+$/.test(a[i])) return null;
        if (!num) { if (a[i] !== b[i]) return null; cols.push({ text: b[i] }); continue; }
        if (Number(b[i]) <= Number(a[i]) || a[i].length > b[i].length) { for (const d of b[i]) cols.push({ steps: [d] }); continue; }
        rose = true;
        const old = a[i].padStart(b[i].length, ' ');
        const last = b[i].length - 1;
        let first = -1;
        for (let j = 0; j <= last; j++) if (old[j] !== b[i][j] && first < 0) first = j;
        for (let j = 0; j <= last; j++) {
          const o = old[j], nw = b[i][j];
          const spin = j > first || j === last;
          if (o === nw && !spin) { cols.push({ steps: [nw] }); continue; }
          const steps = [o];
          if (o === ' ') steps.push(nw);
          else { let d = Number(o); do { d = (d + 1) % 10; steps.push(String(d)); } while (d !== Number(nw) || (spin && steps.length <= STAGE_TRACKER_ODOMETER_TURN)); }
          cols.push({ steps });
        }
      }
      return rose ? cols : null;
    }
    // Pure: the columns that draw `text` at rest — every digit its own
    // { steps: [d] } wheel, every non-digit run a { text } plate — the
    // shape stageTrackerOdometerPlan returns when something rolls, so one
    // renderer draws both states.
    function stageTrackerOdometerRest(text) {
      const cols = [];
      for (const run of (String(text == null ? '' : text).match(/\d+|\D+/g) || [])) {
        if (/^\d+$/.test(run)) for (const d of run) cols.push({ steps: [d] });
        else cols.push({ text: run });
      }
      return cols;
    }
    // Stateful: write `text` into el as an odometer. The element always
    // holds one aria-hidden .ham-odo drum of columns: a .ham-odo-col wheel
    // per digit (a .ham-odo-strip of .ham-odo-d cells; one cell at rest)
    // and a .ham-odo-static plate per run of other characters, so the
    // Merged counter looks like a mileage counter at rest, not only
    // mid-roll (hivecommons/hive#15398 follow-up: the previous overlay
    // existed for 900 ms and the resting value was plain text). The value
    // is on data-odo-value (what a redraw mid-roll compares against, and
    // what the node tests read) and aria-label (what a screen reader
    // reads; the drum's cells are hidden from it). A rise (per
    // stageTrackerOdometerPlan) gives each wheel that changes the digits it
    // passes and the class ham-odo-roll; the strip slides for `ms`
    // (STAGE_TRACKER_ODOMETER_MS unless given), then settles to its final
    // cell. Instant (drawn at rest) on the first write, under reduced
    // motion, and when the plan says nothing rolls. Returns whether a roll
    // started.
    function stageTrackerOdometerSet(el, text, ms) {
      if (!el) return false;
      text = String(text);
      ms = Number(ms) > 0 ? Number(ms) : STAGE_TRACKER_ODOMETER_MS;
      const prev = el.getAttribute('data-odo-value');
      if (prev === text) return false;
      el.setAttribute('data-odo-value', text);
      el.setAttribute('role', 'img');
      el.setAttribute('aria-label', text);
      if (el._odoTimer) { clearTimeout(el._odoTimer); el._odoTimer = 0; }
      const plan = prev == null || stageTrackerReducedMotion() ? null : stageTrackerOdometerPlan(prev, text);
      const wrap = stageTrackerOdometerDraw(el, plan || stageTrackerOdometerRest(text));
      wrap.style.setProperty('--odo-ms', ms + 'ms');
      el.classList.remove('ham-odo-rolling');
      if (!plan) return false;
      el.classList.add('ham-odo-rolling');
      void el.offsetWidth;
      wrap.classList.add('ham-odo-run');
      el._odoTimer = setTimeout(() => {
        el._odoTimer = 0;
        el.classList.remove('ham-odo-rolling');
        stageTrackerOdometerDraw(el, stageTrackerOdometerRest(el.getAttribute('data-odo-value')));
      }, ms + 80);
      return true;
    }
    // Stateful: (re)draw el's drum from columns; returns the .ham-odo wrap.
    function stageTrackerOdometerDraw(el, cols) {
      let wrap = el._odoWrap;
      if (!wrap || wrap.parentNode !== el) {
        // The markup's placeholder text ("0") goes; the drum is the content.
        el.textContent = '';
        wrap = document.createElement('span');
        wrap.className = 'ham-odo';
        wrap.setAttribute('aria-hidden', 'true');
        el.appendChild(wrap);
        el._odoWrap = wrap;
      }
      wrap.classList.remove('ham-odo-run');
      while (wrap.children.length) wrap.children[0].remove();
      for (const c of cols) {
        const col = document.createElement('span');
        if (!c.steps) { col.className = 'ham-odo-static'; col.textContent = c.text; wrap.appendChild(col); continue; }
        col.className = c.steps.length > 1 ? 'ham-odo-col ham-odo-roll' : 'ham-odo-col';
        const strip = document.createElement('span');
        strip.className = 'ham-odo-strip';
        for (const d of c.steps) { const cell = document.createElement('span'); cell.className = 'ham-odo-d'; cell.textContent = d; strip.appendChild(cell); }
        strip.style.setProperty('--odo-shift', (-(c.steps.length - 1) * 100 / c.steps.length).toFixed(3) + '%');
        col.appendChild(strip);
        wrap.appendChild(col);
      }
      return wrap;
    }
    // Stateful: seed an element that has not been written yet with the
    // value it showed last time (so the first real write rolls up from
    // it). Only a value the new text would roll from is taken; anything
    // else leaves the first write instant. Returns whether it seeded.
    function stageTrackerOdometerSeed(el, last, next) {
      if (!el || last == null || el.getAttribute('data-odo-value') != null) return false;
      if (!stageTrackerOdometerPlan(String(last), String(next))) return false;
      el.setAttribute('data-odo-value', String(last));
      el.setAttribute('role', 'img');
      el.setAttribute('aria-label', String(last));
      stageTrackerOdometerDraw(el, stageTrackerOdometerRest(String(last)));
      return true;
    }
    // Pure: the chip's remembered values ({ n, sub }) from a sessionStorage
    // string, or null when absent or malformed.
    function stageTrackerMergedLastParse(raw) {
      if (!raw) return null;
      try { const v = JSON.parse(raw); if (v && typeof v === 'object' && /^\d+$/.test(String(v.n)) && typeof v.sub === 'string') return { n: String(v.n), sub: v.sub }; } catch {}
      return null;
    }
    function stageTrackerMergedLastLoad() {
      try { return stageTrackerMergedLastParse(sessionStorage.getItem(STAGE_TRACKER_MERGED_LAST_KEY)); } catch { return null; }
    }
    function stageTrackerMergedLastSave(n, sub) {
      try { sessionStorage.setItem(STAGE_TRACKER_MERGED_LAST_KEY, JSON.stringify({ n: String(n), sub: String(sub) })); } catch {}
    }
    // Stateful: whether a celebration glyph is in the air or about to be
    // (stageTrackerCelebrateMerges staggers the launches; a merge is
    // pending from the payload that brought it). The drum and the zap
    // wait on this; the flight's own end — its animationend, or
    // stageTrackerAfter's timer at STAGE_TRACKER_CELEBRATE_FLY_MS when the
    // event never fires — is what clears it.
    function stageTrackerMergedInFlight() { return _stageTrackerFlights > 0 || Object.keys(_stageTrackerCelebratePending).length > 0; }
    function stageTrackerMergedCountDraw(tick) {
      const chip = stageTrackerMergedChip();
      const now = Date.now();
      const txt = stageTrackerMergedText(_stageTrackerMerged, now, stageTrackerMergedHoldback(_stageTrackerMerged, _stageTrackerCelebratePending));
      // Never backwards within a tab: a server that has not caught up yet
      // (or lost a merge) does not make the number drop.
      if (txt.count > _stageTrackerMergedShown || tick) _stageTrackerMergedShown = Math.max(_stageTrackerMergedShown, txt.count);
      const shown = _stageTrackerMergedShown;
      if (!chip) return;
      const n = chip.querySelector('.ham-merged-n'), sub = chip.querySelector('.ham-merged-sub');
      // First draw of this page: roll up from what the tab showed before
      // the reload, when that is behind the number we have now.
      const first = !n || n.getAttribute('data-odo-value') == null;
      if (n && first) {
        const last = stageTrackerMergedLastLoad();
        if (last) { stageTrackerOdometerSeed(n, last.n, shown); stageTrackerOdometerSeed(sub, last.sub, txt.sub); }
      }
      // A glyph in the air owns the drum (issue 15568): neither number
      // moves under a flight — not the day/week line, not a total the
      // server already raised, not a merge over the flight cap. The last
      // landing (stageTrackerCelebrateTick) draws once, to the final
      // values, so the bee lands and then the drum rolls.
      const hold = !first && stageTrackerMergedInFlight();
      if (!hold) {
        stageTrackerOdometerSet(n, shown);
        stageTrackerOdometerSet(sub, txt.sub, STAGE_TRACKER_ODOMETER_SUB_MS);
        if (shown > 0) stageTrackerMergedLastSave(shown, txt.sub);
      }
      const title = txt.title.replace(/^Merged: \d+ all time/, `Merged: ${shown} all time`);
      if (chip.title !== title) chip.title = title;
      if (tick && !hold) {
        chip.classList.remove('cue-tick'); void chip.offsetWidth; stageTrackerTransient(chip, 'cue-tick', 320);
        const live = document.getElementById('activity-monitor-merged-live');
        if (live) live.textContent = `Merged: ${shown} all time, ${txt.sub}`;
      }
      stageTrackerMergedPopDraw(txt, now);
    }
    // Stateful: redraw the popover when it is open.
    function stageTrackerMergedPopDraw(txt, nowMs) {
      const pop = document.getElementById('activity-monitor-merged-pop');
      if (!pop || pop.hidden) return;
      const html = stageTrackerMergedPopHtml(txt || stageTrackerMergedText(_stageTrackerMerged, nowMs || Date.now(), 0), nowMs || Date.now());
      if (pop.innerHTML !== html) pop.innerHTML = html;
    }
    function hamMergedToggle(open) {
      const pop = document.getElementById('activity-monitor-merged-pop');
      const chip = stageTrackerMergedChip();
      if (!pop) return false;
      const want = typeof open === 'boolean' ? open : !!pop.hidden;
      pop.hidden = !want;
      if (chip) chip.setAttribute('aria-expanded', want ? 'true' : 'false');
      if (want) stageTrackerMergedPopDraw(null, Date.now());
      return want;
    }
    function hamMergedOpen() {
      hamMergedToggle(true);
      const bar = document.getElementById('activity-monitor-sticky');
      if (bar && typeof bar.scrollIntoView === 'function' && !stageTrackerRectInView(bar.getBoundingClientRect(), window.innerWidth || 0, window.innerHeight || 0)) bar.scrollIntoView({ block: 'start', behavior: stageTrackerReducedMotion() ? 'auto' : 'smooth' });
    }
    document.addEventListener('click', function(e) {
      const pop = document.getElementById('activity-monitor-merged-pop');
      if (!pop || pop.hidden) return;
      if (e.target && typeof e.target.closest === 'function' && e.target.closest('.merge-banner')) return;
      const bar = document.getElementById('activity-monitor-sticky');
      if (bar && e.target && typeof e.target.closest === 'function' && e.target.closest('#activity-monitor-sticky')) return;
      hamMergedToggle(false);
    });
    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      const pop = document.getElementById('activity-monitor-merged-pop');
      if (pop && !pop.hidden) { hamMergedToggle(false); const chip = stageTrackerMergedChip(); if (chip && chip.focus) chip.focus(); }
    });
    // The glyph has landed (or left the screen, or faded): count the PR,
    // pop the chip, zap — once the last glyph is down; a landing while
    // others still fly is folded into theirs (issue 15568).
    function stageTrackerCelebrateTick(key, preview) {
      delete _stageTrackerCelebratePending[key];
      const i = String(key).indexOf('/pr#');
      if (!preview && i > 0) {
        const repo = key.slice(0, i), number = Number(key.slice(i + 4)) || 0;
        const known = ((_stageTrackerMerged.server && _stageTrackerMerged.server.recent) || []).some(e => stageTrackerMergedIdent(e.repo, e.number) === stageTrackerMergedIdent(repo, number));
        if (!known && !_stageTrackerMerged.local[key]) _stageTrackerMerged.local[key] = { repo, number, at: Date.now(), title: _stageTrackerPRTitle[key] || '' };
      }
      stageTrackerMergedCountDraw(true);
      if (!stageTrackerMergedInFlight() && stageTrackerSoundGate()) stageTrackerPlayZap(1);
    }
    var _stageTrackerCelebrating = {};
    var _stageTrackerFlights = 0;
    // Pure: the PR keys a row draws, space-separated — its own key for a
    // PR row, the covering PRs for an issue row (one inline, several as
    // sub-rows). Stamped as data-ham-prs so a merged PR can be traced to
    // the row it was drawn on even when it had no sub-row of its own.
    function stageTrackerRowPRKeys(item) {
      if (!item) return '';
      if (item.kind === 'pr') return stageTrackerPRKey(item.repo, item.number);
      return (Array.isArray(item.linked_prs) ? item.linked_prs : []).filter(lp => lp && Number(lp.number) > 0).map(lp => stageTrackerPRKey(lp.repo || item.repo, lp.number)).join(' ');
    }
    // PR key → the row key it was last drawn on (from the previous payload's
    // nested_under / linked_prs), for a merge whose row left the DOM
    // between two renders.
    var _stageTrackerPRHost = {};
    function stageTrackerPRHosts(d) {
      const out = {};
      for (const it of (d && Array.isArray(d.items) ? d.items : [])) {
        if (!it) continue;
        if (it.kind === 'pr') out[stageTrackerPRKey(it.repo, it.number)] = Number(it.nested_under) > 0 ? stageTrackerItemKey({ repo: it.repo, kind: 'issue', number: it.nested_under }) : stageTrackerItemKey(it);
        for (const lp of (Array.isArray(it.linked_prs) ? it.linked_prs : [])) if (lp && Number(lp.number) > 0) out[stageTrackerPRKey(lp.repo || it.repo, lp.number)] = stageTrackerItemKey(it);
      }
      return out;
    }
    function stageTrackerCelebrateRowKey(prKey, el) {
      if (!el || typeof el.querySelector !== 'function') return '';
      const own = el.querySelector(`[data-ham-key="${CSS.escape(prKey)}"]`);
      if (own) return prKey;
      const sub = el.querySelector(`[data-ham-sub-key="${CSS.escape(prKey)}"]`);
      const row = sub && typeof sub.closest === 'function' ? sub.closest('[data-ham-key]') : null;
      if (row && row.dataset.hamKey) return row.dataset.hamKey;
      // A single covering PR is drawn inline on its issue's row: no sub-row,
      // so find the row by the PR keys it stamps (then by the last payload).
      const host = el.querySelector(`[data-ham-prs~="${CSS.escape(prKey)}"][data-ham-key]`);
      if (host && host.dataset.hamKey) return host.dataset.hamKey;
      const k = _stageTrackerPRHost[prKey];
      return k && el.querySelector(`[data-ham-key="${CSS.escape(k)}"]`) ? k : '';
    }
    // Stateful: celebrate every freshly merged PR, staggered. A PR whose
    // row is not on the board (filtered out, other page) still bumps the
    // count and gets its banner; its row is celebrated before removal only
    // when it is drawn.
    function stageTrackerCelebrateMerges(keys) {
      const el = document.getElementById('activity-monitor-panel');
      const now = Date.now();
      keys.forEach((key, i) => {
        const rowKey = stageTrackerCelebrateRowKey(key, el);
        const startAt = now + i * STAGE_TRACKER_CELEBRATE_STAGGER_MS;
        if (rowKey) _stageTrackerCelebrating[rowKey] = { key, kind: 'merged', until: startAt + STAGE_TRACKER_CELEBRATE_PIN_MS };
        const number = Number(key.slice(key.indexOf('/pr#') + 4)) || 0;
        stageTrackerMergeBanner({ key, rowKey, number, repo: key.slice(0, key.indexOf('/pr#')), at: now });
        setTimeout(() => {
          const row = rowKey && el ? el.querySelector(`[data-ham-key="${CSS.escape(rowKey)}"]`) : null;
          if (row) celebrateRow(row, { kind: 'merged', key, number });
          else stageTrackerCelebrateTick(key);
        }, i * STAGE_TRACKER_CELEBRATE_STAGGER_MS);
      });
    }
    // Pure: the rows a render must keep although the payload dropped them —
    // every celebrating row still inside its pin window, as the node it
    // already has (its HTML is replayed so the reconcile keeps it).
    function stageTrackerPinnedRows(el, rows, nowMs) {
      const keys = Object.keys(_stageTrackerCelebrating);
      if (!keys.length || !el) return rows;
      const have = {};
      for (const r of rows) have[r.key] = true;
      const out = rows.slice();
      for (const k of keys) {
        const c = _stageTrackerCelebrating[k];
        if (c.until <= nowMs) { delete _stageTrackerCelebrating[k]; continue; }
        if (have[k]) continue;
        const node = el.querySelector(`[data-ham-key="${CSS.escape(k)}"]`);
        if (!node) { delete _stageTrackerCelebrating[k]; continue; }
        out.push({ key: k, html: stageTrackerRowSrcOf(node) || node.outerHTML, pinned: true });
        stageTrackerScheduleRelease(c.until - nowMs);
      }
      return out;
    }
    var _stageTrackerReleaseTimer = 0;
    // Once the pin window ends a render lets the row take its normal exit.
    function stageTrackerScheduleRelease(inMs) {
      if (_stageTrackerReleaseTimer) return;
      _stageTrackerReleaseTimer = setTimeout(() => { _stageTrackerReleaseTimer = 0; renderStageTracker(); }, Math.max(50, inMs + 50));
    }
    // Pure: whether a box is wholly inside the viewport.
    function stageTrackerRectInView(r, vw, vh) {
      return !!r && r.width > 0 && r.height > 0 && r.top >= 0 && r.left >= 0 && r.top + r.height <= vh && r.left + r.width <= vw;
    }
    // Pure: where the glyph flies from the ✓ cell's box — to the chip's
    // centre when the chip is inside the viewport (lands: true), else
    // straight up and out through the top edge (lands: false).
    function stageTrackerFlightTarget(from, chipRect, vw, vh) {
      const fx = from.left + from.width / 2, fy = from.top + from.height / 2;
      if (stageTrackerRectInView(chipRect, vw, vh)) {
        return { lands: true, dx: chipRect.left + chipRect.width / 2 - fx, dy: chipRect.top + chipRect.height / 2 - fy };
      }
      return { lands: false, dx: 0, dy: -(fy + STAGE_TRACKER_CELEBRATE_EXIT_PAD) };
    }
    // Reusable: celebrate one row in place. kind: 'merged' | 'closed-by-merge'.
    // Glow the ✓ cell, write the event line, fly the glyph (or fade it in
    // place under reduced motion; bump at once over the flight cap), then
    // let the row go when its pin window ends.
    function celebrateRow(rowEl, opts) {
      opts = opts || {};
      if (!rowEl || !rowEl.classList) return { flew: false };
      const kind = opts.kind === 'closed-by-merge' ? 'closed-by-merge' : 'merged';
      const key = opts.key || rowEl.dataset.hamKey || '';
      const preview = !!opts.preview;
      rowEl.classList.add('cue-merged');
      if (preview) setTimeout(() => rowEl.classList.remove('cue-merged'), STAGE_TRACKER_CELEBRATE_PIN_MS);
      else {
        const text = kind === 'merged' ? `merged${opts.number ? ' #' + opts.number : ''} · by hive` : `closed by merge${opts.number ? ' of #' + opts.number : ''}`;
        stageTrackerNoteOwnerEvent(rowEl.dataset.hamKey, text, Date.now());
        _stageTrackerEvents[rowEl.dataset.hamKey].who = 'hive';
        stageTrackerEventLineInject(rowEl, rowEl.dataset.hamKey, text);
      }
      if (kind !== 'merged') return { flew: false };
      const cap = rowEl.querySelector('.ham-seg.cap');
      if (!cap || _stageTrackerFlights >= STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS || typeof cap.getBoundingClientRect !== 'function') {
        stageTrackerCelebrateTick(key, preview);
        return { flew: false, lands: false };
      }
      const reduced = stageTrackerReducedMotion();
      const chip = stageTrackerMergedChip();
      const from = cap.getBoundingClientRect();
      const vw = window.innerWidth || 0, vh = window.innerHeight || 0;
      const to = reduced ? { lands: false, dx: 0, dy: 0 } : stageTrackerFlightTarget(from, chip && typeof chip.getBoundingClientRect === 'function' ? chip.getBoundingClientRect() : null, vw, vh);
      const glyph = document.createElement('span');
      glyph.className = 'cue-flight' + (reduced ? ' cue-flight-fade' : '');
      glyph.setAttribute('aria-hidden', 'true');
      glyph.textContent = stageTrackerCelebrateGlyph();
      glyph.style.setProperty('left', (from.left + from.width / 2 - STAGE_TRACKER_CELEBRATE_GLYPH_HALF) + 'px');
      glyph.style.setProperty('top', (from.top + from.height / 2 - STAGE_TRACKER_CELEBRATE_GLYPH_HALF) + 'px');
      glyph.style.setProperty('--cue-dx', to.dx + 'px');
      glyph.style.setProperty('--cue-dy', to.dy + 'px');
      glyph.style.setProperty('--cue-end-opacity', to.lands ? '0.25' : '1');
      glyph.style.setProperty('--cue-fly-ms', STAGE_TRACKER_CELEBRATE_FLY_MS + 'ms');
      document.body.appendChild(glyph);
      _stageTrackerFlights++;
      stageTrackerAfter(glyph, 'animationend', STAGE_TRACKER_CELEBRATE_FLY_MS, () => { glyph.remove(); _stageTrackerFlights--; stageTrackerCelebrateTick(key, preview); });
      return { flew: !reduced, lands: to.lands, faded: reduced };
    }
    // Pure: the banner's text pieces — "#15067 merged", the title, and
    // "(lane, 17:42)".
    // Pure: the banner's PR number as a string — the number field, else the
    // nested pr record, else the GitHub URL, else the PR key. '' when none
    // (so the head never reads a bare "#"). esc() drops non-strings, which
    // is how a numeric field once rendered as "# merged".
    function stageTrackerMergeBannerNumber(b) {
      if (!b) return '';
      const n = Number(b.number) || Number(b.pr_number) || Number(b.pr && b.pr.number) || 0;
      if (n > 0) return String(n);
      const u = String(b.url || (b.pr && b.pr.url) || '').match(/\/pull\/(\d+)/);
      if (u) return u[1];
      const k = String(b.key || '').match(/\/pr#(\d+)$/);
      return k && Number(k[1]) > 0 ? k[1] : '';
    }
    function stageTrackerMergeBannerText(b, nowMs) {
      const d = new Date(b.at || nowMs);
      const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
      const who = stageTrackerMergedByText(b.merged_by);
      const paren = [b.lane || '', who, hm].filter(Boolean).join(', ');
      const num = stageTrackerMergeBannerNumber(b);
      return { number: num, head: `${num ? '#' + num + ' ' : ''}merged${b.preview ? ' (test)' : ''}`, text: `${b.title || b.repo || ''}${paren ? ' (' + paren + ')' : ''}`.trim() };
    }
    // Stateful: a persistent banner, bottom right, for one merge — stays
    // until dismissed or STAGE_TRACKER_MERGE_BANNER_MS, stacked when
    // several, with a "show row" link while the row is on the board.
    function stageTrackerMergeBanner(b) {
      if (!document.body) return null;
      let box = document.getElementById('merge-banners');
      if (!box) { box = document.createElement('div'); box.id = 'merge-banners'; box.className = 'merge-banners'; box.setAttribute('aria-live', 'polite'); document.body.appendChild(box); }
      const recent = ((_stageTrackerMerged.server && _stageTrackerMerged.server.recent) || []).find(e => stageTrackerMergedIdent(e.repo, e.number) === stageTrackerMergedIdent(b.repo, b.number));
      const info = Object.assign({ title: _stageTrackerPRTitle[b.key] || '', merged_by: '', lane: '', url: '' }, recent || {}, b);
      const num = stageTrackerMergeBannerNumber(info);
      // A live merge the ledger has not listed yet still links: the key carries owner/name.
      if (!info.url && info.repo && info.repo.indexOf('/') > 0 && num && !info.preview) info.url = `https://github.com/${info.repo}/pull/${num}`;
      const t = stageTrackerMergeBannerText(info, Date.now());
      const el = document.createElement('div');
      el.className = 'merge-banner';
      el.setAttribute('role', 'status');
      if (b.rowKey) el.setAttribute('data-ham-row', b.rowKey);
      el.innerHTML = `<span class="merge-banner-ico" aria-hidden="true">✓</span><div class="merge-banner-body"><div class="merge-banner-head">${num ? `<span class="merge-banner-num">#${esc(num)}</span> ` : ''}merged${info.preview ? ' <span class="text-muted">(test)</span>' : ''}</div><div class="merge-banner-text" title="${esc(t.text)}">${esc(t.text)}</div><div class="merge-banner-links">${b.rowKey ? `<button type="button" data-action="hamBannerShowRow" data-arg0="${esc(b.rowKey)}" data-stop="1">Show row</button>` : ''}${info.url ? `<a href="${esc(info.url)}" target="_blank" rel="noopener">Open on GitHub</a>` : ''}<button type="button" data-action="hamMergedOpen" data-stop="1">All merges</button></div></div><button type="button" class="merge-banner-close" data-action="hamBannerDismiss" data-arg-types="t" data-stop="1" aria-label="Dismiss">×</button>`;
      box.appendChild(el);
      while (box.children.length > STAGE_TRACKER_MERGE_BANNER_MAX) box.children[0].remove();
      el._timer = setTimeout(() => stageTrackerMergeBannerDismiss(el), STAGE_TRACKER_MERGE_BANNER_MS);
      return el;
    }
    function stageTrackerMergeBannerDismiss(el) {
      if (!el || !el.parentNode) return;
      if (el._timer) { clearTimeout(el._timer); el._timer = 0; }
      if (stageTrackerReducedMotion()) { el.remove(); return; }
      el.classList.add('merge-banner-out');
      stageTrackerAfter(el, 'animationend', 180, () => el.remove());
    }
    function hamBannerDismiss(btn) {
      const el = btn && typeof btn.closest === 'function' ? btn.closest('.merge-banner') : null;
      stageTrackerMergeBannerDismiss(el);
    }
    // "Show row": scroll the row into view and glow it again; when it has
    // already left the board, say so and offer the list instead.
    function hamBannerShowRow(rowKey) {
      const el = document.getElementById('activity-monitor-panel');
      const row = el && rowKey ? el.querySelector(`[data-ham-key="${CSS.escape(rowKey)}"]`) : null;
      if (!row) { showToast('That row has left the board — the merge is in the Merged list.', 'info'); hamMergedToggle(true); return false; }
      if (typeof row.scrollIntoView === 'function') row.scrollIntoView({ block: 'center', behavior: stageTrackerReducedMotion() ? 'auto' : 'smooth' });
      row.classList.remove('cue-merged'); void row.offsetWidth; stageTrackerTransient(row, 'cue-merged', 1600);
      return true;
    }
    // Write (or replace) the row's event line in place, so a pinned row
    // whose HTML is never re-rendered still shows what just happened.
    function stageTrackerEventLineInject(rowEl, key, text) {
      if (!rowEl || typeof rowEl.querySelector !== 'function') return;
      let line = rowEl.querySelector('.cue-event');
      const html = stageTrackerEventHtml(key, { events: _stageTrackerEvents, nowMs: Date.now() });
      if (!html) return;
      const tpl = document.createElement('template');
      tpl.innerHTML = html;
      if (line) line.replaceWith(tpl.content.firstElementChild);
      else { const anchor = rowEl.querySelector('.ham-strip-row'); if (anchor && anchor.parentNode) anchor.parentNode.insertBefore(tpl.content.firstElementChild, anchor.nextSibling); else rowEl.appendChild(tpl.content.firstElementChild); }
      stageTrackerCueAgeSync(rowEl, Date.now());
    }
    // A closed issue is not an event the tracker payload carries today
    // (items only ever list open work and the audit side list is pr_merged
    // only); when a recently_closed list lands, mark those issue keys true
    // in stageTrackerMergeState and they will zap through the same path.

