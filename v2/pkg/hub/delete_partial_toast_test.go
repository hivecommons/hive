package hub

import (
	"strings"
	"testing"
)

// TestPartialDeleteRendersAsInfoNotError guards the Thread B UX fix: when the
// hub returns status "partially_deleted", the delete SUCCEEDED (the registry
// row is gone) but some cloud resources may survive. The dashboard's deleteHive
// used to render that outcome with an 'error' toast, so a partial success looked
// exactly like a failed delete even though the row had vanished. This asserts
// the partially_deleted branch renders as an 'info' toast, not 'error'.
func TestPartialDeleteRendersAsInfoNotError(t *testing.T) {
	// Isolate the partially_deleted branch: from the `if (ok.status ===
	// 'partially_deleted')` test to the `else` that follows it. Bounding on the
	// else keeps an unrelated 'error' toast elsewhere in deleteHive (the
	// !resp.ok path) from masking a regression here.
	const branchStart = "if (ok.status === 'partially_deleted') {"
	start := strings.Index(dashboardHTML, branchStart)
	if start < 0 {
		t.Fatal("deleteHive no longer handles the partially_deleted status")
	}
	rest := dashboardHTML[start+len(branchStart):]
	end := strings.Index(rest, "} else {")
	if end < 0 {
		t.Fatal("could not find the end of the partially_deleted branch")
	}
	branch := rest[:end]

	// Assert on the actual hiveToast(...) CALL, not the whole branch text — the
	// branch's explanatory comment legitimately contains the word 'error' (it
	// describes the old behaviour being fixed), so scanning the raw branch for
	// "'error'" false-positives on the comment. Extract the toast call itself
	// (from `hiveToast(` to the terminating `);`) and check its type argument.
	callAt := strings.Index(branch, "hiveToast(")
	if callAt < 0 {
		t.Fatal("the partially_deleted branch no longer shows a toast")
	}
	call := branch[callAt:]
	if semi := strings.Index(call, ");"); semi >= 0 {
		call = call[:semi+2]
	}

	// The toast type is the last argument; a partial success must be 'info',
	// never 'error' (which would make it look like a failed delete).
	if strings.Contains(call, "'error'") {
		t.Errorf("partial-delete outcome is rendered as an 'error' toast — a partial "+
			"success must not look like a failed delete; toast call was:\n%s", call)
	}
	if !strings.Contains(call, "'info'") {
		t.Errorf("partial-delete outcome should use the 'info' toast type; toast call was:\n%s", call)
	}
	// Guard the reassuring copy so the message stays informational, not alarming.
	if !strings.Contains(call, "manual cleanup") {
		t.Errorf("partial-delete toast should mention manual cleanup; toast call was:\n%s", call)
	}
}
