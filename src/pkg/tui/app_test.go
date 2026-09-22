package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/hivecommons/hive/pkg/tui/panes"
)

// finalWait bounds every WaitFinished. Generous enough that a loaded CI runner
// does not flake, short enough that a model which never quits fails the test
// instead of hanging until the suite-level timeout.
const finalWait = 5 * time.Second

// TestAppQuits is the scaffold's contract: `q` ends the program cleanly.
//
// It drives the real model through teatest rather than calling Update directly,
// so it exercises the whole bubbletea loop — input decoding, the Update return,
// and the tea.Quit command actually terminating the program. A unit test on
// Update alone would still pass if tea.Quit were never returned as a command.
func TestAppQuits(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})

	// WaitFinished fails the test if the program is still running at the
	// deadline, so reaching the next line IS the clean-exit assertion.
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

// TestAppQuitsOnCtrlC covers the other documented quit binding. ctrl+c travels
// a different path through bubbletea than a plain rune, so `q` passing says
// nothing about it.
func TestAppQuitsOnCtrlC(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})

	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

func requireQuitCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("command is nil, want tea.Quit")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("command returned %T, want tea.QuitMsg", msg)
	}
}

func TestHivesOnlyEscAndCtrlCQuit(t *testing.T) {
	_, escCmd := newHivesOnlyModel().Update(tea.KeyMsg{Type: tea.KeyEsc})
	requireQuitCmd(t, escCmd)

	nilOverlay := newHivesOnlyModel()
	nilOverlay.hives = nil
	_, nilOverlayEscCmd := nilOverlay.Update(tea.KeyMsg{Type: tea.KeyEsc})
	requireQuitCmd(t, nilOverlayEscCmd)

	_, ctrlCCmd := newHivesOnlyModel().Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	requireQuitCmd(t, ctrlCCmd)
}

func TestHivesOnlyViewNeverBlank(t *testing.T) {
	sized := func(m model) model {
		m.width, m.height = 100, 30
		return m
	}

	afterEsc := sized(newHivesOnlyModel())
	next, _ := afterEsc.Update(tea.KeyMsg{Type: tea.KeyEsc})
	afterEsc = next.(model)

	afterShiftH := sized(newHivesOnlyModel())
	afterShiftH.hives = nil
	next, _ = afterShiftH.Update(hivesKey)
	afterShiftH = next.(model)

	tooSmall := newHivesOnlyModel()
	tooSmall.width, tooSmall.height = minWidth-1, minHeight

	for name, m := range map[string]model{
		"initial":      sized(newHivesOnlyModel()),
		"after esc":    afterEsc,
		"after H":      afterShiftH,
		"too small":    tooSmall,
		"closed guard": func() model { m := sized(newHivesOnlyModel()); m.hives = nil; return m }(),
	} {
		if got := m.View(); got == "" {
			t.Fatalf("%s hives-only View() is blank", name)
		}
	}
}

func TestOperatorEscHivesOverlayBehaviorUnchanged(t *testing.T) {
	m := newModel()
	m.width, m.height = 100, 30

	next, cmd := m.Update(hivesKey)
	if cmd == nil {
		t.Fatal("H did not start loading the Hives overlay")
	}
	opened := next.(model)
	if opened.hives == nil {
		t.Fatal("H did not open the Hives overlay")
	}

	next, cmd = opened.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatal("Esc with the operator Hives overlay open returned a command; it must not quit")
	}
	if got := next.(model); got.hives != nil {
		t.Fatal("Esc with the operator Hives overlay open did not close it")
	}

	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatal("Esc with no operator overlay returned a command; it must be a no-op")
	}
	if got := next.(model); got.hives != nil || got.focus != m.focus || got.helpVisible {
		t.Fatalf("Esc with no operator overlay changed state: hives=%v focus=%d help=%v",
			got.hives, got.focus, got.helpVisible)
	}
}

// TestAppRendersGrid pins what a sized frame draws since T3: all four pane
// titles, on screen at once. Asserting on the titles rather than a golden
// file keeps this test stable across layout refinement — the full frame's
// exact bytes are pinned separately by the golden test in pkg/tui/panes.
func TestAppRendersGrid(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(comfortableWidth, comfortableHeight))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, title := range paneTitles {
			if !strings.Contains(string(b), title) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(finalWait))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

// TestTabCyclesFocus drives tab and shift+tab through the real program, per
// the T3 acceptance criteria. teatest rather than bare Update calls for the
// same reason as TestAppQuits: input decoding is part of what is asserted —
// shift+tab in particular arrives as its own key sequence, and a handler
// matching the wrong spelling would pass a unit test that hand-builds the
// message it expects.
func TestTabCyclesFocus(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))

	// Two tabs forward, one back: ends on pane 1 iff both directions work
	// and neither is a no-op. (2-0 would also pass if shift+tab did nothing
	// and one tab was dropped, but then the direct cycle test below fails.)
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyShiftTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))

	final, ok := tm.FinalModel(t).(model)
	if !ok {
		t.Fatalf("final model has unexpected type %T", tm.FinalModel(t))
	}
	if final.focus != 1 {
		t.Fatalf("after tab,tab,shift+tab focus = %d, want 1", final.focus)
	}
}

// TestFocusCycleWrapsBothDirections pins the modulo arithmetic directly:
// forward from the last pane wraps to the first, and backward from the first
// wraps to the last — the negative-operand case the "+paneCount-1" spelling
// exists for.
func TestFocusCycleWrapsBothDirections(t *testing.T) {
	m := newModel()
	for i := 0; i < paneCount; i++ {
		if m.focus != i {
			t.Fatalf("tab cycle step %d: focus = %d, want %d", i, m.focus, i)
		}
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(model)
	}
	if m.focus != 0 {
		t.Fatalf("tab from last pane: focus = %d, want wrap to 0", m.focus)
	}
	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := back.(model).focus; got != paneCount-1 {
		t.Fatalf("shift+tab from pane 0: focus = %d, want wrap to %d", got, paneCount-1)
	}
}

// TestFocusHighlightsExactlyOnePane pins the focused-border contract from the
// issue ("the focused pane gets a highlighted border") in a way no golden
// file drift can silently weaken: exactly one cell wears the thick border,
// and tab moves it.
func TestFocusHighlightsExactlyOnePane(t *testing.T) {
	sized, _ := newModel().Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := sized.(model)

	// The thick border's top-left corner only appears on the focused cell.
	const thickCorner = "┏"
	if got := strings.Count(m.View(), thickCorner); got != 1 {
		t.Fatalf("frame has %d thick-border corners, want exactly 1 (one focused pane)", got)
	}
	before := m.View()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	after := next.(model).View()
	if before == after {
		t.Fatal("tab did not move the focus highlight: frame unchanged")
	}
	if got := strings.Count(after, thickCorner); got != 1 {
		t.Fatalf("after tab, frame has %d thick-border corners, want exactly 1", got)
	}
}

// TestViewBeforeSizeMsg guards the zero-size path in View. bubbletea can call
// View before the first tea.WindowSizeMsg arrives; centering into a 0x0 box
// renders nothing, so an unsized frame would flash blank rather than telling
// the operator how to get out.
//
// Since T24 it also pins the ORDER of View's two early returns: width 0 means
// "not measured yet", not "a zero-width terminal", so it must keep reaching
// the splash rather than the below-minimum message — which names a size and
// would be a claim about a terminal nobody has measured.
func TestViewBeforeSizeMsg(t *testing.T) {
	if got := newModel().View(); got != splash {
		t.Fatalf("unsized View() = %q, want %q", got, splash)
	}
}

// TestViewFillsTerminalExactly checks the grid arithmetic: the frame is
// exactly as tall as the terminal (header + two grid rows + footer), and no
// rendered line overflows the width — the remainder-absorbing split, off by
// one, would do both.
func TestViewFillsTerminalExactly(t *testing.T) {
	const w, h = comfortableWidth, comfortableHeight
	m, _ := newModel().Update(tea.WindowSizeMsg{Width: w, Height: h})

	view := m.(model).View()
	lines := strings.Split(view, "\n")
	if len(lines) != h {
		t.Fatalf("sized View() renders %d lines, want exactly %d", len(lines), h)
	}
	for i, line := range lines {
		if lw := lipgloss.Width(line); lw > w {
			t.Fatalf("line %d is %d cells wide, want <= %d:\n%q", i, lw, w, line)
		}
	}
	for _, want := range []string{m.(model).headerText(), footerText} {
		if !strings.Contains(view, want) {
			t.Fatalf("sized View() missing %q", want)
		}
	}
}

func TestZoomToggleAndRestore(t *testing.T) {
	sized, _ := newModel().Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := sized.(model)
	if m.zoomed {
		t.Fatal("zoom starts enabled")
	}
	zoomed, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	z := zoomed.(model)
	if !z.zoomed {
		t.Fatal("z did not enable zoom")
	}
	if view := z.View(); !strings.Contains(view, "AGENTS") || strings.Contains(view, "GOVERNOR") {
		t.Fatalf("zoomed frame should show only the focused pane:\n%s", view)
	}
	restored, _ := z.Update(tea.KeyMsg{Type: tea.KeyEsc})
	r := restored.(model)
	if r.zoomed {
		t.Fatal("esc did not restore the grid")
	}
	toggled, _ := r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !toggled.(model).zoomed {
		t.Fatal("enter did not toggle zoom")
	}
}

// TestUnhandledKeyDoesNotQuit pins that the quit set is exactly the documented
// one. Without this, a `default: return m, tea.Quit` slip would leave every
// other test green while making the TUI exit on any keypress.
func TestUnhandledKeyDoesNotQuit(t *testing.T) {
	_, cmd := newModel().Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd != nil {
		t.Fatal("an unbound key returned a command; only q and ctrl+c should quit")
	}
}

// TestRunOverPipes drives the real program — the constructor Run uses, options
// and all — with a pipe standing in for the terminal.
//
// The teatest cases above build their own program internally, so none of them
// executes tea.NewProgram in app.go. Without this, WithAltScreen could be
// dropped and every other test would still pass, while `hivectl tui` would
// start scribbling over the operator's scrollback instead of taking its own
// screen.
func TestRunOverPipes(t *testing.T) {
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() { done <- run(bytes.NewReader([]byte("q")), &out, runOptions{}) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v, want nil", err)
		}
	case <-time.After(finalWait):
		t.Fatal("run() did not return after q on stdin")
	}

	rendered := out.String()
	if !strings.Contains(rendered, splash) {
		t.Fatalf("run() output does not contain %q:\n%q", splash, rendered)
	}
	// ESC[?1049h / ESC[?1049l are the alt-screen enter/leave pair. Asserting
	// BOTH is the point: entering without leaving is the failure that strands
	// an operator's terminal in the alternate buffer after exit.
	for _, want := range []string{"\x1b[?1049h", "\x1b[?1049l"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("run() output missing alt-screen sequence %q", want)
		}
	}
}

// helpKey is the "?" keypress, spelled once so the overlay tests cannot drift
// apart on how the binding is sent.
var helpKey = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")}

// TestHelpOverlayTogglesOnQuestionMark: "?" raises the overlay and a second
// "?" puts it away, which is the toggle the design doc's table promises.
//
// The second press works through the any-key-dismiss branch rather than a
// dedicated toggle case — the test asserts the observable behaviour, so that
// implementation detail is free to change.
func TestHelpOverlayTogglesOnQuestionMark(t *testing.T) {
	m := newModel()
	m.width, m.height = 100, 30
	if m.helpVisible {
		t.Fatal("helpVisible = true before any key; the overlay must start hidden")
	}

	next, cmd := m.Update(helpKey)
	shown, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned unexpected type %T", next)
	}
	if !shown.helpVisible {
		t.Fatal(`helpVisible = false after "?"; the overlay must open`)
	}
	if cmd != nil {
		t.Error(`"?" returned a command; opening the overlay must not quit or fetch`)
	}

	next, _ = shown.Update(helpKey)
	hidden, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned unexpected type %T", next)
	}
	if hidden.helpVisible {
		t.Fatal(`helpVisible = true after a second "?"; the binding must toggle`)
	}
}

// TestHelpOverlayDismissesOnAnyKey covers the "any key dismisses" contract
// across keys that take genuinely different paths through the app's handling:
// a bound global, an unbound rune, and a special key.
//
// "q" is the one that matters. It is the app's quit binding, so if the dismiss
// branch did not come FIRST it would end the program while the reader believed
// they were closing a dialog — the one misfire a help screen must not have.
func TestHelpOverlayDismissesOnAnyKey(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"q, which is otherwise quit", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}},
		{"tab, which is otherwise focus", tea.KeyMsg{Type: tea.KeyTab}},
		{"an unbound rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}},
		{"a special key", tea.KeyMsg{Type: tea.KeyEsc}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel()
			m.width, m.height = 100, 30
			m.helpVisible = true

			next, cmd := m.Update(tc.key)
			got, ok := next.(model)
			if !ok {
				t.Fatalf("Update returned unexpected type %T", next)
			}
			if got.helpVisible {
				t.Error("helpVisible = true; every key must dismiss the overlay")
			}
			if cmd != nil {
				t.Error("dismissing returned a command; the key must be swallowed, not also acted on")
			}
		})
	}
}

// TestHelpOverlaySwallowsQuit is the same guarantee as the "q" case above, but
// driven through the REAL bubbletea loop: a unit test on Update would still
// pass if tea.Quit reached the program by some other path.
//
// The program must still be running after "?" then "q", and must exit only on
// the "q" that follows.
func TestHelpOverlaySwallowsQuit(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(100, 30))

	tm.Send(helpKey)
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}) // dismisses
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}) // quits
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))

	final, ok := tm.FinalModel(t).(model)
	if !ok {
		t.Fatalf("final model has unexpected type %T", tm.FinalModel(t))
	}
	// Reaching here means the program exited, and it can only have been the
	// third key: if the second "q" had quit, the overlay would still be up.
	if final.helpVisible {
		t.Error("helpVisible = true at exit; the first q should have dismissed the overlay")
	}
}

// TestHelpOverlayDoesNotReachPanes: while the overlay is up, keys are consumed
// by it and never routed to the focused pane, so a pane binding cannot fire
// underneath a modal the operator is reading.
func TestHelpOverlayDoesNotReachPanes(t *testing.T) {
	m := newModel()
	m.width, m.height = 100, 30
	m.helpVisible = true
	before := m.focus

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	got, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned unexpected type %T", next)
	}
	if got.focus != before {
		t.Errorf("focus moved from %d to %d while the overlay was up; tab must only dismiss", before, got.focus)
	}
}

// TestHelpOverlayViewReplacesFrame pins what the operator sees: the overlay's
// content is on screen, the grid's chrome is not, and the frame still fills the
// terminal exactly so dismissing it cannot leave a resized screen behind.
func TestHelpOverlayViewReplacesFrame(t *testing.T) {
	m := newModel()
	m.width, m.height = 100, 30
	m.helpVisible = true

	view := m.View()
	if !strings.Contains(view, "Keybindings") {
		t.Error("overlay view does not carry the table's title")
	}
	if !strings.Contains(view, "press any key to dismiss") {
		t.Error("overlay view does not say how to close itself")
	}
	// The grid's footer strip is part of the frame the overlay covers.
	if strings.Contains(view, footerText) {
		t.Error("the grid's footer is visible through the overlay; it is modal, not translucent")
	}
	if got := lipgloss.Height(view); got != 30 {
		t.Errorf("overlay frame height = %d, want 30", got)
	}
	if got := lipgloss.Width(view); got != 100 {
		t.Errorf("overlay frame width = %d, want 100", got)
	}
}

// TestFooterAdvertisesHelp: the footer strip names only bindings that exist,
// and "?" now does. An operator who never discovers the overlay may as well not
// have it.
func TestFooterAdvertisesHelp(t *testing.T) {
	if !strings.Contains(footerText, "? help") {
		t.Errorf("footerText = %q, want it to advertise the help binding", footerText)
	}
	m := newModel()
	m.width, m.height = 100, 30
	if !strings.Contains(m.View(), "? help") {
		t.Error("the rendered frame does not advertise ? help")
	}
}

func TestFooterAdvertisesZoom(t *testing.T) {
	if !strings.Contains(footerText, "z zoom") {
		t.Errorf("footerText = %q, want it to advertise z zoom", footerText)
	}
	if !strings.Contains(panes.Help(), "z / enter") {
		t.Error("help does not advertise the zoom binding")
	}
}

func TestFooterAdvertisesAttach(t *testing.T) {
	if !strings.Contains(footerText, "a attach") {
		t.Errorf("footerText = %q, want it to advertise the attach binding", footerText)
	}
	m := newModel()
	m.width, m.height = 100, 30
	if !strings.Contains(m.View(), "a attach") {
		t.Error("the rendered frame does not advertise a attach")
	}
}
