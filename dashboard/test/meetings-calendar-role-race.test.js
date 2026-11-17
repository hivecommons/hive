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
  'meetingsFinished', 'meetingsCanDelete', 'meetingsDeleteButtonHtml'];
const CALENDAR = ['calendarRoleKnown', 'calendarOnRoleReady', 'calendarCanView', 'calendarIsOwner', 'fetchCalendar', 'calendarRefreshMeetings',
  'fetchMeetingDrafts', 'renderCalendar', 'renderMeetingDrafts', 'meetingDraftsSplit', 'calendarFormHtml', 'calendarViewerTimeZone',
  'calendarRowHtml', 'calendarFormatTime', 'calendarPlatformLabel', 'calendarMeetingLink', 'calendarActionButtons', 'renderCalendarIndicator',
  'calendarIndicatorState', 'calendarBadgeCount', 'calendarMeetingList', 'calendarCanDelete'];

function makeSandbox(opts) {
  const els = {};
  const el = (id) => (els[id] = els[id] || { id, innerHTML: '', dataset: {}, querySelector: () => null, setAttribute() {} });
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
    _meetingsList: null, _meetingsError: '', _meetingsOpen: null,
    _calendarData: null, _calendarError: '', _calendarMeetings: {},
    _meetingDrafts: null, _meetingDraftsError: '', _meetingDraftRepos: ['o/r'],
    console, Math, Number, Array, Error, Promise, String, Object, Date, Intl, isNaN,
  };
  vm.createContext(sb);
  const consts = ['MEETINGS_ERROR_INLINE_CHARS', 'MEETINGS_PLATFORM_LABELS', 'MEETINGS_PILL_STATUS', 'MEETINGS_LIVE_STATES',
    'CALENDAR_ACTIVE_STATES', 'CALENDAR_DEFAULT_NAME', 'CALENDAR_PLATFORM_LABELS', 'CALENDAR_PLATFORM_ICONS', 'CALENDAR_SOURCE_LABELS',
    'CALENDAR_STATUS_LABELS', 'CALENDAR_PILL_STATUS', 'MEETINGS_FINAL_STATES', 'CALENDAR_FINISHED_STATUSES'].map(extractConst);
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

test('while the role is unknown both panels show loading, not the not-allowed notice, and fetch nothing', async () => {
  const sb = makeSandbox({});
  await sb.fetchMeetings();
  await sb.fetchCalendar();
  assert.match(sb.els['meetings-panel'].innerHTML, /Loading meetings/);
  assert.match(sb.els['calendar-panel'].innerHTML, /Loading calendar/);
  assert.doesNotMatch(sb.els['meetings-panel'].innerHTML + sb.els['calendar-panel'].innerHTML, /visible to the hive owner/);
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
  assert.match(sb.els['calendar-panel'].innerHTML, /Nothing scheduled/);
  assert.match(sb.els['meeting-drafts-panel'].innerHTML, /No drafts waiting/);
  assert.doesNotMatch(sb.els['meetings-panel'].innerHTML, /visible to the hive owner/);
});

test('role arriving as read shows the not-allowed notices', async () => {
  const sb = makeSandbox({});
  sb.window._hiveRole = 'read';
  sb.hiveRoleSettled(false);
  await settle();
  assert.match(sb.els['meetings-panel'].innerHTML, /visible to the hive owner only/);
  assert.match(sb.els['calendar-panel'].innerHTML, /visible to the hive owner and read-write users/);
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
