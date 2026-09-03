package agent

import "github.com/hivecommons/hive/pkg/trajectory"

// trajectoryTranscriptLines is how many trailing pane lines are gathered per
// agent for a trajectory review. The reviewer applies its own (smaller) tail
// cap; this is just an upper bound on what the manager hands over.
const trajectoryTranscriptLines = 400

// TrajectoryAgents implements trajectory.AgentSource: a snapshot of running,
// non-paused agents with their assigned intent (last kick message) and a tail
// of their live transcript. Agents that are not running, are paused, or have
// no captured output yet are omitted. Backend-agnostic — it reads the tmux
// pane every CLI writes to, so it works for claude, copilot, gemini, goose,
// and any inference backend equally.
func (m *Manager) TrajectoryAgents() []trajectory.AgentView {
	m.mu.RLock()
	defer m.mu.RUnlock()

	views := make([]trajectory.AgentView, 0, len(m.agents))
	for name, a := range m.agents {
		if a.State != StateRunning || a.Paused {
			continue
		}
		lines := a.PaneLines(trajectoryTranscriptLines)
		if len(lines) == 0 {
			continue
		}
		transcript := ""
		for i, ln := range lines {
			if i > 0 {
				transcript += "\n"
			}
			transcript += ln
		}
		views = append(views, trajectory.AgentView{
			Name:       name,
			Running:    true,
			Intent:     a.LastKickMessage,
			Transcript: transcript,
		})
	}
	return views
}
