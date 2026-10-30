
    function stageTrackerHoldLabels(item) {
      const labels = Array.isArray(item.labels) ? item.labels : [];
      return labels.filter(label => {
        const l = String(label || '').trim().toLowerCase();
        return l === 'do-not-merge' || l.startsWith('do-not-merge/') || l.startsWith('hive-pause/') ||
          l.indexOf('hold') >= 0 || l.indexOf('on-hold') >= 0 || l.indexOf('hold/review') >= 0;
      });
    }

    // The Open · blocked chevron's reason: the blocker when the payload names
    // one (`blocked_on`: "owner/repo#N" or a list of them, or `blocked_on_pr`:
    // the PR a hive:blocked-on marker names), else the label.
    function stageTrackerBlockedTip(item) {
      const on = item && item.blocked_on;
      const refs = (Array.isArray(on) ? on : (on ? [on] : [])).map(r => String(r || '').trim()).filter(Boolean);
      const pr = stageTrackerBlockedOnPR(item);
      if (pr && refs.indexOf(pr.ref) < 0) refs.push(pr.ref);
      return refs.length ? 'blocked on ' + refs.join(', ') : 'carries the blocked label';
    }

    // The PR a blocked issue waits on (`blocked_on_pr`: {repo, number, url,
    // state}), normalised: `ref` is "owner/repo#N", `short` is "#N" when the
    // PR lives in the item's own repo, `tip` the full explanation. null when
    // the payload names none.
    function stageTrackerBlockedOnPR(item) {
      const pr = item && item.blocked_on_pr;
      const number = pr ? Number(pr.number) || 0 : 0;
      if (!number) return null;
      const repo = String(pr.repo || '').trim();
      const ref = (repo ? repo + '#' : '#') + number;
      const own = String(item.repo || '').trim().toLowerCase();
      const lower = repo.toLowerCase();
      const same = !repo || !own || lower === own || own.endsWith('/' + lower) || lower.endsWith('/' + own);
      const state = String(pr.state || '').trim().toLowerCase();
      const outcome = state === 'merged' ? STAGE_TRACKER_BLOCKED_ON_PR_MERGED : state === 'closed' ? STAGE_TRACKER_BLOCKED_ON_PR_CLOSED : STAGE_TRACKER_BLOCKED_ON_PR_OPEN;
      return { ref, short: same ? '#' + number : ref, url: String(pr.url || '').trim(), state,
        tip: 'Blocked on PR ' + ref + (state ? ' (' + state + ')' : '') + ' — ' + outcome };
    }
    const STAGE_TRACKER_BLOCKED_ON_PR_OPEN = 'lifts when it merges; re-triage if closed unmerged';
    const STAGE_TRACKER_BLOCKED_ON_PR_MERGED = 'merged; the blocked sweep lifts the label next';
    const STAGE_TRACKER_BLOCKED_ON_PR_CLOSED = 'closed unmerged; the blocked sweep re-triages it next';

    // The 072 adoption line (`adoption`: {due, blocked_by, since, nudges,
    // author_online, action, text} — hivecommons/hive#14524): the payload's
    // plain-words text, '' when the row carries none.
    function stageTrackerAdoptionTip(item) {
      const a = item && item.adoption;
      return a && a.text ? String(a.text) : '';
    }
    // Whether the row offers the owner's Adopt: the engine found adoption
    // due and held it for a reason the owner settles (maintainer_policy:
    // ask names `action: "decide:adopt"`).
    function stageTrackerAdoptOffered(item) {
      const a = item && item.kind === 'pr' ? item.adoption : null;
      return !!(a && a.due && a.action === 'decide:adopt');
    }
    // Whether an Adopt now is queued on the row (`adoption.queued_by` /
    // `takeover.forced_by`): the operator already acted; the sweep has not
    // opened the copy yet. The row reads as acted on and offers no Adopt.
    const STAGE_TRACKER_ADOPTION_QUEUED_CHIP = 'adopting… (queued)';
    function stageTrackerAdoptionQueued(item) {
      const a = item && item.kind === 'pr' ? item.adoption : null;
      const tk = item && item.kind === 'pr' ? item.takeover : null;
      return !!((a && a.queued_by) || (tk && tk.forced_by));
    }
    // The chip text: "adopt?" when the owner is asked, "adopting… (queued)"
    // once Adopt now was pressed, else "waiting on @x".
    function stageTrackerAdoptionChipText(item) {
      const a = item && item.adoption;
      if (!a) return '';
      // Guarded: the test harnesses load helpers by name.
      if (typeof stageTrackerAdoptionQueued === 'function' && stageTrackerAdoptionQueued(item)) return STAGE_TRACKER_ADOPTION_QUEUED_CHIP;
      if (stageTrackerAdoptOffered(item)) return 'adopt?';
      if (a.blocked_by) return 'not adopted';
      // Guarded: the test harnesses load helpers by name.
      const text = 'waiting on ' + (a.author ? '@' + a.author : 'author');
      return typeof stageTrackerReworkSuffix === 'function' ? stageTrackerReworkSuffix(text, item) : text;
    }
    function stageTrackerAdoptionChipHtml(item) {
      const tip = stageTrackerAdoptionTip(item);
      if (!tip) return '';
      const offered = stageTrackerAdoptOffered(item);
      const cls = 'ham-badge ham-badge-adoption' + (offered ? ' ham-badge-adoption-ask' : '');
      // The presence indicator rides inside the pill, before the @login
      // (hivecommons/hive#15051); its line joins the tooltip.
      const ptip = stageTrackerPresenceTip(item, Date.now());
      const inner = stageTrackerPresenceInPillHtml(item, stageTrackerAdoptionChipText(item));
      const joined = ptip ? tip + ' · ' + ptip : tip;
      const full = typeof stageTrackerReworkJoinTip === 'function' ? stageTrackerReworkJoinTip(joined, item, Date.now()) : joined;
      // "adopt?" opens the Needs-human popover (its decision_needed carries
      // the Adopt option) when the popover helpers are present.
      const decision = offered && typeof stageTrackerDecisionOf === 'function' && item.decision_needed ? stageTrackerDecisionOf(item) : null;
      if (decision) return stageTrackerDecisionTriggerHtml(decision, inner, full, cls);
      return `<span class="${cls}" title="${esc(full)}" aria-label="${esc('Adoption — ' + full)}">${inner}</span>`;
    }

    // Presence on a waiting-on row (`presence`: {login, state, kind, since,
    // last_seen, next, adopt_at} — hivecommons/hive#15051): whether the
    // person's clanker (their connected contributor agent) is online,
    // offline or not registered here, and the hive's likely next move.
    const STAGE_TRACKER_PRESENCE_STATES = ['online', 'offline', 'unknown'];
    const STAGE_TRACKER_PRESENCE_WORDS = { online: 'clanker online', offline: 'clanker offline', unknown: 'no clanker registered' };
    function stageTrackerPresenceOf(item) {
      const p = item && item.presence;
      if (!p || typeof p !== 'object' || !p.login) return null;
      const state = STAGE_TRACKER_PRESENCE_STATES.indexOf(p.state) >= 0 ? p.state : 'unknown';
      return { login: String(p.login), state, kind: p.kind === 'clanker' ? 'clanker' : 'human', since: p.since || '', last_seen: p.last_seen || '', next: p.next === 'adopt' ? 'adopt' : 'wait', adopt_at: p.adopt_at || '' };
    }
    // "26h" / "40m" / "3d": how long the clanker has been gone, counted from
    // its last heartbeat or the start of the wait, whichever is later.
    function stageTrackerPresenceGone(p, nowMs) {
      const from = Math.max(Date.parse(p.last_seen || '') || 0, Date.parse(p.since || '') || 0);
      if (!from || nowMs <= from) return '';
      const min = Math.floor((nowMs - from) / 60000);
      if (min < 60) return Math.max(1, min) + 'm';
      const h = Math.floor(min / 60);
      return h < 48 ? h + 'h' : Math.floor(h / 24) + 'd';
    }
    // "at 15:10 (in 38m)" / "due now".
    function stageTrackerPresenceAdoptAt(p, nowMs) {
      const at = Date.parse(p.adopt_at || '');
      if (!at) return '';
      if (at <= nowMs) return 'due now';
      const min = Math.ceil((at - nowMs) / 60000);
      const inWords = min < 60 ? 'in ' + min + 'm' : min < 48 * 60 ? 'in ' + Math.floor(min / 60) + 'h' : 'in ' + Math.floor(min / 1440) + 'd';
      return 'at ' + agentNowClockTime(at, nowMs) + ' (' + inWords + ')';
    }
    // The plain-words tooltip:
    //   Waiting on @Danathar's clanker (online, last seen 2 min ago) — hive is waiting for them
    //   Waiting on @hanthor's clanker (offline 26h) — hive will adopt at 15:10 (in 38m)
    //   Waiting on @zoe (human) — waiting for a human reply
    function stageTrackerPresenceTip(item, nowMs) {
      const p = stageTrackerPresenceOf(item);
      if (!p) return '';
      const now = Number(nowMs) || Date.now();
      const hive = "Waiting on @" + p.login + "'s clanker (";
      const adopt = stageTrackerPresenceAdoptAt(p, now);
      let head, next;
      if (p.state === 'online') {
        const seenMs = Date.parse(p.last_seen || '');
        head = hive + 'online' + (seenMs ? ', last seen ' + agentNowAgo(now - seenMs) : '') + ')';
        next = 'hive is waiting for them' + (adopt ? '; adopts ' + adopt + ' if nothing moves' : '');
      } else if (p.state === 'offline') {
        const gone = stageTrackerPresenceGone(p, now);
        head = hive + 'offline' + (gone ? ' ' + gone : '') + ')';
        next = p.next === 'adopt' && adopt ? 'hive will adopt ' + adopt : 'hive is waiting for them';
      } else if (p.kind === 'human') {
        head = 'Waiting on @' + p.login + ' (human)';
        next = 'waiting for a human reply' + (adopt ? '; adoption ' + adopt : '');
      } else {
        head = hive + 'not registered here)';
        next = p.next === 'adopt' && adopt ? 'hive will adopt ' + adopt : 'hive is waiting for them';
      }
      return head + ' — ' + next;
    }
    // The indicator's own aria-label: the state in words, then the next move.
    function stageTrackerPresenceAria(item, nowMs) {
      const p = stageTrackerPresenceOf(item);
      if (!p) return '';
      const word = p.kind === 'human' ? 'human' : STAGE_TRACKER_PRESENCE_WORDS[p.state];
      return '@' + p.login + ': ' + word + ' — ' + stageTrackerPresenceTip(item, nowMs).split(' — ').slice(1).join(' — ');
    }
    // The indicator: the chevrons' person glyph for a human (no dot — a
    // human has no presence to report), the clanker's robot head with the
    // online / offline / unknown dot beside it for a contributor's hive.
    function stageTrackerPresenceDotHtml(item, nowMs) {
      const p = stageTrackerPresenceOf(item);
      if (!p) return '';
      const ico = stageTrackerIcon(p.kind, 'ham-ico ham-presence-ico', '');
      const dot = p.kind === 'human' ? '' : '<i class="ham-presence-dot"></i>';
      return `<span class="ham-presence ham-presence-${p.state} ham-presence-${p.kind}" role="img" aria-label="${esc(stageTrackerPresenceAria(item, nowMs))}" data-ham-presence="${p.state}" data-ham-presence-kind="${p.kind}">${ico}${dot}</span>`;
    }
    // The holder's live state after the chevron's "· @odttlnt's clanker":
    // " · [■] offline 3h" when the row's presence is for that clanker and
    // it is gone, " · [○] not registered" when no clanker is registered
    // here for the login. An online clanker adds nothing (the row is
    // moving, nothing to flag); a human has no presence. The title is the
    // presence tooltip, so the next automatic step is one hover away.
    function stageTrackerHolderPresenceHtml(item, claimant, nowMs) {
      const p = stageTrackerPresenceOf(item);
      if (!p || p.kind !== 'clanker' || !claimant || claimant.kind !== 'clanker' || !claimant.login) return '';
      if (String(claimant.login).toLowerCase() !== p.login.toLowerCase()) return '';
      const now = Number(nowMs) || Date.now();
      let word = '';
      if (p.state === 'offline') word = 'offline' + (stageTrackerPresenceGone(p, now) ? ' ' + stageTrackerPresenceGone(p, now) : '');
      else if (p.state === 'unknown') word = 'not registered';
      if (!word) return '';
      const tip = stageTrackerPresenceTip(item, now);
      return `<span class="ham-holder-presence ham-presence ham-presence-${p.state} ham-presence-clanker" title="${esc(tip)}" aria-label="${esc(tip)}" data-ham-presence="${p.state}"> · <i class="ham-presence-dot" aria-hidden="true"></i>${esc(word)}</span>`;
    }
    // Pill text with the indicator placed just before the @login (or
    // leading, when the text names nobody): "waiting on [robot ●] @hanthor".
    function stageTrackerPresenceInPillHtml(item, text) {
      const dot = stageTrackerPresenceDotHtml(item, Date.now());
      const t = String(text || '');
      if (!dot) return esc(t);
      const i = t.indexOf('@');
      if (i < 0) return dot + esc(t);
      return esc(t.slice(0, i)) + dot + esc(t.slice(i));
    }
    // The waiting-on pill for a row with presence but no adoption line (071
    // Needs author, 072 before the first nudge, a human's PR): "waiting on
    // [person] @zoe" — the glyph says who, the title says the rest. A
    // person's own PR reads "waiting on @zoe to push": the hive waits for
    // their commit, it is not asking anyone to close it.
    function stageTrackerPresenceChipText(item) {
      const p = stageTrackerPresenceOf(item);
      if (!p) return '';
      const ownPR = item && item.kind === 'pr' && item.author_kind === 'human' && p.kind === 'human' && String(item.author || '').toLowerCase() === String(p.login).toLowerCase();
      const text = 'waiting on @' + p.login + (ownPR ? ' to push' : '');
      // Guarded: the test harnesses load helpers by name.
      return typeof stageTrackerReworkSuffix === 'function' ? stageTrackerReworkSuffix(text, item) : text;
    }
    function stageTrackerPresenceChipHtml(item) {
      const p = stageTrackerPresenceOf(item);
      if (!p || stageTrackerAdoptionTip(item)) return '';
      const ptip = stageTrackerPresenceTip(item, Date.now());
      const tip = typeof stageTrackerReworkJoinTip === 'function' ? stageTrackerReworkJoinTip(ptip, item, Date.now()) : ptip;
      return `<span class="ham-badge ham-badge-waiting" title="${esc(tip)}" aria-label="${esc(tip)}">${stageTrackerPresenceInPillHtml(item, stageTrackerPresenceChipText(item))}</span>`;
    }

    // Rework pickup on a returned PR (`rework`: {state, actor, requested_at,
    // since, evidence, mode, ahead, head} — hivecommons/hive#15280). The
    // waiting-on pill says who, this says whether they have picked it up:
    //   requested    — the request went out; nothing has moved on the PR
    //                  since (evidence "queued": the fix sits in the clanker's
    //                  hub queue, `ahead` fixes before it).
    //   acknowledged — the PR saw activity after the request (a comment or
    //                  a push the sweep has not routed yet).
    //   in_progress  — the clanker's relay took the fix task (claim).
    //   pushed       — the head moved since the request; 050 on the next sweep.
    const STAGE_TRACKER_REWORK_WORDS = { requested: 'no response', acknowledged: 'acknowledged', in_progress: 'working', pushed: 're-review pending' };
    function stageTrackerReworkOf(item) {
      const r = item && item.rework;
      if (!r || !STAGE_TRACKER_REWORK_WORDS[r.state]) return null;
      return r;
    }
    function stageTrackerReworkWord(item) {
      const r = stageTrackerReworkOf(item);
      if (!r) return '';
      if (r.state === 'requested' && r.evidence === 'queued') return 'queued';
      return STAGE_TRACKER_REWORK_WORDS[r.state];
    }
    // "waiting on @x · acknowledged" — the pill text with the pickup word.
    function stageTrackerReworkSuffix(text, item) {
      const w = stageTrackerReworkWord(item);
      return w ? text + ' · ' + w : text;
    }
    function stageTrackerReworkTip(item, nowMs) {
      const r = stageTrackerReworkOf(item);
      if (!r) return '';
      const now = Number(nowMs) || Date.now();
      const who = r.actor ? '@' + r.actor : 'the author';
      const when = (iso) => {
        const ms = Date.parse(iso || '');
        return Number.isFinite(ms) ? agentNowAgo(now - ms) + ' (' + agentNowClockTime(ms, now) + ')' : '';
      };
      const req = when(r.requested_at);
      const since = when(r.since);
      switch (r.state) {
        case 'pushed':
          return who + ' pushed a new head' + (since ? ' ' + since : '') + ' · re-review pending';
        case 'in_progress':
          return who + "'s clanker picked up the rework" + (since ? ' ' + since : '') + (r.mode ? ' (' + r.mode + ')' : '');
        case 'acknowledged':
          return who + ' acknowledged · activity on the PR' + (since ? ' ' + since : '');
        default:
          if (r.evidence === 'queued') return 'rework queued for ' + who + "'s clanker" + (r.ahead ? ' · ' + r.ahead + ' ahead' : '') + (since ? ' · queued ' + since : '');
          return 'rework requested' + (req ? ' ' + req : '') + ' · no response from ' + who + ' yet';
      }
    }
    function stageTrackerReworkJoinTip(tip, item, nowMs) {
      const rt = stageTrackerReworkTip(item, nowMs);
      if (!rt) return tip || '';
      return tip ? tip + ' · ' + rt : rt;
    }
    /* Takeover (hivecommons/hive#15038): a PR waiting on a contributor's
       clanker (072 returned-to-contributor / 071 needs-author) carries
       `takeover` {author, author_online, adopt_now, adopt_now_reason,
       reroute, reroute_reason, clankers[], rerouted, grace_elapsed}. The
       row menu then offers Adopt now (forced adoption, no grace wait) and
       Reroute to another clanker (an online registered contributor hive,
       or "any available"); the server disables each with a reason. */
    const STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE = "closes the contributor's PR after hive opens its copy; their commits + sign-off are preserved";
    const STAGE_TRACKER_REROUTE_ANY = 'any';
    const STAGE_TRACKER_REROUTE_PREFIX = 'decide:reroute:';
    function stageTrackerTakeoverOf(item) {
      return item && item.kind === 'pr' && item.takeover && typeof item.takeover === 'object' ? item.takeover : null;
    }
    // Pure: the base action id — "decide:reroute:<login>" acts as "decide:reroute".
    function stageTrackerActionBase(id) {
      return String(id || '').indexOf(STAGE_TRACKER_REROUTE_PREFIX) === 0 ? 'decide:reroute' : String(id || '');
    }
    function stageTrackerRerouteTargetOf(id) {
      return String(id || '').indexOf(STAGE_TRACKER_REROUTE_PREFIX) === 0 ? String(id).slice(STAGE_TRACKER_REROUTE_PREFIX.length) : '';
    }
    // Pure: one submenu line per online clanker — "@login · hive · online 2h · 1 in hand".
    function stageTrackerClankerLabel(c, nowMs) {
      const since = Date.parse(c.online_since || '');
      const parts = ['@' + (c.login || '?')];
      if (c.hive_name) parts.push(String(c.hive_name));
      if (since && typeof agentNowAgo === 'function') parts.push('online ' + agentNowAgo((typeof nowMs === 'number' ? nowMs : Date.now()) - since));
      const load = Number(c.load) || 0;
      parts.push(load ? load + ' in hand' : 'idle');
      return parts.join(' · ');
    }
    // Pure: the Reroute submenu — the clankers online now, then "any available".
    function stageTrackerRerouteOptions(tk, enabled, why, nowMs) {
      const out = (tk && Array.isArray(tk.clankers) ? tk.clankers : []).map(c => ({ id: STAGE_TRACKER_REROUTE_PREFIX + (c.login || ''), label: stageTrackerClankerLabel(c, nowMs), enabled, why, clanker: c }));
      out.push({ id: STAGE_TRACKER_REROUTE_PREFIX + STAGE_TRACKER_REROUTE_ANY, label: 'any available' + (out.length ? ' (' + out.length + ' online)' : ''), enabled, why });
      return out;
    }
    // Pure: the takeover items of a row menu; [] when the row has none.
    function stageTrackerTakeoverActions(item, ctx, nowMs) {
      const tk = stageTrackerTakeoverOf(item);
      if (!tk) return [];
      ctx = ctx || {};
      const write = ctx.canHold !== false && !!ctx.user;
      const whyWrite = write ? '' : STAGE_TRACKER_WHY_WRITE;
      const adoptOk = write && !!tk.adopt_now && !tk.forced_by;
      const adoptWhy = adoptOk ? '' : (whyWrite || tk.adopt_now_reason || 'Not available on this row');
      const rerouteOk = write && !!tk.reroute;
      const rerouteWhy = rerouteOk ? '' : (whyWrite || tk.reroute_reason || 'Not available on this row');
      const out = [];
      // Adopt now is spent once pressed: the row shows "adopting… (queued)"
      // instead of offering the same action again (hivecommons/hive#15038).
      if (!tk.forced_by) out.push({ id: 'decide:adopt-now', label: STAGE_TRACKER_ACTION_LABELS['decide:adopt-now'] + '…', note: STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE, enabled: adoptOk, why: adoptWhy, danger: true });
      out.push({ id: 'decide:reroute', label: STAGE_TRACKER_ACTION_LABELS['decide:reroute'], note: tk.rerouted && tk.rerouted.to ? 'rerouted → @' + tk.rerouted.to + ' already; pick again to move it' : (rerouteOk ? 'hands the reviewer\'s requested changes to another online clanker' : ''), enabled: rerouteOk, why: rerouteWhy, submenu: stageTrackerRerouteOptions(tk, rerouteOk, rerouteWhy, nowMs) });
      return out;
    }
    // Pure: the "rerouted → @x" pill with the target's presence dot; '' when
    // the row is not rerouted.
    function stageTrackerReroutePillHtml(item) {
      const tk = stageTrackerTakeoverOf(item);
      const r = tk && tk.rerouted && tk.rerouted.to ? tk.rerouted : null;
      if (!r) return '';
      const tip = r.text || ('Rerouted to @' + r.to + (r.by ? ' by @' + r.by : ''));
      const presence = r.target_online ? 'online' : 'offline';
      return `<span class="ham-badge ham-badge-reroute${r.target_online ? ' ham-reroute-online' : ''}" title="${esc(tip)}" aria-label="${esc('Rerouted — ' + tip + ' (@' + r.to + ' ' + presence + ')')}"><span class="ham-presence-dot" aria-hidden="true"></span>rerouted → @${esc(r.to)}</span>`;
    }

    // "Sentinel flagged security-relevant changes at <head short sha>:
    // <reason>" — the sentinel wording for an item's tooltips and aria
    // labels; '' when the item carries no alert. Without a reason or a
    // head the generic sentinel description stands in.
    function stageTrackerSentinelTip(item) {
      if (!item || !item.sentinel_alert) return '';
      const head = String(item.head || '').slice(0, STAGE_TRACKER_SHORT_SHA);
      const reason = String(item.sentinel_reason || '').trim();
      return STAGE_TRACKER_SENTINEL_TIP + (head ? ' at ' + head : '') + ': ' + (reason || STAGE_TRACKER_SENTINEL_TIP_FALLBACK);
    }
    // Whether the item's lit chevron word carries the alert itself (a PR
    // parked at 085, or an issue riding one): the chip then adds nothing.
    function stageTrackerSentinelInLane(item) {
      return !!(item && item.sentinel_alert) && (Number(item.stage) || 0) === STAGE_TRACKER_NEEDS_DECISION && (item.kind === 'pr' || !!item.via_pr);
    }
    // A returned PR (071/072) keeps its routing word — the human already
    // acted, nothing is owed by the maintainer — and carries the alert as
    // a " · sentinel" suffix instead (hivecommons/hive#15159); the chip
    // then adds nothing.
    function stageTrackerSentinelAsSuffix(item) {
      const st = Number(item && item.stage) || 0;
      return !!(item && item.sentinel_alert) && (st === 71 || st === 72) && (item.kind === 'pr' || !!item.via_pr);
    }
    // Pure: whose PR a 071/072 row waits on — the row's own author, or, for
    // an issue row drawn through its one covering PR (via_pr), that PR's
    // author. The issue's author is whoever filed it (on hive's hive the
    // reviewer's bot, hivecommons-hive[bot]); naming them on the chevron
    // read "Returned to @hivecommons-hive[bot]'s hive" for a clanker's PR.
    function stageTrackerReturnedAuthor(item) {
      let src = item || {};
      if (src.via_pr && Array.isArray(src.linked_prs)) {
        const here = src.linked_prs.filter(lp => lp && lp.author && (!Number(lp.drawn_under) || Number(lp.drawn_under) === (Number(src.number) || 0)));
        if (here.length === 1) src = here[0];
      }
      return { author: String(src.author || '').replace(/^@/, ''), kind: String(src.author_kind || '') };
    }
    // "'s clanker" for a clanker's PR (the person's relay agent picks it
    // up), "'s hive" for another hive's, nothing for a person's own.
    function stageTrackerToolingSuffix(kind) {
      if (kind === 'clanker') return "'s clanker";
      if (kind === 'hive') return "'s hive";
      return '';
    }
    // The 071/072 chevron word names the routing: "Needs @author" at 071,
    // "Returned to @author's clanker" at 072 for a clanker-authored PR (the
    // contributor's agent picks it up), "Returned to @author" for a human's.
    // A row already superseded by a merged PR says that instead — nothing
    // is owed by the author; a maintainer closes it (hivecommons/hive#15303
    // sat as "Returned to …" while its issue was closed by #15304).
    function stageTrackerReturnedWord(item, fallback) {
      const st = Number(item && item.stage) || 0;
      const sup = item && item.superseded && typeof item.superseded === 'object' ? Number(item.superseded.closer_pr) || 0 : 0;
      const who = stageTrackerReturnedAuthor(item);
      let text = fallback;
      if (sup) text = 'Superseded by #' + sup + ' · decide';
      else if (who.author && st === 71) text = 'Needs @' + who.author;
      else if (who.author && st === 72) text = 'Returned to @' + who.author + stageTrackerToolingSuffix(who.kind);
      return text + (stageTrackerSentinelAsSuffix(item) ? ' · ' + STAGE_TRACKER_SENTINEL_CHIP : '');
    }

