package reach

// Error-rate deltas per deploy window (#3995, phase 2c of #3973).
//
// The hub stores only each hive's LATEST component_reach report (2a's
// deliberate design) — sufficient for reach, useless for before/after. Phase
// 2c adds a fleet-level per-component history of hourly windows on the hub
// (pkg/hub/reach_history.go); this file holds the shapes that history is read
// through and the pure delta computation over it.
//
// D4 semantics (accepted design, #3973): the delta is keyed on the DEPLOY
// WINDOW, not the PR — the deployed commit AssignWindows attributed the PR to.
// Co-deployed PRs therefore share one delta by construction and are labeled
// shared; this package never reimplements windowing (two implementations that
// can disagree would be a correctness bug — windows.go is the one authority).

import (
	"sort"
	"time"
)

// minCommitMatchLen is the shortest commit-id prefix accepted as identifying
// a commit when comparing a short spoke-reported SHA against another id.
// Seven hex chars is git's own default abbreviation floor; anything shorter
// is too collision-prone to attribute error rates to.
const minCommitMatchLen = 7

// HistorySample is one fleet-summed activity bucket for a component:
// spans_total/spans_error accumulated across every reporting hive whose
// rolling 1h window fell into this hour, split by the commit that was
// RUNNING (the anchoring rule — a window's errors belong to the binary that
// produced them, never to a tag).
type HistorySample struct {
	// WindowStart is the hour bucket (UTC, truncated to the hour). Spoke
	// rolling windows have arbitrary starts, so the hub buckets them onto a
	// common hourly grid to make cross-hive summation meaningful; the up-to-1h
	// smear this introduces is inherent and applies equally to both sides of a
	// delta.
	WindowStart time.Time `json:"window_start"`
	Commit      string    `json:"commit"`
	SpansTotal  int64     `json:"spans_total"`
	SpansError  int64     `json:"spans_error"`
}

// HistorySource is the seam between this package and the hub's retention
// store, mirroring ReachReporter's role for the latest-report store.
type HistorySource interface {
	// ComponentWindows returns the fleet-level history samples for one
	// component, oldest WindowStart first. The returned slice is a snapshot
	// the caller may read freely; an untracked component returns nil.
	ComponentWindows(component string) []HistorySample
}

// StubHistorySource is the in-memory HistorySource used by tests and as the
// safe default: absent data reads as "no history", never as zero error rate.
type StubHistorySource struct {
	// Windows, when non-nil, maps component → samples (oldest first).
	Windows map[string][]HistorySample
}

// ComponentWindows implements HistorySource.
func (s *StubHistorySource) ComponentWindows(component string) []HistorySample {
	if s == nil || s.Windows == nil {
		return nil
	}
	return s.Windows[component]
}

// ComponentErrorDelta is the per-(component, deploy-window) error-rate delta:
// the error ratio in windows AFTER the deployed commit first appears versus an
// equal count of windows immediately BEFORE. Measured is FALSE whenever either
// side lacks data (deploy never observed, no before-history, or zero spans on
// a side) — unmeasured must be distinguishable from zero-delta everywhere this
// surfaces, so consumers MUST check Measured before reading the numbers.
type ComponentErrorDelta struct {
	Component string `json:"component"`
	// ErrorRateBefore / ErrorRateAfter are span-error ratios in [0,1].
	ErrorRateBefore float64 `json:"error_rate_before"`
	ErrorRateAfter  float64 `json:"error_rate_after"`
	// Delta is after − before: positive means errors ROSE after the deploy.
	Delta float64 `json:"delta"`
	// WindowsCompared is the per-side window count (equal by construction).
	WindowsCompared int  `json:"windows_compared"`
	Measured        bool `json:"measured"`
}

// commitsMatch reports whether two commit ids identify the same commit,
// tolerating the short-vs-full SHA mismatch between spoke reports (ldflags
// short hash) and other sources: exact equality, or one being a prefix of the
// other with at least minCommitMatchLen characters.
func commitsMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	return len(shorter) >= minCommitMatchLen && longer[:len(shorter)] == shorter
}

// ComputeErrorDelta computes the D4 error-rate delta for one component across
// the deploy window that shipped deployedCommit. samples is the component's
// fleet history, oldest first (HistorySource.ComponentWindows order).
//
// Semantics, exactly:
//   - the boundary is the earliest bucket where deployedCommit is observed
//     RUNNING (never a merge or tag event — the #3816 anchoring rule);
//   - the AFTER side counts only spans attributed to deployedCommit, so a
//     staggered rollout's old-binary traffic never dilutes the new code's
//     ratio;
//   - the BEFORE side counts all pre-boundary traffic (the old regime,
//     whichever commits it ran);
//   - both sides use an EQUAL count of windows, taken adjacent to the
//     boundary (the most comparable regimes).
//
// A caution the numbers cannot express: windows are summed across hives, so
// one high-volume spoke CAN dominate — and therefore skew — a window's ratio.
// The clamps upstream bound how far, but they cannot make a fleet sum immune
// to a single loud reporter; treat small deltas accordingly.
func ComputeErrorDelta(samples []HistorySample, component, deployedCommit string) ComponentErrorDelta {
	out := ComponentErrorDelta{Component: component}
	if deployedCommit == "" || len(samples) == 0 {
		return out
	}

	// Group samples into buckets, ascending. The input contract is sorted,
	// but a stable delta must not depend on a caller honoring it — re-sort.
	byBucket := map[time.Time][]HistorySample{}
	for _, s := range samples {
		byBucket[s.WindowStart] = append(byBucket[s.WindowStart], s)
	}
	buckets := make([]time.Time, 0, len(byBucket))
	for b := range byBucket {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Before(buckets[j]) })

	// Boundary: earliest bucket where the deployed commit was seen running.
	boundary := -1
	for i, b := range buckets {
		for _, s := range byBucket[b] {
			if commitsMatch(s.Commit, deployedCommit) {
				boundary = i
				break
			}
		}
		if boundary >= 0 {
			break
		}
	}
	if boundary < 0 {
		// Deploy never observed in the retained history — unmeasured, and
		// distinguishable from "measured, delta 0".
		return out
	}

	// AFTER: buckets from the boundary on that carry deployed-commit traffic.
	var afterBuckets []time.Time
	for _, b := range buckets[boundary:] {
		for _, s := range byBucket[b] {
			if commitsMatch(s.Commit, deployedCommit) {
				afterBuckets = append(afterBuckets, b)
				break
			}
		}
	}
	beforeBuckets := buckets[:boundary]

	compared := len(afterBuckets)
	if len(beforeBuckets) < compared {
		compared = len(beforeBuckets)
	}
	if compared == 0 {
		return out
	}
	// Equal counts, adjacent to the boundary: the LAST `compared` before
	// windows and the FIRST `compared` after windows.
	beforeBuckets = beforeBuckets[len(beforeBuckets)-compared:]
	afterBuckets = afterBuckets[:compared]

	var beforeTotal, beforeErr, afterTotal, afterErr int64
	for _, b := range beforeBuckets {
		for _, s := range byBucket[b] {
			beforeTotal += s.SpansTotal
			beforeErr += s.SpansError
		}
	}
	for _, b := range afterBuckets {
		for _, s := range byBucket[b] {
			if commitsMatch(s.Commit, deployedCommit) {
				afterTotal += s.SpansTotal
				afterErr += s.SpansError
			}
		}
	}
	if beforeTotal <= 0 || afterTotal <= 0 {
		// A ratio over zero executions is undefined; reporting it as 0.0
		// would fabricate a perfect error rate from absence of data.
		return out
	}

	out.ErrorRateBefore = float64(beforeErr) / float64(beforeTotal)
	out.ErrorRateAfter = float64(afterErr) / float64(afterTotal)
	out.Delta = out.ErrorRateAfter - out.ErrorRateBefore
	out.WindowsCompared = compared
	out.Measured = true
	return out
}
