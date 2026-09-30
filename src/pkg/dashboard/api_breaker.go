package dashboard

import (
	"net/http"
	"strconv"
	"strings"
)

// handleBreakerState returns the current fleet-breaker state: whether it is
// engaged and the set of agents it is holding paused. Read-only (GET), so any
// authenticated role may call it.
func (s *Server) handleBreakerState(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	engaged, agents := s.deps.AgentMgr.BreakerState()
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"engaged": engaged,
		"agents":  agents,
	})
}

// handleBreakerEngage throws the fleet breaker: it pauses every running,
// non-on-demand agent and records exactly that set. Already-paused and
// on-demand agents are left untouched (see Manager.EngageBreaker). Restricted
// to owner role because engaging the breaker halts all agents fleet-wide.
func (s *Server) handleBreakerEngage(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	paused := s.deps.AgentMgr.EngageBreaker()
	s.auditFromRequest(r, "breaker-engage",
		auditDetail("count", strconv.Itoa(len(paused)), "agents", strings.Join(paused, ",")), "")
	// Persist so an engaged breaker survives a crash, not just graceful
	// shutdown. Refresh so the dashboard reflects the newly-paused agents.
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"engaged": true,
		"agents":  paused,
	})
}

// handleBreakerRelease disengages the fleet breaker: it resumes ONLY the agents
// the breaker paused and still owns (PausedTrigger unchanged). On-demand,
// pre-existing, and operator-re-paused agents are never resumed (see
// Manager.ReleaseBreaker). Restricted to owner role.
func (s *Server) handleBreakerRelease(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	resumed := s.deps.AgentMgr.ReleaseBreaker(s.deps.Ctx)
	s.auditFromRequest(r, "breaker-release",
		auditDetail("count", strconv.Itoa(len(resumed)), "agents", strings.Join(resumed, ",")), "")
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"engaged": false,
		"agents":  resumed,
	})
}
