package hub

import (
	"fmt"
)

// subCriterion is one criterion of an axis for one hive: the static spec plus
// this hive's measurement (or the reason there is none).
type subCriterion struct {
	spec     criterionSpec
	measured bool
	value    float64
	reason   string // why unmeasured; shown as "not measured yet — …"
	nudge    string // shown when this criterion is the axis's weakest link
	fact     string // neutral clause for the one-line summary, e.g. "1.2% of merges reverted"
}

func (c subCriterion) score() int { return c.spec.curve.score(c.value) }

// measured builds a scored criterion. fact is the plain clause the summary
// quotes; nudge is the action when this is the axis's weakest measured link.
func measured(name string, value float64, fact, nudge string) subCriterion {
	return subCriterion{spec: criterionSpecs[name], measured: true, value: value, fact: fact, nudge: nudge}
}

// unmeasured lists a criterion the axis is defined by but this hive has no
// input for. It is shown, never scored, and never counted in the axis mean.
func unmeasured(name, reason string) subCriterion {
	return subCriterion{spec: criterionSpecs[name], reason: reason}
}

// Reasons for the inputs the hub does not receive yet. Kept as constants so
// the explainer, the docs and the tests say the same thing about what would
// feed each criterion.
const (
	reasonNoEvidence    = "spoke has not reported an evidence summary yet"
	reasonFewVerdicts   = "fewer than 10 reviewed verdicts in 90 days"
	reasonFewMerges     = "fewer than 10 merged PRs with evidence in 90 days"
	reasonFewWrites     = "fewer than 10 proxied agent writes recorded"
	reasonNoOrgCount    = "spokes do not report the org's repo count yet"
	reasonNoFleetStats  = "spoke has not reported its PR counts yet"
	reasonNoBudget      = "no budget window or hive-state token trend reported"
	reasonNoAgents      = "no agents configured"
	reasonIdleForCost   = "hive is not shipping, so burn rate says nothing about efficiency"
	reasonFewPRsForRate = "fewer than 4 agent PRs reached a verdict"
	reasonNoHiveState   = "spoke has not reported 7-day hive-state trends yet"
	reasonNoRelay       = "contributor relay not reported"
	reasonNoHoldQueue   = "hold and review queues not reported"
	reasonNoSLA         = "SLA counts not reported"
	reasonNoTimeToMerge = "spokes do not report time-to-merge yet"
	reasonNoLaunchCost  = "per-launch cost stays on the spoke's launch-cost API; not on the heartbeat yet"
	reasonNoHoldLift    = "hold-lift latency is not reported yet"
	reasonNoReturn      = "contributor return rate is not reported yet"
	reasonNoPulse       = "no direct feedback signal exists yet; a thumbs up/down on agent output would feed it"
)

// trustCriteria: how much rope the humans have given this hive AND whether the
// outcomes justify it. Delegation (autonomy, self-merge, acceptance) says what
// was granted; the evidence criteria (reverts, false approves, coverage,
// boundary blocks) say whether that trust is being honoured.
func trustCriteria(in quadrantInputs) []subCriterion {
	e := in.evidence
	var out []subCriterion

	if e == nil {
		out = append(out, unmeasured(critRevert, reasonNoEvidence), unmeasured(critFalseApprove, reasonNoEvidence))
	} else {
		if rate, ok := evidenceRate(e.Reverted14d90d, e.Merged90d); ok {
			n := ""
			if rate > 0.05 {
				n = fmt.Sprintf("%d of %d merged PRs were reverted or fixed within 14 days", e.Reverted14d90d, e.Merged90d)
			}
			out = append(out, measured(critRevert, rate, fmtPercent(rate)+" of merges reverted", n))
		} else {
			out = append(out, unmeasured(critRevert, reasonFewMerges))
		}
		if rate, ok := evidenceRate(e.FalseApprove90d, e.Verdicts90d); ok {
			n := ""
			if rate > 0.05 {
				n = fmt.Sprintf("Humans had to fix %d of %d swarm-approved PRs", e.FalseApprove90d, e.Verdicts90d)
			}
			out = append(out, measured(critFalseApprove, rate, fmtPercent(rate)+" of approvals needed a human fix", n))
		} else {
			out = append(out, unmeasured(critFalseApprove, reasonFewVerdicts))
		}
	}

	if in.acmmLevel > 0 {
		out = append(out, measured(critAutonomy, float64(in.acmmLevel), fmt.Sprintf("at ACMM L%d", in.acmmLevel),
			fmt.Sprintf("At ACMM L%d — consider trying the next level on one repo", in.acmmLevel)))
	} else {
		out = append(out, unmeasured(critAutonomy, "ACMM level not reported"))
	}

	if in.selfMergeAllowed != nil {
		v, n, f := 0.0, "Self-authored auto-merge is not allowed — raise the ACMM level to delegate it", "self-merge not delegated"
		if *in.selfMergeAllowed {
			v, n, f = 1, "", "self-merge delegated"
		}
		out = append(out, measured(critDelegation, v, f, n))
	} else {
		out = append(out, unmeasured(critDelegation, reasonNoEvidence))
	}

	if in.mergeAcceptance != nil {
		n := ""
		if *in.mergeAcceptance < 0.7 {
			n = fmt.Sprintf("%.0f%% of agent PRs are merged — tune the mission", *in.mergeAcceptance*100)
		}
		out = append(out, measured(critAcceptance, *in.mergeAcceptance, fmtPercent(*in.mergeAcceptance)+" of agent PRs merged", n))
	} else if in.prsMerged90d == nil || in.prsRejected90d == nil {
		out = append(out, unmeasured(critAcceptance, reasonNoFleetStats))
	} else {
		out = append(out, unmeasured(critAcceptance, reasonFewPRsForRate))
	}

	// Evidence coverage: merged PRs that carry an evidence record over the
	// hive's merged count. Uses the org-wide collector count as denominator
	// because that is the figure the evidence view's record_coverage uses.
	switch {
	case e == nil:
		out = append(out, unmeasured(critCoverage, reasonNoEvidence))
	case in.prsMerged90d == nil || *in.prsMerged90d <= 0:
		out = append(out, unmeasured(critCoverage, reasonNoFleetStats))
	default:
		cov := float64(e.Merged90d) / float64(*in.prsMerged90d)
		if cov > 1 {
			cov = 1
		}
		n := ""
		if cov < 0.5 {
			n = fmt.Sprintf("Only %s of merged PRs have an evidence record — enable the evidence pipeline on every repo", fmtPercent(cov))
		}
		out = append(out, measured(critCoverage, cov, fmtPercent(cov)+" of merges carry evidence", n))
	}

	// Boundary block rate from the proxy write counters, summed across tiers.
	if e == nil || e.WriteCounters == nil {
		out = append(out, unmeasured(critBoundary, reasonNoEvidence))
	} else {
		attempted, blocked := 0, 0
		for _, c := range e.WriteCounters {
			attempted += c.Attempted
			blocked += c.Blocked
		}
		if rate, ok := evidenceRate(blocked, attempted); ok {
			n := ""
			if rate > 0.1 {
				n = fmt.Sprintf("%d of %d agent writes were blocked at the proxy — agents are reaching past their tier", blocked, attempted)
			}
			out = append(out, measured(critBoundary, rate, fmtPercent(rate)+" of agent writes blocked", n))
		} else {
			out = append(out, unmeasured(critBoundary, reasonFewWrites))
		}
	}

	if in.reposInOrg > 0 && in.reposEnrolled >= 0 {
		ratio := float64(in.reposEnrolled) / float64(in.reposInOrg)
		n := ""
		if in.reposEnrolled < in.reposInOrg {
			n = fmt.Sprintf("Only %d of %d org repos enrolled", in.reposEnrolled, in.reposInOrg)
		}
		out = append(out, measured(critBreadth, ratio, fmtPercent(ratio)+" of org repos enrolled", n))
	} else {
		out = append(out, unmeasured(critBreadth, reasonNoOrgCount))
	}
	return out
}

// efficiencyCriteria: what an outcome costs. The headline is tokens per merged
// PR; burn rate is judged only for hives that are shipping, because a hive
// that spends nothing and ships nothing is idle, not efficient.
func efficiencyCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion
	shipping := (in.prsMerged90d != nil && *in.prsMerged90d > 0) ||
		(in.mergesPerWeek != nil && *in.mergesPerWeek > 0) ||
		(in.tasksCompleted7d != nil && *in.tasksCompleted7d > 0)

	switch {
	case in.tokensPerDay == nil:
		out = append(out, unmeasured(critTokensPerPR, reasonNoBudget))
	case in.prsMerged90d == nil:
		out = append(out, unmeasured(critTokensPerPR, reasonNoFleetStats))
	case *in.prsMerged90d < minMergedPRsForCostRatio:
		out = append(out, unmeasured(critTokensPerPR, fmt.Sprintf("fewer than %d merged PRs in 90 days", minMergedPRsForCostRatio)))
	default:
		// tokens/day × 90 days ÷ merges in 90 days. Approximate: the burn
		// rate is current while the merge count is a trailing window.
		per := *in.tokensPerDay * 90 / float64(*in.prsMerged90d)
		out = append(out, measured(critTokensPerPR, per, fmtTokens(per)+" per merged PR",
			"Token spend per merged PR is high — check for agents looping or oversized missions"))
	}

	switch {
	case in.tokensPerDay == nil:
		out = append(out, unmeasured(critTokensPerDay, reasonNoBudget))
	case !shipping:
		out = append(out, unmeasured(critTokensPerDay, reasonIdleForCost))
	default:
		out = append(out, measured(critTokensPerDay, *in.tokensPerDay, "burning "+fmtTokensPerDay(*in.tokensPerDay),
			"Token burn rate is high for what is shipping"))
	}

	if in.prsMerged90d != nil && in.prsRejected90d != nil {
		total := *in.prsMerged90d + *in.prsRejected90d
		if total >= minPRsForReworkRate {
			rate := float64(*in.prsRejected90d) / float64(total)
			n := ""
			if rate > 0.25 {
				n = fmt.Sprintf("%d of %d agent PRs were rejected — tune the mission", *in.prsRejected90d, total)
			}
			out = append(out, measured(critRework, rate, fmtPercent(rate)+" of agent PRs rejected", n))
		} else {
			out = append(out, unmeasured(critRework, reasonFewPRsForRate))
		}
	} else {
		out = append(out, unmeasured(critRework, reasonNoFleetStats))
	}

	switch {
	case in.agentCount <= 0:
		out = append(out, unmeasured(critPerAgent, reasonNoAgents))
	case in.prsMerged90d == nil:
		out = append(out, unmeasured(critPerAgent, reasonNoFleetStats))
	default:
		perAgent := float64(*in.prsMerged90d) / float64(in.agentCount)
		n := ""
		if perAgent < 1 {
			n = fmt.Sprintf("%d agents but %d merged PRs — consider pausing idle agents", in.agentCount, *in.prsMerged90d)
		}
		out = append(out, measured(critPerAgent, perAgent, fmt.Sprintf("%.1f merges per agent", perAgent), n))
	}

	if w := hiveStateWeek(in); w != nil && (w.AgentSamples > 0 || w.AgentIdleRatio > 0) {
		// Older spokes omit the denominator; only a positive ratio proves observations.
		n := ""
		if w.AgentIdleRatio > 0.75 {
			n = fmt.Sprintf("Agents were idle in %s of samples this week — give them work or pause seats", fmtPercent(w.AgentIdleRatio))
		}
		out = append(out, measured(critIdle, w.AgentIdleRatio, "agents idle "+fmtPercent(w.AgentIdleRatio)+" of the time", n))
	} else {
		out = append(out, unmeasured(critIdle, reasonNoHiveState))
	}

	out = append(out, unmeasured(critCostPerMerge, reasonNoLaunchCost))
	return out
}

// productivityCriteria: throughput attributable to THIS hive. The headline is
// merges per active repo per week from the hive's own audit summary; the
// org-wide collector count is only the fallback, normalised per enrolled repo.
func productivityCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion

	switch {
	case in.mergesPerWeek != nil:
		repos := in.activeRepos
		if repos < 1 {
			repos = 1
		}
		per := *in.mergesPerWeek / float64(repos)
		out = append(out, measured(critThroughput, per, fmtPerWeek(per), zeroMergeNudge(in, per)))
	case in.prsMerged90d != nil:
		repos := in.reposEnrolled
		if repos < 1 {
			repos = 1
		}
		per := float64(*in.prsMerged90d) / (90.0 / 7.0) / float64(repos)
		out = append(out, measured(critThroughput, per, fmtPerWeek(per)+" (org-wide count)", zeroMergeNudge(in, per)))
	default:
		out = append(out, unmeasured(critThroughput, reasonNoFleetStats))
	}

	if in.tasksCompleted7d != nil {
		n := ""
		if *in.tasksCompleted7d == 0 && in.contributorCount > 0 {
			n = "Contributor relay is idle"
		}
		out = append(out, measured(critRelay, float64(*in.tasksCompleted7d), fmt.Sprintf("%d relay tasks this week", *in.tasksCompleted7d), n))
	} else {
		out = append(out, unmeasured(critRelay, reasonNoRelay))
	}

	if in.contributorCount > 0 {
		ratio := float64(in.activeContributors) / float64(in.contributorCount)
		n := ""
		if in.activeContributors < in.contributorCount {
			n = fmt.Sprintf("%d of %d contributors active", in.activeContributors, in.contributorCount)
		}
		out = append(out, measured(critEngagement, ratio, fmt.Sprintf("%d of %d contributors active", in.activeContributors, in.contributorCount), n))
	} else {
		out = append(out, unmeasured(critEngagement, "no contributors enrolled"))
	}

	// WorkSource is empty for the default GitHub Issues source, so this is
	// always measured: an empty string is a real answer.
	if in.workSource != "" && in.workSource != "issues" {
		out = append(out, measured(critWorkSource, 1, "finds its own work", ""))
	} else {
		out = append(out, measured(critWorkSource, 0, "works only human-filed issues", "All work is human-filed — let the scanner find work"))
	}

	if in.holdTotal != nil || in.awaitingReview != nil {
		blocked := 0
		if in.holdTotal != nil {
			blocked += *in.holdTotal
		}
		if in.awaitingReview != nil {
			blocked += *in.awaitingReview
		}
		n := ""
		if blocked > 0 {
			n = fmt.Sprintf("%d items waiting on a human decision", blocked)
		}
		out = append(out, measured(critHumanBlocked, float64(blocked), fmt.Sprintf("%d items waiting on a human", blocked), n))
	} else {
		out = append(out, unmeasured(critHumanBlocked, reasonNoHoldQueue))
	}

	if in.slaViolations != nil {
		n := ""
		if *in.slaViolations > 0 {
			n = fmt.Sprintf("%d items past their SLA", *in.slaViolations)
		}
		out = append(out, measured(critSLA, float64(*in.slaViolations), fmt.Sprintf("%d items past SLA", *in.slaViolations), n))
	} else {
		out = append(out, unmeasured(critSLA, reasonNoSLA))
	}

	out = append(out, unmeasured(critTimeToMerge, reasonNoTimeToMerge))
	return out
}

// zeroMergeNudge tells a throttled hive about its budget rather than telling it
// to ship more, which would be both wrong and useless.
func zeroMergeNudge(in quadrantInputs, perWeek float64) string {
	if perWeek > 0 {
		return ""
	}
	if in.budgetExhausted != nil && *in.budgetExhausted {
		return "Budget exhausted — agent kicks are suppressed"
	}
	return "No agent merges in the window"
}

// satisfactionCriteria: how it feels to depend on the swarm. The measured
// signals are how often a human had to lift a swarm block and how often work
// stayed inside its SLA; the rest of the axis is listed so the reader can see
// what is still missing, instead of a bare dash.
func satisfactionCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion
	e := in.evidence
	switch {
	case e == nil:
		out = append(out, unmeasured(critOverride, reasonNoEvidence))
	default:
		if rate, ok := evidenceRate(e.FalseBlock90d, e.Verdicts90d); ok {
			n := ""
			if rate > 0.05 {
				n = fmt.Sprintf("Humans overrode %d of %d swarm blocks — the gate may be too strict", e.FalseBlock90d, e.Verdicts90d)
			}
			out = append(out, measured(critOverride, rate, fmtPercent(rate)+" of swarm blocks lifted by a human", n))
		} else {
			out = append(out, unmeasured(critOverride, reasonFewVerdicts))
		}
	}

	if w := hiveStateWeek(in); w != nil {
		kept := 1 - w.SLAViolationRate
		if kept < 0 {
			kept = 0
		}
		n := ""
		if kept < 0.9 {
			n = fmt.Sprintf("SLA was breached in %s of samples this week — clear the oldest queue items", fmtPercent(w.SLAViolationRate))
		}
		out = append(out, measured(critSLAKept, kept, "SLA kept "+fmtPercent(kept)+" of the week", n))
	} else {
		out = append(out, unmeasured(critSLAKept, reasonNoHiveState))
	}

	out = append(out,
		unmeasured(critHoldLift, reasonNoHoldLift),
		unmeasured(critReturn, reasonNoReturn),
		unmeasured(critPulse, reasonNoPulse))
	return out
}

// hiveStateWeek returns the 7-day hive-state window when the spoke reported
// enough samples for its ratios to mean anything.
func hiveStateWeek(in quadrantInputs) *hiveStateWindow {
	if in.evidence == nil || in.evidence.HiveState == nil {
		return nil
	}
	w := in.evidence.HiveState.Last7d
	if w.Samples < minHiveStateSamples {
		return nil
	}
	return &hiveStateWindow{AgentSamples: w.AgentSamples, AgentIdleRatio: w.AgentIdleRatio, SLAViolationRate: w.SLAViolationRate}
}

type hiveStateWindow struct {
	AgentSamples     int
	AgentIdleRatio   float64
	SLAViolationRate float64
}

// unscoredReason explains, per axis, what evidence is missing when NONE of
// the axis's criteria is measured. Shown in the hover so a collapsed spoke
// reads as "not measured yet" rather than as a gap the viewer has to interpret.
func unscoredReason(axis string) string {
	switch axis {
	case AxisSatisfaction:
		return "Not measured yet: needs ≥10 reviewed verdicts or a 7-day hive-state trend from the spoke"
	case AxisEfficiency:
		return "Not measured yet: needs a budget window or token trend and PR counts from the spoke"
	case AxisProductivity:
		return "Not measured yet: no activity or PR counts reported by the spoke"
	case AxisTrust:
		return "Not measured yet: ACMM level and evidence summary not reported by the spoke"
	default:
		return "Not measured yet"
	}
}

// criteriaFor dispatches to the right sub-criteria builder for an axis. The
// result lists every criterion in quadrantCriteriaOrder exactly once.
func criteriaFor(axis string, in quadrantInputs) []subCriterion {
	switch axis {
	case AxisTrust:
		return trustCriteria(in)
	case AxisEfficiency:
		return efficiencyCriteria(in)
	case AxisProductivity:
		return productivityCriteria(in)
	case AxisSatisfaction:
		return satisfactionCriteria(in)
	default:
		return nil
	}
}
