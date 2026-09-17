package agent

import "github.com/hivecommons/hive/pkg/agentmode"

// AgentMode and its constants live in the stdlib-only leaf pkg/agentmode so
// policy-layer packages (pkg/toolapprove, pkg/turn) can name a tier without
// importing this runtime package. The aliases below keep every existing
// consumer of agent.AgentMode / agent.Mode* working unchanged.
type AgentMode = agentmode.AgentMode

const (
	ModeAdvisory       = agentmode.ModeAdvisory
	ModeIssuesOnly     = agentmode.ModeIssuesOnly
	ModeIssuesAndPRs   = agentmode.ModeIssuesAndPRs
	ModeIssuesPRsMerge = agentmode.ModeIssuesPRsMerge
)

// ParseAgentMode converts a string like "ADVISORY" to an AgentMode.
// Accepts "NO_GITHUB" as a legacy alias for ADVISORY.
func ParseAgentMode(s string) (AgentMode, bool) {
	return agentmode.ParseAgentMode(s)
}

// ReviewerRole is the agent role that earns the comment-capable review tier.
const ReviewerRole = agentmode.ReviewerRole

// TokenTierForRole returns the scoped-token tier for a mode and role. See
// agentmode.TokenTierForRole: an ADVISORY agent whose role is "reviewer" mints
// a tier that may post a PR review, which a plain advisor may not.
func TokenTierForRole(m AgentMode, role string) string {
	return agentmode.TokenTierForRole(m, role)
}
