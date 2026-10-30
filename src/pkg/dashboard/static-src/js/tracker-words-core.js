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
    //                              merge pass: "next pass in 42s"
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
    // Pure: why a PR at stage, with reason, is somebody's to repair right
    // now — '' when nothing is. Mirrors stageTrackerTrainUnderRepair in
    // Go (the rule that seats a PR in, or drops it from, its merge train)
    // so the chevron column and train membership read one predicate: a
    // PR being fixed (070), waiting on its author (071) or contributor
    // (072), freshly adopted (074), parked on a decision (085), or whose
    // required checks are red rides no train; all but the decision park
    // light CI / Review whatever the gate or the planner last recorded.
    const STAGE_TRACKER_REPAIR_STAGE_WHY = { 70: 'fixing', 71: 'needs-author', 72: 'returned-to-contributor', 74: 'adopted-by-hive', 85: 'needs-decision' };
    function stageTrackerUnderRepair(stage, reason) {
      const why = STAGE_TRACKER_REPAIR_STAGE_WHY[Number(stage) || 0];
      if (why) return why;
      const lower = String(reason || '').trim().toLowerCase();
      if (lower.indexOf('required-check-failing:') === 0 || lower.indexOf('check-failing:') === 0 || lower === 'ci-red') return 'checks-failing';
      return '';
    }
    // The first merge-ready stage (080 awaiting merge admission). The
    // server seats no train member below it (a PR short of the queue
    // carries an `overlap` hint instead); the provisional words below are
    // the fallback for a payload that still does, so the chevron never
    // contradicts the caption.
    const STAGE_TRACKER_MERGE_READY = 80;
    // The blocker headline for a provisional train member, by stage —
    // the same words the stage column shows (stageTrackerShort in Go), so
    // the chevron and the caption name one blocker (hive's own #15577 read
    // "Merge queued · 1st" while its caption said "check build-and-test
    // still running"). Keys below STAGE_TRACKER_MERGE_READY only.
    const STAGE_TRACKER_PROVISIONAL_STAGE_WORD = { 10: 'Actionable', 20: 'Claimed', 30: 'PR in progress', 40: 'Covered by PR', 45: 'Needs human decision', 50: 'CI / Review', 60: 'Review passed', 70: 'Fixing', 71: 'Needs author', 72: 'Returned to contributor', 74: 'Adopted by hive' };
    // Pure: true when the row's queue slot is a forecast — the server says
    // so (merge_train.provisional), or, from an older server, the row's
    // own stage is short of the merge gate.
    function stageTrackerTrainProvisional(item) {
      const mt = stageTrackerTrain(item);
      if (!mt) return false;
      if (mt.provisional === true) return true;
      if (mt.provisional === false) return false;
      const stage = Number(item.stage) || 0;
      return stage > 0 && stage < STAGE_TRACKER_MERGE_READY;
    }
    // Pure: the provisional member's headline — its stage's blocker word.
    function stageTrackerTrainProvisionalHeadline(item) {
      const stage = Number(item && item.stage) || 0;
      // Guarded: test harnesses lift this function without the active-word table.
      const active = typeof STAGE_TRACKER_ACTIVE_WORD !== 'undefined' ? STAGE_TRACKER_ACTIVE_WORD[stage] : '';
      return STAGE_TRACKER_PROVISIONAL_STAGE_WORD[stage] || active || STAGE_TRACKER_PROVISIONAL_STAGE_WORD[50];
    }
    // Pure: the trailing slot words for a provisional member — "queued 1st
    // of 2 once ready" — never the headline. trains (the count of trains
    // on the page) adds the train tag when there is more than one.
    function stageTrackerTrainProvisionalSuffix(item, trains) {
      const mt = stageTrackerTrain(item);
      if (!mt) return '';
      const pos = mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT ? 1 : (stageTrackerTrainPlace(mt).pos || 2);
      return 'queued ' + stageTrackerTrainOrdinalOfN(mt, pos) + ' once ready' + stageTrackerTrainTagSuffix(mt, trains);
    }
    // Pure: "1st of 2" — the place with the queue length, so two trains on
    // one page do not read as two "1st"s with nothing to tell them apart
    // (the owner's ask: "how are there 2 'merge queued 1st'?"). The bare
    // ordinal when the length is unknown.
    function stageTrackerTrainOrdinalOfN(mt, pos) {
      pos = Number(pos) || 0;
      const len = Math.max(stageTrackerTrainPlace(mt).len, pos);
      return stageTrackerOrdinal(pos) + (len >= 2 ? ' of ' + len : '');
    }
    // Pure: the short name that tells one train from another — the
    // basename of its first overlapping file ("main.go"), else its head
    // ("#15567"); '' with neither.
    function stageTrackerTrainTag(mt) {
      if (!mt) return '';
      const files = Array.isArray(mt.overlap_files) ? mt.overlap_files.filter(Boolean) : [];
      if (files.length) return String(files[0]).split('/').pop();
      const head = stageTrackerTrainPlace(mt).head;
      return head ? '#' + head : '';
    }
    // The chevron's train tag lead: "· train main.go".
    const STAGE_TRACKER_TRAIN_TAG_LEAD = 'train ';
    // Pure: " · train main.go" when more than one train is on the page
    // (trains is that count); '' for one train or none.
    function stageTrackerTrainTagSuffix(mt, trains) {
      if (!((Number(trains) || 0) > 1)) return '';
      const tag = stageTrackerTrainTag(mt);
      return tag ? ' · ' + STAGE_TRACKER_TRAIN_TAG_LEAD + tag : '';
    }
    // Pure: the hover sentence naming the train — "Train of 2 PRs
    // overlapping main.go; merges after #15567." ("merges first" at the
    // front); '' when the tag is not shown.
    function stageTrackerTrainTagTip(mt, trains) {
      if (!mt || !stageTrackerTrainTagSuffix(mt, trains)) return '';
      const p = stageTrackerTrainPlace(mt);
      const files = Array.isArray(mt.overlap_files) ? mt.overlap_files.filter(Boolean) : [];
      const over = files.length ? ' overlapping ' + files.map(f => String(f).split('/').pop()).join(', ') : '';
      const isFront = mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT || p.pos === 1;
      const order = isFront ? 'merges first' : (p.head ? 'merges after #' + p.head : 'merges in turn');
      return 'Train of ' + Math.max(p.len, p.pos || 1) + ' PRs' + over + '; ' + order + '.';
    }
    // Pure: { count, index } over the page's row items — how many distinct
    // trains are drawn, and each train key's 0-based slot in order of
    // first appearance (the rail accent cycles on it). Entries may be
    // items or the grouped view's {item} / {group} wrappers.
    function stageTrackerTrainIndex(items) {
      const index = {};
      let count = 0;
      (items || []).forEach(e => {
        const it = e && e.item ? e.item : (e && e.group ? null : e);
        const k = it ? stageTrackerTrainKey(it) : '';
        if (k && index[k] === undefined) index[k] = count++;
      });
      return { count, index };
    }
    // How many rail accents cycle before repeating (.ham-train-k0..k3).
    const STAGE_TRACKER_TRAIN_ACCENTS = 4;
    // Pure: " ham-train-k2" for a train's accent slot; '' when unknown.
    function stageTrackerTrainAccentClass(idx) {
      return typeof idx === 'number' && idx >= 0 ? ' ham-train-k' + (idx % STAGE_TRACKER_TRAIN_ACCENTS) : '';
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
    const STAGE_TRACKER_TRAIN_HEAD_STALL_WORD = { 'needs-human': 'needs maintainer merge', ci: 'waiting for CI', soaking: 'soak overran', ready: 'waiting for the next merge pass', merging: 'merge call in flight', fixing: 'being fixed by its author', unknown: 'head not in this view' };
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
    // The queued word is the place with the queue length — "Merge queued
    // · 2nd of 3" — so it reads the same with the merge sweep on or off
    // (#15395); never the state slug, never "front" or "queued-behind",
    // and never '' for a state this table does not know: an unmapped slug
    // reads spelled out with the row's place ("Waiting for front · 3rd").
    // trains is the count of trains on the page: past one, the word ends
    // with the train's tag ("· train main.go") so two "1st"s tell apart.
    function stageTrackerTrainWord(item, trains) {
      const mt = stageTrackerTrain(item);
      if (!mt) return '';
      // A slot held ahead of readiness reads blocker first, slot second.
      if ((mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT || mt.state === STAGE_TRACKER_TRAIN_STATE_QUEUED) && stageTrackerTrainProvisional(item)) return stageTrackerTrainProvisionalHeadline(item) + ' · ' + stageTrackerTrainProvisionalSuffix(item, trains);
      const stalled = stageTrackerTrainStalledWord(mt);
      if (stalled && (mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT || mt.state === STAGE_TRACKER_TRAIN_STATE_QUEUED)) return stalled;
      const p = stageTrackerTrainPlace(mt);
      const tag = stageTrackerTrainTagSuffix(mt, trains);
      switch (mt.state) {
        case STAGE_TRACKER_TRAIN_STATE_FRONT: return STAGE_TRACKER_TRAIN_STATE_WORD[mt.state] + ' · ' + stageTrackerTrainOrdinalOfN(mt, 1) + tag;
        case STAGE_TRACKER_TRAIN_STATE_QUEUED: return STAGE_TRACKER_TRAIN_STATE_WORD[mt.state] + ' · ' + stageTrackerTrainOrdinalOfN(mt, p.pos || 2) + tag;
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
    // Pure: the ETA words — "ETA ~25m (2 ahead · next pass in 40s)",
    // "running late — estimated ~25m, still waiting", or '' with no
    // estimate. Always a "~": an estimate, not a promise.
    function stageTrackerTrainEtaWords(mt, nowMs) {
      const at = stageTrackerTrainEtaAt(mt, nowMs);
      if (!(at > 0)) return '';
      const left = stageTrackerShortLeft(at - nowMs);
      const basis = String(mt.eta_basis || '').trim();
      if (mt.late) return 'Running late — estimated ~' + stageTrackerShortLeft((Number(mt.eta_s) || 0) * 1000) + ', still waiting' + (basis ? ' (' + basis + ')' : '') + '.';
      return 'ETA ' + (left ? '~' + left : 'any moment now') + (basis ? ' (' + basis + ')' : '') + '.';
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
    // One sentence of merge-train mechanics for every train hover.
    const STAGE_TRACKER_TRAIN_MECHANICS = 'Each one is rebased onto the base only after the PR ahead of it merges — it cannot be rebased before that. Only green, approved PRs (merge queue, 080+) are counted; a PR still at CI / Review is not in the train.';
    // The muted hover note on a PR the server took out of its merge train
    // while it is under repair (left_train: {why, note} — fixing, needs
    // author, needs decision, red checks). The row rides no train and
    // wears no ordinal; this line is the only trace.
    const STAGE_TRACKER_LEFT_TRAIN_NOTE = 'left the merge train while being fixed; rejoins when green';
    // Pure: the left-train note for a row, '' when it rides or never rode.
    function stageTrackerLeftTrainNote(item) {
      const lt = item && item.left_train;
      if (!lt || typeof lt !== 'object' || stageTrackerTrain(item)) return '';
      return String(lt.note || STAGE_TRACKER_LEFT_TRAIN_NOTE);
    }
    /* ---- overlap hint (a PR short of the merge queue) ---------------------
       The merge train is a merge-queue concept: only PRs at 080 and
       beyond ride one. A PR still at CI / Review whose files overlap
       another open PR's carries `overlap: {behind, with, files, note}`
       instead of `merge_train` — hive's own page once drew "merge train ·
       2 PRs · 1st: #15637" under two rows that both read CI / Review. The
       row shows one muted badge, "overlaps #15637", whose hover is the
       note: "overlaps #15637 (sidebar.html, index.html) — will queue
       behind it". No rail, no ordinal, no header. */
    // Pure: the row's overlap hint, or null when it rides a train or
    // overlaps nothing.
    function stageTrackerOverlap(item) {
      const o = item && item.overlap;
      if (!o || typeof o !== 'object' || stageTrackerTrain(item)) return null;
      const withPRs = (Array.isArray(o.with) ? o.with : []).map(Number).filter(n => n > 0);
      if (!(Number(o.behind) > 0) && !withPRs.length) return null;
      return o;
    }
    // Pure: the badge word — "overlaps #15637" (the PR it queues behind,
    // else the first PR it overlaps, "+1" for the rest).
    function stageTrackerOverlapBadgeText(item) {
      const o = stageTrackerOverlap(item);
      if (!o) return '';
      const withPRs = (Array.isArray(o.with) ? o.with : []).map(Number).filter(n => n > 0);
      const lead = Number(o.behind) > 0 ? Number(o.behind) : withPRs[0];
      const rest = withPRs.filter(n => n !== lead).length;
      return 'overlaps #' + lead + (Number(o.behind) > 0 ? '' : (rest > 0 ? ' +' + rest : ''));
    }
    // Pure: the hover sentence — the server's note, else built from the
    // parts the same way.
    function stageTrackerOverlapTip(item) {
      const o = stageTrackerOverlap(item);
      if (!o) return '';
      if (o.note) return String(o.note);
      const files = (Array.isArray(o.files) ? o.files : []).filter(Boolean);
      const paren = files.length ? ' (' + files.join(', ') + ')' : '';
      if (Number(o.behind) > 0) return 'overlaps #' + Number(o.behind) + paren + ' — will queue behind it';
      const withPRs = (Array.isArray(o.with) ? o.with : []).map(Number).filter(n => n > 0);
      return 'overlaps ' + withPRs.map(n => '#' + n).join(', ') + paren + ' — merges ahead of ' + (withPRs.length > 1 ? 'them once all are ready' : 'it once both are ready');
    }
    // Pure: the muted badge beside the row's chips; '' without a hint.
    function stageTrackerOverlapBadgeHtml(item) {
      const text = stageTrackerOverlapBadgeText(item);
      if (!text) return '';
      const tip = 'Not in the merge queue yet: ' + stageTrackerOverlapTip(item);
      return `<span class="ham-badge ham-badge-overlap" title="${esc(tip)}" aria-label="${esc(tip)}">${esc(text)}</span>`;
    }
    // Tooltip lines: the queue ahead and the overlapping files — or, for a
    // PR that left its train, the one muted note — or, for a PR short of
    // the merge queue, the overlap hint.
    // header is true for the train header's own hover, which speaks for
    // the whole train rather than one member's slot.
    function stageTrackerTrainLines(item, header) {
      const mt = stageTrackerTrain(item);
      if (!mt) {
        const left = stageTrackerLeftTrainNote(item);
        if (left) return ['Merge train: ' + left];
        // Guarded: test harnesses lift this function on its own.
        const over = typeof stageTrackerOverlapTip === 'function' ? stageTrackerOverlapTip(item) : '';
        return over ? ['Not in the merge queue yet: ' + over] : [];
      }
      const ahead = Array.isArray(mt.ahead) ? mt.ahead.filter(n => Number(n) > 0) : [];
      const files = Array.isArray(mt.overlap_files) ? mt.overlap_files.filter(Boolean) : [];
      const size = Number(mt.size) || 0;
      const base = String(mt.id || '').split('#')[0] || 'the same base';
      // The mechanics the owner asked about: shared files mean a fixed
      // order, and a member can only rebase once the one ahead has merged.
      const lines = ['Merge train: ' + size + ' PRs in the merge queue to ' + base + ' touch the same files, so they merge one at a time, in order' + (mt.front ? ' (1st: #' + mt.front + ')' : '')];
      // Guarded: test harnesses lift this function without the constant.
      if (typeof STAGE_TRACKER_TRAIN_MECHANICS !== 'undefined') lines.push(STAGE_TRACKER_TRAIN_MECHANICS);
      if (header) {
        // nothing more: the header speaks for the whole train
      } else if (mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT) lines.push('Position: 1st — merges next; the lane then updates or rebases the others');
      else {
        const prev = ahead.length ? ahead[ahead.length - 1] : (Number(mt.front) || 0);
        lines.push('Position: ' + stageTrackerOrdinal(mt.position) + ' of ' + size + (prev ? ' · after #' + prev + ' merges' : '') + (ahead.length > 1 ? ' · ahead: ' + ahead.map(n => '#' + n).join(', ') : ''));
      }
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
    // Pure: where a train's members are on the client, for the header's
    // missing line — listed: PR numbers in the filtered, ordered list
    // (any page); known: PR numbers in the whole payload. Built once per
    // render in tracker-filter.js from the rows the train helpers see (a
    // PR drawn under its issue counts under its own number).
    function stageTrackerTrainWhere(listed, known) {
      const nums = rows => {
        const out = {};
        (Array.isArray(rows) ? rows : []).forEach(it => {
          const rt = stageTrackerRowTrain(it && it.item ? it.item : it);
          if (rt && rt.pr > 0) out[rt.pr] = true;
          if (it && it.kind === 'pr' && Number(it.number) > 0) out[Number(it.number)] = true;
        });
        return out;
      };
      return { listed: nums(listed), known: nums(known) };
    }
    // Header line over a train's rows: "merge train · 4 PRs · 1st: #15074".
    // The count is the rows drawn under the header, never the server's
    // size alone: with shown (the PR numbers rendered in this run) a
    // train the page only partly carries says where the rest are —
    // "merge train · 1 of 3 PRs here · 1st: #15425 · on another page:
    // #15529, #15530" or "… · hidden by the filter: #15529" — so the header
    // never claims rows that are not there and never leaves the owner
    // asking what "not in this view" means. The server's size counts
    // only PRs in the merge queue (080 and beyond); a PR still at CI /
    // Review is not a member and is not mentioned here (hivecommons/hive#15653).
    // where is stageTrackerTrainWhere's answer; without it the rest read
    // "not on this page".
    function stageTrackerTrainHeaderText(mt, shown, where) {
      const size = Number(mt.size) || 0;
      const front = Number(mt.front) || 0;
      if (!Array.isArray(shown)) return 'merge train · ' + size + ' PRs · 1st: #' + front;
      const here = shown.length;
      let text = 'merge train · ' + (here < size ? here + ' of ' + size + ' PRs here' : here + (here === 1 ? ' PR' : ' PRs')) + ' · 1st: #' + front;
      if (here < size) {
        const missing = stageTrackerTrainMissing(mt, shown);
        const unnamed = size - here - missing.length;
        if (!where || !where.listed) {
          const named = missing.map(n => '#' + n);
          if (unnamed > 0) named.push(unnamed + ' more');
          text += ' · not on this page: ' + (named.length ? named.join(', ') : (size - here) + ' PRs');
          return text;
        }
        const paged = missing.filter(n => where.listed[n]);
        const filtered = missing.filter(n => !where.listed[n] && where.known && where.known[n]);
        const gone = missing.filter(n => !where.listed[n] && !(where.known && where.known[n]));
        if (paged.length) text += ' · on another page: ' + paged.map(n => '#' + n).join(', ');
        if (filtered.length) text += ' · hidden by the filter: ' + filtered.map(n => '#' + n).join(', ');
        const rest = gone.map(n => '#' + n);
        if (unnamed > 0) rest.push(unnamed + ' more');
        if (rest.length) text += ' · not in this payload: ' + rest.join(', ');
      }
      return text;
    }
    // idx is the train's accent slot on the page (stageTrackerTrainIndex);
    // the header and its rows share the rail colour it picks.
    function stageTrackerTrainHeaderHtml(item, shown, idx, where) {
      const rt = stageTrackerRowTrain(item);
      if (!rt) return '';
      const mt = rt.mt;
      const files = Array.isArray(mt.overlap_files) ? mt.overlap_files.filter(Boolean) : [];
      const tip = stageTrackerTrainLines(item.kind === 'pr' ? item : { merge_train: mt }, true).join('\n');
      return `<div class="ham-train-head${stageTrackerTrainAccentClass(idx)}" data-ham-train="${esc(stageTrackerTrainKey(item))}" role="row" title="${esc(tip)}">
        <span class="ham-train-chip">${stageTrackerTrainIconHtml('ham-train-icon')} ${esc(stageTrackerTrainHeaderText(mt, shown, where))}</span>${files.length ? `<span class="ham-train-files">overlapping: ${esc(files.join(', '))}</span>` : ''}
      </div>`;
    }
    // Row classes for a train member: the rail, whether it leads / ends
    // the run (prev/next are the neighbouring rows, or null), and the
    // train's accent slot (idx, from stageTrackerTrainIndex) so its rows
    // and header share one colour.
    function stageTrackerTrainRowClass(item, prev, next, idx) {
      const k = stageTrackerTrainKey(item);
      if (!k) return '';
      const rt = stageTrackerRowTrain(item);
      let cls = ' ham-train' + stageTrackerTrainAccentClass(idx);
      if (rt.mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT) cls += ' ham-train-front';
      if (!next || stageTrackerTrainKey(next) !== k) cls += ' ham-train-last';
      return cls;
    }
    // Inserts a train header entry before the first row of each train run.
    // Entries are {key, html}; items are the page's row items in order. The
    // header counts the run it heads — the rows actually drawn under it.
    // where (stageTrackerTrainWhere) tells the header where the members
    // it does not draw are.
    function stageTrackerTrainRows(items, rowOf, where) {
      const out = [];
      let prevKey = '';
      const list = items || [];
      const itemOf = e => (e && e.item ? e.item : (e && e.group ? null : e));
      const trains = stageTrackerTrainIndex(list);
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
          out.push({ key: 'train:' + k, html: stageTrackerTrainHeaderHtml(it, shown, trains.index[k], where) });
        }
        prevKey = k;
        out.push(rowOf(e, i));
      });
      return out;
    }
