package hub

import (
	"strings"
	"time"
)

// Bridging the registry to the scorer: turning what the hub stores into the
// evidence the quadrant scores, and attaching results to a dashboard payload.
//
// Everything here is derived on read and never persisted, mirroring how the
// user-journey status is attached alongside it. The difference — and the reason
// the quadrant is attached to the row wrapper rather than the registry entry —
// is that a quadrant is fleet-relative: it is a property of a hive WITHIN a
// population, not of the hive alone.

// budgetSpendPerDay normalises a hive's windowed token spend to a daily rate.
//
// The raw figure cannot be compared across hives: the budget window length is
// each spoke's own governor.budget.period_days, so a hive on a 30-day window
// would look thirty times hungrier than an identical hive on a daily one. It is
// also mid-window at an arbitrary point, so a hive beat two hours into its
// window has barely spent anything yet through no virtue of its own.
//
// Normalising by ELAPSED time rather than by window length handles both: it
// answers "how fast is this hive burning right now", which is the comparable
// quantity.
//
// Returns ok=false whenever the answer would be noise rather than a measurement
// — no spend reported, no window bounds to scope it, or so little elapsed time
// that the rate is dominated by rounding.
func budgetSpendPerDay(e RegistryEntry, now time.Time) (float64, bool) {
	if e.BudgetCurrentSpend == nil || e.BudgetWindowStartsAt.IsZero() {
		return 0, false
	}
	elapsed := now.Sub(e.BudgetWindowStartsAt)
	// A window that has not yet accumulated a meaningful span gives a rate
	// dominated by whatever happened in the first minutes. A clock skew
	// between spoke and hub can also put the start in the future.
	if elapsed < minBudgetWindowElapsed {
		return 0, false
	}
	days := elapsed.Hours() / 24
	if days <= 0 {
		return 0, false
	}
	return float64(*e.BudgetCurrentSpend) / days, true
}

// minBudgetWindowElapsed is how much of a budget window must have passed before
// its spend rate means anything. Six hours is long enough that a single burst
// of agent activity no longer dominates the average.
const minBudgetWindowElapsed = 6 * time.Hour

// mergeAcceptanceRate is merged / (merged + rejected) over the reported window.
//
// This restores the trust sub-criterion the platform appeared not to support:
// the counts already travel on the heartbeat, so no new instrumentation is
// needed to compute it hub-side.
//
// It is weighted as ONE of trust's criteria and never allowed to stand alone,
// because the underlying metric has two documented weaknesses. It is
// Goodhart-gameable through PR size — a hive whose agents learn to submit
// trivially small changes scores well without being more trustworthy — and it
// is censored by construction: it counts only PRs that reached a verdict, so it
// stayed green throughout a fleet-wide outage during which almost nothing was
// being submitted at all.
func mergeAcceptanceRate(e RegistryEntry) (float64, bool) {
	if e.PRsMerged90d == nil || e.PRsRejected90d == nil {
		return 0, false
	}
	total := *e.PRsMerged90d + *e.PRsRejected90d
	if total < minPRsForReworkRate {
		return 0, false
	}
	return float64(*e.PRsMerged90d) / float64(total), true
}

// fleetCountGroup identifies hives whose org-scoped counters measure the SAME
// underlying work, so a criterion's population can count that work once.
//
// The key is (org, ai-author) because that is exactly the query the spoke's
// FleetStatsCollector issues — NewFleetStatsCollector(ghClient, author, org)
// counts AI-author PRs across the whole org. Two hives agreeing on both fields
// are therefore not two measurements of similar size, they are one measurement
// reported twice. Three hives in the live fleet each reported prsMerged90d=3746
// for precisely this reason.
//
// Org alone would be too coarse — two teams in one org running different AI
// authors do produce genuinely distinct output and must each count. Including
// the author keeps those separate while still collapsing true duplicates.
//
// Returns "" when either half is unknown, which means "ungrouped": the hive
// counts once on its own, the safe default because it cannot be shown to
// duplicate anyone. Case is folded since GitHub logins and orgs are
// case-insensitive and spokes report them as configured.
func fleetCountGroup(e RegistryEntry) string {
	org := strings.ToLower(strings.TrimSpace(e.Org))
	// AIAuthorEffective is who the agents ACTUALLY author as (the App bot login
	// when ai_author is unset), which is the identity the collector's search
	// resolves to. Prefer it, and fall back to the configured author.
	author := strings.ToLower(strings.TrimSpace(e.AIAuthorEffective))
	if author == "" {
		author = strings.ToLower(strings.TrimSpace(e.AIAuthor))
	}
	if org == "" || author == "" {
		return ""
	}
	return org + "/" + author
}

// quadrantInputsFor extracts the evidence for one hive.
//
// The nil-vs-zero discipline the registry maintains is carried through here
// unchanged: a nil pointer on the entry stays nil in the inputs, so the scorer
// sees "not reported" rather than a measurement of zero. Dereferencing into a
// plain value here — the obvious simplification — would silently convert every
// unreported signal into a genuine low score.
func quadrantInputsFor(e RegistryEntry, now time.Time) quadrantInputs {
	in := quadrantInputs{
		acmmLevel:  e.ACMMLevel,
		agentCount: e.AgentCount,
		// The org-wide counters are collected per (org, ai-author) pair — see
		// NewFleetStatsCollector — so that pair, not the hive ID, is what makes
		// two hives' counts the same measurement rather than two of them.
		countGroup: fleetCountGroup(e),
		// WorkSource is empty for the DEFAULT GitHub Issues source rather than
		// unknown, so an empty string is a real answer here: the hive is
		// reading human-filed issues.
		workSource:         e.WorkSource,
		contributorCount:   e.ContributorCount,
		activeContributors: e.ActiveContributors,
		reposEnrolled:      len(e.Repos),
		prsMerged90d:       e.PRsMerged90d,
		prsRejected90d:     e.PRsRejected90d,
		holdTotal:          e.HoldTotal,
		awaitingReview:     e.AwaitingReview,
		slaViolations:      e.SLAViolations,
		tasksCompleted7d:   e.TasksCompleted7d,
		budgetExhausted:    e.BudgetExhausted,
	}
	if rate, ok := budgetSpendPerDay(e, now); ok {
		in.tokensPerDay = &rate
	}
	if rate, ok := mergeAcceptanceRate(e); ok {
		in.mergeAcceptance = &rate
	}
	return in
}

// canSeeQuadrant reports whether a caller may see a hive's quadrant.
//
// Scores rank hives against one another, so showing one to an unrelated user
// would disclose how somebody else's project is performing. The rule is
// therefore the same one the row's other owner-scoped affordances use: a role
// on the hive, or hub admin.
//
// Note this gates only the per-hive score. The fleet AVERAGE is an aggregate
// over many hives and identifies none of them, so it is not gated here.
func canSeeQuadrant(role string, isAdmin bool) bool {
	return isAdmin || strings.TrimSpace(role) != ""
}

// attachQuadrants scores a dashboard payload in place.
//
// The population is exactly the rows passed in — the caller's filtered view —
// which is what keeps a row's score and the header's aggregate consistent with
// each other. Ranking against the whole registry instead would let the header
// polygon disagree with the rows it summarises, which is worse than either
// being imprecise.
//
// Returns the fleet average for the same population so the caller can render
// the reference polygon and the header without scoring twice.
func attachQuadrants(rows []MyHiveEntry, isAdmin bool, now time.Time) Quadrant {
	if len(rows) == 0 {
		return Quadrant{}
	}

	inputs := make([]quadrantInputs, len(rows))
	for i := range rows {
		inputs[i] = quadrantInputsFor(rows[i].RegistryEntry, now)
	}

	scores := ScoreFleet(inputs)

	// The average is computed over EVERY row, including ones this caller may
	// not see individually. That is deliberate: the reference polygon should
	// describe the fleet the hive is actually being ranked against, and it
	// discloses nothing about any single hive.
	avg := FleetAverage(scores)

	for i := range rows {
		if !canSeeQuadrant(rows[i].Role, isAdmin) {
			continue
		}
		// Skip rows that scored nothing at all rather than attaching an empty
		// quadrant: the browser renders no chart for a nil, but would draw a
		// fully-collapsed kite for a present-but-empty one, which reads as a
		// hive scoring zero everywhere instead of one with no data yet.
		if scores[i].ScoredAxes == 0 {
			continue
		}
		q := scores[i]
		rows[i].Quadrant = &q
	}

	return avg
}
