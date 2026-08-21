package hub

import (
	"math"
	"sort"
	"strings"
)

// The hive quadrant: a four-axis score — Trust, Efficiency, Satisfaction,
// Productivity — computed for every hive and for the fleet as a whole.
//
// It is a NUDGE INSTRUMENT, not a report card. Every sub-criterion is chosen so
// that a low score points at a specific next action ("move the governor off
// observe-only", "only 1 of 9 repos enrolled"), and the renderers surface those
// nudges alongside the number. A score nobody can act on does not belong here.
//
// Two design rules run through the whole file and must not be relaxed:
//
//  1. SCORES ARE FLEET-RELATIVE, BUT GATED BY ABSOLUTE ACTIVITY. An axis is a
//     percentile against the other hives in the same population, which keeps the
//     chart self-calibrating as the fleet's baseline moves. A percentile ALONE,
//     however, is blind to magnitude, and on an idle fleet that blindness is
//     catastrophic: the percentile definition puts a uniformly-tied population
//     at 50, so when 28 of 62 hives have merged nothing at all, all 28 score ~50
//     for doing literally nothing, and a hive with THREE merged PRs in ninety
//     days outranks all of them and lands near the top. The chart then reports a
//     busy, healthy fleet that does not exist.
//
//     Every percentile is therefore multiplied by an ACTIVITY FACTOR derived
//     from the criterion's raw magnitude (see activityFactor). Zero measured
//     activity scores at/near zero regardless of rank; activity ramps the
//     percentile in over a small absolute threshold so one PR does not vault a
//     hive to the top of the fleet but ten does count fully.
//
//  2. ABSENT EVIDENCE IS NOT A ZERO — AND IS NOT THE SAME AS MEASURED ZERO.
//     An axis with too little data reports Scored=false and renders as a
//     collapsed spoke. A hive that genuinely reported zero is a different thing:
//     it IS measured, it DOES score, and rule 1 means it scores LOW rather than
//     at the median. Collapsing these two cases together is the bug in both
//     directions — scoring a never-reporting hive at 0 nags the wrong people,
//     and scoring a measured-zero hive at 50 flatters the whole fleet.
//     Every axis is subject to the sufficiency floor, not just Satisfaction.
//
// Computed on read and never persisted on the registry entry, mirroring the
// Journey status this file sits alongside.

// Quadrant axis identifiers. These strings are the wire form (JSON keys, sort
// keys in the dashboard) and are part of the contract with the browser; do not
// rename them without updating sortedDashHives and the renderers.
const (
	AxisTrust        = "trust"
	AxisEfficiency   = "efficiency"
	AxisSatisfaction = "satisfaction"
	AxisProductivity = "productivity"
)

// QuadrantAxisOrder is the canonical axis order: Trust top, Productivity right,
// Satisfaction bottom, Efficiency left. The renderers depend on this being
// STABLE — the whole point of the small kite in the table is that an operator
// learns the shape once and can then read every row without a legend. Never
// reorder per-hive or per-view.
var QuadrantAxisOrder = []string{AxisTrust, AxisProductivity, AxisSatisfaction, AxisEfficiency}

// AxisScore is one axis of one hive's quadrant.
//
// Scored=false means "not enough evidence", NOT "zero". Renderers must draw an
// unscored axis as a collapsed spoke and must never fold it into an average.
// Reason carries the human explanation for why it is unscored, so the hover can
// say what is missing instead of showing a bare gap.
type AxisScore struct {
	Axis   string `json:"axis"`
	Score  int    `json:"score"`  // 0-100 percentile; meaningless unless Scored
	Scored bool   `json:"scored"` // false = insufficient data, render collapsed
	Reason string `json:"reason,omitempty"`

	// Delta is this hive's score minus the fleet median for the axis. It is
	// what turns the hover from a readout into a nudge — "Efficiency 34 · -21"
	// tells the owner where they sit, which is the thing that prompts action.
	Delta int `json:"delta"`

	// Nudge is the specific next action suggested by the weakest contributing
	// sub-criterion, or empty when the axis is healthy or unscored.
	Nudge string `json:"nudge,omitempty"`
}

// Quadrant is a hive's full four-axis score plus the composite.
type Quadrant struct {
	Axes []AxisScore `json:"axes"`

	// Composite is the mean of the SCORED axes only. Unscored axes are
	// excluded rather than counted as zero, so a hive is never punished in the
	// composite for a signal the platform does not collect yet.
	Composite int `json:"composite"`

	// ScoredAxes is how many of the four axes carried enough evidence. It is
	// surfaced so a composite built from one axis is never mistaken for one
	// built from four.
	ScoredAxes int `json:"scored_axes"`
}

// quadrantInputs is the per-hive evidence the scorer consumes. It is a distinct
// boundary type — deliberately NOT the registry entry — so the scoring rules
// stay pure and table-testable, and so a caller must think about where each
// signal comes from rather than reaching into whatever happens to be on the
// entry.
//
// Pointer fields mean "not reported". A nil is the ONLY way to express absent
// evidence; a zero value is a real measurement of zero. Conflating the two is
// precisely the bug the sufficiency floor exists to prevent.
type quadrantInputs struct {
	// countGroup identifies hives whose org-scoped counters measure THE SAME
	// underlying work. It is the (org, ai-author) pair the spoke's
	// FleetStatsCollector queries GitHub with: that collector counts AI-author
	// PRs across the whole org, so every hive enrolled in an org reports that
	// org's ENTIRE output as its own.
	//
	// Three hives in the live fleet each reported prsMerged90d=3746 for this
	// reason — one org's output counted three times. Left unhandled it does not
	// merely give those three hives the same score, it stretches the whole
	// distribution: the population sample gains two phantom members at the top,
	// pushing every other hive's percentile down and making the fleet's real
	// spread unreadable.
	//
	// Population building therefore counts a group ONCE (see dedupeByGroup).
	// Empty means "ungrouped" — the hive stands alone and always counts once,
	// which is the correct default for a hive whose org is unknown.
	countGroup string

	// --- Trust -----------------------------------------------------------
	acmmLevel     int // 1-6; 0 = unknown
	reposEnrolled int
	reposInOrg    int // 0 = unknown, so breadth is unscorable

	// mergeAcceptance is merged / (merged + rejected) over the reported
	// window, computed hub-side from counts that already travel.
	//
	// Weighted as one criterion among several and never alone: the metric is
	// Goodhart-gameable through PR size, and it is censored by construction
	// because it counts only PRs that reached a verdict — it stayed green
	// through a fleet-wide outage in which almost nothing was submitted.
	mergeAcceptance *float64

	// --- Efficiency ------------------------------------------------------
	// tokensPerDay is the hive's CURRENT burn rate, derived from the governor's
	// windowed budget spend normalised by elapsed window time.
	//
	// Normalising matters twice over: budget windows are per-spoke
	// configuration (default 7 days but not fixed), and any given beat lands at
	// an arbitrary point inside one. Comparing raw spend across hives would
	// rank window length and beat timing rather than efficiency.
	//
	// Tokens rather than dollars is deliberate — the USD figures never leave
	// the spoke, and tokens are provider-price-independent, so a hive's score
	// does not move when a model is repriced or when two hives run different
	// backends.
	tokensPerDay *float64

	// budgetExhausted reports the governor actively suppressing agent kicks.
	// It bears on both efficiency and productivity: near-zero throughput means
	// something very different when a hive is throttled than when it is idle.
	budgetExhausted *bool

	// prsMerged90d / prsRejected90d are the spoke's AI-author PR counts across
	// its org, already travelling on the heartbeat via FleetStatsCollector.
	// Nil means "not computed yet", which the existing hub code is careful to
	// distinguish from zero — the same discipline this struct uses throughout.
	//
	// NOTE the window mismatch: tokensTotal is lifetime while these are 90-day.
	// The resulting ratio is therefore approximate, and the hover must say so
	// rather than presenting it as an exact cost-per-PR.
	prsMerged90d   *int
	prsRejected90d *int
	agentCount     int

	// --- Productivity ----------------------------------------------------
	// Throughput reuses prsMerged90d above: it is org-wide and attributed to
	// the AI author, which is exactly the attribution this axis requires. The
	// repo-wide github.PRIssueCounts is NOT available hub-side (it never leaves
	// the spoke), and the outreach counter is -org: scoped, so neither of the
	// two tempting alternatives is usable here even if they were reachable.
	workSource         string
	contributorCount   int
	activeContributors int

	// tasksCompleted7d is contributor-relay throughput, summed on the spoke
	// from its hourly buckets. A flat zero is a real measurement for a hive
	// with no contributors rather than a gap — read alongside contributorCount
	// to tell the two apart.
	tasksCompleted7d *int

	// holdTotal and awaitingReview both measure work stalled on a HUMAN rather
	// than on the agents, from opposite ends: items parked behind a hold label,
	// and plans blocked on a decision. slaViolations is work aging past its
	// threshold whatever the cause.
	//
	// These are inverse signals — more is worse — which the criteria below
	// express through higherIsBetter rather than by negating the values, so the
	// raw numbers stay readable in a debugger.
	holdTotal      *int
	awaitingReview *int
	slaViolations  *int

	// --- Satisfaction ----------------------------------------------------
	// Intentionally empty. There is no satisfaction signal in the platform
	// today. Operational hygiene (health checks, advisory depth, attention
	// debt) is deliberately NOT substituted here: hygiene is not satisfaction,
	// and padding the axis with a proxy would make every other axis less
	// trustworthy by association. The axis renders collapsed until a real
	// signal — e.g. a lightweight pulse on agent output — exists.
}

// Sufficiency floors. Below these a sub-criterion contributes nothing and, if
// an axis has no contributing sub-criteria at all, the axis is unscored.
//
// These are named rather than inlined because they encode a judgement call —
// "how much output before a cost-per-PR figure means anything" — that a future
// reader will want to find and argue with.
const (
	// minMergedPRsForCostRatio is how many merged PRs a hive needs before
	// tokens-per-PR is meaningful. With one or two merges the ratio is
	// dominated by fixed setup spend and says more about when the hive was
	// created than how efficiently it runs.
	minMergedPRsForCostRatio = 5

	// minPRsForReworkRate is the smallest merged+rejected total that gives a
	// stable rejection rate. Below it a single rejection swings the figure by
	// tens of percent, which would flag hives for noise.
	minPRsForReworkRate = 4

	// minFleetForPercentile is the smallest population in which a percentile
	// is honest. Ranking three hives against each other produces scores of 0,
	// 50 and 100 that look authoritative and mean almost nothing.
	minFleetForPercentile = 5
)

// activityFloor is the absolute magnitude at which a criterion's percentile
// counts in full.
//
// It exists because a percentile cannot see magnitude. Below this value the
// percentile is scaled down toward zero; at or above it the fleet-relative rank
// stands unmodified. The scale is the criterion's own native unit, which is why
// each criterion carries its own floor rather than sharing one constant — five
// merged PRs and five contributor-relay tasks are not the same amount of
// evidence.
//
// activityFloor==0 means the criterion is EXEMPT: its value is a ratio,
// posture or inverse count where "zero" is not "idle". A rework rate of zero is
// the BEST possible outcome, not an absence of activity, and gating it would
// score the cleanest hives worst. The exemption is opt-in per criterion for
// exactly this reason — a blanket gate would silently invert those axes.
const (
	// activityFloorMergedPRs is the merged-PR count at which throughput
	// percentile counts in full. Ten in ninety days is roughly one a week: a
	// hive that is genuinely, continuously working rather than one that landed
	// a couple of things. Below it the ramp means three merged PRs earns about
	// a third of its rank, which is the intended answer to "a hive with 3
	// merged PRs must not outrank 28 idle ones and land high".
	activityFloorMergedPRs = 10

	// activityFloorRelayTasks is the 7-day contributor-relay count at which the
	// relay reads as genuinely in use. Set at five — under one a day — because
	// the relay is a lighter-weight action than a merged PR and a lower bar
	// still distinguishes "in use" from "configured and idle".
	activityFloorRelayTasks = 5

	// activityFloorOutputPerAgent is merged PRs per agent at which a seat reads
	// as productive. One over ninety days is a deliberately low bar — the
	// criterion flags agents that shipped NOTHING, and anything above zero at
	// least demonstrates the seat works.
	activityFloorOutputPerAgent = 1

	// activityFloorTokensPerDay is the daily burn at which a hive is doing
	// enough work for its cost figures to describe anything. A hive burning
	// almost nothing is not efficient, it is idle — and without this it would
	// take the top of the lower-is-better cost ranking precisely BECAUSE it
	// does nothing, which is the same median-for-idleness bug wearing a
	// different sign. 50k tokens/day is a few agent runs.
	activityFloorTokensPerDay = 50000
)

// activityFactor ramps a criterion's absolute magnitude into a 0..1 multiplier
// on its fleet percentile.
//
// The ramp is LINEAR from 0 at zero activity to 1 at the floor, then flat. A
// linear ramp is deliberate over a step: a step at the floor would create a
// cliff where the tenth merged PR is worth more than the previous nine put
// together, which is both unfair and easy to game. A linear ramp makes each
// unit of real work worth the same amount until the criterion is satisfied.
//
// The multiplicative form — score = percentile × factor — is what preserves the
// fleet-relative property while fixing its blindness. Rank still decides the
// ORDER among active hives; magnitude decides whether the hive is in the
// conversation at all. An additive or replacement scheme would throw away the
// self-calibration that makes the fleet-average polygon meaningful.
//
// floor<=0 means the criterion is exempt (see the constants above) and the
// percentile passes through untouched.
func activityFactor(value, floor float64) float64 {
	if floor <= 0 {
		return 1
	}
	if value <= 0 {
		// Measured zero. NOT unscored — the hive reported, so it scores, and it
		// scores at the bottom rather than at the fleet median. This single
		// line is the difference between "28 idle hives at 50" and the truth.
		return 0
	}
	if value >= floor {
		return 1
	}
	return value / floor
}

// dedupeByGroup collapses values that share a countGroup down to one
// representative, so an org-wide counter reported by several hives in the same
// org contributes to a criterion's population ONCE.
//
// The representative is the MAXIMUM of the group's reported values rather than
// the first or the mean. The counter is org-wide and identical by construction,
// so in the healthy case every choice agrees; they differ only when the hives'
// collectors ran at different times or one is stale, and the freshest count is
// the largest over a fixed trailing window. Taking the max therefore keeps the
// most complete measurement instead of letting collection timing decide.
//
// Hives with an empty countGroup are ungrouped and each contribute their own
// value, which is the right default when the org is unknown.
//
// Note this dedupes the POPULATION only. Each hive is still SCORED against that
// population with its own value, so co-org hives still get identical scores —
// correctly, since they are reporting the same underlying work. What is fixed
// is the distribution no longer being stretched by phantom duplicates.
func dedupeByGroup(values []float64, groups []string) []float64 {
	out := make([]float64, 0, len(values))
	best := map[string]int{} // countGroup -> index into out
	for i, v := range values {
		g := ""
		if i < len(groups) {
			g = strings.TrimSpace(groups[i])
		}
		if g == "" {
			out = append(out, v)
			continue
		}
		if j, seen := best[g]; seen {
			if v > out[j] {
				out[j] = v
			}
			continue
		}
		best[g] = len(out)
		out = append(out, v)
	}
	return out
}

// percentileRank returns where value sits within population, as 0-100.
//
// It uses the standard "fraction of the population strictly below, plus half
// those equal" definition, which keeps ties at the same score and puts a
// uniformly-tied population at 50 rather than at 0 or 100 depending on
// comparison direction.
//
// higherIsBetter=false inverts the sense, for metrics like cost-per-PR where a
// smaller number is the good outcome.
func percentileRank(value float64, population []float64, higherIsBetter bool) int {
	if len(population) < minFleetForPercentile {
		return 0
	}
	below, equal := 0, 0
	for _, p := range population {
		switch {
		case p == value:
			equal++
		case (p < value) == higherIsBetter:
			below++
		}
	}
	pct := (float64(below) + 0.5*float64(equal)) / float64(len(population)) * 100
	return clampScore(int(math.Round(pct)))
}

// clampScore keeps a score inside 0-100 regardless of arithmetic drift.
func clampScore(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// medianOf returns the median of a sample, or 0 for an empty one. Used for the
// per-axis Delta, which is what makes the hover actionable.
func medianOf(sample []int) int {
	if len(sample) == 0 {
		return 0
	}
	s := append([]int(nil), sample...)
	sort.Ints(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
