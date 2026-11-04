    // A requirement is not an available action or the kind of a claim holder.
    // Only the finished server snapshot supplies human_attention; its identity
    // must still match the PR this row actually represents.
    function stageTrackerHumanAttentionOf(item) {
      if (!item || item.advisory || item.hold || item.state === 'closed' || Number(item.stage) >= STAGE_TRACKER_TERMINAL
        || item.superseded_by || item.adopted_by) return null;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.some(m => m === 'draft' || m === 'hold' || m === 'merged')) return null;
      const a = item.human_attention;
      if (!a || a.kind !== item.kind || Number(a.number) !== Number(item.number)) return null;
      if (a.kind === 'pr' && String(a.head || '') !== String(item.head || '')) return null;
      return a;
    }
    // Pure: the decision a row carries itself when the server supplies no
    // human_attention — a 045 issue or 085 PR with decision_needed, or a
    // needs-human flag with a blocker (hivecommons/docs#612, rationguard#480).
    // Same shape as human_attention ({reason, asked_by, question, synthetic})
    // so the note line reads the same either way. null when the row is not
    // parked on a person, or when human_attention already says it.
    function stageTrackerNeedsHumanAsk(item) {
      if (!item || item.advisory || item.hold || item.state === 'closed' || Number(item.stage) >= STAGE_TRACKER_TERMINAL
        || item.superseded_by || item.adopted_by) return null;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.some(m => m === 'draft' || m === 'hold' || m === 'merged')) return null;
      if (stageTrackerHumanAttentionOf(item)) return null;
      const n = Number(item.stage) || 0;
      const flagged = item.needs_human === true || mods.indexOf('needs-human') >= 0 || mods.indexOf('needs-decision') >= 0
        || n === (typeof STAGE_TRACKER_NEEDS_DECISION === 'number' ? STAGE_TRACKER_NEEDS_DECISION : 85)
        || n === (typeof STAGE_TRACKER_ISSUE_NEEDS_DECISION === 'number' ? STAGE_TRACKER_ISSUE_NEEDS_DECISION : 45);
      if (!flagged) return null;
      const d = item.decision_needed || {};
      const b = item.blocker || {};
      const actor = item.stage_actor || {};
      const question = String(d.question || '').trim();
      const why = String(actor.why || '').trim();
      const whyClause = why.indexOf(' — ') >= 0 ? why.slice(why.indexOf(' — ') + 3).trim() : '';
      // Only evidence the server put on the row counts — a parked stage
      // alone synthesizes nothing (the row may be mid-refresh or hive-owned).
      const reason = question || String(b.detail || '').trim() || String(d.blocker || '').trim() || String(b.short || '').trim() || (actor.kind === 'human' ? whyClause : '');
      if (!reason) return null;
      return { kind: item.kind, number: Number(item.number) || 0, reason, question, asked_by: String(d.asked_by || actor.login && actor.kind === 'hive' && 'the hive' || '').trim() || 'the hive', synthetic: true };
    }
    // Pure: "Needs human: <short> — <first sentence of the reason>", the
    // short reason first so the row names the blocker before explaining it.
    // A reason that already starts with the short form is not repeated.
    // whole: keep the whole reason (a decision question is the ask itself).
    function stageTrackerNeedsHumanReasonText(item, reason, whole) {
      const full = String(reason || '').trim();
      const short = typeof stageTrackerNeedsHumanShort === 'function' ? stageTrackerNeedsHumanShort(item) : '';
      if (!short || full.toLowerCase().indexOf(short.toLowerCase()) === 0) return full;
      const m = whole ? null : full.match(/^[\s\S]*?[.!?](?=\s|$)/);
      const first = m ? m[0].trim() : full;
      return first ? short + ' — ' + first : short;
    }
    // Pure: the label of the row's lead action, when a lifted copy has one
    // (tracker-actions). '' without one, or when the lead is the optional
    // delegation itself — that is not what the person is asked to do.
    function stageTrackerNeedsHumanLeadLabel(item, subRows) {
      if (typeof stageTrackerLeadActionOnRow !== 'function' || typeof stageTrackerActionCtx !== 'function') return '';
      const ctx = stageTrackerActionCtx(item.repo);
      if (!ctx) return '';
      ctx.rows = typeof _stageTrackerRows !== 'undefined' ? _stageTrackerRows : null;
      const action = stageTrackerLeadActionOnRow(item, ctx, subRows);
      if (!action || !action.label || action.id === 'decide:delegate' || action.id === 'decide:delegate-all') return '';
      return String(action.label).replace(/\s*[▸›]\s*$/, '');
    }
    // Pure: the one open covering PR a person other than the hive is pushing
    // for this issue, when exactly one covers it. null otherwise.
    function stageTrackerNeedsHumanPusher(item) {
      if (!item || item.kind !== 'issue') return null;
      if (typeof stageTrackerLinkedPROpen !== 'function') return null;
      const open = (Array.isArray(item.linked_prs) ? item.linked_prs : []).filter(lp => lp && stageTrackerLinkedPROpen(lp, item));
      if (open.length !== 1) return null;
      const lp = open[0];
      const human = (lp.author_kind || '') === 'human' || (lp.author_kind || '') === 'maintainer';
      return human && lp.author ? lp : null;
    }
    function stageTrackerHumanAttentionLine(item, subRows) {
      const a = stageTrackerHumanAttentionOf(item) || stageTrackerNeedsHumanAsk(item);
      if (!a) return '';
      const reason = stageTrackerNeedsHumanReasonText(item, String(a.reason || '').trim() || 'Human decision required; reason unavailable', !!(a.synthetic && a.question));
      const parts = ['Needs human: ' + reason];
      if (a.owner) parts.push('next move: @' + a.owner);
      if (a.asked_by) parts.push((a.synthetic ? 'asked by ' : 'requested by ') + a.asked_by);
      const lead = stageTrackerNeedsHumanLeadLabel(item, subRows);
      if (lead) parts.push('lead: ' + lead + ' ▸');
      const pusher = stageTrackerNeedsHumanPusher(item);
      if (pusher) parts.push('@' + pusher.author + ' is pushing it in #' + Number(pusher.number));
      return parts.join(' · ');
    }
    function stageTrackerHumanRequirementText(item, subRows) {
      if (!item || item.advisory || item.hold || item.state === 'closed' || Number(item.stage) >= STAGE_TRACKER_TERMINAL || item.superseded_by || item.adopted_by ||
        (item.modifiers || []).some(m => m === 'draft' || m === 'hold' || m === 'merged')) return '';
      const human = stageTrackerHumanAttentionLine(item, subRows);
      const d = item && item.decided;
      const review = d && d.current_blocker && String(d.head || '') === String(item.head || '')
        ? 'Current blocker: ' + d.current_blocker + ' · operator approval recorded separately, not review approval or merge readiness' : '';
      return [human, review].filter(Boolean).join(' · ');
    }
    function stageTrackerHumanContextOf(item, stages, subRows) {
      if (!item) return [];
      const own = stageTrackerHumanRequirementText(item, subRows);
      // An issue's own ask is prefixed with its number, like a PR's line,
      // so a row whose strip a covering PR drives still names the asker.
      const asks = item.kind === 'issue' && !!(stageTrackerHumanAttentionOf(item) || stageTrackerNeedsHumanAsk(item));
      const out = own ? [{ item, text: (asks ? '#' + Number(item.number) + ' — ' : '') + own }] : [];
      if (item.kind !== 'issue') return out;
      // Include external drawn_under references, but never walk RFC ancestry
      // or assign one child's requirement to the issue / its other PRs.
      for (const lp of (Array.isArray(item.linked_prs) ? item.linked_prs : [])) {
        if (!stageTrackerLinkedPROpen(lp, item)) continue;
        const pr = stageTrackerLinkedPRItem(item, lp);
        const text = stageTrackerHumanRequirementText(pr);
        if (!text) continue;
        const drawn = stageTrackerLinkedSorted(item, stages);
        if (drawn.length > 1 && drawn.some(p => Number(p.number) === Number(lp.number))) continue;
        const where = lp.drawn_under && Number(lp.drawn_under) !== Number(item.number)
          ? ' · shown under issue #' + lp.drawn_under : '';
        out.push({ item: pr, text: 'PR #' + pr.number + ' — ' + text + where });
      }
      return out;
    }
    // A human-requirement line is a hive note: it opens, then tucks into
    // its ⬡ icon like the "Hive: …" line (stageTrackerHumanNoteHtml,
    // tracker-motion). Lifted copies without tracker-motion (tests,
    // previews) render the plain row instead.
    function stageTrackerHumanContextLine(item, bodyHtml, text, nowMs) {
      if (typeof stageTrackerHumanNoteHtml === 'function' && typeof stageTrackerItemKey === 'function') {
        return stageTrackerHumanNoteHtml(stageTrackerItemKey(item), bodyHtml, text, nowMs);
      }
      return `<div class="cue-event ham-human-context"><span>${bodyHtml}</span></div>`;
    }
    /* ---- One note line per row -----------------------------------------
       Owner (2026-11-05): "we don't need 2 hive comments — there should
       always be only one." A row used to stack its "Needs human: …" line
       over the "Hive: moved to …" stage note. Now every candidate is
       ranked — needs-human / decision (0) > blocker (1) > next move /
       claim (2) > optional delegation (3) > stage-transition note (4) —
       and the row renders the winner alone. Same-rank human asks (an
       issue's and its PR's) share the line, " · " apart, each with its
       View link. A lower line's facts ride along as a trailing clause
       only when they say something the row does not already: a stage
       note that merely restates the lit chevron ("moved to Needs human
       decision" under a strip lit at Needs human decision) is dropped. */
    const STAGE_TRACKER_NOTE_RANK_HUMAN = 0;
    const STAGE_TRACKER_NOTE_RANK_BLOCKER = 1;
    const STAGE_TRACKER_NOTE_RANK_NEXT = 2;
    const STAGE_TRACKER_NOTE_RANK_OPTIONAL = 3;
    // Mod-change words (tracker-motion) a "Needs human:" line already says.
    const STAGE_TRACKER_NOTE_HUMAN_RESTATES = ['waiting on a person', 'waiting on a decision'];
    function stageTrackerNoteRankOf(text) {
      const t = String(text || '');
      if (/(^|— )Needs human:|decision context unavailable/.test(t)) return STAGE_TRACKER_NOTE_RANK_HUMAN;
      if (/Current blocker:/.test(t)) return STAGE_TRACKER_NOTE_RANK_BLOCKER;
      if (/^Optional delegation:/.test(t)) return STAGE_TRACKER_NOTE_RANK_OPTIONAL;
      return STAGE_TRACKER_NOTE_RANK_NEXT;
    }
    // Pure: the candidate human lines for the row, each { body, text, rank }.
    function stageTrackerHumanContextEntries(item, stages, nowMs, subRows) {
      const required = stageTrackerHumanContextOf(item, stages, subRows);
      const entry = (body, text) => ({ body, text, rank: stageTrackerNoteRankOf(text) });
      const lines = required.map(e => {
        const it = e.item;
        const href = it.url || item.url;
        const view = href ? 'View ' + (it.kind === 'pr' ? 'PR' : 'issue') + ' #' + Number(it.number) : '';
        return entry(esc(e.text) + (view ? ` <a href="${esc(href)}" target="_blank" rel="noopener">${view}</a>` : ''), e.text + (view ? ' · ' + view : ''));
      });
      if (lines.length || !item || item.advisory || item.hold || item.state === 'closed') return lines;
      const stale = item.human_attention && !stageTrackerHumanAttentionOf(item);
      const staleLinks = item.kind === 'issue' ? (item.linked_prs || []).filter(lp =>
        stageTrackerLinkedPROpen(lp, item) && lp.human_attention && !stageTrackerHumanAttentionOf(stageTrackerLinkedPRItem(item, lp))) : [];
      if (stale || staleLinks.length) {
        const refs = staleLinks.length ? staleLinks.map(lp => 'PR #' + lp.number).join(', ') : item.kind === 'pr' ? 'PR #' + item.number : 'Issue #' + item.number;
        const text = refs + ' · decision context unavailable for the displayed identity/head';
        return [entry(esc(text) + (item.url ? ` · <a href="${esc(item.url)}" target="_blank" rel="noopener">View current item</a>` : ''), text)];
      }
      const linked = item.kind === 'issue' ? stageTrackerLinkedSorted(item, stages) : [];
      if (linked.length > 1 || item.kind === 'issue' && (item.linked_prs || []).some(lp =>
        stageTrackerLinkedPROpen(lp, item) && stageTrackerHumanAttentionOf(stageTrackerLinkedPRItem(item, lp)))) return [];
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.some(m => m === 'draft' || m === 'hold' || m === 'merged') || item.superseded_by) return [];
      const actor = item.stage_actor;
      const actorHuman = actor && (actor.kind === 'human' || actor.kind === 'maintainer');
      const claimant = item.claimant;
      if (actorHuman) {
        // A person doing author/claim work is not automatically a decision
        // gate. Keep the actual actor and their full current explanation.
        const who = actor.login ? '@' + actor.login : 'a person';
        const prNumber = item.kind === 'issue' && (item.linked_pr || ((item.linked_prs || []).length === 1 && item.linked_prs[0].number));
        const target = prNumber ? ' on PR #' + prNumber : '';
        const text = 'Next move: ' + who + target + (actor.why ? ' — ' + actor.why : ' · action reason unavailable');
        lines.push(entry(esc(text), text));
      } else if (claimant && claimant.kind === 'person' && claimant.login) {
        const text = 'Claimed by @' + claimant.login + ' · claim ownership, not a human decision request';
        lines.push(entry(esc(text), text));
      }
      if (typeof stageTrackerLeadActionOnRow === 'function' && typeof stageTrackerActionCtx === 'function') {
        const ctx = stageTrackerActionCtx(item.repo);
        if (ctx) {
          ctx.nowMs = nowMs;
          ctx.rows = typeof _stageTrackerRows !== 'undefined' ? _stageTrackerRows : null;
          const action = stageTrackerLeadActionOnRow(item, ctx, subRows);
          if (action && (action.id === 'decide:delegate' || action.id === 'decide:delegate-all')) {
            const text = 'Optional delegation: Let hive decide is available; it does not itself mean human input is required.';
            lines.push(entry(esc(text), text));
          }
        }
      }
      return lines;
    }
    // Pure: the row's current stage as the chevron names it ('' unknown).
    function stageTrackerNoteStageName(item, stages) {
      if (!item) return '';
      const sg = (Array.isArray(stages) ? stages : []).find(x => Number(x.number) === (Number(item.stage) || 0));
      if (typeof stageTrackerStageName === 'function') return stageTrackerStageName(sg) || stageTrackerStageName({ label: item.stage_label || '', short: '' });
      return sg ? String(sg.short || sg.label || '') : String(item.stage_label || '');
    }
    // Pure: the stage note's words worth keeping under a human line, or
    // '' when the line (or the lit chevron) already says it.
    function stageTrackerNoteFoldText(evText, lineText, item, stages) {
      const t = String(evText || '').trim();
      if (!t) return '';
      const line = String(lineText || '').toLowerCase();
      if (line.indexOf(t.toLowerCase()) >= 0) return '';
      if (/needs human:/i.test(line) && STAGE_TRACKER_NOTE_HUMAN_RESTATES.indexOf(t) >= 0) return '';
      const m = /^(?:moved to|fell back to) (.+)$/.exec(t);
      if (m) {
        const name = stageTrackerNoteStageName(item, stages).toLowerCase();
        const said = m[1].split(' — ')[0].trim().toLowerCase();
        if (name && said === name && m[1].indexOf(' — ') < 0) return '';
      }
      return t;
    }
    // ctx / key (the row's events map and its key) let the human line
    // absorb the row's stage note so the row shows one line, not two.
    function stageTrackerHumanContextHtml(item, stages, nowMs, subRows, ctx, key) {
      let entries = stageTrackerHumanContextEntries(item, stages, nowMs, subRows);
      if (!entries.length) return '';
      const best = Math.min.apply(null, entries.map(e => e.rank));
      // "Let hive decide is available" is a control the row shows anyway;
      // it only earns the line when nothing else wants it.
      if (best < STAGE_TRACKER_NOTE_RANK_OPTIONAL) entries = entries.filter(e => e.rank < STAGE_TRACKER_NOTE_RANK_OPTIONAL);
      entries.sort((a, b) => a.rank - b.rank);
      let body = entries.map(e => e.body).join(' · ');
      let text = entries.map(e => e.text).join(' · ');
      const ev = ctx && ctx.events && key ? ctx.events[key] : null;
      const fold = ev ? stageTrackerNoteFoldText(ev.text, text, item, stages) : '';
      if (fold) {
        const who = ev.who === 'you' ? 'You: ' : ev.who === 'merge' ? 'Merge: ' : '';
        body += ' · ' + esc(who + fold); text += ' · ' + who + fold;
      }
      return stageTrackerHumanContextLine(item, body, text, nowMs);
    }
    // Pure: ctx without the row's stage note once the human line has
    // taken it (folded or dropped), so the event slot stays empty.
    function stageTrackerNoteTakenCtx(ctx, key) {
      if (!ctx || !ctx.events || !key || !ctx.events[key]) return ctx;
      const events = Object.assign({}, ctx.events);
      delete events[key];
      return Object.assign({}, ctx, { events });
    }
