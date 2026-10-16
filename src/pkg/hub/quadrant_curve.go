package hub

import (
	"fmt"
	"math"
)

// curveKind selects how a criterion's native value maps onto 0–100.
type curveKind int

const (
	// curveLinear interpolates straight between the worst and best values.
	// Used for ratios and small counts, where each unit is worth the same.
	curveLinear curveKind = iota
	// curveLog interpolates on log10 of the value. Used for quantities that
	// span orders of magnitude (tokens, merges per week) where going from 1
	// to 10 should be worth as much as going from 10 to 100.
	curveLog
)

// curve maps a criterion's native value to a 0–100 score. worst scores 0 and
// best scores 100; values beyond either end are clamped. Direction is implied:
// best < worst means lower is better. Every curve is a named, documented
// constant in criterionSpecs below so the reader can see — and argue with —
// exactly what each number means.
type curve struct {
	kind        curveKind
	worst, best float64
}

func (c curve) lowerIsBetter() bool { return c.best < c.worst }

// score maps v onto 0–100 along the curve.
func (c curve) score(v float64) int {
	worst, best, x := c.worst, c.best, v
	if c.kind == curveLog {
		if x <= 0 {
			// log10(0) is undefined; a zero quantity is the best possible
			// cost and the worst possible output, which is what clamping to
			// the matching end of the curve gives.
			if c.lowerIsBetter() {
				return 100
			}
			return 0
		}
		worst, best, x = math.Log10(worst), math.Log10(best), math.Log10(x)
	}
	if best == worst {
		return 0
	}
	t := (x - worst) / (best - worst)
	return clampScore(int(math.Round(math.Max(0, math.Min(1, t)) * 100)))
}

// Curve thresholds. Each pair is (worst → 0, best → 100) in the criterion's
// native unit. Named so that docs, tests and the explainer all cite the same
// number.
const (
	// ACMM autonomy: L1 is the floor every hive starts at, L6 full autonomy.
	curveAutonomyWorst, curveAutonomyBest = 1, 6
	// Merge acceptance: half of agent PRs rejected is the floor; all merged
	// is the ceiling.
	curveAcceptanceWorst, curveAcceptanceBest = 0.5, 1.0
	// Revert rate (merged PRs reverted or fixed within 14 days): zero is
	// best; one in ten is the industry "high change-failure" line.
	curveRevertWorst, curveRevertBest = 0.10, 0.0
	// False-approve rate (humans had to fix a swarm-approved PR): same scale
	// as reverts, since both are approved work that turned out wrong.
	curveFalseApproveWorst, curveFalseApproveBest = 0.10, 0.0
	// Evidence coverage: share of merged PRs with an evidence record.
	curveCoverageWorst, curveCoverageBest = 0.0, 1.0
	// Boundary block rate: proxy-blocked share of attempted agent writes.
	// One in five attempts blocked means agents routinely reach past their
	// tier.
	curveBoundaryWorst, curveBoundaryBest = 0.20, 0.0
	// Merge throughput, merged PRs per active repo per week. Log: a tenth of
	// a merge a week (one a quarter) is idle; ten a week is a repo whose
	// agents land two things every working day.
	curveThroughputWorst, curveThroughputBest = 0.1, 10.0
	// Relay tasks completed in 7 days.
	curveRelayWorst, curveRelayBest = 0.0, 20.0
	// Items waiting on a human (hold labels + plans awaiting review).
	curveHumanBlockedWorst, curveHumanBlockedBest = 20.0, 0.0
	// Items past their SLA right now.
	curveSLAWorst, curveSLABest = 10.0, 0.0
	// Tokens per merged PR. Log: 100k tokens is a tidy single-agent change;
	// 20M is a merge that burned a whole budget window.
	curveTokensPerMergeWorst, curveTokensPerMergeBest = 20_000_000.0, 100_000.0
	// Tokens per day, judged only for hives that are shipping. Log: 100k a
	// day is a few agent runs; 10M a day is a hive that needs looking at.
	curveTokensPerDayWorst, curveTokensPerDayBest = 10_000_000.0, 100_000.0
	// Rework rate: share of agent PRs rejected.
	curveReworkWorst, curveReworkBest = 0.5, 0.0
	// Merged PRs per agent seat over 90 days.
	curveOutputPerAgentWorst, curveOutputPerAgentBest = 0.0, 10.0
	// Share of agent samples found idle over the last 7 days.
	curveIdleWorst, curveIdleBest = 1.0, 0.0
	// Human override rate (humans lifted a swarm block).
	curveOverrideWorst, curveOverrideBest = 0.10, 0.0
	// SLA kept: share of 7-day samples with no SLA violation.
	curveSLAKeptWorst, curveSLAKeptBest = 0.0, 1.0
)

// Criterion weights. The axis is the weighted mean of its MEASURED criteria.
// Weights are small integers so the reader can reason about them ("reverts
// count double"); the explainer shows them.
const (
	weightHeadline = 3.0 // the criterion the axis is named for
	weightMajor    = 2.0
	weightMinor    = 1.0
)

// criterionSpec is the static definition of one criterion: how it is labelled,
// weighted, scaled and explained. The per-hive value comes from the axis
// builders in quadrant_axes.go; everything the explainer says about the
// criterion that does not depend on the hive lives here.
type criterionSpec struct {
	name   string
	label  string
	weight float64
	curve  curve
	format func(float64) string
	// good is the "what good looks like" sentence. how is derived from the
	// curve unless set.
	good string
	how  string
}

// how renders the curve as one sentence in the criterion's own units.
func (s criterionSpec) howText() string {
	if s.how != "" {
		return s.how
	}
	shape := "Linear"
	if s.curve.kind == curveLog {
		shape = "Logarithmic"
	}
	return fmt.Sprintf("%s: %s scores 0, %s scores 100.", shape, s.format(s.curve.worst), s.format(s.curve.best))
}

// Display formatters shared by the catalogue and by the fleet medians.
func fmtPercent(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }
func fmtCount(v float64) string   { return fmt.Sprintf("%d", int(math.Round(v))) }
func fmtLevel(v float64) string   { return fmt.Sprintf("L%d", int(math.Round(v))) }
func fmtYesNo(v float64) string {
	if v >= 0.5 {
		return "yes"
	}
	return "no"
}
func fmtPerWeek(v float64) string { return fmt.Sprintf("%.1f merges/repo/week", v) }
func fmtPerAgent(v float64) string {
	return fmt.Sprintf("%.1f merges/agent", v)
}
func fmtTokens(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.1fB tokens", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM tokens", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0fk tokens", v/1e3)
	}
	return fmt.Sprintf("%.0f tokens", v)
}
func fmtTokensPerDay(v float64) string { return fmtTokens(v) + "/day" }

// Criterion names are the wire keys; the dashboard explainer renders them by
// label, so renaming a key is safe for the browser but breaks any golden test.
const (
	critAutonomy     = "autonomy"
	critDelegation   = "delegation"
	critAcceptance   = "merge_acceptance"
	critBreadth      = "breadth"
	critRevert       = "revert_rate"
	critFalseApprove = "false_approve_rate"
	critCoverage     = "evidence_coverage"
	critBoundary     = "boundary_block_rate"
	critThroughput   = "merge_throughput"
	critRelay        = "relay_7d"
	critEngagement   = "contributor_engagement"
	critWorkSource   = "work_source"
	critHumanBlocked = "human_blocked"
	critSLA          = "sla_violations"
	critTimeToMerge  = "time_to_merge"
	critTokensPerPR  = "tokens_per_merge"
	critTokensPerDay = "tokens_per_day"
	critRework       = "rework_rate"
	critPerAgent     = "output_per_agent"
	critIdle         = "agent_idle_ratio"
	critCostPerMerge = "cost_per_merge"
	critOverride     = "human_override_rate"
	critSLAKept      = "sla_kept"
	critHoldLift     = "hold_lift_latency"
	critReturn       = "contributor_return"
	critPulse        = "pulse"
)

// criterionSpecs is the catalogue. quadrantCriteriaOrder below fixes the
// order each axis lists them in.
var criterionSpecs = map[string]criterionSpec{
	// --- Trust ---
	critAutonomy: {name: critAutonomy, label: "Autonomy level", weight: weightMajor,
		curve: curve{curveLinear, curveAutonomyWorst, curveAutonomyBest}, format: fmtLevel,
		good: "L5 or L6: humans let the swarm merge on its own within guardrails."},
	critDelegation: {name: critDelegation, label: "Self-merge delegated", weight: weightMinor,
		curve: curve{curveLinear, 0, 1}, format: fmtYesNo,
		how:  "Yes scores 100, no scores 0.",
		good: "The App may merge its own PRs without a human approval."},
	critAcceptance: {name: critAcceptance, label: "Merge acceptance", weight: weightMinor,
		curve: curve{curveLinear, curveAcceptanceWorst, curveAcceptanceBest}, format: fmtPercent,
		good: "Nine in ten agent PRs reach a merge rather than a rejection."},
	critBreadth: {name: critBreadth, label: "Org repos enrolled", weight: weightMinor,
		curve: curve{curveLinear, 0, 1}, format: fmtPercent,
		good: "Most of the org's repos are enrolled in the hive."},
	critRevert: {name: critRevert, label: "Reverted within 14 days", weight: weightMajor,
		curve: curve{curveLinear, curveRevertWorst, curveRevertBest}, format: fmtPercent,
		good: "Under 2% of merged PRs are reverted or follow-up-fixed."},
	critFalseApprove: {name: critFalseApprove, label: "Approved then fixed by a human", weight: weightMajor,
		curve: curve{curveLinear, curveFalseApproveWorst, curveFalseApproveBest}, format: fmtPercent,
		good: "Under 2% of swarm-approved PRs needed a human fix."},
	critCoverage: {name: critCoverage, label: "Merges with evidence", weight: weightMinor,
		curve: curve{curveLinear, curveCoverageWorst, curveCoverageBest}, format: fmtPercent,
		good: "Every merged PR has a review evidence record."},
	critBoundary: {name: critBoundary, label: "Agent writes blocked at the boundary", weight: weightMinor,
		curve: curve{curveLinear, curveBoundaryWorst, curveBoundaryBest}, format: fmtPercent,
		good: "Agents rarely attempt a write their tier does not allow."},

	// --- Productivity ---
	critThroughput: {name: critThroughput, label: "Merges per active repo per week", weight: weightHeadline,
		curve: curve{curveLog, curveThroughputWorst, curveThroughputBest}, format: fmtPerWeek,
		good: "Several merges a week in each repo the hive works."},
	critRelay: {name: critRelay, label: "Contributor-relay tasks (7d)", weight: weightMinor,
		curve: curve{curveLinear, curveRelayWorst, curveRelayBest}, format: fmtCount,
		good: "Contributors hand the relay a few tasks a day."},
	critEngagement: {name: critEngagement, label: "Contributors active", weight: weightMinor,
		curve: curve{curveLinear, 0, 1}, format: fmtPercent,
		good: "Most enrolled contributors are active."},
	critWorkSource: {name: critWorkSource, label: "Finds its own work", weight: weightMinor,
		curve: curve{curveLinear, 0, 1}, format: fmtYesNo,
		how:  "Scanner-sourced work scores 100, human-filed issues only scores 0.",
		good: "The scanner finds work rather than waiting for humans to file it."},
	critHumanBlocked: {name: critHumanBlocked, label: "Items waiting on a human", weight: weightMinor,
		curve: curve{curveLinear, curveHumanBlockedWorst, curveHumanBlockedBest}, format: fmtCount,
		good: "Nothing parked behind a hold label or an unreviewed plan."},
	critSLA: {name: critSLA, label: "Items past SLA", weight: weightMinor,
		curve: curve{curveLinear, curveSLAWorst, curveSLABest}, format: fmtCount,
		good: "No work aging past its service threshold."},
	critTimeToMerge: {name: critTimeToMerge, label: "Time to merge", weight: weightMajor,
		curve: curve{curveLog, 14, 0.5}, format: func(v float64) string { return fmt.Sprintf("%.1f days", v) },
		good: "Agent PRs merge within a day of opening."},

	// --- Efficiency ---
	critTokensPerPR: {name: critTokensPerPR, label: "Tokens per merged PR", weight: weightHeadline,
		curve: curve{curveLog, curveTokensPerMergeWorst, curveTokensPerMergeBest}, format: fmtTokens,
		good: "A merged PR costs a few hundred thousand tokens or less."},
	critTokensPerDay: {name: critTokensPerDay, label: "Token burn rate", weight: weightMinor,
		curve: curve{curveLog, curveTokensPerDayWorst, curveTokensPerDayBest}, format: fmtTokensPerDay,
		good: "Daily burn stays in the low millions while the hive ships."},
	critRework: {name: critRework, label: "Agent PRs rejected", weight: weightMinor,
		curve: curve{curveLinear, curveReworkWorst, curveReworkBest}, format: fmtPercent,
		good: "Few agent PRs are closed without merging."},
	critPerAgent: {name: critPerAgent, label: "Merged PRs per agent (90d)", weight: weightMinor,
		curve: curve{curveLinear, curveOutputPerAgentWorst, curveOutputPerAgentBest}, format: fmtPerAgent,
		good: "Every agent seat lands something most weeks."},
	critIdle: {name: critIdle, label: "Agent time idle (7d)", weight: weightMinor,
		curve: curve{curveLinear, curveIdleWorst, curveIdleBest}, format: fmtPercent,
		good: "Agents are working in most samples, not waiting."},
	critCostPerMerge: {name: critCostPerMerge, label: "Cost per merged PR", weight: weightMajor,
		curve: curve{curveLog, 100, 1}, format: func(v float64) string { return fmt.Sprintf("$%.2f", v) },
		good: "A merged PR costs a few dollars."},

	// --- Satisfaction ---
	critOverride: {name: critOverride, label: "Swarm blocks lifted by a human", weight: weightMajor,
		curve: curve{curveLinear, curveOverrideWorst, curveOverrideBest}, format: fmtPercent,
		good: "Humans almost never have to override a swarm block."},
	critSLAKept: {name: critSLAKept, label: "SLA kept (7d)", weight: weightMinor,
		curve: curve{curveLinear, curveSLAKeptWorst, curveSLAKeptBest}, format: fmtPercent,
		good: "Work is answered inside its SLA nearly all the time."},
	critHoldLift: {name: critHoldLift, label: "Time to lift a hold", weight: weightMinor,
		curve: curve{curveLog, 7, 0.25}, format: func(v float64) string { return fmt.Sprintf("%.1f days", v) },
		good: "Holds are resolved within a few hours."},
	critReturn: {name: critReturn, label: "Contributors who come back", weight: weightMinor,
		curve: curve{curveLinear, 0, 1}, format: fmtPercent,
		good: "Most contributors who used the relay use it again."},
	critPulse: {name: critPulse, label: "Direct feedback on agent output", weight: weightMajor,
		curve: curve{curveLinear, 0, 1}, format: fmtPercent,
		how:  "Share of thumbs-up on agent output, once collected.",
		good: "Humans rate most agent output as helpful."},
}

// quadrantCriteriaOrder fixes the criteria each axis lists, in display order.
// Measured or not, every criterion here appears in the explainer so the reader
// sees the whole definition of the axis.
var quadrantCriteriaOrder = map[string][]string{
	AxisTrust:        {critRevert, critFalseApprove, critAutonomy, critDelegation, critAcceptance, critCoverage, critBoundary, critBreadth},
	AxisProductivity: {critThroughput, critRelay, critEngagement, critWorkSource, critHumanBlocked, critSLA, critTimeToMerge},
	AxisEfficiency:   {critTokensPerPR, critTokensPerDay, critRework, critPerAgent, critIdle, critCostPerMerge},
	AxisSatisfaction: {critOverride, critSLAKept, critHoldLift, critReturn, critPulse},
}
