    function stageTrackerPRKey(repo, number) { return (repo || '') + '/pr#' + (Number(number) || 0); }
    function stageTrackerItemKey(it) { return (it.repo || '') + '/' + (it.kind || '') + '#' + (Number(it.number) || 0); }

    // Pure: PR key → merged? for one payload. Issues are not tracked. A
    // recently_merged repo given by its short name is resolved against the
    // owner/name of the items (any kind, any repo) when exactly one matches.
    function stageTrackerMergeState(d, prev) {
      const out = {};
      if (!d || typeof d !== 'object') return out;
      const byShort = {};
      // A dropped row (including one from before a reload) still teaches
      // the short repo alias used by the audit side list.
      for (const key of Object.keys(prev || {})) {
        const repo = key.slice(0, key.lastIndexOf('/pr#'));
        const slash = repo.lastIndexOf('/');
        if (slash > 0) { const short = repo.slice(slash + 1); byShort[short] = (short in byShort && byShort[short] !== repo) ? '' : repo; }
      }
      for (const it of (Array.isArray(d.items) ? d.items : [])) {
        if (!it) continue;
        // Every item of every repo (issues too) teaches the short → full
        // map, so a repo whose last open PR just merged still resolves.
        const repo = String(it.repo || '');
        const slash = repo.lastIndexOf('/');
        if (slash > 0) { const short = repo.slice(slash + 1); byShort[short] = (short in byShort && byShort[short] !== repo) ? '' : repo; }
        for (const lp of (Array.isArray(it.linked_prs) ? it.linked_prs : [])) {
          if (!lp || !Number(lp.number)) continue;
          const pr = typeof stageTrackerLinkedPRItem === 'function' ? stageTrackerLinkedPRItem(it, lp) : Object.assign({ repo: it.repo }, lp);
          const prRepo = String(pr.repo || ''), at = prRepo.lastIndexOf('/');
          if (at > 0) { const short = prRepo.slice(at + 1); byShort[short] = (short in byShort && byShort[short] !== prRepo) ? '' : prRepo; }
          if (lp.state === 'closed' && Number(lp.stage) < STAGE_TRACKER_TERMINAL) continue;
          out[stageTrackerPRKey(pr.repo, pr.number)] = (Number(pr.stage) || 0) >= STAGE_TRACKER_TERMINAL;
        }
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

    // Pure: merged facts are irreversible; dropped open observations remain
    // comparable. A PR the snapshot dropped before the audit log
    // recorded its merge stays comparable until recently_merged settles it
    // (or, when it was closed unmerged, for the life of the tab — a few keys).
    function stageTrackerCarryOpen(prev, next) {
      const out = Object.assign({}, next || {});
      if (!prev) return out;
      for (const k of Object.keys(prev)) {
        if (prev[k] === true) out[k] = true;
        else if (prev[k] === false && !(k in out)) out[k] = false;
      }
      return out;
    }

    // Pure: PR keys open in prev and merged in next, sorted. No prev → none.
    function stageTrackerNewlyMerged(prev, next) {
      if (!prev || !next) return [];
      return Object.keys(next).filter(k => next[k] === true && prev[k] === false).sort();
    }

    const STAGE_TRACKER_MERGE_SESSION_KEY = 'hive.tracker.merge-session.v1';
    // Shape of a stageTrackerPRKey() key as stored in the session record.
    const STAGE_TRACKER_MERGE_SESSION_PR_KEY_RE = /^.+\/pr#[1-9][0-9]*$/;
    var _stageTrackerMergeSessionLoaded = false;
    // Persist both the last observation and the delivered keys: saving only
    // delivered keys loses an open→merged transition across a reload. The
    // first visit still seeds history silently rather than replaying it.
    function stageTrackerReconcileMerges(d) {
      if (!_stageTrackerMergeSessionLoaded) {
        _stageTrackerMergeSessionLoaded = true;
        try {
          const saved = JSON.parse(sessionStorage.getItem(STAGE_TRACKER_MERGE_SESSION_KEY));
          if (saved && saved.state && typeof saved.state === 'object' && !Array.isArray(saved.state)) {
            const state = {}, delivered = {};
            for (const key of Object.keys(saved.state)) {
              if (!STAGE_TRACKER_MERGE_SESSION_PR_KEY_RE.test(key) || typeof saved.state[key] !== 'boolean') continue;
              state[key] = saved.state[key];
            }
            for (const key of Object.keys(saved.delivered || {})) {
              if (STAGE_TRACKER_MERGE_SESSION_PR_KEY_RE.test(key) && saved.delivered[key] === true) delivered[key] = true;
            }
            _stageTrackerMergeState = state;
            _stageTrackerZapped = delivered;
          }
        } catch {} // Storage disabled/corrupt: keep the in-memory path.
      }
      stageTrackerNoteMerges(d);
      try { sessionStorage.setItem(STAGE_TRACKER_MERGE_SESSION_KEY, JSON.stringify({ state: _stageTrackerMergeState, delivered: _stageTrackerZapped })); } catch {}
    }

    function stageTrackerNoteMerges(d) {
      const nowMs = Date.now();
      _stageTrackerMergingHoldMs = stageTrackerMergingHoldMsOf(d, _stageTrackerMergingHoldMs);
      const raw = stageTrackerCarryOpen(_stageTrackerMergeState, stageTrackerMergeState(d, _stageTrackerMergeState));
      // The merge gate's hold (hivecommons/hive#15245, #15808) keeps a
      // row the payload dropped mid-merge on the board at "Merging" until
      // the merged fact arrives (then the merge is fresh at once) or the
      // hold window passes.
      _stageTrackerMergingHold = stageTrackerMergingHoldNext(_stageTrackerMergingHold, d, raw, nowMs);
      const until = stageTrackerMergingHoldUntil(_stageTrackerMergingHold, nowMs);
      if (until) stageTrackerMergingHoldSchedule(until - nowMs);
      const prevItems = _stageTrackerPRItems;
      _stageTrackerPRItems = stageTrackerPRItemsOf(d);
      Object.assign(_stageTrackerPRLast, _stageTrackerPRItems);
      const next = raw;
      const fresh = stageTrackerNewlyMerged(_stageTrackerMergeState, next).filter(k => !_stageTrackerZapped[k]);
      // A PR that left the board closed, not merged (recently_closed, the
      // closed twin of recently_merged): say so once, neutrally — no bee,
      // no count — instead of letting the row vanish in silence.
      const closed = stageTrackerNewlyClosed(d, _stageTrackerMergeState, next, _stageTrackerPRItems, _stageTrackerClosedNoted);
      const closedItems = closed.map(k => prevItems[k] || _stageTrackerPRLast[k]);
      closed.forEach(k => { _stageTrackerClosedNoted[k] = true; delete next[k]; delete _stageTrackerPRLast[k]; });
      _stageTrackerMergeState = next;
      // A merged PR is no longer in this payload, so its last host survives.
      if (typeof stageTrackerPRHosts === 'function') Object.assign(_stageTrackerPRHost, stageTrackerPRHosts(d));
      if (typeof stageTrackerTerminalObserve === 'function') stageTrackerTerminalObserve(d, fresh, nowMs);
      // With the celebration on, each fresh merge is counted (and the zap
      // plays) when its token lands on the counter; off, both happen now.
      const celebrate = fresh.length && typeof stageTrackerCelebrateSetting === 'function' && stageTrackerCelebrateSetting();
      if (celebrate) fresh.forEach(k => { _stageTrackerCelebratePending[k] = true; });
      if (typeof stageTrackerMergedSync === 'function') stageTrackerMergedSync(d, fresh, Date.now());
      if (typeof stageTrackerQueueSync === 'function') stageTrackerQueueSync(d && d.items, Date.now(), d && d.base_preparation);
      if (typeof stageTrackerClosedToast === 'function') closed.forEach((k, i) => stageTrackerClosedToast(k, closedItems[i]));
      if (!fresh.length) return;
      fresh.forEach(k => { _stageTrackerZapped[k] = true; });
      if (celebrate) { stageTrackerCelebrateMerges(fresh); return; }
      if (stageTrackerSoundGate()) stageTrackerPlayZap(Math.min(fresh.length, STAGE_TRACKER_SOUND_MAX_PLAYS));
    }

    /* ---- Merge gate: "Merging" bridged until the merged fact (#15245, #15808) ----
       Operator (#15245): "I have seen some items go straight to merge
       without showing in the Merge gate column as 'merging'." The sweep
       merges inside one pass, so a PR can go from 060/090/095 to merged
       between two polls. The payload's merge_gate says merging (with
       merging_since) while the merge call is in flight; when the payload
       then drops the PR before the merged fact (recently_merged / the
       ledger) has landed, the SPA keeps a hold per PR — from the moment
       it saw merging — and until the hold window has passed the row stays
       on the board at 100 "Merging" (its last item, synthesised), the
       gate saying it is waiting for GitHub to confirm.
       Operator (#15808): the hold used to be a fixed 30 s mask over the
       merged fact itself — "Merging · merge landed" sat there while the
       bee waited — and a row that vanished without the SPA seeing merged
       never celebrated. Now the hold only bridges the gap: the moment the
       merged fact is in a payload the hold drops, the merge is fresh and
       the celebration launches (bee, then the odometer on its landing,
       #15763). The window is dashboard.stage_tracker.merged_hold_s
       (payload merged_hold_ms; default 8 s, floor 3 s), a ceiling, not a
       delay. A refused merge (merge-failed) drops the hold at once. All
       pure except the schedule/release pair; run under node by
       stage_tracker_merging_hold_test.go. */
    const STAGE_TRACKER_MERGE_QUEUE = 100;
    const STAGE_TRACKER_MERGING_HOLD_DEFAULT_MS = 8000;
    const STAGE_TRACKER_MERGING_HOLD_MIN_MS = 3000;
    var _stageTrackerMergingHoldMs = STAGE_TRACKER_MERGING_HOLD_DEFAULT_MS;
    var _stageTrackerMergingHold = {};
    var _stageTrackerPRItems = {};
    // PR key → the last item ever seen for it (survives the payload
    // dropping it: the closed toast's reason, the hold's row).
    var _stageTrackerPRLast = {};
    var _stageTrackerClosedNoted = {};
    var _stageTrackerMergingHoldTimer = 0;
    // Pure: the hold window from a payload's merged_hold_ms (clamped to the
    // floor), else the value in force.
    function stageTrackerMergingHoldMsOf(d, cur) {
      const n = d && typeof d === 'object' ? Number(d.merged_hold_ms) : 0;
      if (!(n > 0)) return cur || STAGE_TRACKER_MERGING_HOLD_DEFAULT_MS;
      return Math.max(STAGE_TRACKER_MERGING_HOLD_MIN_MS, n);
    }
    function stageTrackerMergingHoldMs() { return _stageTrackerMergingHoldMs || STAGE_TRACKER_MERGING_HOLD_DEFAULT_MS; }
    // Pure: PR key → item for one payload (the last open item a hold
    // synthesises its row from).
    function stageTrackerPRItemsOf(d) {
      const out = {};
      for (const it of ((d && Array.isArray(d.items)) ? d.items : [])) if (it && it.kind === 'pr') out[stageTrackerPRKey(it.repo, it.number)] = it;
      return out;
    }
    // Pure: whether a hold is still inside its window at nowMs.
    function stageTrackerMergingHoldLive(hold, nowMs) { return !!hold && nowMs < hold.since + stageTrackerMergingHoldMs(); }
    // Pure: the holds after one payload. rawState is the merge-state map
    // (PR key → merged?) after it.
    //   • a PR the payload shows at merge_gate.state 'merging' opens (or
    //     keeps) a hold from merging_since (else from now, else the one it had)
    //   • a PR shown at 'merge-failed' — or open at any other state — drops
    //     its hold at once
    //   • a held PR the payload dropped keeps its hold while the window is
    //     live and the merged fact is still missing; the merged fact (or
    //     the window's end) drops it — the merge is then read as fresh
    function stageTrackerMergingHoldNext(prev, d, rawState, nowMs) {
      prev = prev || {}; rawState = rawState || {};
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
        if (out[key] || items[key] || rawState[key] === true) continue;
        if (stageTrackerMergingHoldLive(prev[key], nowMs)) out[key] = prev[key];
      }
      return out;
    }
    // Pure: the earliest ms at which a live hold ends (0 when none).
    function stageTrackerMergingHoldUntil(holds, nowMs) {
      let until = 0;
      for (const key of Object.keys(holds || {})) {
        const h = holds[key];
        if (!stageTrackerMergingHoldLive(h, nowMs)) continue;
        const end = h.since + stageTrackerMergingHoldMs();
        if (!until || end < until) until = end;
      }
      return until;
    }
    // Pure: the row a held PR shows while the payload no longer lists it —
    // its last item at 100 "Merging", the gate waiting for GitHub's word.
    function stageTrackerMergingHoldItem(hold, key) {
      const base = hold && hold.item ? hold.item : { kind: 'pr' };
      const mg = Object.assign({}, base.merge_gate || {}, { state: 'merging', merging_since: new Date(hold.since).toISOString(), held: true, sweep_running: false, next_sweep_at: '', soak_until: '', soak_remaining_seconds: 0 });
      if (hold.path) mg.merge_path = hold.path;
      return Object.assign({}, base, { stage: STAGE_TRACKER_MERGE_QUEUE, stage_slug: 'merge-queue', stage_label: 'stage/100-merge-queue', merge_gate: mg, caption: 'Merging — waiting for GitHub to confirm; ✓ follows', actor: 'hive', stage_actor: Object.assign({}, base.stage_actor || {}, { kind: 'hive', login: '', why: 'hive issued the merge; the row advances to ✓ once GitHub confirms' }), soak_remaining_seconds: 0, modifiers: (Array.isArray(base.modifiers) ? base.modifiers : []).filter(m => m !== 'stalled' && m !== 'blocked'), held_merging: true });
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
    // the synthesised row leaves the board.
    function stageTrackerMergingHoldSchedule(inMs) {
      if (_stageTrackerMergingHoldTimer) return;
      _stageTrackerMergingHoldTimer = setTimeout(stageTrackerMergingHoldRelease, Math.max(50, inMs + 50));
    }
    function stageTrackerMergingHoldRelease() {
      _stageTrackerMergingHoldTimer = 0;
      if (!_stageTracker || typeof _stageTracker !== 'object') return;
      stageTrackerReconcileMerges(_stageTracker);
      renderStageTracker(undefined, true);
    }

    /* ---- Closed, not merged: a neutral word on the way out (#15808) ----
       Operator: #15793 "vanished" at 11:46:54 and read as a silent merge;
       it had been closed as superseded by #15783. Items only list open
       work, so the SPA learns of a close the same way it learns of a
       merge: a side list (recently_closed) names the PRs that left closed.
       Every PR this tab saw open that the list now says closed gets one
       info toast — "#15793 closed · superseded by #15783", the reason read
       from the row's last caption — with the standard countdown; no bee,
       no odometer. */
    const STAGE_TRACKER_CLOSED_REASON_MAX = 72;
    // Pure: PR keys the tab saw open (prevState false, or carried open) that
    // d.recently_closed lists, not merged, not listed open now, not yet noted.
    function stageTrackerNewlyClosed(d, prevState, rawState, items, noted) {
      const list = d && Array.isArray(d.recently_closed) ? d.recently_closed : [];
      if (!list.length || !prevState) return [];
      const out = [];
      for (const c of list) {
        if (!c || !c.repo || !c.number) continue;
        const key = stageTrackerPRKey(c.repo, c.number);
        if (prevState[key] !== false || rawState[key] === true || (items && items[key]) || (noted && noted[key]) || out.indexOf(key) >= 0) continue;
        out.push(key);
      }
      return out.sort();
    }
    // Pure: why a closed PR left, from its last item — "superseded by
    // #15783" when the row said so, else its caption's first clause.
    function stageTrackerClosedReason(it) {
      if (!it) return 'closed without merging';
      const sup = Number(it.superseded_by) || Number(it.superseded && it.superseded.by) || 0;
      const cap = String(it.caption || '');
      const m = cap.match(/superseded by #(\d+)/i) || cap.match(/duplicate of #(\d+)/i);
      if (sup > 0) return 'superseded by #' + sup;
      if (m) return (m[0].toLowerCase().startsWith('dup') ? 'duplicate of #' : 'superseded by #') + m[1];
      let clause = cap.split(/[;—]|\. /)[0].trim();
      if (!clause) return 'closed without merging';
      if (clause.length > STAGE_TRACKER_CLOSED_REASON_MAX) clause = clause.slice(0, STAGE_TRACKER_CLOSED_REASON_MAX - 1).trimEnd() + '…';
      return clause;
    }
    // Pure: the toast's words.
    function stageTrackerClosedToastText(key, it) {
      const n = Number(String(key || '').slice(String(key || '').indexOf('/pr#') + 4)) || 0;
      return (n ? '#' + n + ' ' : '') + 'closed · ' + stageTrackerClosedReason(it);
    }
    // Stateful: one neutral toast for a PR that left the board closed.
    function stageTrackerClosedToast(key, it) {
      if (typeof showToast !== 'function') return null;
      return showToast(stageTrackerClosedToastText(key, it), 'info');
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
    // Pure: how many merges the server already counts whose bump the chip
    // still owes the bee — a glyph pending or in the air — so the landing
    // is when it bumps.
    function stageTrackerMergedHoldback(state, pending) {
      const known = {};
      for (const e of ((state && state.server && state.server.recent) || [])) known[stageTrackerMergedIdent(e.repo, e.number)] = true;
      const keys = Object.assign({}, pending || {});
      let n = 0;
      for (const key of Object.keys(keys)) { const i = key.indexOf('/pr#'); if (i > 0 && known[stageTrackerMergedIdent(key.slice(0, i), key.slice(i + 4))]) n++; }
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
    // event never fires — is what clears it. (The merge-gate hold no
    // longer masks a known merge, #15808: the glyph is pending from the
    // payload that brings the merged fact.)
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
      if (pop && !pop.hidden) { e.preventDefault(); hamMergedToggle(false); const chip = stageTrackerMergedChip(); if (chip && chip.focus) chip.focus(); }
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
    var _stageTrackerTerminalRows = {};
    var _stageTrackerTerminalFacts = {};
    // Keep models, not stale HTML, for the existing short terminal/pin window.
    // No queue/admission observation is required for a verified merged fact.
    function stageTrackerTerminalObserve(d, fresh, nowMs) {
      const previous = _stageTrackerTerminalRows, rows = Object.assign({}, previous);
      const current = new Set();
      for (const item of (d && Array.isArray(d.items) ? d.items : [])) if (item) {
        const key = stageTrackerItemKey(item);
        current.add(key);
        rows[key] = { item, at: nowMs, visible_prs: (item.linked_prs || []).map(lp => stageTrackerItemKey(stageTrackerLinkedPRItem(item, lp))) };
      }
      const el = document.getElementById('activity-monitor-panel');
      for (const key of fresh) {
        let rowKey = stageTrackerCelebrateRowKey(key, el) || _stageTrackerPRHost[key] || key;
        if (!rows[rowKey] && rows[key]) rowKey = key;
        if (!rows[rowKey]) continue;
        const prior = _stageTrackerCelebrating[rowKey] || {};
        _stageTrackerTerminalFacts[key] = Object.assign({}, _stageTrackerTerminalFacts[key], {
          observed_at: nowMs, until: nowMs + STAGE_TRACKER_CELEBRATE_PIN_MS,
        });
        _stageTrackerCelebrating[rowKey] = Object.assign({}, prior, {
          key, keys: Array.from(new Set((prior.keys || []).concat(key))), kind: 'merged',
          until: Math.max(Number(prior.until) || 0, nowMs + STAGE_TRACKER_CELEBRATE_PIN_MS),
        });
        stageTrackerScheduleRelease(STAGE_TRACKER_CELEBRATE_PIN_MS);
      }
      const wanted = new Set();
      const detached = [];
      for (const rowKey of Object.keys(rows)) {
        const pin = _stageTrackerCelebrating[rowKey], active = pin && pin.until > nowMs;
        const item = rows[rowKey].item, old = previous[rowKey] && previous[rowKey].item;
        if (item.kind === 'issue' && old && Array.isArray(old.linked_prs)) {
          const linked = (item.linked_prs || []).slice(), keys = new Set(linked.map(lp => stageTrackerItemKey(stageTrackerLinkedPRItem(item, lp))));
          let unresolved = 0;
          for (const lp of old.linked_prs) {
            const key = stageTrackerItemKey(stageTrackerLinkedPRItem(old, lp));
            const fact = _stageTrackerTerminalFacts[key];
            if (keys.has(key)) continue;
            if (fact && fact.until > nowMs) { linked.push(lp); keys.add(key); }
            else if (_stageTrackerMergeState[key] === false && unresolved < STAGE_TRACKER_MERGED_LIST_MAX) {
              linked.push(lp); keys.add(key); unresolved++;
            }
          }
          rows[rowKey] = Object.assign({}, rows[rowKey], { item: Object.assign({}, item, { linked_prs: linked }) });
        }
        const source = rows[rowKey].item;
        const prs = source.kind === 'pr' ? [source] : (source.linked_prs || []).map(lp => stageTrackerLinkedPRItem(source, lp));
        if (!current.has(rowKey) && !active) {
          if (!prs.some(pr => _stageTrackerMergeState[stageTrackerItemKey(pr)] === false)) { delete rows[rowKey]; continue; }
          detached.push(rowKey);
        }
      }
      // Missing-but-unsettled rows are bounded history, not visible holds.
      // Keep them available when the canonical merge ledger lags eviction.
      detached.sort((a, b) => rows[b].at - rows[a].at);
      for (const key of detached.slice(STAGE_TRACKER_MERGED_LIST_MAX)) delete rows[key];
      for (const row of Object.values(rows)) {
        if (row.item.kind === 'pr') wanted.add(stageTrackerItemKey(row.item));
        for (const lp of (row.item.linked_prs || [])) wanted.add(stageTrackerItemKey(stageTrackerLinkedPRItem(row.item, lp)));
      }
      _stageTrackerTerminalRows = rows;
      // Reuse the canonical merge observer's alias resolution, without adding
      // any item stages to this one-record observation.
      const teaching = Object.values(rows).map(row => ({ repo: row.item.repo }));
      const ledger = d && d.merged && Array.isArray(d.merged.recent) ? d.merged.recent : [];
      for (const m of (d && Array.isArray(d.recently_merged) ? d.recently_merged : []).concat(ledger)) {
        const facts = stageTrackerMergeState({ items: teaching, recently_merged: [m] }, _stageTrackerMergeState);
        for (const key of Object.keys(facts)) {
          if (!wanted.has(key)) continue;
          const prior = _stageTrackerTerminalFacts[key] || {};
          _stageTrackerTerminalFacts[key] = Object.assign({}, prior, {
            merged_by: m.merged_by || prior.merged_by || '',
          });
        }
      }
      for (const key of Object.keys(_stageTrackerTerminalFacts)) if (!wanted.has(key)) delete _stageTrackerTerminalFacts[key];
    }
    function stageTrackerTerminalItem(item, key) {
      const fact = _stageTrackerTerminalFacts[key] || {};
      return Object.assign({}, item, {
        stage: STAGE_TRACKER_TERMINAL, stage_slug: 'merged', state: 'closed', terminal_merge: true,
        merged_by: fact.merged_by || item.merged_by || '', modifiers: [], deferred_modifiers: [],
        caption: 'Merged', reason: '', preparation: null, merge_train: null, merge_gate: null,
        decision_needed: null, human_attention: null, decided: null, hold: null, adoption: null,
        manual_merge: false, human_merge_reason: '', human_merge_reason_text: '', human_approver: '',
        superseded: null, superseded_by: 0, adopted_by: 0,
        push: null, rework: null, presence: null, claimant: null,
        stage_actor: { kind: 'none', why: 'merged' }, sentinel_alert: false,
      });
    }
    function stageTrackerTerminalView(d, nowMs) {
      if (!d || !Array.isArray(d.items)) return d;
      const items = d.items.map(item => {
        const rowKey = stageTrackerItemKey(item), pin = _stageTrackerCelebrating[rowKey];
        const active = pin && pin.until > nowMs;
        if (item.kind === 'pr' && _stageTrackerMergeState[rowKey] === true) {
          const fact = _stageTrackerTerminalFacts[rowKey];
          return (fact && fact.until > nowMs) || Number(item.stage) >= STAGE_TRACKER_TERMINAL ? stageTrackerTerminalItem(item, rowKey) : null;
        }
        if (item.kind !== 'issue') return item;
        const saved = _stageTrackerTerminalRows[rowKey];
        const source = (item.linked_prs || []).slice();
        const keys = new Set(source.map(lp => stageTrackerItemKey(stageTrackerLinkedPRItem(item, lp))));
        if (active && saved) for (const lp of (saved.item.linked_prs || [])) {
          const key = stageTrackerItemKey(stageTrackerLinkedPRItem(saved.item, lp)), fact = _stageTrackerTerminalFacts[key];
          if (fact && fact.until > nowMs && !keys.has(key)) { source.push(lp); keys.add(key); }
        }
        const linked = source.map(lp => {
          const key = stageTrackerItemKey(stageTrackerLinkedPRItem(item, lp));
          if (_stageTrackerMergeState[key] !== true) return lp;
          const fact = _stageTrackerTerminalFacts[key];
          return (fact && fact.until > nowMs) || Number(lp.stage) >= STAGE_TRACKER_TERMINAL ? stageTrackerTerminalItem(lp, key) : null;
        }).filter(Boolean);
        return Object.assign({}, item, { linked_prs: linked });
      }).filter(Boolean);
      return Object.assign({}, d, { items });
    }
    function stageTrackerTerminalPinnedItem(rowKey, nowMs) {
      const saved = _stageTrackerTerminalRows[rowKey];
      if (!saved) return null;
      let item = saved.item;
      if (item.kind === 'issue') {
        const visible = new Set(saved.visible_prs || []);
        item = Object.assign({}, item, { linked_prs: (item.linked_prs || []).filter(lp => {
          const key = stageTrackerItemKey(stageTrackerLinkedPRItem(item, lp)), fact = _stageTrackerTerminalFacts[key];
          return visible.has(key) || (fact && fact.until > nowMs);
        }) });
      }
      return stageTrackerTerminalView({ items: [item] }, nowMs).items[0] || null;
    }
    function stageTrackerTerminalEvents(nowMs) {
      let nextUntil = Infinity;
      for (const rowKey of Object.keys(_stageTrackerCelebrating)) {
        const pin = _stageTrackerCelebrating[rowKey];
        if (pin.kind !== 'merged' || pin.until <= nowMs) continue;
        for (const key of (pin.keys || [pin.key])) {
          const fact = _stageTrackerTerminalFacts[key] || {};
          if (!fact.observed_at || fact.until <= nowMs) continue;
          nextUntil = Math.min(nextUntil, fact.until);
          const by = stageTrackerMergedByText(fact.merged_by);
          const number = Number(key.slice(key.lastIndexOf('/pr#') + 4));
          const event = { who: 'merge', at: fact.observed_at, text: 'Merged PR #' + number + (by ? ' ' + by : '') };
          _stageTrackerEvents[key] = Object.assign({}, _stageTrackerEvents[key], event);
          if (key === pin.key) _stageTrackerEvents[rowKey] = Object.assign({}, _stageTrackerEvents[rowKey], event);
        }
      }
      if (Number.isFinite(nextUntil)) stageTrackerScheduleRelease(nextUntil - nowMs);
    }
    function stageTrackerMergedPRKnown(item) {
      return !!item && item.kind === 'pr' && typeof _stageTrackerMergeState !== 'undefined' &&
        !!_stageTrackerMergeState && _stageTrackerMergeState[stageTrackerItemKey(item)] === true;
    }
    var _stageTrackerFlights = 0;
    // Pure: the PR keys a row draws, space-separated — its own key for a
    // PR row, the covering PRs for an issue row (one inline, several as
    // sub-rows). Stamped as data-ham-prs so a merged PR can be traced to
    // the row it was drawn on even when it had no sub-row of its own.
    function stageTrackerRowPRKeys(item) {
      if (!item) return '';
      if (item.kind === 'pr') return stageTrackerPRKey(item.repo, item.number);
      return (Array.isArray(item.linked_prs) ? item.linked_prs : []).filter(lp => lp && Number(lp.number) > 0 && (!Number(lp.drawn_under) || Number(lp.drawn_under) === Number(item.number))).map(lp => {
        const pr = typeof stageTrackerLinkedPRItem === 'function' ? stageTrackerLinkedPRItem(item, lp) : { repo: lp.repo || item.repo, number: lp.number };
        return stageTrackerPRKey(pr.repo, pr.number);
      }).join(' ');
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
      }
      for (const it of (d && Array.isArray(d.items) ? d.items : [])) {
        if (!it || it.kind !== 'issue') continue;
        for (const lp of (Array.isArray(it.linked_prs) ? it.linked_prs : [])) {
          if (!lp || !Number(lp.number) || (Number(lp.drawn_under) && Number(lp.drawn_under) !== Number(it.number))) continue;
          const pr = typeof stageTrackerLinkedPRItem === 'function' ? stageTrackerLinkedPRItem(it, lp) : { repo: lp.repo || it.repo, number: lp.number };
          out[stageTrackerPRKey(pr.repo, pr.number)] = stageTrackerItemKey(it);
        }
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
        const fact = typeof _stageTrackerTerminalFacts !== 'undefined' ? _stageTrackerTerminalFacts[key] : null;
        if (fact && fact.observed_at) fact.until = Math.max(fact.until, startAt + STAGE_TRACKER_CELEBRATE_PIN_MS);
        if (rowKey) _stageTrackerCelebrating[rowKey] = Object.assign({}, _stageTrackerCelebrating[rowKey], {
          key, kind: 'merged', until: Math.max((_stageTrackerCelebrating[rowKey] || {}).until || 0, startAt + STAGE_TRACKER_CELEBRATE_PIN_MS),
        });
        const number = Number(key.slice(key.indexOf('/pr#') + 4)) || 0;
        const banner = stageTrackerMergeBanner({ key, rowKey, number, repo: key.slice(0, key.indexOf('/pr#')), at: now });
        setTimeout(() => {
          const row = rowKey && el ? el.querySelector(`[data-ham-key="${CSS.escape(rowKey)}"]`) : null;
          if (row) celebrateRow(row, { kind: 'merged', key, number, merged_by: fact && fact.merged_by });
          else celebrateDetached(key, banner);
        }, i * STAGE_TRACKER_CELEBRATE_STAGGER_MS);
      });
    }
    // Pure: where a row-less glyph takes off — the banner's ✓ when it has
    // a box, else the bottom centre of the panel (or the viewport). Null
    // when nothing has a box (no layout, e.g. a hidden tab).
    function stageTrackerDetachedOrigin(bannerRect, panelRect, vw, vh) {
      const ok = r => !!r && r.width > 0 && r.height > 0;
      if (ok(bannerRect)) return bannerRect;
      if (ok(panelRect)) return { left: panelRect.left + panelRect.width / 2 - STAGE_TRACKER_CELEBRATE_GLYPH_HALF, top: Math.min(panelRect.top + panelRect.height, vh) - 2 * STAGE_TRACKER_CELEBRATE_GLYPH_HALF, width: 2 * STAGE_TRACKER_CELEBRATE_GLYPH_HALF, height: 2 * STAGE_TRACKER_CELEBRATE_GLYPH_HALF };
      if (vw > 0 && vh > 0) return { left: vw / 2 - STAGE_TRACKER_CELEBRATE_GLYPH_HALF, top: vh - 2 * STAGE_TRACKER_CELEBRATE_GLYPH_HALF, width: 2 * STAGE_TRACKER_CELEBRATE_GLYPH_HALF, height: 2 * STAGE_TRACKER_CELEBRATE_GLYPH_HALF };
      return null;
    }
    // Stateful: celebrate a merge whose row is not on the board (below the
    // fold, filtered, another page, already evicted — hivecommons/hive#15808:
    // #15707's toast showed, but no bee and no count). The glyph flies from
    // the toast's ✓ (else the panel's foot) to the chip; the count rolls
    // when it lands, exactly as for a drawn row. The toast, the bee and the
    // odometer are one pipeline: a merged fact that reaches here has all three.
    function celebrateDetached(key, banner, preview) {
      const rect = el => el && typeof el.getBoundingClientRect === 'function' ? el.getBoundingClientRect() : null;
      const ico = banner && typeof banner.querySelector === 'function' ? banner.querySelector('.merge-banner-ico') : null;
      const vw = window.innerWidth || 0, vh = window.innerHeight || 0;
      const from = stageTrackerDetachedOrigin(rect(ico) || rect(banner), rect(document.getElementById('activity-monitor-panel')), vw, vh);
      if (!from) { stageTrackerCelebrateTick(key, preview); return { flew: false, lands: false }; }
      return stageTrackerFlyGlyph(from, key, preview);
    }
    // Pure: the rows a render must keep although the payload dropped them —
    // every celebrating row still inside its pin window, as the node it
    // already has (its HTML is replayed so the reconcile keeps it).
    function stageTrackerPinnedRows(el, rows, nowMs, renderItem) {
      const keys = Object.keys(_stageTrackerCelebrating);
      if (!keys.length || !el) return rows;
      const have = {};
      for (const r of rows) have[r.key] = true;
      const out = rows.slice();
      for (const k of keys) {
        const c = _stageTrackerCelebrating[k];
        if (c.until <= nowMs) { delete _stageTrackerCelebrating[k]; continue; }
        if (have[k]) { stageTrackerScheduleRelease(c.until - nowMs); continue; }
        const item = c.kind === 'merged' && typeof renderItem === 'function' && typeof stageTrackerTerminalPinnedItem === 'function' ? stageTrackerTerminalPinnedItem(k, nowMs) : null;
        if (item) {
          out.push({ key: k, html: renderItem(item), pinned: true });
          stageTrackerScheduleRelease(c.until - nowMs);
          continue;
        }
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
        const by = opts.merged_by ? (opts.merged_by === 'hive' ? 'by hive' : 'by @' + opts.merged_by) : '';
        const text = kind === 'merged' ? `merged${opts.number ? ' #' + opts.number : ''}${by ? ' · ' + by : ''}` : `closed by merge${opts.number ? ' of #' + opts.number : ''}`;
        stageTrackerNoteOwnerEvent(rowEl.dataset.hamKey, text, Date.now());
        _stageTrackerEvents[rowEl.dataset.hamKey].who = kind === 'merged' ? 'merge' : 'hive';
        stageTrackerEventLineInject(rowEl, rowEl.dataset.hamKey, text);
      }
      if (kind !== 'merged') return { flew: false };
      const cap = rowEl.querySelector('.ham-seg.cap');
      if (!cap || typeof cap.getBoundingClientRect !== 'function') return celebrateDetached(key, null, preview);
      return stageTrackerFlyGlyph(cap.getBoundingClientRect(), key, preview);
    }
    // Stateful: fly the glyph from a box to the chip (or fade it in place
    // under reduced motion; count at once over the flight cap). Landing
    // calls stageTrackerCelebrateTick, which rolls the odometer (#15763).
    function stageTrackerFlyGlyph(from, key, preview) {
      if (!from || _stageTrackerFlights >= STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS) {
        stageTrackerCelebrateTick(key, preview);
        return { flew: false, lands: false };
      }
      const reduced = stageTrackerReducedMotion();
      const chip = stageTrackerMergedChip();
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
    // Event identity only: never borrow the currently selected/primary repo.
    // Keep an explicit short audit repo when no canonical identity is carried.
    function stageTrackerMergeBannerRepo(b) {
      if (!b) return '';
      const repo = String(b.repo || (b.pr && b.pr.repo) || '');
      if (repo.indexOf('/') > 0) return repo;
      const key = String(b.key || '').match(/^(.+)\/pr#\d+$/);
      if (key) return key[1];
      const url = String(b.url || (b.pr && b.pr.url) || '').match(/^https:\/\/github\.com\/([^/]+\/[^/]+)\/pull\/\d+(?:[?#].*)?$/);
      return url ? url[1] : repo;
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
    // until dismissed or stageTrackerMergeBannerMs() (twice the info toast
    // duration, STAGE_TRACKER_MERGE_BANNER_MS at the default), stacked when
    // several, with a "show row" link while the row is on the board. The
    // auto-dismiss goes through toastCountdownBar like every other timed
    // toast, so the banner drains a .toast-bar along its bottom edge and
    // hover / focus inside it holds both timer and bar together.
    function stageTrackerMergeBanner(b) {
      if (!document.body) return null;
      let box = document.getElementById('merge-banners');
      if (!box) { box = document.createElement('div'); box.id = 'merge-banners'; box.className = 'merge-banners'; box.setAttribute('aria-live', 'polite'); document.body.appendChild(box); }
      const repo = stageTrackerMergeBannerRepo(b);
      const recent = repo ? ((_stageTrackerMerged.server && _stageTrackerMerged.server.recent) || []).find(e => stageTrackerMergeBannerRepo(e).toLowerCase() === repo.toLowerCase() && stageTrackerMergeBannerNumber(e) === stageTrackerMergeBannerNumber(b)) : null;
      const info = Object.assign({ title: _stageTrackerPRTitle[b.key] || '', merged_by: '', lane: '', url: '' }, recent || {}, b, { repo });
      const num = stageTrackerMergeBannerNumber(info);
      const repoChip = stageTrackerRepoChipHtml(repo, { repoChips: { primary: stageTrackerPrimaryRepo(), active: _stageTrackerFilter.repo } });
      // A live merge the ledger has not listed yet still links: the key carries owner/name.
      if (!info.url && info.repo && info.repo.indexOf('/') > 0 && num && !info.preview) info.url = `https://github.com/${info.repo}/pull/${num}`;
      const t = stageTrackerMergeBannerText(info, Date.now());
      const el = document.createElement('div');
      el.className = 'merge-banner';
      el.setAttribute('role', 'status');
      if (b.rowKey) el.setAttribute('data-ham-row', b.rowKey);
      el.innerHTML = `<span class="merge-banner-ico" aria-hidden="true">✓</span><div class="merge-banner-body"><div class="merge-banner-head">${repoChip ? repoChip + ' ' : ''}${num ? `<span class="merge-banner-num">#${esc(num)}</span> ` : ''}merged${info.preview ? ' <span class="text-muted">(test)</span>' : ''}</div><div class="merge-banner-text" title="${esc(t.text)}">${esc(t.text)}</div><div class="merge-banner-links">${b.rowKey ? `<button type="button" data-action="hamBannerShowRow" data-arg0="${esc(b.rowKey)}" data-stop="1">Show row</button>` : ''}${info.url ? `<a href="${esc(info.url)}" target="_blank" rel="noopener">Open on GitHub</a>` : ''}<button type="button" data-action="hamMergedOpen" data-stop="1">All merges</button></div></div><button type="button" class="merge-banner-close" data-action="hamBannerDismiss" data-arg-types="t" data-stop="1" aria-label="Dismiss">×</button>`;
      box.appendChild(el);
      while (box.children.length > STAGE_TRACKER_MERGE_BANNER_MAX) { toastCountdownCancel(box.children[0]); box.children[0].remove(); }
      toastCountdownBar(el, stageTrackerMergeBannerMs(), () => stageTrackerMergeBannerDismiss(el));
      return el;
    }
    function stageTrackerMergeBannerDismiss(el) {
      if (!el || !el.parentNode) return;
      toastCountdownCancel(el);
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
