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

// ReviewerRole is the agent role that earns the comment-capable review tier.
const ReviewerRole = "reviewer"

// TokenTierForRole returns the GitHub App scoped-token tier for a mode, taking
// the agent's functional role into account.
//
// The mode ladder is ordinal — advisory < issues < issues+prs < merge — and
// every capability predicate above is a >= comparison against it. "May post a
// review on a PR, but may not push, merge, or open an issue" is not a point on
// that ladder: it sits above advisory for pull requests and below newcomer for
// issues. Encoding it as a new AgentMode would therefore break the ordering
// that CanCreateIssues/CanCreatePRs/CanMerge rely on.
//
// So it is expressed as a role refinement of the advisory mode instead. An
// ADVISORY reviewer mints the "reviewer" tier (Contents:read, PullRequests:
// write) rather than "advisor" (Contents:read, PullRequests:read), which is
// what lets it post the verdict it just computed. Every other mode, and every
// other role, is unchanged.
//
// This matters for spokes that do not auto-merge: an advisor-tier reviewer's
// verdict has exactly one consumer, the merge-eligibility gate, so where no
// merge sweep runs the review is computed and thrown away (#7469).
func TokenTierForRole(m AgentMode, role string) string {
	if m == ModeAdvisory && role == ReviewerRole {
		return "reviewer"
	}
	return m.TokenTier()
}

// CanComment reports whether an agent at this mode may post a comment on an
// issue. It sits at the ISSUES_ONLY rung: the newcomer token tier is the first
// with Issues:write, and ADVISORY (advisor, and the reviewer refinement whose
// extra permission is PullRequests:write only) may not write to issues at all.
// Used by the issue-claim path (hivecommons/hive#8380) to decide whether a
// claim is posted on the forge or recorded on the lease only.
func (m AgentMode) CanComment() bool { return m >= ModeIssuesOnly }

// ModeForTokenTier is the inverse of TokenTier / TokenTierForRole: the mode a
// scoped-token tier corresponds to. The reviewer tier maps to ADVISORY (it is
// a role refinement of that mode, see TokenTierForRole); "merger" is the
// contribute queue's name for the tier above trusted and shares its mode. An
// unknown tier reports ADVISORY and false, so a caller that gates a write on
// the result fails closed.
func ModeForTokenTier(tier string) (AgentMode, bool) {
	switch tier {
	case "advisor", "reviewer":
		return ModeAdvisory, true
	case "newcomer":
		return ModeIssuesOnly, true
	case "contributor":
		return ModeIssuesAndPRs, true
	case "trusted", "merger":
		return ModeIssuesPRsMerge, true
	default:
		return ModeAdvisory, false
	}
}
