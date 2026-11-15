    // ── Inbox: source-agnostic triage of voice recordings and dictations ──
    // Items come from knowledge connectors (plaud, wisprflow, …); each one is
    // already a Knowledge fact. Routing them to spek (RFC + child issues) or
    // dibs happens inside the hive via POST /api/inbox/route.
    const INBOX_POLL_MS = 60000;
    const INBOX_BUSY_POLL_MS = 5000;
    const INBOX_SOURCE_ICONS = { plaud: '🎙️', wisprflow: '🗣️', zoom: '📹', teams: '👥', discord: '🎧', 'google-meet': '🎥', webex: '🎬', 'file-drop': '📎', webhook: '🔗' };
    const INBOX_DEST_LABELS = { spek_rfc: 'RFC', dibs: 'dibs' };
    // Summaries longer than this many characters start collapsed.
    const INBOX_SUMMARY_COLLAPSE_CHARS = 280;
    let _inboxCache = null;
    let _inboxSelected = new Set();
    let _inboxShowProcessed = false;
    let _inboxBusyTimer = null;
    // Per-item view state: expanded summaries, open transcript disclosures
    // and fetched content (summary/transcript/highlights). Audio links are
    // never kept here: a minted link lives only in the <audio> element.
    let _inboxExpanded = new Set();
    let _inboxTranscriptOpen = new Set();
    let _inboxContent = {};
    // Items whose Transcribe request is in flight (button spinner).
    let _inboxTranscribing = new Set();
    // Bytes per MB for the transcription size limit field.
    const INBOX_TX_BYTES_PER_MB = 1048576;
    const INBOX_TX_STATUS_LABELS = { queued: 'Transcription queued', running: 'Transcribing', done: 'Machine transcript', failed: 'Transcription failed', skipped: 'Not transcribed' };

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
    function inboxRouteBusy(items) {
      return (items || []).some(it => Object.values(it.routes || {}).some(r => r && (r.status === 'queued' || r.status === 'running')) ||
        (it.transcription && (it.transcription.status === 'queued' || it.transcription.status === 'running')));
    }

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
        const tx = it.transcribable ? ' Or click 🎧 Transcribe to have this hive transcribe it with Deepgram.' : '';
        // Meeting platforms whose pending items read "<label> has no transcript"
        // (inline so the extracted function stays self-contained in tests).
        const meetingLabel = { zoom: 'Zoom', teams: 'Teams', 'google-meet': 'Google Meet', webex: 'Webex' }[it.source];
        if (meetingLabel) return '<div class="inbox-pending muted-xs" title="The platform reports no transcript or summary for this recording yet">Not transcribed yet: ' + meetingLabel + ' has no transcript or summary for this recording (cloud transcription may still be processing or is disabled); the hive re-checks it on every sync.' + tx + '</div>';
        if (it.source === 'discord') return '<div class="inbox-pending muted-xs" title="Discord has no transcripts; none was posted with this recording">Not transcribed yet: Discord has no built-in transcription and no transcript file was posted with this recording; play it here, or post a .vtt/.txt transcript in the same message. The hive re-checks it on every sync.' + tx + '</div>';
        return '<div class="inbox-pending muted-xs" title="The MCP reports no transcript or summary block for this recording">Not transcribed yet in Plaud: open the recording in the Plaud app and run transcription; the hive re-checks it on every sync.' + tx + '</div>';
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
      else html += '<div class="muted-xs">No transcript yet' + (c.pending ? (it.source === 'plaud' ? ': Plaud has not transcribed this recording.' : ': the meeting platform has not produced a transcript for this recording.') : '.') + '</div>';
      if (c.highlights) html += '<div class="muted-xs inbox-highlights"><strong>Highlights</strong> (flagged on the device)<pre class="inbox-transcript-text">' + escapeHtml(c.highlights) + '</pre></div>';
      return html + '</div>';
    }

    // Machine transcription: a state pill (spinner while queued/running, the
    // reason when it failed or was skipped) and the 🎧 Transcribe button on
    // pending items with audio, for owners and read-write sessions.
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
        if (st.reason && (st.status === 'failed' || st.status === 'skipped')) html += ' <span class="muted-xs inbox-tx-reason">' + escapeHtml(st.reason) + '</span>';
      }
      if (it.transcribable && inboxViewerCanPlay()) {
        const inflight = _inboxTranscribing.has(key);
        const retry = st && (st.status === 'failed' || st.status === 'skipped');
        html += '<button type="button" class="hv-btn btn-secondary btn-sm inbox-transcribe" data-action="inboxTranscribe" data-arg0="' + src + '" data-arg1="' + id + '"' + (inflight ? ' disabled aria-busy="true"' : '') +
          ' title="Send this recording to Deepgram under the hive owner\'s own key; the transcript is kept on this hive only">' +
          (inflight ? '<span class="inbox-tx-spinner" aria-hidden="true"></span> Sending…' : (retry ? '🎧 Retry transcription' : '🎧 Transcribe')) + '</button>';
      }
      return html ? '<div class="inbox-tx">' + html + '</div>' : '';
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
      const meta = [escapeHtml(it.source), when ? 'recorded ' + when : '', dur].filter(Boolean).join(' · ');
      const routes = it.routes || {};
      let pills = '';
      for (const d of Object.keys(routes)) pills += inboxStatePill(d, routes[d]) + ' ';
      if (it.dismissed) pills += '<span class="kc-pill" title="Kept as a Knowledge fact only">knowledge only</span>';
      const owner = viewerIsOwner();
      const src = escapeHtml(it.source), id = escapeHtml(it.id);
      let actions = '';
      if (inboxViewerCanPlay()) actions += '<button type="button" class="hv-btn btn-secondary btn-sm inbox-play" data-action="inboxPlay" data-arg0="' + src + '" data-arg1="' + id + '" title="Fetch a fresh 24h audio link and play it here">▶ Play recording</button>';
      actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxToggleTranscript" data-arg0="' + src + '" data-arg1="' + id + '" aria-expanded="' + (_inboxTranscriptOpen.has(key) ? 'true' : 'false') + '">' + (_inboxTranscriptOpen.has(key) ? '▾' : '▸') + ' Transcript</button>';
      if (owner) {
        const retry = (d) => routes[d] && routes[d].status === 'failed' ? 'Retry ' : '→ ';
        actions += '<button type="button" class="hv-btn btn-primary btn-sm" data-action="inboxOpenRoute" data-arg0="' + src + '" data-arg1="' + id + '" data-arg2="spek_rfc">' + retry('spek_rfc') + 'RFC…</button>';
        actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxOpenRoute" data-arg0="' + src + '" data-arg1="' + id + '" data-arg2="dibs">' + retry('dibs') + 'dibs…</button>';
        if (!it.dismissed) actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxDismiss" data-arg0="' + src + '" data-arg1="' + id + '" title="Keep it as a Knowledge fact; no RFC, no dibs idea">Knowledge only</button>';
        else actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxUndismiss" data-arg0="' + src + '" data-arg1="' + id + '">Back to inbox</button>';
      }
      if (it.fact_slug) actions += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxOpenFact" data-arg0="' + escapeHtml(it.fact_slug) + '" data-arg1="' + escapeHtml(it.layer || '') + '" title="This item is already a Knowledge fact">📚 Fact</button>';
      return '<div class="kb-fact-item inbox-item" data-inbox-key="' + escapeHtml(key) + '">' +
        (owner ? '<label class="inbox-check"><input type="checkbox" data-change-action="inboxToggleSelect" data-arg0="' + src + '" data-arg1="' + id + '"' + checked + ' aria-label="Select ' + escapeHtml(it.title) + '"></label>' : '') +
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

    // ▶ Play recording: mint a fresh presigned link through the hive and play
    // it inline. The link is only ever placed on the <audio> element; the
    // hive stores nothing and the next click mints a new one.
    async function inboxPlay(src, id) {
      const key = src + '/' + id;
      const mount = document.querySelector('[data-inbox-media="' + CSS.escape(key) + '"]');
      if (mount) mount.innerHTML = '<div class="muted-xs">Fetching audio link…</div>';
      try {
        const r = await fetch(inboxApiPath(src, id, 'audio'), { cache: 'no-store' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        if (!data.url) throw new Error('no audio link returned');
        const target = document.querySelector('[data-inbox-media="' + CSS.escape(key) + '"]');
        if (!target) return;
        target.innerHTML = '';
        const audio = document.createElement('audio');
        audio.controls = true;
        audio.autoplay = true;
        audio.preload = 'auto';
        audio.className = 'inbox-audio';
        audio.src = data.url;
        target.appendChild(audio);
        const note = document.createElement('div');
        note.className = 'muted-xs';
        note.textContent = 'Fresh link' + (data.expires_at ? ', valid until ' + formatDashboardDateTime(data.expires_at) : '') + '. Click ▶ Play recording again for a new one.';
        target.appendChild(note);
      } catch (e) {
        if (mount) mount.innerHTML = '';
        showToast('Audio unavailable: ' + e.message, 'error');
      }
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
      if (!settings.spek_ready && owner) html += '<p class="muted-xs">RFC filing needs a <code>spektacular</code> binary on the hive host (<code>inbox.spek.binary</code>) or at least one enabled agent to hand the job to (<code>inbox.spek.agent</code> overrides the default).</p>';
      const shown = items.filter(it => _inboxShowProcessed || !it.processed);
      if (owner && shown.length) {
        const sel = _inboxSelected.size;
        html += '<div class="kc-actions mb-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxSelectAll">' + (sel === shown.length ? 'Clear selection' : 'Select all') + '</button>' +
          (sel ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxDismissSelected">Knowledge only (' + sel + ')</button>' : '') + '</div>';
      }
      if (!shown.length) html += '<div class="muted-note">' + (items.length ? 'Everything is triaged. Toggle “show processed” to see filed and dismissed items.' : 'Nothing yet. Connect Plaud or another voice source under Knowledge → Integrations; new recordings land here after the next sync (a newly added connector starts on the next hive restart).') + '</div>';
      else html += '<div class="kb-fact-list inbox-list">' + shown.map(inboxItemHtml).join('') + '</div>';
      inboxPreserveAudio(panel, () => { panel.innerHTML = html; });
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
      try { await inboxPostDismiss([{ source: src, item_id: id }], false); showToast('Kept as a Knowledge fact only', 'success'); await fetchInbox(); }
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
      try { await inboxPostDismiss(items, false); _inboxSelected.clear(); await fetchInbox(); }
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
        body += '<p class="muted-note">A hive-run <code>spektacular voice</code> turns the transcript into a spec, then files one RFC issue plus child issues on the chosen repository. The children are picked up by the hive like any other issue.</p>' +
          '<label class="muted-note inbox-field">Repository<select class="field-input field-input-block mt-2" id="inbox-route-repo">' + opts + '</select></label>' +
          (repos.length ? '' : '<p class="kc-error muted-xs">No repositories configured on this hive.</p>');
      } else {
        body += '<p class="muted-note">Posts the title, summary and transcript to dibs as a private draft idea' + (settings.dibs_url ? ' at <code>' + escapeHtml(settings.dibs_url) + '</code>' : '') + '. Provenance links back to <code>' + escapeHtml(src) + '://recordings/' + escapeHtml(id) + '</code>.</p>' +
          (!settings.dibs_url ? '<p class="kc-error muted-xs">dibs is not configured — set the URL under ⚙ Auto-routing and <code>inbox.dibs.token_env</code> in the hive config.</p>' : !settings.dibs_token_set ? '<p class="kc-error muted-xs">dibs token missing — set the environment variable named by <code>inbox.dibs.token_env</code>.</p>' : '');
      }
      body += '<div class="hive-dialog-error" role="alert" id="inbox-route-error"></div>' +
        '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-action="kbCloseModal">Cancel</button>' +
        '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="inboxConfirmRoute" data-arg0="' + escapeHtml(src) + '" data-arg1="' + escapeHtml(id) + '" data-arg2="' + escapeHtml(dest) + '">' + (dest === 'spek_rfc' ? 'File RFC' : 'Send to dibs') + '</button></div>';
      kbShowModal(dest === 'spek_rfc' ? '→ RFC (spektacular)' : '→ dibs', body, { width: '520px' });
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
        showToast((INBOX_DEST_LABELS[dest] || dest) + ' queued — the pill updates when it finishes.', 'success');
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

    // Transcription section: spoke-local Deepgram transcription with the
    // owner's own key. The key value is never shown, only whether the named
    // variable is set on this hive.
    function inboxTranscriptionFormHtml(tx) {
      tx = tx || {};
      const field = (name, label, value, placeholder, type) => '<label class="muted-note inbox-field">' + label +
        '<input class="kb-search-box mt-2" type="' + (type || 'text') + '" data-inbox-tx-field="' + name + '" autocomplete="off" placeholder="' + escapeHtml(placeholder || '') + '" value="' + escapeHtml(value == null ? '' : String(value)) + '"></label>';
      const mode = tx.mode === 'auto' ? 'auto' : 'manual';
      const env = tx.api_key_env || 'DEEPGRAM_API_KEY';
      const sizeMB = tx.max_bytes ? Math.round(Number(tx.max_bytes) / INBOX_TX_BYTES_PER_MB) : '';
      return '<div class="inbox-tx-settings" data-inbox-tx-section="1">' +
        '<div class="section-label mt-4 mb-2">Transcription</div>' +
        '<p class="muted-xs">Recordings that arrive without text can be transcribed by Deepgram on this hive. Each hive uses its owner\'s own Deepgram account: nothing is shared with other hives or paid for centrally, and the transcript is stored only on this hive. ' + yamlHint('inbox.transcription.enabled') + '</p>' +
        '<div class="inbox-tx-notice" role="note"><strong>Before you enable this:</strong> recordings contain other people\'s voices. When a recording is transcribed, this hive sends its audio to Deepgram under the hive owner\'s Deepgram account and key, with Deepgram\'s model-training opt-out set. Make sure the people you record are comfortable with that.</div>' +
        '<label class="muted-note inbox-toggle mt-2"><input type="checkbox" data-inbox-tx-field="consent_acknowledged"' + (tx.consent_acknowledged ? ' checked' : '') + '> I understand that audio is sent to Deepgram under my own account ' + yamlHint('inbox.transcription.consent_acknowledged') + '</label>' +
        '<label class="muted-note inbox-toggle mt-2"><input type="checkbox" data-inbox-tx-field="enabled"' + (tx.enabled ? ' checked' : '') + '> Enable transcription</label>' +
        '<div class="inbox-settings-grid">' +
        '<label class="muted-note inbox-field">Mode<select class="field-input field-input-block mt-2" data-inbox-tx-field="mode">' +
        '<option value="manual"' + (mode === 'manual' ? ' selected' : '') + '>manual: only when someone clicks Transcribe</option>' +
        '<option value="auto"' + (mode === 'auto' ? ' selected' : '') + '>auto: also new recordings from the sources below</option></select></label>' +
        field('sources', 'Auto-mode sources', (tx.sources || []).join(', '), 'plaud') +
        field('api_key_env', 'API key environment variable', env, 'DEEPGRAM_API_KEY') +
        field('model', 'Model', tx.model || 'nova-3', 'nova-3') +
        field('language', 'Language', tx.language || 'auto', 'auto or en, de, pt-BR') +
        field('monthly_budget_minutes', 'Monthly budget (minutes)', tx.monthly_budget_minutes || '', '600', 'number') +
        field('daily_max_items', 'Daily limit (recordings)', tx.daily_max_items || '', '20', 'number') +
        field('max_duration_minutes', 'Longest recording (minutes)', tx.max_duration_minutes || '', '120', 'number') +
        field('max_mb', 'Largest upload (MB)', sizeMB, '500', 'number') +
        '</div>' +
        '<label class="muted-note inbox-toggle mt-2"><input type="checkbox" data-inbox-tx-field="diarize"' + (tx.diarize !== false ? ' checked' : '') + '> Label speakers (Speaker 1, Speaker 2, …)</label>' +
        '<p class="muted-xs">Key <code>$' + escapeHtml(env) + '</code>: ' + (tx.key_present ? '<span class="kc-pill" data-status="ok">key present</span>' : '<span class="kc-pill" data-status="error">key missing</span> set it in this hive\'s environment (it is never stored in hive.yaml)') + '. ' +
        yamlHint('inbox.transcription.mode') + ' ' + yamlHint('inbox.transcription.sources[]') + ' ' + yamlHint('inbox.transcription.api_key_env') + ' ' + yamlHint('inbox.transcription.provider') + ' ' + yamlHint('inbox.transcription.model') + ' ' + yamlHint('inbox.transcription.diarize') + ' ' + yamlHint('inbox.transcription.language') + ' ' +
        yamlHint('inbox.transcription.max_duration_minutes') + ' ' + yamlHint('inbox.transcription.max_bytes') + ' ' + yamlHint('inbox.transcription.monthly_budget_minutes') + ' ' + yamlHint('inbox.transcription.daily_max_items') + '</p></div>';
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
        provider: (current && current.provider) || 'deepgram',
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
      if (tx.enabled && !tx.consent_acknowledged) return { error: 'Tick the consent box to enable transcription: recordings contain other people\'s voices and the audio is sent to Deepgram under your account.' };
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
        '<p class="text-muted fs-base">Default RFC repository, dibs URL, auto-routing rules and Deepgram transcription for voice recordings. ' + yamlHint('inbox.spek.repo') + ' ' + yamlHint('inbox.dibs.url') + ' ' + yamlHint('inbox.rules') + ' ' + yamlHint('inbox.transcription') + '</p>' +
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
