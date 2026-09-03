// The help overlay's golden lives here alongside grid.golden, per the design
// doc's testing convention (rendering goldens under
// src/pkg/tui/panes/testdata/) and the T23 acceptance criteria, which name
// panes/testdata/help.golden.
//
// Regenerate after a DELIBERATE change with:
//
//	cd src && go test ./pkg/tui/panes/... -update
//
// and read the regenerated file in the diff — a golden updated without being
// looked at asserts nothing.
package panes_test

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// TestHelpOverlayGolden pins the complete 100x30 frame with the help overlay
// raised, byte for byte.
//
// WHY THIS DRIVES View() DIRECTLY rather than going through teatest as
// TestGridGolden does. That test captures the program's real output STREAM,
// which is why it needs stripSplashRace: bubbletea flushes the pre-size splash
// on its own ticker, and whether that transient frame reaches the wire is a
// race between two of its goroutines (#5131). Sizing the model and calling
// View() renders the same frame with no renderer, no goroutines and no stream
// — so the flake class that test had to normalize away cannot occur here at
// all, and the golden pins the frame rather than a transcript of how it was
// painted.
//
// Init is deliberately not called, so no poll is issued and the frame cannot
// depend on whether a dashboard happens to be listening on this machine.
func TestHelpOverlayGolden(t *testing.T) {
	m := tui.New()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})

	view := m.View()
	// The frame must be exactly the terminal it was given: an overlay that
	// grew the frame would scroll the operator's screen when it opened.
	if got := lipgloss.Width(view); got != 100 {
		t.Errorf("frame width = %d, want 100", got)
	}
	if got := lipgloss.Height(view); got != 30 {
		t.Errorf("frame height = %d, want 30", got)
	}

	requireGolden(t, []byte(view), filepath.Join("testdata", "help.golden"))
}

// TestHelpListsEveryDesignDocBinding guards the one thing a golden cannot: the
// golden pins whatever the table SAYS, so it would happily freeze a table that
// had quietly lost a row.
//
// HelpBindings is a hand-transcribed second copy of the design doc's §4 table
// (there is no runtime binding registry to derive it from), so this asserts the
// count and that every key is spelled exactly once.
func TestHelpListsEveryDesignDocBinding(t *testing.T) {
	bindings := panes.HelpBindings()

	// The nine rows of src/docs/design/tui.md §4.
	wantKeys := []string{
		"tab / shift+tab", "?", "q / ctrl+c", "j / k, ↓ / ↑",
		"p", "m", "K", "A", "a",
	}
	if len(bindings) != len(wantKeys) {
		t.Fatalf("HelpBindings() has %d rows, want %d — the design doc's §4 table", len(bindings), len(wantKeys))
	}
	seen := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		if seen[b.Keys] {
			t.Errorf("binding %q listed twice", b.Keys)
		}
		seen[b.Keys] = true
		if b.Action == "" || b.Scope == "" {
			t.Errorf("binding %q has an empty action or scope: %+v", b.Keys, b)
		}
	}
	for _, k := range wantKeys {
		if !seen[k] {
			t.Errorf("binding %q is missing from the help table", k)
		}
	}
}

// TestHelpMarksOnlyWiredBindingsAvailable is the honesty check. app.go's
// footerText refuses to advertise actions that do nothing; help is where an
// operator goes to learn what they CAN do, so it matters more here.
//
// When a later task wires its key it flips Available and updates this list —
// which is the point: the flag cannot rot silently.
func TestHelpMarksOnlyWiredBindingsAvailable(t *testing.T) {
	wantAvailable := map[string]bool{
		"tab / shift+tab": true, // T3
		"?":               true, // T23, this task
		"q / ctrl+c":      true, // T1
		"j / k, ↓ / ↑":    true, // T5 and T11
		"p":               true, // T15
		"K":               true, // T21
		"a":               true, // T22
	}
	for _, b := range panes.HelpBindings() {
		if got := b.Available; got != wantAvailable[b.Keys] {
			t.Errorf("binding %q Available = %v, want %v — a binding is available only once its key is wired in app.go",
				b.Keys, got, wantAvailable[b.Keys])
		}
	}
}

// TestHelpRendersEveryBinding: every row reaches the rendered box. A row that
// is filtered out by the available/unavailable split — the one structural way
// this table can lose an entry — fails here.
func TestHelpRendersEveryBinding(t *testing.T) {
	out := panes.Help()
	for _, b := range panes.HelpBindings() {
		if !strings.Contains(out, b.Keys) {
			t.Errorf("rendered help does not contain key %q", b.Keys)
		}
		if !strings.Contains(out, b.Action) {
			t.Errorf("rendered help does not contain action %q", b.Action)
		}
	}
	if !strings.Contains(out, "press any key to dismiss") {
		t.Error("rendered help does not say how to close itself")
	}
}
