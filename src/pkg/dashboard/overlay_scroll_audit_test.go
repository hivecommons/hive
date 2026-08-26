package dashboard

import (
	"strings"
	"testing"
)

// TestOverlayScrollContainmentAudit pins the scroll-chaining containment on
// every overlay-like surface found by the #4805 audit — not just the config
// modal family covered by #2766/#4803. Each rule below stops a wheel/touch
// that reaches a scroller's boundary from chaining into the dashboard behind
// the overlay.
func TestOverlayScrollContainmentAudit(t *testing.T) {
	html := indexHTML(t)

	// Every modal backdrop carries containment (previously only .config-overlay).
	backdropRule := []string{
		".config-overlay,",
		".gh-auth-overlay,",
		".cred-dialog-overlay,",
		".hive-dialog-overlay,",
		".nous-config-overlay,",
		".acmm-dialog-overlay {",
	}
	for _, snippet := range backdropRule {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q from the shared backdrop containment rule (#4805)", snippet)
		}
	}

	// Scrollable surfaces inside the non-config modals.
	modalScrollers := []string{
		".gh-setup-modal,",
		".acmm-dialog,",
		".nous-config-dialog > .nous-config-body,",
		".nous-config-overlay textarea,",
		".nous-config-overlay pre,",
		".welcome-body,",
		".config-stats-list {",
	}
	for _, snippet := range modalScrollers {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q from the modal-scroller containment rule (#4805)", snippet)
		}
	}

	// The floating (non-modal) chat panel: the body lock never applies to it,
	// so containment is the only defense. Each of its scrollers must carry the
	// containment inside its own rule.
	for rule, name := range map[string]string{
		".hive-chat-panel {":    "chat panel",
		".hive-chat-messages {": "chat message history",
		".chat-raw-details pre": "chat raw-response pre",
		".hive-chat-input {":    "chat input textarea",
		".oc-sidebar {":         "fixed sidebar",
	} {
		idx := strings.Index(html, rule)
		if idx < 0 {
			t.Errorf("index.html has no %q rule (%s)", rule, name)
			continue
		}
		end := strings.Index(html[idx:], "}")
		if end < 0 {
			t.Errorf("%q rule is unterminated", rule)
			continue
		}
		if !strings.Contains(html[idx:idx+end], "overscroll-behavior") {
			t.Errorf("%q rule (%s) is missing overscroll-behavior containment — its scroll boundary chains to the dashboard (#4805)", rule, name)
		}
	}
}

// TestModalScrollLockObservesLateOverlays pins the installModalScrollLock fix
// for overlays created after startup (#4805): kbShowModal's #kb-modal-overlay
// and the acmm dialog are appended once and then toggled via style.display.
// Without re-attaching attribute observers on childList mutations, closing one
// left body.modal-open stuck (page frozen) and reopening never re-locked
// (scroll bled behind the modal from the second open on).
func TestModalScrollLockObservesLateOverlays(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function observeOverlays() {",
		"if (muts[i].type === 'childList') { observeOverlays(); break; }",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — late-created overlays escape the shared body scroll-lock (#4805)", snippet)
		}
	}
	// The acmm dialog must rely on the shared lock, not a second bespoke
	// body.style.overflow mechanism that fights the class-based lock.
	idx := strings.Index(html, "function acmmShowDialog(")
	end := strings.Index(html, "function acmmCloseDialog(")
	if idx < 0 || end < 0 || end < idx {
		t.Fatal("cannot locate acmmShowDialog/acmmCloseDialog in index.html")
	}
	closeEnd := end + strings.Index(html[end:], "\n    }")
	if strings.Contains(html[idx:closeEnd], "document.body.style.overflow") {
		t.Error("acmm dialog manages body.style.overflow itself — a second scroll-lock mechanism alongside installModalScrollLock (#4805)")
	}
}
