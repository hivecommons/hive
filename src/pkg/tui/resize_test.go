package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
)

// paneTitles is what a drawn grid always shows, at any size at or above the
// minimum. The too-small frame shows none of them, which is what makes this
// list usable as the "grid or message?" discriminator below.
var paneTitles = []string{"AGENTS", "GOVERNOR", "TOKENS", "EVENTS"}

// TestResizeRendersGridOrMessageBySize is T24's central contract, driven
// through the real program at three sizes the issue names: comfortably large,
// exactly the minimum, and one cell below it in both dimensions.
//
// It goes through teatest rather than calling View directly because the size
// arrives as a message: a guard that read some other field, or an Update that
// dropped tea.WindowSizeMsg, would still satisfy a hand-sized model.
func TestResizeRendersGridOrMessageBySize(t *testing.T) {
	cases := []struct {
		name     string
		w, h     int
		wantGrid bool
	}{
		{"large", 120, 40, true},
		// The minimum EXACTLY. This is the case a `<=` typo turns into the
		// too-small message, hiding the grid from a terminal that fits it.
		{"minimum", minWidth, minHeight, true},
		{"one column below", minWidth - 1, minHeight, false},
		{"one row below", minWidth, minHeight - 1, false},
		{"below minimum", minWidth - 1, minHeight - 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(tc.w, tc.h))

			want := tooSmallText
			if tc.wantGrid {
				want = paneTitles[0]
			}
			teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
				return strings.Contains(string(b), want)
			}, teatest.WithDuration(finalWait))

			tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
			tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))

			final, ok := tm.FinalModel(t).(model)
			if !ok {
				t.Fatalf("final model has unexpected type %T", tm.FinalModel(t))
			}
			assertFrameFits(t, final.View(), tc.w, tc.h)

			view := final.View()
			if tc.wantGrid && !strings.Contains(view, paneTitles[0]) {
				t.Fatalf("%dx%d: grid frame does not contain the focused pane title %q", tc.w, tc.h, paneTitles[0])
			}
			if tc.wantGrid && selectGridShape(tc.w, tc.h) == gridShapeTwoByTwo {
				for _, title := range paneTitles {
					if !strings.Contains(view, title) {
						t.Fatalf("%dx%d: 2x2 frame is missing %q", tc.w, tc.h, title)
					}
				}
			}
			if got := strings.Contains(view, tooSmallText); got == tc.wantGrid {
				t.Fatalf("%dx%d: frame contains the too-small message = %v, want %v", tc.w, tc.h, got, !tc.wantGrid)
			}
		})
	}
}

func TestSelectGridShape(t *testing.T) {
	cases := []struct {
		name       string
		w, h       int
		want       gridShape
		wantPanes  int
		secondPane int
	}{
		{"comfortable keeps 2x2", comfortableWidth, comfortableHeight, gridShapeTwoByTwo, 4, 1},
		{"wide but short uses two columns", comfortableWidth, minHeight, gridShapeTwoColumns, 2, 1},
		{"narrow uses one column", 80, 24, gridShapeColumn, 1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectGridShape(tc.w, tc.h); got != tc.want {
				t.Fatalf("selectGridShape(%d,%d) = %v, want %v", tc.w, tc.h, got, tc.want)
			}
			m := newModel()
			gotPanes := m.visiblePaneIndexes(tc.want)
			if len(gotPanes) != tc.wantPanes {
				t.Fatalf("visible panes = %v, want %d panes", gotPanes, tc.wantPanes)
			}
			if tc.secondPane >= 0 && gotPanes[1] != tc.secondPane {
				t.Fatalf("visible panes = %v, want second pane %d", gotPanes, tc.secondPane)
			}
		})
	}
}

// TestResizeSharesSpaceAtAnySize pins the "recompute the grid so panes share
// the space at any size" half of the issue: every drawn grid fills its
// terminal exactly, and both split points stay balanced as the size changes.
//
// The odd numbers are the point. The halving in View gives the remainder to
// the right column and bottom row, so an odd width or height is exactly where
// an off-by-one leaves a one-cell gap or one cell of overflow.
func TestResizeSharesSpaceAtAnySize(t *testing.T) {
	sizes := [][2]int{
		{minWidth, minHeight},
		{61, 21},
		{80, 24},
		{101, 33},
		{200, 60},
	}
	for _, s := range sizes {
		w, h := s[0], s[1]
		next, _ := newModel().Update(tea.WindowSizeMsg{Width: w, Height: h})
		view := next.(model).View()
		assertFrameFits(t, view, w, h)
		// Every line of a grid frame reaches the right edge; a pane that
		// failed to take its share of a resize would leave one short.
		for i, line := range strings.Split(view, "\n") {
			if lw := lipgloss.Width(line); lw != w {
				t.Fatalf("%dx%d: line %d is %d cells wide, want exactly %d:\n%q", w, h, i, lw, w, line)
			}
		}
	}
}

// TestResizeBelowMinimumAndBackRestoresGrid covers the sequence an operator
// actually produces by dragging a window edge: past the floor and back again.
//
// The restored frame must be byte-identical to one that was never shrunk. It
// is today because no state is derived from the size, and this test is what
// keeps that true — a future cached layout invalidated on the way down but not
// on the way up would leave a permanently broken grid after one resize.
func TestResizeBelowMinimumAndBackRestoresGrid(t *testing.T) {
	const w, h = 100, 30

	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(w, h))
	tm.Send(tea.WindowSizeMsg{Width: 30, Height: 10})
	tm.Send(tea.WindowSizeMsg{Width: w, Height: h})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))

	final, ok := tm.FinalModel(t).(model)
	if !ok {
		t.Fatalf("final model has unexpected type %T", tm.FinalModel(t))
	}

	pristine, _ := newModel().Update(tea.WindowSizeMsg{Width: w, Height: h})
	if got, want := final.View(), pristine.(model).View(); got != want {
		t.Fatalf("frame after shrinking below the minimum and growing back differs from a never-shrunk frame:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestTooSmallFrameFitsAnyTerminal pins the clip in tooSmallView.
//
// The message is 40 cells wide, so any terminal narrower than that is one the
// message itself would overflow — and a minimum-size message that spills past
// the right edge is the same broken frame it exists to replace. 1x1 is in the
// list because a terminal can genuinely report it mid-drag.
func TestTooSmallFrameFitsAnyTerminal(t *testing.T) {
	sizes := [][2]int{{1, 1}, {5, 2}, {12, 3}, {40, 4}, {41, 19}, {minWidth - 1, minHeight - 1}}
	for _, s := range sizes {
		w, h := s[0], s[1]
		next, _ := newModel().Update(tea.WindowSizeMsg{Width: w, Height: h})
		assertFrameFits(t, next.(model).View(), w, h)
	}
}

// TestTooSmallTextNamesTheEnforcedMinimum pins the message to the constants
// the guard actually compares against. Told to resize to a number that is not
// the threshold, an operator resizes to it and still sees the message.
func TestTooSmallTextNamesTheEnforcedMinimum(t *testing.T) {
	if want := fmt.Sprintf("%dx%d", minWidth, minHeight); !strings.Contains(tooSmallText, want) {
		t.Fatalf("tooSmallText = %q, want it to name the enforced minimum %q", tooSmallText, want)
	}
}

// assertFrameFits is the invariant every frame owes the terminal at every
// size: exactly as many lines as it has rows, and no line wider than it is.
func assertFrameFits(t *testing.T, view string, w, h int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) != h {
		t.Fatalf("%dx%d: frame renders %d lines, want exactly %d", w, h, len(lines), h)
	}
	for i, line := range lines {
		if lw := lipgloss.Width(line); lw > w {
			t.Fatalf("%dx%d: line %d is %d cells wide, want <= %d:\n%q", w, h, i, lw, w, line)
		}
	}
}
