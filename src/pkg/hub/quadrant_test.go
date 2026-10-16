package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
)

// These tests pin the invariants that are SILENT when broken. A quadrant that
// scores wrongly still renders a plausible kite, so nothing throws — the chart
// just quietly misinforms the operator. Each test below guards a specific way
// that could happen.

func iptr(v int) *int         { return &v }
func fptr(v float64) *float64 { return &v }

// fleetOf builds n hives with a little variation so fleet medians are not all
// ties.
func fleetOf(n int, mut func(i int, in *quadrantInputs)) []quadrantInputs {
	out := make([]quadrantInputs, n)
	for i := range out {
		out[i] = quadrantInputs{
			acmmLevel:     (i % 5) + 1,
			reposEnrolled: i%4 + 1,
		}
		if mut != nil {
			mut(i, &out[i])
		}
	}
	return out
}

func axisOf(q Quadrant, axis string) (AxisScore, bool) {
	for _, a := range q.Axes {
		if a.Axis == axis {
			return a, true
		}
	}
	return AxisScore{}, false
}

func criterionOf(t *testing.T, a AxisScore, name string) CriterionScore {
	t.Helper()
	for _, c := range a.Criteria {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("criterion %q not listed on axis %s", name, a.Axis)
	return CriterionScore{}
}

// evidenceWith builds a heartbeat evidence block with enough verdicts and
// merges to clear minEvidenceN.
func evidenceWith(verdicts, falseApprove, falseBlock, merged, reverted int) *HeartbeatEvidence {
	return &HeartbeatEvidence{Verdicts90d: verdicts, FalseApprove90d: falseApprove, FalseBlock90d: falseBlock,
		Merged90d: merged, Reverted14d90d: reverted}
}

// --- Curves -----------------------------------------------------------------

// TestCurveEdges pins the 0–100 mapping at and beyond both ends, in both
// directions and both shapes, so a threshold change is a deliberate edit here
// rather than a silent drift in every score.
func TestCurveEdges(t *testing.T) {
	cases := []struct {
		name string
		c    curve
		v    float64
		want int
	}{
		{"linear worst", curve{curveLinear, 0, 1}, 0, 0},
		{"linear best", curve{curveLinear, 0, 1}, 1, 100},
		{"linear mid", curve{curveLinear, 0, 1}, 0.5, 50},
		{"linear beyond best clamps", curve{curveLinear, 0, 1}, 3, 100},
		{"linear below worst clamps", curve{curveLinear, 0, 1}, -1, 0},
		{"lower-is-better worst", curve{curveLinear, curveRevertWorst, curveRevertBest}, 0.10, 0},
		{"lower-is-better best", curve{curveLinear, curveRevertWorst, curveRevertBest}, 0, 100},
		{"lower-is-better half", curve{curveLinear, curveRevertWorst, curveRevertBest}, 0.05, 50},
		{"lower-is-better beyond worst clamps", curve{curveLinear, curveRevertWorst, curveRevertBest}, 0.9, 0},
		{"log worst", curve{curveLog, curveThroughputWorst, curveThroughputBest}, 0.1, 0},
		{"log best", curve{curveLog, curveThroughputWorst, curveThroughputBest}, 10, 100},
		{"log geometric midpoint", curve{curveLog, curveThroughputWorst, curveThroughputBest}, 1, 50},
		{"log zero output is worst", curve{curveLog, curveThroughputWorst, curveThroughputBest}, 0, 0},
		{"log zero cost is best", curve{curveLog, curveTokensPerMergeWorst, curveTokensPerMergeBest}, 0, 100},
		{"log cost mid", curve{curveLog, 1000, 10}, 100, 50},
		{"degenerate curve scores zero", curve{curveLinear, 1, 1}, 1, 0},
	}
	for _, c := range cases {
		if got := c.c.score(c.v); got != c.want {
			t.Errorf("%s: score(%v) = %d, want %d", c.name, c.v, got, c.want)
		}
	}
}

// TestEveryCriterionHasASpec guards the catalogue: every name an axis lists
// resolves to a spec with a label, a positive weight, a formatter, a non-flat
// curve and the two explainer sentences. A missing entry would render as an
// empty row with weight 0 and no explanation.
func TestEveryCriterionHasASpec(t *testing.T) {
	seen := map[string]string{}
	for _, axis := range QuadrantAxisOrder {
		for _, name := range quadrantCriteriaOrder[axis] {
			if prev, dup := seen[name]; dup {
				t.Errorf("criterion %q listed on both %s and %s — it would count twice", name, prev, axis)
			}
			seen[name] = axis
			spec, ok := criterionSpecs[name]
			if !ok {
				t.Errorf("criterion %q on %s has no spec", name, axis)
				continue
			}
			if spec.name != name || spec.label == "" || spec.weight <= 0 || spec.format == nil || spec.good == "" {
				t.Errorf("criterion %q spec is incomplete: %+v", name, spec)
			}
			if spec.curve.best == spec.curve.worst {
				t.Errorf("criterion %q has a flat curve", name)
			}
			if strings.TrimSpace(spec.howText()) == "" {
				t.Errorf("criterion %q has no 'how it is scored' sentence", name)
			}
		}
	}
	for name := range criterionSpecs {
		if _, listed := seen[name]; !listed {
			t.Errorf("spec %q is not listed on any axis", name)
		}
	}
}

// TestCriteriaTableGolden pins each axis's criteria list and weights. Any
// change here is a change to what the quadrant MEANS and must be deliberate —
// and mirrored in docs/fleet-health.md.
func TestCriteriaTableGolden(t *testing.T) {
	want := map[string][]string{
		AxisTrust:        {"revert_rate:2", "false_approve_rate:2", "autonomy:2", "delegation:1", "merge_acceptance:1", "evidence_coverage:1", "boundary_block_rate:1", "breadth:1"},
		AxisProductivity: {"merge_throughput:3", "relay_7d:1", "contributor_engagement:1", "work_source:1", "human_blocked:1", "sla_violations:1", "time_to_merge:2"},
		AxisEfficiency:   {"tokens_per_merge:3", "tokens_per_day:1", "rework_rate:1", "output_per_agent:1", "agent_idle_ratio:1", "cost_per_merge:2"},
		AxisSatisfaction: {"human_override_rate:2", "sla_kept:1", "hold_lift_latency:1", "contributor_return:1", "pulse:2"},
	}
	for _, axis := range QuadrantAxisOrder {
		var got []string
		for _, c := range criteriaFor(axis, quadrantInputs{}) {
			got = append(got, c.spec.name+":"+fmtCount(c.spec.weight))
		}
		if strings.Join(got, ",") != strings.Join(want[axis], ",") {
			t.Errorf("%s criteria table changed:\n got %v\nwant %v", axis, got, want[axis])
		}
		// The builder must list the catalogue's order exactly once each, or
		// the fleet explainer (which walks quadrantCriteriaOrder) and the
		// per-hive explainer (which walks the builder) would disagree.
		var order []string
		for _, c := range criteriaFor(axis, quadrantInputs{}) {
			order = append(order, c.spec.name)
		}
		if strings.Join(order, ",") != strings.Join(quadrantCriteriaOrder[axis], ",") {
			t.Errorf("%s builder order %v != quadrantCriteriaOrder %v", axis, order, quadrantCriteriaOrder[axis])
		}
	}
}

// --- Unmeasured is not zero ------------------------------------------------------

// TestUnmeasuredCriterionExcludedFromAxis is the single most important
// invariant in the package: a criterion with no input is listed as unmeasured
// and contributes NOTHING to the axis, rather than a zero that drags it down.
func TestUnmeasuredCriterionExcludedFromAxis(t *testing.T) {
	// Only autonomy is measured on Trust here: L6 scores 100, so the axis
	// must be exactly 100. If any unmeasured criterion leaked in as a zero
	// the weighted mean would fall well below.
	a := scoreAxis(AxisTrust, trustCriteria(quadrantInputs{acmmLevel: 6}))
	if !a.Scored || a.Score != 100 {
		t.Fatalf("Trust with only L6 measured = %d (scored=%v), want 100 — an unmeasured criterion is being scored as zero", a.Score, a.Scored)
	}
	if a.Measured != 1 || a.Unmeasured != 7 {
		t.Errorf("measured/unmeasured = %d/%d, want 1/7", a.Measured, a.Unmeasured)
	}
	for _, c := range a.Criteria {
		if c.Name == critAutonomy {
			continue
		}
		if c.Measured || c.Reason == "" || c.Score != 0 || c.Display != "" {
			t.Errorf("criterion %s should be listed unmeasured with a reason, got %+v", c.Name, c)
		}
	}
}

// TestNoCriteriaMeansUnscoredWithReason: an axis with nothing measured is
// unscored (collapsed spoke), never 0, and says what is missing.
func TestNoCriteriaMeansUnscoredWithReason(t *testing.T) {
	qs := ScoreFleet([]quadrantInputs{{}})
	for _, a := range qs[0].Axes {
		if a.Axis == AxisProductivity {
			// work_source is always measured (empty = human-filed issues), so
			// Productivity scores even on an empty input.
			continue
		}
		if a.Scored {
			t.Errorf("%s scored %d on an empty input", a.Axis, a.Score)
		}
		if a.Reason == "" || !strings.Contains(a.Reason, "Not measured yet") {
			t.Errorf("%s unscored without a 'Not measured yet' reason: %q", a.Axis, a.Reason)
		}
		if !strings.Contains(a.Summary, "not measured yet") {
			t.Errorf("%s summary should say not measured yet: %q", a.Axis, a.Summary)
		}
	}
}

// TestMeasuredZeroDiffersFromNil: a reported zero scores at the bottom of its
// curve; a nil is unmeasured. The two must never collapse into one.
func TestMeasuredZeroDiffersFromNil(t *testing.T) {
	withZero := scoreAxis(AxisProductivity, productivityCriteria(quadrantInputs{tasksCompleted7d: iptr(0)}))
	withNil := scoreAxis(AxisProductivity, productivityCriteria(quadrantInputs{}))
	z := criterionOf(t, withZero, critRelay)
	n := criterionOf(t, withNil, critRelay)
	if !z.Measured || z.Score != 0 {
		t.Errorf("relay 0 should be measured and score 0, got %+v", z)
	}
	if n.Measured {
		t.Errorf("relay nil should be unmeasured, got %+v", n)
	}
	if withZero.Measured == withNil.Measured {
		t.Error("a measured zero and a nil changed the measured count identically")
	}
}

// --- Satisfaction root cause ------------------------------------------------------

// TestSatisfactionScoresFromEvidenceAndHiveState is the fix for the fleet-wide
// "SATIS —": a single hive with ≥10 verdicts (or a 7-day hive-state trend)
// scores the axis, with no population floor to kill it, and the unmeasured
// criteria are listed so the reader sees what would feed them.
func TestSatisfactionScoresFromEvidenceAndHiveState(t *testing.T) {
	a := scoreAxis(AxisSatisfaction, satisfactionCriteria(quadrantInputs{evidence: evidenceWith(20, 0, 1, 20, 0)}))
	if !a.Scored {
		t.Fatalf("satisfaction unscored with 20 verdicts: %s", a.Reason)
	}
	o := criterionOf(t, a, critOverride)
	if !o.Measured || o.Display != "5.0%" || o.Score != 50 {
		t.Errorf("override rate 1/20 → %+v, want measured 5.0%% scoring 50", o)
	}
	if a.Unmeasured != 4 {
		t.Errorf("unmeasured = %d, want 4 (sla_kept, hold-lift, return, pulse)", a.Unmeasured)
	}
	p := criterionOf(t, a, critPulse)
	if p.Measured || !strings.Contains(p.Reason, "thumbs") {
		t.Errorf("pulse must be unmeasured and say what would feed it, got %+v", p)
	}

	// Hive state alone also scores the axis.
	hs := &HeartbeatEvidence{HiveState: &hivestate.Trends{Last7d: hivestate.Window{Samples: 24, SLAViolationRate: 0.25}}}
	b := scoreAxis(AxisSatisfaction, satisfactionCriteria(quadrantInputs{evidence: hs}))
	if !b.Scored || b.Score != 75 {
		t.Errorf("SLA kept 75%% alone → score %d (scored=%v), want 75", b.Score, b.Scored)
	}

	// Below the verdict floor the override criterion is unmeasured, not 0.
	few := scoreAxis(AxisSatisfaction, satisfactionCriteria(quadrantInputs{evidence: evidenceWith(5, 0, 5, 5, 0)}))
	if c := criterionOf(t, few, critOverride); c.Measured {
		t.Errorf("5 verdicts must leave override unmeasured, got %+v", c)
	}
	if few.Scored {
		t.Error("nothing measured must leave satisfaction unscored")
	}
}

// --- Trust fed from evidence -------------------------------------------------------

func TestTrustUsesEvidence(t *testing.T) {
	e := evidenceWith(40, 2, 0, 50, 1)
	e.WriteCounters = map[string]EvidenceWriteCount{"L3": {Attempted: 30, Blocked: 3}, "L4": {Attempted: 20, Blocked: 2}}
	in := quadrantInputs{acmmLevel: 4, prsMerged90d: iptr(200), prsRejected90d: iptr(20), evidence: e}
	in.mergeAcceptance = fptr(200.0 / 220)
	a := scoreAxis(AxisTrust, trustCriteria(in))
	if !a.Scored {
		t.Fatal(a.Reason)
	}
	checks := map[string]struct {
		display string
		score   int
	}{
		critRevert:       {"2.0%", 80},
		critFalseApprove: {"5.0%", 50},
		critCoverage:     {"25.0%", 25},
		critBoundary:     {"10.0%", 50},
		critAutonomy:     {"L4", 60},
	}
	for name, want := range checks {
		c := criterionOf(t, a, name)
		if !c.Measured || c.Display != want.display || c.Score != want.score {
			t.Errorf("%s = %+v, want %s scoring %d", name, c, want.display, want.score)
		}
	}
	if c := criterionOf(t, a, critDelegation); c.Measured {
		t.Error("delegation must be unmeasured when automerge_self_authored is nil")
	}
	// Weighted mean: revert 80×2 + false-approve 50×2 + autonomy 60×2 +
	// acceptance (90.9% → 82)×1 + coverage 25×1 + boundary 50×1 over weight 9.
	if want := (80*2 + 50*2 + 60*2 + 82 + 25 + 50 + 4) / 9; a.Score != want {
		t.Errorf("Trust = %d, want weighted mean %d", a.Score, want)
	}
}

// --- Efficiency does not reward idleness -------------------------------------------

func TestEfficiencyBurnRateOnlyForShippingHives(t *testing.T) {
	idle := scoreAxis(AxisEfficiency, efficiencyCriteria(quadrantInputs{tokensPerDay: fptr(1000), prsMerged90d: iptr(0), prsRejected90d: iptr(0)}))
	if c := criterionOf(t, idle, critTokensPerDay); c.Measured {
		t.Errorf("an idle hive's burn rate must be unmeasured, got %+v", c)
	}
	busy := scoreAxis(AxisEfficiency, efficiencyCriteria(quadrantInputs{tokensPerDay: fptr(1_000_000), prsMerged90d: iptr(30), prsRejected90d: iptr(0), agentCount: 3}))
	if c := criterionOf(t, busy, critTokensPerDay); !c.Measured || c.Score != 50 {
		t.Errorf("1M tokens/day on a shipping hive → %+v, want measured scoring 50", c)
	}
	// 1M/day × 90 / 30 merges = 3M per merge: log midpoint of 100k..20M is
	// ~1.41M, so 3M lands a little below 50.
	if c := criterionOf(t, busy, critTokensPerPR); !c.Measured || c.Display != "3.0M tokens" || c.Score < 30 || c.Score > 45 {
		t.Errorf("tokens per merge → %+v, want 3.0M tokens scoring 30–45", c)
	}
	if c := criterionOf(t, busy, critCostPerMerge); c.Measured || !strings.Contains(c.Reason, "launch-cost") {
		t.Errorf("cost per merge must be listed unmeasured naming the launch-cost API, got %+v", c)
	}
}

// --- Productivity scale ---------------------------------------------------------------

// TestProductivityUsesHiveSpecificThroughput: the hive's own RepoActivity
// merges beat the org-wide collector count, and a busy hive actually scores
// high — the fix for "16/100 on a fleet with thousands of merges".
func TestProductivityUsesHiveSpecificThroughput(t *testing.T) {
	own := scoreAxis(AxisProductivity, productivityCriteria(quadrantInputs{mergesPerWeek: fptr(12), activeRepos: 3, prsMerged90d: iptr(3746)}))
	c := criterionOf(t, own, critThroughput)
	if !c.Measured || c.Display != "4.0 merges/repo/week" || c.Score < 75 || c.Score > 85 {
		t.Errorf("throughput from RepoActivity → %+v, want 4.0/repo/week scoring ~80", c)
	}
	fallback := scoreAxis(AxisProductivity, productivityCriteria(quadrantInputs{prsMerged90d: iptr(130), reposEnrolled: 2}))
	f := criterionOf(t, fallback, critThroughput)
	if !f.Measured || !strings.HasPrefix(f.Display, "5.1 merges") || f.Score < 80 {
		t.Errorf("fallback throughput → %+v, want ~5.1/repo/week from the org count", f)
	}
	if c := criterionOf(t, fallback, critTimeToMerge); c.Measured || c.Reason == "" {
		t.Errorf("time to merge must be listed unmeasured, got %+v", c)
	}
}

// --- Fleet median -------------------------------------------------------------------

func TestFleetMedianNotMean(t *testing.T) {
	// Nine hives at L1 (Trust 0) and one at L6 (Trust 100): the mean is 10,
	// the median 0. The fleet must report the median and n.
	in := fleetOf(10, func(i int, in *quadrantInputs) {
		in.acmmLevel = 1
		if i == 9 {
			in.acmmLevel = 6
		}
	})
	fleet := FleetMedian(ScoreFleet(in))
	if fleet.Aggregate != QuadrantFleetAggregate || fleet.N != 10 {
		t.Errorf("fleet aggregate/n = %q/%d, want median/10", fleet.Aggregate, fleet.N)
	}
	tr, _ := axisOf(fleet, AxisTrust)
	if !tr.Scored || tr.Score != 0 || tr.N != 10 {
		t.Errorf("fleet Trust = %+v, want median 0 over n=10", tr)
	}
	au := criterionOf(t, tr, critAutonomy)
	if !au.Measured || au.N != 10 || au.Display != "L1" {
		t.Errorf("fleet autonomy criterion = %+v, want median L1 over 10 hives", au)
	}
	if c := criterionOf(t, tr, critRevert); c.Measured || c.Reason == "" {
		t.Errorf("a criterion no hive measured must be unmeasured with a reason on the fleet, got %+v", c)
	}
	if !strings.Contains(tr.Summary, "median of 10 hives") {
		t.Errorf("fleet summary = %q", tr.Summary)
	}
	sat, _ := axisOf(fleet, AxisSatisfaction)
	if sat.Scored || sat.Reason == "" {
		t.Errorf("fleet satisfaction with no reporting hive = %+v, want unscored with reason", sat)
	}
}

func TestFleetMedianIgnoresUnscored(t *testing.T) {
	in := fleetOf(6, func(i int, in *quadrantInputs) {
		if i < 2 {
			in.evidence = evidenceWith(20, 0, 2, 20, 0) // 10% override → 0
		}
	})
	fleet := FleetMedian(ScoreFleet(in))
	sat, _ := axisOf(fleet, AxisSatisfaction)
	if !sat.Scored || sat.N != 2 {
		t.Errorf("fleet satisfaction should be the median of the 2 scoring hives, got %+v", sat)
	}
}

// --- Composite, delta, nudges -------------------------------------------------------------

func TestCompositeExcludesUnscoredAxes(t *testing.T) {
	qs := ScoreFleet(fleetOf(10, func(i int, in *quadrantInputs) { in.prsMerged90d = iptr(i) }))
	for i, q := range qs {
		if q.ScoredAxes == 0 {
			t.Fatalf("hive %d scored nothing", i)
		}
		sum := 0
		for _, a := range q.Axes {
			if a.Scored {
				sum += a.Score
			}
		}
		want := sum / q.ScoredAxes
		if diff := q.Composite - want; diff > 1 || diff < -1 {
			t.Errorf("hive %d: composite %d not the mean of %d scored axes (~%d)", i, q.Composite, q.ScoredAxes, want)
		}
	}
}

func TestDeltaAgainstFleetMedian(t *testing.T) {
	in := fleetOf(5, func(i int, in *quadrantInputs) { in.acmmLevel = i + 1 }) // Trust 0,20,40,60,80
	qs := ScoreFleet(in)
	for i, q := range qs {
		tr, _ := axisOf(q, AxisTrust)
		if want := tr.Score - 40; tr.Delta != want {
			t.Errorf("hive %d Trust delta = %d, want %d (median 40)", i, tr.Delta, want)
		}
	}
}

// TestNudgeOnlyWhenWeak keeps the instrument from nagging: a nudge implies a
// measured criterion scored at or below 50, and it is that criterion's nudge.
func TestNudgeOnlyWhenWeak(t *testing.T) {
	inputs := fleetOf(12, func(i int, in *quadrantInputs) {
		in.prsMerged90d = iptr(i * 3)
		in.workSource = "issues"
	})
	for i, q := range ScoreFleet(inputs) {
		for _, a := range q.Axes {
			if !a.Scored || a.Nudge == "" {
				continue
			}
			weak := false
			for _, c := range criteriaFor(a.Axis, inputs[i]) {
				if c.measured && c.nudge == a.Nudge && c.score() <= 50 {
					weak = true
				}
			}
			if !weak {
				t.Errorf("hive %d: axis %s carries nudge %q but no measured criterion at or below 50 owns it", i, a.Axis, a.Nudge)
			}
		}
	}
}

func TestNudgeAbsentWhenEverythingIsStrong(t *testing.T) {
	in := quadrantInputs{mergesPerWeek: fptr(30), activeRepos: 2, tasksCompleted7d: iptr(40),
		contributorCount: 4, activeContributors: 4, workSource: "linear", holdTotal: iptr(0), slaViolations: iptr(0)}
	a := scoreAxis(AxisProductivity, productivityCriteria(in))
	if a.Nudge != "" || a.Score < 95 {
		t.Errorf("a hive at the top of every productivity curve got score %d nudge %q", a.Score, a.Nudge)
	}
}

// --- Summaries & wire shape ----------------------------------------------------------------

func TestAxisSummaryReadsPlainly(t *testing.T) {
	e := evidenceWith(40, 0, 0, 50, 0)
	a := scoreAxis(AxisTrust, trustCriteria(quadrantInputs{acmmLevel: 3, prsMerged90d: iptr(800), evidence: e}))
	if !strings.HasPrefix(a.Summary, "Trust "+fmtCount(float64(a.Score))+": ") {
		t.Errorf("summary should open with the axis and score: %q", a.Summary)
	}
	if !strings.Contains(a.Summary, "0.0% of merges reverted but 6.2% of merges carry evidence") {
		t.Errorf("summary should quote the strongest and weakest facts: %q", a.Summary)
	}
	if !strings.Contains(a.Summary, "4 of 8 criteria not measured yet") {
		t.Errorf("summary should count unmeasured criteria: %q", a.Summary)
	}
}

// TestQuadrantJSONBackwardCompatible: the pre-existing keys the browser and
// the iOS widget read are still emitted with the same names and types.
func TestQuadrantJSONBackwardCompatible(t *testing.T) {
	qs := ScoreFleet(fleetOf(5, nil))
	b, err := json.Marshal(FleetMedian(qs))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"axes", "composite", "scored_axes", "aggregate", "n"} {
		if _, ok := m[k]; !ok {
			t.Errorf("fleet quadrant JSON lacks %q", k)
		}
	}
	var axes []map[string]json.RawMessage
	if err := json.Unmarshal(m["axes"], &axes); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"axis", "score", "scored", "delta", "criteria", "measured", "unmeasured", "summary"} {
		if _, ok := axes[0][k]; !ok {
			t.Errorf("axis JSON lacks %q", k)
		}
	}
}

func TestAxisOrderStable(t *testing.T) {
	for _, q := range ScoreFleet(fleetOf(3, nil)) {
		for i, a := range q.Axes {
			if a.Axis != QuadrantAxisOrder[i] {
				t.Fatalf("axis %d is %s, want %s", i, a.Axis, QuadrantAxisOrder[i])
			}
		}
	}
}

// TestGovernorCadenceIsNotATrustSignal: the governor's modes are WORKLOAD
// CADENCE and must never be folded into Trust.
func TestGovernorCadenceIsNotATrustSignal(t *testing.T) {
	base := scoreAxis(AxisTrust, trustCriteria(quadrantInputsFor(RegistryEntry{Org: "o", AIAuthor: "a", ACMMLevel: 3}, time.Now())))
	for _, mode := range []string{"SURGE", "BUSY", "QUIET", "IDLE"} {
		in := quadrantInputsFor(RegistryEntry{Org: "o", AIAuthor: "a", ACMMLevel: 3, GovernorMode: mode}, time.Now())
		if a := scoreAxis(AxisTrust, trustCriteria(in)); a.Score != base.Score || a.Measured != base.Measured {
			t.Errorf("governor mode %q changed Trust", mode)
		}
	}
}

// TestInputsFromRegistryEntry covers the new attach paths: RepoActivity →
// merges per week, and the hive-state token trend as the burn-rate fallback.
func TestInputsFromRegistryEntry(t *testing.T) {
	e := RegistryEntry{
		ACMMLevel:                    3,
		RepoActivity:                 []RepoActivityWire{{Repo: "o/a", Merges: ActivityStatWire{Count: 14}}, {Repo: "o/b", Merges: ActivityStatWire{Count: 0}}},
		RepoActivityCountWindowHours: 14 * 24,
		Evidence:                     &HeartbeatEvidence{HiveState: &hivestate.Trends{Last7d: hivestate.Window{Samples: 48, TokensPerDay: 250000}}},
	}
	in := quadrantInputsFor(e, time.Now())
	if in.mergesPerWeek == nil || *in.mergesPerWeek != 7 || in.activeRepos != 2 {
		t.Errorf("mergesPerWeek/activeRepos = %v/%d, want 7/2", in.mergesPerWeek, in.activeRepos)
	}
	if in.tokensPerDay == nil || *in.tokensPerDay != 250000 {
		t.Errorf("tokensPerDay fallback = %v, want 250000 from hive state", in.tokensPerDay)
	}
	if _, _, ok := repoMergesPerWeek(RegistryEntry{}); ok {
		t.Error("no RepoActivity must be unmeasured")
	}
}

func TestEmptyFleetDoesNotPanic(t *testing.T) {
	if got := ScoreFleet(nil); len(got) != 0 {
		t.Errorf("expected no scores for an empty fleet, got %d", len(got))
	}
	fleet := FleetMedian(nil)
	if fleet.Composite != 0 || fleet.ScoredAxes != 0 || fleet.N != 0 {
		t.Errorf("empty fleet median should be zero-valued, got %+v", fleet)
	}
	if len(fleet.Axes) != len(QuadrantAxisOrder) {
		t.Errorf("fleet must still emit all four axes so the renderer has a stable shape, got %d", len(fleet.Axes))
	}
}
