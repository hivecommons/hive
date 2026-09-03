package panes_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// TestAgentsGolden pins the complete 48x12 content-only pane with running,
// paused, and error rows, as required by T5.
func TestAgentsGolden(t *testing.T) {
	observedAt := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	msg := panes.AgentsMsg{
		Agents: []client.Agent{
			{Name: "scanner", DisplayName: "Scanner", Enabled: true, Backend: "claude", Model: "claude-opus-4-5"},
			{Name: "quality", Enabled: false, Backend: "copilot", Model: "gpt-5"},
			{Name: "reviewer", DisplayName: "Reviewer", Enabled: true, Backend: "codex", Model: "gpt-5.1-codex-max"},
		},
		States: map[string]panes.AgentState{
			"scanner":  {Status: panes.AgentStatusRunning, LastActivity: observedAt.Add(-35 * time.Second)},
			"quality":  {Status: panes.AgentStatusPaused, LastActivity: observedAt.Add(-7 * time.Minute)},
			"reviewer": {Status: panes.AgentStatusError, LastActivity: observedAt.Add(-26 * time.Hour)},
		},
		ObservedAt: observedAt,
	}

	pane, _ := panes.NewAgents().Update(msg)
	requireGolden(t, []byte(pane.View(48, 12)), filepath.Join("testdata", "agents.golden"))
}
