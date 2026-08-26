package dashboard

import (
	"strings"
	"testing"
)

// Regression guard for the budget "reset window now" button: the success path
// once called an undefined renderAll(), so Safari threw "Can't find variable:
// renderAll" inside the surrounding try/catch and the operator saw BOTH a
// success toast and a "Failed to reset budget window" error toast for a reset
// that had actually succeeded server-side (v4 build 9f31439).
func TestBudgetResetHandlerHasNoStaleCallees(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	// Stale callees: functions that were never defined in any inline script
	// block. Calling them throws ReferenceError at click time, which nearby
	// catch blocks then mislabel as an action failure.
	for _, stale := range []string{"renderAll(", "hiveToast("} {
		if strings.Contains(html, stale) {
			t.Errorf("index.html calls %q, which is never defined — use refreshStatus()/showToast() instead", stale)
		}
	}

	// The reset handler must refresh the budget bar/banners in place after a
	// successful reset (no page reload), and the refresh must not be able to
	// surface as a reset failure.
	for _, snippet := range []string{
		"function refreshStatus()",
		"Budget window reset — spend restarts at 0, suppressed kicks resume",
		"refreshStatus(); // repaint budget bar/banners in place; errors swallowed inside",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
}
