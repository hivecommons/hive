package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestMeetingsVoiceToggleNamesTheAction pins the live Teams finding: the
// meeting card's button read "Voice on" (the state) but clicking it turned
// voice off, so an operator who had ticked Voice at join switched it off
// thinking it was the control to turn it on. The button now names what a
// click does, and carries the state in aria-pressed.
func TestMeetingsVoiceToggleNamesTheAction(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "(active ? meetingsVoiceToggleHtml(m) +") {
		t.Fatal("the meeting card must render its voice control through meetingsVoiceToggleHtml")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; the wiring assertion above still ran")
	}
	script := `const escapeHtml = s => String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');
` + extractJSFunction(t, html, "meetingsVoiceToggleHtml") + "\n    }\n" + `
const assert = require('assert');
const on = meetingsVoiceToggleHtml({ id: 'mtg-1', voice_enabled: true });
assert.match(on, />🔊 Turn voice off</, 'voice on: the label is the action, turning it off');
assert.match(on, /data-arg1="false"/, 'voice on: a click turns it off');
assert.match(on, /aria-pressed="true"/);
assert.ok(!/>[^<]*Voice on</.test(on), 'never the bare state as the label');
const off = meetingsVoiceToggleHtml({ id: 'mtg-1', voice_enabled: false });
assert.match(off, />🔇 Turn voice on</);
assert.match(off, /data-arg1="true"/);
assert.match(off, /aria-pressed="false"/);
assert.match(off, /data-arg0="mtg-1"/);
console.log('ok');
`
	raw, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil || !strings.Contains(string(raw), "ok") {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
}
