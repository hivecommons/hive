    function stageTrackerWorkers(status) {
      const out = {};
      for (const a of ((status && status.agents) || [])) {
        const la = a && a.lastAction;
        if (!la || a.busy !== 'working' || !la.current || !la.repo || !(Number(la.number) > 0)) continue;
        out[String(la.repo).toLowerCase() + '#' + Number(la.number)] = { agent: a.name || 'an agent', action: la.action || '', at: la.at || '' };
      }
      return out;
    }
    function stageTrackerWorkerKey(repo, number) { return String(repo || '').toLowerCase() + '#' + (Number(number) || 0); }
    // Pure: the agent on this row — the item itself, or a PR shown on an
    // issue's row. A bare repo name in lastAction matches on its tail.
    function stageTrackerWorkingOn(item, workers) {
      if (!item || !workers) return null;
      const find = (repo, n) => {
        const k = stageTrackerWorkerKey(repo, n);
        if (workers[k]) return workers[k];
        const kr = k.slice(0, k.lastIndexOf('#'));
        for (const wk of Object.keys(workers)) { const i = wk.lastIndexOf('#'); const wr = wk.slice(0, i); if (wk.slice(i) === '#' + (Number(n) || 0) && (kr.endsWith('/' + wr) || wr.endsWith('/' + kr))) return workers[wk]; }
        return null;
      };
      const own = find(item.repo, item.number);
      if (own) return own;
      for (const lp of (Array.isArray(item.linked_prs) ? item.linked_prs : [])) { const w = find(item.repo, lp.number); if (w) return w; }
      return null;
    }
    // Pure: the first words of the chevron tooltip while an agent is on
    // the item — "Working now: scout commented · 2m ago — "; '' otherwise.
    function stageTrackerCueLead(item, worker, nowMs) {
      if (worker) {
        const age = worker.at ? stageTrackerAgoText(Date.parse(worker.at), nowMs) : '';
        return 'Working now: ' + worker.agent + ' ' + (STAGE_TRACKER_WORK_WORDS[worker.action] || 'is on it') + (age ? ' · ' + age : '') + ' — ';
      }
      // Waiting: the tooltip already says where it stands and whose move
      // it is ("Needs decision — … · Next move: human"); nothing to add.
      return '';
    }
    function stageTrackerAgoText(ms, nowMs) {
      if (!(ms > 0)) return '';
      const s = Math.max(0, Math.round(((typeof nowMs === 'number' ? nowMs : Date.now()) - ms) / 1000));
      if (s < 60) return s + 's ago';
      const m = Math.floor(s / 60);
      if (m < 60) return m + 'm ago';
      const h = Math.floor(m / 60);
      return h < 48 ? h + 'h ago' : Math.floor(h / 24) + 'd ago';
    }
    // Pure: the row's cue classes — working while an agent is on it,
    // setback while it has fallen back.
    function stageTrackerCueClasses(item, ctx) {
      const out = [];
      if (stageTrackerWorkingOn(item, ctx && ctx.workers) || stageTrackerRebaseMergeLive(item)) out.push('cue-working');
      const mods = Array.isArray(item && item.modifiers) ? item.modifiers : [];
      if (mods.indexOf('regressed') >= 0) out.push('cue-setback');
      // cue-merged is set by celebrateRow for the moment a merge lands, not
      // for every row that already shows a merged PR.
      return out.length ? ' ' + out.join(' ') : '';
    }
    // The event line: one short, plain sentence per row about the latest
    // thing that happened to it, from the diff between two payloads (no
    // extra requests — the tracker's own poll / SSE refetch feeds it) or
    // from the owner's own row action. Lives STAGE_TRACKER_EVENT_MS, then
    // leaves on the next render; dims for its last third.
    const STAGE_TRACKER_EVENT_MS = 120000;
    const STAGE_TRACKER_EVENT_MOD_WORDS = {
      hold: ['put on hold', 'hold released'], blocked: ['blocked', 'unblocked'], stalled: ['stalled', 'moving again'], regressed: ['fell back a stage', 'recovered'],
      'needs-rebase': ['needs a rebase', 'rebased'], 'needs-human': ['waiting on a person', 'a person answered'], 'needs-decision': ['waiting on a decision', 'decision made'],
      'needs-triage': ['waiting on triage', 'triage accepted'], draft: ['marked draft', 'ready for review'], unauthorized: ['blocked on permission', 'permission granted'],
    };
    // Pure: key → the fields the event line watches.
    function stageTrackerStateMap(items) {
      const out = {};
      const put = (key, it) => { out[key] = { stage: Number(it.stage) || 0, label: it.stage_label || '', caption: it.caption || '', mods: (Array.isArray(it.modifiers) ? it.modifiers : []).slice().sort(), rework: it.rework && it.rework.state ? String(it.rework.state) : '', reworkActor: it.rework && it.rework.actor ? String(it.rework.actor) : '' }; };
      for (const it of (Array.isArray(items) ? items : [])) {
        if (!it || !it.kind || !(Number(it.number) > 0)) continue;
        put(stageTrackerItemKey(it), it);
        for (const lp of (Array.isArray(it.linked_prs) ? it.linked_prs : [])) if (Number(lp.number) > 0) put(stageTrackerItemKey({ repo: it.repo, kind: 'pr', number: lp.number }), lp);
      }
      return out;
    }
    // Pure: plain words for what changed between two states; '' if nothing
    // worth a line did. Order: where it went → what it now waits on. A
    // lower stage number is "fell back to" only when the stage table says
    // the hop is a regression (`falls_to`, from stagelabels.IsRegression):
    // 015 → 010 is triage accepted, 085 → 080 a decision made, 072 → 070
    // the author resuming — those read "moved to", with the caption kept
    // so the line still says why. Without `falls_to` (an older payload)
    // any lower number is a fall.
    function stageTrackerStageFell(prev, next, stages) {
      if (!(next.stage < prev.stage)) return false;
      const from = (stages || []).find(x => Number(x.number) === prev.stage);
      if (!from || !Array.isArray(from.falls_to)) return true;
      return from.falls_to.indexOf(next.stage) >= 0;
    }
    function stageTrackerEventText(prev, next, stages) {
      if (!prev || !next) return '';
      if (next.stage !== prev.stage) {
        if (next.stage >= STAGE_TRACKER_TERMINAL) return 'merged';
        const sg = (stages || []).find(x => Number(x.number) === next.stage);
        // Plain words, never a stage slug ("fell back to stage/060-reviewed").
        const named = typeof stageTrackerStageName === 'function' ? stageTrackerStageName({ label: next.label, short: '' }) || stageTrackerStageName(sg) : '';
        const name = named || next.label || (sg && (sg.label || sg.short)) || ('stage ' + next.stage);
        const back = stageTrackerStageFell(prev, next, stages);
        const why = (back || next.stage < prev.stage) && next.caption ? ' — ' + next.caption : '';
        return (back ? 'fell back to ' : 'moved to ') + name + why;
      }
      for (const m of next.mods) if (prev.mods.indexOf(m) < 0 && STAGE_TRACKER_EVENT_MOD_WORDS[m]) return STAGE_TRACKER_EVENT_MOD_WORDS[m][0];
      for (const m of prev.mods) if (next.mods.indexOf(m) < 0 && STAGE_TRACKER_EVENT_MOD_WORDS[m]) return STAGE_TRACKER_EVENT_MOD_WORDS[m][1];
      // Guarded: the test harnesses load helpers by name.
      const rework = typeof stageTrackerReworkEventText === 'function' ? stageTrackerReworkEventText(prev, next) : '';
      if (rework) return rework;
      if (next.caption && next.caption !== prev.caption) return next.caption;
      return '';
    }
    // Rework pickup on a returned PR (hivecommons/hive#15280): the row stays
    // at 071 / 072, so the stage diff is silent — say who moved instead.
    const STAGE_TRACKER_REWORK_EVENT_WORDS = { acknowledged: 'acknowledged the rework request', in_progress: "'s clanker picked up the rework", pushed: 'pushed a new head — re-review pending' };
    function stageTrackerReworkEventText(prev, next) {
      const to = next && next.rework ? String(next.rework) : '';
      if (!to || to === (prev && prev.rework ? String(prev.rework) : '')) return '';
      const w = STAGE_TRACKER_REWORK_EVENT_WORDS[to];
      if (!w) return '';
      const who = next.reworkActor ? '@' + next.reworkActor : 'the author';
      return w.charAt(0) === "'" ? who + w : who + ' ' + w;
    }
    let _stageTrackerStates = null;
    let _stageTrackerEvents = {};
    // Pure: the events map after payload d at nowMs — new lines for what
    // changed, expired lines gone.
    function stageTrackerEventsNext(events, prevStates, nextStates, stages, nowMs) {
      const out = {};
      for (const k of Object.keys(events || {})) if (nowMs - events[k].at < STAGE_TRACKER_EVENT_MS) out[k] = events[k];
      if (!prevStates) return out;
      for (const k of Object.keys(nextStates)) {
        const text = stageTrackerEventText(prevStates[k], nextStates[k], stages);
        if (text) out[k] = { who: 'hive', text, at: nowMs };
      }
      return out;
    }
    function stageTrackerNoteEvents(d, nowMs) {
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      const next = stageTrackerStateMap(d && d.items);
      _stageTrackerEvents = stageTrackerEventsNext(_stageTrackerEvents, _stageTrackerStates, next, d && d.stages, nowMs);
      _stageTrackerStates = next;
    }
    // The owner's own action writes the row's line at once ("you: approved").
    function stageTrackerNoteOwnerEvent(key, text, nowMs) {
      if (!key || !text) return;
      _stageTrackerEvents[key] = { who: 'you', text, at: typeof nowMs === 'number' ? nowMs : Date.now() };
    }
    // Pure: the event line markup; the age is filled in place by
    // stageTrackerCueAgeSync so a ticking clock never re-renders the row.
    function stageTrackerEventHtml(key, ctx) {
      const ev = ctx && ctx.events ? ctx.events[key] : null;
      if (!ev) return '';
      const nowMs = ctx && typeof ctx.nowMs === 'number' ? ctx.nowMs : Date.now();
      const fading = nowMs - ev.at > STAGE_TRACKER_EVENT_MS * 2 / 3;
      return `<p class="cue-event${fading ? ' cue-event-fading' : ''}" data-cue-key="${esc(key)}"><span class="cue-event-who">${esc(ev.who === 'you' ? 'You' : 'Hive')}:</span> <span class="cue-event-text">${esc(ev.text)}</span> <span class="cue-event-age" data-at="${ev.at}"></span></p>`;
    }
    const STAGE_TRACKER_CUE_TICK_MS = 15000;
    function stageTrackerCueAgeSync(el, nowMs) {
      if (!el || typeof el.querySelectorAll !== 'function') return;
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      for (const n of Array.from(el.querySelectorAll('.cue-event-age[data-at]'))) {
        const t = stageTrackerAgoText(Number(n.getAttribute('data-at')), nowMs);
        if (n.textContent !== t) n.textContent = t;
      }
      if (typeof stageTrackerGateCountdownSync === 'function') stageTrackerGateCountdownSync(el, nowMs);
    }
    let _stageTrackerCueTimer = null;
    function stageTrackerCueTick() {
      if (_stageTrackerCueTimer) return;
      _stageTrackerCueTimer = dashboardSetInterval('stage-tracker-cue', () => stageTrackerCueAgeSync(document.getElementById('activity-monitor-panel')), STAGE_TRACKER_CUE_TICK_MS);
    }

    /* ---- Movement animation ------------------------------------------------
       A move is detected from data, not DOM: each payload is reduced to item
       key → column index (stageTrackerColMap); keys whose column differs from
       the previous payload's are moves (stageTrackerColMoves), the first
       payload never animates. The pure row builder stamps data-ham-prev-col /
       data-ham-col on the moved strips and stageTrackerAnimateMoves() plays
       the chosen effect after the render. renderStageTracker() does not
       rebuild the panel's innerHTML (the hosted hive refreshes the payload
       about once a second, which used to wipe a move mid-flight and leave
       only its opening flash visible): stageTrackerReconcile() keys every
       row on its item (data-ham-key), keeps unchanged and still-animating
       rows' nodes, swaps changed ones, fades new rows in and collapses
       departed rows out. Detour stages share a column through
       stageTrackerAnchor, so 050 → 070 is not a move; 110 (merged) is the
       cap column. */
    const STAGE_TRACKER_MOTION_KEY = 'hiveStageTrackerMotion';
    const STAGE_TRACKER_MOTION_DEFAULT = 'wash';
    const STAGE_TRACKER_MOTION_NONE = 'none';
    const STAGE_TRACKER_MOTIONS = [
      { id: 'none', key: '', label: 'None', desc: 'No flash, no movement effect; the lit chevron simply changes.' },
      { id: 'puck', key: 'A', label: 'Sliding puck', desc: 'The highlight glides from the old column to the new one and settles.' },
      { id: 'ripple', key: 'B', label: 'Ripple', desc: 'Each chevron between old and new flashes faintly in turn.' },
      { id: 'ghost', key: 'C', label: 'Ghost crossfade', desc: 'The old highlight fades out drifting in the travel direction while the new one fades in.' },
      { id: 'comet', key: 'D', label: 'Comet', desc: 'A glowing streak travels the strip\u2019s centre line and the destination flares on arrival.' },
      { id: 'bump', key: 'E', label: 'Destination bump', desc: 'The new chevron scales up slightly and a small arrow rises from it.' },
      { id: 'wash', key: 'F', label: 'Colour wash', desc: 'A soft band sweeps across the chevrons from old to new, leaving a brief afterglow.' },
    ];
    const STAGE_TRACKER_MOVE_MS = 450;
    // Mirrors --ham-move-flash-ms / --ham-move-flash-gap-ms / --ham-move-flash-count.
    const STAGE_TRACKER_FLASH_MS = 140;
    const STAGE_TRACKER_FLASH_GAP_MS = 90;
    const STAGE_TRACKER_FLASH_COUNT = 2;
    // A refresh that moves more rows than this is a reconciler sweep, not
    // movement worth following; the state still advances, nothing animates.
    const STAGE_TRACKER_MOTION_MAX_ROWS = 12;
    // Mirrors --ham-move-exit-ms / --ham-move-enter-ms.
    const STAGE_TRACKER_EXIT_MS = 300;
    const STAGE_TRACKER_ENTER_MS = 220;
    // A row mid-move is kept across refreshes until its plan has landed
    // (flash + move + landing glow stay under this at 1×); the deferred
    // refresh runs then.
    const STAGE_TRACKER_SETTLE_MS = 1200;
    const STAGE_TRACKER_HEADER_KEY = '__cols';
    const STAGE_TRACKER_CAP_COLUMN = STAGE_TRACKER_PATH.length;
    var _stageTrackerCols = null;
    var _stageTrackerMoves = {};
    var _stageTrackerRowSrc = typeof WeakMap === 'function' ? new WeakMap() : null;
    var _stageTrackerSettleTimer = 0;

    // Pure: the strip column (0-based; STAGE_TRACKER_PATH.length = merged cap)
    // an item's stage lights, or -1 for an unknown stage.
    function stageTrackerColIndex(item, stages) {
      const anchor = stageTrackerAnchor(item.stage, stages, item.via_pr ? 'pr' : item.kind, item);
      if (anchor >= STAGE_TRACKER_TERMINAL) return STAGE_TRACKER_CAP_COLUMN;
      return STAGE_TRACKER_PATH.indexOf(anchor);
    }

    // Pure: item key → column for every row a payload draws. An issue with
    // one covering PR rides that PR's stage (as stageTrackerRowHtml draws
    // it); every linked PR is keyed on its own so nested sub-rows move too.
    function stageTrackerColMap(items, stages) {
      const out = {};
      if (!Array.isArray(items)) return out;
      for (const it of items) {
        if (!it || !it.kind || !(Number(it.number) > 0)) continue;
        const linked = it.kind === 'issue' ? stageTrackerLinkedSorted(it, stages).filter(lp => lp && Number(lp.number) > 0) : [];
        const shown = linked.length === 1 ? Object.assign({}, it, { via_pr: true, stage: linked[0].stage || 40 }) : it;
        out[stageTrackerItemKey(it)] = stageTrackerColIndex(shown, stages);
        for (const lp of linked) out[stageTrackerItemKey({ repo: it.repo, kind: 'pr', number: lp.number })] = stageTrackerColIndex({ kind: 'pr', stage: lp.stage || 0 }, stages);
      }
      return out;
    }

    // Pure: key → { from, to } for every key both maps know whose column
    // changed, either direction. No previous map (first payload) or a new
    // key (nothing to move from) → nothing.
    function stageTrackerColMoves(prev, next) {
      const out = {};
      if (!prev || !next) return out;
      for (const k of Object.keys(next)) {
        if (!(k in prev)) continue;
        const from = prev[k], to = next[k];
        if (from !== to && from >= 0 && to >= 0) out[k] = { from, to };
      }
      return out;
    }

    // Pure: attributes the row builder stamps on a moved strip's <ol>.
    function stageTrackerMoveAttrs(key, ctx) {
      const mv = ctx && ctx.moves ? ctx.moves[key] : null;
      if (!mv) return '';
      return ` data-ham-prev-col="${Number(mv.from)}" data-ham-col="${Number(mv.to)}"`;
    }

    function stageTrackerMotionSetting() {
      let v = '';
      try { v = localStorage.getItem(STAGE_TRACKER_MOTION_KEY) || ''; } catch { v = ''; }
      return STAGE_TRACKER_MOTIONS.some(m => m.id === v) ? v : STAGE_TRACKER_MOTION_DEFAULT;
    }
    function stageTrackerSetMotion(id) {
      const v = STAGE_TRACKER_MOTIONS.some(m => m.id === id) ? id : STAGE_TRACKER_MOTION_DEFAULT;
      try { localStorage.setItem(STAGE_TRACKER_MOTION_KEY, v); } catch {}
      return v;
    }
    function stageTrackerReducedMotion() {
      if (document.body && document.body.classList.contains('ham-rm-preview')) return true;
      return typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    }
    // Pure: which effect to play for a move — '' for none. 'none' disables
    // outright; reduced motion turns every other choice into the crossfade.
    function stageTrackerMotionEffect(setting, reduced) {
      if (setting === STAGE_TRACKER_MOTION_NONE) return '';
      if (!STAGE_TRACKER_MOTIONS.some(m => m.id === setting)) setting = STAGE_TRACKER_MOTION_DEFAULT;
      return reduced ? 'rm-fade' : setting;
    }

    // -- effect plumbing -------------------------------------------------
    function stageTrackerAnimScale(strip) {
      const v = parseFloat(getComputedStyle(strip.parentElement).getPropertyValue('--ham-anim-scale'));
      return v > 0 ? v : 1;
    }
    function stageTrackerMoveMs(strip, frac) { return Math.round(STAGE_TRACKER_MOVE_MS * (frac == null ? 1 : frac) * stageTrackerAnimScale(strip)); }
    function stageTrackerSegBox(li) {
      const r = li.getBoundingClientRect(), p = li.parentElement.parentElement.getBoundingClientRect();
      return { left: r.left - p.left, width: r.width, center: r.left - p.left + r.width / 2 };
    }
    function stageTrackerAnimLayer(strip) {
      const row = strip.parentElement;
      let layer = row.querySelector(':scope > .ham-anim-layer');
      if (!layer) { layer = document.createElement('div'); layer.className = 'ham-anim-layer'; layer.setAttribute('aria-hidden', 'true'); row.appendChild(layer); }
      return layer;
    }
    // Run fn once the animation/transition ends; the timeout is the safety
    // net for a tab that is hidden (no frames) so nothing is left behind.
    function stageTrackerAfter(el, evName, durMs, fn) {
      let done = false;
      const go = () => { if (done) return; done = true; el.removeEventListener(evName, go); fn(); };
      el.addEventListener(evName, go);
      setTimeout(go, durMs * 1.5);
    }
    function stageTrackerTransient(li, cls, durMs) { li.classList.add(cls); stageTrackerAfter(li, 'animationend', durMs, () => li.classList.remove(cls)); }
    function stageTrackerGhostEl(strip, idx, cls, toMerged) {
      const b = stageTrackerSegBox(strip.children[idx]);
      const g = document.createElement('div');
      g.className = 'ham-anim-ghost ' + cls + (idx === 0 ? ' first' : '') + (toMerged ? ' to-merged' : '');
      g.style.left = b.left + 'px'; g.style.width = b.width + 'px';
      return g;
    }

    // -- the effects (one function each; the registry below picks one) ----
    function stageTrackerAnimRmFade(strip, from, to) { stageTrackerTransient(strip.children[to], 'ham-rm-fade', stageTrackerMoveMs(strip)); }
    function stageTrackerAnimPuck(strip, from, to) {
      const dest = strip.children[to], toMerged = to >= STAGE_TRACKER_CAP_COLUMN;
      const puck = stageTrackerGhostEl(strip, from, 'ham-puck', toMerged);
      stageTrackerAnimLayer(strip).appendChild(puck);
      dest.classList.add('ham-arriving');
      const b = stageTrackerSegBox(dest);
      requestAnimationFrame(() => requestAnimationFrame(() => { puck.style.left = b.left + 'px'; puck.style.width = b.width + 'px'; }));
      setTimeout(() => {
        dest.classList.remove('ham-arriving');
        puck.classList.add('landed');
        stageTrackerAfter(puck, 'transitionend', stageTrackerMoveMs(strip, 0.4), () => puck.remove());
      }, stageTrackerMoveMs(strip));
    }
    function stageTrackerAnimRipple(strip, from, to) {
      const dir = to > from ? 1 : -1, count = Math.abs(to - from) + 1;
      const step = Math.min(90, 420 / count) * stageTrackerAnimScale(strip);
      for (let k = 0; k < count; k++) {
        const li = strip.children[from + dir * k];
        li.style.setProperty('--ham-ripple-delay', Math.round(k * step) + 'ms');
        stageTrackerTransient(li, 'ham-ripple', stageTrackerMoveMs(strip, 0.6) + k * step);
      }
    }
    function stageTrackerAnimGhost(strip, from, to) {
      const dir = to > from ? 1 : -1;
      const ghost = stageTrackerGhostEl(strip, from, 'ham-ghost-old', to >= STAGE_TRACKER_CAP_COLUMN);
      ghost.style.setProperty('--ham-dir', dir);
      stageTrackerAnimLayer(strip).appendChild(ghost);
      stageTrackerAfter(ghost, 'animationend', stageTrackerMoveMs(strip), () => ghost.remove());
      const dest = strip.children[to];
      dest.style.setProperty('--ham-dir', dir);
      stageTrackerTransient(dest, 'ham-ghost-in', stageTrackerMoveMs(strip));
    }
    function stageTrackerAnimComet(strip, from, to) {
      const dir = to > from ? 1 : -1;
      const a = stageTrackerSegBox(strip.children[from]).center, b = stageTrackerSegBox(strip.children[to]).center;
      const c = document.createElement('div');
      c.className = 'ham-comet' + (dir < 0 ? ' rev' : '') + (to >= STAGE_TRACKER_CAP_COLUMN ? ' to-merged' : '');
      stageTrackerAnimLayer(strip).appendChild(c);
      // The glowing head sits at the column centre; the tail trails behind.
      const w = c.getBoundingClientRect().width || 64;
      const at = x => `translateX(${dir > 0 ? x - w : x}px)`;
      c.style.transform = at(a);
      requestAnimationFrame(() => requestAnimationFrame(() => { c.style.transform = at(b); }));
      setTimeout(() => {
        c.classList.add('landed');
        stageTrackerTransient(strip.children[to], 'ham-comet-hit', stageTrackerMoveMs(strip, 0.7));
        stageTrackerAfter(c, 'transitionend', stageTrackerMoveMs(strip, 0.35), () => c.remove());
      }, stageTrackerMoveMs(strip));
    }
    function stageTrackerAnimBump(strip, from, to) {
      const dir = to > from ? 1 : -1, dest = strip.children[to];
      stageTrackerTransient(dest, 'ham-bump', stageTrackerMoveMs(strip, 0.8));
      const ar = document.createElement('span');
      ar.className = 'ham-arrow';
      ar.textContent = dir > 0 ? '\u2192' : '\u2190';
      ar.style.left = stageTrackerSegBox(dest).center + 'px';
      ar.style.setProperty('--ham-dir', dir);
      if (to >= STAGE_TRACKER_CAP_COLUMN) ar.style.setProperty('--ham-move-color', 'var(--green)');
      stageTrackerAnimLayer(strip).appendChild(ar);
      stageTrackerAfter(ar, 'animationend', stageTrackerMoveMs(strip), () => ar.remove());
    }
    function stageTrackerAnimWash(strip, from, to) {
      const dir = to > from ? 1 : -1, count = Math.abs(to - from) + 1, scale = stageTrackerAnimScale(strip);
      // One continuous band: each chevron's sweep starts as the previous one
      // is 80% through, and the whole run stays inside the 300–700 ms window.
      const per = Math.max(120, Math.min(260, 560 / count)) * scale;
      for (let k = 0; k < count; k++) {
        const li = strip.children[from + dir * k];
        li.style.setProperty('--ham-wash-ms', Math.round(per) + 'ms');
        li.style.setProperty('--ham-wash-delay', Math.round(k * per * 0.8) + 'ms');
        if (dir < 0) li.classList.add('rev');
        li.classList.add('ham-wash');
        stageTrackerAfter(li, 'animationend', per + k * per * 0.8, () => li.classList.remove('ham-wash', 'rev'));
      }
      const dest = strip.children[to];
      dest.style.setProperty('--ham-wash-glow-delay', Math.round((count - 1) * per * 0.8) + 'ms');
      stageTrackerTransient(dest, 'ham-wash-glow', stageTrackerMoveMs(strip, 0.9) + (count - 1) * per * 0.8);
    }
    const STAGE_TRACKER_MOTION_FNS = { 'rm-fade': stageTrackerAnimRmFade, puck: stageTrackerAnimPuck, ripple: stageTrackerAnimRipple, ghost: stageTrackerAnimGhost, comet: stageTrackerAnimComet, bump: stageTrackerAnimBump, wash: stageTrackerAnimWash };

    // Pure: the phases one move plays. Every effect is preceded by the
    // origin flash (two pulses; a single soft pulse under reduced motion)
    // and reveals the destination either as the travel starts (effects
    // that fade it in themselves) or when the travel lands. At 1× the
    // whole sequence is 460 + 450 ms plus the landing glow, under 1.2 s.
    function stageTrackerMovePlan(effect, scale) {
      const s = scale > 0 ? scale : 1;
      const soft = effect === 'rm-fade';
      const flashes = soft ? 1 : STAGE_TRACKER_FLASH_COUNT;
      const flashMs = Math.round((STAGE_TRACKER_FLASH_MS + STAGE_TRACKER_FLASH_GAP_MS) * flashes * s);
      const moveMs = Math.round(STAGE_TRACKER_MOVE_MS * s);
      const revealAtLand = effect === 'ripple' || effect === 'comet' || effect === 'wash';
      return { flashes, soft, flashMs, moveMs, revealAtLand, totalMs: flashMs + moveMs };
    }

    // Play one move on a strip whose chevrons already show the new state:
    // (1) flash the origin while the destination waits dimmed, (2) run the
    // effect, (3) land with the destination lit. data-ham-phase on the
    // strip names the current phase.
    function stageTrackerAnimateMove(strip, from, to, effect) {
      const fn = STAGE_TRACKER_MOTION_FNS[effect];
      if (!fn || !strip || from === to) return false;
      const dest = strip.children[to], src = strip.children[from];
      if (!dest || !src || dest.classList.contains('blank')) return false;
      stageTrackerAnimLayer(strip).innerHTML = '';
      const plan = stageTrackerMovePlan(effect, stageTrackerAnimScale(strip));
      strip.dataset.hamPhase = 'flash';
      dest.classList.add('ham-pending');
      src.style.setProperty('--ham-flash-count', String(plan.flashes));
      src.classList.add('ham-flash');
      if (plan.soft) src.classList.add('soft');
      stageTrackerAfter(src, 'animationend', plan.flashMs, () => {
        src.classList.remove('ham-flash', 'soft');
        src.style.removeProperty('--ham-flash-count');
        strip.dataset.hamPhase = 'move';
        if (!plan.revealAtLand) dest.classList.remove('ham-pending');
        fn(strip, from, to);
        setTimeout(() => {
          dest.classList.remove('ham-pending');
          strip.dataset.hamPhase = 'land';
          stageTrackerAfter(dest, 'transitionend', plan.moveMs * 0.3, () => { if (strip.dataset.hamPhase === 'land') delete strip.dataset.hamPhase; });
        }, plan.moveMs);
      });
      return true;
    }

    // After a render: every strip the row builder flagged as moved plays the
    // effect the setting picks, unless the tab is hidden or too many moved.
    function stageTrackerAnimateMoves(root) {
      if (!root) return 0;
      const effect = stageTrackerMotionEffect(stageTrackerMotionSetting(), stageTrackerReducedMotion());
      const strips = Array.from(root.querySelectorAll('.ham-strip[data-ham-prev-col]'));
      const play = !!effect && !document.hidden && strips.length > 0 && strips.length <= STAGE_TRACKER_MOTION_MAX_ROWS;
      let n = 0;
      for (const strip of strips) {
        if (play && stageTrackerAnimateMove(strip, Number(strip.dataset.hamPrevCol), Number(strip.dataset.hamCol), effect)) n++;
        // The stamp is one render's worth; a kept node must not replay it.
        delete strip.dataset.hamPrevCol; delete strip.dataset.hamCol;
      }
      return n;
    }

    // -- keyed row reconcile ---------------------------------------------
    // Pure: a row's source with the one-render move stamp removed, so the
    // render after a move sees the same row and keeps its (animating) node.
    function stageTrackerRowSrcKey(html) {
      return String(html || '').replace(/ data-ham-prev-col="\d+" data-ham-col="\d+"/g, '');
    }
    // Pure: whether the stamped move attributes name this strip.
    function stageTrackerRowHasMove(html) { return / data-ham-prev-col="\d+"/.test(String(html || '')); }
    function stageTrackerRowAnimating(node) {
      return !!(node && typeof node.querySelector === 'function' && node.querySelector('.ham-strip[data-ham-phase]'));
    }
    function stageTrackerRowNode(key, html) {
      const tpl = document.createElement('template');
      tpl.innerHTML = html;
      const node = tpl.content.firstElementChild;
      node.dataset.hamKey = key;
      if (_stageTrackerRowSrc) _stageTrackerRowSrc.set(node, stageTrackerRowSrcKey(html));
      return node;
    }
    function stageTrackerRowSrcOf(node) { return _stageTrackerRowSrc ? _stageTrackerRowSrc.get(node) : undefined; }
    function stageTrackerRaf2(fn) {
      if (typeof requestAnimationFrame !== 'function') { fn(); return; }
      requestAnimationFrame(() => requestAnimationFrame(fn));
    }
    // Pure: whether departures/arrivals play at all — None and a hidden
    // tab never do; a sweep that turns over more rows than
    // STAGE_TRACKER_MOTION_MAX_ROWS is a filter or page change, not movement.
    function stageTrackerRowMotionOn(effect, hidden, turnover) {
      return !!effect && !hidden && turnover <= STAGE_TRACKER_MOTION_MAX_ROWS;
    }
    // A row leaving the list: collapse + fade from its measured height, then
    // remove. Without motion it is simply removed.
    function stageTrackerRowExit(node, animate) {
      if (!animate) { node.remove(); return; }
      node.style.setProperty('--ham-row-h', (Number(node.offsetHeight) || 0) + 'px');
      node.classList.add('ham-leaving');
      stageTrackerAfter(node, 'animationend', STAGE_TRACKER_EXIT_MS, () => node.remove());
    }
    function stageTrackerRowEnter(node) {
      node.classList.add('ham-entering');
      stageTrackerAfter(node, 'animationend', STAGE_TRACKER_ENTER_MS, () => node.classList.remove('ham-entering'));
    }
    // Reconcile the panel with the rows a render produced ([{ key, html }],
    // the column header first). Rows are matched on key: an unchanged row
    // keeps its node (so an in-flight move is never interrupted), a row
    // mid-move whose only change is incidental (its "since" text ticked)
    // is kept too and a settle re-render is scheduled, a changed row is
    // swapped for a fresh node, a new row fades in, a departed row
    // collapses out. Returns the counts for tests.
    function stageTrackerReconcile(el, rows, opts) {
      opts = opts || {};
      const old = {};
      let leaving = 0;
      for (const n of Array.from(el.children)) {
        if (!n.dataset || !n.dataset.hamKey) { n.remove(); continue; }
        if (n.classList.contains('ham-leaving')) { leaving++; continue; }
        old[n.dataset.hamKey] = n;
      }
      const initial = !Object.keys(old).length && !leaving;
      const seen = {};
      let entering = 0, exiting = 0;
      for (const r of rows) { seen[r.key] = true; if (!old[r.key]) entering++; }
      for (const k of Object.keys(old)) if (!seen[k]) exiting++;
      const animate = !initial && stageTrackerRowMotionOn(opts.effect, opts.hidden, entering + exiting);
      const out = { kept: 0, replaced: 0, entered: 0, exited: 0, deferred: 0 };
      // Departures first, so a collapsing row holds its place while the
      // rows below it are laid out around it.
      for (const k of Object.keys(old)) if (!seen[k]) { stageTrackerRowExit(old[k], animate && k !== STAGE_TRACKER_HEADER_KEY); out.exited++; }
      let idx = 0;
      const place = node => {
        while (el.children[idx] && el.children[idx] !== node && el.children[idx].classList.contains('ham-leaving')) idx++;
        if (el.children[idx] !== node) el.insertBefore(node, el.children[idx] || null);
        idx++;
      };
      for (const r of rows) {
        const prev = old[r.key];
        const src = stageTrackerRowSrcKey(r.html);
        const moved = stageTrackerRowHasMove(r.html);
        if (prev && !moved && stageTrackerRowSrcOf(prev) === src) { place(prev); out.kept++; continue; }
        if (prev && !moved && stageTrackerRowAnimating(prev)) { place(prev); out.deferred++; continue; }
        const node = stageTrackerRowNode(r.key, r.html);
        if (prev) { el.replaceChild(node, prev); out.replaced++; }
        else { out.entered++; if (animate && r.key !== STAGE_TRACKER_HEADER_KEY) stageTrackerRowEnter(node); }
        place(node);
      }
      return out;
    }
    // A refresh that found a row mid-move re-renders once the move has
    // settled, so the row's incidental changes still land.
    function stageTrackerScheduleSettle() {
      if (_stageTrackerSettleTimer) return;
      _stageTrackerSettleTimer = setTimeout(() => { _stageTrackerSettleTimer = 0; renderStageTracker(); }, STAGE_TRACKER_SETTLE_MS);
    }

