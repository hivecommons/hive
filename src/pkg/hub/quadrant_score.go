package hub

import "math"

// Fleet-relative assembly: turning each hive's raw sub-criteria into percentile
// scores against the population it is being viewed with.
//
// The population matters. Scores are computed against the CURRENTLY FILTERED
// set of hives, not against the whole registry, so that a filtered view and its
// header aggregate agree with each other. Ranking a filtered hive against
// hives the operator has filtered out would make the header polygon and the
// rows it summarizes disagree, which is worse than either being "wrong".

// ScoreFleet computes quadrants for a population of hives at once.
//
// It must be a fleet-level operation rather than a per-hive one because a
// percentile is meaningless without the population. Callers pass every hive in
// the current view and receive scores aligned by index.
//
// Small populations: below minFleetForPercentile a percentile would be noise
// dressed up as precision (ranking three hives yields 0/50/100 regardless of
// how close they actually are), so every axis reports unscored and the chart
// honestly shows nothing rather than a confident fiction.
func ScoreFleet(inputs []quadrantInputs) []Quadrant {
	out := make([]Quadrant, len(inputs))
	if len(inputs) == 0 {
		return out
	}

	// Gather each axis's sub-criteria for every hive up front, so a criterion's
	// population can be assembled before any hive is scored.
	perHive := make([]map[string][]subCriterion, len(inputs))
	for i, in := range inputs {
		perHive[i] = map[string][]subCriterion{}
		for _, axis := range QuadrantAxisOrder {
			perHive[i][axis] = criteriaFor(axis, in)
		}
	}

	// Build the population sample for every (axis, criterion) pair. Only hives
	// that actually reported a criterion contribute to its population — a hive
	// that declined must not be counted as a zero, or it would drag the whole
	// fleet's distribution toward the bottom and make everyone else look good
	// for the wrong reason.
	type critKey struct{ axis, name string }
	populations := map[critKey][]float64{}
	for i := range inputs {
		for axis, crits := range perHive[i] {
			for _, c := range crits {
				k := critKey{axis, c.name}
				populations[k] = append(populations[k], c.value)
			}
		}
	}

	// First pass: per-hive, per-axis scores.
	axisSamples := map[string][]int{} // for medians / deltas
	for i := range inputs {
		q := Quadrant{Axes: make([]AxisScore, 0, len(QuadrantAxisOrder))}
		for _, axis := range QuadrantAxisOrder {
			crits := perHive[i][axis]
			as := AxisScore{Axis: axis}

			if len(crits) == 0 {
				as.Scored = false
				as.Reason = unscoredReason(axis)
				q.Axes = append(q.Axes, as)
				continue
			}

			// Average the criterion percentiles. Equal weighting is deliberate:
			// any weighting scheme would encode a claim about relative
			// importance that nobody has validated, and it would be invisible
			// to the people reading the chart.
			sum, n := 0, 0
			weakest, weakestScore := "", 101
			for _, c := range crits {
				pop := populations[critKey{axis, c.name}]
				if len(pop) < minFleetForPercentile {
					continue
				}
				s := percentileRank(c.value, pop, c.higherIsBetter)
				sum += s
				n++
				if c.nudge != "" && s < weakestScore {
					weakest, weakestScore = c.nudge, s
				}
			}

			if n == 0 {
				// Criteria existed but the fleet is too small to rank them.
				as.Scored = false
				as.Reason = "Fleet too small to compare against"
				q.Axes = append(q.Axes, as)
				continue
			}

			as.Score = clampScore(int(math.Round(float64(sum) / float64(n))))
			as.Scored = true
			// Only surface a nudge when the axis is actually weak. A nudge on a
			// strong axis reads as nagging and trains people to ignore them.
			if weakestScore <= 50 {
				as.Nudge = weakest
			}
			q.Axes = append(q.Axes, as)
			axisSamples[axis] = append(axisSamples[axis], as.Score)
		}
		out[i] = q
	}

	// Second pass: deltas against the per-axis fleet median, and the composite.
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

// FleetAverage collapses a population's quadrants into a single reference
// polygon — the shape drawn behind each row's kite and at the top of the
// dashboard.
//
// Per axis it averages only the hives that scored that axis. An axis nobody
// scored comes back unscored, so the aggregate never invents a value for a
// signal the fleet does not collect.
func FleetAverage(qs []Quadrant) Quadrant {
	avg := Quadrant{Axes: make([]AxisScore, 0, len(QuadrantAxisOrder))}
	sums := map[string]int{}
	counts := map[string]int{}
	for _, q := range qs {
		for _, a := range q.Axes {
			if !a.Scored {
				continue
			}
			sums[a.Axis] += a.Score
			counts[a.Axis]++
		}
	}
	total, scored := 0, 0
	for _, axis := range QuadrantAxisOrder {
		as := AxisScore{Axis: axis}
		if counts[axis] > 0 {
			as.Score = clampScore(int(math.Round(float64(sums[axis]) / float64(counts[axis]))))
			as.Scored = true
			total += as.Score
			scored++
		} else {
			as.Reason = unscoredReason(axis)
		}
		avg.Axes = append(avg.Axes, as)
	}
	avg.ScoredAxes = scored
	if scored > 0 {
		avg.Composite = clampScore(int(math.Round(float64(total) / float64(scored))))
	}
	return avg
}
