    // ── Meetings: hive as a live meeting participant (Quorum, #18284) ──
    // Owner-only. The list comes from GET /api/meetings; an opened meeting
    // streams its transcript, Spek intents and research over
    // GET /api/meetings/{id}/ws. /api/events "meeting" frames refresh the list.
    const MEETINGS_POLL_MS = 60000;
    // Transcript lines kept in the open panel.
    const MEETINGS_MAX_LINES = 500;
    // Intent lines kept in the open panel.
    const MEETINGS_MAX_INTENTS = 100;
    const MEETINGS_LIVE_STATES = { requested: 1, joining: 1, lobby: 1, live: 1 };
    let _meetingsList = null;
    let _meetingsError = '';
    let _meetingsOpen = null; // { id, ws, meeting, lines, partial, intents, live }
    // Ended meetings are inbox items too (source "meeting").
    if (typeof INBOX_SOURCE_ICONS === 'object' && !INBOX_SOURCE_ICONS.meeting) INBOX_SOURCE_ICONS.meeting = '🎙';

    function meetingsIsOwner() { return window._hiveRole === 'owner'; }
    function meetingsLabel(m) {
      if (m.title) return m.title;
      if (m.platform === 'discord') return 'Discord ' + (m.channel_id || '');
      return m.url || m.platform || m.id;
    }

    async function fetchMeetings() {
      const host = document.getElementById('meetings-panel');
      if (!host) return;
      if (!meetingsIsOwner()) {
        host.innerHTML = '<p class="muted-xs">Meetings are visible to the hive owner only.</p>';
        return;
      }
      try {
        const r = await fetch('/api/meetings');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) { _meetingsList = []; _meetingsError = data.error || ('HTTP ' + r.status); }
        else { _meetingsList = data.meetings || []; _meetingsError = ''; }
      } catch (e) { return; }
      renderMeetings();
    }
    dashboardScheduleWork('meetings', fetchMeetings).catch(console.error);
    dashboardSetInterval('meetings', fetchMeetings, MEETINGS_POLL_MS);

    function meetingsJoinFormHtml() {
      return '<div class="meetings-join">' +
        '<label>Meeting link (Google Meet, Teams, Zoom)<input class="kb-search-box" type="url" data-meetings-field="url" placeholder="https://meet.google.com/abc-defg-hij" autocomplete="off"></label>' +
        '<label>Title (optional)<input class="kb-search-box" type="text" data-meetings-field="title" placeholder="Weekly sync" autocomplete="off"></label>' +
        '<label class="inbox-toggle"><input type="checkbox" data-meetings-field="voice"> Voice</label>' +
        '<button type="button" class="hv-btn btn-primary btn-sm" data-action="meetingsJoin">Send hive to a meeting</button></div>' +
        '<details class="mt-2"><summary class="muted-xs">Discord voice channel instead</summary><div class="meetings-join-discord mt-2">' +
        '<label>Guild ID<input class="kb-search-box" type="text" data-meetings-field="guild" autocomplete="off"></label>' +
        '<label>Voice channel ID<input class="kb-search-box" type="text" data-meetings-field="channel" autocomplete="off"></label></div></details>' +
        '<div class="hive-dialog-error" role="alert" data-meetings-field="error"></div>';
    }

    function meetingsRowHtml(m) {
      const active = !!MEETINGS_LIVE_STATES[m.state];
      const open = _meetingsOpen && _meetingsOpen.id === m.id;
      const request = m.state === 'requested' && m.requested_by;
      if (request) {
        return '<div class="meetings-row"><span class="badge">join request</span>' +
          '<span class="meetings-row-title">' + escapeHtml((m.requested_by.name || m.requested_by.id || 'someone') + ' asked hive into ' + meetingsLabel(m)) + '</span>' +
          '<button type="button" class="hv-btn btn-primary btn-sm" data-action="meetingsApproveRequest" data-arg0="' + escapeHtml(m.id) + '">Approve</button>' +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsDeclineRequest" data-arg0="' + escapeHtml(m.id) + '">Decline</button></div>';
      }
      return '<div class="meetings-row' + (open ? ' is-open' : '') + '">' +
        '<span class="badge">' + escapeHtml(m.state || '') + '</span>' +
        '<span class="meetings-row-title" title="' + escapeHtml(m.url || '') + '">' + escapeHtml(meetingsLabel(m)) + '</span>' +
        (m.state_reason ? '<span class="muted-xs">' + escapeHtml(m.state_reason) + '</span>' : '') +
        (m.consent_error && !m.consent_posted_at ? '<span class="muted-xs" title="' + escapeHtml(m.consent_error) + '">chat unavailable: consent notice not posted yet</span>' : '') +
        (m.voice_enabled && !m.voice_capable ? '<span class="muted-xs">joined without voice; replies go to chat</span>' : '') +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsOpenPanel" data-arg0="' + escapeHtml(m.id) + '">' + (open ? 'Close' : 'Open') + '</button>' +
        (active ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsToggleVoice" data-arg0="' + escapeHtml(m.id) + '" data-arg1="' + (m.voice_enabled ? 'false' : 'true') + '">' + (m.voice_enabled ? 'Voice on' : 'Voice off') + '</button>' +
          '<button type="button" class="hv-btn btn-danger btn-sm" data-action="meetingsLeave" data-arg0="' + escapeHtml(m.id) + '">Leave</button>' : '') +
        '</div>';
    }

    function renderMeetings() {
      const host = document.getElementById('meetings-panel');
      if (!host) return;
      let html = '';
      if (_meetingsError) {
        html += '<p class="kc-error muted-xs">' + escapeHtml(_meetingsError) + '</p>';
      } else {
        html += meetingsJoinFormHtml();
      }
      const list = _meetingsList || [];
      html += '<div class="meetings-list">' + (list.length ? list.map(meetingsRowHtml).join('') : (_meetingsError ? '' : '<p class="muted-xs">No meetings yet.</p>')) + '</div>';
      html += '<div id="meetings-live-panel"></div>';
      host.innerHTML = html;
      const liveCount = list.filter(m => MEETINGS_LIVE_STATES[m.state]).length;
      if (typeof refreshSectionCardShell === 'function') refreshSectionCardShell('meetings-section');
      if (typeof setSectionSummary === 'function') setSectionSummary('meetings-section', liveCount ? liveCount + ' live' : 'no live meetings');
      renderMeetingsLive();
    }

    function meetingsField(name) { return document.querySelector('[data-meetings-field="' + name + '"]'); }

    async function meetingsPost(path, body) {
      const r = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      return data;
    }

    async function meetingsJoin() {
      const err = meetingsField('error');
      const guild = ((meetingsField('guild') || {}).value || '').trim();
      const channel = ((meetingsField('channel') || {}).value || '').trim();
      const body = { title: ((meetingsField('title') || {}).value || '').trim(), voice: !!(meetingsField('voice') || {}).checked };
      if (guild || channel) { body.platform = 'discord'; body.guild_id = guild; body.channel_id = channel; }
      else body.url = ((meetingsField('url') || {}).value || '').trim();
      try {
        const m = await meetingsPost('/api/meetings', body);
        showToast('Hive is joining the meeting', 'success');
        await fetchMeetings();
        if (m && m.id) meetingsOpenPanel(m.id);
      } catch (e) { if (err) err.textContent = e.message; }
    }

    async function meetingsLeave(id) {
      try { await meetingsPost('/api/meetings/' + encodeURIComponent(id) + '/leave'); showToast('Hive left the meeting', 'success'); }
      catch (e) { showToast('Leave failed: ' + e.message, 'error'); }
      fetchMeetings();
    }

    async function meetingsToggleVoice(id, enabled) {
      try { await meetingsPost('/api/meetings/' + encodeURIComponent(id) + '/voice', { enabled: enabled === 'true' || enabled === true }); }
      catch (e) { showToast('Voice change failed: ' + e.message, 'error'); }
      fetchMeetings();
    }

    async function meetingsApproveRequest(id) {
      try { await meetingsPost('/api/meetings/' + encodeURIComponent(id) + '/approve'); showToast('Hive is joining', 'success'); }
      catch (e) { showToast('Approve failed: ' + e.message, 'error'); }
      fetchMeetings();
    }

    async function meetingsDeclineRequest(id) {
      try { await meetingsPost('/api/meetings/' + encodeURIComponent(id) + '/decline'); }
      catch (e) { showToast('Decline failed: ' + e.message, 'error'); }
      fetchMeetings();
    }

    async function meetingsApprove(id, rid) {
      try { await meetingsPost('/api/meetings/' + encodeURIComponent(id) + '/research/' + encodeURIComponent(rid) + '/approve'); showToast('Research approved', 'success'); }
      catch (e) { showToast('Approve failed: ' + e.message, 'error'); }
    }

    function meetingsClosePanel() {
      if (_meetingsOpen && _meetingsOpen.ws) { try { _meetingsOpen.ws.close(); } catch (e) { /* already closed */ } }
      _meetingsOpen = null;
    }

    function meetingsOpenPanel(id) {
      const wasOpen = _meetingsOpen && _meetingsOpen.id === id;
      meetingsClosePanel();
      if (wasOpen) { renderMeetings(); return; }
      const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
      const ws = new WebSocket(proto + location.host + '/api/meetings/' + encodeURIComponent(id) + '/ws');
      _meetingsOpen = { id: id, ws: ws, meeting: null, lines: [], partial: null, intents: [], live: null };
      ws.onmessage = (e) => { try { meetingsOnFrame(JSON.parse(e.data)); } catch (err) { /* ignore malformed frame */ } };
      ws.onclose = () => { if (_meetingsOpen && _meetingsOpen.ws === ws) _meetingsOpen.ws = null; };
      renderMeetings();
    }

    function meetingsOnFrame(f) {
      const o = _meetingsOpen;
      if (!o || !f || f.meeting_id !== o.id) return;
      switch (f.kind) {
        case 'snapshot':
          o.meeting = (f.data || {}).meeting || null;
          o.lines = ((f.data || {}).transcript || []).slice(-MEETINGS_MAX_LINES);
          o.live = (f.data || {}).live || null;
          break;
        case 'meeting': o.meeting = f.data; break;
        case 'line': {
          const ev = f.data || {};
          if (ev.type === 'segment' && !ev.final) { o.partial = ev; break; }
          o.partial = null;
          o.lines.push(ev);
          if (o.lines.length > MEETINGS_MAX_LINES) o.lines.shift();
          break;
        }
        case 'intent':
          o.intents.push((f.data || {}).intent);
          if (o.intents.length > MEETINGS_MAX_INTENTS) o.intents.shift();
          break;
        case 'research': {
          const it = (f.data || {}).research;
          if (!it) break;
          o.live = o.live || {};
          const list = o.live.research = o.live.research || [];
          const i = list.findIndex(x => x.rid === it.rid);
          if (i >= 0) list[i] = it; else list.push(it);
          break;
        }
        case 'notes': meetingsRefreshLive(o.id); break;
        case 'spek': meetingsRefreshLive(o.id); break;
        default: break;
      }
      renderMeetingsLive();
    }

    async function meetingsRefreshLive(id) {
      try {
        const r = await fetch('/api/meetings/' + encodeURIComponent(id));
        if (!r.ok) return;
        const data = await r.json();
        if (_meetingsOpen && _meetingsOpen.id === id) { _meetingsOpen.live = data.live || null; renderMeetingsLive(); }
      } catch (e) { /* transient */ }
    }

    function meetingsLineHtml(ev, partial) {
      const who = (ev.speaker && ev.speaker.name) || (ev.author && ev.author.name) || '';
      return '<p class="meetings-line' + (partial ? ' is-partial' : '') + '"><span class="meetings-who">' + escapeHtml(who) + (ev.type === 'chat' ? ' (chat)' : '') + '</span>' + escapeHtml(ev.text || '') + '</p>';
    }

    function renderMeetingsLive() {
      const host = document.getElementById('meetings-live-panel');
      if (!host) return;
      const o = _meetingsOpen;
      if (!o) { host.innerHTML = ''; return; }
      const live = o.live || {};
      const notes = live.notes || {};
      const sections = ['summary', 'decisions', 'actions', 'questions', 'notes'];
      const notesHtml = sections.filter(s => (notes[s] || []).length).map(s =>
        '<div><div class="section-label m-0">' + escapeHtml(s) + '</div><ul>' + notes[s].map(n => '<li>' + escapeHtml(n) + '</li>').join('') + '</ul></div>').join('') || '<p class="muted-xs">No notes yet.</p>';
      const research = (live.research || []).map(it =>
        '<div class="meetings-research-item"><div><span class="badge">' + escapeHtml(it.status) + '</span> ' + escapeHtml(it.query) + '</div>' +
        (it.summary ? '<div class="muted-xs">' + escapeHtml(it.summary) + '</div>' : '') +
        (it.status === 'pending_approval' ? '<button type="button" class="hv-btn btn-primary btn-sm mt-2" data-action="meetingsApprove" data-arg0="' + escapeHtml(o.id) + '" data-arg1="' + escapeHtml(it.rid) + '">Approve research</button>' : '') +
        '</div>').join('') || '<p class="muted-xs">No research yet.</p>';
      const lines = o.lines.map(ev => meetingsLineHtml(ev, false)).join('') + (o.partial ? meetingsLineHtml(o.partial, true) : '');
      const intents = o.intents.map(i => '<div>' + escapeHtml(typeof i === 'string' ? i : JSON.stringify(i)) + '</div>').join('');
      const spek = live.spek_status ? '<p class="kc-error muted-xs">' + escapeHtml(live.spek_status) + '</p>' :
        (live.spek_mode ? '<p class="muted-xs">Spektacular: ' + escapeHtml(live.spek_mode) + '</p>' : '');
      host.innerHTML = spek + '<div class="meetings-live">' +
        '<div><div class="section-label m-0">Transcript' + (o.ws ? '' : ' (disconnected)') + '</div><div class="meetings-transcript">' + (lines || '<p class="muted-xs">Waiting for the first words…</p>') + '</div></div>' +
        '<div class="meetings-side"><div><div class="section-label m-0">Notes</div>' + notesHtml + '</div>' +
        '<div><div class="section-label m-0">Research</div><div class="meetings-research">' + research + '</div></div>' +
        '<div><div class="section-label m-0">Spek intents</div><div class="meetings-intents">' + (intents || '<p class="muted-xs">None yet.</p>') + '</div></div></div></div>';
      const tr = host.querySelector('.meetings-transcript');
      if (tr) tr.scrollTop = tr.scrollHeight;
    }

    // Settings → Meetings: enable, Vexa gateway, write-only secrets.
    function renderGovMeetings() {
      return '<div class="config-section" data-config-section="Meetings"><h3>Meetings</h3>' +
        '<p class="text-muted fs-base">Send hive into Google Meet, Teams, Zoom or Discord meetings to take notes, ask clarifying questions and run research. ' +
        yamlHint('meetings.enabled') + ' ' + yamlHint('meetings.vexa.url') + ' ' + yamlHint('meetings.vexa.api_key_file') + ' ' + yamlHint('meetings.discord.token_file') + ' ' + yamlHint('meetings.transcribe_url') + ' ' + yamlHint('meetings.transcribe_token_file') + ' Changes apply after a restart.</p>' +
        '<div id="meetings-settings-inline"><p class="muted-xs">Loading…</p></div></div>';
    }

    function meetingsSettingsFormHtml(s) {
      const eff = s.effective || {};
      const rows = Object.keys(eff).sort().map(k => '<tr><td><code>' + escapeHtml(k) + '</code></td><td>' + escapeHtml(eff[k]) + '</td></tr>').join('');
      return '<div class="meetings-settings-form">' +
        '<label class="inbox-toggle"><input type="checkbox" data-meetings-setting="enabled"' + (s.enabled ? ' checked' : '') + '> Meetings enabled' + (s.enabled_env_override ? ' (set by HIVE_MEETINGS_ENABLED; the toggle has no effect)' : '') + '</label>' +
        '<p class="muted-xs">' + (s.running ? 'Running on this hive.' : 'Not running on this hive.') + ' Spektacular model: ' + escapeHtml(s.spek_model || '') + '.</p>' +
        '<div class="meetings-settings-grid">' +
        '<label class="muted-note inbox-field">Vexa gateway URL<input class="kb-search-box mt-2" type="url" data-meetings-setting="vexa_url" placeholder="https://vexa.example.org" autocomplete="off" value="' + escapeHtml(s.vexa_url || '') + '"></label>' +
        '<label class="muted-note inbox-field">Vexa tenant key ' + (s.vexa_key_set ? '(set)' : '(not set)') + '<input class="kb-search-box mt-2" type="password" data-meetings-setting="vexa_api_key" placeholder="leave blank to keep" autocomplete="new-password"></label>' +
        '<label class="muted-note inbox-field">Discord bot token ' + (s.discord_token_set ? '(set)' : '(not set, optional)') + '<input class="kb-search-box mt-2" type="password" data-meetings-setting="discord_token" placeholder="leave blank to keep" autocomplete="new-password"></label>' +
        '<label class="muted-note inbox-field">Transcription token ' + (s.transcribe_token_set ? '(set)' : '(not set, needed for Discord)') + '<input class="kb-search-box mt-2" type="password" data-meetings-setting="transcribe_token" placeholder="leave blank to keep" autocomplete="new-password"></label></div>' +
        '<p class="muted-xs" data-meetings-setting="discord_hint">Discord needs three things: the Discord bot token, the transcribe URL (<code>meetings.transcribe_url</code> or <code>HIVE_MEETINGS_TRANSCRIBE_URL</code>; where the shared service runs in the same cluster, the in-cluster <code>http://vexa-transcription.meetings.svc.cluster.local:8000</code>) and the transcription token. Transcribe URL now: ' +
        (eff['meetings.transcribe_url'] ? '<code>' + escapeHtml(eff['meetings.transcribe_url']) + '</code>' : 'not set') + '.</p>' +
        '<details class="mt-4"><summary class="muted-xs">Other meetings settings (hive.yaml or HIVE_MEETINGS_* env)</summary><table class="meetings-effective">' + rows + '</table></details>' +
        '<div class="hive-dialog-error" role="alert" data-meetings-setting="error"></div>' +
        '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="meetingsSaveSettings">Save</button></div></div>';
    }

    async function loadMeetingsSettingsInline() {
      const host = document.getElementById('meetings-settings-inline');
      if (!host) return;
      try {
        const r = await fetch('/api/meetings/settings');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        host.innerHTML = meetingsSettingsFormHtml(data);
      } catch (e) {
        host.innerHTML = '<p class="kc-error muted-xs">Meetings settings unavailable: ' + escapeHtml(e.message) + '</p>';
      }
    }

    function meetingsSetting(name) { return document.querySelector('[data-meetings-setting="' + name + '"]'); }

    async function meetingsSaveSettings() {
      const errEl = meetingsSetting('error');
      const body = { enabled: !!(meetingsSetting('enabled') || {}).checked, vexa_url: ((meetingsSetting('vexa_url') || {}).value || '').trim() };
      const key = ((meetingsSetting('vexa_api_key') || {}).value || '').trim();
      const tok = ((meetingsSetting('discord_token') || {}).value || '').trim();
      const tx = ((meetingsSetting('transcribe_token') || {}).value || '').trim();
      if (key) body.vexa_api_key = key;
      if (tok) body.discord_token = tok;
      if (tx) body.transcribe_token = tx;
      try {
        const data = await meetingsPost('/api/meetings/settings', body);
        showToast(data.restart_required ? 'Meetings settings saved; restart the hive to apply' : 'Meetings settings saved', 'success');
        const host = document.getElementById('meetings-settings-inline');
        if (host) host.innerHTML = meetingsSettingsFormHtml(data);
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Save failed: ' + e.message, 'error');
      }
    }
