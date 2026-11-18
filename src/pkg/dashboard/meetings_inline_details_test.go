package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestMeetingsDetailsOpenInPlace pins the Meetings row: a chevron button left
// of the row (the control Customers shares) opens the details under the row,
// and the Spektacular status is one notice with a linked issue.
func TestMeetingsDetailsOpenInPlace(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"rowChevronHtml('meetingsToggle', m.id, open,",
		`class="meetings-details" id="`,
		"function meetingsToggle(id)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard lacks %q", want)
		}
	}
	if strings.Contains(html, "data-action=\"meetingsOpenPanel\"") {
		t.Fatal("the row must not carry the old Open button")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; the wiring assertions above still ran")
	}
	script := `const escapeHtml = s => String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
` + extractJSFunction(t, html, "rowChevronHtml") + "\n    }\n" + extractJSFunction(t, html, "meetingsSpekNoticeHtml") + "\n    }\n" + `
const assert = require('assert');
const closed = rowChevronHtml('meetingsToggle', 'm1', false, 'details of x', 'meetings-details-m1');
assert.match(closed, /class="row-chevron"/);
assert.match(closed, /aria-expanded="false"/);
assert.ok(!/aria-controls/.test(closed), 'a closed row controls no region');
const open = rowChevronHtml('meetingsToggle', 'm1', true, 'details of x', 'meetings-details-m1');
assert.match(open, /aria-expanded="true"/);
assert.match(open, /aria-controls="meetings-details-m1"/);
assert.match(open, /aria-label="Collapse details of x"/);
const n = meetingsSpekNoticeHtml('Notes and clarifying questions are off for this meeting: Spektacular live mode is not running (it needs hivecommons/spektacular#126). The transcript still works.');
assert.match(n, /meetings-notice-title">Notes and clarifying questions are off for this meeting</);
assert.match(n, /href="https:\/\/github.com\/hivecommons\/spektacular\/issues\/126"/);
assert.ok(!n.includes('<script'), 'text is escaped');
console.log('ok');
`
	raw, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil || !strings.Contains(string(raw), "ok") {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
}
