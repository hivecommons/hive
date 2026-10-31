    function stageTrackerPagerHtml(page) {
      if (page.pages <= 1) return '';
      return `<nav class="ham-pager" aria-label="Tracker pages"><button type="button" class="ham-chip" data-action="hamFilter" data-arg0="page" data-arg1="prev"${page.page <= 1 ? ' disabled' : ''} aria-label="Previous page">‹ Prev</button><span>page ${page.page} of ${page.pages}</span><button type="button" class="ham-chip" data-action="hamFilter" data-arg0="page" data-arg1="next"${page.page >= page.pages ? ' disabled' : ''} aria-label="Next page">Next ›</button></nav>`;
    }

    // "updated 12s ago · cache" — re-stamped every STAGE_TRACKER_CLOCK_MS
    // without a fetch; amber once the payload is older than two TTLs or
    // once the poll itself is failing (pollError = short reason, '' = ok).
    // A failing poll also demotes "fresh" to "cache": the payload was built
    // for a request long gone, and the server has not answered since.
    function stageTrackerUpdatedHtml(d, nowMs, pollError, pollErrorSinceMs) {
      const at = Date.parse((d && d.generated_at) || '') || 0;
      if (!at) return '';
      const ageMs = Math.max(0, nowMs - at);
      const ttlMs = (Number(d.cache_ttl_s) || 0) * 1000;
      const failing = !!pollError;
      const stale = failing || (ttlMs > 0 && ageMs > 2 * ttlMs);
      const source = failing ? 'cache' : (d.source || 'cache');
      const src = source === 'fresh' ? 'built for this request' : 'from the server cache';
      const failTip = failing ? '. Polling is failing (' + pollError + (pollErrorSinceMs ? ', since ' + agentNowClockTime(pollErrorSinceMs, nowMs) : '') + '): the dashboard keeps showing this payload until the server answers again' : '';
      const failTag = failing ? ` · <span class="ham-poll-failing">poll failing (${esc(pollError)})</span>` : '';
      return `<span class="ham-updated${stale ? ' ham-stale' : ''}" title="${esc('Payload built ' + agentNowClockTime(at, nowMs) + ' ' + src + (ttlMs ? '; the server rebuilds it at most every ' + Math.round(ttlMs / 1000) + 's and makes no GitHub call per request' : '') + failTip)}">updated ${esc(stageTrackerShortAgo(ageMs) === 'now' ? Math.floor(ageMs / 1000) + 's' : stageTrackerShortAgo(ageMs))} ago · ${esc(source)}${failTag}</span>`;
    }
    // The ADR-0025 central stage engine's status strip: mode and admission
    // anomalies (governor.labels.stage.engine.mode). Hover text on the
    // items only, per the tracker hover rules.
    function stageTrackerEngineHtml(d) {
      const e = d && d.engine;
      if (!e || !e.mode) return '';
      const anomalies = Number(e.anomalies) || 0;
      const modeHelp = { shadow: 'shadow: the engine derives and counts but writes nothing (dry run; stage labels are left as found).', enforce: 'enforce: the engine derives and writes; its admission table counts out-of-order transitions.' }[e.mode] || '';
      return `<span class="ham-engine" data-mode="${esc(e.mode)}">`
        + `<span class="ham-engine-mode" title="${esc('Central stage engine (ADR-0025). ' + modeHelp + ' ' + (Number(e.derivations) || 0) + ' derivations since start. Settings → Labels → Central stage engine mode.')}">engine ${esc(e.mode)}</span>`
        + `<span class="ham-engine-anomaly${anomalies ? ' ham-engine-warn' : ''}" title="${esc('Transition anomalies: stage moves the admission table did not expect (stage_transition_anomaly_total).')}">${esc(anomalies)} anomal${anomalies === 1 ? 'y' : 'ies'}</span>`
        + '</span>';
    }
    function stageTrackerTickClock() {
      const el = document.querySelector('#activity-monitor-meta .ham-updated');
      if (!el || !_stageTracker) return;
      const wrap = document.createElement('span');
      wrap.innerHTML = stageTrackerUpdatedHtml(_stageTracker, Date.now(), _stageTrackerPollError, _stageTrackerPollErrorSince);
      if (wrap.firstChild) el.replaceWith(wrap.firstChild);
    }
    function stageTrackerPollFailed(reason) {
      if (!_stageTrackerPollError) _stageTrackerPollErrorSince = Date.now();
      _stageTrackerPollError = reason;
      stageTrackerTickClock();
    }
    function stageTrackerPollOk() {
      _stageTrackerPollError = '';
      _stageTrackerPollErrorSince = 0;
    }

    function renderStageTracker(data, countRefresh = false) {
      if (data && typeof data === 'object') { _stageTracker = data; stageTrackerReconcileMerges(data); stageTrackerNoteOpened(data); stageTrackerNoteMoves(data); }
      // Row-action feedback rides the payload: a result toast leaves and a
      // row stops "acting" once the item it targets reads a new state.
      if (data && typeof data === 'object' && typeof stageTrackerResultToastsSync === 'function') { stageTrackerNoteEvents(data); stageTrackerResultToastsSync(data); stageTrackerPendingExpire(data); }
      const el = document.getElementById('activity-monitor-panel');
      if (!el) return;
      const nowMs = Date.now();
      // Rows under a "Merging" hold the payload already dropped ride along (#15245).
      const held = typeof stageTrackerMergingHoldView === 'function' ? stageTrackerMergingHoldView(_stageTracker, _stageTrackerMergingHold, nowMs) : _stageTracker;
      const d = typeof stageTrackerTerminalView === 'function' ? stageTrackerTerminalView(held, nowMs) : held;
      if (!d || d.status === 'initializing' || !Array.isArray(d.items)) {
        if (typeof stageTrackerPreparationVisualSync === 'function') stageTrackerPreparationVisualSync(null, [], [], [], nowMs);
        if (typeof stageTrackerNativeQueueSync === 'function') stageTrackerNativeQueueSync(null, [], nowMs, '');
        if (typeof stageTrackerExecutionSync === 'function') stageTrackerExecutionSync(null, nowMs);
        stageTrackerSidebarCountsUnknown();
        setIfChanged(el, '<div class="ham-empty">Waiting for the first scan…</div>');
        return;
      }
      const f = _stageTrackerFilter = stageTrackerNormalizeFilter(_stageTrackerFilter);
      if (typeof stageTrackerTerminalEvents === 'function') stageTrackerTerminalEvents(nowMs);
      const stages = d.stages || [];
      const repos = Array.from(new Set(d.items.map(it => it.repo).filter(Boolean))).sort();
      const sel = document.getElementById('activity-monitor-repo');
      if (sel) {
        const want = '<option value="">All repos</option>' + repos.map(r => `<option value="${esc(r)}"${r === f.repo ? ' selected' : ''}>${esc(r)}</option>`).join('');
        if (sel.innerHTML !== want) sel.innerHTML = want;
      }
      stageTrackerSyncToolbar(f);
      const ctx = { nowMs, stages, moves: _stageTrackerMoves };
      ctx.workers = stageTrackerWorkers(window._lastStatus);
      ctx.events = _stageTrackerEvents;
      ctx.noteTuck = stageTrackerNoteTuckSetting();
      ctx.sharedPRs = stageTrackerSharedPRs(d.items);
      ctx.items = d.items;
      // Repo chips ride the setting against how many repos the payload spans.
      if (stageTrackerRepoChipOn(stageTrackerRepoChipSetting(), repos.length)) ctx.repoChips = { primary: stageTrackerPrimaryRepo(), active: f.repo };
      const visible = stageTrackerVisible(d.items, f, ctx);
      const ordered = stageTrackerTrainContiguous(stageTrackerSort(visible, stages, f));
      // A TV display (display=1&density=tv) shows the rows that fit the
      // screen, ungrouped, with "+N more" in place of the pager.
      const tv = typeof stageTrackerDisplayTv === 'function' && stageTrackerDisplayTv();
      const grouping = _stageTrackerGroup === 'repo' && !tv;
      const page = tv
        ? stageTrackerDisplayCapPage(ordered, stageTrackerDisplayTvCap())
        : grouping
          ? stageTrackerGroupPage(stageTrackerGroups(ordered, { primary: stageTrackerPrimaryRepo(), collapsed: _stageTrackerCollapsed, stages }), f)
          : stageTrackerPage(ordered, f);
      if (!tv && page.page !== f.page) f.page = page.page;
      // The rows this render draws (this page, group headers aside), for
      // stageTrackerPRListedOwnRow: a covered issue defers to its PR's row
      // only when that row is actually on screen. Kept beside the payload,
      // not on it — the payload object stays the server's.
      _stageTrackerRows = page.rows.map(e => grouping ? e.item : e).filter(Boolean);
      if (typeof stageTrackerPreparationVisualSync === 'function') stageTrackerPreparationVisualSync(d, ordered, page.rows, stages, nowMs);
      if (typeof stageTrackerNativeQueueSync === 'function') stageTrackerNativeQueueSync(d, page.rows, nowMs, f.repo);
      if (typeof stageTrackerExecutionSync === 'function') stageTrackerExecutionSync(d, nowMs);
      // Attention is counted over the filtered set, nested PRs included.
      const filteredAll = d.items.filter(it => stageTrackerMatches(it, f, ctx));
      const attention = filteredAll.filter(stageTrackerNeedsAttention).length;
      // "N filtered out" and the sidebar pill count against the default
      // view's rows (stageTrackerRowCount), so drawing a PR on its issue's
      // row is never reported as a filter and the pill holds still while
      // the kind tabs or chips narrow the list.
      const allRows = stageTrackerFilterActive(f) ? stageTrackerRowCount(d.items, ctx) : visible.length;
      // The sidebar pill, the header lead and the "of N" line come out of
      // one stageTrackerCountsText call per render, so they cannot
      // disagree: the pill is the default view's row count (owner: rows,
      // not items — a covering PR shares its issue's row); needs-attention
      // is the warn-coloured header suffix and the pill's tooltip, counted
      // over every item regardless of filter.
      const attentionAll = d.items.filter(stageTrackerNeedsAttention).length;
      const counts = stageTrackerCountsText(page, allRows, attention, stageTrackerOpenCounts(d), attentionAll, stageTrackerFoldedCount(d.items, ctx));
      setSectionSummary('activity-monitor-section', counts.header.replace(/(\d+) needs? attention/, m => `<span class="text-warn">${m}</span>`));
      const badge = document.getElementById('activity-monitor-sidebar-badge');
      if (badge) { badge.textContent = String(counts.badge); badge.title = counts.badgeTitle; if (counts.badge) badge.removeAttribute('data-zero'); else badge.setAttribute('data-zero', ''); }
      navCountDelta(badge, counts.badge, counts.badgeTitle, !!data || countRefresh);
      stageTrackerRenderWaiting(d, nowMs, !!data || countRefresh);
      const labelsEl = document.getElementById('activity-monitor-labels');
      if (labelsEl) setIfChanged(labelsEl, stageTrackerLabelsHtml(stageTrackerLabelFacets(d.items, f)));
      stageTrackerRenderLabelDialog();
      const meta = document.getElementById('activity-monitor-meta');
      if (meta) setIfChanged(meta, `<span class="ham-count"${counts.note ? ` title="${esc(counts.note)}"` : ''}>${esc(counts.showing)}</span>${stageTrackerEngineHtml(d)}${stageTrackerUpdatedHtml(d, nowMs, _stageTrackerPollError, _stageTrackerPollErrorSince)}${stageTrackerPagerHtml(page)}`);
      const foot = document.getElementById('activity-monitor-meta-foot');
      if (foot) setIfChanged(foot, tv ? stageTrackerDisplayMoreHtml(page.more) : page.pages > 1 ? `<span class="ham-count">${esc(counts.showing)}</span>${stageTrackerPagerHtml(page)}` : '');
      let effect = stageTrackerMotionEffect(stageTrackerMotionSetting(), stageTrackerReducedMotion());
      if (typeof stageTrackerDisplayEffect === 'function') effect = stageTrackerDisplayEffect(effect);
      if (!visible.length && !Object.keys(_stageTrackerCelebrating).length) {
        _stageTrackerMoves = {};
        stageTrackerSelectionSync(el, []);
        const emptyHtml = (typeof stageTrackerPreparationMissingRows === 'function' ? stageTrackerPreparationMissingRows().map(row => row.html).join('') : '') + `<div class="ham-empty">${d.items.length ? 'Nothing matches the current filters.' : 'Nothing open is being tracked right now.'}</div>`;
        // The counter goes back to its toolbar home before the header
        // (and its slot) leaves with the rows.
        if (el.querySelector('[data-ham-key]')) {
          stageTrackerReconcile(el, [], { effect, hidden: document.hidden });
          // Departing rows collapse first; the empty line follows them out.
          if (el.querySelector('.ham-leaving')) { setTimeout(() => { if (!el.querySelector('[data-ham-key]')) setIfChanged(el, emptyHtml); }, STAGE_TRACKER_EXIT_MS * 1.5); return; }
        }
        setIfChanged(el, emptyHtml);
        return;
      }
      // Grouped: header rows keyed group:owner/repo sit between the item
      // rows; the reconcile treats them like any keyed row (kept while
      // unchanged, collapsed out with their last row).
      // A row celebrating its merge is pinned past the payload that
      // dropped it (stageTrackerPinnedRows) until its moment is over.
      // A merge train's rows carry a header line and a rail (#15117); the
      // row renderer sees its neighbours to close the rail on the last one.
      const pageItems = stageTrackerAdoptOrphanPRs(page.rows.map(e => grouping ? e.item : e));
      // Every train on the page, so a queue word can name its train when
      // there is more than one and each train's rows share a rail accent.
      const trains = typeof stageTrackerTrainIndex === 'function' ? stageTrackerTrainIndex(pageItems) : { count: 0, index: {} };
      const itemRow = (it, i) => {
        const near = Object.assign({}, ctx, { prevRow: i > 0 && pageItems[i - 1] ? pageItems[i - 1] : null, nextRow: pageItems[i + 1] ? pageItems[i + 1] : null, trains: trains.count, trainIndex: trains.index });
        return { key: stageTrackerItemKey(it), html: stageTrackerRowHtml(it, stages, near) };
      };
      const rowOf = (e, i) => grouping && !e.item
        ? { key: e.group.key, html: stageTrackerGroupHeaderHtml(e.group, stages, ctx) }
        : itemRow(pageItems[i], i);
      const trainRows = stageTrackerTrainRows(page.rows, rowOf, typeof stageTrackerTrainWhere === 'function' ? stageTrackerTrainWhere(ordered, d.items) : null);
      const inlineRows = typeof stageTrackerPreparationInlineRows === 'function' ? stageTrackerPreparationInlineRows(trainRows, pageItems, stages) : trainRows;
      const rows = stageTrackerPinnedRows(el, [{ key: STAGE_TRACKER_HEADER_KEY, html: stageTrackerHeaderHtml(stages) }].concat(inlineRows), nowMs, it => stageTrackerRowHtml(it, stages, ctx));
      const rec = stageTrackerReconcile(el, rows, { effect, hidden: document.hidden });
      if (rec.deferred) stageTrackerScheduleSettle();
      // Selection is keyed on the row, never baked into its HTML, so a kept
      // node stays selected across the refresh and a departed row leaves it.
      stageTrackerSelectionSync(el, rows.map(r => r.key));
      if (typeof stageTrackerPendingSync === 'function') stageTrackerPendingSync(el);
      stageTrackerCueAgeSync(el, nowMs);
      stageTrackerCueTick();
      if (tv) stageTrackerDisplayMeasureRows(el);
      // Moves play once, on the render that follows the payload; a filter
      // or paging re-render of the same payload does not replay them.
      if (Object.keys(_stageTrackerMoves).length) { stageTrackerAnimateMoves(el); _stageTrackerMoves = {}; }
    }

    // Stateful: reduce the payload to columns, diff against the previous
    // one, and keep the moves for the render that follows.
    function stageTrackerNoteMoves(d) {
      const next = stageTrackerColMap(d && d.items, d && d.stages);
      _stageTrackerMoves = stageTrackerColMoves(_stageTrackerCols, next);
      _stageTrackerCols = next;
    }

    // The endpoint is served from a server-side cache with an ETag; a poll
    // that matches it costs one 304 and no re-render. The filter/sort state
    // is restored from storage on the first fetch.
    // SSE nudges arrive once per status publish and once per webhook
    // delivery, uncoalesced: a busy hive sent ~36 a minute, each a refetch
    // and a reconcile of the list. The first nudge in a window fetches at
    // once; the rest collapse into one trailing fetch when the window ends.
    // A nudge that lands while the tab is hidden is remembered and served
    // on the next visibilitychange instead.
    const STAGE_TRACKER_NUDGE_COALESCE_MS = 2000;
    let _stageTrackerNudgeTimer = null;
    let _stageTrackerNudgeAt = 0;
    let _stageTrackerNudgeSkipped = false;
    function stageTrackerNudge(now) {
      now = typeof now === 'number' ? now : Date.now();
      if (document.hidden) { _stageTrackerNudgeSkipped = true; return false; }
      const wait = _stageTrackerNudgeAt + STAGE_TRACKER_NUDGE_COALESCE_MS - now;
      if (wait <= 0) { _stageTrackerNudgeAt = now; fetchStageTracker(); return true; }
      if (!_stageTrackerNudgeTimer) {
        _stageTrackerNudgeTimer = setTimeout(() => {
          _stageTrackerNudgeTimer = null;
          _stageTrackerNudgeAt = Date.now();
          if (document.hidden) { _stageTrackerNudgeSkipped = true; return; }
          fetchStageTracker();
        }, wait);
      }
      return false;
    }
    function stageTrackerResumeIfSkipped() {
      if (!_stageTrackerNudgeSkipped) return;
      _stageTrackerNudgeSkipped = false;
      _stageTrackerNudgeAt = Date.now();
      fetchStageTracker();
    }
