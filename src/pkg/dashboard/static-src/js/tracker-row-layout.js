
    // Ghost strip for a title line: the lead cell (Open..Claimed) carries
    // the item's own number + title, the PR cell (PR in progress..Merge gate)
    // the given refs, and the cap cell the modifier chip, bottom-aligned so
    // it sits on the last title line, directly over the strip's right end.
    // The since column is an empty spacer here (same width as the strip
    // row's, which carries the time on step): a wrapped title must not float
    // the age away from the chevrons it describes.
    function stageTrackerHeadColsHtml(prRefs, leadHtml, chipsHtml, prTargetKey) {
      const wide = !(prRefs && prRefs.length);
      return `<div class="ham-strip-row ham-head-cols"><ol class="ham-strip"><li class="ham-seg ham-head-lead${wide ? ' ham-head-wide' : ''}">${leadHtml || ''}</li>${wide ? '' : `<li class="ham-seg ham-head-pr"${prTargetKey ? ` data-ham-target-key="${esc(prTargetKey)}"` : ''}>${prRefs.join('')}</li>`}<li class="ham-seg cap"><span class="ham-badges">${chipsHtml || ''}</span></li></ol><span class="ham-since"></span></div>`;
    }

    function stageTrackerSinceHtml(item, nowMs) {
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      let sinceMs = Date.parse(item.stage_since || '');
      // A blocked row without stage_since still shows its age, from created_at.
      if (!sinceMs && mods.indexOf('blocked') >= 0) sinceMs = stageTrackerOpenSince(item).ms;
      const stalled = mods.indexOf('stalled') >= 0;
      return `<span class="ham-since${stalled ? ' ham-stalled' : ''}">${esc(sinceMs ? stageTrackerShortAgo(nowMs - sinceMs) : '')}</span>`;
    }

    const STAGE_TRACKER_LINKED_TIP = "linked PR, shown on its issue's row";
    // Issue numbers that list each covering PR, by "repo#pr", for the PRs
    // listed by more than one open issue (so a row can say "also under #N").
    function stageTrackerSharedPRs(items) {
      const by = {};
      (items || []).forEach(it => {
        if (!it || it.kind !== 'issue') return;
        (Array.isArray(it.linked_prs) ? it.linked_prs : []).forEach(lp => {
          const k = (it.repo || '') + '#' + (Number(lp.number) || 0);
          (by[k] = by[k] || []).push(Number(it.number) || 0);
        });
      });
      const out = {};
      Object.keys(by).forEach(k => { if (by[k].length > 1) out[k] = by[k].slice().sort((a, b) => a - b); });
      return out;
    }
    // Hover text for a PR drawn on its issue's row (it rides the PR link:
    // the row's hover budget allows no other titled element). Every row but
    // the lowest-numbered issue listing the PR also names where else it shows.
    function stageTrackerLinkedTip(issue, prNumber, ctx) {
      const all = (ctx && ctx.sharedPRs && ctx.sharedPRs[(issue.repo || '') + '#' + (Number(prNumber) || 0)]) || [];
      const others = all.filter(n => n !== Number(issue.number));
      const also = others.length && all[0] !== Number(issue.number) ? ' · also under #' + others.join(', #') : '';
      return STAGE_TRACKER_LINKED_TIP + also;
    }
    // "↳" ahead of such a PR, so the shared row is recognisable as the PR's
    // place in the list (aria-label only; no title of its own).
    const STAGE_TRACKER_LINKED_GLYPH = '<span class="ham-linked-ico" aria-label="linked PR">↳</span>';

    // A covering PR nested under its issue: "[author icon] #N title" at the
    // PR column, then the PR's own strip from PR in progress onwards. The
    // PR's full tooltip rides its link; the sub-row container has none.
    // Pure: a nested PR as a row item of its own — the object every row
    // action receives (runRowAction(action, row)), so a ⋯ menu or popover
    // on a sub-row acts on THAT PR: its number, repo and head, never the
    // parent issue's. `parent` names the issue; `siblings` the other open
    // PRs under it (the superseded-by prompt pre-fills the one sibling).
    function stageTrackerLinkedPRItem(issue, lp) {
      const open = Array.isArray(issue.linked_prs) ? issue.linked_prs.filter(x => x && x.number !== lp.number && stageTrackerLinkedPROpen(x, issue)) : [];
      const canonical = /^https?:\/\/[^/]+\/([^/]+\/[^/]+)\/pull\/\d+(?:[?#].*)?$/.exec(String(lp.url || ''));
      return { repo: canonical ? canonical[1] : (lp.repo || issue.repo), number: lp.number, kind: 'pr', title: lp.title || '', url: lp.url || '', author: lp.author || '', author_kind: lp.author_kind || '',
        state: lp.state || '', terminal_merge: !!lp.terminal_merge, merged_by: lp.merged_by || '',
        stage: lp.stage || 0, stage_since: lp.stage_since || '', stage_slug: lp.stage_slug || '', modifiers: lp.modifiers || [], deferred_modifiers: lp.deferred_modifiers || [], caption: lp.caption || '', reason: lp.reason || '', preparation: lp.preparation || null, human_attention: lp.human_attention || null,
        human_approver: lp.human_approver || '', manual_merge: !!lp.manual_merge, human_merge_reason: lp.human_merge_reason || '', human_merge_reason_text: lp.human_merge_reason_text || '', sentinel_alert: !!lp.sentinel_alert, sentinel_reason: lp.sentinel_reason || '', sentinel_notice: !!lp.sentinel_notice, head: lp.head || '', base_ref: lp.base_ref || '', decision_needed: lp.decision_needed || null,
        adoption: lp.adoption || null, takeover: lp.takeover || null, superseded: !!lp.superseded, supersedes: lp.supersedes || null, parent: Number(issue.number) || 0, siblings: open.map(x => Number(x.number) || 0),
        last_operator_action: lp.last_operator_action || null, operator_action_count: lp.operator_action_count || 0, hold: lp.hold || null, stage_actor: lp.stage_actor || null, claimant: lp.claimant || null, presence: lp.presence || null, rework: lp.rework || null, push: lp.push || null, merge_gate: lp.merge_gate || null, merge_train: lp.merge_train || null, left_train: lp.left_train || null, overlap: lp.overlap || null,
        decided: lp.decided || null, adopted_by: Number(lp.adopted_by) || 0, adopted_by_state: lp.adopted_by_state || '', adopted_from: lp.adopted_from || null, lineage: lp.lineage || null, state: lp.state || '', superseded_by: Number(lp.superseded_by) || 0 };
    }
    // Pure: whether a linked PR is still open (not merged / closed /
    // superseded). With `issue`, an adopted original whose hive copy is
    // listed under the same issue counts as superseded even while GitHub
    // still has it open (the write-back gate kept it at 072): the pair
    // {original, copy} is one logical PR, so #15495 never reads as "2
    // PRs" needing a human to pick (hivecommons/hive#15495).
    function stageTrackerLinkedPROpen(lp, issue) {
      if (!lp || lp.superseded || String(lp.state || '') === 'closed' || (Number(lp.stage) || 0) >= STAGE_TRACKER_TERMINAL || (Array.isArray(lp.modifiers) ? lp.modifiers : []).indexOf('merged') >= 0) return false;
      const by = Number(lp.superseded_by || lp.adopted_by) || 0;
      if (by > 0 && issue && lp.adopted_by_state !== 'closed' && lp.adopted_by_state !== 'closed-twice' && lp.adopted_by_state !== 'copy-failed') {
        const linked = Array.isArray(issue.linked_prs) ? issue.linked_prs : [];
        if (linked.some(x => x && Number(x.number) === by && String(x.state || '') !== 'closed')) return false;
      }
      return true;
    }
    // Pure: an issue's open nested PRs as row items.
    function stageTrackerNestedOpenPRs(issue) {
      if (!issue || issue.kind === 'pr') return [];
      return (Array.isArray(issue.linked_prs) ? issue.linked_prs : []).filter(lp => stageTrackerLinkedPROpen(lp, issue)).map(lp => stageTrackerLinkedPRItem(issue, lp));
    }
    // Lineage: where an adopted PR came from (`lineage`: {kind, from_number,
    // from_url, from_author, from_author_kind, to_number, to_url, at,
    // reason}; optional, nothing without it). #15198 showed the hive's copy
    // #15308 "Fixing" with no trace of @thatguywob's #15226 it was adopted
    // from, so sending the fix to a hive agent read as a mistake. The chip
    // sits beside the PR number on every PR mention (top-level row, single
    // covering PR, sub-row) and links to the other PR; the author's kind is
    // the icon only, never a word.
    const STAGE_TRACKER_LINEAGE_REASON_TIP = {
      'ci-red-contributor-offline': "Hive took this PR over after CI went red and @AUTHOR's agent didn't respond. Original commits kept.",
      'ci-red-contributor-unregistered': "Hive took this PR over after CI went red and @AUTHOR has no agent registered here. Original commits kept.",
      'operator-asked': 'Hive took this PR over at a maintainer\'s request. Original commits kept.',
      'dco-rehome': 'Hive re-homed this PR to repair its DCO sign-off. Original commits kept.',
      'signoff-rehome': 'Hive re-homed this PR so its sign-off survives a squash. Original commits kept.',
      'rebase-rehome': 'Hive re-homed this PR onto its own branch to finish the rebase. Original commits kept.',
      'push-rehome': "Hive re-homed this PR because @AUTHOR's branch could no longer be pushed. Original commits kept.",
      'in-place-stalled': 'Hive re-homed this PR after a review finding sat unaddressed. Original commits kept.'
    };
    // Pure: the chip's visible words — "adopted from @thatguywob's #15226 · 6h".
    function stageTrackerLineageText(item, nowMs) {
      const l = item && item.lineage;
      if (!l || !l.kind) return '';
      const ago = stageTrackerAgoText(Date.parse(l.at || ''), nowMs).replace(/ ago$/, '');
      // Once the head moves the chip says so instead of the adoption's age:
      // "adopted from @x's #N · pushed 2m ago" (hivecommons/hive#15784).
      const pushed = typeof stageTrackerPushLineageSuffix === 'function' ? stageTrackerPushLineageSuffix(item, nowMs) : '';
      const tail = pushed || (ago ? ' · ' + ago : '');
      if (l.kind === 'superseded') return Number(l.to_number) > 0 ? 'superseded by #' + Number(l.to_number) + tail : '';
      if (!(Number(l.from_number) > 0)) return '';
      const whose = l.from_author ? '@' + l.from_author + "'s " : '';
      return (l.kind === 'rebased-by-hive' ? 'rebased by hive from ' : 'adopted from ') + whose + '#' + Number(l.from_number) + tail;
    }
    // Pure: the hover — why the hive took it over, in plain words.
    function stageTrackerLineageTip(item) {
      const l = item && item.lineage;
      if (!l || !l.kind) return '';
      if (l.kind === 'superseded') return 'Hive took this PR over as #' + Number(l.to_number) + '; this original is closed when that merges.';
      const who = l.from_author ? '@' + l.from_author : 'the contributor';
      const tip = STAGE_TRACKER_LINEAGE_REASON_TIP[l.reason] || STAGE_TRACKER_LINEAGE_REASON_TIP['ci-red-contributor-offline'];
      return tip.split('@AUTHOR').join(who) + stageTrackerSupersededOriginalClause(Number(l.from_number), l.from_state);
    }
    // Pure: the hover clause that says what became of the superseded
    // original the chip stands in for — closed, or still open and closing
    // when the copy merges; '' when the server does not say.
    function stageTrackerSupersededOriginalClause(number, state) {
      if (!(number > 0)) return '';
      if (state === 'closed') return ' #' + number + ' is closed as superseded.';
      if (state === 'open') return ' #' + number + ' is superseded and still open; it closes when this merges.';
      return '';
    }
    function stageTrackerLineageChipHtml(item, nowMs) {
      const text = stageTrackerLineageText(item, nowMs);
      if (!text) return '';
      const l = item.lineage;
      const tip = stageTrackerLineageTip(item);
      const icon = l.kind !== 'superseded' && l.from_author_kind ? stageTrackerIcon(l.from_author_kind, 'ham-author-ico ham-author-' + esc(l.from_author_kind), l.from_author_kind) : '';
      const href = l.kind === 'superseded' ? l.to_url : l.from_url;
      const attrs = `class="ham-badge ham-lineage" title="${esc(tip)}" aria-label="${esc(text + ' — ' + tip)}"`;
      return href ? `<a ${attrs} href="${esc(href)}" target="_blank" rel="noopener">${icon}${esc(text)}</a>` : `<span ${attrs}>${icon}${esc(text)}</span>`;
    }
    // Pure: the one clause a caption or preview adds after the PR number —
    // " (adopted from #15226)"; '' without lineage. An issue row reads its
    // single covering PR's lineage.
    function stageTrackerLineageClause(item) {
      let l = item && item.lineage;
      if (!l && item && item.kind === 'issue' && Array.isArray(item.linked_prs) && item.linked_prs.length === 1) l = item.linked_prs[0].lineage;
      if (!l || !(Number(l.from_number) > 0) || l.kind === 'superseded') return '';
      return ' (adopted from #' + Number(l.from_number) + ')';
    }
    // Pure: the "↳ adopted from #N · you let hive decide 17:51" chip on the
    // hive's copy of an adopted PR (hivecommons/hive#15126).
    function stageTrackerAdoptedFromChipHtml(pr, nowMs) {
      const af = pr && pr.adopted_from;
      if (!af || !af.number) return '';
      let text = '↳ adopted from #' + af.number;
      if (af.decided_at) {
        const what = af.decided_action === 'let-hive-decide' ? 'you let hive decide' : 'you approved';
        text += ' · ' + what + ' ' + agentNowClockTime(Date.parse(af.decided_at), typeof nowMs === 'number' ? nowMs : Date.now());
      }
      const tip = 'Hive re-authored #' + af.number + (af.author ? ' (by @' + af.author + ')' : '') + ' as this PR with the same commits and sign-off' + (af.decided_by ? '; the decision on #' + af.number + ' was ' + (af.decided_action || 'yours') + ' by @' + af.decided_by : '') + '.' + (stageTrackerSupersededOriginalClause(Number(af.number), af.state) || ' #' + af.number + ' is closed as superseded when this merges.');
      return `<a class="ham-badge ham-adopted-from" href="${esc(af.url || '#')}" target="_blank" rel="noopener" title="${esc(tip)}">${esc(text)}</a>`;
    }
    // Pure: the reciprocal of the "↳ #N · under #M" chip (hivecommons/
    // hive#15806) — on the PR drawn here, "also closes #A, #B" naming the
    // other listed open issues it resolves, every number a link; past
    // three it reads "#A, #B, #C +N" and the hover names every title.
    function stageTrackerAlsoClosesChipHtml(lp) {
      const list = Array.isArray(lp && lp.also_closes) ? lp.also_closes.filter(li => li && Number(li.number) > 0) : [];
      if (!list.length) return '';
      const shown = list.slice(0, 3);
      const links = shown.map(li => `<a href="${esc(li.url || '#')}" target="_blank" rel="noopener">#${Number(li.number)}</a>`).join(', ');
      const more = list.length > shown.length ? ' +' + (list.length - shown.length) : '';
      const tip = 'this PR also resolves ' + list.map(li => '#' + Number(li.number) + (li.title ? ' (' + li.title + ')' : '')).join(', ');
      return `<span class="ham-badge ham-also-closes" title="${esc(tip)}">also closes ${links}${more}</span>`;
    }
    // Pure: the lineage-style chips of a covering PR drawn under an issue —
    // its lineage / adopted-from chip, then the also-closes chip; null = none.
    function stageTrackerLinkedChipsHtml(lp, lineage) {
      const also = typeof stageTrackerAlsoClosesChipHtml === 'function' ? stageTrackerAlsoClosesChipHtml(lp) : '';
      const out = [lineage || '', also].filter(Boolean).join(' ');
      return out || null;
    }
    // The row's inline lead action (tracker-actions.js
    // stageTrackerLeadActionHtml), computed against the viewer's own
    // permissions on the item's repo; '' where the actions module or a
    // viewer context is absent (the node harness lifts this file alone).
    // subRows: the linked PRs drawn as sub-rows under an issue row — a lead
    // that acts on one of them renders there, never on both rows.
    function stageTrackerLeadButtonHtml(item, nowMs, subRows) {
      if (typeof stageTrackerLeadActionHtml !== 'function' || typeof stageTrackerActionCtx !== 'function' || !item) return '';
      const ctx = stageTrackerActionCtx(item.repo);
      if (!ctx) return '';
      ctx.nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      // The rows this render draws, so a covered issue can defer to its PR's row.
      ctx.rows = typeof _stageTrackerRows !== 'undefined' ? _stageTrackerRows : null;
      return stageTrackerLeadActionHtml(item, ctx, subRows);
    }
    // PR link, then the lineage chip (null = none), then the base pill.
    function stageTrackerPRPillsHtml(ref, lineage, baseRef) {
      return ref + (lineage == null ? '' : ' ' + lineage) + (baseRef ? ` <span class="ham-badge">${esc(baseRef)}</span>` : '');
    }
    function stageTrackerSubRowHtml(lp, issue, stages, nowMs, ctx) {
      const pr = stageTrackerLinkedPRItem(issue, lp);
      if (typeof stageTrackerPreparationMark === 'function') stageTrackerPreparationMark(pr);
      const superseded = typeof stageTrackerLinkedSupersededText === 'function' ? stageTrackerLinkedSupersededText(lp, issue) : '';
      let ref = stageTrackerPRRef(pr.number, pr.url, pr.author_kind, pr.author, pr.title, (pr.title ? pr.title + ' · ' : '') + (superseded ? superseded + ' · ' + stageTrackerLinkedSupersededTip(lp) : stageTrackerTooltip(pr, nowMs)) + ' · ' + stageTrackerLinkedTip(issue, pr.number, ctx));
      // One line (hivecommons/hive: "consolidate from 3 lines of text to
      // 1"): connector, repo chip, author icon + #N + title, the lineage /
      // adopted-from chip, then the base pill — all siblings of the flex-row
      // PR cell; the title ellipsizes, the pills never shrink. A superseded
      // original wears neither pill — it closes as superseded, not as the
      // closer of anything.
      const lineage = superseded ? '' : ((typeof stageTrackerLineageChipHtml === 'function' ? stageTrackerLineageChipHtml(pr, nowMs) : '') || (typeof stageTrackerAdoptedFromChipHtml === 'function' ? stageTrackerAdoptedFromChipHtml(pr, nowMs) : ''));
      const chip = ctx && ctx.repoChips ? stageTrackerRepoChipHtml(pr.repo, ctx) : '';
      if (chip) ref = `<span class="ham-pr-line">${chip}${ref}</span>`;
      ref = stageTrackerPRPillsHtml(ref, !superseded && typeof stageTrackerLinkedChipsHtml === 'function' ? stageTrackerLinkedChipsHtml(lp, lineage) : (lineage || null), lp.base_ref);
      ref = STAGE_TRACKER_LINKED_GLYPH + ref;
      if (superseded) {
        // The adopted original: one muted line, no chevron track, no row
        // tools, no needs-human styling — the copy carries the chevron. The
        // trailing note never wraps; the title ellipsizes first (owner,
        // hivecommons/hive#15511: "superseded · hive adopted → #15574").
        return `<div class="ham-sub ham-sub-superseded" data-ham-kind="pr" data-ham-stage="${pr.stage}" data-ham-sub-key="${esc(stageTrackerItemKey(pr))}" data-ham-superseded-by="${Number(lp.superseded_by || lp.adopted_by) || 0}">
          <div class="ham-row-head">
            ${stageTrackerHeadColsHtml([ref + ` <span class="ham-superseded-note" title="${esc(stageTrackerLinkedSupersededTip(lp))}">${esc(superseded)}</span>`], '', stageTrackerActedBadgeHtml(pr, nowMs))}
          </div>
        </div>`;
      }
      return `<div class="ham-sub${typeof stageTrackerCueClasses === 'function' ? stageTrackerCueClasses(pr, ctx) : ''}${typeof stageTrackerPreparationRowClass === 'function' ? stageTrackerPreparationRowClass(pr, stages, ctx) : ''}" data-ham-kind="pr" data-ham-stage="${pr.stage}" data-ham-sub-key="${esc(stageTrackerItemKey(pr))}">
          ${typeof stageTrackerPreparationNodeHtml === 'function' ? stageTrackerPreparationNodeHtml(pr, stages, ctx) : ''}
          ${typeof stageTrackerRowToolsHtml === 'function' ? stageTrackerRowToolsHtml(pr) : ''}
          <div class="ham-row-head">
            ${stageTrackerHeadColsHtml([ref], '', stageTrackerActedBadgeHtml(pr, nowMs) + stageTrackerChipsHtml(pr) + (typeof stageTrackerLeadButtonHtml === 'function' ? stageTrackerLeadButtonHtml(pr, nowMs) : ''))}
          </div>
          <div class="ham-strip-row">
            <ol class="ham-strip" aria-label="Pipeline progress"${stageTrackerMoveAttrs(stageTrackerItemKey(pr), ctx)}>${stageTrackerStripItems(pr, stages, { from: STAGE_TRACKER_PR_COLUMN }, nowMs, ctx)}</ol>
            ${stageTrackerSinceHtml(pr, nowMs)}
          </div>
          ${typeof stageTrackerHumanContextHtml === 'function' ? stageTrackerHumanContextHtml(pr, stages, nowMs) : ''}${stageTrackerCurrentStageHtml(pr, stages, { from: STAGE_TRACKER_PR_COLUMN })}${typeof stageTrackerPreparationCommentHtml === 'function' ? stageTrackerPreparationCommentHtml(pr, ctx, stageTrackerItemKey(pr)) : typeof stageTrackerEventHtml === 'function' ? stageTrackerEventHtml(stageTrackerItemKey(pr), ctx) : ''}${typeof stageTrackerExecutionHtml === 'function' ? stageTrackerExecutionHtml(pr) : ''}
        </div>`;
    }

    // Covering PRs of an issue, furthest along first, attention-first
    // within a stage (stageTrackerAttentionTier), then longest on step.
    // A PR that closes several open issues is drawn once: the server names
    // the issue whose row carries it (drawn_under); the other issues list it
    // as a compact "under #M" chip instead of a second identical strip
    // (hivecommons/hive#14921 under #14910 and #14911).
    function stageTrackerLinkedDrawnHere(lp, item) {
      const under = Number(lp && lp.drawn_under) || 0;
      return !under || under === (Number(item && item.number) || 0);
    }
    function stageTrackerLinkedElsewhere(item) {
      const linked = Array.isArray(item.linked_prs) ? item.linked_prs : [];
      return linked.filter(lp => !stageTrackerLinkedDrawnHere(lp, item)).sort((x, y) => (x.number || 0) - (y.number || 0));
    }
    function stageTrackerLinkedElsewhereHtml(item) {
      return stageTrackerLinkedElsewhere(item).map(lp => `<a class="ham-badge ham-linked-elsewhere" href="${esc(lp.url || '#')}" target="_blank" rel="noopener" title="${esc('PR #' + (Number(lp.number) || 0) + ' covers this issue too; its row is drawn under #' + Number(lp.drawn_under))}">↳ #${Number(lp.number) || 0} · under #${Number(lp.drawn_under)}</a>`).join(' ');
    }
    function stageTrackerLinkedSorted(item, stages) {
      const linked = Array.isArray(item.linked_prs) ? item.linked_prs.filter(lp => stageTrackerLinkedDrawnHere(lp, item) && !stageTrackerLinkedSupersededGone(lp, item)) : (item.linked_pr ? [{ number: item.linked_pr }] : []);
      const sinceOf = lp => Date.parse(lp.stage_since || '') || Infinity;
      return linked.sort((x, y) => (stageTrackerAnchor(y.stage, stages, 'pr', y) - stageTrackerAnchor(x.stage, stages, 'pr', x)) || (stageTrackerAttentionTier(x) - stageTrackerAttentionTier(y)) || (sinceOf(x) - sinceOf(y)) || ((x.number || 0) - (y.number || 0)));
    }

    // Pure: a PR whose drawn_under issue is not a row of this page (filtered
    // out or on another page) would otherwise be drawn nowhere. The first
    // listed issue that covers it draws it instead; later ones chip to that
    // row. Returns the rows (same length, nulls kept), copying only changed ones.
    function stageTrackerAdoptOrphanPRs(rows) {
      const listed = new Set();
      for (const it of rows) if (it && it.kind === 'issue') listed.add(it.repo + '#' + it.number);
      const claimedBy = {};
      return rows.map(it => {
        if (!it || it.kind !== 'issue' || !Array.isArray(it.linked_prs)) return it;
        let changed = false;
        const linked = it.linked_prs.map(lp => {
          const under = Number(lp && lp.drawn_under) || 0;
          if (!under || under === (Number(it.number) || 0) || listed.has(it.repo + '#' + under)) return lp;
          const key = it.repo + '#' + lp.number;
          if (!(key in claimedBy)) claimedBy[key] = Number(it.number) || 0;
          if (claimedBy[key] === (Number(it.number) || 0)) { changed = true; return Object.assign({}, lp, { drawn_under: 0 }); }
          changed = true;
          return Object.assign({}, lp, { drawn_under: claimedBy[key] });
        });
        return changed ? Object.assign({}, it, { linked_prs: linked }) : it;
      });
    }

    // The stage a covering PR drives its issue's strip at: its own, or 040
    // covered-by-PR while it has none yet (an open PR filed seconds ago,
    // still waiting for the stage-label pass). One rule for the strip and
    // the list order — hive's own #15654 (000 New, nested #15655 drawn as
    // "PR in progress") sat at the bottom among the New rows because the
    // sort key fell back to the parent's unlabelled stage while the strip
    // did not.
    const STAGE_TRACKER_COVERED_BY_PR = 40;
    function stageTrackerLeadStage(lp) {
      return Number(lp && lp.stage) || STAGE_TRACKER_COVERED_BY_PR;
    }
    // Pure: the row whose live work drives an issue's strip and list place
    // — its furthest covering PR, else the issue itself.
    function stageTrackerLead(item, stages) {
      const linked = item.kind === 'issue' ? stageTrackerLinkedSorted(item, stages) : [];
      if (!linked.length) return { lead: item, kind: item.kind, stage: Number(item.stage) || 0, since: item.stage_since || '' };
      const lp = linked[0];
      return { lead: lp, kind: 'pr', stage: stageTrackerLeadStage(lp), since: lp.stage_since || item.stage_since || '' };
    }
    // List order: furthest along the strip first — a covered issue counts as
    // far along as its furthest covering PR — then longest-waiting first.
    function stageTrackerSortKey(item, stages) {
      const l = stageTrackerLead(item, stages);
      return { anchor: stageTrackerAnchor(l.stage, stages, l.kind, l.lead), since: Date.parse(l.since) || Infinity };
    }

    /* ---- Repo chip and group-by-repo (pure; run under node by
       stage_tracker_repo_grouping_test.go) ------------------------------- */
    // Deterministic palette slot for a repo: the string hash the contributor
    // tint fallback uses (contribTintFallback), bucketed onto the
    // --ham-repo-tint-N tokens so the chip CSS names no literal colour.
    function stageTrackerRepoTint(repo) {
      const text = String(repo || '');
      let hash = 0;
      for (let i = 0; i < text.length; i++) hash = ((hash << 5) - hash + text.charCodeAt(i)) | 0;
      return Math.abs(hash) % STAGE_TRACKER_REPO_TINTS;
    }
    function stageTrackerRepoShort(repo) {
      const s = String(repo || '');
      const i = s.indexOf('/');
      return i >= 0 ? s.slice(i + 1) : s;
    }
    // The configured primary repo may arrive as owner/repo or as the bare
    // name; an item's repo is owner/repo.
    function stageTrackerIsPrimary(repo, primary) {
      if (!repo || !primary) return false;
      if (repo === primary) return true;
      return String(primary).indexOf('/') < 0 && stageTrackerRepoShort(repo) === primary;
    }
    // Whether rows carry the chip under a setting, given how many repos the
    // payload spans.
    function stageTrackerRepoChipOn(mode, repoCount) {
      if (mode === 'never') return false;
      if (mode === 'always') return true;
      return (Number(repoCount) || 0) > 1;
    }
    // The chip. ctx.repoChips = { primary, active } turns it on
    // (renderStageTracker sets it from the setting); absent → ''. The
    // primary repo's chip is muted; click filters the list to the repo, or
    // clears the filter when it already is that repo.
    function stageTrackerRepoChipHtml(repo, ctx) {
      if (!ctx || !ctx.repoChips || !repo) return '';
      const primary = stageTrackerIsPrimary(repo, ctx.repoChips.primary);
      const active = ctx.repoChips.active === repo;
      const tip = repo + (primary ? ' (primary repo)' : '') + (active ? ' — showing only this repo; click to show all repos' : ' — click to show only this repo');
      return `<button type="button" class="ham-repo-chip ham-repo-tint-${stageTrackerRepoTint(repo)}${primary ? ' ham-repo-primary' : ''}" data-action="hamRepoChip" data-arg0="${esc(repo)}" data-stop="1" aria-pressed="${active ? 'true' : 'false'}" title="${esc(tip)}">${esc(stageTrackerRepoShort(repo))}</button>`;
    }
