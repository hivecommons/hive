package hub

import (
	"fmt"
	"math"
)

func criterionExplanation(d criterionDefinition) CriterionScore {
	curve := "linear"
	if d.logarithmic {
		curve = "log1p"
	}
	return CriterionScore{
		Name: d.name, Covers: d.covers, Unit: d.unit, Source: d.source, Good: d.good,
		How:    fmt.Sprintf("100 × (f(value) − f(%g)) / (f(%g) − f(%g)); f=%s; clamped to 0–100; weight 1", d.low, d.high, d.low, curve),
		Weight: 1, Reason: "Not measured yet: not reported or below the stated sample floor",
	}
}

// ScoreFleet uses absolute curves; a hive's score does not change when a peer
// joins, leaves, or improves. The population is used ONLY for deltas and n.
func ScoreFleet(inputs []quadrantInputs) []Quadrant {
	out := make([]Quadrant, len(inputs))
	samples := map[string][]int{}
	for i, in := range inputs {
		q := Quadrant{Axes: make([]AxisScore, 0, len(QuadrantAxisOrder))}
		for _, axis := range QuadrantAxisOrder {
			a := AxisScore{Axis: axis}
			measured := map[string]subCriterion{}
			for _, c := range criteriaFor(axis, in) {
				if !math.IsNaN(c.value) && !math.IsInf(c.value, 0) && c.value >= 0 {
					measured[c.name] = c
				}
			}
			sum, weight, weakest := 0.0, 0.0, 101
			for _, d := range criterionDefinitions(axis) {
				c := criterionExplanation(d)
				if d.name == "merges_per_repo_week" || d.name == "tokens_per_pr" {
					if in.repoMerges14d != nil {
						days := in.repoMergeDays
						if days <= 0 {
							days = 14
						}
						c.Source += fmt.Sprintf("; using hive audit count over %g days", days)
					} else {
						c.Source += "; using org-wide AI-author fallback, not per-hive attribution"
					}
				}
				if v, ok := measured[d.name]; ok {
					s := criterionScore(d, v.value)
					c.Value, c.Score, c.Measured, c.Reason = &v.value, &s, true, ""
					sum += float64(s) * c.Weight
					weight += c.Weight
					if s <= 50 && s < weakest && v.nudge != "" {
						a.Nudge, weakest = v.nudge, s
					}
				}
				a.Criteria = append(a.Criteria, c)
			}
			if weight > 0 {
				a.Scored, a.Score = true, clampScore(int(math.Round(sum/weight)))
				a.Summary = fmt.Sprintf("%s %d/100 from %d measured criteria; gaps excluded", axis, a.Score, int(weight))
				samples[axis] = append(samples[axis], a.Score)
			} else {
				a.Reason = unscoredReason(axis)
				a.Summary = a.Reason
			}
			q.Axes = append(q.Axes, a)
		}
		out[i] = q
	}
	for i := range out {
		for j := range out[i].Axes {
			a := &out[i].Axes[j]
			a.N = len(samples[a.Axis])
			if a.Scored {
				a.Delta = a.Score - medianOf(samples[a.Axis])
			}
		}
		setComposite(&out[i])
	}
	return out
}

func setComposite(q *Quadrant) {
	sum := 0
	for _, a := range q.Axes {
		if a.Scored {
			sum += a.Score
			q.ScoredAxes++
		}
	}
	if q.ScoredAxes > 0 {
		q.Composite = int(math.Round(float64(sum) / float64(q.ScoredAxes)))
	}
}

// FleetAverage keeps its name for source compatibility; the polygon is now the
// per-axis MEDIAN of scored hives. Criterion values are also medians, never a
// pooled rate or a claim that the median sub-scores reconstruct the axis score.
func FleetAverage(qs []Quadrant) Quadrant {
	out := Quadrant{Axes: make([]AxisScore, 0, len(QuadrantAxisOrder))}
	for _, axis := range QuadrantAxisOrder {
		a := AxisScore{Axis: axis}
		var scores []int
		for _, q := range qs {
			for _, s := range q.Axes {
				if s.Axis == axis && s.Scored {
					scores = append(scores, s.Score)
				}
			}
		}
		a.N = len(scores)
		for _, d := range criterionDefinitions(axis) {
			c := criterionExplanation(d)
			var values []float64
			var subs []int
			for _, q := range qs {
				for _, s := range q.Axes {
					if s.Axis == axis {
						for _, sub := range s.Criteria {
							if sub.Name == d.name && sub.Measured && sub.Value != nil && sub.Score != nil {
								values = append(values, *sub.Value)
								subs = append(subs, *sub.Score)
							}
						}
					}
				}
			}
			if len(values) > 0 {
				v, s := medianFloat(values), medianOf(subs)
				c.Measured, c.Value, c.Score, c.Reason = true, &v, &s, ""
				c.Source += fmt.Sprintf("; median of %d reporting hives (not pooled)", len(values))
			}
			a.Criteria = append(a.Criteria, c)
		}
		if len(scores) > 0 {
			a.Scored, a.Score = true, medianOf(scores)
			a.Summary = fmt.Sprintf("%s median %d/100 · n=%d scored hives", axis, a.Score, a.N)
		} else {
			a.Reason = unscoredReason(axis)
			a.Summary = a.Reason
		}
		out.Axes = append(out.Axes, a)
	}
	setComposite(&out)
	return out
}
