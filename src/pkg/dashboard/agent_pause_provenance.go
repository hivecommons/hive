package dashboard

import (
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/pauseprov"
)

// Pause provenance on the agent status (hivecommons/hive#18916): who or what
// paused an agent, when and why, and who or what last resumed it. The runtime
// keeps the pause trigger, acting user, reason and time with the persisted
// pause state; pkg/pauseprov turns them into a readable answer and keeps the
// last resume.

// agentPauseProvenance is the provenance of one agent from its runtime
// status, or nil when there is nothing to say.
func agentPauseProvenance(name string, proc *crux.Process) *pauseprov.Provenance {
	if proc == nil {
		return nil
	}
	return pauseprov.Describe(proc.Paused,
		pauseprov.PauseFacts{Trigger: proc.PausedTrigger, By: proc.PausedBy, Reason: proc.PausedReason, At: proc.PausedAt},
		pauseprov.Default().LastResume(name))
}

// applyPauseProvenance attaches each agent's provenance to its status card.
func applyPauseProvenance(agents []FrontendAgent, statuses map[string]*crux.Process) {
	for i := range agents {
		agents[i].PauseProvenance = agentPauseProvenance(agents[i].Name, statuses[agents[i].Name])
	}
}

// pausedKickMessage rewrites the refusal of a kick to a paused agent so it
// says who or what paused it, when, why, and where to resume ("paused by
// <actor> at <time>: <reason>; resume here"). Any other message, and a paused
// agent with no provenance to give, is returned unchanged.
func (s *Server) pausedKickMessage(agent, msg string) string {
	if s == nil || s.deps == nil || s.deps.AgentMgr == nil || classifyKickError(msg) != kickRefusalPaused {
		return msg
	}
	proc, err := s.deps.AgentMgr.GetStatus(agent)
	if err != nil || proc == nil {
		return msg
	}
	if rewritten := pauseprov.RefusalMessage(agent, agentPauseProvenance(agent, proc)); rewritten != "" {
		return rewritten
	}
	return msg
}
