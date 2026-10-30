    /* ---- Merge queue popover (hivecommons/hive#15395) ----------------------
       The sticky bar's "Merge queue" chip lists every merge train in the
       payload in merge order — #, title, state, ETA — so an operator can
       read which PR goes next without hovering each chevron. Same popover
       pattern as "Merged" (hamMergedToggle). */
    // Pure: the trains in the payload, each as {key, mt, members[]} with
    // members in merge order (head first).
    function stageTrackerQueueTrains(items) {
      const byKey = {};
      const order = [];
      for (const it of (Array.isArray(items) ? items : [])) {
        const mt = typeof stageTrackerTrain === 'function' ? stageTrackerTrain(it) : null;
        if (!mt) continue;
        const k = (it.repo || '') + '|' + (mt.id || ('#' + mt.front));
        if (!byKey[k]) { byKey[k] = { key: k, repo: it.repo || '', mt, members: [] }; order.push(k); }
        byKey[k].members.push(it);
      }
      return order.map(k => {
        const t = byKey[k];
        t.members.sort((a, b) => (Number(a.merge_train.position) || 0) - (Number(b.merge_train.position) || 0) || ((a.number || 0) - (b.number || 0)));
        // The head's own row carries the freshest head facts.
        t.mt = t.members[0].merge_train;
        return t;
      });
    }
    // Pure: one member's state in plain words — never the state slug.
    function stageTrackerQueueMemberState(it, nowMs) {
      const mt = it.merge_train || {};
      if (mt.state === STAGE_TRACKER_TRAIN_STATE_FRONT || (Number(mt.position) || 0) === 1) return String(mt.head_reason || '').trim() || 'merges next';
      if (mt.state === STAGE_TRACKER_TRAIN_STATE_REBASING) return 'being rebased · attempt ' + (Number(mt.attempt) || 0) + '/' + (Number(mt.max_attempts) || 0);
      if (mt.state === STAGE_TRACKER_TRAIN_STATE_EXHAUSTED) return 'needs a human rebase';
      const mg = it.merge_gate || {};
      return 'waiting its turn' + (mg.sweep_running ? ' · checking now' : '');
    }
    // Pure: the popover body.
    function stageTrackerPreparationWord(p) {
      if (!p) return '';
      if (p.state !== 'current') return (p.role === 'owner' ? 'Recorded preparation owner' : 'Recorded waiting ticket') + ' · ' + (p.detail || 'current state unknown');
      return p.role === 'owner' ? 'Preparing base' : (Number(p.behind) > 0 ? 'Waiting for preparation behind #' + Number(p.behind) : 'Waiting for preparation; owner not confirmed');
    }
    function stageTrackerPreparationAge(p, nowMs) {
      const since = Date.parse(p && p.since || '');
      if (!Number.isFinite(since) || since <= 0 || since > nowMs) return 'age unknown';
      const minutes = Math.floor((nowMs - since) / 60000);
      return minutes < 1 ? 'less than 1m' : minutes + 'm';
    }
    function stageTrackerPreparationChipHtml(item) {
      const p = item && item.preparation;
      if (!p || p.state === 'inactive') return '';
      const word = stageTrackerPreparationWord(p);
      return `<span class="ham-badge" title="${esc(word + ' · ticket head ' + p.head_sha + ' · ' + stageTrackerPreparationAge(p, Date.now()) + '. Preparation is not CI, review approval, or a ready merge-train seat.')}">${esc(word)}</span>`;
    }
    function stageTrackerPreparationPopHtml(queues, nowMs) {
      return (Array.isArray(queues) ? queues : []).map(q => {
        const members = (q.owner ? [q.owner] : []).concat(q.waiting || []);
        const heading = `<div class="ham-queue-train">Base preparation · ${esc(q.repo || '')} → ${esc(q.base_ref || 'unknown base')}</div>`;
        if (q.state !== 'observed') return heading + `<div class="ham-empty">${esc(q.message || 'Preparation unknown')}</div>`;
        const rows = members.map(p => {
          const identity = 'Ticket head ' + (p.head_sha || 'unknown') + (p.current_head ? '; snapshot head ' + p.current_head : '');
          const meta = [stageTrackerPreparationWord(p), stageTrackerPreparationAge(p, nowMs), p.pipeline ? 'Pipeline snapshot: ' + p.pipeline : '', identity].filter(Boolean).join(' · ');
          return `<li><span class="ham-merged-num">#${Number(p.number) || 0}</span><span class="ham-merged-meta">${esc(meta)}</span></li>`;
        }).join('');
        const observed = Date.parse(q.observed_at || '');
        return heading + (rows ? `<ol class="ham-merged-list ham-queue-list">${rows}</ol>` : `<div class="ham-empty">${esc(q.message || 'No recorded preparation tickets')}</div>`) + `<div class="ham-merged-pop-foot">Persisted FIFO tickets, not ready train seats or GitHub native queue membership. Owner head changes are unverified here. Read ${esc(Number.isFinite(observed) ? new Date(observed).toISOString() : 'time unknown')}; CI / Review stages remain unchanged.</div>`;
      }).join('');
    }
    function stageTrackerQueuePopHtml(items, nowMs, preparation) {
      nowMs = typeof nowMs === 'number' ? nowMs : Date.now();
      const trains = stageTrackerQueueTrains(items);
      const prep = typeof stageTrackerPreparationPopHtml === 'function' ? stageTrackerPreparationPopHtml(preparation, nowMs) : '';
      const head = `<div class="ham-merged-pop-head"><span>Merge queue</span><span class="ham-merged-pop-sum">${trains.length ? esc(String(trains.reduce((n, t) => n + t.members.length, 0))) + ' PRs in ' + esc(String(trains.length)) + (trains.length === 1 ? ' train' : ' trains') : 'no train right now'}</span></div>`;
      if (!trains.length) return head + prep + '<div class="ham-empty">No merge train right now. PRs that touch the same files queue here and merge one at a time, in order.</div>';
      const body = trains.map(t => {
        const place = stageTrackerTrainPlace(t.mt);
        const stalled = stageTrackerTrainStalledWord(t.mt);
        const base = String(t.mt.id || '').split('#')[0] || 'base';
        const title = `<div class="ham-queue-train${stalled ? ' ham-queue-stalled' : ''}">${stageTrackerTrainIconHtml('ham-train-icon')} ${esc(String(t.repo || ''))} → ${esc(base)} · ${esc(String(place.len))} PRs · head #${esc(String(place.head))}${stalled ? ' · ' + esc(stalled.replace(STAGE_TRACKER_TRAIN_STALLED_LEAD, 'stalled: ')) : ''}</div>`;
        const rows = t.members.map(it => {
          const mt = it.merge_train || {};
          const n = esc(String(it.number || ''));
          const num = it.url ? `<a href="${esc(it.url)}" target="_blank" rel="noopener">#${n}</a>` : `#${n}`;
          const etaAt = !mt.stalled ? stageTrackerTrainEtaAt(mt, nowMs) : 0;
          const eta = etaAt > 0 ? (mt.late ? 'running late' : 'ETA ' + stageTrackerTrainEtaDetail(etaAt, nowMs)) : '';
          const meta = [esc(stageTrackerQueueMemberState(it, nowMs)), esc(eta)].filter(Boolean).join(' · ');
          return `<li><span class="ham-merged-num">${esc(String(mt.position || ''))}. ${num}</span><span class="ham-merged-title" title="${esc(it.title || '')}">${esc(it.title || '')}</span><span class="ham-merged-meta">${meta}</span></li>`;
        }).join('');
        return title + `<ol class="ham-merged-list ham-queue-list">${rows}</ol>`;
      }).join('');
      const foot = '<div class="ham-merged-pop-foot">Hive merges one train member at a time, head first; the rest rebase or update and take the next turn. The ETA is an estimate: the head\u2019s remaining soak, one sweep per PR ahead without a clock, and the wait for the next sweep.</div>';
      return head + prep + body + foot;
    }
    var _stageTrackerQueueItems = [];
    var _stageTrackerPreparationQueues = [];
    function stageTrackerQueueChip() { return document.getElementById('activity-monitor-queue'); }
    // Pure over the DOM: the chip reads the train count and hides with no
    // train; an open popover is redrawn.
    function stageTrackerQueueSync(items, nowMs, preparation) {
      _stageTrackerQueueItems = Array.isArray(items) ? items : [];
      _stageTrackerPreparationQueues = Array.isArray(preparation) ? preparation : [];
      const chip = stageTrackerQueueChip();
      if (!chip) return;
      const trains = stageTrackerQueueTrains(_stageTrackerQueueItems);
      const n = trains.reduce((a, t) => a + t.members.length, 0);
      const stalled = trains.some(t => t.mt && t.mt.stalled);
      const prepTickets = _stageTrackerPreparationQueues.reduce((n, q) => n + (q.owner ? 1 : 0) + (q.waiting || []).length, 0);
      const prepUnknown = _stageTrackerPreparationQueues.some(q => q.state !== 'observed');
      chip.hidden = !trains.length && !_stageTrackerPreparationQueues.length;
      const nEl = chip.querySelector('.ham-queue-n');
      const count = _stageTrackerPreparationQueues.length ? n + ' train · ' + (prepUnknown ? 'prep unknown' : prepTickets + ' prep tickets') : String(n);
      if (nEl && nEl.textContent !== count) nEl.textContent = count;
      const title = (trains.length ? 'Merge queue: ' + n + ' PRs in ' + trains.length + (trains.length === 1 ? ' train' : ' trains') + (stalled ? ' · stalled' : '') : 'Merge queue: no ready train') + (_stageTrackerPreparationQueues.length ? ' · base preparation: ' + (prepUnknown ? 'state unknown' : prepTickets + ' recorded tickets (not readiness)') : '');
      if (chip.title !== title) chip.title = title;
      chip.classList.toggle('ham-queue-stalled', stalled);
      stageTrackerQueuePopDraw(nowMs);
    }
    function stageTrackerQueuePopDraw(nowMs) {
      const pop = document.getElementById('activity-monitor-queue-pop');
      if (!pop || pop.hidden) return;
      const html = stageTrackerQueuePopHtml(_stageTrackerQueueItems, nowMs || Date.now(), _stageTrackerPreparationQueues);
      if (pop.innerHTML !== html) pop.innerHTML = html;
    }
    function hamQueueToggle(open) {
      const pop = document.getElementById('activity-monitor-queue-pop');
      const chip = stageTrackerQueueChip();
      if (!pop) return false;
      const want = typeof open === 'boolean' ? open : !!pop.hidden;
      pop.hidden = !want;
      if (chip) chip.setAttribute('aria-expanded', want ? 'true' : 'false');
      if (want) stageTrackerQueuePopDraw(Date.now());
      return want;
    }
    document.addEventListener('click', function(e) {
      const pop = document.getElementById('activity-monitor-queue-pop');
      if (!pop || pop.hidden) return;
      if (e.target && typeof e.target.closest === 'function' && e.target.closest('#activity-monitor-sticky')) return;
      hamQueueToggle(false);
    });
    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      const pop = document.getElementById('activity-monitor-queue-pop');
      if (pop && !pop.hidden) { hamQueueToggle(false); const chip = stageTrackerQueueChip(); if (chip && chip.focus) chip.focus(); }
    });
