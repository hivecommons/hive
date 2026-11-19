package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/crux"
)

// ClaudeLoginRevokedAlertID is the system-alert id for the "Claude login
// revoked" banner (hivecommons/hive#18732). One id: the governor cycle
// reasserts or clears it from the agents' BackendAuth marks, and the Claude
// sign-in handler clears it the moment the operator completes the flow.
const ClaudeLoginRevokedAlertID = "claude-login-revoked"

// claudeLoginRevokedAlertSeverity: the fleet cannot work and nothing automatic
// will fix it, so it is the same severity as a blown provider budget.
const claudeLoginRevokedAlertSeverity = "error"

// ClaudeLoginRevokedAgents lists, sorted, the agents whose provider rejected
// the stored Claude credential on a live check.
func ClaudeLoginRevokedAgents(statuses map[string]*crux.Process) []string {
	var names []string
	for name, proc := range statuses {
		if proc != nil && proc.BackendAuth.Status == crux.BackendAuthRevoked {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ClaudeLoginRevokedAlertMessage is the banner text. It names the agents and
// says what will and will not happen: no restarts until the sign-in, then one
// restart per agent still at the prompt. Never carries token material.
func ClaudeLoginRevokedAlertMessage(names []string) string {
	n := len(names)
	noun := "agents are"
	if n == 1 {
		noun = "agent is"
	}
	return fmt.Sprintf(
		"Claude login revoked: sign in again. The provider rejected the stored Claude credential (HTTP 401), "+
			"so %d %s waiting at the login prompt and will not be restarted until you sign in to Claude from the dashboard: %s. "+
			"Agents still at the prompt are restarted once after the sign-in.",
		n, noun, strings.Join(names, ", "),
	)
}

// ReconcileClaudeLoginRevokedAlert raises or clears the banner from the
// current agent statuses. Level-triggered: called once per governor cycle.
func (s *Server) ReconcileClaudeLoginRevokedAlert(statuses map[string]*crux.Process) {
	if s == nil {
		return
	}
	names := ClaudeLoginRevokedAgents(statuses)
	if len(names) == 0 {
		s.ClearSystemAlert(ClaudeLoginRevokedAlertID)
		return
	}
	s.AddSystemAlert(ClaudeLoginRevokedAlertID, claudeLoginRevokedAlertSeverity, ClaudeLoginRevokedAlertMessage(names))
}
