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

    // The phone tier of the Activity monitor: the same media query the
    // sheet-vs-popover decision uses (STAGE_TRACKER_SHEET_QUERY in
    // tracker-actions.js), so the two cannot drift.
    function stageTrackerPhone() {
      return stageTrackerIsPhoneSheet();
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
      f.attentionFirst = f.attentionFirst !== false;
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

    // How many narrowing filters the phone Filters sheet holds are set:
    // everything stageTrackerFilterActive counts except the kind /
    // attention / waiting-on-you chips, which sit on the compact bar.
    function stageTrackerSheetFilterCount(f) {
      f = stageTrackerNormalizeFilter(f);
      return (f.repo ? 1 : 0) + (f.q ? 1 : 0) + (f.age !== 'any' ? 1 : 0) + (f.author !== 'any' ? 1 : 0) + (f.acted !== 'any' ? 1 : 0) + f.labels.length + (f.needsHuman ? 1 : 0) + (f.onHold ? 1 : 0);
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
    // A standing meta issue is never a row (stageTrackerIsStandingMeta).
    function stageTrackerVisible(items, filter, ctx) {
      const f = stageTrackerNormalizeFilter(filter || _stageTrackerFilter);
      const rows = (items || []).filter(it => !stageTrackerIsStandingMeta(it) && stageTrackerMatches(it, f, ctx));
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

    // Pure: a standing meta issue — a bot Dependency Dashboard or the Hive
    // Advisory Report, the enumeration's hive_advisory / dependency_dashboard
    // buckets. It is a control panel, not pipeline work, so the monitor never
    // draws it (hivecommons/hive#13257). It stays in the payload's items, so
    // open_issues still equals the Overview tile, and the count line names
    // the difference.
    function stageTrackerIsStandingMeta(it) {
      return !!(it && it.kind === 'issue' && (it.bucket === 'hive_advisory' || it.bucket === 'dependency_dashboard'));
    }

    // Pure: how many items are standing meta issues (not rows).
    function stageTrackerStandingCount(items) {
      return (items || []).filter(stageTrackerIsStandingMeta).length;
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

    // Who finishes a row, for the 'finisher' sort: 0 Hive end to end, 1 not
    // known, 2 waiting on someone else, 3 needs a person. An issue riding one
    // covering PR takes the PR's answer (as its chip does); an issue with
    // several takes the one furthest from Hive.
    const STAGE_TRACKER_FINISHER_RANK = { hive: 0, elsewhere: 2, person: 3 };
    function stageTrackerFinisherRank(item, stages) {
      const rank = f => (Object.prototype.hasOwnProperty.call(STAGE_TRACKER_FINISHER_RANK, f || '') ? STAGE_TRACKER_FINISHER_RANK[f] : 1);
      const linked = item && item.kind === 'issue' ? stageTrackerLinkedSorted(item, stages) : [];
      if (linked.length === 1 && linked[0].finisher) return rank(linked[0].finisher);
      if (linked.length > 1) return Math.max(...linked.map(lp => rank(lp.finisher)));
      return rank(item && item.finisher);
    }

    /* ---- Maintainer notes (JS) --------------------------------------------
       Private per-item notes maintainers leave for each other on a row
       (/api/notes, api_maintainer_notes.go). They never reach GitHub; every
       signed-in maintainer of the hive sees them, and the scheduler types
       them into the item's agent kicks. The row's 🗒 button opens an inline
       expander (list + one-line composer, ⌘/Ctrl+Enter adds). Pure helpers
       (…Html, hamNotesIndex) run under node in
       stage_tracker_maintainer_notes_test.go; the handlers below fetch. */
    const MAINTAINER_NOTES_MAX = 2000;
    const MAINTAINER_NOTES_GLYPH = '🗒';
    const MAINTAINER_NOTES_AVATAR_PX = 32;
    const MAINTAINER_NOTES_PRIVACY = 'Private to this hive\u2019s maintainers \u2014 not posted to GitHub; included in agent kicks for the item.';
    var _hamNotes = {};
    var _hamNotesOpen = new Set();
    var _hamNotesDraft = {};
    var _hamNotesLoaded = false;
    var _hamNotesBusy = new Set();

    function hamNotesKey(repo, number) { return String(repo || '').toLowerCase() + '#' + (Number(number) || 0); }
    function hamNotesPanelId(key) { return 'ham-notes-' + String(key || '').replace(/[^a-z0-9]+/gi, '-'); }
    // Pure: a /api/notes list → { 'owner/repo#n': [notes, oldest first] }.
    function hamNotesIndex(notes) {
      const out = {};
      for (const n of (Array.isArray(notes) ? notes : [])) {
        if (!n || !n.repo || !(Number(n.number) > 0)) continue;
        const k = hamNotesKey(n.repo, n.number);
        (out[k] = out[k] || []).push(n);
      }
      for (const k of Object.keys(out)) out[k].sort((a, b) => (Date.parse(a.created_at || '') || 0) - (Date.parse(b.created_at || '') || 0));
      return out;
    }
    function hamNotesFor(item) { return (item && _hamNotes[hamNotesKey(item.repo, item.number)]) || []; }
    function hamNotesCount(item) { return hamNotesFor(item).length; }
    // The identity the notes UI draws for: signed in, login, write on the
    // repo (null = not known yet), verified owner (may delete any note).
    function hamNotesCtx(item) {
      const base = typeof stageTrackerActionCtx === 'function' && item ? (stageTrackerActionCtx(item.repo) || {}) : {};
      const role = (typeof window !== 'undefined' && window._hiveRole) || 'read';
      const owner = typeof dashboardRoleAtLeast === 'function' && dashboardRoleAtLeast(role, 'owner');
      return { user: !!base.user, login: String(base.login || ''), canHold: base.canHold, owner: !!owner };
    }
    // Pure: the row's notes affordance — nothing for a signed-out visitor
    // (the notes are the maintainers' own), a count badge once the item
    // has any. Sits with the row's decision controls.
    // The phone card tier (hivecommons/hive#17161) renders the badge
    // only once an item has notes, so an unnoted row keeps its two-line
    // head; the ⋯ menu's "Maintainer notes…" is the way in there.
    function hamNotesPhoneTier() {
      return typeof matchMedia === 'function' && matchMedia('(max-width: 640px), ((pointer: coarse) and (max-width: 820px))').matches;
    }
    function stageTrackerNotesButtonHtml(item, ctx) {
      if (!item || !ctx || !ctx.user) return '';
      const key = hamNotesKey(item.repo, item.number);
      const n = hamNotesCount(item);
      if (!n && hamNotesPhoneTier()) return '';
      const open = _hamNotesOpen.has(key);
      const ref = stageTrackerRefOf(item);
      const tip = (n ? n + ' maintainer note' + (n === 1 ? '' : 's') + '. ' : 'Leave a note for other maintainers. ') + MAINTAINER_NOTES_PRIVACY;
      return `<button type="button" class="ham-notes-btn${n ? ' ham-notes-has' : ''}" data-action="hamNotesToggle" data-arg-types="e,t" data-stop="1" data-prevent="1" data-ham-notes-key="${esc(key)}" aria-expanded="${open ? 'true' : 'false'}" aria-controls="${esc(hamNotesPanelId(key))}" aria-label="${esc('Maintainer notes for ' + ref + (n ? ' (' + n + ')' : ''))}" title="${esc(tip)}"><span class="ham-notes-glyph" aria-hidden="true">${MAINTAINER_NOTES_GLYPH}</span>${n ? `<span class="ham-notes-count">${n}</span>` : ''}</button>`;
    }
    // Pure: one note — avatar, login, relative time (ticked in place by
    // stageTrackerCueAgeSync via .cue-event-age), Delete for its author
    // or an owner, then the text (plain, pre-wrapped).
    function stageTrackerNoteHtml(n, ctx) {
      const login = String(n.author || '');
      const ms = Date.parse(n.created_at || '') || 0;
      const mine = !!(ctx && ctx.login) && login.toLowerCase() === String(ctx.login).toLowerCase();
      const canDelete = mine || !!(ctx && ctx.owner);
      return `<li class="ham-note" data-ham-note-id="${esc(n.id || '')}"><div class="ham-note-meta"><img class="ham-note-avatar" src="https://github.com/${encodeURIComponent(login)}.png?size=${MAINTAINER_NOTES_AVATAR_PX}" alt="" loading="lazy" width="${MAINTAINER_NOTES_AVATAR_PX}" height="${MAINTAINER_NOTES_AVATAR_PX}"><span class="ham-note-author">@${esc(login)}</span><time class="cue-event-age ham-note-when" data-at="${ms}" datetime="${esc(n.created_at || '')}" title="${esc(n.created_at || '')}"></time>${canDelete ? `<button type="button" class="ham-note-del" data-action="hamNoteDelete" data-arg-types="e,t" data-stop="1" data-prevent="1" aria-label="${esc('Delete note by @' + login)}">Delete</button>` : ''}</div><div class="ham-note-text">${esc(n.text || '')}</div></li>`;
    }
    // Pure: the open expander — the notes, then the composer (a signed-in
    // user without write on the repo reads but cannot add). '' when the
    // row's expander is closed.
    function stageTrackerNotesPanelHtml(item, ctx) {
      if (!item || !ctx || !ctx.user) return '';
      const key = hamNotesKey(item.repo, item.number);
      if (!_hamNotesOpen.has(key)) return '';
      const notes = hamNotesFor(item);
      const ref = stageTrackerRefOf(item);
      const draft = _hamNotesDraft[key] || '';
      const list = notes.length
        ? `<ul class="ham-notes-list">${notes.map(n => stageTrackerNoteHtml(n, ctx)).join('')}</ul>`
        : `<p class="ham-notes-empty">No maintainer notes on ${esc(ref)} yet.</p>`;
      const add = ctx.canHold !== false
        ? `<div class="ham-notes-add"><textarea class="ham-notes-input" rows="1" maxlength="${MAINTAINER_NOTES_MAX}" placeholder="Note for other maintainers\u2026" aria-label="${esc('New maintainer note for ' + ref)}" data-input-action="hamNotesDraft" data-keydown-action="hamNotesKeydown" data-keys="Enter" data-arg-types="e,t">${esc(draft)}</textarea><button type="button" class="hv-btn btn-secondary ham-notes-submit" data-action="hamNoteAdd" data-arg-types="e,t" data-stop="1" data-prevent="1" title="Add note (\u2318/Ctrl+Enter)">Add note</button></div>`
        : `<p class="ham-notes-empty">${esc(STAGE_TRACKER_WHY_WRITE)} to add a note.</p>`;
      return `<div class="ham-notes" id="${esc(hamNotesPanelId(key))}" data-ham-notes="${notes.length}" data-ham-notes-key="${esc(key)}" role="region" aria-label="${esc('Maintainer notes for ' + ref)}"><div class="ham-notes-head">${esc(MAINTAINER_NOTES_PRIVACY)}</div>${list}${add}</div>`;
    }
    function hamNotesKeyOf(el) { const p = el && el.closest ? el.closest('[data-ham-notes-key]') : null; return p ? p.getAttribute('data-ham-notes-key') || '' : ''; }
    function hamNotesSplitKey(key) { const i = String(key).lastIndexOf('#'); return { repo: String(key).slice(0, i), number: Number(String(key).slice(i + 1)) || 0 }; }
    // Loads every live note once a maintainer is signed in (the count
    // badges), or one item's after its `maintainer-notes` SSE frame.
    async function hamNotesLoad(key) {
      const role = window._hiveRole || 'read';
      if (!window._hiveUser && !(typeof dashboardRoleAtLeast === 'function' && dashboardRoleAtLeast(role, 'owner'))) return;
      try {
        const q = key ? hamNotesSplitKey(key) : null;
        const r = await fetch(q ? '/api/notes?repo=' + encodeURIComponent(q.repo) + '&number=' + q.number : '/api/notes');
        if (!r.ok) return;
        const d = await r.json();
        const idx = hamNotesIndex(d && d.notes);
        if (q) { if (idx[key]) _hamNotes[key] = idx[key]; else delete _hamNotes[key]; }
        else { _hamNotes = idx; _hamNotesLoaded = true; }
        renderStageTracker();
      } catch (e) { /* keep the previous notes; the next poll or frame retries */ }
    }
    // Pure: the row menu's entry — the count in its label once there are notes.
    function stageTrackerNotesMenuItem(item) {
      const n = hamNotesCount(item);
      return { id: 'notes', label: 'Maintainer notes' + (n ? ' (' + n + ')' : '') + '\u2026', hint: 'private', enabled: true, why: '' };
    }
    function hamNotesOpenFor(item) {
      const key = hamNotesKey(item.repo, item.number);
      _hamNotesOpen.add(key);
      renderStageTracker();
      const p = document.getElementById(hamNotesPanelId(key));
      const ta = p && p.querySelector('.ham-notes-input');
      if (ta) ta.focus(); else if (p && p.focus) { p.setAttribute('tabindex', '-1'); p.focus(); }
      if (!_hamNotesLoaded) hamNotesLoad();
    }
    function hamNotesToggle(e, btn) {
      const key = btn && btn.getAttribute ? btn.getAttribute('data-ham-notes-key') : '';
      if (!key) return;
      if (_hamNotesOpen.has(key)) _hamNotesOpen.delete(key); else _hamNotesOpen.add(key);
      renderStageTracker();
      if (_hamNotesOpen.has(key)) {
        const p = document.getElementById(hamNotesPanelId(key));
        const ta = p && p.querySelector('.ham-notes-input');
        if (ta) ta.focus();
        if (!_hamNotesLoaded) hamNotesLoad();
      }
    }
    function hamNotesDraft(e, ta) { const key = hamNotesKeyOf(ta); if (key && ta) _hamNotesDraft[key] = ta.value; }
    function hamNotesKeydown(e, ta) { if (e && e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); hamNoteAdd(e, ta); } }
    async function hamNoteAdd(e, el) {
      const key = hamNotesKeyOf(el);
      if (!key || _hamNotesBusy.has(key)) return;
      const p = document.getElementById(hamNotesPanelId(key));
      const ta = p && p.querySelector('.ham-notes-input');
      const text = String(ta ? ta.value : (_hamNotesDraft[key] || '')).trim();
      if (!text) { if (ta) ta.focus(); return; }
      if (text.length > MAINTAINER_NOTES_MAX) { showToast('A note is at most ' + MAINTAINER_NOTES_MAX + ' characters', 'error'); return; }
      const q = hamNotesSplitKey(key);
      _hamNotesBusy.add(key);
      try {
        const r = await fetch('/api/notes', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ repo: q.repo, number: q.number, text }) });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) { showToast('Could not add the note: ' + (d && d.error ? d.error : 'HTTP ' + r.status), 'error'); return; }
        delete _hamNotesDraft[key];
        if (d && d.note) { const cur = (_hamNotes[key] || []).filter(n => n.id !== d.note.id); cur.push(d.note); _hamNotes[key] = cur; }
        renderStageTracker();
        const p2 = document.getElementById(hamNotesPanelId(key));
        const ta2 = p2 && p2.querySelector('.ham-notes-input');
        if (ta2) ta2.focus();
      } catch (err) {
        showToast('Could not add the note: ' + (err && err.message ? err.message : 'network error'), 'error');
      } finally { _hamNotesBusy.delete(key); }
    }
    async function hamNoteDelete(e, btn) {
      const li = btn && btn.closest ? btn.closest('.ham-note[data-ham-note-id]') : null;
      const id = li ? li.getAttribute('data-ham-note-id') : '';
      const key = hamNotesKeyOf(btn);
      if (!id || !key) return;
      try {
        const r = await fetch('/api/notes/' + encodeURIComponent(id), { method: 'DELETE' });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) { showToast('Could not delete the note: ' + (d && d.error ? d.error : 'HTTP ' + r.status), 'error'); return; }
        _hamNotes[key] = (_hamNotes[key] || []).filter(n => n.id !== id);
        if (!_hamNotes[key].length) delete _hamNotes[key];
        renderStageTracker();
      } catch (err) {
        showToast('Could not delete the note: ' + (err && err.message ? err.message : 'network error'), 'error');
      }
    }

    // The canonical tracker count (owner): rows of the default view — every
    // item on its own row except a PR drawn on a listed issue's row.
    function stageTrackerRowCount(items, ctx) {
      return stageTrackerVisible(items, stageTrackerDefaultFilter(), ctx).length;
    }

    // Order rows. 'furthest' is the strip order (furthest along first, then
    // longest on step); 'waiting' is longest wait first; 'newest' is most
    // recently opened first; 'number' is repo then number; 'finisher' is
    // Hive end to end first, then waiting on someone else, then needs a
    // person, each in strip order. Under 'furthest' and 'waiting' rows tier
    // attention-first within a column (stageTrackerAttentionTier); dir 'asc'
    // reverses the strip / age / repo / number keys but never the tier, and
    // reverses 'finisher' whole (rows that need a person first).
    // On a phone (opts.phone) with f.attentionFirst the tier becomes the
    // top-level group, ahead of the column, so rows parked on a person lead
    // the list; desktop ordering never reads it.
    function stageTrackerSort(rows, stages, f, opts) {
      f = stageTrackerNormalizeFilter(f);
      const tierFirst = !!(opts && opts.phone) && f.attentionFirst && (f.sort === 'furthest' || f.sort === 'waiting');
      const byRepoNumber = (x, y) => ((x.repo || '').localeCompare(y.repo || '')) || ((x.number || 0) - (y.number || 0));
      const sign = f.dir === 'asc' ? -1 : 1;
      const byTier = (x, y) => stageTrackerAttentionTier(x) - stageTrackerAttentionTier(y);
      let cmp;
      switch (f.sort) {
        case 'waiting': cmp = (x, y) => byTier(x, y) || sign * (((stageTrackerWaitSince(x) || Infinity) - (stageTrackerWaitSince(y) || Infinity)) || byRepoNumber(x, y)); break;
        case 'newest': cmp = (x, y) => sign * (((Date.parse(y.created_at || '') || 0) - (Date.parse(x.created_at || '') || 0)) || byRepoNumber(y, x)); break;
        case 'number': cmp = (x, y) => sign * byRepoNumber(y, x); break;
        case 'finisher': {
          const keyOf = it => stageTrackerSortKey(it, stages);
          cmp = (x, y) => { const a = keyOf(x), b = keyOf(y); return sign * ((stageTrackerFinisherRank(x, stages) - stageTrackerFinisherRank(y, stages)) || (b.anchor - a.anchor) || (a.since - b.since) || byRepoNumber(x, y)); };
          break;
        }
        default: {
          const keyOf = it => stageTrackerSortKey(it, stages);
          cmp = (x, y) => { const a = keyOf(x), b = keyOf(y); return sign * (b.anchor - a.anchor) || byTier(x, y) || sign * ((a.since - b.since) || byRepoNumber(x, y)); };
        }
      }
      const ordered = cmp;
      if (tierFirst) cmp = (x, y) => byTier(x, y) || ordered(x, y);
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
    // Label chips: every label on any item that can be a row (never a
    // standing meta issue, stageTrackerIsStandingMeta), stage/NNN first by
    // number then the rest alphabetically (stageTrackerLabelCompare). The
    // row shows the first STAGE_TRACKER_LABEL_CHIP_LIMIT plus every selected
    // label; `all` is the complete list the "+N more" dialog offers, so no
    // label is ever out of reach. `needsHuman` and `onHold` are the
    // synthetic facets: their counts are the items stageTrackerItemNeedsHuman
    // / stageTrackerItemOnHold accept — the same predicates the filter uses.
    function stageTrackerLabelFacets(items, f, opts) {
      f = stageTrackerNormalizeFilter(f);
      items = (items || []).filter(it => !stageTrackerIsStandingMeta(it));
      const counts = {};
      items.forEach(it => (Array.isArray(it.labels) ? it.labels : []).forEach(l => { const k = String(l).toLowerCase(); counts[k] = (counts[k] || 0) + 1; }));
      const all = Object.keys(counts).sort(stageTrackerLabelCompare);
      const shown = all.slice(0, STAGE_TRACKER_LABEL_CHIP_LIMIT);
      f.labels.forEach(l => { if (counts[l] && shown.indexOf(l) < 0) shown.push(l); });
      shown.sort(stageTrackerLabelCompare);
      const chip = l => ({ label: l, count: counts[l], on: f.labels.indexOf(l) >= 0 });
      const needsHuman = { count: items.filter(stageTrackerItemNeedsHuman).length, on: f.needsHuman };
      const onHold = { count: items.filter(stageTrackerItemOnHold).length, on: f.onHold };
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
    function stageTrackerCountsText(page, allCount, attention, open, attentionAll, folded, standing) {
      const filteredOut = Math.max(0, allCount - page.total);
      const showing = page.total ? `showing ${page.from}–${page.to} of ${page.total}` : 'showing 0 of 0';
      const attnText = n => n ? n + ' need' + (n === 1 ? 's' : '') + ' attention' : 'nothing needs attention';
      const attn = attnText(attention);
      const attnAll = attnText(Number.isFinite(Number(attentionAll)) ? Number(attentionAll) : attention);
      const rowsWord = n => n === 1 ? 'row' : 'rows';
      const parts = stageTrackerRowBreakdown(allCount, open, folded, standing);
      const noteBits = [];
      if (parts.shared) noteBits.push(`${parts.shared} ${parts.shared === 1 ? 'PR' : 'PRs'} shown on ${parts.shared === 1 ? 'its' : 'their'} issue's row`);
      if (parts.folded) noteBits.push(`${parts.folded} review ${parts.folded === 1 ? 'note' : 'notes'} folded under ${parts.folded === 1 ? 'its' : 'their'} PR`);
      if (parts.standing) noteBits.push(`${parts.standing} bot ${parts.standing === 1 ? 'dashboard' : 'dashboards'} or advisory ${parts.standing === 1 ? 'report' : 'reports'} not listed`);
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
    // under its PR (`folded`, see stageTrackerVisible) and a standing meta
    // issue (`standing`); a PR is a row only when it is not drawn under a
    // listed issue, so standalonePRs = rows − (issues − folded − standing)
    // and shared = (issues − folded − standing + PRs) − rows. `known` is
    // false without totals. `issues` stays the open-issue total so the
    // breakdown still adds up to the Overview tile.
    function stageTrackerRowBreakdown(rowCount, open, folded, standing) {
      const rows = Math.max(0, Number(rowCount) || 0);
      if (!open || !Number.isFinite(Number(open.issues)) || !Number.isFinite(Number(open.prs))) return { known: false, rows, issues: 0, prs: 0, standalonePRs: 0, shared: 0, folded: 0, standing: 0 };
      const issues = Math.max(0, Number(open.issues)), prs = Math.max(0, Number(open.prs));
      const fold = Math.min(issues, Math.max(0, Number(folded) || 0));
      const meta = Math.min(issues - fold, Math.max(0, Number(standing) || 0));
      const standalonePRs = Math.min(prs, Math.max(0, rows - (issues - fold - meta)));
      return { known: true, rows, issues, prs, standalonePRs, shared: Math.max(0, issues - fold - meta + prs - rows), folded: fold, standing: meta };
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

