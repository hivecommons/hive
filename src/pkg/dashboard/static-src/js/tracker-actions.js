    // Pure: plain words for a glossary slug ("supersede" → "close this PR and
    // open a fresh hive-authored PR with the same commits"). Strips a lane
    // prefix ("adoption-stalled:", "contributor-lane:"); unknown slugs are
    // humanised (dashes/underscores → spaces), never shown raw. itemKind
    // 'issue' takes the entry's issue words where it has them: an issue's
    // approve is "approved to proceed — an agent may claim it", never
    // "approved for merge" (#15021).
    function stageTrackerPlainWords(slug, detail, itemKind) {
      let s = String(slug == null ? '' : slug).trim().toLowerCase();
      if (!s) return '';
      const hit = k => STAGE_TRACKER_DECISION_GLOSSARY[k] ? STAGE_TRACKER_DECISION_GLOSSARY[k] : null;
      let e = hit(s);
      if (!e && s.indexOf(':') > 0) { e = hit(s.slice(0, s.indexOf(':'))) || hit(s.slice(s.indexOf(':') + 1).trim()); }
      if (!e && s.indexOf('/') > 0) e = hit(s.slice(s.lastIndexOf('/') + 1));
      if (e) {
        const issue = itemKind === 'issue';
        if (detail) return (issue && e.issue_detail) || e.detail;
        return (issue && e.issue_plain) || e.plain;
      }
      return s.replace(/^[a-z-]+:/, '').replace(/[-_]+/g, ' ').trim();
    }
    // Pure: the DCO caveat the merge confirmations print BEFORE acting
    // when the stage record counted unsigned commits on the head (#15136):
    // merging as-is then fails where DCO is required; re-authoring is the
    // separate choice.
    function stageTrackerDCOCaveat(items) {
      const rows = (Array.isArray(items) ? items : [items]).filter(it => it && Number(it.dco_unsigned) > 0);
      if (!rows.length) return '';
      const it = rows[0];
      const total = Number(it.dco_total) || 0;
      const of = total > 0 ? ' of ' + total + ' commit' + (total === 1 ? '' : 's') : ' commit' + (Number(it.dco_unsigned) === 1 ? '' : 's');
      return ' Before you do: ' + Number(it.dco_unsigned) + of + ' on ' + (rows.length > 1 ? 'the first of these' : 'this head') + ' lack a Signed-off-by (DCO). "Re-author as hive PR" is the separate choice for that; approving does not re-author.';
    }
    const STAGE_TRACKER_ACTION_LABELS = {
      'decide:delegate': 'Let hive decide', 'decide:delegate-all': 'Let hive decide — all PRs', 'decide:delegate:rerun': 'Re-run decision', 'decide:approve': 'Approve', 'decide:direction': 'Give direction',
      'decide:close-superseded': 'Close as superseded by', 'decide:close-wontfix': "Close as won't-fix",
      'decide:close-read': 'Close now',
      'decide:adopt': 'Adopt (hive takes over)',
      'decide:queue-merge': 'Approve and let hive merge', 'decide:merge-now': 'Merge now as maintainer',
      'decide:re-author': 'Re-author as hive PR',
      'decide:adopt-now': 'Adopt now — hive takes it over', 'decide:reroute': 'Reroute to another clanker',
      hold: 'Hold', release: 'Release hold', retry: 'Re-evaluate now', 'accept-triage': 'Accept triage', 'needs-human': 'Mark needs-human',
      open: 'Open on GitHub', copy: 'Copy link',
      'open:app-settings': 'Grant the App permission', 'blocker:recheck': 'Re-check permissions', 'blocker:patch': 'Prepare the patch for a person to push',
      'blocker:close-partial': 'Mark partially-addressed and close', 'blocker:wait': 'Keep waiting',
    };
    // Actions that end with a commit on the base branch — the lead
    // "Approve and let hive merge" (the hive's sweep merges on its next
    // tick once CI is green) and the secondary "Merge now as maintainer":
    // merger or owner only, like POST .../queue-automerge.
    const STAGE_TRACKER_MERGE_ACTIONS = ['decide:queue-merge', 'decide:merge-now'];
    // Generic items every row has; a server-named decision option with one
    // of these ids is not repeated at the top of the menu.
    const STAGE_TRACKER_GENERIC_ACTIONS = ['hold', 'release', 'retry', 'needs-human', 'accept-triage', 'open', 'copy', 'decide:delegate', 'decide:delegate-all', 'decide:adopt'];
    // Accept triage shows on an issue the chevron calls "Triage needed":
    // stage 015, or the needs-triage pill on a pre-claim stage (010 / 017 /
    // 045). A needs-triage left on a claimed or covered issue is reported
    // as stale_labels, not a modifier, so no action is offered for it
    // (hivecommons/hive#14586). Never on a PR.
    const STAGE_TRACKER_TRIAGE_STAGE = 15;
    const STAGE_TRACKER_ACTIONABLE_STAGE = 10;
    const STAGE_TRACKER_TRIAGE_LABEL = 'triage/accepted';
    const STAGE_TRACKER_NEW_MOVER_MAINTAINER = 'maintainer';
    const STAGE_TRACKER_NEW_MOVER_HIVE = 'hive';
    const STAGE_TRACKER_MINUTE_MS = 60000;
    const STAGE_TRACKER_WHY_WRITE = 'Sign in with write access to this repo';
    const STAGE_TRACKER_WHY_ROLE = 'Needs the merger or owner role';
    const STAGE_TRACKER_DELEGATE_POLL_MS = 1500;
    const STAGE_TRACKER_DELEGATE_POLL_MAX = 40;
    // An issue row with this many open nested PRs offers "Let hive decide —
    // all N PRs" (one decision per PR, server-side fan-out) and no longer a
    // single Decide that would silently pick one (hivecommons/hive#15009).
    const STAGE_TRACKER_DELEGATE_ALL_MIN = 2;
    var _stageTrackerSel = new Set();
    var _stageTrackerSelAnchor = '';
    var _stageTrackerMenu = null;

    function stageTrackerRefOf(item) { return (item.repo || '') + '#' + (Number(item.number) || 0); }
    // Pure: the gutter controls of a row. Selection state is never written
    // here (the reconcile keys rows on their HTML); stageTrackerApplySelection
    // sets aria-checked / .ham-selected on the live node.
    function stageTrackerRowToolsHtml(item) {
      const ref = stageTrackerRefOf(item);
      return `<span class="ham-row-tools"><button type="button" class="ham-sel" role="checkbox" aria-checked="false" aria-label="Select ${esc(ref)} (Shift+click for a range)" data-action="hamSelectClick" data-arg-types="e,t" data-stop="1" data-prevent="1"></button><button type="button" class="ham-kebab" aria-label="Actions for ${esc(ref)}" aria-haspopup="menu" data-action="hamRowMenu" data-arg-types="e,t" data-stop="1" data-prevent="1">⋯</button></span>`;
    }
    // Pure: the next selection after a click on `key`. Shift extends from
    // the anchor over the rows' current order (Cmd/Ctrl+Shift keeps what was
    // selected outside the range); a plain or Cmd/Ctrl click toggles the row
    // and moves the anchor.
    function stageTrackerSelectionNext(sel, anchor, keys, key, mods) {
      mods = mods || {};
      const next = new Set(mods.shift && mods.meta ? sel : []);
      if (mods.shift && anchor && keys.indexOf(anchor) >= 0 && keys.indexOf(key) >= 0) {
        const a = keys.indexOf(anchor), b = keys.indexOf(key);
        for (let i = Math.min(a, b); i <= Math.max(a, b); i++) next.add(keys[i]);
        return { sel: next, anchor };
      }
      sel.forEach(k => next.add(k));
      if (next.has(key)) next.delete(key); else next.add(key);
      return { sel: next, anchor: key };
    }
    // Pure: a row key that stands for an item — not the column header, not
    // a group:owner/repo header — and so can be selected.
    function stageTrackerSelectableKey(key) {
      return !!key && key !== STAGE_TRACKER_HEADER_KEY && key.indexOf(STAGE_TRACKER_GROUP_KEY_PREFIX) !== 0;
    }
    // Pure: keep only the keys still rendered as selectable rows.
    function stageTrackerSelectionPrune(sel, keys) {
      const present = new Set(keys.filter(stageTrackerSelectableKey));
      const next = new Set();
      sel.forEach(k => { if (present.has(k)) next.add(k); });
      return next;
    }
    // The nested PR sub-rows of a row node, in document order.
    function stageTrackerSubNodes(n) {
      return n && typeof n.querySelectorAll === 'function' ? Array.from(n.querySelectorAll('[data-ham-sub-key]')) : [];
    }
    function stageTrackerSubKeyOf(n) {
      return n ? (n.dataset && n.dataset.hamSubKey) || (typeof n.getAttribute === 'function' ? n.getAttribute('data-ham-sub-key') : '') || '' : '';
    }
    function stageTrackerApplySelection(el, sel) {
      let any = false;
      const paint = (n, key) => {
        const on = sel.has(key);
        if (on) any = true;
        if (on) n.classList.add('ham-selected'); else n.classList.remove('ham-selected');
        const cb = typeof n.querySelector === 'function' ? n.querySelector('.ham-sel') : null;
        if (cb) cb.setAttribute('aria-checked', on ? 'true' : 'false');
      };
      for (const n of Array.from(el.children || [])) {
        const key = n.dataset && n.dataset.hamKey;
        if (!stageTrackerSelectableKey(key)) continue;
        paint(n, key);
        for (const sub of stageTrackerSubNodes(n)) paint(sub, stageTrackerSubKeyOf(sub));
      }
      if (any) el.classList.add('ham-has-sel'); else el.classList.remove('ham-has-sel');
      return any;
    }
    // Row keys in document order, each row followed by its nested PR
    // sub-row keys, so a Shift+click range runs over sub-rows too.
    function stageTrackerRowKeys(el) {
      const out = [];
      for (const n of Array.from(el.children || [])) {
        const key = n.dataset && n.dataset.hamKey;
        if (!stageTrackerSelectableKey(key)) continue;
        out.push(key);
        for (const sub of stageTrackerSubNodes(n)) { const k = stageTrackerSubKeyOf(sub); if (k && out.indexOf(k) < 0) out.push(k); }
      }
      return out;
    }
    // After a render: drop keys whose rows left, paint the rest, refresh the bar.
    function stageTrackerSelectionSync(el, keys) {
      _stageTrackerSel = stageTrackerSelectionPrune(_stageTrackerSel, keys);
      if (_stageTrackerSelAnchor && keys.indexOf(_stageTrackerSelAnchor) < 0) _stageTrackerSelAnchor = '';
      stageTrackerApplySelection(el, _stageTrackerSel);
      stageTrackerRenderSelBar();
      stageTrackerWireRowActions(el);
    }
    function stageTrackerItemByKey(d, key) {
      if (!d || !Array.isArray(d.items)) return null;
      for (const it of d.items) if (stageTrackerItemKey(it) === key) return it;
      for (const it of d.items) {
        for (const lp of (Array.isArray(it.linked_prs) ? it.linked_prs : [])) {
          const pr = stageTrackerLinkedPRItem(it, lp);
          if (stageTrackerItemKey(pr) === key) return pr;
        }
      }
      return null;
    }
    function stageTrackerItemsForKeys(keys) {
      return keys.map(k => stageTrackerItemByKey(_stageTracker, k)).filter(Boolean);
    }
    // What the signed-in viewer may do on this repo. Hold permission is
    // looked up lazily (the server answers 403 either way); needs-human is
    // merger/owner only.
    function stageTrackerActionCtx(repo) {
      const role = window._hiveRole || 'read';
      const owner = dashboardRoleAtLeast(role, 'owner');
      const user = !!window._hiveUser || owner;
      if (typeof ensureRepoHoldPermission === 'function') ensureRepoHoldPermission(repo);
      const canHold = owner || (typeof repoHoldPermissionKnown === 'function' && repoHoldPermissionKnown(repo)) ? true : (user ? null : false);
      return { user, canHold, canHuman: user && dashboardRoleAtLeast(role, 'merger'), login: String(window._hiveUser || ''), governor: (window._lastStatus && window._lastStatus.governor) || null };
    }
    // Pure: the item a row's "Decide…" acts on. A PR parked at 085 (the
    // "Needs human decision" pill on the Merge queue column) decides itself;
    // an issue row decides a nested PR parked at 085 when it has one (its
    // own stage is only mirrored from that PR), else itself when parked at
    // 045. null when nothing on the row is waiting on a person.
    // Pure: whether a PR waits on a person — parked at 045/085, the
    // needs-decision pill, superseded, or "Ready for human merge" (a
    // human-merge reason such as an untrusted author).
    function stageTrackerPRParked(it) {
      return !!it && (!!it.superseded || (Array.isArray(it.modifiers) ? it.modifiers : []).indexOf('needs-decision') >= 0 || STAGE_TRACKER_DECISION_STAGES.indexOf(Number(it.stage) || 0) >= 0 || !!it.manual_merge || !!it.human_merge_reason);
    }
    // Pure: the parked nested PRs of an issue row, as row items. An adopted
    // original whose hive copy is listed under the issue is not parked —
    // its "changes requested" belongs to the pair the copy now carries.
    function stageTrackerParkedNestedPRs(item) {
      if (!item || item.kind === 'pr') return [];
      return (Array.isArray(item.linked_prs) ? item.linked_prs : []).filter(lp => lp && stageTrackerPRParked(lp) && (!!lp.superseded || stageTrackerLinkedPROpen(lp, item))).map(lp => stageTrackerLinkedPRItem(item, lp));
    }
    function stageTrackerDecisionTarget(item) {
      if (!item) return null;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if (item.kind === 'pr') return stageTrackerPRParked(item) ? item : null;
      // Exactly one parked nested PR: the issue row decides it. Two or more
      // is ambiguous — each sub-row has its own menu, and the issue row
      // offers "Let hive decide — all N PRs" (hivecommons/hive#15009).
      const parkedPRs = stageTrackerParkedNestedPRs(item);
      if (parkedPRs.length === 1) return parkedPRs[0];
      if (parkedPRs.length > 1) return null;
      if (item.mirrored) return null;
      return (mods.indexOf('needs-decision') >= 0 || (Number(item.stage) || 0) === 45) ? item : null;
    }
    // Pure: what bulk "Let hive decide" acts on for one row — its decision
    // target when it has one (so "Decide PR #N" on an issue row stays on
    // that PR), else every open PR of a multi-PR parent (the delegate-all
    // fan-out); [] when there is nothing to decide.
    function stageTrackerDelegateTargets(item) {
      const target = stageTrackerDecisionTarget(item);
      if (target) return [target];
      const nested = stageTrackerNestedOpenPRs(item);
      return nested.length >= STAGE_TRACKER_DELEGATE_ALL_MIN ? nested : [];
    }
    // Pure: who carries a direction on these PRs — the hive's own author
    // agent, or (a clanker-authored PR, hivecommons/hive#15159) the owning
    // contributor's clanker, which is nudged with the text.
    function stageTrackerDirectionCarrier(items) {
      const list = (items || []).filter(it => it && it.kind === 'pr');
      if (!list.length || !list.every(it => it.author_kind === 'clanker')) return 'the author agent';
      const authors = Array.from(new Set(list.map(it => String(it.author || '').replace(/^@/, '')).filter(Boolean)));
      if (authors.length === 1) return "@" + authors[0] + "'s clanker";
      return "the contributor's clanker";
    }
    // Pure: the decision choices for one decision target. On an issue
    // parked at 045 the verbs are /hive approve and /hive decision. On a PR
    // parked at 085, Approve posts the /lgtm + /approve commands as the
    // operator (the merge queue decides from there — nothing merges here),
    // Close as superseded names the PR that won, and Give direction is the
    // /hive decision fix lane.
    function stageTrackerDecideOptions(item, ctx) {
      const enabled = ctx.canHold !== false && !!ctx.user;
      const why = enabled ? '' : STAGE_TRACKER_WHY_WRITE;
      const out = [];
      // First: the hive decides as the owner's delegate (policy + rails on
      // the server; recorded as a human decision with an audit marker).
      out.push({ id: 'decide:delegate', label: 'Let hive decide', hint: 'delegate', enabled, why });
      if (item.kind === 'pr') {
        out.push({ id: 'decide:approve', label: 'Approve (/lgtm + /approve)', enabled, why });
        out.push({ id: 'decide:close-superseded', label: 'Close as superseded by…', enabled, why, danger: true });
        out.push({ id: 'decide:close-wontfix', label: "Won't fix…", enabled, why, danger: true });
        out.push({ id: 'decide:direction', label: 'Give direction…', enabled, why });
        return out;
      }
      out.push({ id: 'decide:approve', label: 'Approve direction', enabled, why });
      out.push({ id: 'decide:direction', label: 'Send back to the hive…', enabled, why });
      out.push({ id: 'decide:close-superseded', label: 'Close as superseded by…', enabled, why, danger: true });
      out.push({ id: 'decide:close-wontfix', label: "Close as won't-fix…", enabled, why, danger: true });
      return out;
    }
    // Pure: the operator note for labels the row's stage has outrun
    // (stale_labels: a needs-triage still on a claimed issue, which the
    // reconciler removes on its next pass — hivecommons/hive#14586). '' when none.
    function stageTrackerStaleLabelsText(item) {
      const stale = item && Array.isArray(item.stale_labels) ? item.stale_labels.filter(Boolean) : [];
      if (!stale.length) return '';
      return 'Stale label' + (stale.length > 1 ? 's' : '') + ': ' + stale.join(', ') + ' (stage has moved on; the hive removes it on its next pass)';
    }
    // Pure: whether a row is an issue waiting on a maintainer's triage.
    function stageTrackerTriageNeeded(item) {
      if (!item || item.kind === 'pr') return false;
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      if ((Number(item.stage) || 0) === STAGE_TRACKER_TRIAGE_STAGE || mods.indexOf('needs-triage') >= 0) return true;
      // A label-less 000-new issue the reporter-trust gate holds for a
      // maintainer (next_mover: maintainer) before the first stage pass
      // has even placed it at 015 (hivecommons/hive#15215).
      const fresh = stageTrackerTriageNewState(item);
      return !!fresh && fresh.next_mover === STAGE_TRACKER_NEW_MOVER_MAINTAINER;
    }
    // Pure: the 000-new narration of a row, or null (stageTrackerNewState
    // without the dependency, for the row-actions helpers).
    function stageTrackerTriageNewState(item) {
      return item && (Number(item.stage) || 0) === 0 && item.new && typeof item.new === 'object' ? item.new : null;
    }
    // Pure: the triage label Accept triage applies — the one the row's
    // 000-new narration names in backticks, else the default.
    function stageTrackerTriageLabel(item) {
      const fresh = stageTrackerTriageNewState(item);
      const m = fresh && typeof fresh.next_mover_text === 'string' ? fresh.next_mover_text.match(/`([^`]+)`/) : null;
      return m ? m[1] : STAGE_TRACKER_TRIAGE_LABEL;
    }
    // Pure: milliseconds until the hive's next pass for a row — the 000-new
    // narration's next_pass_at, else the governor's published next kick —
    // or null when neither is known.
    function stageTrackerNextPassLeftMs(item, nowMs, governor) {
      const fresh = stageTrackerTriageNewState(item);
      let at = Date.parse((fresh && fresh.next_pass_at) || '');
      if (!at && governor) at = Date.parse(governor.nextKickAt || '');
      return at ? at - nowMs : null;
    }
    // Pure: "≈13 min" / "due now" from a next-pass delta, '' when unknown.
    function stageTrackerNextPassWord(leftMs) {
      if (leftMs == null || !Number.isFinite(leftMs)) return '';
      if (leftMs <= 0) return 'due now';
      const min = Math.max(1, Math.round(leftMs / STAGE_TRACKER_MINUTE_MS));
      return '≈' + min + ' min';
    }
    // Pure: the Accept triage hint — what the click writes, as whom, and
    // when the hive takes it: "adds `triage/accepted` as @you; the hive
    // takes it on its next pass (≈13 min)".
    function stageTrackerTriageHint(item, ctx, nowMs, governor) {
      ctx = ctx || {};
      const who = ctx.login ? '@' + ctx.login : 'you';
      const when = stageTrackerNextPassWord(stageTrackerNextPassLeftMs(item, nowMs || Date.now(), governor));
      return 'adds `' + stageTrackerTriageLabel(item) + '` as ' + who + '; the hive takes it on its next pass' + (when ? ' (' + when + ')' : '');
    }
    // Pure: the item as it reads once triage is accepted — 010 Actionable,
    // the needs-triage pill and the 👤 next-move gone — for the optimistic
    // re-render before the tracker's next payload confirms it.
    function stageTrackerTriageAccepted(item) {
      const next = Object.assign({}, item);
      next.modifiers = (Array.isArray(item.modifiers) ? item.modifiers : []).filter(m => m !== 'needs-triage');
      if ((Number(item.stage) || 0) === STAGE_TRACKER_TRIAGE_STAGE) {
        next.stage = STAGE_TRACKER_ACTIONABLE_STAGE;
        next.stage_slug = 'actionable';
        next.stage_label = 'stage/010-actionable';
        next.caption = ''; next.actor = ''; next.reason = '';
        return next;
      }
      // A 000-new row has no stage label to move yet: the triage gate reads
      // done and the hive's stage-label pass is the next mover, exactly
      // what the server narrates once the label is on.
      const fresh = stageTrackerTriageNewState(item);
      if (fresh && fresh.next_mover === STAGE_TRACKER_NEW_MOVER_MAINTAINER) {
        const text = 'the stage-label pass (→ 010 actionable), then the scanner';
        const gates = (Array.isArray(fresh.gates) ? fresh.gates : []).map(g => g && g.name === 'triage' ? Object.assign({}, g, { state: 'done', text: 'triage accepted (' + stageTrackerTriageLabel(item) + ')' }) : (g && g.name === 'actionable' ? Object.assign({}, g, { state: 'next' }) : g));
        next.new = Object.assign({}, fresh, { next_mover: STAGE_TRACKER_NEW_MOVER_HIVE, next_mover_text: text, gates, text: 'New · waiting for ' + text });
        next.caption = next.new.text; next.actor = STAGE_TRACKER_NEW_MOVER_HIVE;
      }
      return next;
    }
    // Pure: whether this viewer may run a decision option: write access,
    // and the merger/owner role for anything that merges.
    function stageTrackerActionAllowed(id, ctx) {
      ctx = ctx || {};
      if (STAGE_TRACKER_MERGE_ACTIONS.indexOf(id) >= 0) return ctx.canHuman ? { enabled: true, why: '' } : { enabled: false, why: STAGE_TRACKER_WHY_ROLE };
      const can = ctx.canHold !== false && !!ctx.user;
      return { enabled: can, why: can ? '' : STAGE_TRACKER_WHY_WRITE };
    }
    // Pure: the row's lead actions — the server's decision_needed.options
    // (the item's own, else its one asking nested PR's) that neither the
    // generic items every row has nor the Decide… submenu (`taken`) already
    // offer, in the server's order, each carrying its consequence as the
    // hint. Enabled only when the server and this viewer's permissions
    // agree; otherwise disabled with the reason.
    function stageTrackerLeadActions(item, ctx, taken) {
      const dec = typeof stageTrackerDecisionOf === 'function' ? stageTrackerDecisionOf(item) : null;
      const d = dec && dec.item && dec.item.decision_needed;
      const server = d && Array.isArray(d.options) ? d.options : [];
      const out = [];
      for (const o of server) {
        // A server-marked lead (`lead`) heads the menu even when the
        // Decide… submenu lists the same verb generically: the row's
        // answer is this one, with its consequence.
        if (!o || !o.action || STAGE_TRACKER_GENERIC_ACTIONS.indexOf(o.action) >= 0 || o.action.indexOf('decide:') !== 0 || (taken && taken[o.action] && !o.lead)) continue;
        const allowed = stageTrackerActionAllowed(o.action, ctx);
        const enabled = !!o.enabled && allowed.enabled;
        const why = enabled ? '' : (o.enabled ? allowed.why : (o.reason_if_disabled || 'Not available on this row'));
        // A destructive option never leads the row on its own: only when
        // the server marks it (`lead`, the reviewer named the PR that
        // supersedes this one) does a close become the inline button. It
        // still sits in the ⋯ menu either way.
        const danger = /^decide:close-/.test(o.action);
        out.push({ id: o.action, label: o.label || STAGE_TRACKER_ACTION_LABELS[o.action] || o.action, hint: o.consequence || '', enabled, why, danger, lead: !danger || !!o.lead, target: dec.item });
      }
      return out;
    }
    // Pure: the actions that apply to one row in its current state.
    // The Close-now hint and confirm text of an advisory row. A checklist
    // the hive is working (advisory.total > 0) says so: the sweep ticks the
    // items and closes it itself, so closing now is optional.
    function stageTrackerAdvisoryCloseHint(advisory) {
      if (!advisory) return '';
      if (advisory.empty) return 'nothing found · closes as completed';
      if (advisory.total > 0 && advisory.resolved < advisory.total) return 'optional · hive is working ' + advisory.resolved + '/' + advisory.total + ' items; close anyway';
      if (advisory.total > 0) return 'all items resolved · closes as completed';
      return 'advisory acknowledged · closes as completed';
    }
    function stageTrackerAdvisoryCloseMessage(items, refs, who) {
      const adv = items && items.length === 1 ? stageTrackerAdvisoryOf(items[0]) : null;
      if (adv && adv.total > 0 && adv.resolved < adv.total) {
        return 'Close ' + refs + ' now, as ' + who + '? The hive is working its items (' + adv.resolved + '/' + adv.total + ' resolved) and closes it itself when all are; closing now is optional. The issue closes as completed with a note that you read it.';
      }
      return 'Close ' + refs + ' as read, as ' + who + '? It is an advisory for maintainers — nothing waits on it; the issue closes as completed with a note that you read it.';
    }
    function stageTrackerRowActions(item, ctx) {
      ctx = ctx || {};
      const mods = Array.isArray(item.modifiers) ? item.modifiers : [];
      const has = m => mods.indexOf(m) >= 0;
      const write = ctx.canHold !== false;
      const out = [];
      const nested = stageTrackerNestedOpenPRs(item);
      if (nested.length >= STAGE_TRACKER_DELEGATE_ALL_MIN) {
        const can = write && !!ctx.user;
        out.push({ id: 'decide:delegate-all', label: 'Let hive decide — all ' + nested.length + ' PRs', hint: 'per PR', enabled: can, why: can ? '' : STAGE_TRACKER_WHY_WRITE, prs: nested.map(p => p.number) });
      }
      // The row asks the human for something (decision_needed on the item
      // or its one asking nested PR): the server's answers lead the menu,
      // each with its consequence, so the pill's ask always has its action
      // right here (hivecommons/hive#15078: "Ready for human merge" with no
      // way to merge).
      const target = stageTrackerDecisionTarget(item);
      const submenu = target ? stageTrackerDecideOptions(target, ctx) : [];
      const taken = {};
      for (const o of submenu) taken[o.id] = true;
      const lead = stageTrackerLeadActions(item, ctx, taken);
      // Accept triage heads the menu of an issue waiting on a maintainer's
      // triage (015, the needs-triage pill, or a 000-new reporter-trust
      // hold): the ask the row makes has its answer first.
      const triage = stageTrackerTriageNeeded(item);
      if (triage) out.push({ id: 'accept-triage', label: 'Accept triage', hint: stageTrackerTriageHint(item, ctx, ctx.nowMs, ctx.governor), enabled: write, why: write ? '' : STAGE_TRACKER_WHY_WRITE, lead: true });
      // An advisory issue (hivecommons/hive#15381) asks for one thing:
      // read it, then close it. "Close now" leads; it is the existing
      // decision close path with the verdict `close-read`. A checklist the
      // hive is still working says so: closing it is optional
      // (hivecommons/hive#15754).
      const advisory = typeof stageTrackerAdvisoryOf === 'function' ? stageTrackerAdvisoryOf(item) : null;
      if (advisory) out.push(Object.assign({ id: 'decide:close-read', label: STAGE_TRACKER_ACTION_LABELS['decide:close-read'], hint: stageTrackerAdvisoryCloseHint(advisory), lead: true }, stageTrackerActionAllowed('decide:close-read', ctx)));
      for (const a of lead) out.push(a);
      if (target) {
        const on = target === item ? '' : ' PR #' + (Number(target.number) || 0);
        out.push({ id: 'decide', label: 'Decide' + on + '…', enabled: write, why: write ? '' : STAGE_TRACKER_WHY_WRITE, submenu, target, sep: lead.length > 0 || triage });
      }
      // A nudged contributor PR the engine would adopt but for the owner's
      // say-so (maintainer_policy: ask) offers Adopt (hivecommons/hive#14524).
      if (typeof stageTrackerBlockerActions === 'function') for (const a of stageTrackerBlockerActions(item, ctx)) out.push(a);
      if (typeof stageTrackerAdoptOffered === 'function' && stageTrackerAdoptOffered(item)) out.push({ id: 'decide:adopt', label: STAGE_TRACKER_ACTION_LABELS['decide:adopt'] + '…', hint: 'adoption', enabled: write && !!ctx.user, why: write && ctx.user ? '' : STAGE_TRACKER_WHY_WRITE });
      // A PR waiting on a contributor's clanker offers Adopt now and Reroute
      // (hivecommons/hive#15038); each disabled with the server's reason.
      if (typeof stageTrackerTakeoverActions === 'function') for (const a of stageTrackerTakeoverActions(item, ctx, ctx.nowMs)) out.push(a);
      out.push(has('hold')
        ? { id: 'release', label: 'Release hold', enabled: write, why: write ? '' : STAGE_TRACKER_WHY_WRITE, sep: triage && !target }
        : { id: 'hold', label: 'Hold', enabled: write, why: write ? '' : STAGE_TRACKER_WHY_WRITE, sep: triage && !target });
      out.push({ id: 'retry', label: 'Re-evaluate now', hint: 'rescan', enabled: true, why: '' });
      if (!has('needs-human')) out.push({ id: 'needs-human', label: 'Mark needs-human…', enabled: !!ctx.canHuman, why: ctx.canHuman ? '' : STAGE_TRACKER_WHY_ROLE });
      out.push({ id: 'open', label: 'Open on GitHub', enabled: !!item.url, why: item.url ? '' : 'No GitHub link', sep: true });
      out.push({ id: 'copy', label: 'Copy link', enabled: !!item.url, why: item.url ? '' : 'No GitHub link' });
      return out;
    }
    // Pure: the one action a row leads with, rendered inline on the row
    // header so the ask the caption makes has its answer in sight
    // (hivecommons/hive#15456: "waiting for a maintainer's triage" with
    // Accept triage hidden behind ⋯). The ⋯ menu's `lead` entry first
    // (Accept triage, the server's lead answer such as Merge now, Close
    // (read)); else the first Decide… option (Let hive decide, carrying the
    // decide target); else Adopt, Adopt now or Let hive decide — all N
    // PRs. null when the row asks nothing of the viewer here. Disabled
    // entries are returned as-is so the button can say why.
    function stageTrackerLeadAction(item, ctx) {
      if (!item) return null;
      const actions = stageTrackerRowActions(item, ctx);
      const lead = actions.find(a => a.lead);
      if (lead) return lead;
      const decide = actions.find(a => a.id === 'decide');
      const safe = decide && Array.isArray(decide.submenu) ? decide.submenu.filter(x => x && !x.danger) : [];
      if (safe.length) {
        const o = safe.find(x => x.enabled) || safe[0];
        const enabled = !!decide.enabled && !!o.enabled;
        return Object.assign({}, o, { target: decide.target, enabled, why: enabled ? '' : (decide.enabled ? o.why : decide.why) });
      }
      for (const id of ['decide:adopt', 'decide:adopt-now', 'decide:delegate-all']) {
        const a = actions.find(x => x.id === id);
        if (a && (a.enabled || id !== 'decide:adopt-now')) return a;
      }
      if (typeof stageTrackerDevWarn === 'function' && stageTrackerMaintainerMoved(item)) stageTrackerDevWarn('row ' + stageTrackerRefOf(item) + ' waits on the maintainer but offers no lead action');
      return null;
    }
    // Pure: whether the server says the next move is the maintainer's.
    function stageTrackerMaintainerMoved(item) {
      return !!item && (item.actor === 'maintainer' || !!(item.new && item.new.next_mover === 'maintainer'));
    }
    // A console warning on a development host only (localhost, 127.0.0.1).
    function stageTrackerDevWarn(msg) {
      const host = typeof window !== 'undefined' && window.location ? String(window.location.hostname || '') : '';
      if ((host === 'localhost' || host === '127.0.0.1') && typeof console !== 'undefined' && console.warn) console.warn('[tracker] ' + msg);
    }
    // Pure: the lead the row itself renders. An issue row whose lead acts
    // on a nested PR (stageTrackerDecisionTarget resolved to the one parked
    // PR) renders none when that PR has its own sub-row carrying the same
    // button — "Let hive decide" showed twice per item (hive#15320 →
    // #15647, hive#15338 → #15648). The ⋯ menu's "Decide PR #N…" stays on
    // the parent. subRows: the linked PRs drawn as sub-rows, or none.
    // Pure: is the covering PR also a row of its own among rows, the rows
    // the render draws (a top-level row, not a sub-row)? Then only that row
    // carries the button. rows is ctx.rows at the call site — the drawn
    // page (tracker-filter-render.js _stageTrackerRows), not the payload's
    // items: a PR nested_under a listed issue is in items but is never its
    // own row outside the Pull requests view (tracker-filter.js
    // stageTrackerVisible), and a filter or the page may hide the PR while
    // it shows the issue. With no rows given nothing is drawn, so nothing
    // is "listed" and the chip is not shown.
    function stageTrackerPRListedOwnRow(issue, t, rows) {
      if (!Array.isArray(rows)) return false;
      const key = stageTrackerItemKey({ repo: t.repo || issue.repo, kind: 'pr', number: t.number });
      return rows.some(it => it && stageTrackerItemKey(it) === key);
    }
    // Pure: the number of the other listed issue whose row draws the
    // covering PR, or 0. The server nests a PR that closes several issues
    // under the lowest one (stage_tracker_links.go NestedUnder → each
    // issue's linked_prs[].drawn_under); the PR is then never a row of its
    // own and the secondary issue draws no sub-row either (hivecommons/hive
    // #15503 under #15502), so the decision button belongs to the row that
    // draws the PR and this issue chips to it.
    function stageTrackerPRDrawnUnderOther(issue, t, rows) {
      if (!Array.isArray(rows) || !issue || !Array.isArray(issue.linked_prs)) return 0;
      const lp = issue.linked_prs.find(x => x && Number(x.number) === Number(t.number) && (!x.repo || !t.repo || x.repo === t.repo));
      const under = Number(lp && lp.drawn_under) || 0;
      if (!under || under === (Number(issue.number) || 0)) return 0;
      const key = stageTrackerItemKey({ repo: t.repo || issue.repo, kind: 'issue', number: under });
      return rows.some(it => it && stageTrackerItemKey(it) === key) ? under : 0;
    }
    function stageTrackerLeadActionOnRow(item, ctx, subRows) {
      const a = stageTrackerLeadAction(item, ctx);
      if (a && a.target && item.kind === 'issue' && a.target.kind === 'pr') {
        const rows = ctx && ctx.rows;
        if (stageTrackerPRListedOwnRow(item, a.target, rows)) return Object.assign({}, a, { passive: a.target.number });
        const under = stageTrackerPRDrawnUnderOther(item, a.target, rows);
        if (under) return Object.assign({}, a, { passive: a.target.number, passiveUnder: under });
      }
      if (!a || !a.target || !Array.isArray(subRows) || !subRows.length || item.kind !== 'issue') return a;
      const t = a.target;
      if (t.kind !== 'pr') return a;
      const lp = subRows.find(x => x && Number(x.number) === Number(t.number) && !x.superseded && (!x.repo || !t.repo || x.repo === t.repo));
      if (!lp) return a;
      const sub = typeof stageTrackerLinkedPRItem === 'function' ? stageTrackerLinkedPRItem(item, lp) : null;
      return sub && stageTrackerLeadAction(sub, ctx) ? null : a;
    }
    // The inline lead button for a row: a real <button> carrying the same
    // id, hint and enable/disable reason as the ⋯ menu entry it mirrors;
    // '' when the row has no lead. Click handling is delegated in
    // stageTrackerWireRowActions and runs runRowAction, the menu's path.
    function stageTrackerLeadActionHtml(item, ctx, subRows) {
      const a = stageTrackerLeadActionOnRow(item, ctx, subRows);
      if (!a) return '';
      // The covering PR has its own row (or is drawn under another listed
      // issue): the decision — whatever its lead (Let hive decide, Merge
      // now, Adopt…) — is taken there, so this row shows a passive chip
      // that names that lead rather than a second button.
      // Clicking it scrolls to the row that carries the button.
      if (a.passive) {
        const where = a.passiveUnder ? 'the row of #' + Number(a.passiveUnder) + ', which draws the PR' : 'its row';
        const repo = (a.target && a.target.repo) || item.repo;
        const to = stageTrackerItemKey({ repo, kind: 'pr', number: a.passive });
        const via = a.passiveUnder ? ` data-ham-scroll-row="${esc(stageTrackerItemKey({ repo, kind: 'issue', number: a.passiveUnder }))}"` : '';
        return `<button type="button" class="ham-badge ham-decision-pending" data-ham-scroll-to="${esc(to)}"${via} title="${esc('The decision is on PR #' + a.passive + ' — ' + String(a.label || a.id) + ' lives on ' + where + '. Click to go there.')}">decision pending on #${Number(a.passive) || 0}</button>`;
      }
      const label = String(a.label || a.id);
      const tip = a.enabled ? String(a.hint || a.note || a.consequence || '') : String(a.why || '');
      const ref = typeof stageTrackerRefOf === 'function' ? stageTrackerRefOf(item) : '';
      const state = a.enabled ? (tip ? ` title="${esc(tip)}"` : '') : ` aria-disabled="true" title="${esc(tip)}"`;
      return `<button type="button" class="ham-lead-act${a.danger ? ' ham-menu-danger' : ''}" data-ham-act="${esc(a.id)}"${state} aria-label="${esc(label + (ref ? ' — ' + ref : ''))}">${esc(label)}</button>`;
    }
    // Pure: the actions for a row parked on a blocker (hivecommons/hive#15099):
    // the server lists them in decision_needed.options (a capability gap
    // offers the App settings link, a permission re-check and the disabled
    // patch hand-off; every blocker offers close-as-partial and wait). The
    // link opens for anyone; the rest need write access.
    const STAGE_TRACKER_BLOCKER_ACTIONS = ['open:app-settings', 'blocker:recheck', 'blocker:patch', 'blocker:close-partial', 'blocker:wait'];
    function stageTrackerBlockerActions(item, ctx) {
      ctx = ctx || {};
      if (!item || !item.blocker || !item.decision_needed) return [];
      const write = ctx.canHold !== false && !!ctx.user;
      const server = Array.isArray(item.decision_needed.options) ? item.decision_needed.options : [];
      const out = [];
      for (const o of server) {
        if (STAGE_TRACKER_BLOCKER_ACTIONS.indexOf(o.action) < 0) continue;
        const link = o.action === 'open:app-settings';
        const enabled = !!o.enabled && (link || o.action === 'blocker:wait' || write);
        const why = enabled ? '' : (o.enabled ? STAGE_TRACKER_WHY_WRITE : (o.reason_if_disabled || ''));
        out.push({ id: o.action, label: o.label || STAGE_TRACKER_ACTION_LABELS[o.action] || o.action, hint: 'blocked', enabled, why, url: o.url || '', danger: o.action === 'blocker:close-partial' });
      }
      return out;
    }
    function stageTrackerFlatActions(actions) {
      const flat = {};
      for (const a of actions) {
        flat[a.id] = a;
        for (const s of (a.submenu || [])) flat[s.id] = s;
      }
      return flat;
    }
    // Pure: the actions for a selection — each enabled only when it applies
    // to, and is allowed on, every item; otherwise disabled with the first
    // reason. Open is per-row only; Copy copies every link.
    function stageTrackerSelectionActions(items, ctxOf) {
      const per = items.map(it => ({ it, flat: stageTrackerFlatActions(stageTrackerRowActions(it, typeof ctxOf === 'function' ? ctxOf(it.repo) : ctxOf)) }));
      const judge = id => {
        for (const p of per) {
          // A multi-PR parent delegates all its PRs, not one ambiguous target.
          const a = p.flat[id] || (id === 'decide:delegate' ? p.flat['decide:delegate-all'] : null);
          if (!a) return { enabled: false, why: 'Not applicable to ' + stageTrackerRefOf(p.it) };
          if (!a.enabled) return { enabled: false, why: a.why + ' (' + stageTrackerRefOf(p.it) + ')' };
        }
        return { enabled: items.length > 0, why: items.length ? '' : 'Nothing selected' };
      };
      const decideIds = ['decide:delegate', 'decide:approve', 'decide:direction', 'decide:close-superseded', 'decide:close-wontfix'];
      const submenu = decideIds.map(id => Object.assign({ id, label: STAGE_TRACKER_ACTION_LABELS[id] + (id === 'decide:approve' || id === 'decide:delegate' ? '' : '…'), danger: id.indexOf('close') > 0 }, judge(id)));
      // The kind mix counts a multi-PR parent's open PRs (what bulk
      // delegation acts on), so a parent plus a parked issue is refused.
      const targets = items.map(stageTrackerDelegateTargets);
      if (items.length && targets.every(ts => ts.length)) {
        const kinds = {};
        for (const t of [].concat(...targets)) kinds[t.kind] = true;
        if (kinds.pr && kinds.issue) {
          for (const sm of submenu) if (sm.enabled) Object.assign(sm, { enabled: false, why: 'Selection mixes issues and pull requests waiting on a decision' });
        }
      }
      const out = [Object.assign({ id: 'decide', label: 'Decide…', submenu }, submenu.some(s => s.enabled) ? { enabled: true, why: '' } : { enabled: false, why: submenu[0].why })];
      // One click for the whole selection: each item is decided on its own
      // (a nested PR sub-row by its own number), summed up in one toast.
      out.push(Object.assign({}, submenu[0], { label: 'Let hive decide (' + items.length + ' selected)', hint: 'per item' }));
      // Accept triage for every selected issue waiting on a maintainer's
      // triage, one click (hivecommons/hive#15475: four "Triage needed"
      // rows selected, no bulk Accept triage). Rows that are not — PRs, issues past 015 —
      // are skipped, and the count says so ("3 of 4"). It leads the bar
      // and the menu when the whole selection is triage, as on one row.
      const triage = stageTrackerSelectionTriage(items, per);
      if (triage) { if (triage.lead) out.unshift(triage); else out.push(triage); }
      for (const id of STAGE_TRACKER_SEL_ACTIONS) out.push(Object.assign({ id, label: STAGE_TRACKER_ACTION_LABELS[id] + (id === 'needs-human' ? '…' : id === 'copy' ? 's' : '') }, judge(id)));
      return out;
    }
    // Pure: the selection's Accept triage entry, or null when no selected
    // row is waiting on triage. `per` is the per-item flat action map
    // stageTrackerSelectionActions built; enabled when every eligible row
    // allows it (the first reason otherwise), `lead` when every selected
    // row is eligible.
    function stageTrackerSelectionTriage(items, per) {
      const eligible = per.filter(p => p.flat['accept-triage']);
      if (!eligible.length) return null;
      const blocked = eligible.find(p => !p.flat['accept-triage'].enabled);
      const all = eligible.length === items.length;
      return {
        id: 'accept-triage',
        label: 'Accept triage (' + (all ? items.length + ' selected' : eligible.length + ' of ' + items.length) + ')',
        hint: all ? 'per issue' : 'skips the ' + (items.length - eligible.length) + ' not waiting on triage',
        enabled: !blocked,
        why: blocked ? blocked.flat['accept-triage'].why + ' (' + stageTrackerRefOf(blocked.it) + ')' : '',
        lead: all,
        count: eligible.length,
      };
    }
    function stageTrackerMenuItemHtml(a) {
      const dis = a.enabled ? '' : ` aria-disabled="true" title="${esc(a.why || '')}"`;
      if (a.submenu) {
        return `${a.sep ? '<div class="ham-menu-sep" role="separator"></div>' : ''}<button type="button" role="menuitem" class="ham-menu-item" data-ham-act="${esc(a.id)}" aria-haspopup="menu" aria-expanded="false" tabindex="-1"${dis}>${stageTrackerMenuTextHtml(a)}<span class="ham-menu-more" aria-hidden="true">▸</span></button><div class="ham-submenu" role="menu" aria-label="${esc(a.label)}">${a.submenu.map(stageTrackerMenuItemHtml).join('')}</div>`;
      }
      // A hint stacks under the label (ham-menu-item-hinted) and wraps, so
      // the consequence line never overflows the menu (#15165).
      return `${a.sep ? '<div class="ham-menu-sep" role="separator"></div>' : ''}<button type="button" role="menuitem" class="ham-menu-item${a.danger ? ' ham-menu-danger' : ''}${a.hint ? ' ham-menu-item-hinted' : ''}" data-ham-act="${esc(a.id)}" tabindex="-1"${dis}>${stageTrackerMenuTextHtml(a)}${a.hint ? `<span class="ham-menu-hint">${esc(a.hint)}</span>` : ''}</button>`;
    }
    // Pure: true when any action (or submenu action) carries a hint.
    function stageTrackerMenuHasHints(actions) {
      return (actions || []).some(a => !!a.hint || (a.submenu && stageTrackerMenuHasHints(a.submenu)));
    }
    // Pure: the label, with its consequence line under it when the action has one.
    function stageTrackerMenuTextHtml(a) {
      if (!a.note) return esc(a.label);
      return `<span class="ham-menu-text"><span>${esc(a.label)}</span><span class="ham-menu-note">${esc(a.note)}</span></span>`;
    }
    // Pure: the menu markup (roles menu / menuitem, the Decide group as a
    // nested menu toggled by its item). A menu with hints is ham-menu-hinted
    // (wider, hints stacked and wrapping).
    function stageTrackerMenuHtml(actions, head) {
      return `<div class="ham-menu${stageTrackerMenuHasHints(actions) ? ' ham-menu-hinted' : ''}" role="menu" aria-label="Row actions" tabindex="-1">${head ? `<div class="ham-menu-head">${esc(head)}</div>` : ''}${actions.map(stageTrackerMenuItemHtml).join('')}</div>`;
    }
    // Pure: place the menu at the pointer, flipped/clamped to stay on screen.
    function stageTrackerMenuPosition(x, y, w, h, vw, vh) {
      const m = STAGE_TRACKER_MENU_MARGIN;
      let left = x, top = y;
      if (left + w + m > vw) left = Math.max(m, Math.min(x - w, vw - w - m));
      if (top + h + m > vh) top = Math.max(m, Math.min(y - h, vh - h - m));
      return { left: Math.max(m, left), top: Math.max(m, top) };
    }
    function stageTrackerMenuItems(menu) {
      return Array.from(menu.querySelectorAll('[role="menuitem"]')).filter(b => {
        const sub = b.closest('.ham-submenu');
        return !sub || (sub.previousElementSibling && sub.previousElementSibling.getAttribute('aria-expanded') === 'true');
      });
    }
    function stageTrackerMenuFocus(menu, dir, from) {
      const items = stageTrackerMenuItems(menu).filter(b => b.getAttribute('aria-disabled') !== 'true');
      if (!items.length) { menu.focus(); return; }
      let i = items.indexOf(from);
      if (dir === 'first') i = 0; else if (dir === 'last') i = items.length - 1;
      else i = (i + dir + items.length) % items.length;
      items[i].focus();
    }
    // While the context menu is open every row shows its gutter at full
    // emphasis (the CSS keys on .ham-menu-open on the panel), so the row
    // the menu acts on is not the only one with visible controls.
    function stageTrackerSetMenuOpen(el, on) {
      if (!el || !el.classList) return;
      if (on) el.classList.add('ham-menu-open'); else el.classList.remove('ham-menu-open');
    }
    function stageTrackerCloseMenu(refocus) {
      const m = _stageTrackerMenu;
      if (!m) return;
      _stageTrackerMenu = null;
      stageTrackerSetMenuOpen(document.getElementById('activity-monitor-panel'), false);
      document.removeEventListener('pointerdown', m.onAway, true);
      document.removeEventListener('keydown', m.onKey, true);
      window.removeEventListener('resize', m.onAway);
      window.removeEventListener('scroll', m.onAway, true);
      if (m.el.parentNode) m.el.parentNode.removeChild(m.el);
      if (refocus && m.restore && typeof m.restore.focus === 'function' && document.contains(m.restore)) m.restore.focus();
    }
    // Open the menu for `items` at (x, y) — or below `anchor` — and run the
    // picked action on them.
    // Pure: does the menu (or one of its submenus) offer an enabled Let hive decide.
    function stageTrackerMenuOffersDelegate(actions) {
      return (actions || []).some(a => a && ((a.id === 'decide:delegate' && a.enabled !== false) || stageTrackerMenuOffersDelegate(a.submenu)));
    }
    function stageTrackerOpenMenu(items, at) {
      stageTrackerCloseMenu(false);
      if (!items.length) return;
      const actions = items.length === 1 ? stageTrackerRowActions(items[0], stageTrackerActionCtx(items[0].repo)) : stageTrackerSelectionActions(items, stageTrackerActionCtx);
      if (items.length === 1 && stageTrackerMenuOffersDelegate(actions)) stageTrackerPrefetchDelegatePlan(items[0]);
      const head = items.length === 1 ? stageTrackerRefOf(items[0]) + (items[0].title ? ' · ' + items[0].title : '') : items.length + ' selected';
      const tpl = document.createElement('template');
      tpl.innerHTML = stageTrackerMenuHtml(actions, head);
      const el = tpl.content.firstElementChild;
      document.body.appendChild(el);
      let x = at.x, y = at.y;
      if (at.anchor && typeof at.anchor.getBoundingClientRect === 'function') { const r = at.anchor.getBoundingClientRect(); x = r.left; y = r.bottom; }
      const pos = stageTrackerMenuPosition(x, y, el.offsetWidth, el.offsetHeight, window.innerWidth, window.innerHeight);
      el.style.setProperty('left', pos.left + 'px');
      el.style.setProperty('top', pos.top + 'px');
      const m = { el, items, restore: at.restore || at.anchor || null };
      m.onAway = e => { if (e.type !== 'pointerdown' || !el.contains(e.target)) stageTrackerCloseMenu(false); };
      m.onKey = e => {
        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); stageTrackerCloseMenu(true); return; }
        if (!el.contains(e.target)) return;
        const b = e.target.closest('[role="menuitem"]');
        switch (e.key) {
          case 'ArrowDown': e.preventDefault(); stageTrackerMenuFocus(el, 1, b); break;
          case 'ArrowUp': e.preventDefault(); stageTrackerMenuFocus(el, -1, b); break;
          case 'Home': e.preventDefault(); stageTrackerMenuFocus(el, 'first'); break;
          case 'End': e.preventDefault(); stageTrackerMenuFocus(el, 'last'); break;
          case 'ArrowRight': if (b && b.getAttribute('aria-haspopup') === 'menu' && b.getAttribute('aria-disabled') !== 'true') { e.preventDefault(); b.setAttribute('aria-expanded', 'true'); stageTrackerMenuFocus(el, 1, b); } break;
          case 'ArrowLeft': { const sub = b && b.closest('.ham-submenu'); if (sub && sub.previousElementSibling) { e.preventDefault(); sub.previousElementSibling.setAttribute('aria-expanded', 'false'); sub.previousElementSibling.focus(); } break; }
          case 'Tab': stageTrackerCloseMenu(true); break;
        }
      };
      el.addEventListener('click', e => {
        const b = e.target.closest('[data-ham-act]');
        if (!b || b.getAttribute('aria-disabled') === 'true') return;
        e.preventDefault(); e.stopPropagation();
        const act = b.dataset.hamAct;
        if (b.getAttribute('aria-haspopup') === 'menu') {
          const open = b.getAttribute('aria-expanded') === 'true';
          b.setAttribute('aria-expanded', open ? 'false' : 'true');
          if (!open) stageTrackerMenuFocus(el, 1, b);
          return;
        }
        stageTrackerCloseMenu(true);
        stageTrackerDoAction(m.items, act);
      });
      document.addEventListener('pointerdown', m.onAway, true);
      document.addEventListener('keydown', m.onKey, true);
      window.addEventListener('resize', m.onAway);
      window.addEventListener('scroll', m.onAway, true);
      _stageTrackerMenu = m;
      stageTrackerSetMenuOpen(document.getElementById('activity-monitor-panel'), true);
      if (at.expandDecide) { const d = el.querySelector('[data-ham-act="decide"]'); if (d && d.getAttribute('aria-disabled') !== 'true') d.setAttribute('aria-expanded', 'true'); }
      stageTrackerMenuFocus(el, 'first');
    }
    // The row (or nested PR) an event happened on, as the items a menu acts
    // on: a selected row with company stands for the whole selection.
    function stageTrackerMenuTargets(target) {
      const sub = target && target.closest ? target.closest('[data-ham-sub-key]') : null;
      const row = target && target.closest ? target.closest('.ham-row[data-ham-key]') : null;
      if (!row && !sub) return null;
      const key = sub ? sub.dataset.hamSubKey : row.dataset.hamKey;
      const list = row ? row.parentNode : sub.closest('#activity-monitor-panel');
      const keys = _stageTrackerSel.has(key) && _stageTrackerSel.size > 1 && list ? stageTrackerRowKeys(list).filter(k => _stageTrackerSel.has(k)) : [key];
      return { items: stageTrackerItemsForKeys(keys), restore: sub ? (sub.querySelector('.ham-kebab') || row) : row };
    }
    function stageTrackerWireRowActions(el) {
      if (!el || !el.dataset || el.dataset.hamWired === '1' || typeof el.addEventListener !== 'function') return;
      el.dataset.hamWired = '1';
      el.addEventListener('contextmenu', e => {
        if (e.shiftKey) return; // Shift+right-click keeps the browser's own menu
        const t = stageTrackerMenuTargets(e.target);
        if (!t || !t.items.length) return;
        e.preventDefault();
        stageTrackerOpenMenu(t.items, { x: e.clientX, y: e.clientY, restore: t.restore });
      });
      el.addEventListener('keydown', e => {
        const row = e.target.closest ? e.target.closest('.ham-row[data-ham-key]') : null;
        if (!row) return;
        if ((e.shiftKey && e.key === 'F10') || e.key === 'ContextMenu') {
          const t = stageTrackerMenuTargets(e.target);
          if (!t || !t.items.length) return;
          e.preventDefault();
          stageTrackerOpenMenu(t.items, { anchor: row, restore: row });
          return;
        }
        if (e.key === ' ' && e.target === row) {
          e.preventDefault();
          stageTrackerSelect(row.dataset.hamKey, { shift: e.shiftKey, meta: e.metaKey || e.ctrlKey });
        }
      });
      // The covered issue's "decision pending on #N" chip goes to the row
      // that carries the decision.
      el.addEventListener('click', e => {
        const c = e.target.closest ? e.target.closest('[data-ham-scroll-to]') : null;
        if (!c) return;
        e.preventDefault(); e.stopPropagation();
        stageTrackerScrollToRow(el, c.getAttribute('data-ham-scroll-to'), c.getAttribute('data-ham-scroll-row'));
      });
      // Revealing a row's Let hive decide (hover or focus) prefetches its plan.
      const prefetchLead = e => {
        const b = e.target.closest ? e.target.closest('.ham-lead-act[data-ham-act="decide:delegate"]') : null;
        if (!b || b.getAttribute('aria-disabled') === 'true') return;
        const sub = b.closest('[data-ham-sub-key]');
        const row = b.closest('.ham-row[data-ham-key]');
        const key = sub ? sub.dataset.hamSubKey : (row ? row.dataset.hamKey : '');
        const item = key ? stageTrackerItemByKey(_stageTracker, key) : null;
        if (item) stageTrackerPrefetchDelegatePlan(item);
      };
      el.addEventListener('pointerover', prefetchLead);
      el.addEventListener('focusin', prefetchLead);
      // The row's inline lead button runs the same path as its ⋯ entry.
      el.addEventListener('click', e => {
        const b = e.target.closest ? e.target.closest('.ham-lead-act[data-ham-act]') : null;
        if (!b) return;
        e.preventDefault(); e.stopPropagation();
        if (b.getAttribute('aria-disabled') === 'true') return;
        // The one row (or nested PR) the button sits on, never the selection.
        const sub = b.closest('[data-ham-sub-key]');
        const row = b.closest('.ham-row[data-ham-key]');
        const key = sub ? sub.dataset.hamSubKey : (row ? row.dataset.hamKey : '');
        const item = key ? stageTrackerItemByKey(_stageTracker, key) : null;
        if (!item) return;
        stageTrackerDecisionRun(item, b.dataset.hamAct);
      });
      // Cmd/Ctrl- or Shift-click anywhere on a row (not on its links or
      // buttons) selects it too.
      el.addEventListener('click', e => {
        if (!(e.metaKey || e.ctrlKey || e.shiftKey)) return;
        if (e.target.closest('a, button, input, select, textarea')) return;
        const row = e.target.closest ? e.target.closest('.ham-row[data-ham-key]') : null;
        if (!row) return;
        e.preventDefault();
        stageTrackerSelect(row.dataset.hamKey, { shift: e.shiftKey, meta: e.metaKey || e.ctrlKey });
      });
      if (typeof stageTrackerWireDecisionPop === 'function') stageTrackerWireDecisionPop(el);
    }

    // ---- "Needs human" decision popover (owner request, 2026-10-28) ----
    // Hovering or focusing the "Needs human decision" chevron word or the
    // needs-human pill says WHAT the person must decide — the reviewer's own
    // question, who asked and when, the blocker in plain words — and offers
    // the actions that advance the row, "Let hive decide" first. The buttons
    // run the row context menu's code path (runRowAction when the shared
    // helper is present, else stageTrackerDoAction), so the same result
    // toast and in-flight chevron apply. The payload's `decision_needed`
    // (stage_tracker_decision.go) carries the text and the option ids.
    const STAGE_TRACKER_DECISION_LEAVE_MS = 250;
    var _stageTrackerDecisionPop = null;
    // Pure: the row's decision — the item's own, else (an issue listing a
    // parked PR) the first nested PR's, as the item the actions act on.
    // An issue row drawn through its one covering PR (via_pr) carries the
    // PR's decision_needed under the issue's number: key it on the PR, or
    // the popover looks up the issue, finds no decision and never opens.
    function stageTrackerDecisionOf(item) {
      if (!item) return null;
      if (item.decision_needed && item.decision_needed.question && !item.via_pr) return { key: stageTrackerItemKey(item), item };
      if (item.kind !== 'issue') return null;
      // Two or more nested PRs each asking: the issue's chevron has no one
      // decision to show — each sub-row's own chevron opens its PR's.
      const asking = (Array.isArray(item.linked_prs) ? item.linked_prs : []).filter(lp => lp && lp.decision_needed && lp.decision_needed.question);
      if (asking.length !== 1) return null;
      const pr = stageTrackerLinkedPRItem(item, asking[0]);
      return { key: stageTrackerItemKey(pr), item: pr };
    }
    // Pure: the trigger — the chevron word or the pill, as a button that
    // owns the popover. No native title: the popover is the hover text.
    function stageTrackerDecisionTriggerHtml(decision, innerHtml, tip, cls) {
      return `<button type="button" class="${esc(cls || '')} ham-decision-trigger" data-ham-decision="${esc(decision.key)}"${tip ? ` data-ham-tip="${esc(tip)}"` : ''} aria-haspopup="dialog" aria-expanded="false">${innerHtml}</button>`;
    }
    // Pure: the popover's options — the server's (what the row's state
    // allows) narrowed by the client's (what this viewer may do): enabled
    // only when both agree, the first reason wins. "Let hive decide" leads.
    function stageTrackerDecisionOptions(item, ctx) {
      const d = item && item.decision_needed;
      const server = d && Array.isArray(d.options) ? d.options : [];
      const flat = stageTrackerFlatActions(stageTrackerRowActions(item, ctx || { user: false, canHold: false, canHuman: false }));
      // An issue row's popover may name its nested PR's takeover options.
      const tkTarget = typeof stageTrackerTakeoverTarget === 'function' ? stageTrackerTakeoverTarget(item) : null;
      if (tkTarget && tkTarget !== item) for (const a of stageTrackerTakeoverActions(tkTarget, ctx || { user: false, canHold: false, canHuman: false })) if (!flat[a.id]) flat[a.id] = a;
      const out = server.map(o => {
        const c = flat[o.action];
        const enabled = !!o.enabled && !!(c && c.enabled);
        const why = enabled ? '' : (o.reason_if_disabled || (c ? c.why : 'Not available on this row') || '');
        return { id: o.action, label: o.label || STAGE_TRACKER_ACTION_LABELS[o.action] || o.action, enabled, why, consequence: o.consequence || '', danger: !!(c && c.danger) || /^decide:close-/.test(o.action), url: o.url || (c && c.url) || '' };
      });
      // "Let hive decide" leads — except where the server's first option
      // is the merge the pill asks for (090: hivecommons/hive#15078).
      const lead = out.findIndex(o => o.id === 'decide:delegate');
      if (lead > 0 && STAGE_TRACKER_MERGE_ACTIONS.indexOf(out[0].id) < 0) out.unshift(out.splice(lead, 1)[0]);
      return out;
    }
    // Pure: drop any sentence already said earlier in the text, so a hover
    // never repeats itself (hivecommons/hive#15394).
    function stageTrackerSayOnce(parts) {
      const seen = new Set();
      const out = [];
      for (const part of parts) {
        const keep = [];
        for (const raw of String(part || '').split(/(?<=[.?!])\s+|\s+—\s+(?=[A-Z@])|\s+·\s+/)) {
          const key = raw.trim().replace(/[.?!]+$/, '').toLowerCase();
          if (!key || seen.has(key)) continue;
          seen.add(key);
          keep.push(raw.trim());
        }
        if (keep.length) out.push(keep.join(' · '));
      }
      return out;
    }
    // Pure: the popover markup — one sentence of state, one primary action,
    // "More ⋯" for the rest, who asked and when, and everything technical
    // behind one collapsed Details (hivecommons/hive#15394).
    function stageTrackerDecisionPopHtml(item, options, nowMs, tip) {
      const d = item.decision_needed || {};
      const ref = stageTrackerRefOf(item);
      const sinceMs = Date.parse(d.since || item.stage_since || '');
      const meta = ['Asked by ' + (d.asked_by || 'the hive'), sinceMs ? agentNowAgo(nowMs - sinceMs) : ''].filter(Boolean).join(' · ');
      // The one recommended action: the first the viewer may take, else the
      // lead option greyed with its reason. Short verbs here; the ⋯ menu
      // keeps the full labels.
      const shortLabel = { 'decide:queue-merge': 'Let hive merge', 'decide:merge-now': 'Merge now', 'decide:direction': 'Send back' };
      const primary = options.length ? (options.find(o => o.enabled) || options[0]) : null;
      const lead = primary ? `<button type="button" class="ham-decision-act ham-decision-lead${primary.danger ? ' ham-menu-danger' : ''}" data-ham-act="${esc(primary.id)}"${primary.enabled ? (primary.consequence ? ` title="${esc(primary.consequence)}"` : '') : ` aria-disabled="true" title="${esc(primary.why)}"`}>${esc(shortLabel[primary.id] || primary.label)}</button>` : '';
      const more = options.length > 1 ? `<button type="button" class="ham-decision-act ham-decision-more" data-ham-more="1" title="All actions for this row">More ⋯</button>` : '';
      const question = d.question || '';
      const said = typeof stageTrackerSayOnce === 'function' ? stageTrackerSayOnce([question, d.blocker, d.detail, tip]) : [question, d.blocker, d.detail, tip].filter(Boolean);
      const details = question ? said.slice(1) : said;
      const fold = details.length ? `<details class="ham-decision-details"><summary>Details</summary>${details.map(x => `<p class="ham-decision-blocker">${esc(x)}</p>`).join('')}</details>` : '';
      // The PR the reviewer named as already carrying this change
      // (hivecommons/hive#15499): a chip under the question so the person
      // sees what "close as a no-op" is measured against.
      const redundantOf = Number(d.redundant_of) || 0;
      const redundantChip = redundantOf ? `<p class="ham-decision-redundant"><span class="ham-chip" title="${esc('A reviewer found this change already on the base through #' + redundantOf)}">redundant of #${redundantOf}</span></p>` : '';
      return `<div class="ham-decision-pop" role="dialog" aria-label="${esc('Decision needed on ' + ref)}" tabindex="-1">
        <div class="ham-decision-head">${esc(ref)}${item.title ? ' · ' + esc(item.title) : ''}</div>
        <p class="ham-decision-q">${esc(question)}</p>${redundantChip}
        ${typeof stageTrackerPresenceTip === 'function' && stageTrackerPresenceTip(item, nowMs) ? `<p class="ham-decision-presence">${stageTrackerPresenceDotHtml(item, nowMs)}${esc(stageTrackerPresenceTip(item, nowMs))}</p>` : ''}
        ${lead || more ? `<div class="ham-decision-acts">${lead}${more}</div>` : ''}
        <p class="ham-decision-meta">${esc(meta)}</p>
        ${fold}
      </div>`;
    }
    // Run a popover action down the row context menu's path.
    // Scroll the tracker row (or nested PR sub-row) with key into view and
    // focus it; fallbackKey names the row that draws it when it is not a
    // row of its own. False when neither is on the board.
    function stageTrackerScrollToRow(el, key, fallbackKey) {
      if (!el || typeof el.querySelector !== 'function' || !key) return false;
      const sel = k => `[data-ham-key="${CSS.escape(k)}"], [data-ham-sub-key="${CSS.escape(k)}"]`;
      const row = el.querySelector(sel(key)) || (fallbackKey ? el.querySelector(sel(fallbackKey)) : null);
      if (!row) return false;
      const reduced = typeof stageTrackerReducedMotion === 'function' && stageTrackerReducedMotion();
      if (typeof row.scrollIntoView === 'function') row.scrollIntoView({ block: 'center', behavior: reduced ? 'auto' : 'smooth' });
      if (typeof row.focus === 'function') row.focus({ preventScroll: true });
      return true;
    }
    function stageTrackerDecisionRun(item, act) {
      if (typeof runRowAction === 'function') return runRowAction(act, item);
      return stageTrackerDoAction([item], act);
    }
    function stageTrackerCloseDecisionPop(refocus) {
      const p = _stageTrackerDecisionPop;
      if (!p) return;
      _stageTrackerDecisionPop = null;
      clearTimeout(p.timer);
      document.removeEventListener('pointerdown', p.onAway, true);
      document.removeEventListener('keydown', p.onKey, true);
      window.removeEventListener('resize', p.onAway);
      window.removeEventListener('scroll', p.onAway, true);
      if (p.el.parentNode) p.el.parentNode.removeChild(p.el);
      const trig = stageTrackerDecisionTrigger(p.key);
      if (trig) trig.setAttribute('aria-expanded', 'false');
      if (refocus && trig && typeof trig.focus === 'function') trig.focus();
    }
    function stageTrackerDecisionTrigger(key) {
      const el = document.getElementById('activity-monitor-panel');
      if (!el || !key) return null;
      for (const b of el.querySelectorAll('.ham-decision-trigger')) if (b.dataset.hamDecision === key) return b;
      return null;
    }
    // Open (or pin) the popover for a trigger. Hover opens it unpinned: it
    // closes once neither the trigger nor the popover is hovered. Click or
    // focus pins it: it then closes on Esc, click-away, scroll or resize.
    function stageTrackerOpenDecisionPop(trigger, pin) {
      const key = trigger && trigger.dataset ? trigger.dataset.hamDecision : '';
      if (!key) return;
      if (_stageTrackerDecisionPop && _stageTrackerDecisionPop.key === key) {
        if (pin) _stageTrackerDecisionPop.pinned = true;
        clearTimeout(_stageTrackerDecisionPop.timer);
        return;
      }
      stageTrackerCloseDecisionPop(false);
      stageTrackerCloseMenu(false);
      const item = stageTrackerItemByKey(_stageTracker, key);
      if (!item || !item.decision_needed) return;
      const options = stageTrackerDecisionOptions(item, stageTrackerActionCtx(item.repo));
      if (options.some(o => o && o.id === 'decide:delegate' && o.enabled)) stageTrackerPrefetchDelegatePlan(item);
      const tpl = document.createElement('template');
      tpl.innerHTML = stageTrackerDecisionPopHtml(item, options, Date.now(), trigger.dataset.hamTip || '');
      const el = tpl.content.firstElementChild;
      document.body.appendChild(el);
      const r = trigger.getBoundingClientRect();
      const pos = stageTrackerMenuPosition(r.left, r.bottom, el.offsetWidth, el.offsetHeight, window.innerWidth, window.innerHeight);
      el.style.setProperty('left', pos.left + 'px');
      el.style.setProperty('top', pos.top + 'px');
      trigger.setAttribute('aria-expanded', 'true');
      const p = { el, key, item, pinned: !!pin, timer: 0 };
      p.onAway = e => { if (e.type !== 'pointerdown' || !(el.contains(e.target) || (e.target.closest && e.target.closest('.ham-decision-trigger') === stageTrackerDecisionTrigger(key)))) stageTrackerCloseDecisionPop(false); };
      p.onKey = e => {
        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); stageTrackerCloseDecisionPop(true); return; }
        if (e.key === 'Tab' && el.contains(e.target)) {
          const acts = Array.from(el.querySelectorAll('.ham-decision-act, .ham-decision-details > summary'));
          const last = acts[acts.length - 1], first = acts[0];
          if ((!e.shiftKey && e.target === last) || (e.shiftKey && (e.target === first || e.target === el))) stageTrackerCloseDecisionPop(true);
        }
      };
      el.addEventListener('pointerenter', () => clearTimeout(p.timer));
      el.addEventListener('pointerleave', () => stageTrackerDecisionPopLeave());
      el.addEventListener('focusin', () => { p.pinned = true; clearTimeout(p.timer); });
      el.addEventListener('click', e => {
        const moreBtn = e.target.closest('[data-ham-more]');
        if (moreBtn) {
          // "More ⋯" hands over to the row's own menu, anchored on the trigger.
          e.preventDefault(); e.stopPropagation();
          const anchor = stageTrackerDecisionTrigger(key) || trigger;
          stageTrackerCloseDecisionPop(false);
          stageTrackerOpenMenu([p.item], { anchor, restore: anchor });
          return;
        }
        const b = e.target.closest('[data-ham-act]');
        if (!b || b.getAttribute('aria-disabled') === 'true') return;
        e.preventDefault(); e.stopPropagation();
        const act = b.dataset.hamAct;
        stageTrackerCloseDecisionPop(true);
        stageTrackerDecisionRun(p.item, act);
      });
      document.addEventListener('pointerdown', p.onAway, true);
      document.addEventListener('keydown', p.onKey, true);
      window.addEventListener('resize', p.onAway);
      window.addEventListener('scroll', p.onAway, true);
      _stageTrackerDecisionPop = p;
      if (pin && pin.focus) { const first = el.querySelector('.ham-decision-act:not([aria-disabled="true"])'); (first || el).focus(); }
    }
    // The pointer left the trigger or the popover: close after a beat unless
    // it is pinned or the pointer is back over either (rows re-render, so
    // the trigger is looked up afresh by key).
    function stageTrackerDecisionPopLeave() {
      const p = _stageTrackerDecisionPop;
      if (!p || p.pinned) return;
      clearTimeout(p.timer);
      p.timer = setTimeout(() => {
        const q = _stageTrackerDecisionPop;
        if (!q || q.pinned) return;
        const trig = stageTrackerDecisionTrigger(q.key);
        if (q.el.matches(':hover') || (trig && trig.matches(':hover'))) { stageTrackerDecisionPopLeave(); return; }
        stageTrackerCloseDecisionPop(false);
      }, STAGE_TRACKER_DECISION_LEAVE_MS);
    }
    function stageTrackerWireDecisionPop(el) {
      el.addEventListener('pointerover', e => {
        const b = e.target.closest ? e.target.closest('.ham-decision-trigger') : null;
        if (b && el.contains(b) && e.pointerType !== 'touch') stageTrackerOpenDecisionPop(b, false);
      });
      el.addEventListener('pointerout', e => {
        const b = e.target.closest ? e.target.closest('.ham-decision-trigger') : null;
        if (b) stageTrackerDecisionPopLeave();
      });
      el.addEventListener('focusin', e => {
        const b = e.target.closest ? e.target.closest('.ham-decision-trigger') : null;
        if (b) stageTrackerOpenDecisionPop(b, true);
      });
      el.addEventListener('click', e => {
        const b = e.target.closest ? e.target.closest('.ham-decision-trigger') : null;
        if (!b || e.metaKey || e.ctrlKey || e.shiftKey) return;
        e.preventDefault(); e.stopPropagation();
        if (_stageTrackerDecisionPop && _stageTrackerDecisionPop.key === b.dataset.hamDecision && _stageTrackerDecisionPop.pinned) { stageTrackerCloseDecisionPop(false); return; }
        stageTrackerOpenDecisionPop(b, { focus: true });
      });
      el.addEventListener('keydown', e => {
        const b = e.target.closest ? e.target.closest('.ham-decision-trigger') : null;
        if (b && (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); stageTrackerOpenDecisionPop(b, { focus: true }); }
      });
    }
    function stageTrackerSelect(key, mods) {
      const el = document.getElementById('activity-monitor-panel');
      if (!el || !key) return;
      const next = stageTrackerSelectionNext(_stageTrackerSel, _stageTrackerSelAnchor, stageTrackerRowKeys(el), key, mods);
      _stageTrackerSel = next.sel;
      _stageTrackerSelAnchor = next.anchor;
      stageTrackerApplySelection(el, _stageTrackerSel);
      stageTrackerRenderSelBar();
    }
    function hamSelectClick(e, btn) {
      const sub = btn && btn.closest ? btn.closest('[data-ham-sub-key]') : null;
      const row = btn && btn.closest ? btn.closest('.ham-row[data-ham-key]') : null;
      const key = sub ? sub.dataset.hamSubKey : (row ? row.dataset.hamKey : '');
      if (key) stageTrackerSelect(key, { shift: !!(e && e.shiftKey), meta: !!(e && (e.metaKey || e.ctrlKey)) });
    }
    function hamRowMenu(e, btn) {
      const t = stageTrackerMenuTargets(btn);
      if (!t || !t.items.length) return;
      stageTrackerOpenMenu(t.items, { anchor: btn, restore: btn });
    }
    function hamSelClear() {
      _stageTrackerSel = new Set();
      _stageTrackerSelAnchor = '';
      const el = document.getElementById('activity-monitor-panel');
      if (el) stageTrackerApplySelection(el, _stageTrackerSel);
      stageTrackerRenderSelBar();
    }
    function hamSelAction(id, btn) {
      if (btn && btn.getAttribute('aria-disabled') === 'true') return;
      const el = document.getElementById('activity-monitor-panel');
      const items = stageTrackerItemsForKeys(el ? stageTrackerRowKeys(el).filter(k => _stageTrackerSel.has(k)) : []);
      if (!items.length) return;
      if (id === 'decide') { stageTrackerOpenMenu(items, { anchor: btn, restore: btn, expandDecide: true }); return; }
      stageTrackerDoAction(items, id);
    }
    // Pure: the selection bar — count, Clear, and the shared actions
    // (disabled ones keep their tooltip, so aria-disabled rather than disabled).
    // An entry flagged `lead` (Accept triage when every selected row waits
    // on triage) renders as the row header's lead button, not a chip.
    function stageTrackerSelBarHtml(items, actions) {
      if (!items.length) return '';
      const btn = a => `<button type="button" class="${a.lead ? 'ham-lead-act' : 'ham-chip'}" data-action="hamSelAction" data-arg0="${esc(a.id)}" data-arg-types="s,t"${a.enabled ? '' : ` aria-disabled="true" title="${esc(a.why || '')}"`}>${esc(a.label)}</button>`;
      return `<span class="ham-sel-count">${items.length} selected</span><button type="button" class="ham-chip" data-action="hamSelClear">Clear</button>${actions.map(btn).join('')}`;
    }
    function stageTrackerRenderSelBar() {
      const bar = document.getElementById('activity-monitor-selbar');
      if (!bar) return;
      const el = document.getElementById('activity-monitor-panel');
      const items = stageTrackerItemsForKeys(el ? stageTrackerRowKeys(el).filter(k => _stageTrackerSel.has(k)) : []);
      if (!items.length) { bar.hidden = true; bar.innerHTML = ''; return; }
      bar.hidden = false;
      const html = stageTrackerSelBarHtml(items, stageTrackerSelectionActions(items, stageTrackerActionCtx));
      if (bar.innerHTML !== html) bar.innerHTML = html;
    }
    function stageTrackerActionLabel(id) {
      const base = typeof stageTrackerActionBase === 'function' ? stageTrackerActionBase(id) : id;
      return STAGE_TRACKER_ACTION_LABELS[base] || id;
    }
    // Pure: which endpoint an action hits for one item. `once` marks a
    // hive-wide call made one time per batch.
    function stageTrackerActionRequest(item, id, payload) {
      payload = payload || {};
      const p = repoParts(item.repo);
      const n = Number(item.number) || 0;
      if (!p.owner || !p.repo || !n) return null;
      const seg = encodeURIComponent(p.owner) + '/' + encodeURIComponent(p.repo);
      const type = item.kind === 'pr' ? 'pr' : 'issue';
      switch (id) {
        case 'hold': return { url: '/api/repos/' + seg + '/items/' + n + '/hold', body: { held: true, type } };
        case 'release': return { url: '/api/repos/' + seg + '/items/' + n + '/hold', body: { held: false, type } };
        case 'retry': return { url: '/api/repos/rescan', body: {}, once: true };
        case 'needs-human': return { url: '/api/review/pipeline/' + seg + '/' + n + '/send-to-human', body: { reason: payload.comment || ('flagged from the activity monitor for ' + stageTrackerRefOf(item)) } };
        case 'accept-triage': return item.kind === 'pr' ? null : { url: '/api/repos/' + seg + '/items/' + n + '/accept-triage', body: {} };
        case 'blocker:recheck': return item.kind === 'pr' ? null : { url: '/api/repos/' + seg + '/items/' + n + '/blocker/recheck', body: {} };
        case 'blocker:close-partial': return item.kind === 'pr' ? null : { url: '/api/repos/' + seg + '/items/' + n + '/blocker/close-partial', body: { comment: payload.comment || '' } };
      }
      if (id === 'decide:delegate' || id === 'decide:delegate:rerun') {
        const body = { type, reason: item.reason || '', caption: item.caption || '', sentinel_reason: item.sentinel_reason || '' };
        // Re-run skips the "already recorded" dedup on a stuck decision.
        if (id === 'decide:delegate:rerun') body.force = true;
        if (payload.chosen) body.chosen = payload.chosen;
        // The head keys the server's plan cache: a push is a fresh plan.
        if (item.head) body.head = String(item.head);
        return { url: '/api/repos/' + seg + '/items/' + n + '/decide-delegated', body, poll: true };
      }
      if (id === 'decide:delegate-all') {
        // The issue's open nested PRs by number: the server decides each
        // one and answers with `items` (one outcome per PR) + a summary.
        const prs = stageTrackerNestedOpenPRs(item).map(p => p.number);
        if (type !== 'issue' || prs.length < STAGE_TRACKER_DELEGATE_ALL_MIN) return null;
        return { url: '/api/repos/' + seg + '/items/' + n + '/decide-delegated', body: { type, prs, reason: item.reason || '', caption: item.caption || '', sentinel_reason: item.sentinel_reason || '' }, poll: true };
      }
      if (id === 'decide:adopt-now') {
        return { url: '/api/repos/' + seg + '/items/' + n + '/decision', body: { decision: 'adopt', force: true, comment: payload.comment || '', type } };
      }
      if (stageTrackerActionBase(id) === 'decide:reroute') {
        const to = payload.to || stageTrackerRerouteTargetOf(id) || STAGE_TRACKER_REROUTE_ANY;
        return { url: '/api/repos/' + seg + '/items/' + n + '/decision', body: { decision: 'reroute', to, comment: payload.comment || '', type } };
      }
      if (id.indexOf('decide:') === 0) {
        const body = { decision: id.slice('decide:'.length), comment: payload.comment || '', type };
        if (payload.supersededBy) body.superseded_by = payload.supersededBy;
        if (payload.chosen) body.chosen = payload.chosen;
        return { url: '/api/repos/' + seg + '/items/' + n + '/decision', body };
      }
      return null;
    }
    // Pure: one line for the result toast. A bulk Accept triage sums up as
    // "Accepted 4 issues" (the per-row toasts carry the rest).
    function stageTrackerActionSummary(label, results, id) {
      const n = results.length;
      const fails = results.filter(r => !r.ok);
      const ok = n - fails.length;
      let msg = label + ': ' + (id === 'decide:delegate' || id === 'decide:delegate-all' ? stageTrackerDelegateTally(results) : ok + '/' + n + ' done');
      if (id === 'accept-triage' && n > 1) msg = 'Accepted ' + ok + (fails.length ? ' of ' + n : '') + ' issue' + (ok === 1 && !fails.length ? '' : 's');
      const debounced = results.some(r => r.ok && r.data && (r.data.status === 'debounced' || r.data.status === 'in-progress'));
      if (debounced) msg += ' (a rescan was already running — the next one is queued by the hive)';
      if (fails.length) {
        msg += '. Failed: ' + fails.slice(0, STAGE_TRACKER_MENU_FAIL_LINES).map(r => stageTrackerRefOf(r.item) + ' — ' + r.error).join('; ');
        if (fails.length > STAGE_TRACKER_MENU_FAIL_LINES) msg += '; +' + (fails.length - STAGE_TRACKER_MENU_FAIL_LINES) + ' more';
      }
      const decided = stageTrackerDelegatedSummary(results);
      if (decided) msg += '. ' + decided;
      return { msg, type: fails.length ? 'error' : 'success' };
    }
    // Pure: the delegate tally — "3 sent · 2 decided · 1 no-op[ · 1 failed]".
    // An issue decided per PR (`items`) counts each PR, not the issue.
    function stageTrackerDelegateTally(results) {
      let sent = 0, decided = 0, noop = 0, failed = 0;
      const one = (ok, action, error) => { sent++; if (!ok || error) failed++; else if (action === STAGE_TRACKER_ACTION_NOOP) noop++; else decided++; };
      for (const r of results) {
        const subs = r && r.ok && r.data && Array.isArray(r.data.items) ? r.data.items : null;
        if (subs && subs.length) { for (const it of subs) one(true, it.result && it.result.action, it.error); continue; }
        one(!!r.ok, r.result ? r.result.action : (r.data && r.data.result ? r.data.result.action : ''), '');
      }
      return sent + ' sent · ' + decided + ' decided · ' + noop + ' no-op' + (failed ? ' · ' + failed + ' failed' : '');
    }
    // Pure: the per-PR outcomes of an issue decided per PR, as
    // {item, result} pairs for one toast each; [] when the job had none.
    function stageTrackerDelegatedSubResults(issue, data) {
      const subs = data && Array.isArray(data.items) ? data.items : [];
      return subs.map(it => {
        const n = Number(it.number) || 0;
        const key = stageTrackerItemKey({ repo: issue.repo, kind: 'pr', number: n });
        const item = stageTrackerItemByKey(_stageTracker, key) || { repo: issue.repo, kind: 'pr', number: n, url: '' };
        const result = it.error ? stageTrackerErrorResult('decide:delegate', item, it.error) : stageTrackerResultOf('decide:delegate', item, it);
        return { item, result };
      });
    }
    // Pure: one clause per delegated decision — "#12 approve (small-low-risk)",
    // with "kept for owner" when the hive only recommended.
    function stageTrackerDelegatedSummary(results) {
      const parts = [];
      for (const r of results) {
        const subs = r && r.ok && r.data && Array.isArray(r.data.items) ? r.data.items : [];
        for (const it of subs) {
          const sd = it.decision;
          if (sd) parts.push((r.item.repo || '') + '#' + it.number + ' ' + (sd.verdict || '?') + (sd.rule ? ' (' + sd.rule + ')' : '') + (sd.keeps_park ? ' — kept for the owner' : ''));
          else if (it.error) parts.push((r.item.repo || '') + '#' + it.number + ' failed — ' + it.error);
        }
        const d = r && r.ok && r.data && r.data.decision;
        if (!d) continue;
        parts.push(stageTrackerRefOf(r.item) + ' ' + (d.verdict || '?') + (d.rule ? ' (' + d.rule + ')' : '') + (d.keeps_park ? ' — kept for the owner' : ''));
      }
      return parts.length ? 'Hive decided: ' + parts.slice(0, STAGE_TRACKER_MENU_FAIL_LINES).join('; ') + (parts.length > STAGE_TRACKER_MENU_FAIL_LINES ? '; +' + (parts.length - STAGE_TRACKER_MENU_FAIL_LINES) + ' more' : '') : '';
    }
    // Pure: whether a delegated-decision job has finished.
    function stageTrackerDelegatedDone(job) { return !!job && job.status !== 'running'; }
    // Poll a delegated-decision job until it ends (or the budget runs out);
    // resolves with the last job payload read.
    async function stageTrackerAwaitDelegated(url, job) {
      for (let i = 0; i < STAGE_TRACKER_DELEGATE_POLL_MAX && !stageTrackerDelegatedDone(job); i++) {
        await new Promise(res => setTimeout(res, STAGE_TRACKER_DELEGATE_POLL_MS));
        try {
          const r = await fetch(url);
          if (r.ok) job = await r.json();
        } catch (e) { /* keep polling */ }
      }
      return job;
    }
    function stageTrackerProgressToast(label, n) {
      const el = showToast('', 'info', true);
      const text = document.createTextNode('');
      if (el) el.appendChild(text);
      return {
        update(i, item) { text.nodeValue = label + ' ' + (i + 1) + '/' + n + ' — ' + stageTrackerRefOf(item) + '…'; },
        done(msg, type) { if (!msg) { if (el) dismissToast(el); return; } if (el) dismissToast(el, msg, type); else showToast(msg, type); },
      };
    }
    // Replace one item in the current tracker payload with fn(item) and
    // re-render, so the row reads its new state before the next fetch.
    function stageTrackerPatchItem(item, fn) {
      const d = _stageTracker;
      if (!d || !Array.isArray(d.items)) return;
      const key = stageTrackerItemKey(item);
      const i = d.items.findIndex(x => stageTrackerItemKey(x) === key);
      if (i < 0) return;
      d.items[i] = fn(d.items[i]);
      if (typeof renderStageTracker === 'function') renderStageTracker(d);
    }
    function stageTrackerAfterActions() {
      _stageTrackerEtag = '';
      _stageTrackerSig = '';
      fetchStageTracker();
      fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
    }
    // Run one action over the items in order, one request each (a hive-wide
    // one once), reporting progress and the per-row outcome.
    async function stageTrackerRunActions(items, id, payload, rows) {
      const label = stageTrackerActionLabel(id);
      const prog = stageTrackerProgressToast(label, items.length);
      const results = [];
      const once = {};
      const toasts = [];
      const pending = [];
      for (let i = 0; i < items.length; i++) {
        const it = items[i];
        prog.update(i, it);
        const req = stageTrackerActionRequest(it, id, payload);
        if (!req) { results.push({ item: it, ok: false, error: 'not applicable' }); continue; }
        if (req.once && once[req.url]) { results.push({ item: it, ok: true, data: once[req.url] }); continue; }
        // The clicked row and the item acted on (a nested PR for an issue
        // row's decision) both read "acting…" from the first byte sent.
        const keys = stageTrackerPendingKeys(it, rows);
        const pend = stageTrackerSetPending(keys, stageTrackerActingCaption(id, it));
        pending.push(pend);
        stageTrackerClearActionFailure(it);
        try {
          const r = await fetch(req.url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(req.body) });
          let data = await r.json().catch(() => ({}));
          if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
          if (data.minStatusSeq != null) _statusSeqFloor = Math.max(_statusSeqFloor || 0, Number(data.minStatusSeq) || 0);
          if (req.poll && !stageTrackerDelegatedDone(data)) {
            showToast('Hive is deciding ' + stageTrackerRefOf(it) + '…', 'info');
            data = await stageTrackerAwaitDelegated(req.url, data);
          }
          if (req.poll) {
            if (!stageTrackerDelegatedDone(data)) throw new Error('still deciding — check the row shortly');
            if (data.status === 'failed') throw new Error(data.error || 'the hive could not decide');
          }
          if (req.once) once[req.url] = data;
          if (id === 'accept-triage') stageTrackerPatchItem(it, stageTrackerTriageAccepted);
          const result = stageTrackerResultOf(id, it, data);
          results.push({ item: it, ok: true, data, result });
          // No change → nothing to wait for; otherwise the row keeps
          // acting until the tracker reports the target's new state.
          // An approval is recorded the moment the server answers; the
          // row's next state (the gate, or "Rebasing · attempt n/2") is
          // the tracker's to show, so "Approving…" must not linger beside
          // the ✓ waiting for it (#15103).
          if (result.action === 'refused') stageTrackerNoteActionFailure(it, id, result.outcome || 'refused');
          if (result.action === STAGE_TRACKER_ACTION_NOOP || result.action === STAGE_TRACKER_ACTION_NO_EFFECT || result.action === 'refused' || result.action === 'approved') stageTrackerClearPending(pend);
          else {
            if (typeof stageTrackerDecidePending === 'function') stageTrackerDecidePending(pend, result, stageTrackerItemByKey(_stageTracker, stageTrackerTargetKey(result.target)));
            // Closed / merged: the terminal caption stays on the chevron
            // until the row leaves the list on the next tracker poll.
            const terminal = stageTrackerTerminalCaption(result);
            if (terminal) { pend.caption = terminal; pend.decided = true; }
            stageTrackerArmPending(pend, result.target);
          }
          if (result.action !== STAGE_TRACKER_ACTION_NOOP && result.action !== STAGE_TRACKER_ACTION_NO_EFFECT && typeof stageTrackerNoteOwnerEvent === 'function') stageTrackerNoteOwnerEvent(stageTrackerTargetKey(result.target) || stageTrackerItemKey(it), STAGE_TRACKER_ACTION_WORDS[result.action] || stageTrackerActionLabel(id));
          if (items.length <= STAGE_TRACKER_RESULT_TOAST_MAX) toasts.push(stageTrackerResultToast(result, it, { id, rowRef: stageTrackerRefOf(it) }));
          // An issue decided per PR: one toast per PR as well, each on its
          // own sub-row (hivecommons/hive#15009).
          for (const sub of stageTrackerDelegatedSubResults(it, data)) {
            if (sub.result.action !== STAGE_TRACKER_ACTION_NOOP && sub.result.action !== 'error') { const sp = stageTrackerSetPending([stageTrackerItemKey(sub.item)], stageTrackerActingCaption('decide:delegate')); pending.push(sp); if (typeof stageTrackerDecidePending === 'function') stageTrackerDecidePending(sp, sub.result, sub.item); stageTrackerArmPending(sp, sub.result.target); }
            if (items.length <= STAGE_TRACKER_RESULT_TOAST_MAX) toasts.push(stageTrackerResultToast(sub.result, sub.item, { id: 'decide:delegate', rowRef: stageTrackerRefOf(sub.item) }));
          }
        } catch (e) {
          const error = e && e.message ? e.message : 'network error';
          results.push({ item: it, ok: false, error });
          stageTrackerClearPending(pend);
          stageTrackerNoteActionFailure(it, id, stageTrackerActionLabel(id) + ' failed: ' + error);
          if (items.length <= STAGE_TRACKER_RESULT_TOAST_MAX) toasts.push(stageTrackerResultToast(stageTrackerErrorResult(id, it, error), it, { id, rowRef: stageTrackerRefOf(it) }));
        }
      }
      const sum = stageTrackerActionSummary(label, results, id);
      // One item that succeeded: its result toast says it all. A batch or
      // a failure keeps the summary line beside the per-item toasts.
      if (toasts.length === 1 && results.length === 1 && results[0].ok) prog.done(null); else prog.done(sum.msg, sum.type);
      stageTrackerAfterActions();
      results.toasts = toasts;
      results.pending = pending;
      return results;
    }
    // What an action needs from the operator first (in-app dialogs only);
    // null = cancelled.
    // Pure: the other open PRs in the same repos as items, newest first, as
    // datalist options for the "superseded by" picker.
    function stageTrackerSupersederOptions(d, items) {
      const repos = {};
      const skip = {};
      for (const it of items) { repos[it.repo] = true; skip[stageTrackerItemKey(it)] = true; }
      const out = [];
      const seen = {};
      const add = pr => {
        const k = stageTrackerItemKey(pr);
        if (skip[k] || seen[k] || !repos[pr.repo]) return;
        seen[k] = true;
        out.push({ value: pr.repo + '#' + pr.number, label: (pr.title || '').slice(0, 80) });
      };
      for (const it of ((d && d.items) || [])) {
        if (it.kind === 'pr') add(it);
        for (const lp of (Array.isArray(it.linked_prs) ? it.linked_prs : [])) add({ repo: it.repo, number: lp.number, kind: 'pr', title: lp.title || '' });
      }
      return out.sort((a, b) => Number(b.value.split('#')[1]) - Number(a.value.split('#')[1]));
    }
    // Pure: the pre-filled winner for "Close as superseded by": the merged
    // PR the supersession sweep already detected on the row (its closer —
    // #15303 sat superseded by #15304 with the field empty), else, for a
    // nested PR with exactly one open sibling under the same issue (two PRs
    // for one issue), that sibling; '' when neither is known. A closer in
    // another repo is only named when the sweep spelt it owner/repo.
    function stageTrackerSupersederDefault(items) {
      const one = Array.isArray(items) && items.length === 1 ? items[0] : null;
      if (!one) return '';
      const sup = one.superseded && typeof one.superseded === 'object' ? one.superseded : null;
      const closer = sup ? Number(sup.closer_pr) || 0 : 0;
      if (closer) {
        const cr = String(sup.closer_repo || '');
        return (cr.indexOf('/') >= 0 ? cr : String(one.repo || '')) + '#' + closer;
      }
      if (Array.isArray(one.siblings) && one.siblings.length === 1) return one.repo + '#' + one.siblings[0];
      return '';
    }
    // Pure: validates a "superseded by" reference; "" is allowed (no winner named).
    function stageTrackerSupersederValid(s, items) {
      s = String(s || '').trim();
      if (!s) return true;
      const m = s.match(/^(?:https?:\/\/[^\/]+\/([^\/#]+\/[^\/#]+)\/(?:pull|issues)\/(\d+)\/?|(?:([^\/#\s]+\/[^\/#\s]+))?#?(\d+))$/);
      if (!m) return 'Enter a PR number, owner/repo#number or a GitHub PR URL';
      const repo = m[1] || m[3] || (items[0] && items[0].repo) || '';
      const n = Number(m[2] || m[4]);
      for (const it of items) if (it.repo === repo && Number(it.number) === n) return 'A pull request cannot supersede itself';
      return true;
    }
    // Pure: the sentinel preface for a Decide… dialog — one line per
    // flagged PR, so the operator reads the security reason before
    // choosing Approve; '' when nothing in items is flagged.
    function stageTrackerSentinelPreface(items) {
      return (items || []).filter(it => it && it.sentinel_alert).map(it => '⚠ ' + stageTrackerRefOf(it) + ' — ' + stageTrackerSentinelTip(it)).join('\n');
    }
    // Every confirm a row action raises can be opted out of per browser,
    // per action id (the dialog's "Don't ask again for this action" box;
    // the tracker's ⚙ settings → Confirmations → Reset confirmations turns
    // them all back on). A skipped confirm runs the action at once; what the
    // dialog would have said — notably a sentinel ⚠ line, which is per head
    // — still reaches the owner in the persistent result toast. Prompts
    // whose input is required (a direction, the superseding PR) cannot be
    // skipped; optional-comment prompts can, and then post without a comment.
    const STAGE_TRACKER_CONFIRM_KEY_PREFIX = 'hive.tracker.confirm.';
    const STAGE_TRACKER_CONFIRM_SKIP = 'skip';
    const STAGE_TRACKER_CONFIRM_CHECK_LABEL = 'Don\'t ask again for this action';
    const STAGE_TRACKER_DELEGATE_EXPLANATION = 'It records a "/hive decision" (or a recommendation when the question is the hive\'s own policy), lifts the park and stamps an audit marker. Rails: it never merges, never acts on a held item, never grants secrets or permissions, and leaves the sentinel label to a person.';
    function stageTrackerConfirmSkipped(id) {
      try { return localStorage.getItem(STAGE_TRACKER_CONFIRM_KEY_PREFIX + id) === STAGE_TRACKER_CONFIRM_SKIP; } catch { return false; }
    }
    function stageTrackerSetConfirmSkip(id, skip) {
      try { if (skip) localStorage.setItem(STAGE_TRACKER_CONFIRM_KEY_PREFIX + id, STAGE_TRACKER_CONFIRM_SKIP); else localStorage.removeItem(STAGE_TRACKER_CONFIRM_KEY_PREFIX + id); } catch {}
      return !!skip;
    }
    // The action ids whose confirm is skipped in this browser, in menu order.
    function stageTrackerSkippedConfirms() {
      return Object.keys(STAGE_TRACKER_ACTION_LABELS).filter(stageTrackerConfirmSkipped);
    }
    function stageTrackerResetConfirms() {
      const n = stageTrackerSkippedConfirms().length;
      for (const id of Object.keys(STAGE_TRACKER_ACTION_LABELS)) stageTrackerSetConfirmSkip(id, false);
      if (typeof stageTrackerRenderSettings === 'function') stageTrackerRenderSettings();
      if (typeof showToast === 'function') showToast(n ? 'Row actions ask again before they run (' + n + ' reset)' : 'No confirmations were skipped', 'success');
      return n;
    }
    // Pure: the hiveConfirm / hivePrompt config for a skippable ask — the
    // opt-out checkbox is added unless the input is required.
    function stageTrackerAskConfig(cfg, required) {
      return required ? cfg : Object.assign({}, cfg, { check: { label: STAGE_TRACKER_CONFIRM_CHECK_LABEL } });
    }
    // A yes/no dialog (kind: confirm) or an optional text dialog (kind: prompt) that
    // honours the per-action opt-out. Resolves true / the entered text, ''
    // for a skipped prompt, or null when cancelled.
    async function stageTrackerAsk(id, kind, cfg) {
      if (stageTrackerConfirmSkipped(id)) return kind === 'prompt' ? '' : true;
      const res = kind === 'prompt' ? await hivePrompt(stageTrackerAskConfig(cfg, false)) : await hiveConfirm(stageTrackerAskConfig(cfg, false));
      if (res == null || res === false) return null;
      if (res && typeof res === 'object' && res.checked) { stageTrackerSetConfirmSkip(id, true); if (typeof stageTrackerRenderSettings === 'function') stageTrackerRenderSettings(); }
      return kind === 'prompt' ? (res && typeof res === 'object' ? res.value : res) : true;
    }
    // Pure: the Let hive decide confirm — the sentinel ⚠ line(s) first, the
    // question, then the owner-delegate explanation.
    function stageTrackerDelegateConfirm(items) {
      const refs = (items || []).map(stageTrackerRefOf).join(', ');
      const sentinel = stageTrackerSentinelPreface(items);
      return { title: 'Let hive decide', message: (sentinel ? sentinel + '\n\n' : '') + 'Let the hive decide ' + refs + ' as the owner\'s delegate, under the owner-delegate policy? ' + STAGE_TRACKER_DELEGATE_EXPLANATION, confirmLabel: 'Let hive decide', danger: !!sentinel };
    }
    // The "Let hive decide" preview (hivecommons/hive#15126), redesigned on
    // operator feedback from hive's own spoke (the dialog for
    // hivecommons/hive#15332: "yikes - this is a mess … this needs to be
    // exceedingly simple to understand and act on"). Budget: title + one
    // subject line; ONE decision sentence; ONE why line; at most two
    // "then" items; everything else — the sentinel finding, the rails, the
    // rungs ruled out — behind a single collapsed Details. Two buttons,
    // [Cancel] [Let hive do that]; the merge button — "Approve and let hive
    // merge" (queue-merge), or "Merge now as maintainer" when the hive
    // would approve anyway — only when the plan says the head is mergeable
    // as it is.
    const STAGE_TRACKER_PLAN_BUTTON_DELEGATE = 'Let hive do that';
    const STAGE_TRACKER_PLAN_BUTTON_MERGE = 'Approve and let hive merge';
    const STAGE_TRACKER_PLAN_MERGE_ACTIONS = ['decide:queue-merge', 'decide:merge-now'];
    const STAGE_TRACKER_PLAN_TITLE = 'Let hive decide';
    const STAGE_TRACKER_PLAN_EYEBROW = 'Hive would';
    const STAGE_TRACKER_PLAN_NEXT_MAX = 2;
    const STAGE_TRACKER_PLAN_TITLE_MAX = 48;
    const STAGE_TRACKER_PLAN_RAILS = 'Rails: never merges, never touches held items, never grants secrets; the sentinel label stays with a person.';
    const STAGE_TRACKER_PLAN_MERGE_WHY = 'Not mergeable as it is';
    // Pure: the decision sentence without a stale "Hive would:" prefix —
    // the eyebrow says it once (the duplicated prefix of #15332's dialog).
    function stageTrackerPlanSentence(pl) {
      let text = String(pl.summary || pl.would || 'decide it').trim().replace(/^(hive would:\s*)+/i, '');
      if (!text) text = 'decide it';
      text = text.charAt(0).toUpperCase() + text.slice(1);
      return /[.!?]$/.test(text) ? text : text + '.';
    }
    function stageTrackerPlanTruncate(text, max) {
      text = String(text || '').trim();
      return text.length > max ? text.slice(0, max - 1).replace(/\s+\S*$/, '') + '…' : text;
    }
    // Pure: one view model for every decision the preview can show — a
    // PR or an issue; direction, adopt, close, approve, recommend-only.
    //   { title, subject, finding, decisions: [{ ref, sentence, why, next[] }],
    //     ruledOut[], rails, mergeable, mergeWhy, mergeAction, mergeLabel, danger }
    function stageTrackerDelegatePlanView(plans, sentinel) {
      plans = plans || [];
      // One item: "repo#N · title"; a batch names each item on its own
      // block instead, so no reference is printed twice.
      let subject = plans.length + ' items';
      if (plans.length === 1) {
        subject = plans[0].ref || ('#' + plans[0].number);
        if (plans[0].title) subject += ' · ' + stageTrackerPlanTruncate(plans[0].title, STAGE_TRACKER_PLAN_TITLE_MAX);
      }
      const decisions = plans.map(pl => ({
        ref: pl.ref || ('#' + pl.number),
        sentence: stageTrackerPlanSentence(pl),
        why: pl.why ? String(pl.why).trim() : '',
        next: (pl.consequences || []).slice(0, STAGE_TRACKER_PLAN_NEXT_MAX).map(String),
      }));
      const ruledOut = [].concat(...plans.map(pl => (pl.ladder && pl.ladder.ruled_out) || []));
      const mergeable = stageTrackerDelegatePlanMergeable(plans);
      return {
        title: STAGE_TRACKER_PLAN_TITLE,
        subject,
        finding: sentinel || '',
        decisions,
        ruledOut,
        rails: STAGE_TRACKER_PLAN_RAILS,
        mergeable,
        mergeWhy: mergeable ? '' : (stageTrackerDelegatePlanMergeReason(plans) || STAGE_TRACKER_PLAN_MERGE_WHY),
        showMerge: stageTrackerDelegatePlanOffersMerge(plans),
        mergeAction: stageTrackerDelegatePlanMergeAction(plans),
        mergeLabel: stageTrackerDelegatePlanMergeLabel(plans),
        danger: !!sentinel,
      };
    }
    // Pure: the dialog's inner HTML from the view model. The Details
    // disclosure is the only place the finding, the rails and the ruled-out
    // rungs appear; it is collapsed by default.
    function stageTrackerDelegatePlanHtml(view) {
      const esc = hiveDialogEscape;
      const blocks = view.decisions.map(d => {
        const multi = view.decisions.length > 1;
        return '<div class="ham-plan-decision">'
          + (multi ? '<div class="ham-plan-ref">' + esc(d.ref) + '</div>' : '')
          + '<div class="ham-plan-eyebrow">' + esc(STAGE_TRACKER_PLAN_EYEBROW) + '</div>'
          + '<p class="ham-plan-sentence">' + esc(d.sentence) + '</p>'
          + (d.why ? '<p class="ham-plan-why"><span class="ham-plan-label">Why</span> ' + esc(d.why) + '</p>' : '')
          + (d.next.length ? '<div class="ham-plan-label">Then</div><ol class="ham-plan-next">' + d.next.map(n => '<li>' + esc(n) + '</li>').join('') + '</ol>' : '')
          + '</div>';
      }).join('');
      const details = []
        .concat(view.finding ? ['<p class="ham-plan-finding">' + esc(view.finding) + '</p>'] : [])
        .concat(view.ruledOut.length ? ['<div class="ham-plan-label">Why not the others</div><ul class="ham-plan-ruled-out">' + view.ruledOut.map(r => '<li>' + esc(r) + '</li>').join('') + '</ul>'] : [])
        .concat(['<p class="ham-plan-rails">' + esc(view.rails) + '</p>']);
      return '<div id="hive-dialog-title" class="hive-dialog-title">' + esc(view.title) + '</div>'
        + '<div class="ham-plan-subject">' + esc(view.subject) + (view.finding ? ' <span class="ham-plan-chip" title="' + esc(view.finding) + '">⚠ Sentinel</span>' : '') + '</div>'
        + '<div class="hive-dialog-body ham-plan-body">' + blocks + '</div>'
        + '<details class="ham-plan-details"><summary>Details</summary>' + details.join('') + '</details>'
        + '<div class="hive-dialog-buttons">'
        + '<button type="button" class="hv-btn btn-secondary hive-dialog-btn cancel">Cancel</button>'
        + (view.showMerge ? '<button type="button" class="hv-btn btn-secondary hive-dialog-btn ham-plan-merge"' + (view.mergeable ? '' : ' aria-disabled="true" disabled title="' + esc(view.mergeWhy) + '"') + '>' + esc(view.mergeLabel || STAGE_TRACKER_PLAN_BUTTON_MERGE) + '</button>' : '')
        + '<button type="button" class="hv-btn ' + (view.danger ? 'btn-danger' : 'btn-primary') + ' hive-dialog-btn primary" data-initial-focus>' + esc(STAGE_TRACKER_PLAN_BUTTON_DELEGATE) + '</button>'
        + '</div>';
    }
    // Pure: the preview's visible words — everything outside Details and
    // the buttons. Kept for the three-second budget test.
    function stageTrackerDelegatePlanVisibleText(html) {
      return String(html).replace(/<details[\s\S]*?<\/details>/g, ' ').replace(/<div class="hive-dialog-buttons">[\s\S]*$/, ' ').replace(/<[^>]+>/g, ' ').replace(/&[a-z]+;/g, ' ').replace(/\s+/g, ' ').trim();
    }
    // Pure: the merge alternative every plan shares — the lead queue-merge
    // ("Approve and let hive merge") when each plan lists it, else merge-now
    // ("Merge now as maintainer", the one left when the hive would approve
    // anyway); '' when the plans disagree or list neither.
    function stageTrackerDelegatePlanMergeAction(plans) {
      for (const action of STAGE_TRACKER_PLAN_MERGE_ACTIONS) {
        if ((plans || []).length > 0 && plans.every(pl => (pl.alternatives || []).some(a => a.action === action))) return action;
      }
      return '';
    }
    // Pure: does the dialog offer a merge button at all — only when every
    // plan says the head is mergeable as it is (the ladder's
    // mergeable-as-is rung, or green and conflict-free without a ladder)
    // and they share a merge alternative.
    function stageTrackerDelegatePlanOffersMerge(plans) {
      return (plans || []).length > 0 && plans.every(pl => pl.mergeable_as_is === true) && stageTrackerDelegatePlanMergeAction(plans) !== '';
    }
    // Pure: the merge button's label — the shared alternative's own label.
    function stageTrackerDelegatePlanMergeLabel(plans) {
      const action = stageTrackerDelegatePlanMergeAction(plans);
      for (const pl of plans || []) for (const a of pl.alternatives || []) if (a.action === action && a.label) return a.label;
      return (action && STAGE_TRACKER_ACTION_LABELS[action]) || STAGE_TRACKER_PLAN_BUTTON_MERGE;
    }
    // Pure: can the merge button run — offered, and every plan lists the
    // shared merge alternative as enabled (merge rights).
    function stageTrackerDelegatePlanMergeable(plans) {
      const action = stageTrackerDelegatePlanMergeAction(plans);
      return stageTrackerDelegatePlanOffersMerge(plans) && plans.every(pl => (pl.alternatives || []).some(a => a.action === action && a.enabled !== false));
    }
    function stageTrackerDelegatePlanMergeReason(plans) {
      const action = stageTrackerDelegatePlanMergeAction(plans);
      for (const pl of plans || []) for (const a of pl.alternatives || []) if (a.action === action && a.enabled === false && a.reason_if_disabled) return a.reason_if_disabled;
      return '';
    }
    // The plan gathers the PR live from GitHub (seconds). Revealing a
    // row's decision options (the decision popover, the ⋯ menu, hovering or
    // focusing its Let hive decide button) prefetches it; the click takes
    // the prefetched answer when it is younger than the server's plan cache
    // TTL (api_repo_item_decide_delegated_plan.go), else asks again — which
    // the server's cache then answers at once (hivecommons/hive#15773).
    const STAGE_TRACKER_PLAN_PREFETCH_MS = 30000;
    const _stageTrackerPlanPrefetch = new Map();
    function stageTrackerPlanRequestKey(req) { return req.url + '/plan ' + JSON.stringify(req.body); }
    async function stageTrackerPostDelegatePlan(item, req, prefetch) {
      try {
        const headers = { 'Content-Type': 'application/json' };
        if (prefetch) headers['X-Hive-Prefetch'] = '1';
        const r = await fetch(req.url + '/plan', { method: 'POST', headers, body: JSON.stringify(req.body) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) return { ref: stageTrackerRefOf(item), number: item.number, error: data.error || ('HTTP ' + r.status) };
        return Object.assign({ ref: stageTrackerRefOf(item) + (typeof stageTrackerLineageClause === 'function' ? stageTrackerLineageClause(item) : '') }, data);
      } catch (e) { return { ref: stageTrackerRefOf(item), number: item.number, error: String(e && e.message || e) }; }
    }
    // Fire-and-forget: one plan request per Let hive decide target of the
    // row, at most once per STAGE_TRACKER_PLAN_PREFETCH_MS. Returns how many
    // requests it started.
    function stageTrackerPrefetchDelegatePlan(item, nowMs) {
      if (!item) return 0;
      const now = typeof nowMs === 'number' ? nowMs : Date.now();
      for (const [k, v] of _stageTrackerPlanPrefetch) if (now - v.at >= STAGE_TRACKER_PLAN_PREFETCH_MS) _stageTrackerPlanPrefetch.delete(k);
      let n = 0;
      for (const t of stageTrackerDelegateTargets(item)) {
        const req = stageTrackerActionRequest(t, 'decide:delegate', {});
        if (!req) continue;
        const k = stageTrackerPlanRequestKey(req);
        if (_stageTrackerPlanPrefetch.has(k)) continue;
        _stageTrackerPlanPrefetch.set(k, { at: now, promise: stageTrackerPostDelegatePlan(t, req, true) });
        n++;
      }
      return n;
    }
    async function stageTrackerFetchDelegatePlan(item) {
      const req = stageTrackerActionRequest(item, 'decide:delegate', {});
      if (!req) return { ref: stageTrackerRefOf(item), number: item.number, error: 'no route' };
      // A prefetched answer serves one click; Retry always asks again.
      const k = stageTrackerPlanRequestKey(req);
      const hit = _stageTrackerPlanPrefetch.get(k);
      _stageTrackerPlanPrefetch.delete(k);
      if (hit && Date.now() - hit.at < STAGE_TRACKER_PLAN_PREFETCH_MS) {
        const pl = await hit.promise;
        if (pl && !pl.error) return pl;
      }
      return stageTrackerPostDelegatePlan(item, req, false);
    }
    // The preview opens at once as a skeleton (hivecommons/hive#15773):
    // the title, the subject the row already knows, "Asking the hive…" and
    // placeholder lines; Cancel works, Let hive do that waits for the plan.
    // The skeleton lines do not move under prefers-reduced-motion
    // (tracker.css). A failed plan reads inline with Retry, never a toast
    // over a dialog that already opened.
    const STAGE_TRACKER_PLAN_SKELETON_LINES = 3;
    const STAGE_TRACKER_PLAN_RETRY = 'Retry';
    const STAGE_TRACKER_PLAN_BUTTONS_OPEN = '<div class="hive-dialog-buttons">';
    // Pure: the title + subject + "Asking the hive…" skeleton body.
    function stageTrackerDelegatePlanSkeletonContent(subject, sentinel, caption) {
      const esc = hiveDialogEscape;
      const lines = Array.from({ length: STAGE_TRACKER_PLAN_SKELETON_LINES }, () => '<span class="ham-plan-skeleton-line" aria-hidden="true"></span>').join('');
      return '<div id="hive-dialog-title" class="hive-dialog-title">' + esc(STAGE_TRACKER_PLAN_TITLE) + '</div>'
        + '<div class="ham-plan-subject">' + esc(subject) + (sentinel ? ' <span class="ham-plan-chip" title="' + esc(sentinel) + '">⚠ Sentinel</span>' : '') + '</div>'
        + '<div class="hive-dialog-body ham-plan-body ham-plan-skeleton" role="status" aria-live="polite">'
        + '<div class="ham-plan-eyebrow">' + esc(STAGE_TRACKER_PLAN_EYEBROW) + '</div>'
        + '<p class="ham-plan-loading"><span class="toast-spinner cue-acting-spin" aria-hidden="true"></span> ' + esc(caption || 'Asking the hive…') + '</p>'
        + lines + '</div>';
    }
    // Pure: the whole skeleton dialog — the content plus the buttons the
    // filled preview keeps: Cancel (focused, enabled), the merge button
    // (hidden until a plan offers it) and Let hive do that (disabled).
    function stageTrackerDelegatePlanSkeletonHtml(subject, sentinel, caption) {
      const esc = hiveDialogEscape;
      return '<div class="ham-plan-content">' + stageTrackerDelegatePlanSkeletonContent(subject, sentinel, caption) + '</div>'
        + STAGE_TRACKER_PLAN_BUTTONS_OPEN
        + '<button type="button" class="hv-btn btn-secondary hive-dialog-btn cancel" data-initial-focus>Cancel</button>'
        + '<button type="button" class="hv-btn btn-secondary hive-dialog-btn ham-plan-merge" hidden>' + esc(STAGE_TRACKER_PLAN_BUTTON_MERGE) + '</button>'
        + '<button type="button" class="hv-btn ' + (sentinel ? 'btn-danger' : 'btn-primary') + ' hive-dialog-btn primary" disabled aria-disabled="true">' + esc(STAGE_TRACKER_PLAN_BUTTON_DELEGATE) + '</button>'
        + '</div>';
    }
    // Pure: the inline error that replaces the skeleton, with Retry.
    function stageTrackerDelegatePlanErrorContent(subject, failed) {
      const esc = hiveDialogEscape;
      const what = (failed || []).map(pl => (pl.ref || ('#' + pl.number)) + ' (' + pl.error + ')').join(', ');
      return '<div id="hive-dialog-title" class="hive-dialog-title">' + esc(STAGE_TRACKER_PLAN_TITLE) + '</div>'
        + '<div class="ham-plan-subject">' + esc(subject) + '</div>'
        + '<div class="hive-dialog-body ham-plan-body">'
        + '<p class="ham-plan-error" role="alert">Could not preview ' + esc(what) + '</p>'
        + '<button type="button" class="hv-btn btn-secondary hive-dialog-btn ham-plan-retry">' + esc(STAGE_TRACKER_PLAN_RETRY) + '</button>'
        + '</div>';
    }
    // Pure: the subject line the skeleton shows before the plan names it.
    function stageTrackerDelegatePlanSubject(items) {
      items = items || [];
      if (items.length !== 1) return items.length + ' items';
      return stageTrackerRefOf(items[0]) + (items[0].title ? ' · ' + stageTrackerPlanTruncate(items[0].title, STAGE_TRACKER_PLAN_TITLE_MAX) : '');
    }
    // The preview: resolves { chosen } for Let hive do that, { chosen,
    // merge: <action id> } for the merge button, null on Cancel. load()
    // resolves the plans (one per item); it runs again on Retry. opts:
    // { subject, caption, onSettled } — onSettled runs each time a load ends.
    function stageTrackerDelegatePlanDialog(load, sentinel, opts) {
      opts = opts || {};
      return new Promise(resolve => {
        const overlay = document.createElement('div');
        overlay.className = 'hive-dialog-overlay';
        hiveRememberModalOpen(overlay);
        overlay.innerHTML = '<div class="hive-dialog ham-plan-dialog" role="dialog" aria-modal="true" aria-labelledby="hive-dialog-title" aria-busy="true">' + stageTrackerDelegatePlanSkeletonHtml(opts.subject || '', sentinel, opts.caption) + '</div>';
        document.body.appendChild(overlay);
        const dlg = overlay.querySelector('.ham-plan-dialog');
        const content = overlay.querySelector('.ham-plan-content');
        const primary = overlay.querySelector('.primary');
        const merge = overlay.querySelector('.ham-plan-merge');
        let view = null, would = '', loading = false;
        const close = hiveWireDialog(overlay, resolve, null, '.primary', '.cancel', done => {
          // Enter: Retry or Cancel when focused; Let hive do that only once the plan is in.
          const at = document.activeElement;
          if (at && at.classList && at.classList.contains('ham-plan-retry')) { run(); return; }
          if (at && at.classList && at.classList.contains('cancel')) { done(null); return; }
          if (view) done({ chosen: STAGE_TRACKER_PLAN_BUTTON_DELEGATE + ' · previewed ' + would });
        });
        if (merge) merge.addEventListener('click', () => { if (view && view.mergeable) close({ chosen: view.mergeLabel + ' · previewed ' + would, merge: view.mergeAction }); });
        content.addEventListener('click', e => { if (e.target.closest && e.target.closest('.ham-plan-retry')) { e.preventDefault(); run(); } });
        async function run() {
          if (loading || view) return;
          loading = true;
          dlg.setAttribute('aria-busy', 'true');
          content.innerHTML = stageTrackerDelegatePlanSkeletonContent(opts.subject || '', sentinel, opts.caption);
          let plans;
          try { plans = await load(); } catch (e) { plans = [{ ref: opts.subject || '', error: String(e && e.message || e) }]; }
          loading = false;
          if (typeof opts.onSettled === 'function') opts.onSettled();
          if (!overlay.parentNode) return;
          dlg.removeAttribute('aria-busy');
          const failed = (plans || []).filter(pl => !pl || pl.error);
          if (failed.length || !plans || !plans.length) {
            content.innerHTML = stageTrackerDelegatePlanErrorContent(opts.subject || '', failed.length ? failed : [{ ref: opts.subject || '', error: 'no plan' }]);
            const retry = content.querySelector('.ham-plan-retry');
            if (retry) retry.focus();
            return;
          }
          view = stageTrackerDelegatePlanView(plans, sentinel);
          would = plans.map(pl => pl.would || '?').filter((v, i, a) => a.indexOf(v) === i).join('/');
          const html = stageTrackerDelegatePlanHtml(view);
          const cut = html.indexOf(STAGE_TRACKER_PLAN_BUTTONS_OPEN);
          content.innerHTML = cut >= 0 ? html.slice(0, cut) : html;
          if (merge && view.showMerge) {
            merge.hidden = false;
            merge.textContent = view.mergeLabel || STAGE_TRACKER_PLAN_BUTTON_MERGE;
            if (!view.mergeable) { merge.disabled = true; merge.setAttribute('aria-disabled', 'true'); merge.setAttribute('title', view.mergeWhy); }
          }
          primary.classList.toggle('btn-danger', !!view.danger);
          primary.classList.toggle('btn-primary', !view.danger);
          primary.disabled = false;
          primary.removeAttribute('aria-disabled');
          primary.focus();
        }
        run();
      });
    }
    // Let hive decide → plan → preview → act. Returns the delegate payload,
    // or null when cancelled or when the operator took the merge
    // alternative (which runs as its own row action with the chosen button
    // recorded on the ledger).
    // The items whose preview is open or loading: a second click (the
    // button, the ⋯ entry, the popover) while one is up never opens another.
    const _stageTrackerDelegatePreviewing = new Set();
    async function stageTrackerDelegatePreview(items, rows) {
      const keys = items.map(stageTrackerItemKey);
      if (keys.some(k => _stageTrackerDelegatePreviewing.has(k))) return null;
      keys.forEach(k => _stageTrackerDelegatePreviewing.add(k));
      // The plan gathers the PR live (seconds): the row caption and its Let
      // hive decide button both read the preview's own caption ("Asking the
      // hive…", one phrase from STAGE_TRACKER_ACTING_CAPTIONS, distinct from
      // the confirmed action's "Hive deciding…") until the plan lands in the
      // skeleton preview, which opens at once. The pend is tagged with the
      // button it busies: a confirmed action's pend (or any other ⋯ action
      // on the row) leaves it alone.
      const caption = stageTrackerActingCaption('decide:delegate:preview');
      const pend = stageTrackerSetPending([].concat(...items.map(it => stageTrackerPendingKeys(it, rows))).filter((k, i, a) => a.indexOf(k) === i), caption, undefined, 'decide:delegate');
      let picked;
      try {
        picked = await stageTrackerDelegatePlanDialog(() => Promise.all(items.map(stageTrackerFetchDelegatePlan)), stageTrackerSentinelPreface(items), { subject: stageTrackerDelegatePlanSubject(items), caption, onSettled: () => stageTrackerClearPending(pend) });
      } finally {
        stageTrackerClearPending(pend);
        keys.forEach(k => _stageTrackerDelegatePreviewing.delete(k));
      }
      if (!picked) return null;
      if (picked.merge) {
        await stageTrackerRunActions(items, picked.merge, { comment: '', chosen: picked.chosen }, items);
        return null;
      }
      return { chosen: picked.chosen };
    }
    async function stageTrackerActionPayload(items, id, rows) {
      const refs = items.map(stageTrackerRefOf).join(', ');
      const who = window._hiveUser || 'you';
      const prs = items.length > 0 && items.every(it => it.kind === 'pr');
      const sentinel = id.indexOf('decide:') === 0 ? stageTrackerSentinelPreface(items) : '';
      const preface = sentinel ? sentinel + '\n\n' : '';
      switch (id) {
        case 'decide:delegate':
          // Preview first (hivecommons/hive#15126): nothing is written
          // until the operator picks a button.
          return stageTrackerDelegatePreview(items, rows);
        case 'decide:delegate-all': {
          // Confirms as Let hive decide (same opt-out key), naming each PR.
          const prs = [].concat(...items.map(stageTrackerNestedOpenPRs));
          return (await stageTrackerAsk('decide:delegate', 'confirm', stageTrackerDelegateConfirm(prs))) ? {} : null;
        }
        case 'decide:delegate:rerun':
          // Offered only from a "hasn't taken effect" toast the operator
          // has already read; no second confirmation.
          return {};
        case 'blocker:recheck':
          return {};
        case 'blocker:close-partial':
          return (await stageTrackerAsk(id, 'confirm', { title: 'Close as partially addressed', message: 'Label ' + refs + ' hive/partially-addressed and close ' + (items.length > 1 ? 'them' : 'it') + ' as ' + who + '? The remaining work stays described in the closing comment; nothing is merged.', confirmLabel: 'Close as partial', danger: true })) ? {} : null;
        case 'decide:approve':
          if (prs) {
            return (await stageTrackerAsk(id, 'confirm', { title: 'Approve pull request' + (items.length > 1 ? 's' : ''), message: preface + 'Post "/lgtm" and "/approve" on ' + refs + ' as ' + who + '? The merge queue takes it from there — the dashboard does not merge.', confirmLabel: 'Post /lgtm + /approve', danger: !!sentinel })) ? {} : null;
          }
          return (await stageTrackerAsk(id, 'confirm', { title: 'Approve direction', message: 'Record "/hive approve" on ' + refs + ' as ' + who + '? The hive un-parks the issue' + (items.length > 1 ? 's' : '') + ' and carries on; nothing is merged.', confirmLabel: 'Approve' })) ? {} : null;
        case 'decide:direction': {
          // The direction is required, so this prompt is never skipped.
          const v = await hivePrompt(prs
            ? { title: 'Give direction', label: preface + 'What should the hive change on ' + refs + '? (recorded as "/hive decision …"; ' + stageTrackerDirectionCarrier(items) + ' pushes a new head)', placeholder: 'e.g. drop the retry loop and keep the flag', validate: s => s ? true : 'A direction is required', confirmLabel: 'Send' }
            : { title: 'Send back to the hive', label: 'Direction for the hive on ' + refs + ' (recorded as "/hive decision …")', placeholder: 'e.g. take option B and keep the API as is', validate: s => s ? true : 'A direction is required', confirmLabel: 'Send' });
          return v == null ? null : { comment: v };
        }
        case 'decide:close-superseded': {
          const options = stageTrackerSupersederOptions(_stageTracker, items);
          const sibling = stageTrackerSupersederDefault(items);
          const by = await hivePrompt({ title: 'Close as superseded by', label: 'Which pull request supersedes ' + refs + '? (#number, owner/repo#number or URL; leave empty if none)', placeholder: options.length ? options[0].value : '#123', defaultValue: sibling, options, validate: s => stageTrackerSupersederValid(s, items) });
          if (by == null) return null;
          const v = await stageTrackerAsk(id, 'prompt', { title: 'Close as superseded', label: 'Comment for ' + refs + ' (optional)', placeholder: 'Why this is closed', confirmLabel: 'Close' });
          return v == null ? null : { comment: v, supersededBy: by };
        }
        case 'decide:close-wontfix': {
          const v = await stageTrackerAsk(id, 'prompt', { title: stageTrackerActionLabel(id), label: 'Comment for ' + refs + ' (optional)', placeholder: 'Why this is closed', confirmLabel: 'Close' });
          return v == null ? null : { comment: v };
        }
        case 'decide:close-read':
          return (await stageTrackerAsk(id, 'confirm', { title: 'Close advisory', message: stageTrackerAdvisoryCloseMessage(items, refs, who), confirmLabel: 'Close' })) ? {} : null;
        case 'decide:merge-now': {
          const base = items.map(it => it.base_ref || '').filter(Boolean)[0] || 'its base branch';
          if (items.some(stageTrackerNeedsRebase)) {
            return (await stageTrackerAsk(id, 'confirm', { title: STAGE_TRACKER_REBASE_MERGE_LABEL, message: preface + refs + ' is behind ' + base + ' (needs a rebase), so GitHub would refuse a plain merge. Record your approval as ' + who + ' and have the hive rebase it onto ' + base + ', wait for CI, then merge it with your approval on record? You get a toast when it lands or if the rebase conflicts.' + stageTrackerDCOCaveat(items), confirmLabel: 'Approve & rebase-merge', danger: true })) ? {} : null;
          }
          return (await stageTrackerAsk(id, 'confirm', { title: 'Merge now as maintainer', message: preface + 'Merge ' + refs + ' into ' + base + ' now, credited to ' + who + '? Hive left this merge to a person; this is you taking that call yourself instead of letting hive merge on its next tick. The version you see is the one that merges.' + stageTrackerDCOCaveat(items), confirmLabel: 'Merge now', danger: true })) ? {} : null;
        }
        case 'decide:queue-merge':
          return (await stageTrackerAsk(id, 'confirm', { title: 'Approve and let hive merge', message: preface + 'Approve ' + refs + ' as ' + who + '? Your approval stands in for the author\'s trust on this version: hive merges it as-is through its own App on the next tick once CI is green, and will not adopt or re-author it.' + stageTrackerDCOCaveat(items), confirmLabel: 'Approve' })) ? {} : null;
        case 'decide:re-author': {
          const caveat = stageTrackerDCOCaveat(items);
          return (await stageTrackerAsk(id, 'confirm', { title: 'Re-author as hive PR', message: preface + 'Have the hive re-author ' + refs + ' as ' + who + '?' + (caveat || ' Its head cannot merge as-is.') + ' The hive re-homes the commits on a hive-owned, signed-off branch and opens a superseding PR; this one is closed when that merges. Nothing is merged as-is, and the author\'s commits stay attributed.', confirmLabel: 'Re-author', danger: true })) ? {} : null;
        }
        case 'decide:adopt': {
          const why = items.map(it => stageTrackerAdoptionTip(it)).filter(Boolean).join('\n');
          return (await hiveConfirm({ title: 'Adopt pull request' + (items.length > 1 ? 's' : ''), message: (why ? why + '\n\n' : '') + 'Have the hive adopt ' + refs + ' as ' + who + '? The author\'s commits and sign-off are kept; the hive rebases and fixes CI (in place when GitHub allows, else on a superseding hive PR that closes this one when it merges). Recorded as your decision on the PR.', confirmLabel: 'Adopt' })) ? {} : null;
        }
        case 'needs-human': {
          const v = await stageTrackerAsk(id, 'prompt', { title: 'Mark needs-human', label: 'Why does ' + refs + ' need a human? (optional)', placeholder: 'Reason', confirmLabel: 'Mark' });
          return v == null ? null : { comment: v };
        }
        case 'decide:adopt-now': {
          const online = items.some(it => { const tk = stageTrackerTakeoverOf(it); return tk && tk.author_online; });
          return (await stageTrackerAsk(id, 'confirm', stageTrackerAdoptNowConfirm(refs, who, online, preface))) ? {} : null;
        }
        case 'decide:reroute': {
          // No target yet (the Needs-human popover's option): ask for one —
          // required, so this prompt is never skipped — then confirm.
          const tk = stageTrackerTakeoverOf(items[0]) || {};
          const options = stageTrackerRerouteOptions(tk, true, '').map(o => ({ value: stageTrackerRerouteTargetOf(o.id), label: o.label }));
          const to = await hivePrompt({ title: stageTrackerActionLabel(id), label: 'Which clanker takes ' + refs + ' over? (a login from the list, or "any")', placeholder: STAGE_TRACKER_REROUTE_ANY, defaultValue: STAGE_TRACKER_REROUTE_ANY, options, validate: v => stageTrackerRerouteTargetValid(v, tk), confirmLabel: 'Next' });
          if (to == null) return null;
          const target = String(typeof to === 'object' ? to.value : to).replace(/^@/, '').trim() || STAGE_TRACKER_REROUTE_ANY;
          return (await stageTrackerAsk(id, 'confirm', stageTrackerRerouteConfirm(refs, who, target, tk))) ? { to: target } : null;
        }
      }
      if (stageTrackerActionBase(id) === 'decide:reroute') {
        const target = stageTrackerRerouteTargetOf(id) || STAGE_TRACKER_REROUTE_ANY;
        return (await stageTrackerAsk('decide:reroute', 'confirm', stageTrackerRerouteConfirm(refs, who, target, stageTrackerTakeoverOf(items[0])))) ? { to: target } : null;
      }
      return {};
    }
    // Pure: the Adopt now confirm — the consequence line is the message.
    function stageTrackerAdoptNowConfirm(refs, who, authorOnline, preface) {
      const tail = authorOnline ? ' The author\'s clanker is online: the adoption comment tells it, politely, that the maintainer took the PR over.' : '';
      return { title: 'Adopt now', message: (preface || '') + 'Have the hive adopt ' + refs + ' now as ' + who + ', without waiting for the offline grace window or the maintainer policy? This ' + STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE + '.' + tail, confirmLabel: 'Adopt now', danger: true };
    }
    function stageTrackerRerouteConfirm(refs, who, target, tk) {
      const any = !target || target === STAGE_TRACKER_REROUTE_ANY;
      const toWho = any ? 'any available clanker' : '@' + target + '\'s clanker';
      const author = tk && tk.author ? '@' + tk.author : 'the author';
      return { title: stageTrackerActionLabel('decide:reroute'), message: 'Hand the reviewer\'s requested changes on ' + refs + ' to ' + toWho + ' as ' + who + '? ' + author + ' is told by comment; the target fixes the PR or opens a replacement carrying ' + author + '\'s commits and sign-off. If nobody picks it up within the pickup window the hive falls back to Adopt now.', confirmLabel: 'Reroute' };
    }
    // Pure: a reroute target is "any" or a clanker the row lists as online.
    function stageTrackerRerouteTargetValid(v, tk) {
      const t = String(v || '').replace(/^@/, '').trim();
      if (!t || t === STAGE_TRACKER_REROUTE_ANY) return true;
      const list = tk && Array.isArray(tk.clankers) ? tk.clankers : [];
      return list.some(c => String(c.login || '').toLowerCase() === t.toLowerCase()) ? true : '@' + t + ' is not online now — pick a listed clanker or "any"';
    }
    /* Every menu item goes through runRowAction(action, row): it asks what
       the action needs (in-app dialogs, each skippable per action), marks
       the affected row(s) as acting, runs the requests, and renders one
       persistent result toast per item from the server's shared `result`
       {action, target, outcome, next, rule?, applied_at?} — or from the
       legacy fields when an older hive answers without one. The toast and
       the acting state both leave when the tracker reports the target in a
       new state (stage, slug or caption), or when the result is a no-op. */
    const STAGE_TRACKER_ACTION_NOOP = 'noop-already-applied';
    // A recorded decision whose expected effect never showed (#14822): the
    // toast says why in plain words and carries the next actions.
    const STAGE_TRACKER_ACTION_NO_EFFECT = 'no-effect';
    const STAGE_TRACKER_RESULT_TOAST_MAX = 5;
    // Tracker toast lifetimes (hivecommons/hive#15813). An informational
    // result toast ("adopted", "/lgtm posted", a no-op) and the waiting-on-you
    // nudge close after the info duration; an
    // error result closes after the error duration (it keeps its × for an
    // earlier dismissal); a toast carrying next-action buttons is a prompt
    // and waits for the operator. Both durations are the hive's
    // dashboard.stage_tracker.toast_info_s / toast_error_s, read from the
    // tracker payload's settings block; this browser's localStorage copy
    // is only the fallback for when the server write was refused.
    const STAGE_TRACKER_TOAST_DEFAULT_S = Object.freeze({ info: 15, error: 30 });
    const STAGE_TRACKER_TOAST_MIN_S = 3;
    const STAGE_TRACKER_TOAST_MAX_S = 120;
    const STAGE_TRACKER_TOAST_KEY = 'hive.tracker.toastS';
    // The merge banner stays this many times longer than an info toast
    // (30 s at the default), so the two move together.
    const STAGE_TRACKER_MERGE_BANNER_FACTOR = 2;
    // Pure: a toast duration in seconds, or 0 when the value is not a
    // usable number (0 itself means "the default").
    function stageTrackerToastSecondsValid(v) {
      const n = Number(v);
      if (!isFinite(n) || n <= 0) return 0;
      return Math.max(STAGE_TRACKER_TOAST_MIN_S, Math.min(Math.round(n), STAGE_TRACKER_TOAST_MAX_S));
    }
    // Pure: the effective duration in seconds for kind ('info' | 'error').
    // This browser's fallback copy wins when present — it only exists when
    // the hive refused the write, so the payload still carries the old
    // value — then the server's settings block, then the default.
    function stageTrackerToastSeconds(kind, settings, local) {
      kind = kind === 'error' ? 'error' : 'info';
      const fromLocal = stageTrackerToastSecondsValid(local && local[kind]);
      if (fromLocal) return fromLocal;
      const fromServer = stageTrackerToastSecondsValid(settings && settings['toast_' + kind + '_s']);
      if (fromServer) return fromServer;
      return STAGE_TRACKER_TOAST_DEFAULT_S[kind];
    }
    function stageTrackerToastLocal() {
      try { const v = JSON.parse(localStorage.getItem(STAGE_TRACKER_TOAST_KEY) || 'null'); return v && typeof v === 'object' ? v : null; } catch { return null; }
    }
    function stageTrackerToastMs(kind) {
      return stageTrackerToastSeconds(kind, _stageTracker && _stageTracker.settings, stageTrackerToastLocal()) * 1000;
    }
    function stageTrackerMergeBannerMs() { return stageTrackerToastMs('info') * STAGE_TRACKER_MERGE_BANNER_FACTOR; }
    const STAGE_TRACKER_ACTING_MAX_MS = 180000;
    const STAGE_TRACKER_ACTING_CAPTIONS = {
      'hold': 'Holding…', 'release': 'Releasing hold…', 'retry': 'Re-evaluating…', 'needs-human': 'Marking needs-human…', 'accept-triage': 'Accepting triage…',
      'blocker:recheck': 'Checking App permissions…', 'blocker:close-partial': 'Closing as partial…',
      'decide:delegate': 'Hive deciding…', 'decide:delegate:preview': 'Asking the hive…', 'decide:delegate-all': 'Hive deciding each PR…', 'decide:delegate:rerun': 'Hive deciding again…', 'decide:approve': 'Approving…', 'decide:direction': 'Recording direction…', 'decide:close-superseded': 'Closing…', 'decide:close-wontfix': 'Closing…', 'decide:close-read': 'Closing…',
      'decide:merge-now': 'Merging…', 'decide:queue-merge': 'Recording approval…', 'decide:re-author': 'Queuing re-authoring…',
      'decide:adopt': 'Adopting…', 'decide:adopt-now': 'Adopting now…', 'decide:reroute': 'Rerouting…',
    };
    const STAGE_TRACKER_ACTION_WORDS = {
      adopted: 'adopted', approved: 'approved', recommended: 'recommendation only', closed: 'closed', directed: 'direction recorded', refused: 'refused', 'decided-each': 'decided per PR',
      merged: 'merged', 'queued-for-merge': 'approved — hive will merge on the next tick once CI is green', [STAGE_TRACKER_ACTION_REBASE_QUEUED]: 'rebase queued, merge to follow',
      held: 'held', released: 'hold released', rescanned: 'rescanned', 'triage-accepted': 'triage accepted', 'needs-human': 'marked needs-human', rerouted: 'rerouted', [STAGE_TRACKER_ACTION_NOOP]: 'no change',
      [STAGE_TRACKER_ACTION_NO_EFFECT]: 'no effect yet',
    };
    // Once the server has answered, a terminal action's caption says so
    // until the tracker drops the row ("Closed · leaving list"); the row
    // cannot come back meanwhile because the server filters an
    // operator-closed item out of its next snapshots (hivecommons/hive#15436).
    const STAGE_TRACKER_TERMINAL_CAPTIONS = { closed: 'Closed · leaving list', merged: 'Merged · leaving list' };
    function stageTrackerTerminalCaption(result) {
      return STAGE_TRACKER_TERMINAL_CAPTIONS[result && result.action] || '';
    }
    // The ledger name of a row action id (the inverse of the badge's
    // STAGE_TRACKER_ACTED_OPTION_ID), for a failure noted on the client.
    const STAGE_TRACKER_ACTION_LEDGER_NAME = { 'decide:delegate': 'let-hive-decide', 'decide:approve': 'approve', 'decide:direction': 'give-direction', 'decide:close-superseded': 'close-superseded', 'decide:close-wontfix': 'wontfix', 'decide:close-read': 'close-read', 'accept-triage': 'accept-triage', hold: 'hold', release: 'release', 'needs-human': 'mark-needs-human', 'decide:merge-now': 'merge-now', 'decide:queue-merge': 'queue-merge', 'decide:re-author': 're-author', 'decide:adopt': 'adopt', 'decide:adopt-now': 'adopt-now', 'decide:reroute': 'reroute' };
    // A failed or refused action is noted on the row at once, so the
    // history badge turns red and its hover's first line names the reason
    // — the server's ledger only sees 5xx and refused answers, and a
    // network error never reaches it (hivecommons/hive#15436). Keyed by
    // item; the next action on the row, or a newer server record, replaces it.
    var _stageTrackerActionFailures = {};
    function stageTrackerNoteActionFailure(item, id, why, now) {
      const key = stageTrackerItemKey(item);
      const at = new Date(typeof now === 'number' ? now : Date.now()).toISOString();
      _stageTrackerActionFailures[key] = { action: STAGE_TRACKER_ACTION_LEDGER_NAME[stageTrackerActionBase(id)] || stageTrackerActionBase(id), actor: (typeof window !== 'undefined' && window._hiveUser) || '', at, outcome: 'failed', detail: why, progress: why, state: 'failed' };
      return _stageTrackerActionFailures[key];
    }
    function stageTrackerClearActionFailure(item) { delete _stageTrackerActionFailures[stageTrackerItemKey(item)]; }
    // Pure: the client-noted failure for an item when it is newer than the
    // server's record (or there is none); null otherwise.
    function stageTrackerActionFailureOf(item) {
      const f = item ? _stageTrackerActionFailures[stageTrackerItemKey(item)] : null;
      if (!f) return null;
      const srv = item.last_operator_action;
      if (srv && (Date.parse(srv.at || '') || 0) >= (Date.parse(f.at) || 0)) return null;
      return f;
    }
    function stageTrackerActingCaption(id, item) {
      if (id === 'decide:merge-now' && item && stageTrackerNeedsRebase(item)) return 'Rebasing, then merging…';
      return STAGE_TRACKER_ACTING_CAPTIONS[stageTrackerActionBase(id)] || 'acting…';
    }
    // Pure: the state signature a toast / acting row waits to see change.
    function stageTrackerItemState(it) {
      if (!it) return '';
      return String(Number(it.stage) || 0) + '|' + (it.stage_slug || '') + '|' + (it.caption || '') + '|' + (Array.isArray(it.modifiers) ? it.modifiers.slice().sort().join(',') : '') + (it.rebase_merge && it.rebase_merge.state ? '|rm:' + it.rebase_merge.state : '') + (it.rebase && it.rebase.text ? '|rb:' + it.rebase.text : '');
    }
    function stageTrackerTargetKey(target) {
      return target && target.number ? stageTrackerItemKey({ repo: target.repo, kind: target.kind === 'pr' ? 'pr' : 'issue', number: target.number }) : '';
    }
    // Pure: the server's result, or one assembled from the legacy fields
    // of an older hive's answer so the toast still says what happened.
    function stageTrackerResultOf(id, item, data) {
      data = data || {};
      if (data.result && typeof data.result === 'object') {
        const r = Object.assign({}, data.result);
        if (!r.target || !r.target.number) r.target = { repo: item.repo, kind: item.kind, number: item.number, url: item.url || '' };
        return r;
      }
      const target = { repo: item.repo, kind: item.kind, number: item.number, url: item.url || '' };
      if (data.status === 'debounced' || data.status === 'in-progress') return { action: STAGE_TRACKER_ACTION_NOOP, target, outcome: 'No change — a rescan ' + (data.status === 'debounced' ? 'finished moments ago' : 'is already running') + '.', next: '' };
      if (data.alreadyAccepted) return { action: STAGE_TRACKER_ACTION_NOOP, target, outcome: 'No change — triage was already accepted.', next: '' };
      const d = data.decision;
      if (d && typeof d === 'object') return { action: d.verdict || 'recommended', rule: d.rule || '', target, outcome: stageTrackerPlainWords(d.verdict || '?', false, item.kind) + (d.rule ? ' (' + stageTrackerPlainWords(d.rule, false, item.kind) + ')' : '') + (d.keeps_park ? ' — kept for the owner' : ''), next: '' };
      return { action: id, target, outcome: stageTrackerActionLabel(id) + ' done.', next: '' };
    }
    function stageTrackerErrorResult(id, item, error) {
      return { action: 'error', target: { repo: item.repo, kind: item.kind, number: item.number, url: item.url || '' }, outcome: error, next: '' };
    }
    // Pure: the toast's lines. "Hive decided on org/r#14938 → PR #14947:
    // adopted (contributor offline) — <outcome>" / "What next".
    function stageTrackerResultText(result, item, ctx) {
      ctx = ctx || {};
      const t = result.target || {};
      const rowRef = ctx.rowRef || stageTrackerRefOf(item);
      const tKey = stageTrackerTargetKey(t);
      const toward = tKey && tKey !== stageTrackerItemKey(item) ? ' → ' + (t.kind === 'pr' ? 'PR' : 'issue') + ' #' + t.number : '';
      const noop = result.action === STAGE_TRACKER_ACTION_NOOP;
      const noEffect = result.action === STAGE_TRACKER_ACTION_NO_EFFECT;
      const error = result.action === 'error';
      let head;
      const hiveDecided = !error && (result.rule || result.action === 'recommended' || (result.action === 'adopted' && ctx.id !== 'decide:adopt-now'));
      if (error) head = stageTrackerActionLabel(ctx.id || '') + ' failed on ' + rowRef + toward;
      else if (hiveDecided) head = 'Hive decided on ' + rowRef + toward;
      else if (result.action === STAGE_TRACKER_ACTION_REBASE_QUEUED || (ctx.id === 'decide:merge-now' && stageTrackerNeedsRebase(item))) head = STAGE_TRACKER_REBASE_MERGE_LABEL + ' on ' + rowRef + toward;
      else head = stageTrackerActionLabel(ctx.id || '') + ' on ' + rowRef + toward;
      const word = error ? '' : (STAGE_TRACKER_ACTION_WORDS[result.action] || result.action || '');
      // The rule in plain words from the glossary, never the bare slug.
      const tKind = t.kind === 'pr' || t.kind === 'issue' ? t.kind : item.kind;
      const rule = result.rule ? ' (' + stageTrackerPlainWords(result.rule, false, tKind) + ')' : '';
      if (word) head += ': ' + word + rule;
      const nowMs = ctx && typeof ctx.nowMs === 'number' ? ctx.nowMs : Date.now();
      const local = typeof stageTrackerLocalStamps === 'function' ? (x => stageTrackerLocalStamps(x, nowMs)) : (x => x);
      let outcome = local(result.outcome || '');
      // A decision's toast leads with its outcome in one line ("Adopted:
      // hive opened #15123 …"); who decided what, and under which rule, is
      // the second line (hivecommons/hive#15126).
      if (hiveDecided && !noop && !noEffect && outcome) { const lead = outcome; outcome = head; head = lead; }
      const warn = stageTrackerResultWarn(result, item);
      const posted = Array.isArray(result.posted) ? result.posted.filter(p => typeof p === 'string' && p.trim()).map(p => p.replace(/`/g, '')) : [];
      const refused = result.action === 'refused';
      // Options ride a no-effect / refused result, and an approval on a PR
      // that needs a rebase ("Rebase, then merge as maintainer").
      const options = (noEffect || refused || result.action === 'approved') && Array.isArray(result.options) ? result.options.filter(o => o && o.action && o.label) : [];
      return { head, outcome, next: local(result.next || ''), warn, posted: posted.length ? 'Posted: ' + posted.join(' · ') : '', options, why: noEffect ? stageTrackerPlainWords(result.why || 'no-effect', true, tKind) : '', type: error ? 'error' : (noEffect || refused ? 'warning' : (noop ? 'info' : 'success')), url: t.url || item.url || '', linkText: 'Open ' + (t.kind === 'pr' ? 'PR' : 'issue') + ' #' + (t.number || item.number) };
    }
    // Pure: the ⚠ sentinel paragraph a result toast carries. The row's
    // alert is what the action started from, so it is repeated only while
    // the alert is still open: a result that says it cleared the alert
    // (sentinel_cleared) drops the warning; one that says it is still on
    // (sentinel_open) keeps it, with Next saying what the person must do.
    function stageTrackerResultWarn(result, item) {
      if (result && result.sentinel_cleared) return '';
      return typeof stageTrackerSentinelPreface === 'function' ? stageTrackerSentinelPreface([item]) : '';
    }
    // Pure: RFC3339 stamps in a server sentence read as local clock time
    // ("at 11:40" today, "Oct 27, 11:40" otherwise), never raw ISO.
    const STAGE_TRACKER_RFC3339_RE = /\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})\b/g;
    function stageTrackerLocalStamps(text, nowMs) {
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      return String(text || '').replace(STAGE_TRACKER_RFC3339_RE, m => { const ms = Date.parse(m); return isNaN(ms) ? m : agentNowClockTime(ms, nowMs) + (formatDashboardDate(new Date(ms)) === formatDashboardDate(nowMs) ? ' today' : ''); });
    }
    let _stageTrackerResultToasts = [];
    // Build and show the result toast; it leaves on ×, when the tracker
    // reports the target in a new state (see stageTrackerResultToastsSync)
    // or, for a plain informational result, after the info toast duration
    // (see stageTrackerResultToastTimed); an error leaves after the error
    // toast duration.
    function stageTrackerResultToast(result, item, ctx) {
      const text = stageTrackerResultText(result, item, ctx);
      const container = document.getElementById('toast-container');
      const el = document.createElement('div');
      el.className = 'toast ' + text.type + ' ham-result-toast';
      const head = document.createElement('div'); head.className = 'ham-result-head'; head.textContent = text.head; el.appendChild(head);
      if (text.outcome) { const o = document.createElement('div'); o.className = 'ham-result-outcome'; o.textContent = text.outcome; el.appendChild(o); }
      if (text.warn) { const w = document.createElement('div'); w.className = 'ham-result-warn'; w.textContent = text.warn; el.appendChild(w); }
      if (text.next) { const n = document.createElement('div'); n.className = 'ham-result-next'; n.textContent = text.next; el.appendChild(n); }
      if (text.posted) { const p = document.createElement('div'); p.className = 'ham-result-posted'; p.textContent = text.posted; el.appendChild(p); }
      if (text.options && text.options.length) {
        // The stuck decision's next actions (#14822): each runs the row
        // action on the toast's target; "open:github" is a link.
        const acts = document.createElement('div'); acts.className = 'ham-result-options';
        const target = { repo: (result.target || {}).repo || item.repo, kind: (result.target || {}).kind || item.kind, number: (result.target || {}).number || item.number, url: text.url, reason: item.reason || '', caption: item.caption || '', sentinel_reason: item.sentinel_reason || '' };
        for (const o of text.options) {
          const b = document.createElement('button'); b.type = 'button'; b.className = 'ham-decision-act'; b.textContent = o.label;
          if (o.enabled === false) { b.setAttribute('aria-disabled', 'true'); if (o.reason_if_disabled) b.title = o.reason_if_disabled; }
          b.addEventListener('click', e => {
            e.stopPropagation();
            if (o.enabled === false) return;
            if (o.action === 'open:github') { if (text.url) window.open(text.url, '_blank', 'noopener'); return; }
            if (o.action === 'open:app-settings') { if (o.url) window.open(o.url, '_blank', 'noopener'); return; }
            el.remove();
            runRowAction(o.action, [target]);
          });
          acts.appendChild(b);
        }
        el.appendChild(acts);
      }
      if (text.url) {
        const a = document.createElement('a'); a.className = 'ham-result-link'; a.href = text.url; a.target = '_blank'; a.rel = 'noopener'; a.textContent = text.linkText + ' ↗';
        a.addEventListener('click', e => e.stopPropagation());
        el.appendChild(a);
      }
      if (container) container.appendChild(el);
      makeToastSticky(el);
      if (text.type === 'error') toastCountdownBar(el, stageTrackerToastMs('error'), () => dismissToast(el, null, null, true));
      else if (stageTrackerResultToastTimed(text)) toastCountdownBar(el, stageTrackerToastMs('info'), () => dismissToast(el, null, null, true));
      const key = stageTrackerTargetKey(result.target) || stageTrackerItemKey(item);
      const entry = { el, key, state: stageTrackerItemState(stageTrackerItemByKey(_stageTracker, key)), result, text };
      if (text.type !== 'error') _stageTrackerResultToasts.push(entry);
      return entry;
    }
    // Pure: does a result toast close itself? Errors need acknowledging
    // and a toast offering next actions is a prompt, so both stay until
    // the operator answers; everything else is information and times out.
    function stageTrackerResultToastTimed(text) {
      return text.type !== 'error' && !(text.options && text.options.length);
    }
    // Pure: has the toast's target left the state it was shown in?
    function stageTrackerResultToastStale(entry, d) {
      const it = stageTrackerItemByKey(d, entry.key);
      if (!it) return true;
      return stageTrackerItemState(it) !== entry.state;
    }
    function stageTrackerResultToastsSync(d) {
      const keep = [];
      for (const e of _stageTrackerResultToasts) {
        if (!e.el.parentNode) continue;
        if (stageTrackerResultToastStale(e, d)) dismissToast(e.el, null, null, true); else keep.push(e);
      }
      _stageTrackerResultToasts = keep;
    }
    let _stageTrackerPending = [];
    // Keys that read "acting…": the clicked row(s) and the item acted on.
    function stageTrackerPendingKeys(item, rows) {
      const keys = [stageTrackerItemKey(item)];
      for (const r of (rows || [])) { const k = stageTrackerItemKey(r); if (keys.indexOf(k) < 0 && (k === keys[0] || stageTrackerDecisionTargetKey(r) === keys[0])) keys.push(k); }
      return keys;
    }
    function stageTrackerDecisionTargetKey(row) { const t = typeof stageTrackerDecisionTarget === 'function' ? stageTrackerDecisionTarget(row) : null; return t ? stageTrackerItemKey(t) : ''; }
    // Pure: is a row still waiting for the hive to decide (no verdict yet)?
    function stageTrackerRowDeciding(key) {
      return _stageTrackerPending.some(p => !p.decided && p.keys.indexOf(key) >= 0);
    }
    // Once the verdict is in, the caption names it ("Adopting · opening hive
    // copy…"); the ✓ badge may show from here on (#15126).
    function stageTrackerDecidePending(pend, result, item) {
      pend.decided = true;
      pend.caption = stageTrackerDecidedActing(result, item);
      stageTrackerPendingSync(document.getElementById('activity-monitor-panel'));
    }
    // busy: the data-ham-act of the row's lead button that reads busy while
    // this entry is pending ('' — the default — busies no button).
    function stageTrackerSetPending(keys, caption, now, busy) {
      const pend = { keys: keys.slice(), caption, key: '', state: null, decided: false, at: typeof now === 'number' ? now : Date.now(), busy: busy || '' };
      _stageTrackerPending.push(pend);
      stageTrackerPendingSync(document.getElementById('activity-monitor-panel'));
      return pend;
    }
    // After a result: wait for the target's state to change before clearing.
    function stageTrackerArmPending(pend, target) {
      pend.key = stageTrackerTargetKey(target) || pend.keys[0];
      pend.state = stageTrackerItemState(stageTrackerItemByKey(_stageTracker, pend.key));
      if (pend.keys.indexOf(pend.key) < 0) pend.keys.push(pend.key);
      stageTrackerPendingSync(document.getElementById('activity-monitor-panel'));
    }
    function stageTrackerClearPending(pend) {
      _stageTrackerPending = _stageTrackerPending.filter(p => p !== pend);
      stageTrackerPendingSync(document.getElementById('activity-monitor-panel'));
    }
    // Pure: the pending entries still waiting after payload d at time now.
    function stageTrackerPendingLive(pending, d, now) {
      return pending.filter(p => {
        if (now - p.at > STAGE_TRACKER_ACTING_MAX_MS) return false;
        if (!p.key) return true;
        const it = stageTrackerItemByKey(d, p.key);
        return !!it && stageTrackerItemState(it) === p.state;
      });
    }
    function stageTrackerPendingExpire(d, now) { _stageTrackerPending = stageTrackerPendingLive(_stageTrackerPending, d, typeof now === 'number' ? now : Date.now()); }
    // The row's lead button while a tagged pend (busyAct, its data-ham-act)
    // waits: busy, spinner, the pend's own caption (the same phrase the row
    // caption shows), not clickable again. The button's own aria-disabled /
    // title are kept and put back when the pend clears, so a lead the row
    // rendered disabled stays disabled.
    function stageTrackerLeadBusySync(lead, busyAct, caption) {
      if (!lead) return;
      const on = !!busyAct && lead.getAttribute('data-ham-act') === busyAct;
      const isBusy = lead.getAttribute('aria-busy') === 'true';
      if (on && !isBusy) {
        lead.setAttribute('data-ham-label', lead.textContent);
        lead.setAttribute('data-ham-was-disabled', lead.getAttribute('aria-disabled') === 'true' ? 'true' : '');
        lead.setAttribute('data-ham-title', lead.getAttribute('title') || '');
        lead.setAttribute('aria-busy', 'true'); lead.setAttribute('aria-disabled', 'true'); lead.removeAttribute('title');
        lead.textContent = '';
        const sp = document.createElement('span'); sp.className = 'toast-spinner cue-acting-spin'; lead.appendChild(sp);
        lead.appendChild(document.createTextNode(' ' + String(caption || 'acting…')));
      } else if (!on && isBusy) {
        lead.removeAttribute('aria-busy');
        if (lead.getAttribute('data-ham-was-disabled') === 'true') lead.setAttribute('aria-disabled', 'true'); else lead.removeAttribute('aria-disabled');
        const title = lead.getAttribute('data-ham-title');
        if (title) lead.setAttribute('title', title); else lead.removeAttribute('title');
        lead.textContent = lead.getAttribute('data-ham-label') || '';
        lead.removeAttribute('data-ham-label'); lead.removeAttribute('data-ham-was-disabled'); lead.removeAttribute('data-ham-title');
      }
    }
    // Mark rows / sub-rows acting; captions are real nodes so a kept row
    // keeps its caption and a re-rendered one gets it back.
    function stageTrackerPendingSync(el) {
      if (!el || typeof el.querySelectorAll !== 'function') return;
      const want = {}, busy = {};
      for (const p of _stageTrackerPending) for (const k of p.keys) { want[k] = p.caption; if (p.busy) busy[k] = p; }
      const nodes = Array.from(el.querySelectorAll('[data-ham-key], [data-ham-sub-key]'));
      for (const n of nodes) {
        const key = n.getAttribute('data-ham-sub-key') || n.getAttribute('data-ham-key');
        const cap = want[key];
        stageTrackerLeadBusySync(n.querySelector(':scope > .ham-row-head .ham-lead-act[data-ham-act]'), busy[key] ? busy[key].busy : '', busy[key] ? busy[key].caption : '');
        const strip = n.querySelector(':scope > .ham-strip-row');
        // The caption lives inside the lit chevron (the pill), never as a
        // separate element beside the ✓ badge (#15126); a row without a
        // lit chevron (a cap-only strip) falls back to the strip row.
        const host = strip ? (strip.querySelector(':scope > .ham-strip > .ham-seg.active:not(.cap)') || strip) : null;
        let capEl = strip ? (strip.querySelector(':scope > .cue-acting-cap') || (host !== strip ? host.querySelector(':scope > .cue-acting-cap') : null)) : null;
        if (cap) {
          n.classList.add('cue-acting');
          if (capEl && capEl.parentNode !== host) { capEl.remove(); capEl = null; }
          if (host && !capEl) { capEl = document.createElement('span'); capEl.className = 'cue-acting-cap'; capEl.setAttribute('role', 'status'); const sp = document.createElement('span'); sp.className = 'toast-spinner cue-acting-spin'; capEl.appendChild(sp); capEl.appendChild(document.createTextNode('')); host.appendChild(capEl); }
          if (capEl) capEl.lastChild.nodeValue = cap;
        } else {
          n.classList.remove('cue-acting');
          if (capEl) capEl.remove();
        }
      }
    }
    // Pure: the row's advisory block when it is one (tracker-row.js owns the
    // detection for the chevron; here only the server's flag counts).
    function stageTrackerAdvisoryOf(item) {
      return item && item.kind !== 'pr' && item.advisory && typeof item.advisory === 'object' ? item.advisory : null;
    }
    // A row action whose target mapping came up empty says so instead of
    // returning silently (hivecommons/hive#15436).
    function stageTrackerNothingToActOn(id, rows) {
      const refs = (rows || []).map(stageTrackerRefOf).filter(Boolean).join(', ');
      if (typeof showToast === 'function') showToast(stageTrackerActionLabel(id) + ': nothing to act on' + (refs ? ' — ' + refs + ' is not waiting on that decision' : ''), 'warning');
    }
    async function runRowAction(action, row) {
      let items = Array.isArray(row) ? row.slice() : (row ? [row] : []);
      const id = action;
      const rows = items.slice();
      if (!items.length) return { toasts: [], pending: [] };
      if (id === 'open') {
        for (const it of items) if (it.url) window.open(it.url, '_blank', 'noopener');
        return { toasts: [], pending: [] };
      }
      if (id === 'open:app-settings') {
        // The App settings page: the link the server put on the option.
        const url = stageTrackerBlockerOptionUrl(items[0], id);
        if (url) window.open(url, '_blank', 'noopener'); else showToast('No App settings link — the GitHub App slug is not configured', 'error');
        return { toasts: [], pending: [] };
      }
      if (id === 'blocker:wait' || id === 'blocker:patch') {
        showToast(id === 'blocker:wait' ? 'Left as is — the row stays blocked until the permission is granted or a person acts.' : 'Not available: the hive has no identity that may push workflow files.', 'info');
        return { toasts: [], pending: [] };
      }
      if (id === 'copy') {
        const text = items.map(it => it.url).filter(Boolean).join('\n');
        try {
          if (!navigator.clipboard) throw new Error('clipboard unavailable');
          await navigator.clipboard.writeText(text);
          showToast(items.length > 1 ? 'Copied ' + items.length + ' links' : 'Link copied', 'success');
        } catch (e) { showToast('Could not copy: ' + (e && e.message ? e.message : 'clipboard unavailable'), 'error'); }
        return { toasts: [], pending: [] };
      }
      if (id === 'decide:adopt') {
        // Adopt acts on the nudged PR row itself (072, not a decision park).
        items = items.filter(stageTrackerAdoptOffered);
        if (!items.length) return { toasts: [], pending: [] };
      } else if (id === 'decide:delegate-all') {
        // The issue row itself is the request; the server fans out per PR.
        items = items.filter(it => stageTrackerNestedOpenPRs(it).length >= STAGE_TRACKER_DELEGATE_ALL_MIN);
      } else if (id === 'decide:adopt-now' || stageTrackerActionBase(id) === 'decide:reroute') {
        // Adopt now / Reroute act on the PR waiting on its contributor (072 / 071).
        items = items.map(stageTrackerTakeoverTarget).filter(Boolean);
        if (!items.length) return { toasts: [], pending: [] };
      } else if (id === 'accept-triage') {
        // Accept triage acts on the issues waiting on it; a bulk selection
        // may carry PRs or issues past 015, which are skipped and counted
        // out in the menu label ("3 of 4").
        items = items.filter(stageTrackerTriageNeeded);
        if (!items.length) { stageTrackerNothingToActOn(id, rows); return { toasts: [], pending: [] }; }
      } else if (id === 'decide:close-read') {
        // Close (read) acts on the advisory row itself: an advisory is not
        // a decision park, so it has no decision target
        // (hivecommons/hive#15436 — the click used to map to nothing and
        // return without a request, a dialog or a toast).
        items = items.filter(stageTrackerAdvisoryOf);
        if (!items.length) { stageTrackerNothingToActOn(id, rows); return { toasts: [], pending: [] }; }
      } else if (id.indexOf('decide:') === 0) {
        // A row decides its parked PR when it has one (the issue's stage is
        // only mirrored from it); dedupe when the PR row is selected too.
        const seen = {};
        // Bulk delegation expands a parent with no single target into the
        // same open PRs as delegate-all — before deduping, so a selected
        // parent and its sub-rows run each PR once, with every PR named in
        // the shared confirmation. A parent with one parked PR keeps
        // deciding just that PR ("Decide PR #N › Let hive decide").
        items = [].concat(...items.map(it => id === 'decide:delegate' ? stageTrackerDelegateTargets(it) : [stageTrackerDecisionTarget(it)]))
          .filter(t => t && !seen[stageTrackerItemKey(t)] && (seen[stageTrackerItemKey(t)] = true));
        if (!items.length) { stageTrackerNothingToActOn(id, rows); return { toasts: [], pending: [] }; }
      }
      const payload = await stageTrackerActionPayload(items, id, rows);
      if (payload == null) return { toasts: [], pending: [], cancelled: true };
      const results = await stageTrackerRunActions(items, id, payload, rows);
      return { toast: results.toasts[0] || null, toasts: results.toasts, pending: results.pending, results };
    }
    // Pure: the PR a takeover action acts on — the PR row itself, or the
    // issue's nested PR waiting on its contributor.
    function stageTrackerTakeoverTarget(item) {
      if (!item) return null;
      if (item.kind === 'pr') return stageTrackerTakeoverOf(item) ? item : null;
      for (const lp of (Array.isArray(item.linked_prs) ? item.linked_prs : [])) {
        if (lp && lp.takeover) return stageTrackerItemByKey(_stageTracker, stageTrackerItemKey({ repo: item.repo, kind: 'pr', number: lp.number })) || stageTrackerLinkedPRItem(item, lp);
      }
      return null;
    }
    // Pure: the link a blocker option carries (decision_needed.options[].url).
    function stageTrackerBlockerOptionUrl(item, id) {
      const d = item && item.decision_needed;
      const opts = d && Array.isArray(d.options) ? d.options : [];
      for (const o of opts) if (o && o.action === id && o.url) return o.url;
      return (item && item.optionUrls && item.optionUrls[id]) || '';
    }
    // Menu / selection bar entry point: items are the clicked or selected rows.
    async function stageTrackerDoAction(items, id) { return runRowAction(id, items); }

    /* ---- Activity monitor sounds (#13275) ---------------------------------
       Opt-in "bug zapper" when a PR the tab was already watching merges,
       and a short bee-like buzz when an issue or PR first appears.
       The merge detector is pure (stageTrackerMergeState +
       stageTrackerCarryOpen + stageTrackerNewlyMerged, run under node by
       stage_tracker_merge_sound_test.go): a PR counts as newly merged when
       an earlier payload listed it open and a later one lists it merged —
       a stage at/after the terminal cap, or its key in recently_merged
       (the audit log's pr_merged entries, which is how a PR that simply
       dropped out of the open list is told apart from one closed
       unmerged). The open list and the audit entry do not always land in
       the same payload — a webhook-patched snapshot can drop the PR a poll
       before the poller records the merge — so a PR seen open is carried
       as "still open" after it vanishes until recently_merged settles it.
       recently_merged may name the repo by its short name; it is matched
       against the owner/name the items carry. First load has no previous
       payload, so it never fires; issues never fire; a per-PR "zapped" set
       stops replays; N merges in one refresh play at most
       STAGE_TRACKER_SOUND_MAX_PLAYS zaps. The new-item detector
       (stageTrackerOpenKeys + stageTrackerNewlyOpened) works the same way
       on every item's key with a per-key "buzzed" set and the same cap.
       Both sounds are synthesised with the Web Audio API (no asset, no
       URL — the CSP allows neither). Zap: a bug-zapper arc baked into one
       buffer — a ~4 ms broadband "snap", then a sizzle of bright noise
       and a 240 Hz square chopped by a 120 Hz rectified mains pulse
       train, with a random sputter gate (the arc drops out and
       re-strikes), under a fast-attack / ~80 ms-sustain / decaying
       envelope, high-passed at 700 Hz. Buzz: a sawtooth near 120 Hz with a ~30 Hz
       amplitude tremolo through a lowpass, quieter than the zap. The
       AudioContext is created lazily on a user gesture and resumed when
       suspended; a context the autoplay policy still holds suspended at
       play time re-arms the gesture listeners instead of queueing stale
       voices. A reload (the SPA reloads itself whenever the hive deploys
       a new version) is a fresh document with no user activation, so a
       context created for the first automatic play after it is suspended
       and every zap/buzz is dropped until the user clicks or presses a
       key on the page: the chip turns amber and a one-time toast with an
       "Enable sounds" action says so, instead of failing silently. Gesture
       plays (▶ Play, turning sound on) start their voices inside the
       gesture itself alongside resume() — WebKit unlocks the context on
       either, and only inside the gesture — and a resume() that resolves
       before the state has flipped waits for statechange rather than
       counting as blocked. */
    const STAGE_TRACKER_SOUND_KEY = 'hive.tracker.sound';
    const STAGE_TRACKER_SOUND_MAX_PLAYS = 2;
    const STAGE_TRACKER_SOUND_OFFSET_S = 0.26;
    const STAGE_TRACKER_SOUND_MASTER_GAIN = 0.5;
    const STAGE_TRACKER_ZAP_S = 0.45;
    const STAGE_TRACKER_ZAP_SNAP_DECAY = 900;
    const STAGE_TRACKER_ZAP_MAINS_HZ = 120;
    const STAGE_TRACKER_ZAP_PULSE_POW = 6;
    const STAGE_TRACKER_ZAP_SQUARE_HZ = 240;
    const STAGE_TRACKER_ZAP_NOISE_MIX = 0.75;
    const STAGE_TRACKER_ZAP_SQUARE_MIX = 0.25;
    const STAGE_TRACKER_ZAP_SPUTTER_P = 0.02;
    const STAGE_TRACKER_ZAP_RESTRIKE_P = 0.25;
    const STAGE_TRACKER_ZAP_ATTACK_S = 0.004;
    const STAGE_TRACKER_ZAP_SUSTAIN_S = 0.08;
    const STAGE_TRACKER_ZAP_DECAY = 7;
    const STAGE_TRACKER_ZAP_HIGHPASS_HZ = 700;
    const STAGE_TRACKER_GAIN_FLOOR = 0.0001;
    const STAGE_TRACKER_BUZZ_S = 0.18;
    const STAGE_TRACKER_BUZZ_MASTER_GAIN = 0.2;
    const STAGE_TRACKER_BUZZ_HZ = 120;
    const STAGE_TRACKER_BUZZ_DRIFT_HZ = 14;
    const STAGE_TRACKER_BUZZ_TREMOLO_HZ = 30;
    const STAGE_TRACKER_BUZZ_TREMOLO_DEPTH = 0.45;
    const STAGE_TRACKER_BUZZ_LOWPASS_HZ = 900;
    const STAGE_TRACKER_BUZZ_ATTACK_S = 0.015;
    // User-supplied clips (settings modal): one per event, kept in
    // IndexedDB as raw bytes and decoded on the AudioContext when it exists.
    const STAGE_TRACKER_VOLUME_KEY = 'hive.tracker.sound.volume';
    const STAGE_TRACKER_VOLUME_MAX = 100;
    const STAGE_TRACKER_VOLUME_DEFAULT = 100;
    const STAGE_TRACKER_CLIP_DB = 'hive-tracker-sounds';
    const STAGE_TRACKER_CLIP_STORE = 'clips';
    const STAGE_TRACKER_CLIP_MAX_BYTES = 4 * 1024 * 1024;
    const STAGE_TRACKER_CLIP_PREVIEW_GAP_S = 0.12;
    // AudioContext.resume() is asynchronous (and in Safari the context is
    // 'suspended' or 'interrupted' until it settles): a play inside a user
    // gesture waits this long for it; an automatic play waits only briefly so
    // voices never queue up and burst out together on a later gesture.
    const STAGE_TRACKER_GESTURE_RESUME_MS = 1000;
    const STAGE_TRACKER_AUTO_RESUME_MS = 250;
    const STAGE_TRACKER_SOUND_BLOCKED_MSG = 'The browser blocked audio for this page — click ▶ Play again, or allow sound/auto-play for this site in the browser\'s site settings.';
    const STAGE_TRACKER_SOUND_UNSUPPORTED_MSG = 'This browser has no Web Audio support, so the tracker sounds cannot play.';
    const STAGE_TRACKER_SOUND_PLAYING_LABEL = 'Playing…';
    // Autoplay-policy affordance: shown once per page load when sound is on
    // but the browser holds the audio engine back until a click or key press
    // on this page (every reload — including the automatic one on a new hive
    // version — starts without user activation).
    const STAGE_TRACKER_SOUND_HELD_MSG = 'Tracker sounds are on, but the browser holds audio back until you click or press a key on this page.';
    const STAGE_TRACKER_SOUND_HELD_SKIPPED_MSG = ' A zap or buzz was just skipped.';
    const STAGE_TRACKER_SOUND_HELD_ACTION = 'Enable sounds';
    const STAGE_TRACKER_SOUND_HELD_TOAST_MS = 60000;
    const STAGE_TRACKER_SOUND_HELD_CLASS = 'ham-sound-blocked';
    const STAGE_TRACKER_SOUND_HELD_TITLE = 'Sound on, but held back by the browser until you click or press a key on this page';
    // Gestures the autoplay policies count as user activation: WebKit does
    // not treat every pointerdown as one, so click and touchend are armed too.
    const STAGE_TRACKER_PRIME_EVENTS = Object.freeze(['pointerdown', 'keydown', 'click', 'touchend']);
    const STAGE_TRACKER_SOUND_KINDS = Object.freeze({
      open: { label: 'Open sound', builtin: 'Built-in (bee buzz)', event: 'a new issue or pull request appears' },
      merge: { label: 'Merge sound', builtin: 'Built-in (zapper)', event: 'a pull request merges' },
    });
    var _stageTrackerMergeState = null;
    var _stageTrackerZapped = {};
    var _stageTrackerOpenState = null;
    var _stageTrackerBuzzed = {};
    var _stageTrackerAudioCtx = null;
    var _stageTrackerSoundOn = false;
    var _stageTrackerSoundPrimed = false;
    var _stageTrackerSoundHeldShown = false;
    var _stageTrackerVolume = STAGE_TRACKER_VOLUME_DEFAULT;
    // kind → { name, bytes (ArrayBuffer), buffer (AudioBuffer|null), failed }
    var _stageTrackerClips = { open: null, merge: null };
    var _stageTrackerSettingsReturnFocus = null;

