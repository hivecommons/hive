    // ── "Your turn" (hivecommons/hive#11507, part C) ─────────────────────
    //
    // The server decides, once per status refresh, whether an issue or pull
    // request waits on the operator (pkg/dashboard/ownership.go) and stamps the
    // decision on the row as `ownership`. Everything below READS that decision:
    // the browser never derives "your turn" from labels or bands. The checkbox glyph
    // below means "your turn" and nothing else on the dashboard, and this constant
    // is the only place it is written.
    const YOUR_TURN_GLYPH = '☐';
    // The inbox's group order; `inbox_rank` already encodes it, the list is here
    // only so the strip can name its groups in the same order.
    const YOUR_TURN_VERB_ORDER = ['INSPECT', 'DECIDE', 'STEER', 'SPEC', 'REVIEW', 'TRIAGE', 'ADMIT', 'CLOSE'];
    // Cards of repositories at these effective levels show only their
    // your-turn rows and fold the rest into one summary line.
    const YOUR_TURN_COLLAPSE_MIN_LEVEL = 5;
    const YOUR_TURN_EXPANDED_KEY_PREFIX = 'hive-repos-yourturn-expanded:';

    function yourTurnOf(row) {
      const own = row && row.ownership;
      return own && own.your_turn ? own : null;
    }

    // Your-turn rows precede every other row inside a band.
    function yourTurnFirst(a, b) {
      return Number(!yourTurnOf(a)) - Number(!yourTurnOf(b));
    }

    function yourTurnTip(own) {
      return own.verb + ' — ' + own.reason;
    }

    // A row that is waiting on someone else carries the same ownership.reason
    // as a your-turn row (who it waits for, and when it becomes yours), but no
    // marker renders it. The tooltip is where that reason shows (#13183).
    function waitingElsewhereTip(row) {
      const own = row && row.ownership;
      if (!own || own.your_turn || !own.waiting_elsewhere || !own.reason) return '';
      return ' [waiting on someone else: ' + own.reason + ']';
    }

    function yourTurnKey(repoFull, kind, number) {
      return String(repoFull || '').toLowerCase() + '|' + kind + '|' + Number(number);
    }

    // The marker is a menu button, not a checkbox: it opens the item's menu and
    // does nothing else. Its accessible name carries the verb and the reason;
    // the same text is shown on hover and focus (yourTurnShowTip). The inbox
    // rows open the same menu from a plain "Options" button, so the glyph stays
    // on the card rows.
    function yourTurnButtonHTML(repoFull, kind, row, variant) {
      const own = yourTurnOf(row);
      if (!own || !row.number) return '';
      const tip = yourTurnTip(own);
      const open = !!(_yourTurnMenu && _yourTurnMenu.key === yourTurnKey(repoFull, kind, row.number) && _yourTurnMenu.variant === variant);
      const marker = variant === 'marker';
      const label = marker ? 'Your turn: ' + tip + '. Opens a menu of options.' : 'Options for ' + repoFull + '#' + row.number + ': ' + tip;
      return `<button type="button" class="yt-menu-btn ${marker ? 'yt-marker' : 'yt-inbox-btn'}" aria-haspopup="menu" aria-expanded="${open ? 'true' : 'false'}" aria-label="${esc(label)}" data-yt-tip="${esc(tip)}" data-yt-variant="${variant}" data-yt-repo="${esc(repoFull)}" data-yt-kind="${esc(kind)}" data-yt-number="${Number(row.number)}" data-action="openYourTurnMenu" data-arg-types="t" data-stop="1" data-prevent="1">${marker ? YOUR_TURN_GLYPH : 'Options'}</button>`;
    }

    function yourTurnMarkerHTML(repoFull, kind, row) {
      return yourTurnButtonHTML(repoFull, kind, row, 'marker');
    }

    // On a your-turn row the marker sits before the state chip, in the chip's
    // own grid cell; every other row keeps its markup as it was.
    function yourTurnLead(marker, chip) {
      return marker ? `<span class="repo-pill-lead">${marker}${chip}</span>` : chip;
    }

    // What the menu offers for one item. Pure: every input comes in `ctx`, so the
    // per-verb table of #11507 can be run against fixtures.
    //   ctx.own          the row's ownership (verb, reason_code, reason, action)
    //   ctx.kind         'issue' | 'pr'
    //   ctx.url          the item on GitHub
    //   ctx.isOwner      the user holds the owner role
    //   ctx.planningAllowed  the hive plans (L5+), as for the 📐 button
    //   ctx.hasPlan      the issue already has a plan (the 📐 button is gone)
    //   ctx.canToggleHold    the live repository write gate used by hold/release
    //   ctx.held         the PR is held
    //   ctx.sentinelLabel, ctx.admissionLabels   names for the notes
    //   ctx.labelDriven  the repo's sweep refuses /hive approve and /hive decision
    // Only actions the dashboard already offers, only to a user it already
    // offers them to, are listed; "Open on GitHub" is always present.
    function yourTurnMenuModel(ctx) {
      const own = ctx.own;
      const actions = [];
      const after = [];
      const parkedIssue = ctx.kind === 'issue' && String(own.reason_code || '').indexOf('parked_needs_') === 0;
      if (own.verb === 'DECIDE' || own.verb === 'STEER' || (own.verb === 'REVIEW' && parkedIssue)) {
        if (ctx.labelDriven) after.push({ type: 'note', text: 'This repository is label-driven: accept or un-park it with the repository\'s triage labels; Hive refuses /hive approve and /hive decision here.' });
        else after.push({ type: 'copy', text: '/hive approve' }, { type: 'copy', text: '/hive decision <text>' });
      }
      if (ctx.kind === 'issue' && own.action === 'release' && own.release_tag && ctx.canToggleHold && !ctx.labelDriven) {
        actions.push({ type: 'action', id: 'release', label: 'Release ' + own.release_tag });
      }
      if (own.verb === 'SPEC') {
        if (ctx.kind === 'issue' && ctx.isOwner && ctx.planningAllowed && !ctx.hasPlan) actions.push({ type: 'action', id: 'design', label: 'Design before planning' });
        after.push({ type: 'note', text: 'Adding the spec or removing the label releases it.' });
      }
      if (own.verb === 'TRIAGE') {
        after.push({ type: 'note', text: 'Adding the approved-direction label, assigning a person, or closing it acknowledges it.' });
      }
      if (own.verb === 'ADMIT') {
        const labels = (ctx.admissionLabels || []).filter(Boolean);
        after.push({ type: 'note', text: labels.length ? 'Add the label ' + labels.join(' or ') + ' to admit it.' : 'Add an admission label to admit it.' });
      }
      if (own.verb === 'REVIEW' && ctx.kind === 'pr' && own.action === 'release_hold' && ctx.held && ctx.canToggleHold) {
        actions.push({ type: 'action', id: 'release_hold', label: 'Release hold' });
      }
      if (own.verb === 'INSPECT') {
        after.push({ type: 'note', text: 'Removing the ' + (ctx.sentinelLabel || 'sentinel') + ' label after review releases it.' });
      }
      // A run or approval-desk wait links to the section that acts on it (#12380).
      if (own.link && String(own.link).charAt(0) === '#') {
        actions.push({ type: 'link', label: String(own.reason_code || '').indexOf('run_') === 0 ? 'Open in Runs' : 'Open in Approvals', href: own.link, sameTab: true });
      }
      if (own.verb === 'CLOSE' && own.reason_code === 'reporter_confirmation_stale') {
        after.push({ type: 'copy', text: '/fixed' });
      }
      return {
        verb: own.verb,
        reason: own.reason,
        entries: actions.concat([{ type: 'link', label: 'Open on GitHub', href: ctx.url }], after)
      };
    }

    function yourTurnMenuHTML(model, ctx) {
      const items = model.entries.map(e => {
        if (e.type === 'action') {
          return `<button type="button" role="menuitem" tabindex="-1" class="yt-item" data-yt-act="${esc(e.id)}" data-repo="${esc(ctx.repo)}" data-number="${Number(ctx.number)}" data-url="${esc(ctx.url)}" data-title="${esc(ctx.title || '')}">${esc(e.label)}</button>`;
        }
        if (e.type === 'link') {
          if (e.sameTab) return `<a role="menuitem" tabindex="-1" class="yt-item" href="${esc(e.href)}">${esc(e.label)}</a>`;
          return `<a role="menuitem" tabindex="-1" class="yt-item" href="${esc(e.href)}" target="_blank" rel="noopener">${esc(e.label)}</a>`;
        }
        if (e.type === 'copy') {
          return `<div class="yt-copy" role="none"><code>${esc(e.text)}</code><button type="button" role="menuitem" tabindex="-1" class="yt-item yt-copy-btn" data-yt-copy="${esc(e.text)}" aria-label="${esc('Copy ' + e.text)}">Copy</button></div>`;
        }
        return `<div class="yt-note" role="none">${esc(e.text)}</div>`;
      }).join('');
      return `<div class="yt-menu-head" id="yt-menu-head" role="presentation"><span class="yt-verb">${esc(model.verb)}</span> <span class="yt-reason">${esc(model.reason)}</span></div>${items}`;
    }

    // Locates the live row behind a marker in the current snapshot, so a menu
    // opened before a refresh still reads current data.
    function yourTurnFindRow(repoFull, kind, number) {
      const repos = (window._lastStatus || {}).repos || [];
      const repo = repos.find(r => String(r.full || r.name || '').toLowerCase() === String(repoFull || '').toLowerCase());
      if (!repo) return null;
      const n = Number(number);
      const pick = list => (list || []).find(x => Number(x && x.number) === n);
      if (kind === 'pr') {
        const held = pick(repo.heldPrs);
        const row = held || pick(repo.openPrs);
        return row ? { repo, row, held: !!held } : null;
      }
      const held = pick(repo.heldIssues);
      const row = held || pick(repo.actionableIssues) || pick(repo.nonActionableIssues);
      return row ? { repo, row, held: !!held } : null;
    }

    function yourTurnMenuContext(repoFull, kind, number) {
      const found = yourTurnFindRow(repoFull, kind, number);
      const own = found && yourTurnOf(found.row);
      if (!own) return null;
      const base = (window._githubBaseUrl || 'https://github.com').replace(/\/$/, '');
      const role = window._hiveRole || 'read';
      return {
        repo: repoFull,
        kind,
        number: Number(number),
        title: found.row.title || '',
        own,
        held: found.held,
        url: found.row.url || (base + '/' + repoFull + '/' + (kind === 'pr' ? 'pull' : 'issues') + '/' + number),
        isOwner: dashboardRoleAtLeast(role, 'owner'),
        planningAllowed: planningAllowed(),
        hasPlan: !!planIssueLinks()[repoFull + '#' + number],
        canToggleHold: repoHoldPermissionKnown(repoFull),
        sentinelLabel: window._hiveSentinelLabel || '',
        admissionLabels: (window._lastStatus || {}).admissionLabels || [],
        labelDriven: !!own.label_driven
      };
    }

    // Keyboard rules of the menu: arrows (and Home/End) move between entries,
    // Escape closes and returns focus to the marker, Tab closes and lets focus
    // continue from the marker. Enter and Space activate the focused entry
    // natively (they are buttons and a link).
    function yourTurnMenuNav(key, index, count) {
      if (key === 'Escape') return { close: true, refocus: true };
      if (key === 'Tab') return { close: true, refocus: true };
      if (!count) return null;
      if (key === 'ArrowDown') return { index: (index + 1) % count };
      if (key === 'ArrowUp') return { index: (index - 1 + count) % count };
      if (key === 'Home') return { index: 0 };
      if (key === 'End') return { index: count - 1 };
      return null;
    }

    let _yourTurnMenu = null;

    function yourTurnMarkerKey(marker) {
      return yourTurnKey(marker.dataset.ytRepo, marker.dataset.ytKind, marker.dataset.ytNumber);
    }

    // The button that opened the menu: the very element when it is still on the
    // page, else the same item's button of the same kind after a repaint.
    function yourTurnOpenerFor(open) {
      if (open.opener && open.opener.isConnected) return open.opener;
      return Array.from(document.querySelectorAll('.yt-menu-btn')).find(m => yourTurnMarkerKey(m) === open.key && m.dataset.ytVariant === open.variant) || null;
    }

    function closeYourTurnMenu(refocus) {
      const open = _yourTurnMenu;
      if (!open) return;
      _yourTurnMenu = null;
      open.el.remove();
      document.removeEventListener('click', yourTurnOutsideClick, true);
      window.removeEventListener('resize', yourTurnDismiss);
      document.removeEventListener('scroll', yourTurnDismiss, true);
      const marker = yourTurnOpenerFor(open);
      if (marker) {
        marker.setAttribute('aria-expanded', 'false');
        if (refocus) marker.focus();
      }
    }

    // A resize closes the menu. A scroll closes it only when it moved the
    // button the menu hangs from: focusing an entry may scroll the page a little
    // without the menu having drifted from its button.
    function yourTurnDismiss(e) {
      if (e && e.type === 'scroll' && _yourTurnMenu) {
        if (e.target && _yourTurnMenu.el.contains(e.target)) return;
        const opener = yourTurnOpenerFor(_yourTurnMenu);
        if (opener) {
          const now = opener.getBoundingClientRect();
          if (Math.abs(now.top - _yourTurnMenu.rect.top) < 2 && Math.abs(now.left - _yourTurnMenu.rect.left) < 2) return;
        }
      }
      closeYourTurnMenu(false);
    }

    function yourTurnOutsideClick(e) {
      if (!_yourTurnMenu) return;
      if (_yourTurnMenu.el.contains(e.target)) return;
      // A click on the marker that owns the menu is its own toggle.
      const marker = e.target.closest ? e.target.closest('.yt-menu-btn') : null;
      if (marker && yourTurnMarkerKey(marker) === _yourTurnMenu.key && marker.dataset.ytVariant === _yourTurnMenu.variant) return;
      closeYourTurnMenu(false);
    }

    function yourTurnMenuItems(menu) {
      return Array.from(menu.querySelectorAll('[role="menuitem"]'));
    }

    function yourTurnMenuKeydown(e) {
      if (!_yourTurnMenu) return;
      const items = yourTurnMenuItems(_yourTurnMenu.el);
      const step = yourTurnMenuNav(e.key, items.indexOf(document.activeElement), items.length);
      if (!step) return;
      if (step.close) {
        if (e.key !== 'Tab') e.preventDefault();
        closeYourTurnMenu(step.refocus);
        return;
      }
      e.preventDefault();
      items[step.index].focus();
    }

    function yourTurnMenuClick(e) {
      const el = e.target.closest ? e.target.closest('.yt-item') : null;
      if (!el || !_yourTurnMenu) return;
      const ctx = _yourTurnMenu.ctx;
      if (el.dataset.ytCopy) {
        const text = el.dataset.ytCopy;
        closeYourTurnMenu(true);
        writeClipboardText(text).then(ok => showToast(ok ? 'Copied ' + text : 'Could not copy — select and copy it by hand: ' + text, ok ? 'success' : 'error'));
        return;
      }
      if (el.dataset.ytAct) {
        // The snapshot may have refreshed while the menu was open: perform the
        // action only if the item's current decision still offers it.
        const fresh = yourTurnMenuContext(ctx.repo, ctx.kind, ctx.number);
        const offered = !!fresh && yourTurnMenuModel(fresh).entries.some(e => e.type === 'action' && e.id === el.dataset.ytAct);
        closeYourTurnMenu(true);
        if (!offered) {
          showToast('This item changed since the menu opened — open it again to see its options', 'info');
          return;
        }
        if (el.dataset.ytAct === 'release_hold') toggleRepoItemHold(ctx.repo, ctx.number, 'pr', false, null);
        else if (el.dataset.ytAct === 'release') releaseIssueOwnerStep(ctx.repo, ctx.number, ctx.own.release_tag);
        else if (el.dataset.ytAct === 'design') designIssue(el);
        return;
      }
      // "Open on GitHub": the link navigates on its own.
      closeYourTurnMenu(false);
    }

    // Opens the menu for the marker that was clicked. Nothing else happens on
    // the click: every effect belongs to an entry the operator then chooses.
    function openYourTurnMenu(marker) {
      if (!marker || !marker.dataset) return;
      const repo = marker.dataset.ytRepo;
      const kind = marker.dataset.ytKind;
      const number = Number(marker.dataset.ytNumber);
      const key = yourTurnKey(repo, kind, number);
      const variant = marker.dataset.ytVariant;
      const wasOpen = _yourTurnMenu && _yourTurnMenu.key === key && _yourTurnMenu.variant === variant;
      closeYourTurnMenu(false);
      yourTurnHideTip();
      if (wasOpen) return;
      const ctx = yourTurnMenuContext(repo, kind, number);
      if (!ctx) return;
      const menu = document.createElement('div');
      menu.className = 'yt-menu';
      menu.setAttribute('role', 'menu');
      menu.setAttribute('aria-labelledby', 'yt-menu-head');
      menu.innerHTML = yourTurnMenuHTML(yourTurnMenuModel(ctx), ctx);
      document.body.appendChild(menu);
      const rect = marker.getBoundingClientRect();
      const left = Math.max(4, Math.min(rect.left, window.innerWidth - menu.offsetWidth - 4));
      const below = rect.bottom + 4;
      const top = below + menu.offsetHeight > window.innerHeight - 4 ? Math.max(4, rect.top - menu.offsetHeight - 4) : below;
      menu.style.left = left + 'px';
      menu.style.top = top + 'px';
      marker.setAttribute('aria-expanded', 'true');
      menu.addEventListener('keydown', yourTurnMenuKeydown);
      menu.addEventListener('click', yourTurnMenuClick);
      _yourTurnMenu = { el: menu, key, variant, opener: marker, ctx, rect: { top: rect.top, left: rect.left } };
      document.addEventListener('click', yourTurnOutsideClick, true);
      window.addEventListener('resize', yourTurnDismiss);
      document.addEventListener('scroll', yourTurnDismiss, true);
      const first = yourTurnMenuItems(menu)[0];
      if (first) first.focus();
    }

    // Hover and keyboard focus both show the verb and the reason (a native
    // title is not shown on focus), in a tooltip tied to the marker.
    function yourTurnShowTip(marker) {
      if (!marker || !marker.dataset.ytTip) return;
      if (_yourTurnMenu && _yourTurnMenu.key === yourTurnMarkerKey(marker) && _yourTurnMenu.variant === marker.dataset.ytVariant) return;
      let tip = document.getElementById('yt-tip');
      if (!tip) {
        tip = document.createElement('div');
        tip.id = 'yt-tip';
        tip.className = 'yt-tip';
        tip.setAttribute('role', 'tooltip');
        document.body.appendChild(tip);
      }
      tip.textContent = marker.dataset.ytTip;
      tip.hidden = false;
      const rect = marker.getBoundingClientRect();
      tip.style.left = Math.max(4, Math.min(rect.left, window.innerWidth - tip.offsetWidth - 4)) + 'px';
      const above = rect.top - tip.offsetHeight - 4;
      tip.style.top = (above >= 4 ? above : rect.bottom + 4) + 'px';
      marker.setAttribute('aria-describedby', 'yt-tip');
    }

    function yourTurnHideTip() {
      const tip = document.getElementById('yt-tip');
      if (tip) tip.hidden = true;
      document.querySelectorAll('.yt-menu-btn[aria-describedby]').forEach(m => m.removeAttribute('aria-describedby'));
    }

    function installYourTurnListeners(doc) {
      const markerOf = e => (e.target && e.target.closest ? e.target.closest('.yt-menu-btn') : null);
      doc.addEventListener('mouseover', e => { const m = markerOf(e); if (m) yourTurnShowTip(m); });
      doc.addEventListener('mouseout', e => { if (markerOf(e)) yourTurnHideTip(); });
      doc.addEventListener('focusin', e => { const m = markerOf(e); if (m) yourTurnShowTip(m); });
      doc.addEventListener('focusout', e => { if (markerOf(e)) yourTurnHideTip(); });
    }
    installYourTurnListeners(document);

    // ── The critical path (#13646) ──────────────────────────────────────

    // your-turn critical path helpers begin
    // The server stamps every your-turn row with what it waits on, how many
    // your-turn items it unblocks, the PRs it may conflict with (a soft hint,
    // never an edge), the one action that moves it and, unless it is a root
    // cause itself, the root cause (blocker) it collapses under.

    // A row reference ("#N" relative to repo, or "owner/repo#N") as a key.
    function cpRowKey(repo, ref) {
      const text = String(ref || '');
      const at = text.lastIndexOf('#');
      if (at < 0) return '';
      return String(at === 0 ? repo : text.slice(0, at)).toLowerCase() + '#' + text.slice(at + 1);
    }

    function cpUnblocks(row) { return Number(row && row.unblocks) || 0; }

    // Most unblocked first; ties keep the order they came in (age, rank).
    function cpByUnblocks(a, b) { return cpUnblocks(b) - cpUnblocks(a); }

    function cpWaitsText(row) {
      const label = w => w.kind === 'base-check' ? w.ref + ' (red on base)' : w.kind === 'approval' ? 'approval' : w.kind === 'merge' ? '' : (w.ref || w.kind);
      const parts = ((row && row.waits_on) || []).map(label).filter(Boolean);
      return parts.length ? 'waits on ' + parts.join(', ') : '';
    }

    function cpActionText(row) {
      const labels = { merge: 'merge', 'release-hold': 'release hold', rebase: 'rebase', rerun: 'rerun', approve: 'approve' };
      if (!row || !labels[row.action]) return '';
      if (!row.action_ref) return labels[row.action];
      return labels[row.action] + (row.action === 'rebase' || row.action === 'rerun' ? ' after ' : ' ') + row.action_ref;
    }

    // The "unblocks N" / "waits on …" chips and the soft "may conflict with".
    function cpChipsHTML(row) {
      if (!row || !Array.isArray(row.waits_on)) return '';
      const out = [];
      const n = cpUnblocks(row);
      if (n) out.push(`<span class="cp-chip cp-unblocks" title="${esc(n + ' your-turn item' + (n === 1 ? ' waits' : 's wait') + ' on this, directly or through another')}">unblocks ${n}</span>`);
      const waits = cpWaitsText(row);
      if (waits) out.push(`<span class="cp-chip cp-waits" title="${esc(waits + (row.blocker ? ' — root cause ' + row.blocker : ''))}">${esc(waits)}</span>`);
      const soft = (row.may_conflict_with || []).map(Number).filter(Boolean);
      if (soft.length) out.push(`<span class="cp-chip soft" title="Touches the same files or closes the same issue. Not a dependency: either can land first.">may conflict with ${esc(soft.map(x => '#' + x).join(', '))}</span>`);
      return out.join('');
    }

    function cpActionHTML(row) {
      const text = cpActionText(row);
      return text ? `<span class="cp-action" title="${esc('Next action: ' + text)}">${esc(text)}</span>` : '';
    }

    // Folds every item whose blocker is also among items under that blocker.
    // items are { repo, kind, row }; the result keeps the order of the items
    // not folded, each with the dependents folded under it.
    function cpCollapse(items) {
      const byKey = new Map();
      (items || []).forEach(item => byKey.set(cpRowKey(item.repo, '#' + item.row.number), item));
      const dependents = new Map();
      const folded = new Set();
      (items || []).forEach(item => {
        const blocker = item.row && item.row.blocker ? byKey.get(cpRowKey(item.repo, item.row.blocker)) : null;
        if (!blocker || blocker === item || (blocker.row && blocker.row.blocker)) return;
        if (!dependents.has(blocker)) dependents.set(blocker, []);
        dependents.get(blocker).push(item);
        folded.add(item);
      });
      return (items || []).filter(item => !folded.has(item)).map(item => ({ item, dependents: dependents.get(item) || [] }));
    }

    // "8 PRs waiting on #255 (dakota-iso)"
    function cpCollapsedSummary(blocker, dependents) {
      const n = dependents.length;
      const kinds = new Set(dependents.map(d => d.kind));
      const noun = kinds.size !== 1 ? 'item' : (kinds.has('pr') ? 'PR' : 'issue');
      const repo = String(blocker.repo || '').split('/').pop();
      return n + ' ' + noun + (n === 1 ? '' : 's') + ' waiting on #' + blocker.row.number + (repo ? ' (' + repo + ')' : '');
    }

    // One expandable row under the blocker holding its dependents.
    function cpDependentsHTML(entry, renderRow) {
      if (!entry || !entry.dependents.length) return '';
      return `<details class="cp-dependents"><summary>${esc(cpCollapsedSummary(entry.item, entry.dependents))}</summary>${entry.dependents.map(renderRow).join('')}</details>`;
    }
    // your-turn critical path helpers end

    // ── The inbox ────────────────────────────────────────────────────────

    // Every your-turn item on the repositories shown, in the server's order.
    // `repos` are the repositories the cards are drawn for; the pill filter and
    // the Overview filter narrow the rows exactly as they narrow the cards.
    function yourTurnInboxItems(repos, pillFilter, overviewFilter) {
      const items = [];
      (repos || []).forEach(r => {
        const repoFull = r.full || r.name || '';
        const matches = overviewFilter ? overviewProjectFilterRepoMatches(r, overviewFilter) : null;
        const heldIssueKeys = new Set((r.heldIssues || []).map(i => String(i.number)));
        const issues = matches ? matches.issues.map(entry => entry.issue) : repoCardTrackedIssues(r);
        issues.forEach(i => {
          const held = heldIssueKeys.has(String(i.number));
          if (yourTurnOf(i) && repoPillFilterMatches(repoIssueFilterKinds(i, i, held), pillFilter)) items.push({ repo: repoFull, kind: 'issue', row: i, held });
        });
        // Bandless rows are not entries of the filter match; they pass the same
        // kind/term test the tile and the card bucket count them by.
        repoCardUntrackedIssues(r).filter(i => !overviewFilter || overviewProjectFilterMatchesEntry({ kind: 'issue', item: i }, overviewFilter)).forEach(i => {
          if (yourTurnOf(i) && repoPillFilterMatches(repoNonActionableIssueKinds(repoNonActionableIssueBucketSpec(i), i), pillFilter)) items.push({ repo: repoFull, kind: 'issue', row: i, held: false });
        });
        const prs = matches
          ? matches.prs.map(entry => ({ pr: entry.pr, held: entry.held }))
          : (r.openPrs || []).map(pr => ({ pr, held: false })).concat((r.heldPrs || []).map(pr => ({ pr, held: true })));
        prs.forEach(entry => {
          if (yourTurnOf(entry.pr) && repoPillFilterMatches(repoPRFilterKinds(entry.pr, entry.held, entry.pr), pillFilter)) items.push({ repo: repoFull, kind: 'pr', row: entry.pr, held: entry.held });
        });
      });
      const rank = item => Number(item.row.ownership.inbox_rank) || Number.MAX_SAFE_INTEGER;
      return items.sort((a, b) => cpByUnblocks(a.row, b.row) || rank(a) - rank(b));
    }

    // N excludes CLOSE; M is the CLOSE count.
    function yourTurnCounts(items) {
      const m = (items || []).filter(item => item.row.ownership.verb === 'CLOSE').length;
      return { n: (items || []).length - m, m };
    }

    function yourTurnInboxHeaderText(counts) {
      return 'Your turn · ' + counts.n + (counts.m ? ' · ' + counts.m + ' to close' : '');
    }

    // The section counter: hidden only when both numbers are zero.
    function yourTurnCounterText(counts) {
      if (!counts.n && !counts.m) return '';
      return '· ' + counts.n + ' your turn' + (counts.m ? ' · ' + counts.m + ' to close' : '');
    }

    // Empty-state wording by the effective levels of the repositories shown.
    function yourTurnEmptyText(levels) {
      const known = (levels || []).map(Number).filter(l => l > 0);
      if (known.length && known.every(l => l <= 2)) return 'Hive is advisory at this level, so nothing is routed to you.';
      if (known.length && known.every(l => l >= 5)) return 'No exceptions: Hive is handling everything at this level.';
      return 'Nothing is waiting on you.';
    }

    // The reviewer agent's most recent verdict on a held agent PR, from the
    // review fields the snapshot already carries.
    function yourTurnReviewerVerdict(item) {
      const p = item.row;
      if (item.kind !== 'pr' || !item.held || !p.role || !(p.review_state || p.review_url)) return '';
      const state = p.review_state ? String(p.review_state).replace(/_/g, ' ') : 'commented';
      const when = pillAge(p.reviewed_at);
      const text = 'Reviewer: ' + state + (when ? ' ' + when : '');
      return p.review_url
        ? `<a class="yt-verdict" href="${esc(p.review_url)}" target="_blank" rel="noopener" title="Open the reviewer agent's most recent review">${esc(text)}</a>`
        : `<span class="yt-verdict">${esc(text)}</span>`;
    }

    function yourTurnInboxRowHTML(item) {
      const own = item.row.ownership;
      const base = (window._githubBaseUrl || 'https://github.com').replace(/\/$/, '');
      const url = item.row.url || (base + '/' + item.repo + '/' + (item.kind === 'pr' ? 'pull' : 'issues') + '/' + item.row.number);
      const verdict = yourTurnReviewerVerdict(item);
      return `<div class="yt-inbox-row"><span class="yt-inbox-repo">${esc(item.repo)}</span><a class="yt-inbox-title" href="${esc(url)}" target="_blank" rel="noopener" title="${esc(item.row.title || '')}"><span class="pill-num">#${Number(item.row.number)}</span> <span class="pill-title">${esc(item.row.title || '')}</span></a><span class="yt-verb">${esc(own.verb)}</span><span class="yt-inbox-reason-cell"><span class="yt-reason">${esc(own.reason)}</span>${cpChipsHTML(item.row)}</span><span class="yt-inbox-end">${verdict}${cpActionHTML(item.row)}${yourTurnButtonHTML(item.repo, item.kind, item.row, 'inbox')}</span></div>`;
    }

    function yourTurnInboxHTML(items, levels) {
      const counts = yourTurnCounts(items);
      const head = `<div class="yt-inbox-head" id="yt-inbox-head">${esc(yourTurnInboxHeaderText(counts))}</div>`;
      if (!items.length) return `${head}<div class="yt-inbox-empty">${esc(yourTurnEmptyText(levels))}</div>`;
      // Items waiting on another item of the inbox fold under it (#13646).
      const entries = cpCollapse(items);
      const groups = YOUR_TURN_VERB_ORDER.map(verb => {
        const rows = items.filter(item => item.row.ownership.verb === verb);
        const shown = entries.filter(entry => entry.item.row.ownership.verb === verb);
        return rows.length ? `<div class="yt-inbox-group" data-yt-verb="${esc(verb)}"><div class="yt-inbox-group-title">${esc(verb)} · ${rows.length}</div>${shown.map(entry => yourTurnInboxRowHTML(entry.item) + cpDependentsHTML(entry, yourTurnInboxRowHTML)).join('')}</div>` : '';
      }).join('');
      return `${head}<div class="yt-inbox-body">${groups}</div>`;
    }

    // ── L5/L6 card default ───────────────────────────────────────────────

    function yourTurnExpandedKey() {
      return YOUR_TURN_EXPANDED_KEY_PREFIX + (((window._lastStatus || {}).hiveId) || 'default');
    }

    function yourTurnExpandedRepos() {
      try { const list = JSON.parse(localStorage.getItem(yourTurnExpandedKey()) || '[]'); return Array.isArray(list) ? list.map(v => String(v).toLowerCase()) : []; } catch { return []; }
    }

    function toggleYourTurnCollapse(repoFull) {
      const key = String(repoFull || '').toLowerCase();
      const set = new Set(yourTurnExpandedRepos());
      if (set.has(key)) set.delete(key); else set.add(key);
      try {
        if (set.size) localStorage.setItem(yourTurnExpandedKey(), JSON.stringify(Array.from(set)));
        else localStorage.removeItem(yourTurnExpandedKey());
      } catch {}
      repaintReposForWidth();
    }

    // A repository's effective level: its own, else the hive's.
    function repoEffectiveLevel(repo) {
      return Number((repo && repo.acmmLevel) || (window._lastStatus || {}).acmmLevel || 0);
    }

    // The card view at L5/L6: your-turn rows stay, every other row is counted
    // into one summary line per band. Below L5, or when the operator expanded
    // the card, the groups come back untouched.
    function repoCardCollapseView(level, expanded, issueGroups, nonActionableGroups, prGroups) {
      if (level < YOUR_TURN_COLLAPSE_MIN_LEVEL) return { eligible: false, collapsed: false, issueGroups, nonActionableGroups, prGroups, hidden: [], hiddenTotal: 0 };
      if (expanded) return { eligible: true, collapsed: false, issueGroups, nonActionableGroups, prGroups, hidden: [], hiddenTotal: 0 };
      const hidden = [];
      const note = (kind, label, count) => { if (count) hidden.push({ kind, label, count }); };
      const keepIssues = issueGroups.map(g => {
        const kept = g.issues.filter(entry => yourTurnOf(entry.issue));
        note('issues', g.label, g.issues.length - kept.length);
        return Object.assign({}, g, { issues: kept });
      }).filter(g => g.issues.length);
      const keepNonActionable = nonActionableGroups.map(g => {
        const kept = g.issues.filter(yourTurnOf);
        note('issues', g.label, g.issues.length - kept.length);
        return Object.assign({}, g, { issues: kept });
      }).filter(g => g.issues.length);
      const keepPrs = prGroups.map(g => {
        const kept = g.prs.filter(entry => yourTurnOf(entry.pr));
        note('prs', g.label, g.prs.length - kept.length);
        return Object.assign({}, g, { prs: kept });
      }).filter(g => g.prs.length);
      return { eligible: true, collapsed: true, issueGroups: keepIssues, nonActionableGroups: keepNonActionable, prGroups: keepPrs, hidden, hiddenTotal: hidden.reduce((n, h) => n + h.count, 0) };
    }

    function repoCardCollapseSummaryText(view) {
      const part = (kind, title) => {
        const list = view.hidden.filter(h => h.kind === kind).map(h => h.label + ' ' + h.count);
        return list.length ? title + ': ' + list.join(' · ') : '';
      };
      return view.hiddenTotal + ' more — ' + [part('issues', 'Issues'), part('prs', 'PRs')].filter(Boolean).join(' · ');
    }

    function repoCardCollapseHTML(repoFull, view) {
      if (!view.eligible) return '';
      if (view.collapsed && !view.hiddenTotal) return '';
      const text = view.collapsed ? repoCardCollapseSummaryText(view) : 'Showing every item';
      return `<div class="yt-collapse"><button type="button" class="yt-collapse-btn" data-action="toggleYourTurnCollapse" data-arg0="${esc(repoFull)}" data-stop="1" data-prevent="1" aria-expanded="${view.collapsed ? 'false' : 'true'}" title="${view.collapsed ? 'Show the other rows on this card' : 'Collapse the card to its rows that are your turn'}"><span aria-hidden="true">${view.collapsed ? '▸' : '▾'}</span> ${esc(text)}</button></div>`;
    }
    // ── Label filter (hf) pure helpers (begin) ──────────────────────────────
    // Faceted label filter for the project cards (hivecommons/hive#12172).
    // Everything between the begin/end markers is DOM-free so
    // TestDashboardLabelFilterHelpers can run it under node.
    const HF_STORE_PREFIX = 'hive-label-filters:';
    const HF_STORE_VERSION = 1;
    const HF_DEFAULT_PRESET_KEY = 'preset:needs-human';
    const HF_KINDS = Object.freeze([{ key: 'issue', label: 'Issues' }, { key: 'pr', label: 'PRs' }]);
    const HF_STATES = Object.freeze([
      { key: 'unclaimed', label: 'Unclaimed', tip: 'Issue with no assignee or live claim' },
      { key: 'claimed', label: 'Claimed', tip: 'Issue assigned, claimed or withheld behind a claim' },
      { key: 'parked', label: 'Parked', tip: 'Held or hard-suppressed — out of every agent lane' },
      { key: 'covered-by-pr', label: 'Covered by PR', tip: 'Issue hive has verified a PR covers' }
    ]);
    const HF_AUTHORS = Object.freeze([
      { key: 'hive', label: 'Hive', tip: 'Filed by this hive\u2019s agents' },
      { key: 'human', label: 'Human', tip: 'Filed by a person' },
      { key: 'bot', label: 'Bot', tip: 'Filed by another bot (renovate, dependabot, …)' }
    ]);
    const HF_LABEL_TONE = Object.freeze({ 'stage/045-needs-decision': 'decision', 'stage/085-needs-decision': 'decision', 'needs-decision': 'decision', 'needs-spec': 'spec', 'needs-signal': 'signal', 'needs-human': 'human' });
    const HF_TEXT_DEBOUNCE_MS = 250;
    const HF_MAX_ROW_LABELS = 3;

    function hfEmptyFilter() {
      return { labels: [], terms: [], kinds: [], states: [], authors: [], matchAll: false };
    }
    function hfNormalizeFilter(raw) {
      const f = hfEmptyFilter();
      if (!raw || typeof raw !== 'object') return f;
      const strs = v => Array.isArray(v) ? v.map(x => String(x == null ? '' : x).trim()).filter(Boolean) : [];
      f.labels = Array.from(new Set(strs(raw.labels).map(l => l.toLowerCase())));
      f.terms = Array.from(new Set(strs(raw.terms).map(t => { const p = hfParseTerm(t); return p ? p.raw : ''; }).filter(Boolean)));
      f.kinds = strs(raw.kinds).filter(k => HF_KINDS.some(x => x.key === k));
      f.states = strs(raw.states).filter(k => HF_STATES.some(x => x.key === k));
      f.authors = strs(raw.authors).filter(k => HF_AUTHORS.some(x => x.key === k));
      f.matchAll = !!raw.matchAll;
      return f;
    }
    /** Canonical string for equality checks: label order matters (it is the sort priority); the rest is order-free. */
    function hfFilterSignature(filter) {
      const f = hfNormalizeFilter(filter);
      return JSON.stringify([f.labels, f.terms.slice().sort(), f.kinds.slice().sort(), f.states.slice().sort(), f.authors.slice().sort(), f.matchAll]);
    }
    function hfFilterIsEmpty(filter) {
      const f = hfNormalizeFilter(filter);
      return !f.labels.length && !f.terms.length && !f.kinds.length && !f.states.length && !f.authors.length;
    }
    /**
     * One text chip: `-` prefix excludes, surrounding quotes make a phrase,
     * otherwise every word must appear (AND within a term). Plain substring —
     * users never type regex.
     */
    function hfParseTerm(raw) {
      let s = String(raw == null ? '' : raw).trim();
      if (!s) return null;
      let negate = false;
      if (s.charAt(0) === '-') { negate = true; s = s.slice(1).trim(); if (!s) return null; }
      let phrase = false;
      const quoted = /^"(.*)"$/.exec(s) || /^\u201c(.*)\u201d$/.exec(s);
      if (quoted) { phrase = true; s = quoted[1].trim(); }
      s = s.toLowerCase();
      if (!s) return null;
      const words = phrase ? [s.replace(/\s+/g, ' ')] : s.split(/\s+/).filter(Boolean);
      return { raw: (negate ? '-' : '') + (phrase ? '"' + words[0] + '"' : words.join(' ')), negate, phrase, words };
    }
    function hfTermHits(term, haystack) {
      return !!term && term.words.every(w => haystack.indexOf(w) >= 0);
    }
    function hfAuthorKind(item, aiAuthor) {
      if (!item) return 'human';
      const author = String(item.author || '').trim();
      if (item.app_authored || item.hive_attributed || (aiAuthor && author && author.toLowerCase() === String(aiAuthor).toLowerCase())) return 'hive';
      if (/\[bot\]$/i.test(author) || /-bot$|\bbot\b/i.test(author)) return 'bot';
      return 'human';
    }
    /** Normalise one snapshot record into a filterable entry. kind is 'issue' or 'pr'. */
    function hfEntry(kind, item, repo, opts) {
      const o = opts || {};
      const labels = (item && Array.isArray(item.labels) ? item.labels : []).map(l => String(l == null ? '' : l).trim()).filter(Boolean);
      const states = new Set();
      if (o.held || o.bucket === 'filtered') states.add('parked');
      if (kind === 'issue') {
        const claimed = (Array.isArray(item.assignees) && item.assignees.length > 0) || !!item.claimed_by || o.bucket === 'claimed' || o.bucket === 'claimed_by_pr';
        states.add(claimed ? 'claimed' : 'unclaimed');
        if (o.coveringPR) states.add('covered-by-pr');
      }
      const hay = [item.title || '', '#' + (item.number || ''), labels.join(' '), item.body || ''].join('\n').toLowerCase();
      return {
        kind, item, repo: repo || item.repo || '', held: !!o.held, bucket: o.bucket || '',
        number: Number(item.number || 0), title: String(item.title || ''), url: String(item.url || ''),
        labels, labelsLower: new Set(labels.map(l => l.toLowerCase())), states, authorKind: hfAuthorKind(item, o.aiAuthor),
        createdAt: Date.parse(item.created_at || '') || 0, updatedAt: Date.parse(item.updated_at || item.created_at || '') || 0,
        haystack: hay
      };
    }
    function hfSplitTerms(filter) {
      const parsed = (filter.terms || []).map(hfParseTerm).filter(Boolean);
      return { positive: parsed.filter(t => !t.negate), negative: parsed.filter(t => t.negate) };
    }
    /** Labels + positive text terms form one criteria pool, combined by matchAll (AND) or any (OR). Negative terms always exclude. */
    function hfCriteriaMatch(filter, entry, terms) {
      const t = terms || hfSplitTerms(filter);
      if (t.negative.some(term => hfTermHits(term, entry.haystack))) return false;
      const checks = [];
      (filter.labels || []).forEach(l => checks.push(entry.labelsLower.has(String(l).toLowerCase())));
      t.positive.forEach(term => checks.push(hfTermHits(term, entry.haystack)));
      if (!checks.length) return true;
      return filter.matchAll ? checks.every(Boolean) : checks.some(Boolean);
    }
    function hfGroupMatch(selected, has) {
      return !selected || !selected.length || selected.some(has);
    }
    function hfGroupsMatch(filter, entry, skip) {
      return (skip === 'kinds' || hfGroupMatch(filter.kinds, k => k === entry.kind))
        && (skip === 'states' || hfGroupMatch(filter.states, k => entry.states.has(k)))
        && (skip === 'authors' || hfGroupMatch(filter.authors, k => k === entry.authorKind));
    }
    function hfEntryMatches(filter, entry, terms) {
      return hfGroupsMatch(filter, entry) && hfCriteriaMatch(filter, entry, terms);
    }
    function hfMatchingEntries(filter, entries) {
      const f = hfNormalizeFilter(filter);
      const terms = hfSplitTerms(f);
      return (entries || []).filter(e => hfEntryMatches(f, e, terms));
    }
    /** Priority index of the first filter label the entry carries; Infinity when none (or the filter has no labels). */
    function hfLabelRank(filter, entry) {
      const labels = (filter && filter.labels) || [];
      for (let i = 0; i < labels.length; i++) if (entry.labelsLower.has(String(labels[i]).toLowerCase())) return i;
      return Infinity;
    }
    /** Preset label priority first, then newest-first. */
    function hfSortEntries(entries, filter) {
      return (entries || []).slice().sort((a, b) => {
        const ra = hfLabelRank(filter, a), rb = hfLabelRank(filter, b);
        if (ra !== rb) return ra < rb ? -1 : 1;
        return (b.createdAt - a.createdAt) || (b.number - a.number);
      });
    }
    /**
     * Live facet counts. Labels: entries that pass the kind/state/author
     * groups and the negative terms and carry the label (independent of the
     * other positive criteria, so the chips read "how many would this add").
     * Other groups: entries matching the filter with that group swapped for
     * the single value.
     */
    function hfFacetCounts(filter, entries) {
      const f = hfNormalizeFilter(filter);
      const terms = hfSplitTerms(f);
      const labels = new Map();
      const kinds = {}, states = {}, authors = {};
      (entries || []).forEach(e => {
        if (hfGroupsMatch(f, e) && !terms.negative.some(t => hfTermHits(t, e.haystack))) {
          e.labels.forEach(l => {
            const key = l.toLowerCase();
            const cur = labels.get(key) || { label: l, count: 0 };
            cur.count++;
            labels.set(key, cur);
          });
        }
        const crit = hfCriteriaMatch(f, e, terms);
        if (crit && hfGroupsMatch(f, e, 'kinds')) kinds[e.kind] = (kinds[e.kind] || 0) + 1;
        if (crit && hfGroupsMatch(f, e, 'states')) e.states.forEach(s => { states[s] = (states[s] || 0) + 1; });
        if (crit && hfGroupsMatch(f, e, 'authors')) authors[e.authorKind] = (authors[e.authorKind] || 0) + 1;
      });
      f.labels.forEach(l => { if (!labels.has(l)) labels.set(l, { label: l, count: 0 }); });
      return { labels, kinds, states, authors };
    }
    function hfToggleIn(list, value) {
      const idx = list.indexOf(value);
      if (idx >= 0) list.splice(idx, 1); else list.push(value);
      return list;
    }
    function hfPresetFilter(preset) {
      return hfNormalizeFilter({ labels: (preset && preset.labels) || [], states: (preset && preset.states) || [] });
    }
    function hfLabelToneClass(label) {
      const key = String(label || '').toLowerCase();
      if (HF_LABEL_TONE[key]) return 'hf-label-' + HF_LABEL_TONE[key];
      if (/^hold(\/|$)|^on-hold$|hold\//.test(key)) return 'hf-label-hold';
      if (key === 'stage/stalled') return 'hf-label-hold';
      return '';
    }
    function hfDefaultStore() {
      return { v: HF_STORE_VERSION, activeKey: HF_DEFAULT_PRESET_KEY, active: null, saved: [], hiddenPresets: [], presetPins: {}, order: [] };
    }
    function hfNormalizeStore(raw) {
      const d = hfDefaultStore();
      if (!raw || typeof raw !== 'object') return d;
      d.activeKey = typeof raw.activeKey === 'string' ? raw.activeKey : d.activeKey;
      d.active = raw.active ? hfNormalizeFilter(raw.active) : null;
      d.saved = Array.isArray(raw.saved) ? raw.saved.filter(s => s && s.id && s.name).map(s => ({ id: String(s.id), name: String(s.name).slice(0, 60), filter: hfNormalizeFilter(s.filter), pinned: !!s.pinned })) : [];
      d.hiddenPresets = Array.isArray(raw.hiddenPresets) ? raw.hiddenPresets.map(String) : [];
      d.presetPins = raw.presetPins && typeof raw.presetPins === 'object' ? Object.assign({}, raw.presetPins) : {};
      d.order = Array.isArray(raw.order) ? raw.order.map(String) : [];
      return d;
    }
    /** Resolve the key ('preset:<key>' | 'saved:<id>' | '') to {name, filter, source}. */
    function hfResolveKey(store, presets, key) {
      if (key && key.indexOf('preset:') === 0) {
        const p = (presets || []).find(x => 'preset:' + x.key === key);
        if (p) return { key, name: p.label, filter: hfPresetFilter(p), source: 'preset', preset: p };
      }
      if (key && key.indexOf('saved:') === 0) {
        const s = (store.saved || []).find(x => 'saved:' + x.id === key);
        if (s) return { key, name: s.name, filter: hfNormalizeFilter(s.filter), source: 'saved', saved: s };
      }
      return { key: '', name: 'Custom', filter: hfEmptyFilter(), source: 'custom' };
    }
    /** The filter in force: the explicit edit if there is one, else the active preset/saved filter. */
    function hfActiveFilter(store, presets) {
      if (store.active) return hfNormalizeFilter(store.active);
      return hfResolveKey(store, presets, store.activeKey).filter;
    }
    function hfPinnedKeys(store, presets) {
      const keys = [];
      (presets || []).forEach(p => {
        const k = 'preset:' + p.key;
        const pinned = Object.prototype.hasOwnProperty.call(store.presetPins || {}, p.key) ? !!store.presetPins[p.key] : k === HF_DEFAULT_PRESET_KEY;
        if (pinned && (store.hiddenPresets || []).indexOf(p.key) < 0) keys.push(k);
      });
      (store.saved || []).forEach(s => { if (s.pinned) keys.push('saved:' + s.id); });
      const order = store.order || [];
      return keys.sort((a, b) => {
        const ia = order.indexOf(a), ib = order.indexOf(b);
        return (ia < 0 ? order.length : ia) - (ib < 0 ? order.length : ib) || keys.indexOf(a) - keys.indexOf(b);
      });
    }
    function hfNewId() {
      return 'f' + Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
    }
    // ── Label filter (hf) pure helpers (end) ────────────────────────────────

    let _hfStore = null;
    let _hfStoreKey = '';
    const _hfUI = { editorOpen: false, confirmDelete: '', showHidden: false, draftTerm: '', textTimer: null, crossRepo: false };
    function hfStoreKey() { return HF_STORE_PREFIX + miniHiveKey(); }
    function hfStore() {
      const key = hfStoreKey();
      if (!_hfStore || _hfStoreKey !== key) {
        let raw = null;
        try { raw = JSON.parse(localStorage.getItem(key) || 'null'); } catch (e) { raw = null; }
        _hfStore = hfNormalizeStore(raw);
        _hfStoreKey = key;
      }
      return _hfStore;
    }
    function hfSaveStore() {
      try { localStorage.setItem(hfStoreKey(), JSON.stringify(hfStore())); } catch (e) { /* storage unavailable */ }
    }
    function hfPresets() {
      const list = ((window._lastStatus || {}).labelPresets) || [];
      return Array.isArray(list) ? list.filter(p => p && p.key) : [];
    }
    function hfVisiblePresets() {
      const hidden = hfStore().hiddenPresets || [];
      return hfPresets().filter(p => hidden.indexOf(p.key) < 0);
    }
    function hfCurrent() {
      const store = hfStore();
      const presets = hfPresets();
      const resolved = hfResolveKey(store, presets, store.activeKey);
      const filter = hfActiveFilter(store, presets);
      const modified = !!store.active && hfFilterSignature(store.active) !== hfFilterSignature(resolved.filter);
      return { store, presets, resolved, filter, modified, name: resolved.name };
    }
    /** Every open issue/PR of a repo as filter entries, from the snapshot the card already renders. */
    function hfRepoEntries(repo) {
      if (!repo) return [];
      const name = repo.full || repo.name || '';
      const aiAuthor = window._aiAuthor || '';
      const seen = new Set();
      const out = [];
      const push = (kind, item, opts) => {
        if (!item || !item.number) return;
        const key = kind + ':' + item.number;
        if (seen.has(key)) return;
        seen.add(key);
        out.push(hfEntry(kind, item, name, Object.assign({ aiAuthor }, opts)));
      };
      (repo.actionableIssues || []).forEach(i => push('issue', i, { coveringPR: !!issueCoveringPR(i) }));
      (repo.heldIssues || []).forEach(i => push('issue', i, { held: true, coveringPR: !!issueCoveringPR(i) }));
      (repo.nonActionableIssues || []).forEach(i => push('issue', i, { bucket: String(i.bucket || ''), coveringPR: !!issueCoveringPR(i) }));
      (repo.openPrs || []).forEach(p => push('pr', p, {}));
      (repo.heldPrs || []).forEach(p => push('pr', p, { held: true }));
      return out;
    }
    function hfAllEntries(repos) {
      return (repos || (window._lastStatus || {}).repos || []).reduce((acc, r) => acc.concat(hfRepoEntries(r)), []);
    }
    /** Filter with the still-being-typed text term folded in, for live counts. */
    function hfEffectiveFilter(filter) {
      const draft = hfParseTerm(_hfUI.draftTerm);
      if (!draft) return filter;
      const f = hfNormalizeFilter(filter);
      if (f.terms.indexOf(draft.raw) < 0) f.terms = f.terms.concat([draft.raw]);
      return f;
    }
    function hfSectionId(repoFull) {
      return 'hf-repo-' + String(repoFull || '').toLowerCase().replace(/[^a-z0-9]+/g, '-');
    }
    function hfSectionCollapsed(sectionId, count) {
      let raw = null;
      try { raw = localStorage.getItem(SECTION_LS_PREFIX + sectionId); } catch (e) { raw = null; }
      if (raw === '1') return true;
      if (raw === '0') return false;
      return count === 0;
    }
    const HF_PR_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true" fill="currentColor"><path d="M4.5 3a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0Zm0 10a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0ZM3.75 5.25v5.5h-1.5v-5.5h1.5ZM14.5 13a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0Zm-.75-2.25h-1.5V6.5a1 1 0 0 0-1-1H9.56l1.22 1.22-1.06 1.06L6.69 4.75l3.03-3.03 1.06 1.06L9.56 4h1.69a2.5 2.5 0 0 1 2.5 2.5v4.25Z"/></svg>';
    const HF_ISSUE_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true" fill="currentColor"><path d="M8 1.5a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13ZM0 8a8 8 0 1 1 16 0A8 8 0 0 1 0 8Zm9 3a1 1 0 1 1-2 0 1 1 0 0 1 2 0Zm-.25-6.25v4h-1.5v-4h1.5Z"/></svg>';
    function hfRowLabels(entry, filter) {
      const wanted = (filter && filter.labels) || [];
      let show = entry.labels.filter(l => wanted.indexOf(l.toLowerCase()) >= 0);
      show.sort((a, b) => wanted.indexOf(a.toLowerCase()) - wanted.indexOf(b.toLowerCase()));
      if (!show.length) show = entry.labels.slice();
      const extra = show.length - HF_MAX_ROW_LABELS;
      return show.slice(0, HF_MAX_ROW_LABELS).map(l => `<span class="hf-label ${hfLabelToneClass(l)}" title="${esc(l)}">${esc(l)}</span>`).join('')
        + (extra > 0 ? `<span class="hf-label" title="${esc(entry.labels.join(', '))}">+${extra}</span>` : '');
    }
    function hfRowHtml(entry, filter) {
      const base = (window._githubBaseUrl || 'https://github.com') + '/' + entry.repo;
      const url = entry.url || (base + (entry.kind === 'pr' ? '/pull/' : '/issues/') + entry.number);
      const age = pillAge(entry.item.created_at);
      const tip = (entry.kind === 'pr' ? 'PR ' : 'Issue ') + '#' + entry.number + ': ' + entry.title + (entry.item.author ? ' — @' + entry.item.author : '') + (entry.labels.length ? ' — labels: ' + entry.labels.join(', ') : '') + (entry.held ? ' — held' : '') + (entry.bucket ? ' — ' + entry.bucket : '');
      return `<div class="hf-item hf-item-${entry.kind}"><a class="hf-item-link" href="${esc(url)}" target="_blank" rel="noopener" title="${esc(tip)}"><span class="hf-item-icon ${entry.kind}">${entry.kind === 'pr' ? HF_PR_ICON : HF_ISSUE_ICON}</span><span class="hf-item-num">#${entry.number}</span><span class="hf-item-title">${esc(entry.title)}</span></a><span class="hf-item-labels">${hfRowLabels(entry, filter)}</span><span class="hf-item-age" title="${esc(entry.item.created_at || '')}">${esc(age)}</span></div>`;
    }
    /** The collapsible "Filter" section inside one repo card. */
    function hfRepoSectionHtml(repo) {
      const cur = hfCurrent();
      const repoFull = repo.full || repo.name || '';
      const filter = hfEffectiveFilter(cur.filter);
      const entries = hfSortEntries(hfMatchingEntries(filter, hfRepoEntries(repo)), filter);
      const id = hfSectionId(repoFull);
      const collapsed = hfSectionCollapsed(id, entries.length);
      const name = cur.name + (cur.modified ? ' *' : '');
      const rows = entries.length ? entries.map(e => hfRowHtml(e, filter)).join('') : `<div class="hf-empty">${hfFilterIsEmpty(filter) ? 'No filter — open the editor to pick labels or text' : 'Nothing in ' + esc(repoFull) + ' matches this filter'}</div>`;
      return `<div class="hf-section${collapsed ? ' collapsed' : ''}" id="${id}" data-hf-repo="${esc(repoFull)}" data-hf-count="${entries.length}">`
        + `<div class="hf-header" role="button" tabindex="0" aria-expanded="${collapsed ? 'false' : 'true'}" aria-controls="${id}-body" data-action="hfToggleSection" data-keydown-action="hfToggleSection" data-keys="Enter, " data-arg0="${id}" data-arg-types="s" title="${esc(cur.filter.labels.length ? 'Labels: ' + cur.filter.labels.join(', ') : 'Filtered view')}${esc(cur.filter.terms.length ? ' · text: ' + cur.filter.terms.join(', ') : '')}"><span class="section-chevron${collapsed ? ' collapsed' : ''}">▼</span><span class="hf-title">Filter · ${esc(name)}</span><span class="badge-count hf-count"${entries.length ? '' : ' data-zero'}>${entries.length}</span><button type="button" class="hf-edit" data-action="hfOpenEditor" data-stop="1" title="Edit the filter (applies to every project card)">edit</button></div>`
        + `<div class="hf-body${collapsed ? ' collapsed' : ''}" id="${id}-body">${rows}</div></div>`;
    }
    function hfToggleSection(sectionId) {
      const section = document.getElementById(sectionId);
      if (!section) return;
      const body = section.querySelector('.hf-body');
      const collapsed = !section.classList.contains('collapsed');
      writeSectionCollapsedKey(sectionId, collapsed);
      if (!collapsed) {
        // Make the default explicit so an empty section the user opened stays open.
        try { localStorage.setItem(SECTION_LS_PREFIX + sectionId, '0'); } catch (e) { /* storage unavailable */ }
      }
      section.classList.toggle('collapsed', collapsed);
      if (body) body.classList.toggle('collapsed', collapsed);
      const chevron = section.querySelector('.section-chevron');
      if (chevron) chevron.classList.toggle('collapsed', collapsed);
      const header = section.querySelector('.hf-header');
      if (header) header.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    }
    function hfRerender() {
      if (typeof renderRepos === 'function' && window._lastStatus) renderRepos(window._lastStatus.repos || []);
      hfRenderEditor(true);
      hfRenderPinned();
      hfRenderCrossRepo();
    }
    function hfSetActive(filter) {
      const store = hfStore();
      store.active = hfNormalizeFilter(filter);
      hfSaveStore();
      hfRerender();
    }
    function hfActivate(key) {
      const store = hfStore();
      store.activeKey = String(key || '');
      store.active = null;
      _hfUI.confirmDelete = '';
      hfSaveStore();
      hfRerender();
    }
    function hfOpenEditor() {
      _hfUI.editorOpen = true;
      hfRenderEditor();
      const el = document.getElementById('hf-editor');
      if (el && el.scrollIntoView) el.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    }
    function hfToggleEditor() {
      _hfUI.editorOpen = !_hfUI.editorOpen;
      hfRenderEditor();
    }
    function hfToggleLabel(label) {
      const f = hfCurrent().filter;
      hfToggleIn(f.labels, String(label).toLowerCase());
      hfSetActive(f);
    }
    function hfToggleFacet(group, value) {
      const f = hfCurrent().filter;
      if (!Array.isArray(f[group])) return;
      hfToggleIn(f[group], value);
      hfSetActive(f);
    }
    function hfSetMatchAll(on) {
      const f = hfCurrent().filter;
      f.matchAll = !!on;
      hfSetActive(f);
    }
    function hfAddTerm(raw) {
      const term = hfParseTerm(raw);
      _hfUI.draftTerm = '';
      if (!term) { hfRerender(); return; }
      const f = hfCurrent().filter;
      if (f.terms.indexOf(term.raw) < 0) f.terms.push(term.raw);
      hfSetActive(f);
      const input = document.getElementById('hf-term-input');
      if (input) { input.value = ''; input.focus(); }
    }
    function hfRemoveTerm(raw) {
      const f = hfCurrent().filter;
      f.terms = f.terms.filter(t => t !== raw);
      hfSetActive(f);
    }
    function hfTermInput(value) {
      _hfUI.draftTerm = String(value || '');
      if (_hfUI.textTimer) clearTimeout(_hfUI.textTimer);
      _hfUI.textTimer = setTimeout(() => {
        _hfUI.textTimer = null;
        hfRenderFacets();
        if (typeof renderRepos === 'function' && window._lastStatus) renderRepos(window._lastStatus.repos || []);
        if (typeof hfRenderPinned === 'function') hfRenderPinned();
      }, HF_TEXT_DEBOUNCE_MS);
    }
    function hfTermKey(event) {
      const input = event && event.target;
      if (!input) return;
      if (event.key === 'Enter' || event.key === ',') {
        event.preventDefault();
        hfAddTerm(input.value);
      } else if (event.key === 'Escape') {
        input.value = '';
        hfTermInput('');
      }
    }
    function hfReset() {
      const store = hfStore();
      store.activeKey = '';
      store.active = hfEmptyFilter();
      hfSaveStore();
      hfRerender();
    }
    function hfSaveAsNew() {
      const cur = hfCurrent();
      const input = document.getElementById('hf-name-input');
      const name = String((input && input.value) || '').trim();
      if (!name) { showToast('Give the filter a name first', 'error'); if (input) input.focus(); return; }
      if (hfFilterIsEmpty(cur.filter)) { showToast('Pick at least one label, facet or text term before saving', 'error'); return; }
      const id = hfNewId();
      cur.store.saved.push({ id, name: name.slice(0, 60), filter: hfNormalizeFilter(cur.filter), pinned: false });
      cur.store.activeKey = 'saved:' + id;
      cur.store.active = null;
      hfSaveStore();
      showToast('Saved filter “' + name + '”', 'success');
      hfRerender();
    }
    function hfUpdateSaved() {
      const cur = hfCurrent();
      if (cur.resolved.source !== 'saved') return;
      cur.resolved.saved.filter = hfNormalizeFilter(cur.filter);
      cur.store.active = null;
      hfSaveStore();
      showToast('Updated “' + cur.resolved.saved.name + '”', 'success');
      hfRerender();
    }
    function hfRenameSaved() {
      const cur = hfCurrent();
      if (cur.resolved.source !== 'saved') return;
      const input = document.getElementById('hf-name-input');
      const name = String((input && input.value) || '').trim();
      if (!name) { showToast('Enter the new name first', 'error'); if (input) input.focus(); return; }
      cur.resolved.saved.name = name.slice(0, 60);
      hfSaveStore();
      hfRerender();
    }
    function hfAskDelete(id) {
      _hfUI.confirmDelete = String(id || '');
      hfRenderEditor();
    }
    function hfDeleteSaved(id) {
      const store = hfStore();
      const victim = store.saved.find(s => s.id === id);
      store.saved = store.saved.filter(s => s.id !== id);
      if (store.activeKey === 'saved:' + id) { store.activeKey = ''; store.active = victim ? hfNormalizeFilter(victim.filter) : hfEmptyFilter(); }
      _hfUI.confirmDelete = '';
      hfSaveStore();
      if (victim) showToast('Deleted filter “' + victim.name + '”', 'info');
      hfRerender();
    }
    function hfHidePreset(key) {
      const store = hfStore();
      if (store.hiddenPresets.indexOf(key) < 0) store.hiddenPresets.push(key);
      if (store.activeKey === 'preset:' + key) { store.activeKey = ''; store.active = hfCurrent().filter; }
      hfSaveStore();
      hfRerender();
    }
    function hfUnhidePreset(key) {
      const store = hfStore();
      store.hiddenPresets = store.hiddenPresets.filter(k => k !== key);
      hfSaveStore();
      hfRerender();
    }
    function hfToggleShowHidden() {
      _hfUI.showHidden = !_hfUI.showHidden;
      hfRenderEditor();
    }
    function hfTogglePin(key) {
      const store = hfStore();
      const presets = hfPresets();
      const pinned = hfPinnedKeys(store, presets).indexOf(key) >= 0;
      if (key.indexOf('preset:') === 0) store.presetPins[key.slice('preset:'.length)] = !pinned;
      else if (key.indexOf('saved:') === 0) { const s = store.saved.find(x => 'saved:' + x.id === key); if (s) s.pinned = !pinned; }
      hfSaveStore();
      hfRerender();
    }
    function hfChipHtml(label, active, action, arg, count, tip, extraClass) {
      return `<button type="button" class="hf-chip${active ? ' active' : ''}${extraClass ? ' ' + extraClass : ''}" data-action="${action}" data-arg0="${esc(arg)}" data-arg-types="s" title="${esc(tip || label)}">${esc(label)}${count == null ? '' : ` <span class="hf-chip-count">${count}</span>`}</button>`;
    }
    function hfFacetsHtml(cur, entries) {
      const filter = hfEffectiveFilter(cur.filter);
      const counts = hfFacetCounts(filter, entries);
      const labelRows = Array.from(counts.labels.values()).sort((a, b) => {
        const sa = filter.labels.indexOf(a.label.toLowerCase()), sb = filter.labels.indexOf(b.label.toLowerCase());
        if ((sa >= 0) !== (sb >= 0)) return sa >= 0 ? -1 : 1;
        if (sa >= 0 && sb >= 0) return sa - sb;
        return b.count - a.count || a.label.localeCompare(b.label);
      });
      const labelChips = labelRows.length ? labelRows.map(r => hfChipHtml(r.label, filter.labels.indexOf(r.label.toLowerCase()) >= 0, 'hfToggleLabel', r.label, r.count, r.count + ' open item' + (r.count === 1 ? '' : 's') + ' carry ' + r.label)).join('') : '<span class="hf-muted">No labels on any open item yet</span>';
      const group = (title, defs, key, countMap, tipFn) => `<div class="hf-facet-group hf-facet-${key}"><div class="hf-facet-title">${title}</div><div class="hf-facet-chips">${defs.map(d => hfChipHtml(d.label, filter[key].indexOf(d.key) >= 0, 'hfToggleFacet' + key.charAt(0).toUpperCase() + key.slice(1), d.key, countMap[d.key] || 0, tipFn ? tipFn(d) : d.tip || d.label)).join('')}</div></div>`;
      const mode = `<span class="hf-mode" title="How selected labels and text terms combine"><button type="button" class="${filter.matchAll ? '' : 'active'}" data-action="hfSetMatchAll" data-arg0="false" data-arg-types="j">match any</button><button type="button" class="${filter.matchAll ? 'active' : ''}" data-action="hfSetMatchAll" data-arg0="true" data-arg-types="j">match all</button></span>`;
      return `<div class="hf-facet-group hf-facet-labels"><div class="hf-facet-title">Labels ${mode}</div><div class="hf-facet-chips">${labelChips}</div></div>`
        + group('Type', HF_KINDS, 'kinds', counts.kinds)
        + group('State', HF_STATES, 'states', counts.states)
        + group('Author', HF_AUTHORS, 'authors', counts.authors);
    }
    function hfTermsHtml(cur) {
      const terms = cur.filter.terms.map(t => {
        const p = hfParseTerm(t);
        const tip = p && p.negate ? 'Excludes items whose title, number, labels or body contain ' + t.slice(1) : (p && p.phrase ? 'Exact phrase' : 'Every word must appear');
        return `<span class="hf-chip active${p && p.negate ? ' hf-chip-exclude' : ''}" title="${esc(tip)}">${esc(t)}<button type="button" class="hf-chip-x" data-action="hfRemoveTerm" data-arg0="${esc(t)}" data-arg-types="s" aria-label="Remove ${esc(t)}">×</button></span>`;
      }).join('');
      return `<span class="hf-row-label">Text</span>${terms}<input id="hf-term-input" class="hf-input" type="text" autocomplete="off" spellcheck="false" placeholder="keyword · &quot;exact phrase&quot; · -exclude · Enter adds" value="${esc(_hfUI.draftTerm)}" data-input-action="hfTermInput" data-keydown-action="hfTermKey" data-keys="*" data-arg-types="v" title="Case-insensitive match over title, number, labels and body excerpt. Quote a phrase, prefix - to exclude. Enter or comma adds the chip.">`;
    }
    function hfManageHtml(cur) {
      const store = cur.store;
      const r = cur.resolved;
      const saved = r.source === 'saved';
      const preset = r.source === 'preset';
      const pinned = hfPinnedKeys(store, cur.presets).indexOf(r.key) >= 0;
      const bits = [];
      bits.push(`<input id="hf-name-input" class="hf-input" type="text" maxlength="60" placeholder="${saved ? 'New name' : 'Name this filter'}" value="${esc(saved ? r.name : '')}" data-keydown-action="hfNameKey" data-keys="Enter" data-arg-types="e" title="Name for Save as new / Rename">`);
      bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfSaveAsNew" title="Save the current selection as a new named filter">Save as new</button>`);
      if (saved && cur.modified) bits.push(`<button type="button" class="hv-btn btn-primary btn-sm" data-action="hfUpdateSaved" title="Overwrite “${esc(r.name)}” with the current selection">Update “${esc(r.name)}”</button>`);
      if (saved) bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfRenameSaved" title="Rename to the name typed on the left">Rename</button>`);
      if (saved) {
        if (_hfUI.confirmDelete === r.saved.id) bits.push(`<span class="hf-confirm">Delete “${esc(r.name)}”? <button type="button" class="hv-btn btn-danger btn-sm" data-action="hfDeleteSaved" data-arg0="${esc(r.saved.id)}" data-arg-types="s">Delete</button><button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfAskDelete" data-arg0="" data-arg-types="s">Cancel</button></span>`);
        else bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfAskDelete" data-arg0="${esc(r.saved.id)}" data-arg-types="s" title="Delete this saved filter">Delete</button>`);
      }
      if (preset) bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfHidePreset" data-arg0="${esc(r.preset.key)}" data-arg-types="s" title="Hide this built-in preset from the chips row (restore it from “hidden”)">Hide preset</button>`);
      if (saved || preset) bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfTogglePin" data-arg0="${esc(r.key)}" data-arg-types="s" aria-pressed="${pinned ? 'true' : 'false'}" title="${pinned ? 'Unpin from the sidebar' : 'Pin to the sidebar with a live count across all projects'}">${pinned ? '★ Pinned' : '☆ Pin to sidebar'}</button>`);
      bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfReset" title="Clear every facet and text term">Clear</button>`);
      if (cur.modified && !saved) bits.push(`<span class="hf-muted">Unsaved changes to ${esc(r.name)}</span>`);
      return bits.join('');
    }
    function hfNameKey(event) {
      if (!event || event.key !== 'Enter') return;
      event.preventDefault();
      if (hfCurrent().resolved.source === 'saved') hfRenameSaved(); else hfSaveAsNew();
    }
    function hfPresetsRowHtml(cur, entries) {
      const store = cur.store;
      const chips = [];
      const countFor = f => hfMatchingEntries(f, entries).length;
      hfVisiblePresets().forEach(p => {
        const key = 'preset:' + p.key;
        chips.push(hfChipHtml(p.label, store.activeKey === key, 'hfActivate', key, countFor(hfPresetFilter(p)), (p.description || p.label) + (p.labels && p.labels.length ? ' — ' + p.labels.join(', ') : ''), store.activeKey === key && cur.modified ? 'hf-chip-modified' : ''));
      });
      store.saved.forEach(s => {
        const key = 'saved:' + s.id;
        chips.push(hfChipHtml((s.pinned ? '★ ' : '') + s.name, store.activeKey === key, 'hfActivate', key, countFor(s.filter), 'Saved filter' + (s.filter.labels.length ? ' — ' + s.filter.labels.join(', ') : '') + (s.filter.terms.length ? ' — ' + s.filter.terms.join(', ') : ''), store.activeKey === key && cur.modified ? 'hf-chip-modified' : ''));
      });
      if (!store.activeKey) chips.push(hfChipHtml('Custom', true, 'hfActivate', '', countFor(cur.filter), 'The current unsaved selection'));
      const hidden = hfPresets().filter(p => store.hiddenPresets.indexOf(p.key) >= 0);
      let hiddenHtml = '';
      if (hidden.length) {
        hiddenHtml = `<button type="button" class="hf-link" data-action="hfToggleShowHidden">${hidden.length} hidden</button>`
          + (_hfUI.showHidden ? hidden.map(p => `<button type="button" class="hf-chip" data-action="hfUnhidePreset" data-arg0="${esc(p.key)}" data-arg-types="s" title="Restore the ${esc(p.label)} preset">↩ ${esc(p.label)}</button>`).join('') : '');
      }
      return `<span class="hf-row-label">Filter</span>${chips.join('')}${hiddenHtml}<button type="button" class="hf-chip-x hf-editor-close" data-action="hfToggleEditor" aria-label="Close the filter editor" title="Close">×</button>`;
    }
    function hfRenderFacets() {
      const el = document.getElementById('hf-facets');
      if (!el) return;
      const cur = hfCurrent();
      setIfChanged(el, hfFacetsHtml(cur, hfAllEntries()));
    }
    function hfRenderEditor(force) {
      const el = document.getElementById('hf-editor');
      if (!el) return;
      el.hidden = !_hfUI.editorOpen;
      const btn = document.getElementById('repos-filter-editor-btn');
      if (btn) btn.setAttribute('aria-pressed', _hfUI.editorOpen ? 'true' : 'false');
      if (!_hfUI.editorOpen) return;
      const cur = hfCurrent();
      const entries = hfAllEntries();
      const presetsEl = document.getElementById('hf-presets');
      const manageEl = document.getElementById('hf-manage');
      const termsEl = document.getElementById('hf-terms');
      const facetsEl = document.getElementById('hf-facets');
      // A poll-time repaint must not yank the caret out of a field the user is typing in.
      const active = document.activeElement;
      const typingIn = node => !force && !!active && active.tagName === 'INPUT' && !!node && node.contains(active);
      if (presetsEl) setIfChanged(presetsEl, hfPresetsRowHtml(cur, entries));
      if (manageEl && !typingIn(manageEl)) setIfChanged(manageEl, hfManageHtml(cur));
      if (termsEl && !typingIn(termsEl)) setIfChanged(termsEl, hfTermsHtml(cur));
      if (facetsEl) setIfChanged(facetsEl, hfFacetsHtml(cur, entries));
    }
    function hfSummaryLabel() {
      const cur = hfCurrent();
      return cur.name + (cur.modified ? ' *' : '');
    }
