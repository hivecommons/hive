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
//
// activityFloor is the absolute magnitude at which this criterion's percentile
// counts in full — see activityFactor in quadrant.go. Leaving it zero EXEMPTS
// the criterion from the absolute gate, which is correct for ratios and
// postures (where zero is a real, often good, value) and wrong for throughput
// counters (where zero means idle). Every criterion must make this choice
// deliberately; the zero default is safe only because an un-gated criterion
// behaves exactly as it did before the gate existed.
type subCriterion struct {
	name           string
	value          float64
	higherIsBetter bool
	activityFloor  float64
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
// THE GOVERNOR CRITERION HAS BEEN REMOVED, and Trust now scores on three
// criteria: autonomy, merge acceptance and breadth.
//
// It was built on a misreading of the governor, in two compounding ways.
//
// Mechanically it never worked: it matched "observe"/"advisory"/"enforce", but
// governor.Mode (pkg/governor/governor.go) only ever emits SURGE, BUSY, QUIET
// or IDLE. Every hive in the fleet fell through to known=false, so the
// criterion contributed to nothing — one of only three Trust criteria dropped
// silently, with the axis still rendering a perfectly plausible number.
//
// Conceptually it was worse, and this is why it is deleted rather than
// remapped: those values are WORKLOAD CADENCE — how hard the governor is
// currently driving agents — not a trust posture. There is no
// observe/advisory/enforce dimension anywhere in the governor. A hive at SURGE
// is busy, not trusted; a hive at IDLE is quiet, not distrusted. Mapping
// cadence onto a trust rank would take a criterion that measured nothing and
// replace it with one that measures the wrong thing, which is strictly worse
// because it would then look like it was working.
//
// No replacement is substituted, because the hub does not receive one. The
// codebase has three genuine delegated-authority signals and ALL of them are
// spoke-side configuration that never travels on the heartbeat:
//
//   - config.AutoMergeConfig.SelfAuthored (pkg/config/config.go) — the App
//     merging its own PRs with no human approval. This is the strongest
//     delegation bit in the product and would be the ideal trust criterion.
//   - config.ReviewConfig.RequireApproval — the review gate.
//   - config.AgentSandboxConfig / AgentConfig.Tools — sandbox and tool
//     permissions.
//
// None appears on HeartbeatPayload or RegistryEntry; each is read only inside
// cmd/hive/main.go. The hub therefore cannot see whether an operator granted or
// revoked any of them, and acmmLevel — already the autonomy criterion below —
// is the single delegation signal that does arrive.
//
// Recomputing a gate hub-side from acmmLevel (PlanAutoApproveForLevel,
// SelfMergeMinACMMLevel) was considered and rejected: it is a pure function of
// a value this axis ALREADY scores, so it would add no information while
// double-weighting autonomy and looking like a second, independent criterion.
// Inventing a proxy to keep the criterion count at three would repeat exactly
// the mistake being fixed here.
//
// The real fix is to report the signal. Adding
// AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel) to the heartbeat as a
// *bool — nil for old spokes, matching every other quadrant pointer field —
// would give Trust a true delegation criterion. Until then, three.
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

	// Merge acceptance: the share of agent PRs that reached a merge rather than
	// a rejection. This is the humans' verdict on the agents' work, which is
	// what makes it a trust signal rather than a throughput one.
	//
	// Deliberately one criterion among three, never decisive on its own — see
	// the weaknesses documented on quadrantInputs.mergeAcceptance.
	if in.mergeAcceptance != nil {
		n := ""
		if *in.mergeAcceptance < 0.7 {
			n = fmt.Sprintf("%.0f%% of agent PRs are merged — tune the mission",
				*in.mergeAcceptance*100)
		}
		out = append(out, subCriterion{
			name:           "merge_acceptance",
			value:          *in.mergeAcceptance,
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

	// Burn rate: tokens per day in the current budget window. Both sides are
	// now properly scoped — a rate rather than a lifetime total — so this is a
	// genuine comparison rather than the approximate ranking an unnormalised
	// cumulative figure would give.
	//
	// Gated on absolute burn. Without the gate this criterion rewards idleness
	// outright: it is lower-is-better, so a hive spending nothing takes the top
	// of the ranking for having done nothing at all. The floor makes a
	// barely-burning hive score near zero here, and only hives doing real work
	// compete on how cheaply they do it.
	if in.tokensPerDay != nil {
		out = append(out, subCriterion{
			name:           "tokens_per_day",
			value:          *in.tokensPerDay,
			higherIsBetter: false,
			activityFloor:  activityFloorTokensPerDay,
			nudge:          "Token burn rate is high relative to the fleet",
		})
	}

	// Burn per unit of output. The rate above says how fast budget leaves; this
	// says whether anything came back for it, which is the difference between
	// an expensive hive and a wasteful one.
	if in.tokensPerDay != nil && in.prsMerged90d != nil && *in.prsMerged90d >= minMergedPRsForCostRatio {
		out = append(out, subCriterion{
			name:           "tokens_per_pr",
			value:          *in.tokensPerDay / float64(*in.prsMerged90d),
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
		// Gated at one merged PR per agent: an agent that has shipped nothing
		// in ninety days is the definition of idle burn, and this criterion
		// exists to flag exactly that. Without the gate a fleet where most
		// agents ship nothing puts them all at the median for it.
		out = append(out, subCriterion{
			name:           "output_per_agent",
			value:          perAgent,
			higherIsBetter: true,
			activityFloor:  activityFloorOutputPerAgent,
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
			// A throttled hive is not underperforming, it is out of budget.
			// Telling its owner to ship more would be both wrong and useless;
			// the actionable fact is the budget, so say that instead.
			if in.budgetExhausted != nil && *in.budgetExhausted {
				n = "Budget exhausted — agent kicks are suppressed"
			} else {
				n = "No agent-merged PRs in the last 90 days"
			}
		}
		// The headline gate. A measured zero here scores ZERO, not the fleet
		// median — this is the criterion where percentile-blindness did the most
		// damage, since most of the fleet has merged nothing and ties place a
		// uniformly-idle population at 50.
		out = append(out, subCriterion{
			name:           "prs_merged",
			value:          float64(*in.prsMerged90d),
			higherIsBetter: true,
			activityFloor:  activityFloorMergedPRs,
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

	// Contributor-relay throughput over the last seven days. Zero is a real
	// measurement (the relay is configured but idle), so this reports whenever
	// the count is present — but only nudge a hive that actually HAS
	// contributors, since telling a solo hive its relay is idle is noise.
	if in.tasksCompleted7d != nil {
		n := ""
		if *in.tasksCompleted7d == 0 && in.contributorCount > 0 {
			n = "Contributor relay is idle"
		}
		// Gated: an idle relay is idle whatever the rest of the fleet is doing.
		out = append(out, subCriterion{
			name:           "relay_7d",
			value:          float64(*in.tasksCompleted7d),
			higherIsBetter: true,
			activityFloor:  activityFloorRelayTasks,
			nudge:          n,
		})
	}

	// Human bottleneck: work parked behind a hold label or a pending plan
	// decision. Both measure throughput lost to waiting on a person, which is
	// the most addressable kind — the nudge points at a queue somebody can
	// clear today rather than at a system to retune.
	//
	// Combined into one criterion because they are the same phenomenon seen
	// from two ends, and scoring them separately would double their weight
	// against the throughput criteria.
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
		out = append(out, subCriterion{
			name:           "human_blocked",
			value:          float64(blocked),
			higherIsBetter: false,
			nudge:          n,
		})
	}

	// Work aging past its service threshold, whatever the cause.
	if in.slaViolations != nil {
		n := ""
		if *in.slaViolations > 0 {
			n = fmt.Sprintf("%d items past their SLA", *in.slaViolations)
		}
		out = append(out, subCriterion{
			name:           "sla_violations",
			value:          float64(*in.slaViolations),
			higherIsBetter: false,
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
