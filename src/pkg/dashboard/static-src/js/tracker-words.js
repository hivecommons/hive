    function stageTrackerHumanMergeReasonWord(item) {
      const code = String((item && item.human_merge_reason) || '').trim();
      if (!code) return '';
      return STAGE_TRACKER_HUMAN_MERGE_REASON_WORD[code] || code.replace(/-/g, ' ');
    }
    function stageTrackerManualMergeWord(item) {
      const approver = String((item && item.human_approver) || '').trim();
      const why = stageTrackerHumanMergeReasonWord(item);
      const code = String((item && item.human_merge_reason) || '').trim();
      const lead = code === STAGE_TRACKER_BREAKING_CHANGE_CODE ? STAGE_TRACKER_READY_FOR_HUMAN_MERGE_WORD : STAGE_TRACKER_MANUAL_MERGE_WORD;
      return lead + (why ? ' · ' + why : '') + (approver ? ' · approved by @' + approver : '');
    }
    // The plain 090 word (no manual-merge rule): the stage's short word
    // plus the reason, "Ready for human merge · untrusted author".
    function stageTrackerReadyForHumanWord(item, fallback) {
      const why = stageTrackerHumanMergeReasonWord(item);
      return why ? fallback + ' · ' + why : '';
    }
    // 085 under hive:unauthorized (payload unauthorized_step, e.g. "approve
    // workflow runs"): the hive's GitHub App lacks the permission for that
    // step, so the lit chevron names the human step owed instead of the
    // generic "Needs human decision" (hivecommons/hive#14589).
    const STAGE_TRACKER_HUMAN_GATED = 85;
    // 074 Adopted by hive: the original of a supersede-mode adoption has
    // left the merge gate (its question is answered on the superseding
    // PR), so it renders in the CI/Review column with the adoptee named —
    // never "Needs human decision" (hivecommons/hive#14975 → #14980).
    const STAGE_TRACKER_ADOPTED_BY_HIVE = 74;
    // adopted_by_state (hivecommons/hive#15052): "closed" — the copy was
    // closed unmerged and the hive is re-adopting; "closed-twice" — the
    // loop guard parked the PR for a person.
    function stageTrackerAdoptedWord(item) {
      const by = Number(item && item.adopted_by) || 0;
      if (by <= 0) return '';
      const state = String((item && item.adopted_by_state) || '');
      if (state === 'closed') return 'Adopted → #' + by + ' (closed) · recovering';
      if (state === 'closed-twice') return 'Adopted → #' + by + ' (closed twice) · needs a person';
      if (state === 'copy-failed') return 'Adopted → #' + by + ' (copy failed) · reopened, needs a person';
      // hivecommons/hive#15307: one label while the fixer works (#15416 rule).
      if (state === 'resolving-conflicts') return 'Adopting · fixer resolving conflicts';
      return 'Adopted → #' + by;
    }
    const STAGE_TRACKER_UNAUTHORIZED_WORD = 'Awaiting human';
    function stageTrackerUnauthorizedWord(item) {
      const step = String((item && item.unauthorized_step) || '').trim();
      return step ? STAGE_TRACKER_UNAUTHORIZED_WORD + ': ' + step : '';
    }
    // A parked issue with a structured blocker (item.blocker, #15099) reads
    // "Blocked · App lacks workflows permission", never "Needs human
    // decision": nobody is being asked to decide, the hive cannot act.
    const STAGE_TRACKER_BLOCKED_WORD = 'Blocked';
    function stageTrackerBlockerWord(item) {
      const b = item && item.blocker;
      const short = String((b && b.short) || '').trim();
      return short ? STAGE_TRACKER_BLOCKED_WORD + ' · ' + short : '';
    }
    // Issue-slot placeholders for a closing reference whose title the server
    // could not resolve (the issue is closed and unreadable, or of unknown state).
    const STAGE_TRACKER_CLOSED_ISSUE_TITLE = '(closed issue)';
    const STAGE_TRACKER_UNTITLED_ISSUE = '(title unavailable)';
    // linked_issues[].via values that mark a reference link (`Refs #N`, a
    // `(#N, part 1)` title, a first-paragraph mention) rather than a
    // closing one; the issue slot draws these under a "refs" chip.
    const STAGE_TRACKER_SOFT_LINK_VIAS = ['refs', 'title', 'mention'];
    const STAGE_TRACKER_SOFT_LINK_TIP = 'Referenced by the PR (Refs #N / title), not closed by it';
    // Where an issue parked on 045 (or a pre-045 085) lights: Actionable, the issue side.
    const STAGE_TRACKER_ISSUE_DECISION_COLUMN = 10;
    // 015 triage-needed: actionable but no maintainer has looked at it yet.
    // It lights the Actionable column too, with its own word; a maintainer's
    // move. The Open column is untouched by it.
    const STAGE_TRACKER_TRIAGE_NEEDED = 15;
    // 017 possible-existing: the prior-art gate parked the issue on its
    // author (hivecommons/hive#13575). Lights Actionable with its own word.
    const STAGE_TRACKER_POSSIBLY_EXISTS = 17;
    // Stages that share a column (or a detour word) and what the lit chevron says.
    const STAGE_TRACKER_PRE_MERGE_SOAK = 95;
    // Merge-gate pill vocabulary (hivecommons/hive#15073), one word per
    // stage sharing the Merge gate column, in pipeline order:
    //   060 Review passed        — approved, not yet admitted: the pill
    //                              carries the lane's recorded reason ("CI in
    //                              progress", "checks unreadable", "soak 30m
    //                              remaining"); drawn in CI / Review, since it
    //                              can still regress to 070
    //   080 Ready for auto merge — green and approved; waiting for hive's
    //                              merge sweep: "next sweep in 42s"
    //   095 Soaking              — admitted; the soak window runs: "7m left"
    //   100 Merging              — admitted and soaked; the merge is being
    //                              attempted (GitHub merge queue / tide)
    //   110 Merged               — the ✓ cap
    // stageTrackerMergeGateWordHtml adds the live detail; the leading glyph
    // (stageTrackerGateCueHtml) says the same in a tooltip + aria-label.
    // STAGE_TRACKER_ACTIVE_WORD repeats the words literally (fixtures lift
    // it alone); stage_tracker_merge_gate_test.go pins the two equal.
    const STAGE_TRACKER_MERGE_GATE_WORD = { 'gate-pending': 'Review passed', ready: 'Ready for auto merge', soaking: 'Soaking', merging: 'Merging', merged: 'Merged', 'merge-failed': 'Merge failed' };
    // merge_gate.merge_path (the merge call's issuer) in words (#15245).
    const STAGE_TRACKER_MERGE_PATH_WORDS = { sweep: 'automerge sweep', dashboard: 'maintainer merge from the dashboard', relay: 'hive-merge relay', forge: 'forge adapter', queue: 'merge queue' };
    const STAGE_TRACKER_MERGE_FAILED_SHORT_MAX = 60;
    const STAGE_TRACKER_ACTIVE_WORD = { 15: 'Triage needed', 17: 'Possibly exists', 60: 'Review passed', 80: 'Ready for auto merge', 95: 'Soaking', 100: 'Merging', 45: 'Needs human decision', 85: 'Needs human decision' };
    // Whose move it is on each lit chevron: 'author' = the PR's author kind,
    // 'claimant' = the live claim holder's kind; Open and Actionable carry none.
    const STAGE_TRACKER_ACTOR = { 15: 'human', 17: 'human', 20: 'claimant', 30: 'author', 40: 'author', 45: 'human', 50: 'hive', 60: 'hive', 70: 'author', 71: 'author', 72: 'author', 74: 'hive', 80: 'hive', 85: 'human', 90: 'human', 95: 'hive', 100: 'hive' };
    const STAGE_TRACKER_TERMINAL = 110;
    // Motion bucket of the lit chevron: only a step the hive is acting on
    // ('active', gentle pulse) or one waiting on a person ('needs-human',
    // slower amber→red breathe) animates; every other lit step is 'static'.
    // Open, Open · hold and Claimed never move — motion draws the eye to
    // what is running or needs a hand, not to what is merely waiting.
    //
    //   bucket       | stages / conditions (first match wins, top to bottom)
    //   -------------+------------------------------------------------------------
    //   static       | any `hold` modifier (Open · hold); 110 Merged (✓ cap)
    //   needs-human  | modifiers stalled, blocked, needs-human, needs-decision,
    //                | needs-triage
    //   active       | 030 PR in progress / 040 Covered by PR when the author is
    //                | hive or clanker (an agent is working the PR)
    //   static       | 030 / 040 with a human (or unknown) author
    //   needs-human  | 015 Triage needed, 045 Needs decision (issue), 071 Needs
    //                | author, 072 Returned to contributor, 085 Needs decision
    //                | (PR), 090 Ready for human merge
    //   active       | 050 CI / Review pending, 060 Reviewed, 070 Fixing,
    //                | 074 Adopted by hive, 080 Merge gate, 095 Pre-merge soak,
    //                | 100 Merge queue
    //   static       | 000 Open, 010 Actionable, 020 Claimed, and any stage not
    //                | listed above (default)
    const STAGE_TRACKER_MOTION_STATIC = 'static';
    const STAGE_TRACKER_MOTION_ACTIVE = 'active';
    const STAGE_TRACKER_MOTION_HUMAN = 'needs-human';
    const STAGE_TRACKER_MOTION_BY_STAGE = {
      15: STAGE_TRACKER_MOTION_HUMAN, 45: STAGE_TRACKER_MOTION_HUMAN, 71: STAGE_TRACKER_MOTION_HUMAN, 72: STAGE_TRACKER_MOTION_HUMAN, 85: STAGE_TRACKER_MOTION_HUMAN, 90: STAGE_TRACKER_MOTION_HUMAN,
      50: STAGE_TRACKER_MOTION_ACTIVE, 60: STAGE_TRACKER_MOTION_ACTIVE, 70: STAGE_TRACKER_MOTION_ACTIVE, 74: STAGE_TRACKER_MOTION_ACTIVE, 80: STAGE_TRACKER_MOTION_ACTIVE, 95: STAGE_TRACKER_MOTION_ACTIVE, 100: STAGE_TRACKER_MOTION_ACTIVE,
    };
    const STAGE_TRACKER_MOTION_AUTHOR_STAGES = [30, 40];
    const STAGE_TRACKER_MOTION_HUMAN_MODS = ['stalled', 'blocked', 'needs-human', 'needs-decision', 'needs-triage', 'unauthorized'];
    /* ---- merge train (hivecommons/hive#15117) ------------------------------
       Pure helpers; stage_tracker_merge_train_ui_test.go runs them under node.
       A PR's merge_train names its train (id, state, front, position/size,
       the PRs ahead, the overlapping files). The pill wording is the state's,
       the rows of one train are drawn contiguous in merge order under one
       header, and the tooltip lists the queue. */
    const STAGE_TRACKER_TRAIN_STATE_FRONT = 'front';
    const STAGE_TRACKER_TRAIN_STATE_QUEUED = 'queued-behind';
    const STAGE_TRACKER_TRAIN_STATE_REBASING = 'rebasing';
    const STAGE_TRACKER_TRAIN_STATE_EXHAUSTED = 'rebase-exhausted';
    // The merge-queue icon: a side-view railcar on two wheels (14px,
    // currentColor) — the owner read the old ⛓ as a scribble. One path for
    // the Merge queue column header, the train header chip and the queue
    // popover, so the picture means the same thing everywhere.
    const STAGE_TRACKER_TRAIN_ICON_PATH = '<rect x="2" y="3.5" width="12" height="7" rx="1.5"/><path d="M2 7.5h12M6.5 3.5v4M10.5 3.5v4M4 13.5l-1.5 1.5M12 13.5l1.5 1.5"/><circle cx="5" cy="12.5" r="1.5"/><circle cx="11" cy="12.5" r="1.5"/>';
    const STAGE_TRACKER_TRAIN_ICON_TITLE = 'Merge queue';
    // Pure: the inline-SVG train with its "Merge queue" hover.
    function stageTrackerTrainIconHtml(cls) {
      return `<svg class="${esc(cls || 'ham-train-icon')}" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" role="img" aria-label="${esc(STAGE_TRACKER_TRAIN_ICON_TITLE)}"><title>${esc(STAGE_TRACKER_TRAIN_ICON_TITLE)}</title>${STAGE_TRACKER_TRAIN_ICON_PATH}</svg>`;
    }
    // Pure: ordinal place words — "1st", "2nd", "3rd", "4th", "11th",
    // "21st" (the owner's wording for the queue: "Merge queued · 1st").
    // Every merge_train / merge_gate train state in words. A slug reaches a
    // chip only through this table (or stageTrackerTrainWord), never raw:
    // hive's own #15406 once read "front" (hivecommons/hive#15395).
    // Both queue states read "Merge queued"; the place ("· 1st", "· 2nd")
    // is what tells them apart — the owner could not parse "queued-behind".
    const STAGE_TRACKER_TRAIN_STATE_WORD = { front: 'Merge queued', 'queued-behind': 'Merge queued', rebasing: 'Rebasing', 'rebase-exhausted': 'Needs human decision', 'duplicate-of': 'Duplicate of' };
    // merge_gate.state of a PR the queue holds because another open PR
    // fixes the same issue: "duplicate-of:15402" (merge_gate.duplicate_of
    // / duplicate_issue carry the numbers).
    const STAGE_TRACKER_GATE_DUPLICATE_PREFIX = 'duplicate-of:';
    // Pure: the PR number a "duplicate-of:<n>" gate state yields to; 0 for
    // any other state.
    function stageTrackerGateDuplicateOf(state) {
      state = String(state || '');
      return state.indexOf(STAGE_TRACKER_GATE_DUPLICATE_PREFIX) === 0 ? Number(state.slice(STAGE_TRACKER_GATE_DUPLICATE_PREFIX.length)) || 0 : 0;
    }
    // Pure: the pill word for any merge_gate.state — the gate table, then
    // the train table, then the slug spelled out ("merge-failed" → "Merge
    // failed") so an unmapped state still never shows as a slug.
    function stageTrackerMergeGateStateWord(state) {
      state = String(state || '');
      if (STAGE_TRACKER_MERGE_GATE_WORD[state]) return STAGE_TRACKER_MERGE_GATE_WORD[state];
      const dup = stageTrackerGateDuplicateOf(state);
      if (dup) return STAGE_TRACKER_TRAIN_STATE_WORD['duplicate-of'] + ' #' + dup;
      if (STAGE_TRACKER_TRAIN_STATE_WORD[state]) return STAGE_TRACKER_TRAIN_STATE_WORD[state];
      const plain = state.replace(/[-_]+/g, ' ').trim();
      return plain ? plain.charAt(0).toUpperCase() + plain.slice(1) : '';
    }
    function stageTrackerTrain(item) {
      const mt = item && item.merge_train;
      return mt && typeof mt === 'object' && mt.state ? mt : null;
    }
    function stageTrackerOrdinal(n) {
      n = Number(n) || 0;
      const r100 = n % 100, r10 = n % 10;
      if (r100 >= 11 && r100 <= 13) return n + 'th';
      return n + (r10 === 1 ? 'st' : r10 === 2 ? 'nd' : r10 === 3 ? 'rd' : 'th');
    }
    // Pure: the train's head number / queue length / this row's position,
    // read from the queue facts (#15395) with the older front/size as the
    // fallback.
    function stageTrackerTrainPlace(mt) {
      return { head: Number(mt.head_number || mt.front) || 0, len: Number(mt.length || mt.size) || 0, pos: Number(mt.position) || 0 };
    }
    // The stalled chip's lead — "Merge queue · stalled — …". The popover
    // and the queue header strip it down to "stalled: …".
    const STAGE_TRACKER_TRAIN_STALLED_LEAD = 'Merge queue · stalled — ';
    // Why a stalled train's head is not moving, by merge_train.head_state
    // (mergeTrainHead* in Go) — short enough for a chevron. A head on a
    // person says what the person has to do (the owner's ask: "waiting on
    // #15425 (needs maintainer merge)"), not "waiting for a maintainer".
    const STAGE_TRACKER_TRAIN_HEAD_STALL_WORD = { 'needs-human': 'needs maintainer merge', ci: 'waiting for CI', soaking: 'soak overran', ready: 'waiting for a merge sweep', merging: 'merge call in flight', fixing: 'being fixed by its author', unknown: 'head not in this view' };
    // Pure: the short reason a stalled head is stuck — the head_state
    // table, a human-decision head told apart by its reason, else the
    // head's own plain-word reason cut at its first clause.
    function stageTrackerTrainStallWhy(mt) {
      const reason = String(mt.head_reason || '').replace(/[.;].*$/, '').trim();
      const state = String(mt.head_state || '');
      if (state === 'needs-human' && /decision/i.test(reason)) return 'needs a human decision';
      return STAGE_TRACKER_TRAIN_HEAD_STALL_WORD[state] || reason;
    }
    // Pure: "Merge queue · stalled — waiting on #15425 (needs maintainer
    // merge) · 1h 20m" for a member behind a head that has sat past
    // governor.merge_train_stall_warn_s; the head's own row reads "Merge
    // queue · stalled — needs maintainer merge · 1h 20m"; '' when not
    // stalled. The server decides stalled; the words say why and how long.
    function stageTrackerTrainStalledWord(mt) {
      if (!mt || !mt.stalled) return '';
      const p = stageTrackerTrainPlace(mt);
      const why = stageTrackerTrainStallWhy(mt);
      const dur = stageTrackerShortLeft((Number(mt.stalled_s) || 0) * 1000);
      const isHead = mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT || p.pos === 1;
      const what = isHead ? (why || 'the head has not moved') : 'waiting on #' + p.head + (why ? ' (' + why + ')' : '');
      return STAGE_TRACKER_TRAIN_STALLED_LEAD + what + (dur ? ' · ' + dur : '');
    }
    // The chevron word for a train member ('' for a row not in a train).
    // The queued word is the place only — "Merge queued · 2nd" — so it
    // reads the same with the merge sweep on or off (#15395); never the
    // state slug, never "front" or "queued-behind", and never '' for a
    // state this table does not know: an unmapped slug reads spelled out
    // with the row's place ("Waiting for front · 3rd").
    function stageTrackerTrainWord(item) {
      const mt = stageTrackerTrain(item);
      if (!mt) return '';
      const stalled = stageTrackerTrainStalledWord(mt);
      if (stalled && (mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT || mt.state === STAGE_TRACKER_TRAIN_STATE_QUEUED)) return stalled;
      const p = stageTrackerTrainPlace(mt);
      switch (mt.state) {
        case STAGE_TRACKER_TRAIN_STATE_FRONT: return STAGE_TRACKER_TRAIN_STATE_WORD[mt.state] + ' · ' + stageTrackerOrdinal(1);
        case STAGE_TRACKER_TRAIN_STATE_QUEUED: return STAGE_TRACKER_TRAIN_STATE_WORD[mt.state] + ' · ' + stageTrackerOrdinal(p.pos || 2);
        case STAGE_TRACKER_TRAIN_STATE_REBASING: return 'Rebasing · attempt ' + (Number(mt.attempt) || 0) + '/' + (Number(mt.max_attempts) || 0);
        case STAGE_TRACKER_TRAIN_STATE_EXHAUSTED: return 'Needs human decision · rebase failed ' + (Number(mt.attempt) === 2 ? 'twice' : (Number(mt.attempt) || 0) + ' times');
      }
      const word = STAGE_TRACKER_TRAIN_STATE_WORD[mt.state] || stageTrackerMergeGateStateWord(mt.state) || STAGE_TRACKER_TRAIN_STATE_WORD[STAGE_TRACKER_TRAIN_STATE_QUEUED];
      return p.pos > 0 ? word + ' · ' + stageTrackerOrdinal(p.pos) : word;
    }
    // Pure: the member's countdown target in ms (eta_at, else now+eta_s);
    // 0 with no estimate.
    function stageTrackerTrainEtaAt(mt, nowMs) {
      if (!mt) return 0;
      const at = mt.eta_at ? Date.parse(mt.eta_at) : 0;
      if (at > 0) return at;
      const s = Number(mt.eta_s) || 0;
      return s > 0 ? nowMs + s * 1000 : 0;
    }
    // Pure: the ETA words — "ETA ~25m (2 ahead · next sweep in 40s)",
    // "running late — estimated ~25m, still waiting", or '' with no
    // estimate. Always a "~": an estimate, not a promise.
    function stageTrackerTrainEtaWords(mt, nowMs) {
      const at = stageTrackerTrainEtaAt(mt, nowMs);
      if (!(at > 0)) return '';
      const left = stageTrackerShortLeft(at - nowMs);
      const basis = String(mt.eta_basis || '').trim();
      if (mt.late) return 'Running late — estimated ~' + stageTrackerShortLeft((Number(mt.eta_s) || 0) * 1000) + ', still waiting' + (basis ? ' (' + basis + ')' : '') + '.';
      return 'ETA ' + (left ? '~' + left : 'any sweep now') + (basis ? ' (' + basis + ')' : '') + '.';
    }
    // Pure: the queued row's hover — "2nd of 6 in the merge queue · behind
    // #15253 (soaking, 12m left) · ETA ~25m. Hive merges one at a time, in
    // order." / "1st of 6 in the merge queue · merges next · ETA ~4m." A
    // head on a person or CI has no estimate, so the hover names what the
    // head waits on instead. Never "front" or "queued-behind".
    function stageTrackerTrainQueueTip(mt, nowMs) {
      if (!mt) return '';
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      const p = stageTrackerTrainPlace(mt);
      const why = String(mt.head_reason || '').trim();
      const etaWords = stageTrackerTrainEtaWords(mt, nowMs).replace(/\.$/, '');
      const stalledFor = stageTrackerShortLeft((Number(mt.stalled_s) || 0) * 1000);
      const order = ' Hive merges one at a time, in order.';
      const first = mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT || (p.pos === 1 && mt.state === STAGE_TRACKER_TRAIN_STATE_QUEUED);
      if (first) {
        let tip = stageTrackerOrdinal(1) + ' of ' + Math.max(p.len, 1) + ' in the merge queue · merges next' + (why ? ' (' + why + ')' : '');
        if (mt.stalled) tip += ' · stalled: it has not moved for ' + stalledFor;
        else if (etaWords) tip += ' · ' + etaWords;
        return tip + '.' + order;
      }
      let tip = stageTrackerOrdinal(p.pos || 2) + ' of ' + Math.max(p.len, p.pos || 2) + ' in the merge queue · behind #' + p.head + (why ? ' (' + why + ')' : '');
      if (mt.stalled) tip += ' · merge queue stalled: #' + p.head + ' has not moved for ' + stalledFor + (why ? ' — ' + why : '') + '; no estimate until it moves';
      else if (etaWords) tip += ' · ' + etaWords;
      else if (why) tip += ' · no estimate until #' + p.head + ' moves';
      return tip + '.' + order;
    }
    // Tooltip lines: the queue ahead and the overlapping files.
    function stageTrackerTrainLines(item) {
      const mt = stageTrackerTrain(item);
      if (!mt) return [];
      const ahead = Array.isArray(mt.ahead) ? mt.ahead.filter(n => Number(n) > 0) : [];
      const files = Array.isArray(mt.overlap_files) ? mt.overlap_files.filter(Boolean) : [];
      const lines = ['Merge train: ' + (Number(mt.size) || 0) + ' PRs to ' + (String(mt.id || '').split('#')[0] || 'the same base') + ' touch the same files; they merge one at a time, in order' + (mt.front ? ' (1st: #' + mt.front + ')' : '')];
      if (mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT) lines.push('Position: 1st — merges next; the lane then updates or rebases the others');
      else lines.push('Position: ' + stageTrackerOrdinal(mt.position) + ' of ' + (Number(mt.size) || 0) + (ahead.length ? ' · ahead: ' + ahead.map(n => '#' + n).join(', ') : ''));
      if (mt.state === STAGE_TRACKER_TRAIN_STATE_REBASING) lines.push('The PR ahead merged; rebase attempt ' + (Number(mt.attempt) || 0) + ' of ' + (Number(mt.max_attempts) || 0) + ' in progress');
      if (mt.state === STAGE_TRACKER_TRAIN_STATE_EXHAUSTED) lines.push('Rebase failed ' + (Number(mt.attempt) || 0) + ' of ' + (Number(mt.max_attempts) || 0) + ' times — a maintainer rebases, then merges');
      if (files.length) lines.push('Overlapping files: ' + files.join(', '));
      return lines;
    }
    // Pure: the train a row rides — a PR's own merge_train, or, for an
    // issue row, the merge_train of its open linked PR (the PR drawn as the
    // issue's sub-row, nested_under). Returns { mt, pr } or null. The train
    // header must sit over every member that is on the page: hive's
    // v5#15425 read "3 PRs" over one row because #15529 and #15530 were
    // drawn under their issues #15493 and #15523 and the header only
    // counted PR rows.
    function stageTrackerRowTrain(item) {
      if (!item) return null;
      const own = stageTrackerTrain(item);
      if (own) return { mt: own, pr: Number(item.number) || 0 };
      if (item.kind === 'pr') return null;
      const prs = Array.isArray(item.linked_prs) ? item.linked_prs : [];
      const open = prs.filter(lp => lp && stageTrackerTrain(lp) && !lp.superseded && String(lp.state || '') !== 'closed' && (Number(lp.stage) || 0) < STAGE_TRACKER_TERMINAL && (Array.isArray(lp.modifiers) ? lp.modifiers : []).indexOf('merged') < 0);
      if (!open.length) return null;
      open.sort((a, b) => (Number(a.merge_train.position) || 0) - (Number(b.merge_train.position) || 0) || ((Number(a.number) || 0) - (Number(b.number) || 0)));
      return { mt: stageTrackerTrain(open[0]), pr: Number(open[0].number) || 0 };
    }
    function stageTrackerTrainKey(item) {
      const rt = stageTrackerRowTrain(item);
      return rt ? (item.repo || '') + '|' + (rt.mt.id || ('#' + rt.mt.front)) : '';
    }
    // Pure: the member's merge order on its row (position, then PR number).
    function stageTrackerTrainOrder(item) {
      const rt = stageTrackerRowTrain(item);
      return rt ? [Number(rt.mt.position) || 0, rt.pr] : [0, 0];
    }
    // Pure: re-orders a sorted row list so each train's members sit together
    // at the first member's slot, in merge order (front first). An issue
    // row whose nested PR is a member is a member row.
    function stageTrackerTrainContiguous(rows) {
      rows = Array.isArray(rows) ? rows : [];
      const byTrain = {};
      rows.forEach(it => { const k = stageTrackerTrainKey(it); if (k) (byTrain[k] = byTrain[k] || []).push(it); });
      const emitted = {};
      const out = [];
      rows.forEach(it => {
        const k = stageTrackerTrainKey(it);
        if (!k) { out.push(it); return; }
        if (emitted[k]) return;
        emitted[k] = true;
        byTrain[k].slice().sort((a, b) => { const x = stageTrackerTrainOrder(a), y = stageTrackerTrainOrder(b); return (x[0] - y[0]) || (x[1] - y[1]); }).forEach(m => out.push(m));
      });
      return out;
    }
    // Pure: the member numbers of a train the page does not show, given the
    // train's record (ahead + size on the last member tells the whole
    // queue when the view carries it) and the numbers shown under the
    // header. shown is the PR numbers rendered in this run.
    function stageTrackerTrainMissing(mt, shown) {
      const known = {};
      (Array.isArray(mt.members) ? mt.members : []).forEach(n => { if (Number(n) > 0) known[Number(n)] = true; });
      if (Number(mt.front) > 0) known[Number(mt.front)] = true;
      (Array.isArray(mt.ahead) ? mt.ahead : []).forEach(n => { if (Number(n) > 0) known[Number(n)] = true; });
      const here = {};
      (shown || []).forEach(n => { here[Number(n)] = true; });
      return Object.keys(known).map(Number).filter(n => !here[n]).sort((a, b) => a - b);
    }
    // Header line over a train's rows: "merge train · 4 PRs · 1st: #15074".
    // The count is the rows drawn under the header, never the server's
    // size alone: with shown (the PR numbers rendered in this run) a
    // train the view only partly carries reads "merge train · 1 of 3 PRs
    // here · 1st: #15425 · not in this view: #15529, #15530" so the header
    // never claims rows that are not there (the train icon sits in front
    // of it in the HTML).
    function stageTrackerTrainHeaderText(mt, shown) {
      const size = Number(mt.size) || 0;
      const front = Number(mt.front) || 0;
      if (!Array.isArray(shown)) return 'merge train · ' + size + ' PRs · 1st: #' + front;
      const here = shown.length;
      let text = 'merge train · ' + (here < size ? here + ' of ' + size + ' PRs here' : here + (here === 1 ? ' PR' : ' PRs')) + ' · 1st: #' + front;
      if (here < size) {
        const missing = stageTrackerTrainMissing(mt, shown);
        const named = missing.map(n => '#' + n);
        const unnamed = size - here - missing.length;
        if (unnamed > 0) named.push(unnamed + ' more');
        text += ' · not in this view: ' + (named.length ? named.join(', ') : (size - here) + ' PRs');
      }
      return text;
    }
    function stageTrackerTrainHeaderHtml(item, shown) {
      const rt = stageTrackerRowTrain(item);
      if (!rt) return '';
      const mt = rt.mt;
      const files = Array.isArray(mt.overlap_files) ? mt.overlap_files.filter(Boolean) : [];
      const tip = stageTrackerTrainLines(item.kind === 'pr' ? item : { merge_train: mt }).join('\n');
      return `<div class="ham-train-head" data-ham-train="${esc(stageTrackerTrainKey(item))}" role="row" title="${esc(tip)}">
        <span class="ham-train-chip">${stageTrackerTrainIconHtml('ham-train-icon')} ${esc(stageTrackerTrainHeaderText(mt, shown))}</span>${files.length ? `<span class="ham-train-files">overlapping: ${esc(files.join(', '))}</span>` : ''}
      </div>`;
    }
    // Row classes for a train member: the rail, and whether it leads / ends
    // the run (prev/next are the neighbouring rows, or null).
    function stageTrackerTrainRowClass(item, prev, next) {
      const k = stageTrackerTrainKey(item);
      if (!k) return '';
      const rt = stageTrackerRowTrain(item);
      let cls = ' ham-train';
      if (rt.mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT) cls += ' ham-train-front';
      if (!next || stageTrackerTrainKey(next) !== k) cls += ' ham-train-last';
      return cls;
    }
    // Inserts a train header entry before the first row of each train run.
    // Entries are {key, html}; items are the page's row items in order. The
    // header counts the run it heads — the rows actually drawn under it.
    function stageTrackerTrainRows(items, rowOf) {
      const out = [];
      let prevKey = '';
      const list = items || [];
      const itemOf = e => (e && e.item ? e.item : (e && e.group ? null : e));
      list.forEach((e, i) => {
        // Entries are items, or the grouped view's {item} / {group} wrappers.
        const it = itemOf(e);
        const k = it ? stageTrackerTrainKey(it) : '';
        if (k && k !== prevKey) {
          const shown = [];
          for (let j = i; j < list.length; j++) {
            const m = itemOf(list[j]);
            if (!m || stageTrackerTrainKey(m) !== k) break;
            shown.push(stageTrackerRowTrain(m).pr);
          }
          out.push({ key: 'train:' + k, html: stageTrackerTrainHeaderHtml(it, shown) });
        }
        prevKey = k;
        out.push(rowOf(e, i));
      });
      return out;
    }

    function stageTrackerMotion(item) {
      const mods = Array.isArray(item && item.modifiers) ? item.modifiers : [];
      if (mods.indexOf('hold') >= 0) return STAGE_TRACKER_MOTION_STATIC;
      if (mods.some(m => STAGE_TRACKER_MOTION_HUMAN_MODS.indexOf(m) >= 0)) return STAGE_TRACKER_MOTION_HUMAN;
      const n = Number(item && item.stage) || 0;
      if (n >= STAGE_TRACKER_TERMINAL) return STAGE_TRACKER_MOTION_STATIC;
      if (STAGE_TRACKER_MOTION_AUTHOR_STAGES.indexOf(n) >= 0) {
        const ak = stageTrackerActorKind(item);
        return ak === 'hive' || ak === 'clanker' ? STAGE_TRACKER_MOTION_ACTIVE : STAGE_TRACKER_MOTION_STATIC;
      }
      return STAGE_TRACKER_MOTION_BY_STAGE[n] || STAGE_TRACKER_MOTION_STATIC;
    }
    const STAGE_TRACKER_TRAIL_HOPS = 4;
    // Sentinel alert on a PR (payload sentinel_alert, from the sentinel-alert
    // label or the head's evidence bundle): the human-decision lane names
    // it in the chevron word itself; every other lane adds a chip. The
    // words come from the hive's sentinel (possible security override,
    // privilege escalation or codebase damage), so a maintainer reads the
    // security reason before the row's "needs human".
    const STAGE_TRACKER_SENTINEL_WORD = 'Sentinel alert · needs human';
    const STAGE_TRACKER_SENTINEL_CHIP = 'sentinel';
    const STAGE_TRACKER_SENTINEL_TIP = 'Sentinel flagged security-relevant changes';
    const STAGE_TRACKER_SENTINEL_TIP_FALLBACK = 'possible security override, privilege escalation or codebase damage — a maintainer must review before merge';
    // Merge-gate pills the server held back (payload deferred_modifiers:
    // sentinel, needs-human, needs-decision, a merge-time unauthorized
    // step) because the PR has not reached the gate yet: nothing a person
    // does now moves the row, so no pill is drawn and the row stays out of
    // "Needs attention"; the row tooltip alone names what waits at the gate.
    const STAGE_TRACKER_DEFERRED_NOTE = 'Pending at merge queue: ';
    function stageTrackerDeferredMods(item) {
      return Array.isArray(item && item.deferred_modifiers) ? item.deferred_modifiers : [];
    }
    const STAGE_TRACKER_SHORT_SHA = 7;
    var _stageTracker = null;
    var _stageTrackerSig = '';
    var _stageTrackerEtag = '';
    // Last poll outcome: '' while polls succeed, else a short reason ('HTTP
    // 504', 'unreachable'). Without it a payload that was "fresh" when it
    // arrived keeps saying so while every later poll fails — the operator
    // reads "updated 8m ago · fresh" and cannot tell a quiet hive from a
    // dashboard that has lost the server (#14416).
    var _stageTrackerPollError = '';
    var _stageTrackerPollErrorSince = 0;
    // Filter / sort / paging state. Everything but the page survives a reload
    // (localStorage, like the Review window) so a maintainer's view sticks.
    const STAGE_TRACKER_FILTER_KEY = 'hive-activity-monitor-filter';
    const STAGE_TRACKER_PAGE_SIZES = [25, 50, 100];
    const STAGE_TRACKER_DEFAULT_PAGE_SIZE = 50;
    const STAGE_TRACKER_LABEL_CHIP_LIMIT = 24;
    const STAGE_TRACKER_CLOCK_MS = 10000;
    const STAGE_TRACKER_AGE_MS = { '1h': 3600000, '1d': 86400000, '1w': 604800000 };
    const STAGE_TRACKER_ATTENTION_STAGES = [45, 71, 72, 85, 90];
    // "Needs human" (one definition with Go stageTrackerNeedsHuman; the
    // payload's needs_human flag/count come from it too): a person's
    // decision or action is the ONLY thing that advances the row — 015
    // triage, 045/085 needs-decision, 090 human merge; 071/072 only when
    // the author is a human; or a needs-human / needs-decision /
    // unauthorized pill. hold is "On hold" (its own chip), never this.
    const STAGE_TRACKER_NEEDS_HUMAN_STAGES = [15, 45, 85, 90];
    const STAGE_TRACKER_NEEDS_HUMAN_AUTHOR_STAGES = [71, 72];
    const STAGE_TRACKER_NEEDS_HUMAN_MODS = ['needs-human', 'needs-decision', 'unauthorized'];
    const STAGE_TRACKER_NEEDS_HUMAN_TIP = 'Only a person can move these: triage needed, needs decision, human author must act, human merge, or a needs-human / unauthorized pill';
    const STAGE_TRACKER_ON_HOLD_TIP = 'Paused on purpose with a hold label — waiting on nobody; remove the hold to resume';
    // Label families a chip row is built from, in display order; other
    // labels follow when at least two items carry them.
    const STAGE_TRACKER_LABEL_FAMILIES = ['stage/', 'needs-', 'author/', 'role/', 'hold', 'on-hold', 'do-not-merge', 'hive-pause/'];
    const STAGE_TRACKER_SORTS = ['furthest', 'waiting', 'newest', 'number'];
    // Attention tiers within a column (stageTrackerAttentionTier; same
    // values as Go): the pills that sink a row, and the direction button's
    // note that the arrow never flips the tier.
    const STAGE_TRACKER_TIER_ATTENTION = 0;
    const STAGE_TRACKER_TIER_NORMAL = 1;
    const STAGE_TRACKER_TIER_SUNK = 2;
    const STAGE_TRACKER_SUNK_MODS = ['hold', 'draft'];
    const STAGE_TRACKER_DIR_TIER_NOTE = ' · Attention-first within each column; arrow flips age order';
    // Repo chip + group-by-repo. The chip before an item's number names its
    // repo (short name; owner/repo in the tooltip; click = the Repo filter)
    // and is shown always / only when the payload spans several repos
    // (default) / never. Group puts rows under one collapsible header per
    // repo. Both settings and the collapsed set persist per browser.
    const STAGE_TRACKER_REPO_CHIP_KEY = 'hive.tracker.repochip';
    const STAGE_TRACKER_REPO_CHIP_MODES = [
      { id: 'always', label: 'Always' },
      { id: 'multi', label: 'Only when several repos are listed' },
      { id: 'never', label: 'Never' },
    ];
    const STAGE_TRACKER_REPO_CHIP_DEFAULT = 'multi';
    const STAGE_TRACKER_GROUP_KEY = 'hive.tracker.group';
    const STAGE_TRACKER_GROUP_COLLAPSED_KEY = 'hive.tracker.group.collapsed';
    const STAGE_TRACKER_GROUPS = ['none', 'repo'];
    const STAGE_TRACKER_GROUP_DEFAULT = 'none';
    const STAGE_TRACKER_GROUP_KEY_PREFIX = 'group:';
    // Palette slots --ham-repo-tint-0..N-1 in tokens.css.
    const STAGE_TRACKER_REPO_TINTS = 6;
    // Mini stage distribution on a group header: bar height steps.
    const STAGE_TRACKER_GROUP_DIST_STEPS = 4;
    var _stageTrackerGroup = STAGE_TRACKER_GROUP_DEFAULT;
    var _stageTrackerCollapsed = {};
    function stageTrackerDefaultFilter() {
      return { kind: 'all', repo: '', q: '', labels: [], needsHuman: false, onHold: false, age: 'any', author: 'any', attention: false, acted: 'any', sort: 'furthest', dir: 'desc', size: STAGE_TRACKER_DEFAULT_PAGE_SIZE, page: 1 };
    }
    var _stageTrackerFilter = stageTrackerDefaultFilter();
    // Type-to-filter text of the "all labels" dialog (not persisted).
    var _stageTrackerLabelQuery = '';

    function stageTrackerHeadline(item) {
      if (item.superseded && item.superseded.closer_pr) return 'Superseded by #' + item.superseded.closer_pr + ' — decide';
      const n = Number(item.stage) || 0;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const holdLabels = stageTrackerHoldLabels(item);
      if (mods.indexOf('stalled') >= 0) return 'Stalled';
      if (mods.indexOf('regressed') >= 0 && !stageTrackerRegressionHandled(item)) return 'Sent back';
      // Advisory (hivecommons/hive#15381): the chip is the headline.
      if (n === 0 && item.kind !== 'pr' && item.advisory && typeof item.advisory === 'object') return item.advisory.chip || 'Advisory · read & close';
      if (n === 0 && mods.indexOf('hold') >= 0) return 'Open · on hold' + (holdLabels.length ? ' (' + holdLabels.join(', ') + ')' : '');
      if (n === 0 && mods.indexOf('blocked') >= 0) return 'Open · blocked';
      if (n === 0 && mods.indexOf('draft') >= 0) return 'Open · draft';
      // Guarded: test harnesses lift this function by name without the 000-new helpers.
      if (n === 0 && typeof stageTrackerOpenTriageNeeded === 'function' && stageTrackerOpenTriageNeeded(item)) return 'Triage needed';
      if (n === 0 && typeof stageTrackerNewState === 'function' && stageTrackerNewState(item)) return 'New';
      // An issue parked on a person (045, or a pre-045 085 with no covering
      // PR) is parked itself, so no "(its PR)" suffix.
      const viaPR = item.kind === 'issue' && n >= 50 && (n !== 85 || item.via_pr) ? ' (its PR)' : '';
      switch (n) {
        case 0: return 'Open';
        case 10: return 'Waiting for a worker';
        case 15: return 'Triage needed';
        case 17: return 'Possibly exists · needs author';
        case 20: return 'Claimed';
        case 30: return 'PR in progress';
        case 40: return 'Covered by a PR, not yet in review';
        case 45: return 'Needs a human decision';
        case 50: return 'Waiting for review' + viaPR;
        case 60: return 'Reviewed' + viaPR;
        case 70: return 'Fixing review findings' + viaPR;
        case 71: return 'Waiting on the author' + viaPR;
        case 72: return 'Back with the contributor' + viaPR;
        case 74: return 'Hive took it over' + viaPR;
        case 80: return 'Not yet admitted to auto merge' + viaPR;
        case 85: return 'Needs a human decision' + viaPR;
        case 90: return 'Ready for a human merge' + (stageTrackerHumanMergeReasonWord(item) ? ' · ' + stageTrackerHumanMergeReasonWord(item) : '') + viaPR;
        case 95: return 'Pre-merge soak' + stageTrackerSoakLeft(item) + viaPR;
        case 100: return 'Admitted — merging' + viaPR;
        case 110: return 'Merged';
        default: return item.stage_label || 'Tracking';
      }
    }

    // " — Xm left" for a 095 row whose payload carries the soak remaining.
    function stageTrackerSoakLeft(item) {
      const sec = Number(item.soak_remaining_seconds) || (item.merge_gate && Number(item.merge_gate.soak_remaining_seconds)) || 0;
      if (sec <= 0) return '';
      const min = Math.ceil(sec / 60);
      return ' — ' + (min >= 60 ? Math.floor(min / 60) + 'h' + (min % 60 ? (min % 60) + 'm' : '') : min + 'm') + ' left';
    }

    /* ---- Merge gate (hivecommons/hive#15073) --------------------------------
       Pure helpers over the payload's merge_gate {state, reason,
       sweep_running, last_sweep_at, next_sweep_at, soak_until,
       soak_remaining_seconds}: the pill word with its live detail, the
       plain-words sentence the leading glyph carries as tooltip + aria-label,
       and the glyph itself. Every row on the gate gets the glyph — a sweep
       on it now spins, a wait shows the clock — so two rows in the same
       state never differ. Countdowns tick in place (data-ham-until) from the
       cue age timer; no re-render. Run under node by
       stage_tracker_merge_gate_test.go. */
    // Pure: a stage number's merge-gate state when the payload has none.
    function stageTrackerMergeGateStateOf(item) {
      if (item && item.merge_gate && item.merge_gate.state) return String(item.merge_gate.state);
      const n = Number(item && item.stage) || 0;
      if (n === 60) return 'gate-pending';
      if (n === 80) return 'ready';
      if (n === STAGE_TRACKER_PRE_MERGE_SOAK) return 'soaking';
      if (n === 100) return 'merging';
      if (n >= STAGE_TRACKER_TERMINAL) return 'merged';
      return '';
    }
    // Pure: "42s" / "3m" / "1h05m" for a positive duration; '' otherwise.
    function stageTrackerShortLeft(ms) {
      if (!(ms > 0)) return '';
      const s = Math.ceil(ms / 1000);
      if (s < 60) return s + 's';
      const m = Math.ceil(s / 60);
      if (m < 60) return m + 'm';
      return Math.floor(m / 60) + 'h' + (m % 60 ? String(m % 60).padStart(2, '0') + 'm' : '');
    }
    // Pure: the gate's reason token in plain words — "CI running
    // (build-and-test)", "CI failed: lint"; '' for none. Pending is never
    // "red" (hivecommons/docs#512 read "CI is red (check-pending)").
    const STAGE_TRACKER_GATE_REASON_WORDS = {
      'check-pending': 'CI running', 'required-check-pending': 'CI running', 'check-failing': 'CI failed', 'required-check-failing': 'CI failed',
      'optional-check-failing': 'an optional check is red', conflicting: 'branch has conflicts', 'needs-rebase': 'needs a rebase', 'dco-missing': 'DCO sign-off missing',
    };
    // Pure: the 060 "Review passed" pill's detail from the lane's recorded
    // reason (stagelabels RouteHuman): "CI in progress" (checks pending),
    // "checks unreadable", "soak 30m remaining"; a gate token falls back to
    // its gate words; '' when there is no reason.
    function stageTrackerReviewPassedDetail(reason) {
      const raw = String(reason || '').trim();
      if (!raw) return '';
      const lower = raw.toLowerCase();
      if (/^(required-)?check-pending\b|^ci-unverified$|check\(s\) still running|^pending\b|-pending\b/.test(lower)) return 'CI in progress';
      if (/could not be read|unreadable|checks-unknown|^ci-unknown$/.test(lower)) return 'checks unreadable';
      const soak = /soak(?:ing)?\W*(?:remaining=)?(\d+(?:h\d+)?(?:m\d*)?(?:s)?)\s*(?:remaining)?/.exec(lower);
      if (soak) return 'soak ' + soak[1].replace(/(\d+m)0s$/, '$1').replace(/(\d+h)0m$/, '$1') + ' remaining';
      return stageTrackerGateReasonText(raw);
    }
    function stageTrackerGateReasonText(reason) {
      const raw = String(reason || '').trim();
      if (!raw) return '';
      const i = raw.indexOf(':');
      const slug = (i >= 0 ? raw.slice(0, i) : raw).replace(/\(.*$/, '');
      const detail = i >= 0 ? raw.slice(i + 1).replace(/\(.*$/, '') : '';
      const words = STAGE_TRACKER_GATE_REASON_WORDS[slug];
      if (!words) return raw;
      if (!detail) return words;
      return /failed$/.test(words) ? words + ': ' + detail : words + ' (' + detail + ')';
    }
    // Pure: "open PR #15090 covers it · at 060-reviewed" from an issue row's
    // lane reason ("open PR #15090 covers it (PR at 060-reviewed)"); '' for
    // any other reason. The lane reason is the issue's, never a gate token,
    // so it must not be read as one (hivecommons/hive#15084 regression).
    const STAGE_TRACKER_COVER_REASON_RE = /^open PR #(\d+) covers it(?: \(PR at ([^)]*)\))?$/i;
    // Stage → operator phrase, mirrored from stageTrackerStagePhrase
    // (stage_tracker_vocabulary.go): every string the page renders names a
    // stage by its chevron's words, never by a code ("060 reviewed").
    const STAGE_TRACKER_STAGE_PHRASE = { 10: 'actionable', 15: 'waiting for triage', 17: 'waiting for the author to confirm the gap (prior art found)', 20: 'claimed', 30: 'a PR in progress', 40: 'covered by a PR', 45: 'waiting for a human decision', 50: 'in CI / review', 60: 'review passed · waiting for merge admission', 70: 'being fixed by hive', 71: 'waiting for its author', 72: 'returned to the contributor', 74: 'adopted by hive', 80: 'at the merge queue', 85: 'waiting for a human decision', 90: 'ready for human merge', 95: 'soaking before merge', 100: 'in the merge queue' };
    // Pure: a stage's name in plain words for cues and tooltips — its short
    // word when the stages table has one, else its label with the lane's
    // "stage/060-" slug prefix dropped and the slug humanised ("Reviewed").
    // Minimal local fix for "fell back to stage/060-reviewed"
    // (hivecommons/docs#512); the copy sweep retires stage slugs globally.
    function stageTrackerStageName(st) {
      if (!st) return '';
      if (st.short) return String(st.short);
      const label = String(st.label || '').trim();
      const m = /^(?:stage\/)?\d{3}-(.+)$/.exec(label);
      if (!m) return label;
      const words = m[1].replace(/[-_]+/g, ' ');
      return words.charAt(0).toUpperCase() + words.slice(1);
    }
    // Pure: a stage code as the lane spells it ("060-reviewed", "stage/060-reviewed") → its phrase; the input when unknown.
    function stageTrackerStagePhrase(code) {
      const m = /^(?:stage\/)?(\d{3})(?:[- ][a-z-]+)?$/i.exec(String(code || '').trim());
      const n = m ? Number(m[1]) : Number(code);
      return STAGE_TRACKER_STAGE_PHRASE[n] || String(code || '');
    }
    function stageTrackerCoverReasonText(item) {
      const m = STAGE_TRACKER_COVER_REASON_RE.exec(String((item && item.reason) || '').trim());
      if (!m) return '';
      return 'open PR #' + m[1] + ' covers it' + (m[2] ? ' · its PR is ' + stageTrackerStagePhrase(m[2]) : '');
    }
    // Pure: the nested PR an issue row mirrors — the one its lane reason
    // names, else the linked PR at the issue's mirrored stage. null for PR
    // rows and for issues with no linked PR. The issue row reads the gate
    // from it so the row and its PR sub-row never disagree.
    // hivecommons/hive#15126 — the post-decision state machine, read from
    // the server's `decided` projection:
    //   deciding   the job runs: spinner caption inside the pill, no badge
    //   decided    verdict recorded, effect pending: the row sits on the
    //              decided stage; pill = decided.text (+ countdown); ✓ badge
    //              with decided.title; the in-flight caption names the
    //              decision ("Adopting · opening hive copy…"), never a
    //              verb from another decision
    //   landed     effect visible: adoption pair explicit, no projection
    //   timed-out  no effect past decisions.delegate.effect_timeout: the
    //              row is back on its real stage, badge amber, options
    // Pure: the merge-gate-info shape for a pending decision, else null.
    function stageTrackerDecidedOf(it, nowMs) {
      const d = it && it.decided;
      if (!d || d.effect !== 'pending' || !d.text) return null;
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      const sep = String(d.text).indexOf(' · ');
      const word = sep > 0 ? d.text.slice(0, sep) : (d.word || d.text);
      let detail = sep > 0 ? d.text.slice(sep + 3) : '';
      const until = d.next_at ? Date.parse(d.next_at) : 0;
      if (until > nowMs) detail += (detail ? ' · ' : '') + 'in ' + stageTrackerShortLeft(until - nowMs);
      return { state: 'decided', word, detail, tip: d.title || d.text, cue: 'waiting', until: until > nowMs ? until : 0, decided: d };
    }
    // Pure: the in-flight caption for a recorded decision — from the
    // server's projection when it has one, else from the result's action.
    const STAGE_TRACKER_DECIDED_ACTING = { adopted: 'Adopting · opening hive copy…', approved: 'Approving…', directed: 'Sending back…', closed: 'Closing…', merged: 'Merging…', 'queued-for-merge': 'Queuing for merge…', recommended: 'Recommendation recorded' };
    function stageTrackerDecidedActing(result, item) {
      const d = item && item.decided;
      if (d && d.acting) return d.acting;
      return STAGE_TRACKER_DECIDED_ACTING[result && result.action] || 'Hive decided…';
    }
    // Pure: the ✓ badge's first line for a decided row: the decision name
    // and who / what triggered it — "you · Let hive decide · hive chose
    // adopt-supersede, 17:51".
    function stageTrackerDecidedLead(d, a, nowMs) {
      if (!d) return '';
      const atMs = Date.parse(d.at || (a && a.at) || '') || nowMs;
      const me = !d.actor || (window._hiveUser && String(d.actor).toLowerCase() === String(window._hiveUser).toLowerCase());
      const who = me ? 'you' : '@' + d.actor;
      const chose = d.action === 'let-hive-decide' ? 'Let hive decide · hive chose ' + (d.verdict || '?') : stageTrackerActedLabel(d.action);
      const lead = who + ' · ' + chose + ', ' + agentNowClockTime(atMs, nowMs);
      return lead + (d.title ? '\n' + d.title : '');
    }
    // Pure: an adopted original listed under its issue — closed, or still
    // open with the hive's copy already in the list — draws as one dimmed
    // line without a chevron track (hivecommons/hive#15126).
    function stageTrackerLinkedSupersededText(lp, issue) {
      if (!lp) return '';
      const by = Number(lp.superseded_by || lp.adopted_by) || 0;
      if (String(lp.state || '') === 'closed') return 'closed · superseded by #' + by;
      if (by > 0 && lp.adopted_by_state !== 'closed' && lp.adopted_by_state !== 'closed-twice') {
        const linked = Array.isArray(issue && issue.linked_prs) ? issue.linked_prs : [];
        if (linked.some(x => Number(x.number) === by)) return 'Adopted → #' + by + ' · superseded (hive re-authored with sign-off)';
      }
      return '';
    }
    function stageTrackerCoveringPR(item) {
      if (!item || item.kind === 'pr') return null;
      const prs = Array.isArray(item.linked_prs) ? item.linked_prs : [];
      if (!prs.length) return null;
      const m = STAGE_TRACKER_COVER_REASON_RE.exec(String(item.reason || '').trim());
      const n = m ? Number(m[1]) : 0;
      return prs.find(p => n && Number(p.number) === n) || prs.find(p => Number(p.stage) === Number(item.stage)) || null;
    }
    // Pure: { word, detail, tip, cue } for an item on the gate, null off it.
    //   word   — the pill word (STAGE_TRACKER_MERGE_GATE_WORD)
    //   detail — the live part after " · " ("next sweep in 42s", "7m left");
    //            always a gate fact, never the issue's lane reason
    //   tip    — one plain sentence for the glyph's title + aria-label; on
    //            an issue row it leads with "open PR #N covers it · at <stage>"
    //   cue    — 'running' (sweep on it now) | 'waiting' | 'soaking' | 'merging' | ''
    //   until  — ms epoch the detail counts down to (0 when none)
    function stageTrackerMergeGateInfo(item, nowMs) {
      const rb = item && item.rebase;
      if (rb && rb.text && !(item.rebase_merge && item.rebase_merge.state && item.rebase_merge.state !== 'failed' && item.rebase_merge.state !== 'merged')) {
        // The fixer's autonomous rebase-resolve loop guard: "Rebasing ·
        // attempt 1/2" spins; past the cap the row asks for a person.
        const tip = rb.exhausted
          ? 'hive rebased this PR ' + rb.max + ' times and GitHub still reports conflicts' + (rb.head ? ' on ' + rb.head : '') + '. A person has to resolve it (or Adopt into fresh branch / Close as superseded from the row menu).'
          : "hive's fixer is rebasing this PR onto its base branch and resolving conflicts (attempt " + rb.attempt + ' of ' + rb.max + '). A pushed head that is still conflicting is the next attempt.';
        return { state: rb.exhausted ? 'rebase-failed' : 'rebasing', word: rb.text, detail: '', tip, cue: rb.exhausted ? 'waiting' : 'running', until: 0 };
      }
      const cover = stageTrackerCoveringPR(item);
      const src = cover && cover.merge_gate && cover.merge_gate.state ? cover : item;
      const state = stageTrackerMergeGateStateOf(src);
      // A decided row may sit on a stage with no merge_gate yet; the
      // decision still owns the pill (#15126).
      if (!state && typeof stageTrackerDecidedOf === 'function' && (stageTrackerDecidedOf(src, nowMs) || stageTrackerDecidedOf(item, nowMs))) return stageTrackerMergeGateInfoFor(src, item, nowMs);
      if (!state || state === 'merged') return null;
      const info = stageTrackerMergeGateInfoFor(src, item, nowMs);
      const coverWhy = stageTrackerCoverReasonText(item);
      if (coverWhy) info.tip = coverWhy.charAt(0).toUpperCase() + coverWhy.slice(1) + ' — ' + info.tip;
      return info;
    }
    // src carries the merge_gate / stage read; item is the row itself (its
    // lane reason stands in for a gate reason only on a PR row).
    function stageTrackerMergeGateInfoFor(src, item, nowMs) {
      const state = stageTrackerMergeGateStateOf(src);
      const mg = (src && src.merge_gate) || {};
      // A recorded decision whose effect is pending (hivecommons/hive#15126):
      // the pill reads the decision, not the gate — "Adopted · hive rebases
      // & merges next pass · in 3m".
      const dec = typeof stageTrackerDecidedOf === 'function' ? (stageTrackerDecidedOf(src, nowMs) || stageTrackerDecidedOf(item, nowMs)) : null;
      if (dec) return dec;
      const laneReason = item && item.kind !== 'issue' && !stageTrackerCoverReasonText(item) ? item.reason : '';
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      const word = stageTrackerMergeGateStateWord(state);
      const nextAt = mg.next_sweep_at ? Date.parse(mg.next_sweep_at) : 0;
      const lastAt = mg.last_sweep_at ? Date.parse(mg.last_sweep_at) : 0;
      const lastSeen = lastAt > 0 ? ' Last checked ' + stageTrackerAgoText(lastAt, nowMs) + '.' : '';
      // Two open PRs for one issue (hivecommons/hive#15395): the queue
      // kept one and holds this one — "Duplicate of #15402 · needs a
      // pick"; no clock, no train. Let hive decide on either row closes
      // the held one as superseded.
      const dupOf = stageTrackerGateDuplicateOf(state);
      if (dupOf) {
        const issue = Number(mg.duplicate_issue) || 0;
        const tip = 'This PR and #' + dupOf + ' both fix' + (issue ? ' #' + issue : ' the same issue') + ". Hive's merge queue carries one PR per issue and kept #" + dupOf + ' (a green head first, then the earlier-opened); this one is held out of the queue. Let hive decide on either row closes this one as superseded with credit, or close it yourself.' + lastSeen;
        return { state, word: word, detail: 'needs a pick', tip, cue: 'waiting', until: 0, duplicateOf: dupOf };
      }
      // A merge-train member (hivecommons/hive#15395): the label is its
      // place in the queue and nothing else, so the chip does not flip
      // between "… · merge sweep running" and the bare form each sweep,
      // and a front in its soak keeps its place word (never the slug) —
      // its countdown already includes the soak left.
      // The sweep is the spinning ⬡ and a "Checking now…" tooltip line.
      const trainSrc = stageTrackerTrain(src) ? src : (stageTrackerTrain(item) ? item : null);
      const mt = trainSrc ? stageTrackerTrain(trainSrc) : null;
      if (mt && (state === STAGE_TRACKER_TRAIN_STATE_FRONT || state === STAGE_TRACKER_TRAIN_STATE_QUEUED || state === STAGE_TRACKER_TRAIN_STATE_REBASING || state === STAGE_TRACKER_TRAIN_STATE_EXHAUSTED)) {
        const trainWord = stageTrackerTrainWord(trainSrc) || STAGE_TRACKER_TRAIN_STATE_WORD[STAGE_TRACKER_TRAIN_STATE_QUEUED];
        const queued = state === STAGE_TRACKER_TRAIN_STATE_FRONT || state === STAGE_TRACKER_TRAIN_STATE_QUEUED;
        const queueTip = queued ? stageTrackerTrainQueueTip(mt, nowMs) : stageTrackerTrainLines(trainSrc).join('. ') + '.';
        const checking = mg.sweep_running ? ' Checking now…' : '';
        // The countdown to its turn ("· ~4m") ticks from eta_at like the
        // soak clock, so a poll re-render does not reset it; a blocked
        // head or a stalled train shows no clock; past twice the first
        // estimate it reads "running late".
        const etaAt = queued && !mt.stalled ? stageTrackerTrainEtaAt(mt, nowMs) : 0;
        const detail = etaAt > 0 ? (mt.late ? 'running late' : stageTrackerTrainEtaDetail(etaAt, nowMs)) : '';
        return { state, word: trainWord, detail, tip: queueTip + checking + lastSeen, cue: mg.sweep_running ? 'running' : 'waiting', until: etaAt > 0 && !mt.late ? etaAt : 0, train: mt, stalled: !!mt.stalled };
      }
      // A soak clock (own or mirrored) outranks "merge sweep running": the
      // sweep reads the clock, it does not replace it. Any gate stage the
      // lane holds for the soak — 095 admitted, or 060/080 while trust
      // still blocks the head — counts the same clock down.
      const clock = stageTrackerSoakClock(src, item, nowMs);
      if (clock && (state === 'soaking' || clock.fromSoak)) {
        // At 060/080 the stage keeps its own word and the detail says
        // "soak 12m left"; at 095 the soak is the stage.
        clock.prefix = state === 'soaking' ? '' : 'soak ';
        const soak = stageTrackerSoakWords(clock, nowMs, nextAt, !!mg.sweep_running);
        return { state, word: clock.restarted || state === 'soaking' ? soak.word : word, detail: soak.detail, tip: soak.tip + lastSeen, cue: 'soaking', until: clock.restarted ? 0 : clock.until, soak: soak.tip, soakClock: clock };
      }
      // 060 Review passed: the pill carries the lane's own recorded reason
      // ("CI in progress", "checks unreadable", "soak 30m remaining" —
      // stagelabels RouteHuman), in the CI / Review column; the spinning cue
      // says the sweep is on it now.
      const preWhy = state === 'gate-pending' ? (stageTrackerReviewPassedDetail(mg.reason || laneReason) || 'waiting for merge admission') : '';
      if (state === 'merge-failed') return stageTrackerMergeFailedInfo(mg, nowMs, nextAt, lastSeen);
      if (mg.sweep_running) {
        return { state, word, detail: preWhy || 'merge sweep running', tip: (preWhy ? 'Review passed, not yet admitted to auto merge — ' + preWhy + '; ' : '') + "hive's merge sweep is running on this PR now.", cue: 'running', until: 0 };
      }
      const nextWords = nextAt > nowMs ? 'next sweep in ' + stageTrackerShortLeft(nextAt - nowMs) : (nextAt > 0 ? 'sweep due now' : '');
      const sooner = ' A GitHub event on the PR (check run, review, push) triggers a pass sooner.';
      if (state === 'soaking') {
        return { state, word, detail: 'soak window complete', tip: 'Admitted; the soak window is complete — hive merges on its next sweep' + (nextWords ? ' (' + nextWords + ')' : '') + '.' + lastSeen, cue: 'soaking', until: 0 };
      }
      if (state === 'ready') {
        return { state, word, detail: nextWords, tip: "Approved and green; waiting for hive's merge sweep" + (nextWords ? ' — ' + nextWords + '.' : '.') + sooner + lastSeen, cue: 'waiting', until: nextAt > nowMs ? nextAt : 0 };
      }
      if (state === 'merging') {
        // A live merge call (merging_since, hivecommons/hive#15245) says
        // who issued it and how long ago; a held row (the SPA keeping
        // "Merging" lit for the toast's lifetime after the merge landed)
        // says the ✓ follows. Without either, the lane's 100 record.
        const since = mg.merging_since ? Date.parse(mg.merging_since) : 0;
        const sinceWords = since > 0 ? ' since ' + stageTrackerAgoText(since, nowMs) : '';
        const who = STAGE_TRACKER_MERGE_PATH_WORDS[mg.merge_path] || '';
        if (mg.held) return { state, word, detail: 'merge landed', tip: 'hive merged it' + (who ? ' (' + who + ')' : '') + sinceWords + '; the row advances to ✓ in a moment.', cue: 'merging', until: 0 };
        if (since > 0) return { state, word, detail: 'merge call in flight', tip: 'hive is merging it now' + (who ? ' (' + who + ')' : '') + ' — the merge call has been in flight' + sinceWords + '.', cue: 'merging', until: 0 };
        return { state, word, detail: '', tip: 'Admitted and soaked; hive is merging it now (GitHub merge queue / merge window).' + lastSeen, cue: 'merging', until: 0 };
      }
      const why = stageTrackerGateReasonText(mg.reason || laneReason);
      return { state, word, detail: preWhy, tip: 'Review passed, not yet admitted to auto merge' + (why ? ' — ' + why : '') + '.' + (nextWords ? ' hive re-checks the gate on its ' + nextWords.replace(/^next /, 'next ') + '.' : '') + sooner + lastSeen, cue: 'waiting', until: 0 };
    }
    // Refusal kind (merge_gate.merge_failed_kind, stageTrackerMergeRefusal in
    // Go) → the chip's short detail. The chip never carries the raw error
    // (hivecommons/docs#512 drew "PUT https://api.github.com/…" across the
    // column); the plain sentence and the raw text ride the tooltip.
    const STAGE_TRACKER_MERGE_REFUSAL_DETAIL = { protection: 'branch protection', review: 'review required', method: 'merge method off', 'merge-queue': 'merge queue only', 'head-moved': 'needs rebase', forbidden: 'not allowed', refused: 'GitHub refused' };
    // Pure: an older payload's raw refusal → its kind and sentence (the Go
    // helper's mapping, abridged); 'refused' when nothing matches.
    function stageTrackerMergeRefusalFromRaw(why) {
      const m = /(\d+) of (\d+) required status checks? (?:are|is) expected/i.exec(why);
      if (m) return { kind: 'checks-pending', text: m[1] + ' required check' + (m[1] === '1' ? " hasn't" : "s haven't") + ' finished', checks: Number(m[1]) };
      if (/required status check ["']([^"']+)["'] is expected/i.test(why)) return { kind: 'checks-pending', text: 'a required check hasn\'t finished', checks: 1 };
      if (/\b409\b|branch was modified/i.test(why)) return { kind: 'head-moved', text: 'the branch moved under the merge call; it needs a rebase', checks: 0 };
      if (/\b403\b|resource not accessible|forbidden/i.test(why)) return { kind: 'forbidden', text: "hive isn't allowed to merge here", checks: 0 };
      if (/\b405\b|not mergeable|blocked|protected branch/i.test(why)) return { kind: 'protection', text: 'branch protection blocks it', checks: 0 };
      return { kind: 'refused', text: 'GitHub refused the merge', checks: 0 };
    }
    // Pure: the merge-failed pill. A refusal because required checks have
    // not reported is not a failure — the sweep called too early — so the
    // chip reads "Waiting for CI · 4 checks"; a sweep retrying now reads
    // "Merge failed · retrying"; otherwise "Merge failed · <short reason>".
    // The chip is a fixed short phrase (≤ 5 words); the tooltip carries the
    // plain sentence and GitHub's raw text after "Details:".
    function stageTrackerMergeFailedInfo(mg, nowMs, nextAt, lastSeen) {
      const state = 'merge-failed';
      const why = String(mg.merge_failed || '').trim();
      const fallback = why && !mg.merge_failed_kind ? stageTrackerMergeRefusalFromRaw(why) : null;
      const kind = mg.merge_failed_kind || (fallback && fallback.kind) || '';
      const text = mg.merge_failed_text || (fallback && fallback.text) || '';
      const checks = Number(mg.merge_failed_checks) || (fallback && fallback.checks) || 0;
      const at = mg.merge_failed_at ? Date.parse(mg.merge_failed_at) : 0;
      const who = STAGE_TRACKER_MERGE_PATH_WORDS[mg.merge_path] || '';
      const when = (who ? ' (' + who + ')' : '') + (at > 0 ? ' ' + stageTrackerAgoText(at, nowMs) : '');
      const nextWords = nextAt > nowMs ? 'next sweep in ' + stageTrackerShortLeft(nextAt - nowMs) : (nextAt > 0 ? 'sweep due now' : '');
      const details = why ? ' Details: ' + why : '';
      if (kind === 'checks-pending') {
        const tip = 'hive tried to merge this' + when + ' before its required checks had reported — ' + (text || "required checks haven't finished") + '. It merges once they pass' + (mg.sweep_running ? "; hive's merge sweep is on it now" : (nextWords ? ' (' + nextWords + ')' : '')) + '.' + lastSeen + details;
        return { state, word: 'Waiting for CI', detail: checks > 0 ? checks + ' check' + (checks === 1 ? '' : 's') : '', tip, cue: mg.sweep_running ? 'running' : 'waiting', until: 0 };
      }
      const word = STAGE_TRACKER_MERGE_GATE_WORD[state];
      const sentence = 'hive tried to merge this' + when + ' and GitHub refused' + (text ? ' — ' + text : '') + '.';
      if (mg.sweep_running) {
        return { state, word, detail: 'retrying', tip: sentence + " hive's merge sweep is retrying it now." + lastSeen + details, cue: 'running', until: 0 };
      }
      return { state, word, detail: STAGE_TRACKER_MERGE_REFUSAL_DETAIL[kind] || 'GitHub refused', tip: sentence + ' The lane re-evaluates it on its next pass' + (nextWords ? ' (' + nextWords + ')' : '') + '.' + lastSeen + details, cue: 'waiting', until: 0 };
    }
    // Pure: a refusal's first line, cut to STAGE_TRACKER_MERGE_FAILED_SHORT_MAX
    // (tooltips and toasts; the pill detail comes from the refusal kind).
    function stageTrackerMergeFailedShort(why) {
      const line = String(why || '').split('\n')[0].replace(/^merging PR \S+ \([^)]*\): /, '').trim();
      if (!line) return 'GitHub refused the merge';
      return line.length > STAGE_TRACKER_MERGE_FAILED_SHORT_MAX ? line.slice(0, STAGE_TRACKER_MERGE_FAILED_SHORT_MAX - 1).trimEnd() + '…' : line;
    }
    // Pure: the pill word with its live detail — "Ready for auto merge ·
    // next sweep in 42s". The detail span carries data-ham-until so the
    // countdown ticks in place.
    function stageTrackerMergeGateWordHtml(item, nowMs) {
      const info = stageTrackerMergeGateInfo(item, nowMs);
      if (!info) return '';
      // A soak detail carries its clock so the tick can redo both the
      // words and the hover sentence (data-ham-soak-tip is the sentence
      // currently written into the row's titles; the tick swaps it out).
      const c = info.soakClock;
      const soakAttrs = c ? ` data-ham-soak="1" data-ham-until="${c.until}" data-ham-until-kind="soaking" data-ham-soak-window="${c.windowS}" data-ham-soak-head="${esc(c.head)}" data-ham-soak-restarted="${c.restarted ? 1 : 0}" data-ham-soak-prefix="${esc(c.prefix || '')}" data-ham-next="${c.nextAt || 0}" data-ham-sweeping="${c.sweepRunning ? 1 : 0}" data-ham-soak-tip="${esc(info.soak)}"` : '';
      const detail = info.detail ? ` <span class="ham-gate-detail"${soakAttrs || (info.until ? ` data-ham-until="${info.until}" data-ham-until-kind="${esc(info.state)}"` : '')}>· ${esc(info.detail)}</span>` : '';
      return `<span class="ham-seg-t${info.stalled ? ' ham-queue-stalled' : ''}">${esc(info.word)}${detail}</span>`;
    }
    /* ---- Soak clock ---------------------------------------------------------
       The payload's `soak` {started_at, until, window_s, head_sha} is the
       lane's clock for the head: it must sit unchanged for window_s before
       the hive merges it. One clock serves 095 (admitted, soaking) and a
       trust-blocked head at 060/080 that sits through the same window.
       Older payloads fall back to merge_gate.soak_until /
       soak_remaining_seconds. Static text only — no spinner. */
    // Pure: the clock for a row, or null when it is not soaking.
    //   until / started / windowS / head — from the payload
    //   restarted — the clock's head is not the row's head: a push
    //               restarted the soak and the lane has not re-read it
    //   fromSoak  — the clock came from the `soak` payload (so it shows
    //               at 060/080 too), not the legacy 095-only fields
    function stageTrackerSoakClock(src, item, nowMs) {
      const soak = (src && src.soak) || (item && item.soak) || null;
      const mg = (src && src.merge_gate) || {};
      let until = 0, windowS = 0, head = '', started = 0, fromSoak = false;
      if (soak && soak.until) {
        until = Date.parse(soak.until); windowS = Number(soak.window_s) || 0; head = String(soak.head_sha || ''); started = soak.started_at ? Date.parse(soak.started_at) : 0; fromSoak = true;
      } else {
        const soakSec = Number(mg.soak_remaining_seconds) || Number(src && src.soak_remaining_seconds) || Number(item && item.soak_remaining_seconds) || 0;
        until = mg.soak_until ? Date.parse(mg.soak_until) : (soakSec > 0 ? nowMs + soakSec * 1000 : 0);
      }
      if (!(until > 0)) return null;
      const rowHead = String((src && src.head) || (item && item.head) || '');
      return { until, started, windowS, head, restarted: !!(head && rowHead && head !== rowHead), fromSoak, nextAt: mg.next_sweep_at ? Date.parse(mg.next_sweep_at) : 0, sweepRunning: !!mg.sweep_running };
    }
    // Pure: "12m 30s" / "45s" / "1h 05m" for the hover; '' when none.
    function stageTrackerSoakLeftText(ms) {
      if (!(ms > 0)) return '';
      const s = Math.ceil(ms / 1000);
      if (s < 60) return s + 's';
      const m = Math.floor(s / 60), r = s % 60;
      if (m < 60) return m + 'm' + (r ? ' ' + r + 's' : '');
      return Math.floor(m / 60) + 'h ' + String(m % 60).padStart(2, '0') + 'm';
    }
    // Pure: "8:42 PM" for the soak's end, in the dashboard's time zone.
    function stageTrackerClockText(ms) {
      const opts = { hour: 'numeric', minute: '2-digit' };
      if (typeof formatDashboardTime === 'function') return formatDashboardTime(ms, opts);
      try { return new Intl.DateTimeFormat(undefined, opts).format(new Date(ms)); } catch (e) { return new Date(ms).toISOString(); }
    }
    // Pure: { word, detail, tip } for a soak clock at nowMs.
    //   soaking   — "Soaking · 12m left"; the hover carries the rule, the
    //               live time left and the end time
    //   ready     — past until: "Soaking · ready — next sweep in 3m"
    //   restarted — "Soak restarted · new head"
    function stageTrackerSoakWords(clock, nowMs, nextAt, sweepRunning) {
      const short = clock.head ? clock.head.slice(0, 7) : '';
      const who = short ? 'Head ' + short : 'The head';
      const mins = clock.windowS > 0 ? Math.round(clock.windowS / 60) : 0;
      const window = mins > 0 ? mins + ' minute' + (mins === 1 ? '' : 's') : 'the soak window';
      if (clock.restarted) {
        return { word: 'Soak restarted', detail: 'new head', tip: who + ' was replaced by a new push, so the soak starts over on the new head: it must sit ' + window + ' without new commits before the hive merges it. hive re-reads it on its next sweep.' };
      }
      const prefix = clock.prefix || '';
      const leftMs = clock.until - nowMs;
      if (leftMs > 0) {
        return { word: 'Soaking', detail: prefix + stageTrackerShortLeft(leftMs) + ' left', tip: who + ' must sit ' + window + ' without new commits before the hive merges it · ' + stageTrackerSoakLeftText(leftMs) + ' left · soak ends ' + stageTrackerClockText(clock.until) };
      }
      const next = sweepRunning ? 'merge sweep running' : (nextAt > nowMs ? 'next sweep in ' + stageTrackerShortLeft(nextAt - nowMs) : 'sweep due now');
      return { word: 'Soaking', detail: (prefix ? 'soaked — ' : 'ready — ') + next, tip: who + ' sat ' + window + ' without new commits — soaked since ' + stageTrackerClockText(clock.until) + '; hive merges it on its next sweep (' + next + ').' };
    }
    // Pure: the leading glyph for a row on the gate — spinning while hive's
    // sweep is on it, a clock while it waits — with the sentence as title
    // and aria-label. '' off the gate.
    function stageTrackerGateCueHtml(item, nowMs) {
      const info = stageTrackerMergeGateInfo(item, nowMs);
      if (!info) return '';
      const spin = info.cue === 'running' || info.cue === 'merging';
      return `<span class="ham-gate-ico${spin ? ' cue-spin' : ''}" data-gate-cue="${esc(info.cue)}" role="img" title="${esc(info.tip)}" aria-label="${esc(info.tip)}">${stageTrackerIcon(spin ? 'sweep' : 'wait', 'ham-ico')}</span>`;
    }
    // Pure: the queue chip's countdown words — "~4m" until the estimate,
    // "any sweep now" once it has passed.
    function stageTrackerTrainEtaDetail(etaAt, nowMs) {
      const left = stageTrackerShortLeft(etaAt - nowMs);
      return left ? '~' + left : 'any sweep now';
    }
    // Pure: ticks every countdown in el from nowMs; "sweep due now" once a
    // ready row's next pass has come.
    function stageTrackerGateCountdownSync(el, nowMs) {
      if (!el || typeof el.querySelectorAll !== 'function') return;
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      for (const n of Array.from(el.querySelectorAll('.ham-gate-detail[data-ham-until]'))) {
        const until = Number(n.getAttribute('data-ham-until'));
        const kind = n.getAttribute('data-ham-until-kind');
        if (n.getAttribute('data-ham-soak') === '1') { stageTrackerSoakTick(n, nowMs); continue; }
        const left = stageTrackerShortLeft(until - nowMs);
        let t = kind === 'soaking' ? (left ? '· ' + left + ' left' : '· soak window complete') : (left ? '· next sweep in ' + left : '· sweep due now');
        if (kind === 'decided') t = n.textContent.replace(/ · in [^·]*$|· in [^·]*$/, '') + (left ? ' · in ' + left : ' · due now');
        if (kind === STAGE_TRACKER_TRAIN_STATE_QUEUED || kind === STAGE_TRACKER_TRAIN_STATE_FRONT) t = '· ' + stageTrackerTrainEtaDetail(until, nowMs);
        if (n.textContent !== t) n.textContent = t;
      }
    }
    // Pure over the node: redo a soak detail's words and swap the live
    // hover sentence into the row's titles (the chevron and its glyph), so
    // the time left reads right at hover time without a re-render.
    function stageTrackerSoakTick(n, nowMs) {
      const clock = { until: Number(n.getAttribute('data-ham-until')), windowS: Number(n.getAttribute('data-ham-soak-window')) || 0, head: n.getAttribute('data-ham-soak-head') || '', restarted: n.getAttribute('data-ham-soak-restarted') === '1', prefix: n.getAttribute('data-ham-soak-prefix') || '' };
      const words = stageTrackerSoakWords(clock, nowMs, Number(n.getAttribute('data-ham-next')) || 0, n.getAttribute('data-ham-sweeping') === '1');
      const t = '· ' + words.detail;
      if (n.textContent !== t) n.textContent = t;
      const prev = n.getAttribute('data-ham-soak-tip') || '';
      if (!prev || prev === words.tip || typeof n.closest !== 'function') return;
      const li = n.closest('li');
      const targets = li ? [li].concat(Array.from(li.querySelectorAll('[title]'))) : [];
      for (const t2 of targets) {
        for (const attr of ['title', 'aria-label']) {
          const v = t2.getAttribute(attr);
          if (v && v.indexOf(prev) >= 0) t2.setAttribute(attr, v.split(prev).join(words.tip));
        }
      }
      n.setAttribute('data-ham-soak-tip', words.tip);
    }

    function stageTrackerHoldLabels(item) {
      const labels = Array.isArray(item.labels) ? item.labels : [];
      return labels.filter(label => {
        const l = String(label || '').trim().toLowerCase();
        return l === 'do-not-merge' || l.startsWith('do-not-merge/') || l.startsWith('hive-pause/') ||
          l.indexOf('hold') >= 0 || l.indexOf('on-hold') >= 0 || l.indexOf('hold/review') >= 0;
      });
    }

    // The Open · blocked chevron's reason: the blocker when the payload names
    // one (`blocked_on`: "owner/repo#N" or a list of them, or `blocked_on_pr`:
    // the PR a hive:blocked-on marker names), else the label.
    function stageTrackerBlockedTip(item) {
      const on = item && item.blocked_on;
      const refs = (Array.isArray(on) ? on : (on ? [on] : [])).map(r => String(r || '').trim()).filter(Boolean);
      const pr = stageTrackerBlockedOnPR(item);
      if (pr && refs.indexOf(pr.ref) < 0) refs.push(pr.ref);
      return refs.length ? 'blocked on ' + refs.join(', ') : 'carries the blocked label';
    }

    // The PR a blocked issue waits on (`blocked_on_pr`: {repo, number, url,
    // state}), normalised: `ref` is "owner/repo#N", `short` is "#N" when the
    // PR lives in the item's own repo, `tip` the full explanation. null when
    // the payload names none.
    function stageTrackerBlockedOnPR(item) {
      const pr = item && item.blocked_on_pr;
      const number = pr ? Number(pr.number) || 0 : 0;
      if (!number) return null;
      const repo = String(pr.repo || '').trim();
      const ref = (repo ? repo + '#' : '#') + number;
      const own = String(item.repo || '').trim().toLowerCase();
      const lower = repo.toLowerCase();
      const same = !repo || !own || lower === own || own.endsWith('/' + lower) || lower.endsWith('/' + own);
      const state = String(pr.state || '').trim().toLowerCase();
      const outcome = state === 'merged' ? STAGE_TRACKER_BLOCKED_ON_PR_MERGED : state === 'closed' ? STAGE_TRACKER_BLOCKED_ON_PR_CLOSED : STAGE_TRACKER_BLOCKED_ON_PR_OPEN;
      return { ref, short: same ? '#' + number : ref, url: String(pr.url || '').trim(), state,
        tip: 'Blocked on PR ' + ref + (state ? ' (' + state + ')' : '') + ' — ' + outcome };
    }
    const STAGE_TRACKER_BLOCKED_ON_PR_OPEN = 'lifts when it merges; re-triage if closed unmerged';
    const STAGE_TRACKER_BLOCKED_ON_PR_MERGED = 'merged; the blocked sweep lifts the label next';
    const STAGE_TRACKER_BLOCKED_ON_PR_CLOSED = 'closed unmerged; the blocked sweep re-triages it next';

    // The 072 adoption line (`adoption`: {due, blocked_by, since, nudges,
    // author_online, action, text} — hivecommons/hive#14524): the payload's
    // plain-words text, '' when the row carries none.
    function stageTrackerAdoptionTip(item) {
      const a = item && item.adoption;
      return a && a.text ? String(a.text) : '';
    }
    // Whether the row offers the owner's Adopt: the engine found adoption
    // due and held it for a reason the owner settles (maintainer_policy:
    // ask names `action: "decide:adopt"`).
    function stageTrackerAdoptOffered(item) {
      const a = item && item.kind === 'pr' ? item.adoption : null;
      return !!(a && a.due && a.action === 'decide:adopt');
    }
    // The chip text: "adopt?" when the owner is asked, else "waiting on @x".
    function stageTrackerAdoptionChipText(item) {
      const a = item && item.adoption;
      if (!a) return '';
      if (stageTrackerAdoptOffered(item)) return 'adopt?';
      if (a.blocked_by) return 'not adopted';
      // Guarded: the test harnesses load helpers by name.
      const text = 'waiting on ' + (a.author ? '@' + a.author : 'author');
      return typeof stageTrackerReworkSuffix === 'function' ? stageTrackerReworkSuffix(text, item) : text;
    }
    function stageTrackerAdoptionChipHtml(item) {
      const tip = stageTrackerAdoptionTip(item);
      if (!tip) return '';
      const offered = stageTrackerAdoptOffered(item);
      const cls = 'ham-badge ham-badge-adoption' + (offered ? ' ham-badge-adoption-ask' : '');
      // The presence indicator rides inside the pill, before the @login
      // (hivecommons/hive#15051); its line joins the tooltip.
      const ptip = stageTrackerPresenceTip(item, Date.now());
      const inner = stageTrackerPresenceInPillHtml(item, stageTrackerAdoptionChipText(item));
      const joined = ptip ? tip + ' · ' + ptip : tip;
      const full = typeof stageTrackerReworkJoinTip === 'function' ? stageTrackerReworkJoinTip(joined, item, Date.now()) : joined;
      // "adopt?" opens the Needs-human popover (its decision_needed carries
      // the Adopt option) when the popover helpers are present.
      const decision = offered && typeof stageTrackerDecisionOf === 'function' && item.decision_needed ? stageTrackerDecisionOf(item) : null;
      if (decision) return stageTrackerDecisionTriggerHtml(decision, inner, full, cls);
      return `<span class="${cls}" title="${esc(full)}" aria-label="${esc('Adoption — ' + full)}">${inner}</span>`;
    }

    // Presence on a waiting-on row (`presence`: {login, state, kind, since,
    // last_seen, next, adopt_at} — hivecommons/hive#15051): whether the
    // person's clanker (their connected contributor agent) is online,
    // offline or not registered here, and the hive's likely next move.
    const STAGE_TRACKER_PRESENCE_STATES = ['online', 'offline', 'unknown'];
    const STAGE_TRACKER_PRESENCE_WORDS = { online: 'clanker online', offline: 'clanker offline', unknown: 'no clanker registered' };
    function stageTrackerPresenceOf(item) {
      const p = item && item.presence;
      if (!p || typeof p !== 'object' || !p.login) return null;
      const state = STAGE_TRACKER_PRESENCE_STATES.indexOf(p.state) >= 0 ? p.state : 'unknown';
      return { login: String(p.login), state, kind: p.kind === 'clanker' ? 'clanker' : 'human', since: p.since || '', last_seen: p.last_seen || '', next: p.next === 'adopt' ? 'adopt' : 'wait', adopt_at: p.adopt_at || '' };
    }
    // "26h" / "40m" / "3d": how long the clanker has been gone, counted from
    // its last heartbeat or the start of the wait, whichever is later.
    function stageTrackerPresenceGone(p, nowMs) {
      const from = Math.max(Date.parse(p.last_seen || '') || 0, Date.parse(p.since || '') || 0);
      if (!from || nowMs <= from) return '';
      const min = Math.floor((nowMs - from) / 60000);
      if (min < 60) return Math.max(1, min) + 'm';
      const h = Math.floor(min / 60);
      return h < 48 ? h + 'h' : Math.floor(h / 24) + 'd';
    }
    // "at 15:10 (in 38m)" / "due now".
    function stageTrackerPresenceAdoptAt(p, nowMs) {
      const at = Date.parse(p.adopt_at || '');
      if (!at) return '';
      if (at <= nowMs) return 'due now';
      const min = Math.ceil((at - nowMs) / 60000);
      const inWords = min < 60 ? 'in ' + min + 'm' : min < 48 * 60 ? 'in ' + Math.floor(min / 60) + 'h' : 'in ' + Math.floor(min / 1440) + 'd';
      return 'at ' + agentNowClockTime(at, nowMs) + ' (' + inWords + ')';
    }
    // The plain-words tooltip:
    //   Waiting on @Danathar's clanker (online, last seen 2 min ago) — hive is waiting for them
    //   Waiting on @hanthor's clanker (offline 26h) — hive will adopt at 15:10 (in 38m)
    //   Waiting on @zoe (human) — waiting for a human reply
    function stageTrackerPresenceTip(item, nowMs) {
      const p = stageTrackerPresenceOf(item);
      if (!p) return '';
      const now = Number(nowMs) || Date.now();
      const hive = "Waiting on @" + p.login + "'s clanker (";
      const adopt = stageTrackerPresenceAdoptAt(p, now);
      let head, next;
      if (p.state === 'online') {
        const seenMs = Date.parse(p.last_seen || '');
        head = hive + 'online' + (seenMs ? ', last seen ' + agentNowAgo(now - seenMs) : '') + ')';
        next = 'hive is waiting for them' + (adopt ? '; adopts ' + adopt + ' if nothing moves' : '');
      } else if (p.state === 'offline') {
        const gone = stageTrackerPresenceGone(p, now);
        head = hive + 'offline' + (gone ? ' ' + gone : '') + ')';
        next = p.next === 'adopt' && adopt ? 'hive will adopt ' + adopt : 'hive is waiting for them';
      } else if (p.kind === 'human') {
        head = 'Waiting on @' + p.login + ' (human)';
        next = 'waiting for a human reply' + (adopt ? '; adoption ' + adopt : '');
      } else {
        head = hive + 'not registered here)';
        next = p.next === 'adopt' && adopt ? 'hive will adopt ' + adopt : 'hive is waiting for them';
      }
      return head + ' — ' + next;
    }
    // The indicator's own aria-label: the state in words, then the next move.
    function stageTrackerPresenceAria(item, nowMs) {
      const p = stageTrackerPresenceOf(item);
      if (!p) return '';
      const word = p.kind === 'human' ? 'human' : STAGE_TRACKER_PRESENCE_WORDS[p.state];
      return '@' + p.login + ': ' + word + ' — ' + stageTrackerPresenceTip(item, nowMs).split(' — ').slice(1).join(' — ');
    }
    // The indicator: the chevrons' person glyph for a human (no dot — a
    // human has no presence to report), the clanker's robot head with the
    // online / offline / unknown dot beside it for a contributor's hive.
    function stageTrackerPresenceDotHtml(item, nowMs) {
      const p = stageTrackerPresenceOf(item);
      if (!p) return '';
      const ico = stageTrackerIcon(p.kind, 'ham-ico ham-presence-ico', '');
      const dot = p.kind === 'human' ? '' : '<i class="ham-presence-dot"></i>';
      return `<span class="ham-presence ham-presence-${p.state} ham-presence-${p.kind}" role="img" aria-label="${esc(stageTrackerPresenceAria(item, nowMs))}" data-ham-presence="${p.state}" data-ham-presence-kind="${p.kind}">${ico}${dot}</span>`;
    }
    // Pill text with the indicator placed just before the @login (or
    // leading, when the text names nobody): "waiting on [robot ●] @hanthor".
    function stageTrackerPresenceInPillHtml(item, text) {
      const dot = stageTrackerPresenceDotHtml(item, Date.now());
      const t = String(text || '');
      if (!dot) return esc(t);
      const i = t.indexOf('@');
      if (i < 0) return dot + esc(t);
      return esc(t.slice(0, i)) + dot + esc(t.slice(i));
    }
    // The waiting-on pill for a row with presence but no adoption line (071
    // Needs author, 072 before the first nudge, a human's PR): "waiting on
    // [person] @zoe" — the glyph says who, the title says the rest.
    function stageTrackerPresenceChipText(item) {
      const p = stageTrackerPresenceOf(item);
      if (!p) return '';
      const text = 'waiting on @' + p.login;
      // Guarded: the test harnesses load helpers by name.
      return typeof stageTrackerReworkSuffix === 'function' ? stageTrackerReworkSuffix(text, item) : text;
    }
    function stageTrackerPresenceChipHtml(item) {
      const p = stageTrackerPresenceOf(item);
      if (!p || stageTrackerAdoptionTip(item)) return '';
      const ptip = stageTrackerPresenceTip(item, Date.now());
      const tip = typeof stageTrackerReworkJoinTip === 'function' ? stageTrackerReworkJoinTip(ptip, item, Date.now()) : ptip;
      return `<span class="ham-badge ham-badge-waiting" title="${esc(tip)}" aria-label="${esc(tip)}">${stageTrackerPresenceInPillHtml(item, stageTrackerPresenceChipText(item))}</span>`;
    }

    // Rework pickup on a returned PR (`rework`: {state, actor, requested_at,
    // since, evidence, mode, ahead, head} — hivecommons/hive#15280). The
    // waiting-on pill says who, this says whether they have picked it up:
    //   requested    — the request went out; nothing has moved on the PR
    //                  since (evidence "queued": the fix sits in the clanker's
    //                  hub queue, `ahead` fixes before it).
    //   acknowledged — the PR saw activity after the request (a comment or
    //                  a push the sweep has not routed yet).
    //   in_progress  — the clanker's relay took the fix task (claim).
    //   pushed       — the head moved since the request; 050 on the next sweep.
    const STAGE_TRACKER_REWORK_WORDS = { requested: 'no response', acknowledged: 'acknowledged', in_progress: 'working', pushed: 're-review pending' };
    function stageTrackerReworkOf(item) {
      const r = item && item.rework;
      if (!r || !STAGE_TRACKER_REWORK_WORDS[r.state]) return null;
      return r;
    }
    function stageTrackerReworkWord(item) {
      const r = stageTrackerReworkOf(item);
      if (!r) return '';
      if (r.state === 'requested' && r.evidence === 'queued') return 'queued';
      return STAGE_TRACKER_REWORK_WORDS[r.state];
    }
    // "waiting on @x · acknowledged" — the pill text with the pickup word.
    function stageTrackerReworkSuffix(text, item) {
      const w = stageTrackerReworkWord(item);
      return w ? text + ' · ' + w : text;
    }
    function stageTrackerReworkTip(item, nowMs) {
      const r = stageTrackerReworkOf(item);
      if (!r) return '';
      const now = Number(nowMs) || Date.now();
      const who = r.actor ? '@' + r.actor : 'the author';
      const when = (iso) => {
        const ms = Date.parse(iso || '');
        return Number.isFinite(ms) ? agentNowAgo(now - ms) + ' (' + agentNowClockTime(ms, now) + ')' : '';
      };
      const req = when(r.requested_at);
      const since = when(r.since);
      switch (r.state) {
        case 'pushed':
          return who + ' pushed a new head' + (since ? ' ' + since : '') + ' · re-review pending';
        case 'in_progress':
          return who + "'s clanker picked up the rework" + (since ? ' ' + since : '') + (r.mode ? ' (' + r.mode + ')' : '');
        case 'acknowledged':
          return who + ' acknowledged · activity on the PR' + (since ? ' ' + since : '');
        default:
          if (r.evidence === 'queued') return 'rework queued for ' + who + "'s clanker" + (r.ahead ? ' · ' + r.ahead + ' ahead' : '') + (since ? ' · queued ' + since : '');
          return 'rework requested' + (req ? ' ' + req : '') + ' · no response from ' + who + ' yet';
      }
    }
    function stageTrackerReworkJoinTip(tip, item, nowMs) {
      const rt = stageTrackerReworkTip(item, nowMs);
      if (!rt) return tip || '';
      return tip ? tip + ' · ' + rt : rt;
    }
    /* Takeover (hivecommons/hive#15038): a PR waiting on a contributor's
       clanker (072 returned-to-contributor / 071 needs-author) carries
       `takeover` {author, author_online, adopt_now, adopt_now_reason,
       reroute, reroute_reason, clankers[], rerouted, grace_elapsed}. The
       row menu then offers Adopt now (forced adoption, no grace wait) and
       Reroute to another clanker (an online registered contributor hive,
       or "any available"); the server disables each with a reason. */
    const STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE = "closes the contributor's PR after hive opens its copy; their commits + sign-off are preserved";
    const STAGE_TRACKER_REROUTE_ANY = 'any';
    const STAGE_TRACKER_REROUTE_PREFIX = 'decide:reroute:';
    function stageTrackerTakeoverOf(item) {
      return item && item.kind === 'pr' && item.takeover && typeof item.takeover === 'object' ? item.takeover : null;
    }
    // Pure: the base action id — "decide:reroute:<login>" acts as "decide:reroute".
    function stageTrackerActionBase(id) {
      return String(id || '').indexOf(STAGE_TRACKER_REROUTE_PREFIX) === 0 ? 'decide:reroute' : String(id || '');
    }
    function stageTrackerRerouteTargetOf(id) {
      return String(id || '').indexOf(STAGE_TRACKER_REROUTE_PREFIX) === 0 ? String(id).slice(STAGE_TRACKER_REROUTE_PREFIX.length) : '';
    }
    // Pure: one submenu line per online clanker — "@login · hive · online 2h · 1 in hand".
    function stageTrackerClankerLabel(c, nowMs) {
      const since = Date.parse(c.online_since || '');
      const parts = ['@' + (c.login || '?')];
      if (c.hive_name) parts.push(String(c.hive_name));
      if (since && typeof agentNowAgo === 'function') parts.push('online ' + agentNowAgo((typeof nowMs === 'number' ? nowMs : Date.now()) - since));
      const load = Number(c.load) || 0;
      parts.push(load ? load + ' in hand' : 'idle');
      return parts.join(' · ');
    }
    // Pure: the Reroute submenu — the clankers online now, then "any available".
    function stageTrackerRerouteOptions(tk, enabled, why, nowMs) {
      const out = (tk && Array.isArray(tk.clankers) ? tk.clankers : []).map(c => ({ id: STAGE_TRACKER_REROUTE_PREFIX + (c.login || ''), label: stageTrackerClankerLabel(c, nowMs), enabled, why, clanker: c }));
      out.push({ id: STAGE_TRACKER_REROUTE_PREFIX + STAGE_TRACKER_REROUTE_ANY, label: 'any available' + (out.length ? ' (' + out.length + ' online)' : ''), enabled, why });
      return out;
    }
    // Pure: the takeover items of a row menu; [] when the row has none.
    function stageTrackerTakeoverActions(item, ctx, nowMs) {
      const tk = stageTrackerTakeoverOf(item);
      if (!tk) return [];
      ctx = ctx || {};
      const write = ctx.canHold !== false && !!ctx.user;
      const whyWrite = write ? '' : STAGE_TRACKER_WHY_WRITE;
      const adoptOk = write && !!tk.adopt_now;
      const adoptWhy = adoptOk ? '' : (whyWrite || tk.adopt_now_reason || 'Not available on this row');
      const rerouteOk = write && !!tk.reroute;
      const rerouteWhy = rerouteOk ? '' : (whyWrite || tk.reroute_reason || 'Not available on this row');
      return [
        { id: 'decide:adopt-now', label: STAGE_TRACKER_ACTION_LABELS['decide:adopt-now'] + '…', note: STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE, enabled: adoptOk, why: adoptWhy, danger: true },
        { id: 'decide:reroute', label: STAGE_TRACKER_ACTION_LABELS['decide:reroute'], note: tk.rerouted && tk.rerouted.to ? 'rerouted → @' + tk.rerouted.to + ' already; pick again to move it' : (rerouteOk ? 'hands the reviewer\'s requested changes to another online clanker' : ''), enabled: rerouteOk, why: rerouteWhy, submenu: stageTrackerRerouteOptions(tk, rerouteOk, rerouteWhy, nowMs) },
      ];
    }
    // Pure: the "rerouted → @x" pill with the target's presence dot; '' when
    // the row is not rerouted.
    function stageTrackerReroutePillHtml(item) {
      const tk = stageTrackerTakeoverOf(item);
      const r = tk && tk.rerouted && tk.rerouted.to ? tk.rerouted : null;
      if (!r) return '';
      const tip = r.text || ('Rerouted to @' + r.to + (r.by ? ' by @' + r.by : ''));
      const presence = r.target_online ? 'online' : 'offline';
      return `<span class="ham-badge ham-badge-reroute${r.target_online ? ' ham-reroute-online' : ''}" title="${esc(tip)}" aria-label="${esc('Rerouted — ' + tip + ' (@' + r.to + ' ' + presence + ')')}"><span class="ham-presence-dot" aria-hidden="true"></span>rerouted → @${esc(r.to)}</span>`;
    }

    // "Sentinel flagged security-relevant changes at <head short sha>:
    // <reason>" — the sentinel wording for an item's tooltips and aria
    // labels; '' when the item carries no alert. Without a reason or a
    // head the generic sentinel description stands in.
    function stageTrackerSentinelTip(item) {
      if (!item || !item.sentinel_alert) return '';
      const head = String(item.head || '').slice(0, STAGE_TRACKER_SHORT_SHA);
      const reason = String(item.sentinel_reason || '').trim();
      return STAGE_TRACKER_SENTINEL_TIP + (head ? ' at ' + head : '') + ': ' + (reason || STAGE_TRACKER_SENTINEL_TIP_FALLBACK);
    }
    // Whether the item's lit chevron word carries the alert itself (a PR
    // parked at 085, or an issue riding one): the chip then adds nothing.
    function stageTrackerSentinelInLane(item) {
      return !!(item && item.sentinel_alert) && (Number(item.stage) || 0) === STAGE_TRACKER_NEEDS_DECISION && (item.kind === 'pr' || !!item.via_pr);
    }
    // A returned PR (071/072) keeps its routing word — the human already
    // acted, nothing is owed by the maintainer — and carries the alert as
    // a " · sentinel" suffix instead (hivecommons/hive#15159); the chip
    // then adds nothing.
    function stageTrackerSentinelAsSuffix(item) {
      const st = Number(item && item.stage) || 0;
      return !!(item && item.sentinel_alert) && (st === 71 || st === 72) && (item.kind === 'pr' || !!item.via_pr);
    }
    // Pure: whose PR a 071/072 row waits on — the row's own author, or, for
    // an issue row drawn through its one covering PR (via_pr), that PR's
    // author. The issue's author is whoever filed it (on hive's hive the
    // reviewer's bot, hivecommons-hive[bot]); naming them on the chevron
    // read "Returned to @hivecommons-hive[bot]'s hive" for a clanker's PR.
    function stageTrackerReturnedAuthor(item) {
      let src = item || {};
      if (src.via_pr && Array.isArray(src.linked_prs)) {
        const here = src.linked_prs.filter(lp => lp && lp.author && (!Number(lp.drawn_under) || Number(lp.drawn_under) === (Number(src.number) || 0)));
        if (here.length === 1) src = here[0];
      }
      return { author: String(src.author || '').replace(/^@/, ''), kind: String(src.author_kind || '') };
    }
    // "'s clanker" for a clanker's PR (the person's relay agent picks it
    // up), "'s hive" for another hive's, nothing for a person's own.
    function stageTrackerToolingSuffix(kind) {
      if (kind === 'clanker') return "'s clanker";
      if (kind === 'hive') return "'s hive";
      return '';
    }
    // The 071/072 chevron word names the routing: "Needs @author" at 071,
    // "Returned to @author's clanker" at 072 for a clanker-authored PR (the
    // contributor's agent picks it up), "Returned to @author" for a human's.
    // A row already superseded by a merged PR says that instead — nothing
    // is owed by the author; a maintainer closes it (hivecommons/hive#15303
    // sat as "Returned to …" while its issue was closed by #15304).
    function stageTrackerReturnedWord(item, fallback) {
      const st = Number(item && item.stage) || 0;
      const sup = item && item.superseded && typeof item.superseded === 'object' ? Number(item.superseded.closer_pr) || 0 : 0;
      const who = stageTrackerReturnedAuthor(item);
      let text = fallback;
      if (sup) text = 'Superseded by #' + sup + ' · decide';
      else if (who.author && st === 71) text = 'Needs @' + who.author;
      else if (who.author && st === 72) text = 'Returned to @' + who.author + stageTrackerToolingSuffix(who.kind);
      return text + (stageTrackerSentinelAsSuffix(item) ? ' · ' + STAGE_TRACKER_SENTINEL_CHIP : '');
    }

