package hub

import "fmt"

// Per-axis scoring. Each axis is built from sub-criteria, and each sub-criterion
// yields both a comparable raw value AND the nudge it implies when weak. The
// nudge is not decoration: an axis that only reports a number tells an owner
// they are a 34, which is not actionable. Carrying the specific next action —
// "only 1 of 9 repos enrolled" — is the whole reason this instrument exists.
//
// Every sub-criterion may decline to report (ok=false) when its evidence is
// missing or below a floor. An axis with no reporting sub-criteria is unscored,
// NOT zero. See the sufficiency-floor rule in quadrant.go.

// subCriterion is one measured contributor to an axis.
//
// value is on whatever native scale the criterion measures; the axis converts
// it to a fleet percentile later. higherIsBetter records the direction, since
// cost-style criteria are better when smaller and a single comparator cannot
// guess that.
type subCriterion struct {
	name           string
	value          float64
	higherIsBetter bool
	nudge          string // shown when this criterion is the axis's weakest link
}

// --- Trust -------------------------------------------------------------------

// trustCriteria scores how much rope the humans have given this hive.
//
// Trust is deliberately about DELEGATION, not about outcomes: a hive that is
// trusted with more scope and a stricter governor posture scores higher even if
// its throughput is modest. Mixing outcome quality in here would make the axis
// a second productivity score.
//
// Note on merge acceptance: the obvious fourth criterion is the agent PR merge
// rate, but the platform cannot compute it today — see the acmmadvisor note in
// quadrant.go and the closed-unmerged gap in QUADRANT-DESIGN.md. It is omitted
// rather than approximated, so Trust scores on three criteria.
func trustCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion

	// Autonomy level. The single clearest expression of trust in the product:
	// a human raised this deliberately, and it gates what agents may do.
	if in.acmmLevel > 0 {
		out = append(out, subCriterion{
			name:           "autonomy",
			value:          float64(in.acmmLevel),
			higherIsBetter: true,
			nudge:          fmt.Sprintf("At ACMM L%d — consider trying the next level on one repo", in.acmmLevel),
		})
	}

	// Governor posture. observe -> advisory -> enforce is an increasing
	// willingness to let the governor act rather than merely watch.
	if rank, known := normalizeGovernorMode(in.governorMode); known {
		n := ""
		if rank == 0 {
			n = "Governor is observe-only — move it to advisory to let it act"
		}
		out = append(out, subCriterion{
			name:           "governor",
			value:          float64(rank),
			higherIsBetter: true,
			nudge:          n,
		})
	}

	// Scope breadth: what fraction of the org's repos are enrolled. Scored as a
	// ratio rather than a raw count so a 3-repo org enrolling everything is not
	// beaten by a 200-repo org enrolling five.
	//
	// Requires knowing the org total; when that is unknown the criterion
	// declines rather than assuming the enrolled count IS the total, which
	// would score every hive a perfect 1.0 and quietly flatten the axis.
	if in.reposInOrg > 0 && in.reposEnrolled >= 0 {
		ratio := float64(in.reposEnrolled) / float64(in.reposInOrg)
		n := ""
		if in.reposEnrolled < in.reposInOrg {
			n = fmt.Sprintf("Only %d of %d org repos enrolled", in.reposEnrolled, in.reposInOrg)
		}
		out = append(out, subCriterion{
			name:           "breadth",
			value:          ratio,
			higherIsBetter: true,
			nudge:          n,
		})
	}

	return out
}

// --- Efficiency --------------------------------------------------------------

// efficiencyCriteria scores the cost of an outcome.
//
// Cost ratios use the REPO-WIDE merged/closed counts on purpose: the numerator
// is the spoke's total spend, so the denominator must be the repo's total
// output for the ratio to mean anything. (Dividing total spend by only
// agent-authored merges would overstate cost per unit of work.) This is the
// opposite choice from Productivity, which must be agent-attributed — see the
// correctness note in QUADRANT-DESIGN.md.
func efficiencyCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion

	// Tokens per merged PR. Below the floor the ratio is dominated by fixed
	// setup spend and says more about the hive's age than its efficiency.
	//
	// The window mismatch is real and deliberate: tokensTotal is lifetime while
	// prsMerged90d covers 90 days, so a long-lived hive's ratio is inflated by
	// history it has already paid for. It is still the only cost signal the hub
	// receives, and it ranks hives usefully because every hive is distorted in
	// the same direction. The hover must present it as approximate.
	if in.tokensTotal != nil && in.prsMerged90d != nil && *in.prsMerged90d >= minMergedPRsForCostRatio {
		out = append(out, subCriterion{
			name:           "tokens_per_pr",
			value:          float64(*in.tokensTotal) / float64(*in.prsMerged90d),
			higherIsBetter: false,
			nudge:          "Token spend per merged PR is high relative to the fleet",
		})
	}

	// Rework: PRs the agent opened that were rejected rather than merged. This
	// is budget spent on work nobody wanted, which is the most actionable form
	// of inefficiency — it points at mission tuning rather than at throttling.
	//
	// Requires a meaningful denominator; with two or three total PRs the rate
	// swings wildly on a single rejection.
	if in.prsMerged90d != nil && in.prsRejected90d != nil {
		total := *in.prsMerged90d + *in.prsRejected90d
		if total >= minPRsForReworkRate {
			rate := float64(*in.prsRejected90d) / float64(total)
			n := ""
			if rate > 0.25 {
				n = fmt.Sprintf("%d of %d agent PRs were rejected — tune the mission",
					*in.prsRejected90d, total)
			}
			out = append(out, subCriterion{
				name:           "rework_rate",
				value:          rate,
				higherIsBetter: false,
				nudge:          n,
			})
		}
	}

	// Idle burn: agents that exist but are not shipping. Measured as merged PRs
	// per agent, so it is a productivity-per-seat figure rather than a raw
	// count — three agents producing what one could is the waste being nudged.
	//
	// Requires at least one agent; a hive with zero agents is not inefficient,
	// it is simply not running, which the Productivity axis already expresses.
	if in.agentCount > 0 && in.prsMerged90d != nil {
		perAgent := float64(*in.prsMerged90d) / float64(in.agentCount)
		n := ""
		if perAgent < 1 {
			n = fmt.Sprintf("%d agents but %d merged PRs — consider pausing idle agents",
				in.agentCount, *in.prsMerged90d)
		}
		out = append(out, subCriterion{
			name:           "output_per_agent",
			value:          perAgent,
			higherIsBetter: true,
			nudge:          n,
		})
	}

	return out
}

// --- Productivity ------------------------------------------------------------

// productivityCriteria scores throughput that is ACTUALLY ATTRIBUTABLE to the
// hive's agents.
//
// The counts here must come from the in-org, agent-authored search
// (SearchPRCount), never from the repo-wide PRIssueCounts and never from the
// outreach counter. Repo-wide counts would let a busy human repo with an idle
// hive score high; the outreach counter is -org: scoped and would omit
// essentially all real output. Both mistakes are easy to make and neither is
// visible in the resulting number.
func productivityCriteria(in quadrantInputs) []subCriterion {
	var out []subCriterion

	// Agent-merged PR throughput over the reporting window.
	if in.prsMerged90d != nil {
		n := ""
		if *in.prsMerged90d == 0 {
			n = "No agent-merged PRs in the last 90 days"
		}
		out = append(out, subCriterion{
			name:           "prs_merged",
			value:          float64(*in.prsMerged90d),
			higherIsBetter: true,
			nudge:          n,
		})
	}

	// Contributor engagement: how many of the hive's known contributors are
	// actually active. A hive with many enrolled but few active contributors is
	// under-using the relay, which is a different problem from having none.
	//
	// Scored as a ratio so a hive with three engaged contributors is not beaten
	// by one with thirty enrolled and three engaged.
	if in.contributorCount > 0 {
		ratio := float64(in.activeContributors) / float64(in.contributorCount)
		n := ""
		if in.activeContributors < in.contributorCount {
			n = fmt.Sprintf("%d of %d contributors active",
				in.activeContributors, in.contributorCount)
		}
		out = append(out, subCriterion{
			name:           "contributor_engagement",
			value:          ratio,
			higherIsBetter: true,
			nudge:          n,
		})
	}

	// Work-source autonomy: whether the hive finds its own work or waits to be
	// handed it. A hive reading only human-filed issues is capped at the rate
	// humans file them, which is the constraint this nudges at.
	if in.workSource != "" {
		selfDirected := 0.0
		n := "All work is human-filed — let the scanner find work"
		if in.workSource != "issues" && in.workSource != "" {
			selfDirected = 1.0
			n = ""
		}
		out = append(out, subCriterion{
			name:           "work_source",
			value:          selfDirected,
			higherIsBetter: true,
			nudge:          n,
		})
	}

	// Contributor-relay throughput. Zero is a real measurement (the relay is
	// configured but idle), so this reports whenever the count is present.
	if in.relayCompletedTasks != nil {
		n := ""
		if *in.relayCompletedTasks == 0 {
			n = "Contributor relay is idle"
		}
		out = append(out, subCriterion{
			name:           "relay",
			value:          float64(*in.relayCompletedTasks),
			higherIsBetter: true,
			nudge:          n,
		})
	}

	return out
}

// --- Satisfaction ------------------------------------------------------------

// satisfactionCriteria is intentionally empty.
//
// There is no satisfaction signal in the platform today. The available
// candidates — health checks, advisory depth, attention debt, contributor
// engagement — are OPERATIONAL HYGIENE, and hygiene is not satisfaction: a hive
// can be perfectly healthy and still be frustrating to work with, and a hive
// with a deep advisory backlog can have delighted users. Substituting hygiene
// here would produce a number that looks measured but is not, and it would
// undermine confidence in the three axes that are honestly derived.
//
// The axis therefore renders as a collapsed spoke with an explicit reason. That
// empty spoke is itself the finding: it says the platform does not yet measure
// how this feels to use. A real signal — e.g. a lightweight pulse on agent
// output — would populate it.
func satisfactionCriteria(_ quadrantInputs) []subCriterion { return nil }

// unscoredReason explains, per axis, what evidence is missing. Shown in the
// hover so a collapsed spoke reads as "not measured yet" rather than as a gap
// the viewer has to interpret.
func unscoredReason(axis string) string {
	switch axis {
	case AxisSatisfaction:
		return "No satisfaction signal is collected yet"
	case AxisEfficiency:
		return "Not enough merged output to judge cost efficiency yet"
	case AxisProductivity:
		return "No agent activity reported yet"
	case AxisTrust:
		return "Autonomy and governor posture not reported yet"
	default:
		return "Not enough data"
	}
}

// criteriaFor dispatches to the right sub-criteria builder for an axis.
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
