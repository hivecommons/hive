    // ── Calendar: scheduled meetings hive joins (hivecommons/hive#18394) ──
    // GET /api/meetings/schedule lists upcoming and recent scheduled
    // meetings; the scheduler joins each one shortly before it starts. The
    // top-bar "in a meeting" indicator and the Calendar sidebar badge follow
    // /api/events "meeting" frames over an initial GET /api/meetings.
    const CALENDAR_POLL_MS = 60000;
    // Meeting states the indicator counts as "in a meeting".
    const CALENDAR_ACTIVE_STATES = { joining: 1, lobby: 1, live: 1 };
    const CALENDAR_DEFAULT_NAME = 'Hive';
    const CALENDAR_PLATFORM_LABELS = { google_meet: 'Google Meet', teams: 'Teams', zoom: 'Zoom', discord: 'Discord' };
    const CALENDAR_PLATFORM_ICONS = { google_meet: '🟢', teams: '🟣', zoom: '🔵', discord: '🎧' };
    const CALENDAR_SOURCE_LABELS = { email: 'email invite', manual: 'added here', discord: 'Discord' };
    const CALENDAR_STATUS_LABELS = {
      scheduled: 'scheduled', awaiting_approval: 'awaiting approval', live: 'live', ended: 'ended',
      cancelled: 'cancelled', declined: 'declined', failed: 'failed', blocked: 'blocked', skipped: 'skipped'
    };
    const CALENDAR_REPEAT_RULES = {
      none: '', daily: 'FREQ=DAILY', weekdays: 'FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR', weekly: 'FREQ=WEEKLY', monthly: 'FREQ=MONTHLY'
    };
    let _calendarData = null;
    let _calendarError = '';
    // Meetings by id, from GET /api/meetings and "meeting" frames.
    let _calendarMeetings = {};

    function calendarCanView() {
      return typeof dashboardRoleAtLeast === 'function' ? dashboardRoleAtLeast(window._hiveRole || 'read', 'read-write') : window._hiveRole === 'owner';
    }
    function calendarIsOwner() { return window._hiveRole === 'owner'; }

    function calendarPlatformLabel(p) { return CALENDAR_PLATFORM_LABELS[p] || p || 'meeting'; }

    // The viewer's own time zone: no timeZone option, so Intl uses it.
    function calendarFormatTime(iso) {
      if (!iso) return '';
      const d = new Date(iso);
      if (isNaN(d.getTime())) return '';
      try {
        return new Intl.DateTimeFormat(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', timeZoneName: 'short' }).format(d);
      } catch (e) { return d.toISOString(); }
    }

    function calendarViewerTimeZone() {
      try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch (e) { return ''; }
    }

    // calendarIndicatorState is the top-bar indicator for a set of meetings:
    // hidden with none in joining, lobby or live; one opens its live panel,
    // several open the Calendar section.
    function calendarIndicatorState(meetings, name) {
      const who = name || CALENDAR_DEFAULT_NAME;
      const active = (meetings || []).filter(m => m && CALENDAR_ACTIVE_STATES[m.state]);
      if (!active.length) return { count: 0, hidden: true, text: '', title: '', target: '' };
      const text = '🐝 ' + who + ' is in ' + (active.length === 1 ? 'a meeting' : active.length + ' meetings');
      const title = active.map(m => (CALENDAR_PLATFORM_ICONS[m.platform] || '🎙') + ' ' + calendarPlatformLabel(m.platform) + ': ' +
        (m.title || m.url || m.channel_id || m.id) + (m.state === 'live' ? '' : ' (' + m.state + ')')).join('\n');
      return { count: active.length, hidden: false, text: text, title: title, target: active.length === 1 ? active[0].id : 'calendar-section' };
    }

    // calendarBadgeCount is the Calendar sidebar badge: meetings hive is in
    // plus scheduled meetings awaiting the owner's approval.
    function calendarBadgeCount(meetings, upcoming) {
      const live = (meetings || []).filter(m => m && CALENDAR_ACTIVE_STATES[m.state]).length;
      const pending = (upcoming || []).filter(e => e && e.status === 'awaiting_approval').length;
      return live + pending;
    }

    function calendarMeetingList() { return Object.keys(_calendarMeetings).map(k => _calendarMeetings[k]); }

    function renderCalendarIndicator() {
      const el = document.getElementById('oc-meeting-indicator');
      const name = (_calendarData && _calendarData.attendee_name) || CALENDAR_DEFAULT_NAME;
      const st = calendarIndicatorState(calendarMeetingList(), name);
      if (el) {
        el.hidden = st.hidden;
        el.dataset.target = st.target;
        el.title = st.title || '';
        el.setAttribute('aria-label', st.text || 'Not in a meeting');
        const label = el.querySelector('.nav-chip__label');
        if (label) label.textContent = st.text;
      }
      const badge = document.getElementById('calendar-sidebar-badge');
      const upcoming = (_calendarData && _calendarData.upcoming) || [];
      const n = calendarBadgeCount(calendarMeetingList(), upcoming);
      if (badge && typeof setSidebarCountBadge === 'function') setSidebarCountBadge(badge, n, n ? n + ' live or awaiting approval' : '');
    }

    function calendarIndicatorClick() {
      const el = document.getElementById('oc-meeting-indicator');
      const target = el ? el.dataset.target : '';
      if (!target || target === 'calendar-section') { ocNavigate('calendar-section'); return; }
      calendarOpenMeeting(target);
    }

    // calendarOpenMeeting shows one meeting's live panel (transcript and
    // notes) in the Meetings section.
    function calendarOpenMeeting(id) {
      if (!id) return;
      ocNavigate('meetings-section');
      if (typeof meetingsOpenPanel === 'function' && !(typeof _meetingsOpen !== 'undefined' && _meetingsOpen && _meetingsOpen.id === id)) meetingsOpenPanel(id);
    }

    async function calendarRefreshMeetings() {
      if (!calendarIsOwner()) { _calendarMeetings = {}; renderCalendarIndicator(); return; }
      try {
        const r = await fetch('/api/meetings');
        if (!r.ok) return;
        const data = await r.json().catch(() => ({}));
        const next = {};
        (data.meetings || []).forEach(m => { if (m && m.id) next[m.id] = m; });
        _calendarMeetings = next;
      } catch (e) { return; }
      renderCalendarIndicator();
    }

    // calendarOnMeetingFrame applies one /api/events "meeting" frame
    // ({meeting_id, meeting}) without polling.
    function calendarOnMeetingFrame(p) {
      if (!p || !p.meeting_id) return;
      if (p.meeting && p.meeting.id) _calendarMeetings[p.meeting_id] = p.meeting;
      renderCalendarIndicator();
      const linked = _calendarData && (_calendarData.upcoming || []).some(e => e.meeting_id === p.meeting_id);
      if (linked) fetchCalendar();
    }

    async function fetchCalendar() {
      const host = document.getElementById('calendar-panel');
      if (!calendarCanView()) {
        if (host) host.innerHTML = '<p class="muted-xs">The calendar is visible to the hive owner and read-write users.</p>';
        return;
      }
      try {
        const r = await fetch('/api/meetings/schedule');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) { _calendarData = null; _calendarError = data.error || ('HTTP ' + r.status); }
        else { _calendarData = data; _calendarError = ''; }
      } catch (e) { return; }
      renderCalendar();
    }
    dashboardScheduleWork('calendar', () => Promise.all([fetchCalendar(), calendarRefreshMeetings()])).catch(console.error);
    dashboardSetInterval('calendar', fetchCalendar, CALENDAR_POLL_MS);

    function calendarFormHtml() {
      return '<details class="calendar-add"><summary class="muted-xs">Add meeting</summary><div class="calendar-add-grid mt-2">' +
        '<label>Meeting link (Google Meet, Teams, Zoom)<input class="kb-search-box" type="url" data-calendar-field="url" placeholder="https://meet.google.com/abc-defg-hij" autocomplete="off"></label>' +
        '<label>Title (optional)<input class="kb-search-box" type="text" data-calendar-field="title" maxlength="200" placeholder="Weekly sync" autocomplete="off"></label>' +
        '<label>Starts<input class="kb-search-box" type="datetime-local" data-calendar-field="start"></label>' +
        '<label>Ends (optional)<input class="kb-search-box" type="datetime-local" data-calendar-field="end"></label>' +
        '<label>Repeats<select class="kb-search-box" data-calendar-field="repeat"><option value="none">Does not repeat</option><option value="daily">Daily</option>' +
        '<option value="weekdays">Every weekday</option><option value="weekly">Weekly</option><option value="monthly">Monthly</option></select></label>' +
        '<button type="button" class="hv-btn btn-primary btn-sm" data-action="calendarAdd">Add meeting</button></div>' +
        '<p class="muted-xs">Times are in your time zone (' + escapeHtml(calendarViewerTimeZone() || 'local') + '). ' +
        (calendarIsOwner() ? 'Hive joins about a minute before the start.' : 'The hive owner approves meetings you add.') + '</p>' +
        '<div class="hive-dialog-error" role="alert" data-calendar-field="error"></div></details>';
    }

    function calendarActionButtons(e) {
      if (!calendarIsOwner()) return '';
      const id = escapeHtml(e.id);
      const btn = (action, label, cls) => '<button type="button" class="hv-btn ' + cls + ' btn-sm" data-action="calendarAct" data-arg0="' + id + '" data-arg1="' + action + '">' + label + '</button>';
      let out = '';
      if (e.status === 'awaiting_approval') out += btn('approve', 'Approve', 'btn-primary') + btn('decline', 'Decline', 'btn-secondary');
      if (e.status === 'scheduled' || e.status === 'awaiting_approval' || (e.status === 'live' && e.rrule)) out += btn('skip', e.rrule ? 'Skip next' : 'Skip', 'btn-secondary');
      if (e.status === 'scheduled' || e.status === 'awaiting_approval' || e.status === 'live') out += btn('cancel', 'Cancel', 'btn-danger');
      return out;
    }

    function calendarMeetingLink(e) {
      const last = (e.history || []).length ? e.history[e.history.length - 1] : null;
      const mid = e.meeting_id || (last && last.meeting_id) || '';
      if (!mid) return '';
      const live = e.status === 'live' || (last && last.status === 'live');
      return '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="calendarOpenMeeting" data-arg0="' + escapeHtml(mid) + '">' + (live ? 'Open live panel' : 'Notes') + '</button>';
    }

    function calendarRowHtml(e) {
      const platform = e.platform || '';
      const organizer = e.organizer ? (e.organizer.name || e.organizer.email || '') : (e.invited_by || '');
      const next = (e.next_occurrences || [])[0];
      const status = e.status || '';
      const times = calendarFormatTime(e.start) + (e.end ? ' – ' + calendarFormatTime(e.end) : '');
      return '<div class="calendar-row" data-calendar-event="' + escapeHtml(e.id) + '" data-status="' + escapeHtml(status) + '">' +
        '<span class="badge calendar-status" data-status="' + escapeHtml(status) + '">' + escapeHtml(CALENDAR_STATUS_LABELS[status] || status) + '</span>' +
        '<span class="badge calendar-platform" data-platform="' + escapeHtml(platform) + '">' + (CALENDAR_PLATFORM_ICONS[platform] || '🎙') + ' ' + escapeHtml(calendarPlatformLabel(platform)) + '</span>' +
        '<div class="calendar-row-main"><div class="calendar-row-title" title="' + escapeHtml(e.join_url || '') + '">' + escapeHtml(e.summary || e.join_url || 'Meeting') + '</div>' +
        '<div class="muted-xs">' + escapeHtml(times) + (e.rrule ? ' · repeats' : '') +
        (next && e.rrule ? ' · next ' + escapeHtml(calendarFormatTime(next)) : '') +
        (organizer ? ' · ' + escapeHtml(organizer) : '') + ' · ' + escapeHtml(CALENDAR_SOURCE_LABELS[e.source] || e.source || '') +
        (e.status_reason ? ' · ' + escapeHtml(e.status_reason) : '') + '</div></div>' +
        calendarMeetingLink(e) + calendarActionButtons(e) + '</div>';
    }

    function renderCalendar() {
      refreshSectionCardShell('calendar-section');
      const host = document.getElementById('calendar-panel');
      renderCalendarIndicator();
      if (!host) return;
      if (_calendarError) {
        host.innerHTML = '<p class="kc-error muted-xs">' + escapeHtml(_calendarError) + '</p>';
        setSectionSummary('calendar-section', 'unavailable');
        return;
      }
      const data = _calendarData || {};
      const upcoming = data.upcoming || [];
      const recent = data.recent || [];
      const pending = upcoming.filter(e => e.status === 'awaiting_approval').length;
      host.innerHTML = calendarFormHtml() +
        '<div class="section-label m-0">Upcoming</div><div class="calendar-list">' + (upcoming.length ? upcoming.map(calendarRowHtml).join('') : '<p class="muted-xs">Nothing scheduled.</p>') + '</div>' +
        (recent.length ? '<div class="section-label m-0">Recent</div><div class="calendar-list">' + recent.map(calendarRowHtml).join('') + '</div>' : '');
      setSectionSummary('calendar-section', upcoming.length + ' upcoming' + (pending ? ' · ' + pending + ' awaiting approval' : ''));
    }

    function calendarField(name) { return document.querySelector('[data-calendar-field="' + name + '"]'); }

    async function calendarPost(path, body) {
      const r = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      return data;
    }

    // calendarLocalISO turns a datetime-local value (the viewer's zone) into
    // an RFC 3339 UTC time.
    function calendarLocalISO(v) {
      if (!v) return '';
      const d = new Date(v);
      return isNaN(d.getTime()) ? '' : d.toISOString();
    }

    async function calendarAdd() {
      const err = calendarField('error');
      const repeat = (calendarField('repeat') || {}).value || 'none';
      const body = {
        url: ((calendarField('url') || {}).value || '').trim(),
        title: ((calendarField('title') || {}).value || '').trim(),
        start: calendarLocalISO((calendarField('start') || {}).value),
        end: calendarLocalISO((calendarField('end') || {}).value),
        rrule: CALENDAR_REPEAT_RULES[repeat] || '',
        timezone: calendarViewerTimeZone()
      };
      if (!body.url || !body.start) { if (err) err.textContent = 'A meeting link and a start time are required.'; return; }
      try {
        const ev = await calendarPost('/api/meetings/schedule', body);
        showToast(ev && ev.status === 'awaiting_approval' ? 'Meeting added; it waits for the owner to approve' : 'Meeting scheduled', 'success');
        fetchCalendar();
      } catch (e) { if (err) err.textContent = e.message; }
    }

    async function calendarAct(id, action) {
      try { await calendarPost('/api/meetings/schedule/' + encodeURIComponent(id) + '/' + encodeURIComponent(action)); }
      catch (e) { showToast('Could not ' + action + ': ' + e.message, 'error'); }
      fetchCalendar();
    }

    // ── Drafts from meetings (hivecommons/hive#18406) ──
    // Items people asked Betty for in a meeting (an issue, an RFC, a Spek
    // session, a knowledge note) wait here. Nothing is created until an
    // owner or read-write user approves; Edit changes the draft first.
    const MEETING_DRAFTS_POLL_MS = 30000;
    const MEETING_DRAFT_TYPES = { issue: 'Issue', rfc: 'RFC', spek: 'Spek session', knowledge: 'Knowledge note' };
    const MEETING_DRAFT_STATUS = { pending: 'awaiting approval', approving: 'creating…', done: 'created', failed: 'failed', rejected: 'rejected' };
    let _meetingDrafts = null;
    let _meetingDraftsError = '';
    let _meetingDraftRepos = [];

    // meetingDraftsSplit is the pending list and the recently decided ones.
    function meetingDraftsSplit(drafts) {
      const all = (drafts || []).filter(d => d && d.id);
      return {
        pending: all.filter(d => d.status === 'pending' || d.status === 'failed' || d.status === 'approving'),
        decided: all.filter(d => d.status === 'done' || d.status === 'rejected')
      };
    }

    async function fetchMeetingDrafts() {
      if (!calendarCanView()) return;
      try {
        const r = await fetch('/api/meetings/actions');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) { _meetingDrafts = null; _meetingDraftsError = data.error || ('HTTP ' + r.status); }
        else { _meetingDrafts = data; _meetingDraftsError = ''; }
        if (!_meetingDraftRepos.length) {
          const s = await fetch('/api/meetings/actions/settings');
          if (s.ok) { const sd = await s.json().catch(() => ({})); _meetingDraftRepos = sd.repos || []; }
        }
      } catch (e) { return; }
      renderMeetingDrafts();
    }
    dashboardScheduleWork('meeting-drafts', fetchMeetingDrafts).catch(console.error);
    dashboardSetInterval('meeting-drafts', fetchMeetingDrafts, MEETING_DRAFTS_POLL_MS);

    function meetingDraftEditHtml(d) {
      const id = escapeHtml(d.id);
      const types = Object.keys(MEETING_DRAFT_TYPES).map(t => '<option value="' + t + '"' + (t === d.type ? ' selected' : '') + '>' + MEETING_DRAFT_TYPES[t] + '</option>').join('');
      const repos = (_meetingDraftRepos || []).slice();
      if (d.repo && repos.indexOf(d.repo) < 0) repos.unshift(d.repo);
      const repoOpts = repos.map(r => '<option value="' + escapeHtml(r) + '"' + (r === d.repo ? ' selected' : '') + '>' + escapeHtml(r) + '</option>').join('');
      return '<details class="calendar-add"><summary class="muted-xs">Edit</summary><div class="calendar-add-grid mt-2">' +
        '<label>Type<select class="kb-search-box" data-draft-id="' + id + '" data-draft-field="type">' + types + '</select></label>' +
        '<label>Title<input class="kb-search-box" type="text" maxlength="200" data-draft-id="' + id + '" data-draft-field="title" value="' + escapeHtml(d.title || '') + '"></label>' +
        (repoOpts ? '<label>Repo<select class="kb-search-box" data-draft-id="' + id + '" data-draft-field="repo">' + repoOpts + '</select></label>' : '') +
        '<label>Body<textarea class="kb-search-box" rows="4" maxlength="4000" data-draft-id="' + id + '" data-draft-field="body">' + escapeHtml(d.body || '') + '</textarea></label>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="meetingDraftSave" data-arg0="' + id + '">Save</button></div></details>';
    }

    function meetingDraftRowHtml(d) {
      const id = escapeHtml(d.id);
      const status = d.status || '';
      const open = status === 'pending' || status === 'failed';
      const btn = (action, label, cls) => '<button type="button" class="hv-btn ' + cls + ' btn-sm" data-action="meetingDraftAct" data-arg0="' + id + '" data-arg1="' + action + '">' + label + '</button>';
      const where = d.type === 'knowledge' ? 'knowledge vault' : (d.repo || 'no repo set');
      const result = d.result_url ? ' · <a href="' + escapeHtml(d.result_url) + '" target="_blank" rel="noopener">' + escapeHtml(d.result_url) + '</a>' : (d.result_note ? ' · ' + escapeHtml(d.result_note) : '');
      return '<div class="calendar-row" data-meeting-draft="' + id + '" data-status="' + (open ? 'awaiting_approval' : escapeHtml(status)) + '">' +
        '<span class="badge calendar-status" data-status="' + escapeHtml(status) + '">' + escapeHtml(MEETING_DRAFT_STATUS[status] || status) + '</span>' +
        '<span class="badge">' + escapeHtml(MEETING_DRAFT_TYPES[d.type] || d.type || '') + '</span>' +
        '<div class="calendar-row-main"><div class="calendar-row-title">' + escapeHtml(d.title || '') + '</div>' +
        '<div class="muted-xs">' + escapeHtml(where) + (d.meeting_title ? ' · from ' + escapeHtml(d.meeting_title) : '') +
        (d.requested_by ? ' · asked by ' + escapeHtml(d.requested_by) + ' (display name)' : '') +
        (d.auto_approved ? ' · auto-approved' : '') + (d.decided_by && !d.auto_approved ? ' · ' + escapeHtml(status) + ' by ' + escapeHtml(d.decided_by) : '') +
        result + (d.error ? ' · <span class="kc-error">' + escapeHtml(d.error) + '</span>' : '') + '</div>' +
        (d.body ? '<div class="muted-xs">' + escapeHtml(d.body) + '</div>' : '') +
        (d.excerpt ? '<details><summary class="muted-xs">Transcript excerpt</summary><pre class="muted-xs">' + escapeHtml(d.excerpt) + '</pre></details>' : '') +
        (open ? meetingDraftEditHtml(d) : '') + '</div>' +
        (d.meeting_id ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="calendarOpenMeeting" data-arg0="' + escapeHtml(d.meeting_id) + '">Meeting</button>' : '') +
        (open ? btn('approve', 'Approve', 'btn-primary') + btn('reject', 'Reject', 'btn-secondary') : '') + '</div>';
    }

    function renderMeetingDrafts() {
      const host = document.getElementById('meeting-drafts-panel');
      if (!host) return;
      if (_meetingDraftsError) {
        host.innerHTML = '<div class="section-label m-0">Drafts from meetings</div><p class="kc-error muted-xs">' + escapeHtml(_meetingDraftsError) + '</p>';
        return;
      }
      const split = meetingDraftsSplit((_meetingDrafts || {}).drafts);
      host.innerHTML = '<div class="section-label m-0">Drafts from meetings</div>' +
        '<p class="muted-xs">Issues, RFCs, Spek sessions and knowledge notes people asked for in a meeting. Nothing is created until you approve it.</p>' +
        '<div class="calendar-list">' + (split.pending.length ? split.pending.map(meetingDraftRowHtml).join('') : '<p class="muted-xs">No drafts waiting.</p>') + '</div>' +
        (split.decided.length ? '<details><summary class="muted-xs">Recent (' + split.decided.length + ')</summary><div class="calendar-list">' + split.decided.map(meetingDraftRowHtml).join('') + '</div></details>' : '');
    }

    function meetingDraftField(id, name) { return document.querySelector('[data-draft-id="' + CSS.escape(id) + '"][data-draft-field="' + name + '"]'); }

    async function meetingDraftSave(id) {
      const body = {};
      ['type', 'title', 'body', 'repo'].forEach(k => { const el = meetingDraftField(id, k); if (el) body[k] = el.value; });
      try { await calendarPost('/api/meetings/actions/' + encodeURIComponent(id) + '/edit', body); showToast('Draft saved', 'success'); }
      catch (e) { showToast('Could not save the draft: ' + e.message, 'error'); }
      fetchMeetingDrafts();
    }

    async function meetingDraftAct(id, action) {
      try {
        const d = await calendarPost('/api/meetings/actions/' + encodeURIComponent(id) + '/' + encodeURIComponent(action));
        if (action === 'approve') showToast(d && d.status === 'done' ? 'Created' + (d.result_url ? ': ' + d.result_url : '') : 'Not created: ' + ((d && d.error) || 'see the draft'), d && d.status === 'done' ? 'success' : 'error');
      } catch (e) { showToast('Could not ' + action + ': ' + e.message, 'error'); }
      fetchMeetingDrafts();
    }
