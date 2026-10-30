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
    // Pure: the PR a nested original was superseded by — `superseded_by`
    // from the server, or `adopted_by` while the copy is live — when that
    // copy is itself listed under the issue; else 0.
    function stageTrackerLinkedSuperseder(lp, issue) {
      if (!lp) return 0;
      const live = lp.adopted_by_state !== 'closed' && lp.adopted_by_state !== 'closed-twice' && lp.adopted_by_state !== 'copy-failed';
      const by = Number(lp.superseded_by) || (live ? Number(lp.adopted_by) || 0 : 0);
      if (!(by > 0) || by === Number(lp.number)) return 0;
      const linked = Array.isArray(issue && issue.linked_prs) ? issue.linked_prs : [];
      return linked.some(x => x && Number(x.number) === by && String(x.state || '') !== 'closed') ? by : 0;
    }
    // Pure: an adopted original listed under its issue — closed, or still
    // open — with the hive's copy in the list draws as one muted line
    // without a chevron track (hivecommons/hive#15126, #15511): the note
    // reads "superseded · hive adopted → #N", "superseded · closed · hive
    // adopted → #N" once the close landed. '' when the copy is not listed.
    function stageTrackerLinkedSupersededText(lp, issue) {
      const by = stageTrackerLinkedSuperseder(lp, issue);
      if (!by) return '';
      return 'superseded · ' + (String(lp.state || '') === 'closed' ? 'closed · ' : '') + 'hive adopted → #' + by;
    }
    // Pure: the hover behind that note — what hive did and what becomes of
    // this PR.
    function stageTrackerLinkedSupersededTip(lp) {
      const by = Number(lp && (lp.superseded_by || lp.adopted_by)) || 0;
      if (!by) return '';
      return 'Hive re-authored this PR as #' + by + ' with the same commits and sign-off. ' + (String(lp.state || '') === 'closed' ? 'This PR is closed as superseded.' : 'This PR closes as superseded when #' + by + ' merges.');
    }
    // Pure: a closed original whose superseder is no longer listed open
    // under the issue (merged, closed, filtered) has nothing left to say
    // and is not drawn.
    function stageTrackerLinkedSupersededGone(lp, issue) {
      return !!lp && String(lp.state || '') === 'closed' && !stageTrackerLinkedSuperseder(lp, issue);
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
    //   detail — the live part after " · " ("next pass in 42s", "7m left");
    //            always a gate fact, never the issue's lane reason
    //   tip    — one plain sentence for the glyph's title + aria-label; on
    //            an issue row it leads with "open PR #N covers it · at <stage>"
    //   cue    — 'running' (sweep on it now) | 'waiting' | 'soaking' | 'merging' | ''
    //   until  — ms epoch the detail counts down to (0 when none)
    // trains — how many trains the page draws (ctx.trains); past one a
    // queue word names its train.
    function stageTrackerMergeGateInfo(item, nowMs, trains) {
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
      if (!state && typeof stageTrackerDecidedOf === 'function' && (stageTrackerDecidedOf(src, nowMs) || stageTrackerDecidedOf(item, nowMs))) return stageTrackerMergeGateInfoFor(src, item, nowMs, trains);
      if (!state || state === 'merged') return null;
      const info = stageTrackerMergeGateInfoFor(src, item, nowMs, trains);
      const coverWhy = stageTrackerCoverReasonText(item);
      if (coverWhy) info.tip = coverWhy.charAt(0).toUpperCase() + coverWhy.slice(1) + ' — ' + info.tip;
      return info;
    }
    // src carries the merge_gate / stage read; item is the row itself (its
    // lane reason stands in for a gate reason only on a PR row).
    function stageTrackerMergeGateInfoFor(src, item, nowMs, trains) {
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
      // between "… · checking now" and the bare form each sweep,
      // and a front in its soak keeps its place word (never the slug) —
      // its countdown already includes the soak left.
      // The sweep is the spinning ⬡ and a "Checking now…" tooltip line.
      const trainSrc = stageTrackerTrain(src) ? src : (stageTrackerTrain(item) ? item : null);
      const mt = trainSrc ? stageTrackerTrain(trainSrc) : null;
      // A provisional slot (the member's own stage is short of the gate):
      // the chip's headline is the stage's blocker, the slot a muted
      // trailing detail, and the hover leads with what blocks it.
      if (mt && (state === STAGE_TRACKER_TRAIN_STATE_FRONT || state === STAGE_TRACKER_TRAIN_STATE_QUEUED) && stageTrackerTrainProvisional(trainSrc)) {
        const blocker = stageTrackerReviewPassedDetail(laneReason || mg.reason) || String((trainSrc && trainSrc.caption) || '').split(/ — /)[0].trim();
        const headline = stageTrackerTrainProvisionalHeadline(trainSrc);
        const tagTip = stageTrackerTrainTagTip(mt, trains);
        const tip = headline + (blocker ? ' — ' + blocker : '') + '. ' + (tagTip ? tagTip + ' ' : '') + stageTrackerTrainQueueTip(mt, nowMs).replace(/ · merges next/, ' · merges first once it is ready') + (mg.sweep_running ? ' Checking now…' : '') + lastSeen;
        return { state, word: headline, detail: stageTrackerTrainProvisionalSuffix(trainSrc, trains), tip, cue: 'waiting', until: 0, train: mt, stalled: false, provisional: true };
      }
      if (mt && (state === STAGE_TRACKER_TRAIN_STATE_FRONT || state === STAGE_TRACKER_TRAIN_STATE_QUEUED || state === STAGE_TRACKER_TRAIN_STATE_REBASING || state === STAGE_TRACKER_TRAIN_STATE_EXHAUSTED)) {
        const trainWord = stageTrackerTrainWord(trainSrc, trains) || STAGE_TRACKER_TRAIN_STATE_WORD[STAGE_TRACKER_TRAIN_STATE_QUEUED];
        const queued = state === STAGE_TRACKER_TRAIN_STATE_FRONT || state === STAGE_TRACKER_TRAIN_STATE_QUEUED;
        const tagTip = queued ? stageTrackerTrainTagTip(mt, trains) : '';
        const queueTip = (tagTip ? tagTip + ' ' : '') + (queued ? stageTrackerTrainQueueTip(mt, nowMs) : stageTrackerTrainLines(trainSrc).join('. ') + '.');
        const checking = mg.sweep_running ? ' Checking now…' : '';
        // The countdown to its turn ("· ~4m") ticks from eta_at like the
        // soak clock, so a poll re-render does not reset it; a blocked
        // head or a stalled train shows no clock; past twice the first
        // estimate it reads "running late".
        const etaAt = queued && !mt.stalled ? stageTrackerTrainEtaAt(mt, nowMs) : 0;
        const detail = etaAt > 0 ? (mt.late ? 'running late' : stageTrackerTrainEtaDetail(etaAt, nowMs)) : '';
        return { state, word: trainWord, detail, tip: queueTip + checking + lastSeen, cue: mg.sweep_running ? 'running' : 'waiting', until: etaAt > 0 && !mt.late ? etaAt : 0, train: mt, stalled: !!mt.stalled };
      }
      // A soak clock (own or mirrored) outranks "checking now": the
      // sweep reads the clock, it does not replace it. Any gate stage the
      // lane holds for the soak — 095 admitted, or 060/080 while trust
      // still blocks the head — counts the same clock down.
      const clock = stageTrackerSoakClock(src, item, nowMs);
      if (clock && (state === 'soaking' || clock.fromSoak)) {
        // At 060/080 the stage keeps its own word and the detail says
        // "soak 12m left"; at 095 the soak is the stage.
        clock.prefix = state === 'soaking' ? '' : 'soak ';
        const soak = stageTrackerSoakWords(clock, nowMs, nextAt, !!mg.sweep_running);
        return { state, word: clock.restarted || state === 'soaking' ? soak.word : word, detail: soak.detail, tip: soak.tip + lastSeen, cue: soak.cue || 'soaking', until: clock.restarted ? 0 : clock.until, soak: soak.tip, soakClock: clock };
      }
      // 060 Review passed: the pill carries the lane's own recorded reason
      // ("CI in progress", "checks unreadable", "soak 30m remaining" —
      // stagelabels RouteHuman), in the CI / Review column; the spinning cue
      // says the sweep is on it now.
      const preWhy = state === 'gate-pending' ? (stageTrackerReviewPassedDetail(mg.reason || laneReason) || 'waiting for merge admission') : '';
      if (state === 'merge-failed') return stageTrackerMergeFailedInfo(mg, nowMs, nextAt, lastSeen);
      if (mg.sweep_running) {
        return { state, word, detail: preWhy || 'checking now', tip: (preWhy ? 'Review passed, not yet admitted to auto merge — ' + preWhy + '; ' : '') + 'hive is checking this PR now.', cue: 'running', until: 0 };
      }
      const nextWords = stageTrackerNextPassWords(nextAt, nowMs);
      const sooner = ' A GitHub event on the PR (check run, review, push) triggers a pass sooner.';
      if (state === 'soaking') {
        return { state, word, detail: 'complete', tip: 'Soak complete — merges on the next pass' + (nextWords ? ' (' + nextWords + ')' : '') + '.' + lastSeen, cue: 'soaking', until: 0 };
      }
      if (state === 'ready') {
        return { state, word, detail: nextWords, tip: "Approved and green; waiting for hive's next merge pass" + (nextWords ? ' — ' + nextWords + '.' : '.') + sooner + lastSeen, cue: 'waiting', until: nextAt > nowMs ? nextAt : 0 };
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
      const nextWords = stageTrackerNextPassWords(nextAt, nowMs);
      const details = why ? ' Details: ' + why : '';
      if (kind === 'checks-pending') {
        const tip = 'hive tried to merge this' + when + ' before its required checks had reported — ' + (text || "required checks haven't finished") + '. It merges once they pass' + (mg.sweep_running ? '; hive is on it now' : (nextWords ? ' (' + nextWords + ')' : '')) + '.' + lastSeen + details;
        return { state, word: 'Waiting for CI', detail: checks > 0 ? checks + ' check' + (checks === 1 ? '' : 's') : '', tip, cue: mg.sweep_running ? 'running' : 'waiting', until: 0 };
      }
      const word = STAGE_TRACKER_MERGE_GATE_WORD[state];
      const sentence = 'hive tried to merge this' + when + ' and GitHub refused' + (text ? ' — ' + text : '') + '.';
      if (mg.sweep_running) {
        return { state, word, detail: 'retrying', tip: sentence + ' hive is retrying it now.' + lastSeen + details, cue: 'running', until: 0 };
      }
      return { state, word, detail: STAGE_TRACKER_MERGE_REFUSAL_DETAIL[kind] || 'GitHub refused', tip: sentence + ' The lane re-evaluates it on its next pass' + (nextWords ? ' (' + nextWords + ')' : '') + '.' + lastSeen + details, cue: 'waiting', until: 0 };
    }
    // Pure: the hover note for a refusal that belongs to an earlier head
    // (merge_gate.prior_head_merge_failed*): the PR's head moved, or the
    // record fell back below 080 to wait on CI, after GitHub refused the
    // merge. The chevron reads the live stage; this line is the history —
    // "merge attempt on previous head failed: branch protection". '' when
    // there is none. Never a chevron word.
    function stageTrackerPriorHeadMergeFailedNote(item) {
      const mg = item && item.merge_gate;
      if (!mg || (!mg.prior_head_merge_failed && !mg.prior_head_merge_failed_kind)) return '';
      const why = String(mg.prior_head_merge_failed || '').trim();
      const fallback = why && !mg.prior_head_merge_failed_kind ? stageTrackerMergeRefusalFromRaw(why) : null;
      const kind = mg.prior_head_merge_failed_kind || (fallback && fallback.kind) || '';
      const detail = kind === 'checks-pending' ? 'required checks had not reported' : (STAGE_TRACKER_MERGE_REFUSAL_DETAIL[kind] || stageTrackerMergeFailedShort(why));
      const head = String(mg.prior_head_merge_failed_head || '').slice(0, 7);
      return 'merge attempt on previous head' + (head ? ' ' + head : '') + ' failed: ' + detail;
    }
    // Pure: a refusal's first line, cut to STAGE_TRACKER_MERGE_FAILED_SHORT_MAX
    // (tooltips and toasts; the pill detail comes from the refusal kind).
    function stageTrackerMergeFailedShort(why) {
      const line = String(why || '').split('\n')[0].replace(/^merging PR \S+ \([^)]*\): /, '').trim();
      if (!line) return 'GitHub refused the merge';
      return line.length > STAGE_TRACKER_MERGE_FAILED_SHORT_MAX ? line.slice(0, STAGE_TRACKER_MERGE_FAILED_SHORT_MAX - 1).trimEnd() + '…' : line;
    }
    // Pure: the pill word with its live detail — "Ready for auto merge ·
    // next pass in 42s". The detail span carries data-ham-until so the
    // countdown ticks in place.
    function stageTrackerMergeGateWordHtml(item, nowMs, trains) {
      const info = stageTrackerMergeGateInfo(item, nowMs, trains);
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
    // Pure: "next pass in 42s" until the lane's next pass, "pass due now"
    // once it has come, '' when the next pass is unknown. Never the word
    // "sweep" — that is the lane's internal name, not a state.
    function stageTrackerNextPassWords(nextAt, nowMs) {
      if (nextAt > nowMs) return 'next pass in ' + stageTrackerShortLeft(nextAt - nowMs);
      return nextAt > 0 ? 'pass due now' : '';
    }
    // Pure: { word, detail, tip, cue } for a soak clock at nowMs.
    //   soaking   — "Soaking · 12m left"; the hover carries the rule, the
    //               live time left and the end time
    //   complete  — past until, waiting for the lane's next pass:
    //               "Soaking · complete"
    //   mergeable — past until and the lane's pass is on it now:
    //               "Mergeable · merging now"
    //   restarted — "Soak restarted · new head"
    function stageTrackerSoakWords(clock, nowMs, nextAt, sweepRunning) {
      const short = clock.head ? clock.head.slice(0, 7) : '';
      const who = short ? 'Head ' + short : 'The head';
      const mins = clock.windowS > 0 ? Math.round(clock.windowS / 60) : 0;
      const window = mins > 0 ? mins + ' minute' + (mins === 1 ? '' : 's') : 'the soak window';
      if (clock.restarted) {
        return { word: 'Soak restarted', detail: 'new head', tip: who + ' was replaced by a new push, so the soak starts over on the new head: it must sit ' + window + ' without new commits before the hive merges it. hive re-reads it on its next pass.' };
      }
      const prefix = clock.prefix || '';
      const leftMs = clock.until - nowMs;
      if (leftMs > 0) {
        return { word: 'Soaking', detail: prefix + stageTrackerShortLeft(leftMs) + ' left', tip: who + ' must sit ' + window + ' without new commits before the hive merges it · ' + stageTrackerSoakLeftText(leftMs) + ' left · soak ends ' + stageTrackerClockText(clock.until) };
      }
      const soaked = who + ' sat ' + window + ' without new commits — soaked since ' + stageTrackerClockText(clock.until) + '. ';
      if (sweepRunning) {
        return { word: 'Mergeable', detail: prefix ? 'soaked — merging now' : 'merging now', tip: soaked + 'Mergeable — merging now.', cue: 'running' };
      }
      const next = stageTrackerNextPassWords(nextAt, nowMs);
      return { word: 'Soaking', detail: prefix ? 'soak complete' : 'complete', tip: soaked + 'Soak complete — merges on the next pass' + (next ? ' (' + next + ')' : '') + '.', cue: 'soaking' };
    }
    // Pure: the leading glyph for a row on the gate — spinning while hive's
    // sweep is on it, a clock while it waits — with the sentence as title
    // and aria-label. '' off the gate.
    function stageTrackerGateCueHtml(item, nowMs, trains) {
      const info = stageTrackerMergeGateInfo(item, nowMs, trains);
      if (!info) return '';
      const spin = info.cue === 'running' || info.cue === 'merging';
      return `<span class="ham-gate-ico${spin ? ' cue-spin' : ''}" data-gate-cue="${esc(info.cue)}" role="img" title="${esc(info.tip)}" aria-label="${esc(info.tip)}">${stageTrackerIcon(spin ? 'sweep' : 'wait', 'ham-ico')}</span>`;
    }
    // Pure: the queue chip's countdown words — "~4m" until the estimate,
    // "any moment now" once it has passed.
    function stageTrackerTrainEtaDetail(etaAt, nowMs) {
      const left = stageTrackerShortLeft(etaAt - nowMs);
      return left ? '~' + left : 'any moment now';
    }
    // Pure: ticks every countdown in el from nowMs; "pass due now" once a
    // ready row's next pass has come.
    function stageTrackerGateCountdownSync(el, nowMs) {
      if (!el || typeof el.querySelectorAll !== 'function') return;
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      for (const n of Array.from(el.querySelectorAll('.ham-gate-detail[data-ham-until]'))) {
        const until = Number(n.getAttribute('data-ham-until'));
        const kind = n.getAttribute('data-ham-until-kind');
        if (n.getAttribute('data-ham-soak') === '1') { stageTrackerSoakTick(n, nowMs); continue; }
        const left = stageTrackerShortLeft(until - nowMs);
        let t = kind === 'soaking' ? (left ? '· ' + left + ' left' : '· complete') : (left ? '· next pass in ' + left : '· pass due now');
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
      // At 095 the soak owns the pill word too: a soaked head under the
      // lane's pass flips "Soaking" to "Mergeable" without a re-render.
      const wordNode = n.previousSibling;
      if (!clock.prefix && wordNode && wordNode.nodeType === 3 && wordNode.textContent.trim() !== words.word) wordNode.textContent = words.word + ' ';
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
