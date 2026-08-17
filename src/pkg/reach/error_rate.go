package reach

import (
	"sort"
)

// ComponentErrorDelta records the error rate change for a single component
// between the deploy window introducing a PR and the preceding deploy window
// (#3995, phase 2c of #3973).
type ComponentErrorDelta struct {
	Component       string   `json:"component"`
	SpansBefore     int64    `json:"spans_before"`
	ErrorsBefore    int64    `json:"errors_before"`
	ErrorRateBefore *float64 `json:"error_rate_before,omitempty"`
	SpansAfter      int64    `json:"spans_after"`
	ErrorsAfter     int64    `json:"errors_after"`
	ErrorRateAfter  *float64 `json:"error_rate_after,omitempty"`
	Delta           *float64 `json:"delta,omitempty"`
}

// ComputeErrorDeltas computes the pre/post deploy error-rate deltas for a PR
// across all its attributable components (#3995, phase 2c of #3973).
//
// The "after" window is the deploy window commit that shipped the PR
// (report.DeployWindow). The "before" window is the immediately preceding
// commit in deployedOldestFirst (the chronological deploy history derived from
// what the fleet reports running, the #3816 anchoring rule).
//
// D4 Co-attribution: PRs sharing a deploy window share reach and error-rate
// deltas by construction and are labeled shared_with.
func ComputeErrorDeltas(report *PRReachReport, deployedOldestFirst []string, reports map[string][]ComponentReach) {
	if report == nil || report.DeployWindow == "" || len(report.Attribution.Components) == 0 {
		return
	}

	currIdx := -1
	for i, c := range deployedOldestFirst {
		if c == report.DeployWindow {
			currIdx = i
			break
		}
	}
	if currIdx == -1 {
		return
	}

	var prevCommit string
	if currIdx > 0 {
		prevCommit = deployedOldestFirst[currIdx-1]
	}
	currCommit := report.DeployWindow

	touched := make(map[string]bool, len(report.Attribution.Components))
	for _, c := range report.Attribution.Components {
		touched[c] = true
	}

	var totalSpansBefore, totalErrorsBefore int64
	var totalSpansAfter, totalErrorsAfter int64
	componentStats := make(map[string]*ComponentErrorDelta, len(report.Attribution.Components))

	for _, comp := range report.Attribution.Components {
		componentStats[comp] = &ComponentErrorDelta{Component: comp}
	}

	for _, entries := range reports {
		for _, e := range entries {
			if !touched[e.Component] {
				continue
			}
			stats := componentStats[e.Component]
			if prevCommit != "" && e.Commit == prevCommit {
				stats.SpansBefore += e.SpansTotal
				stats.ErrorsBefore += e.SpansError
				totalSpansBefore += e.SpansTotal
				totalErrorsBefore += e.SpansError
			}
			if e.Commit == currCommit {
				stats.SpansAfter += e.SpansTotal
				stats.ErrorsAfter += e.SpansError
				totalSpansAfter += e.SpansTotal
				totalErrorsAfter += e.SpansError
			}
		}
	}

	deltas := make([]ComponentErrorDelta, 0, len(report.Attribution.Components))
	for _, comp := range report.Attribution.Components {
		stats := componentStats[comp]
		if stats.SpansBefore > 0 {
			rateB := float64(stats.ErrorsBefore) / float64(stats.SpansBefore)
			stats.ErrorRateBefore = &rateB
		}
		if stats.SpansAfter > 0 {
			rateA := float64(stats.ErrorsAfter) / float64(stats.SpansAfter)
			stats.ErrorRateAfter = &rateA
		}
		if stats.ErrorRateBefore != nil && stats.ErrorRateAfter != nil {
			d := *stats.ErrorRateAfter - *stats.ErrorRateBefore
			stats.Delta = &d
		}
		deltas = append(deltas, *stats)
	}
	sort.Slice(deltas, func(i, j int) bool {
		return deltas[i].Component < deltas[j].Component
	})
	report.ComponentErrorDeltas = deltas

	if totalSpansBefore > 0 {
		rateB := float64(totalErrorsBefore) / float64(totalSpansBefore)
		report.ErrorRateBefore = &rateB
	}
	if totalSpansAfter > 0 {
		rateA := float64(totalErrorsAfter) / float64(totalSpansAfter)
		report.ErrorRateAfter = &rateA
	}
	if report.ErrorRateBefore != nil && report.ErrorRateAfter != nil {
		d := *report.ErrorRateAfter - *report.ErrorRateBefore
		report.ErrorRateDelta = &d
	}
}

// PRReachRate computes the fraction (0.0–1.0) of deployed, attributable PRs
// that achieved non-zero production reach (at least 1 hive executing touched
// components). It is the reach signal wired alongside MergeSuccessRate (#3972)
// in the ACMM advisor's inputs (#3995, phase 2c of #3973).
//
// measured is false when no deployed attributable PRs exist in the evaluated
// set, so the caller leaves the signal at its honest, conservative zero
// instead of fabricating a 1.0 or 0.0 rate.
func PRReachRate(reports []PRReachReport) (rate float64, measured bool) {
	eligible := 0
	reached := 0
	for _, r := range reports {
		if !r.Deployed || len(r.Attribution.Components) == 0 {
			continue
		}
		eligible++
		if r.ReachCount > 0 {
			reached++
		}
	}
	if eligible == 0 {
		return 0, false
	}
	return float64(reached) / float64(eligible), true
}
