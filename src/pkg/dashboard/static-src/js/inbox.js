    // ── Inbox: source-agnostic triage of voice recordings and dictations ──
    // Items come from knowledge connectors (plaud, wisprflow, …); each one is
    // already a Knowledge fact. Routing them to spek (RFC + child issues) or
    // dibs happens inside the hive via POST /api/inbox/route.
    const INBOX_POLL_MS = 60000;
    const INBOX_BUSY_POLL_MS = 5000;
    const INBOX_SOURCE_ICONS = { plaud: '🎙️', wisprflow: '🗣️', zoom: '📹', teams: '👥', discord: '🎧', 'google-meet': '🎥', webex: '🎬', 'slack-huddles': '💬', 'fireflies': '🪲', 'fathom': '🌊', 'limitless': '📿', 'granola': '🥣', 'otter': '🦦', 'file-drop': '📎', webhook: '🔗', 'hive-recording': '⏺️', upload: '📎', email: '✉️' };
    const INBOX_DEST_LABELS = { spek_rfc: 'RFC', dibs: 'dibs' };
    // Summaries longer than this many characters, or more lines than the
    // clamp (the CSS line-clamp of .inbox-summary.is-collapsed), start collapsed.
    const INBOX_SUMMARY_COLLAPSE_CHARS = 280;
    const INBOX_SUMMARY_CLAMP_LINES = 3;
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
    // Drag-and-drop ingest state (see the end of this file): upload rows,
    // the ＋ Add panel, the send queue and whether the last click was inside
    // the Inbox (for paste).
    let _inboxUploads = [];
    let _inboxUploadSeq = 0;
    let _inboxAddOpen = false;
    let _inboxUploadQueue = Promise.resolve();
    let _inboxPointerInside = false;
    // Whether the "+N more" disclosure of unconfigured sources is open.
    let _inboxSourcesOpen = false;

    function inboxSourceIcon(src) { return INBOX_SOURCE_ICONS[src] || '📥'; }
    // Inline SVG glyphs for the per-item icon buttons (play, share, text
    // disclosure, open file, delete). Each is decorative: the button carries
    // the accessible name (aria-label) and the tooltip (title).
    function inboxIconSvg(name) {
      const paths = {
        play: '<path d="M4.5 3.2v9.6a.6.6 0 0 0 .9.5l7.6-4.8a.6.6 0 0 0 0-1L5.4 2.7a.6.6 0 0 0-.9.5z" fill="currentColor" stroke="none"/>',
        share: '<path d="M6.8 9.2a3 3 0 0 0 4.2 0l2-2a3 3 0 0 0-4.2-4.2l-1 1"/><path d="M9.2 6.8a3 3 0 0 0-4.2 0l-2 2a3 3 0 0 0 4.2 4.2l1-1"/>',
        'chevron-right': '<path d="M6 3.5 10.5 8 6 12.5"/>',
        'chevron-down': '<path d="M3.5 6 8 10.5 12.5 6"/>',
        file: '<path d="M10.5 5.5 6.2 9.8a1.5 1.5 0 0 0 2.1 2.1l4.6-4.6a3 3 0 0 0-4.2-4.2L4 7.8a4.2 4.2 0 0 0 6 6l3-3"/>',
        trash: '<path d="M3 4.5h10"/><path d="M6.5 4.5V3h3v1.5"/><path d="M4.5 4.5l.6 8.2a1 1 0 0 0 1 .8h3.8a1 1 0 0 0 1-.8l.6-8.2"/>'
      };
      return '<svg class="inbox-ic" viewBox="0 0 16 16" aria-hidden="true" focusable="false">' + (paths[name] || '') + '</svg>';
    }
    // inboxTitleParts splits a trailing recording id off a title:
    // "Meeting notes: standup 2026-11-17 (mtg-078395cc1f76)" becomes
    // { title: "Meeting notes: standup 2026-11-17", id: "mtg-078395cc1f76" }.
    // The id goes to the meta line; the full title stays as the tooltip.
    function inboxTitleParts(title) {
      const full = String(title || '');
      const m = /^(.*\S)\s*\(((?:mtg|rec|meeting)-[A-Za-z0-9_-]+)\)\s*$/.exec(full);
      return m ? { title: m[1], id: m[2], full } : { title: full, id: '', full };
    }
    // inboxMarkdownToText renders a Markdown preview as plain text: heading
    // marks, emphasis, code ticks, list bullets, quotes and link syntax go,
    // the words stay (mentions stay plain text). Blank runs collapse.
    function inboxMarkdownToText(md) {
      return String(md || '')
        .replace(/```[^\n]*\n?/g, '')
        .replace(/^[ \t]{0,3}#{1,6}[ \t]+/gm, '')
        .replace(/^[ \t]*>[ \t]?/gm, '')
        .replace(/^[ \t]*(?:[-*+]|\d+[.)])[ \t]+/gm, '')
        .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
        .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
        .replace(/(\*\*|__)(.+?)\1/g, '$2')
        .replace(/(^|[^\w*])\*([^*\n]+)\*(?!\w)/g, '$1$2')
        .replace(/`([^`\n]*)`/g, '$1')
        .replace(/[ \t]+$/gm, '')
        .replace(/\n{3,}/g, '\n\n')
        .trim();
    }
    // Uploaded documents have no audio; uploaded audio/video does. Mailed-in
    // audio (source email) is opened as a file, not streamed.
    // Meetings hive attended (source meeting) never keep audio: the bot only
    // transcribes (docs/meetings.md, "No audio is stored"), so there is nothing
    // to play and no player, play button or error toast is offered.
    function inboxItemHasAudio(it) { return !it || (it.source !== 'upload' && it.source !== 'email' && it.source !== 'meeting') || (it.source === 'upload' && (it.upload_kind === 'audio' || it.upload_kind === 'video')); }
    // Mailed-in files (source email) are opened from the hive itself: GET /api/inbox/email/{id}/file.
    function inboxEmailFileUrl(it) { return '/api/inbox/email/' + encodeURIComponent(it.id) + '/file'; }
    // The role is known once /api/role settled (answered, or retries gave up:
    // then it is undefined and every gate treats the viewer as read-only).
    // Before that the Inbox renders a neutral loading state, never a guessed 'read'.
    function inboxRoleKnown() { return window._hiveRoleSettled === true || window._hiveRole !== undefined; }
    function inboxOnRoleReady() { if (_inboxCache) renderInbox(); }
    (window._hiveRoleListeners = window._hiveRoleListeners || []).push(inboxOnRoleReady);
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
        if (typeof inboxRecSyncDone === 'function') inboxRecSyncDone(_inboxCache.items);
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
      // Sent into an Inception (spek) session: "routed: session/<id> (<name>)"
      // with a button that opens that session's tab.
      if (dest === 'session') {
        return '<span class="kc-pill" data-status="ok" title="Sent into an Inception (spek) session">spek session</span> <span class="muted-xs">routed: ' + escapeHtml(st.note || ('session/' + (st.session || ''))) + '</span>' +
          (st.session ? ' <button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxOpenSession" data-arg0="' + escapeHtml(st.session) + '">open session</button>' : '');
      }
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

    // Summary / memo text: a plain-text preview (a document's Markdown is
    // rendered as text) clamped to INBOX_SUMMARY_CLAMP_LINES lines with a
    // Show more / Show less toggle once it is longer than
    // INBOX_SUMMARY_COLLAPSE_CHARS or that many lines. Falls back to the
    // pending note or the fact excerpt.
    function inboxSummaryHtml(it, key) {
      const src = escapeHtml(it.source), id = escapeHtml(it.id);
      const cached = _inboxContent[key];
      const raw = (cached && cached.summary) || it.summary || '';
      const summary = it.source === 'upload' || it.source === 'email' ? inboxMarkdownToText(raw) : raw;
      if (summary) {
        const long = summary.length > INBOX_SUMMARY_COLLAPSE_CHARS || summary.split('\n').length > INBOX_SUMMARY_CLAMP_LINES;
        const expanded = _inboxExpanded.has(key);
        let html = '<div class="inbox-summary' + (long && !expanded ? ' is-collapsed' : '') + '">' + escapeHtml(summary) + '</div>';
        if (long) html += '<button type="button" class="hv-btn btn-ghost btn-sm inbox-more" data-action="inboxToggleSummary" data-arg0="' + src + '" data-arg1="' + id + '" aria-expanded="' + (expanded ? 'true' : 'false') + '">' + (expanded ? 'Show less' : 'Show more') + '</button>';
        return html;
      }
      // Any text (a machine transcript included) means the item is not
      // pending any more, whatever the source still reports: the "not
      // transcribed yet" line would contradict the transcript shown below.
      const hasText = !!(it.has_transcript || (it.transcription && it.transcription.status === 'done') || (cached && (cached.transcript || cached.summary)));
      if (!hasText && (it.pending || (cached && cached.pending))) {
        // Auto-transcribed items (the hive's own recordings, dropped and
        // mailed-in audio, auto-mode sources) need no click.
        const tx = it.auto_transcribe && !it.transcribe_unavailable ? ' Hive transcribes this automatically; the transcript appears here when it is ready.'
          : it.transcribable && !it.transcribe_unavailable ? ' Hive can transcribe this: click 🎧 Transcribe.'
          : (it.transcribe_unavailable ? ' Hive can transcribe this once transcription is set up (see 🎧 Transcribe below).' : '');
        // Meeting platforms whose pending items read "<label> has no transcript"
        // (inline so the extracted function stays self-contained in tests).
        const meetingLabel = { zoom: 'Zoom', teams: 'Teams', 'google-meet': 'Google Meet', webex: 'Webex', 'slack-huddles': 'Slack', 'fireflies': 'Fireflies', 'fathom': 'Fathom', 'limitless': 'Limitless', 'granola': 'Granola', 'otter': 'Otter' }[it.source];
        if (meetingLabel) return '<div class="inbox-pending muted-xs" title="The platform reports no transcript or summary for this recording yet">Not transcribed yet: ' + meetingLabel + ' has no transcript or summary for this recording (cloud transcription may still be processing or is disabled); the hive re-checks it on every sync.' + tx + '</div>';
        if (it.source === 'hive-recording') return '<div class="inbox-pending muted-xs" title="Recorded in this hive\'s dashboard; no transcript yet">Not transcribed yet: recorded in this hive.' + (it.transcribe_unavailable === 'disabled' ? ' Transcription is off on this hive, so it is transcribed once an owner enables it in Inbox ⚙ → Transcription.' : tx) + '</div>';
        if (it.source === 'upload') return '<div class="inbox-pending muted-xs" title="Added to the Inbox as audio; it has no text until it is transcribed">Not transcribed yet: this audio was added to the Inbox without text.' + tx + '</div>';
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
      // A dropped document's text is the markdown the hive extracted.
      if (it.source === 'upload' && !inboxItemHasAudio(it) && !c.transcript) return '<div class="inbox-transcript"><div class="muted-xs"><strong>Extracted text</strong></div><pre class="inbox-transcript-text">' + escapeHtml(c.markdown || '') + '</pre></div>';
      let html = '<div class="inbox-transcript">';
      if (c.transcript && c.transcript_label) html += '<div class="muted-xs"><strong>' + escapeHtml(c.transcript_label) + '</strong></div>';
      if (c.transcript) html += '<pre class="inbox-transcript-text">' + (it.source === 'meeting' ? meetingsTranscriptTextHtml(c.transcript) : escapeHtml(c.transcript)) + '</pre>';
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

    // What the item's Knowledge fact holds, as a status word in the meta
    // line (not a control, not a floating pill): the source's own text, this
    // hive's machine transcript, or metadata only while no text exists yet.
    function inboxFactStateHtml(it) {
      if (!it.fact_slug) return '';
      const factStates = {
        ready: { label: 'In knowledge', status: 'ok', title: 'Stored as a Knowledge fact with the source\'s own text; knowledge search and agents see it.' },
        machine: { label: 'In knowledge (machine text)', status: 'ok', title: 'Stored as a Knowledge fact with this hive\'s machine transcript; the source\'s own text replaces it when it exists.' },
        pending: { label: 'Pending text', status: 'not loaded', title: 'Stored as a Knowledge fact with metadata only (title, date, duration, highlights); no text yet, so knowledge search finds no words.' },
      };
      const fs = factStates[it.fact_state] || factStates.ready;
      return '<span class="inbox-meta-state" role="status" data-status="' + fs.status + '" data-fact-state="' + escapeHtml(it.fact_state || 'ready') + '" title="' + escapeHtml(fs.title) + '">' + escapeHtml(fs.label) + '</span>';
    }

    // Machine transcription: a state pill (spinner while queued/running, the
    // reason when it failed or was skipped), the progress bar while busy, and
    // the 🎧 Transcribe / Retry button on pending items with audio, for
    // owners and read-write sessions.
    function inboxTranscriptionHtml(it, key) {
      const st = it.transcription;
      const src = escapeHtml(it.source), id = escapeHtml(it.id);
      let html = '';
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
      } else if (it.auto_transcribe && !it.transcribe_unavailable) {
        // Not started yet: the hive queues it on its own (at once, or at
        // its next sweep), so this reads as waiting, not as a to-do.
        html += '<span class="kc-pill" data-status="syncing" data-inbox-tx-auto title="This hive transcribes this recording automatically; no click is needed">' +
          '<span class="inbox-tx-spinner" aria-hidden="true"></span> Transcribing soon</span>';
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
          (inflight ? '<span class="inbox-tx-spinner" aria-hidden="true"></span> Sending…' : (retry ? '🎧 Retry transcription' : (it.auto_transcribe ? '🎧 Transcribe now' : '🎧 Transcribe'))) + '</button>';
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

    // Share history pills: one per share record, in the form
    // "→ RFC #18412 · andy · 2m ago". They are status, not controls: a plain
    // link to the target when there is one (never for read-only viewers),
    // otherwise a span. Only the latest record per destination shows; "+N"
    // expands the rest.
    const _inboxHistoryOpen = new Set();
    const INBOX_SHARE_LABELS = { rfc: 'RFC', dibs: 'DIBS', knowledge: 'Knowledge', spek: 'spek' };
    function inboxSharePillText(rec) {
      const dest = rec.destination;
      const ref = rec.ref || {};
      let label = '→ ' + (INBOX_SHARE_LABELS[dest] || dest);
      if (dest === 'rfc' && ref.number) label += ' #' + ref.number;
      if (dest === 'spek' && ref.name) label += ': ' + ref.name;
      const busy = rec.status === 'queued' || rec.status === 'transcribing' || rec.status === 'in_progress';
      if (rec.status === 'failed') label += ' failed';
      else if (rec.status === 'transcribing') label += ' (transcribing)';
      else if (busy) label += ' …';
      const when = rec.started_at && !String(rec.started_at).startsWith('0001') ? relativeTimeShort(rec.started_at) : '';
      return label + ' · ' + (rec.actor || 'unknown') + (when ? ' · ' + when : '');
    }
    // inboxShareTargetHref returns the link target of a record, or '' when
    // it has none or the viewer may not open it.
    function inboxShareTargetHref(rec, canOpen) {
      const ref = rec.ref || {};
      if (!canOpen) return '';
      if ((rec.destination === 'rfc' || rec.destination === 'dibs') && /^https?:\/\//.test(ref.url || '')) return ref.url;
      if (rec.destination === 'spek' && ref.id) return '#section-sessions-section?session=' + encodeURIComponent(ref.id);
      return '';
    }
    function inboxSharePillHtml(rec, it) {
      const busy = rec.status === 'queued' || rec.status === 'transcribing' || rec.status === 'in_progress';
      const status = rec.status === 'failed' ? 'error' : (rec.status === 'done' ? 'ok' : 'syncing');
      const stamp = rec.finished_at || rec.started_at;
      const title = (rec.status === 'failed' && rec.reason ? rec.reason + ' · ' : (rec.reason && busy ? rec.reason + ' · ' : '')) + (stamp ? formatDashboardDateTime(stamp) : '');
      const inner = (busy ? '<span class="inbox-tx-spinner" aria-hidden="true"></span> ' : '') + escapeHtml(inboxSharePillText(rec));
      const attrs = ' class="kc-pill inbox-share-pill" data-status="' + status + '" data-share-dest="' + escapeHtml(rec.destination) + '" title="' + escapeHtml(title) + '"';
      const canOpen = inboxViewerCanPlay();
      const href = inboxShareTargetHref(rec, canOpen);
      if (href) return '<a' + attrs + ' href="' + escapeHtml(href) + '"' + (/^https?:/.test(href) ? ' target="_blank" rel="noopener noreferrer"' : '') + '>' + inner + '</a>';
      if (canOpen && rec.destination === 'knowledge' && (rec.ref || {}).id && it && it.fact_slug) {
        const layer = inboxFactLayer(it);
        if (layer) return '<a' + attrs + ' href="#" data-action="inboxOpenFact" data-prevent="1" data-arg0="' + escapeHtml(it.fact_slug) + '" data-arg1="' + escapeHtml(layer) + '">' + inner + '</a>';
      }
      return '<span' + attrs + '>' + inner + '</span>';
    }
    function inboxSharePillsHtml(it, key) {
      const hist = (it && it.share_history) || [];
      if (!hist.length) return '';
      const open = _inboxHistoryOpen.has(key);
      const latest = {};
      hist.forEach(r => { latest[r.destination] = r; });
      const shown = hist.filter(r => open || latest[r.destination] === r);
      const hidden = hist.length - shown.length;
      let html = shown.map(r => inboxSharePillHtml(r, it)).join(' ');
      if (hidden > 0 || (open && hist.length > Object.keys(latest).length)) {
        html += ' <a href="#" class="muted-xs inbox-share-more" data-action="inboxToggleHistory" data-prevent="1" data-arg0="' + escapeHtml(it.source) + '" data-arg1="' + escapeHtml(it.id) + '" aria-expanded="' + (open ? 'true' : 'false') + '" title="' + (open ? 'Show only the latest share per destination' : 'Show the full share history') + '">' + (open ? 'less' : '+' + hidden) + '</a>';
      }
      return html;
    }
    function inboxToggleHistory(src, id) {
      const key = src + '/' + id;
      if (_inboxHistoryOpen.has(key)) _inboxHistoryOpen.delete(key); else _inboxHistoryOpen.add(key);
      renderInbox();
    }

    function inboxItemHtml(it) {
      const key = inboxItemKey(it);
      const checked = _inboxSelected.has(key) ? ' checked' : '';
      const when = it.recorded_at && !String(it.recorded_at).startsWith('0001') ? relativeTimeShort(it.recorded_at) : '';
      const dur = inboxFmtDuration(it.duration_ms);
      // The backing Knowledge fact is a plain link in the metadata line, not
      // an action: it only opens the fact.
      const factLayer = it.fact_slug && inboxFactLayer(it);
      const factLink = factLayer ? '<a href="#" class="inbox-fact-link" data-action="inboxOpenFact" data-prevent="1" data-arg0="' + escapeHtml(it.fact_slug) + '" data-arg1="' + escapeHtml(factLayer) + '" title="Open this item\'s Knowledge fact">open in Knowledge</a>' : '';
      // The title is shown without its trailing recording id; the id and the
      // Knowledge state join the meta line, and the full title is the tooltip.
      const tp = inboxTitleParts(it.title || it.id);
      const idMeta = tp.id ? '<span class="inbox-meta-id" title="Recording id">' + escapeHtml(tp.id) + '</span>' : '';
      const state = inboxFactStateHtml(it);
      // Files and links someone added show where they came from and who
      // added them ("upload · notes.md · added 2h ago by ana").
      const meta = it.source === 'upload'
        ? [escapeHtml(it.origin || 'upload'), when ? 'added ' + when : '', it.added_by ? 'by ' + escapeHtml(it.added_by) : '', dur, idMeta, state, factLink].filter(Boolean).join(' · ')
        : it.source === 'email'
          ? ['email', it.sender ? 'from ' + escapeHtml(it.sender) : '', when ? 'received ' + when : '', escapeHtml(it.upload_kind || ''), idMeta, state].filter(Boolean).join(' · ')
          : [escapeHtml(it.source), when ? 'recorded ' + when : '', it.recorded_by ? 'by ' + escapeHtml(it.recorded_by) : '', dur, idMeta, state, factLink].filter(Boolean).join(' · ');
      const routes = it.routes || {};
      let pills = '';
      for (const d of Object.keys(routes)) pills += inboxStatePill(d, routes[d]) + ' ';
      if (it.dismissed) pills += '<span class="kc-pill" title="Kept as a Knowledge fact only">knowledge only</span>';
      if (it.awaiting_approval) pills += '<span class="kc-pill" data-status="warn" title="Mailed in by a sender outside this hive\'s allowlist: it cannot be shared or transcribed until the owner approves it">awaiting approval</span> ';
      pills += inboxSharePillsHtml(it, key);
      for (const d of Object.keys(it.share_intents || {})) {
        const si = it.share_intents[d];
        if (si && si.status === 'failed') pills += '<span class="kc-pill" data-status="error" title="' + escapeHtml(si.error || '') + '">→ ' + escapeHtml(inboxIntentLabel(d)) + ' failed</span> <span class="kc-error muted-xs" role="alert">' + escapeHtml(si.error || '') + '</span> ';
      }
      const owner = viewerIsOwner();
      const src = escapeHtml(it.source), id = escapeHtml(it.id);
      // The action row: icon buttons of one size (play, share, open file,
      // delete) and one compact text toggle (Transcript / Text). Every icon
      // button names itself with aria-label and title. A destructive control
      // goes last, after the separator: #18694's Delete for every item kind
      // drops in there with the same inbox-action-danger class.
      let actions = '';
      // A hive recording past retention has no audio left: no play control.
      if (it.audio_expired) actions += '<span class="muted-xs inbox-audio-expired" title="The raw audio was deleted by retention; the transcript and the Knowledge fact are kept">audio expired (kept ' + (Number(it.audio_retention_days) || 5) + ' days)</span>';
      else if (inboxViewerCanPlay() && inboxItemHasAudio(it)) actions += '<button type="button" class="hv-btn btn-icon btn-sm inbox-action inbox-play" data-action="inboxPlay" data-arg0="' + src + '" data-arg1="' + id + '" aria-label="Play recording" title="Play recording (streams through this hive)">' + inboxIconSvg('play') + '</button>';
      const txOpen = _inboxTranscriptOpen.has(key);
      const txLabel = it.source === 'upload' && !inboxItemHasAudio(it) ? 'Text' : 'Transcript';
      actions += '<button type="button" class="hv-btn btn-ghost btn-sm inbox-action inbox-action-text" data-action="inboxToggleTranscript" data-arg0="' + src + '" data-arg1="' + id + '" aria-expanded="' + (txOpen ? 'true' : 'false') + '" title="' + (txOpen ? 'Hide the ' : 'Show the ') + txLabel.toLowerCase() + '">' + inboxIconSvg(txOpen ? 'chevron-down' : 'chevron-right') + txLabel + '</button>';
      if (it.source === 'email' && inboxViewerCanPlay()) actions += '<a class="hv-btn btn-icon btn-sm inbox-action" href="' + escapeHtml(inboxEmailFileUrl(it)) + '" target="_blank" rel="noopener noreferrer" aria-label="Open file" title="Open the mailed-in file">' + inboxIconSvg('file') + '</a>';
      if (it.awaiting_approval) {
        if (owner) actions += '<button type="button" class="hv-btn btn-primary btn-sm inbox-action" data-action="inboxEmailApprove" data-arg0="' + id + '" title="Let this mailed-in item be shared and transcribed like any other">Approve</button>' +
          '<button type="button" class="hv-btn btn-secondary btn-sm inbox-action" data-action="inboxEmailDecline" data-arg0="' + id + '" title="Delete this mailed-in item">Decline</button>';
      } else {
        actions += '<button type="button" class="hv-btn btn-icon btn-sm inbox-action inbox-share" data-action="inboxShare" data-arg0="' + src + '" data-arg1="' + id + '" aria-label="Share" title="Share: file this item in the hive, or copy a link to it (not the recording)" data-arg-types="s,s,t" aria-haspopup="menu" aria-expanded="false">' + inboxIconSvg('share') + '</button>';
      }
      // Delete for every item kind (#18694, inbox-delete.js) is the last
      // control, set apart by a separator.
      const del = typeof inboxDeleteButtonHtml === 'function' ? inboxDeleteButtonHtml(it) : '';
      if (del) actions += '<span class="inbox-actions-sep" role="separator"></span>' + del;
      return '<div class="kb-fact-item inbox-item" data-inbox-key="' + escapeHtml(key) + '">' +
        (inboxCanShare(window._hiveRole) ? '<label class="inbox-check"><input type="checkbox" data-change-action="inboxToggleSelect" data-arg0="' + src + '" data-arg1="' + id + '"' + checked + ' aria-label="Select ' + escapeHtml(tp.title) + '"></label>' : '') +
        '<div class="inbox-icon" title="' + src + '">' + inboxSourceIcon(it.source) + '</div>' +
        '<div class="kb-fact-body"><div class="kb-fact-title inbox-title"' + (tp.id ? ' title="' + escapeHtml(tp.full) + '"' : '') + '>' + escapeHtml(tp.title) + '</div>' +
        '<div class="muted-xs inbox-meta">' + meta + '</div>' +
        inboxSummaryHtml(it, key) +
        '<div class="inbox-media" data-inbox-media="' + escapeHtml(key) + '"></div>' +
        (it.source === 'meeting' ? '<div class="muted-xs inbox-audio-none">Audio is not kept for live meetings</div>' : '') +
        (it.source === 'email' && it.upload_kind === 'image' && inboxViewerCanPlay() ? '<img class="inbox-email-image" loading="lazy" src="' + escapeHtml(inboxEmailFileUrl(it)) + '" alt="' + escapeHtml(it.title || 'mailed-in image') + '">' : '') +
        (pills ? '<div class="inbox-pills">' + pills + '</div>' : '') +
        inboxTranscriptionHtml(it, key) +
        '<div class="inbox-actions">' + actions + '</div>' +
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
    // inboxAudioNoteText is the line under the player. Audio this hive
    // stores itself (its own recordings, dropped audio files) is kept until
    // retention deletes it (audio_expires_at); a third-party source's audio
    // is only streamed through and never stored here.
    function inboxAudioNoteText(it, fmtDate) {
      const restart = ' Click Play again to restart.';
      if (it && (it.source === 'hive-recording' || it.source === 'upload')) {
        const exp = it.audio_expires_at && !String(it.audio_expires_at).startsWith('0001') ? it.audio_expires_at : '';
        const when = exp && fmtDate ? fmtDate(exp) : '';
        return (when ? 'The audio is kept on this hive until ' + when + ', then deleted; the transcript stays.' : 'The audio is kept on this hive.') + restart;
      }
      return 'Streaming through this hive; the recording is not stored.' + restart;
    }
    function inboxPlay(src, id) {
      if (src === 'meeting') return;
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
      note.textContent = inboxAudioNoteText(inboxFind(src, id), d => formatDashboardDate(d, { year: 'numeric', month: 'short', day: 'numeric' }));
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
      // spek…: send into an Inception (spek) session (the dialog and the
      // seed API live with the session code below). An audio-only item is
      // transcribed first and seeds the session when its transcript is ready.
      { id: 'spek', dest: 'spek', label: 'spek…', enabled: (it, role) => !inboxCanShare(role) ? false : inboxShareGate(it, 'spek') || true, run: it => inboxOpenSessionDialog([inboxItemKey(it)]), bulkLabel: 'spek…', bulk: keys => inboxOpenSessionDialog(keys) },
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
    function inboxIntentLabel(dest) { return ({ spek_rfc: 'RFC', dismiss: 'Knowledge', spek: 'spek' })[dest] || dest; }
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
      const manage = typeof inboxDeleteBulkGroups === 'function' ? inboxDeleteBulkGroups() : [];
      inboxPopMenu(btn, inboxShareMenuHtml(inboxBulkMenuGroups().concat(manage), '', ''), d => d.inboxShareGroup === 'manage' ? inboxDeleteSelected() : inboxBulkMenuRun(d.inboxShareAct));
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
      if (!data || !inboxRoleKnown()) { panel.innerHTML = '<div class="muted-note">Loading inbox…</div>'; return; }
      const items = data.items || [];
      const unprocessed = Number(data.unprocessed) || 0;
      if (badge) setSidebarCountBadge(badge, unprocessed, unprocessed + ' unprocessed');
      setSectionSummary('inbox-section', unprocessed + ' to triage · ' + items.length + ' total');
      const owner = viewerIsOwner();
      const settings = data.settings || {};
      const rules = Array.isArray(settings.rules) ? settings.rules.length : 0;
      if (_inboxFocusKey && items.some(it => it.processed && inboxItemKey(it) === _inboxFocusKey)) _inboxShowProcessed = true;
      const shown = inboxShownItems();
      const selectedBeforeRender = _inboxSelected.size;
      inboxPruneSelection(shown);
      const closeBulkMenu = _inboxSelected.size !== selectedBeforeRender && _inboxShareMenu?.btn.dataset.action === 'inboxBulkMenu';
      const restoreBulkFocus = closeBulkMenu && (_inboxShareMenu.btn === document.activeElement || _inboxShareMenu.el.contains(document.activeElement));
      if (closeBulkMenu) inboxShareMenuClose(false);
      // Sources line: only configured sources are chips (with the account
      // hint); the rest sit behind a "+N more" disclosure next to Connect
      // sources, so the header is one quiet line instead of a wall of
      // "not configured".
      const sources = inboxSourcesHtml(data.sources || []);
      // Toolbar, one row: primary actions on the left (Record, Add), the
      // view controls on the right (Show processed, Select all, the bulk
      // Hive menu, Auto-routing), every one of them a button of one size.
      const sel = _inboxSelected.size;
      const canSelect = inboxCanShare(window._hiveRole) && shown.length > 0;
      let html = sources +
        '<div class="inbox-toolbar" role="toolbar" aria-label="Inbox actions">' +
        '<div class="inbox-toolbar-group">' +
        (typeof inboxRecordButtonHtml === 'function' ? inboxRecordButtonHtml(settings) : '') +
        (inboxCanAdd() ? '<button type="button" class="hv-btn btn-primary btn-sm" data-action="inboxToggleAdd" aria-expanded="' + (_inboxAddOpen ? 'true' : 'false') + '" aria-controls="inbox-add" title="Add files, a link or a quick note to the Inbox (or drop files anywhere on it, or paste with Ctrl/Cmd+V)">＋ Add</button>' : '') +
        '</div><div class="inbox-toolbar-group">' +
        '<button type="button" class="hv-btn btn-secondary btn-sm inbox-filter" data-action="inboxToggleProcessed" aria-pressed="' + (_inboxShowProcessed ? 'true' : 'false') + '" title="Also list items already filed or kept as Knowledge">Show processed</button>' +
        (canSelect ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxSelectAll" title="' + (sel === shown.length ? 'Clear the selection' : 'Select every listed item for the Hive menu') + '">' + (sel === shown.length ? 'Clear selection' : 'Select all') + '</button>' : '') +
        (canSelect && sel ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxBulkMenu" data-arg-types="t" aria-haspopup="menu" aria-expanded="false" title="File, keep or delete the selected items">Hive (' + sel + ') ▾</button>' : '') +
        (owner ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxOpenSettings" title="Auto-routing rules, default RFC repository, dibs URL, transcription">⚙ Auto-routing (' + rules + ')</button>' : '') +
        '</div></div>';
      if (typeof inboxInputMethodsHtml === 'function') html += inboxInputMethodsHtml();
      if (typeof emailInRowHtml === 'function') html += emailInRowHtml('inbox', data.email_in);
      if (data.error) html += '<div class="kc-error muted-note">' + escapeHtml(data.error) + '</div>';
      if (window._hiveRoleFailed && !window._hiveRole) html += '<div class="muted-xs">Could not determine your role, so actions are read-only. It will retry automatically.</div>';
      if (inboxCanAdd()) html += inboxAddPanelHtml() + inboxUploadsHtml();
      if (!settings.spek_ready && owner) html += '<p class="muted-xs">RFC filing needs at least one enabled, unpaused agent to draft the spec with spektacular (<code>inbox.spek.agent</code> overrides the default; <code>inbox.spek.binary</code> names the CLI the agent runs).</p>';
      if (!shown.length) html += '<div class="muted-note">' + (items.length ? 'Everything is triaged. Toggle “show processed” to see filed and dismissed items.' : 'Nothing yet. Connect Plaud or another voice source under Knowledge → Integrations; new recordings land here after the next sync (a newly added connector starts on the next hive restart).') + '</div>';
      else html += '<div class="kb-fact-list inbox-list">' + shown.map(inboxItemHtml).join('') + '</div>';
      const paint = () => { panel.innerHTML = html; };
      inboxPreserveAudio(panel, typeof inboxNoteKeepFocus === 'function' ? () => inboxNoteKeepFocus(paint) : paint);
      if (restoreBulkFocus) {
        const control = panel.querySelector('[data-action="inboxBulkMenu"]') || panel.querySelector('[data-action="inboxToggleProcessed"]');
        control?.focus();
      }
      inboxApplyFocus(panel);
    }

    // inboxSourceChip is one source as a chip: connected (with its account),
    // configured but not connected, or not configured.
    function inboxSourceChip(s) {
      const status = s.connected ? 'ok' : (s.configured ? 'error' : '');
      const label = inboxSourceIcon(s.type) + ' ' + escapeHtml(s.type) + (s.connected ? (s.account ? ': ' + escapeHtml(s.account) : ': connected') : s.configured ? ': not connected' : ': not configured');
      return '<span class="kc-pill inbox-source" data-status="' + status + '" data-source="' + escapeHtml(s.type) + '">' + label + '</span>';
    }
    function inboxSourcesHtml(all) {
      const configured = all.filter(s => s.configured || s.connected);
      const rest = all.filter(s => !(s.configured || s.connected));
      const connect = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbOpenIntegrations" title="Connect a voice or meeting source under Knowledge → Integrations">Connect sources</button>';
      let html = '<div class="inbox-sources">' + (configured.length ? configured.map(inboxSourceChip).join(' ') : '<span class="muted-xs">No voice sources configured.</span>');
      if (rest.length && configured.length) html += ' <button type="button" class="hv-btn btn-ghost btn-sm inbox-sources-more" data-action="inboxToggleSources" aria-expanded="' + (_inboxSourcesOpen ? 'true' : 'false') + '" aria-controls="inbox-sources-rest" title="' + (_inboxSourcesOpen ? 'Hide' : 'Show') + ' the ' + rest.length + ' sources that are not configured">' + (_inboxSourcesOpen ? 'Hide ' + rest.length + ' more' : '+' + rest.length + ' more') + '</button>';
      html += ' ' + connect + '</div>';
      if (rest.length && configured.length) html += '<div id="inbox-sources-rest" class="inbox-sources inbox-sources-rest"' + (_inboxSourcesOpen ? '' : ' hidden') + '>' + rest.map(inboxSourceChip).join(' ') + '</div>';
      return html;
    }
    function inboxToggleSources() { _inboxSourcesOpen = !_inboxSourcesOpen; renderInbox(); }

    function inboxShownItems() {
      return ((_inboxCache || {}).items || []).filter(it => _inboxShowProcessed || !it.processed);
    }
    function inboxPruneSelection(shown) {
      if (!_inboxSelected.size) return;
      const shownKeys = new Set(shown.map(inboxItemKey));
      for (const key of _inboxSelected) {
        if (!shownKeys.has(key)) _inboxSelected.delete(key);
      }
    }
    function inboxToggleProcessed() { _inboxShowProcessed = !_inboxShowProcessed; renderInbox(); }
    function inboxToggleSelect(src, id) {
      const key = src + '/' + id;
      if (_inboxSelected.has(key)) _inboxSelected.delete(key); else _inboxSelected.add(key);
      renderInbox();
    }
    function inboxSelectAll() {
      const shown = inboxShownItems();
      if (_inboxSelected.size === shown.length) _inboxSelected.clear();
      else _inboxSelected = new Set(shown.map(inboxItemKey));
      renderInbox();
    }
    function inboxFind(src, id) { return ((_inboxCache || {}).items || []).find(it => it.source === src && it.id === id); }
    function inboxFactLayer(it) {
      return it.fact_source_layer || (it.fact_path ? '' : it.layer || 'all');
    }
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
    // Approval queue for mailed-in files from outside the allowlist (#18395).
    async function inboxEmailAct(id, action) {
      try {
        const r = await fetch('/api/inbox/email/' + encodeURIComponent(id) + '/' + action, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        showToast(action === 'approve' ? 'Approved' : 'Declined and deleted', 'success');
      } catch (e) {
        showToast('Could not ' + action + ': ' + e.message, 'error');
      }
      await fetchInbox();
    }
    function inboxEmailApprove(id) { return inboxEmailAct(id, 'approve'); }
    function inboxEmailDecline(id) { return inboxEmailAct(id, 'decline'); }

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

    // Share → Hive → spek…: send items into an interactive Inception (spek)
    // session, a new one (name from the first item, group "inbox", the last
    // CLI this browser used) or a running one the viewer may type into. The
    // hive writes one enveloped file per item under .hive/inbox/ in the
    // session's workspace and queues a fixed Send that names those files
    // (POST /api/sessions with seed_items, or POST /api/sessions/{id}/seed).
    const INBOX_SESSION_GROUP = 'inbox';
    const INBOX_SESSION_VENDOR_KEY = 'hive.sessions.lastVendor';
    const INBOX_SESSION_NAME_MAX = 64;
    let _inboxSessionKeys = [];
    function inboxSessionCanSend() { return window._hiveRole === 'owner' || window._hiveRole === 'read-write'; }
    function inboxKeyRef(key) { const i = key.indexOf('/'); return { source: key.slice(0, i), id: key.slice(i + 1) }; }
    function inboxSessionName(items) {
      const first = (items[0] && (items[0].title || items[0].id)) || 'Inbox';
      const name = Array.from(String(first).trim()).slice(0, INBOX_SESSION_NAME_MAX).join('');
      return name || 'Inbox';
    }
    function inboxSessionLastVendor() { try { return localStorage.getItem(INBOX_SESSION_VENDOR_KEY) || ''; } catch (e) { return ''; } }
    // inboxSessionPickVendor: the last-used vendor when it is still available,
    // else the first available one.
    function inboxSessionPickVendor(vendors, last) {
      const avail = (vendors || []).filter(v => v && v.available);
      const hit = avail.find(v => v.id === last);
      return hit ? hit.id : (avail[0] ? avail[0].id : '');
    }
    // Running sessions the viewer can send into: the list already holds only
    // sessions the viewer may see, and for owner/read-write seeing a running
    // session (own or shared) is being allowed to type into it.
    function inboxSessionTargets(sessions) {
      return (sessions || []).filter(s => s && s.status === 'running');
    }
    function inboxSessionDialogHtml(items, data, last) {
      const vendors = (data.vendors || []).filter(v => v && v.available);
      const running = inboxSessionTargets(data.sessions);
      const pick = inboxSessionPickVendor(vendors, last);
      const titles = items.map(it => escapeHtml(it.title || it.id));
      let body = '<p class="muted-note m-0"><strong>' + (items.length === 1 ? titles[0] : items.length + ' items') + '</strong></p>';
      if (items.length > 1) body += '<ul class="muted-xs inbox-session-items">' + titles.map(t => '<li>' + t + '</li>').join('') + '</ul>';
      body += '<p class="muted-note">The hive writes ' + (items.length === 1 ? 'the item' : 'each item') + ' (title, summary, transcript, highlights) to <code>.hive/inbox/</code> in the session\'s workspace and queues a Send asking the CLI to start a spec with the spek-new skill. The text goes in as untrusted data, never as instructions.</p>';
      if (items.some(it => it.pending)) body += '<p class="muted-xs">An item with audio but no text yet is transcribed first (🎧 Transcribe) and goes into the session when its transcript is ready.</p>';
      body += '<fieldset class="inbox-session-mode"><label class="muted-note"><input type="radio" name="inbox-session-mode" value="new" checked data-inbox-session-mode="new"> Start a new session</label>' +
        '<div class="inbox-settings-grid">' +
        '<label class="muted-note inbox-field">Name<input class="kb-search-box mt-2" id="inbox-session-name" maxlength="' + INBOX_SESSION_NAME_MAX + '" autocomplete="off" value="' + escapeHtml(inboxSessionName(items)) + '"></label>' +
        '<label class="muted-note inbox-field">Group<input class="kb-search-box mt-2" id="inbox-session-group" autocomplete="off" value="' + INBOX_SESSION_GROUP + '"></label>' +
        '<label class="muted-note inbox-field">CLI<select class="field-input field-input-block mt-2" id="inbox-session-vendor">' +
        vendors.map(v => '<option value="' + escapeHtml(v.id) + '"' + (v.id === pick ? ' selected' : '') + '>' + escapeHtml(v.label || v.id) + '</option>').join('') + '</select></label></div>' +
        (vendors.length ? '' : '<p class="kc-error muted-xs">No CLI is installed on this hive.</p>') +
        '<label class="muted-note"><input type="radio" name="inbox-session-mode" value="existing" data-inbox-session-mode="existing"' + (running.length ? '' : ' disabled') + '> Add to a running session</label>' +
        '<select class="field-input field-input-block mt-2" id="inbox-session-existing" aria-label="Running session"' + (running.length ? '' : ' disabled') + '>' +
        (running.length ? running.map(s => '<option value="' + escapeHtml(s.id) + '">' + escapeHtml((s.name || s.id) + (s.group ? ' · ' + s.group : '') + ' (' + (s.vendor || '') + ')') + '</option>').join('') : '<option value="">No running session you can type into</option>') +
        '</select></fieldset>';
      body += '<div class="hive-dialog-error" role="alert" id="inbox-session-error"></div>' +
        '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-action="kbCloseModal">Cancel</button>' +
        '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxConfirmSession">Send to session</button></div>';
      return body;
    }
    // inboxOpenSessionDialog(keys): keys are "source/id" (one item, or the
    // multi-select). The session list is read fresh so it names only
    // sessions that are running now.
    async function inboxOpenSessionDialog(keys) {
      if (!inboxSessionCanSend()) return;
      keys = (keys || []).filter(Boolean);
      const items = keys.map(k => { const r = inboxKeyRef(k); return inboxFind(r.source, r.id) || { source: r.source, id: r.id, title: r.id }; });
      if (!items.length) return;
      let data = {};
      try {
        const r = await fetch('/api/sessions', { headers: _inceptionAuthHeaders(), credentials: 'same-origin', cache: 'no-store' });
        data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      } catch (e) { showToast('Could not list sessions: ' + e.message, 'error'); return; }
      if (data.enabled === false) { showToast('Inception (spek) sessions are not enabled on this hive.', 'error'); return; }
      _inboxSessionKeys = keys;
      kbShowModal('Send to an Inception (spek) session', inboxSessionDialogHtml(items, data, inboxSessionLastVendor()), { width: '560px' });
    }
    function inboxOpenSessionSelected() { return inboxOpenSessionDialog(Array.from(_inboxSelected)); }
    // inboxSessionRequest builds the POST for the dialog's choice.
    function inboxSessionRequest(mode, keys, fields) {
      const items = keys.map(inboxKeyRef);
      if (mode === 'existing') return { path: '/api/sessions/' + encodeURIComponent(fields.session) + '/seed', body: { items: items } };
      return { path: '/api/sessions', body: { name: fields.name, group: fields.group, vendor: fields.vendor, shared: false, seed_items: items } };
    }
    // inboxSessionErrorText: a 409 for an item without text says how to fix it.
    function inboxSessionErrorText(status, data) {
      const msg = (data && data.error) || ('HTTP ' + status);
      if (status === 409 && data && data.reason === 'no_text') return msg + (data.transcribe ? ' Use 🎧 Transcribe on the item, then send it again.' : '');
      return msg;
    }
    async function inboxConfirmSession() {
      const val = id => { const el = document.getElementById(id); return el ? String(el.value || '').trim() : ''; };
      const modeEl = document.querySelector('[data-inbox-session-mode]:checked');
      const mode = modeEl ? modeEl.value : 'new';
      const errEl = document.getElementById('inbox-session-error');
      const fields = { name: val('inbox-session-name'), group: val('inbox-session-group'), vendor: val('inbox-session-vendor'), session: val('inbox-session-existing') };
      const fail = m => { if (errEl) errEl.textContent = m; else showToast(m, 'error'); };
      if (mode === 'new' && (!fields.name || !fields.vendor)) { fail('A new session needs a name and a CLI.'); return; }
      if (mode === 'existing' && !fields.session) { fail('Pick a running session.'); return; }
      const keys = _inboxSessionKeys.slice();
      const req = inboxSessionRequest(mode, keys, fields);
      let data = {};
      try {
        const r = await fetch(req.path, { method: 'POST', headers: _inceptionAuthHeaders(), credentials: 'same-origin', body: JSON.stringify(req.body) });
        data = await r.json().catch(() => ({}));
        if (!r.ok || data.ok === false) { fail(inboxSessionErrorText(r.status, data)); return; }
      } catch (e) { fail(e.message); return; }
      kbCloseModal();
      if (mode === 'new' && fields.vendor) { try { localStorage.setItem(INBOX_SESSION_VENDOR_KEY, fields.vendor); } catch (e) { /* convenience only */ } }
      for (const k of keys) _inboxSelected.delete(k);
      const sess = data.session || { id: fields.session };
      const waiting = ((data.seed || {}).waiting || []).length;
      showToast('Sent ' + keys.length + ' item' + (keys.length === 1 ? '' : 's') + ' to ' + (sess.name || fields.name || 'the session') +
        (waiting ? ' (' + waiting + ' transcribing first; ' + (waiting === 1 ? 'it goes' : 'they go') + ' in when the transcript is ready)' : ''), 'success');
      if (typeof sessionsFocus === 'function') sessionsFocus(sess);
      fetchInbox();
    }
    // inboxOpenSession: the "open session" button on a routed item.
    function inboxOpenSession(id) {
      if (typeof sessionsFocus === 'function') sessionsFocus({ id: id });
    }

    // Settings drawer: default RFC repo, dibs URL, and ordered auto-routing rules.
    let _inboxDraftRules = [];
    function inboxRuleRowHtml(rule, i) {
      const sources = ((_inboxCache || {}).settings || {}).sources || [];
      const srcOpts = ['<option value=""' + (!rule.source ? ' selected' : '') + '>any source</option>'].concat(sources.map(s => '<option value="' + escapeHtml(s) + '"' + (rule.source === s ? ' selected' : '') + '>' + escapeHtml(s) + '</option>')).join('');
      const destOpts = [['spek_rfc', '→ RFC'], ['dibs', '→ dibs'], ['dismiss', 'Knowledge only']].map(([v, l]) => '<option value="' + v + '"' + (rule.destination === v ? ' selected' : '') + '>' + l + '</option>').join('');
      return '<div class="inbox-rule" data-rule-index="' + i + '">' +
        '<select class="kb-search-box" data-rule-field="source" aria-label="Source">' + srcOpts + '</select>' +
        '<input class="kb-search-box" data-rule-field="title_prefix" placeholder="title prefix (RFC:)" value="' + escapeHtml(rule.title_prefix || '') + '" aria-label="Title prefix">' +
        '<input class="kb-search-box" data-rule-field="tag" placeholder="or tag" value="' + escapeHtml(rule.tag || '') + '" aria-label="Tag">' +
        '<select class="kb-search-box" data-rule-field="destination" aria-label="Destination">' + destOpts + '</select>' +
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
      host.innerHTML = _inboxDraftRules.length ? _inboxDraftRules.map(inboxRuleRowHtml).join('') : '<p class="muted-xs">No rules: every item waits here for manual triage.</p>';
    }
    // The form has two sections on one vertical rhythm: Routing (default RFC
    // repository, dibs URL, rules) and Transcription. Config keys sit in a
    // "Config keys" footnote at the end of each section, away from the
    // labels. In the modal the Save bar stays visible while the body scrolls.
    function inboxSettingsFormHtml(settings, inline) {
      const buttons = inline
        ? '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxSaveSettings">Save</button></div>'
        : '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-action="kbCloseModal">Cancel</button>' +
          '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxSaveSettings">Save</button></div>';
      const keys = list => '<details class="inbox-set-keys"><summary class="muted-xs">Config keys</summary><p class="muted-xs">' + list.map(yamlHint).join(' ') + '</p></details>';
      return '<div class="inbox-settings-form" data-inbox-form="' + (inline ? 'inline' : 'modal') + '">' +
        '<section class="inbox-set-section" data-inbox-set="routing" aria-label="Routing">' +
        '<h4 class="section-label inbox-set-title">Routing</h4>' +
        '<p class="muted-xs">New items are checked against the rules below, in order, after every connector sync; the first match routes the item automatically. Items that arrived before a rule existed are never re-routed.</p>' +
        '<div class="inbox-settings-grid">' +
        '<label class="inbox-field"><span class="inbox-field-label">Default RFC repository</span><input class="kb-search-box" type="text" data-inbox-field="spek-repo" placeholder="owner/repo" autocomplete="off" value="' + escapeHtml(settings.spek_repo || '') + '"></label>' +
        '<label class="inbox-field"><span class="inbox-field-label">dibs URL</span><input class="kb-search-box" type="text" data-inbox-field="dibs-url" placeholder="https://dibs.example.org" autocomplete="off" value="' + escapeHtml(settings.dibs_url || '') + '"></label></div>' +
        '<p class="muted-xs inbox-set-status" data-inbox-dibs-token><span>dibs token</span> <span class="kc-pill" data-status="' + (settings.dibs_token_set ? 'ok' : 'error') + '">' + (settings.dibs_token_set ? 'set' : 'not set') + '</span> ' +
        '<span>' + (settings.dibs_token_set ? 'Read from this hive\'s environment.' : 'Set it in this hive\'s environment; it is never stored in hive.yaml.') + '</span></p>' +
        '<div class="row-between inbox-set-subhead"><span class="inbox-field-label">Rules</span><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxAddRule">Add rule</button></div>' +
        '<div class="inbox-settings-rules"></div>' +
        keys(['inbox.spek.repo', 'inbox.dibs.url', 'inbox.dibs.token_env', 'inbox.rules']) +
        '</section>' +
        inboxTranscriptionFormHtml(settings.transcription) +
        '<div class="inbox-set-footer"><div class="hive-dialog-error" role="alert" data-inbox-field="error"></div>' + buttons + '</div></div>';
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
    // Provider selector, then one row per provider: name, one-line
    // description, a status pill in its own column, and what is missing (or
    // any other detail) on its own muted line. Never a key value.
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
        const pill = name === 'auto'
          ? (tx.resolved_provider ? '<span class="kc-pill" data-status="ok">now: ' + escapeHtml(tx.resolved_provider) + '</span>' : '')
          : (st ? '<span class="kc-pill" data-status="' + (st.available ? 'ok' : 'error') + '">' + (st.available ? 'ready' : 'not ready') + '</span>' : '');
        const detail = name !== 'auto' && st && st.detail ? '<span class="muted-xs inbox-tx-provider-detail">' + escapeHtml(st.detail) + '</span>' : '';
        rows += '<div class="inbox-tx-provider" role="listitem" data-inbox-tx-provider="' + name + '">' +
          '<span class="inbox-tx-provider-name">' + name + '</span>' +
          '<span class="inbox-tx-provider-desc">' + escapeHtml(info[name]) + '</span>' +
          '<span class="inbox-tx-provider-status">' + pill + '</span>' + detail + '</div>';
      }
      return '<label class="inbox-field"><span class="inbox-field-label">Provider</span><select class="kb-search-box" data-inbox-tx-field="provider" data-change-action="inboxTxFieldsChanged" data-arg-types="t">' + opts + '</select></label>' +
        '<div class="inbox-tx-providers" role="list" aria-label="Transcription providers">' + rows + '</div>';
    }

    // Transcription section: spoke-local transcription with whisper (on the
    // hive), the hive's Gemini credentials, or the owner's Deepgram key. Key
    // values are never shown, only whether they are present on this hive.
    // A provider's own fields show only while that provider is selected, or
    // while auto could pick it (it is ready, or its readiness is unknown);
    // hidden fields keep their values, so saving sends the same payload.
    function inboxTranscriptionFormHtml(tx) {
      tx = tx || {};
      const provider = inboxTxProviderInfo()[tx.provider] ? tx.provider : 'auto';
      const unready = (Array.isArray(tx.providers) ? tx.providers : []).filter(p => p && p.available === false).map(p => p.name);
      const shows = names => names.split(' ').some(n => provider === n || (provider === 'auto' && unready.indexOf(n) < 0));
      const only = names => ' data-inbox-tx-for="' + names + '"' + (shows(names) ? '' : ' hidden');
      const field = (name, label, value, placeholder, type, attrs) => '<label class="inbox-field"' + (attrs || '') + '><span class="inbox-field-label">' + label + '</span>' +
        '<input class="kb-search-box" type="' + (type || 'text') + '" data-inbox-tx-field="' + name + '" autocomplete="off" placeholder="' + escapeHtml(placeholder || '') + '" value="' + escapeHtml(value == null ? '' : String(value)) + '"></label>';
      const mode = tx.mode === 'auto' ? 'auto' : 'manual';
      const env = tx.api_key_env || 'DEEPGRAM_API_KEY';
      const sizeMB = tx.max_bytes ? Math.round(Number(tx.max_bytes) / INBOX_TX_BYTES_PER_MB) : '';
      const whisperLabels = { 'tiny.en': ' (78 MB, fastest)', tiny: ' (78 MB, any language)', 'base.en': ' (148 MB)', base: ' (148 MB, any language)', 'small.en': ' (488 MB, about 3x slower)', small: ' (488 MB, any language, about 3x slower)' };
      const keys = ['enabled', 'consent_acknowledged', 'provider', 'mode', 'sources[]', 'language', 'whisper_model', 'whisper_threads', 'gemini_model', 'api_key_env', 'model', 'diarize',
        'monthly_budget_minutes', 'daily_max_items', 'max_duration_minutes', 'max_bytes'];
      return '<section class="inbox-set-section inbox-tx-settings" data-inbox-tx-section="1" data-inbox-set="transcription" aria-label="Transcription"' +
        ' data-inbox-tx-unready="' + escapeHtml(unready.join(',')) + '" data-inbox-tx-resolved="' + escapeHtml(tx.resolved_provider || '') + '">' +
        '<h4 class="section-label inbox-set-title">Transcription</h4>' +
        '<p class="muted-xs">Recordings that arrive without text can be transcribed by this hive. Each hive transcribes on its own: with whisper on its own CPU, or with its own Gemini or Deepgram account. Nothing is shared with other hives or paid for centrally, and the transcript is stored only on this hive.</p>' +
        inboxTxProviderHtml(tx) +
        '<div class="inbox-set-consent" role="group" aria-label="Consent and enable">' +
        '<div class="inbox-tx-notice" role="note"><strong>Before you enable this:</strong> recordings contain other people\'s voices. <span data-inbox-tx-consent>' + escapeHtml(inboxTxConsentText(tx.provider || 'auto', tx.resolved_provider)) + '</span> Make sure the people you record are comfortable with that.</div>' +
        '<label class="inbox-toggle"><input type="checkbox" data-inbox-tx-field="consent_acknowledged"' + (tx.consent_acknowledged ? ' checked' : '') + '><span>I understand where the audio goes with the provider above</span></label>' +
        '<label class="inbox-toggle"><input type="checkbox" data-inbox-tx-field="enabled"' + (tx.enabled ? ' checked' : '') + '><span>Enable transcription</span></label></div>' +
        '<div class="inbox-set-subhead"><span class="inbox-field-label">Options</span></div>' +
        '<p class="muted-xs">Audio this hive holds itself (dashboard recordings, dropped audio and video files) is transcribed when it arrives. Mode decides what happens to recordings from synced sources.</p>' +
        '<div class="inbox-settings-grid">' +
        '<label class="inbox-field"><span class="inbox-field-label">Mode</span><select class="kb-search-box" data-inbox-tx-field="mode" data-change-action="inboxTxFieldsChanged" data-arg-types="t">' +
        '<option value="manual"' + (mode === 'manual' ? ' selected' : '') + '>manual: synced recordings wait for a click on Transcribe</option>' +
        '<option value="auto"' + (mode === 'auto' ? ' selected' : '') + '>auto: also new recordings from the sources listed</option></select></label>' +
        field('sources', 'Auto-mode sources', (tx.sources || []).join(', '), 'plaud', 'text', ' data-inbox-tx-mode="auto"' + (mode === 'auto' ? '' : ' hidden')) +
        field('language', 'Language', tx.language || 'auto', 'auto or en, de, pt-BR') +
        '<label class="inbox-field"' + only('whisper') + '><span class="inbox-field-label">Whisper model</span><select class="kb-search-box" data-inbox-tx-field="whisper_model">' +
        ['', 'tiny.en', 'tiny', 'base.en', 'base', 'small.en', 'small'].map(m => '<option value="' + m + '"' + ((tx.whisper_model || '') === m ? ' selected' : '') + '>' + (m ? m + whisperLabels[m] : 'default: base.en (base for other languages)') + '</option>').join('') + '</select></label>' +
        field('whisper_threads', 'Whisper threads', tx.whisper_threads || '', 'half the CPUs', 'number', only('whisper')) +
        field('gemini_model', 'Gemini model', tx.gemini_model || '', 'gemini-3.5-flash', 'text', only('gemini')) +
        field('api_key_env', 'Deepgram key environment variable', env, 'DEEPGRAM_API_KEY', 'text', only('deepgram')) +
        field('model', 'Deepgram model', tx.model || 'nova-3', 'nova-3', 'text', only('deepgram')) +
        '</div>' +
        '<label class="inbox-toggle"' + only('gemini deepgram') + '><input type="checkbox" data-inbox-tx-field="diarize"' + (tx.diarize !== false ? ' checked' : '') + '><span>Label speakers (Speaker 1, Speaker 2, …; Deepgram and Gemini, whisper has no speaker labels)</span></label>' +
        '<p class="muted-xs inbox-set-status"' + only('deepgram') + '>Deepgram key <code>$' + escapeHtml(env) + '</code>: ' + (tx.key_present ? '<span class="kc-pill" data-status="ok">key present</span>' : '<span class="kc-pill" data-status="error">key missing</span> <span>set it in this hive\'s environment (it is never stored in hive.yaml)</span>') + '</p>' +
        '<div class="inbox-set-subhead"><span class="inbox-field-label">Limits</span></div>' +
        '<div class="inbox-settings-grid">' +
        field('monthly_budget_minutes', 'Monthly budget (minutes)', tx.monthly_budget_minutes || '', '600', 'number') +
        field('daily_max_items', 'Daily limit (recordings)', tx.daily_max_items || '', '20', 'number') +
        field('max_duration_minutes', 'Longest recording (minutes)', tx.max_duration_minutes || '', '120', 'number') +
        field('max_mb', 'Largest upload (MB)', sizeMB, '500', 'number') +
        '</div>' +
        inboxTranscriptionUsageHtml(tx) +
        '<details class="inbox-set-keys"><summary class="muted-xs">Config keys</summary><p class="muted-xs">' + keys.map(k => yamlHint('inbox.transcription.' + k)).join(' ') + '</p></details>' +
        '</section>';
    }
    // inboxTxFieldsChanged follows the Provider and Mode selectors: it shows
    // only the fields of the provider in use (or of the ones auto could
    // pick) and the auto-mode sources, and rewrites the consent notice for
    // the selected provider. Values are left alone.
    function inboxTxFieldsChanged(el) {
      const root = el && el.closest ? el.closest('[data-inbox-tx-section]') : null;
      if (!root) return;
      const val = name => { const e = root.querySelector('[data-inbox-tx-field="' + name + '"]'); return e ? String(e.value || '') : ''; };
      const provider = val('provider') || 'auto';
      const unready = String(root.dataset.inboxTxUnready || '').split(',').filter(Boolean);
      root.querySelectorAll('[data-inbox-tx-for]').forEach(n => {
        n.hidden = !String(n.dataset.inboxTxFor || '').split(' ').some(p => provider === p || (provider === 'auto' && unready.indexOf(p) < 0));
      });
      root.querySelectorAll('[data-inbox-tx-mode]').forEach(n => { n.hidden = val('mode') !== n.dataset.inboxTxMode; });
      const notice = root.querySelector('[data-inbox-tx-consent]');
      if (notice) notice.textContent = inboxTxConsentText(provider, root.dataset.inboxTxResolved || '');
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
        '<p class="text-muted fs-base">Files dropped onto the Inbox: largest file, files per drop, per-hive quota and how long originals are kept (the extracted text always stays). ' + yamlHint('inbox.uploads.max_file_bytes') + ' ' + yamlHint('inbox.uploads.max_files_per_drop') + ' ' + yamlHint('inbox.uploads.quota_bytes') + ' ' + yamlHint('inbox.uploads.document_retention_days') + ' ' + yamlHint('inbox.uploads.media_retention_days') + '</p>' +
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

    // ── Add files to the Inbox: drop anywhere on the section, ＋ Add, or
    // paste with Ctrl/Cmd+V while the Inbox has focus. Owner and read-write
    // only; read sessions see none of it. Each file is sent on its own
    // request (POST /api/inbox/uploads) so its row can show progress and its
    // own error; the hive sniffs the content and refuses anything outside the
    // allowlist, these client checks only spare a pointless upload.
    const INBOX_UPLOAD_LABELS = { queued: 'Waiting', uploading: 'Uploading', done: 'Added', error: 'Not added' };
    function inboxUploadSettings() { return (((_inboxCache || {}).settings) || {}).uploads || {}; }
    function inboxCanAdd() { return inboxViewerCanPlay() && !!inboxUploadSettings().enabled; }
    function inboxFmtBytes(n) {
      const b = Number(n) || 0;
      if (b < 1024) return b + ' B';
      if (b < 1048576) return (b / 1024).toFixed(1) + ' KB';
      return (b / 1048576).toFixed(1) + ' MB';
    }
    // inboxUploadPlan checks a drop against the limits before anything is
    // sent: per file, too many files, too large, empty, or an extension the
    // hive does not read. Files without an extension go to the hive, which
    // decides from the content.
    function inboxUploadPlan(files, limits) {
      const maxFiles = Number(limits.max_files_per_drop) || 0;
      const maxDoc = Number(limits.max_file_bytes) || 0;
      // Audio and video use the recording cap (max_media_bytes).
      const media = Array.isArray(limits.media_extensions) ? limits.media_extensions : [];
      const maxMedia = Number(limits.max_media_bytes) || maxDoc;
      const exts = (Array.isArray(limits.extensions) ? limits.extensions : []).concat(media);
      return Array.from(files || []).map((f, i) => {
        const name = (f && f.name) || 'pasted file';
        const row = { name, size: Number(f && f.size) || 0, file: f, status: 'queued', progress: 0 };
        const dot = name.lastIndexOf('.');
        const ext = dot > 0 ? name.slice(dot).toLowerCase() : '';
        const maxBytes = media.indexOf(ext) >= 0 ? maxMedia : maxDoc;
        if (maxFiles && i >= maxFiles) { row.status = 'error'; row.code = 'too_many_files'; row.error = 'Too many files in one drop: at most ' + maxFiles + ' at a time.'; }
        else if (!row.size) { row.status = 'error'; row.code = 'empty'; row.error = 'The file is empty.'; }
        else if (maxBytes && row.size > maxBytes) { row.status = 'error'; row.code = 'too_large'; row.error = 'Too large: ' + inboxFmtBytes(row.size) + ' (the limit is ' + inboxFmtBytes(maxBytes) + ').'; }
        else if (ext && exts.length && exts.indexOf(ext) < 0) { row.status = 'error'; row.code = 'unsupported_type'; row.error = 'Unsupported type ' + ext + ': the Inbox reads ' + exts.join(' ') + '.'; }
        return row;
      });
    }
    function inboxUploadRowHtml(row) {
      const pct = Math.max(0, Math.min(100, Math.round((Number(row.progress) || 0) * 100)));
      const pill = row.status === 'done' ? 'ok' : (row.status === 'error' ? 'error' : 'syncing');
      const label = row.link && row.status === 'uploading' ? 'Fetching' : (INBOX_UPLOAD_LABELS[row.status] || row.status) + (row.status === 'uploading' ? ' ' + pct + '%' : '');
      let html = '<li class="inbox-upload" data-inbox-upload="' + escapeHtml(String(row.id)) + '" data-status="' + escapeHtml(row.status) + '">' +
        '<span class="inbox-upload-name">' + escapeHtml(row.name) + '</span> <span class="muted-xs">' + escapeHtml(row.link ? 'link' : inboxFmtBytes(row.size)) + '</span> ' +
        '<span class="kc-pill" data-status="' + pill + '">' + escapeHtml(label) + '</span>';
      if (!row.link && (row.status === 'uploading' || row.status === 'queued')) html += '<progress class="kc-progress-bar inbox-upload-bar" max="100" value="' + pct + '" aria-label="' + escapeHtml('Uploading ' + row.name) + '"></progress>';
      if (row.status === 'done' && row.key) html += ' <a href="#" class="inbox-link" data-action="inboxFocusItem" data-prevent="1" data-arg0="' + escapeHtml(row.key) + '">show item</a>';
      if (row.status === 'error' && row.error) html += ' <span class="kc-error muted-xs" role="alert">' + escapeHtml(row.error) + '</span>';
      return html + '</li>';
    }
    function inboxUploadsHtml() {
      if (!_inboxUploads.length) return '';
      const finished = _inboxUploads.some(r => r.status === 'done' || r.status === 'error');
      return '<div class="inbox-uploads"><div class="row-between"><span class="muted-xs">Added files</span>' +
        (finished ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxClearUploads">Clear finished</button>' : '') +
        '</div><ul class="inbox-upload-list" aria-label="Files being added">' + _inboxUploads.map(inboxUploadRowHtml).join('') + '</ul></div>';
    }
    function inboxAddPanelHtml() {
      if (!_inboxAddOpen) return '';
      const u = inboxUploadSettings();
      const exts = (Array.isArray(u.extensions) ? u.extensions : []).concat(Array.isArray(u.media_extensions) ? u.media_extensions : []);
      const limit = (u.max_files_per_drop ? 'up to ' + u.max_files_per_drop + ' files' : 'several files') + (u.max_file_bytes ? ' of ' + inboxFmtBytes(u.max_file_bytes) + ' each' : '') + (u.max_media_bytes ? ' (audio and video up to ' + inboxFmtBytes(u.max_media_bytes) + ', transcribed when transcription is on)' : '');
      return '<div class="inbox-add" id="inbox-add" role="group" aria-label="Add files, links and notes to the Inbox">' +
        '<p class="muted-xs m-0">Drop files anywhere on the Inbox, paste them with Ctrl/Cmd+V, or choose them here: ' + escapeHtml(limit) + '. Supported: ' + escapeHtml(exts.join(' ')) + '. Files stay on this hive; their text reaches an LLM only when you share the item.</p>' +
        '<div class="kc-actions mt-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxChooseFiles">Choose files…</button>' +
        '<input type="file" multiple hidden class="inbox-file-input" id="inbox-file-input" accept="' + escapeHtml(exts.join(',')) + '" data-change-action="inboxPickFiles" data-arg-types="t" aria-label="Files to add to the Inbox"></div>' +
        (u.links ? '<div class="kc-actions mt-2"><input type="url" class="kb-search-box inbox-add-url" id="inbox-add-url" placeholder="https://… (a page, or a linked md, json, csv… file)" autocomplete="off" aria-label="Link to add to the Inbox" data-enter-action="inboxAddUrlFromField">' +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxAddUrlFromField">Add link</button></div>' +
          '<p class="muted-xs m-0">The hive fetches the link itself (public http/https addresses only, at most 3 redirects) and reads it like a dropped file.</p>' : '') +
        (typeof inboxNoteHtml === 'function' ? inboxNoteHtml() : '') + '</div>';
    }
    function inboxToggleAdd() { _inboxAddOpen = !_inboxAddOpen; renderInbox(); if (_inboxAddOpen) { const b = document.querySelector('#inbox-add [data-action="inboxChooseFiles"]'); if (b && b.focus) b.focus(); } }
    function inboxChooseFiles() { const el = document.getElementById('inbox-file-input'); if (el) el.click(); }
    function inboxPickFiles(el) { const files = Array.from((el && el.files) || []); if (el) el.value = ''; inboxAddFiles(files); }
    function inboxClearUploads() { _inboxUploads = _inboxUploads.filter(r => r.status !== 'done' && r.status !== 'error'); renderInbox(); }
    function inboxFocusItem(key) { _inboxFocusKey = key; renderInbox(); }
    function inboxAnnounce(msg) { const el = document.getElementById('inbox-upload-status'); if (el) el.textContent = msg; }
    // inboxUploadRowUpdate redraws one row in place (progress events must
    // not rebuild the whole Inbox); without the row it re-renders.
    function inboxUploadRowUpdate(row) {
      const el = document.querySelector('[data-inbox-upload="' + row.id + '"]');
      if (el) el.outerHTML = inboxUploadRowHtml(row); else renderInbox();
    }
    // inboxAddFiles plans a drop, shows a row per file and sends the
    // accepted ones one after another; the Inbox is re-read once they finish.
    function inboxAddFiles(files) {
      if (!inboxCanAdd()) return Promise.resolve([]);
      const rows = inboxUploadPlan(files, inboxUploadSettings());
      if (!rows.length) return Promise.resolve([]);
      for (const r of rows) r.id = ++_inboxUploadSeq;
      _inboxUploads = _inboxUploads.concat(rows);
      renderInbox();
      const send = rows.filter(r => r.status === 'queued');
      const refused = rows.length - send.length;
      inboxAnnounce('Adding ' + send.length + ' file' + (send.length === 1 ? '' : 's') + ' to the Inbox' + (refused ? '; ' + refused + ' not added' : ''));
      _inboxUploadQueue = _inboxUploadQueue.then(async () => {
        for (const r of send) await inboxUploadSend(r);
        const ok = send.filter(r => r.status === 'done').length;
        inboxAnnounce(ok + ' of ' + rows.length + ' file' + (rows.length === 1 ? '' : 's') + ' added to the Inbox');
        if (ok) await fetchInbox();
        return rows;
      });
      return _inboxUploadQueue;
    }
    function inboxNewXHR() { return new XMLHttpRequest(); }
    function inboxUploadForm(row) { const fd = new FormData(); fd.append('file', row.file, row.name); return fd; }
    // inboxUploadSend posts one file with upload progress; it always resolves.
    function inboxUploadSend(row) {
      return new Promise(resolve => {
        const xhr = inboxNewXHR();
        const finish = () => { inboxUploadRowUpdate(row); resolve(row); };
        xhr.open('POST', '/api/inbox/uploads');
        if (xhr.upload) xhr.upload.onprogress = e => { if (e && e.lengthComputable && e.total) { row.progress = e.loaded / e.total; inboxUploadRowUpdate(row); } };
        xhr.onload = () => {
          let data = {};
          try { data = JSON.parse(xhr.responseText || '{}'); } catch (e) { data = {}; }
          const res = (Array.isArray(data.results) ? data.results : [])[0];
          if (xhr.status >= 200 && xhr.status < 300 && res && res.ok) {
            row.status = 'done'; row.progress = 1; row.key = res.source + '/' + res.item_id; row.title = res.title || row.name;
          } else {
            row.status = 'error'; row.code = (res && res.code) || ''; row.error = (res && res.error) || data.error || ('HTTP ' + xhr.status);
          }
          finish();
        };
        xhr.onerror = () => { row.status = 'error'; row.error = 'The upload did not reach the hive (network error).'; finish(); };
        row.status = 'uploading';
        inboxUploadRowUpdate(row);
        xhr.send(inboxUploadForm(row));
      });
    }
    // Drop zone: the whole Inbox section. Only drags that carry files are
    // taken; the overlay shows while one is over the section.
    function inboxDragHasFiles(e) { const t = e && e.dataTransfer && e.dataTransfer.types; return !!t && Array.from(t).indexOf('Files') >= 0; }
    function inboxInSection(node) { return !!(node && node.closest && node.closest('#inbox-section')); }
    function inboxSetDragOver(on) { const sec = document.getElementById('inbox-section'); if (sec) sec.classList.toggle('inbox-dragover', !!on); }
    function inboxOnDragOver(e) {
      if (!inboxCanAdd() || !inboxDragHasFiles(e) || !inboxInSection(e.target)) return;
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
      inboxSetDragOver(true);
    }
    function inboxOnDragLeave(e) { if (inboxInSection(e.target) && !inboxInSection(e.relatedTarget)) inboxSetDragOver(false); }
    function inboxOnDrop(e) {
      inboxSetDragOver(false);
      if (!inboxCanAdd() || !inboxDragHasFiles(e) || !inboxInSection(e.target)) return;
      e.preventDefault();
      inboxAddFiles(Array.from((e.dataTransfer && e.dataTransfer.files) || []));
    }
    // Paste: files on the clipboard while the Inbox has focus (or was the
    // last thing clicked) are added like a drop.
    function inboxOnPaste(e) {
      if (!inboxCanAdd()) return;
      if (!(inboxInSection(e.target) || inboxInSection(document.activeElement) || _inboxPointerInside)) return;
      const files = Array.from((e.clipboardData && e.clipboardData.files) || []);
      if (files.length) { e.preventDefault(); inboxAddFiles(files); return; }
      // A pasted link (one http/https URL) outside a text field is added
      // like a dropped one; pasting into a field stays a normal paste.
      const text = String((e.clipboardData && e.clipboardData.getData && e.clipboardData.getData('text')) || '').trim();
      const field = e.target && e.target.closest && e.target.closest('input, textarea, [contenteditable="true"]');
      if (field) return;
      // Text of several lines goes to the Quick note box for review.
      if (typeof inboxPasteAsNote === 'function' && inboxPasteAsNote(text)) { e.preventDefault(); return; }
      if (!inboxUploadSettings().links || !INBOX_URL_RE.test(text)) return;
      e.preventDefault();
      inboxAddUrl(text);
    }
    const INBOX_URL_RE = /^https?:\/\/[^\s]+$/i;
    function inboxAddUrlFromField() {
      const el = document.getElementById('inbox-add-url');
      const raw = el ? String(el.value || '').trim() : '';
      if (!raw) return Promise.resolve(null);
      if (el) el.value = '';
      return inboxAddUrl(raw);
    }
    // inboxAddUrl shows a row for one link and asks the hive to fetch it
    // (POST /api/inbox/uploads/url); the hive applies the SSRF rules.
    function inboxAddUrl(raw) {
      if (!inboxCanAdd() || !inboxUploadSettings().links) return Promise.resolve(null);
      let host = raw;
      try { host = new URL(raw).host || raw; } catch (e) { host = raw; }
      const row = { id: ++_inboxUploadSeq, name: host, size: 0, link: raw, status: 'queued', progress: 0 };
      if (!INBOX_URL_RE.test(raw)) { row.status = 'error'; row.code = 'link_refused'; row.error = 'Only http and https links can be added.'; }
      _inboxUploads = _inboxUploads.concat([row]);
      renderInbox();
      if (row.status === 'error') return Promise.resolve(row);
      inboxAnnounce('Fetching ' + host + ' for the Inbox');
      _inboxUploadQueue = _inboxUploadQueue.then(async () => {
        row.status = 'uploading';
        inboxUploadRowUpdate(row);
        try {
          const r = await fetch('/api/inbox/uploads/url', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ url: raw }) });
          const data = await r.json().catch(() => ({}));
          const res = (Array.isArray(data.results) ? data.results : [])[0];
          if (r.ok && res && res.ok) { row.status = 'done'; row.key = res.source + '/' + res.item_id; row.title = res.title || host; }
          else { row.status = 'error'; row.code = (res && res.code) || ''; row.error = (res && res.error) || data.error || ('HTTP ' + r.status); }
        } catch (e) {
          row.status = 'error'; row.error = 'The request did not reach the hive (network error).';
        }
        inboxUploadRowUpdate(row);
        inboxAnnounce(row.status === 'done' ? host + ' added to the Inbox' : host + ' not added: ' + row.error);
        if (row.status === 'done') await fetchInbox();
        return row;
      });
      return _inboxUploadQueue;
    }
    document.addEventListener('dragenter', inboxOnDragOver);
    document.addEventListener('dragover', inboxOnDragOver);
    document.addEventListener('dragleave', inboxOnDragLeave);
    document.addEventListener('drop', inboxOnDrop);
    document.addEventListener('paste', inboxOnPaste);
    document.addEventListener('pointerdown', e => { _inboxPointerInside = inboxInSection(e.target); }, true);
