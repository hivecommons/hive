// Package agentmode holds the AgentMode ladder — the ordered GitHub
// interaction tiers an agent can hold at a given ACMM level.
//
// It is deliberately stdlib-only and a leaf in the import graph: the mode
// vocabulary is a shared contract consumed by policy-layer packages
// (pkg/toolapprove, pkg/turn) that must not drag in the pkg/agent runtime and
// its transitive dependency closure just to name a tier. pkg/agent re-exports
// everything here under its original names, so runtime-side consumers are
// unaffected.
package agentmode

// AgentMode describes the GitHub interaction tier for an agent at a given ACMM level.
type AgentMode int

const (
	ModeAdvisory       AgentMode = iota // Advisory beads only, governor posts digests
	ModeIssuesOnly                      // Open issues, no PRs
	ModeIssuesAndPRs                    // Issues + PRs, without merge authority
	ModeIssuesPRsMerge                  // Issues + PRs + auto-merge on green CI
)

const agentModeCount = 4

var modeNames = [agentModeCount]string{
	"ADVISORY",
	"ISSUES_ONLY",
	"ISSUES_AND_PRS",
	"ISSUES_PRS_MERGE",
}

var modeEmojis = [agentModeCount]string{
	"\U0001F4DD", // 📝
	"\U0001F3AB", // 🎫
	"\U0001F527", // 🔧
	"\U0001F680", // 🚀
}

var modeSuffixes = [agentModeCount]string{
	"-advisory",
	"-issues",
	"-holdgated",
	"-automerge",
}

func (m AgentMode) String() string {
	if int(m) < agentModeCount {
		return modeNames[m]
	}
	return "UNKNOWN"
}

func (m AgentMode) Emoji() string {
	if int(m) < agentModeCount {
		return modeEmojis[m]
	}
	return ""
}

func (m AgentMode) Suffix() string {
	if int(m) < agentModeCount {
		return modeSuffixes[m]
	}
	return "-advisory"
}

func (m AgentMode) CanCreateIssues() bool { return m >= ModeIssuesOnly }
func (m AgentMode) CanCreatePRs() bool    { return m >= ModeIssuesAndPRs }
func (m AgentMode) CanMerge() bool        { return m >= ModeIssuesPRsMerge }
func (m AgentMode) CanPush() bool         { return m >= ModeIssuesAndPRs }
func (m AgentMode) NeedsMCPWrite() bool   { return m >= ModeIssuesOnly }

// TokenTier returns the GitHub App scoped-token tier for this mode.
func (m AgentMode) TokenTier() string {
	switch m {
	case ModeAdvisory:
		return "advisor"
	case ModeIssuesOnly:
		return "newcomer"
	case ModeIssuesAndPRs:
		return "contributor"
	case ModeIssuesPRsMerge:
		return "trusted"
	default:
		return "advisor"
	}
}

// ParseAgentMode converts a string like "ADVISORY" to an AgentMode.
// Accepts "NO_GITHUB" as a legacy alias for ADVISORY.
func ParseAgentMode(s string) (AgentMode, bool) {
	if s == "NO_GITHUB" {
		return ModeAdvisory, true
	}
	for i, n := range modeNames {
		if n == s {
			return AgentMode(i), true
		}
	}
	return ModeAdvisory, false
}
