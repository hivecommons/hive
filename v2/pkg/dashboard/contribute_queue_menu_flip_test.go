package dashboard

import (
	"strings"
	"testing"
)

// TestContributeQueueMenuFlipsUp guards the fix for the ready-work queue's per-row
// "⋯" menu being clipped by the .cc-queue scroll container (overflow-y:auto) when a
// row near the BOTTOM opens its menu — the operator reported the move/hold menu was
// "behind the scroll borders" ("Optional hold reason" cut off). The fix: a
// .cc-q-menu-up variant anchored to bottom:100%, applied by the open handler when
// the menu would extend past the container's clip boundary.
func TestContributeQueueMenuFlipsUp(t *testing.T) {
	body := renderContributePage(t)
	mustContain := []string{
		// The flip-up CSS variant exists and anchors upward.
		".cc-q-menu.cc-q-menu-up{top:auto;bottom:100%",
		// The open handler measures against the scrolling .cc-queue clip box.
		"btn.closest('.cc-queue')",
		// ...and applies the up-variant class when there's no room below.
		"menu.classList.add('cc-q-menu-up')",
	}
	for _, s := range mustContain {
		if !strings.Contains(body, s) {
			t.Errorf("contribute page missing queue-menu flip-up snippet %q", s)
		}
	}
	// Closing a menu must also clear the up-variant so a reopened row isn't stuck flipped.
	if !strings.Contains(body, "classList.remove('cc-q-menu-up')") {
		t.Errorf("ccCloseQueueMenus must clear cc-q-menu-up on close")
	}
}
