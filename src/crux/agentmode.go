package crux

import "github.com/hivecommons/hive/crux/agentmode"

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
	if name == "supervisor" {
		return ModeAdvisory
	}
	switch level {
	case 1:
		return ModeAdvisory
	case 2:
		return ModeAdvisory
	case 3:
		if name == "quality" {
			return ModeIssuesAndPRs
		}
		return ModeAdvisory
	case 4:
		switch name {
		case "quality", "sec-check", "ci-maintainer":
			return ModeIssuesAndPRs
		case "scanner", "guide":
			return ModeIssuesOnly
		default:
			return ModeAdvisory
		}
	case 5:
		return ModeIssuesAndPRs
	case 6:
		if name == "scanner" {
			return ModeIssuesPRsMerge
		}
		return ModeIssuesAndPRs
	default:
		return ModeAdvisory
	}
}

// AgentAvailableAtACMMLevel is the ACMM roster gate for level-scoped agents.
// Dashboard: status and pack rosters; hub: agent capability rows.
func AgentAvailableAtACMMLevel(name string, level int) bool {
	switch name {
	case "telemetry", "operations":
		return level >= operabilityAgentMinACMMLevel
	default:
		return true
	}
}

// operabilityAgentMinACMMLevel is the ACMM level the operability agents
// (telemetry, operations) exist from.
const operabilityAgentMinACMMLevel = 5
