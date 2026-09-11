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
