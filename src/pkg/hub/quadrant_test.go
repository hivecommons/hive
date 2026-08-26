package hub

import (
	"strings"
	"testing"
	"time"
)

// These tests pin the invariants that are SILENT when broken. A quadrant that
// scores wrongly still renders a plausible kite, so nothing throws — the chart
// just quietly misinforms the operator. Each test below guards a specific way
// that could happen.

func iptr(v int) *int         { return &v }
func fptr(v float64) *float64 { return &v }

// fleetOf builds n hives with enough variation to exercise percentile ranking,
// so tests are never accidentally run against a population below the floor.
func fleetOf(n int, mut func(i int, in *quadrantInputs)) []quadrantInputs {
	out := make([]quadrantInputs, n)
	for i := range out {
		out[i] = quadrantInputs{
			acmmLevel:     (i % 5) + 1,
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
		in.tasksCompleted7d = iptr(i)
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
			in.tasksCompleted7d = iptr(10 + i)
		}
	})
	// Same six values, but the decliners report a real zero instead of nil.
	withZeros := fleetOf(10, func(i int, in *quadrantInputs) {
		if i < 6 {
			in.tasksCompleted7d = iptr(10 + i)
		} else {
			in.tasksCompleted7d = iptr(0)
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

// TestNudgeOnlyWhenWeak keeps the instrument from nagging. A nudge that fires
// on something already strong trains people to ignore nudges everywhere.
//
// The gate is deliberately on the weakest CRITERION, not on the axis average. A
// hive can sit at a high autonomy level with a strict governor and still have
// only one of eight repos enrolled: the axis averages well, but "only 1 of 8
// org repos enrolled" is precisely the thing that would move it forward.
// Suppressing that because the axis looks fine would hide the one actionable
// finding on the row, which is the opposite of what this instrument is for.
//
// So the invariant is: a nudge implies SOME contributing criterion ranked at or
// below the median — never that the whole axis did.
func TestNudgeOnlyWhenWeak(t *testing.T) {
	inputs := fleetOf(12, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(i * 3)
		in.workSource = "issues"
	})
	qs := ScoreFleet(inputs)

	// criterionPercentile re-derives one criterion's rank against the same
	// population ScoreFleet used, so the assertion checks the real gate rather
	// than a reimplementation of it.
	criterionPercentile := func(axis string, idx int, want subCriterion) int {
		var pop []float64
		for j := range inputs {
			for _, oc := range criteriaFor(axis, inputs[j]) {
				if oc.name == want.name {
					pop = append(pop, oc.value)
				}
			}
		}
		return percentileRank(want.value, pop, want.higherIsBetter)
	}

	for i, q := range qs {
		for _, a := range q.Axes {
			if !a.Scored || a.Nudge == "" {
				continue
			}
			weakEnough := false
			for _, c := range criteriaFor(a.Axis, inputs[i]) {
				if c.nudge == a.Nudge && criterionPercentile(a.Axis, i, c) <= 50 {
					weakEnough = true
				}
			}
			if !weakEnough {
				t.Errorf("hive %d: axis %s carries nudge %q, but no contributing criterion ranked at or below the median",
					i, a.Axis, a.Nudge)
			}
		}
	}
}

// TestNudgeAbsentWhenEverythingIsStrong is the positive control for the gate
// above: a hive that leads the fleet on every productivity criterion must carry
// no productivity nudge, or the gate is suppressing nothing at all.
func TestNudgeAbsentWhenEverythingIsStrong(t *testing.T) {
	inputs := fleetOf(12, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr((i + 1) * 10)
		in.tasksCompleted7d = iptr((i + 1) * 10)
		in.contributorCount = 4
		in.activeContributors = 4
		// Non-default work source, so that criterion is at its best value too.
		in.workSource = "linear"
	})
	qs := ScoreFleet(inputs)
	for _, a := range qs[11].Axes {
		if a.Axis == AxisProductivity && a.Nudge != "" {
			t.Errorf("the fleet-leading hive still carries a productivity nudge %q — the gate suppresses nothing", a.Nudge)
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

// TestCostRatioFloor guards the tokens-per-PR RATIO against being computed off
// one or two merges, where it reflects fixed setup spend rather than how the
// hive runs.
//
// It asserts the criterion, not the axis. The axis legitimately still scores
// here: the burn RATE (tokens per day) carries no PR floor because it is
// meaningful whatever a hive has shipped, and only the ratio that divides by
// merged PRs needs a denominator worth dividing by.
func TestCostRatioFloor(t *testing.T) {
	below := efficiencyCriteria(quadrantInputs{
		tokensPerDay: fptr(50000),
		prsMerged90d: iptr(minMergedPRsForCostRatio - 1),
	})
	for _, c := range below {
		if c.name == "tokens_per_pr" {
			t.Errorf("tokens_per_pr was computed on %d merged PRs — below the floor",
				minMergedPRsForCostRatio-1)
		}
	}
	// Positive control: at the floor the ratio DOES appear, proving the check
	// above observes the floor rather than a criterion that never fires.
	at := efficiencyCriteria(quadrantInputs{
		tokensPerDay: fptr(50000),
		prsMerged90d: iptr(minMergedPRsForCostRatio),
	})
	found := false
	for _, c := range at {
		if c.name == "tokens_per_pr" {
			found = true
		}
	}
	if !found {
		t.Errorf("tokens_per_pr missing at %d merged PRs — the floor test proves nothing",
			minMergedPRsForCostRatio)
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

// axisOf pulls one axis out of a quadrant, for the tests below.
func axisOf(q Quadrant, axis string) (AxisScore, bool) {
	for _, a := range q.Axes {
		if a.Axis == axis {
			return a, true
		}
	}
	return AxisScore{}, false
}

// --- Absolute activity floor (BUG 1) -----------------------------------------

// TestIdleFleetScoresLowNotMedian is the headline regression.
//
// A percentile puts a uniformly-tied population at 50 — ties count as half —
// so before the activity gate a fleet where NOBODY had merged anything scored
// every hive at ~50 on productivity and reported a healthy, average fleet that
// did not exist. The live fleet is exactly this shape: 28 of 62 hives at zero
// merged PRs.
//
// The invariant: doing nothing, measured, scores near zero. Not near the median.
func TestIdleFleetScoresLowNotMedian(t *testing.T) {
	// Twenty hives, all genuinely idle: they REPORT (so they are scored, not
	// unscored) and every reported number is zero.
	idle := ScoreFleet(fleetOf(20, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(0)
		in.tasksCompleted7d = iptr(0)
	}))

	for i, q := range idle {
		a, ok := axisOf(q, AxisProductivity)
		if !ok || !a.Scored {
			t.Fatalf("hive %d: productivity must still be SCORED — a measured zero is "+
				"a real measurement, not absent evidence", i)
		}
		if a.Score >= idleFleetMaxScore {
			t.Errorf("hive %d: an entirely idle hive scored productivity %d — "+
				"doing nothing is ranking at or near the fleet median, which is the "+
				"bug this gate exists to prevent", i, a.Score)
		}
	}

	// Positive control. The same fleet shape, but every hive genuinely busy,
	// MUST produce high scores — otherwise the assertion above would pass on a
	// scorer that simply returns zero for everything and proves nothing.
	busy := ScoreFleet(fleetOf(20, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(activityFloorMergedPRs * (i + 2))
		in.tasksCompleted7d = iptr(activityFloorRelayTasks * (i + 2))
	}))
	top, ok := axisOf(busy[19], AxisProductivity)
	if !ok || !top.Scored {
		t.Fatal("positive control: the busiest hive must score productivity")
	}
	if top.Score <= idleFleetMaxScore {
		t.Errorf("positive control failed: the fleet-leading BUSY hive scored only %d — "+
			"the idle assertion above would pass even on a scorer that always returns 0",
			top.Score)
	}
}

// idleFleetMaxScore is the ceiling an entirely idle hive must stay under.
//
// Set well below 50 (the value a tied percentile produces) so the test fails
// loudly if the gate is removed, but not at 0 — the productivity axis averages
// several criteria and the exempt ones (work source, human-blocked, SLA) still
// contribute a real rank to an idle hive, which is correct: a hive with nothing
// blocked genuinely is not blocked.
const idleFleetMaxScore = 35

// TestSinglePRDoesNotReachTopDecile pins the ramp's shape.
//
// The operator's specific complaint: one hive with THREE merged PRs outranked
// 28 idle ones and landed high. Rank alone said it beat 28 hives, so it did.
// With the gate its three PRs earn ~30% of that rank, which keeps it ABOVE the
// idle hives (it did do more) without putting it near the top of the fleet.
func TestSinglePRDoesNotReachTopDecile(t *testing.T) {
	// One hive with a single merged PR among nineteen idle ones — the live
	// fleet's shape, where a lone PR previously outranked everybody.
	in := fleetOf(20, func(i int, in *quadrantInputs) {
		if i == 0 {
			in.prsMerged90d = iptr(1)
			return
		}
		in.prsMerged90d = iptr(0)
	})
	qs := ScoreFleet(in)

	lone, ok := axisOf(qs[0], AxisProductivity)
	if !ok || !lone.Scored {
		t.Fatal("the one active hive must score productivity")
	}
	if lone.Score >= topDecileScore {
		t.Errorf("a hive with ONE merged PR scored %d, reaching the top decile — "+
			"outranking idle hives is not the same as leading the fleet", lone.Score)
	}

	// It must still be ABOVE the idle hives: the gate scales activity down, it
	// does not erase the distinction between one PR and none. Without this the
	// test would pass on a gate that flattened everybody to zero.
	idle, _ := axisOf(qs[1], AxisProductivity)
	if lone.Score <= idle.Score {
		t.Errorf("the hive with one merged PR (%d) did not outscore an idle one (%d) — "+
			"the gate has erased real activity rather than scaling it", lone.Score, idle.Score)
	}

	// Positive control: a hive at the activity floor in the SAME fleet shape
	// does reach the top decile, proving the ceiling above is enforced by the
	// ramp rather than by the criterion being incapable of scoring high.
	atFloor := fleetOf(20, func(i int, in *quadrantInputs) {
		if i == 0 {
			in.prsMerged90d = iptr(activityFloorMergedPRs)
			return
		}
		in.prsMerged90d = iptr(0)
	})
	lead, ok := axisOf(ScoreFleet(atFloor)[0], AxisProductivity)
	if !ok || lead.Score < topDecileScore {
		t.Errorf("positive control failed: a hive at the %d-PR activity floor scored only %d — "+
			"the top decile is unreachable, so the ceiling test proves nothing",
			activityFloorMergedPRs, lead.Score)
	}
}

// topDecileScore is the "top of the fleet" boundary the ramp must keep a
// barely-active hive out of, and a floor-satisfying hive must be able to reach.
const topDecileScore = 90

// TestMeasuredZeroStillDiffersFromNil is the load-bearing invariant the
// activity gate must NOT break.
//
// The gate makes measured zero score LOW, which brings it closer to the
// intuition of "nothing". It must still be categorically different from nil:
// zero is a measurement and SCORES; nil is absent evidence and stays
// Scored=false, rendering as a collapsed spoke. Folding them together would
// tell owners they are failing at something never measured.
func TestMeasuredZeroStillDiffersFromNil(t *testing.T) {
	// Every productivity signal absent → unscored.
	absent := ScoreFleet(fleetOf(10, nil))
	for i, q := range absent {
		if a, ok := axisOf(q, AxisProductivity); ok && a.Scored {
			t.Fatalf("hive %d: productivity scored %d with NO productivity signal reported — "+
				"absent evidence has collapsed into a measurement", i, a.Score)
		}
	}

	// The same fleet reporting a real zero → scored, and scored LOW.
	measured := ScoreFleet(fleetOf(10, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(0)
	}))
	for i, q := range measured {
		a, ok := axisOf(q, AxisProductivity)
		if !ok || !a.Scored {
			t.Fatalf("hive %d: a reported zero must be SCORED — it is a measurement", i)
		}
		if a.Score >= idleFleetMaxScore {
			t.Errorf("hive %d: measured zero scored %d, near the median", i, a.Score)
		}
	}
}

// --- Org-shared counts (BUG 3) -----------------------------------------------

// TestOrgSharedCountsDoNotMultiply guards the population against one org's
// output being counted once per hive enrolled in it.
//
// FleetStatsCollector counts AI-author PRs across a whole ORG, so every hive in
// that org reports the org's entire output as its own. Three live hives each
// reported prsMerged90d=3746 for this reason. Counted three times, that single
// org adds two phantom members at the top of the distribution and pushes every
// other hive's percentile down.
// orgDedupeResidualTolerance is how far a deduped fleet's score may sit from
// the same fleet with a single hive in the org.
//
// It is not zero because dedupe legitimately SHRINKS the population: three rows
// collapse to one, so the percentile's denominator drops by two and every rank
// shifts by a point or so. That residual is arithmetic, not multiplication. The
// bug being guarded moves the score by tens of points, so a few points of slack
// keeps the test honest without making it brittle to population size.
const orgDedupeResidualTolerance = 5

func TestOrgSharedCountsDoNotMultiply(t *testing.T) {
	// A modest hive whose rank we watch, plus one org reported by three hives.
	const shared = 3746
	build := func(coOrgHives int) []quadrantInputs {
		in := fleetOf(10, func(i int, in *quadrantInputs) {
			in.prsMerged90d = iptr(activityFloorMergedPRs * 2)
		})
		for i := 0; i < coOrgHives; i++ {
			in[i].countGroup = "bigco/ai-bot"
			in[i].prsMerged90d = iptr(shared)
		}
		return in
	}

	// Hive 9 is an ordinary hive in every case. Its score is the probe: one
	// org's output must weigh on the distribution the same whether that org is
	// represented by one hive or by three.
	one, _ := axisOf(ScoreFleet(build(1))[9], AxisProductivity)
	three, _ := axisOf(ScoreFleet(build(3))[9], AxisProductivity)
	if !one.Scored || !three.Scored {
		t.Fatal("expected the ordinary hive to score productivity in both fleets")
	}

	// The counterfactual: the SAME three co-org hives left ungrouped, i.e. the
	// buggy behaviour where one org's output is counted once per hive in it.
	undeduped := build(3)
	for i := 0; i < 3; i++ {
		undeduped[i].countGroup = ""
	}
	multiplied, _ := axisOf(ScoreFleet(undeduped)[9], AxisProductivity)

	// Triple-counting must visibly depress an unrelated hive's rank — that is
	// the harm. If it does not, this test is measuring nothing.
	if multiplied.Score >= one.Score {
		t.Fatalf("positive control failed: triple-counting one org left an unrelated hive at "+
			"%d vs %d — the duplicates are not affecting the distribution at all, so the "+
			"dedupe assertion below proves nothing", multiplied.Score, one.Score)
	}

	// And dedupe must undo essentially all of that harm. An exact match with
	// the single-reporter case is not required: collapsing three rows to one
	// legitimately shrinks the population by two, which shifts a percentile
	// slightly. What must not survive is the multiplication.
	if three.Score <= multiplied.Score {
		t.Errorf("dedupe did not help: unrelated hive scored %d with dedupe and %d without, "+
			"against %d when the org had a single hive", three.Score, multiplied.Score, one.Score)
	}
	if diff := one.Score - three.Score; diff > orgDedupeResidualTolerance {
		t.Errorf("an unrelated hive scored %d when one org-hive reported but %d when three "+
			"co-org hives did (tolerance %d) — one org's output is still being weighed "+
			"more than once", one.Score, three.Score, orgDedupeResidualTolerance)
	}
}

// TestCountGroupKeyedOnOrgAndAuthor pins what makes two counts "the same
// measurement". Org alone would wrongly merge two teams in one org running
// different AI authors, whose output genuinely IS distinct.
func TestCountGroupKeyedOnOrgAndAuthor(t *testing.T) {
	same := fleetCountGroup(RegistryEntry{Org: "BigCo", AIAuthor: "ai-bot"})
	if got := fleetCountGroup(RegistryEntry{Org: "bigco", AIAuthor: "AI-Bot"}); got != same {
		t.Errorf("case difference produced different groups (%q vs %q) — GitHub orgs and "+
			"logins are case-insensitive, so duplicates would slip through", got, same)
	}
	if got := fleetCountGroup(RegistryEntry{Org: "bigco", AIAuthor: "other-bot"}); got == same {
		t.Error("two different AI authors in one org share a count group — genuinely " +
			"distinct output is being deduped away")
	}
	// Unknown halves must never group: an unidentifiable hive counts once, on
	// its own, rather than being merged with every other unidentifiable hive.
	if g := fleetCountGroup(RegistryEntry{Org: "bigco"}); g != "" {
		t.Errorf("a hive with no AI author got group %q — it cannot be shown to duplicate anyone", g)
	}
	if g := fleetCountGroup(RegistryEntry{AIAuthor: "ai-bot"}); g != "" {
		t.Errorf("a hive with no org got group %q", g)
	}
	// AIAuthorEffective is who the agents ACTUALLY author as and must win.
	eff := fleetCountGroup(RegistryEntry{Org: "bigco", AIAuthor: "cfg", AIAuthorEffective: "app[bot]"})
	if eff != "bigco/app[bot]" {
		t.Errorf("effective author ignored: got %q", eff)
	}
}

// --- Trust criterion set (BUG 2) ---------------------------------------------

// TestTrustCriteriaSetIsExplicit stops Trust silently dropping a criterion.
//
// This is the exact failure being fixed: the governor criterion matched
// "observe"/"advisory"/"enforce" while governor.Mode only ever emits SURGE,
// BUSY, QUIET or IDLE, so it contributed to NOTHING on every hive in the fleet
// — one of three Trust criteria gone, with the axis still rendering a plausible
// number and nothing anywhere reporting a problem.
//
// Asserting the criterion SET by name means any future mismatch of this shape
// fails loudly here instead of vanishing into a percentile.
func TestTrustCriteriaSetIsExplicit(t *testing.T) {
	// A hive reporting every trust signal the hub can receive.
	full := quadrantInputs{
		acmmLevel:       4,
		reposEnrolled:   3,
		reposInOrg:      9,
		mergeAcceptance: fptr(0.8),
	}
	want := map[string]bool{
		"autonomy":         true,
		"merge_acceptance": true,
		"breadth":          true,
	}
	got := map[string]bool{}
	for _, c := range trustCriteria(full) {
		got[c.name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("trust criterion %q did not fire on a hive reporting every trust signal — "+
				"a criterion is being silently dropped", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected trust criterion %q — if it is a real delegation signal, add it "+
				"to want; if it is a proxy for one, it does not belong on Trust", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("trust scored on %d criteria, want %d: got %v", len(got), len(want), got)
	}
}

// TestGovernorCadenceIsNotATrustSignal is the regression guard for the
// conceptual half of the bug. The governor's modes are WORKLOAD CADENCE — how
// hard it is currently driving agents — and must never be folded into Trust.
// A hive at SURGE is busy, not trusted; a hive at IDLE is quiet, not
// distrusted.
//
// The check is structural: no trust criterion may vary with the governor's
// actual emitted modes, which is guaranteed here by quadrantInputs no longer
// carrying the mode at all. If somebody reintroduces it, this fails.
func TestGovernorCadenceIsNotATrustSignal(t *testing.T) {
	for _, mode := range []string{"SURGE", "BUSY", "QUIET", "IDLE"} {
		e := RegistryEntry{Org: "o", AIAuthor: "a", ACMMLevel: 3, GovernorMode: mode}
		in := quadrantInputsFor(e, time.Now())
		base := quadrantInputsFor(RegistryEntry{Org: "o", AIAuthor: "a", ACMMLevel: 3}, time.Now())
		if len(trustCriteria(in)) != len(trustCriteria(base)) {
			t.Errorf("governor mode %q changed the trust criteria — workload cadence is "+
				"being read as a trust posture", mode)
		}
	}
	// Positive control: something that IS a trust signal does change the set,
	// proving the comparison above can detect a difference at all.
	withACMM := trustCriteria(quadrantInputs{acmmLevel: 3})
	without := trustCriteria(quadrantInputs{})
	if len(withACMM) == len(without) {
		t.Error("positive control failed: ACMM level did not change the trust criteria, " +
			"so the cadence assertion above cannot detect anything")
	}
}

// --- Activity ramp shape ------------------------------------------------------

// TestActivityFactorRamp pins the ramp itself: zero at zero, linear to the
// floor, flat above it, and exempt when no floor is set.
func TestActivityFactorRamp(t *testing.T) {
	if f := activityFactor(0, 10); f != 0 {
		t.Errorf("zero activity gave factor %v, want 0 — idle would still rank", f)
	}
	if f := activityFactor(5, 10); f != 0.5 {
		t.Errorf("half the floor gave factor %v, want 0.5 — the ramp is not linear", f)
	}
	if f := activityFactor(10, 10); f != 1 {
		t.Errorf("at the floor the factor was %v, want 1", f)
	}
	if f := activityFactor(1000, 10); f != 1 {
		t.Errorf("far above the floor the factor was %v, want 1 — activity beyond the "+
			"floor must not keep inflating the rank", f)
	}
	// Exemption: ratios and postures where zero is a real (often good) value.
	// Gating rework_rate would score the CLEANEST hives worst.
	if f := activityFactor(0, 0); f != 1 {
		t.Errorf("an exempt criterion was gated (factor %v, want 1) — criteria where zero "+
			"is a good outcome would be inverted", f)
	}
}

// TestCleanHiveNotPunishedByGate is the inversion guard for the exemption. A
// hive with zero REJECTED PRs has the best possible rework rate; if the gate
// were applied blanketly its zero would score it worst on efficiency, exactly
// reversing the axis.
func TestCleanHiveNotPunishedByGate(t *testing.T) {
	qs := ScoreFleet(fleetOf(10, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(20)
		in.prsRejected90d = iptr(i * 2) // hive 0 clean, hive 9 mostly rework
	}))
	clean, ok1 := axisOf(qs[0], AxisEfficiency)
	messy, ok2 := axisOf(qs[9], AxisEfficiency)
	if !ok1 || !ok2 || !clean.Scored || !messy.Scored {
		t.Fatal("expected efficiency to score for both hives")
	}
	if clean.Score <= messy.Score {
		t.Errorf("the clean hive scored %d and the high-rework hive %d — the activity gate "+
			"has inverted a lower-is-better criterion by treating its zero as idleness",
			clean.Score, messy.Score)
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
