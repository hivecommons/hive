package dashboard

import (
	"strings"
	"testing"
)

// TestContributeQueueMenuUsesFixedViewportPlacement guards the fix for the
// ready-work queue's per-row "⋯" menu being clipped by the .cc-queue scroll
// container (overflow-y:auto) when a row near the BOTTOM opens its menu — the
// operator reported the move/hold/move-to menu was "behind the scroll area". The
// fix: fixed-position viewport placement shared with the custom-CSS popover, using
// the queue panel as a boundary for flip/clamp decisions.
func TestContributeQueueMenuUsesFixedViewportPlacement(t *testing.T) {
	body := renderContributePage(t)
	mustContain := []string{
		// The queue menu itself escapes the scrolling clip.
		".cc-q-menu{position:fixed",
		// The shared helper places fixed popovers by measuring trigger geometry.
		"function ccPlaceFixedPopover(anchor,pop,opts)",
		"anchor.getBoundingClientRect()",
		// The open handler clamps/flips against the visible scrolling queue panel.
		"ccPlaceFixedPopover(btn,menu,{align:'right',gap:6",
		"boundary:btn.closest('.cc-queue')",
		// Scroll/resize invalidates viewport positioning and closes the open menu.
		"window.addEventListener('resize',function(){ccCloseQueueMenus();});",
		"window.addEventListener('scroll',function(){ccCloseQueueMenus();},true);",
	}
	for _, s := range mustContain {
		if !strings.Contains(body, s) {
			t.Errorf("contribute page missing queue-menu flip-up snippet %q", s)
		}
	}
	// The old absolute-positioned flip class stayed inside the overflow clip and
	// cannot fix the reported bug; keep it out so regressions are obvious.
	if strings.Contains(body, "cc-q-menu-up") {
		t.Errorf("queue menu should use fixed viewport placement, not cc-q-menu-up")
	}
}
