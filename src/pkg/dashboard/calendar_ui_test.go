package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCalendarSectionWiring pins the Calendar section (hivecommons/hive#18394):
// the sidebar entry directly after Inbox with its badge, the section placed
// after Inbox, its card config, the schedule API calls, the add form, the
// approve/decline/cancel/skip actions and the live/notes link.
func TestCalendarSectionWiring(t *testing.T) {
	html := indexHTML(t)
	inboxNav := strings.Index(html, `data-section="inbox-section" data-action="ocNavigate"`)
	calNav := strings.Index(html, `data-section="calendar-section" data-action="ocNavigate"`)
	meetNav := strings.Index(html, `data-section="meetings-section" data-action="ocNavigate"`)
	if inboxNav < 0 || calNav < inboxNav || meetNav < calNav {
		t.Fatalf("Calendar must sit right after Inbox in the left menu: inbox=%d calendar=%d meetings=%d", inboxNav, calNav, meetNav)
	}
	between := html[inboxNav:calNav]
	if strings.Count(between, `class="oc-nav-item"`) != 1 {
		t.Fatal("another menu entry sits between Inbox and Calendar")
	}
	inboxSec := strings.Index(html, `id="inbox-section" data-dashboard-section="inbox-section"`)
	calSec := strings.Index(html, `id="calendar-section" data-dashboard-section="calendar-section"`)
	if inboxSec < 0 || calSec < inboxSec {
		t.Fatalf("Calendar section must follow Inbox: inbox=%d calendar=%d", inboxSec, calSec)
	}
	for _, want := range []string{
		`id="calendar-sidebar-badge" class="badge-count oc-nav-count" data-zero hidden`,
		`id="calendar-panel"`,
		`'calendar-section': { title: '📅 Calendar', summary: 'nothing scheduled' }`,
		`'calendar-section': { heading: 'Calendar', help:`,
		`fetch('/api/meetings/schedule')`,
		`calendarPost('/api/meetings/schedule', body)`,
		`'/api/meetings/schedule/' + encodeURIComponent(id) + '/' + encodeURIComponent(action)`,
		`btn('approve', 'Approve', 'btn-primary') + btn('decline', 'Decline', 'btn-secondary')`,
		`btn('cancel', 'Cancel', 'btn-danger')`,
		`data-action="calendarOpenMeeting"`,
		`(live ? 'Open live panel' : 'Notes')`,
		`data-calendar-field="start"`,
		`type="datetime-local"`,
		`timezone: calendarViewerTimeZone()`,
		`Intl.DateTimeFormat().resolvedOptions().timeZone`,
		`setSectionSummary('calendar-section'`,
		`refreshSectionCardShell('calendar-section')`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Calendar wiring missing %q", want)
		}
	}
}

// TestCalendarMeetingIndicatorWiring pins the top-bar indicator: hidden by
// default, driven by /api/events "meeting" frames over GET /api/meetings.
func TestCalendarMeetingIndicatorWiring(t *testing.T) {
	topbar := dashboardTopbarHTML(t)
	if !strings.Contains(topbar, `id="oc-meeting-indicator" data-action="calendarIndicatorClick"`) ||
		!strings.Contains(topbar, `aria-label="Not in a meeting" hidden>`) {
		t.Fatal("the meeting indicator must be in the top bar and hidden by default")
	}
	html := indexHTML(t)
	for _, want := range []string{
		`if (typeof calendarOnMeetingFrame === 'function') {`,
		`calendarOnMeetingFrame(JSON.parse(e.data || '{}'))`,
		`const r = await fetch('/api/meetings');`,
		`ocNavigate('calendar-section')`,
		`meetingsOpenPanel(id)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("indicator wiring missing %q", want)
		}
	}
}

// TestCalendarIndicatorStates executes the indicator and badge rules: none,
// one live, two live, lobby, and the badge counting pending approvals.
func TestCalendarIndicatorStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the indicator rules were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "CALENDAR_ACTIVE_STATES"),
		jsConstLine(t, html, "CALENDAR_DEFAULT_NAME"),
		jsConstLine(t, html, "CALENDAR_PLATFORM_LABELS"),
		jsConstLine(t, html, "CALENDAR_PLATFORM_ICONS"),
		jsFunc(t, html, "calendarPlatformLabel"),
		jsFunc(t, html, "calendarIndicatorState"),
		jsFunc(t, html, "calendarBadgeCount"),
		`let s = calendarIndicatorState([], 'Betty');`,
		`assert.strictEqual(s.hidden, true); assert.strictEqual(s.count, 0);`,
		`s = calendarIndicatorState([{id:'m1', state:'ended'}, {id:'m2', state:'failed'}, {id:'m3', state:'requested'}], 'Betty');`,
		`assert.strictEqual(s.hidden, true, 'ended, failed and requested meetings hide the indicator');`,
		`s = calendarIndicatorState([{id:'m1', state:'live', platform:'google_meet', title:'Weekly sync'}], 'Betty');`,
		`assert.strictEqual(s.text, '🐝 Betty is in a meeting');`,
		`assert.strictEqual(s.target, 'm1');`,
		`assert.ok(s.title.includes('Google Meet: Weekly sync'), s.title);`,
		`s = calendarIndicatorState([{id:'m1', state:'live', platform:'zoom'}, {id:'m2', state:'live', platform:'teams', title:'Retro'}], 'Betty');`,
		`assert.strictEqual(s.text, '🐝 Betty is in 2 meetings');`,
		`assert.strictEqual(s.target, 'calendar-section');`,
		`assert.ok(s.title.includes('Teams: Retro') && s.title.includes('Zoom:'), s.title);`,
		`s = calendarIndicatorState([{id:'m4', state:'lobby', platform:'teams', title:'1:1'}], '');`,
		`assert.strictEqual(s.text, '🐝 Hive is in a meeting');`,
		`assert.ok(s.title.includes('(lobby)'), s.title);`,
		`s = calendarIndicatorState([{id:'m5', state:'joining', platform:'discord', channel_id:'42'}], 'Betty');`,
		`assert.strictEqual(s.count, 1); assert.ok(s.title.includes('Discord: 42'), s.title);`,
		`assert.strictEqual(calendarBadgeCount([], []), 0);`,
		`assert.strictEqual(calendarBadgeCount([{state:'live'}, {state:'ended'}], [{status:'awaiting_approval'}, {status:'scheduled'}, {status:'awaiting_approval'}]), 3);`,
		`assert.strictEqual(calendarBadgeCount(null, null), 0);`,
		`console.log('ok');`,
	}, "\n")
	out, err := runNodeStdin(node, script)
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("indicator rules failed: %v\n%s", err, out)
	}
}

func TestCalendarRefreshPreservesMeetingDraft(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the calendar refresh behavior was not executed")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('node:assert/strict');",
		jsFunc(t, html, "calendarRoleKnown"),
		jsFunc(t, html, "calendarCanView"),
		jsFunc(t, html, "calendarIsOwner"),
		jsFunc(t, html, "calendarFormHtml"),
		jsFunc(t, html, "calendarField"),
		jsFunc(t, html, "fetchCalendar"),
		jsFunc(t, html, "renderCalendar"),
		"const window = { _hiveRole: 'owner' };",
		"let _calendarData = null, _calendarError = '';",
		"function calendarViewerTimeZone() { return 'UTC'; }",
		"function escapeHtml(s) { return s; }",
		"function renderCalendarIndicator() {}",
		"function refreshSectionCardShell() {}",
		"function setSectionSummary() {}",
		"function calendarRowHtml(event) { return event.summary; }",
		"const add = { hidden: true, set innerHTML(html) { this.draft = html ? { open: false, title: '', hint: {} } : null; } };",
		"const email = {}, results = {};",
		"const panel = { set innerHTML(html) { add.innerHTML = html.includes('Add meeting') ? html : ''; results.innerHTML = html; } };",
		"const nodes = { 'calendar-panel': panel, 'calendar-email-in': email, 'calendar-add-form': add, 'calendar-results': results };",
		"const document = { getElementById: id => nodes[id], querySelector: () => add.draft?.hint };",
		"let response = { ok: true, json: async () => ({ upcoming: [] }) };",
		"async function fetch() { return response; }",
		"(async () => {",
		"  await fetchCalendar();",
		"  const draft = add.draft;",
		"  assert.ok(draft, 'initial fetch renders the add form');",
		"  draft.open = true;",
		"  draft.title = 'In progress';",
		"  response = { ok: true, json: async () => ({ upcoming: [{ summary: 'New event' }] }) };",
		"  await fetchCalendar();",
		"  assert.match(results.innerHTML, /New event/);",
		"  assert.equal(add.draft, draft, 'a refresh must keep the same form');",
		"  response = { ok: false, status: 503, json: async () => ({ error: 'Try again' }) };",
		"  await fetchCalendar();",
		"  assert.match(results.innerHTML, /Try again/);",
		"  assert.equal(add.draft, draft, 'an error must keep the same form');",
		"  assert.equal(draft.open, true);",
		"  assert.equal(draft.title, 'In progress');",
		"  window._hiveRole = 'read';",
		"  await fetchCalendar();",
		"  assert.equal(add.hidden, true);",
		"  assert.equal(add.draft, null, 'revoked access clears the form');",
		"})().catch(err => { console.error(err); process.exitCode = 1; });",
	}, "\n")
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("calendar refresh failed: %v\n%s", err, out)
	}
}
