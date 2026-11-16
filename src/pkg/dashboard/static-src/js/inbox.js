    // ── Inbox: source-agnostic triage of voice recordings and dictations ──
    // Items come from knowledge connectors (plaud, wisprflow, …); each one is
    // already a Knowledge fact. Routing them to spek (RFC + child issues) or
    // dibs happens inside the hive via POST /api/inbox/route.
    const INBOX_POLL_MS = 60000;
    const INBOX_BUSY_POLL_MS = 5000;
    const INBOX_SOURCE_ICONS = { plaud: '🎙️', wisprflow: '🗣️', zoom: '📹', teams: '👥', discord: '🎧', 'google-meet': '🎥', webex: '🎬', 'slack-huddles': '💬', 'fireflies': '🪲', 'fathom': '🌊', 'limitless': '📿', 'granola': '🥣', 'otter': '🦦', 'file-drop': '📎', webhook: '🔗' };
    const INBOX_DEST_LABELS = { spek_rfc: 'RFC', dibs: 'dibs' };
    // Summaries longer than this many characters start collapsed.
    const INBOX_SUMMARY_COLLAPSE_CHARS = 280;
    let _inboxCache = null;
    let _inboxSelected = new Set();
    let _inboxShowProcessed = false;
    let _inboxBusyTimer = null;
    // Per-item view state: expanded summaries, open transcript disclosures
    // and fetched content (summary/transcript/highlights). Audio links are
    // never kept here: the player streams from the hive's own origin.
    let _inboxExpanded = new Set();
    let _inboxTranscriptOpen = new Set();
    let _inboxContent = {};
    // Items whose Transcribe request is in flight (button spinner).
    let _inboxTranscribing = new Set();
    // Bytes per MB for the transcription size limit field.
    const INBOX_TX_BYTES_PER_MB = 1048576;
    const INBOX_TX_STATUS_LABELS = { queued: 'Transcription queued', running: 'Transcribing', done: 'Machine transcript', failed: 'Transcription failed', skipped: 'Not transcribed' };
    // While a transcription is queued or running the Inbox polls the cheap
    // GET /api/inbox/transcriptions every INBOX_TX_POLL_MS and redraws only
    // the item's progress slot; when a job leaves that list the full Inbox
    // is re-read once for its outcome.
    const INBOX_TX_POLL_MS = 3000;
    let _inboxTxPollTimer = null;

    function inboxSourceIcon(src) { return INBOX_SOURCE_ICONS[src] || '📥'; }
    function inboxItemKey(it) { return it.source + '/' + it.id; }
    // Playback is a read of private audio: owners and read-write sessions.
    function inboxViewerCanPlay() { return window._hiveRole === 'owner' || window._hiveRole === 'read-write'; }
    function inboxApiPath(src, id, leaf) { return '/api/inbox/' + encodeURIComponent(src) + '/' + encodeURIComponent(id) + '/' + leaf; }
    function inboxFmtDuration(ms) {
      const s = Math.round((Number(ms) || 0) / 1000);
      if (s <= 0) return '';
      const m = Math.floor(s / 60);
      return m >= 60 ? Math.floor(m / 60) + 'h ' + (m % 60) + 'm' : m + 'm ' + (s % 60) + 's';
    }
    // Routes in flight re-read the whole Inbox; transcriptions use the
    // lighter progress poll (inboxTxPoll) instead.
    function inboxRouteBusy(items) {
      return (items || []).some(it => Object.values(it.routes || {}).some(r => r && (r.status === 'queued' || r.status === 'running')));
    }
    function inboxTxBusy(st) { return !!st && (st.status === 'queued' || st.status === 'running'); }
    function inboxTxAnyBusy(items) { return (items || []).some(it => inboxTxBusy(it.transcription)); }

    async function fetchInbox() {
      try {
        const r = await fetch('/api/inbox');
        if (!r.ok) {
          // Never leave "Loading inbox…" up: a 404/500 becomes a visible error.
          const body = await r.json().catch(() => ({}));
          _inboxCache = { items: [], sources: [], settings: {}, error: body.error || ('HTTP ' + r.status) };
          renderInbox();
          return;
        }
        _inboxCache = await r.json();
        renderInbox();
        clearTimeout(_inboxBusyTimer);
        if (inboxRouteBusy(_inboxCache.items)) _inboxBusyTimer = setTimeout(fetchInbox, INBOX_BUSY_POLL_MS);
        if (inboxTxAnyBusy(_inboxCache.items)) inboxTxPollSchedule();
      } catch (e) { /* transient — leave last render in place */ }
    }
    dashboardScheduleWork('inbox', fetchInbox).catch(console.error);
    dashboardSetInterval('inbox', fetchInbox, INBOX_POLL_MS);

    function inboxStatePill(dest, st) {
      const label = INBOX_DEST_LABELS[dest] || dest;
      if (!st) return '';
      const status = st.status === 'filed' || st.status === 'delegated' ? 'ok' : (st.status === 'failed' ? 'error' : 'syncing');
      const text = st.status === 'filed' ? label + ' filed' : st.status === 'delegated' ? label + ' handed to agent' : label + ' ' + st.status;
      const title = st.error ? escapeHtml(st.error) : (st.repo ? escapeHtml(st.repo) : '');
      let html = '<span class="kc-pill" data-status="' + status + '" title="' + title + '">' + escapeHtml(text) + '</span>';
      // A failed route shows its error inline (not only on hover) so a broken
      // hand-off never reads as silently pending; delegated shows the agent.
      if (st.status === 'failed' && st.error) html += ' <span class="kc-error muted-xs" role="alert">' + escapeHtml(st.error) + '</span>';
      else if (st.status === 'delegated' && st.note) html += ' <span class="muted-xs">' + escapeHtml(st.note) + '</span>';
      if (st.url) html += ' <a class="inbox-link" href="' + escapeHtml(st.url) + '" target="_blank" rel="noopener">open ↗</a>';
      if (Array.isArray(st.children) && st.children.length) html += ' <span class="muted-xs">' + st.children.length + ' child issue' + (st.children.length === 1 ? '' : 's') + '</span>';
      return html;
    }

    // Summary / memo text: collapsed past INBOX_SUMMARY_COLLAPSE_CHARS with a
    // more/less toggle. Falls back to the pending note or the fact excerpt.
    function inboxSummaryHtml(it, key) {
      const src = escapeHtml(it.source), id = escapeHtml(it.id);
      const cached = _inboxContent[key];
      const summary = (cached && cached.summary) || it.summary || '';
      if (summary) {
        const long = summary.length > INBOX_SUMMARY_COLLAPSE_CHARS;
        const expanded = _inboxExpanded.has(key);
        let html = '<div class="inbox-summary' + (long && !expanded ? ' is-collapsed' : '') + '">' + escapeHtml(summary) + '</div>';
        if (long) html += '<button type="button" class="hv-btn btn-secondary btn-sm inbox-more" data-action="inboxToggleSummary" data-arg0="' + src + '" data-arg1="' + id + '">' + (expanded ? 'less' : 'more') + '</button>';
        return html;
      }
      if (it.pending || (cached && cached.pending)) {
        const tx = it.transcribable && !it.transcribe_unavailable ? ' Hive can transcribe this: click 🎧 Transcribe.'
          : (it.transcribe_unavailable ? ' Hive can transcribe this once transcription is set up (see 🎧 Transcribe below).' : '');
        // Meeting platforms whose pending items read "<label> has no transcript"
        // (inline so the extracted function stays self-contained in tests).
        const meetingLabel = { zoom: 'Zoom', teams: 'Teams', 'google-meet': 'Google Meet', webex: 'Webex', 'slack-huddles': 'Slack', 'fireflies': 'Fireflies', 'fathom': 'Fathom', 'limitless': 'Limitless', 'granola': 'Granola', 'otter': 'Otter' }[it.source];
        if (meetingLabel) return '<div class="inbox-pending muted-xs" title="The platform reports no transcript or summary for this recording yet">Not transcribed yet: ' + meetingLabel + ' has no transcript or summary for this recording (cloud transcription may still be processing or is disabled); the hive re-checks it on every sync.' + tx + '</div>';
        if (it.source === 'discord') return '<div class="inbox-pending muted-xs" title="Discord has no transcripts; none was posted with this recording">Not transcribed yet: Discord has no built-in transcription and no transcript file was posted with this recording; play it here, or post a .vtt/.txt transcript in the same message. The hive re-checks it on every sync.' + tx + '</div>';
        return '<div class="inbox-pending muted-xs" title="The MCP reports no transcript or summary block for this recording">Not transcribed yet in Plaud: Plaud has not transcribed this recording (open it in the Plaud app and run transcription); the hive re-checks it on every sync.' + tx + '</div>';
      }
      return it.excerpt ? '<div class="kb-fact-snippet">' + escapeHtml(it.excerpt) + '</div>' : '';
    }

    // Transcript disclosure body: loaded on demand from the content endpoint.
    function inboxTranscriptHtml(it, key) {
      if (!_inboxTranscriptOpen.has(key)) return '';
      const c = _inboxContent[key];
      if (!c) return '<div class="inbox-transcript muted-xs">Loading transcript…</div>';
      if (c.error) return '<div class="inbox-transcript kc-error muted-xs">' + escapeHtml(c.error) + '</div>';
      let html = '<div class="inbox-transcript">';
      if (c.transcript && c.transcript_label) html += '<div class="muted-xs"><strong>' + escapeHtml(c.transcript_label) + '</strong></div>';
      if (c.transcript) html += '<pre class="inbox-transcript-text">' + escapeHtml(c.transcript) + '</pre>';
      else html += '<div class="muted-xs">No transcript yet' + (c.pending ? (it.source === 'plaud' ? ': Plaud has not transcribed this recording.' : ': the meeting platform has not produced a transcript for this recording.') + (it.transcribable || it.transcribe_unavailable ? ' Hive can transcribe this: see 🎧 Transcribe on this item.' : '') : '.') + '</div>';
      if (c.highlights) html += '<div class="muted-xs inbox-highlights"><strong>Highlights</strong> (flagged on the device)<pre class="inbox-transcript-text">' + escapeHtml(c.highlights) + '</pre></div>';
      return html + '</div>';
    }

    // Why 🎧 Transcribe is unavailable, by the backend's transcribe_unavailable code.
    function inboxTxUnavailableText(reason) {
      return {
        disabled: 'Transcription is off: enable it in Inbox ⚙ → Transcription',
        no_key: 'The transcription provider is not ready on this hive: see Inbox ⚙ → Transcription for what is missing',
        no_consent: 'Consent not acknowledged',
        budget: 'Monthly budget used'
      }[reason] || '';
    }

    // Time left as people say it: "less than a minute left", "about 4 min
    // left", "about 25 min left" (5-minute steps past 10), "about 1 h 10 min
    // left". '' when unknown.
    function inboxTxEtaText(etaSeconds) {
      const s = Number(etaSeconds);
      if (!(s > 0)) return '';
      if (s < 60) return 'less than a minute left';
      const mins = Math.round(s / 60);
      if (mins < 10) return 'about ' + Math.max(1, mins) + ' min left';
      const stepped = Math.round(s / 300) * 5;
      if (stepped < 60) return 'about ' + stepped + ' min left';
      const h = Math.floor(stepped / 60), m = stepped % 60;
      return 'about ' + h + ' h' + (m ? ' ' + m + ' min' : '') + ' left';
    }
    function inboxTxElapsedText(startedAt, nowMs) {
      const t = startedAt ? Date.parse(startedAt) : NaN;
      if (isNaN(t) || t <= 0) return '';
      const s = Math.max(0, Math.round(((nowMs || Date.now()) - t) / 1000));
      if (s < 60) return s + 's';
      const m = Math.floor(s / 60);
      return m < 60 ? m + 'm ' + (s % 60) + 's' : Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
    }
    // Progress bar for a queued or running transcription, in the connector
    // sync progress styling (kc-progress). Queued shows its place in line;
    // running shows phase, percent, time left (aria-live) and elapsed time.
    function inboxTxProgressHtml(st, nowMs, then) {
      if (!st || (st.status !== 'queued' && st.status !== 'running')) return '';
      const phases = { downloading: 'Downloading audio', decoding: 'Preparing audio', transcribing: 'Transcribing', finalizing: 'Finishing' };
      if (st.status === 'queued' || !st.started_at) {
        const ahead = Number(st.queue_position) || 0;
        const thenText = then ? ', then → ' + then : '';
        const wait = (then ? 'Transcribing' + thenText + ': ' : '') + (ahead > 0 ? 'Waiting: ' + ahead + (ahead === 1 ? ' transcription' : ' transcriptions') + ' ahead (one runs at a time)' : 'Starting…');
        return '<div class="kc-progress" data-inbox-tx-progress data-phase="queued" role="progressbar" aria-label="Transcription queued" aria-valuetext="' + escapeHtml(wait) + '">' +
          '<progress class="kc-progress-bar" aria-hidden="true"></progress>' +
          '<div class="kc-progress-copy"><span class="kc-progress-label">' + escapeHtml(wait) + '</span></div></div>';
      }
      const pct = Math.max(0, Math.min(100, Math.round((Number(st.progress) || 0) * 100)));
      const phase = phases[st.phase] || 'Transcribing';
      const eta = st.phase === 'finalizing' ? 'almost done' : (inboxTxEtaText(st.eta_seconds) || (pct > 0 ? 'finishing up' : 'estimating time left'));
      const elapsed = inboxTxElapsedText(st.started_at, nowMs);
      const label = phase + (then ? ', then → ' + then : '') + ' · ' + pct + '%';
      return '<div class="kc-progress" data-inbox-tx-progress data-phase="' + escapeHtml(st.phase || 'transcribing') + '" role="progressbar" aria-label="Transcription progress" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' + pct + '" aria-valuetext="' + escapeHtml(label + ', ' + eta) + '">' +
        '<progress class="kc-progress-bar" max="100" value="' + pct + '" aria-hidden="true"></progress>' +
        '<div class="kc-progress-copy"><span class="kc-progress-label" title="' + escapeHtml(st.estimated ? 'Estimated from this provider\'s usual speed' : 'Measured from the transcript so far') + '">' + escapeHtml(label) + ' · <span data-inbox-tx-eta aria-live="polite">' + escapeHtml(eta) + '</span></span>' +
        (elapsed ? '<span class="kc-progress-elapsed" title="elapsed">' + escapeHtml(elapsed) + '</span>' : '') + '</div></div>';
    }

    // Machine transcription: a state pill (spinner while queued/running, the
    // reason when it failed or was skipped), the progress bar while busy, and
    // the 🎧 Transcribe / Retry button on pending items with audio, for
    // owners and read-write sessions.
    function inboxTranscriptionHtml(it, key) {
      const st = it.transcription;
      const src = escapeHtml(it.source), id = escapeHtml(it.id);
      // What the item's Knowledge fact holds, as a status pill (not a
      // control): the source's own text, this hive's machine transcript, or
      // metadata only while no text exists yet.
      const factStates = {
        ready: { label: 'In knowledge', status: 'ok', title: 'Stored as a Knowledge fact with the source\'s own text; knowledge search and agents see it.' },
        machine: { label: 'In knowledge (machine text)', status: 'ok', title: 'Stored as a Knowledge fact with this hive\'s machine transcript; the source\'s own text replaces it when it exists.' },
        pending: { label: 'Pending text', status: 'not loaded', title: 'Stored as a Knowledge fact with metadata only (title, date, duration, highlights); no text yet, so knowledge search finds no words.' },
      };
      const fs = factStates[it.fact_state] || factStates.ready;
      let html = it.fact_slug ? '<span class="kc-pill" role="status" data-status="' + fs.status + '" data-fact-state="' + escapeHtml(it.fact_state || 'ready') + '" title="' + escapeHtml(fs.title) + '">' + escapeHtml(fs.label) + '</span>' : '';
      if (st && st.status) {
        const busy = st.status === 'queued' || st.status === 'running';
        const pill = st.status === 'done' ? 'ok' : (st.status === 'failed' ? 'error' : (busy ? 'syncing' : ''));
        let label = INBOX_TX_STATUS_LABELS[st.status] || st.status;
        if (st.status === 'done' && st.provider) label += ' (' + st.provider + '/' + st.model + ')';
        const when = st.updated_at && !String(st.updated_at).startsWith('0001') ? formatDashboardDateTime(st.updated_at) : '';
        html += '<span class="kc-pill" data-status="' + pill + '" title="' + escapeHtml(when ? 'Updated ' + when : '') + '">' +
          (busy ? '<span class="inbox-tx-spinner" aria-hidden="true"></span> ' : '') + escapeHtml(label) + '</span>';
        if (st.reason && (st.status === 'failed' || st.status === 'skipped')) html += ' <span class="muted-xs inbox-tx-reason" role="alert">' + escapeHtml(st.reason) + '</span>';
        if (busy) html += inboxTxProgressHtml(st, undefined, inboxIntentThen(it));
      }
      const why = inboxTxUnavailableText(it.transcribe_unavailable);
      if (why) {
        // Everyone sees the reason; only owner/read-write see the control and the setup link.
        const canAct = inboxViewerCanPlay();
        if (canAct) html += '<button type="button" class="hv-btn btn-secondary btn-sm inbox-transcribe" disabled aria-disabled="true" title="' + escapeHtml(why) + '">🎧 Transcribe</button>';
        html += ' <span class="muted-xs inbox-tx-reason" data-tx-unavailable="' + escapeHtml(it.transcribe_unavailable) + '">' + escapeHtml(why) + '</span>';
        if (canAct) html += ' <button type="button" class="hv-btn btn-secondary btn-sm inbox-tx-setup" data-action="inboxOpenSettings" title="Open Inbox ⚙ → Transcription">Open Transcription settings</button>';
      } else if (it.transcribable && inboxViewerCanPlay()) {
        const inflight = _inboxTranscribing.has(key);
        const retry = st && (st.status === 'failed' || st.status === 'skipped');
        html += '<button type="button" class="hv-btn btn-secondary btn-sm inbox-transcribe" data-action="inboxTranscribe" data-arg0="' + src + '" data-arg1="' + id + '"' + (inflight ? ' disabled aria-busy="true"' : '') +
          ' title="Have this hive transcribe the recording with the provider in Inbox ⚙ → Transcription; the transcript is kept on this hive only">' +
          (inflight ? '<span class="inbox-tx-spinner" aria-hidden="true"></span> Sending…' : (retry ? '🎧 Retry transcription' : '🎧 Transcribe')) + '</button>';
      }
      return html ? '<div class="inbox-tx" data-inbox-tx-slot="' + escapeHtml(key || '') + '">' + html + '</div>' : '';
    }

    // inboxTxApplyActive merges the live states from
    // GET /api/inbox/transcriptions into the cached items and reports the
    // keys that were busy before and are no longer listed (finished).
    function inboxTxApplyActive(items, active) {
      const live = {};
      for (const a of active || []) live[a.source + '/' + a.id] = a.state;
      const finished = [];
      for (const it of items || []) {
        const key = it.source + '/' + it.id;
        if (live[key]) it.transcription = Object.assign({}, it.transcription || {}, live[key]);
        else if (inboxTxBusy(it.transcription)) finished.push(key);
      }
      return finished;
    }
    function inboxTxPollSchedule() {
      if (_inboxTxPollTimer) return;
      _inboxTxPollTimer = setTimeout(inboxTxPoll, INBOX_TX_POLL_MS);
    }
    // One progress poll: redraw each busy item's slot in place (the rest of
    // the Inbox, including a playing recording, is left alone); re-read the
    // Inbox once a job finishes; stop when nothing is busy.
    async function inboxTxPoll() {
      _inboxTxPollTimer = null;
      const items = (_inboxCache || {}).items || [];
      if (!inboxTxAnyBusy(items)) return;
      let finished = [];
      try {
        const r = await fetch('/api/inbox/transcriptions');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const data = await r.json();
        finished = inboxTxApplyActive(items, data.active);
      } catch (e) {
        inboxTxPollSchedule();
        return;
      }
      if (finished.length) { for (const k of finished) delete _inboxContent[k]; await fetchInbox(); return; }
      const panel = document.getElementById('inbox-panel');
      for (const it of items) {
        if (!inboxTxBusy(it.transcription)) continue;
        const key = it.source + '/' + it.id;
        const slot = panel ? Array.from(panel.querySelectorAll('[data-inbox-tx-slot]')).find(el => el.getAttribute('data-inbox-tx-slot') === key) : null;
        if (slot) slot.outerHTML = inboxTranscriptionHtml(it, key);
      }
      inboxTxPollSchedule();
    }

    async function inboxTranscribe(src, id) {
      const key = src + '/' + id;
      if (_inboxTranscribing.has(key)) return;
      _inboxTranscribing.add(key);
      renderInbox();
      try {
        const r = await fetch(inboxApiPath(src, id, 'transcribe'), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        const st = data.state || {};
        if (st.status === 'skipped') showToast('Not transcribed: ' + (st.reason || 'skipped'), 'error');
        else showToast('Transcription queued; the item updates when it finishes.', 'success');
        delete _inboxContent[key];
      } catch (e) {
        showToast('Transcription failed: ' + e.message, 'error');
      } finally {
        _inboxTranscribing.delete(key);
      }
      await fetchInbox();
    }

    function inboxItemHtml(it) {
      const key = inboxItemKey(it);
      const checked = _inboxSelected.has(key) ? ' checked' : '';
      const when = it.recorded_at && !String(it.recorded_at).startsWith('0001') ? relativeTimeShort(it.recorded_at) : '';
      const dur = inboxFmtDuration(it.duration_ms);
      // The backing Knowledge fact is a plain link in the metadata line, not
      // an action: it only opens the fact.
      const factLink = it.fact_slug ? '<a href="#" class="inbox-fact-link" data-action="inboxOpenFact" data-prevent="1" data-arg0="' + escapeHtml(it.fact_slug) + '" data-arg1="' + escapeHtml(it.layer || '') + '" title="Open this item\'s Knowledge fact">open in Knowledge</a>' : '';
      const meta = [escapeHtml(it.source), when ? 'recorded ' + when : '', dur, factLink].filter(Boolean).join(' · ');
      const routes = it.routes || {};
      let pills = '';
      for (const d of Object.keys(routes)) pills += inboxStatePill(d, routes[d]) + ' ';
      if (it.dismissed) pills += '<span class="kc-pill" title="Kept as a Knowledge fact only">knowledge only</span>';
      for (const d of Object.keys(it.share_intents || {})) {
        const si = it.share_intents[d];
        if (si && si.status === 'failed') pills += '<span class="kc-pill" data-status="error" title="' + escapeHtml(si.error || '') + '">→ ' + escapeHtml(inboxIntentLabel(d)) + ' failed</span> <span class="kc-error muted-xs" role="alert">' + escapeHtml(si.error || '') + '</span> ';
      }
      const owner = viewerIsOwner();
      const src = escapeHtml(it.source), id = escapeHtml(it.id);
      let actions = '';
      if (inboxViewerCanPlay()) actions += '<button type="button" class="hv-btn btn-secondary btn-sm inbox-play" data-action="inboxPlay" data-arg0="' + src + '" data-arg1="' + id + '" title="Stream the recording through this hive and play it here">▶ Play recording</button>';
      actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxToggleTranscript" data-arg0="' + src + '" data-arg1="' + id + '" aria-expanded="' + (_inboxTranscriptOpen.has(key) ? 'true' : 'false') + '">' + (_inboxTranscriptOpen.has(key) ? '▾' : '▸') + ' Transcript</button>';
      actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxShare" data-arg-types="s,s,t" data-arg0="' + src + '" data-arg1="' + id + '" aria-haspopup="menu" aria-expanded="false" title="File this item in the hive, or share a link to it (not the recording)">🔗 Share ▾</button>';
      return '<div class="kb-fact-item inbox-item" data-inbox-key="' + escapeHtml(key) + '">' +
        (inboxCanShare(window._hiveRole) ? '<label class="inbox-check"><input type="checkbox" data-change-action="inboxToggleSelect" data-arg0="' + src + '" data-arg1="' + id + '"' + checked + ' aria-label="Select ' + escapeHtml(it.title) + '"></label>' : '') +
        '<div class="inbox-icon" title="' + src + '">' + inboxSourceIcon(it.source) + '</div>' +
        '<div class="kb-fact-body"><div class="kb-fact-title">' + escapeHtml(it.title || it.id) + '</div>' +
        '<div class="muted-xs">' + meta + '</div>' +
        inboxSummaryHtml(it, key) +
        '<div class="inbox-media" data-inbox-media="' + escapeHtml(key) + '"></div>' +
        (pills ? '<div class="inbox-pills">' + pills + '</div>' : '') +
        inboxTranscriptionHtml(it, key) +
        '<div class="kc-actions mt-2">' + actions + '</div>' +
        inboxTranscriptHtml(it, key) + '</div></div>';
    }

    function inboxToggleSummary(src, id) {
      const key = src + '/' + id;
      if (_inboxExpanded.has(key)) _inboxExpanded.delete(key); else _inboxExpanded.add(key);
      renderInbox();
    }

    async function inboxLoadContent(src, id) {
      const key = src + '/' + id;
      try {
        const r = await fetch(inboxApiPath(src, id, 'content'));
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _inboxContent[key] = data;
      } catch (e) {
        _inboxContent[key] = { error: 'Could not load the transcript: ' + e.message };
      }
      renderInbox();
    }
    function inboxToggleTranscript(src, id) {
      const key = src + '/' + id;
      if (_inboxTranscriptOpen.has(key)) { _inboxTranscriptOpen.delete(key); renderInbox(); return; }
      _inboxTranscriptOpen.add(key);
      renderInbox();
      const cached = _inboxContent[key];
      if (!cached || cached.error) inboxLoadContent(src, id);
    }

    // ▶ Play recording: the player streams from the hive's own origin
    // (GET …/audio/stream). The hive mints the presigned link and proxies
    // the bytes, passing Range through for seeking, so the browser never
    // loads cloud storage directly: the CSP (default-src 'self', no
    // media-src) refused that with "Media load rejected by URL safety check",
    // and the presigned link stays off the page.
    function inboxStreamURL(src, id) { return inboxApiPath(src, id, 'audio/stream'); }
    // inboxAudioErrorReason explains a failed <audio>: the hive's JSON error
    // when the stream answered one, else what the browser could not decode.
    async function inboxAudioErrorReason(src, id, audio) {
      try {
        const r = await fetch(inboxStreamURL(src, id), { cache: 'no-store', headers: { Range: 'bytes=0-0' } });
        if (!r.ok) {
          const data = await r.json().catch(() => ({}));
          return data.error || ('HTTP ' + r.status);
        }
        const type = (r.headers.get('content-type') || '').split(';')[0].trim();
        if (r.body && r.body.cancel) r.body.cancel().catch(() => {});
        if (type && audio && audio.canPlayType && !audio.canPlayType(type)) return 'this browser cannot play ' + type + ' recordings (Safari cannot play some Ogg Opus files); try another browser';
      } catch (e) { return e.message; }
      return 'the browser could not play this recording';
    }
    function inboxPlay(src, id) {
      const key = src + '/' + id;
      const target = document.querySelector('[data-inbox-media="' + CSS.escape(key) + '"]');
      if (!target) return;
      target.innerHTML = '';
      const audio = document.createElement('audio');
      audio.controls = true;
      audio.autoplay = true;
      audio.preload = 'auto';
      audio.className = 'inbox-audio';
      audio.addEventListener('error', async () => {
        const reason = await inboxAudioErrorReason(src, id, audio);
        showToast('Audio unavailable: ' + reason, 'error');
      });
      audio.src = inboxStreamURL(src, id);
      target.appendChild(audio);
      const note = document.createElement('div');
      note.className = 'muted-xs';
      note.textContent = 'Streaming through this hive; the recording is not stored. Click ▶ Play recording again to restart.';
      target.appendChild(note);
    }

    // 🔗 Share: a deep link to this item in the Inbox (dashboard URL plus
    // #section-inbox-section?item=<source/id>), never the media URL. The
    // recipient still signs in to this hive and needs owner or read-write
    // access to play the recording.
    const INBOX_SHARE_HASH = '#section-inbox-section?item=';
    function inboxShareLink(src, id) {
      return location.origin + location.pathname + location.search + INBOX_SHARE_HASH + encodeURIComponent(src + '/' + id);
    }
    // inboxFocusKeyFromHash returns the source/id an Inbox deep link names.
    function inboxFocusKeyFromHash(hash) {
      const h = String(hash || '');
      const at = h.indexOf(INBOX_SHARE_HASH);
      if (at < 0) return '';
      try { return decodeURIComponent(h.slice(at + INBOX_SHARE_HASH.length).split('&')[0]); } catch (e) { return ''; }
    }
    async function inboxShareCopy(src, id) {
      const url = inboxShareLink(src, id);
      if (await writeClipboardText(url)) showToast('Inbox link copied', 'success');
      else showToast('Copy this Inbox link: ' + url, 'info', true);
    }
    async function inboxShareNative(src, id) {
      const it = inboxFind(src, id);
      try { await navigator.share({ title: (it && it.title) || 'Inbox item', url: inboxShareLink(src, id) }); }
      catch (e) { if (!e || e.name !== 'AbortError') await inboxShareCopy(src, id); }
    }
    // The Share menu has two groups. Hive actions file or keep the item and
    // need owner or read-write (as do the route and dismiss APIs); Link actions only share
    // a link. Each entry is {id, label, enabled(item, role), run(item)} where
    // label is a string or fn(item), and enabled returns true, false (hide) or
    // a string (show disabled, the string is the reason). Other features add
    // one entry to inboxShareHiveActions (in menu order), e.g. the spek
    // session action, instead of a new row button. An entry may also carry
    // bulk(items) to appear in the multi-select Hive menu.
    const inboxShareHiveActions = [
      { id: 'rfc', dest: 'spek_rfc', label: 'RFC…', enabled: (it, role) => !inboxCanShare(role) ? false : inboxShareGate(it, 'spek_rfc') || (!(((_inboxCache || {}).repos || []).length) ? 'No RFC repository configured: set one in Settings → Inbox' : true), run: it => inboxOpenRoute(it.source, it.id, 'spek_rfc') },
      { id: 'knowledge', dest: 'dismiss', label: it => it.dismissed ? 'Back to inbox' : 'Knowledge', enabled: (it, role) => !inboxCanShare(role) ? false : (it.dismissed ? true : inboxShareGate(it, 'dismiss') || true), run: it => it.dismissed ? inboxUndismiss(it.source, it.id) : inboxDismiss(it.source, it.id), bulkLabel: 'Knowledge', bulk: () => inboxDismissSelected() },
      { id: 'dibs', dest: 'dibs', label: 'DIBS', enabled: (it, role) => !inboxCanShare(role) ? false : inboxDibsReady() ? true : 'Not configured: set the dibs URL in Settings → Inbox → dibs URL', run: it => inboxOpenRoute(it.source, it.id, 'dibs') },
      { id: 'tx-settings', label: '⚙ Transcription settings…', enabled: (it, role) => role === 'owner' && inboxTxBlocked(it) ? true : false, run: () => inboxOpenSettings() },
    ];
    const inboxShareLinkActions = [
      { id: 'copy', label: 'Copy link', enabled: () => true, run: it => inboxShareCopy(it.source, it.id) },
      { id: 'native', label: 'Share…', enabled: () => typeof navigator !== 'undefined' && !!navigator.share, run: it => inboxShareNative(it.source, it.id) },
    ];
    // Owners and read-write viewers may share (route, Knowledge); read may not.
    function inboxCanShare(role) { return role === 'owner' || role === 'read-write'; }
    // An audio-only item (pending, audio, transcription cannot run) cannot be
    // shared to RFC, spek or Knowledge: they need a transcript first.
    function inboxTxBlocked(it) { return !!(it && it.pending && it.transcribe_unavailable); }
    // inboxShareGate returns the disabled reason for a Hive destination that
    // needs a transcript, or '' when it may proceed (audio-only items are
    // transcribed first, then shared).
    function inboxShareGate(it, dest) {
      const intent = (it.share_intents || {})[dest];
      if (intent && intent.status === 'waiting') return 'Transcribing, then → ' + inboxIntentLabel(dest);
      if (inboxTxBlocked(it)) return inboxTxUnavailableText(it.transcribe_unavailable) + ' (a transcript is needed first)';
      return '';
    }
    const INBOX_INTENT_LABELS = { spek_rfc: 'RFC', dismiss: 'Knowledge', spek: 'spek' };
    function inboxIntentLabel(dest) { return INBOX_INTENT_LABELS[dest] || dest; }
    // The first waiting share intent of an item, for the progress label.
    function inboxIntentThen(it) {
      const m = (it && it.share_intents) || {};
      const d = Object.keys(m).find(k => m[k] && m[k].status === 'waiting');
      return d ? inboxIntentLabel(d) : '';
    }
    function inboxDibsReady() { const st = (_inboxCache || {}).settings || {}; return !!st.dibs_url && !!st.dibs_token_set; }
    // inboxShareMenuGroups resolves the registries for one item and the
    // viewer's role into labelled groups; empty groups are dropped.
    function inboxShareMenuGroups(it) {
      const role = window._hiveRole;
      const routes = (it && it.routes) || {};
      const resolve = (list) => list.map(a => {
        const en = a.enabled(it, role);
        if (en === false) return null;
        let label = typeof a.label === 'function' ? a.label(it) : a.label;
        const intents = (it && it.share_intents) || {};
        if (a.dest && ((routes[a.dest] && routes[a.dest].status === 'failed') || (intents[a.dest] && intents[a.dest].status === 'failed'))) label = 'Retry ' + label;
        return { id: a.id, label, disabled: typeof en === 'string', hint: typeof en === 'string' ? en : '' };
      }).filter(Boolean);
      return [{ id: 'hive', label: 'Hive', items: resolve(inboxShareHiveActions) }, { id: 'link', label: 'Link', items: resolve(inboxShareLinkActions) }].filter(g => g.items.length);
    }
    function inboxShareMenuHtml(groups, src, id) {
      return '<div class="ham-menu ham-menu-hinted inbox-share-menu" role="menu" aria-label="Share" tabindex="-1">' + groups.map((g, gi) =>
        (gi ? '<div class="ham-menu-sep" role="separator"></div>' : '') +
        '<div role="group" aria-label="' + escapeHtml(g.label) + '"><div class="ham-menu-head" aria-hidden="true">' + escapeHtml(g.label) + '</div>' + g.items.map(a =>
          '<button type="button" role="menuitem" class="ham-menu-item' + (a.hint ? ' ham-menu-item-hinted' : '') + '" tabindex="-1" data-inbox-share-group="' + escapeHtml(g.id) + '" data-inbox-share-act="' + escapeHtml(a.id) + '" data-src="' + escapeHtml(src) + '" data-id="' + escapeHtml(id) + '"' + (a.disabled ? ' aria-disabled="true"' : '') + '>' +
          '<span>' + escapeHtml(a.label) + '</span>' + (a.hint ? '<span class="ham-menu-hint">' + escapeHtml(a.hint) + '</span>' : '') + '</button>').join('') + '</div>').join('') + '</div>';
    }
    // inboxShareMenuRun calls the registered handler of one entry.
    async function inboxShareMenuRun(group, act, src, id) {
      const it = inboxFind(src, id);
      const entry = (group === 'hive' ? inboxShareHiveActions : inboxShareLinkActions).find(a => a.id === act);
      if (it && entry) await entry.run(it);
    }
    // Bulk Hive menu: entries with bulk(), owner or read-write.
    function inboxBulkMenuGroups() {
      if (!inboxCanShare(window._hiveRole)) return [];
      const items = inboxShareHiveActions.filter(a => typeof a.bulk === 'function').map(a => ({ id: a.id, label: a.bulkLabel || (typeof a.label === 'function' ? a.id : a.label), disabled: false, hint: '' }));
      return items.length ? [{ id: 'hive', label: 'Hive', items }] : [];
    }
    function inboxBulkMenuRun(act) {
      const entry = inboxShareHiveActions.find(a => a.id === act);
      if (entry && entry.bulk) return entry.bulk(Array.from(_inboxSelected));
    }
    let _inboxShareMenu = null;
    function inboxShareMenuClose(restore) {
      const m = _inboxShareMenu;
      if (!m) return;
      _inboxShareMenu = null;
      document.removeEventListener('pointerdown', m.onAway, true);
      document.removeEventListener('keydown', m.onKey, true);
      m.el.remove();
      m.btn.setAttribute('aria-expanded', 'false');
      if (restore && m.btn.focus) m.btn.focus();
    }
    function inboxShareMenuFocus(el, dir, from) {
      const items = Array.from(el.querySelectorAll('[role="menuitem"]')).filter(b => b.getAttribute('aria-disabled') !== 'true');
      if (!items.length) { el.focus(); return; }
      let i = items.indexOf(from);
      if (dir === 'first') i = 0; else if (dir === 'last') i = items.length - 1; else i = (i + dir + items.length) % items.length;
      items[i].focus();
    }
    function inboxPopMenu(btn, html, onPick) {
      if (_inboxShareMenu) { const same = _inboxShareMenu.btn === btn; inboxShareMenuClose(false); if (same) return; }
      const tpl = document.createElement('template');
      tpl.innerHTML = html;
      const el = tpl.content.firstElementChild;
      document.body.appendChild(el);
      const r = btn.getBoundingClientRect();
      el.style.setProperty('left', Math.max(0, Math.min(r.left, window.innerWidth - el.offsetWidth)) + 'px');
      el.style.setProperty('top', r.bottom + 'px');
      btn.setAttribute('aria-expanded', 'true');
      const m = { el, btn };
      m.onAway = e => { if (!el.contains(e.target) && !btn.contains(e.target)) inboxShareMenuClose(false); };
      m.onKey = e => {
        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); inboxShareMenuClose(true); return; }
        const b = e.target.closest ? e.target.closest('[role="menuitem"]') : null;
        switch (e.key) {
          case 'ArrowDown': e.preventDefault(); inboxShareMenuFocus(el, 1, b); break;
          case 'ArrowUp': e.preventDefault(); inboxShareMenuFocus(el, -1, b); break;
          case 'Home': e.preventDefault(); inboxShareMenuFocus(el, 'first'); break;
          case 'End': e.preventDefault(); inboxShareMenuFocus(el, 'last'); break;
          case 'Tab': inboxShareMenuClose(true); break;
        }
      };
      el.addEventListener('click', e => {
        const b = e.target.closest('[data-inbox-share-act]');
        if (!b || b.getAttribute('aria-disabled') === 'true') return;
        e.preventDefault();
        inboxShareMenuClose(true);
        onPick(b.dataset);
      });
      document.addEventListener('pointerdown', m.onAway, true);
      document.addEventListener('keydown', m.onKey, true);
      _inboxShareMenu = m;
      inboxShareMenuFocus(el, 'first');
    }
    // inboxShare opens the per-item Share menu under its button.
    function inboxShare(src, id, btn) {
      const it = inboxFind(src, id);
      if (!it) return;
      inboxPopMenu(btn, inboxShareMenuHtml(inboxShareMenuGroups(it), src, id), d => inboxShareMenuRun(d.inboxShareGroup, d.inboxShareAct, d.src, d.id));
    }
    // inboxBulkMenu opens the Hive menu of the multi-select bar.
    function inboxBulkMenu(btn) {
      inboxPopMenu(btn, inboxShareMenuHtml(inboxBulkMenuGroups(), '', ''), d => inboxBulkMenuRun(d.inboxShareAct));
    }
    let _inboxFocusKey = inboxFocusKeyFromHash(location.hash);
    window.addEventListener('hashchange', (e) => {
      const key = inboxFocusKeyFromHash(e.newURL || location.hash);
      if (key) { _inboxFocusKey = key; renderInbox(); }
    });
    // inboxApplyFocus scrolls a deep-linked item into view once it renders.
    function inboxApplyFocus(panel) {
      if (!_inboxFocusKey || !panel) return;
      const el = panel.querySelector('[data-inbox-key="' + CSS.escape(_inboxFocusKey) + '"]');
      if (!el) return;
      _inboxFocusKey = '';
      el.classList.add('inbox-item-focus');
      el.scrollIntoView({ block: 'center' });
    }

    // A poll must not stop a recording that is playing: live <audio> elements
    // are moved into the freshly rendered item instead of being rebuilt.
    function inboxPreserveAudio(panel, render) {
      const live = {};
      panel.querySelectorAll('[data-inbox-media]').forEach(m => {
        if (m.querySelector('audio')) live[m.dataset.inboxMedia] = Array.from(m.childNodes);
      });
      render();
      for (const key of Object.keys(live)) {
        const target = panel.querySelector('[data-inbox-media="' + CSS.escape(key) + '"]');
        if (target) { target.innerHTML = ''; live[key].forEach(n => target.appendChild(n)); }
      }
    }

    function renderInbox() {
      refreshSectionCardShell('inbox-section');
      const panel = document.getElementById('inbox-panel');
      const badge = document.getElementById('inbox-sidebar-badge');
      const data = _inboxCache;
      if (!panel) return;
      if (!data) { panel.innerHTML = '<div class="muted-note">Loading inbox…</div>'; return; }
      const items = data.items || [];
      const unprocessed = Number(data.unprocessed) || 0;
      if (badge) setSidebarCountBadge(badge, unprocessed, unprocessed + ' unprocessed');
      setSectionSummary('inbox-section', unprocessed + ' to triage · ' + items.length + ' total');
      const owner = viewerIsOwner();
      const sources = (data.sources || []).map(s => {
        const status = s.connected ? 'ok' : (s.configured ? 'error' : '');
        const label = inboxSourceIcon(s.type) + ' ' + escapeHtml(s.type) + (s.connected ? (s.account ? ': ' + escapeHtml(s.account) : ': connected') : s.configured ? ': not connected' : ': not configured');
        return '<span class="kc-pill" data-status="' + status + '">' + label + '</span>';
      }).join(' ');
      const settings = data.settings || {};
      const rules = Array.isArray(settings.rules) ? settings.rules.length : 0;
      let html = '<div class="row-between inbox-toolbar">' +
        '<div class="inbox-sources">' + (sources || '<span class="muted-xs">No voice sources configured.</span>') +
        ' <button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbOpenIntegrations">Connect sources</button></div>' +
        '<div class="kc-actions">' +
        '<label class="muted-xs inbox-toggle"><input type="checkbox" data-change-action="inboxToggleProcessed"' + (_inboxShowProcessed ? ' checked' : '') + '> show processed</label>' +
        (owner ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxOpenSettings" title="Auto-routing rules, default RFC repository, dibs URL, transcription">⚙ Auto-routing (' + rules + ')</button>' : '') +
        '</div></div>';
      if (data.error) html += '<div class="kc-error muted-note">' + escapeHtml(data.error) + '</div>';
      if (!settings.spek_ready && owner) html += '<p class="muted-xs">RFC filing needs at least one enabled, unpaused agent to draft the spec with spektacular (<code>inbox.spek.agent</code> overrides the default; <code>inbox.spek.binary</code> names the CLI the agent runs).</p>';
      if (_inboxFocusKey && items.some(it => it.processed && inboxItemKey(it) === _inboxFocusKey)) _inboxShowProcessed = true;
      const shown = items.filter(it => _inboxShowProcessed || !it.processed);
      if (inboxCanShare(window._hiveRole) && shown.length) {
        const sel = _inboxSelected.size;
        html += '<div class="kc-actions mb-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxSelectAll">' + (sel === shown.length ? 'Clear selection' : 'Select all') + '</button>' +
          (sel ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxBulkMenu" data-arg-types="t" aria-haspopup="menu" aria-expanded="false">Hive (' + sel + ') ▾</button>' : '') + '</div>';
      }
      if (!shown.length) html += '<div class="muted-note">' + (items.length ? 'Everything is triaged. Toggle “show processed” to see filed and dismissed items.' : 'Nothing yet. Connect Plaud or another voice source under Knowledge → Integrations; new recordings land here after the next sync (a newly added connector starts on the next hive restart).') + '</div>';
      else html += '<div class="kb-fact-list inbox-list">' + shown.map(inboxItemHtml).join('') + '</div>';
      inboxPreserveAudio(panel, () => { panel.innerHTML = html; });
      inboxApplyFocus(panel);
    }

    function inboxToggleProcessed() { _inboxShowProcessed = !_inboxShowProcessed; renderInbox(); }
    function inboxToggleSelect(src, id) {
      const key = src + '/' + id;
      if (_inboxSelected.has(key)) _inboxSelected.delete(key); else _inboxSelected.add(key);
      renderInbox();
    }
    function inboxSelectAll() {
      const shown = ((_inboxCache || {}).items || []).filter(it => _inboxShowProcessed || !it.processed);
      if (_inboxSelected.size === shown.length) _inboxSelected.clear();
      else _inboxSelected = new Set(shown.map(inboxItemKey));
      renderInbox();
    }
    function inboxFind(src, id) { return ((_inboxCache || {}).items || []).find(it => it.source === src && it.id === id); }
    function inboxOpenFact(slug, layer) {
      ocNavigate('knowledge-section');
      kbReadFact(slug, layer || undefined);
    }

    async function inboxPostDismiss(items, undo) {
      const r = await fetch('/api/inbox/dismiss', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ items, undo: !!undo }) });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      return data;
    }
    async function inboxDismiss(src, id) {
      try { const d = await inboxPostDismiss([{ source: src, item_id: id }], false); showToast(d && d.waiting_transcript ? 'Transcribing, then → Knowledge' : 'Kept as a Knowledge fact only', 'success'); await fetchInbox(); }
      catch (e) { showToast('Dismiss failed: ' + e.message, 'error'); }
    }
    async function inboxUndismiss(src, id) {
      try { await inboxPostDismiss([{ source: src, item_id: id }], true); await fetchInbox(); }
      catch (e) { showToast('Undo failed: ' + e.message, 'error'); }
    }
    async function inboxDismissSelected() {
      const items = Array.from(_inboxSelected).map(k => { const i = k.indexOf('/'); return { source: k.slice(0, i), item_id: k.slice(i + 1) }; });
      if (!items.length) return;
      if (!(await hiveConfirm('Mark ' + items.length + ' item' + (items.length === 1 ? '' : 's') + ' as Knowledge only? They stay searchable as facts; no RFC or dibs idea is created.', 'Knowledge only'))) return;
      try { const d = await inboxPostDismiss(items, false); _inboxSelected.clear(); if (d && d.waiting_transcript) showToast(d.waiting_transcript + ' item' + (d.waiting_transcript === 1 ? '' : 's') + ' will be kept once transcribed', 'success'); await fetchInbox(); }
      catch (e) { showToast('Dismiss failed: ' + e.message, 'error'); }
    }

    // Route dialog: destination-specific confirm with a repo picker for RFCs.
    function inboxOpenRoute(src, id, dest) {
      const it = inboxFind(src, id);
      if (!it) return;
      const settings = (_inboxCache || {}).settings || {};
      const repos = (_inboxCache || {}).repos || [];
      const def = settings.spek_repo || repos[0] || '';
      let body = '<p class="muted-note m-0"><strong>' + escapeHtml(it.title || it.id) + '</strong></p>';
      if (dest === 'spek_rfc') {
        const opts = repos.map(r => '<option value="' + escapeHtml(r) + '"' + (r === def ? ' selected' : '') + '>' + escapeHtml(r) + '</option>').join('');
        body += '<p class="muted-note">A hive agent drafts a spec from the transcript with spektacular, then runs <code>spektacular publish</code> to file one RFC issue plus child issues on the chosen repository. The children are picked up by the hive like any other issue.</p>' +
          '<label class="muted-note inbox-field">Repository<select class="field-input field-input-block mt-2" id="inbox-route-repo">' + opts + '</select></label>' +
          (repos.length ? '' : '<p class="kc-error muted-xs">No repositories configured on this hive.</p>');
      } else {
        body += '<p class="muted-note">Posts the title, summary and transcript to dibs as a private draft idea' + (settings.dibs_url ? ' at <code>' + escapeHtml(settings.dibs_url) + '</code>' : '') + '. Provenance links back to <code>' + escapeHtml(src) + '://recordings/' + escapeHtml(id) + '</code>.</p>';
      }
      body += '<div class="hive-dialog-error" role="alert" id="inbox-route-error"></div>' +
        '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-action="kbCloseModal">Cancel</button>' +
        '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxConfirmRoute" data-arg0="' + escapeHtml(src) + '" data-arg1="' + escapeHtml(id) + '" data-arg2="' + escapeHtml(dest) + '">' + (dest === 'spek_rfc' ? 'File RFC' : 'Send to dibs') + '</button></div>';
      kbShowModal(dest === 'spek_rfc' ? '→ RFC (spektacular)' : '→ DIBS', body, { width: '520px' });
    }
    async function inboxConfirmRoute(src, id, dest) {
      const repoEl = document.getElementById('inbox-route-repo');
      const repo = repoEl ? repoEl.value : '';
      const errEl = document.getElementById('inbox-route-error');
      try {
        const r = await fetch('/api/inbox/route', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ source: src, item_id: id, destination: dest, repo }) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        kbCloseModal();
        showToast(data.waiting_transcript ? 'Transcribing, then → ' + (INBOX_DEST_LABELS[dest] || dest) + '. It is sent when the transcript is ready.' : (INBOX_DEST_LABELS[dest] || dest) + ' queued — the pill updates when it finishes.', 'success');
        await fetchInbox();
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Route failed: ' + e.message, 'error');
      }
    }

    // Settings drawer: default RFC repo, dibs URL, and ordered auto-routing rules.
    let _inboxDraftRules = [];
    function inboxRuleRowHtml(rule, i) {
      const sources = ((_inboxCache || {}).settings || {}).sources || [];
      const srcOpts = ['<option value=""' + (!rule.source ? ' selected' : '') + '>any source</option>'].concat(sources.map(s => '<option value="' + escapeHtml(s) + '"' + (rule.source === s ? ' selected' : '') + '>' + escapeHtml(s) + '</option>')).join('');
      const destOpts = [['spek_rfc', '→ RFC'], ['dibs', '→ dibs'], ['dismiss', 'Knowledge only']].map(([v, l]) => '<option value="' + v + '"' + (rule.destination === v ? ' selected' : '') + '>' + l + '</option>').join('');
      return '<div class="inbox-rule" data-rule-index="' + i + '">' +
        '<select class="field-input" data-rule-field="source" aria-label="Source">' + srcOpts + '</select>' +
        '<input class="kb-search-box" data-rule-field="title_prefix" placeholder="title prefix (RFC:)" value="' + escapeHtml(rule.title_prefix || '') + '" aria-label="Title prefix">' +
        '<input class="kb-search-box" data-rule-field="tag" placeholder="or tag" value="' + escapeHtml(rule.tag || '') + '" aria-label="Tag">' +
        '<select class="field-input" data-rule-field="destination" aria-label="Destination">' + destOpts + '</select>' +
        '<input class="kb-search-box" data-rule-field="repo" placeholder="owner/repo (RFC only)" value="' + escapeHtml(rule.repo || '') + '" aria-label="Repository">' +
        '<button type="button" class="hv-btn btn-icon btn-sm" data-action="inboxRemoveRule" data-arg0="' + i + '" aria-label="Remove rule">✕</button></div>';
    }
    // The same form renders in the Inbox page modal and inline under
    // Settings → Inbox, and both can sit in the DOM at once (the settings
    // tab stays mounted; the closed modal keeps its last body). Every read
    // and write therefore goes through the active form root: the modal
    // while it is open, otherwise the inline section.
    function inboxSettingsRoot() {
      const overlay = document.getElementById('kb-modal-overlay');
      if (overlay && overlay.style.display !== 'none') {
        const modal = overlay.querySelector('.inbox-settings-form');
        if (modal) return modal;
      }
      return document.querySelector('#inbox-settings-inline .inbox-settings-form');
    }
    function inboxSettingsField(root, name) {
      return root ? root.querySelector('[data-inbox-field="' + name + '"]') : null;
    }
    function inboxReadRules(root) {
      root = root || inboxSettingsRoot();
      if (!root) return [];
      return Array.from(root.querySelectorAll('.inbox-settings-rules .inbox-rule')).map(row => {
        const v = f => { const el = row.querySelector('[data-rule-field="' + f + '"]'); return el ? el.value.trim() : ''; };
        return { source: v('source'), title_prefix: v('title_prefix'), tag: v('tag'), destination: v('destination'), repo: v('repo') };
      });
    }
    function inboxRenderRules(root) {
      root = root || inboxSettingsRoot();
      const host = root ? root.querySelector('.inbox-settings-rules') : null;
      if (!host) return;
      host.innerHTML = _inboxDraftRules.length ? _inboxDraftRules.map(inboxRuleRowHtml).join('') : '<p class="muted-xs">No rules — every item waits here for manual triage.</p>';
    }
    function inboxSettingsFormHtml(settings, inline) {
      const buttons = inline
        ? '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxSaveSettings">Save</button></div>'
        : '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-action="kbCloseModal">Cancel</button>' +
          '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxSaveSettings">Save</button></div>';
      return '<div class="inbox-settings-form" data-inbox-form="' + (inline ? 'inline' : 'modal') + '"><p class="muted-note m-0">New items are checked against these rules in order after every connector sync; the first match routes the item automatically. Items that arrived before a rule existed are never re-routed.</p>' +
        '<div class="inbox-settings-grid">' +
        '<label class="muted-note inbox-field">Default RFC repository<input class="kb-search-box mt-2" type="text" data-inbox-field="spek-repo" placeholder="owner/repo" autocomplete="off" value="' + escapeHtml(settings.spek_repo || '') + '"></label>' +
        '<label class="muted-note inbox-field">dibs URL<input class="kb-search-box mt-2" type="text" data-inbox-field="dibs-url" placeholder="https://dibs.example.org" autocomplete="off" value="' + escapeHtml(settings.dibs_url || '') + '"></label></div>' +
        '<p class="muted-xs">The dibs token is read from the environment variable named by <code>inbox.dibs.token_env</code>' + (settings.dibs_token_set ? ' (set)' : ' (not set)') + '.</p>' +
        '<div class="row-between mt-4 mb-2"><div class="section-label m-0">Rules</div><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxAddRule">Add rule</button></div>' +
        '<div class="inbox-settings-rules"></div>' +
        inboxTranscriptionFormHtml(settings.transcription) +
        '<div class="hive-dialog-error" role="alert" data-inbox-field="error"></div>' + buttons + '</div>';
    }

    // Provider choices for ⚙ → Transcription: one line on speed, speakers
    // and where the audio goes.
    function inboxTxProviderInfo() {
      return {
        auto: 'Picks whisper, then Gemini, then Deepgram: the first one that is ready on this hive.',
        whisper: 'Built in, no account: a 5-minute recording takes a few minutes on the hive\'s CPU; no speaker labels; the audio never leaves this hive.',
        gemini: 'Uses this hive\'s own Gemini credentials: fast, labels speakers, approximate timestamps; the audio is sent to Google.',
        deepgram: 'Uses the owner\'s Deepgram key: fastest, labels speakers; the audio is sent to Deepgram (training opt-out set).'
      };
    }
    // Where the audio goes for the selected provider, for the consent notice.
    // For auto it names what auto resolves to right now.
    function inboxTxConsentText(provider, resolved) {
      const p = provider === 'auto' ? (resolved || '') : provider;
      const dest = {
        whisper: 'With whisper the audio stays on this hive: a sandboxed process here decodes and transcribes it and nothing is sent anywhere.',
        gemini: 'With Gemini this hive sends the audio to Google (the Gemini API) under the hive\'s own Gemini credentials.',
        deepgram: 'With Deepgram this hive sends the audio to Deepgram under the hive owner\'s Deepgram account and key, with Deepgram\'s model-training opt-out set.'
      }[p] || 'Depending on the provider the audio stays on this hive (whisper) or is sent to Google (Gemini) or Deepgram under the hive\'s own account.';
      return (provider === 'auto' && resolved ? 'Auto currently picks ' + resolved + '. ' : '') + dest;
    }
    // Provider selector with each provider's readiness (never a key value).
    function inboxTxProviderHtml(tx) {
      const info = inboxTxProviderInfo();
      const current = info[tx.provider] ? tx.provider : 'auto';
      const status = {};
      for (const p of (Array.isArray(tx.providers) ? tx.providers : [])) status[p.name] = p;
      let opts = '';
      for (const name of ['auto', 'whisper', 'gemini', 'deepgram']) {
        const st = status[name];
        const suffix = name === 'auto' ? (tx.resolved_provider ? ' (now: ' + tx.resolved_provider + ')' : '') : (st && !st.available ? ' (not ready)' : '');
        opts += '<option value="' + name + '"' + (current === name ? ' selected' : '') + '>' + name + suffix + '</option>';
      }
      let rows = '';
      for (const name of ['auto', 'whisper', 'gemini', 'deepgram']) {
        const st = status[name];
        const pill = name === 'auto' ? '' : (st ? '<span class="kc-pill" data-status="' + (st.available ? 'ok' : 'error') + '">' + (st.available ? 'ready' : 'not ready') + '</span> ' + escapeHtml(st.detail || '') : '');
        rows += '<li data-inbox-tx-provider="' + name + '"><strong>' + name + '</strong>: ' + escapeHtml(info[name]) + (pill ? ' ' + pill : '') + '</li>';
      }
      return '<label class="muted-note inbox-field">Provider<select class="field-input field-input-block mt-2" data-inbox-tx-field="provider">' + opts + '</select></label>' +
        '<ul class="muted-xs inbox-tx-providers">' + rows + '</ul>';
    }

    // Transcription section: spoke-local transcription with whisper (on the
    // hive), the hive's Gemini credentials, or the owner's Deepgram key. Key
    // values are never shown, only whether they are present on this hive.
    function inboxTranscriptionFormHtml(tx) {
      tx = tx || {};
      const field = (name, label, value, placeholder, type) => '<label class="muted-note inbox-field">' + label +
        '<input class="kb-search-box mt-2" type="' + (type || 'text') + '" data-inbox-tx-field="' + name + '" autocomplete="off" placeholder="' + escapeHtml(placeholder || '') + '" value="' + escapeHtml(value == null ? '' : String(value)) + '"></label>';
      const mode = tx.mode === 'auto' ? 'auto' : 'manual';
      const env = tx.api_key_env || 'DEEPGRAM_API_KEY';
      const sizeMB = tx.max_bytes ? Math.round(Number(tx.max_bytes) / INBOX_TX_BYTES_PER_MB) : '';
      return '<div class="inbox-tx-settings" data-inbox-tx-section="1">' +
        '<div class="section-label mt-4 mb-2">Transcription</div>' +
        '<p class="muted-xs">Recordings that arrive without text can be transcribed by this hive. Each hive transcribes on its own: with whisper on its own CPU, or with its own Gemini or Deepgram account. Nothing is shared with other hives or paid for centrally, and the transcript is stored only on this hive. ' + yamlHint('inbox.transcription.enabled') + '</p>' +
        inboxTxProviderHtml(tx) +
        '<div class="inbox-tx-notice" role="note"><strong>Before you enable this:</strong> recordings contain other people\'s voices. ' + escapeHtml(inboxTxConsentText(tx.provider || 'auto', tx.resolved_provider)) + ' Make sure the people you record are comfortable with that.</div>' +
        '<label class="muted-note inbox-toggle mt-2"><input type="checkbox" data-inbox-tx-field="consent_acknowledged"' + (tx.consent_acknowledged ? ' checked' : '') + '> I understand where the audio goes with the provider above ' + yamlHint('inbox.transcription.consent_acknowledged') + '</label>' +
        '<label class="muted-note inbox-toggle mt-2"><input type="checkbox" data-inbox-tx-field="enabled"' + (tx.enabled ? ' checked' : '') + '> Enable transcription</label>' +
        '<div class="inbox-settings-grid">' +
        '<label class="muted-note inbox-field">Mode<select class="field-input field-input-block mt-2" data-inbox-tx-field="mode">' +
        '<option value="manual"' + (mode === 'manual' ? ' selected' : '') + '>manual: only when someone clicks Transcribe</option>' +
        '<option value="auto"' + (mode === 'auto' ? ' selected' : '') + '>auto: also new recordings from the sources below</option></select></label>' +
        field('sources', 'Auto-mode sources', (tx.sources || []).join(', '), 'plaud') +
        field('api_key_env', 'Deepgram key environment variable', env, 'DEEPGRAM_API_KEY') +
        field('model', 'Deepgram model', tx.model || 'nova-3', 'nova-3') +
        field('language', 'Language', tx.language || 'auto', 'auto or en, de, pt-BR') +
        '<label class="muted-note inbox-field">Whisper model<select class="field-input field-input-block mt-2" data-inbox-tx-field="whisper_model">' +
        ['', 'tiny.en', 'tiny', 'base.en', 'base', 'small.en', 'small'].map(m => '<option value="' + m + '"' + ((tx.whisper_model || '') === m ? ' selected' : '') + '>' + (m ? m + ({ 'tiny.en': ' (78 MB, fastest)', tiny: ' (78 MB, any language)', 'base.en': ' (148 MB)', base: ' (148 MB, any language)', 'small.en': ' (488 MB, about 3x slower)', small: ' (488 MB, any language, about 3x slower)' })[m] : 'default: base.en (base for other languages)') + '</option>').join('') + '</select></label>' +
        field('whisper_threads', 'Whisper threads', tx.whisper_threads || '', 'half the CPUs', 'number') +
        field('gemini_model', 'Gemini model', tx.gemini_model || '', 'gemini-3.5-flash') +
        field('monthly_budget_minutes', 'Monthly budget (minutes)', tx.monthly_budget_minutes || '', '600', 'number') +
        field('daily_max_items', 'Daily limit (recordings)', tx.daily_max_items || '', '20', 'number') +
        field('max_duration_minutes', 'Longest recording (minutes)', tx.max_duration_minutes || '', '120', 'number') +
        field('max_mb', 'Largest upload (MB)', sizeMB, '500', 'number') +
        '</div>' +
        '<label class="muted-note inbox-toggle mt-2"><input type="checkbox" data-inbox-tx-field="diarize"' + (tx.diarize !== false ? ' checked' : '') + '> Label speakers (Speaker 1, Speaker 2, …; Deepgram and Gemini, whisper has no speaker labels)</label>' +
        inboxTranscriptionUsageHtml(tx) +
        '<p class="muted-xs">Deepgram key <code>$' + escapeHtml(env) + '</code>: ' + (tx.key_present ? '<span class="kc-pill" data-status="ok">key present</span>' : '<span class="kc-pill" data-status="error">key missing</span> set it in this hive\'s environment (it is never stored in hive.yaml)') + '. ' +
        yamlHint('inbox.transcription.mode') + ' ' + yamlHint('inbox.transcription.sources[]') + ' ' + yamlHint('inbox.transcription.api_key_env') + ' ' + yamlHint('inbox.transcription.provider') + ' ' + yamlHint('inbox.transcription.model') + ' ' + yamlHint('inbox.transcription.diarize') + ' ' + yamlHint('inbox.transcription.language') + ' ' +
        yamlHint('inbox.transcription.whisper_model') + ' ' + yamlHint('inbox.transcription.whisper_threads') + ' ' + yamlHint('inbox.transcription.gemini_model') + ' ' +
        yamlHint('inbox.transcription.max_duration_minutes') + ' ' + yamlHint('inbox.transcription.max_bytes') + ' ' + yamlHint('inbox.transcription.monthly_budget_minutes') + ' ' + yamlHint('inbox.transcription.daily_max_items') + '</p></div>';
    }
    // This month's minutes (money with Gemini or Deepgram, CPU time with
    // whisper), today's runs, the egress the hive needs, and a hint when auto
    // mode has no sources.
    function inboxTranscriptionUsageHtml(tx) {
      tx = tx || {};
      const used = Math.round((Number(tx.month_minutes_used) || 0) * 10) / 10;
      const budget = Number(tx.monthly_budget_minutes) || 0;
      const pct = budget > 0 ? Math.min(100, Math.round(used / budget * 100)) : 0;
      const state = pct >= 100 ? 'error' : (pct >= 80 ? 'syncing' : 'ok');
      let html = '<p class="muted-xs inbox-tx-usage" data-inbox-tx-usage="1">This month: <span class="kc-pill" data-status="' + state + '">' + used + ' of ' + budget + ' minutes used</span>' +
        ' · today: ' + (Number(tx.today_items) || 0) + ' of ' + (Number(tx.daily_max_items) || 0) + ' recordings (UTC). With whisper the budget limits CPU minutes on this hive, not money.</p>';
      const hosts = Array.isArray(tx.egress_hosts) ? tx.egress_hosts : [];
      if (hosts.length) html += '<p class="muted-xs">Egress: this hive must reach ' + hosts.map(h => '<code>' + escapeHtml(h) + ':443</code>').join(', ') + ' while transcription is enabled.</p>';
      if (tx.mode === 'auto' && !(tx.sources || []).length) html += '<p class="kc-error muted-xs">Auto mode transcribes only the sources listed above; none are listed, so nothing is transcribed automatically.</p>';
      return html;
    }
    // inboxReadTranscription reads the section back into inbox.transcription.
    // It returns { error } when enabling without the consent box.
    function inboxReadTranscription(root, current) {
      if (!root || !root.querySelector('[data-inbox-tx-section]')) return null;
      const el = name => root.querySelector('[data-inbox-tx-field="' + name + '"]');
      const val = name => { const e = el(name); return e ? String(e.value || '').trim() : ''; };
      const on = name => { const e = el(name); return !!(e && e.checked); };
      const num = name => { const n = Math.floor(Number(val(name))); return n > 0 ? n : 0; };
      const tx = {
        enabled: on('enabled'),
        consent_acknowledged: on('consent_acknowledged'),
        mode: val('mode') || 'manual',
        provider: val('provider') || (current && current.provider) || 'auto',
        model: val('model'),
        api_key_env: val('api_key_env'),
        sources: val('sources').split(',').map(x => x.trim()).filter(Boolean),
        diarize: on('diarize'),
        language: val('language'),
        monthly_budget_minutes: num('monthly_budget_minutes'),
        daily_max_items: num('daily_max_items'),
        max_duration_minutes: num('max_duration_minutes'),
        max_bytes: num('max_mb') * INBOX_TX_BYTES_PER_MB,
      };
      // Whisper fields are sent only when the form has them, so an older
      // payload shape stays exactly as it was.
      if (el('whisper_model')) tx.whisper_model = val('whisper_model');
      if (el('whisper_threads')) tx.whisper_threads = num('whisper_threads');
      if (el('gemini_model')) tx.gemini_model = val('gemini_model');
      if (tx.enabled && !tx.consent_acknowledged) return { error: 'Tick the consent box to enable transcription: recordings contain other people\'s voices, and with Gemini or Deepgram the audio is sent to that provider under the hive\'s own account.' };
      return { transcription: tx };
    }
    function inboxOpenSettings() {
      const settings = (_inboxCache || {}).settings || {};
      _inboxDraftRules = (settings.rules || []).map(r => Object.assign({}, r));
      kbShowModal('⚙ Inbox auto-routing', inboxSettingsFormHtml(settings, false), { width: '720px' });
      inboxRenderRules();
    }
    // Settings → Inbox: the same form inline, loaded from /api/inbox/settings.
    function renderGovInbox() {
      return '<div class="config-section" data-config-section="Inbox"><h3>Inbox</h3>' +
        '<p class="text-muted fs-base">Default RFC repository, dibs URL, auto-routing rules and machine transcription (whisper, Gemini or Deepgram) for voice recordings. ' + yamlHint('inbox.spek.repo') + ' ' + yamlHint('inbox.dibs.url') + ' ' + yamlHint('inbox.rules') + ' ' + yamlHint('inbox.transcription') + '</p>' +
        '<div id="inbox-settings-inline"><p class="muted-xs">Loading…</p></div></div>';
    }
    async function loadInboxSettingsInline() {
      const host = document.getElementById('inbox-settings-inline');
      if (!host) return;
      try {
        const r = await fetch('/api/inbox/settings');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _inboxCache = Object.assign({}, _inboxCache, { settings: data });
        _inboxDraftRules = (data.rules || []).map(x => Object.assign({}, x));
        host.innerHTML = inboxSettingsFormHtml(data, true);
        inboxRenderRules(host.querySelector('.inbox-settings-form'));
      } catch (e) {
        host.innerHTML = '<p class="kc-error muted-xs">Inbox settings unavailable: ' + escapeHtml(e.message) + '</p>';
      }
    }
    function inboxAddRule() {
      _inboxDraftRules = inboxReadRules();
      _inboxDraftRules.push({ source: '', title_prefix: 'RFC:', tag: '', destination: 'spek_rfc', repo: '' });
      inboxRenderRules();
    }
    function inboxRemoveRule(i) {
      _inboxDraftRules = inboxReadRules();
      _inboxDraftRules.splice(Number(i), 1);
      inboxRenderRules();
    }
    async function inboxSaveSettings() {
      const root = inboxSettingsRoot();
      if (!root) return;
      const errEl = inboxSettingsField(root, 'error');
      const rules = inboxReadRules(root).map(r => { const o = {}; for (const k of Object.keys(r)) if (r[k]) o[k] = r[k]; return o; });
      const payload = { rules, spek_repo: (inboxSettingsField(root, 'spek-repo') || {}).value || '', dibs_url: (inboxSettingsField(root, 'dibs-url') || {}).value || '' };
      const tx = inboxReadTranscription(root, (((_inboxCache || {}).settings) || {}).transcription);
      if (tx && tx.error) { if (errEl) errEl.textContent = tx.error; else showToast(tx.error, 'error'); return; }
      if (tx) payload.transcription = tx.transcription;
      try {
        const r = await fetch('/api/inbox/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        if (root.dataset.inboxForm !== 'inline') kbCloseModal();
        showToast('Inbox settings saved', 'success');
        await fetchInbox();
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Save failed: ' + e.message, 'error');
      }
    }

    // Hosted-hive Plaud sign-in completion: Plaud's OAuth client only redirects
    // to localhost:8199, so the operator pastes either the redirect URL the
    // browser landed on or the tokens-mcp.json contents; the hive finishes
    // the login in-pod.
    // The same endpoint finishes a Zoom user-level sign-in from its pasted
    // redirect URL (kbZoomPasteHtml); the label follows the connector type.
    async function kbPlaudPasteComplete(name) {
      const label = kbAuthLabel(name);
      const el = document.getElementById('kb-plaud-paste-' + name);
      const raw = el ? el.value.trim() : '';
      if (!raw) { showToast('Paste the redirect URL' + (label === 'Plaud' ? ' or the tokens JSON' : '') + ' first.', 'error'); return; }
      const body = raw.startsWith('{') ? { tokens: raw } : { redirect_url: raw };
      try {
        const r = await fetch('/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth/complete', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _kbPlaudAuth[name] = data;
        kbRenderIntegrationsModal();
        showToast(data.connected ? label + ' connected' + (data.account ? ' as ' + data.account : '') : label + ' sign-in submitted; click Check.', data.connected ? 'success' : 'info');
        fetchInbox();
      } catch (e) {
        showToast(label + ' sign-in failed: ' + e.message, 'error');
      }
    }
    function kbPlaudPasteHtml(name) {
      const n = escapeHtml(name);
      const a = _kbPlaudAuth[name] || {};
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs">Recordings are triaged in the Inbox, not here.</span></div>';
      return '<details class="kb-plaud-paste mt-2"><summary class="muted-xs">Hive runs on a server? Finish sign-in by pasting</summary>' +
        '<p class="muted-xs">Plaud only redirects to <code>localhost:8199</code>. Open the authorization link, approve, then paste the <code>localhost:8199/auth/callback?…</code> URL your browser lands on (within 10 minutes). Alternatively paste the contents of <code>~/.plaud/tokens-mcp.json</code> from a laptop where <code>plaud-mcp</code> is signed in: on macOS run <code>cat ~/.plaud/tokens-mcp.json | pbcopy</code> (Linux: <code>xclip -selection clipboard &lt; ~/.plaud/tokens-mcp.json</code>) to copy it without showing it, then paste below.</p>' +
        '<textarea class="kb-search-box" id="kb-plaud-paste-' + n + '" rows="3" placeholder="http://localhost:8199/auth/callback?code=…  or  {&quot;access_token&quot;: …}"></textarea>' +
        '<div class="kc-actions mt-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbPlaudPasteComplete" data-arg0="' + n + '">Submit</button>' + openInbox + '</div></details>';
    }
