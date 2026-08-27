package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
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

// TestAppRendersSplash pins the one thing the frame draws. Asserting on the
// text rather than a golden file keeps this test stable across the layout work
// in later tasks: the splash line is the contract, its exact placement is not.
func TestAppRendersSplash(t *testing.T) {
	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(80, 24))

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(string(b), splash)
	}, teatest.WithDuration(finalWait))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
}

// TestViewBeforeSizeMsg guards the zero-size path in View. bubbletea can call
// View before the first tea.WindowSizeMsg arrives; centering into a 0x0 box
// renders nothing, so an unsized frame would flash blank rather than telling
// the operator how to get out.
func TestViewBeforeSizeMsg(t *testing.T) {
	if got := newModel().View(); got != splash {
		t.Fatalf("unsized View() = %q, want %q", got, splash)
	}
}

// TestViewCentersWhenSized checks the sized path actually pads, so a future
// refactor cannot quietly drop the centering and still pass on substring
// matches alone.
func TestViewCentersWhenSized(t *testing.T) {
	m, _ := newModel().Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	if !strings.Contains(view, splash) {
		t.Fatalf("sized View() does not contain %q:\n%s", splash, view)
	}
	if lines := strings.Count(view, "\n"); lines < 2 {
		t.Fatalf("sized View() has %d newlines, want a padded frame:\n%s", lines, view)
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
	go func() { done <- run(bytes.NewReader([]byte("q")), &out) }()

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
