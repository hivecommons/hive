package agent

import (
	"github.com/hivecommons/hive/crux/trajectory"
)

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
		a.paneMu.RLock()
		lastOutput := a.LastPaneChange
		a.paneMu.RUnlock()
		views = append(views, trajectory.AgentView{
			Name:         name,
			Running:      true,
			Intent:       a.LastKickMessage,
			Transcript:   transcript,
			Idle:         trajectoryIdle(a),
			LastOutputAt: lastOutput,
		})
	}
	return views
}

// trajectoryIdle reports whether the manager's own state proves the agent has
// no turn in progress: its most recent kicked turn has been observed to end
// (KickOutcome settled against LastKick), or it is parked at the prompt
// waiting on CI. An agent that has never been kicked has no turn either.
// This is agent/governor state only — the pane transcript is deliberately
// not consulted, so a stalled-but-running turn never reads as idle
// (hivecommons/hive#12132). Caller holds m.mu.
func trajectoryIdle(a *AgentProcess) bool {
	if a.LastKick == nil {
		return true
	}
	return a.KickOutcome.Settled(a.LastKick) || a.AwaitingCI
}
