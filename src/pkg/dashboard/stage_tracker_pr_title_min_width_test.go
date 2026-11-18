package dashboard

import (
	"strings"
	"testing"
)

// TestStageTrackerPRTitleKeepsAMinimumWidth guards the linked-PR line: with
// min-width 0, a repo chip and base pill that filled the 20em PR line left the
// #N and title a sliver, and overflow-wrap: anywhere drew the title one letter
// per line (iPad Chrome, utah-packages#1212). The PR ref and its title keep a
// real minimum width, so they move to the next line instead.
func TestStageTrackerPRTitleKeepsAMinimumWidth(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".ham-head-cols .ham-seg.ham-head-pr > .ham-pr-line > .ham-pr-ref { flex: 1 1 0; min-width: min(100%, 12em); }",
		".ham-head-pr .ham-pr-title { flex: 1 1 0; min-width: min(100%, 8em); white-space: normal; overflow: visible; overflow-wrap: anywhere; }",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("PR title minimum width missing %q", want)
		}
	}
	if strings.Contains(html, ".ham-head-pr > .ham-pr-line > .ham-pr-ref { flex: 1 1 0; min-width: 0; }") {
		t.Fatal("the PR ref inside a PR line must not shrink to zero width")
	}
}
