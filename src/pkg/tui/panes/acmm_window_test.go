package panes

import (
	"testing"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// windowOverlay builds an overlay with n zero-value packs and the cursor at
// selected. window() only reads len(status.Packs) and selected, so the packs
// themselves can stay empty — this test pins the scroll arithmetic, not the
// rendering (the golden tests own that).
func windowOverlay(n, selected int) ACMMOverlay {
	return ACMMOverlay{
		status:   client.ACMMStatus{Packs: make([]client.Pack, n)},
		selected: selected,
	}
}

func TestACMMWindowShortListIsNeverScrolled(t *testing.T) {
	// A list that fits (n <= acmmVisibleRows) must be shown whole regardless
	// of where the cursor is — including the empty list.
	for _, tc := range []struct{ n, selected int }{
		{0, 0},
		{1, 0},
		{acmmVisibleRows, 0},
		{acmmVisibleRows, acmmVisibleRows - 1},
	} {
		start, end := windowOverlay(tc.n, tc.selected).window()
		if start != 0 || end != tc.n {
			t.Errorf("n=%d selected=%d: got window [%d,%d), want [0,%d)",
				tc.n, tc.selected, start, end, tc.n)
		}
	}
}

func TestACMMWindowClampsAtTheTop(t *testing.T) {
	// Cursor near the head: the centering offset would go negative, so the
	// window must clamp to the first acmmVisibleRows entries.
	start, end := windowOverlay(20, 0).window()
	if start != 0 || end != acmmVisibleRows {
		t.Fatalf("got window [%d,%d), want [0,%d)", start, end, acmmVisibleRows)
	}
}

func TestACMMWindowCentersTheCursorMidList(t *testing.T) {
	const n, selected = 20, 10
	start, end := windowOverlay(n, selected).window()
	if want := selected - acmmVisibleRows/2; start != want {
		t.Errorf("start = %d, want %d (cursor centered)", start, want)
	}
	if end != start+acmmVisibleRows {
		t.Errorf("window height = %d, want %d", end-start, acmmVisibleRows)
	}
	if selected < start || selected >= end {
		t.Errorf("cursor %d fell outside window [%d,%d)", selected, start, end)
	}
}

func TestACMMWindowClampsAtTheBottom(t *testing.T) {
	// Cursor on the last row: centering would run past the end, so the window
	// must clamp to the final acmmVisibleRows entries — never padding with
	// out-of-range indices the render loop would index out of bounds on.
	const n = 20
	start, end := windowOverlay(n, n-1).window()
	if start != n-acmmVisibleRows || end != n {
		t.Fatalf("got window [%d,%d), want [%d,%d)", start, end, n-acmmVisibleRows, n)
	}
}
