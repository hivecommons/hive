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
    let _meetingsTrusted = [];
    // Budget meters by meeting id and the hive-wide ceilings, from GET /api/meetings/budget.
    let _meetingsBudget = {};
    let _meetingsLedger = null;
    // Levels the meter badges (the server's budget steps).
    const MEETINGS_BUDGET_BADGES = { warning: 'budget 80%', degraded: 'budget 90%: shorter answers', exhausted: 'budget exhausted' };
    let _meetingsOpen = null; // { id, ws, meeting, lines, partial, intents, live }
    // The row whose chevron has the focus after a toggle re-draws the list.
    let _meetingsFocusRow = '';
    // Ended meetings are inbox items too (source "meeting").
    if (typeof INBOX_SOURCE_ICONS === 'object' && !INBOX_SOURCE_ICONS.meeting) INBOX_SOURCE_ICONS.meeting = '🎙';

    // Raw errors longer than this collapse behind a "details" toggle.
    const MEETINGS_ERROR_INLINE_CHARS = 140;
    const MEETINGS_PLATFORM_LABELS = { google_meet: 'Google Meet', teams: 'Teams', zoom: 'Zoom', discord: 'Discord' };
    // Meeting state to the kc-pill status palette (green, blue, red, muted).
    const MEETINGS_PILL_STATUS = { live: 'ok', requested: 'syncing', joining: 'syncing', lobby: 'syncing', failed: 'error', blocked: 'error' };

    // Final meeting states: the row offers 🗑 Delete and "Clear finished"
    // takes it. Joining, lobby and live meetings are left first.
    const MEETINGS_FINAL_STATES = { ended: 1, failed: 1, blocked: 1, handed_off: 1, purged: 1 };

    function meetingsIsOwner() { return window._hiveRole === 'owner'; }
    // The role is known once /api/role settled (answered, or its retries gave
    // up). Until then the panel shows a neutral loading state, never the
    // owner-only notice, and re-renders when gh-auth.js notifies role
    // listeners (same contract as the Inbox, #18442).
    function meetingsRoleKnown() { return window._hiveRoleSettled === true || window._hiveRole !== undefined; }
    function meetingsOnRoleReady() { fetchMeetings(); }
    (window._hiveRoleListeners = window._hiveRoleListeners || []).push(meetingsOnRoleReady);
    function meetingsLabel(m) {
      if (m.title) return m.title;
      if (m.platform === 'discord') return 'Discord ' + (m.channel_id || '');
      return m.url || m.platform || m.id;
    }

    async function fetchMeetings() {
      const host = document.getElementById('meetings-panel');
      if (!host) return;
      if (!meetingsRoleKnown()) {
        host.innerHTML = '<p class="muted-xs">Loading meetings…</p>';
        return;
      }
      if (!meetingsIsOwner()) {
        host.innerHTML = '<p class="empty-dashed">Meetings are visible to the hive owner only.</p>' +
          (window._hiveRoleFailed && !window._hiveRole ? '<p class="muted-xs">Could not determine your role. It will retry automatically.</p>' : '');
        return;
      }
      try {
        const r = await fetch('/api/meetings');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) { _meetingsList = []; _meetingsError = data.error || ('HTTP ' + r.status); }
        else { _meetingsList = data.meetings || []; _meetingsError = ''; _meetingsEmailIn = data.email_in || null; _meetingsTrusted = data.trusted_requesters || []; }
      } catch (e) {
        // Keep the last list; say the hive is unreachable instead of leaving "Loading meetings…".
        if (_meetingsList === null) _meetingsList = [];
        _meetingsError = 'Meetings are unreachable right now; retrying.';
      }
      await fetchMeetingsBudget();
      renderMeetings();
    }

    // The meter is numbers only; a failure leaves the last one.
    async function fetchMeetingsBudget() {
      try {
        const r = await fetch('/api/meetings/budget');
        if (!r.ok) return;
        const data = await r.json();
        const byId = {};
        (data.meetings || []).forEach(b => { byId[b.meeting_id] = b; });
        _meetingsBudget = byId;
        _meetingsLedger = data.ledger || null;
      } catch (e) { /* transient */ }
    }

    // meetingsBudgetText is the meter's text alternative.
    function meetingsBudgetText(b) {
      return (b.percent || 0) + '% of the meeting budget used';
    }

    function meetingsBudgetHtml(b) {
      if (!b || !b.token_budget) return '';
      const text = meetingsBudgetText(b);
      const badge = MEETINGS_BUDGET_BADGES[b.level];
      const detail = (b.tokens_used || 0) + ' of ' + b.token_budget + ' tokens · ' + (b.intents_answered || 0) + ' answered' +
        (b.skipped ? ' · ' + b.skipped + ' question' + (b.skipped === 1 ? '' : 's') + ' not answered' : '');
      return '<div class="meetings-budget" data-level="' + escapeHtml(b.level || 'ok') + '">' +
        '<div class="meetings-budget-line"><span class="meetings-budget-text">' + escapeHtml(text) + '</span>' +
        '<span class="meetings-budget-num">' + escapeHtml(detail) + '</span>' +
        (badge ? '<span class="kc-pill" data-status="' + (b.level === 'warning' ? 'syncing' : 'error') + '">' + escapeHtml(badge) + '</span>' : '') + '</div>' +
        '<meter class="meetings-meter" min="0" max="100" low="79" high="89" optimum="0" value="' + Number(b.percent || 0) + '" aria-label="' + escapeHtml(text) + '" title="' + escapeHtml(text) + '">' + escapeHtml(text) + '</meter>' +
        '</div>';
    }

    // The ceilings hive-wide, for the live view.
    function meetingsLedgerText(l) {
      if (!l) return '';
      const part = (name, used, ceil) => ceil ? name + ' ' + used + ' of ' + ceil + ' tokens' : '';
      return [part('hour', l.hourly_used || 0, l.hourly_ceiling), part('day', l.daily_used || 0, l.daily_ceiling)].filter(Boolean).join(' · ');
    }

    // meetingsSpekNoticeHtml shows the server's Spektacular status as one calm
    // notice: the text before the first colon is the headline, the rest is the
    // explanation, and a repo reference (hivecommons/name#123) is a link to
    // that issue.
    function meetingsSpekNoticeHtml(text) {
      const raw = String(text || '');
      const cut = raw.indexOf(': ');
      const title = cut > 0 ? raw.slice(0, cut) : '';
      const body = cut > 0 ? raw.slice(cut + 2) : raw;
      const link = t => escapeHtml(t).replace(/hivecommons\/([A-Za-z0-9._-]+)#(\d+)/g, '<a href="https://github.com/hivecommons/$1/issues/$2" target="_blank" rel="noopener noreferrer">$&</a>');
      return '<div class="meetings-notice" role="status">' + (title ? '<div class="meetings-notice-title">' + escapeHtml(title) + '</div>' : '') +
        '<p class="meetings-notice-body">' + link(body) + '</p></div>';
    }

    // The questions skipped for budget, with their text (owner view only).
    function meetingsSkippedHtml(live) {
      const list = (live && live.skipped) || [];
      if (!list.length) return '';
      return '<section class="meetings-section"><h4 class="section-label meetings-section-label">Not answered: budget</h4><ul class="meetings-skipped">' +
        list.map(q => '<li>' + escapeHtml((q.who ? q.who + ': ' : '') + (q.text || '')) + '</li>').join('') + '</ul></section>';
    }
    dashboardScheduleWork('meetings', fetchMeetings).catch(console.error);
    dashboardSetInterval('meetings', fetchMeetings, MEETINGS_POLL_MS);

    function meetingsJoinFormHtml() {
      return '<div class="meetings-join">' +
        '<div class="meetings-join-fields">' +
        '<label class="meetings-field">Meeting link (Google Meet, Teams, Zoom)<input class="kb-search-box" type="url" data-meetings-field="url" placeholder="https://meet.google.com/abc-defg-hij" autocomplete="off"></label>' +
        '<label class="meetings-field">Title (optional)<input class="kb-search-box" type="text" data-meetings-field="title" placeholder="Weekly sync" autocomplete="off"></label></div>' +
        '<details class="meetings-join-more"><summary class="muted-xs">Discord voice channel instead</summary><div class="meetings-join-fields mt-2">' +
        '<label class="meetings-field">Guild ID<input class="kb-search-box" type="text" data-meetings-field="guild" autocomplete="off"></label>' +
        '<label class="meetings-field">Voice channel ID<input class="kb-search-box" type="text" data-meetings-field="channel" autocomplete="off"></label></div></details>' +
        '<div class="meetings-join-actions">' +
        '<label class="meetings-voice-toggle"><input type="checkbox" data-meetings-field="voice"><span>Voice</span></label>' +
        '<button type="button" class="hv-btn btn-primary btn-sm" data-action="meetingsJoin">Send hive to a meeting</button></div>' +
        '<div class="hive-dialog-error" role="alert" data-meetings-field="error"></div></div>';
    }

    function meetingsFormatTime(iso) {
      if (!iso) return '';
      const d = new Date(iso);
      if (isNaN(d.getTime())) return '';
      try { return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }).format(d); }
      catch (e) { return d.toISOString(); }
    }

    // meetingsFriendlyReason is a plain-language line for a meeting's
    // state_reason; the raw reason stays available under "details".
    function meetingsFriendlyReason(m) {
      const raw = String(m.state_reason || '');
      if (!raw) return '';
      // Discord has no guest lobby: a blocked Discord join is the bot's own
      // server permissions on that voice channel (#18598).
      if (m.platform === 'discord' && (m.state === 'blocked' || /blocked|no permission/.test(raw))) {
        return 'The Discord bot cannot join that voice channel. In the server, give the bot\'s role View Channel, Connect and Speak on the channel, then try again.';
      }
      if (m.state === 'blocked') return 'The meeting refused a guest bot.';
      if (/^unreachable$/.test(raw)) return 'Left: none of its replies could be delivered to the meeting, in chat or by voice.';
      if (/consent_undelivered/.test(raw)) return 'Left: the consent notice could not be delivered' + (m.consent_error ? ' (' + m.consent_error + ')' : '') + '.';
      if (/lobby_timeout/.test(raw)) return 'Nobody admitted hive from the lobby in time.';
      if (/platform_unavailable/.test(raw)) return 'This meeting platform is not available on this hive right now.';
      if (/invalid_argument/.test(raw)) return 'The meeting service did not accept this meeting link.';
      if (/upstream_error|Vexa|answered 5\d\d/.test(raw)) return 'The meeting service could not start the bot.';
      if (m.state === 'failed') return 'Hive could not join this meeting.';
      return raw.length > MEETINGS_ERROR_INLINE_CHARS ? '' : raw;
    }

    function meetingsReasonHtml(m) {
      const raw = String(m.state_reason || '');
      if (!raw) return '';
      const friendly = meetingsFriendlyReason(m);
      const cls = (m.state === 'failed' || m.state === 'blocked') ? 'meetings-row-error' : 'muted-xs';
      if (friendly === raw) return '<div class="' + cls + '">' + escapeHtml(raw) + '</div>';
      return '<div class="' + cls + '">' + escapeHtml(friendly || 'Details') + '</div>' +
        '<details class="meetings-row-details"><summary class="muted-xs">details</summary><div class="meetings-row-raw">' + escapeHtml(raw) + '</div></details>';
    }

    function meetingsRowHtml(m) {
      const active = !!MEETINGS_LIVE_STATES[m.state];
      const open = _meetingsOpen && _meetingsOpen.id === m.id;
      const request = m.state === 'requested' && m.requested_by;
      if (request) {
        return '<div class="meetings-row" data-state="requested"><div class="meetings-row-main"><div class="meetings-row-head">' +
          '<span class="meetings-row-title">' + escapeHtml((m.requested_by.name || m.requested_by.id || 'someone') + ' asked hive into ' + meetingsLabel(m)) + '</span>' +
          '<span class="kc-pill" data-status="syncing">join request</span></div>' + meetingsReasonHtml(m) + '</div>' +
          '<div class="meetings-row-actions">' +
          '<button type="button" class="hv-btn btn-primary btn-sm" data-action="meetingsApproveRequest" data-arg0="' + escapeHtml(m.id) + '">Approve</button>' +
          (m.platform === 'discord' && /^[0-9]{5,25}$/.test(m.requested_by.id || '') ?
            '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsApproveAlways" data-arg0="' + escapeHtml(m.id) + '"' +
            ' title="Approve now, and approve this person\'s future Discord requests automatically. Matched by their Discord user id, not their name.">Always approve this person</button>' : '') +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsDeclineRequest" data-arg0="' + escapeHtml(m.id) + '">Decline</button>' +
          meetingsDeleteButtonHtml(m) + '</div></div>';
      }
      const state = m.state || '';
      const when = meetingsFormatTime(m.live_at || m.created_at);
      const meta = [MEETINGS_PLATFORM_LABELS[m.platform] || m.platform || '', when].filter(Boolean).join(' · ');
      const pid = 'meetings-details-' + String(m.id).replace(/[^A-Za-z0-9_-]/g, '_');
      return '<div class="meetings-item" data-meeting-item="' + escapeHtml(m.id) + '">' +
        '<div class="meetings-row' + (open ? ' is-open' : '') + '" data-state="' + escapeHtml(state) + '">' +
        rowChevronHtml('meetingsToggle', m.id, open, 'details of ' + meetingsLabel(m), pid) +
        '<div class="meetings-row-main"><div class="meetings-row-head" data-action="meetingsToggle" data-arg0="' + escapeHtml(m.id) + '">' +
        '<span class="meetings-row-title" title="' + escapeHtml(m.url || '') + '">' + escapeHtml(meetingsLabel(m)) + '</span>' +
        '<span class="kc-pill" data-status="' + escapeHtml(MEETINGS_PILL_STATUS[state] || '') + '">' + escapeHtml(state) + '</span></div>' +
        (meta ? '<div class="muted-xs">' + escapeHtml(meta) + '</div>' : '') +
        meetingsBudgetHtml(_meetingsBudget[m.id]) +
        meetingsReasonHtml(m) +
        (active && m.consent_error && !m.consent_posted_at ? '<div class="muted-xs" title="' + escapeHtml(m.consent_error) + '">chat unavailable: consent notice not posted yet</div>' : '') +
        (m.consent_via === 'voice' ? '<div class="muted-xs">consent notice spoken: the meeting chat could not deliver it</div>' : '') +
        (m.voice_enabled && !m.voice_capable ? '<div class="muted-xs">joined without voice; replies go to chat</div>' : '') +
        '</div><div class="meetings-row-actions">' +
        (active ? meetingsVoiceToggleHtml(m) +
          '<button type="button" class="hv-btn btn-danger btn-sm" data-action="meetingsLeave" data-arg0="' + escapeHtml(m.id) + '">Leave</button>' : '') +
        meetingsDeleteButtonHtml(m) + '</div></div>' +
        (open ? '<div class="meetings-details" id="' + pid + '" role="region" aria-label="Details of ' + escapeHtml(meetingsLabel(m)) + '"><div id="meetings-live-panel"></div></div>' : '') + '</div>';
    }

    // meetingsVoiceToggleHtml: the card's voice control names the action it
    // takes ("Turn voice off" while voice is on), never the current state
    // alone: a "Voice on" button that turned voice off read as the control
    // to switch it on, and an operator who had ticked Voice at join turned
    // it off by clicking it. aria-pressed and the title carry the state.
    function meetingsVoiceToggleHtml(m) {
      const on = !!(m && m.voice_enabled);
      const label = on ? 'Turn voice off' : 'Turn voice on';
      const title = on
        ? 'Voice replies are on: she answers out loud when addressed. Click to answer in chat only.'
        : 'Voice replies are off: she answers in chat. Click to let her answer out loud when addressed.';
      return '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsToggleVoice" data-arg0="' + escapeHtml((m && m.id) || '') +
        '" data-arg1="' + (on ? 'false' : 'true') + '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + escapeHtml(title) + '">' +
        (on ? '🔊 ' : '🔇 ') + label + '</button>';
    }

    // meetingsCanDelete: a meeting in a final state, or a platform join
    // request still waiting (deleting it declines it). Owner only.
    function meetingsCanDelete(m, role) {
      if (!m || role !== 'owner') return false;
      if (MEETINGS_FINAL_STATES[m.state]) return true;
      return m.state === 'requested' && !!m.requested_by;
    }

    function meetingsDeleteButtonHtml(m) {
      if (!meetingsCanDelete(m, window._hiveRole)) return '';
      return '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsDelete" data-arg0="' + escapeHtml(m.id) + '"' +
        ' title="Delete this meeting, its transcript and its Vexa data; facts and drafts already made from it stay">🗑 Delete</button>';
    }

    // meetingsDeleteText is the in-page confirm text for one meeting.
    function meetingsDeleteText(m) {
      if (m && m.state === 'requested') {
        return 'This declines the join request and removes it. It cannot be undone.';
      }
      return 'This deletes the meeting record, its transcript, notes and live state on this hive, its data in Vexa, and its Calendar entry if it has no other occurrences. ' +
        'Knowledge facts and drafts already made from it are not touched. It cannot be undone.';
    }

    // meetingsFinished lists the meetings "Clear finished" deletes: final
    // states that finished before now.
    function meetingsFinished(list, now) {
      const cut = now instanceof Date ? now.getTime() : Date.now();
      return (list || []).filter(m => {
        if (!m || !MEETINGS_FINAL_STATES[m.state]) return false;
        const t = new Date(m.ended_at || m.updated_at || m.created_at || 0).getTime();
        return !isNaN(t) && t < cut;
      });
    }

    function meetingsClearFinishedText(n) {
      return 'This deletes ' + n + ' finished meeting' + (n === 1 ? '' : 's') + ' (ended, failed, blocked or declined): their records, transcripts, notes and live state on this hive, their data in Vexa, and Calendar entries that existed only for them. ' +
        'Live meetings and pending join requests stay. Knowledge facts and drafts already made from them are not touched. It cannot be undone.';
    }

    function meetingsAfterDelete(ids) {
      if (_meetingsOpen && ids.indexOf(_meetingsOpen.id) >= 0) meetingsClosePanel();
      if (typeof calendarRefreshMeetings === 'function') calendarRefreshMeetings();
      if (typeof fetchCalendar === 'function') fetchCalendar();
    }

    async function meetingsDelete(id) {
      const m = (_meetingsList || []).find(x => x && x.id === id);
      if (!m) return;
      const ok = await hiveConfirm({ title: m.state === 'requested' ? 'Delete this join request?' : 'Delete this meeting?', message: meetingsDeleteText(m), confirmLabel: 'Delete', danger: true });
      if (!ok) return;
      try {
        const r = await fetch('/api/meetings/' + encodeURIComponent(id), { method: 'DELETE' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        if (data.upstream_error) showToast('Meeting deleted here; Vexa did not confirm its copy was erased: ' + data.upstream_error, 'warning');
        else showToast('Meeting deleted', 'success');
        meetingsAfterDelete([id]);
      } catch (e) {
        showToast('Could not delete the meeting: ' + e.message, 'error');
      }
      await fetchMeetings();
    }

    async function meetingsClearFinished() {
      const now = new Date();
      const finished = meetingsFinished(_meetingsList, now);
      if (!finished.length) return;
      const ok = await hiveConfirm({ title: 'Clear finished meetings?', message: meetingsClearFinishedText(finished.length), confirmLabel: 'Delete ' + finished.length, danger: true });
      if (!ok) return;
      try {
        const data = await meetingsPost('/api/meetings/clear-finished', { before: now.toISOString() });
        const deleted = data.deleted || [];
        const upstream = deleted.filter(d => d && d.upstream_error).length;
        showToast('Deleted ' + deleted.length + ' meeting' + (deleted.length === 1 ? '' : 's') + (upstream ? '; Vexa did not confirm ' + upstream + ' erase' + (upstream === 1 ? '' : 's') : ''), upstream ? 'warning' : 'success');
        meetingsAfterDelete(deleted.map(d => d.id));
      } catch (e) {
        showToast('Could not clear finished meetings: ' + e.message, 'error');
      }
      await fetchMeetings();
    }

    function renderMeetings() {
      const host = document.getElementById('meetings-panel');
      if (!host) return;
      let html = '';
      if (_meetingsError) {
        html += '<p class="kc-error muted-xs">' + escapeHtml(_meetingsError) + '</p>';
      } else {
        html += (typeof emailInRowHtml === 'function' ? emailInRowHtml('meetings', _meetingsEmailIn) : '') + meetingsJoinFormHtml();
      }
      const list = _meetingsList || [];
      const finished = meetingsFinished(list);
      if (finished.length && meetingsIsOwner()) {
        html += '<div class="meetings-row-actions"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsClearFinished"' +
          ' title="Delete every finished meeting after one confirm">🗑 Clear finished (' + finished.length + ')</button></div>';
      }
      html += meetingsTrustedHtml();
      html += '<div class="meetings-list">' + (list.length ? list.map(meetingsRowHtml).join('') : (_meetingsError ? '' : '<p class="empty-dashed">No meetings yet.</p>')) + '</div>';
      host.innerHTML = html;
      const liveCount = list.filter(m => MEETINGS_LIVE_STATES[m.state]).length;
      if (typeof refreshSectionCardShell === 'function') refreshSectionCardShell('meetings-section');
      if (typeof setSectionSummary === 'function') setSectionSummary('meetings-section', liveCount ? liveCount + ' live' : 'no live meetings');
      renderMeetingsLive();
      if (_meetingsFocusRow) {
        const want = _meetingsFocusRow;
        _meetingsFocusRow = '';
        const chev = Array.prototype.find.call(host.querySelectorAll('.row-chevron'), c => c.getAttribute('data-arg0') === want);
        if (chev) chev.focus();
      }
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

    async function meetingsApproveAlways(id) {
      try { await meetingsPost('/api/meetings/' + encodeURIComponent(id) + '/approve', { always: true }); showToast('Hive is joining, and will join this person\'s future requests', 'success'); }
      catch (e) { showToast('Approve failed: ' + e.message, 'error'); }
      fetchMeetings();
    }

    async function meetingsUntrust(platform, uid) {
      try {
        const r = await fetch('/api/meetings/trusted/' + encodeURIComponent(platform) + '/' + encodeURIComponent(uid), { method: 'DELETE' });
        if (!r.ok) { const d = await r.json().catch(() => ({})); throw new Error(d.error || ('HTTP ' + r.status)); }
        showToast('Removed. Their requests wait for approval again', 'success');
      } catch (e) { showToast('Remove failed: ' + e.message, 'error'); }
      fetchMeetings();
    }

    // meetingsTrustedHtml lists the people whose Discord requests join at once.
    function meetingsTrustedHtml() {
      const list = _meetingsTrusted || [];
      if (!list.length) return '';
      return '<div class="meetings-trusted"><div class="section-label m-0">Always approved on Discord</div>' + list.map(t =>
        '<div class="meetings-row-actions"><span class="muted-xs">' + escapeHtml((t.name || 'unnamed') + ' (Discord id ' + t.id + ')') + '</span>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingsUntrust" data-arg0="' + escapeHtml(t.platform) + '" data-arg1="' + escapeHtml(t.id) + '">Remove</button></div>').join('') + '</div>';
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

    // meetingsToggle opens the details under the meeting's row, or folds them
    // when they are already open. The chevron and the row title both call it.
    function meetingsToggle(id) {
      _meetingsFocusRow = id;
      meetingsOpenPanel(id);
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
          if (ev.type !== 'reply') o.partial = null;
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
        case 'budget': meetingsRefreshLive(o.id); break;
        case 'dropped': meetingsRefreshLive(o.id); break;
        case 'reach': meetingsRefreshLive(o.id); break;
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
        if (_meetingsOpen && _meetingsOpen.id === id) {
          _meetingsOpen.live = data.live || null;
          if (data.budget) _meetingsBudget[id] = data.budget;
          renderMeetingsLive();
        }
      } catch (e) { /* transient */ }
    }

    function meetingsLineHtml(ev, partial) {
      if (ev.type === 'reply') return meetingsReplyLineHtml(ev);
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
        '<div class="meetings-notes-group"><div class="meetings-notes-label">' + escapeHtml(s) + '</div><ul>' + notes[s].map(n => '<li>' + escapeHtml(n) + '</li>').join('') + '</ul></div>').join('') || '<p class="meetings-empty">No notes yet.</p>';
      const research = (live.research || []).map(it =>
        '<div class="meetings-research-item"><div><span class="badge">' + escapeHtml(it.status) + '</span> ' + escapeHtml(it.query) + '</div>' +
        (it.summary ? '<div class="muted-xs">' + escapeHtml(it.summary) + '</div>' : '') +
        (it.status === 'failed' && it.error ? '<div class="meetings-row-error">' + escapeHtml(it.error) + '</div>' : '') +
        (it.waiting && it.status === 'queued' ? '<div class="muted-xs">waiting behind another question</div>' : '') +
        (it.status === 'pending_approval' ? '<button type="button" class="hv-btn btn-primary btn-sm mt-2" data-action="meetingsApprove" data-arg0="' + escapeHtml(o.id) + '" data-arg1="' + escapeHtml(it.rid) + '">Approve research</button>' : '') +
        '</div>').join('') || '<p class="meetings-empty">No research yet.</p>';
      const lines = o.lines.map(ev => meetingsLineHtml(ev, false)).join('') + (o.partial ? meetingsLineHtml(o.partial, true) : '');
      const intents = o.intents.map(i => '<div>' + escapeHtml(typeof i === 'string' ? i : JSON.stringify(i)) + '</div>').join('');
      const spek = live.spek_status ? meetingsSpekNoticeHtml(live.spek_status) :
        (live.spek_mode ? '<p class="meetings-notice-line">Spektacular: ' + escapeHtml(live.spek_mode) + '</p>' : '');
      const ledger = meetingsLedgerText(_meetingsLedger);
      const sect = (label, body) => '<section class="meetings-section"><h4 class="section-label meetings-section-label">' + label + '</h4>' + body + '</section>';
      const head = spek + meetingsBudgetHtml(_meetingsBudget[o.id]) + (ledger ? '<p class="meetings-hivewide">Hive-wide: ' + escapeHtml(ledger) + '</p>' : '') + meetingsReachHtml(live);
      host.innerHTML = (head ? '<div class="meetings-details-head">' + head + '</div>' : '') + '<div class="meetings-live">' +
        sect('Transcript' + (o.ws ? '' : ' (disconnected)'), '<div class="meetings-transcript">' + (lines || '<p class="meetings-empty">Waiting for the first words…</p>') + '</div>') +
        '<div class="meetings-side">' + sect('Notes', notesHtml) +
        meetingsSkippedHtml(live) +
        sect('Research', '<div class="meetings-research">' + research + '</div>') +
        sect('Spek intents', '<div class="meetings-intents">' + (intents || '<p class="meetings-empty">None yet.</p>') + '</div>') + '</div></div>';
      const tr = host.querySelector('.meetings-transcript');
      if (tr) tr.scrollTop = tr.scrollHeight;
    }

    // Settings → Meetings: enable, Vexa gateway, write-only secrets.
    function renderGovMeetings() {
      return '<div class="config-section" data-config-section="Meetings"><h3>Meetings</h3>' +
        '<p class="text-muted fs-base">Send hive into Google Meet, Teams, Zoom or Discord meetings to take notes, ask clarifying questions and run research. ' +
        yamlHint('meetings.enabled') + ' ' + yamlHint('meetings.vexa.url') + ' ' + yamlHint('meetings.vexa.api_key_file') + ' ' + yamlHint('meetings.discord.token_file') + ' ' + yamlHint('meetings.transcribe_url') + ' ' + yamlHint('meetings.transcribe_token_file') + ' ' + yamlHint('meetings.tts_url') + ' ' + yamlHint('meetings.tts_token_file') + ' Changes apply after a restart.</p>' +
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
        '<label class="muted-note inbox-field">Transcription token ' + (s.transcribe_token_set ? '(set)' : '(not set, needed for Discord)') + '<input class="kb-search-box mt-2" type="password" data-meetings-setting="transcribe_token" placeholder="leave blank to keep" autocomplete="new-password"></label>' +
        '<label class="muted-note inbox-field">TTS token ' + (s.tts_token_set ? '(set)' : '(not set, needed for Discord speech)') + '<input class="kb-search-box mt-2" type="password" data-meetings-setting="tts_token" placeholder="leave blank to keep" autocomplete="new-password"></label></div>' +
        '<p class="muted-xs" data-meetings-setting="discord_hint">Discord needs three things: the Discord bot token, the transcribe URL (<code>meetings.transcribe_url</code> or <code>HIVE_MEETINGS_TRANSCRIBE_URL</code>; where the shared service runs in the same cluster, the in-cluster <code>http://vexa-transcription.meetings.svc.cluster.local:8000</code>) and the transcription token. Transcribe URL now: ' +
        (eff['meetings.transcribe_url'] ? '<code>' + escapeHtml(eff['meetings.transcribe_url']) + '</code>' : 'not set') + '. Discord speech also needs the TTS URL (<code>meetings.tts_url</code> or <code>HIVE_MEETINGS_TTS_URL</code>; in-cluster <code>http://vexa-tts.meetings.svc.cluster.local:8880</code>) and the TTS token. TTS URL now: ' +
        (eff['meetings.tts_url'] ? '<code>' + escapeHtml(eff['meetings.tts_url']) + '</code>' : 'not set') + '.</p>' +
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
      const tts = ((meetingsSetting('tts_token') || {}).value || '').trim();
      if (key) body.vexa_api_key = key;
      if (tok) body.discord_token = tok;
      if (tx) body.transcribe_token = tx;
      if (tts) body.tts_token = tts;
      try {
        const data = await meetingsPost('/api/meetings/settings', body);
        showToast(data.restart_required ? 'Meetings settings saved; restart the hive to apply' : 'Meetings settings saved', 'success');
        const host = document.getElementById('meetings-settings-inline');
        if (host) host.innerHTML = meetingsSettingsFormHtml(data);
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Save failed: ' + e.message, 'error');
      }
    }
