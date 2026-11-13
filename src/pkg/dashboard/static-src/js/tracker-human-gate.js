    /* ---- The human gate: one control per gated row ----------------------
       A row a person must settle used to say so three or four ways at once
       — a "Needs human · review" chevron word, a `sentinel` chip, a
       needs-decision pill, `Approve ▸` and `Let hive decide ▸` — and still
       offered no verb for some gates (a sentinel alert had nothing that
       confirmed it; a "needs decision" had nothing that decided). The
       maintainer on hivecommons/hive (2026-11-10): "I don't see a consistent
       way to settle each of these using the UI. Some are confusing, others
       have no controls to take action, others have too many."

       Now every gated row carries exactly ONE segmented pill:
         [👤 Needs human · <reasons>] [verb] [verb] [verb] [… more]
       The status segment names why (review / decision / sentinel / hold,
       comma-separated when they stack) and carries the reviewer's question
       or the sentinel rule as its hover and popover; the verb segments are
       the fixed set for those reasons (STAGE_TRACKER_GATE_VERBS), unioned
       and de-duplicated, at most STAGE_TRACKER_GATE_VISIBLE visible with the
       rest behind a `…` menu. "Needs human" is never a chevron word: the lit
       chevron is the row's real stage with the amber ham-human-gated outline
       as the bar's only cue. A secondary row of a shared PR renders no gate
       of its own; its "decision pending on #N" chip is the link to the row
       that does. Every verb runs the row menu's path (runRowAction: in-app
       modal, result toast, acting row); the pill reads "settling…" until the
       next snapshot. */
    const STAGE_TRACKER_GATE_WORD = 'Needs human';
    const STAGE_TRACKER_GATE_SETTLING = 'settling…';
    const STAGE_TRACKER_GATE_VISIBLE = 3;
    const STAGE_TRACKER_GATE_FOLD_QUERY = '(max-width: 1100px)';
    // Reason → verbs, in the order they stand on the pill. The same reason
    // always offers the same verbs (the maintainer's "consistent way").
    const STAGE_TRACKER_GATE_VERBS = {
      review: ['decide:approve', 'gate:request-changes', 'decide:delegate'],
      decision: ['gate:decide', 'decide:approve', 'decide:delegate'],
      sentinel: ['gate:confirm-sentinel', 'decide:delegate'],
      hold: ['release', 'decide:delegate'],
    };
    const STAGE_TRACKER_GATE_ORDER = ['review', 'decision', 'sentinel', 'hold'];
    const STAGE_TRACKER_GATE_PR_ONLY = ['decide:approve', 'gate:request-changes'];
    // The verb's label on the pill; "Approve" reads "Approve as-is" when the
    // gate is a decision (a GitHub approval of the head as it stands).
    const STAGE_TRACKER_GATE_LABELS = {
      'decide:approve': 'Approve', 'gate:request-changes': 'Request changes…', 'decide:delegate': 'Let hive decide',
      'gate:decide': 'Decide…', 'gate:confirm-sentinel': 'Confirm safe', release: 'Release hold',
    };
    const STAGE_TRACKER_GATE_APPROVE_AS_IS = 'Approve as-is';
    // What each verb posts, for the pill hover and the docs table.
    const STAGE_TRACKER_GATE_POSTS = {
      'decide:approve': 'Posts a GitHub approval (/lgtm + /approve) on the current head.',
      'gate:request-changes': 'Posts your text as "/hive decision …" — the hive pushes a new head.',
      'decide:delegate': 'The hive decides for you under its policy and rails; recorded as your decision.',
      'gate:decide': 'Answers the question as "/hive decision …" — the hive carries on with your answer.',
      'gate:confirm-sentinel': 'Comments your confirmation and removes the sentinel label.',
      release: 'Removes the hold label; the lane may merge.',
    };

    // Pure: whether the row is a PR (its own, or an issue drawn through its
    // one covering PR) — the gate is a PR matter except for a parked issue.
    function stageTrackerGateIsPR(item) {
      return !!item && (item.via_pr ? 'pr' : item.kind) === 'pr';
    }
    // Pure: the reasons a person is owed on the row, each with the text the
    // status segment explains itself with. [] when nobody is owed anything.
    //   review   — the reviewer's verdict requires a human (review_activity
    //              human_decision on this head); tip = the reviewer's question
    //   decision — stage/085 (PR) or 045 (issue), or a needs-decision /
    //              needs-human modifier; tip = the decision question
    //   sentinel — a blocking sentinel alert; tip = the rule and paths
    //   hold     — a hold label; tip = who held it and why
    function stageTrackerHumanGateReasons(item) {
      if (!item || item.state === 'closed' || item.superseded || item.superseded_by || item.adopted_by) return [];
      if (Number(item.stage) >= (typeof STAGE_TRACKER_TERMINAL === 'number' ? STAGE_TRACKER_TERMINAL : 110)) return [];
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.indexOf('merged') >= 0 || mods.indexOf('draft') >= 0) return [];
      const out = [];
      const pr = stageTrackerGateIsPR(item);
      const onHead = a => !!a && (!a.head || !item.head || String(a.head) === String(item.head));
      const review = pr && onHead(item.review_activity) ? item.review_activity : null;
      if (review && review.state === 'human_decision') {
        out.push({ key: 'review', tip: String(review.blocker || review.detail || review.caption || '').trim() || 'A reviewer asked for a person\'s sign-off.' });
      }
      const stage = Number(item.stage) || 0;
      const prDecision = typeof STAGE_TRACKER_NEEDS_DECISION === 'number' ? STAGE_TRACKER_NEEDS_DECISION : 85;
      const issueDecision = typeof STAGE_TRACKER_ISSUE_NEEDS_DECISION === 'number' ? STAGE_TRACKER_ISSUE_NEEDS_DECISION : 45;
      // A pre-045 issue still wearing 085 is parked on a person all the same.
      const decisionStage = pr ? stage === prDecision : (stage === issueDecision || stage === prDecision);
      // A DCO sign-off ask (dco-missing, hive-signoff-attribution) is the
      // author's move, named by the waiting-on pill; needs_human alone does
      // not make it a decision (hivecommons/hive#17789).
      const authorDCO = /^(dco-missing|hive-signoff-attribution)$/.test(String(item.reason || '').trim());
      const flagged = decisionStage || mods.indexOf('needs-decision') >= 0 || mods.indexOf('needs-human') >= 0 || (item.needs_human === true && !authorDCO);
      // An unauthorized step and a structured blocker name the person owed
      // in their own pills (hive-bot-lacks-permission, Blocked · …); they
      // are not a decision anyone is asked for.
      if (flagged && !String(item.unauthorized_step || '').trim() && !(item.blocker && String(item.blocker.short || '').trim())) {
        const d = item.decision_needed || {};
        const attention = typeof stageTrackerHumanAttentionOf === 'function' ? stageTrackerHumanAttentionOf(item) : null;
        const ask = typeof stageTrackerNeedsHumanAsk === 'function' ? stageTrackerNeedsHumanAsk(item) : null;
        const short = typeof stageTrackerNeedsHumanShort === 'function' ? stageTrackerNeedsHumanShort(item) : '';
        const tip = String(d.question || (attention && (attention.question || attention.reason)) || (ask && (ask.question || ask.reason)) || short || '').trim();
        // The review gate already carries the reviewer's question; the same
        // question is not a second reason.
        if (!(review && review.state === 'human_decision' && out.length && (!tip || out[0].tip === tip))) out.push({ key: 'decision', tip: tip || 'A person\'s decision is owed.' });
      }
      if (item.sentinel_alert && !item.sentinel_notice) {
        out.push({ key: 'sentinel', tip: typeof stageTrackerSentinelTipWithPaths === 'function' ? stageTrackerSentinelTipWithPaths(item) : (typeof stageTrackerSentinelTip === 'function' ? stageTrackerSentinelTip(item) : 'Sentinel alert') });
      }
      if (item.hold || mods.indexOf('hold') >= 0) {
        const tip = typeof stageTrackerHoldTip === 'function' ? stageTrackerHoldTip(item, Date.now()) : '';
        out.push({ key: 'hold', tip: tip || 'A hold label keeps the hive off this item until a person releases it.' });
      }
      out.sort((x, y) => STAGE_TRACKER_GATE_ORDER.indexOf(x.key) - STAGE_TRACKER_GATE_ORDER.indexOf(y.key));
      return out;
    }
    // A model reply's action id: `wf:` + the verb as the model declares it.
    const STAGE_TRACKER_GATE_MODEL_PREFIX = 'wf:';
    // Pure: the verbs for a set of reasons — the union in reason order,
    // de-duplicated, first label wins (so "Approve" on review + decision
    // reads "Approve", and "Approve as-is" when the decision stands alone).
    // modelVerbs are the bound workflow model's replies for the row's
    // state (hivecommons/hive#17620, RFC phase 4): they follow the built-in
    // verbs as `wf:<verb>`, each posting "/hive <verb>", skipping any whose
    // label a built-in verb already reads.
    function stageTrackerHumanGateVerbs(reasons, item, modelVerbs) {
      const ids = [];
      // A row with no PR has no head to approve or send back; those verbs
      // belong to PR rows (or an issue drawn through its covering PR).
      const onPR = item == null || stageTrackerGateIsPR(item);
      (reasons || []).forEach(r => (STAGE_TRACKER_GATE_VERBS[r.key] || []).forEach(id => { if (ids.indexOf(id) < 0 && (onPR || STAGE_TRACKER_GATE_PR_ONLY.indexOf(id) < 0)) ids.push(id); }));
      const onlyDecision = (reasons || []).length && reasons.every(r => r.key === 'decision');
      const out = ids.map(id => ({ id, label: id === 'decide:approve' && onlyDecision ? STAGE_TRACKER_GATE_APPROVE_AS_IS : STAGE_TRACKER_GATE_LABELS[id] || id, posts: STAGE_TRACKER_GATE_POSTS[id] || '' }));
      const taken = out.map(v => String(v.label).replace(/…$/, '').toLowerCase());
      (Array.isArray(modelVerbs) ? modelVerbs : []).forEach(verb => {
        const v = String(verb || '').trim();
        const id = STAGE_TRACKER_GATE_MODEL_PREFIX + v;
        if (!v || out.some(x => x.id === id) || taken.indexOf(v.toLowerCase()) >= 0) return;
        taken.push(v.toLowerCase());
        out.push({ id, label: typeof stageTrackerWorkflowReplyLabel === 'function' ? stageTrackerWorkflowReplyLabel(v) : v, posts: 'Posts "/hive ' + v + '" — a reply the repository\'s workflow model declares for this state; the hive applies the state\'s label when the model maps one.', model: true });
      });
      return out;
    }
    // Pure: the status segment's words — "Needs human · review, sentinel".
    function stageTrackerHumanGateStatusText(reasons) {
      return STAGE_TRACKER_GATE_WORD + (reasons && reasons.length ? ' · ' + reasons.map(r => r.key).join(', ') : '');
    }
    // Pure: the status segment's hover — one line per reason.
    function stageTrackerHumanGateTip(reasons) {
      return (reasons || []).map(r => r.key + ': ' + r.tip).join('\n');
    }
    // Pure: whether a verb is enabled for this viewer and why not. The
    // row's own lead actions (server-judged delegate, approve) are trusted
    // first; the rest follow the hold permission the endpoints share.
    function stageTrackerHumanGateVerbState(id, item, ctx, leads) {
      const lead = (leads || []).find(a => a && a.id === id);
      if (lead) return { enabled: lead.enabled !== false, why: lead.enabled === false ? String(lead.why || '') : '' };
      return typeof stageTrackerActionAllowed === 'function' ? stageTrackerActionAllowed(id, ctx) : { enabled: true, why: '' };
    }
    function stageTrackerHumanGateVerbHtml(item, v, state, more) {
      const ref = typeof stageTrackerRefOf === 'function' ? stageTrackerRefOf(item) : '';
      const tip = state.enabled ? v.posts : state.why;
      const attrs = `data-ham-act="${esc(v.id)}"${state.enabled ? '' : ' aria-disabled="true"'}${tip ? ` title="${esc(tip)}"` : ''} aria-label="${esc(v.label + (ref ? ' — ' + ref : ''))}"`;
      if (more) return `<button type="button" role="menuitem" class="ham-menu-item ham-lead-act ham-gate-act" ${attrs} tabindex="-1">${esc(v.label)}</button>`;
      return `<button type="button" class="ham-lead-act ham-gate-act" ${attrs}>${esc(v.label)}</button>`;
    }
    // How many verbs ride inline: three on the strip tier; one on the
    // stacked head (≤ 1100px, which includes the phone card) so the pill
    // stays one line and leaves the title its room — the rest fold behind
    // the … menu. Rows re-render when the viewport crosses the breakpoint.
    function stageTrackerHumanGateVisible() {
      return typeof window !== 'undefined' && typeof window.matchMedia === 'function' && window.matchMedia(STAGE_TRACKER_GATE_FOLD_QUERY).matches ? 1 : STAGE_TRACKER_GATE_VISIBLE;
    }
    if (typeof window !== 'undefined' && typeof window.matchMedia === 'function') {
      const mq = window.matchMedia(STAGE_TRACKER_GATE_FOLD_QUERY);
      if (mq && typeof mq.addEventListener === 'function') mq.addEventListener('change', () => { if (typeof renderStageTracker === 'function') renderStageTracker(); });
    }
    // Pure: the pill. '' when the row has no gate. The status segment is
    // the decision popover's trigger when the row carries a decision, else
    // a plain segment with the reasons as its hover.
    function stageTrackerHumanGateHtml(item, shown, nowMs, subRows, opts) {
      shown = shown || item;
      if (!item) return '';
      const reasons = stageTrackerHumanGateReasons(shown);
      if (!reasons.length) return '';
      // A secondary row of a shared PR (drawn under another issue's row,
      // hivecommons/hive#17333) never carries the gate: the primary row
      // does, and this row's only control is the chip that links to it.
      if (opts && opts.sharedUnder) {
        if (typeof stageTrackerItemKey !== 'function') return '';
        const under = Number(opts.sharedUnder);
        const lp = (Array.isArray(item.linked_prs) ? item.linked_prs : []).find(x => x && Number(x.drawn_under) === under) || (shown.via_pr ? { number: shown.number } : null);
        const prNum = Number(lp && lp.number) || 0;
        const row = stageTrackerItemKey({ repo: item.repo, kind: 'issue', number: under });
        const to = prNum ? stageTrackerItemKey({ repo: item.repo, kind: 'pr', number: prNum }) : row;
        return `<button type="button" class="ham-badge ham-decision-pending" data-ham-scroll-to="${esc(to)}" data-ham-scroll-row="${esc(row)}" title="${esc('The human gate is settled on the row of #' + under + ', which draws ' + (prNum ? 'PR #' + prNum : 'the PR') + '. Click to go there.')}">decision pending on #${prNum || under}</button>`;
      }
      const ctx = typeof stageTrackerActionCtx === 'function' ? stageTrackerActionCtx(item.repo) : null;
      if (ctx) { ctx.nowMs = typeof nowMs === 'number' ? nowMs : Date.now(); ctx.rows = typeof _stageTrackerRows !== 'undefined' ? _stageTrackerRows : null; }
      const leads = ctx && typeof stageTrackerLeadActionsOnRow === 'function' ? stageTrackerLeadActionsOnRow(item, ctx, subRows) : [];
      // The decision lives on another listed row: that row carries the
      // gate; this one only points at it.
      const passive = leads.find(a => a && a.passive);
      if (passive) return typeof stageTrackerLeadPillHtml === 'function' ? stageTrackerLeadPillHtml(item, passive) : '';
      // A gate owed on a PR drawn under this issue row renders on that sub-row.
      if (Array.isArray(subRows) && subRows.length && item.kind === 'issue' && !item.via_pr && !leads.length && subRows.some(s => stageTrackerHumanGateReasons(s).length)) return '';
      const workflows = opts && opts.workflows !== undefined ? opts.workflows : (typeof _stageTracker !== 'undefined' && _stageTracker ? _stageTracker.workflows : null);
      const modelVerbs = typeof stageTrackerWorkflowReplies === 'function' ? stageTrackerWorkflowReplies(shown, workflows) : [];
      const verbs = stageTrackerHumanGateVerbs(reasons, shown, modelVerbs);
      const text = stageTrackerHumanGateStatusText(reasons);
      const tip = stageTrackerHumanGateTip(reasons);
      const ico = typeof stageTrackerIcon === 'function' ? stageTrackerIcon('human', 'ham-ico ham-mod-ico') : '';
      const inner = `${ico}<span class="ham-mod-word">${esc(text)}</span>`;
      const decision = typeof stageTrackerDecisionOf === 'function' ? stageTrackerDecisionOf(item) : null;
      const status = decision && typeof stageTrackerDecisionTriggerHtml === 'function'
        ? stageTrackerDecisionTriggerHtml(decision, inner, tip, 'ham-gate-status ham-mod-ask')
        : `<span class="ham-gate-status ham-mod-ask" title="${esc(tip)}" aria-label="${esc(text + ' — ' + tip)}">${inner}</span>`;
      const visibleCount = stageTrackerHumanGateVisible();
      const shownVerbs = verbs.slice(0, visibleCount);
      const moreVerbs = verbs.slice(visibleCount);
      const acts = shownVerbs.map(v => stageTrackerHumanGateVerbHtml(item, v, stageTrackerHumanGateVerbState(v.id, shown, ctx, leads), false)).join('');
      // The menu's items wait in a <template> (no hidden buttons in the row;
      // the header-geometry harness hit-tests every button it finds) and are
      // stamped into the wrap the first time the … opens.
      const more = moreVerbs.length ? `<span class="ham-gate-more-wrap"><button type="button" class="ham-lead-act ham-gate-act ham-gate-more" data-ham-gate-more aria-haspopup="menu" aria-expanded="false" aria-label="More ways to settle this gate" title="${esc(moreVerbs.map(v => v.label).join(' · '))}">…</button><template class="ham-gate-menu-tpl"><div class="ham-menu ham-gate-menu" role="menu" aria-label="More ways to settle this gate" hidden>${moreVerbs.map(v => stageTrackerHumanGateVerbHtml(item, v, stageTrackerHumanGateVerbState(v.id, shown, ctx, leads), true)).join('')}</div></template></span>` : '';
      const key = typeof stageTrackerItemKey === 'function' ? stageTrackerItemKey(item) : '';
      return `<span class="ham-human-gate" data-ham-gate="${esc(key)}" data-ham-gate-reasons="${esc(reasons.map(r => r.key).join(' '))}" role="group" aria-label="${esc(text)}">${status}${acts}${more}</span>`;
    }
    // The pill's optimistic state after one of its verbs ran: the status
    // reads "settling…" until the next snapshot re-renders the row.
    function stageTrackerHumanGateSettling(button) {
      const gate = button && button.closest ? button.closest('.ham-human-gate') : null;
      if (!gate) return;
      gate.classList.add('ham-gate-settling');
      const word = gate.querySelector('.ham-gate-status .ham-mod-word');
      if (word) word.textContent = STAGE_TRACKER_GATE_SETTLING;
      gate.querySelectorAll('.ham-gate-act').forEach(b => b.setAttribute('aria-disabled', 'true'));
    }
    // The `…` menu: open / close on its button, close on outside click or
    // Escape, the same popover pattern the row's ⋯ menu uses.
    function stageTrackerWireHumanGate(el) {
      if (!el || el._hamGateWired) return;
      el._hamGateWired = true;
      const closeAll = () => el.querySelectorAll('.ham-gate-menu:not([hidden])').forEach(m => { m.hidden = true; const b = m.previousElementSibling; if (b) b.setAttribute('aria-expanded', 'false'); });
      el.addEventListener('click', e => {
        const b = e.target.closest ? e.target.closest('[data-ham-gate-more]') : null;
        if (!b) { if (!(e.target.closest && e.target.closest('.ham-gate-menu'))) closeAll(); return; }
        e.preventDefault(); e.stopPropagation();
        let menu = b.nextElementSibling;
        if (menu && menu.tagName === 'TEMPLATE') { const stamped = menu.content.firstElementChild.cloneNode(true); menu.replaceWith(stamped); menu = stamped; }
        const open = menu && menu.hidden;
        closeAll();
        if (menu && open) { menu.hidden = false; b.setAttribute('aria-expanded', 'true'); const first = menu.querySelector('[role="menuitem"]'); if (first) first.focus(); }
      });
      el.addEventListener('keydown', e => { if (e.key === 'Escape') closeAll(); });
    }
