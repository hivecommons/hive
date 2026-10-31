    const STAGE_TRACKER_ACTED_STATES = ['waiting', 'addressed', 'stuck', 'pending', 'failed', 'executing', 'blocked'];
    const STAGE_TRACKER_ACTED_FILTERS = ['any', 'awaiting', 'acted', 'stuck'];
    const STAGE_TRACKER_ACTED_STUCK_DEFAULT_S = 30 * 60;
    // Every state wears the same history mark (the clock-and-arrow the
    // column header uses): the badge is an audit affordance, never a "done"
    // tick. Colour and border carry the state; the aria-label says it in words.
    const STAGE_TRACKER_HISTORY_ICON = '<svg class="ham-ico ham-history-ico" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + STAGE_TRACKER_ICONS.history + '</svg>';
    const STAGE_TRACKER_ACTED_GLYPH = { waiting: STAGE_TRACKER_HISTORY_ICON, addressed: STAGE_TRACKER_HISTORY_ICON, stuck: STAGE_TRACKER_HISTORY_ICON, pending: STAGE_TRACKER_HISTORY_ICON, failed: STAGE_TRACKER_HISTORY_ICON, executing: STAGE_TRACKER_HISTORY_ICON, blocked: STAGE_TRACKER_HISTORY_ICON };
    function stageTrackerActedLabel(action) { return STAGE_TRACKER_ACTED_LABELS[action] || String(action || '').replace(/-/g, ' '); }
    // Pure: the record a row's badge reads — the item's own, else (an issue
    // row drawn over its single covering PR) that PR's; null when none.
    function stageTrackerActedSource(item) {
      if (!item) return null;
      // A failure this client just saw outranks an older server record
      // (hivecommons/hive#15436): the badge goes red at once.
      const failed = typeof stageTrackerActionFailureOf === 'function' ? stageTrackerActionFailureOf(item) : null;
      if (failed) return { item, action: failed, count: (Number(item.operator_action_count) || 0) + 1 };
      if (item.last_operator_action) return { item, action: item.last_operator_action, count: Number(item.operator_action_count) || 1 };
      const linked = Array.isArray(item.linked_prs) ? item.linked_prs : [];
      if (item.kind === 'issue' && linked.length === 1 && linked[0].last_operator_action) {
        return { item: { repo: item.repo, number: linked[0].number, kind: 'pr', stage: linked[0].stage || 0, modifiers: linked[0].modifiers || [], reason: linked[0].reason || '', caption: linked[0].caption || '', decision_needed: linked[0].decision_needed || null, decided: linked[0].decided || null }, action: linked[0].last_operator_action, count: Number(linked[0].operator_action_count) || 1 };
      }
      return null;
    }
    // Pure: the badge state as of nowMs. The server judges `stuck` when it
    // builds the payload; a row still `waiting` ages into `stuck` here once
    // the window passes, so the badge turns amber between refreshes too.
    function stageTrackerActedState(action, nowMs) {
      if (!action) return '';
      let state = STAGE_TRACKER_ACTED_STATES.indexOf(action.state) >= 0 ? action.state : 'waiting';
      if (state === 'waiting') {
        const at = Date.parse(action.at || '');
        const windowMs = (Number(action.stuck_after_seconds) || STAGE_TRACKER_ACTED_STUCK_DEFAULT_S) * 1000;
        if (at && (Number(nowMs) || Date.now()) - at >= windowMs) state = 'stuck';
      }
      return state;
    }
    // Pure: minutes/hours/days in words for the badge tooltip.
    function stageTrackerActedAgo(ms) {
      const min = Math.max(0, Math.floor((Number(ms) || 0) / 60000));
      if (min < 1) return 'just now';
      if (min < 60) return min + ' min ago';
      const h = Math.floor(min / 60);
      if (h < 48) return h + ' h ago';
      return Math.floor(h / 24) + ' d ago';
    }
    function stageTrackerActedOutcomeWords(action) {
      switch (action.outcome) {
        case 'applied': return 'done' + (action.detail ? ' — ' + stageTrackerActedPlain(action.detail) : '');
        case 'no-op': return 'nothing changed' + (action.detail ? ': ' + stageTrackerActedPlain(action.detail) : '');
        case 'failed': return 'failed' + (action.detail ? ' — ' + stageTrackerActedPlain(action.detail) : '');
        case 'pending': return action.action === 'merge-now' ? 'rebasing, then merging…' : (action.action === 'queue-merge' ? 'approved — hive will merge on the next tick once CI is green' : 'the hive is deciding…');
        case 'executing': return (action.detail ? stageTrackerActedPlain(action.detail) : 'decided — the hive is carrying it out…');
        case 'no-effect': return "hasn't taken effect" + (action.detail ? ' — ' + stageTrackerActedPlain(action.detail) : '');
        case 'blocked': return 'blocked' + (action.detail ? ' — ' + stageTrackerActedPlain(action.detail) : '');
        case 'superseded-by-state-change': return 'the item moved on by itself';
      }
      return action.outcome || '';
    }
    // Pure: the stage in plain words, never a code ("Merge gate", not 085).
    function stageTrackerActedStageName(n) {
      const num = Number(n) || 0;
      const st = ((_stageTracker && _stageTracker.stages) || []).find(s => Number(s.number) === num);
      return st && (st.short || st.label) ? (st.short || st.label) : (num ? 'stage ' + num : 'an unknown stage');
    }
    // Pure: operator-facing gloss for the slugs the hive writes into stage
    // reasons and endpoint status words. Unknown input is returned with its
    // dashes and colons spelled as words so nothing reads like a code.
    const STAGE_TRACKER_ACTED_PLAIN = {
      'contributor-lane:changes-requested': 'a reviewer requested changes; needs a new commit',
      'contributor-lane:approved': 'a reviewer approved it',
      'contributor-lane:ci-failed': 'CI failed on the latest commit',
      'needs-rebase': 'needs a rebase onto the base branch',
      'needs-human': 'waiting on a person',
      'needs-decision': 'waiting on your decision',
      unparked: 'released to the hive',
      'approval-commented': 'your approval was posted as a comment',
      closed: 'closed on GitHub',
      held: 'put on hold',
      released: 'hold released',
      'already accepted': 'it was already accepted',
      'approval-recorded': 'recorded your approval', 'rebase-kicked': 'started the rebase', rebased: 'rebased it onto the base branch', 'ci-green': 'saw CI go green', merged: 'merged it',
      'rebase-conflict': 'the rebase conflicts', 'ci-red': 'CI went red on the rebased head', 'rebase-timeout': 'the rebase did not land in time', 'head-not-pushable': 'the hive cannot push to this fork', 'foreign-head': 'the head moved after your approval, pushed by someone other than the hive',
    };
    function stageTrackerActedPlain(text) {
      const t = String(text || '').trim();
      if (!t) return '';
      if (STAGE_TRACKER_ACTED_PLAIN[t]) return STAGE_TRACKER_ACTED_PLAIN[t];
      if (/^hive decided: /.test(t)) return t.replace(/^hive decided: /, 'the hive decided: ').replace(/ \(([^)]+)\)/, (m, r) => ' (rule: ' + r.replace(/[-_:]/g, ' ') + ')');
      if (/^decided: /.test(t)) return t.replace(/^decided: /, 'the hive decided: ').replace(/ \(([^)]+)\)/, (m, r) => ' (rule: ' + r.replace(/[-_:]/g, ' ') + ')');
      if (/^[a-z0-9]+([-_:][a-z0-9]+)+$/.test(t)) return t.replace(/[-_:]/g, ' ');
      // Slugs inside a sentence ("… — contributor-lane:changes-requested blocks it").
      return t.replace(/\b[a-z0-9]+(?:[-_:][a-z0-9]+)+\b/g, m => STAGE_TRACKER_ACTED_PLAIN[m] || (/[a-z]/.test(m) ? m.replace(/[-_:]/g, ' ') : m));
    }
    // Row-menu action id each ledger action was submitted under, so the
    // option the owner just took is not offered back to them.
    const STAGE_TRACKER_ACTED_OPTION_ID = { 'let-hive-decide': 'decide:delegate', approve: 'decide:approve', 'give-direction': 'decide:direction', 'close-superseded': 'decide:close-superseded', wontfix: 'decide:close-wontfix', 'close-read': 'decide:close-read', 'close-issue': 'decide:close-issue', 'accept-triage': 'accept-triage', hold: 'hold', 'merge-now': 'decide:merge-now', 'queue-merge': 'decide:queue-merge', 're-author': 'decide:re-author', adopt: 'decide:adopt', 'adopt-now': 'decide:adopt-now', reroute: 'decide:reroute' };
    // Pure: what the owner can still do when their action changed nothing
    // (no-op) or the item has not moved (stuck): the enabled options from
    // the row's decision_needed payload, minus the one just taken.
    function stageTrackerActedOptions(item, action, state) {
      if (!(action.outcome === 'no-op' || state === 'stuck' || state === 'failed' || state === 'blocked')) return '';
      const dn = item && item.decision_needed;
      const opts = dn && Array.isArray(dn.options) ? dn.options : [];
      const taken = STAGE_TRACKER_ACTED_OPTION_ID[action.action] || '';
      return opts.filter(o => o && o.enabled && o.action !== taken && o.label).map(o => o.label).join(' · ');
    }
    // Pure: the tooltip — the decision trail in plain words, in the order
    // every cue on the row uses: what it is waiting on → what you chose →
    // what the hive did next (who, when). ≤ 2 short lines per fact.
    function stageTrackerActedTip(item, nowMs) {
      const src = stageTrackerActedSource(item);
      if (!src) return '';
      const a = src.action;
      nowMs = Number(nowMs) || Date.now();
      const state = stageTrackerActedState(a, nowMs);
      const atMs = Date.parse(a.at || '') || nowMs;
      const me = !a.actor || (window._hiveUser && a.actor.toLowerCase() === String(window._hiveUser).toLowerCase());
      const who = me ? 'You' : '@' + a.actor;
      const label = stageTrackerActedLabel(a.action) + (a.target ? ' ' + a.target : '');
      const ago = stageTrackerActedAgo(nowMs - atMs);
      const stageNow = stageTrackerActedStageName(src.item.stage);
      const waitingOn = stageTrackerActedPlain(src.item.caption || src.item.reason);
      const lines = [];
      const dec = src.item.decided || (item && item.decided);
      if (dec && typeof stageTrackerDecidedLead === 'function') lines.push(stageTrackerDecidedLead(dec, a, nowMs));
      else if (state === 'addressed') lines.push('Addressed — now at ' + stageNow + '.');
      else if (state === 'stuck') lines.push('Still at ' + stageNow + ' ' + ago.replace(/ ago$/, '') + ' after you asked' + (waitingOn ? ' — hive says: ' + waitingOn : '') + '.');
      else if (state === 'pending') lines.push('At ' + stageNow + ' while the hive decides.');
      else if (state === 'failed') lines.push('Still at ' + stageNow + (a.progress ? ' — ' + stageTrackerActedPlain(a.progress) : ' — try again, or act on GitHub') + '.');
      else if (state === 'executing') lines.push('At ' + stageNow + ' — the hive is carrying it out' + (a.progress ? ': ' + stageTrackerActedPlain(a.progress) : '') + '.');
      else if (state === 'blocked') lines.push('Blocked at ' + stageNow + (a.progress ? ' — ' + stageTrackerActedPlain(a.progress) : ' — the hive lacks a permission it needs') + '.');
      else lines.push('At ' + stageNow + (waitingOn ? ' — ' + waitingOn : '') + '.');
      lines.push(who + ' chose ' + label + ' · ' + agentNowClockTime(atMs, nowMs) + ' (' + ago + ') · ' + stageTrackerActedOutcomeWords(a) + '.');
      if (a.progress && state !== 'executing' && state !== 'blocked' && state !== 'failed') lines.push('Hive now: ' + stageTrackerActedPlain(a.progress) + '.');
      if (a.human) lines.push('You can: ' + a.human);
      else if (state === 'executing') lines.push('Nothing for you to do — the badge updates when the adoption lands.');
      if (a.note) lines.push('Your note: ' + a.note);
      const options = stageTrackerActedOptions(src.item, a, state);
      if (options) lines.push('Options: ' + options + '.');
      const followed = Array.isArray(a.followed) ? a.followed : [];
      followed.slice(-3).forEach(h => {
        const hopMs = Date.parse(h.at || '') || nowMs;
        if (!(Number(h.number) > 0) && h.slug) lines.push('Then the hive ' + stageTrackerActedPlain(h.slug) + (h.reason ? ' — ' + h.reason : '') + ' · ' + stageTrackerActedAgo(nowMs - hopMs) + '.');
        else lines.push('Then the hive ' + (h.regressed ? 'sent it back to ' : 'moved it to ') + stageTrackerActedStageName(h.number) + (h.reason ? ' — ' + stageTrackerActedPlain(h.reason) : '') + ' · ' + stageTrackerActedAgo(nowMs - hopMs) + '.');
      });
      if (src.count > 1) lines.push((src.count - 1) + ' earlier action' + (src.count > 2 ? 's' : '') + ' on this item.');
      return lines.join('\n');
    }
    // Pure: the badge HTML ('' when the row has no recorded action). An
    // in-app tooltip (the shared .config-info portal: hover or focus),
    // never a native title.
    function stageTrackerActedBadgeHtml(item, nowMs) {
      const src = stageTrackerActedSource(item);
      if (!src) return '';
      // While this client is still waiting on its own decision the pill's
      // "Hive deciding…" caption is the one cue (#15126); a server-side
      // pending action still wears the dotted history badge (#15200).
      if (typeof stageTrackerRowDeciding === 'function' && (stageTrackerRowDeciding(stageTrackerItemKey(src.item)) || stageTrackerRowDeciding(stageTrackerItemKey(item)))) return '';
      let state = stageTrackerActedState(src.action, nowMs);
      const dec = src.item.decided || item.decided;
      if (dec) state = dec.effect === 'timed-out' ? 'stuck' : (dec.effect === 'landed' ? 'addressed' : 'waiting');
      const id = 'ham-acted-tip-' + String((src.item.repo || '') + '-' + (Number(src.item.number) || 0)).replace(/[^A-Za-z0-9_-]/g, '-');
      const tip = stageTrackerActedTip(item, nowMs);
      const short = dec ? (dec.word || stageTrackerActedLabel(src.action.action)) + ' — ' + (dec.verdict || '') : stageTrackerActedLabel(src.action.action);
      const n = Math.max(1, Number(src.count) || 1);
      const label = 'History: ' + n + ' action' + (n === 1 ? '' : 's') + ' — ' + short + ' — ' + state;
      return `<span class="ham-badge ham-acted ham-acted-${esc(state)}${dec ? ' ham-acted-decided' : ''} config-info" tabindex="0" role="img" data-action="gh3" data-stop="1" data-ham-acted="${esc(state)}" data-ham-acted-count="${n}" aria-label="${esc(label)}" aria-describedby="${id}">${STAGE_TRACKER_ACTED_GLYPH[state] || STAGE_TRACKER_HISTORY_ICON}<span class="ham-acted-n" aria-hidden="true">${n}</span><span id="${id}" class="config-tooltip ham-acted-tip" role="tooltip">${esc(tip).replace(/\n/g, '<br>')}</span></span>`;
    }
    // Pure: the Decisions filter. awaiting = parked on a person with no
    // action recorded yet (or the last one failed / was overtaken without
    // moving it); acted = any action recorded; stuck = acted and still at
    // the same stage past the window.
    function stageTrackerActedMatches(item, mode, nowMs) {
      if (!mode || mode === 'any') return true;
      const src = stageTrackerActedSource(item);
      const state = src ? stageTrackerActedState(src.action, nowMs) : '';
      switch (mode) {
        case 'acted': return !!src;
        case 'stuck': return state === 'stuck';
        case 'awaiting': return !!stageTrackerDecisionTarget(item) && (!src || state === 'failed');
      }
      return true;
    }
