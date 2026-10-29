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
    function stageTrackerAnchor(stage, stages, kind, item) {
      const cur = Number(stage) || 0;
      if (cur >= STAGE_TRACKER_TERMINAL) return STAGE_TRACKER_TERMINAL;
      const mg = item && item.merge_gate;
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
    function stageTrackerSegments(item, stages, nowMs) {
      const byNum = {};
      (stages || []).forEach(st => { byNum[st.number] = st; });
      const cur = Number(item.stage) || 0;
      const curStage = byNum[cur] || {};
      const anchor = stageTrackerAnchor(cur, stages, item.via_pr ? 'pr' : item.kind, item);
      const terminal = anchor >= STAGE_TRACKER_TERMINAL;
      const sentinelLane = stageTrackerSentinelInLane(item);
      const detour = (!terminal && STAGE_TRACKER_PATH.indexOf(cur) < 0 && cur !== STAGE_TRACKER_TRIAGE_NEEDED && cur !== STAGE_TRACKER_POSSIBLY_EXISTS && cur !== 40 && cur !== 60 && cur !== STAGE_TRACKER_PRE_MERGE_SOAK && cur !== 100)
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
        if (state === 'active' && !detour && n > 0 && typeof stageTrackerTrainWord === 'function') { const tw = stageTrackerTrainWord(item); if (tw) word = tw; }
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
    function stageTrackerActorTip(item) {
      const sa = stageTrackerStageActor(item);
      if (!sa) { const k = stageTrackerActorKind(item); return k ? ' · Next move: ' + k : ''; }
      if (sa.kind === STAGE_TRACKER_ACTOR_NONE) return sa.why ? ' · ' + STAGE_TRACKER_NOBODY_LEAD + ' — ' + sa.why : '';
      return ' · Next move: ' + sa.kind + (sa.login ? ' @' + sa.login : '') + (sa.why ? ' — ' + sa.why : '');
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

    // Column header: the stage names once, aligned with the chevrons below.
    // It is sticky under the Merged bar (tracker-cues CSS), so the names
    // and the chip stay in view while the list scrolls.
    function stageTrackerHeaderHtml(stages) {
      const byNum = {};
      (stages || []).forEach(st => { byNum[st.number] = st; });
      const cols = STAGE_TRACKER_PATH.map(n => {
        const name = n === 0 ? 'Open' : ((byNum[n] || {}).short || (byNum[n] || {}).label || String(n));
        // The Merge queue column wears the same train as the train header chip.
        const ico = n === 80 && typeof stageTrackerTrainIconHtml === 'function' ? stageTrackerTrainIconHtml('ham-ico ham-train-icon ham-col-ico') : '';
        return `<li class="ham-seg"><span class="ham-seg-t">${ico}${esc(name)}</span></li>`;
      }).join('');
      return `<div class="ham-cols" aria-hidden="true"><div class="ham-strip-row"><ol class="ham-strip">${cols}<li class="ham-seg cap ham-history-head" title="History — decisions and actions on this item">${stageTrackerIcon('history', 'ham-ico ham-history-ico', 'History — decisions and actions on this item')}</li></ol><span class="ham-since">since</span></div></div>`;
    }

    // Chevrons for one item. range bounds the columns the item owns (an
    // issue row stops at PR in progress, a nested PR row starts there);
    // columns outside it are invisible spacers that keep the grid aligned.
    // Only the active chevron carries hover text (stage name, description,
    // whose move it is); passed and upcoming chevrons have none.
    function stageTrackerStripItems(item, stages, range, nowMs, ctx) {
      const segs = stageTrackerSegments(item, stages, nowMs);
      const curStage = Number(item.stage) || 0;
      const from = range && range.from != null ? range.from : -Infinity;
      const to = range && range.to != null ? range.to : Infinity;
      const actorKind = stageTrackerActorKind(item);
      const actorIco = actorKind ? stageTrackerIcon(actorKind, 'ham-ico ham-actor-ico', 'Next move: ' + actorKind) : '';
      const actorTip = stageTrackerActorTip(item);
      const claimant = typeof stageTrackerClaimant === 'function' ? stageTrackerClaimant(item) : null;
      const holderShort = claimant ? stageTrackerClaimantShort(claimant) : '';
      const holderHtml = holderShort ? `<span class="ham-holder"> · ${esc(holderShort)}</span>` : '';
      // A row whose next mover is nobody (an unclassified PR under its
      // issue reads "PR in progress") still shows WHO holds it: the
      // holder's own kind icon stands where the actor icon would
      // (hivecommons/hive#15541).
      const holderIco = holderShort && !actorIco ? stageTrackerIcon(stageTrackerClaimantIconKind(claimant), 'ham-ico ham-actor-ico', stageTrackerClaimantLine(claimant, nowMs)) : '';
      // Static chevrons carry no data-motion: the CSS only animates the two
      // moving buckets. The hive-acting pulse needs an agent actually on
      // the item (tracker-cues: working); a hive stage with nobody on it is
      // waiting, and its tooltip says so.
      // tracker-cues helpers are optional here so a lifted copy of this
      // function (tests, previews) renders without them.
      const worker = typeof stageTrackerWorkingOn === 'function' ? stageTrackerWorkingOn(item, ctx && ctx.workers) : null;
      let motion = stageTrackerMotion(item);
      if (motion === STAGE_TRACKER_MOTION_ACTIVE && !worker) motion = STAGE_TRACKER_MOTION_STATIC;
      const motionAttr = motion === STAGE_TRACKER_MOTION_STATIC ? '' : ` data-motion="${motion}"`;
      return segs.map(sg => {
        if (sg.number < from || sg.number > to) return `<li class="ham-seg blank${sg.cap ? ' cap' : ''}" aria-hidden="true"></li>`;
        const sentinel = !!(sg.detour && sg.detour.sentinel);
        const sentinelSuffix = !!(sg.detour && sg.detour.sentinelSuffix);
        const cls = ['ham-seg', sg.state, sg.cap ? 'cap' : '', sg.cap && sg.terminal ? 'merged' : '', sg.detour ? 'detoured' : '', sg.hold ? 'ham-open-hold' : '', sg.blocked ? 'ham-open-blocked' : '', sg.draft ? 'ham-open-draft' : '', sg.isNew ? 'ham-open-new' : '', sg.advisory ? 'ham-open-advisory' : '', sg.openSince && sg.openSince.stale ? 'ham-open-stale' : '', sentinel || sentinelSuffix ? 'ham-sentinel' : ''].filter(Boolean).join(' ');
        const active = sg.state === 'active';
        let tip = '';
        if (active) {
          tip = sg.label + (sg.description ? ' — ' + sg.description : '');
          if (sg.blocked) tip += ' — ' + stageTrackerBlockedTip(item);
          if (sg.draft) tip += ' — ' + STAGE_TRACKER_DRAFT_TIP;
          if (sg.isNew && typeof stageTrackerNewLines === 'function') tip = (typeof stageTrackerOpenTriageNeeded === 'function' && stageTrackerOpenTriageNeeded(item) ? 'Triage needed — ' : 'New — ') + stageTrackerNewLines(item, nowMs).join(' — ');
          if (sg.advisory) tip = (item.advisory && item.advisory.caption) || 'Advisory for maintainers · read, then close';
          if (sg.openSince) tip += ' — open since ' + new Date(sg.openSince.ms).toISOString() + (sg.openSince.fromCreated ? ' (opened)' : '');
          // The shared Merge gate word hides 080 vs 095 vs 100; the chevron tooltip carries it.
          // With merge_gate in the payload the sentence is the gate's own
          // (what holds it, next sweep, soak left — #15073).
          const gateInfo = !sg.detour && typeof stageTrackerMergeGateInfo === 'function' ? stageTrackerMergeGateInfo(item, nowMs) : null;
          if (gateInfo) tip = sg.label + ' — ' + gateInfo.tip;
          else if (!sg.detour && (curStage === 60 || curStage === 80)) tip = sg.label + ' — not yet admitted' + (item.caption || item.reason ? ' — ' + (item.caption || item.reason) : '');
          else if (curStage === STAGE_TRACKER_PRE_MERGE_SOAK) tip = sg.label + ' — admitted, soaking' + stageTrackerSoakLeft(item);
          else if (curStage === 100) tip = sg.label + ' — admitted, merging';
          // Why a person merges a 090 PR leads its tooltip (hivecommons/hive#14658).
          // The reason is the whole story; the stage description would only repeat it (hivecommons/hive#15394).
          else if (curStage === STAGE_TRACKER_READY_FOR_HUMAN_MERGE && item.human_merge_reason_text) tip = ((sg.detour && sg.detour.text) || sg.label) + ' — ' + String(item.human_merge_reason_text).trim();
          if (actorTip && !sg.cap) tip += actorTip;
          if (claimant && !sg.cap) tip += ' · ' + stageTrackerClaimantLine(claimant, nowMs);
          // The sentinel's reason leads the lane tooltip so the security
          // implication is read before the stage mechanics.
          if (sentinel) tip = stageTrackerSentinelTip(item) + ' — ' + tip;
          else if (sentinelSuffix) tip = tip + ' — ' + stageTrackerSentinelTip(item);
          // Working or waiting leads: what happens now, or what it waits on.
          if (!sg.cap && typeof stageTrackerCueLead === 'function') tip = stageTrackerCueLead(item, worker, nowMs) + tip;
        }
        let inner;
        // A lit chevron on a row parked on a person ("Needs human decision",
        // "Triage needed") is the trigger of the decision popover: the word
        // becomes a button and the chevron drops its native title so the
        // popover (which repeats the stage line) is its one hover text.
        const decision = active && !sg.cap && typeof stageTrackerDecisionOf === 'function' ? stageTrackerDecisionOf(item) : null;
        // On the merge gate the word carries its live detail and the
        // leading glyph says in words what hive is doing (#15073).
        const gateWord = active && !sg.cap && !sg.detour && typeof stageTrackerMergeGateWordHtml === 'function' ? stageTrackerMergeGateWordHtml(item, nowMs) : '';
        // A detour word that already names the holder ("Returned to
        // @bob's clanker") does not repeat them after the dot.
        const detourHolder = sg.detour && holderShort && sg.detour.text.indexOf(holderShort) >= 0 ? '' : holderHtml;
        const word = sg.detour ? `<span class="ham-detour${detourHolder ? ' ham-has-holder' : ''}">${esc(sg.detour.text)}${detourHolder}</span>` : (gateWord || `<span class="ham-seg-t${holderHtml ? ' ham-has-holder' : ''}">${esc(sg.short)}${holderHtml}</span>`);
        const gateCue = gateWord && typeof stageTrackerGateCueHtml === 'function' ? stageTrackerGateCueHtml(item, nowMs) : '';
        // The agent-working spinner (tracker-cues) names the agent and what
        // it did, so a spinning glyph is never unexplained.
        const workTip = worker && typeof stageTrackerCueLead === 'function' ? stageTrackerCueLead(item, worker, nowMs).replace(/ — $/, '') : '';
        const workIco = workTip ? `<span class="ham-active-ico-wrap" role="img" title="${esc(workTip)}" aria-label="${esc(workTip)}">${stageTrackerIcon(sg.number, 'ham-ico ham-active-ico')}</span>` : stageTrackerIcon(sg.number, 'ham-ico ham-active-ico');
        // The cap chevron is the ✓ only once the PR is merged; until then
        // the column is the row's history and wears the history mark.
        if (sg.cap) inner = sg.terminal ? stageTrackerIcon(sg.number, 'ham-ico') : stageTrackerIcon('history', 'ham-ico ham-history-ico', 'History — no decisions or actions recorded yet');
        else if (active) inner = gateCue + workIco + (decision ? stageTrackerDecisionTriggerHtml(decision, word, tip) : word) + (actorIco || holderIco);
        else inner = stageTrackerIcon(sg.number, 'ham-ico');
        return `<li class="${cls}"${active ? `${sg.cap ? '' : motionAttr}${decision ? '' : ` title="${esc(tip)}"`} aria-current="step"` : ''}${sentinel ? ` aria-label="${esc(STAGE_TRACKER_SENTINEL_WORD + ' — ' + stageTrackerSentinelTip(item))}"` : (sentinelSuffix ? ` aria-label="${esc(sg.detour.text + ' — ' + stageTrackerSentinelTip(item))}"` : '')}>${inner}</li>`;
      }).join('');
    }

    // Compact strips retain their column alignment; the current step gets a
    // full-width caption so detours and the next actor never need a hover.
    function stageTrackerCurrentStageHtml(item, stages, range, nowMs) {
      if (item.superseded && item.superseded.closer_pr) return `<p class="ham-current-stage">${esc(stageTrackerHeadline(item) + (item.superseded.keep_reason ? " · " + item.superseded.keep_reason : ""))}</p>`;
      const active = stageTrackerSegments(item, stages, nowMs).find(sg => sg.state === 'active'
        && (!range || range.from == null || sg.number >= range.from)
        && (!range || range.to == null || sg.number <= range.to));
      if (!active) return '';
      const actor = active.cap ? '' : stageTrackerActorTip(item);
      const regressed = stageTrackerRegressionText(item, Date.now());
      const text = active.short + (active.detour && active.detour.text !== active.short ? ' · ' + active.detour.text : '') + actor + (regressed ? ' · ' + regressed : '');
      return `<p class="ham-current-stage">${esc(text)}</p>`;
    }

    // "040 Covered by PR" — the stage code plus the strip word the payload
    // names for it (the code alone when the stage table is not loaded yet).
    function stageTrackerStageWord(n) {
      const num = Number(n) || 0;
      const st = ((_stageTracker && _stageTracker.stages) || []).find(s => Number(s.number) === num);
      const code = String(num).padStart(3, '0');
      return st && (st.short || st.label) ? code + ' ' + (st.short || st.label) : code;
    }

    // Modifiers that park an item on a person. A regression on a row that
    // carries none of these and sits on a hive-driven stage is the hive's
    // to work through, so the dashboard must not dress it as a human call.
    const STAGE_TRACKER_HUMAN_MODS = ['hold', 'unauthorized', 'needs-human', 'needs-decision', 'needs-rebase', 'blocked', 'stalled'];
    const STAGE_TRACKER_HANDLED_TEXT = 'stage ↓';
    const STAGE_TRACKER_HANDLED_NOTE = 'the hive is handling it — no action needed; clears on the next forward step';

    // True when the item is regressed but nothing about it waits on a
    // person: no human-gate modifier and not on a human-gated stage. Such a
    // row gets a muted "stage ↓" pill and stays out of "Needs attention".
    function stageTrackerRegressionHandled(item) {
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (mods.indexOf('regressed') < 0) return false;
      if (mods.some(m => STAGE_TRACKER_HUMAN_MODS.indexOf(m) >= 0)) return false;
      return STAGE_TRACKER_ATTENTION_STAGES.indexOf(Number(item.stage) || 0) < 0;
    }

    // The regressed pill's explanation: which stage the item fell from and
    // to, when, and the reason recorded with the fall — so "regressed" never
    // needs a trip to the store to read. '' when the item is not regressed.
    // A hive-handled regression adds the note that nobody needs to act.
    function stageTrackerRegressionText(item, nowMs) {
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const from = Number(item.regressed_from) || 0;
      if (mods.indexOf('regressed') < 0 || !from) return '';
      const cur = Number(item.stage) || 0;
      const to = Number(item.regressed_to) || cur;
      const atMs = Date.parse(item.regressed_at || '');
      let text = 'regressed: ' + stageTrackerStageWord(from) + ' → ' + stageTrackerStageWord(to);
      if (!isNaN(atMs)) text += ' at ' + agentNowClockTime(atMs, nowMs || Date.now());
      if (item.regressed_reason) text += ' because ' + String(item.regressed_reason);
      if (cur && cur !== to) text += ' · now ' + stageTrackerStageWord(cur);
      if (stageTrackerRegressionHandled(item)) text += ' · ' + STAGE_TRACKER_HANDLED_NOTE;
      return text;
    }

    // The name of one trail hop. The payload's history entries carry
    // {number, slug, at, ...} (StageTrackerHop); the strip word for the
    // number wins, the slug stands in before the stage table is loaded, and
    // '' means the hop cannot be named and is left out of the trail.
    function stageTrackerHopName(h) {
      if (!h || typeof h !== 'object') return '';
      const raw = h.number !== undefined ? h.number : h.stage;
      const num = Number(raw);
      if (raw !== undefined && raw !== null && raw !== '' && !isNaN(num)) {
        if (num === 0) return 'Open';
        const st = ((_stageTracker && _stageTracker.stages) || []).find(s => Number(s.number) === num);
        if (st && (st.short || st.label)) return st.short || st.label;
      }
      return h.slug ? String(h.slug) : '';
    }

    // "Trail: CI / Review → Needs human decision" — the last few hops of the
    // item's history by stage name; '' when fewer than two hops can be named.
    function stageTrackerTrailText(item) {
      const hist = Array.isArray(item && item.history) ? item.history : [];
      const names = hist.slice(-STAGE_TRACKER_TRAIL_HOPS).map(stageTrackerHopName).filter(Boolean);
      return names.length > 1 ? names.join(' → ') : '';
    }

    // Row tooltip: headline, reason, repo, author, flags, since, recent hops.
    function stageTrackerTooltip(item, nowMs) {
      const ak = item.author_kind || '';
      // Plain words for author_kind in hover text (hivecommons/hive#15394).
      const akPlain = { human: 'person', clanker: 'agent', hive: 'hive', bot: 'bot' };
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const sinceMs = Date.parse(item.stage_since || '');
      const trail = stageTrackerTrailText(item);
      const openHeld = (Number(item.stage) || 0) === 0 && mods.indexOf('hold') >= 0;
      const lines = openHeld
        ? [stageTrackerHeadline(item) + (item.caption || item.reason ? ' · ' + (item.caption || item.reason) : '') + (item.repo ? ' · ' + item.repo : '')]
        : [stageTrackerHeadline(item), item.caption || item.reason || '', item.repo || ''];
      if (item.author || ak) lines.push('Author: ' + (item.author ? '@' + item.author : '') + (ak ? ' (' + (akPlain[ak] || ak) + (item.author_kind_source === 'inferred' ? ', inferred' : '') + ')' : '') + (item.tier ? ' · ' + item.tier : ''));
      if (item.origin) lines.push('Origin: origin/' + item.origin);
      if (item.kind === 'issue' && Array.isArray(item.linked_prs) && item.linked_prs.length) lines.push('Covering PR: ' + item.linked_prs.map(lp => Number(lp.number) || 0).sort((x, y) => x - y).map(n => '#' + n).join(', '));
      else if (item.kind === 'issue' && item.linked_pr) lines.push('Covering PR: #' + item.linked_pr + (typeof stageTrackerLineageClause === 'function' ? stageTrackerLineageClause(item) : ''));
      const actorLine = stageTrackerActorTip(item);
      if (actorLine) lines.push(actorLine.replace(/^ · /, ''));
      if (typeof stageTrackerNewLines === 'function') lines.push(...stageTrackerNewLines(item, nowMs));
      const blockedPR = mods.indexOf('blocked') >= 0 ? stageTrackerBlockedOnPR(item) : null;
      if (blockedPR) lines.push(blockedPR.tip);
      if (typeof stageTrackerTrainLines === 'function') lines.push(...stageTrackerTrainLines(item));
      // Guarded: the test harnesses load helpers by name, and this one is additive.
      const adoption = typeof stageTrackerAdoptionTip === 'function' ? stageTrackerAdoptionTip(item) : '';
      if (adoption) lines.push('Adoption: ' + adoption + (typeof stageTrackerAdoptOffered === 'function' && stageTrackerAdoptOffered(item) ? ' (row menu → Adopt)' : ''));
      const presence = typeof stageTrackerPresenceTip === 'function' ? stageTrackerPresenceTip(item, nowMs) : '';
      if (presence) lines.push('Presence: ' + presence);
      const rework = typeof stageTrackerReworkTip === 'function' ? stageTrackerReworkTip(item, nowMs) : '';
      if (rework) lines.push('Rework: ' + rework);
      if (mods.length) lines.push('Flags: ' + mods.map(m => STAGE_TRACKER_MOD_TEXT[m] || m).join(', '));
      const stale = stageTrackerStaleLabelsText(item);
      if (stale) lines.push(stale);
      const deferred = stageTrackerDeferredMods(item);
      if (deferred.length) lines.push(STAGE_TRACKER_DEFERRED_NOTE + deferred.map(m => STAGE_TRACKER_MOD_TEXT[m] || m).join(', '));
      if (item.sentinel_alert && deferred.indexOf(STAGE_TRACKER_SENTINEL_CHIP) < 0) lines.push(stageTrackerSentinelTip(item));
      const regressed = stageTrackerRegressionText(item, nowMs);
      if (regressed) lines.push(regressed.charAt(0).toUpperCase() + regressed.slice(1));
      if (mods.indexOf('unauthorized') >= 0 && item.unauthorized) lines.push('Unauthorized: ' + item.unauthorized);
      if (mods.indexOf('hold') >= 0 && typeof stageTrackerHoldPill === 'function' && stageTrackerHoldPill(item)) lines.push(stageTrackerHoldPill(item));
      if (sinceMs) lines.push('Since ' + agentNowClockTime(sinceMs, nowMs) + ' · ' + agentNowAgo(nowMs - sinceMs));
      if (trail) lines.push('Trail: ' + trail);
      // Say it once: a line the caption already said is not repeated (hivecommons/hive#15394).
      return lines.filter((l, i) => l && lines.indexOf(l) === i).join('\n');
    }

    // The sentinel chip: drawn in the chip area (beside needs rebase /
    // regressed) on every lane but the one whose word already says it.
    // Pure: the "supersedes #N" chip on a live PR that replaced a closed
    // original (`supersedes`: {number, url, closed_at, open} — the hive's
    // close-first supersession, hivecommons/hive#15135). The closed PR is
    // never a row; this chip is its only trace. '' without one.
    function stageTrackerSupersedesTip(item, nowMs) {
      const sp = item && item.supersedes;
      if (!sp || !(Number(sp.number) > 0)) return '';
      const ago = stageTrackerAgoText(Date.parse(sp.closed_at || ''), nowMs);
      if (sp.open) return 'Replaces #' + Number(sp.number) + ' (left open with the note' + (ago ? ', ' + ago : '') + ')';
      return 'Replaces #' + Number(sp.number) + ' (closed' + (ago ? ' ' + ago : '') + ')';
    }
    function stageTrackerSupersedesChipHtml(item, nowMs) {
      const tip = stageTrackerSupersedesTip(item, nowMs);
      if (!tip) return '';
      const sp = item.supersedes;
      const text = 'supersedes #' + Number(sp.number);
      const attrs = `class="ham-badge ham-badge-supersedes" title="${esc(tip)}" aria-label="${esc(tip)}"`;
      if (sp.url) return `<a ${attrs} href="${esc(sp.url)}" target="_blank" rel="noopener">${esc(text)}</a>`;
      return `<span ${attrs}>${esc(text)}</span>`;
    }
    function stageTrackerSentinelChipHtml(item) {
      if (!item || !item.sentinel_alert || stageTrackerSentinelInLane(item) || stageTrackerSentinelAsSuffix(item) || stageTrackerDeferredMods(item).indexOf(STAGE_TRACKER_SENTINEL_CHIP) >= 0) return '';
      return `<span class="ham-badge ham-badge-sentinel" title="${esc(stageTrackerSentinelTip(item))}" aria-label="${esc('Sentinel alert — ' + stageTrackerSentinelTip(item))}">${esc(STAGE_TRACKER_SENTINEL_CHIP)}</span>`;
    }

    // Pure: the hold pill text from the row's `hold` provenance ('' when the
    // pass could not attribute the hold, so the plain "hold" word stays).
    function stageTrackerHoldPill(item) {
      const h = item && item.hold;
      if (!h || !h.by) return '';
      if (h.pill) return h.pill;
      const who = h.by === 'hive' ? 'hive' : '@' + h.by;
      return 'Held by ' + who + (h.reason ? ' · ' + h.reason : '');
    }
    // Pure: the hold pill's hover text — who, why, since when.
    function stageTrackerHoldTip(item, nowMs) {
      const h = item && item.hold;
      if (!h || !h.by) return '';
      const sinceMs = Date.parse(h.since || '');
      const lines = [stageTrackerHoldPill(item)];
      if (h.by === 'hive') lines.push(h.source === 'reporter-trust' ? 'The hive set this hold itself: the linked issue\'s reporter is below the trust the gate requires. Release it to let the lane merge, or keep it until a maintainer has reviewed the reporter.' : 'The hive set this hold itself.');
      if (sinceMs) lines.push('Since ' + agentNowClockTime(sinceMs, nowMs) + ' · ' + agentNowAgo(nowMs - sinceMs));
      return lines.join('\n');
    }

    function stageTrackerChipsHtml(item) {
      const sentinel = stageTrackerSentinelChipHtml(item) + (typeof stageTrackerAdoptionChipHtml === 'function' ? stageTrackerAdoptionChipHtml(item) : '') + (typeof stageTrackerPresenceChipHtml === 'function' ? stageTrackerPresenceChipHtml(item) : '') + (typeof stageTrackerReroutePillHtml === 'function' ? stageTrackerReroutePillHtml(item) : '') + (typeof stageTrackerSupersedesChipHtml === 'function' ? stageTrackerSupersedesChipHtml(item, Date.now()) : '');
      const mods = (Array.isArray(item.modifiers) ? item.modifiers.slice() : [])
        .sort((x, y) => STAGE_TRACKER_MOD_PRIORITY.indexOf(x) - STAGE_TRACKER_MOD_PRIORITY.indexOf(y));
      if (!mods.length) return sentinel;
      // A regression the hive is working through yields the lead pill to any
      // other modifier and, alone, draws muted as "stage ↓" rather than red.
      const handled = stageTrackerRegressionHandled(item);
      if (handled && mods.length > 1) mods.push(mods.shift());
      let text = handled && mods[0] === 'regressed' ? STAGE_TRACKER_HANDLED_TEXT : (STAGE_TRACKER_MOD_TEXT[mods[0]] || mods[0].replace(/-/g, ' '));
      // A hold the stage-label pass could attribute names who set it and why
      // ("Held by hive · reporter trust (issue #N by @X, contributor)"), so the
      // operator is not left reading "hold +1" (hivecommons/hive#15020).
      // Guarded: the test harnesses load helpers by name, and this one is additive.
      const holdPill = mods[0] === 'hold' && typeof stageTrackerHoldPill === 'function' ? stageTrackerHoldPill(item) : '';
      if (holdPill) text = holdPill;
      // A row the scheduler withholds for open GitHub "blocked by" dependencies names them (hivecommons/hive#14765);
      // one blocked on a PR (hive:blocked-on marker) names it and links to it (hivecommons/hive#14952).
      const blockedPR = mods[0] === 'blocked' ? stageTrackerBlockedOnPR(item) : null;
      if (mods[0] === 'blocked' && Array.isArray(item.blocked_on) && item.blocked_on.length) text = stageTrackerBlockedTip(item);
      else if (blockedPR) text = 'blocked on ' + blockedPR.short;
      // A blocked row with a hive:recheck-after deadline ("blocked until <RFC3339>") names when the sweep re-checks it.
      const recheck = mods[0] === 'blocked' ? /^blocked until (\S+)/i.exec(String(item.bucket_reason || '')) : null;
      const recheckMs = recheck ? Date.parse(recheck[1]) : NaN;
      if (!isNaN(recheckMs)) text += ' · re-check at ' + agentNowClockTime(recheckMs, Date.now());
      // The regressed pill (lead or folded into "+1") explains itself on
      // hover; failing that, a draft pill (lead or folded) does.
      const regressed = stageTrackerRegressionText(item, Date.now());
      // An unauthorized pill names the missing App permission and the human step;
      // the regressed pill explains itself; a blocked pill names what it waits on;
      // failing all three, a draft pill does.
      const anyBlockedPR = mods.indexOf('blocked') >= 0 ? stageTrackerBlockedOnPR(item) : null;
      const tip = (mods.indexOf('unauthorized') >= 0 && String(item.unauthorized || '').trim()) || regressed
        || (holdPill && typeof stageTrackerHoldTip === 'function' ? stageTrackerHoldTip(item, Date.now()) : '')
        || (mods.indexOf('blocked') >= 0 && Array.isArray(item.blocked_on) && item.blocked_on.length ? 'Withheld from the scanner until ' + item.blocked_on.join(', ') + (item.blocked_on.length > 1 ? ' close' : ' closes') : '')
        || (anyBlockedPR ? anyBlockedPR.tip : '')
        || (mods.indexOf('draft') >= 0 ? STAGE_TRACKER_DRAFT_TIP : '');
      const cls = 'ham-mod-' + mods[0] + (handled && mods[0] === 'regressed' ? ' ham-mod-handled' : '');
      const inner = `${esc(text)}${mods.length > 1 ? ' +' + (mods.length - 1) : ''}`;
      // The lead pill of a row blocked on a PR is the link to that PR.
      if (blockedPR && blockedPR.url) return sentinel + `<a class="ham-badge ham-badge-mod ${esc(cls)}" href="${esc(blockedPR.url)}" target="_blank" rel="noopener"${tip ? ` title="${esc(tip)}"` : ''} aria-label="${esc(blockedPR.tip)}">${inner}</a>`;
      // A lead needs-human / needs human decision pill opens the decision
      // popover (what to decide, who asked, the applicable actions) instead
      // of a native tooltip.
      // A hive-set hold is a decision too (release or keep): its pill opens the same popover.
      const decision = (mods[0] === 'needs-human' || mods[0] === 'needs-decision' || (holdPill && item.hold && item.hold.by === 'hive')) && typeof stageTrackerDecisionOf === 'function' ? stageTrackerDecisionOf(item) : null;
      if (decision) return sentinel + stageTrackerDecisionTriggerHtml(decision, inner, tip, 'ham-badge ham-badge-mod ' + cls);
      return sentinel + `<span class="ham-badge ham-badge-mod ${esc(cls)}"${tip ? ` title="${esc(tip)}"` : ''}>${inner}</span>`;
    }

    /* ---- "Did I act on this?" badge ------------------------------------
       Owner (2026-10-28): "I need a way to see if I selected an action on
       an item so I can keep track of whether or not I addressed it.
       Hovering over the indicator should show me what action I requested."
       The server keeps a ledger of every row action submitted from the
       dashboard (operator_actions.go) and the tracker payload carries the
       newest one per item as `last_operator_action` with a derived state:
       waiting (same stage, recent), addressed (the item moved on, or the
       control asked for is in effect), stuck (same stage past the window),
       pending (Let hive decide still running), failed. The row wears a
       history badge (clock + arrow, with the action count) in the cap cell
       — neutral at rest, amber when stuck, red when failed, dotted while
       pending — whose in-app tooltip names the action,
       when, the outcome and what hive did next. The in-flight spinner (the
       request still on the wire) is a different thing drawn on the chevron;
       this badge appears once the server has recorded the action. */
    const STAGE_TRACKER_ACTED_LABELS = {
      approve: 'Approve', 'give-direction': 'Give direction', 'close-superseded': 'Close as superseded', wontfix: "Close as won't-fix", 'close-read': 'Closed (read)',
      'let-hive-decide': 'Let hive decide', hold: 'Hold', release: 'Release hold', 'accept-triage': 'Accept triage', 'mark-needs-human': 'Mark needs-human',
      'merge-now': 'Approve & merge as maintainer', 'queue-merge': 'Approve — let hive merge',
      adopt: 'Adopt', 'adopt-now': 'Adopt now', reroute: 'Reroute to another clanker',
    };
    // "Rebase, then merge as maintainer" is merge-now on a row that needs a
    // rebase (hivecommons/hive#14822): the server relabels the option and
    // answers 202 rebase-queued; the follow-through's steps come back in
    // last_operator_action.followed[] as hops without a stage number.
    const STAGE_TRACKER_REBASE_MERGE_LABEL = 'Rebase, then merge as maintainer';
    const STAGE_TRACKER_ACTION_REBASE_QUEUED = 'rebase-queued';
    function stageTrackerNeedsRebase(item) {
      if (!item) return false;
      if ((Array.isArray(item.modifiers) ? item.modifiers : []).indexOf('needs-rebase') >= 0) return true;
      if ((Array.isArray(item.labels) ? item.labels : []).some(l => String(l).toLowerCase() === 'needs-rebase')) return true;
      const rm = item.rebase_merge;
      return !!(rm && rm.state && rm.state !== 'merged' && rm.state !== 'failed');
    }
    function stageTrackerRebaseMergeLive(item) {
      const rm = item && item.rebase_merge;
      return !!(rm && (rm.state === 'approved' || rm.state === 'rebasing' || rm.state === 'ci-pending'));
    }
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
        case 'pending': return action.action === 'merge-now' ? 'rebasing, then merging…' : 'the hive is deciding…';
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
    const STAGE_TRACKER_ACTED_OPTION_ID = { 'let-hive-decide': 'decide:delegate', approve: 'decide:approve', 'give-direction': 'decide:direction', 'close-superseded': 'decide:close-superseded', wontfix: 'decide:close-wontfix', 'close-read': 'decide:close-read', 'accept-triage': 'accept-triage', hold: 'hold', 'merge-now': 'decide:merge-now', 'queue-merge': 'decide:queue-merge', 're-author': 'decide:re-author', adopt: 'decide:adopt', 'adopt-now': 'decide:adopt-now', reroute: 'decide:reroute' };
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

    // Ghost strip for a title line: the lead cell (Open..Claimed) carries
    // the item's own number + title, the PR cell (PR in progress..Merge gate)
    // the given refs, and the cap cell the modifier chip, bottom-aligned so
    // it sits on the last title line, directly over the strip's right end.
    // The since column is an empty spacer here (same width as the strip
    // row's, which carries the time on step): a wrapped title must not float
    // the age away from the chevrons it describes.
    function stageTrackerHeadColsHtml(prRefs, leadHtml, chipsHtml) {
      const wide = !(prRefs && prRefs.length);
      return `<div class="ham-strip-row ham-head-cols"><ol class="ham-strip"><li class="ham-seg ham-head-lead${wide ? ' ham-head-wide' : ''}">${leadHtml || ''}</li>${wide ? '' : `<li class="ham-seg ham-head-pr">${prRefs.join('')}</li>`}<li class="ham-seg cap"><span class="ham-badges">${chipsHtml || ''}</span></li></ol><span class="ham-since"></span></div>`;
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
      return { repo: issue.repo, number: lp.number, kind: 'pr', title: lp.title || '', url: lp.url || '', author: lp.author || '', author_kind: lp.author_kind || '',
        stage: lp.stage || 0, stage_since: lp.stage_since || '', stage_slug: lp.stage_slug || '', modifiers: lp.modifiers || [], deferred_modifiers: lp.deferred_modifiers || [], caption: lp.caption || '', reason: lp.reason || '',
        human_approver: lp.human_approver || '', manual_merge: !!lp.manual_merge, human_merge_reason: lp.human_merge_reason || '', human_merge_reason_text: lp.human_merge_reason_text || '', sentinel_alert: !!lp.sentinel_alert, sentinel_reason: lp.sentinel_reason || '', head: lp.head || '', base_ref: lp.base_ref || '', decision_needed: lp.decision_needed || null,
        adoption: lp.adoption || null, takeover: lp.takeover || null, superseded: !!lp.superseded, supersedes: lp.supersedes || null, parent: Number(issue.number) || 0, siblings: open.map(x => Number(x.number) || 0),
        last_operator_action: lp.last_operator_action || null, operator_action_count: lp.operator_action_count || 0, hold: lp.hold || null, stage_actor: lp.stage_actor || null, claimant: lp.claimant || null, presence: lp.presence || null, rework: lp.rework || null, merge_gate: lp.merge_gate || null, merge_train: lp.merge_train || null,
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
      const tail = ago ? ' · ' + ago : '';
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
      return tip.split('@AUTHOR').join(who);
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
      const tip = 'Hive re-authored #' + af.number + (af.author ? ' (by @' + af.author + ')' : '') + ' as this PR with the same commits and sign-off' + (af.decided_by ? '; the decision on #' + af.number + ' was ' + (af.decided_action || 'yours') + ' by @' + af.decided_by : '') + '. #' + af.number + ' is closed as superseded when this merges.';
      return `<a class="ham-badge ham-adopted-from" href="${esc(af.url || '#')}" target="_blank" rel="noopener" title="${esc(tip)}">${esc(text)}</a>`;
    }
    // The row's inline lead action (tracker-actions.js
    // stageTrackerLeadActionHtml), computed against the viewer's own
    // permissions on the item's repo; '' where the actions module or a
    // viewer context is absent (the node harness lifts this file alone).
    function stageTrackerLeadButtonHtml(item, nowMs) {
      if (typeof stageTrackerLeadActionHtml !== 'function' || typeof stageTrackerActionCtx !== 'function' || !item) return '';
      const ctx = stageTrackerActionCtx(item.repo);
      if (!ctx) return '';
      ctx.nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      return stageTrackerLeadActionHtml(item, ctx);
    }
    // PR link, then the lineage chip (null = none), then the base pill.
    function stageTrackerPRPillsHtml(ref, lineage, baseRef) {
      return ref + (lineage == null ? '' : ' ' + lineage) + (baseRef ? ` <span class="ham-badge">${esc(baseRef)}</span>` : '');
    }
    function stageTrackerSubRowHtml(lp, issue, stages, nowMs, ctx) {
      const pr = stageTrackerLinkedPRItem(issue, lp);
      const superseded = typeof stageTrackerLinkedSupersededText === 'function' ? stageTrackerLinkedSupersededText(lp, issue) : '';
      let ref = stageTrackerPRRef(pr.number, pr.url, pr.author_kind, pr.author, pr.title, (pr.title ? pr.title + ' · ' : '') + (superseded || stageTrackerTooltip(pr, nowMs)) + ' · ' + stageTrackerLinkedTip(issue, pr.number, ctx));
      // One line (hivecommons/hive: "consolidate from 3 lines of text to
      // 1"): connector, repo chip, author icon + #N + title, the lineage /
      // adopted-from chip, then the base pill — all siblings of the flex-row
      // PR cell; the title ellipsizes, the pills never shrink.
      const lineage = superseded ? '' : ((typeof stageTrackerLineageChipHtml === 'function' ? stageTrackerLineageChipHtml(pr, nowMs) : '') || (typeof stageTrackerAdoptedFromChipHtml === 'function' ? stageTrackerAdoptedFromChipHtml(pr, nowMs) : ''));
      const chip = ctx && ctx.repoChips ? stageTrackerRepoChipHtml(pr.repo, ctx) : '';
      if (chip) ref = `<span class="ham-pr-line">${chip}${ref}</span>`;
      ref = stageTrackerPRPillsHtml(ref, lineage || null, lp.base_ref);
      ref = STAGE_TRACKER_LINKED_GLYPH + ref;
      if (superseded) {
        // The adopted original: one dimmed line, no chevron track, no row
        // tools, no needs-human styling — the copy carries the chevron.
        return `<div class="ham-sub ham-sub-superseded" data-ham-kind="pr" data-ham-stage="${pr.stage}" data-ham-sub-key="${esc(stageTrackerItemKey(pr))}" data-ham-superseded-by="${Number(lp.superseded_by || lp.adopted_by) || 0}">
          <div class="ham-row-head">
            ${stageTrackerHeadColsHtml([ref + ` <span class="ham-superseded-note">· ${esc(superseded)}</span>`], '', stageTrackerActedBadgeHtml(pr, nowMs))}
          </div>
        </div>`;
      }
      return `<div class="ham-sub${typeof stageTrackerCueClasses === 'function' ? stageTrackerCueClasses(pr, ctx) : ''}" data-ham-kind="pr" data-ham-stage="${pr.stage}" data-ham-sub-key="${esc(stageTrackerItemKey(pr))}">
          ${typeof stageTrackerRowToolsHtml === 'function' ? stageTrackerRowToolsHtml(pr) : ''}
          <div class="ham-row-head">
            ${stageTrackerHeadColsHtml([ref], '', stageTrackerActedBadgeHtml(pr, nowMs) + stageTrackerChipsHtml(pr) + (typeof stageTrackerLeadButtonHtml === 'function' ? stageTrackerLeadButtonHtml(pr, nowMs) : ''))}
          </div>
          <div class="ham-strip-row">
            <ol class="ham-strip" aria-label="Pipeline progress"${stageTrackerMoveAttrs(stageTrackerItemKey(pr), ctx)}>${stageTrackerStripItems(pr, stages, { from: STAGE_TRACKER_PR_COLUMN }, nowMs, ctx)}</ol>
            ${stageTrackerSinceHtml(pr, nowMs)}
          </div>
          ${stageTrackerCurrentStageHtml(pr, stages, { from: STAGE_TRACKER_PR_COLUMN })}${typeof stageTrackerEventHtml === 'function' ? stageTrackerEventHtml(stageTrackerItemKey(pr), ctx) : ''}
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
      const linked = Array.isArray(item.linked_prs) ? item.linked_prs.filter(lp => stageTrackerLinkedDrawnHere(lp, item)) : (item.linked_pr ? [{ number: item.linked_pr }] : []);
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

    // List order: furthest along the strip first — a covered issue counts as
    // far along as its furthest covering PR — then longest-waiting first.
    function stageTrackerSortKey(item, stages) {
      const linked = item.kind === 'issue' ? stageTrackerLinkedSorted(item, stages) : [];
      const lead = linked.length && linked[0].stage ? linked[0] : item;
      const kind = lead === item ? item.kind : 'pr';
      return { anchor: stageTrackerAnchor(lead.stage, stages, kind, lead), since: Date.parse(lead.stage_since || '') || Infinity };
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
      let prRefs = [], shown = item, range = null, subs = '';
      if (isIssue) {
        const linked = stageTrackerLinkedSorted(item, stages);
        if (linked.length === 1) {
          // One covering PR: a single entry whose strip the PR's stage drives.
          const lp = linked[0];
          const who = (lp.author_kind || 'hive') + (lp.author ? ' · @' + lp.author : '');
          prRefs = [STAGE_TRACKER_LINKED_GLYPH + stageTrackerPRPillsHtml(stageTrackerPRRef(lp.number, lp.url, lp.author_kind || '', lp.author, lp.title, (lp.title ? lp.title + '\n' : '') + 'Author: ' + who + ' · ' + stageTrackerLinkedTip(item, lp.number, ctx)), lp.lineage && typeof stageTrackerLineageChipHtml === 'function' ? (stageTrackerLineageChipHtml(lp, nowMs) || null) : null, lp.base_ref)];
          shown = Object.assign({}, item, { via_pr: true, stage: lp.stage || 40, stage_since: lp.stage_since || item.stage_since, modifiers: lp.modifiers || item.modifiers, deferred_modifiers: lp.deferred_modifiers || item.deferred_modifiers || [], caption: lp.caption || item.caption, reason: lp.reason || item.reason,
            human_approver: lp.human_approver || '', manual_merge: !!lp.manual_merge, human_merge_reason: lp.human_merge_reason || '', human_merge_reason_text: lp.human_merge_reason_text || '', sentinel_alert: !!lp.sentinel_alert, sentinel_reason: lp.sentinel_reason || '', head: lp.head || '', decision_needed: lp.decision_needed || null, hold: lp.hold || null, adoption: lp.adoption || null, stage_actor: lp.stage_actor || item.stage_actor || null, claimant: lp.claimant || null, presence: lp.presence || null, rework: lp.rework || null, merge_gate: lp.merge_gate || null, merge_train: lp.merge_train || null, decided: lp.decided || item.decided || null });
        } else if (linked.length) {
          // Several covering PRs: the issue strip ends at Claimed, all passed
          // and nothing lit; one sub-row per PR carries the live chevron.
          range = { to: 20 };
          if (stageTrackerAnchor(item.stage, stages, item.kind) <= 20) shown = Object.assign({}, item, { stage: 40 });
          subs = linked.map(lp => stageTrackerSubRowHtml(lp, item, stages, nowMs, ctx)).join('');
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
            <span class="ham-title" title="${esc(title)}">${esc(title)}</span>
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
            ${li.title ? `<span class="ham-title" title="${esc(li.title)}">${esc(li.title)}</span>` : `<span class="ham-title ham-no-issue-t">${esc(li.placeholder)}</span>`}${li.soft ? ` <span class="ham-badge ham-issue-refs-chip" title="${esc(STAGE_TRACKER_SOFT_LINK_TIP)}">refs</span>` : ''}${li.closed ? ' ' + stageTrackerClosedChipHtml(item, li) : ''}
          </div>`
          : `<div class="ham-head-main ham-no-issue">
            ${stageTrackerIcon('issue', 'ham-kind-ico')}${chip}
            <span class="ham-title ham-no-issue-t">${esc(li.text)}</span>
          </div>`;
      }
      const prs = typeof stageTrackerRowPRKeys === 'function' ? stageTrackerRowPRKeys(item) : '';
      const trainKey = typeof stageTrackerTrainKey === 'function' ? stageTrackerTrainKey(item) : '';
      return `<div class="ham-row${typeof stageTrackerCueClasses === 'function' ? stageTrackerCueClasses(shown, ctx) : ''}${typeof stageTrackerTrainRowClass === 'function' ? stageTrackerTrainRowClass(item, ctx.prevRow || null, ctx.nextRow || null) : ''}" tabindex="0" data-ham-kind="${esc(item.kind)}" data-ham-repo="${esc(item.repo || '')}" data-ham-stage="${Number(item.stage) || 0}"${prs ? ` data-ham-prs="${esc(prs)}"` : ''}${trainKey ? ` data-ham-train="${esc(trainKey)}"` : ''}>
        ${stageTrackerRowToolsHtml(item)}
        <div class="ham-row-head">
          ${stageTrackerHeadColsHtml(prRefs, leadHtml, stageTrackerActedBadgeHtml(item, nowMs) + stageTrackerChipsHtml(shown) + (typeof stageTrackerLeadButtonHtml === 'function' ? stageTrackerLeadButtonHtml(item, nowMs) : ''))}
        </div>
        <div class="ham-strip-row">
          <ol class="ham-strip" aria-label="Pipeline progress"${stageTrackerMoveAttrs(stageTrackerItemKey(item), ctx)}>${stageTrackerStripItems(shown, stages, range, nowMs, ctx)}</ol>
          ${stageTrackerSinceHtml(shown, nowMs)}
        </div>${stageTrackerCurrentStageHtml(shown, stages, range, nowMs)}${typeof stageTrackerEventHtml === 'function' ? stageTrackerEventHtml(stageTrackerItemKey(item), ctx) : ''}${subs}
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
