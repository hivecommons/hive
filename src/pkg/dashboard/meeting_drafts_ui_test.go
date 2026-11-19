package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestMeetingDraftsWiring pins the "Drafts from meetings" list in the
// Calendar section (hivecommons/hive#18406): the panel, the drafts and
// settings API calls, Approve / Edit / Reject, and the note that nothing is
// created before approval.
func TestMeetingDraftsWiring(t *testing.T) {
	html := indexHTML(t)
	cal := strings.Index(html, `id="calendar-panel"`)
	drafts := strings.Index(html, `id="meeting-drafts-panel"`)
	if cal < 0 || drafts < cal {
		t.Fatalf("the drafts panel must follow the calendar panel: calendar=%d drafts=%d", cal, drafts)
	}
	for _, want := range []string{
		`fetch('/api/meetings/actions')`,
		`fetch('/api/meetings/actions/settings')`,
		`'/api/meetings/actions/' + encodeURIComponent(id) + '/edit'`,
		`'/api/meetings/actions/' + encodeURIComponent(id) + '/' + encodeURIComponent(action)`,
		`btn('approve', 'Approve', 'btn-primary') + btn('reject', 'Reject', 'btn-secondary')`,
		`data-action="meetingDraftSave"`,
		`data-action="meetingDraftAct"`,
		`<summary class="muted-xs">Edit</summary>`,
		`Drafts from meetings`,
		`Nothing is created until you approve it.`,
		`(display name)`,
		`dashboardSetInterval('meeting-drafts', fetchMeetingDrafts, MEETING_DRAFTS_POLL_MS)`,
		// The Meeting attendee tab shows and saves meetings.persona.actions.
		`<h4 id="ma-actions-h">Action requests</h4>`,
		`data-action="maSaveActions"`,
		`fetch('/api/meetings/actions/settings', { method: 'PUT'`,
		`issues, RFCs and Spek sessions always wait`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("drafts wiring missing %q", want)
		}
	}
	// Meeting text is escaped everywhere it is rendered.
	row := jsFunc(t, html, "meetingDraftRowHtml")
	for _, field := range []string{"d.title", "d.body", "d.excerpt", "d.requested_by", "d.meeting_title", "d.error", "d.result_url"} {
		if !strings.Contains(row, "escapeHtml("+field) {
			t.Errorf("%s must be escaped in the draft row", field)
		}
	}
}

// TestMeetingDraftsSplit executes the pending/decided split.
func TestMeetingDraftsSplit(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the split rules were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "meetingDraftsSplit"),
		`let s = meetingDraftsSplit([{id:'a', status:'pending'}, {id:'b', status:'done'}, {id:'c', status:'failed'}, {id:'d', status:'rejected'}, {id:'e', status:'approving'}, null, {status:'pending'}]);`,
		`assert.deepStrictEqual(s.pending.map(d => d.id), ['a', 'c', 'e']);`,
		`assert.deepStrictEqual(s.decided.map(d => d.id), ['b', 'd']);`,
		`s = meetingDraftsSplit(null);`,
		`assert.strictEqual(s.pending.length + s.decided.length, 0);`,
		`console.log('ok');`,
	}, "\n")
	out, err := runNodeStdin(node, script)
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("split rules failed: %v\n%s", err, out)
	}
}
