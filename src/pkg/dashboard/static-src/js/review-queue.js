    // reviewQueuePriorityMeta maps the API's priority (high|normal|low) to the
    // review-priority/* label family the backend mirrors onto PRs; the badge
    // colour comes from the .review-priority-<key> CSS class. Unknown values
    // fall back to normal so a new band still renders.
    function reviewQueuePriorityMeta(priority) {
      var p = String(priority || '').toLowerCase();
      if (p === 'high') return { key: 'high', label: 'review-priority/high' };
      if (p === 'low') return { key: 'low', label: 'review-priority/low' };
      return { key: 'normal', label: 'review-priority/normal' };
    }

    function reviewQueueDurationLabel(d) {
      // Go duration string ("5h30m0s") → "5h30m"; falls back to the input.
      var m = /^(?:(\d+)h)?(?:(\d+)m)?(?:[\d.]+s)?$/.exec(String(d || ''));
      if (!m || (!m[1] && !m[2])) return String(d || '');
      var out = '';
      if (m[1]) out += m[1] + 'h';
      if (m[2] && (!m[1] || Number(m[2]) > 0)) out += m[2] + 'm';
      return out || '<1m';
    }
    function reviewQueueStageShort(stage) {
      // "050-review-pending" → "050"; non-numbered values pass through.
      var m = /^(\d{3})-/.exec(String(stage || ''));
      return m ? m[1] : String(stage || '');
    }
    function reviewQueueHopTime(at) {
      return formatDashboardDateTime(at, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }, String(at || ''));
    }
    function reviewQueueAgeLabel(hours) {
      var h = Number(hours) || 0;
      if (h < 24) return h + 'h';
      return Math.floor(h / 24) + 'd';
    }

    function renderReviewQueueEntry(e) {
      var meta = reviewQueuePriorityMeta(e.priority);
      var title = String(e.title || '');
      var titleHtml = e.url
        ? '<a href="' + esc(String(e.url)) + '" target="_blank" rel="noopener noreferrer" class="review-queue-title">' + esc(title) + '</a>'
        : '<span class="review-queue-title">' + esc(title) + '</span>';
      var repoRef = esc(String(e.repo || '')) + '#' + esc(String(e.number || ''));
      var priorityBadge = '<span class="review-priority review-priority-' + meta.key + '" data-priority="' + meta.key + '" title="' + esc(meta.label) + '">' + esc(meta.label) + '</span>';
      var metaBits = [];
      if (e.author) metaBits.push('@' + esc(String(e.author)));
      metaBits.push(e.hive_authored ? 'agent' : 'contributor');
      if (e.ci_state) metaBits.push('CI ' + esc(String(e.ci_state)));
      if (e.confidence_band) metaBits.push(esc(String(e.confidence_band)));
      metaBits.push(reviewQueueAgeLabel(e.age_hours) + ' old');
      if (e.held) metaBits.push('held');
      // Hive-owned stage/author labels (#12429) tell at a glance where the
      // PR sits in the pipeline; other labels stay out of the meta line.
      // stage/stalled means "no activity by anyone but the hive for the
      // stage's dwell" (#12534): its tooltip says for how long. The retired
      // stage/regressed label is replaced by the transition history pill.
      var st = e.stage && typeof e.stage === 'object' ? e.stage : null;
      (Array.isArray(e.labels) ? e.labels : []).map(function(l) { return String(l || ''); })
        .filter(function(l) { return /^(stage|author)\//.test(l.toLowerCase()) && l.toLowerCase() !== 'stage/regressed'; })
        .sort()
        .forEach(function(l) {
          var tip = l;
          if (l.toLowerCase() === 'stage/stalled') tip = 'no activity for ' + (st && st.quiet_for ? reviewQueueDurationLabel(st.quiet_for) : 'the stage dwell') + ' — nobody is working this PR';
          metaBits.push('<span class="hf-label ' + hfLabelToneClass(l) + '" title="' + esc(tip) + '">' + esc(l) + '</span>');
        });
      if (st && st.waiting_on_hive && st.past_dwell) {
        metaBits.push('<span class="hf-label hf-label-hold" title="past the stage dwell while waiting on the hive (checks running or reviewer verdict pending) — the reviewer lane needs attention, not the author">waiting on hive ' + esc(reviewQueueDurationLabel(st.quiet_for)) + '</span>');
      }
      var hops = st && Array.isArray(st.history) ? st.history.filter(function(h) { return h && h.from && h.from !== 'none'; }) : [];
      if (hops.length) {
        var hopText = hops.map(function(h) {
          return reviewQueueStageShort(h.from) + ' → ' + reviewQueueStageShort(h.to) + (h.regressed ? ' ↩' : '') + ' @ ' + reviewQueueHopTime(h.at) + (h.reason ? ' (' + String(h.reason) + ')' : '');
        }).join('\n');
        var back = hops.filter(function(h) { return h.regressed; }).length;
        metaBits.push('<span class="hf-label" title="' + esc('stage history\n' + hopText) + '">' + hops.length + ' hop' + (hops.length === 1 ? '' : 's') + (back ? ', ' + back + ' back' : '') + '</span>');
      }
      var reasons = Array.isArray(e.reasons) ? e.reasons : [];
      var reasonsHtml = reasons.length
        ? '<ul class="review-queue-reasons">' + reasons.map(function(r) { return '<li>' + esc(String(r)) + '</li>'; }).join('') + '</ul>'
        : '<div class="review-queue-noreasons">no ranking reasons recorded</div>';
      return '<div class="review-queue-entry">' +
        '<div class="review-queue-head">' +
          '<span class="review-queue-rank">#' + esc(String(e.position || '')) + '</span>' +
          '<div class="review-queue-body">' +
            '<div class="review-queue-titlerow">' + priorityBadge + titleHtml + '</div>' +
            '<div class="review-queue-meta">' + repoRef + ' · ' + metaBits.join(' · ') + '</div>' +
            reasonsHtml +
            reviewQueueSupersededHtml(e) +
            reviewEvidenceButtonHtml(e.repo, e.number) +
          '</div>' +
        '</div>' +
      '</div>';
    }

    // reviewQueueSupersededHtml renders the supersession recommendation:
    // the superseding PR, an optional auto-close deadline, and
    // owner-only Close now / Keep open actions.
    function reviewQueueSupersededHtml(e) {
      var sup = e && e.superseded;
      if (!sup) return '';
      var closerRef = esc(String(sup.closer_repo || '')) + '#' + esc(String(sup.closer_pr || ''));
      var closerHtml = sup.closer_url
        ? '<a href="' + esc(String(sup.closer_url)) + '" target="_blank" rel="noopener noreferrer">' + closerRef + '</a>'
        : closerRef;
      var ends = sup.grace_ends_at ? new Date(sup.grace_ends_at) : null;
      var endsText = 'close as superseded or keep open';
      var extraFiles = sup.reason === 'superseded_extra_files';
      if (extraFiles) {
        var files = Array.isArray(sup.extra_files) ? sup.extra_files : [];
        var named = files.slice(0, 3).map(function (f) { return esc(String(f)); }).join(', ');
        if (files.length > 3) named += ' and ' + (files.length - 3) + ' more';
        endsText = 'adds files it did not touch' + (named ? ' (' + named + ')' : '') + '; review the extra change, keep open recommended';
      } else if (ends && !isNaN(ends.getTime())) {
        endsText = ends.getTime() <= Date.now()
          ? 'grace window ended ' + esc(formatDashboardDateTime(ends)) + '; closes on the next sweep'
          : 'auto-closes ' + esc(formatDashboardDateTime(ends)) + ' unless someone replies';
      }
      var actions = '';
      if (e.repo && e.number && dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) {
        var args = ' data-arg0="' + esc(String(e.repo)) + '" data-arg1="' + esc(String(e.number)) + '" data-arg-types="s,s"';
        actions = '<span class="review-queue-superseded-actions">' +
          '<button type="button" class="hv-btn btn-danger btn-sm" data-action="reviewQueueSupersededClose"' + args + ' title="Close this PR as superseded now">Close now</button>' +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="reviewQueueSupersededKeepOpen"' + args + ' title="Keep this PR open; the sweep will not auto-close it">Keep open</button>' +
          '</span>';
      }
      return '<div class="review-queue-superseded" data-reason="' + (extraFiles ? 'superseded_extra_files' : 'superseded') + '">' +
        '<span class="review-queue-superseded-badge">superseded</span> by ' + closerHtml + ' · ' + endsText + actions +
      '</div>';
    }

    async function reviewQueueSupersededAction(repo, number, action) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return;
      var parts = String(repo || '').split('/');
      if (parts.length !== 2 || !number) return;
      var ref = repo + '#' + number;
      var closing = action === 'close';
      var ok = await hiveConfirm(closing
        ? { title: 'Close ' + ref + ' as superseded?', message: 'Hive posts the superseded comment, adds hive/superseded and closes the PR now. It can be reopened.', confirmLabel: 'Close now', danger: true }
        : { title: 'Keep ' + ref + ' open?', message: 'Hive adds hive/keep-open so the supersession sweep never auto-closes this PR.', confirmLabel: 'Keep open' });
      if (!ok) return;
      try {
        var resp = await fetch('/api/review/queue/' + encodeURIComponent(parts[0]) + '/' + encodeURIComponent(parts[1]) + '/' + encodeURIComponent(String(number)) + '/superseded/' + (closing ? 'close' : 'keep-open'), { method: 'POST', headers: { 'Content-Type': 'application/json' } });
        var body = null;
        try { body = await resp.json(); } catch (_) { body = null; }
        if (!resp.ok) {
          showToast((closing ? 'Close failed: ' : 'Keep open failed: ') + ((body && body.error) || ('HTTP ' + resp.status)), 'error');
          return;
        }
        showToast(closing ? ref + ' closed as superseded' : ref + ' kept open', 'success');
        fetchReviewQueue();
      } catch (err) {
        showToast((closing ? 'Close failed: ' : 'Keep open failed: ') + (err && err.message ? err.message : 'network error'), 'error');
      }
    }

    function reviewQueueSupersededClose(repo, number) { return reviewQueueSupersededAction(repo, number, 'close'); }
    function reviewQueueSupersededKeepOpen(repo, number) { return reviewQueueSupersededAction(repo, number, 'keep-open'); }

    function renderReviewQueue(data) {
      refreshSectionCardShell('review-queue-section');
      refreshSectionCardShell('review-queue-subsection');
      var panel = document.getElementById('review-queue-panel');
      if (!panel) return;
      var items = (data && Array.isArray(data.items)) ? data.items : [];
      _reviewQueueTotal = Number(data && data.total) || 0;
      _reviewQueueLimit = Number(data && data.limit) || REVIEW_QUEUE_PAGE_SIZE;
      _reviewQueueOffset = Number(data && data.offset) || 0;
      _reviewQueueHasMore = !!(data && data.has_more);
      window._reviewQueueHistory = capDashboardArray((data && Array.isArray(data.history)) ? data.history : window._reviewQueueHistory, DASHBOARD_SERVER_HISTORY_CAP);
      var countEl = document.getElementById('review-queue-count');
      if (countEl) countEl.textContent = _reviewQueueTotal ? _reviewQueueTotal + ' PR' + (_reviewQueueTotal === 1 ? '' : 's') : '';
      setSectionSummary('review-queue-subsection', _reviewQueueTotal + ' PR' + (_reviewQueueTotal === 1 ? '' : 's') + ' ranked');
      updateReviewSectionSummary();
      if (!items.length) {
        panel.innerHTML = '<div class="review-queue-empty">Review queue is empty — no open PRs to rank.</div>';
      } else {
        panel.innerHTML = items.map(renderReviewQueueEntry).join('');
      }
      var paging = document.getElementById('review-queue-paging');
      if (paging) {
        var multiPage = _reviewQueueHasMore || _reviewQueueOffset > 0;
        paging.style.display = multiPage ? 'flex' : 'none';
        var prev = document.getElementById('review-queue-prev');
        var next = document.getElementById('review-queue-next');
        if (prev) prev.disabled = _reviewQueueOffset <= 0;
        if (next) next.disabled = !_reviewQueueHasMore;
        var range = document.getElementById('review-queue-range');
        if (range) {
          var first = items.length ? _reviewQueueOffset + 1 : 0;
          var last = _reviewQueueOffset + items.length;
          range.textContent = first + '–' + last + ' of ' + _reviewQueueTotal;
        }
      }
    }

    async function fetchReviewQueue() {
      var panel = document.getElementById('review-queue-panel');
      try {
        var resp = await fetch('/api/review/queue?limit=' + REVIEW_QUEUE_PAGE_SIZE + '&offset=' + _reviewQueueOffset);
        if (!resp.ok) {
          if (panel && !panel.querySelector('.review-queue-entry')) {
            panel.innerHTML = '<div class="review-queue-empty">Review queue unavailable.</div>';
          }
          return;
        }
        var data = await resp.json();
        renderReviewQueue(data);
      } catch (e) {}
    }

    function reviewQueuePrevPage() {
      if (_reviewQueueOffset <= 0) return;
      _reviewQueueOffset = Math.max(0, _reviewQueueOffset - REVIEW_QUEUE_PAGE_SIZE);
      fetchReviewQueue();
    }

    function reviewQueueNextPage() {
      if (!_reviewQueueHasMore) return;
      _reviewQueueOffset += REVIEW_QUEUE_PAGE_SIZE;
      fetchReviewQueue();
    }
    // fetchReviewQueue() — deferred to _deferredInit

    // --- Review pipeline kanban (#11087) ---
    const REVIEW_PIPELINE_POLL_MS = 30000;
    const REVIEW_PIPELINE_STAGES = [
      ['unreviewed', 'Unreviewed'], ['reviewing', 'Reviewing'], ['changes_requested', 'Changes requested'],
      ['fixing', 'Fixing'], ['human_hold', 'Human hold'], ['approved', 'Approved'],
      ['merged', 'Merged'], ['abandoned', 'Abandoned']
    ];
    let _reviewPipelineCards = [];
    let _reviewPipelineTotal = 0;
    let _reviewPipelineFilters = { repo: '', author: '', stage: '' };
    let _reviewPipelineSig = '';

    // reviewPipelineBucket groups cards by stage, applying the filters. Stages
    // the API returns that are not in the known list get their own column.
    function reviewPipelineBucket(cards, filters, knownStages) {
      var f = filters || {};
      var order = (knownStages || []).map(function(s) { return s[0]; });
      var labels = {};
      (knownStages || []).forEach(function(s) { labels[s[0]] = s[1]; });
      var buckets = {};
      (Array.isArray(cards) ? cards : []).forEach(function(c) {
        if (!c) return;
        var stage = String(c.stage || 'unreviewed');
        if (f.repo && c.repo !== f.repo) return;
        if (f.author && (c.hive_authored ? 'agent' : 'contributor') !== f.author) return;
        if (f.stage && stage !== f.stage) return;
        if (!buckets[stage]) buckets[stage] = [];
        buckets[stage].push(c);
        if (order.indexOf(stage) < 0) { order.push(stage); labels[stage] = stage; }
      });
      return order.filter(function(s) { return !f.stage || s === f.stage; }).map(function(s) {
        return { stage: s, label: labels[s], cards: buckets[s] || [] };
      });
    }

    function reviewPipelineLoopHot(c) {
      var cap = Number(c.loop_cap) || 0;
      return cap > 0 && (Number(c.loop_count) || 0) >= cap - 1;
    }

    function renderReviewPipelineCard(c) {
      var title = String(c.title || '');
      var titleHtml = c.url
        ? '<a href="' + esc(String(c.url)) + '" target="_blank" rel="noopener noreferrer" class="review-pipeline-card-title">' + esc(title || ('#' + c.number)) + '</a>'
        : '<span class="review-pipeline-card-title">' + esc(title) + '</span>';
      var since = c.since ? Math.max(0, Math.floor((Date.now() - Date.parse(c.since)) / 3600000)) : 0;
      var meta = esc(String(c.repo || '')) + '#' + esc(String(c.number || '')) + ' · ' +
        (c.hive_authored ? '🤖 agent' : '👤 contributor') + (c.author ? ' @' + esc(String(c.author)) : '') +
        (c.since ? ' · ' + esc(reviewQueueAgeLabel(since)) + ' in stage' : '') +
        (c.trigger === 'event' ? ' · <span title="Review triggered by a webhook push or review request">⚡ event</span>'
          : c.trigger === 'event_pending' ? ' · <span title="Webhook review dispatch queued (debouncing)">⏳ event queued</span>' : '');
      var chips = (Array.isArray(c.reviewers) ? c.reviewers : []).map(function(r) {
        var label = String(r.perspective || '') + (r.model ? ' · ' + r.model : '');
        return '<span class="review-pipeline-chip" title="' + esc(label) + '">' + esc(label) + '</span>';
      }).join('');
      var sev = c.severity || {};
      var sevHtml = ['p0', 'p1', 'p2', 'p3'].map(function(k) {
        var n = Number(sev[k]) || 0;
        return '<span class="review-pipeline-chip review-pipeline-sev ' + k + '" title="' + k.toUpperCase() + ' findings">' + k.toUpperCase() + ' ' + n + '</span>';
      }).join('');
      var loopHtml = Number(c.loop_cap) > 0
        ? '<span class="review-pipeline-loop' + (reviewPipelineLoopHot(c) ? ' hot' : '') + '" title="Fix loops used of the cap">loop ' + (Number(c.loop_count) || 0) + '/' + Number(c.loop_cap) + '</span>'
        : '';
      var na = c.next_action || {};
      var actionHtml = (na.url && na.kind && na.kind !== 'none')
        ? '<div class="review-pipeline-action"><a class="hv-btn btn-secondary btn-sm" href="' + esc(String(na.url)) + '" target="_blank" rel="noopener noreferrer" title="' + esc(String(na.label || '')) + '">' + esc(String(na.label || na.kind)) + '</a></div>'
        : (na.label ? '<div class="review-pipeline-meta">' + esc(String(na.label)) + '</div>' : '');
      var reasons = Array.isArray(c.reasons) ? c.reasons.join(' · ') : '';
      return '<div class="review-pipeline-card" title="' + esc(reasons) + '">' + titleHtml +
        '<div class="review-pipeline-meta">' + meta + '</div>' +
        (chips ? '<div class="review-pipeline-chips">' + chips + '</div>' : '') +
        '<div class="review-pipeline-chips">' + sevHtml + loopHtml + '</div>' + actionHtml +
        reviewEvidenceButtonHtml(c.repo, c.number) + '</div>';
    }

    // Review evidence (#11062): owner-only modal summarising the PR's
    // evidence bundle with authenticated JSON / ZIP downloads from
    // /api/review/evidence.
    function reviewEvidenceButtonHtml(repo, number) {
      if (!repo || !number || !dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return '';
      return '<div class="review-evidence-action"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="openReviewEvidence" data-arg0="' + esc(String(repo)) + '" data-arg1="' + esc(String(number)) + '" data-arg-types="s,s" title="Review evidence bundle for this PR">Evidence</button></div>';
    }

    function reviewEvidenceUrl(repo, number, head, extra) {
      var q = 'repo=' + encodeURIComponent(String(repo)) + '&number=' + encodeURIComponent(String(number));
      if (head) q += '&head=' + encodeURIComponent(String(head));
      return '/api/review/evidence?' + q + (extra ? '&' + extra : '');
    }

    async function reviewEvidenceFetch(url) {
      var headers = {};
      var t = localStorage.getItem('hive-token');
      if (t) headers['Authorization'] = 'Bearer ' + t;
      return fetch(url, { credentials: 'same-origin', cache: 'no-store', headers: headers });
    }

    async function downloadReviewEvidence(repo, number, head, format) {
      var extra = format === 'zip' ? 'format=zip' : 'format=json';
      try {
        var resp = await reviewEvidenceFetch(reviewEvidenceUrl(repo, number, head, extra));
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        var name = '';
        var cd = resp.headers.get('Content-Disposition') || '';
        var m = cd.match(/filename="?([^";]+)"?/);
        if (m) name = m[1];
        if (!name) name = String(repo).replace('/', '-') + '-' + number + '-evidence.' + (format === 'zip' ? 'zip' : 'json');
        if (format === 'zip') { hiveBinaryExportNotice(name, 'Review evidence ZIP bundles are binary archives.'); return; }
        var text = await resp.text();
        hiveExportViewer({title:name,filename:name,text:text,mime:resp.headers.get('Content-Type') || 'application/json',sourceURL:reviewEvidenceUrl(repo, number, head, '')});
      } catch (err) {
        showToast('Evidence download failed: ' + (err && err.message ? err.message : err), 'error');
      }
    }

    function reviewEvidenceSummaryHtml(b) {
      var verdicts = Array.isArray(b.verdicts) ? b.verdicts.length : 0;
      var merge = b.merge
        ? 'merged' + (b.merge.sha ? ' at ' + String(b.merge.sha).slice(0, 12) : '') + (b.merge.actor ? ' by ' + b.merge.actor : '')
        : 'not merged';
      var rows = [
        ['Bundle', b.id || '—'],
        ['Head', b.head_sha || '—'],
        ['Hash', b.hash ? 'sha256:' + b.hash : '—'],
        ['Signed', (b.signed || b.signature) ? 'yes' : 'no'],
        ['Verdicts', String(verdicts)],
        ['Merge', merge]
      ];
      return '<dl class="review-evidence-grid">' + rows.map(function(r) {
        return '<dt>' + esc(r[0]) + '</dt><dd>' + esc(String(r[1])) + '</dd>';
      }).join('') + '</dl>';
    }

    async function openReviewEvidence(repo, number) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return;
      var overlay = document.createElement('div');
      overlay.className = 'hive-dialog-overlay';
      hiveRememberModalOpen(overlay);
      var id = 'review-evidence-' + Math.random().toString(36).slice(2);
      overlay.innerHTML = '<div class="hive-dialog" role="dialog" aria-modal="true" aria-labelledby="' + id + '-title">' +
        '<div id="' + id + '-title" class="hive-dialog-title">Evidence · ' + esc(String(repo)) + '#' + esc(String(number)) + '</div>' +
        '<div class="review-evidence-content"><div class="review-evidence-status">Loading evidence bundle…</div></div>' +
        '<div class="hive-dialog-buttons">' +
          '<button type="button" class="hv-btn btn-secondary hive-dialog-btn evidence-json" disabled>Download JSON</button>' +
          '<button type="button" class="hv-btn btn-secondary hive-dialog-btn evidence-zip" disabled>Download ZIP</button>' +
          '<button type="button" class="hv-btn btn-primary hive-dialog-btn close" data-initial-focus>Close</button>' +
        '</div></div>';
      document.body.appendChild(overlay);
      hiveWireDialog(overlay, function() {}, null, '.close', null, function(close) { close(null); });
      var content = overlay.querySelector('.review-evidence-content');
      var head = '';
      try {
        var resp = await reviewEvidenceFetch(reviewEvidenceUrl(repo, number));
        var body = null;
        try { body = await resp.json(); } catch (_) { body = null; }
        if (!resp.ok) {
          var msg = (body && body.expired)
            ? 'Evidence for this PR has expired under the retention policy.'
            : (resp.status === 404 ? 'No evidence bundle has been recorded for this PR yet.' : ((body && body.error) || ('HTTP ' + resp.status)));
          content.innerHTML = '<div class="review-evidence-status">' + esc(msg) + '</div>';
          return;
        }
        head = String((body && body.head_sha) || '');
        content.innerHTML = reviewEvidenceSummaryHtml(body || {});
        var jsonBtn = overlay.querySelector('.evidence-json');
        var zipBtn = overlay.querySelector('.evidence-zip');
        jsonBtn.disabled = false;
        zipBtn.disabled = false;
        jsonBtn.addEventListener('click', function() { downloadReviewEvidence(repo, number, head, 'json'); });
        zipBtn.addEventListener('click', function() { downloadReviewEvidence(repo, number, head, 'zip'); });
      } catch (err) {
        content.innerHTML = '<div class="review-evidence-status">' + esc('Could not load evidence: ' + (err && err.message ? err.message : err)) + '</div>';
      }
    }

    function reviewPipelineSubsectionSummary() {
      var counts = { unreviewed: 0, reviewing: 0, changes_requested: 0 };
      (Array.isArray(_reviewPipelineCards) ? _reviewPipelineCards : []).forEach(function(c) {
        var stage = String(c && c.stage || 'unreviewed');
        if (Object.prototype.hasOwnProperty.call(counts, stage)) counts[stage]++;
      });
      return counts.unreviewed + ' unreviewed · ' + counts.reviewing + ' reviewing · ' + counts.changes_requested + ' changes requested';
    }

    function updateReviewSectionSummary() {
      var parts = [];
      if (Number.isFinite(Number(_reviewPipelineTotal))) parts.push(Number(_reviewPipelineTotal) + ' pipeline');
      if (Number.isFinite(Number(_reviewQueueTotal))) parts.push(Number(_reviewQueueTotal) + ' queue');
      setSectionSummary('review-queue-section', parts.length ? parts.join(' · ') : '0 PRs');
    }

    function renderReviewPipelineBoard() {
      refreshSectionCardShell('review-pipeline-subsection');
      var panel = document.getElementById('review-pipeline-panel');
      if (!panel) return;
      var cols = reviewPipelineBucket(_reviewPipelineCards, _reviewPipelineFilters, REVIEW_PIPELINE_STAGES);
      var total = cols.reduce(function(n, c) { return n + c.cards.length; }, 0);
      var countEl = document.getElementById('review-pipeline-count');
      if (countEl) countEl.textContent = total ? total + ' PR' + (total === 1 ? '' : 's') : '';
      _reviewPipelineTotal = total;
      setSectionSummary('review-pipeline-subsection', reviewPipelineSubsectionSummary());
      updateReviewSectionSummary();
      if (!_reviewPipelineCards.length) {
        panel.innerHTML = '<div class="review-pipeline-empty">No open PRs in the review pipeline.</div>';
        return;
      }
      var board = panel.querySelector('.review-pipeline-board');
      var left = board ? board.scrollLeft : 0;
      var scrolls = board ? Array.prototype.map.call(board.querySelectorAll('.review-pipeline-cards'), function(el) { return el.scrollTop; }) : [];
      panel.innerHTML = '<div class="review-pipeline-board">' + cols.map(function(col) {
        return '<div class="review-pipeline-col" data-stage="' + esc(col.stage) + '">' +
          '<div class="review-pipeline-colhead"><span>' + esc(col.label) + '</span><span class="review-pipeline-colcount">' + col.cards.length + '</span></div>' +
          '<div class="review-pipeline-cards">' + (col.cards.length ? col.cards.map(renderReviewPipelineCard).join('') : '<div class="review-pipeline-colempty">No PRs</div>') + '</div></div>';
      }).join('') + '</div>';
      board = panel.querySelector('.review-pipeline-board');
      if (board) {
        board.scrollLeft = left;
        Array.prototype.forEach.call(board.querySelectorAll('.review-pipeline-cards'), function(el, i) { if (scrolls[i]) el.scrollTop = scrolls[i]; });
      }
    }

    function reviewPipelineFilter(key, value) {
      _reviewPipelineFilters[key] = String(value || '');
      renderReviewPipelineBoard();
    }

    function populateReviewPipelineFilters() {
      var repoSel = document.getElementById('review-pipeline-repo');
      if (repoSel) {
        var repos = Array.from(new Set(_reviewPipelineCards.map(function(c) { return c.repo; }).filter(Boolean))).sort();
        var cur = _reviewPipelineFilters.repo;
        repoSel.innerHTML = '<option value="">All repos</option>' + repos.map(function(r) { return '<option value="' + esc(r) + '"' + (r === cur ? ' selected' : '') + '>' + esc(r) + '</option>'; }).join('');
      }
      var stageSel = document.getElementById('review-pipeline-stage');
      if (stageSel && stageSel.options.length <= 1) {
        stageSel.innerHTML = '<option value="">All stages</option>' + REVIEW_PIPELINE_STAGES.map(function(s) { return '<option value="' + s[0] + '">' + esc(s[1]) + '</option>'; }).join('');
      }
    }

    async function fetchReviewPipeline() {
      var panel = document.getElementById('review-pipeline-panel');
      try {
        var resp = await fetch('/api/review/pipeline?limit=200');
        if (!resp.ok) {
          if (panel && !panel.querySelector('.review-pipeline-board')) {
            panel.innerHTML = '<div class="review-pipeline-empty">Review pipeline unavailable.</div>';
          }
          return;
        }
        var data = await resp.json();
        var cards = (data && Array.isArray(data.cards)) ? data.cards : [];
        var sig = JSON.stringify(cards);
        if (sig === _reviewPipelineSig) return;
        _reviewPipelineSig = sig;
        _reviewPipelineCards = cards;
        populateReviewPipelineFilters();
        renderReviewPipelineBoard();
      } catch (e) {
        if (panel && !panel.querySelector('.review-pipeline-board')) {
          panel.innerHTML = '<div class="review-pipeline-empty">Review pipeline unavailable.</div>';
        }
      }
    }

    // --- Contributors & Leaderboard ---
    const CONTRIBUTOR_POLL_MS = 10000;
    const LEADERBOARD_POLL_MS = 30000;

    const TRUST_TIER_COLORS = {
      newcomer:    'var(--yellow)',
      contributor: 'var(--blue)',
      trusted:     'var(--green)',
      merger:      'var(--pink, #f778ba)',
      advisor:     'var(--purple)',
      revoked:     'var(--red)',
    };

    function trustTierBadge(tier) {
      return `<span class="trust-badge" style="display:inline-block;padding:var(--sp-1) var(--sp-4);border-radius:var(--r-sm);font-size:0.7rem;color:var(--muted);border:1px solid var(--line)">${esc(tier || 'unknown')}</span>`;
    }

    // tierProvenance answers "who set this tier, when and why" from the
    // contributor's tier_history (GET /api/contributors): the newest entry
    // whose tier matches the current one. Empty when the registry predates
    // tier history, so the chip stays plain rather than inventing a source.
    function tierProvenance(c) {
      const hist = Array.isArray(c && c.tier_history) ? c.tier_history : [];
      const cur = String((c && c.trust_tier) || '').toLowerCase();
      for (let i = hist.length - 1; i >= 0; i--) {
        const h = hist[i];
        if (!h || String(h.tier || '').toLowerCase() !== cur) continue;
        const whenText = h.at ? formatDashboardDateTime(h.at, undefined, h.at) : '';
        const parts = [`${cur} since ${whenText}`];
        if (h.by) parts.push(`by ${h.by}`);
        if (h.reason) parts.push(h.reason);
        if (h.prev) parts.push(`(was ${h.prev})`);
        return parts.join(' · ');
      }
      return '';
    }

