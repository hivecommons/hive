package hub

import (
	"strings"
	"testing"
)

// These tests pin the invariants that are SILENT when broken. A quadrant that
// scores wrongly still renders a plausible kite, so nothing throws — the chart
// just quietly misinforms the operator. Each test below guards a specific way
// that could happen.

func iptr(v int) *int       { return &v }
func i64ptr(v int64) *int64 { return &v }

// fleetOf builds n hives with enough variation to exercise percentile ranking,
// so tests are never accidentally run against a population below the floor.
func fleetOf(n int, mut func(i int, in *quadrantInputs)) []quadrantInputs {
	out := make([]quadrantInputs, n)
	for i := range out {
		out[i] = quadrantInputs{
			acmmLevel:     (i % 5) + 1,
			governorMode:  []string{"observe", "advisory", "enforce"}[i%3],
			reposEnrolled: i%4 + 1,
			reposInOrg:    8,
		}
		if mut != nil {
			mut(i, &out[i])
		}
	}
	return out
}

// TestUnscoredIsNotZero is the single most important invariant in the package.
// If an unmeasured axis ever scores 0 instead of reporting Scored=false, the
// chart tells owners they are failing at something the platform never measured.
func TestUnscoredIsNotZero(t *testing.T) {
	qs := ScoreFleet(fleetOf(10, nil))
	for _, q := range qs {
		for _, a := range q.Axes {
			if a.Axis != AxisSatisfaction {
				continue
			}
			if a.Scored {
				t.Fatalf("satisfaction must never be scored — no signal exists; got score=%d", a.Score)
			}
			if a.Reason == "" {
				t.Error("an unscored axis must carry a reason, or the hover shows a bare gap")
			}
		}
	}
}

// TestCompositeExcludesUnscoredAxes guards the composite against counting a
// missing signal as a zero. With satisfaction always unscored, a 4-axis mean
// would cap every hive at 75 and make the whole fleet look mediocre.
func TestCompositeExcludesUnscoredAxes(t *testing.T) {
	in := fleetOf(10, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(i)
		in.relayCompletedTasks = iptr(i)
	})
	qs := ScoreFleet(in)
	for i, q := range qs {
		if q.ScoredAxes == 0 {
			continue
		}
		sum := 0
		for _, a := range q.Axes {
			if a.Scored {
				sum += a.Score
			}
		}
		want := sum / q.ScoredAxes
		if diff := q.Composite - want; diff > 1 || diff < -1 {
			t.Errorf("hive %d: composite %d not the mean of %d scored axes (~%d)",
				i, q.Composite, q.ScoredAxes, want)
		}
		if q.ScoredAxes > 3 {
			t.Errorf("hive %d: %d axes scored, but satisfaction has no signal", i, q.ScoredAxes)
		}
	}
}

// TestDecliningHiveDoesNotJoinPopulation is subtle and high-consequence. A hive
// that cannot report a criterion must be ABSENT from that criterion's
// population, not present as a zero — otherwise it drags the distribution down
// and inflates everyone else's percentile.
//
// Positive control included: the same fleet WITH the criterion reported must
// score differently, proving the test observes the mechanism it claims to.
func TestDecliningHiveDoesNotJoinPopulation(t *testing.T) {
	// Six hives report relay activity; four decline entirely (nil).
	withDecliners := fleetOf(10, func(i int, in *quadrantInputs) {
		if i < 6 {
			in.relayCompletedTasks = iptr(10 + i)
		}
	})
	// Same six values, but the decliners report a real zero instead of nil.
	withZeros := fleetOf(10, func(i int, in *quadrantInputs) {
		if i < 6 {
			in.relayCompletedTasks = iptr(10 + i)
		} else {
			in.relayCompletedTasks = iptr(0)
		}
	})

	a := ScoreFleet(withDecliners)
	b := ScoreFleet(withZeros)

	prodOf := func(q Quadrant) (int, bool) {
		for _, ax := range q.Axes {
			if ax.Axis == AxisProductivity {
				return ax.Score, ax.Scored
			}
		}
		return 0, false
	}

	sa, oka := prodOf(a[0])
	sb, okb := prodOf(b[0])
	if !oka || !okb {
		t.Fatal("expected hive 0 to score productivity in both fleets")
	}
	if sa == sb {
		t.Errorf("nil (declined) and 0 (measured) produced the same score %d — "+
			"absent evidence is being treated as a measurement", sa)
	}
}

// TestNudgeOnlyWhenWeak keeps the instrument from nagging. A nudge attached to
// a strong axis trains people to ignore nudges everywhere.
func TestNudgeOnlyWhenWeak(t *testing.T) {
	qs := ScoreFleet(fleetOf(12, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(i * 3)
		in.workSource = "issues"
	}))
	for i, q := range qs {
		for _, a := range q.Axes {
			if a.Scored && a.Score > 50 && a.Nudge != "" {
				t.Errorf("hive %d: axis %s scored %d but still carries a nudge %q",
					i, a.Axis, a.Score, a.Nudge)
			}
		}
	}
}

// TestSmallFleetIsUnscored stops the chart claiming precision it cannot have.
// Ranking three hives yields 0/50/100 no matter how close they really are.
func TestSmallFleetIsUnscored(t *testing.T) {
	qs := ScoreFleet(fleetOf(3, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(i)
	}))
	for i, q := range qs {
		for _, a := range q.Axes {
			if a.Scored {
				t.Errorf("hive %d: axis %s scored in a fleet of 3 — below the percentile floor",
					i, a.Axis)
			}
		}
	}
	// Positive control: the same shape at fleet size 10 DOES score, proving the
	// floor is what suppressed it rather than the inputs being unscorable.
	big := ScoreFleet(fleetOf(10, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(i)
	}))
	any := false
	for _, q := range big {
		if q.ScoredAxes > 0 {
			any = true
		}
	}
	if !any {
		t.Error("positive control failed: fleet of 10 scored nothing, so the small-fleet test proves nothing")
	}
}

// TestCostRatioFloor guards against scoring efficiency off one or two merges,
// where the ratio reflects fixed setup spend rather than how the hive runs.
func TestCostRatioFloor(t *testing.T) {
	qs := ScoreFleet(fleetOf(10, func(i int, in *quadrantInputs) {
		in.tokensTotal = i64ptr(500000)
		in.prsMerged90d = iptr(2) // below minMergedPRsForCostRatio
	}))
	for i, q := range qs {
		for _, a := range q.Axes {
			if a.Axis == AxisEfficiency && a.Scored {
				t.Errorf("hive %d: efficiency scored on 2 merged PRs — below the floor", i)
			}
		}
	}
}

// TestReworkRateRanksRejectionAsBad guards the direction of the rework
// criterion. If it inverts, the hives wasting the most budget on PRs nobody
// merged would score BEST on efficiency — rewarding exactly the behaviour the
// axis exists to flag, while still drawing a perfectly plausible kite.
func TestReworkRateRanksRejectionAsBad(t *testing.T) {
	// Hive 0 is clean (10 merged, 0 rejected); hive 9 is mostly rework.
	qs := ScoreFleet(fleetOf(10, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(10)
		in.prsRejected90d = iptr(i * 2)
	}))
	effOf := func(q Quadrant) (int, bool) {
		for _, a := range q.Axes {
			if a.Axis == AxisEfficiency {
				return a.Score, a.Scored
			}
		}
		return 0, false
	}
	clean, ok1 := effOf(qs[0])
	messy, ok2 := effOf(qs[9])
	if !ok1 || !ok2 {
		t.Fatal("expected efficiency to score for both hives")
	}
	if clean <= messy {
		t.Errorf("rework ranking inverted: clean hive %d, high-rework hive %d — "+
			"wasted effort is scoring as efficiency", clean, messy)
	}
}

// TestReworkFloorSuppressesNoise stops a single rejection out of two PRs
// reading as a 50%% rework rate and flagging a hive that has barely started.
func TestReworkFloorSuppressesNoise(t *testing.T) {
	qs := ScoreFleet(fleetOf(10, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(1)
		in.prsRejected90d = iptr(1) // total 2, below minPRsForReworkRate
	}))
	for i, q := range qs {
		for _, a := range q.Axes {
			if a.Axis != AxisEfficiency || !a.Scored {
				continue
			}
			if strings.Contains(a.Nudge, "rejected") {
				t.Errorf("hive %d: rework nudge fired on a 2-PR sample: %q", i, a.Nudge)
			}
		}
	}
}

// TestAxisOrderStable pins the canonical order. The small kite in the table is
// readable only because the axes never move; a reorder would silently change
// what every previously-learned shape means.
func TestAxisOrderStable(t *testing.T) {
	want := []string{AxisTrust, AxisProductivity, AxisSatisfaction, AxisEfficiency}
	if len(QuadrantAxisOrder) != len(want) {
		t.Fatalf("axis count changed: %v", QuadrantAxisOrder)
	}
	for i := range want {
		if QuadrantAxisOrder[i] != want[i] {
			t.Fatalf("axis order changed at %d: got %s want %s — every learned kite shape now means something else",
				i, QuadrantAxisOrder[i], want[i])
		}
	}
	qs := ScoreFleet(fleetOf(6, nil))
	for _, q := range qs {
		for i, a := range q.Axes {
			if a.Axis != want[i] {
				t.Fatalf("emitted axes out of canonical order: %v", q.Axes)
			}
		}
	}
}

// TestLowerIsBetterInverted proves cost-style criteria rank the right way. If
// this inverts, the most expensive hives score best and the chart rewards
// exactly what it should flag.
func TestLowerIsBetterInverted(t *testing.T) {
	pop := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cheap := percentileRank(1, pop, false)
	dear := percentileRank(10, pop, false)
	if cheap <= dear {
		t.Errorf("cost ranking inverted: cheap=%d dear=%d — expensive hives are scoring better", cheap, dear)
	}
}

// TestFleetAverageIgnoresUnscored keeps the reference polygon honest: an axis
// nobody could score must not appear as a real value behind every row.
func TestFleetAverageIgnoresUnscored(t *testing.T) {
	avg := FleetAverage(ScoreFleet(fleetOf(10, nil)))
	for _, a := range avg.Axes {
		if a.Axis == AxisSatisfaction && a.Scored {
			t.Error("fleet average invented a satisfaction score")
		}
	}
	if avg.ScoredAxes == 0 {
		t.Error("fleet average scored nothing at all — the reference polygon would be empty")
	}
}

// TestEmptyFleetDoesNotPanic covers the day-one state, which is exactly when a
// nil-handling bug would surface in front of the operator.
func TestEmptyFleetDoesNotPanic(t *testing.T) {
	if got := ScoreFleet(nil); len(got) != 0 {
		t.Errorf("expected no scores for an empty fleet, got %d", len(got))
	}
	avg := FleetAverage(nil)
	if avg.Composite != 0 || avg.ScoredAxes != 0 {
		t.Errorf("empty fleet average should be zero-valued, got %+v", avg)
	}
	if len(avg.Axes) != len(QuadrantAxisOrder) {
		t.Errorf("average must still emit all four axes so the renderer has a stable shape, got %d", len(avg.Axes))
	}
}
