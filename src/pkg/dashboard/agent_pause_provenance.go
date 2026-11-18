package dashboard

import (
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/crux"
)

// Pause provenance on the agent status (hivecommons/hive#18916): who or what
// paused an agent, when and why, and who or what last resumed it. The runtime
// keeps the pause trigger, acting user, reason and time with the persisted
// pause state; pkg/pauseprov (behind an adapter in cmd/hive, so the dashboard
// takes no new package import) turns them into a readable answer and keeps the
// last resume.

// PauseProvenanceSource describes an agent's pause provenance.
type PauseProvenanceSource interface {
	// Describe returns the provenance value shown on the status card (nil
	// when there is nothing to say) and, for a paused agent, the kick-refusal
	// text "agent <name> cannot be kicked: it is paused by <actor> at <time>:
	// <reason>; resume here" ("" when not paused).
	Describe(agent string, paused bool, trigger, by, reason string, at time.Time) (prov any, refusal string)
	// IntendResume says who is about to resume agent; the runtime's resume
	// event carries no acting user.
	IntendResume(agent, by string)
}

type pauseSourceHolder struct{ src PauseProvenanceSource }

var pauseProvenanceSource atomic.Pointer[pauseSourceHolder]

// SetPauseProvenanceSource installs the process's source; nil removes it.
func SetPauseProvenanceSource(src PauseProvenanceSource) {
	if src == nil {
		pauseProvenanceSource.Store(nil)
		return
	}
	pauseProvenanceSource.Store(&pauseSourceHolder{src})
}

func currentPauseSource() PauseProvenanceSource {
	if h := pauseProvenanceSource.Load(); h != nil {
		return h.src
	}
	return nil
}

// agentPauseProvenance is the provenance of one agent from its runtime
// status, with the kick-refusal text; both are zero when there is nothing to say.
func agentPauseProvenance(name string, proc *crux.Process) (any, string) {
	src := currentPauseSource()
	if src == nil || proc == nil {
		return nil, ""
	}
	return src.Describe(name, proc.Paused, proc.PausedTrigger, proc.PausedBy, proc.PausedReason, proc.PausedAt)
}

// applyPauseProvenance attaches each agent's provenance to its status card.
func applyPauseProvenance(agents []FrontendAgent, statuses map[string]*crux.Process) {
	for i := range agents {
		agents[i].PauseProvenance, _ = agentPauseProvenance(agents[i].Name, statuses[agents[i].Name])
	}
}

// pausedKickMessage rewrites the refusal of a kick to a paused agent so it
// says who or what paused it, when, why, and where to resume. Any other
// message, and a paused agent with no provenance to give, is returned
// unchanged.
func (s *Server) pausedKickMessage(agent, msg string) string {
	if s == nil || s.deps == nil || s.deps.AgentMgr == nil || classifyKickError(msg) != kickRefusalPaused {
		return msg
	}
	proc, err := s.deps.AgentMgr.GetStatus(agent)
	if err != nil || proc == nil {
		return msg
	}
	if _, refusal := agentPauseProvenance(agent, proc); refusal != "" {
		return refusal
	}
	return msg
}
