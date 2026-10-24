package crux

import (
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/agentmode"
)

// AgentMode is the GitHub interaction tier an agent holds at an ACMM level.
// It is defined in the leaf crux/agentmode; consumers name it here so they do
// not import crux/agent for mode and capability parsing.
type AgentMode = agentmode.AgentMode

const (
	ModeAdvisory       = agentmode.ModeAdvisory
	ModeIssuesOnly     = agentmode.ModeIssuesOnly
	ModeIssuesAndPRs   = agentmode.ModeIssuesAndPRs
	ModeIssuesPRsMerge = agentmode.ModeIssuesPRsMerge
)

// ParseAgentMode converts a configured mode string such as "ADVISORY" to an
// AgentMode. Dashboard: agent cards and Linear agent config.
func ParseAgentMode(s string) (AgentMode, bool) {
	return agentmode.ParseAgentMode(s)
}

// DefaultAgentMode returns the mode an agent gets at an ACMM level with no
// hive.yaml override. Dashboard: the "(default)" indicators.
func DefaultAgentMode(name string, level int) AgentMode {
	return agent.DefaultAgentMode(name, level)
}

// AgentAvailableAtACMMLevel is the ACMM roster gate for level-scoped agents.
// Dashboard: status and pack rosters; hub: agent capability rows.
func AgentAvailableAtACMMLevel(name string, level int) bool {
	return agent.AgentAvailableAtACMMLevel(name, level)
}
