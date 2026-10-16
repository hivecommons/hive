package hub

import (
	"sort"
)

const (
	AxisTrust        = "trust"
	AxisEfficiency   = "efficiency"
	AxisSatisfaction = "satisfaction"
	AxisProductivity = "productivity"
)

// QuadrantAxisOrder is a stable wire and visual contract: top, right, bottom, left.
var QuadrantAxisOrder = []string{AxisTrust, AxisProductivity, AxisSatisfaction, AxisEfficiency}

// CriterionScore explains both measured inputs and explicit gaps. Missing values
// and scores are null, not zero. Weight applies only when Measured is true.
type CriterionScore struct {
	Name     string   `json:"name"`
	Covers   string   `json:"covers"`
	Unit     string   `json:"unit"`
	Source   string   `json:"source"`
	Good     string   `json:"good"`
	How      string   `json:"how"`
	Value    *float64 `json:"value"`
	Score    *int     `json:"score"`
	Measured bool     `json:"measured"`
	Weight   float64  `json:"weight"`
	Reason   string   `json:"reason,omitempty"`
}

// AxisScore retains the original wire fields. Scores are fixed-reference 0–100,
// not percentiles. Scored=false is missing evidence, never a measured zero.
type AxisScore struct {
	Axis     string           `json:"axis"`
	Score    int              `json:"score"`
	Scored   bool             `json:"scored"`
	Reason   string           `json:"reason,omitempty"`
	Delta    int              `json:"delta"`
	Nudge    string           `json:"nudge,omitempty"`
	Criteria []CriterionScore `json:"criteria"`
	Summary  string           `json:"summary"`
	// N is the number of scored hives for this axis in the current view.
	N int `json:"n"`
}

type Quadrant struct {
	Axes []AxisScore `json:"axes"`
	// Composite averages scored axes only.
	Composite  int `json:"composite"`
	ScoredAxes int `json:"scored_axes"`
}

// Pointers preserve absent vs measured-zero heartbeat values. The extraction
// boundary keeps scoring pure and prevents accidental reads of unrelated state.
type quadrantInputs struct {
	reposEnrolled   int
	evidence        *HeartbeatEvidence
	tokensPerDay    *float64
	agentIdleRatio  *float64
	repoMerges14d   *int // audit count; 14 days on older collectors
	repoMergeDays   float64
	prsMerged90d    *int
	budgetExhausted *bool
	slaViolations   *int
}

const minMergedPRsForCostRatio = 5

func clampScore(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func medianFloat(sample []float64) float64 {
	s := append([]float64(nil), sample...)
	if len(s) == 0 {
		return 0
	}
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

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
