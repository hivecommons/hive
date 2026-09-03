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

// TestPauseConfirmGolden pins the complete modal frame at a fixed terminal
// size, including the selected agent, verb and available responses.
func TestPauseConfirmGolden(t *testing.T) {
	m := tui.New()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(panes.AgentsMsg{Agents: []client.Agent{{Name: "scanner", Enabled: true}}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})

	view := m.View()
	if got := lipgloss.Width(view); got != 100 {
		t.Errorf("frame width = %d, want 100", got)
	}
	if got := lipgloss.Height(view); got != 30 {
		t.Errorf("frame height = %d, want 30", got)
	}
	requireGolden(t, []byte(view), filepath.Join("testdata", "confirm.golden"))
}
