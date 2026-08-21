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
//  1. SCORES ARE FLEET-RELATIVE. An axis is a percentile against the other
//     hives in the same population, not an absolute grade. This keeps the chart
//     self-calibrating as the fleet's baseline moves, and it is what makes the
//     fleet-average polygon a meaningful reference rather than a fixed target.
//
//  2. ABSENT EVIDENCE IS NOT A ZERO. An axis with too little data reports
//     Scored=false and renders as a collapsed spoke, visually distinct from a
//     genuine low score. Scoring a brand-new hive's efficiency at 0 because it
//     has not merged anything yet would be a lie that nags the wrong people.
//     Every axis is subject to this floor, not just Satisfaction.
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
	// --- Trust -----------------------------------------------------------
	acmmLevel     int    // 1-6; 0 = unknown
	governorMode  string // observe | advisory | enforce (case-insensitive)
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

// normalizeGovernorMode folds the governor posture to a comparable rank.
// Higher = more trust extended to the hive. Unknown modes rank lowest rather
// than erroring, since the governor gains modes over time and an unrecognised
// value must never crash the dashboard.
func normalizeGovernorMode(mode string) (rank int, known bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "observe", "observe-only", "off":
		return 0, true
	case "advisory", "advise":
		return 1, true
	case "enforce", "enforcing", "on":
		return 2, true
	default:
		return 0, false
	}
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
