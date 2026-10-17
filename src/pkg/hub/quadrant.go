package hub

import (
	"sort"
)

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

// QuadrantAxisLabels are the plain-language axis names used in server-side
// summaries. The browser keeps its own short labels for the glyph.
var QuadrantAxisLabels = map[string]string{
	AxisTrust:        "Trust",
	AxisProductivity: "Productivity",
	AxisSatisfaction: "Satisfaction",
	AxisEfficiency:   "Efficiency",
}

// QuadrantFleetAggregate names how the fleet quadrant collapses per-hive
// scores. It travels on the wire so the card can say "median of n" rather than
// leave the reader to guess whether a few very active hives pulled a mean up.
const QuadrantFleetAggregate = "median"

// CriterionScore is one sub-criterion of one axis as shown to the reader.
//
// Measured=false means the hive (or, on the fleet quadrant, no hive) reported
// the input this criterion needs. An unmeasured criterion is listed — so the
// explainer can say "not measured yet" and what would feed it — but it is
// EXCLUDED from the axis mean, never scored as zero.
//
// On the fleet quadrant Value/Display/Score are medians over the hives that
// measured the criterion and N is how many did.
type CriterionScore struct {
	Name     string  `json:"name"`
	Label    string  `json:"label"`
	Measured bool    `json:"measured"`
	Value    float64 `json:"value"`
	Display  string  `json:"display,omitempty"`
	Score    int     `json:"score"`
	Weight   float64 `json:"weight"`
	How      string  `json:"how"`
	Good     string  `json:"good"`
	Reason   string  `json:"reason,omitempty"`
	N        int     `json:"n,omitempty"`
}

// AxisScore is one axis of one hive's quadrant.
//
// Scored=false means "not enough evidence", NOT "zero". Renderers must draw an
// unscored axis as a collapsed spoke and must never fold it into an average.
// Reason carries the human explanation for why it is unscored, so the hover can
// say what is missing instead of showing a bare gap.
type AxisScore struct {
	Axis   string `json:"axis"`
	Score  int    `json:"score"`  // 0-100; meaningless unless Scored
	Scored bool   `json:"scored"` // false = insufficient data, render collapsed
	Reason string `json:"reason,omitempty"`

	// Delta is this hive's score minus the fleet median for the axis. It is
	// what turns the hover from a readout into a nudge — "Efficiency 34 · -21"
	// tells the owner where they sit, which is the thing that prompts action.
	Delta int `json:"delta"`

	// Nudge is the specific next action suggested by the weakest contributing
	// sub-criterion, or empty when the axis is healthy or unscored.
	Nudge string `json:"nudge,omitempty"`

	// Criteria lists every sub-criterion the axis is defined by, measured or
	// not, in a stable order. It is what the ⓘ explainer renders.
	Criteria []CriterionScore `json:"criteria,omitempty"`

	// Measured / Unmeasured count the criteria above, so a score built from
	// one criterion is never mistaken for one built from six.
	Measured   int `json:"measured"`
	Unmeasured int `json:"unmeasured"`

	// Summary is the one-line plain reading of the axis ("Trust 48: …").
	Summary string `json:"summary,omitempty"`

	// N is, on the fleet quadrant only, how many hives scored this axis.
	N int `json:"n,omitempty"`
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

	// Aggregate and N are set on the fleet quadrant only: how the per-hive
	// scores were collapsed ("median") and over how many hives.
	Aggregate string `json:"aggregate,omitempty"`
	N         int    `json:"n,omitempty"`
}

// quadrantInputs is the per-hive evidence the scorer consumes. It is a distinct
// boundary type — deliberately NOT the registry entry — so the scoring rules
// stay pure and table-testable, and so a caller must think about where each
// signal comes from rather than reaching into whatever happens to be on the
// entry.
//
// Pointer fields mean "not reported". A nil is the ONLY way to express absent
// evidence; a zero value is a real measurement of zero. Conflating the two is
// precisely the bug the measured/unmeasured split exists to prevent.
type quadrantInputs struct {
	// --- Trust -----------------------------------------------------------
	acmmLevel     int // 1-6; 0 = unknown
	reposEnrolled int
	reposInOrg    int // 0 = unknown, so breadth is unmeasured

	// mergeAcceptance is merged / (merged + rejected) over the reported
	// window, computed hub-side from counts that already travel. One criterion
	// among several and never alone: it is Goodhart-gameable through PR size
	// and censored by construction (it counts only PRs that reached a verdict).
	mergeAcceptance *float64

	// selfMergeAllowed is whether the hive's ACMM level lets the App merge its
	// own PRs without a human approval. nil = not reported.
	selfMergeAllowed *bool

	// evidence is the spoke's evidence summary (reviewer verdict outcomes,
	// reverts, proxy write counters, hive-state trends); nil = not reported.
	evidence *HeartbeatEvidence

	// --- Efficiency ------------------------------------------------------
	// tokensPerDay is the hive's CURRENT burn rate: the governor's windowed
	// budget spend normalised by elapsed window time, or the 7-day hive-state
	// trend when the budget window is not reported. Tokens rather than dollars
	// because USD never leaves the spoke and tokens are provider-price
	// independent.
	tokensPerDay *float64

	// budgetExhausted reports the governor actively suppressing agent kicks.
	budgetExhausted *bool

	// prsMerged90d / prsRejected90d are the spoke's AI-author PR counts across
	// its ORG, from FleetStatsCollector. Nil means "not computed yet".
	prsMerged90d   *int
	prsRejected90d *int
	agentCount     int

	// --- Productivity ----------------------------------------------------
	// mergesPerWeek / activeRepos come from the hive's own audit-derived
	// RepoActivity summary, so unlike prsMerged90d they describe THIS hive's
	// output across the repos it actually touched. nil = not reported.
	mergesPerWeek *float64
	activeRepos   int

	workSource         string
	contributorCount   int
	activeContributors int

	// tasksCompleted7d is contributor-relay throughput, summed on the spoke
	// from its hourly buckets. A flat zero is a real measurement for a hive
	// with no contributors rather than a gap.
	tasksCompleted7d *int

	// holdTotal and awaitingReview both measure work stalled on a HUMAN;
	// slaViolations is work aging past its threshold whatever the cause.
	holdTotal      *int
	awaitingReview *int
	slaViolations  *int

	// --- Satisfaction ----------------------------------------------------
	// Fed from evidence (human override rate) and the hive-state SLA trend;
	// the criteria with no signal yet are listed as unmeasured so the axis
	// says what would feed it rather than rendering a bare dash.
}

// Sufficiency floors. Below these a sub-criterion is unmeasured rather than
// scored on noise.
const (
	// minMergedPRsForCostRatio is how many merged PRs a hive needs before
	// tokens-per-merge is meaningful. With one or two merges the ratio is
	// dominated by fixed setup spend.
	minMergedPRsForCostRatio = 5

	// minPRsForReworkRate is the smallest merged+rejected total that gives a
	// stable rejection rate. Below it a single rejection swings the figure by
	// tens of percent.
	minPRsForReworkRate = 4

	// minHiveStateSamples is the smallest 7-day hive-state sample count the
	// trend ratios (agent idle, SLA kept) are read from.
	minHiveStateSamples = 6
)

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
// per-axis Delta and for every fleet figure: a median is what keeps three very
// busy hives from pulling the fleet's number away from what most hives show.
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

// medianFloat is medianOf for native-unit criterion values.
func medianFloat(sample []float64) float64 {
	if len(sample) == 0 {
		return 0
	}
	s := append([]float64(nil), sample...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}
