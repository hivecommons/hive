package hub

import (
	"fmt"
	"math"
	"strings"
)

// scoreAxis turns one hive's criteria for one axis into an AxisScore.
//
// The score is the WEIGHTED MEAN of the measured criteria only. A criterion
// with no input is listed as unmeasured and excluded — never scored 0 — so a
// hive is not punished for a signal the platform does not collect for it yet.
// Every criterion maps to 0–100 through its own named curve (quadrant_curve.go),
// so the number is absolute: a fleet that improves sees its score improve,
// which a percentile could never show.
func scoreAxis(axis string, crits []subCriterion) AxisScore {
	as := AxisScore{Axis: axis, Criteria: make([]CriterionScore, 0, len(crits))}
	var sum, weight float64
	weakestScore, weakestNudge := 101, ""
	var best, worst *CriterionScore
	for _, c := range crits {
		cs := CriterionScore{
			Name:     c.spec.name,
			Label:    c.spec.label,
			Measured: c.measured,
			Weight:   c.spec.weight,
			How:      c.spec.howText(),
			Good:     c.spec.good,
			Reason:   c.reason,
		}
		if c.measured {
			cs.Value = c.value
			cs.Display = c.spec.format(c.value)
			cs.Score = c.score()
			sum += float64(cs.Score) * c.spec.weight
			weight += c.spec.weight
			as.Measured++
			if c.nudge != "" && cs.Score < weakestScore {
				weakestScore, weakestNudge = cs.Score, c.nudge
			}
		} else {
			as.Unmeasured++
		}
		as.Criteria = append(as.Criteria, cs)
		if c.measured {
			i := len(as.Criteria) - 1
			if best == nil || cs.Score > best.Score {
				best = &as.Criteria[i]
			}
			if worst == nil || cs.Score < worst.Score {
				worst = &as.Criteria[i]
			}
		}
	}
	if weight == 0 {
		as.Reason = unscoredReason(axis)
		as.Summary = fmt.Sprintf("%s: not measured yet", QuadrantAxisLabels[axis])
		return as
	}
	as.Score = clampScore(int(math.Round(sum / weight)))
	as.Scored = true
	// Only surface a nudge when the axis is actually weak. A nudge on a
	// strong axis reads as nagging and trains people to ignore them.
	if weakestScore <= 50 {
		as.Nudge = weakestNudge
	}
	as.Summary = axisSummary(axis, as, crits)
	return as
}

// axisSummary is the one-line plain reading under the glyph: the axis score,
// its strongest and weakest measured facts, and how much is still unmeasured.
func axisSummary(axis string, as AxisScore, crits []subCriterion) string {
	var strongest, weakest *subCriterion
	for i := range crits {
		c := &crits[i]
		if !c.measured || c.fact == "" {
			continue
		}
		if strongest == nil || c.score() > strongest.score() {
			strongest = c
		}
		if weakest == nil || c.score() < weakest.score() {
			weakest = c
		}
	}
	parts := []string{}
	switch {
	case strongest == nil:
	case strongest == weakest:
		parts = append(parts, strongest.fact)
	default:
		parts = append(parts, strongest.fact+" but "+weakest.fact)
	}
	if as.Unmeasured > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d criteria not measured yet", as.Unmeasured, as.Measured+as.Unmeasured))
	}
	return fmt.Sprintf("%s %d: %s", QuadrantAxisLabels[axis], as.Score, strings.Join(parts, " · "))
}

// ScoreFleet computes quadrants for a population of hives at once, aligned by
// index. Scoring is per hive and absolute, so it does not depend on who else
// is in the view; the population is needed only for the per-axis Delta against
// the fleet median.
func ScoreFleet(inputs []quadrantInputs) []Quadrant {
	out := make([]Quadrant, len(inputs))
	axisSamples := map[string][]int{}
	for i, in := range inputs {
		q := Quadrant{Axes: make([]AxisScore, 0, len(QuadrantAxisOrder))}
		for _, axis := range QuadrantAxisOrder {
			as := scoreAxis(axis, criteriaFor(axis, in))
			q.Axes = append(q.Axes, as)
			if as.Scored {
				axisSamples[axis] = append(axisSamples[axis], as.Score)
			}
		}
		out[i] = q
	}

	medians := map[string]int{}
	for axis, sample := range axisSamples {
		medians[axis] = medianOf(sample)
	}
	for i := range out {
		sum, n := 0, 0
		for j := range out[i].Axes {
			a := &out[i].Axes[j]
			if !a.Scored {
				continue
			}
			a.Delta = a.Score - medians[a.Axis]
			sum += a.Score
			n++
		}
		out[i].ScoredAxes = n
		if n > 0 {
			// Composite averages ONLY scored axes. Counting an unmeasured axis
			// as zero would punish a hive for a signal the platform does not
			// collect, which is exactly backwards.
			out[i].Composite = clampScore(int(math.Round(float64(sum) / float64(n))))
		}
	}
	return out
}

// FleetMedian collapses a population's quadrants into the single reference
// polygon drawn behind each row's kite and at the top of the dashboard.
//
// Per axis it takes the MEDIAN over the hives that scored that axis and
// records n. A median rather than a mean because a few very active hives
// would otherwise drag the fleet number away from what most hives show. Per
// criterion it likewise takes the median value and score over the hives that
// measured it, so the fleet explainer can say "what ours shows" honestly. An
// axis nobody scored comes back unscored.
func FleetMedian(qs []Quadrant) Quadrant {
	fleet := Quadrant{Axes: make([]AxisScore, 0, len(QuadrantAxisOrder)), Aggregate: QuadrantFleetAggregate, N: len(qs)}
	type critSample struct {
		values []float64
		scores []int
	}
	axisScores := map[string][]int{}
	critSamples := map[string]map[string]*critSample{}
	for _, q := range qs {
		for _, a := range q.Axes {
			if a.Scored {
				axisScores[a.Axis] = append(axisScores[a.Axis], a.Score)
			}
			for _, c := range a.Criteria {
				if !c.Measured {
					continue
				}
				if critSamples[a.Axis] == nil {
					critSamples[a.Axis] = map[string]*critSample{}
				}
				s := critSamples[a.Axis][c.Name]
				if s == nil {
					s = &critSample{}
					critSamples[a.Axis][c.Name] = s
				}
				s.values = append(s.values, c.Value)
				s.scores = append(s.scores, c.Score)
			}
		}
	}
	total, scored := 0, 0
	for _, axis := range QuadrantAxisOrder {
		as := AxisScore{Axis: axis, N: len(axisScores[axis])}
		var facts []string
		for _, name := range quadrantCriteriaOrder[axis] {
			spec := criterionSpecs[name]
			cs := CriterionScore{Name: name, Label: spec.label, Weight: spec.weight, How: spec.howText(), Good: spec.good}
			if s := critSamples[axis][name]; s != nil {
				cs.Measured = true
				cs.N = len(s.scores)
				cs.Value = medianFloat(s.values)
				cs.Display = spec.format(cs.Value)
				cs.Score = medianOf(s.scores)
				as.Measured++
				facts = append(facts, fmt.Sprintf("%s %s (%s)", strings.ToLower(spec.label), cs.Display, pluralHives(cs.N)))
			} else {
				cs.Reason = "no hive in view reports this yet"
				as.Unmeasured++
			}
			as.Criteria = append(as.Criteria, cs)
		}
		if as.N > 0 {
			as.Score = medianOf(axisScores[axis])
			as.Scored = true
			total += as.Score
			scored++
			summary := fmt.Sprintf("%s %d: median of %s", QuadrantAxisLabels[axis], as.Score, pluralHives(as.N))
			if len(facts) > 0 {
				summary += " · " + facts[0]
			}
			if as.Unmeasured > 0 {
				summary += fmt.Sprintf(" · %d of %d criteria not measured by any hive yet", as.Unmeasured, as.Measured+as.Unmeasured)
			}
			as.Summary = summary
		} else {
			as.Reason = "No hive in view has a measured criterion on this axis yet"
			as.Summary = fmt.Sprintf("%s: not measured by any hive in view yet", QuadrantAxisLabels[axis])
		}
		fleet.Axes = append(fleet.Axes, as)
	}
	fleet.ScoredAxes = scored
	if scored > 0 {
		fleet.Composite = clampScore(int(math.Round(float64(total) / float64(scored))))
	}
	return fleet
}

// pluralHives renders "1 hive" / "n hives" for the fleet summaries.
func pluralHives(n int) string {
	if n == 1 {
		return "1 hive"
	}
	return fmt.Sprintf("%d hives", n)
}
