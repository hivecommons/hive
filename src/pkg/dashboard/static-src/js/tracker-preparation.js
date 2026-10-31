    // Preparation decorates the existing rows; it never sorts or admits them.
    var _stageTrackerPreparationShown = new Map();
    var _stageTrackerPreparationRendered = new Map();
    var _stageTrackerPreparationInline = [];
    var _stageTrackerPreparationNow = 0;
    const STAGE_TRACKER_PREPARATION_ACCENT = 3;
    function stageTrackerPreparationKey(repo, number) {
      return String(repo || '').trim().toLowerCase() + '/pr#' + Number(number);
    }
    function stageTrackerPreparationRows(rows, stages) {
      const byKey = new Map();
      const items = (Array.isArray(rows) ? rows : []).map(e => e && e.item ? e.item : e && !e.group ? e : null);
      const adopted = stageTrackerAdoptOrphanPRs(items);
      for (const it of adopted) {
        if (!it) continue;
        const put = pr => {
          const key = stageTrackerPreparationKey(it.repo, pr.number);
          if (it.kind === 'pr' || !byKey.has(key)) byKey.set(key, { item: pr, host: it, nested: it.kind === 'issue' });
        };
        if (it.kind === 'pr') put(it);
        else if (it.kind === 'issue') {
          for (const lp of stageTrackerLinkedSorted(it, stages)) {
            if (stageTrackerLinkedPROpen(lp, it)) put(stageTrackerLinkedPRItem(it, lp));
          }
        }
      }
      return byKey;
    }
    function stageTrackerPreparationVisualModel(data, listedRows, pageRows, stages) {
      const known = stageTrackerPreparationRows(data && data.items, stages);
      const listed = stageTrackerPreparationRows(listedRows, stages);
      const page = stageTrackerPreparationRows(pageRows, stages);
      return (Array.isArray(data && data.base_preparation) ? data.base_preparation : []).map(q => {
        const out = { queue: q, owner: null, waiting: [], unverified: [], members: [], inactive: 0 };
        let uncertainAhead = false;
        let position = 0;
        const member = (p, role) => {
          if (!p || p.state === 'inactive') { if (p) out.inactive++; return null; }
          const key = stageTrackerPreparationKey(q.repo, p.number);
          const found = known.get(key);
          const here = page.get(key);
          if (found && (!stageTrackerLinkedPROpen(found.item, found.host) || found.item.hold
            || (Array.isArray(found.item.modifiers) ? found.item.modifiers : []).some(m => m === 'held' || m === 'draft'))) { out.inactive++; return null; }
          const exact = p.state === 'current' && found && stageTrackerLinkedPROpen(found.item, found.host)
            && found.item.head && found.item.head === p.head_sha
            && (!p.current_head || p.current_head === found.item.head) && found.item.base_ref === q.base_ref
            && (!here || (here.item.head === p.head_sha && here.item.base_ref === q.base_ref));
          const drawable = here && stageTrackerLinkedPROpen(here.item, here.host) && here.item.base_ref === q.base_ref;
          const row = drawable ? here : found;
          const entry = { ticket: p, role, position: 0, state: exact ? 'current' : 'unverified',
            sequence: out.members.length, location: drawable ? 'page' : listed.has(key) ? 'offpage' : 'filtered',
            row, head: row && row.item.head || '', base: q.base_ref, ticketHead: p.head_sha, ticketState: p.state };
          out.members.push(entry);
          if (!exact) {
            out.unverified.push(entry);
            if (role === 'waiting') uncertainAhead = true;
            return null;
          }
          if (role === 'waiting') {
            position++;
            entry.position = uncertainAhead ? 0 : position;
          }
          return entry;
        };
        if (q.state === 'observed') {
          out.owner = member(q.owner, 'owner');
          for (const p of (Array.isArray(q.waiting) ? q.waiting : [])) {
            const entry = member(p, 'waiting');
            if (entry) out.waiting.push(entry);
          }
        }
        return out;
      });
    }
    function stageTrackerPreparationPlace(entry) {
      if (entry.state === 'unverified') return 'Recorded preparation ' + (entry.role === 'owner' ? 'owner' : 'ticket') + ' · unverified';
      if (entry.role === 'owner') return 'Preparing';
      return entry.position > 0 ? 'Waiting ' + stageTrackerOrdinal(entry.position) + ' for preparation' : 'Waiting for preparation · position unverified';
    }
    function stageTrackerPreparationVisualSync(data, listedRows, pageRows, stages, nowMs) {
      _stageTrackerPreparationShown = new Map();
      _stageTrackerPreparationRendered = new Map();
      _stageTrackerPreparationNow = nowMs;
      _stageTrackerPreparationInline = data && Array.isArray(data.items)
        ? stageTrackerPreparationVisualModel(data, listedRows, pageRows, stages)
        : [{ queue: { state: 'unavailable', message: 'Waiting for a current tracker snapshot' }, members: [], unverified: [] }];
      _stageTrackerPreparationInline.forEach((group, index) => {
        for (const e of group.members) {
          e.groupIndex = index;
          if (e.location === 'page' && e.row) _stageTrackerPreparationShown.set(stageTrackerPreparationKey(group.queue.repo, e.ticket.number), e);
        }
      });
    }
    function stageTrackerPreparationEntry(item) {
      if (!item || (item.kind !== 'pr' && !item.via_pr)) return null;
      const number = item.via_pr && item.preparation ? item.preparation.number : item.number;
      const e = _stageTrackerPreparationShown.get(stageTrackerPreparationKey(item.repo, number));
      return e && e.head === item.head && (!item.base_ref || item.base_ref === e.base) ? e : null;
    }
    function stageTrackerPreparationRowInfo(item, stages) {
      if (!item || (typeof stageTrackerTrainKey === 'function' && stageTrackerTrainKey(item))) return null;
      const prs = item.kind === 'pr' ? [item] : item.kind === 'issue'
        ? stageTrackerLinkedSorted(item, stages).map(lp => stageTrackerLinkedPRItem(item, lp)) : [];
      const members = prs.map(pr => stageTrackerPreparationEntry(pr));
      if (!members.length || members.some(e => !e) || members.some(e => e.groupIndex !== members[0].groupIndex)) return null;
      const ordered = members.every((e, i) => !i || e.sequence > members[i - 1].sequence);
      return { groupIndex: members[0].groupIndex, members, ordered };
    }
    function stageTrackerPreparationAdjacent(a, b) {
      return !!(a && b && a.ordered && b.ordered && a.groupIndex === b.groupIndex
        && a.members[a.members.length - 1].sequence < b.members[0].sequence);
    }
    function stageTrackerPreparationHelp(group) {
      const q = group.queue;
      const details = [`${q.repo || 'Repository unknown'} → ${q.base_ref || 'base unknown'}`,
        q.message || 'FIFO follows first qualifying admission, not PR age or guaranteed merge order.',
        'Preparation is not CI, review approval, a ready train seat, or native queue membership.'];
      if (q.observed_at) details.push('Snapshot ' + q.observed_at);
      for (const e of group.members) {
        details.push('#' + e.ticket.number + ' · ' + stageTrackerPreparationPlace(e)
          + (e.location === 'page' ? '' : e.location === 'offpage' ? ' · another page or collapsed repo' : ' · hidden by filters or absent from snapshot')
          + (e.state === 'unverified' ? ' · ' + (e.ticket.detail || 'current identity or eligibility unknown') : '')
          + ' · ticket age ' + stageTrackerPreparationAge(e.ticket, _stageTrackerPreparationNow)
          + ' · ticket head ' + (e.ticketHead || 'unknown')
          + ' · current head ' + (e.head || 'unknown'));
      }
      return details.join(' — ');
    }
    function stageTrackerPreparationTooltip(id, text, cls) {
      return `<span class="config-info ${cls}" tabindex="0" data-stop="1" aria-label="${esc(text)}" aria-describedby="${esc(id)}"><span class="config-tooltip" id="${esc(id)}" role="tooltip">${esc(text)}</span></span>`;
    }
    function stageTrackerPreparationHeaderHtml(group, suffix, continuation) {
      const current = group.members.filter(e => e.state === 'current');
      const shown = current.filter(e => e.location === 'page');
      const q = group.queue;
      const count = group.unverified.length ? current.length + ' confirmed PRs'
        : shown.length === current.length ? current.length + ' PRs' : shown.length + ' of ' + current.length + ' PRs here';
      const front = q.owner && q.owner.state !== 'inactive'
        ? ' · ' + (group.owner ? 'front: #' : 'recorded front: #') + q.owner.number : '';
      const text = 'Preparation train · ' + (q.state !== 'observed' ? 'unavailable' : count + front)
        + (continuation ? ' · continuation' : '');
      const id = 'ham-prep-head-' + encodeURIComponent((q.repo || '') + '|' + (q.base_ref || '') + '|' + suffix);
      return `<div class="ham-train-head${stageTrackerTrainAccentClass(STAGE_TRACKER_PREPARATION_ACCENT)} ham-preparation-head"><span class="ham-train-chip">${stageTrackerTrainIconHtml('ham-train-icon', 'Preparation train')} ${esc(text)}</span>${stageTrackerPreparationTooltip(id, stageTrackerPreparationHelp(group), 'ham-preparation-help')}</div>`;
    }
    function stageTrackerPreparationMissingRows() {
      return _stageTrackerPreparationInline.flatMap((group, index) => group.queue.state !== 'observed'
        || (group.members.length && !group.members.some(e => e.location === 'page'))
        ? [{ key: 'preparation-missing:' + index, html: stageTrackerPreparationHeaderHtml(group, 'missing', false) }] : []);
    }
    function stageTrackerPreparationInlineRows(rows, pageItems, stages) {
      const byKey = new Map(pageItems.filter(Boolean).map(item => [stageTrackerItemKey(item), item]));
      let prev = null;
      const seen = new Set(), out = stageTrackerPreparationMissingRows();
      for (const row of rows) {
        const item = byKey.get(row.key);
        const info = stageTrackerPreparationRowInfo(item, stages);
        if (info && !stageTrackerPreparationAdjacent(prev, info)) {
          out.push({ key: 'preparation:' + row.key, html: stageTrackerPreparationHeaderHtml(
            _stageTrackerPreparationInline[info.groupIndex], row.key, seen.has(info.groupIndex)) });
        }
        if (info) seen.add(info.groupIndex);
        out.push(row);
        prev = info;
      }
      return out;
    }
    function stageTrackerPreparationRowClass(item, stages, ctx) {
      const info = ctx && ctx.preparationInfo || stageTrackerPreparationRowInfo(item, stages);
      if (!info || ctx && ctx.preparationParent) return '';
      const prev = ctx && ctx.preparationPrev !== undefined ? ctx.preparationPrev : stageTrackerPreparationRowInfo(ctx && ctx.prevRow, stages);
      const next = ctx && ctx.preparationNext !== undefined ? ctx.preparationNext : stageTrackerPreparationRowInfo(ctx && ctx.nextRow, stages);
      const gap = !info.ordered || info.members.some((e, i) => i && e.sequence !== info.members[i - 1].sequence + 1)
        || stageTrackerPreparationAdjacent(prev, info) && info.members[0].sequence !== prev.members[prev.members.length - 1].sequence + 1;
      return ' ham-train' + stageTrackerTrainAccentClass(STAGE_TRACKER_PREPARATION_ACCENT) + ' ham-preparation-member'
        + (info.members.some(e => e.role === 'owner' && e.state === 'current') ? ' ham-train-front' : '')
        + (!stageTrackerPreparationAdjacent(info, next) ? ' ham-train-last' : '')
        + (gap ? ' ham-preparation-gap' : '')
        + (info.members.some(e => e.state === 'unverified') ? ' ham-preparation-unverified' : '');
    }
    function stageTrackerPreparationNodeHtml(item, stages, ctx) {
      const info = ctx && ctx.preparationInfo || stageTrackerPreparationRowInfo(item, stages);
      if (!info || ctx && ctx.preparationParent) return '';
      const group = _stageTrackerPreparationInline[info.groupIndex];
      const text = info.members.map(e => '#' + e.ticket.number + ' · ' + stageTrackerPreparationPlace(e)).join(' · ')
        + (!info.ordered ? ' · linked row order differs from FIFO' : '') + ' — ' + stageTrackerPreparationHelp(group);
      return stageTrackerPreparationTooltip('ham-prep-node-' + encodeURIComponent(stageTrackerItemKey(item)), text, 'ham-preparation-node');
    }
    function stageTrackerPreparationMark(item) {
      const e = stageTrackerPreparationEntry(item);
      if (e) _stageTrackerPreparationRendered.set(stageTrackerPreparationKey(item.repo, e.ticket.number), e);
    }
    function stageTrackerPreparationCommentHtml(item, ctx, key) {
      const e = stageTrackerPreparationEntry(item);
      if (!e) return typeof stageTrackerEventHtml === 'function' ? stageTrackerEventHtml(key, ctx) : '';
      stageTrackerPreparationMark(item);
      const event = ctx && ctx.events && ctx.events[key];
      const eventHtml = typeof stageTrackerEventHtml === 'function' ? stageTrackerEventHtml(key, ctx) : '';
      const caption = item.caption || item.reason || (e.state === 'current' ? e.ticket.pipeline : '') || '';
      const captionVisible = caption && event && event.text && event.text.includes(caption)
        && eventHtml && !eventHtml.includes('cue-event-tucked');
      const parts = caption && !captionVisible ? [caption] : [];
      if (!caption) parts.push('Current pipeline blocker unknown');
      parts.push(e.state === 'unverified'
        ? stageTrackerPreparationPlace(e) + ' · ' + (e.ticket.detail || 'current identity or eligibility unknown')
        : e.role === 'owner' ? 'preparing base' : 'queued for base refresh' + (e.ticket.behind ? ' behind PR #' + e.ticket.behind : ' · owner unknown'));
      parts.push('ticket age ' + stageTrackerPreparationAge(e.ticket, _stageTrackerPreparationNow));
      if (e.row.nested) parts.push('shown under issue #' + e.row.host.number);
      return eventHtml + `<div class="cue-event ham-preparation-comment"><span class="cue-event-body">${esc(parts.join(' · '))}</span></div>`;
    }
    function stageTrackerPreparationSubRows(linked, issue, stages, nowMs, ctx) {
      const parent = stageTrackerPreparationRowInfo(issue, stages);
      const infos = linked.map(lp => stageTrackerPreparationRowInfo(stageTrackerLinkedPRItem(issue, lp), stages));
      const seen = new Set();
      return linked.map((lp, i) => {
        const info = infos[i];
        const near = Object.assign({}, ctx, { preparationParent: parent, preparationInfo: info,
          preparationPrev: infos[i - 1] || null, preparationNext: infos[i + 1] || null });
        const heading = !parent && info && !stageTrackerPreparationAdjacent(infos[i - 1], info)
          ? stageTrackerPreparationHeaderHtml(_stageTrackerPreparationInline[info.groupIndex], 'nested-' + issue.number + '-' + lp.number, seen.has(info.groupIndex)) : '';
        if (info) seen.add(info.groupIndex);
        return heading + stageTrackerSubRowHtml(lp, issue, stages, nowMs, near);
      }).join('');
    }
    function stageTrackerPreparationRepresented(item) {
      const p = item && item.preparation;
      if (!p || p.state === 'inactive') return false;
      if (item.kind === 'pr' ? Number(item.number) !== Number(p.number) : !item.via_pr) return false;
      const shown = _stageTrackerPreparationRendered.get(stageTrackerPreparationKey(item.repo, p.number));
      return !!(shown && item.head === shown.head && p.head_sha === shown.ticketHead && p.state === shown.ticketState
        && p.role === shown.role && (!item.base_ref || item.base_ref === shown.base));
    }
