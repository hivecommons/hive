
    // Column header: the stage names once, aligned with the chevrons below.
    // It is sticky under the Merged bar (tracker-cues CSS), so the names
    // and the chip stay in view while the list scrolls.
    function stageTrackerHeaderHtml(stages) {
      const byNum = {};
      (stages || []).forEach(st => { byNum[st.number] = st; });
      const cols = STAGE_TRACKER_PATH.map(n => {
        if (n === 50) return '<li class="ham-seg ham-track-head"><span class="ham-seg-t">CI</span></li><li class="ham-seg ham-track-head"><span class="ham-seg-t">Review</span></li>';
        const name = n === 0 ? 'Open' : ((byNum[n] || {}).short || (byNum[n] || {}).label || String(n));
        // The Merge queue column wears the same train as the train header chip.
        const ico = n === 80 && typeof stageTrackerTrainIconHtml === 'function' ? stageTrackerTrainIconHtml('ham-ico ham-train-icon ham-col-ico') : '';
        return `<li class="ham-seg"><span class="ham-seg-t">${ico}${esc(name)}</span></li>`;
      }).join('');
      return `<div class="ham-cols" aria-hidden="true"><div class="ham-strip-row"><ol class="ham-strip">${cols}<li class="ham-seg cap ham-history-head" title="History — decisions and actions on this item">${stageTrackerIcon('history', 'ham-ico ham-history-ico', 'History — decisions and actions on this item')}</li></ol><span class="ham-since">since</span></div></div>`;
    }

    // Pure: which of the two column-50 tracks the hive is waiting on when
    // both share the current phase (hivecommons/hive#16484). Review
    // findings and red CI are the fixer's turn ("the fix"); otherwise CI
    // leads until it is green, then review. newHead: the head was just
    // pushed, so CI is (re)running on it and the review is a re-review.
    function stageTrackerTrackLead(item) {
      if (!item || !item.head) return null;
      const stateOf = k => { const a = item[k + '_activity']; return a && a.head === item.head && a.state ? a.state : 'unknown'; };
      const ci = stateOf('ci');
      const review = stateOf('review');
      const done = s => s === 'passing' || s === 'approved';
      const code = item.code_activity;
      const push = item.push;
      const newHead = !!((code && code.head === item.head && code.state === 'pushed') || (push && push.state === 'pushed' && push.sha === item.head));
      if (review === 'changes_requested') return { track: 'review', waitingOn: 'the fix', newHead: false };
      if (ci === 'failing') return { track: 'ci', waitingOn: 'the fix', newHead: false };
      if (done(ci) && done(review)) return null;
      if (done(ci)) return { track: 'review', waitingOn: 'review', newHead };
      return { track: 'ci', waitingOn: 'CI', newHead };
    }

    // Pure: the chevron's primary words — the count grammar the operator
    // asked for (hivecommons/hive#16484): "CI 12/14 passed", "CI 11/14 · 1
    // failing (lint)", "CI 0/14 · queued", "CI ?/? · no checks reported
    // yet"; "Review 3/5 perspectives", "Review 5/5 approved", "Review 0/5 ·
    // re-review pending (new head)", "Review 2/5 · awaiting human
    // decision". '' means the track has not started on this head (the
    // chevron shows its glyph alone). current: column 50 is the row's
    // active phase, so even absent evidence is "in progress" there.
    function stageTrackerTrackWord(item, kind, current) {
      const a = item && item[kind + '_activity'];
      const matches = !!(a && item.head && a.head === item.head);
      const state = matches ? a.state || 'unknown' : 'unknown';
      if (state === 'unknown' && !current) return '';
      const n = k => matches ? Number(a[k]) || 0 : 0;
      if (kind === 'ci') {
        const total = n('checks_total');
        const passed = n('checks_passed');
        const failed = n('checks_failed');
        const names = matches && Array.isArray(a.checks_failing) ? a.checks_failing : [];
        const count = 'CI ' + (total ? passed + '/' + total : '?/?');
        const failing = failed ? failed + ' failing' + (names.length ? ' (' + names.slice(0, 2).join(', ') + (names.length > 2 ? ', …' : '') + ')' : '') : '';
        const blocker = matches && a.blocker ? String(a.blocker) : '';
        switch (state) {
          case 'passing': return count + ' passed';
          case 'failing': return count + ' · ' + (failing || 'failing' + (blocker ? ' (' + blocker + ')' : ''));
          case 'running': return count + ' · running' + (failing ? ' · ' + failing : '');
          case 'queued': return count + ' · queued' + (failing ? ' · ' + failing : '');
          case 'skipped': return count + ' · ' + (blocker ? blocker + ' skipped' : 'skipped');
          case 'missing': {
            if (stageTrackerWorkflowApprovalPending(a) > 0) return count + ' · waiting on workflow approval' + (failing ? ' · ' + failing : '');
            return count + ' · ' + (passed ? (blocker ? blocker + ' not reported' : 'not all reported') : 'no checks reported yet') + (failing ? ' · ' + failing : '');
          }
          default: return count + ' · ' + (total ? 'no verdict yet' : 'no checks reported yet');
        }
      }
      const total = n('perspectives_total');
      const done = n('perspectives_done');
      const count = 'Review' + (total ? ' ' + done + '/' + total : '');
      const actor = matches && a.actor ? String(a.actor) : '';
      const code = item.code_activity;
      const push = item.push;
      const newHead = !!((code && code.head === item.head && code.state === 'pushed') || (push && push.state === 'pushed' && push.sha === item.head));
      switch (state) {
        case 'approved': return count + ' approved';
        case 'changes_requested': return count + ' · changes requested';
        // The hive on the repair (adopted / remediating) is not a human
        // decision: the findings are the changes requested. Otherwise the
        // reviews are done and a person's sign-off is what is left — the
        // viewer's when the ask names them, else the owner's. Never
        // "awaiting human" to the human it waits on (hivecommons/hive#16572:
        // "why does review say awaiting humans if 5/5 reviews are good?").
        case 'human_decision': {
          if (stageTrackerHiveOwnsRepair(item)) return count + ' · changes requested';
          const q = item.decision_needed && item.decision_needed.question ? String(item.decision_needed.question).toLowerCase() : '';
          const me = typeof window !== 'undefined' && window._hiveUser ? String(window._hiveUser).toLowerCase() : '';
          const mine = !!me && new RegExp('@' + me.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '(?![\\w-])').test(q);
          return count + (mine ? ' · needs your sign-off' : ' · needs owner sign-off');
        }
        case 'reviewing': return count + ' · ' + (actor ? actor + ' reviewing' : 'in progress');
        case 'adjudicating': return count + ' · ' + (actor ? actor + ' adjudicating' : 'adjudicating');
        case 'assigned': return count + ' · ' + (actor ? actor + ' queued' : 'queued') + stageTrackerQueueAheadWord(a);
        case 'unassigned': return count + ' · queued' + stageTrackerQueueAheadWord(a);
        default: return count + ' · ' + (newHead ? 're-review pending (new head)' : total && done ? 'perspectives' : 'queued');
      }
    }

    // Unreported checks whose workflow runs sit behind GitHub's
    // approve-workflows gate (dependabot, forks): the CI blocker reads
    // "workflow-approval-required:<n>" (stage_tracker_activity.go). A
    // person's click moves them, so the chevron names the wait and wears
    // the person icon (hivecommons/hive#16599).
    const STAGE_TRACKER_WORKFLOW_APPROVAL_BLOCKER = 'workflow-approval-required:';
    const STAGE_TRACKER_WORKFLOW_APPROVAL_TIP = 'Needs a maintainer to approve the workflow runs on GitHub before the checks can start';
    function stageTrackerWorkflowApprovalPending(a) {
      if (!a) return 0;
      const b = String(a.blocker || '');
      if (b.indexOf(STAGE_TRACKER_WORKFLOW_APPROVAL_BLOCKER) === 0) return Math.max(1, Number(b.slice(STAGE_TRACKER_WORKFLOW_APPROVAL_BLOCKER.length)) || 0);
      return a.workflow_approval && Number(a.workflow_approval.pending) > 0 ? Number(a.workflow_approval.pending) : 0;
    }

    // The tiny ring in a CI / Review chevron: total steps around the
    // circle, filled clockwise from twelve — done (green or the state's
    // colour) first, then failed (red), the rest the muted track. Steps are
    // separated by a gap up to 16 of them, a continuous arc beyond. While
    // the track runs the next step pulses. Colours are CSS classes only
    // (tracker.css tokens); the SVG carries no raw colour.
    const STAGE_TRACKER_RING_SEGMENT_GAP_MAX = 16;
    const STAGE_TRACKER_RING_RADIUS = 6;
    const STAGE_TRACKER_RING_GAP_DEG = 14;
    function stageTrackerRingSvg(done, total, failed, state, label) {
      const t = Math.max(0, Math.floor(Number(total) || 0));
      const d = Math.min(t, Math.max(0, Math.floor(Number(done) || 0)));
      const f = Math.min(t - d, Math.max(0, Math.floor(Number(failed) || 0)));
      const st = String(state || '');
      const running = ['running', 'queued', 'reviewing', 'adjudicating', 'assigned'].includes(st);
      const tone = st === 'changes_requested' || st === 'failing' ? 'fail' : st === 'approved' || st === 'passing' ? 'pass' : st === 'human_decision' ? 'decided' : running ? 'live' : 'pass';
      const a11y = label || (t ? `${d} of ${t} done` + (f ? `, ${f} failed` : '') : 'no steps');
      const r = STAGE_TRACKER_RING_RADIUS, c = 8;
      const pt = (deg) => { const rad = (deg - 90) * Math.PI / 180; return (c + r * Math.cos(rad)).toFixed(2) + ' ' + (c + r * Math.sin(rad)).toFixed(2); };
      const arc = (from, to, cls) => {
        if (to - from >= 360) return `<circle class="${cls}" cx="${c}" cy="${c}" r="${r}"/>`;
        if (to <= from) return '';
        const large = to - from > 180 ? 1 : 0;
        return `<path class="${cls}" d="M${pt(from)} A${r} ${r} 0 ${large} 1 ${pt(to)}"/>`;
      };
      const open = `<svg class="ham-ico ham-ring ham-ring-${tone}${running ? ' ham-ring-running' : ''}" viewBox="0 0 16 16" fill="none" stroke-width="2.2" stroke-linecap="butt" role="img" aria-label="${esc(a11y)}" data-ring-total="${t}" data-ring-done="${d}" data-ring-failed="${f}">`;
      if (!t) return open + `<circle class="ham-ring-track" cx="${c}" cy="${c}" r="${r}"/></svg>`;
      const next = Math.min(t - 1, d + f);
      const segs = [];
      if (t <= STAGE_TRACKER_RING_SEGMENT_GAP_MAX) {
        const step = 360 / t;
        const gap = t === 1 ? 0 : Math.min(STAGE_TRACKER_RING_GAP_DEG, step / 3);
        for (let i = 0; i < t; i++) {
          const cls = 'ham-ring-seg ' + (i < d ? 'ham-ring-done' : i < d + f ? 'ham-ring-failed' : 'ham-ring-rest') + (running && i === next ? ' ham-ring-pulse' : '');
          segs.push(arc(i * step + gap / 2, (i + 1) * step - gap / 2, cls));
        }
      } else {
        const unit = 360 / t;
        segs.push(arc(0, 360, 'ham-ring-seg ham-ring-rest ham-ring-track'));
        segs.push(arc(0, d * unit, 'ham-ring-seg ham-ring-done'));
        segs.push(arc(d * unit, (d + f) * unit, 'ham-ring-seg ham-ring-failed'));
        if (running && d + f < t) segs.push(arc(next * unit, (next + 1) * unit, 'ham-ring-seg ham-ring-rest ham-ring-pulse'));
      }
      return open + segs.join('') + '</svg>';
    }

    // Which chevron carries a row stuck at column 50 with no track in
    // motion: the track that has not settled — CI unless it passed, else
    // Review (a pending verdict or a sign-off). A row never shows no
    // filled chevron (hivecommons/hive#16599).
    function stageTrackerStalledTrack(item) {
      const on = kind => { const a = item && item[kind + '_activity']; return a && item.head && a.head === item.head ? a.state || 'unknown' : 'unknown'; };
      if (typeof stageTrackerStageTrack === 'function' && stageTrackerStageTrack(item)) return '';
      if (on('ci') !== 'passing') return 'ci';
      return 'review';
    }

    // ", 3 ahead" when the backend knows the swarm queue position.
    function stageTrackerQueueAheadWord(a) {
      const n = a && Number(a.queue_ahead);
      return Number.isFinite(n) && n > 0 ? ', ' + n + ' ahead' : '';
    }

    // The track that IS the item's current stage: at review pending (050)
    // the Review column. It is lit whenever its verdict has not settled —
    // queued, in progress, adjudicating alike — because the chevron that
    // corresponds to the stage is the lit one (hivecommons/hive#16599,
    // reversing #16545's "queued = pending" for the stage column only).
    function stageTrackerStageTrack(item) {
      if (!item || Number(item.stage) !== 50) return '';
      const a = item.review_activity;
      const st = a && item.head && a.head === item.head ? a.state || 'unknown' : 'unknown';
      return ['approved', 'changes_requested', 'human_decision'].includes(st) ? '' : 'review';
    }

    function stageTrackerHiveOwnsRepair(item) {
      if (!item || item.needs_human || item.on_hold || item.hold) return false;
      const code = item.code_activity;
      const hive = code && code.head === item.head && ['adopted', 'remediating', 'unassigned', 'fix_pushed', 'fixing'].includes(code.state);
      return !!(hive || item.stage === 74 || item.adopted_by);
    }

    // A row parked on a person: the contributor's move (071/072 or a
    // waiting_owner code word), a hold, or a human decision. Nothing of
    // the hive's is in motion on it, so no CI / Review track is amber.
    function stageTrackerWaitingOnPerson(item) {
      if (!item) return false;
      if (item.needs_human || item.on_hold || item.hold) return true;
      const stage = Number(item.stage);
      // A freshly pushed head is in motion: checks run on it, matching
      // tracker-row-anchor's column-50 anchor (hivecommons/hive#16552).
      if ((stage === 71 || stage === 72) && !(item.push && ['pushed', 'checking'].includes(item.push.state))) return true;
      const code = item.code_activity;
      return !!(code && code.head === item.head && code.state === 'waiting_owner');
    }

    // A CI / Review track is in motion only on concrete evidence on the
    // current head: a check-run in_progress / queued, or a reviewer
    // running / queued (or the backend's own active flag). Checks that
    // have not reported, unknown evidence and settled verdicts are not
    // motion (hivecommons/hive#16540).
    function stageTrackerTrackRunning(item, kind) {
      const a = item && item[kind + '_activity'];
      if (!a || !item.head || a.head !== item.head) return false;
      if (stageTrackerWaitingOnPerson(item)) return false;
      const moving = kind === 'ci' ? ['running', 'queued'] : ['reviewing', 'adjudicating', 'assigned'];
      return !!(a.active || moving.includes(a.state));
    }

    // Evidence is supplied independently by the backend. A native 050 label
    // cannot say who reviews, whether checks run, or whether an author codes.
    // The chevron wears its glyph plus the count words from
    // stageTrackerTrackWord (hivecommons/hive#16484). Style follows the
    // track's own state on the current head: running = the full active
    // fill (both tracks may be lit at once; the words disambiguate); done,
    // a finding, pending and decided are words in their colour on the muted
    // field — only a running track is filled (hivecommons/hive#16540); not
    // started = the empty chevron with the glyph alone. Freshness gates only
    // the pulse.
    function stageTrackerActivityTrackHtml(item, kind, sg, blank, nowMs, opts) {
      if (blank) return `<li class="ham-seg blank" data-track="${kind}" data-route-state="blank" aria-hidden="true"></li>`;
      const name = kind === 'ci' ? 'CI' : 'Review';
      const a = item && item[kind + '_activity'];
      const matches = a && item.head && a.head === item.head;
      let caption = matches ? a.caption || name + ' unknown' : name + ' unknown';
      const observed = matches && a.observed_at ? Date.parse(a.observed_at) : NaN;
      const activityFreshMS = 2 * 60 * 1000;
      const clock = Number(nowMs) || Date.now();
      const stale = Number.isFinite(observed) && (clock - observed > activityFreshMS || observed > clock);
      const current = sg.state === 'active';
      const state = matches ? a.state || 'unknown' : 'unknown';
      const known = matches && state !== 'unknown' && Number.isFinite(observed) && !stale;
      const active = known && a.active;
      if (matches && !known && state !== 'unknown') caption = name + ' activity unknown · last observed ' + state + ' · ' + caption;
      const ownsPR = item.kind === 'pr' || item.via_pr;
      const text = typeof stageTrackerTrackWord === 'function' ? stageTrackerTrackWord(item, kind, current) : (matches ? a.caption || name + ' ' + state : '');
      const finished = matches && (state === 'passing' || state === 'approved');
      const finding = matches && (state === 'failing' || state === 'changes_requested');
      // A complete verdict parked on a person is settled (muted icon).
      const settled = matches && state === 'human_decision' && (!known || !a.active);
      // ACTIVE = filled amber with its words; 1 ≤ active ≤ 2 on a row. A
      // track is active while it genuinely runs on the current head, when
      // it is the stage's own track (stageTrackerStageTrack), or when it is
      // the stalled blocker of a row with nothing in motion (opts.stalled,
      // still amber-muted). Everything else is the icon alone — green for
      // a pass, red for a finding, neutral when not run or decided — with
      // its count words in the tooltip (hivecommons/hive#16599).
      const running = stageTrackerTrackRunning(item, kind);
      const stageTrack = current && typeof stageTrackerStageTrack === 'function' && stageTrackerStageTrack(item) === kind;
      const stalled = !!(opts && opts.stalled) && current && !running;
      const lit = running || stageTrack || stalled;
      const cls = lit ? 'active' + (stalled ? ' ham-stalled' : '') : finished ? 'done' : finding ? 'failing' : settled ? 'done decided' : 'todo';
      const approvalPending = kind === 'ci' && matches ? stageTrackerWorkflowApprovalPending(a) : 0;
      const humanAction = approvalPending > 0 ? STAGE_TRACKER_WORKFLOW_APPROVAL_TIP : kind === 'review' && state === 'human_decision' ? 'Needs a person\'s decision' : '';
      const personIco = stalled && humanAction ? stageTrackerIcon('human', 'ham-ico ham-actor-ico', humanAction) : '';
      const source = matches ? a.source || 'unavailable' : 'unavailable';
      const freshness = !Number.isFinite(observed) ? 'observation time unknown' : (stale ? 'stale · ' : 'observed · ') + new Date(observed).toISOString();
      const since = matches && a.since && Number.isFinite(Date.parse(a.since)) ? ' · since ' + new Date(a.since).toISOString() : '';
      let blocker = matches && a.blocker ? ' · ' + a.blocker : '';
      // A sign-off ask: the tooltip carries the question itself (who should
      // decide what, and why — "workflow change from a fork") when the
      // review evidence did not already quote it.
      const ask = matches && state === 'human_decision' && item.decision_needed && item.decision_needed.question ? String(item.decision_needed.question) : '';
      if (ask && blocker.indexOf(ask) < 0) blocker += ' · ' + ask;
      // The backend's plain-language sentence behind the caption rides in
      // the tooltip only; the chevron clamps the caption.
      const detail = matches && a.detail ? ' · ' + a.detail : '';
      let tip = caption + blocker + detail + ' · head ' + (item.head || 'unknown') + ' · ' + source + ' · ' + freshness + since;
      const code = item.code_activity;
      const codeNote = code && code.head === item.head && Number(item.display_column) === 50 &&
        ((kind === 'review' && code.state === 'waiting_fixer') || (kind === 'ci' && code.state === 'waiting_ci')) ? code.caption : '';
      if (codeNote) tip += ' · code source ' + (code.source || 'unavailable') + ' · observed ' + (code.observed_at || 'unknown') + ' · since ' + (code.since || 'unknown') + (code.blocker ? ' · ' + code.blocker : '');
      const contextWord = kind === 'review' && sg.state === 'active' && !codeNote && sg.detour ? sg.detour.text : '';
      let decided = kind === 'review' && sg.state === 'active' && item.decided && typeof stageTrackerMergeGateWordHtml === 'function' ? stageTrackerMergeGateWordHtml(item, nowMs) : '';
      const codeObserved = Date.parse(code && code.observed_at || '');
      const codeFresh = code && code.head === item.head && code.source === 'fix-dispatch+successful-edit+verified-git-worktree' && code.active && code.state === 'fixing' && Number.isFinite(codeObserved) && codeObserved <= clock && clock - codeObserved <= activityFreshMS;
      if (!codeFresh) decided = decided.replace(/>(?:Fixing|Remediating)(?=\s|<)/g, '>Fix requested');
      const sha7 = item.head ? String(item.head).slice(0, 7) : '';
      const lead = current && ownsPR && typeof stageTrackerTrackLead === 'function' ? stageTrackerTrackLead(item) : null;
      // Only an active track carries words; an inactive one is its icon
      // (ring) in the pass / fail / neutral colour, words in the tooltip.
      const verdict = finished ? 'pass' : finding ? 'fail' : '';
      const word = lit ? esc(text) : '';
      const evidenceURL = matches && /^https?:\/\//i.test(String(a.url || '')) ? a.url : '';
      // The glyph is the ring of steps (checks / perspectives) once a
      // count exists; the flask / eye only while no total is known.
      const ringTotal = matches ? Number(a[kind === 'ci' ? 'checks_total' : 'perspectives_total']) || 0 : 0;
      const ringDone = matches ? Number(a[kind === 'ci' ? 'checks_passed' : 'perspectives_done']) || 0 : 0;
      const ringFailed = kind === 'ci' && matches ? Number(a.checks_failed) || 0 : 0;
      const ringLabel = name + ': ' + (text || (ringTotal ? ringDone + '/' + ringTotal : 'no count'));
      const icon = ringTotal && typeof stageTrackerRingSvg === 'function' ? stageTrackerRingSvg(ringDone, ringTotal, ringFailed, running ? (kind === 'ci' ? 'running' : 'reviewing') : state, ringLabel) : stageTrackerIcon(kind === 'ci' ? 'ci' : 50, 'ham-ico', name);
      const evidenceLink = ownsPR && evidenceURL ? `<a href="${esc(evidenceURL)}" target="_blank" rel="noopener noreferrer" aria-label="${esc('Open current-head ' + name + ' evidence')}">${icon}${word}</a>` : icon + word;
      const routeState = (sg.state || 'todo') + (sg.detour ? ' detoured' : '');
      const hold = item.hold;
      const fullTip = (current ? 'Current pipeline phase · ' : '') + name + ': ' + (text ? text + ' · ' : '') + tip +
        (matches && a.actor ? ' · actor ' + a.actor : '') +
        (codeNote || contextWord ? ' · ' + (codeNote || contextWord) : '') +
        (hold ? ' · ' + (hold.pill || 'Held by @' + hold.by) + (hold.reason ? ' · ' + hold.reason : '') : '') +
        (lead ? ' · hive is waiting on ' + lead.waitingOn + (lead.newHead && sha7 ? ' for new head ' + sha7 : '') : '');
      const tipID = 'ham-track-tip-' + [item.repo || '', item.kind || '', item.number || 0, kind].map(x => encodeURIComponent(String(x))).join('-');
      const copy = `<span class="ham-chevron-copy ham-track-state${lit ? '' : ' ham-track-verdict'}">${evidenceLink}</span>${personIco}`;
      return `<li class="ham-seg ham-activity-track ${cls}${current ? ' ham-current' : ''}" data-track="${kind}"${stalled ? ' data-stalled="true"' : ''} data-route-state="${esc(routeState)}" data-activity-state="${esc(state)}" data-display-state="${known ? 'known' : 'unknown'}"${verdict ? ` data-verdict="${verdict}"` : ''}${current ? ' aria-current="step"' : ''}${active ? ' data-motion="active"' : ''}${ownsPR ? ` title="${esc(fullTip)}"` : ''} aria-label="${esc(fullTip)}"><span class="ham-chevron-info config-info" tabindex="0" title="" aria-label="${esc(fullTip)}" aria-describedby="${esc(tipID)}">${copy}<span id="${esc(tipID)}" class="config-tooltip ham-chevron-tip" role="tooltip">${esc(fullTip)}${decided ? `<span class="ham-track-context">${decided}</span>` : ''}</span></span></li>`;
    }

    // Chevrons for one item. range bounds the columns the item owns (an
    // issue row stops at PR in progress, a nested PR row starts there);
    // columns outside it are invisible spacers that keep the grid aligned.
    // Only the active chevron carries hover text (stage name, description,
    // whose move it is); passed and upcoming chevrons have none.
    function stageTrackerStripItems(item, stages, range, nowMs, ctx) {
      const segs = stageTrackerSegments(item, stages, nowMs, ctx);
      const trains = ctx && ctx.trains;
      const curStage = Number(item.stage) || 0;
      const from = range && range.from != null ? range.from : -Infinity;
      const to = range && range.to != null ? range.to : Infinity;
      const actorKind = stageTrackerActorKind(item);
      const humanIcon = actorKind === 'human' || actorKind === 'maintainer';
      // A current requirement from the server (human_attention), or a
      // decision the row carries itself (045 / 085 with decision_needed,
      // which the server leaves out of human_attention past the gate).
      const humanRequired = typeof stageTrackerHumanAttentionOf !== 'function' || !!stageTrackerHumanAttentionOf(item) || (typeof stageTrackerNeedsHumanAsk === 'function' && !!stageTrackerNeedsHumanAsk(item));
      const actorIco = actorKind && (!humanIcon || humanRequired) ? stageTrackerIcon(actorKind, 'ham-ico ham-actor-ico', 'Next move: ' + actorKind) : '';
      const actorTip = stageTrackerActorTip(item);
      // Guarded: test harnesses lift this function by name without stageTrackerHumanReason.
      const humanReason = (!humanIcon || humanRequired) && typeof stageTrackerHumanReason === 'function' ? stageTrackerHumanReason(item, actorKind) : '';
      const claimant = typeof stageTrackerClaimant === 'function' ? stageTrackerClaimant(item) : null;
      const holderShort = claimant ? stageTrackerClaimantShort(claimant) : '';
      // "· @odttlnt's clanker · ■ offline 3h": the holder's live state
      // rides after the holder when the row's presence is theirs.
      const holderPresence = holderShort && typeof stageTrackerHolderPresenceHtml === 'function' ? stageTrackerHolderPresenceHtml(item, claimant, nowMs) : '';
      const holderHtml = holderShort ? `<span class="ham-holder"> · ${esc(holderShort)}${holderPresence}</span>` : '';
      // A row whose next mover is nobody (an unclassified PR under its
      // issue reads "PR in progress") still shows WHO holds it: the
      // holder's own kind icon stands where the actor icon would
      // (hivecommons/hive#15541).
      const holderKind = claimant ? stageTrackerClaimantIconKind(claimant) : '';
      const holderIco = holderShort && !actorIco && (holderKind !== 'human' || humanRequired) ? stageTrackerIcon(holderKind, 'ham-ico ham-actor-ico', stageTrackerClaimantLine(claimant, nowMs)) : '';
      // Static chevrons carry no data-motion: the CSS only animates the two
      // moving buckets. The hive-acting pulse needs an agent actually on
      // the item (tracker-cues: working); a hive stage with nobody on it is
      // waiting, and its tooltip says so.
      // tracker-cues helpers are optional here so a lifted copy of this
      // function (tests, previews) renders without them.
      const worker = typeof stageTrackerWorkingOn === 'function' ? stageTrackerWorkingOn(item, ctx && ctx.workers) : null;
      const code = item.code_activity;
      const codeObserved = code && code.observed_at ? Date.parse(code.observed_at) : NaN;
      const codeFreshMS = 2 * 60 * 1000;
      const clock = Number(nowMs) || Date.now();
      const codeWorking = code && code.head === item.head && code.source === 'fix-dispatch+successful-edit+verified-git-worktree' && code.active && Number.isFinite(codeObserved) && codeObserved <= clock && clock - codeObserved <= codeFreshMS;
      let motion = stageTrackerMotion(item);
      if (code && !codeWorking && Number(item.display_column) === 30) motion = STAGE_TRACKER_MOTION_STATIC;
      if (motion === STAGE_TRACKER_MOTION_ACTIVE && !worker) motion = STAGE_TRACKER_MOTION_STATIC;
      if (codeWorking && Number(item.display_column) === 30) motion = STAGE_TRACKER_MOTION_ACTIVE;
      // Evidence-based remediation (stage_tracker_remediation.go): the PR in
      // progress chevron pulses only while somebody is KNOWN to be on a red
      // PR (kick / claim); "unassigned" is bright but still; a fix head
      // pushed after the red signal is done-style with its word.
      const codeOnHead = code && String(code.head || '') === String(item.head || '');
      const remediation = codeOnHead && ['unassigned', 'remediating', 'fix_pushed'].includes(code.state) ? code.state : '';
      if (remediation === 'remediating' && Number(item.display_column) === 30) motion = STAGE_TRACKER_MOTION_ACTIVE;
      const evidence = item.remediation && item.remediation.kind ? 'evidence: ' + item.remediation.kind + (item.remediation.actor ? ' by ' + item.remediation.actor : '') + (item.remediation.since ? ' since ' + item.remediation.since : '') + (item.remediation.source ? ' (' + item.remediation.source + ')' : '') : '';
      // Both tracks running: the PR waits on them, nobody works it. The
      // stage chevron is settled-current (bright words, no amber, no pulse)
      // so the row never lights three chevrons (hivecommons/hive#16540).
      const tracksRunning = ['ci', 'review'].filter(kind => stageTrackerTrackRunning(item, kind)).length;
      const waitingOnTracks = tracksRunning === 2;
      if (waitingOnTracks) motion = STAGE_TRACKER_MOTION_STATIC;
      const motionAttr = motion === STAGE_TRACKER_MOTION_STATIC ? '' : ` data-motion="${motion}"`;
      // Column 50 is the row's phase and no track moves: one track is
      // filled stalled so the row always shows where it is (#16599).
      const stalledTrack = tracksRunning === 0 && segs.some(sg => sg.number === 50 && sg.state === 'active') && typeof stageTrackerStalledTrack === 'function' ? stageTrackerStalledTrack(item) : '';
      return segs.map(sg => {
        if (sg.number === 50) return ['ci', 'review'].map(kind => stageTrackerActivityTrackHtml(item, kind, sg, sg.number < from || sg.number > to, nowMs, { stalled: stalledTrack === kind })).join('');
        if (sg.number < from || sg.number > to) return `<li class="ham-seg blank${sg.cap ? ' cap' : ''}" aria-hidden="true"></li>`;
        const sentinel = !!(sg.detour && sg.detour.sentinel);
        const sentinelSuffix = !!(sg.detour && sg.detour.sentinelSuffix);
        const fixPushed = sg.number === 30 && sg.state === 'done' && remediation === 'fix_pushed';
        const blockerNote = sg.state === 'done' && sg.blockerNote ? String(sg.blockerNote) : '';
        const cls = ['ham-seg', sg.state, sg.cap ? 'cap' : '', sg.state === 'active' && !sg.cap && waitingOnTracks ? 'ham-settled-current' : '', fixPushed ? 'ham-fix-pushed' : '', blockerNote ? 'ham-blocker-note' : '', sg.state === 'active' && sg.number === 30 && remediation ? 'ham-remediation-' + remediation : '', sg.cap && sg.terminal ? 'merged' : '', sg.detour ? 'detoured' : '', sg.hold ? 'ham-open-hold' : '', sg.blocked ? 'ham-open-blocked' : '', sg.draft ? 'ham-open-draft' : '', sg.isNew ? 'ham-open-new' : '', sg.advisory ? 'ham-open-advisory' : '', sg.openSince && sg.openSince.stale ? 'ham-open-stale' : '', sentinel || sentinelSuffix ? 'ham-sentinel' : ''].filter(Boolean).join(' ');
        const active = sg.state === 'active';
        let tip = '';
        let gateCaption = '';
        if (active) {
          tip = sg.label + (sg.description ? ' — ' + sg.description : '');
          if (sg.blocked) tip += ' — ' + stageTrackerBlockedTip(item);
          if (sg.draft) tip += ' — ' + STAGE_TRACKER_DRAFT_TIP;
          if (sg.isNew && typeof stageTrackerNewLines === 'function') tip = (typeof stageTrackerOpenTriageNeeded === 'function' && stageTrackerOpenTriageNeeded(item) ? 'Triage needed — ' : 'New — ') + stageTrackerNewLines(item, nowMs).join(' — ');
          if (sg.advisory) tip = (item.advisory && item.advisory.caption) || 'Advisory for maintainers · read, then close';
          if (sg.openSince) tip += ' — open since ' + new Date(sg.openSince.ms).toISOString() + (sg.openSince.fromCreated ? ' (opened)' : '');
          // The shared Merge gate word hides 080 vs 095 vs 100; the chevron tooltip carries it.
          // With merge_gate in the payload the sentence is the gate's own
          // (what holds it, next sweep, soak left — #15073).
          const gateInfo = !sg.detour && typeof stageTrackerMergeGateInfo === 'function' ? stageTrackerMergeGateInfo(item, nowMs, trains) : null;
          if (gateInfo) {
            tip = sg.label + ' — ' + gateInfo.tip;
            gateCaption = [gateInfo.word, gateInfo.detail].filter(Boolean).join(' · ');
          }
          else if (!sg.detour && (curStage === 60 || curStage === 80)) tip = sg.label + ' — not yet admitted' + (item.caption || item.reason ? ' — ' + (item.caption || item.reason) : '');
          else if (curStage === STAGE_TRACKER_PRE_MERGE_SOAK) tip = sg.label + ' — admitted, soaking' + stageTrackerSoakLeft(item);
          else if (curStage === 100) tip = sg.label + ' — admitted, merging';
          // Why a person merges a 090 PR leads its tooltip (hivecommons/hive#14658).
          // The reason is the whole story; the stage description would only repeat it (hivecommons/hive#15394).
          else if (curStage === STAGE_TRACKER_READY_FOR_HUMAN_MERGE && item.human_merge_reason_text) tip = ((sg.detour && sg.detour.text) || sg.label) + ' — ' + String(item.human_merge_reason_text).trim();
          // A refusal on an earlier head is history, not the word: the
          // chevron reads the live stage and the hover keeps "merge attempt
          // on previous head failed: branch protection".
          const priorNote = !sg.cap && typeof stageTrackerPriorHeadMergeFailedNote === 'function' ? stageTrackerPriorHeadMergeFailedNote(item) : '';
          if (priorNote) tip += ' — ' + priorNote;
          if (actorTip && !sg.cap) tip += actorTip;
          if (claimant && !sg.cap) tip += ' · ' + stageTrackerClaimantLine(claimant, nowMs);
          // The sentinel's reason leads the lane tooltip so the security
          // implication is read before the stage mechanics.
          if (sentinel) tip = stageTrackerSentinelTip(item) + ' — ' + tip;
          else if (sentinelSuffix) tip = tip + ' — ' + stageTrackerSentinelTip(item);
          // Working or waiting leads: what happens now, or what it waits on.
          if (!sg.cap && typeof stageTrackerCueLead === 'function') tip = stageTrackerCueLead(item, worker, nowMs) + tip;
          if (sg.number === 30 && item.code_activity && Number(item.display_column) === 30) {
            tip = (codeWorking ? code.caption : sg.detour ? sg.detour.text : 'Code activity unknown') + ' · head ' + (item.head || 'unknown') + ' · ' + (code.source || 'unavailable') + ' · observed ' + (code.observed_at || 'unknown') + ' · since ' + (code.since || 'unknown');
            if (code.detail) tip += ' · ' + code.detail;
            if (evidence) tip += ' · ' + evidence;
            if (code.blocker) tip += ' · ' + code.blocker;
            if (claimant) tip += ' · ' + stageTrackerClaimantLine(claimant, nowMs);
          }
          if (waitingOnTracks && !sg.cap) tip = 'Waiting on CI and review, both running — ' + tip;
        }
        let inner;
        // A lit chevron on a row parked on a person ("Needs human decision",
        // "Triage needed") is the trigger of the decision popover: the word
        // becomes a button and the chevron drops its native title so the
        // popover (which repeats the stage line) is its one hover text.
        const decision = active && !sg.cap && typeof stageTrackerDecisionOf === 'function' ? stageTrackerDecisionOf(item) : null;
        // On the merge gate the word carries its live detail and the
        // leading glyph says in words what hive is doing (#15073).
        const gateWord = active && !sg.cap && !sg.detour && typeof stageTrackerMergeGateWordHtml === 'function' ? stageTrackerMergeGateWordHtml(item, nowMs, trains) : '';
        // A detour word that already names the holder ("Returned to
        // @bob's clanker") does not repeat them after the dot.
        const detourHolder = sg.detour && holderShort && sg.detour.text.indexOf(holderShort) >= 0 ? '' : holderHtml;
        // A person's move says what for after the word, unless the word
        // already carries a why ("Ready for human merge · workflow files").
        const wordText = String(sg.detour ? sg.detour.text : (sg.short || ''));
        const reasonHtml = active && !sg.cap && humanReason && !gateWord && wordText.indexOf(' · ') < 0 && wordText.toLowerCase().indexOf(humanReason.toLowerCase()) < 0 ? `<span class="ham-human-reason"> · ${esc(humanReason)}</span>` : '';
        const word = sg.detour ? `<span class="ham-detour${detourHolder ? ' ham-has-holder' : ''}">${esc(sg.detour.text)}${detourHolder}${reasonHtml}</span>` : (gateWord || `<span class="ham-seg-t${holderHtml ? ' ham-has-holder' : ''}">${esc(sg.short)}${holderHtml}${reasonHtml}</span>`);
        const gateCue = gateWord && typeof stageTrackerGateCueHtml === 'function' ? stageTrackerGateCueHtml(item, nowMs, trains) : '';
        // The agent-working spinner (tracker-cues) names the agent and what
        // it did, so a spinning glyph is never unexplained.
        const workTip = code && Number(item.display_column) === 30 ? (codeWorking ? code.caption : '') : worker && typeof stageTrackerCueLead === 'function' ? stageTrackerCueLead(item, worker, nowMs).replace(/ — $/, '') : '';
        const workIco = workTip ? `<span class="ham-active-ico-wrap" role="img" title="${esc(workTip)}" aria-label="${esc(workTip)}">${stageTrackerIcon(sg.number, 'ham-ico ham-active-ico')}</span>` : stageTrackerIcon(sg.number, 'ham-ico ham-active-ico');
        // The cap chevron is the ✓ only once the PR is merged; until then
        // the column is the row's history and wears the history mark.
        if (sg.cap) inner = sg.terminal ? stageTrackerIcon(sg.number, 'ham-ico') : stageTrackerIcon('history', 'ham-ico ham-history-ico', 'History — no decisions or actions recorded yet');
        else if (active) {
          const fullTip = [tip, gateCaption || (sg.detour ? sg.detour.text : sg.short), item.caption, item.reason,
            item.head ? 'head ' + item.head : '',
            item.hold ? (item.hold.pill || 'Held by @' + item.hold.by) + (item.hold.reason ? ' · ' + item.hold.reason : '') : ''].filter(Boolean).join(' · ');
          const tipID = 'ham-chevron-tip-' + [item.repo || '', item.kind || '', item.number || 0, sg.number].map(x => encodeURIComponent(String(x))).join('-');
          const caption = gateWord || word;
          const text = decision ? stageTrackerDecisionTriggerHtml(decision, caption, fullTip) : caption;
          const content = `${gateCue}${workIco}<span class="ham-chevron-copy">${text}</span>${actorIco || holderIco}`;
          // A decision's dialog is its sole hover/focus detail surface.
          inner = decision ? `<span class="ham-chevron-info ham-chevron-decision">${content}</span>` : `<span class="ham-chevron-info config-info" tabindex="0" title="" aria-label="${esc(fullTip)}" aria-describedby="${esc(tipID)}">${content}<span id="${esc(tipID)}" class="config-tooltip ham-chevron-tip" role="tooltip">${esc(fullTip)}</span></span>`;
        } else if (fixPushed) {
          const pushedTip = [code.caption, code.detail, evidence, item.head ? 'head ' + item.head : '', code.since ? 'since ' + code.since : ''].filter(Boolean).join(' · ');
          const tipID = 'ham-chevron-tip-' + [item.repo || '', item.kind || '', item.number || 0, sg.number].map(x => encodeURIComponent(String(x))).join('-');
          // Inactive = icon only (hivecommons/hive#16599): the pushed fix is
          // the green glyph; its caption rides the tooltip.
          inner = `<span class="ham-chevron-info config-info" tabindex="0" title="" aria-label="${esc(pushedTip)}" aria-describedby="${esc(tipID)}">${stageTrackerIcon(sg.number, 'ham-ico')}<span id="${esc(tipID)}" class="config-tooltip ham-chevron-tip" role="tooltip">${esc(pushedTip)}</span></span>`;
        } else if (blockerNote) {
          // The issue's own column keeps the blocker in view while the
          // covering PR's strip runs (hivecommons/docs#612): the word, the
          // person glyph, and the blocker's detail as the hover.
          const b = item.blocker || {};
          const noteTip = [blockerNote, b.detail, item.human_attention && item.human_attention.reason].filter(Boolean).join(' — ');
          const tipID = 'ham-chevron-tip-' + [item.repo || '', item.kind || '', item.number || 0, sg.number].map(x => encodeURIComponent(String(x))).join('-');
          inner = `<span class="ham-chevron-info config-info" tabindex="0" title="" aria-label="${esc(noteTip)}" aria-describedby="${esc(tipID)}">${stageTrackerIcon(sg.number, 'ham-ico')}<span class="ham-chevron-copy"><span class="ham-seg-t">${esc(blockerNote)}</span></span>${stageTrackerIcon('human', 'ham-ico ham-actor-ico', 'Next move: human')}<span id="${esc(tipID)}" class="config-tooltip ham-chevron-tip" role="tooltip">${esc(noteTip)}</span></span>`;
        } else inner = stageTrackerIcon(sg.number, 'ham-ico');
        return `<li class="${cls}"${remediation && sg.number === 30 && (active || fixPushed) ? ` data-remediation="${esc(remediation)}"` : ''}${active ? `${sg.cap ? '' : motionAttr}${decision ? '' : ` title="${esc(tip)}"`} aria-current="step"` : ''}${sentinel ? ` aria-label="${esc(STAGE_TRACKER_SENTINEL_WORD + ' — ' + stageTrackerSentinelTip(item))}"` : (sentinelSuffix ? ` aria-label="${esc(sg.detour.text + ' — ' + stageTrackerSentinelTip(item))}"` : '')}>${inner}</li>`;
      }).join('');
    }

    // Compact strips retain their column alignment; the current step gets a
    // full-width caption so detours and the next actor never need a hover.
    function stageTrackerCurrentStageHtml(item, stages, range, nowMs) {
      if (item.superseded && item.superseded.closer_pr) return `<p class="ham-current-stage">${esc(stageTrackerHeadline(item) + (item.superseded.keep_reason ? " · " + item.superseded.keep_reason : ""))}</p>`;
      const active = stageTrackerSegments(item, stages, nowMs).find(sg => sg.state === 'active'
        && (!range || range.from == null || sg.number >= range.from)
        && (!range || range.to == null || sg.number <= range.to));
      if (!active) return '';
      const actor = active.cap || active.number === 50 ? '' : stageTrackerActorTip(item);
      const by = active.cap && item.terminal_merge && typeof stageTrackerMergedByText === 'function' ? stageTrackerMergedByText(item.merged_by) : '';
      const regressed = stageTrackerRegressionText(item, Date.now());
      let text = active.short + (by ? ' ' + by : '') + (active.detour && active.detour.text !== active.short ? ' · ' + active.detour.text : '') + actor + (regressed ? ' · ' + regressed : '');
      const decidedInfo = active.number === 50 && item.decided && typeof stageTrackerMergeGateInfo === 'function' ? stageTrackerMergeGateInfo(item, nowMs) : null;
      if (decidedInfo) {
        const code = item.code_activity;
        const observed = Date.parse(code && code.observed_at || '');
        const activityFreshMS = 2 * 60 * 1000;
        const clock = Number(nowMs) || Date.now();
        const working = code && code.head === item.head && code.source === 'fix-dispatch+successful-edit+verified-git-worktree' && code.active && code.state === 'fixing' && Number.isFinite(observed) && observed <= clock && clock - observed <= activityFreshMS;
        const word = !working && ['Fixing', 'Remediating'].includes(decidedInfo.word) ? 'Fix requested' : decidedInfo.word;
        text = word + (decidedInfo.detail ? ' · ' + decidedInfo.detail : '') + (regressed ? ' · ' + regressed : '');
      }
      const tracks = (item.kind === 'pr' || item.via_pr) && item.head ? ['ci', 'review'].map(kind => {
        const a = item[kind + '_activity'];
        const name = kind === 'ci' ? 'CI' : 'Review';
        if (!a || a.head !== item.head) return name + ' unknown';
        const observed = Date.parse(a.observed_at || '');
        const activityFreshMS = 2 * 60 * 1000;
        const clock = Number(nowMs) || Date.now();
        const fresh = Number.isFinite(observed) && observed <= clock && clock - observed <= activityFreshMS;
        if ((a.active || ['running', 'reviewing', 'adjudicating'].includes(a.state)) && !fresh) return name + ' activity unknown · last observed ' + (a.state || 'unknown');
        return (a.caption || name + ' unknown') + (!Number.isFinite(observed) ? ' · observation time unknown' : !fresh ? ' · stale observation' : '');
      }).join(' · ') : '';
      return `<p class="ham-current-stage">${esc(text + (tracks ? ' · ' + tracks : ''))}</p>`;
    }

    // "040 Covered by PR" — the stage code plus the strip word the payload
    // names for it (the code alone when the stage table is not loaded yet).
    function stageTrackerStageWord(n) {
      const num = Number(n) || 0;
      const st = ((_stageTracker && _stageTracker.stages) || []).find(s => Number(s.number) === num);
      const code = String(num).padStart(3, '0');
      return st && (st.short || st.label) ? code + ' ' + (st.short || st.label) : code;
    }

    // Modifiers that park an item on a person. A regression on a row that
    // carries none of these and sits on a hive-driven stage is the hive's
    // to work through, so the dashboard must not dress it as a human call.
    const STAGE_TRACKER_HUMAN_MODS = ['hold', 'unauthorized', 'needs-human', 'needs-decision', 'needs-rebase', 'blocked', 'stalled'];
    const STAGE_TRACKER_HANDLED_TEXT = 'stage ↓';
    const STAGE_TRACKER_HANDLED_NOTE = 'the hive is handling it — no action needed; clears on the next forward step';

    // True when the item is regressed but nothing about it waits on a
    // person: no human-gate modifier and not on a human-gated stage. Such a
    // row gets a muted "stage ↓" pill and stays out of "Needs attention".
    function stageTrackerRegressionHandled(item) {
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.indexOf('regressed') < 0) return false;
      if (mods.some(m => STAGE_TRACKER_HUMAN_MODS.indexOf(m) >= 0)) return false;
      return STAGE_TRACKER_ATTENTION_STAGES.indexOf(Number(item.stage) || 0) < 0;
    }

    // The regressed pill's explanation: which stage the item fell from and
    // to, when, and the reason recorded with the fall — so "regressed" never
    // needs a trip to the store to read. '' when the item is not regressed.
    // A hive-handled regression adds the note that nobody needs to act.
    function stageTrackerRegressionText(item, nowMs) {
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const from = Number(item.regressed_from) || 0;
      if (mods.indexOf('regressed') < 0 || !from) return '';
      const cur = Number(item.stage) || 0;
      const to = Number(item.regressed_to) || cur;
      const atMs = Date.parse(item.regressed_at || '');
      let text = 'regressed: ' + stageTrackerStageWord(from) + ' → ' + stageTrackerStageWord(to);
      if (!isNaN(atMs)) text += ' at ' + agentNowClockTime(atMs, nowMs || Date.now());
      if (item.regressed_reason) text += ' because ' + String(item.regressed_reason);
      if (cur && cur !== to) text += ' · now ' + stageTrackerStageWord(cur);
      if (stageTrackerRegressionHandled(item)) text += ' · ' + STAGE_TRACKER_HANDLED_NOTE;
      return text;
    }

    // The name of one trail hop. The payload's history entries carry
    // {number, slug, at, ...} (StageTrackerHop); the strip word for the
    // number wins, the slug stands in before the stage table is loaded, and
    // '' means the hop cannot be named and is left out of the trail.
    function stageTrackerHopName(h) {
      if (!h || typeof h !== 'object') return '';
      const raw = h.number !== undefined ? h.number : h.stage;
      const num = Number(raw);
      if (raw !== undefined && raw !== null && raw !== '' && !isNaN(num)) {
        if (num === 0) return 'Open';
        const st = ((_stageTracker && _stageTracker.stages) || []).find(s => Number(s.number) === num);
        if (st && (st.short || st.label)) return st.short || st.label;
      }
      return h.slug ? String(h.slug) : '';
    }

    // "Trail: CI / Review → Needs human decision" — the last few hops of the
    // item's history by stage name; '' when fewer than two hops can be named.
    function stageTrackerTrailText(item) {
      const hist = Array.isArray(item && item.history) ? item.history : [];
      const names = hist.slice(-STAGE_TRACKER_TRAIL_HOPS).map(stageTrackerHopName).filter(Boolean);
      return names.length > 1 ? names.join(' → ') : '';
    }

    // Row tooltip: headline, reason, repo, author, flags, since, recent hops.
    function stageTrackerTooltip(item, nowMs) {
      const ak = item.author_kind || '';
      // Plain words for author_kind in hover text (hivecommons/hive#15394).
      const akPlain = { human: 'person', clanker: 'agent', hive: 'hive', bot: 'bot' };
      // How the kind was read: author/<kind> label (said nothing), hive
      // markers, or the author's contributor-hub profile (hivecommons/hive#16015).
      const akSource = { inferred: ', inferred', registry: ', registered contributor' }[item.author_kind_source] || '';
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const sinceMs = Date.parse(item.stage_since || '');
      const trail = stageTrackerTrailText(item);
      const openHeld = (Number(item.stage) || 0) === 0 && mods.indexOf('hold') >= 0;
      const lines = openHeld
        ? [stageTrackerHeadline(item) + (item.caption || item.reason ? ' · ' + (item.caption || item.reason) : '') + (item.repo ? ' · ' + item.repo : '')]
        : [stageTrackerHeadline(item), item.caption || item.reason || '', item.repo || ''];
      if (item.author || ak) lines.push('Author: ' + (item.author ? '@' + item.author : '') + (ak ? ' (' + (akPlain[ak] || ak) + akSource + ')' : '') + (item.tier ? ' · ' + item.tier : ''));
      if (item.origin) lines.push('Origin: origin/' + item.origin);
      const claimant = typeof stageTrackerClaimant === 'function' ? stageTrackerClaimant(item) : null;
      if (claimant) lines.push(stageTrackerClaimantLine(claimant, nowMs));
      if (item.kind === 'issue' && Array.isArray(item.linked_prs) && item.linked_prs.length) lines.push('Covering PR: ' + item.linked_prs.map(lp => Number(lp.number) || 0).sort((x, y) => x - y).map(n => '#' + n).join(', '));
      else if (item.kind === 'issue' && item.linked_pr) lines.push('Covering PR: #' + item.linked_pr + (typeof stageTrackerLineageClause === 'function' ? stageTrackerLineageClause(item) : ''));
      let actorLine = stageTrackerActorTip(item).replace(/^ · /, '');
      // The 035 next-move why is the caption again in lower case ("a human
      // decision is owed — workflow path"): said once, the next-move line
      // then names only who moves (hivecommons/hive#15773).
      const whyAt = actorLine.indexOf(' — ');
      if (whyAt > 0 && lines[1] && actorLine.slice(whyAt + 3).trim().toLowerCase() === String(lines[1]).trim().toLowerCase()) actorLine = actorLine.slice(0, whyAt);
      if (actorLine) lines.push(actorLine);
      if (typeof stageTrackerNewLines === 'function') lines.push(...stageTrackerNewLines(item, nowMs));
      const blockedPR = mods.indexOf('blocked') >= 0 ? stageTrackerBlockedOnPR(item) : null;
      if (blockedPR) lines.push(blockedPR.tip);
      if (typeof stageTrackerTrainLines === 'function') lines.push(...stageTrackerTrainLines(item));
      // Guarded: the test harnesses load helpers by name, and this one is additive.
      const adoption = typeof stageTrackerAdoptionTip === 'function' ? stageTrackerAdoptionTip(item) : '';
      if (adoption) lines.push('Adoption: ' + adoption + (typeof stageTrackerAdoptOffered === 'function' && stageTrackerAdoptOffered(item) ? ' (row menu → Adopt)' : ''));
      const presence = typeof stageTrackerPresenceTip === 'function' ? stageTrackerPresenceTip(item, nowMs) : '';
      if (presence) lines.push('Presence: ' + presence);
      const rework = typeof stageTrackerReworkTip === 'function' ? stageTrackerReworkTip(item, nowMs) : '';
      if (rework) lines.push('Rework: ' + rework);
      const push = typeof stageTrackerPushTip === 'function' ? stageTrackerPushTip(item, nowMs) : '';
      if (push) lines.push('Push: ' + push);
      if (mods.length) lines.push('Flags: ' + mods.map(m => STAGE_TRACKER_MOD_TEXT[m] || m).join(', '));
      const stale = stageTrackerStaleLabelsText(item);
      if (stale) lines.push(stale);
      const deferred = stageTrackerDeferredMods(item);
      if (deferred.length) lines.push(STAGE_TRACKER_DEFERRED_NOTE + deferred.map(m => STAGE_TRACKER_MOD_TEXT[m] || m).join(', '));
      if (item.sentinel_alert && deferred.indexOf(STAGE_TRACKER_SENTINEL_CHIP) < 0) lines.push(stageTrackerSentinelTip(item));
      const regressed = stageTrackerRegressionText(item, nowMs);
      if (regressed) lines.push(regressed.charAt(0).toUpperCase() + regressed.slice(1));
      if (mods.indexOf('unauthorized') >= 0 && item.unauthorized) lines.push('Unauthorized: ' + item.unauthorized);
      if (mods.indexOf('hold') >= 0 && typeof stageTrackerHoldPill === 'function' && stageTrackerHoldPill(item)) lines.push(stageTrackerHoldPill(item));
      if (sinceMs) lines.push('Since ' + agentNowClockTime(sinceMs, nowMs) + ' · ' + agentNowAgo(nowMs - sinceMs));
      if (trail) lines.push('Trail: ' + trail);
      // Say it once: a line the caption already said is not repeated (hivecommons/hive#15394).
      return lines.filter((l, i) => l && lines.indexOf(l) === i).join('\n');
    }

    // The sentinel chip: drawn in the chip area (beside needs rebase /
    // regressed) on every lane but the one whose word already says it.
    // Pure: the "supersedes #N" chip on a live PR that replaced a closed
    // original (`supersedes`: {number, url, closed_at, open} — the hive's
    // close-first supersession, hivecommons/hive#15135). The closed PR is
    // never a row; this chip is its only trace. '' without one.
    function stageTrackerSupersedesTip(item, nowMs) {
      const sp = item && item.supersedes;
      if (!sp || !(Number(sp.number) > 0)) return '';
      const ago = stageTrackerAgoText(Date.parse(sp.closed_at || ''), nowMs);
      if (sp.open) return 'Replaces #' + Number(sp.number) + ' (left open with the note' + (ago ? ', ' + ago : '') + ')';
      return 'Replaces #' + Number(sp.number) + ' (closed' + (ago ? ' ' + ago : '') + ')';
    }
    function stageTrackerSupersedesChipHtml(item, nowMs) {
      const tip = stageTrackerSupersedesTip(item, nowMs);
      if (!tip) return '';
      const sp = item.supersedes;
      const text = 'supersedes #' + Number(sp.number);
      const attrs = `class="ham-badge ham-badge-supersedes" title="${esc(tip)}" aria-label="${esc(tip)}"`;
      if (sp.url) return `<a ${attrs} href="${esc(sp.url)}" target="_blank" rel="noopener">${esc(text)}</a>`;
      return `<span ${attrs}>${esc(text)}</span>`;
    }
    function stageTrackerSentinelChipHtml(item) {
      if (!item || !item.sentinel_alert || stageTrackerSentinelInLane(item) || stageTrackerSentinelAsSuffix(item) || stageTrackerDeferredMods(item).indexOf(STAGE_TRACKER_SENTINEL_CHIP) >= 0) return '';
      const label = item.sentinel_notice ? 'Informational security notice' : 'Sentinel alert';
      return `<span class="ham-badge ham-badge-sentinel${item.sentinel_notice ? ' ham-sentinel-notice' : ''}" title="${esc(stageTrackerSentinelTip(item))}" aria-label="${esc(label + ' — ' + stageTrackerSentinelTip(item))}">${esc(STAGE_TRACKER_SENTINEL_CHIP)}</span>`;
    }

    // Pure: the hold pill text from the row's `hold` provenance ('' when the
    // pass could not attribute the hold, so the plain "hold" word stays).
    function stageTrackerHoldPill(item) {
      const h = item && item.hold;
      if (!h || !h.by) return '';
      if (h.pill) return h.pill;
      const who = h.by === 'hive' ? 'hive' : '@' + h.by;
      return 'Held by ' + who + (h.reason ? ' · ' + h.reason : '');
    }
    // Pure: the hold pill's hover text — who, why, since when.
    function stageTrackerHoldTip(item, nowMs) {
      const h = item && item.hold;
      if (!h || !h.by) return '';
      const sinceMs = Date.parse(h.since || '');
      const lines = [stageTrackerHoldPill(item)];
      if (h.by === 'hive') lines.push(h.source === 'reporter-trust' ? 'The hive set this hold itself: the linked issue\'s reporter is below the trust the gate requires. Release it to let the lane merge, or keep it until a maintainer has reviewed the reporter.' : 'The hive set this hold itself.');
      if (sinceMs) lines.push('Since ' + agentNowClockTime(sinceMs, nowMs) + ' · ' + agentNowAgo(nowMs - sinceMs));
      return lines.join('\n');
    }

    // Pure: the one open covering PR an issue's `blocked` pill stands for
    // when nothing else blocks the issue — no structured blocker, no
    // blocked_on dependencies, no blocked-on-PR marker, no re-check
    // deadline — and that PR is parked on a human decision (085). The
    // pill then reads "decision on #N" rather than a bare "blocked"
    // (rationguard#431 under #480). null otherwise.
    function stageTrackerBlockedForDecisionPR(item) {
      if (!item || item.kind !== 'issue' || item.blocker) return null;
      if (Array.isArray(item.blocked_on) && item.blocked_on.length) return null;
      if (typeof stageTrackerBlockedOnPR === 'function' && stageTrackerBlockedOnPR(item)) return null;
      if (/^blocked until /i.test(String(item.bucket_reason || ''))) return null;
      const open = (Array.isArray(item.linked_prs) ? item.linked_prs : []).filter(lp => lp && (typeof stageTrackerLinkedPROpen !== 'function' || stageTrackerLinkedPROpen(lp, item)));
      if (open.length !== 1) return null;
      const lp = open[0];
      const parked = Number(lp.stage) === STAGE_TRACKER_NEEDS_DECISION || (Array.isArray(lp.modifiers) && lp.modifiers.indexOf('needs-decision') >= 0) || !!(lp.decision_needed && lp.decision_needed.question);
      return parked ? lp : null;
    }
    function stageTrackerChipsHtml(item) {
      // A row Hive cannot finish on its own: the server's your-turn decision
      // (item.finisher, from the status refresh rather than the current step)
      // says a person must act, now or at a gate further on such as a
      // required approval or a hand merge. The verb and reason are on hover.
      // The chip already says a person must act, so the needs-human and
      // needs-decision pills beside it would only repeat it ("🧑 needs a
      // person" next to "needs human"); they fold into the chip's hover.
      const why = [item.finisher_verb, item.finisher_reason].filter(Boolean).join(' — ');
      const isPerson = item.finisher === 'person';
      const personMod = m => isPerson && (m === 'needs-human' || m === 'needs-decision');
      const allMods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const folded = allMods.filter(personMod).map(m => STAGE_TRACKER_MOD_TEXT[m] || m.replace(/-/g, ' '));
      const person = isPerson
        ? `<span class="ham-badge ham-finisher-person" title="${esc('Hive cannot finish this on its own' + (why ? ': ' + why : '') + (folded.length ? ' · also flagged: ' + folded.join(', ') : ''))}">🧑 needs a person</span>` : '';
      const sentinel = person + stageTrackerSentinelChipHtml(item) + (typeof stageTrackerPreparationChipHtml === 'function' ? stageTrackerPreparationChipHtml(item) : '') + (typeof stageTrackerAdoptionChipHtml === 'function' ? stageTrackerAdoptionChipHtml(item) : '') + (typeof stageTrackerPresenceChipHtml === 'function' ? stageTrackerPresenceChipHtml(item) : '') + (typeof stageTrackerPushChipHtml === 'function' ? stageTrackerPushChipHtml(item, Date.now()) : '') + (typeof stageTrackerReroutePillHtml === 'function' ? stageTrackerReroutePillHtml(item) : '') + (typeof stageTrackerSupersedesChipHtml === 'function' ? stageTrackerSupersedesChipHtml(item, Date.now()) : '') + (typeof stageTrackerOverlapBadgeHtml === 'function' ? stageTrackerOverlapBadgeHtml(item) : '');
      const mods = allMods.filter(m => !personMod(m))
        .sort((x, y) => STAGE_TRACKER_MOD_PRIORITY.indexOf(x) - STAGE_TRACKER_MOD_PRIORITY.indexOf(y));
      if (!mods.length) return sentinel;
      // A regression the hive is working through yields the lead pill to any
      // other modifier and, alone, draws muted as "stage ↓" rather than red.
      const handled = stageTrackerRegressionHandled(item);
      if (handled && mods.length > 1) mods.push(mods.shift());
      let text = handled && mods[0] === 'regressed' ? STAGE_TRACKER_HANDLED_TEXT : (STAGE_TRACKER_MOD_TEXT[mods[0]] || mods[0].replace(/-/g, ' '));
      // A hold the stage-label pass could attribute names who set it and why
      // ("Held by hive · reporter trust (issue #N by @X, contributor)"), so the
      // operator is not left reading "hold +1" (hivecommons/hive#15020).
      // Guarded: the test harnesses load helpers by name, and this one is additive.
      const holdPill = mods[0] === 'hold' && typeof stageTrackerHoldPill === 'function' ? stageTrackerHoldPill(item) : '';
      if (holdPill) text = holdPill;
      // A row the scheduler withholds for open GitHub "blocked by" dependencies names them (hivecommons/hive#14765);
      // one blocked on a PR (hive:blocked-on marker) names it and links to it (hivecommons/hive#14952).
      const blockedPR = mods[0] === 'blocked' ? stageTrackerBlockedOnPR(item) : null;
      const decisionPR = mods[0] === 'blocked' && !blockedPR ? stageTrackerBlockedForDecisionPR(item) : null;
      if (mods[0] === 'blocked' && Array.isArray(item.blocked_on) && item.blocked_on.length) text = stageTrackerBlockedTip(item);
      else if (blockedPR) text = 'blocked on ' + blockedPR.short;
      else if (decisionPR) text = 'decision on #' + Number(decisionPR.number);
      // A blocked row with a hive:recheck-after deadline ("blocked until <RFC3339>") names when the sweep re-checks it.
      const recheck = mods[0] === 'blocked' ? /^blocked until (\S+)/i.exec(String(item.bucket_reason || '')) : null;
      const recheckMs = recheck ? Date.parse(recheck[1]) : NaN;
      if (!isNaN(recheckMs)) text += ' · re-check at ' + agentNowClockTime(recheckMs, Date.now());
      // The regressed pill (lead or folded into "+1") explains itself on
      // hover; failing that, a draft pill (lead or folded) does.
      const regressed = stageTrackerRegressionText(item, Date.now());
      // An unauthorized pill names the missing App permission and the human step;
      // the regressed pill explains itself; a blocked pill names what it waits on;
      // failing all three, a draft pill does.
      const anyBlockedPR = mods.indexOf('blocked') >= 0 ? stageTrackerBlockedOnPR(item) : null;
      // A needs-human pill's hover is the full ask (hivecommons/docs#612
      // showed a bare muted "needs human" with no reason anywhere on the row).
      const askable = mods[0] === 'needs-human' || mods[0] === 'needs-decision';
      const ask = (askable && typeof stageTrackerHumanAttentionOf === 'function' && stageTrackerHumanAttentionOf(item))
        || (askable && typeof stageTrackerNeedsHumanAsk === 'function' && stageTrackerNeedsHumanAsk(item)) || null;
      const tip = (mods.indexOf('unauthorized') >= 0 && String(item.unauthorized || '').trim()) || regressed
        || (decisionPR ? 'Blocked only because PR #' + Number(decisionPR.number) + ' waits on a human decision' + (decisionPR.decision_needed && decisionPR.decision_needed.question ? ' — ' + decisionPR.decision_needed.question : '') : '')
        || (ask ? ask.reason : '')
        || (holdPill && typeof stageTrackerHoldTip === 'function' ? stageTrackerHoldTip(item, Date.now()) : '')
        || (mods.indexOf('blocked') >= 0 && Array.isArray(item.blocked_on) && item.blocked_on.length ? 'Withheld from the scanner until ' + item.blocked_on.join(', ') + (item.blocked_on.length > 1 ? ' close' : ' closes') : '')
        || (anyBlockedPR ? anyBlockedPR.tip : '')
        || (mods.indexOf('draft') >= 0 ? STAGE_TRACKER_DRAFT_TIP : '');
      const cls = 'ham-mod-' + mods[0] + (handled && mods[0] === 'regressed' ? ' ham-mod-handled' : '');
      const inner = `${esc(text)}${mods.length > 1 ? ' +' + (mods.length - 1) : ''}`;
      // The lead pill of a row blocked on a PR is the link to that PR.
      if (blockedPR && blockedPR.url) return sentinel + `<a class="ham-badge ham-badge-mod ${esc(cls)}" href="${esc(blockedPR.url)}" target="_blank" rel="noopener"${tip ? ` title="${esc(tip)}"` : ''} aria-label="${esc(blockedPR.tip)}">${inner}</a>`;
      // A lead needs-human / needs human decision pill opens the decision
      // popover (what to decide, who asked, the applicable actions) instead
      // of a native tooltip.
      // A hive-set hold is a decision too (release or keep): its pill opens the same popover.
      const decision = (mods[0] === 'needs-human' || mods[0] === 'needs-decision' || decisionPR || (holdPill && item.hold && item.hold.by === 'hive')) && typeof stageTrackerDecisionOf === 'function' ? stageTrackerDecisionOf(item) : null;
      if (decision) return sentinel + stageTrackerDecisionTriggerHtml(decision, inner, tip, 'ham-badge ham-badge-mod ' + cls);
      // No popover to open (the ask is the issue's own while a covering
      // PR drives the strip): the pill is a button that reveals the row's
      // "Needs human: …" note line and carries the full reason as its hover.
      if (ask && typeof stageTrackerItemKey === 'function') return sentinel + `<button type="button" class="ham-badge ham-badge-mod ${esc(cls)} ham-needs-human-pill" data-action="stageTrackerNeedsHumanReveal" data-arg0="${esc(stageTrackerItemKey(item))}" data-stop="1" title="${esc(tip)}" aria-label="${esc(text + ' — ' + ask.reason + '. Click to show why on the row.')}">${inner}</button>`;
      return sentinel + `<span class="ham-badge ham-badge-mod ${esc(cls)}"${tip ? ` title="${esc(tip)}"` : ''}>${inner}</span>`;
    }

    /* ---- "Did I act on this?" badge ------------------------------------
       Owner (2026-10-28): "I need a way to see if I selected an action on
       an item so I can keep track of whether or not I addressed it.
       Hovering over the indicator should show me what action I requested."
       The server keeps a ledger of every row action submitted from the
       dashboard (operator_actions.go) and the tracker payload carries the
       newest one per item as `last_operator_action` with a derived state:
       waiting (same stage, recent), addressed (the item moved on, or the
       control asked for is in effect), stuck (same stage past the window),
       pending (Let hive decide still running), failed. The row wears a
       history badge (clock + arrow, with the action count) in the cap cell
       — neutral at rest, amber when stuck, red when failed, dotted while
       pending — whose in-app tooltip names the action,
       when, the outcome and what hive did next. The in-flight spinner (the
       request still on the wire) is a different thing drawn on the chevron;
       this badge appears once the server has recorded the action. */
    const STAGE_TRACKER_ACTED_LABELS = {
      approve: 'Approve', 'give-direction': 'Give direction', 'close-superseded': 'Close as superseded', wontfix: "Close as won't-fix", 'close-read': 'Closed', 'close-issue': 'Close issue',
      'let-hive-decide': 'Let hive decide', hold: 'Hold', release: 'Release hold', 'accept-triage': 'Accept triage', 'mark-needs-human': 'Mark needs-human',
      'queue-merge': 'Approve and let hive merge', 'merge-now': 'Merge now as maintainer',
      adopt: 'Adopt', 'adopt-now': 'Adopt now', reroute: 'Reroute to another clanker',
      // Historical ledger entries only: the hive now re-authors its own
      // exhausted PRs itself and a person's rewrite of a contributor PR is adopt.
      're-author': 'Re-author (now: adopt)',
    };
    // "Rebase, then merge as maintainer" is merge-now on a row that needs a
    // rebase (hivecommons/hive#14822): the server relabels the option and
    // answers 202 rebase-queued; the follow-through's steps come back in
    // last_operator_action.followed[] as hops without a stage number.
    const STAGE_TRACKER_REBASE_MERGE_LABEL = 'Rebase, then merge as maintainer';
    const STAGE_TRACKER_ACTION_REBASE_QUEUED = 'rebase-queued';
    function stageTrackerNeedsRebase(item) {
      if (!item) return false;
      if ((Array.isArray(item.modifiers) ? item.modifiers : []).indexOf('needs-rebase') >= 0) return true;
      if ((Array.isArray(item.labels) ? item.labels : []).some(l => String(l).toLowerCase() === 'needs-rebase')) return true;
      const rm = item.rebase_merge;
      return !!(rm && rm.state && rm.state !== 'merged' && rm.state !== 'failed');
    }
    function stageTrackerRebaseMergeLive(item) {
      const rm = item && item.rebase_merge;
      return !!(rm && (rm.state === 'approved' || rm.state === 'rebasing' || rm.state === 'ci-pending'));
    }
