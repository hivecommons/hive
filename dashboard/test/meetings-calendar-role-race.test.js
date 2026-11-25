'use strict';

// The Meetings and Calendar panels must not show their "not allowed" notice
// while /api/role is unresolved, and must fetch and render the moment the
// role arrives (same race the Inbox fixed in #18442). They must also sit in
// the dashboard layout template, or layout.js leaves them outside
// #hive-dashboard-root and the card loses its padding tokens. Functions are
// extracted from the generated dashboard page.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

function extractFunction(name) {
  const start = indexHtml.indexOf('function ' + name + '(');
  assert.notEqual(start, -1, `${name} function exists`);
  const brace = indexHtml.indexOf('{', start);
  let depth = 0;
  for (let i = brace; i < indexHtml.length; i++) {
    const ch = indexHtml[i];
    if (ch === '{') depth++;
    if (ch === '}') depth--;
    if (depth === 0) return (indexHtml.slice(start - 6, start) === 'async ' ? 'async ' : '') + indexHtml.slice(start, i + 1);
  }
  throw new Error(`unterminated ${name}`);
}

function extractConst(name) {
  const m = new RegExp('const ' + name + ' = [^;]+;').exec(indexHtml);
  assert.ok(m, `${name} const exists`);
  return m[0];
}

const GH_AUTH = ['hiveRoleSettled'];
const MEETINGS = ['meetingsIsOwner', 'meetingsRoleKnown', 'meetingsOnRoleReady', 'fetchMeetings', 'renderMeetings', 'meetingsJoinFormHtml',
  'meetingsRowHtml', 'meetingsFormatTime', 'meetingsFriendlyReason', 'meetingsReasonHtml', 'meetingsLabel',
  'meetingsFinished', 'meetingsCanDelete', 'meetingsDeleteButtonHtml', 'fetchMeetingsBudget', 'meetingsBudgetHtml', 'meetingsBudgetText', 'meetingsTrustedHtml', 'rowChevronHtml'];
const CALENDAR = ['calendarRoleKnown', 'calendarOnRoleReady', 'calendarCanView', 'calendarIsOwner', 'fetchCalendar', 'calendarRefreshMeetings',
  'fetchMeetingDrafts', 'renderCalendar', 'renderMeetingDrafts', 'meetingDraftsSplit', 'calendarFormHtml', 'calendarField', 'calendarViewerTimeZone',
  'calendarRowHtml', 'calendarFormatTime', 'calendarPlatformLabel', 'calendarMeetingLink', 'calendarActionButtons', 'renderCalendarIndicator',
  'calendarIndicatorState', 'calendarBadgeCount', 'calendarMeetingList', 'calendarCanDelete',
  'calendarDisplayTitle', 'calendarLooksForwarded', 'calendarRepeatText', 'calendarFormatRange', 'calendarWhen', 'calendarApprovalWhy'];

function makeSandbox(opts) {
  const els = {};
  const el = (id) => (els[id] = els[id] || { id, innerHTML: '', dataset: {}, querySelector: () => null, setAttribute() {} });
  el('calendar-email-in').hidden = true;
  el('calendar-add-form').hidden = true;
  const calls = [];
  const sb = {
    window: { _hiveRoleListeners: [] },
    document: { getElementById: (id) => el(id), querySelector: () => null },
    els,
    calls,
    escapeHtml: (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'),
    refreshSectionCardShell: () => {},
    setSectionSummary: () => {},
    setSidebarCountBadge: () => {},
    renderMeetingsLive: () => {},
    dashboardRoleAtLeast: (role, min) => ['read', 'read-write', 'owner'].indexOf(role) >= ['read', 'read-write', 'owner'].indexOf(min),
    fetch: async (url) => {
      calls.push(url);
      if (opts && opts.fetch) return opts.fetch(url);
      if (url === '/api/meetings') return { ok: true, status: 200, json: async () => ({ meetings: (opts && opts.meetings) || [] }) };
      if (url === '/api/meetings/schedule') return { ok: true, status: 200, json: async () => ({ upcoming: [], recent: [] }) };
      if (url === '/api/meetings/actions') return { ok: true, status: 200, json: async () => ({ drafts: [] }) };
      return { ok: true, status: 200, json: async () => ({}) };
    },
    _meetingsList: null, _meetingsError: '', _meetingsOpen: null, _meetingsFocusRow: '', _meetingsBudget: {}, _meetingsLedger: null, _meetingsTrusted: [],
    _calendarData: null, _calendarError: '', _calendarMeetings: {},
    _meetingDrafts: null, _meetingDraftsError: '', _meetingDraftRepos: ['o/r'],
    console, Math, Number, Array, Error, Promise, String, Object, Date, Intl, isNaN,
  };
  vm.createContext(sb);
  const consts = ['MEETINGS_ERROR_INLINE_CHARS', 'MEETINGS_PLATFORM_LABELS', 'MEETINGS_PILL_STATUS', 'MEETINGS_LIVE_STATES', 'MEETINGS_BUDGET_BADGES',
    'CALENDAR_ACTIVE_STATES', 'CALENDAR_DEFAULT_NAME', 'CALENDAR_PLATFORM_LABELS', 'CALENDAR_PLATFORM_ICONS', 'CALENDAR_SOURCE_LABELS',
    'CALENDAR_STATUS_LABELS', 'CALENDAR_PILL_STATUS', 'CALENDAR_FWD_PREFIX', 'MEETINGS_FINAL_STATES', 'CALENDAR_FINISHED_STATUSES'].map(extractConst);
  // const declarations stay script-scoped in a vm context; re-declare the
  // mutable module state with var so the extracted functions share it.
  vm.runInContext(consts.join('\n') + '\n' + GH_AUTH.concat(MEETINGS, CALENDAR).map(extractFunction).join('\n'), sb);
  // Same registrations meetings.js and calendar.js perform at load.
  sb.window._hiveRoleListeners.push(sb.meetingsOnRoleReady, sb.calendarOnRoleReady);
  return sb;
}

const settle = () => new Promise((r) => setImmediate(r));

test('meetings and calendar are in the dashboard layout template', () => {
  const m = /var DASHBOARD_LAYOUT_TEMPLATE=\{main:\[([^\]]+)\]\}/.exec(indexHtml);
  assert.ok(m, 'layout template found');
  assert.match(m[1], /'inbox-section','calendar-section','meetings-section'/);
});

test('calendar link label follows the selected meeting after moving back to an earlier occurrence', () => {
  const sb = makeSandbox({});
  const link = sb.calendarMeetingLink({
    meeting_id: 'earlier',
    status: 'ended',
    history: [{ meeting_id: 'earlier', status: 'ended' }, { meeting_id: 'later', status: 'live' }],
  });
  assert.match(link, /data-arg0="earlier">Notes</);
});

test('while the role is unknown both panels show loading, not the not-allowed notice, and fetch nothing', async () => {
  const sb = makeSandbox({});
  await sb.fetchMeetings();
  await sb.fetchCalendar();
  assert.match(sb.els['meetings-panel'].innerHTML, /Loading meetings/);
  assert.match(sb.els['calendar-results'].innerHTML, /Loading calendar/);
  assert.equal(sb.els['calendar-add-form'].hidden, true);
  assert.doesNotMatch(sb.els['meetings-panel'].innerHTML + sb.els['calendar-results'].innerHTML, /visible to the hive owner/);
  assert.deepEqual(sb.calls, []);
});

test('role arriving as owner fetches and renders the join form and the calendar', async () => {
  const sb = makeSandbox({});
  await sb.fetchMeetings();
  await sb.fetchCalendar();
  sb.window._hiveRole = 'owner';
  sb.hiveRoleSettled(false);
  await settle();
  await settle();
  assert.ok(sb.calls.includes('/api/meetings'));
  assert.ok(sb.calls.includes('/api/meetings/schedule'));
  assert.match(sb.els['meetings-panel'].innerHTML, /data-action="meetingsJoin"/);
  assert.match(sb.els['meetings-panel'].innerHTML, /No meetings yet/);
  assert.match(sb.els['calendar-results'].innerHTML, /Nothing scheduled/);
  assert.equal(sb.els['calendar-add-form'].hidden, false);
  assert.match(sb.els['calendar-add-form'].innerHTML, /data-action="calendarAdd"/);
  assert.match(sb.els['meeting-drafts-panel'].innerHTML, /No drafts waiting/);
  assert.doesNotMatch(sb.els['meetings-panel'].innerHTML, /visible to the hive owner/);
});

test('role arriving as read shows the not-allowed notices', async () => {
  const sb = makeSandbox({});
  sb.window._hiveRole = 'read';
  sb.hiveRoleSettled(false);
  await settle();
  assert.match(sb.els['meetings-panel'].innerHTML, /visible to the hive owner only/);
  assert.match(sb.els['calendar-results'].innerHTML, /visible to the hive owner and read-write users/);
  assert.equal(sb.els['calendar-add-form'].hidden, true);
  assert.equal(sb.calls.length, 0);
});

test('a network failure says so instead of staying on Loading meetings', async () => {
  const sb = makeSandbox({ fetch: async () => { throw new Error('network'); } });
  sb.window._hiveRole = 'owner';
  await sb.fetchMeetings();
  assert.doesNotMatch(sb.els['meetings-panel'].innerHTML, /Loading meetings/);
  assert.match(sb.els['meetings-panel'].innerHTML, /unreachable right now/);
});

test('a failed meeting row shows a pill, a friendly line and the raw error under details', async () => {
  const raw = 'quorum meeting.join: invalid_argument: Vexa POST /bots answered 422: unsupported platform \'zoom\' without a meeting_url';
  const sb = makeSandbox({ meetings: [{ id: 'm1', title: 'Live test', platform: 'zoom', state: 'failed', state_reason: raw, created_at: '2026-11-16T15:00:00Z' }] });
  sb.window._hiveRole = 'owner';
  await sb.fetchMeetings();
  const html = sb.els['meetings-panel'].innerHTML;
  assert.match(html, /<span class="kc-pill" data-status="error">failed<\/span>/);
  assert.match(html, /class="meetings-row-error">The meeting service did not accept this meeting link\./);
  assert.match(html, /<summary class="muted-xs">details<\/summary><div class="meetings-row-raw">quorum meeting\.join/);
  assert.match(html, /Zoom · /);
});

test('calendar row: forwarded prefix stripped for display, series leads with next occurrence, approval reason in words', () => {
  const sb = makeSandbox({});
  sb.window._hiveRole = 'owner';
  assert.equal(sb.calendarDisplayTitle('FW: Fwd: Hive Commons Community Meeting'), 'Hive Commons Community Meeting');
  assert.equal(sb.calendarDisplayTitle('Forward planning'), 'Forward planning');
  assert.equal(sb.calendarDisplayTitle('FW:'), 'FW:');
  assert.equal(sb.calendarRepeatText('FREQ=WEEKLY;BYDAY=TH'), 'repeats weekly');
  assert.equal(sb.calendarRepeatText('FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR'), 'repeats every weekday');
  assert.equal(sb.calendarRepeatText('FREQ=DAILY;INTERVAL=2'), 'repeats every 2 days');
  const ev = {
    id: 'e1', summary: 'FW: Hive Commons Community Meeting', status: 'awaiting_approval', source: 'email', platform: 'teams',
    start: '2026-09-10T14:00:00Z', end: '2026-09-10T15:00:00Z', rrule: 'FREQ=WEEKLY', next_occurrences: ['2026-11-19T15:00:00Z']
  };
  const html = sb.calendarRowHtml(ev);
  assert.match(html, /Hive Commons Community Meeting<\/div>/);
  assert.doesNotMatch(html, />FW:/);
  assert.match(html, /calendar-row-when" title="Series started /);
  assert.match(html, /Next: .*· repeats weekly/);
  assert.doesNotMatch(html, /· repeats ·/);
  assert.match(html, /looks forwarded/);
  assert.match(html, /DMARC aligned/);
  assert.match(html, /calendar-row-actions/);
  assert.equal(sb.calendarApprovalWhy({ status: 'scheduled', source: 'email' }), '');
});

test('the meeting row shows the budget meter with a text alternative and a badge, never question text', async () => {
  const sb = makeSandbox({});
  const b = { meeting_id: 'm1', tokens_used: 124000, token_budget: 200000, percent: 62, intents_answered: 15, level: 'ok', skipped: 0 };
  const ok = sb.meetingsBudgetHtml(b);
  assert.match(ok, /aria-label="62% of the meeting budget used"/);
  assert.match(ok, /<meter /);
  assert.doesNotMatch(ok, /kc-pill/);
  const warn = sb.meetingsBudgetHtml({ ...b, percent: 85, level: 'warning', skipped: 2 });
  assert.match(warn, /budget 80%/);
  assert.match(warn, /2 questions not answered/);
  assert.match(sb.meetingsBudgetHtml({ ...b, percent: 100, level: 'exhausted' }), /budget exhausted/);
  assert.equal(sb.meetingsBudgetHtml(null), '');
});
