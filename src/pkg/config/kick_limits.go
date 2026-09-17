package config

// Kick prompt list caps (hivecommons/hive#7368).
//
// A kick prompt lists the agent's actionable issues and open PRs. Both lists
// are bounded so a hive with a large backlog does not deliver a prompt whose
// size is dominated by items the agent will never reach in one turn: the tail
// of a 300-item list buys nothing, costs tokens on every kick, and — because
// delivery types the prompt into a terminal — stretches the delivery window
// (measured ~3 minutes for a 69.5 KiB kick), which is what made the
// restart-during-delivery loop of #7363 unrecoverable in practice.
//
// The issue cap has always existed (maxIssuesPerKick=100); the PR cap was
// missing, so a spoke with 302 open PRs produced a 69.5 KiB kick against a
// documented ~22 KiB worst case. Both are now one operator-tunable block.

// Defaults for KickLimitsConfig. The issue default is the historical
// constant; the PR default keeps the fully-expanded prompt near the budget
// documented in pkg/dashboard/prompt_history.go (~120 B per PR line).
const (
	DefaultMaxIssuesPerKick = 100
	DefaultMaxPRsPerKick    = 50
)

// Bounds an operator may set a cap to. The ceiling matters: without it a
// setting of `max_prs: 100000` is honoured verbatim and reproduces the very
// unbounded prompt this block exists to prevent, through the control meant to
// prevent it. 500 is far above any useful backlog slice for a single turn
// while still keeping the fully-expanded list inside a sane prompt size.
const (
	MinKickListCap = 1
	MaxKickListCap = 500
)

// clampKickListCap resolves one configured cap: non-positive means "unset, use
// the default", and anything above the ceiling is pinned to it rather than
// rejected, so a bad value in hive.yaml degrades to a safe cap instead of
// failing the hive's startup.
func clampKickListCap(v, def int) int {
	if v <= 0 {
		return def
	}
	if v > MaxKickListCap {
		return MaxKickListCap
	}
	return v
}

// KickLimitsConfig is `governor.kick_limits`: how many items each list in a
// kick prompt may carry. Zero or absent means the default; a negative value is
// treated as the default too (a cap of "none" is exactly the failure this
// exists to prevent). When a list is cut, the prompt says so with an explicit
// "… and N more" line so the agent knows the list is partial.
type KickLimitsConfig struct {
	// MaxIssues caps every issue list in a kick (work list, lane lists, held
	// PR list). Default DefaultMaxIssuesPerKick.
	MaxIssues int `yaml:"max_issues,omitempty" json:"max_issues,omitempty"`
	// MaxPRs caps every PR list in a kick (actionable PRs, stale drafts,
	// merge-eligible, CI-failing). Default DefaultMaxPRsPerKick.
	MaxPRs int `yaml:"max_prs,omitempty" json:"max_prs,omitempty"`
}

// IssuesPerKick returns max_issues with the default and the ceiling applied.
func (k KickLimitsConfig) IssuesPerKick() int {
	return clampKickListCap(k.MaxIssues, DefaultMaxIssuesPerKick)
}

// PRsPerKick returns max_prs with the default and the ceiling applied.
func (k KickLimitsConfig) PRsPerKick() int {
	return clampKickListCap(k.MaxPRs, DefaultMaxPRsPerKick)
}
