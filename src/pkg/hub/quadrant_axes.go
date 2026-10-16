package hub

import (
	"fmt"
	"math"
)

// Fixed reference points, not fleet ranks. These are operator-facing heuristics,
// not merge gates or claims of calibrated probabilities. Keep the explanations
// alongside the curves so the API and browser describe the actual calculation.
const (
	goodMergesPerRepoWeek = 5.0
	poorTokensPerPR       = 10_000_000.0
	poorOutcomeRate       = 0.25
	poorSLACount          = 10.0
)

type subCriterion struct {
	name  string
	value float64
	nudge string
}

type criterionDefinition struct {
	name, covers, unit, source, good string
	low, high                        float64 // native values mapping to 0 and 100 respectively
	logarithmic                      bool
}

func criterionDefinitions(axis string) []criterionDefinition {
	switch axis {
	case AxisTrust:
		return []criterionDefinition{
			{"revert_rate", "Changes reverted or fixed within 14 days", "ratio", "evidence, 90 days; at least 10 merges", "No reverted changes", poorOutcomeRate, 0, false},
			{"false_approve_rate", "Approved work needing a human fix", "ratio", "evidence, 90 days; at least 10 verdicts", "No false approvals", poorOutcomeRate, 0, false},
			{"evidence_coverage", "Merged PRs with an evidence record / org AI-author merges (approximate scope)", "ratio", "evidence and org collector, 90 days", "All merges recorded", 0, 1, false},
			{"boundary_blocked_rate", "Agent write attempts blocked at the proxy boundary (not gate effectiveness)", "ratio", "proxy counters since restart; at least 10 attempts", "No out-of-bound attempts", poorOutcomeRate, 0, false},
		}
	case AxisProductivity:
		return []criterionDefinition{
			{"merges_per_repo_week", "Hive audit merges per enrolled repo per week; fallback is org AI-author output", "merges/repo/week", "collector count-window repo activity (older spokes: 14 days), else approximate 90-day org collector", "5 merges per repo per week", 0, goodMergesPerRepoWeek, true},
			{"time_to_merge", "Time from opening to merging a PR", "hours", "Not on the heartbeat yet", "Not scored until reported", 0, 1, false},
		}
	case AxisEfficiency:
		return []criterionDefinition{
			{"tokens_per_pr", "Current token burn / daily merge rate; approximate, windows differ", "tokens/merge", "budget elapsed window vs hive activity count window (fallback: 90-day org); at least 5 merges", "Lower token cost per delivered change", poorTokensPerPR, 0, true},
			{"agent_idle_ratio", "Idle / (idle + running) agent samples; stopped agents excluded", "ratio", "7-day hive state; at least one hour coverage and observed running/idle agents (older spokes: positive ratio only)", "No idle running agents", 1, 0, false},
			{"launch_cost", "Cost per agent launch", "tokens/launch", "Not on the heartbeat yet", "Not scored until reported", 0, 1, false},
		}
	case AxisSatisfaction:
		return []criterionDefinition{
			{"human_override_rate", "Swarm blocks overridden by humans", "ratio", "evidence, 90 days; at least 10 verdicts", "No unnecessary blocks", poorOutcomeRate, 0, false},
			{"sla_violations", "Work past its service threshold (operational proxy, not a survey)", "items", "current heartbeat SLA count", "No overdue work", poorSLACount, 0, false},
			{"human_feedback", "Direct human satisfaction feedback", "rating", "Not collected yet", "Not scored until reported", 0, 1, false},
		}
	}
	return nil
}

func criterionScore(d criterionDefinition, value float64) int {
	v, low, high := value, d.low, d.high
	if d.logarithmic {
		v, low, high = math.Log1p(math.Max(0, v)), math.Log1p(low), math.Log1p(high)
	}
	return clampScore(int(math.Round(100 * (v - low) / (high - low))))
}

func trustCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion
	if e := in.evidence; e != nil {
		if r, ok := evidenceRate(e.Reverted14d90d, e.Merged90d); ok {
			out = append(out, subCriterion{"revert_rate", r, "Inspect reverted or follow-up-fixed changes"})
		}
		if r, ok := evidenceRate(e.FalseApprove90d, e.Verdicts90d); ok {
			out = append(out, subCriterion{"false_approve_rate", r, "Inspect false approvals in the evidence view"})
		}
		if in.prsMerged90d != nil && *in.prsMerged90d > 0 {
			out = append(out, subCriterion{"evidence_coverage", math.Min(1, float64(e.Merged90d)/float64(*in.prsMerged90d)), "Check evidence recording coverage (org denominator is approximate)"})
		}
		attempted, blocked := 0, 0
		for _, c := range e.WriteCounters {
			attempted += c.Attempted
			blocked += c.Blocked
		}
		if r, ok := evidenceRate(blocked, attempted); ok {
			out = append(out, subCriterion{"boundary_blocked_rate", r, "Inspect out-of-bound write attempts"})
		}
	}
	return out
}

// mergeRate uses hive-specific audit counts when present, including measured
// zero. Org counts are only a fallback, never silently substituted for zero.
func mergeRate(in quadrantInputs) (daily float64, count int, ok bool) {
	if in.repoMerges14d != nil {
		days := in.repoMergeDays
		if days <= 0 {
			days = 14
		}
		return float64(*in.repoMerges14d) / days, *in.repoMerges14d, true
	}
	if in.prsMerged90d != nil {
		return float64(*in.prsMerged90d) / 90, *in.prsMerged90d, true
	}
	return 0, 0, false
}

func productivityCriteria(in quadrantInputs) []subCriterion {
	if daily, _, ok := mergeRate(in); ok && in.reposEnrolled > 0 {
		n := ""
		if daily == 0 {
			n = "No recorded merges — inspect the work queue"
			if in.budgetExhausted != nil && *in.budgetExhausted {
				n = "Budget exhausted — agent kicks are suppressed"
			}
		}
		return []subCriterion{{"merges_per_repo_week", daily * 7 / float64(in.reposEnrolled), n}}
	}
	return nil
}

func efficiencyCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion
	if daily, n, ok := mergeRate(in); ok && n >= minMergedPRsForCostRatio && daily > 0 && in.tokensPerDay != nil {
		out = append(out, subCriterion{"tokens_per_pr", *in.tokensPerDay / daily, "Inspect token spend per delivered change (approximate windows)"})
	}
	if in.agentIdleRatio != nil {
		out = append(out, subCriterion{"agent_idle_ratio", *in.agentIdleRatio, "Consider pausing idle agents"})
	}
	return out
}

func satisfactionCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion
	if e := in.evidence; e != nil {
		if r, ok := evidenceRate(e.FalseBlock90d, e.Verdicts90d); ok {
			out = append(out, subCriterion{"human_override_rate", r, "Inspect blocks overridden by humans"})
		}
	}
	if in.slaViolations != nil {
		out = append(out, subCriterion{"sla_violations", float64(*in.slaViolations), fmt.Sprintf("%d items past their SLA", *in.slaViolations)})
	}
	return out
}

func unscoredReason(axis string) string {
	switch axis {
	case AxisTrust:
		return "Trust evidence not reported or below the 10-outcome sample floor"
	case AxisProductivity:
		return "Merge activity or enrolled repo count not reported"
	case AxisEfficiency:
		return "Need token burn and at least 5 merges, or measured agent idle ratio"
	case AxisSatisfaction:
		return "Need at least 10 reviewed verdicts or a reported SLA count; direct feedback not measured yet"
	default:
		return "Not enough data"
	}
}

func criteriaFor(axis string, in quadrantInputs) []subCriterion {
	switch axis {
	case AxisTrust:
		return trustCriteria(in)
	case AxisProductivity:
		return productivityCriteria(in)
	case AxisEfficiency:
		return efficiencyCriteria(in)
	case AxisSatisfaction:
		return satisfactionCriteria(in)
	default:
		return nil
	}
}
