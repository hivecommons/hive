    // Which strip segment a stage lights: main stages light themselves,
    // 040 shares the PR in progress column with 030, 060 shares CI / Review
    // with 050, 095 and 100 share the Merge gate column with 080, detours
    // light the segment they branch from (stageTrackerColumn in Go).
    // kind is 'issue' or 'pr': an issue parked on a person (045, or a
    // pre-045 085) is parked before any PR exists, so it lights Actionable
    // rather than the merge gate. item (optional) is the row: once a merge
    // call has been made on its head (merge_gate.state merging /
    // merge-failed) the outcome is a Merge gate matter whatever numeric
    // stage the lane fell back to — hivecommons/docs#512 drew "Merge
    // failed" in CI / Review because the PR sat at 060.
    //
    // The column follows the CURRENT state, never the highest stage the
    // row once reached: a PR under repair (stageTrackerUnderRepair) lights
    // CI / Review even when a stale merge_gate ("merge-failed" from the
    // attempt that found the conflict) or a planner-ordered merge_train
    // still rides on the record — hive's own #15574 read "Fixing" under
    // Merge queue after "fell back to Fixing — conflicting".
    function stageTrackerAnchor(stage, stages, kind, item) {
      const cur = Number(stage) || 0;
      if (cur >= STAGE_TRACKER_TERMINAL) return STAGE_TRACKER_TERMINAL;
      // Under repair past 030 → CI / Review, before any gate or train
      // promotion. The one park the predicate also names, 085 needs
      // decision, stays at the gate: the person decides there.
      const repairing = typeof stageTrackerUnderRepair === 'function' && !!stageTrackerUnderRepair(cur, item && item.reason);
      if (repairing && cur > STAGE_TRACKER_PR_COLUMN && cur !== STAGE_TRACKER_NEEDS_DECISION) return 50;
      const mg = item && item.merge_gate;
      const refusal = mg && mg.state === 'merge-failed'
        ? mg.merge_failed_kind || (typeof stageTrackerMergeRefusalFromRaw === 'function' ? stageTrackerMergeRefusalFromRaw(String(mg.merge_failed || '')).kind : '') : '';
      if ((cur === 50 || cur === 60) && refusal === 'checks-pending') return 50;
      if (mg && (mg.state === 'merging' || mg.state === 'merge-failed') && cur > STAGE_TRACKER_PR_COLUMN) return STAGE_TRACKER_MERGE_COLUMN;
      // A merge-train member is a Merge queue matter too, 1st included:
      // the train's facts are mirrored onto its head while the head still
      // sits at 050/070, and hive's own #15400 ("1st") lit CI / Review
      // next to its 2nd-place sibling in Merge queue (hivecommons/hive#15395).
      if (item && item.merge_train && item.merge_train.state && cur > STAGE_TRACKER_PR_COLUMN) return STAGE_TRACKER_MERGE_COLUMN;
      if (cur === STAGE_TRACKER_ISSUE_NEEDS_DECISION || cur === STAGE_TRACKER_TRIAGE_NEEDED || cur === STAGE_TRACKER_POSSIBLY_EXISTS) return STAGE_TRACKER_ISSUE_DECISION_COLUMN;
      if (cur === STAGE_TRACKER_NEEDS_DECISION && kind === 'issue') return STAGE_TRACKER_ISSUE_DECISION_COLUMN;
      if (STAGE_TRACKER_PATH.indexOf(cur) >= 0) return cur;
      if (cur === 40) return STAGE_TRACKER_PR_COLUMN;
      // 060 reviewed is pre-admission (approved, CI pending or red; it can
      // regress to 070): it lights CI / Review (50), never the merge gate.
      if (cur === 60) return 50;
      if (cur === STAGE_TRACKER_PRE_MERGE_SOAK || cur === 100) return STAGE_TRACKER_MERGE_COLUMN;
      const st = (stages || []).find(x => Number(x.number) === cur) || {};
      let from = Number(st.branch_from) || 0;
      if (from === 60) from = 50;
      return STAGE_TRACKER_PATH.indexOf(from) >= 0 ? from : 0;
    }

    // Segment list for one item: path segments with done/active state plus a
    // detour badge when the item sits on a side stage, and the merged end-cap.
    // ctx.trains (how many trains the page draws) lets a queue word name
    // its train when there is more than one.
    function stageTrackerSegments(item, stages, nowMs, ctx) {
      const trains = ctx && ctx.trains;
      const byNum = {};
      (stages || []).forEach(st => { byNum[st.number] = st; });
      const cur = Number(item.stage) || 0;
      const curStage = byNum[cur] || {};
      const anchor = stageTrackerAnchor(cur, stages, item.via_pr ? 'pr' : item.kind, item);
      const terminal = anchor >= STAGE_TRACKER_TERMINAL;
      const sentinelLane = stageTrackerSentinelInLane(item);
      // A 071 / 072 row whose author has pushed is back with the hive: the
      // chevron reads the column's own word ("CI / Review"), not "Needs
      // @author" (hivecommons/hive#15784). Guarded: the harnesses load by name.
      const pushedBack = (cur === 71 || cur === 72) && typeof stageTrackerPushedBack === 'function' && stageTrackerPushedBack(item);
      const detour = (!terminal && !pushedBack && STAGE_TRACKER_PATH.indexOf(cur) < 0 && cur !== STAGE_TRACKER_TRIAGE_NEEDED && cur !== STAGE_TRACKER_POSSIBLY_EXISTS && cur !== 40 && cur !== 60 && cur !== STAGE_TRACKER_PRE_MERGE_SOAK && cur !== 100)
        ? { text: sentinelLane ? STAGE_TRACKER_SENTINEL_WORD : (cur === STAGE_TRACKER_HUMAN_GATED && typeof stageTrackerTrainWord === 'function' && stageTrackerTrainWord(item)) || (cur === STAGE_TRACKER_READY_FOR_HUMAN_MERGE && item.manual_merge ? stageTrackerManualMergeWord(item) : (cur === STAGE_TRACKER_HUMAN_GATED && stageTrackerUnauthorizedWord(item)) || (typeof stageTrackerBlockerWord === 'function' && cur === STAGE_TRACKER_ISSUE_NEEDS_DECISION && stageTrackerBlockerWord(item)) || ((cur === STAGE_TRACKER_ADOPTED_BY_HIVE || (cur === STAGE_TRACKER_HUMAN_GATED && item.adopted_by_state)) && stageTrackerAdoptedWord(item)) || (cur === STAGE_TRACKER_READY_FOR_HUMAN_MERGE && stageTrackerReadyForHumanWord(item, STAGE_TRACKER_ACTIVE_WORD[cur] || curStage.short || curStage.label || String(cur))) || (STAGE_TRACKER_ACTIVE_WORD[cur] || curStage.short || curStage.label || String(cur))), sentinel: sentinelLane } : null;
      if (detour && cur === 70 && !detour.sentinel && typeof stageTrackerFixingWord === 'function') detour.text = stageTrackerFixingWord(item, detour.text);
      if (detour && (cur === 71 || cur === 72) && typeof stageTrackerReturnedWord === 'function') {
        detour.text = stageTrackerReturnedWord(item, detour.text);
        detour.sentinelSuffix = stageTrackerSentinelAsSuffix(item);
      }
      const segs = STAGE_TRACKER_PATH.map(n => {
        const st = n === 0 ? { label: 'Open', short: 'Open', description: 'Open, not yet in a stage' } : (byNum[n] || {});
        let state = 'todo';
        if (terminal || n < anchor) state = 'done';
        else if (n === anchor) state = 'active';
        // The lit column's tooltip names the real stage (030 vs 040, 060 vs
        // 080) — for a detour too: a 085 row lights the 080 column, and its
        // hover must say "Needs human decision", not borrow the column's
        // "Awaiting merge admission" text (hivecommons/hive#14821, #14822
        // read as 080 with "Next move: human").
        const tipStage = (state === 'active' && curStage.label) ? curStage : st;
        let word = state === 'active' && STAGE_TRACKER_ACTIVE_WORD[cur] ? STAGE_TRACKER_ACTIVE_WORD[cur] : (st.short || st.label || String(n));
        // A merge-train member's word is its place in the train (#15117).
        if (state === 'active' && !detour && n > 0 && typeof stageTrackerTrainWord === 'function') { const tw = stageTrackerTrainWord(item, trains); if (tw) word = tw; }
        // A PR the hive is rebasing (the train's or the fixer's episode)
        // reads "Rebasing · attempt 1/2" in the CI / Review column it sits
        // in (hive#15825) — its head has no CI yet, so no merge-queue word.
        if (state === 'active' && !detour && n > 0 && n < 80 && item.rebase && item.rebase.text && !(typeof stageTrackerTrain === 'function' && stageTrackerTrain(item))) word = String(item.rebase.text);
        const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
        const openHold = n === 0 && state === 'active' && mods.indexOf('hold') >= 0;
        // hold is a human instruction, so it wins over blocked when both are present.
        const openBlocked = n === 0 && state === 'active' && !openHold && mods.indexOf('blocked') >= 0;
        // A draft PR with no stage is the author's, not the hive's: the
        // Open word says so, muted, and it never tints stale (#14572).
        const openDraft = n === 0 && state === 'active' && !openHold && !openBlocked && mods.indexOf('draft') >= 0;
        // 000-new: the payload says nothing has placed the item yet and
        // names who moves it next (hivecommons/hive#15094).
        const openNew = n === 0 && state === 'active' && !openHold && !openBlocked && !openDraft && typeof stageTrackerNewState === 'function' && !!stageTrackerNewState(item);
        const advisory = n === 0 && state === 'active' && !openHold && !openBlocked && typeof stageTrackerAdvisoryOf === 'function' ? stageTrackerAdvisoryOf(item) : null;
        // An issue waiting on a maintainer's triage says so in the column
        // (hivecommons/hive#15437 read "New" while the caption held the ask).
        const openTriage = n === 0 && state === 'active' && !openHold && !openBlocked && !openDraft && !advisory && typeof stageTrackerOpenTriageNeeded === 'function' && stageTrackerOpenTriageNeeded(item);
        if (openHold) word = 'Open · hold';
        else if (openBlocked) word = 'Open · blocked';
        else if (advisory) word = advisory.chip || 'Advisory · read & close';
        else if (openDraft) word = 'Draft';
        else if (openTriage) word = 'Triage needed';
        else if (openNew) word = 'New';
        // A plain, blocked or draft Open step (no stage label, not held)
        // says how long it has sat there, from the same instant the since
        // column shows — a stale blocked item is exactly what should stand
        // out. An advisory never does: nothing waits on it.
        let openSince = null;
        if (n === 0 && state === 'active' && !openHold && !advisory && !detour && nowMs) {
          const os = stageTrackerOpenSince(item);
          if (os.ms) { word += ' · since ' + stageTrackerShortAgo(nowMs - os.ms); openSince = Object.assign({ stale: !openDraft && nowMs - os.ms > STAGE_TRACKER_OPEN_STALE_DAYS * 86400000 }, os); }
        }
        return { number: n, code: n ? String(n).padStart(3, '0') : '', short: word, label: (typeof stageTrackerStageName === 'function' && (stageTrackerStageName(tipStage) || stageTrackerStageName(st))) || tipStage.label || st.label || String(n), description: tipStage.description || st.description || '', state, terminal: false, detour: n === anchor ? detour : null, hold: openHold, blocked: openBlocked, draft: openDraft, isNew: openNew && !advisory, advisory: !!advisory, openSince };
      });
      segs.push({ number: STAGE_TRACKER_TERMINAL, code: '', short: 'Merged', label: 'Merged', description: 'Merged — the pipeline is complete', state: terminal ? 'active' : 'todo', terminal, cap: true, detour: null });
      return segs;
    }

    const STAGE_TRACKER_MOD_PRIORITY = ['stalled', 'regressed', 'hold', 'unauthorized', 'needs-human', 'needs-decision', 'needs-triage', 'possible-dupe', 'needs-rebase', 'blocked', 'draft'];
    const STAGE_TRACKER_MOD_TEXT = { 'needs-decision': 'needs human decision', 'needs-triage': 'triage needed', 'possible-dupe': 'possible dupe' };
    // The draft pill and the Draft chevron explain themselves on hover: no
    // maintainer action is pending, the lanes resume when the author is ready.
    const STAGE_TRACKER_DRAFT_TIP = 'Draft PR — hive review and merge lanes skip drafts until the author marks it ready for review.';
    // 16-unit monochrome stroke icons, one per strip column (and the two kinds).
    // History (the right-most column): a clock face with a counter-clockwise
    // arrow — the row's decisions and actions, never a "done" mark (owner,
    // 2026-10-28: the ✓ read as passed). One path (`history` below), shared
    // by the column header, the unmerged cap chevron and the acted badge.
    const STAGE_TRACKER_ICONS = {
      0: '<circle cx="8" cy="8" r="5"/>',
      10: '<path d="M9 1 3 9h4l-1 6 6-8H8z"/>',
      20: '<rect x="3" y="7" width="10" height="7" rx="1"/><path d="M5 7V5a3 3 0 0 1 6 0v2"/>',
      30: '<circle cx="4" cy="3" r="1.5"/><circle cx="4" cy="13" r="1.5"/><circle cx="12" cy="5" r="1.5"/><path d="M4 4.5v7M12 6.5c0 3-8 1-8 4"/>',
      40: '<circle cx="4" cy="3" r="1.5"/><circle cx="4" cy="13" r="1.5"/><circle cx="12" cy="12" r="1.5"/><path d="M4 4.5v7M4 6c0 3 6 2 6.5 5"/>',
      50: '<path d="M1 8s2.5-5 7-5 7 5 7 5-2.5 5-7 5-7-5-7-5z"/><circle cx="8" cy="8" r="2"/>',
      // Merge queue: the same railcar as the column header and train chip
      // (STAGE_TRACKER_TRAIN_ICON_PATH in tracker-words.js; a test keeps them equal).
      80: '<rect x="2" y="3.5" width="12" height="7" rx="1.5"/><path d="M2 7.5h12M6.5 3.5v4M10.5 3.5v4M4 13.5l-1.5 1.5M12 13.5l1.5 1.5"/><circle cx="5" cy="12.5" r="1.5"/><circle cx="11" cy="12.5" r="1.5"/>',
      100: '<path d="M3 5h10M3 8h10M3 11h10"/>',
      110: '<path d="M3 8l3 3 7-7"/>',
      history: '<path d="M2 8a6 6 0 1 0 6-6 6.5 6.5 0 0 0-4.5 1.83L2 5.33"/><path d="M2 2v3.33h3.33"/><path d="M8 4.67V8l2.67 1.33"/>',
      issue: '<circle cx="8" cy="8" r="6"/><circle cx="8" cy="8" r="1.5"/>',
      human: '<circle cx="8" cy="5" r="3"/><path d="M2.5 14.5a5.5 5.5 0 0 1 11 0"/>',
      clanker: '<rect x="3" y="5" width="10" height="8" rx="1.5"/><path d="M8 5V2M5 2h6"/><circle cx="6" cy="9" r="0.9"/><circle cx="10" cy="9" r="0.9"/>',
      hive: '<path d="M8 1.5 13.6 4.75v6.5L8 14.5 2.4 11.25v-6.5z"/>',
      // A non-hive automation account (github-actions[bot], renovate): a cog,
      // distinct from the clanker's robot head.
      bot: '<circle cx="8" cy="8" r="2.2"/><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4"/>',
      pr: '<circle cx="4" cy="3" r="1.5"/><circle cx="4" cy="13" r="1.5"/><circle cx="12" cy="13" r="1.5"/><path d="M4 4.5v7M12 11.5V7a2 2 0 0 0-2-2H8"/>',
      // Merge-gate cues (#15073): sweep = two arrows chasing (spins while
      // hive's sweep is on the PR); wait = a clock (waiting for the next pass).
      sweep: '<path d="M13.5 8a5.5 5.5 0 0 1-9.6 3.7M2.5 8a5.5 5.5 0 0 1 9.6-3.7"/><path d="M12.5 1.5v3h-3M3.5 14.5v-3h3"/>',
      wait: '<circle cx="8" cy="8" r="6"/><path d="M8 4.5V8l2.5 1.5"/>',
    };
    // label is exposed to assistive tech only (aria-label, no <title>): hover
    // text in a tracker row belongs to the links, titles and the lit chevron.
    function stageTrackerIcon(key, cls, label) {
      const body = STAGE_TRACKER_ICONS[key];
      if (!body) return '';
      return `<svg class="${cls}" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"${label ? ` role="img" aria-label="${esc(label)}"` : ' aria-hidden="true"'}>${body}</svg>`;
    }

    // "[author icon] #N title" for the PR in progress column of the title line.
    // tip, when given, replaces the default "title / Author" hover text.
    function stageTrackerPRRef(number, url, authorKind, author, title, tip) {
      const ak = authorKind || 'hive';
      const who = ak + (author ? ' · @' + author : '');
      return `<a class="ham-pr-ref" href="${esc(url || '#')}" target="_blank" rel="noopener" title="${esc(tip || ((title ? title + '\n' : '') + 'Author: ' + who))}">${stageTrackerIcon(ak, 'ham-author-ico ham-author-' + esc(ak), who)}<span class="ham-pr-num">#${Number(number) || 0}</span>${title ? `<span class="ham-pr-title">${esc(title)}</span>` : ''}</a>`;
    }

    // Author kind of whoever must move on the item's current stage ('' = none).
    // The server's stage_actor {kind, login, since, why} is the source of
    // truth when present (kind 'none' → no icon); the stage-table heuristic
    // below only serves payloads from an older server.
    const STAGE_TRACKER_ACTOR_NONE = 'none';
    const STAGE_TRACKER_NOBODY_LEAD = 'no one is working on this right now';
    function stageTrackerStageActor(item) {
      const sa = item && item.stage_actor;
      return sa && typeof sa === 'object' && sa.kind ? sa : null;
    }
    function stageTrackerActorKind(item) {
      // Guarded: test harnesses lift this function by name without the 000-new helpers.
      const fresh = typeof stageTrackerNewState === 'function' ? stageTrackerNewState(item) : null;
      if (fresh) return fresh.next_mover === 'maintainer' ? 'human' : 'hive';
      const sa = stageTrackerStageActor(item);
      if (sa) return sa.kind === STAGE_TRACKER_ACTOR_NONE ? '' : String(sa.kind);
      const role = STAGE_TRACKER_ACTOR[Number(item.stage) || 0] || '';
      if (role === 'claimant') return item.claimant_kind || '';
      if (role === 'author') {
        if (item.kind === 'pr') return item.author_kind || '';
        const lp = Array.isArray(item.linked_prs) ? item.linked_prs[0] : null;
        return (lp && lp.author_kind) || '';
      }
      return role;
    }
    // " · Next move: hive @fixer — fixer commented · 3m ago", or, with
    // nobody on it, " · no one is working on this right now — <why>".
    // A person's move with no stage_actor.why says why from the row's own
    // reason, so the 👤 marker never leaves the question unanswered.
    function stageTrackerHumanWhy(item, kind) {
      if (kind !== 'human' && kind !== 'maintainer') return '';
      return String(item.human_merge_reason_text || item.reason || '').trim();
    }
    // The short "what for" after the chevron word on a person's move
    // ("PR in progress · #15759 needs a decision"): the server's
    // stage_actor.human_reason (≤40 chars), else the row's own reason
    // clipped, else a generic ask — never '' for a human move, so no 👤
    // marker ships without its why (hivecommons/hive#15773). The full
    // question stays in the tooltip (stageTrackerActorTip).
    const STAGE_TRACKER_HUMAN_REASON_MAX = 40;
    const STAGE_TRACKER_HUMAN_REASON_FALLBACK = 'needs a person';
    function stageTrackerHumanReason(item, kind) {
      if (kind !== 'human' && kind !== 'maintainer') return '';
      const sa = stageTrackerStageActor(item);
      const r = String((sa && sa.human_reason) || stageTrackerHumanWhy(item, kind) || STAGE_TRACKER_HUMAN_REASON_FALLBACK).replace(/\s+/g, ' ').trim();
      return r.length > STAGE_TRACKER_HUMAN_REASON_MAX ? r.slice(0, STAGE_TRACKER_HUMAN_REASON_MAX - 1).replace(/\s+\S*$/, '') + '…' : r;
    }
    function stageTrackerActorTip(item) {
      // Guarded: test harnesses lift this function by name without stageTrackerHumanWhy.
      const humanWhy = typeof stageTrackerHumanWhy === 'function' ? stageTrackerHumanWhy : () => '';
      const sa = stageTrackerStageActor(item);
      if (!sa) { const k = stageTrackerActorKind(item); const w = humanWhy(item, k); return k ? ' · Next move: ' + k + (w ? ' — ' + w : '') : ''; }
      if (sa.kind === STAGE_TRACKER_ACTOR_NONE) return sa.why ? ' · ' + STAGE_TRACKER_NOBODY_LEAD + ' — ' + sa.why : '';
      const why = sa.why || humanWhy(item, sa.kind);
      return ' · Next move: ' + sa.kind + (sa.login ? ' @' + sa.login : '') + (why ? ' — ' + why : '');
    }
    // Who holds the row: the payload's claimant {kind, login, agent, hive,
    // since, source} (hivecommons/hive#15422). The chevron gets one short
    // token after the stage word; the hover gets one "Held by:" line.
    const STAGE_TRACKER_HIVE_PREFIXES = ['hosted-ibm-', 'hosted-'];
    const STAGE_TRACKER_CLAIMANT_SOURCE = { 'claim-ledger': 'claim ledger', 'claim-comment': 'claim comment', assignee: 'assignee', 'kick-history': 'kick history', author: 'PR author' };
    function stageTrackerShortHive(hive) {
      const h = String(hive || '');
      const p = STAGE_TRACKER_HIVE_PREFIXES.find(x => h.startsWith(x));
      return p ? h.slice(p.length) : h;
    }
    function stageTrackerClaimant(item) {
      const c = item && item.claimant;
      return c && typeof c === 'object' && c.kind && (c.login || c.agent) ? c : null;
    }
    // The strip icon for a holder's kind: ⬡ hive agent, robot clanker, person.
    const STAGE_TRACKER_CLAIMANT_ICON = { 'hive-agent': 'hive', clanker: 'clanker', person: 'human' };
    function stageTrackerClaimantIconKind(c) {
      return (c && STAGE_TRACKER_CLAIMANT_ICON[c.kind]) || 'human';
    }
    // "quality", "quality@alchemy", "@alice" or "@bob's clanker"; '' when unknown.
    function stageTrackerClaimantShort(c) {
      if (!c) return '';
      if (c.kind === 'hive-agent') return c.agent ? c.agent + (c.hive ? '@' + stageTrackerShortHive(c.hive) : '') : (c.login ? '@' + c.login : '');
      if (!c.login) return '';
      return '@' + c.login + (c.kind === 'clanker' ? "'s clanker" : '');
    }
    // "Held by: hive agent quality@hosted-ibm-alchemy since 3h ago (claim comment)".
    function stageTrackerClaimantLine(c, nowMs) {
      if (!c) return '';
      let who;
      if (c.kind === 'hive-agent') who = 'hive agent ' + (c.agent || '@' + c.login) + (c.hive ? '@' + c.hive : '');
      else who = '@' + c.login + (c.kind === 'clanker' ? "'s clanker" : '');
      const sinceMs = Date.parse(c.since || '');
      let since = '';
      if (sinceMs && nowMs) { const ago = stageTrackerShortAgo(nowMs - sinceMs); since = ago === 'now' ? ' since just now' : ' since ' + ago + ' ago'; }
      const src = STAGE_TRACKER_CLAIMANT_SOURCE[c.source] || c.source || '';
      return 'Held by: ' + who + since + (src ? ' (' + src + ')' : '');
    }
    // The Fixing column word when nobody is fixing: a bot-authored PR the
    // hive does not touch, or an unowned red PR, reads as its state in
    // plain words instead of an active "Fixing" (hivecommons/docs#500).
    function stageTrackerFixingWord(item, fallback) {
      const sa = stageTrackerStageActor(item);
      if (!sa || (sa.kind !== STAGE_TRACKER_ACTOR_NONE && sa.kind !== 'bot')) return fallback;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const state = mods.indexOf('needs-rebase') >= 0 ? 'Conflicting' : 'Red';
      return state + (sa.kind === 'bot' ? ' · bot PR' : ' · nobody on it');
    }

    // Advisory (hivecommons/hive#15381): a payload `advisory` block means
    // the issue is a note for maintainers, not work — the chevron word is
    // its chip, it never reads stale or "since Nh", and the row action is
    // Close (read). Pure so the fixture tests can lift it by name.
    function stageTrackerAdvisoryOf(item) {
      if (!item || item.kind === 'pr' || !item.advisory || typeof item.advisory !== 'object') return null;
      return item.advisory;
    }
    // 000-new (hivecommons/hive#15094): a payload `new` block on a stage-0
    // row means nothing has placed the item yet. These are pure so the
    // fixture tests can lift them by name.
    const STAGE_TRACKER_NEW_GATE_MARK = { done: ' ✓', next: ' (next)' };
    function stageTrackerNewState(item) {
      if (!item || (Number(item.stage) || 0) !== 0 || !item.new || typeof item.new !== 'object') return null;
      return item.new;
    }
    // Pure: an issue in the Open column that waits on a maintainer's triage
    // — the needs-triage pill, or a 000-new reporter-trust hold whose next
    // mover is the maintainer (triage gate next) before the awaiting label
    // has landed. The chevron then says "Triage needed", not "New": the ask
    // is in the column, not only in the caption.
    function stageTrackerOpenTriageNeeded(item) {
      if (!item || item.kind === 'pr' || (Number(item.stage) || 0) !== 0) return false;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.indexOf('needs-triage') >= 0) return true;
      const fresh = stageTrackerNewState(item);
      if (!fresh || fresh.next_mover !== 'maintainer') return false;
      const gates = Array.isArray(fresh.gates) ? fresh.gates : [];
      return gates.some(g => g && g.name === 'triage' && g.state === 'next');
    }
    // "Next: …" and "Gates ahead: triage ✓ → actionable → claimed" for the
    // row tooltip and the lit chevron. nowMs refreshes the next-pass countdown.
    function stageTrackerNewLines(item, nowMs) {
      const fresh = stageTrackerNewState(item);
      if (!fresh) return [];
      let next = 'Next: ' + (fresh.next_mover_text || fresh.next_mover || 'the next pass');
      const passMs = Date.parse(fresh.next_pass_at || '');
      if (passMs && nowMs) {
        const left = passMs - nowMs;
        next += left > 0 ? ' · next hive pass in ' + stageTrackerShortAgo(left) : ' · next hive pass due now';
      }
      const lines = [next];
      const gates = Array.isArray(fresh.gates) ? fresh.gates : [];
      if (gates.length) {
        lines.push('Gates ahead: ' + gates.map(g => (g.name || '') + (STAGE_TRACKER_NEW_GATE_MARK[g.state] || '')).join(' → '));
        gates.forEach(g => { if (g.text) lines.push('  ' + (g.name || '') + ': ' + g.text); });
      }
      return lines;
    }

    // An Open item is tinted stale once it has sat unlabelled this long.
    const STAGE_TRACKER_OPEN_STALE_DAYS = 2;
    // When an item entered its current (Open) state: the payload's
    // stage_since — the value the since column draws — else the time it was
    // opened. fromCreated marks the fallback so the tooltip can say so.
    function stageTrackerOpenSince(item) {
      const since = Date.parse((item && item.stage_since) || '');
      if (since) return { ms: since, fromCreated: false };
      const created = Date.parse((item && item.created_at) || '');
      return { ms: created || 0, fromCreated: !!created };
    }

    function stageTrackerShortAgo(ms) {
      const sec = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
      if (sec < 60) return 'now';
      const min = Math.floor(sec / 60);
      if (min < 60) return min + 'm';
      const hours = Math.floor(min / 60);
      if (hours < 48) return hours + 'h';
      return Math.floor(hours / 24) + 'd';
    }
