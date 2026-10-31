    // Queue references have their own rail: never reorder, copy, or reparent
    // pipeline rows to make preparation look like merge-train admission.
    var _stageTrackerPreparationShown = new Map();
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
        const out = { queue: q, owner: null, waiting: [], unverified: [], inactive: 0 };
        let uncertainAhead = false;
        let position = 0;
        const member = (p, role) => {
          if (!p || p.state === 'inactive') { if (p) out.inactive++; return null; }
          const key = stageTrackerPreparationKey(q.repo, p.number);
          const found = known.get(key);
          const here = page.get(key);
          const exact = p.state === 'current' && found && found.item.head && found.item.head === p.head_sha
            && (!p.current_head || p.current_head === found.item.head) && found.item.base_ref === q.base_ref
            && (!here || (here.item.head === p.head_sha && here.item.base_ref === q.base_ref));
          if (!exact) {
            out.unverified.push({ ticket: p, role });
            if (role === 'waiting') uncertainAhead = true;
            return null;
          }
          const entry = { ticket: p, role, position: 0, location: page.has(key) ? 'page' : listed.has(key) ? 'offpage' : 'filtered',
            row: page.get(key) || found };
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
      if (entry.role === 'owner') return 'Preparing';
      return entry.position > 0 ? 'Waiting ' + stageTrackerOrdinal(entry.position) + ' for preparation' : 'Waiting for preparation · position unverified';
    }
    function stageTrackerPreparationVisualHtml(model, nowMs) {
      const groups = (model || []).map((group, index) => {
        const q = group.queue;
        const members = (group.owner ? [group.owner] : []).concat(group.waiting);
        const shown = members.filter(e => e.location === 'page');
        const accent = stageTrackerTrainAccentClass(index);
        const heading = `<h3 class="ham-train-head${accent}"><span class="ham-train-chip">${stageTrackerTrainIconHtml('ham-train-icon', 'Preparation queue')} Preparation queue · ${esc(q.repo || '')} → ${esc(q.base_ref || 'unknown base')}</span></h3>`;
        if (q.state !== 'observed') return `<section class="ham-preparation-group${accent}" aria-label="Preparation queue">${heading}<p class="ham-preparation-note">${esc(q.message || 'Preparation state unknown')}</p></section>`;
        const owner = group.owner;
        const ownerContext = owner && owner.location !== 'page' ? `Owner #${owner.ticket.number} · ${owner.ticket.pipeline || 'current pipeline blocker unknown'} · ticket age ${stageTrackerPreparationAge(owner.ticket, nowMs)}` : '';
        const rows = shown.map(e => {
          const it = e.row.item;
          const place = stageTrackerPreparationPlace(e);
          const where = e.row.nested ? 'shown under issue #' + e.row.host.number : 'shown in the pipeline below';
          const detail = [e.ticket.pipeline || 'current pipeline blocker unknown', 'ticket age ' + stageTrackerPreparationAge(e.ticket, nowMs), where].join(' · ');
          const label = '#' + e.ticket.number + (it.title ? ' ' + it.title : '');
          const ref = it.url ? `<a href="${esc(it.url)}" target="_blank" rel="noopener">${esc(label)}</a>` : `<span>${esc(label)}</span>`;
          return `<li class="ham-preparation-member${e.role === 'owner' ? ' ham-preparation-owner' : ''}" title="${esc(place + ' · ' + detail + '. Ticket head ' + e.ticket.head_sha + '. Preparation is not approval or a ready merge-train seat.')}"><span class="ham-train-chip">${esc(place)}</span><span class="ham-preparation-ref">${ref}</span><span class="ham-preparation-detail">${esc(detail)}</span></li>`;
        }).join('');
        const missing = ['offpage', 'filtered'].map(location => {
          const entries = members.filter(e => e.location === location);
          return entries.length ? `<p class="ham-preparation-note">${location === 'offpage' ? 'On another page or in a collapsed repo' : 'Not shown by the current filters'}: ${esc(entries.map(e => stageTrackerPreparationPlace(e) + ' #' + e.ticket.number).join(' · '))}</p>` : '';
        }).join('');
        const unverified = role => group.unverified.filter(e => e.role === role).map(e => `<p class="ham-preparation-note">Unverified recorded ${e.role === 'owner' ? 'owner' : 'ticket'} #${Number(e.ticket.number) || 0} · ${esc(e.ticket.detail || 'current identity or eligibility unknown')} · ticket age ${esc(stageTrackerPreparationAge(e.ticket, nowMs))}. No confirmed position or current-head approval is implied.</p>`).join('');
        const count = `${shown.length} of ${members.length} confirmed tickets shown on this page`;
        return `<section class="ham-preparation-group${accent}" aria-label="Preparation queue">${heading}<p class="ham-preparation-note">${esc(count)}</p>${ownerContext ? `<p class="ham-preparation-note">${esc(ownerContext)}</p>` : ''}${unverified('owner')}${rows ? `<ol class="ham-preparation-rail" aria-label="Confirmed preparation order on this page">${rows}</ol>` : '<p class="ham-preparation-note">No confirmed preparation members displayed on this page.</p>'}${missing}${unverified('waiting')}${group.inactive ? `<p class="ham-preparation-note">${group.inactive} inactive tickets excluded.</p>` : ''}</section>`;
      }).join('');
      return groups ? `<div class="ham-preparation-intro">Preparation order, not guaranteed merge order. FIFO follows first qualifying admission, not PR age. Independent ready PRs may merge; tickets can leave and re-admit. These are references to the pipeline rows below, whose stages, sorts and trees are unchanged.</div>${groups}` : '';
    }
    function stageTrackerPreparationVisualSync(data, listedRows, pageRows, stages, nowMs) {
      _stageTrackerPreparationShown = new Map();
      const el = document.getElementById('activity-monitor-preparation');
      if (!el) return;
      if (!data || !Array.isArray(data.items)) {
        el.hidden = false;
        el.innerHTML = '<p class="ham-preparation-note">Preparation queue unavailable · waiting for a current tracker snapshot.</p>';
        return;
      }
      const model = stageTrackerPreparationVisualModel(data, listedRows, pageRows, stages);
      const html = stageTrackerPreparationVisualHtml(model, nowMs);
      el.hidden = !html;
      if (el.innerHTML !== html) el.innerHTML = html;
      for (const group of model) {
        for (const e of (group.owner ? [group.owner] : []).concat(group.waiting)) {
          if (e.location === 'page') _stageTrackerPreparationShown.set(stageTrackerPreparationKey(group.queue.repo, e.ticket.number), { head: e.ticket.head_sha, base: group.queue.base_ref });
        }
      }
    }
    function stageTrackerPreparationRepresented(item) {
      const p = item && item.preparation;
      if (!p || p.state !== 'current') return false;
      if (item.kind === 'pr' ? Number(item.number) !== Number(p.number) : !item.via_pr) return false;
      const shown = _stageTrackerPreparationShown.get(stageTrackerPreparationKey(item.repo, p.number));
      return !!(shown && item.head === shown.head && p.head_sha === shown.head && (!p.current_head || p.current_head === shown.head) && (!item.base_ref || item.base_ref === shown.base));
    }
