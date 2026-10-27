package crux

import (
	"reflect"
	"testing"
)

func TestProcess_PaneLinesWithoutCapture(t *testing.T) {
	var p Process
	if got := p.PaneLines(10); got != nil {
		t.Fatalf("PaneLines on an empty snapshot = %v, want nil", got)
	}
	// A capture without a filter is unanswerable too: the runtime owns the
	// prompt-aware filtering, and the snapshot never guesses at it.
	p.SetPaneCapture([]string{"a", "b"}, nil)
	if got := p.FilteredPaneLines(1); got != nil {
		t.Fatalf("FilteredPaneLines with no filter = %v, want nil", got)
	}
}

func TestProcess_PaneLinesApplyRuntimeFilter(t *testing.T) {
	var p Process
	var sawN int
	p.SetPaneCapture([]string{"one", "two", "three"}, func(lines []string, n int) []string {
		sawN = n
		if n >= len(lines) {
			return lines
		}
		return lines[len(lines)-n:]
	})

	if got := p.PaneLines(2); !reflect.DeepEqual(got, []string{"two", "three"}) {
		t.Fatalf("PaneLines(2) = %v, want [two three]", got)
	}
	if sawN != 2 {
		t.Fatalf("filter saw n=%d, want 2", sawN)
	}
	if got := p.FilteredPaneLines(10); !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("FilteredPaneLines(10) = %v, want the whole capture", got)
	}
}
