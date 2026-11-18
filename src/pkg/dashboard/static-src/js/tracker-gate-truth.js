    /* ---- The human gate's one source of truth (hivecommons/hive#18769) ---
       The gate pill and its verbs used to read two different things: the
       pill lit "Needs human · review" from the cached reviewer verdict for
       the head (review_activity.state === 'human_decision'), while every
       verb acted only on a PR the hive had actually parked on a person
       (runRowAction → stageTrackerDecisionTarget). A verdict never changes
       for a head, so once a person approved it the pill stayed and each
       verb answered "nothing to act on" (hivecommons/hive#18667).

       One rule now, shared by the pill and the action:
         stageTrackerGateParked  — the hive parks the row on a person; the
                                   review reason and a bare needs_human
                                   stand only while it does
         stageTrackerGateAccepts — a verb is drawn only when runRowAction
                                   would find something to act on
       The server states the same rule per row as `human_gate`
       (stage_tracker_gate.go; testdata/gate_states.json pins both to one
       table) and names what an item with no decision pending actually
       waits on — required checks (running, stuck, unreported, red), the
       review of a new head, the merge queue, its author, a conflict. */
    const STAGE_TRACKER_GATE_WAIT_WORD = 'waiting on ';
    const STAGE_TRACKER_GATE_NO_DECISION = 'No decision pending';
    const STAGE_TRACKER_GATE_APPROVED_BY = 'Approved by @';
    const STAGE_TRACKER_GATE_STALE = ' is not waiting on that decision';
    /* ---- Mid-flight PRs (hivecommons/hive#18889) --------------------------
       A decision is a call on the head as it stands. While the CURRENT
       head's CI is running, its review is queued or unfinished, or the CI /
       review on file belong to an earlier head (a push restarted both), the
       call is premature. The server states it per row as `mid_flight`
       ({kind, text, head, park}); the verbs that rest on the head being
       checked are drawn disabled with the reason instead of offered. A park
       recorded for an earlier head does not count. */
    const STAGE_TRACKER_MIDFLIGHT_VERBS = ['decide:delegate', 'decide:approve', 'decide:direction', 'gate:decide'];
    // Pure: the server's mid_flight on the row, null when the head is settled.
    function stageTrackerMidFlightOf(item) {
      const m = item && item.mid_flight;
      return m && typeof m === 'object' && String(m.text || '').trim() ? m : null;
    }
    // Pure: the reason a decision on the row is not offered yet, '' when it is.
    function stageTrackerMidFlightWhy(item) {
      const m = stageTrackerMidFlightOf(item);
      if (!m) return '';
      const park = String(m.park || '').trim();
      return 'Waiting on ' + String(m.text).trim() + ': the decision opens when CI and review finish on the current head' + (park ? ' (parked because ' + park + ')' : '');
    }
    // Pure: disables, in place, the options that rest on the head being
    // checked while the row is mid-flight; the options come back unchanged
    // otherwise.
    function stageTrackerMidFlightApply(options, item) {
      const why = stageTrackerMidFlightWhy(item);
      if (!why) return options;
      (options || []).forEach(o => { if (o && STAGE_TRACKER_MIDFLIGHT_VERBS.indexOf(o.id) >= 0) { o.enabled = false; o.why = why; } });
      return options;
    }
    // Pure: the server's gate for the row (`human_gate`), null when absent.
    function stageTrackerServerGate(item) {
      const g = item && item.human_gate;
      return g && typeof g === 'object' && Array.isArray(g.reasons) ? g : null;
    }
    // Pure: whether the hive parks the row on a person. A PR (or an issue
    // drawn through its one covering PR) by its own stage and pills; a
    // plain issue when a decision can be recorded on it.
    function stageTrackerGateParked(item) {
      if (!item) return false;
      if ((item.via_pr ? 'pr' : item.kind) === 'pr') return typeof stageTrackerPRParked === 'function' ? stageTrackerPRParked(item) : true;
      return typeof stageTrackerDecisionTarget === 'function' ? !!stageTrackerDecisionTarget(item) : true;
    }
    // Pure: what a decide:* action on the row acts on — runRowAction's own
    // mapping, so the pill and the click can never disagree.
    function stageTrackerGateTargets(item, id) {
      if (id === 'decide:delegate') return stageTrackerDelegateTargets(item);
      return [stageTrackerDecisionTarget(item)].filter(Boolean);
    }
    // Pure: the action precondition — whether a gate verb would act on the
    // row. Only the decide:* verbs have one; the rest post on the row itself.
    function stageTrackerGateAccepts(item, id) {
      if (String(id || '').indexOf('decide:') !== 0) return true;
      return stageTrackerGateTargets(item, id).length > 0;
    }
    // Pure: whether the reviewer's sign-off ask on this head is no longer
    // open — the verdict still reads human_decision but the hive does not
    // park the PR on it (a person approved, or the head is not there yet).
    function stageTrackerGateAnswered(item) {
      if (!item || (item.via_pr ? 'pr' : item.kind) !== 'pr') return false;
      // A closed, merged, superseded or draft head has no ask to answer.
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (item.state === 'closed' || item.superseded || item.superseded_by || item.adopted_by || mods.indexOf('merged') >= 0 || mods.indexOf('draft') >= 0) return false;
      if (Number(item.stage) >= (typeof STAGE_TRACKER_TERMINAL === 'number' ? STAGE_TRACKER_TERMINAL : 110)) return false;
      const a = item.review_activity;
      if (!a || a.state !== 'human_decision' || (a.head && item.head && String(a.head) !== String(item.head))) return false;
      return !stageTrackerGateParked(item);
    }
    // Pure: the CI words of a head with stuck required checks, '' when none
    // is stuck: "'PR Type', 'PR Issue' stuck", or "13 running · 2 stuck".
    function stageTrackerStuckChecksWord(a, pendingCount) {
      const stuck = a && Array.isArray(a.checks_stuck) ? a.checks_stuck.map(String).filter(Boolean) : [];
      if (!stuck.length) return '';
      const names = stuck.map(n => "'" + n + "'").join(', ');
      const moving = Math.max(0, (Number(pendingCount) || 0) - stuck.length);
      return moving ? moving + ' running · ' + stuck.length + ' stuck (' + names + ')' : names + ' stuck';
    }
    // Pure: what the row waits on when no decision is pending — the
    // server's sentence, else the CI track's own words. '' when unknown.
    function stageTrackerGateWaitText(item) {
      const g = stageTrackerServerGate(item);
      if (g && g.waiting_on && g.waiting_on.text) return String(g.waiting_on.text);
      const ci = item && item.ci_activity;
      if (!ci || (ci.head && item.head && String(ci.head) !== String(item.head))) return '';
      const stuck = stageTrackerStuckChecksWord(ci, Array.isArray(ci.checks_pending) ? ci.checks_pending.length : 0);
      if (stuck) return 'required checks: ' + stuck;
      const tally = Number(ci.checks_total) > 0 ? ' (' + (Number(ci.checks_passed) || 0) + '/' + Number(ci.checks_total) + ')' : '';
      if (ci.state === 'failing' && Array.isArray(ci.checks_failing) && ci.checks_failing.length) return 'required checks failing: ' + ci.checks_failing.join(', ');
      if (ci.state === 'missing' && Array.isArray(ci.checks_pending) && ci.checks_pending.length) return 'required checks not reported: ' + ci.checks_pending.join(', ');
      return ci.state === 'running' || ci.state === 'queued' ? 'CI running' + tally : '';
    }
    // Pure: the passive pill of a PR whose sign-off ask is answered: who
    // approved (when the lane recorded it) and the real blocker. '' when
    // the ask is still open or the row never had one.
    function stageTrackerGateWaitingHtml(item) {
      if (!stageTrackerGateAnswered(item)) return '';
      const g = stageTrackerServerGate(item);
      const by = String((g && g.answered_by) || item.human_approver || '').replace(/^@/, '').trim();
      const wait = stageTrackerGateWaitText(item);
      const lead = by ? STAGE_TRACKER_GATE_APPROVED_BY + by : STAGE_TRACKER_GATE_NO_DECISION;
      const text = lead + (wait ? ' · ' + STAGE_TRACKER_GATE_WAIT_WORD + wait.replace(/ \(in progress [^)]*\)/, '') : '');
      const tip = (by ? 'A person approved this head; the reviewer\'s sign-off question is answered.' : 'The reviewer\'s sign-off question is not open: the hive does not park this head on a person.') + (wait ? ' It is ' + STAGE_TRACKER_GATE_WAIT_WORD + wait + '.' : '') + ' Nothing here needs a decision.';
      return `<span class="ham-badge ham-gate-waiting" role="status" data-ham-gate-waiting="${esc(String((g && g.waiting_on && g.waiting_on.kind) || ''))}" title="${esc(tip)}" aria-label="${esc(text)}">${esc(text)}</span>`;
    }
    // The toast of a verb clicked where nothing waits on it (a stale view):
    // it names what the item IS waiting on, and the tracker re-reads the
    // server so the row drops the verb.
    function stageTrackerGateStaleNote(rows) {
      const list = (rows || []).filter(Boolean);
      const refs = list.map(stageTrackerRefOf).filter(Boolean).join(', ');
      if (!refs) return '';
      const wait = list.length === 1 ? stageTrackerGateWaitText(list[0]) : '';
      return ' — ' + refs + (wait ? ' is ' + STAGE_TRACKER_GATE_WAIT_WORD + wait : STAGE_TRACKER_GATE_STALE);
    }
    function stageTrackerGateRefresh() {
      if (typeof fetchStageTracker !== 'function') return;
      _stageTrackerEtag = ''; _stageTrackerSig = '';
      fetchStageTracker();
    }
