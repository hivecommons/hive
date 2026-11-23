package dashboard

import (
	"strings"
	"testing"
)

// On phones the top bar's right strip clipped its overflow while packed to
// the end, so chips past the start edge were cut off with no way to reach
// them: "Waiting on you · 17" showed as "u · 17", and Report a bug and
// Settings were gone. The strip now scrolls, starts at the end, drops the two
// chips the menus already carry, and the waiting chip shows only its count.
func TestTopbarPhoneStripScrollsInsteadOfClipping(t *testing.T) {
	html := indexHTML(t)
	start := strings.Index(html, "@media (max-width: 768px) {")
	if start < 0 {
		t.Fatal("static dashboard is missing the 768px phone block")
	}
	block := html[start:]
	if end := strings.Index(block, "@media (max-width: 1024px) and (max-height: 500px)"); end > 0 {
		block = block[:end]
	}
	for _, want := range []string{
		".oc-topbar-right {\n        justify-content: flex-start;\n        flex-wrap: nowrap;\n        overflow-x: auto;",
		".oc-topbar-right::before {\n        content: \"\";\n        flex: 1 0 0;",
		"#feedback-bug-btn,\n      .oc-topbar-right:has(#oc-gh-avatar-wrap:not([hidden])) #oc-settings-btn {\n        display: none;",
		".oc-waiting-on-you .nav-chip__label {\n        display: none;",
		".oc-waiting-on-you::after {\n        content: attr(data-count);",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("768px phone block is missing %q", want)
		}
	}
	if strings.Contains(block, "justify-content: flex-end;\n        flex-wrap: nowrap;") {
		t.Error("the phone right strip is packed to the end again; its overflow would be cut off past the start edge")
	}
	for _, want := range []string{
		`id="oc-waiting-on-you" data-action="showWaitingOnYou" data-count="0"`,
		"pill.setAttribute('data-count', String(w.count));",
		"function initTopbarStripScroll() {",
		"initTopbarStripScroll();",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("static dashboard is missing %q", want)
		}
	}
}
