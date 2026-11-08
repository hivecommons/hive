package dashboard

import (
	"net/http"
	"sort"
)

// claudeSignInResumeTrigger is the pause trigger recorded when a dashboard
// Claude sign-in resumes an agent.
const claudeSignInResumeTrigger = "claude-sign-in"

// loginDetectorPauseTrigger is the trigger the login detector pauses with
// (pkg/loginscan). A pause carries its trigger in crux.Process.PausedTrigger,
// which is how the sign-in tells the detector's own pauses apart from an
// operator's ("dashboard-api"), the fleet breaker's, a hook's, a relay's or
// any other system pause.
const loginDetectorPauseTrigger = "login-detector"

// resumeLoginPausedClaudeAgents resumes the claude agents the login detector
// paused, and only those, after a successful fleet-wide Claude sign-in. Those
// pauses mean "this agent is at a login prompt"; the sign-in is the fix, so
// leaving them paused would make the owner resume every agent by hand. Every
// other pause is someone's decision and is left alone. Each resume is audited
// under the signing-in user. Returns the resumed agent names, sorted.
func (s *Server) resumeLoginPausedClaudeAgents(r *http.Request) []string {
	statuses := s.deps.AgentMgr.AllStatuses()
	names := make([]string, 0, len(statuses))
	for name := range statuses {
		names = append(names, name)
	}
	sort.Strings(names)
	var resumed []string
	for _, name := range names {
		proc := statuses[name]
		if proc == nil || !proc.Paused || proc.PausedTrigger != loginDetectorPauseTrigger {
			continue
		}
		backend := proc.Config.Backend
		if proc.BackendOverride != "" {
			backend = proc.BackendOverride
		}
		if backend != "claude" {
			// A Claude sign-in does not fix a Copilot or Gemini login prompt.
			continue
		}
		if err := s.deps.AgentMgr.Resume(s.deps.Ctx, name, claudeSignInResumeTrigger, "Claude sign-in completed; releasing the login-detector pause"); err != nil {
			s.deps.Logger.Warn("Claude sign-in: resume of login-paused agent failed", "agent", name, "error", err)
			continue
		}
		s.auditFromRequest(r, "resume", auditDetail("trigger", claudeSignInResumeTrigger, "prev_trigger", loginDetectorPauseTrigger), name)
		resumed = append(resumed, name)
	}
	return resumed
}
