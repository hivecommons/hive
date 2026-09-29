package config

import (
	"strings"
	"time"
)

// AdvisoryConfig controls the advisory digest's size and the lifecycle of the
// beads behind it.
//
// Two operator complaints motivate it: advisory beads never closed once filed,
// so healed findings accumulated in the digest forever; and every finding was
// rendered, so a repo owner opening the digest met dozens of items with no
// indication of which few mattered.
type AdvisoryConfig struct {
	// MaxFindings caps how many findings the digest renders, chosen by severity
	// then recency across all agents.
	//
	// 0 means UNSET and resolves to defaultAdvisoryMaxFindings on load — a
	// plain int cannot tell "the operator asked for zero" from "the key is
	// absent", and defaulting is the behavior an untouched hive must get. The
	// way to lift the cap is therefore ShowAll, not max_findings: 0, which
	// would silently revert to 10 on the next config reload.
	MaxFindings int `yaml:"max_findings" json:"max_findings"`
	// ShowAll bypasses MaxFindings entirely — the opt-in for owners who want
	// the full list, and the ONLY supported way to render an uncapped digest.
	ShowAll bool `yaml:"show_all" json:"show_all"`
	// StalenessDays is how long an open advisory bead may go without being
	// re-reported before the hive auto-closes it. Default
	// defaultAdvisoryStalenessDays.
	StalenessDays int `yaml:"staleness_days" json:"staleness_days"`
	// PRAutoClose retires an advisory finding when a merged PR's title is
	// close enough to the finding's title. *bool so absent is distinct from an
	// explicit false; default true.
	PRAutoClose *bool `yaml:"pr_autoclose,omitempty" json:"pr_autoclose,omitempty"`
	// UpdateIntervalS throttles how often, in seconds, the digest comment on
	// the pinned advisory issue is refreshed (#4820). 0 (or absent) means
	// UNSET and keeps today's behavior: a post attempt every governor eval
	// cycle (~60s at the default cadence). Operators raise it to reduce
	// GitHub API writes and notification churn on watched repos.
	//
	// The raw value is stored as written so hive.yaml round-trips byte-for-
	// byte; consumers resolve it through UpdateInterval, which clamps into
	// [MinAdvisoryUpdateIntervalS, MaxAdvisoryUpdateIntervalS]. The max
	// exists for the hub's wedged-digest alarm: its staleness threshold
	// (90 min) must stay comfortably above every healthy configured cadence
	// so a user-lengthened interval never false-alarms as a wedge — pinned by
	// TestAdvisoryStaleThresholdCoversMaxUpdateInterval in pkg/hub.
	UpdateIntervalS int `yaml:"update_interval_s,omitempty" json:"update_interval_s,omitempty"`
	// Target selects where the digest comment lives: AdvisoryTargetGitHub
	// (the pinned advisory issue on the primary repo — the default, and the
	// only behavior before this key existed) or AdvisoryTargetLinear (one
	// comment on the Linear issue named by LinearIssue, rewritten each cycle
	// with the same body the GitHub comment would get). Empty means UNSET
	// and resolves to GitHub through ResolvedTarget; an unknown value fails
	// closed at post time rather than silently falling back to GitHub.
	Target string `yaml:"target,omitempty" json:"target,omitempty"`
	// LinearIssue is the Linear issue identifier (e.g. "ONB-123") that hosts
	// the digest when Target is AdvisoryTargetLinear. Required for that
	// target: an empty value logs an error naming this key and skips the
	// post — the digest is never redirected to GitHub without being asked.
	// Authentication reuses governor.work_source.linear.api_key.
	LinearIssue string `yaml:"linear_issue,omitempty" json:"linear_issue,omitempty"`
	// QueueHealth tunes the owner-advice queue-health rules (#9103): the
	// thresholds at which the weekly advice tells the owner to reduce the
	// blocked PR queue, clear the human gate, and so on. Every field is a
	// whole number and zero/absent means the shipped default (see
	// hiveadvisor.DefaultThresholds), so hive.yaml round-trips without the
	// key until an operator tunes one.
	QueueHealth AdvisoryQueueHealthConfig `yaml:"queue_health,omitempty" json:"queue_health,omitempty"`
}

// AdvisoryQueueHealthConfig holds the queue-health rule thresholds. The
// *_pct keys are percentages of the relevant Overview total; the defaults are
// starting points to calibrate against real hives, not settled policy.
type AdvisoryQueueHealthConfig struct {
	// BlockedPRPct: Blocked PRs at or above this share of open PRs fire
	// "Reduce the blocked PR queue". Default 50.
	BlockedPRPct int `yaml:"blocked_pr_pct,omitempty" json:"blocked_pr_pct,omitempty"`
	// NeedsHumanPRPct: Needs-human PRs at or above this share of open PRs
	// fire "Clear the human gate on PRs". Default 25.
	NeedsHumanPRPct int `yaml:"needs_human_pr_pct,omitempty" json:"needs_human_pr_pct,omitempty"`
	// NeedsHumanIssuePct: Needs-human issues at or above this share of open
	// issues fire "Unblock the human queue". Default 30.
	NeedsHumanIssuePct int `yaml:"needs_human_issue_pct,omitempty" json:"needs_human_issue_pct,omitempty"`
	// StaleBlockedPRs: this many Blocked PRs past dashboard.issue_bands.stale_days
	// fire "Review stale PRs". Default 10.
	StaleBlockedPRs int `yaml:"stale_blocked_prs,omitempty" json:"stale_blocked_prs,omitempty"`
	// LaneSharePct: one agent lane producing at least this share of Blocked
	// PRs fires "Throttle lane X". Default 50.
	LaneSharePct int `yaml:"lane_share_pct,omitempty" json:"lane_share_pct,omitempty"`
	// CheckSharePct: one check failing on at least this share of CI-blocked
	// PRs fires "Fix check X first". Default 50.
	CheckSharePct int `yaml:"check_share_pct,omitempty" json:"check_share_pct,omitempty"`
}

// Advisory digest targets accepted by AdvisoryConfig.Target.
const (
	AdvisoryTargetGitHub = "github"
	AdvisoryTargetLinear = "linear"
)

// ResolvedTarget returns Target with the unset default applied: an empty
// string is GitHub, because that is what every hive did before the key
// existed. Any other value is returned trimmed and lower-cased so a caller
// can reject what it does not recognize instead of guessing.
func (a AdvisoryConfig) ResolvedTarget() string {
	t := strings.ToLower(strings.TrimSpace(a.Target))
	if t == "" {
		return AdvisoryTargetGitHub
	}
	return t
}

// PRAutoCloseEnabled resolves AdvisoryConfig.PRAutoClose with its default (on).
func (a AdvisoryConfig) PRAutoCloseEnabled() bool {
	return a.PRAutoClose == nil || *a.PRAutoClose
}

// Bounds for AdvisoryConfig.UpdateIntervalS. Exported because the dashboard
// PUT validates against them and pkg/hub pins the invariant that its
// advisory-staleness threshold exceeds the maximum allowed posting cadence
// (so a healthy slow digest never reads as wedged).
const (
	// MinAdvisoryUpdateIntervalS floors a configured interval at 30s: below
	// the ~60s eval cycle the throttle is meaningless, and a typo like 3
	// would silently disable the setting.
	MinAdvisoryUpdateIntervalS = 30
	// MaxAdvisoryUpdateIntervalS caps the interval at one hour. The hub flags
	// a digest as wedged when its last successful post is older than 90
	// minutes; capping the healthy cadence at 60 minutes keeps every allowed
	// interval comfortably inside that threshold.
	MaxAdvisoryUpdateIntervalS = 3600
)

// UpdateInterval resolves UpdateIntervalS to the effective posting throttle.
// 0 means no throttle — post every eval cycle, exactly the pre-#4820 behavior
// — and a set value is clamped into [MinAdvisoryUpdateIntervalS,
// MaxAdvisoryUpdateIntervalS]. Negative values are treated as unset rather
// than clamped up, so garbage cannot silently slow a hive down.
func (a AdvisoryConfig) UpdateInterval() time.Duration {
	if a.UpdateIntervalS <= 0 {
		return 0
	}
	s := a.UpdateIntervalS
	if s < MinAdvisoryUpdateIntervalS {
		s = MinAdvisoryUpdateIntervalS
	}
	if s > MaxAdvisoryUpdateIntervalS {
		s = MaxAdvisoryUpdateIntervalS
	}
	return time.Duration(s) * time.Second
}
