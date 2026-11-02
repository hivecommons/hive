    // Column name for a group header's distribution tooltip (as the column header draws it).
    function stageTrackerColName(idx, stages) {
      if (idx >= STAGE_TRACKER_PATH.length) return 'Merged';
      const n = STAGE_TRACKER_PATH[idx];
      const st = (stages || []).find(s => Number(s.number) === n) || {};
      return n === 0 ? 'Open' : (st.short || st.label || String(n));
    }
    // Rows grouped by repo: [{ repo, key, rows, count, attention, cols,
    // collapsed }], the primary repo first, then by row count (desc), then
    // by name. cols is the per-column row count (a covered issue rides its
    // furthest PR, as the strip draws it); needs-attention is counted over
    // the rows. opts: { primary, collapsed: { repo: true }, stages }.
    function stageTrackerGroups(rows, opts) {
      opts = opts || {};
      const by = {};
      for (const it of rows || []) {
        const r = it.repo || '';
        if (!by[r]) by[r] = { repo: r, key: STAGE_TRACKER_GROUP_KEY_PREFIX + r, rows: [], count: 0, attention: 0, cols: Array.from({ length: STAGE_TRACKER_PATH.length + 1 }, () => 0), collapsed: !!(opts.collapsed && opts.collapsed[r]) };
        const g = by[r];
        g.rows.push(it);
        g.count++;
        if (stageTrackerNeedsAttention(it)) g.attention++;
        const anchor = stageTrackerSortKey(it, opts.stages || []).anchor;
        const col = anchor >= STAGE_TRACKER_TERMINAL ? STAGE_TRACKER_PATH.length : STAGE_TRACKER_PATH.indexOf(anchor);
        if (col >= 0) g.cols[col]++;
      }
      const prim = g => stageTrackerIsPrimary(g.repo, opts.primary) ? 0 : 1;
      return Object.keys(by).map(r => by[r]).sort((a, b) => (prim(a) - prim(b)) || (b.count - a.count) || a.repo.localeCompare(b.repo));
    }
    // One page of grouped entries: { group } headers and { group, item }
    // rows. Headers do not count toward the page size; a page that opens
    // mid-group repeats that group's header; a collapsed group is its
    // header alone. total counts every row (so a collapsed group is not
    // reported as filtered out); from/to and the pages run over the rows
    // of expanded groups (shown).
    function stageTrackerGroupPage(groups, f) {
      f = stageTrackerNormalizeFilter(f);
      const flat = [];
      let total = 0;
      for (const g of groups || []) { total += g.count; flat.push({ group: g }); if (!g.collapsed) for (const it of g.rows) flat.push({ group: g, item: it }); }
      const shown = flat.filter(e => e.item).length;
      const pages = Math.max(1, Math.ceil(shown / f.size));
      const page = Math.min(f.page, pages);
      const lo = (page - 1) * f.size, hi = page * f.size;
      const rows = [];
      let n = 0, lastHeader = null;
      for (const e of flat) {
        if (!e.item) {
          // A header sits at the row position its rows start; one exactly
          // at the page end opens the next page unless this is the last.
          if (n >= lo && (n < hi || page === pages)) { rows.push(e); lastHeader = e.group; }
          continue;
        }
        if (n >= lo && n < hi) {
          if (lastHeader !== e.group) { rows.push({ group: e.group }); lastHeader = e.group; }
          rows.push(e);
        }
        n++;
      }
      return { rows, page, pages, from: shown ? lo + 1 : 0, to: Math.min(shown, hi), total, shown, size: f.size };
    }
    // Group header row: caret toggle, repo chip, owner/repo, row count,
    // needs-attention count and a mini per-column distribution. Keyed
    // `group:owner/repo` by the render so the reconcile keeps it stable.
    function stageTrackerGroupHeaderHtml(g, stages, ctx) {
      const max = Math.max(1, ...g.cols);
      const dist = g.cols.map((c, i) => `<i class="ham-gd ham-gd-${c ? Math.max(1, Math.ceil(c / max * STAGE_TRACKER_GROUP_DIST_STEPS)) : 0}" title="${esc(stageTrackerColName(i, stages) + ': ' + c)}"></i>`).join('');
      const attn = g.attention ? `<span class="ham-group-attn">${g.attention} need${g.attention === 1 ? 's' : ''} attention</span>` : '';
      // The header always carries the chip, whatever the row setting.
      const chipCtx = { repoChips: Object.assign({}, (ctx && ctx.repoChips) || {}) };
      return `<div class="ham-group${g.collapsed ? ' ham-group-collapsed' : ''}" data-ham-repo="${esc(g.repo)}" role="row">
        <button type="button" class="ham-group-toggle" data-action="hamGroupToggle" data-arg0="${esc(g.repo)}" data-stop="1" aria-expanded="${g.collapsed ? 'false' : 'true'}" aria-label="${esc((g.collapsed ? 'Expand ' : 'Collapse ') + g.repo)}"><span class="ham-group-caret" aria-hidden="true">${g.collapsed ? '▸' : '▾'}</span></button>
        ${stageTrackerRepoChipHtml(g.repo, chipCtx)}<span class="ham-group-name">${esc(g.repo)}</span>
        <span class="ham-group-n">${g.count} ${g.count === 1 ? 'row' : 'rows'}</span>${attn}
        <span class="ham-group-dist" role="img" aria-label="${esc(g.cols.map((c, i) => stageTrackerColName(i, stages) + ' ' + c).join(', '))}">${dist}</span>
      </div>`;
    }

    // Row = title line (number, title, chips, since) + thin strip. Non-active
    // chevrons carry only a grey icon; the active one carries words.
    // Everything else is in the tooltip on the row's own #N link: the issue
    // ref on the left for an issue row, the ham-pr-ref in the PR column for a
    // PR row (the row container, kind/author icons, chips, the since cell and
    // a PR row's issue slot carry none).
    // An issue with no PR yet, or exactly one covering PR, is a single entry
    // (the PR's stage drives the strip and its "[icon] #N title" sits in the
    // PR column). With two or more covering PRs the issue strip stops at
    // Claimed and one sub-row per PR continues it. A PR drawn on its own
    // row owns the whole strip; its issue slot names the issue it closes or
    // the "no linked issue" placeholder, never the PR itself.
    function stageTrackerRowHtml(item, stages, ctx) {
      ctx = ctx || {};
      const nowMs = Number(ctx.nowMs) || Date.now();
      const isIssue = item.kind === 'issue';
      const title = item.title || '';
      let prRefs = [], shown = item, range = null, subs = '', subRows = null, prTargetKey = '';
      if (isIssue) {
        const linked = stageTrackerLinkedSorted(item, stages);
        if (linked.length === 1) {
          // One covering PR: a single entry whose strip the PR's stage drives.
          const lp = linked[0];
          const pr = stageTrackerLinkedPRItem(item, lp);
          prTargetKey = stageTrackerItemKey(pr);
          const who = (lp.author_kind || 'hive') + (lp.author ? ' · @' + lp.author : '');
          const lineage = (lp.lineage && typeof stageTrackerLineageChipHtml === 'function' ? stageTrackerLineageChipHtml(lp, nowMs) : '') || (typeof stageTrackerAdoptedFromChipHtml === 'function' ? stageTrackerAdoptedFromChipHtml(lp, nowMs) : '');
          prRefs = [STAGE_TRACKER_LINKED_GLYPH + stageTrackerPRPillsHtml(stageTrackerPRRef(lp.number, lp.url, lp.author_kind || '', lp.author, lp.title, (lp.title ? lp.title + '\n' : '') + 'Author: ' + who + ' · ' + stageTrackerLinkedTip(item, lp.number, ctx)), typeof stageTrackerLinkedChipsHtml === 'function' ? stageTrackerLinkedChipsHtml(lp, lineage) : (lineage || null), lp.base_ref)];
          shown = Object.assign({}, item, { via_pr: true, stage: stageTrackerLeadStage(lp), stage_since: lp.stage_since || item.stage_since, modifiers: lp.modifiers || item.modifiers, deferred_modifiers: lp.deferred_modifiers || item.deferred_modifiers || [], caption: lp.caption || item.caption, reason: lp.reason || item.reason, preparation: lp.preparation || null,
            terminal_merge: !!lp.terminal_merge, merged_by: lp.merged_by || '',
            human_approver: lp.human_approver || '', manual_merge: !!lp.manual_merge, human_merge_reason: lp.human_merge_reason || '', human_merge_reason_text: lp.human_merge_reason_text || '', sentinel_alert: !!lp.sentinel_alert, sentinel_reason: lp.sentinel_reason || '', sentinel_notice: !!lp.sentinel_notice, head: lp.head || '', decision_needed: lp.decision_needed || null, hold: lp.hold || null, adoption: lp.adoption || null, stage_actor: lp.stage_actor || item.stage_actor || null, claimant: lp.claimant || null, presence: lp.presence || null, rework: lp.rework || null, push: lp.push || null, merge_gate: lp.merge_gate || null, merge_train: lp.merge_train || null, left_train: lp.left_train || null, overlap: lp.overlap || null, decided: lp.decided || item.decided || null });
        } else if (linked.length) {
          // Several covering PRs: the issue strip ends at Claimed, all passed
          // and nothing lit; one sub-row per PR carries the live chevron.
          range = { to: 20 };
          if (stageTrackerAnchor(item.stage, stages, item.kind) <= 20) shown = Object.assign({}, item, { stage: 40 });
          subs = typeof stageTrackerPreparationSubRows === 'function' ? stageTrackerPreparationSubRows(linked, item, stages, nowMs, ctx) : linked.map(lp => stageTrackerSubRowHtml(lp, item, stages, nowMs, ctx)).join('');
          subRows = linked;
        }
      }
      // Invariant (owner): the issue slot on the left never holds a PR. A
      // PR whose issue is listed is nested under it; a PR seen on its own
      // (no issue, or the Pull requests view) puts the issue it closes — or
      // a muted "no linked issue" placeholder — on the left and names
      // itself in the PR column, exactly where a covering PR sits on an
      // issue row. That holds when the issue is already closed too
      // (closed_issue set, hivecommons/hive#14655): the issue still leads,
      // struck through with a `closed` chip — `closed by #N` when the
      // server knows the merged PR that closed it, so a duplicate fix reads
      // as one (hivecommons/hive#15232) — and the PR stays in the PR column
      // rather than taking the issue slot. A not-planned close adds a chip
      // saying the PR may be moot.
      // The repo chip (when on) leads the row, before the issue slot's #N.
      const chip = ctx.repoChips ? stageTrackerRepoChipHtml(item.repo, ctx) : '';
      let leadHtml;
      if (isIssue) {
        leadHtml = `<div class="ham-head-main">
            ${stageTrackerIcon('issue', 'ham-kind-ico')}${chip}
            <a class="ham-ref" href="${esc(item.url || '#')}" target="_blank" rel="noopener" title="${esc(stageTrackerTooltip(shown, nowMs))}">#${Number(item.number) || 0}</a>
            ${stageTrackerTitleHtml(title)}
            ${item.claim_reference ? `<span class="ham-badge ham-issue-refs-chip" title="${esc(STAGE_TRACKER_SOFT_LINK_TIP)}">refs</span>` : ''}
            ${typeof stageTrackerLinkedElsewhereHtml === 'function' ? stageTrackerLinkedElsewhereHtml(item) : ''}
            ${item.is_tracker ? `<span class="ham-badge">tracker${item.children && item.children.total ? ' ' + Number(item.children.completed) + '/' + Number(item.children.total) : ''}</span>` : ''}
          </div>`;
      } else {
        const li = stageTrackerPRIssueSlot(item);
        prRefs = [stageTrackerPRRef(item.number, item.url, item.author_kind || '', item.author, title, (title ? title + ' · ' : '') + stageTrackerTooltip(shown, nowMs)) + (item.lineage && typeof stageTrackerLineageChipHtml === 'function' ? ' ' + stageTrackerLineageChipHtml(item, nowMs) : '')];
        leadHtml = li.number
          ? `<div class="ham-head-main${li.closed ? ' ham-issue-closed' : ''}">
            ${stageTrackerIcon('issue', 'ham-kind-ico')}${chip}
            <a class="ham-ref" href="${esc(li.url || '#')}" target="_blank" rel="noopener">#${li.number}</a>
            ${li.title ? stageTrackerTitleHtml(li.title) : `<span class="ham-title ham-no-issue-t">${esc(li.placeholder)}</span>`}${li.soft ? ` <span class="ham-badge ham-issue-refs-chip" title="${esc(STAGE_TRACKER_SOFT_LINK_TIP)}">refs</span>` : ''}${li.closed ? ' ' + stageTrackerClosedChipHtml(item, li) : ''}
          </div>`
          : `<div class="ham-head-main ham-no-issue">
            ${stageTrackerIcon('issue', 'ham-kind-ico')}${chip}
            <span class="ham-title ham-no-issue-t">${esc(li.text)}</span>
          </div>`;
        if (typeof stageTrackerLinkedIssueToolsHtml === 'function') leadHtml += stageTrackerLinkedIssueToolsHtml(item);
      }
      const prs = typeof stageTrackerRowPRKeys === 'function' ? stageTrackerRowPRKeys(item) : '';
      const trainKey = typeof stageTrackerTrainKey === 'function' ? stageTrackerTrainKey(item) : '';
      if (typeof stageTrackerPreparationMark === 'function') stageTrackerPreparationMark(shown);
      return `<div class="ham-row${typeof stageTrackerCueClasses === 'function' ? stageTrackerCueClasses(shown, ctx) : ''}${typeof stageTrackerTrainRowClass === 'function' ? stageTrackerTrainRowClass(item, ctx.prevRow || null, ctx.nextRow || null, ctx.trainIndex && trainKey ? ctx.trainIndex[trainKey] : undefined) : ''}${typeof stageTrackerPreparationRowClass === 'function' ? stageTrackerPreparationRowClass(item, stages, ctx) : ''}" tabindex="0" data-ham-kind="${esc(item.kind)}" data-ham-repo="${esc(item.repo || '')}" data-ham-stage="${Number(item.stage) || 0}"${prs ? ` data-ham-prs="${esc(prs)}"` : ''}${trainKey ? ` data-ham-train="${esc(trainKey)}"` : ''}>
        ${typeof stageTrackerPreparationNodeHtml === 'function' ? stageTrackerPreparationNodeHtml(item, stages, ctx) : ''}
        ${stageTrackerRowToolsHtml(item)}
        <div class="ham-row-head">
          ${stageTrackerHeadColsHtml(prRefs, leadHtml, stageTrackerActedBadgeHtml(item, nowMs) + stageTrackerChipsHtml(shown) + (typeof stageTrackerLeadButtonHtml === 'function' ? stageTrackerLeadButtonHtml(item, nowMs, subRows) : ''), prTargetKey)}
        </div>
        <div class="ham-strip-row">
          <ol class="ham-strip" aria-label="Pipeline progress"${stageTrackerMoveAttrs(stageTrackerItemKey(item), ctx)}>${stageTrackerStripItems(shown, stages, range, nowMs, ctx)}</ol>
          ${stageTrackerSinceHtml(shown, nowMs)}
        </div>${typeof stageTrackerHumanContextHtml === 'function' ? stageTrackerHumanContextHtml(item, stages, nowMs, subRows) : ''}${stageTrackerCurrentStageHtml(shown, stages, range, nowMs)}${typeof stageTrackerPreparationCommentHtml === 'function' ? stageTrackerPreparationCommentHtml(shown, ctx, stageTrackerItemKey(item)) : typeof stageTrackerEventHtml === 'function' ? stageTrackerEventHtml(stageTrackerItemKey(item), ctx) : ''}${typeof stageTrackerExecutionHtml === 'function' ? stageTrackerExecutionHtml(shown) : ''}${typeof stageTrackerReviewNotesHtml === 'function' ? stageTrackerReviewNotesHtml(item, ctx) : ''}${subs}
      </div>`;
    }

    /* ---- tracker-cues (JS): working / waiting / event line ----------------
       See the CSS block of the same name. Pure helpers, run under node by
       stage_tracker_cues_test.go. */
    // Pure: which open items a hive agent is on right now, from the status
    // payload the dashboard already polls: busy agents whose lastAction is
    // in their current round. Keyed owner/repo#number (lower-case); an
    // action names the item but not its kind, so rows match by number.
    const STAGE_TRACKER_WORK_WORDS = {
      agent_pr_created: 'opened a pull request', agent_issue_created: 'opened an issue', agent_comment_created: 'commented', agent_issue_claimed: 'claimed it',
      agent_label_applied: 'changed labels', agent_review_requested: 'asked for review', agent_issue_closed: 'closed it', pr_closed: 'closed it', agent_pr_reviewed: 'reviewed it', pr_merged: 'merged it',
    };
