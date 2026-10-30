    // What a PR row's issue slot shows: the issue the PR closes (number,
    // title, url) — an open one first, else the closed one GitHub links
    // it to, with closed: true so the row draws it struck through under a
    // "closed" chip (a superseded PR whose body still says `Fixes #N`
    // pairs with an issue an earlier PR already fixed) — or { text } for
    // the "no linked issue" placeholder when its body names none. A closed
    // issue whose title the server could not resolve shows the muted
    // STAGE_TRACKER_CLOSED_ISSUE_TITLE placeholder, never a blank.
    // A link the server tagged with a soft `via` (refs / title / mention —
    // the PR works towards the issue without closing it) carries soft: true
    // so the row draws a "refs" chip beside it; a closing link never does.
    function stageTrackerPRIssueSlot(item) {
      const linked = (Array.isArray(item.linked_issues) ? item.linked_issues : []).filter(li => li && Number(li.number) > 0);
      const soft = li => STAGE_TRACKER_SOFT_LINK_VIAS.indexOf(li.via || '') >= 0;
      const open = linked.find(li => Number(li.number) === Number(item.nested_under)) || linked.find(li => li.listed || (li.state || '') === 'open');
      if (open) {
        const slot = { number: Number(open.number), title: open.title || '', url: open.url || '' };
        if (soft(open)) slot.soft = true;
        return slot;
      }
      const li = linked.find(l => (l.state || '') === 'closed') || linked[0];
      if (li) {
        const closed = (li.state || '') === 'closed';
        const title = li.title || '';
        const slot = { number: Number(li.number), title: title, url: li.url || '', closed: closed, placeholder: !title && closed ? STAGE_TRACKER_CLOSED_ISSUE_TITLE : (!title ? STAGE_TRACKER_UNTITLED_ISSUE : '') };
        if (soft(li)) slot.soft = true;
        if (closed) {
          // closed_issue is the same link after the server's closed-by pass;
          // prefer its closer / state reason when it names this issue.
          const ci = item.closed_issue && Number(item.closed_issue.number) === slot.number ? item.closed_issue : li;
          const by = Number(ci.closed_by || li.closed_by) || 0;
          if (by > 0) slot.closed_by = by;
          const reason = String(ci.state_reason || li.state_reason || '').toLowerCase();
          if (reason) slot.state_reason = reason;
        }
        return slot;
      }
      return { number: 0, text: 'no linked issue' };
    }
    // Pure: the chip(s) beside a closed issue in a PR row's issue slot.
    // `closed by #N` linking to the merged PR that closed it (GitHub's
    // closedByPullRequestsReferences, else the server's merged ledger) when
    // another PR delivered the issue — the open PR on this row is then a
    // follow-up or a duplicate (hivecommons/hive#15232). When no merged
    // closer is known the chip still says the issue closed while THIS PR is
    // unmerged, so the row cannot read as the issue having been closed by
    // an unmerged PR: the hive only ever closes an issue after its PR has
    // merged. A not-planned close adds an "issue closed (not planned)"
    // chip: the PR may be moot (hivecommons/hive#14655).
    function stageTrackerClosedChipHtml(item, slot) {
      // Inline (not a module const): the UI test lifts this function alone.
      const unmergedTip = 'Issue #' + slot.number + ' is closed, but this PR is still open and unmerged: it did not close the issue. A person or another merged PR did (the hive closes an issue only after its PR merges); this hive could not name the closer.';
      const notPlanned = String(slot.state_reason || '') === 'not_planned';
      const by = Number(slot.closed_by) || 0;
      let chip;
      if (by > 0 && by !== Number(item.number)) {
        const url = stageTrackerItemURL(item.repo, by, 'pull', item.url);
        chip = `<a class="ham-badge ham-issue-closed-chip ham-closed-by-chip" href="${esc(url)}" target="_blank" rel="noopener" title="${esc('Issue #' + slot.number + ' was closed by the merge of #' + by + '; this PR is still open against it and merging it will not close anything')}">closed by #${by}</a>`;
      } else {
        chip = `<span class="ham-badge ham-issue-closed-chip" title="${esc(unmergedTip)}">closed · this PR unmerged</span>`;
      }
      return chip + (notPlanned ? ' <span class="ham-badge ham-issue-closed-chip ham-closed-not-planned" title="The issue this PR closes was closed as not planned; the PR may be moot.">issue closed (not planned)</span>' : '');
    }
    // Pure: the GitHub URL of #number in repo, derived from a sibling item's
    // URL when one is at hand (same host, same owner/repo), else from the
    // repo name on github.com.
    function stageTrackerItemURL(repo, number, kind, siblingURL) {
      const m = /^(https?:\/\/[^/]+\/[^/]+\/[^/]+)\/(?:pull|issues)\/\d+/.exec(String(siblingURL || ''));
      const base = m ? m[1] : 'https://github.com/' + String(repo || '');
      return base + '/' + (kind === 'pull' ? 'pull' : 'issues') + '/' + (Number(number) || 0);
    }

    function stageTrackerNeedsAttention(item) {
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const n = Number(item.stage) || 0;
      // An advisory is a note, not work (hivecommons/hive#15381).
      if (item && item.advisory) return false;
      if (stageTrackerRegressionHandled(item)) return false;
      return mods.length > 0 || STAGE_TRACKER_ATTENTION_STAGES.indexOf(n) >= 0;
    }

    // Ordering tier of a row within its column (mirrors Go
    // stageTrackerAttentionTier): rows parked on a person first, then the
    // rest, then rows nobody should act on — hold (incl. do-not-merge/*),
    // draft, and a PR standing in for a closed issue. Only the pills a row
    // shows count (deferred_modifiers never promote it); the direction
    // toggle leaves the tier order alone.
    function stageTrackerAttentionTier(row) {
      const mods = Array.isArray(row.modifiers) ? row.modifiers : [];
      if (STAGE_TRACKER_SUNK_MODS.some(m => mods.indexOf(m) >= 0) || (row.closed_issue && Number(row.closed_issue.number) > 0)) return STAGE_TRACKER_TIER_SUNK;
      return stageTrackerNeedsAttention(row) ? STAGE_TRACKER_TIER_ATTENTION : STAGE_TRACKER_TIER_NORMAL;
    }

    // The instant an item's current wait began: when it entered its step,
    // else when it was opened (an item with no step yet is waiting since then).
    function stageTrackerWaitSince(item) {
      return Date.parse(item.stage_since || '') || Date.parse(item.created_at || '') || 0;
    }

    // Label family a label belongs to for the chip row: the text before the
    // first "/" or the "needs-" prefix; '' for everything else. Chips from one
    // family OR together, families AND together.
    function stageTrackerLabelFamily(label) {
      const l = String(label || '').toLowerCase();
      for (const fam of STAGE_TRACKER_LABEL_FAMILIES) if (l.startsWith(fam)) return fam;
      return '';
    }

    // label facet helpers begin
    // "On hold": the row wears the hold pill (Go stageTrackerOnHold).
    function stageTrackerItemOnHold(item) {
      const mods = item && Array.isArray(item.modifiers) ? item.modifiers : [];
      return mods.indexOf('hold') >= 0;
    }
    // "Needs human" — mirrors Go stageTrackerNeedsHuman (see the constants
    // above); the chip's count and the filter both call this, so they
    // cannot disagree. Stage/modifier based: labels alone (needs-signal,
    // hold, …) never qualify a row. A held row is paused, not waiting.
    function stageTrackerItemNeedsHuman(item) {
      if (!item || item.advisory || stageTrackerItemOnHold(item)) return false;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.some(m => STAGE_TRACKER_NEEDS_HUMAN_MODS.indexOf(m) >= 0)) return true;
      const n = Number(item.stage) || 0;
      if (STAGE_TRACKER_NEEDS_HUMAN_STAGES.indexOf(n) >= 0) return true;
      return STAGE_TRACKER_NEEDS_HUMAN_AUTHOR_STAGES.indexOf(n) >= 0 && (item.author_kind || '') === 'human';
    }

    // Chip order: every stage/NNN-* label first by NNN, then everything
    // else alphabetically, case-insensitive.
    function stageTrackerLabelCompare(a, b) {
      a = String(a || ''); b = String(b || '');
      const sa = /^stage\/(\d+)/i.exec(a), sb = /^stage\/(\d+)/i.exec(b);
      if (sa && sb) return (Number(sa[1]) - Number(sb[1])) || a.toLowerCase().localeCompare(b.toLowerCase()) || a.localeCompare(b);
      if (sa) return -1;
      if (sb) return 1;
      return a.toLowerCase().localeCompare(b.toLowerCase()) || a.localeCompare(b);
    }
    // label facet helpers end

    // Normalise a filter object: unknown values fall back to the default.
    function stageTrackerNormalizeFilter(raw) {
      const d = stageTrackerDefaultFilter();
      const f = Object.assign(d, raw && typeof raw === 'object' ? raw : {});
      if (['all', 'pr', 'issue'].indexOf(f.kind) < 0) f.kind = 'all';
      f.repo = typeof f.repo === 'string' ? f.repo : '';
      f.q = typeof f.q === 'string' ? f.q : '';
      f.labels = Array.isArray(f.labels) ? f.labels.filter(l => typeof l === 'string' && l).map(l => l.toLowerCase()) : [];
      f.needsHuman = !!f.needsHuman;
      f.onHold = !!f.onHold;
      f.waitingOnYou = !!f.waitingOnYou;
      if (!(f.age in STAGE_TRACKER_AGE_MS) && f.age !== 'stale') f.age = 'any';
      if (['human', 'clanker', 'hive'].indexOf(f.author) < 0) f.author = 'any';
      f.attention = !!f.attention;
      if (STAGE_TRACKER_ACTED_FILTERS.indexOf(f.acted) < 0) f.acted = 'any';
      if (STAGE_TRACKER_SORTS.indexOf(f.sort) < 0) f.sort = 'furthest';
      f.dir = f.dir === 'asc' ? 'asc' : 'desc';
      f.size = STAGE_TRACKER_PAGE_SIZES.indexOf(Number(f.size)) >= 0 ? Number(f.size) : STAGE_TRACKER_DEFAULT_PAGE_SIZE;
      f.page = Math.max(1, Math.floor(Number(f.page)) || 1);
      return f;
    }

    // True when any narrowing filter (not sort / paging) is set.
    function stageTrackerFilterActive(f) {
      return f.kind !== 'all' || !!f.repo || !!f.q || f.labels.length > 0 || f.needsHuman || f.onHold || f.waitingOnYou || f.age !== 'any' || f.author !== 'any' || f.attention || f.acted !== 'any';
    }

    // Does one item pass the narrowing filters? ctx.nowMs anchors the age
    // test; ctx.stages supplies the dwell for the "stale" age.
    function stageTrackerMatches(item, f, ctx) {
      ctx = ctx || {};
      if (f.kind !== 'all' && item.kind !== f.kind) return false;
      if (f.waitingOnYou && !item.waiting_on_you) return false;
      if (f.repo && item.repo !== f.repo) return false;
      if (f.q) {
        const q = f.q.trim().toLowerCase();
        const num = q.replace(/^#/, '');
        const byNumber = /^\d+$/.test(num) && String(item.number) === num;
        const byTitle = String(item.title || '').toLowerCase().indexOf(q) >= 0;
        const byLinked = (Array.isArray(item.linked_prs) ? item.linked_prs : []).some(lp => (/^\d+$/.test(num) && String(lp.number) === num) || String(lp.title || '').toLowerCase().indexOf(q) >= 0);
        const byIssue = (Array.isArray(item.linked_issues) ? item.linked_issues : []).some(li => (/^\d+$/.test(num) && String(li.number) === num) || String(li.title || '').toLowerCase().indexOf(q) >= 0);
        if (!byNumber && !byTitle && !byLinked && !byIssue) return false;
      }
      if (f.labels.length) {
        const have = new Set((Array.isArray(item.labels) ? item.labels : []).map(l => String(l).toLowerCase()));
        const byFamily = {};
        f.labels.forEach(l => { const fam = stageTrackerLabelFamily(l) || l; (byFamily[fam] = byFamily[fam] || []).push(l); });
        for (const fam in byFamily) if (!byFamily[fam].some(l => have.has(l))) return false;
      }
      if (f.needsHuman && !stageTrackerItemNeedsHuman(item)) return false;
      if (f.onHold && !stageTrackerItemOnHold(item)) return false;
      if (f.author !== 'any' && (item.author_kind || '') !== f.author) return false;
      if (f.attention && !stageTrackerNeedsAttention(item)) return false;
      if (f.acted !== 'any' && !stageTrackerActedMatches(item, f.acted, Number(ctx.nowMs) || Date.now())) return false;
      if (f.age !== 'any') {
        const nowMs = Number(ctx.nowMs) || Date.now();
        const since = stageTrackerWaitSince(item);
        if (f.age === 'stale') {
          const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
          const st = (ctx.stages || []).find(x => Number(x.number) === (Number(item.stage) || 0)) || {};
          const dwellMs = (Number(st.dwell_seconds) || 0) * 1000;
          if (!(mods.indexOf('stalled') >= 0 || (dwellMs > 0 && since > 0 && nowMs - since > dwellMs))) return false;
        } else if (!(since > 0 && nowMs - since > STAGE_TRACKER_AGE_MS[f.age])) return false;
      }
      return true;
    }

    // Rows for the current filter. Outside the Pull requests view a PR that
    // is nested under a listed issue is drawn as that issue's sub-row only.
    // A `blocked` review-backlog issue (review_note_of = its source PR) is
    // never a row of its own while that PR is open: it folds under the PR's
    // row (one muted "N review notes blocked on this PR" line), or, when the
    // PR is not drawn in this view, into the lowest-numbered note of its
    // group, so a PR with eight notes costs the count one row, not eight.
    function stageTrackerVisible(items, filter, ctx) {
      const f = stageTrackerNormalizeFilter(filter || _stageTrackerFilter);
      const rows = (items || []).filter(it => stageTrackerMatches(it, f, ctx));
      if (f.kind === 'pr') return rows;
      const issues = new Set(rows.filter(it => it.kind === 'issue').map(it => it.repo + '#' + it.number));
      const out = rows.filter(it => !(it.kind === 'pr' && it.nested_under && issues.has(it.repo + '#' + it.nested_under)));
      const drawnPRs = new Set(rows.filter(it => it.kind === 'pr').map(it => it.repo + '#' + it.number));
      const lead = {};
      for (const it of out) {
        if (!stageTrackerIsReviewNote(it)) continue;
        const k = it.repo + '#' + it.review_note_of;
        if (!(k in lead) || Number(it.number) < lead[k]) lead[k] = Number(it.number);
      }
      return out.filter(it => {
        if (!stageTrackerIsReviewNote(it)) return true;
        const k = it.repo + '#' + it.review_note_of;
        return !drawnPRs.has(k) && lead[k] === Number(it.number);
      });
    }

    // Pure: an issue the tracker folds as a review note (see stageTrackerVisible).
    function stageTrackerIsReviewNote(it) {
      return !!(it && it.kind === 'issue' && Number(it.review_note_of) > 0);
    }

    // Pure: the review notes folded into this row. On a PR row, its
    // review_notes; on the lead note of a group whose PR is not drawn, the
    // other open notes of that group (from the full item list).
    function stageTrackerFoldedReviewNotes(item, items) {
      if (!item) return [];
      if (item.kind === 'pr') return Array.isArray(item.review_notes) ? item.review_notes : [];
      if (!stageTrackerIsReviewNote(item)) return [];
      return (items || []).filter(it => stageTrackerIsReviewNote(it) && it.repo === item.repo && it.review_note_of === item.review_note_of && it.number !== item.number)
        .sort((a, b) => a.number - b.number);
    }

    // The muted one-line summary of a row's folded review notes, with each
    // note's number as a link so nothing becomes unreachable.
    function stageTrackerReviewNotesHtml(item, ctx) {
      const notes = stageTrackerFoldedReviewNotes(item, ctx && ctx.items);
      if (!notes.length) return '';
      const links = notes.map(n => n.url ? `<a href="${esc(n.url)}" target="_blank" rel="noopener" title="${esc(n.title || '')}">#${Number(n.number)}</a>` : `<span title="${esc(n.title || '')}">#${Number(n.number)}</span>`).join(', ');
      const lead = item.kind === 'pr'
        ? `${notes.length} review note${notes.length === 1 ? '' : 's'} blocked on this PR`
        : `+${notes.length} more review note${notes.length === 1 ? '' : 's'} blocked on PR #${Number(item.review_note_of)}`;
      return `<div class="ham-review-notes" data-ham-review-notes="${notes.length}">${esc(lead)} · ${links}</div>`;
    }

    // The canonical tracker count (owner): rows of the default view — every
    // item on its own row except a PR drawn on a listed issue's row.
    function stageTrackerRowCount(items, ctx) {
      return stageTrackerVisible(items, stageTrackerDefaultFilter(), ctx).length;
    }

    // Order rows. 'furthest' is the strip order (furthest along first, then
    // longest on step); 'waiting' is longest wait first; 'newest' is most
    // recently opened first; 'number' is repo then number. Under 'furthest'
    // and 'waiting' rows tier attention-first within a column
    // (stageTrackerAttentionTier); dir 'asc' reverses the strip / age /
    // repo / number keys but never the tier.
    function stageTrackerSort(rows, stages, f) {
      f = stageTrackerNormalizeFilter(f);
      const byRepoNumber = (x, y) => ((x.repo || '').localeCompare(y.repo || '')) || ((x.number || 0) - (y.number || 0));
      const sign = f.dir === 'asc' ? -1 : 1;
      const byTier = (x, y) => stageTrackerAttentionTier(x) - stageTrackerAttentionTier(y);
      let cmp;
      switch (f.sort) {
        case 'waiting': cmp = (x, y) => byTier(x, y) || sign * (((stageTrackerWaitSince(x) || Infinity) - (stageTrackerWaitSince(y) || Infinity)) || byRepoNumber(x, y)); break;
        case 'newest': cmp = (x, y) => sign * (((Date.parse(y.created_at || '') || 0) - (Date.parse(x.created_at || '') || 0)) || byRepoNumber(y, x)); break;
        case 'number': cmp = (x, y) => sign * byRepoNumber(y, x); break;
        default: {
          const keyOf = it => stageTrackerSortKey(it, stages);
          cmp = (x, y) => { const a = keyOf(x), b = keyOf(y); return sign * (b.anchor - a.anchor) || byTier(x, y) || sign * ((a.since - b.since) || byRepoNumber(x, y)); };
        }
      }
      return (rows || []).slice().sort(cmp);
    }

    // One page of rows plus the numbers the "showing X–Y of N" line needs.
    // A page past the end clamps to the last page.
    function stageTrackerPage(rows, f) {
      f = stageTrackerNormalizeFilter(f);
      const total = (rows || []).length;
      const pages = Math.max(1, Math.ceil(total / f.size));
      const page = Math.min(f.page, pages);
      const from = total ? (page - 1) * f.size + 1 : 0;
      const to = Math.min(total, page * f.size);
      return { rows: (rows || []).slice(from ? from - 1 : 0, to), page, pages, from, to, total, size: f.size };
    }

    // label facet model begin
    // Label chips: every label on any item, stage/NNN first by number then
    // the rest alphabetically (stageTrackerLabelCompare). The row shows the
    // first STAGE_TRACKER_LABEL_CHIP_LIMIT plus every selected label; `all`
    // is the complete list the "+N more" dialog offers, so no label is ever
    // out of reach. `needsHuman` and `onHold` are the synthetic facets:
    // their counts are the items stageTrackerItemNeedsHuman /
    // stageTrackerItemOnHold accept — the same predicates the filter uses.
    function stageTrackerLabelFacets(items, f, opts) {
      f = stageTrackerNormalizeFilter(f);
      const counts = {};
      (items || []).forEach(it => (Array.isArray(it.labels) ? it.labels : []).forEach(l => { const k = String(l).toLowerCase(); counts[k] = (counts[k] || 0) + 1; }));
      const all = Object.keys(counts).sort(stageTrackerLabelCompare);
      const shown = all.slice(0, STAGE_TRACKER_LABEL_CHIP_LIMIT);
      f.labels.forEach(l => { if (counts[l] && shown.indexOf(l) < 0) shown.push(l); });
      shown.sort(stageTrackerLabelCompare);
      const chip = l => ({ label: l, count: counts[l], on: f.labels.indexOf(l) >= 0 });
      const needsHuman = { count: (items || []).filter(stageTrackerItemNeedsHuman).length, on: f.needsHuman };
      const onHold = { count: (items || []).filter(stageTrackerItemOnHold).length, on: f.onHold };
      return { chips: shown.map(chip), all: all.map(chip), hidden: Math.max(0, all.length - shown.length), needsHuman, onHold };
    }

    // Rows of the "all labels" dialog: `facets.all` narrowed by a
    // case-insensitive substring query; selected labels always stay listed
    // so they can be cleared.
    function stageTrackerLabelDialogData(facets, query) {
      const q = String(query || '').trim().toLowerCase();
      const all = facets && Array.isArray(facets.all) ? facets.all : [];
      const rows = q ? all.filter(c => c.on || c.label.indexOf(q) >= 0) : all;
      const selected = all.filter(c => c.on).length + (facets && facets.needsHuman && facets.needsHuman.on ? 1 : 0) + (facets && facets.onHold && facets.onHold.on ? 1 : 0);
      return { rows, total: all.length, selected, query: q, needsHuman: facets ? facets.needsHuman : null, onHold: facets ? facets.onHold : null };
    }
    // label facet model end

    // "showing X–Y of N (M filtered out)", the header's "N rows · M need
    // attention" and the sidebar pill — all from one call, so they cannot
    // disagree. The canonical count is ROWS (owner): allCount is the row
    // count of the default view (a filter narrows page.total, never
    // allCount), and the pill shows it. A covering PR is drawn on its
    // issue's row rather than its own, so rows can sit below the payload's
    // open items; `open` ({issues, prs}) lets the line footnote that gap
    // ("2 PRs shown on their issue's row") instead of printing two totals.
    // Because the sidebar Overview pill prints the same open set as
    // issues/PRs (33/13) while this pill prints rows (37), `breakdown`
    // spells out how the row count is made ("33 issues + 4 PRs on their
    // own row") in the header and the pill tooltip, so the two pills read
    // as different views of one population rather than a disagreement.
    function stageTrackerCountsText(page, allCount, attention, open, attentionAll, folded) {
      const filteredOut = Math.max(0, allCount - page.total);
      const showing = page.total ? `showing ${page.from}–${page.to} of ${page.total}` : 'showing 0 of 0';
      const attnText = n => n ? n + ' need' + (n === 1 ? 's' : '') + ' attention' : 'nothing needs attention';
      const attn = attnText(attention);
      const attnAll = attnText(Number.isFinite(Number(attentionAll)) ? Number(attentionAll) : attention);
      const rowsWord = n => n === 1 ? 'row' : 'rows';
      const parts = stageTrackerRowBreakdown(allCount, open, folded);
      const noteBits = [];
      if (parts.shared) noteBits.push(`${parts.shared} ${parts.shared === 1 ? 'PR' : 'PRs'} shown on ${parts.shared === 1 ? 'its' : 'their'} issue's row`);
      if (parts.folded) noteBits.push(`${parts.folded} review ${parts.folded === 1 ? 'note' : 'notes'} folded under ${parts.folded === 1 ? 'its' : 'their'} PR`);
      const note = noteBits.join(' · ');
      const breakdown = parts.known ? `${parts.issues} ${parts.issues === 1 ? 'issue' : 'issues'} + ${parts.standalonePRs} ${parts.standalonePRs === 1 ? 'PR' : 'PRs'} on ${parts.standalonePRs === 1 ? 'its' : 'their'} own row` : '';
      return {
        showing: showing + (filteredOut ? ` (${filteredOut} filtered out)` : ''),
        header: `${page.total}${filteredOut ? ' of ' + allCount : ''} ${rowsWord(page.total)}${breakdown ? ` (${breakdown})` : ''} · ${attn}` + (note ? ` · ${note}` : ''),
        badge: allCount,
        badgeTitle: `${allCount} ${rowsWord(allCount)}` + (breakdown ? ` (${breakdown})` : '') + ` · ${attnAll}` + (note ? ` · ${note}` : ''),
        note,
        breakdown,
      };
    }

    // Pure: how the default view's row count decomposes against the open
    // item totals. Every open issue is a row except a review note folded
    // under its PR (`folded`, see stageTrackerVisible); a PR is a row only
    // when it is not drawn under a listed issue, so standalonePRs = rows −
    // (issues − folded) and shared = (issues − folded + PRs) − rows. `known`
    // is false without totals. `issues` stays the open-issue total so the
    // breakdown still adds up to the Overview tile.
    function stageTrackerRowBreakdown(rowCount, open, folded) {
      const rows = Math.max(0, Number(rowCount) || 0);
      if (!open || !Number.isFinite(Number(open.issues)) || !Number.isFinite(Number(open.prs))) return { known: false, rows, issues: 0, prs: 0, standalonePRs: 0, shared: 0, folded: 0 };
      const issues = Math.max(0, Number(open.issues)), prs = Math.max(0, Number(open.prs));
      const fold = Math.min(issues, Math.max(0, Number(folded) || 0));
      const standalonePRs = Math.min(prs, Math.max(0, rows - (issues - fold)));
      return { known: true, rows, issues, prs, standalonePRs, shared: Math.max(0, issues - fold + prs - rows), folded: fold };
    }

    // Pure: how many issues of the default view are folded review notes
    // rather than rows (stageTrackerVisible drops them; the count line
    // explains the difference).
    function stageTrackerFoldedCount(items, ctx) {
      const f = stageTrackerDefaultFilter();
      const shown = new Set(stageTrackerVisible(items, f, ctx).map(it => it.repo + '#' + it.number));
      return (items || []).filter(it => stageTrackerIsReviewNote(it) && stageTrackerMatches(it, f, ctx) && !shown.has(it.repo + '#' + it.number)).length;
    }

    // The tracker's open issue / PR counts: the payload's open_issues/open_prs
    // when present, else counted from the items. Items is the canonical
    // open-items set, so this equals the Overview tile's totals.
    function stageTrackerOpenCounts(d) {
      if (d && Number.isFinite(Number(d.open_issues)) && Number.isFinite(Number(d.open_prs))) return { issues: Number(d.open_issues), prs: Number(d.open_prs) };
      const items = Array.isArray(d?.items) ? d.items : [];
      return { issues: items.filter(it => it && it.kind !== 'pr').length, prs: items.filter(it => it && it.kind === 'pr').length };
    }

    function stageTrackerLoadFilter() {
      try { _stageTrackerFilter = stageTrackerNormalizeFilter(JSON.parse(localStorage.getItem(STAGE_TRACKER_FILTER_KEY) || 'null')); } catch { _stageTrackerFilter = stageTrackerDefaultFilter(); }
      _stageTrackerFilter.page = 1;
    }
    function stageTrackerSaveFilter() {
      try { const f = Object.assign({}, _stageTrackerFilter); delete f.page; localStorage.setItem(STAGE_TRACKER_FILTER_KEY, JSON.stringify(f)); } catch {}
    }
    // Repo chip / group settings and the per-repo collapsed set (localStorage).
    function stageTrackerRepoChipSetting() {
      let v = '';
      try { v = localStorage.getItem(STAGE_TRACKER_REPO_CHIP_KEY) || ''; } catch { v = ''; }
      return STAGE_TRACKER_REPO_CHIP_MODES.some(m => m.id === v) ? v : STAGE_TRACKER_REPO_CHIP_DEFAULT;
    }
    function stageTrackerSetRepoChip(id) {
      const v = STAGE_TRACKER_REPO_CHIP_MODES.some(m => m.id === id) ? id : STAGE_TRACKER_REPO_CHIP_DEFAULT;
      try { localStorage.setItem(STAGE_TRACKER_REPO_CHIP_KEY, v); } catch {}
      return v;
    }
    function stageTrackerGroupSetting() {
      let v = '';
      try { v = localStorage.getItem(STAGE_TRACKER_GROUP_KEY) || ''; } catch { v = ''; }
      return STAGE_TRACKER_GROUPS.indexOf(v) >= 0 ? v : STAGE_TRACKER_GROUP_DEFAULT;
    }
    function stageTrackerSetGroup(id) {
      const v = STAGE_TRACKER_GROUPS.indexOf(id) >= 0 ? id : STAGE_TRACKER_GROUP_DEFAULT;
      try { localStorage.setItem(STAGE_TRACKER_GROUP_KEY, v); } catch {}
      return v;
    }
    function stageTrackerLoadCollapsed() {
      try {
        const o = JSON.parse(localStorage.getItem(STAGE_TRACKER_GROUP_COLLAPSED_KEY) || '{}');
        if (!o || typeof o !== 'object' || Array.isArray(o)) return {};
        const out = {};
        for (const k of Object.keys(o)) if (o[k]) out[k] = true;
        return out;
      } catch { return {}; }
    }
    function stageTrackerSaveCollapsed(c) {
      try { localStorage.setItem(STAGE_TRACKER_GROUP_COLLAPSED_KEY, JSON.stringify(c || {})); } catch {}
    }
    // Pure: the collapsed set with one repo toggled.
    function stageTrackerToggleCollapsed(collapsed, repo) {
      const out = Object.assign({}, collapsed || {});
      if (out[repo]) delete out[repo]; else out[repo] = true;
      return out;
    }
    function stageTrackerLoadGrouping() {
      _stageTrackerGroup = stageTrackerGroupSetting();
      _stageTrackerCollapsed = stageTrackerLoadCollapsed();
    }
    function stageTrackerPrimaryRepo() { return (typeof window !== 'undefined' && window._primaryRepo) || ''; }
    function hamGroup(value) {
      _stageTrackerGroup = stageTrackerSetGroup(value);
      _stageTrackerFilter.page = 1;
      renderStageTracker();
      stageTrackerRenderSettings();
    }
    function hamGroupToggle(repo) {
      _stageTrackerCollapsed = stageTrackerToggleCollapsed(_stageTrackerCollapsed, repo);
      stageTrackerSaveCollapsed(_stageTrackerCollapsed);
      renderStageTracker();
    }
    // A row's repo chip: filter to that repo, or back to all when it already is.
    function hamRepoChip(repo) { hamFilter('repo', _stageTrackerFilter.repo === repo ? '' : repo); }
    function stageTrackerRepoChipChange(value) {
      stageTrackerSetRepoChip(value);
      renderStageTracker();
    }

    function hamFilter(key, value) {
      const f = _stageTrackerFilter;
      switch (key) {
        case 'kind': f.kind = value || 'all'; break;
        case 'repo': f.repo = value || ''; break;
        case 'q': f.q = value || ''; break;
        case 'age': f.age = value || 'any'; break;
        case 'author': f.author = value || 'any'; break;
        case 'attention': f.attention = value === 'toggle' ? !f.attention : !!value; break;
        case 'acted': f.acted = value || 'any'; break;
        case 'sort': f.sort = value || 'furthest'; break;
        case 'dir': f.dir = value === 'toggle' ? (f.dir === 'asc' ? 'desc' : 'asc') : value; break;
        case 'size': f.size = Number(value) || STAGE_TRACKER_DEFAULT_PAGE_SIZE; break;
        case 'label': { const l = String(value || '').toLowerCase(); const i = f.labels.indexOf(l); if (i >= 0) f.labels.splice(i, 1); else if (l) f.labels.push(l); break; }
        case 'needsHuman': f.needsHuman = value === 'toggle' ? !f.needsHuman : !!value; break;
        case 'onHold': f.onHold = value === 'toggle' ? !f.onHold : !!value; break;
        case 'waitingOnYou': f.waitingOnYou = value === 'toggle' ? !f.waitingOnYou : !!value; break;
        case 'labelsClear': f.labels = []; f.needsHuman = false; f.onHold = false; break;
        case 'page': f.page = value === 'prev' ? f.page - 1 : value === 'next' ? f.page + 1 : Number(value) || 1; break;
        case 'reset': { const keep = { sort: f.sort, dir: f.dir, size: f.size }; _stageTrackerFilter = Object.assign(stageTrackerDefaultFilter(), keep); break; }
      }
      if (key !== 'page') _stageTrackerFilter.page = 1;
      _stageTrackerFilter = stageTrackerNormalizeFilter(_stageTrackerFilter);
      stageTrackerSaveFilter();
      renderStageTracker();
    }

    // Push the filter state into the toolbar controls (they may have been
    // restored from storage or reset).
    function stageTrackerSyncToolbar(f) {
      document.querySelectorAll('#activity-monitor-toolbar .ham-chip[data-arg0="kind"]').forEach(b => b.setAttribute('aria-pressed', b.dataset.arg1 === f.kind ? 'true' : 'false'));
      const setVal = (id, v) => { const el = document.getElementById(id); if (el && el.value !== String(v) && document.activeElement !== el) el.value = String(v); };
      setVal('activity-monitor-q', f.q);
      setVal('activity-monitor-age', f.age);
      setVal('activity-monitor-author', f.author);
      setVal('activity-monitor-acted', f.acted);
      setVal('activity-monitor-sort', f.sort);
      setVal('activity-monitor-group', _stageTrackerGroup);
      setVal('activity-monitor-size', f.size);
      const att = document.getElementById('activity-monitor-attention');
      if (att) att.setAttribute('aria-pressed', f.attention ? 'true' : 'false');
      const wait = document.getElementById('activity-monitor-waiting');
      if (wait) wait.setAttribute('aria-pressed', f.waitingOnYou ? 'true' : 'false');
      const dir = document.getElementById('activity-monitor-dir');
      if (dir) { dir.textContent = f.dir === 'asc' ? '↑' : '↓'; dir.setAttribute('title', (f.dir === 'asc' ? 'Ascending — click for descending' : 'Descending — click for ascending') + STAGE_TRACKER_DIR_TIER_NOTE); }
      const reset = document.getElementById('activity-monitor-reset');
      if (reset) reset.hidden = !stageTrackerFilterActive(f);
    }

    function stageTrackerLabelChipHtml(c) {
      return `<button type="button" class="ham-chip ham-label-chip" data-action="hamFilter" data-arg0="label" data-arg1="${esc(c.label)}" aria-pressed="${c.on ? 'true' : 'false'}" title="${esc((c.on ? 'Remove label filter ' : 'Filter by label ') + c.label)}">${esc(c.label)}<span class="ham-chip-n">${c.count}</span></button>`;
    }
    function stageTrackerNeedsHumanChipHtml(nh) {
      if (!nh) return '';
      const tip = (nh.on ? 'Remove the Needs human filter. ' : '') + STAGE_TRACKER_NEEDS_HUMAN_TIP;
      return `<button type="button" class="ham-chip ham-needs-human-chip" data-action="hamFilter" data-arg0="needsHuman" data-arg1="toggle" aria-pressed="${nh.on ? 'true' : 'false'}" title="${esc(tip)}">🙋 Needs human<span class="ham-chip-n">${nh.count}</span></button>`;
    }
    function stageTrackerOnHoldChipHtml(oh) {
      if (!oh) return '';
      const tip = (oh.on ? 'Remove the On hold filter. ' : '') + STAGE_TRACKER_ON_HOLD_TIP;
      return `<button type="button" class="ham-chip ham-on-hold-chip" data-action="hamFilter" data-arg0="onHold" data-arg1="toggle" aria-pressed="${oh.on ? 'true' : 'false'}" title="${esc(tip)}">⏸ On hold<span class="ham-chip-n">${oh.count}</span></button>`;
    }
    function stageTrackerLabelsHtml(facets) {
      if (!facets.chips.length && !facets.needsHuman && !facets.onHold) return '';
      return stageTrackerNeedsHumanChipHtml(facets.needsHuman)
        + stageTrackerOnHoldChipHtml(facets.onHold)
        + facets.chips.map(stageTrackerLabelChipHtml).join('')
        + (facets.hidden ? `<button type="button" class="ham-chip ham-labels-more" data-action="openStageTrackerLabelDialog" aria-haspopup="dialog" title="Show all ${facets.all.length} labels with counts; pick any to filter">+${facets.hidden} more</button>` : '');
    }

    // "All labels" dialog: every label with its count, same order as the
    // row, a type-to-filter box, the same toggle the chips use and Clear.
    // In-app modal (config-overlay recipe), never a native prompt.
    function stageTrackerLabelDialogHtml(data) {
      return `<div class="ham-label-dialog-tools"><input type="search" class="ham-input" id="activity-monitor-labels-q" placeholder="Type to filter labels…" autocomplete="off" spellcheck="false" aria-label="Filter the label list" value="${esc(data.query)}" data-input-action="stageTrackerLabelDialogQuery" data-arg-types="w"><span class="ham-count">${esc(stageTrackerLabelDialogCountText(data))}</span><button type="button" class="hv-btn btn-secondary btn-sm" data-action="hamFilter" data-arg0="labelsClear"${data.selected ? '' : ' disabled'} title="Clear every label filter">Clear</button></div>`
        + (data.needsHuman || data.onHold ? `<div class="ham-labels ham-label-dialog-needs-human">${stageTrackerNeedsHumanChipHtml(data.needsHuman)}${stageTrackerOnHoldChipHtml(data.onHold)}</div>` : '')
        + `<div class="ham-labels ham-label-dialog-list" role="group" aria-label="All labels">${stageTrackerLabelDialogListHtml(data)}</div>`;
    }
    function stageTrackerLabelDialogListHtml(data) {
      return data.rows.length ? data.rows.map(stageTrackerLabelChipHtml).join('') : `<div class="ham-empty">No label matches “${esc(data.query)}”</div>`;
    }
    function stageTrackerLabelDialogCountText(data) {
      return (data.rows.length === data.total ? data.total + ' labels' : data.rows.length + ' of ' + data.total + ' labels') + ' · ' + data.selected + ' selected';
    }
    // Redraw the open dialog. While the filter box has focus only the list,
    // the count, the Clear button and the Needs human chip are patched so a
    // poll-driven render never steals the caret.
    function stageTrackerRenderLabelDialog() {
      const body = document.getElementById('activity-monitor-labels-body');
      if (!body || !_stageTracker || !Array.isArray(_stageTracker.items)) return;
      const facets = stageTrackerLabelFacets(_stageTracker.items, _stageTrackerFilter);
      const data = stageTrackerLabelDialogData(facets, _stageTrackerLabelQuery);
      const q = document.getElementById('activity-monitor-labels-q');
      if (!q || document.activeElement !== q) { setIfChanged(body, stageTrackerLabelDialogHtml(data)); return; }
      const list = body.querySelector('.ham-label-dialog-list');
      if (list) setIfChanged(list, stageTrackerLabelDialogListHtml(data));
      const count = body.querySelector('.ham-label-dialog-tools .ham-count');
      if (count && count.textContent !== stageTrackerLabelDialogCountText(data)) count.textContent = stageTrackerLabelDialogCountText(data);
      const clear = body.querySelector('.ham-label-dialog-tools .hv-btn');
      if (clear) clear.disabled = !data.selected;
      const nh = body.querySelector('.ham-label-dialog-needs-human');
      if (nh) setIfChanged(nh, stageTrackerNeedsHumanChipHtml(data.needsHuman) + stageTrackerOnHoldChipHtml(data.onHold));
    }
    var _stageTrackerLabelDialogReturnFocus = null;
    function openStageTrackerLabelDialog() {
      if (document.getElementById('activity-monitor-labels-overlay')) return;
      _stageTrackerLabelDialogReturnFocus = document.activeElement;
      const overlay = document.createElement('div');
      overlay.id = 'activity-monitor-labels-overlay';
      overlay.className = 'config-overlay';
      hiveRememberModalOpen(overlay);
      overlay.innerHTML = `<div class="config-modal overview-settings-modal ham-label-dialog" role="dialog" aria-modal="true" aria-labelledby="activity-monitor-labels-title">
        <div class="config-header">
          <h2 class="config-title" id="activity-monitor-labels-title">All labels</h2>
          <button type="button" class="config-close" data-action="closeStageTrackerLabelDialog" aria-label="Close the label list">&times;</button>
        </div>
        <div class="config-body overview-settings-body modal-scroll" id="activity-monitor-labels-body"></div>
      </div>`;
      overlay.addEventListener('click', e => { if (e.target === overlay) closeStageTrackerLabelDialog(); });
      document.body.appendChild(overlay);
      stageTrackerRenderLabelDialog();
      const q = document.getElementById('activity-monitor-labels-q');
      if (q) setTimeout(() => q.focus(), 0);
    }
    function closeStageTrackerLabelDialog() {
      const overlay = document.getElementById('activity-monitor-labels-overlay');
      if (overlay) overlay.remove();
      _stageTrackerLabelQuery = '';
      const ret = _stageTrackerLabelDialogReturnFocus;
      _stageTrackerLabelDialogReturnFocus = null;
      if (ret && ret.focus && document.contains(ret)) setTimeout(() => ret.focus({ preventScroll: true }), 0);
    }
    function stageTrackerLabelDialogQuery(value) {
      _stageTrackerLabelQuery = String(value || '');
      stageTrackerRenderLabelDialog();
    }

    function stageTrackerPagerHtml(page) {
      if (page.pages <= 1) return '';
      return `<nav class="ham-pager" aria-label="Tracker pages"><button type="button" class="ham-chip" data-action="hamFilter" data-arg0="page" data-arg1="prev"${page.page <= 1 ? ' disabled' : ''} aria-label="Previous page">‹ Prev</button><span>page ${page.page} of ${page.pages}</span><button type="button" class="ham-chip" data-action="hamFilter" data-arg0="page" data-arg1="next"${page.page >= page.pages ? ' disabled' : ''} aria-label="Next page">Next ›</button></nav>`;
    }

    // "updated 12s ago · cache" — re-stamped every STAGE_TRACKER_CLOCK_MS
    // without a fetch; amber once the payload is older than two TTLs or
    // once the poll itself is failing (pollError = short reason, '' = ok).
    // A failing poll also demotes "fresh" to "cache": the payload was built
    // for a request long gone, and the server has not answered since.
    function stageTrackerUpdatedHtml(d, nowMs, pollError, pollErrorSinceMs) {
      const at = Date.parse((d && d.generated_at) || '') || 0;
      if (!at) return '';
      const ageMs = Math.max(0, nowMs - at);
      const ttlMs = (Number(d.cache_ttl_s) || 0) * 1000;
      const failing = !!pollError;
      const stale = failing || (ttlMs > 0 && ageMs > 2 * ttlMs);
      const source = failing ? 'cache' : (d.source || 'cache');
      const src = source === 'fresh' ? 'built for this request' : 'from the server cache';
      const failTip = failing ? '. Polling is failing (' + pollError + (pollErrorSinceMs ? ', since ' + agentNowClockTime(pollErrorSinceMs, nowMs) : '') + '): the dashboard keeps showing this payload until the server answers again' : '';
      const failTag = failing ? ` · <span class="ham-poll-failing">poll failing (${esc(pollError)})</span>` : '';
      return `<span class="ham-updated${stale ? ' ham-stale' : ''}" title="${esc('Payload built ' + agentNowClockTime(at, nowMs) + ' ' + src + (ttlMs ? '; the server rebuilds it at most every ' + Math.round(ttlMs / 1000) + 's and makes no GitHub call per request' : '') + failTip)}">updated ${esc(stageTrackerShortAgo(ageMs) === 'now' ? Math.floor(ageMs / 1000) + 's' : stageTrackerShortAgo(ageMs))} ago · ${esc(source)}${failTag}</span>`;
    }
    // The ADR-0025 central stage engine's status strip: mode and admission
    // anomalies (governor.labels.stage.engine.mode). Hover text on the
    // items only, per the tracker hover rules.
    function stageTrackerEngineHtml(d) {
      const e = d && d.engine;
      if (!e || !e.mode) return '';
      const anomalies = Number(e.anomalies) || 0;
      const modeHelp = { shadow: 'shadow: the engine derives and counts but writes nothing (dry run; stage labels are left as found).', enforce: 'enforce: the engine derives and writes; its admission table counts out-of-order transitions.' }[e.mode] || '';
      return `<span class="ham-engine" data-mode="${esc(e.mode)}">`
        + `<span class="ham-engine-mode" title="${esc('Central stage engine (ADR-0025). ' + modeHelp + ' ' + (Number(e.derivations) || 0) + ' derivations since start. Settings → Labels → Central stage engine mode.')}">engine ${esc(e.mode)}</span>`
        + `<span class="ham-engine-anomaly${anomalies ? ' ham-engine-warn' : ''}" title="${esc('Transition anomalies: stage moves the admission table did not expect (stage_transition_anomaly_total).')}">${esc(anomalies)} anomal${anomalies === 1 ? 'y' : 'ies'}</span>`
        + '</span>';
    }
    function stageTrackerTickClock() {
      const el = document.querySelector('#activity-monitor-meta .ham-updated');
      if (!el || !_stageTracker) return;
      const wrap = document.createElement('span');
      wrap.innerHTML = stageTrackerUpdatedHtml(_stageTracker, Date.now(), _stageTrackerPollError, _stageTrackerPollErrorSince);
      if (wrap.firstChild) el.replaceWith(wrap.firstChild);
    }
    function stageTrackerPollFailed(reason) {
      if (!_stageTrackerPollError) _stageTrackerPollErrorSince = Date.now();
      _stageTrackerPollError = reason;
      stageTrackerTickClock();
    }
    function stageTrackerPollOk() {
      _stageTrackerPollError = '';
      _stageTrackerPollErrorSince = 0;
    }

    function renderStageTracker(data) {
      if (data && typeof data === 'object') { _stageTracker = data; stageTrackerNoteMerges(data); stageTrackerNoteOpened(data); stageTrackerNoteMoves(data); }
      // Row-action feedback rides the payload: a result toast leaves and a
      // row stops "acting" once the item it targets reads a new state.
      if (data && typeof data === 'object' && typeof stageTrackerResultToastsSync === 'function') { stageTrackerNoteEvents(data); stageTrackerResultToastsSync(data); stageTrackerPendingExpire(data); }
      const el = document.getElementById('activity-monitor-panel');
      if (!el) return;
      const nowMs = Date.now();
      // Rows under a "Merging" hold the payload already dropped ride along (#15245).
      const d = typeof stageTrackerMergingHoldView === 'function' ? stageTrackerMergingHoldView(_stageTracker, _stageTrackerMergingHold, nowMs) : _stageTracker;
      if (!d || d.status === 'initializing' || !Array.isArray(d.items)) {
        setIfChanged(el, '<div class="ham-empty">Waiting for the first scan…</div>');
        return;
      }
      const f = _stageTrackerFilter = stageTrackerNormalizeFilter(_stageTrackerFilter);
      const stages = d.stages || [];
      const repos = Array.from(new Set(d.items.map(it => it.repo).filter(Boolean))).sort();
      const sel = document.getElementById('activity-monitor-repo');
      if (sel) {
        const want = '<option value="">All repos</option>' + repos.map(r => `<option value="${esc(r)}"${r === f.repo ? ' selected' : ''}>${esc(r)}</option>`).join('');
        if (sel.innerHTML !== want) sel.innerHTML = want;
      }
      stageTrackerSyncToolbar(f);
      const ctx = { nowMs, stages, moves: _stageTrackerMoves };
      ctx.workers = stageTrackerWorkers(window._lastStatus);
      ctx.events = _stageTrackerEvents;
      ctx.noteTuck = stageTrackerNoteTuckSetting();
      ctx.sharedPRs = stageTrackerSharedPRs(d.items);
      ctx.items = d.items;
      // Repo chips ride the setting against how many repos the payload spans.
      if (stageTrackerRepoChipOn(stageTrackerRepoChipSetting(), repos.length)) ctx.repoChips = { primary: stageTrackerPrimaryRepo(), active: f.repo };
      const visible = stageTrackerVisible(d.items, f, ctx);
      const ordered = stageTrackerTrainContiguous(stageTrackerSort(visible, stages, f));
      const grouping = _stageTrackerGroup === 'repo';
      const page = grouping
        ? stageTrackerGroupPage(stageTrackerGroups(ordered, { primary: stageTrackerPrimaryRepo(), collapsed: _stageTrackerCollapsed, stages }), f)
        : stageTrackerPage(ordered, f);
      if (page.page !== f.page) f.page = page.page;
      // Attention is counted over the filtered set, nested PRs included.
      const filteredAll = d.items.filter(it => stageTrackerMatches(it, f, ctx));
      const attention = filteredAll.filter(stageTrackerNeedsAttention).length;
      // "N filtered out" and the sidebar pill count against the default
      // view's rows (stageTrackerRowCount), so drawing a PR on its issue's
      // row is never reported as a filter and the pill holds still while
      // the kind tabs or chips narrow the list.
      const allRows = stageTrackerFilterActive(f) ? stageTrackerRowCount(d.items, ctx) : visible.length;
      // The sidebar pill, the header lead and the "of N" line come out of
      // one stageTrackerCountsText call per render, so they cannot
      // disagree: the pill is the default view's row count (owner: rows,
      // not items — a covering PR shares its issue's row); needs-attention
      // is the warn-coloured header suffix and the pill's tooltip, counted
      // over every item regardless of filter.
      const attentionAll = d.items.filter(stageTrackerNeedsAttention).length;
      const counts = stageTrackerCountsText(page, allRows, attention, stageTrackerOpenCounts(d), attentionAll, stageTrackerFoldedCount(d.items, ctx));
      setSectionSummary('activity-monitor-section', counts.header.replace(/(\d+) needs? attention/, m => `<span class="text-warn">${m}</span>`));
      const badge = document.getElementById('activity-monitor-sidebar-badge');
      if (badge) { badge.textContent = String(counts.badge); badge.title = counts.badgeTitle; if (counts.badge) badge.removeAttribute('data-zero'); else badge.setAttribute('data-zero', ''); }
      stageTrackerRenderWaiting(d, nowMs);
      const labelsEl = document.getElementById('activity-monitor-labels');
      if (labelsEl) setIfChanged(labelsEl, stageTrackerLabelsHtml(stageTrackerLabelFacets(d.items, f)));
      stageTrackerRenderLabelDialog();
      const meta = document.getElementById('activity-monitor-meta');
      if (meta) setIfChanged(meta, `<span class="ham-count"${counts.note ? ` title="${esc(counts.note)}"` : ''}>${esc(counts.showing)}</span>${stageTrackerEngineHtml(d)}${stageTrackerUpdatedHtml(d, nowMs, _stageTrackerPollError, _stageTrackerPollErrorSince)}${stageTrackerPagerHtml(page)}`);
      const foot = document.getElementById('activity-monitor-meta-foot');
      if (foot) setIfChanged(foot, page.pages > 1 ? `<span class="ham-count">${esc(counts.showing)}</span>${stageTrackerPagerHtml(page)}` : '');
      const effect = stageTrackerMotionEffect(stageTrackerMotionSetting(), stageTrackerReducedMotion());
      if (!visible.length && !Object.keys(_stageTrackerCelebrating).length) {
        _stageTrackerMoves = {};
        stageTrackerSelectionSync(el, []);
        const emptyHtml = `<div class="ham-empty">${d.items.length ? 'Nothing matches the current filters.' : 'Nothing open is being tracked right now.'}</div>`;
        // The counter goes back to its toolbar home before the header
        // (and its slot) leaves with the rows.
        if (el.querySelector('[data-ham-key]')) {
          stageTrackerReconcile(el, [], { effect, hidden: document.hidden });
          // Departing rows collapse first; the empty line follows them out.
          if (el.querySelector('.ham-leaving')) { setTimeout(() => { if (!el.querySelector('[data-ham-key]')) setIfChanged(el, emptyHtml); }, STAGE_TRACKER_EXIT_MS * 1.5); return; }
        }
        setIfChanged(el, emptyHtml);
        return;
      }
      // Grouped: header rows keyed group:owner/repo sit between the item
      // rows; the reconcile treats them like any keyed row (kept while
      // unchanged, collapsed out with their last row).
      // A row celebrating its merge is pinned past the payload that
      // dropped it (stageTrackerPinnedRows) until its moment is over.
      // A merge train's rows carry a header line and a rail (#15117); the
      // row renderer sees its neighbours to close the rail on the last one.
      const pageItems = stageTrackerAdoptOrphanPRs(page.rows.map(e => grouping ? e.item : e));
      // Every train on the page, so a queue word can name its train when
      // there is more than one and each train's rows share a rail accent.
      const trains = typeof stageTrackerTrainIndex === 'function' ? stageTrackerTrainIndex(pageItems) : { count: 0, index: {} };
      const itemRow = (it, i) => {
        const near = Object.assign({}, ctx, { prevRow: i > 0 && pageItems[i - 1] ? pageItems[i - 1] : null, nextRow: pageItems[i + 1] ? pageItems[i + 1] : null, trains: trains.count, trainIndex: trains.index });
        return { key: stageTrackerItemKey(it), html: stageTrackerRowHtml(it, stages, near) };
      };
      const rowOf = (e, i) => grouping && !e.item
        ? { key: e.group.key, html: stageTrackerGroupHeaderHtml(e.group, stages, ctx) }
        : itemRow(pageItems[i], i);
      const rows = stageTrackerPinnedRows(el, [{ key: STAGE_TRACKER_HEADER_KEY, html: stageTrackerHeaderHtml(stages) }]
        .concat(stageTrackerTrainRows(page.rows, rowOf, typeof stageTrackerTrainWhere === 'function' ? stageTrackerTrainWhere(ordered, d.items) : null)), nowMs);
      const rec = stageTrackerReconcile(el, rows, { effect, hidden: document.hidden });
      if (rec.deferred) stageTrackerScheduleSettle();
      // Selection is keyed on the row, never baked into its HTML, so a kept
      // node stays selected across the refresh and a departed row leaves it.
      stageTrackerSelectionSync(el, rows.map(r => r.key));
      if (typeof stageTrackerPendingSync === 'function') stageTrackerPendingSync(el);
      stageTrackerCueAgeSync(el, nowMs);
      stageTrackerCueTick();
      // Moves play once, on the render that follows the payload; a filter
      // or paging re-render of the same payload does not replay them.
      if (Object.keys(_stageTrackerMoves).length) { stageTrackerAnimateMoves(el); _stageTrackerMoves = {}; }
    }

    // Stateful: reduce the payload to columns, diff against the previous
    // one, and keep the moves for the render that follows.
    function stageTrackerNoteMoves(d) {
      const next = stageTrackerColMap(d && d.items, d && d.stages);
      _stageTrackerMoves = stageTrackerColMoves(_stageTrackerCols, next);
      _stageTrackerCols = next;
    }

    // The endpoint is served from a server-side cache with an ETag; a poll
    // that matches it costs one 304 and no re-render. The filter/sort state
    // is restored from storage on the first fetch.
    // SSE nudges arrive once per status publish and once per webhook
    // delivery, uncoalesced: a busy hive sent ~36 a minute, each a refetch
    // and a reconcile of the list. The first nudge in a window fetches at
    // once; the rest collapse into one trailing fetch when the window ends.
    // A nudge that lands while the tab is hidden is remembered and served
    // on the next visibilitychange instead.
    const STAGE_TRACKER_NUDGE_COALESCE_MS = 2000;
    let _stageTrackerNudgeTimer = null;
    let _stageTrackerNudgeAt = 0;
    let _stageTrackerNudgeSkipped = false;
    function stageTrackerNudge(now) {
      now = typeof now === 'number' ? now : Date.now();
      if (document.hidden) { _stageTrackerNudgeSkipped = true; return false; }
      const wait = _stageTrackerNudgeAt + STAGE_TRACKER_NUDGE_COALESCE_MS - now;
      if (wait <= 0) { _stageTrackerNudgeAt = now; fetchStageTracker(); return true; }
      if (!_stageTrackerNudgeTimer) {
        _stageTrackerNudgeTimer = setTimeout(() => {
          _stageTrackerNudgeTimer = null;
          _stageTrackerNudgeAt = Date.now();
          if (document.hidden) { _stageTrackerNudgeSkipped = true; return; }
          fetchStageTracker();
        }, wait);
      }
      return false;
    }
    function stageTrackerResumeIfSkipped() {
      if (!_stageTrackerNudgeSkipped) return;
      _stageTrackerNudgeSkipped = false;
      _stageTrackerNudgeAt = Date.now();
      fetchStageTracker();
    }

    async function fetchStageTracker() {
      try {
        if (!_stageTrackerSig && !_stageTrackerEtag) stageTrackerLoadGrouping();
        if (!_stageTrackerSig && !_stageTrackerEtag) { stageTrackerLoadFilter(); stageTrackerSoundInit(); }
        if (!_stageTrackerSig && !_stageTrackerEtag) stageTrackerApplyUrlFilter();
        const r = await fetch('/api/stage-tracker', _stageTrackerEtag ? { headers: { 'If-None-Match': _stageTrackerEtag } } : undefined);
        if (r.status === 304) {
          // A 304 carries no body, so take generated_at/source from the headers or the "updated" chip goes amber on a quiet hive.
          const at = r.headers.get('X-Hive-Stage-Tracker-Generated-At');
          const src = r.headers.get('X-Hive-Stage-Tracker-Source');
          if (_stageTracker) {
            if (at) _stageTracker.generated_at = at;
            if (src) _stageTracker.source = src;
          }
          stageTrackerPollOk();
          stageTrackerTickClock();
          return;
        }
        if (!r.ok) { stageTrackerPollFailed('HTTP ' + r.status); return; }
        _stageTrackerEtag = r.headers.get('ETag') || '';
        const text = await r.text();
        stageTrackerPollOk();
        if (text === _stageTrackerSig) { stageTrackerTickClock(); return; }
        _stageTrackerSig = text;
        renderStageTracker(JSON.parse(text));
        dashboardSetInterval('stage-tracker-clock', stageTrackerTickClock, STAGE_TRACKER_CLOCK_MS);
      } catch (e) {
        // Network failure or an aborted body: keep the previous render but
        // say so in the "updated" chip instead of letting it read "fresh".
        stageTrackerPollFailed('unreachable');
      }
    }

    /* ---- Row actions: context menu + multi-select -------------------------
       A right-click (or the ⋯ kebab, Shift+F10, the context-menu key) on a
       row opens an in-app menu of the actions that apply to that row's
       state. The checkbox in the left gutter selects rows (Shift+click for
       a range, Cmd/Ctrl+click to toggle); the selection bar above the list
       carries the actions valid for every selected row and greys out the
       rest with the reason. Selection is keyed on the row's item key, so
       the ~1 s reconcile keeps it; rows that leave the list drop out of it.
       Every action goes through an existing dashboard endpoint (hold,
       send-to-human, rescan) or the decision endpoint, which records the
       same /hive approve|decision a maintainer would — nothing merges. */
    const STAGE_TRACKER_DECISION_STAGES = [45, 85];
    const STAGE_TRACKER_SEL_ACTIONS = ['hold', 'release', 'retry', 'needs-human', 'copy'];
    const STAGE_TRACKER_MENU_MARGIN = 4;
    const STAGE_TRACKER_MENU_FAIL_LINES = 3;
    // One source of plain words for every decision rule / action / reason
    // (mirrors pkg/delegatedecision.GlossaryJSON(); stage_tracker_glossary_test.go
    // fails when they drift). Rule slugs are never shown bare: use
    // stageTrackerPlainWords(slug). hivecommons/hive#14822.
    const STAGE_TRACKER_DECISION_GLOSSARY = {
      "accept-proposal": {"kind": "action", "plain": "accepted the proposal as written", "detail": "The proposal is accepted as written with its stated defaults."},
      "accept-ratchet": {"kind": "action", "plain": "accepted as a ratchet", "detail": "Accepted as a ratchet: baseline recorded now, failures only on increases."},
      "adopt": {"kind": "action", "plain": "hive takes over finishing this PR", "detail": "Adopt means the hive takes over finishing this PR: it rebases, fixes CI and merges it, keeping the author's commits and sign-off."},
      "adopt-supersede": {"kind": "action", "plain": "close this PR and open a fresh hive-authored PR with the same commits", "detail": "Supersede means the hive closes this PR and opens a fresh hive-authored PR carrying the same commits and sign-off, then finishes it itself."},
      "adopted": {"kind": "action", "plain": "close this PR and open a fresh hive-authored PR with the same commits", "detail": "Supersede means the hive closes this PR and opens a fresh hive-authored PR carrying the same commits and sign-off, then finishes it itself."},
      "adoption-disabled": {"kind": "reason", "plain": "adoption is switched off on this hive", "detail": "Stage adoption is disabled on this hive (governor.labels.stage.adoption), so a request to take a PR over is never carried out."},
      "adoption-executing": {"kind": "reason", "plain": "the adoption engine is carrying it out", "detail": "The decision is recorded and the adoption engine picks it up on its next stage pass (a few minutes): it pushes the commits to a hive branch and opens the hive's copy. Nothing to do yet."},
      "adoption-stalled:contributor-lane:workflow-path": {"kind": "reason", "plain": "the PR edits .github/workflows; the hive's App lacks the `workflows` permission, so it cannot adopt it", "detail": "The PR changes files under .github/workflows. The hive's GitHub App has no `workflows` permission, so the adoption fence refuses to publish those edits from the hive. Grant the App the `workflows` permission and re-run the decision, or have a person rebase and merge it on GitHub, or close it as superseded."},
      "adoption-undone": {"kind": "reason", "plain": "the hive's copy was closed without merging and the adoption was undone", "detail": "The hive opened its copy, but the copy was closed without merging and a fresh one could not be opened, so the adoption was undone and the PR is back in its normal flow. Re-run the decision to adopt it again, or act on GitHub."},
      "adoption/supersede": {"kind": "action", "plain": "close this PR and open a fresh hive-authored PR with the same commits", "detail": "Supersede means the hive closes this PR and opens a fresh hive-authored PR carrying the same commits and sign-off, then finishes it itself."},
      "already-fixed": {"kind": "rule", "plain": "the issue was already fixed by a merged PR; closed as superseded", "detail": "The issue this PR fixes is already closed and a merged PR closed it (or a merged PR declares it supersedes this one), so nothing is left to deliver. The hive closes the PR as superseded with a note naming the merging PR; it never adopts it."},
      "approve": {"kind": "action", "plain": "approved for merge", "detail": "The hive approved it; the merge queue merges it once every check passes.", "issue_plain": "approved to proceed — an agent may claim it", "issue_detail": "The hive approved the direction; the issue leaves Needs decision and an agent may claim it. Nothing is merged."},
      "approve-review": {"kind": "action", "plain": "approved for merge", "detail": "The hive approved it; the merge queue merges it once every check passes.", "issue_plain": "approved to proceed — an agent may claim it", "issue_detail": "The hive approved the direction; the issue leaves Needs decision and an agent may claim it. Nothing is merged."},
      "approved": {"kind": "action", "plain": "approved for merge", "detail": "The hive approved it; the merge queue merges it once every check passes.", "issue_plain": "approved to proceed — an agent may claim it", "issue_detail": "The hive approved the direction; the issue leaves Needs decision and an agent may claim it. Nothing is merged."},
      "author-association": {"kind": "reason", "plain": "no merge lane vouches for this PR's author; a maintainer merges", "detail": "The PR is ready, but its author is not a collaborator and holds no trusted or merger tier, so no merge lane vouches for them and the hive never merges it unattended. A maintainer approves it (Approve and let hive merge — hive merges on the next tick once CI is green — or Merge now as maintainer), or trusts the author under 👥 Contributors."},
      "author-online": {"kind": "reason", "plain": "the author's clanker or hive is connected, so hive won't take the PR over", "detail": "Adoption is for authors whose clanker or hive has gone offline; this author's is connected, so the hive leaves the PR with them."},
      "behind": {"kind": "reason", "plain": "the branch is behind the base branch", "detail": "The branch is behind the base branch and must be updated before it can merge."},
      "blocker": {"kind": "reason", "plain": "the hive can't do this itself", "detail": "The item is blocked on something the hive cannot do: a permission the GitHub App lacks, or a step only a person may take. No decision clears it; grant the permission (and re-check) or do the step by hand (hivecommons/hive#15099)."},
      "capability": {"kind": "reason", "plain": "the hive can't do this itself", "detail": "The item is blocked on something the hive cannot do: a permission the GitHub App lacks, or a step only a person may take. No decision clears it; grant the permission (and re-check) or do the step by hand (hivecommons/hive#15099)."},
      "capability-gap": {"kind": "rule", "plain": "the GitHub App lacks a permission; deciding cannot clear it", "detail": "The blocker is a missing GitHub App permission (for example `workflows`). Approving a direction would only re-apply needs-human, so the park stays until the owner grants the permission or a person applies the change."},
      "capability:workflows-permission": {"kind": "reason", "plain": "the PR edits .github/workflows; the hive's App lacks the `workflows` permission, so it cannot adopt it", "detail": "The PR changes files under .github/workflows. The hive's GitHub App has no `workflows` permission, so the adoption fence refuses to publish those edits from the hive. Grant the App the `workflows` permission and re-run the decision, or have a person rebase and merge it on GitHub, or close it as superseded."},
      "changes-requested": {"kind": "reason", "plain": "a reviewer asked for changes; a new commit is needed", "detail": "A reviewer asked for changes on this branch. Approving cannot override that: only a new commit, or a maintainer merging by hand, moves it."},
      "changes_requested": {"kind": "reason", "plain": "a reviewer asked for changes; a new commit is needed", "detail": "A reviewer asked for changes on this branch. Approving cannot override that: only a new commit, or a maintainer merging by hand, moves it."},
      "checks-failing": {"kind": "reason", "plain": "CI is failing on this branch", "detail": "Required checks are failing on this branch; nothing merges until they are green."},
      "ci-failing": {"kind": "reason", "plain": "CI is failing on this branch", "detail": "Required checks are failing on this branch; nothing merges until they are green."},
      "ci-ratchet": {"kind": "rule", "plain": "a CI hygiene gate, landed as a ratchet", "detail": "A new CI hygiene gate lands as a ratchet: it records today's baseline and fails only on increases, so existing work is never blocked."},
      "ci-red": {"kind": "reason", "plain": "CI is failing on this branch", "detail": "Required checks are failing on this branch; nothing merges until they are green."},
      "close-as-duplicate": {"kind": "action", "plain": "closed as a duplicate", "detail": "Closed because an older item already tracks the same work."},
      "close-duplicate": {"kind": "action", "plain": "closed as a duplicate", "detail": "Closed because an older item already tracks the same work."},
      "close-not-now": {"kind": "action", "plain": "declined for now; closed as not planned", "detail": "The hive declined the proposal for now and closed it as not planned, with the reason and what evidence would reopen it."},
      "close-superseded": {"kind": "action", "plain": "closed because another PR carries the work", "detail": "Closed because another PR carries the same work; the author is credited there.", "issue_plain": "closed because another issue carries the work", "issue_detail": "Closed because another issue already tracks the same work; the discussion continues there."},
      "closed": {"kind": "action", "plain": "closed because another PR carries the work", "detail": "Closed because another PR carries the same work; the author is credited there.", "issue_plain": "closed because another issue carries the work", "issue_detail": "Closed because another issue already tracks the same work; the discussion continues there."},
      "comment-decision": {"kind": "action", "plain": "recorded the decision on GitHub", "detail": "The hive posted the decision as a `/hive decision` comment, the way a maintainer would."},
      "conflict": {"kind": "reason", "plain": "the branch conflicts with the base branch", "detail": "The branch has merge conflicts with the base branch; it needs a rebase before anything can merge."},
      "conflicting": {"kind": "reason", "plain": "the branch conflicts with the base branch", "detail": "The branch has merge conflicts with the base branch; it needs a rebase before anything can merge."},
      "contributor-lane:author-association": {"kind": "reason", "plain": "no merge lane vouches for this PR's author; a maintainer merges", "detail": "The PR is ready, but its author is not a collaborator and holds no trusted or merger tier, so no merge lane vouches for them and the hive never merges it unattended. A maintainer approves it (Approve and let hive merge — hive merges on the next tick once CI is green — or Merge now as maintainer), or trusts the author under 👥 Contributors."},
      "contributor-lane:changes-requested": {"kind": "reason", "plain": "a reviewer asked for changes; a new commit is needed", "detail": "A reviewer asked for changes on this branch. Approving cannot override that: only a new commit, or a maintainer merging by hand, moves it."},
      "contributor-lane:sentinel-blocked": {"kind": "reason", "plain": "security-sensitive files changed; a maintainer must approve", "detail": "Security-sensitive files changed. The hive never merges until a maintainer reviews and removes the sentinel label."},
      "contributor-lane:workflow-path": {"kind": "reason", "plain": "the PR edits .github/workflows; the hive's App lacks the `workflows` permission, so it cannot adopt it", "detail": "The PR changes files under .github/workflows. The hive's GitHub App has no `workflows` permission, so the adoption fence refuses to publish those edits from the hive. Grant the App the `workflows` permission and re-run the decision, or have a person rebase and merge it on GitHub, or close it as superseded."},
      "contributor-offline": {"kind": "rule", "plain": "the author hasn't been reachable since the nudge", "detail": "The PR's author (or their clanker or hive) has been offline since the hive nudged them, so the hive takes the work over instead of waiting. The decision itself names which: the author's clanker, a connected hive, or the author."},
      "dco": {"kind": "reason", "plain": "a commit is missing its sign-off", "detail": "A commit on the branch is missing its Developer Certificate of Origin sign-off; the author must sign it off."},
      "dco-missing": {"kind": "reason", "plain": "a commit is missing its sign-off", "detail": "A commit on the branch is missing its Developer Certificate of Origin sign-off; the author must sign it off."},
      "dedupe": {"kind": "rule", "plain": "an older issue already tracks this", "detail": "An older issue already tracks the same work; one thread keeps it moving."},
      "default-approve": {"kind": "rule", "plain": "no special rule applies; the hive took its own recommendation", "detail": "No owner rule singles this issue out, so the hive went with its own recommendation — the reversible choice that keeps work moving."},
      "default-release": {"kind": "rule", "plain": "no special rule applies; a green branch is approved", "detail": "No owner rule holds this PR back; its branch is green and a merge is reversible, so it is approved."},
      "design-proposal": {"kind": "rule", "plain": "a proposal with clear acceptance criteria, accepted as written", "detail": "The proposal states concrete acceptance criteria, so it is accepted as written; anything not stated keeps today's behaviour."},
      "directed": {"kind": "action", "plain": "told the author agent what to change", "detail": "The hive recorded direction for the author agent, which pushes a new commit with that change.", "issue_plain": "gave direction on the issue", "issue_detail": "The hive recorded direction on the issue; the agent that claims it works from that direction."},
      "direction": {"kind": "action", "plain": "told the author agent what to change", "detail": "The hive recorded direction for the author agent, which pushes a new commit with that change.", "issue_plain": "gave direction on the issue", "issue_detail": "The hive recorded direction on the issue; the agent that claims it works from that direction."},
      "dirty": {"kind": "reason", "plain": "the branch conflicts with the base branch", "detail": "The branch has merge conflicts with the base branch; it needs a rebase before anything can merge."},
      "effect-unknown": {"kind": "reason", "plain": "the hive did not say why", "detail": "The hive could not tell from the lane reason why the decision has not taken effect."},
      "fix-then-merge": {"kind": "rule", "plain": "the branch isn't mergeable yet", "detail": "The branch cannot merge as it is (conflicts, red CI or a missing sign-off); the hive told the author agent exactly what to fix."},
      "flaky-test": {"kind": "rule", "plain": "a flaky test, quarantined with a retry", "detail": "The failure is a flaky test: it is quarantined with a retry and a separate deflake task is filed, so it blocks nothing."},
      "hive-policy": {"kind": "rule", "plain": "this changes the hive's own rules", "detail": "This changes the hive's own settings, governance or security posture; the hive only recommends and the owner confirms."},
      "hold": {"kind": "reason", "plain": "on hold; the hive does nothing until it is released", "detail": "The PR is on hold. The hive does nothing with it until a person releases the hold.", "issue_detail": "The issue is on hold. No agent claims it until a person releases the hold."},
      "human-changes-requested": {"kind": "reason", "plain": "a reviewer asked for changes; a new commit is needed", "detail": "A reviewer asked for changes on this branch. Approving cannot override that: only a new commit, or a maintainer merging by hand, moves it."},
      "human-merge": {"kind": "reason", "plain": "no merge lane vouches for this PR's author; a maintainer merges", "detail": "The PR is ready, but its author is not a collaborator and holds no trusted or merger tier, so no merge lane vouches for them and the hive never merges it unattended. A maintainer approves it (Approve and let hive merge — hive merges on the next tick once CI is green — or Merge now as maintainer), or trusts the author under 👥 Contributors."},
      "human-only": {"kind": "reason", "plain": "the hive can't do this itself", "detail": "The item is blocked on something the hive cannot do: a permission the GitHub App lacks, or a step only a person may take. No decision clears it; grant the permission (and re-check) or do the step by hand (hivecommons/hive#15099)."},
      "label:needs-human": {"kind": "reason", "plain": "marked needs-human; a person must remove the label", "detail": "The item is marked needs-human: automated fixes ran out or a person flagged it. The hive waits until the label is removed."},
      "labels-add": {"kind": "action", "plain": "added labels", "detail": "The hive added labels to the item."},
      "labels-remove": {"kind": "action", "plain": "removed labels", "detail": "The hive removed labels from the item."},
      "link": {"kind": "action", "plain": "cross-linked a related item", "detail": "The hive left a comment linking a related issue or PR."},
      "llm-delegate": {"kind": "rule", "plain": "decided by the hive's model under the owner's rules", "detail": "No mechanical rule applied, so the hive's model decided under the owner's standing rules; the same safety rails applied."},
      "mergeable-as-is": {"kind": "rule", "plain": "mergeable and green as it is; handed to the merge queue", "detail": "The head is mergeable with green CI, so no adoption is needed: the hive approves it and the merge queue takes it as it is."},
      "needs-human": {"kind": "reason", "plain": "marked needs-human; a person must remove the label", "detail": "The item is marked needs-human: automated fixes ran out or a person flagged it. The hive waits until the label is removed."},
      "no-effect": {"kind": "reason", "plain": "the decision hasn't taken effect", "detail": "The hive recorded this decision, but what it was supposed to cause has not happened within the expected time."},
      "no-new-commit": {"kind": "reason", "plain": "no new commit has been pushed", "detail": "The hive gave direction, but no new commit has been pushed yet."},
      "not-closed-yet": {"kind": "reason", "plain": "the close didn't land", "detail": "The hive decided to close it, but it is still open; the close call did not land."},
      "not-merged-yet": {"kind": "reason", "plain": "the merge queue hasn't merged it", "detail": "The hive approved it but the merge queue has not merged it; its lane reason says what still blocks it."},
      "not-now": {"kind": "action", "plain": "declined for now; closed as not planned", "detail": "The hive declined the proposal for now and closed it as not planned, with the reason and what evidence would reopen it."},
      "operator-action": {"kind": "action", "plain": "told the spoke owner what to do; nothing for an agent here", "detail": "The hive named the spoke owner and the one action they take (for example re-authenticate the provider). The item stays parked, gets no approved-direction, and clears when the report stops recurring."},
      "one-pr-per-issue": {"kind": "rule", "plain": "two PRs fix the same issue; the merge queue keeps one", "detail": "Two open PRs declare the same closing issue. The merge queue carries one PR per issue — a green head first, then the earlier-opened — and the other closes as superseded with credit."},
      "owner-only": {"kind": "rule", "plain": "only the hive owner can do this", "detail": "This needs a secret, token or organization permission that only the hive owner holds; the hive cannot do it for you."},
      "owner_only": {"kind": "rule", "plain": "only the hive owner can do this", "detail": "This needs a secret, token or organization permission that only the hive owner holds; the hive cannot do it for you."},
      "park-label-stuck": {"kind": "reason", "plain": "stage/045-needs-decision is still on the issue; the scanner skips it until that label clears", "detail": "The hive approved the issue and applied approved-direction, but the stage/045-needs-decision park label was never removed, so the scanner still skips the issue. Lift the park (it removes the label through the hive's audited label path) or re-evaluate the issue so the stage engine re-reads it."},
      "prefer-contributor": {"kind": "rule", "plain": "a contributor's PR and a hive PR do the same work", "detail": "A contributor's PR and a hive-authored PR cover the same work; the hive prefers the contributor's when it is mergeable and current."},
      "proposal-approve-evidence": {"kind": "rule", "plain": "a proposal backed by an incident or a repeated class; approved", "detail": "The hive-filed proposal names an incident it would have prevented, or prior instances of the same class fixed one at a time, so its cost is justified and it is approved as proposed."},
      "proposal-not-now": {"kind": "rule", "plain": "a hive-filed proposal with no incident or recurrence behind it; declined for now", "detail": "The proposal shows neither an incident it would have prevented nor a repeated class it would end, so its value is speculative. The hive declines it for now and closes it as not planned; re-propose with evidence."},
      "quarantine-flaky": {"kind": "action", "plain": "quarantined the flaky test", "detail": "The flaky test is quarantined with a retry and a deflake task is filed."},
      "rail-hold": {"kind": "rule", "plain": "the item is on hold", "detail": "The item carries the hold label; the hive never acts on a held item until a person releases the hold."},
      "ready-for-human-merge": {"kind": "reason", "plain": "no merge lane vouches for this PR's author; a maintainer merges", "detail": "The PR is ready, but its author is not a collaborator and holds no trusted or merger tier, so no merge lane vouches for them and the hive never merges it unattended. A maintainer approves it (Approve and let hive merge — hive merges on the next tick once CI is green — or Merge now as maintainer), or trusts the author under 👥 Contributors."},
      "rebase-then-merge": {"kind": "rule", "plain": "only conflicts block it; a rebase then merge", "detail": "Conflicts are the only blocker. The unattended rebase-then-merge path lands with hivecommons/hive#15104; until then the ladder records this rung and moves on."},
      "recommend-only": {"kind": "action", "plain": "recommended only; a person confirms", "detail": "The hive only recommended; the item stays parked until a person confirms."},
      "recommended": {"kind": "action", "plain": "recommended only; a person confirms", "detail": "The hive only recommended; the item stays parked until a person confirms."},
      "red": {"kind": "reason", "plain": "CI is failing on this branch", "detail": "Required checks are failing on this branch; nothing merges until they are green."},
      "refused": {"kind": "action", "plain": "did not decide", "detail": "The hive did not decide this item; its safety rails stopped it (for example, the item is on hold)."},
      "reporter-trust": {"kind": "rule", "plain": "the hold on an unknown reporter is cleared because the fix is sound", "detail": "The PR was held until the reporter's trust was known; its fix is green and mergeable, so the hold is cleared."},
      "request_changes": {"kind": "reason", "plain": "a reviewer asked for changes; a new commit is needed", "detail": "A reviewer asked for changes on this branch. Approving cannot override that: only a new commit, or a maintainer merging by hand, moves it."},
      "requires_human": {"kind": "reason", "plain": "marked needs-human; a person must remove the label", "detail": "The item is marked needs-human: automated fixes ran out or a person flagged it. The hive waits until the label is removed."},
      "return-to-author": {"kind": "rule", "plain": "a person pushed this PR; the hive asked them, not adopted it", "detail": "The PR was pushed by a person (author/human or one of this hive's owners), so the hive never adopts it. It told the author by name what to fix (rebase, sign-off, CI) and relayed the reviewer's question."},
      "return-to-contributor": {"kind": "rule", "plain": "the author's clanker or hive is connected; returned to them with the fix list", "detail": "The author's clanker or hive is connected (or has picked the rework up), so the hive hands the PR back to them with the exact fix list instead of adopting it."},
      "review-backlog": {"kind": "rule", "plain": "the PR closed without merging, so the issue stays open", "detail": "The fixing PR closed unmerged; the issue stays open and is re-triaged rather than auto-closed."},
      "reviewer-verdict:request_changes": {"kind": "reason", "plain": "a reviewer asked for changes; a new commit is needed", "detail": "A reviewer asked for changes on this branch. Approving cannot override that: only a new commit, or a maintainer merging by hand, moves it."},
      "self-decision-read-as-changes": {"kind": "reason", "plain": "hive's own decision comment was read as a reviewer asking for changes", "detail": "The merge queue read the hive's own `/hive decision` comment as a reviewer asking for changes, so the decision blocked the PR it meant to advance (hivecommons/hive#14822). A new commit or a maintainer merge clears it."},
      "selfreport-operator-action": {"kind": "rule", "plain": "a spoke self-report only its operator can fix; not code work", "detail": "The issue is a fleet self-report (expired login, credentials, disk, network, App setup) whose remedy is on the reporting spoke. The hive tells that spoke's owner what to do and keeps it out of the actionable set; no agent is sent to code."},
      "sentinel": {"kind": "reason", "plain": "security-sensitive files changed; a maintainer must approve", "detail": "Security-sensitive files changed. The hive never merges until a maintainer reviews and removes the sentinel label."},
      "sentinel-alert": {"kind": "reason", "plain": "security-sensitive files changed; a maintainer must approve", "detail": "Security-sensitive files changed. The hive never merges until a maintainer reviews and removes the sentinel label."},
      "sentinel-blocked": {"kind": "reason", "plain": "security-sensitive files changed; a maintainer must approve", "detail": "Security-sensitive files changed. The hive never merges until a maintainer reviews and removes the sentinel label."},
      "sentinel-trusted": {"kind": "rule", "plain": "security-sensitive files changed, but the author is trusted", "detail": "Security-sensitive files changed; because the author is trusted the hive approved, but a person must still remove the sentinel label before it merges."},
      "sentinel-untrusted": {"kind": "rule", "plain": "security-sensitive files changed; a maintainer must approve", "detail": "Security-sensitive files changed in a PR from an author without write access; a maintainer must review and approve it."},
      "small-low-risk": {"kind": "rule", "plain": "a small, low-risk change on a green branch", "detail": "The change is small and low-risk (docs, CI pins, text, workflow hygiene) and CI is green, so it is approved."},
      "still-parked": {"kind": "reason", "plain": "the item is still waiting on a person", "detail": "The item is still parked for a person's decision; the recorded decision did not move it."},
      "supersede": {"kind": "action", "plain": "close this PR and open a fresh hive-authored PR with the same commits", "detail": "Supersede means the hive closes this PR and opens a fresh hive-authored PR carrying the same commits and sign-off, then finishes it itself."},
      "supersede-now": {"kind": "action", "plain": "close this PR and open a fresh hive-authored PR with the same commits", "detail": "Supersede means the hive closes this PR and opens a fresh hive-authored PR carrying the same commits and sign-off, then finishes it itself."},
      "superseded": {"kind": "action", "plain": "closed because another PR carries the work", "detail": "Closed because another PR carries the same work; the author is credited there.", "issue_plain": "closed because another issue carries the work", "issue_detail": "Closed because another issue already tracks the same work; the discussion continues there."},
      "untrusted-author": {"kind": "reason", "plain": "no merge lane vouches for this PR's author; a maintainer merges", "detail": "The PR is ready, but its author is not a collaborator and holds no trusted or merger tier, so no merge lane vouches for them and the hive never merges it unattended. A maintainer approves it (Approve and let hive merge — hive merges on the next tick once CI is green — or Merge now as maintainer), or trusts the author under 👥 Contributors."},
      "workflow-path": {"kind": "reason", "plain": "the PR edits .github/workflows; the hive's App lacks the `workflows` permission, so it cannot adopt it", "detail": "The PR changes files under .github/workflows. The hive's GitHub App has no `workflows` permission, so the adoption fence refuses to publish those edits from the hive. Grant the App the `workflows` permission and re-run the decision, or have a person rebase and merge it on GitHub, or close it as superseded."},
    };
