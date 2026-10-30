
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
      const segs = stageTrackerSegments(item, stages, nowMs, ctx);
      const trains = ctx && ctx.trains;
      const curStage = Number(item.stage) || 0;
      const from = range && range.from != null ? range.from : -Infinity;
      const to = range && range.to != null ? range.to : Infinity;
      const actorKind = stageTrackerActorKind(item);
      const actorIco = actorKind ? stageTrackerIcon(actorKind, 'ham-ico ham-actor-ico', 'Next move: ' + actorKind) : '';
      const actorTip = stageTrackerActorTip(item);
      const claimant = typeof stageTrackerClaimant === 'function' ? stageTrackerClaimant(item) : null;
      const holderShort = claimant ? stageTrackerClaimantShort(claimant) : '';
      // "· @odttlnt's clanker · ■ offline 3h": the holder's live state
      // rides after the holder when the row's presence is theirs.
      const holderPresence = holderShort && typeof stageTrackerHolderPresenceHtml === 'function' ? stageTrackerHolderPresenceHtml(item, claimant, nowMs) : '';
      const holderHtml = holderShort ? `<span class="ham-holder"> · ${esc(holderShort)}${holderPresence}</span>` : '';
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
          const gateInfo = !sg.detour && typeof stageTrackerMergeGateInfo === 'function' ? stageTrackerMergeGateInfo(item, nowMs, trains) : null;
          if (gateInfo) tip = sg.label + ' — ' + gateInfo.tip;
          else if (!sg.detour && (curStage === 60 || curStage === 80)) tip = sg.label + ' — not yet admitted' + (item.caption || item.reason ? ' — ' + (item.caption || item.reason) : '');
          else if (curStage === STAGE_TRACKER_PRE_MERGE_SOAK) tip = sg.label + ' — admitted, soaking' + stageTrackerSoakLeft(item);
          else if (curStage === 100) tip = sg.label + ' — admitted, merging';
          // Why a person merges a 090 PR leads its tooltip (hivecommons/hive#14658).
          // The reason is the whole story; the stage description would only repeat it (hivecommons/hive#15394).
          else if (curStage === STAGE_TRACKER_READY_FOR_HUMAN_MERGE && item.human_merge_reason_text) tip = ((sg.detour && sg.detour.text) || sg.label) + ' — ' + String(item.human_merge_reason_text).trim();
          // A refusal on an earlier head is history, not the word: the
          // chevron reads the live stage and the hover keeps "merge attempt
          // on previous head failed: branch protection".
          const priorNote = !sg.cap && typeof stageTrackerPriorHeadMergeFailedNote === 'function' ? stageTrackerPriorHeadMergeFailedNote(item) : '';
          if (priorNote) tip += ' — ' + priorNote;
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
        const gateWord = active && !sg.cap && !sg.detour && typeof stageTrackerMergeGateWordHtml === 'function' ? stageTrackerMergeGateWordHtml(item, nowMs, trains) : '';
        // A detour word that already names the holder ("Returned to
        // @bob's clanker") does not repeat them after the dot.
        const detourHolder = sg.detour && holderShort && sg.detour.text.indexOf(holderShort) >= 0 ? '' : holderHtml;
        const word = sg.detour ? `<span class="ham-detour${detourHolder ? ' ham-has-holder' : ''}">${esc(sg.detour.text)}${detourHolder}</span>` : (gateWord || `<span class="ham-seg-t${holderHtml ? ' ham-has-holder' : ''}">${esc(sg.short)}${holderHtml}</span>`);
        const gateCue = gateWord && typeof stageTrackerGateCueHtml === 'function' ? stageTrackerGateCueHtml(item, nowMs, trains) : '';
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
      const push = typeof stageTrackerPushTip === 'function' ? stageTrackerPushTip(item, nowMs) : '';
      if (push) lines.push('Push: ' + push);
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
      const sentinel = stageTrackerSentinelChipHtml(item) + (typeof stageTrackerPreparationChipHtml === 'function' ? stageTrackerPreparationChipHtml(item) : '') + (typeof stageTrackerAdoptionChipHtml === 'function' ? stageTrackerAdoptionChipHtml(item) : '') + (typeof stageTrackerPresenceChipHtml === 'function' ? stageTrackerPresenceChipHtml(item) : '') + (typeof stageTrackerPushChipHtml === 'function' ? stageTrackerPushChipHtml(item, Date.now()) : '') + (typeof stageTrackerReroutePillHtml === 'function' ? stageTrackerReroutePillHtml(item) : '') + (typeof stageTrackerSupersedesChipHtml === 'function' ? stageTrackerSupersedesChipHtml(item, Date.now()) : '') + (typeof stageTrackerOverlapBadgeHtml === 'function' ? stageTrackerOverlapBadgeHtml(item) : '');
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
      approve: 'Approve', 'give-direction': 'Give direction', 'close-superseded': 'Close as superseded', wontfix: "Close as won't-fix", 'close-read': 'Closed',
      'let-hive-decide': 'Let hive decide', hold: 'Hold', release: 'Release hold', 'accept-triage': 'Accept triage', 'mark-needs-human': 'Mark needs-human',
      'queue-merge': 'Approve and let hive merge', 'merge-now': 'Merge now as maintainer',
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
