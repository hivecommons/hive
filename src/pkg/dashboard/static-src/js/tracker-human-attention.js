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
    function stageTrackerHumanAttentionLine(item) {
      const a = stageTrackerHumanAttentionOf(item);
      if (!a) return '';
      const reason = String(a.reason || '').trim() || 'Human decision required; reason unavailable';
      const parts = ['Needs human: ' + reason];
      if (a.owner) parts.push('next move: @' + a.owner);
      if (a.asked_by) parts.push('requested by ' + a.asked_by);
      return parts.join(' · ');
    }
    function stageTrackerHumanRequirementText(item) {
      if (!item || item.advisory || item.hold || item.state === 'closed' || Number(item.stage) >= STAGE_TRACKER_TERMINAL || item.superseded_by || item.adopted_by ||
        (item.modifiers || []).some(m => m === 'draft' || m === 'hold' || m === 'merged')) return '';
      const human = stageTrackerHumanAttentionLine(item);
      const d = item && item.decided;
      const review = d && d.current_blocker && String(d.head || '') === String(item.head || '')
        ? 'Current blocker: ' + d.current_blocker + ' · operator approval recorded separately, not review approval or merge readiness' : '';
      return [human, review].filter(Boolean).join(' · ');
    }
    function stageTrackerHumanContextOf(item, stages) {
      if (!item) return [];
      const own = stageTrackerHumanRequirementText(item);
      const out = own ? [{ item, text: own }] : [];
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
    function stageTrackerHumanContextHtml(item, stages, nowMs, subRows) {
      const required = stageTrackerHumanContextOf(item, stages);
      const lines = required.map(entry => {
        const it = entry.item;
        const href = it.url || item.url;
        const view = href ? 'View ' + (it.kind === 'pr' ? 'PR' : 'issue') + ' #' + Number(it.number) : '';
        const body = esc(entry.text) + (view ? ` <a href="${esc(href)}" target="_blank" rel="noopener">${view}</a>` : '');
        return stageTrackerHumanContextLine(item, body, entry.text + (view ? ' · ' + view : ''), nowMs);
      });
      if (lines.length || !item || item.advisory || item.hold || item.state === 'closed') return lines.join('');
      const stale = item.human_attention && !stageTrackerHumanAttentionOf(item);
      const staleLinks = item.kind === 'issue' ? (item.linked_prs || []).filter(lp =>
        stageTrackerLinkedPROpen(lp, item) && lp.human_attention && !stageTrackerHumanAttentionOf(stageTrackerLinkedPRItem(item, lp))) : [];
      if (stale || staleLinks.length) {
        const refs = staleLinks.length ? staleLinks.map(lp => 'PR #' + lp.number).join(', ') : item.kind === 'pr' ? 'PR #' + item.number : 'Issue #' + item.number;
        const text = refs + ' · decision context unavailable for the displayed identity/head';
        return stageTrackerHumanContextLine(item, esc(text) + (item.url ? ` · <a href="${esc(item.url)}" target="_blank" rel="noopener">View current item</a>` : ''), text, nowMs);
      }
      const linked = item.kind === 'issue' ? stageTrackerLinkedSorted(item, stages) : [];
      if (linked.length > 1 || item.kind === 'issue' && (item.linked_prs || []).some(lp =>
        stageTrackerLinkedPROpen(lp, item) && stageTrackerHumanAttentionOf(stageTrackerLinkedPRItem(item, lp)))) return '';
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.some(m => m === 'draft' || m === 'hold' || m === 'merged') || item.superseded_by) return '';
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
        lines.push(stageTrackerHumanContextLine(item, esc(text), text, nowMs));
      } else if (claimant && claimant.kind === 'person' && claimant.login) {
        const text = 'Claimed by @' + claimant.login + ' · claim ownership, not a human decision request';
        lines.push(stageTrackerHumanContextLine(item, esc(text), text, nowMs));
      }
      if (typeof stageTrackerLeadActionOnRow === 'function' && typeof stageTrackerActionCtx === 'function') {
        const ctx = stageTrackerActionCtx(item.repo);
        if (ctx) {
          ctx.nowMs = nowMs;
          ctx.rows = typeof _stageTrackerRows !== 'undefined' ? _stageTrackerRows : null;
          const action = stageTrackerLeadActionOnRow(item, ctx, subRows);
          if (action && (action.id === 'decide:delegate' || action.id === 'decide:delegate-all')) {
            const text = 'Optional delegation: Let hive decide is available; it does not itself mean human input is required.';
            lines.push(stageTrackerHumanContextLine(item, esc(text), text, nowMs));
          }
        }
      }
      return lines.join('');
    }
