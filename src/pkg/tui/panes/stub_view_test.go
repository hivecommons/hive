package panes

import (
	"strings"
	"testing"
)

// TestBareStubViewRendersPlaceholder pins stub.View directly. Every shipped
// pane now overrides View, so this method is reachable only through the NEXT
// pane that embeds stub before its real content lands — exactly the moment
// nothing else would catch a regression in the shared fallback.
func TestBareStubViewRendersPlaceholder(t *testing.T) {
	s := stub{title: "PENDING"}

	const w, h = 30, 6
	view := s.View(w, h)
	for _, want := range []string{"PENDING", placeholder} {
		if !strings.Contains(view, want) {
			t.Fatalf("stub.View() missing %q:\n%s", want, view)
		}
	}
	if lines := strings.Count(view, "\n") + 1; lines != h {
		t.Fatalf("stub.View() renders %d lines, want exactly %d", lines, h)
	}
	if vw := visibleWidth(view); vw != w {
		t.Fatalf("stub.View() widest line is %d cells, want exactly %d", vw, w)
	}

	if got := s.View(0, 0); got != "" {
		t.Fatalf("stub.View(0,0) = %q, want empty", got)
	}
}
