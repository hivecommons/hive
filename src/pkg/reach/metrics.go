package reach

import (
	"os"
	"sort"
	"strconv"
	"time"
)

// DefaultNeverRanDays is how long after merge a deployed-but-unexecuted PR
// waits before the never-ran flag raises (#3994). Three days gives every
// heartbeat cadence in the fleet several full cycles to report a first
// execution before the PR is called unused. Tunable via
// NeverRanDaysEnvVar.
const DefaultNeverRanDays = 3

// NeverRanDaysEnvVar overrides DefaultNeverRanDays (integer days, > 0).
const NeverRanDaysEnvVar = "HIVE_REACH_NEVER_RAN_DAYS"

// NeverRanThreshold resolves the never-ran grace period from the
// environment, falling back to DefaultNeverRanDays on absent/invalid values.
func NeverRanThreshold() time.Duration {
	days := DefaultNeverRanDays
	if raw := os.Getenv(NeverRanDaysEnvVar); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			days = parsed
		}
	}
	return time.Duration(days) * 24 * time.Hour
}

// PRReachReport is the per-PR output of the phase 2b join (#3994): D3
// attribution with explicit coverage, D4 co-attribution labels, the
// ancestry-joined reach metrics, first-execution latency and the never-ran
// flag.
type PRReachReport struct {
	PR          int       `json:"pr"`
	Title       string    `json:"title,omitempty"`
	MergeCommit string    `json:"merge_commit"`
	MergedAt    time.Time `json:"merged_at"`

	// Attribution carries the components plus the honest unattributable
	// share and coverage fraction (D3).
	Attribution Attribution `json:"attribution"`

	// DeployWindow / SharedWith are the D4 labels: the deployed commit that
	// shipped this PR ("" = merged but not yet observed running anywhere)
	// and the other PRs riding the same deploy. Reach and latency below are
	// shared with those PRs by construction.
	DeployWindow string `json:"deploy_window,omitempty"`
	SharedWith   []int  `json:"shared_with,omitempty"`

	// Deployed is true when at least one hive reports running a commit that
	// contains this PR — regardless of whether its components executed.
	Deployed bool `json:"deployed"`

	// ReachHives / ReachCount: the distinct hives whose reported commit is a
	// descendant of the merge commit AND whose report shows one of the PR's
	// components executing (spans_total > 0) with first_seen after merge.
	ReachHives []string `json:"reach_hives"`
	ReachCount int      `json:"reach_count"`

	// FirstExecution is the earliest qualifying first_seen across the
	// fleet; FirstExecutionLatencySeconds is merge → that instant. Nil when
	// nothing qualifying has executed yet.
	FirstExecution               *time.Time `json:"first_execution,omitempty"`
	FirstExecutionLatencySeconds *float64   `json:"first_execution_latency_seconds,omitempty"`

	// NeverRan raises when the PR is merged AND deployed AND every
	// attributable component still shows zero qualifying executions after
	// the grace period — the category acceptance-rate cannot see (#3973).
	// A PR with no attributable components (docs-only) never raises it.
	NeverRan              bool `json:"never_ran"`
	NeverRanThresholdDays int  `json:"never_ran_threshold_days"`
}

// Analyzer joins merged-PR facts against the fleet's reach reports. All
// dependencies are injected: Ancestry (hub repo clone or a test fake),
// Reporter (2a's store once merged; StubReachReporter until then), the
// never-ran grace period, and the clock.
type Analyzer struct {
	Ancestry Ancestry
	Reporter ReachReporter
	// NeverRanAfter is the never-ran grace period; zero means
	// NeverRanThreshold() (env-tunable default).
	NeverRanAfter time.Duration
	// Now is the clock; nil means time.Now. Injected for tests.
	Now func() time.Time
}

// Analyze produces the reach report for one PR. The D4 window labels
// (DeployWindow/SharedWith) are filled by the caller via AssignWindows over
// the full PR set — a single PR in isolation cannot know its co-riders.
func (a *Analyzer) Analyze(pr PRInfo) (PRReachReport, error) {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	grace := a.NeverRanAfter
	if grace == 0 {
		grace = NeverRanThreshold()
	}

	report := PRReachReport{
		PR:                    pr.Number,
		Title:                 pr.Title,
		MergeCommit:           pr.MergeCommit,
		MergedAt:              pr.MergedAt,
		Attribution:           Attribute(pr.Files),
		ReachHives:            []string{},
		NeverRanThresholdDays: int(grace / (24 * time.Hour)),
	}

	touched := map[string]bool{}
	for _, c := range report.Attribution.Components {
		touched[c] = true
	}

	var firstExecution *time.Time
	for hiveID, entries := range a.Reporter.LatestReach() {
		qualified := false
		for _, entry := range entries {
			if entry.Commit == "" {
				continue
			}
			// Ancestry gate: the hive must be RUNNING a commit that
			// contains the PR (anchoring rule, #3973/#3816).
			contains, err := a.Ancestry.IsAncestor(pr.MergeCommit, entry.Commit)
			if err != nil {
				return PRReachReport{}, err
			}
			if !contains {
				continue
			}
			report.Deployed = true
			// Execution gate: one of the PR's components ran on that
			// build, first observed after the merge.
			if !touched[entry.Component] || entry.SpansTotal <= 0 || !entry.FirstSeen.After(pr.MergedAt) {
				continue
			}
			qualified = true
			if firstExecution == nil || entry.FirstSeen.Before(*firstExecution) {
				t := entry.FirstSeen
				firstExecution = &t
			}
		}
		if qualified {
			report.ReachHives = append(report.ReachHives, hiveID)
		}
	}
	sort.Strings(report.ReachHives)
	report.ReachCount = len(report.ReachHives)

	if firstExecution != nil {
		report.FirstExecution = firstExecution
		latency := firstExecution.Sub(pr.MergedAt).Seconds()
		report.FirstExecutionLatencySeconds = &latency
	}

	// Never-ran: merged, deployed, attributable — and still nothing
	// qualifying executed after the grace period.
	if report.Deployed &&
		len(report.Attribution.Components) > 0 &&
		report.ReachCount == 0 &&
		now().Sub(pr.MergedAt) >= grace {
		report.NeverRan = true
	}
	return report, nil
}
