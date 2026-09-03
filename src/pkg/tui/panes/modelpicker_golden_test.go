package panes_test

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui"
	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// TestModelPickerGolden pins the complete overlay frame at a fixed terminal
// size, in the state an operator first sees it: opened over a real fleet row,
// with the catalogue call still in flight.
//
// The LOADING state is what is pinned rather than a populated list, and that
// is deliberate. A populated frame would need a dashboard to answer, which
// would make the golden depend on whether something is listening on this
// machine — the same reason TestHelpOverlayGolden does not call Init. Driving
// Update and View directly renders the frame with no renderer, no goroutines
// and no network, so the file pins the frame rather than a transcript of a
// race. The list, qualification and failure states are asserted by
// modelpicker_test.go against the pane directly.
//
// Regenerate after a DELIBERATE change with:
//
//	cd src && go test ./pkg/tui/panes/... -update
//
// and read the regenerated file in the diff.
func TestModelPickerGolden(t *testing.T) {
	m := tui.New()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(panes.AgentsMsg{Agents: []client.Agent{
		{Name: "scanner", DisplayName: "Scanner", Enabled: true, Backend: "claude", Model: "claude-opus-4-5"},
	}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})

	view := m.View()
	// The overlay must not grow the frame: one that did would scroll the
	// operator's screen the moment it opened.
	if got := lipgloss.Width(view); got != 100 {
		t.Errorf("frame width = %d, want 100", got)
	}
	if got := lipgloss.Height(view); got != 30 {
		t.Errorf("frame height = %d, want 30", got)
	}
	requireGolden(t, []byte(view), filepath.Join("testdata", "modelpicker.golden"))
}
