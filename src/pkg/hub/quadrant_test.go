package hub

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
)

func iptr(v int) *int         { return &v }
func fptr(v float64) *float64 { return &v }
func bptr(v bool) *bool       { return &v }
func axisOf(q Quadrant, axis string) (AxisScore, bool) {
	for _, a := range q.Axes {
		if a.Axis == axis {
			return a, true
		}
	}
	return AxisScore{}, false
}
func findCriterion(t *testing.T, cs []subCriterion, name string) subCriterion {
	t.Helper()
	for _, c := range cs {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("criterion %s missing: %+v", name, cs)
	return subCriterion{}
}

func TestQuadrantCriteriaGolden(t *testing.T) {
	in := quadrantInputs{
		reposEnrolled: 2, repoMerges14d: iptr(20), prsMerged90d: iptr(40),
		tokensPerDay: fptr(100000), agentIdleRatio: fptr(0.25), slaViolations: iptr(2),
		evidence: &HeartbeatEvidence{Merged90d: 20, Reverted14d90d: 1, Verdicts90d: 20, FalseApprove90d: 2, FalseBlock90d: 1,
			WriteCounters: map[string]EvidenceWriteCount{"L3": {Attempted: 20, Blocked: 1}}},
	}
	q := ScoreFleet([]quadrantInputs{in})[0]
	want := map[string][]int{
		AxisTrust:        {80, 60, 50, 80},
		AxisProductivity: {100},    // 20 merges / 2 weeks / 2 repos = 5 per repo per week
		AxisEfficiency:   {31, 75}, // 70k tokens/merge: 1-log1p(70k)/log1p(10M)
		AxisSatisfaction: {80, 80},
	}
	for _, a := range q.Axes {
		var got []int
		for _, c := range a.Criteria {
			if c.Measured {
				got = append(got, *c.Score)
			}
		}
		if !reflect.DeepEqual(got, want[a.Axis]) {
			t.Errorf("%s sub-scores = %v, want %v", a.Axis, got, want[a.Axis])
		}
		if !a.Scored || a.N != 1 || a.Delta != 0 {
			t.Errorf("single-hive axis: %+v", a)
		}
	}
	// Existing JSON fields survive; new missing measurements serialize as null.
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"axes"`, `"composite"`, `"scored_axes"`, `"delta"`, `"criteria"`, `"value":null`, `"score":null`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("missing wire contract %s", key)
		}
	}
}

func TestQuadrantCurveEdges(t *testing.T) {
	for _, axis := range QuadrantAxisOrder {
		for _, d := range criterionDefinitions(axis) {
			if got := criterionScore(d, d.low); got != 0 {
				t.Errorf("%s zero edge = %d", d.name, got)
			}
			if got := criterionScore(d, d.high); got != 100 {
				t.Errorf("%s full edge = %d", d.name, got)
			}
			previous := criterionScore(d, 0)
			for i := 1; i <= 100; i++ {
				value := float64(i) * math.Max(d.low, d.high) / 50
				got := criterionScore(d, value)
				if got < 0 || got > 100 || (d.high > d.low && got < previous) || (d.high < d.low && got > previous) {
					t.Errorf("%s non-monotonic at %g: %d → %d", d.name, value, previous, got)
				}
				previous = got
			}
		}
	}
}

func TestQuadrantMissingExcludedAndZeroMeasured(t *testing.T) {
	missing := ScoreFleet([]quadrantInputs{{}})[0]
	if missing.ScoredAxes != 0 {
		t.Fatalf("absent evidence scored: %+v", missing)
	}
	for _, a := range missing.Axes {
		if a.Reason == "" || len(a.Criteria) == 0 {
			t.Errorf("no explanation: %+v", a)
		}
		for _, c := range a.Criteria {
			if c.Measured || c.Value != nil || c.Score != nil {
				t.Errorf("gap invented value: %+v", c)
			}
		}
	}
	q := ScoreFleet([]quadrantInputs{{reposEnrolled: 1, repoMerges14d: iptr(0)}})[0]
	a, _ := axisOf(q, AxisProductivity)
	if !a.Scored || a.Score != 0 || q.ScoredAxes != 1 || q.Composite != 0 {
		t.Fatalf("zero vs missing: %+v", q)
	}
	// Only idle ratio measured: missing token cost must not halve the axis.
	q = ScoreFleet([]quadrantInputs{{agentIdleRatio: fptr(0)}})[0]
	a, _ = axisOf(q, AxisEfficiency)
	if !a.Scored || a.Score != 100 || q.Composite != 100 {
		t.Fatalf("gap counted as zero: %+v", q)
	}
}

func TestQuadrantIndependentOfPopulationAndFloors(t *testing.T) {
	in := quadrantInputs{evidence: &HeartbeatEvidence{Verdicts90d: 10, FalseBlock90d: 1}}
	one := ScoreFleet([]quadrantInputs{in})[0]
	fleet := ScoreFleet([]quadrantInputs{in, {}, {}, {evidence: &HeartbeatEvidence{Verdicts90d: 100}}})
	a, _ := axisOf(one, AxisSatisfaction)
	b, _ := axisOf(fleet[0], AxisSatisfaction)
	if !a.Scored || a.Score != 60 || a.Score != b.Score || b.N != 2 {
		t.Fatalf("population floor/rank survived: %+v vs %+v", a, b)
	}
	in.evidence.Verdicts90d = 9
	a, _ = axisOf(ScoreFleet([]quadrantInputs{in})[0], AxisSatisfaction)
	if a.Scored {
		t.Fatal("sub-sample rate scored")
	}
}

func TestFleetMedianIgnoresGapsAndOutliers(t *testing.T) {
	qs := []Quadrant{
		{Axes: []AxisScore{{Axis: AxisTrust, Scored: true, Score: 10}}},
		{Axes: []AxisScore{{Axis: AxisTrust, Scored: true, Score: 20}}},
		{Axes: []AxisScore{{Axis: AxisTrust, Scored: true, Score: 100}}},
		{Axes: []AxisScore{{Axis: AxisTrust}}},
	}
	q := FleetAverage(qs)
	a, _ := axisOf(q, AxisTrust)
	if a.Score != 20 || a.N != 3 || q.Composite != 20 || q.ScoredAxes != 1 {
		t.Fatalf("not a scored-only median: %+v", q)
	}
	if got := FleetAverage(nil); got.ScoredAxes != 0 || len(got.Axes) != 4 {
		t.Fatalf("empty: %+v", got)
	}
	if len(ScoreFleet(nil)) != 0 {
		t.Fatal("empty fleet scored")
	}
}

func TestTrustEvidenceFloorsAndCoverage(t *testing.T) {
	in := quadrantInputs{evidence: &HeartbeatEvidence{Merged90d: 9, Verdicts90d: 9,
		WriteCounters: map[string]EvidenceWriteCount{"L3": {Attempted: 9}}}}
	if len(trustCriteria(in)) != 0 {
		t.Fatal("insufficient samples scored")
	}
	in.prsMerged90d = iptr(20)
	c := findCriterion(t, trustCriteria(in), "evidence_coverage")
	if c.value != .45 {
		t.Fatalf("coverage: %+v", c)
	}
	in.evidence.Merged90d, in.evidence.Verdicts90d = 10, 10
	in.evidence.WriteCounters["L4"] = EvidenceWriteCount{Attempted: 1}
	for _, name := range []string{"revert_rate", "false_approve_rate", "boundary_blocked_rate"} {
		if c := findCriterion(t, trustCriteria(in), name); c.value != 0 {
			t.Fatalf("clean zero lost: %+v", c)
		}
	}
	in.evidence.Merged90d = 30
	if c := findCriterion(t, trustCriteria(in), "evidence_coverage"); c.value != 1 {
		t.Fatal("scope mismatch must not exceed full coverage")
	}
}

func TestFleetCriterionMedians(t *testing.T) {
	qs := ScoreFleet([]quadrantInputs{{slaViolations: iptr(0)}, {slaViolations: iptr(2)}, {}})
	a, _ := axisOf(FleetAverage(qs), AxisSatisfaction)
	if a.N != 2 || a.Score != 90 {
		t.Fatalf("fleet axis: %+v", a)
	}
	for _, c := range a.Criteria {
		if c.Name == "sla_violations" && (!c.Measured || c.Value == nil || *c.Value != 1 || *c.Score != 90) {
			t.Fatalf("median criterion: %+v", c)
		}
		if c.Name == "human_override_rate" && c.Measured {
			t.Fatal("missing fleet rate invented")
		}
	}
}

func TestQuadrantThroughputAndCostWindows(t *testing.T) {
	in := quadrantInputs{reposEnrolled: 2, repoMerges14d: iptr(20), repoMergeDays: 7, prsMerged90d: iptr(999), tokensPerDay: fptr(200000)}
	if c := findCriterion(t, productivityCriteria(in), "merges_per_repo_week"); c.value != 10 {
		t.Fatalf("repo count window: %+v", c)
	}
	if c := findCriterion(t, efficiencyCriteria(in), "tokens_per_pr"); c.value != 70000 {
		t.Fatalf("cost must divide daily rates: %+v", c)
	}
	in.repoMerges14d = iptr(0)
	if c := findCriterion(t, productivityCriteria(in), "merges_per_repo_week"); c.value != 0 {
		t.Fatal("measured zero fell back to org")
	}
	if len(efficiencyCriteria(in)) != 0 {
		t.Fatal("cost with zero merges")
	}
	in.repoMerges14d = nil
	in.prsMerged90d = iptr(90)
	if c := findCriterion(t, productivityCriteria(in), "merges_per_repo_week"); c.value != 3.5 {
		t.Fatalf("org fallback rate: %+v", c)
	}
	in.prsMerged90d = iptr(4)
	if len(efficiencyCriteria(in)) != 0 {
		t.Fatal("cost below sample floor")
	}
}

func TestQuadrantAttachActivityAndTrends(t *testing.T) {
	now := time.Now()
	e := RegistryEntry{Repos: []string{"o/a", "o/b"}, AgentCount: 1,
		PRsMerged90d: iptr(900), RepoActivityCountWindowHours: 168, RepoActivityCollectedAt: now,
		RepoActivity: []RepoActivityWire{{Repo: "o/a", Merges: ActivityStatWire{Count: 10}}, {Repo: "O/A", Merges: ActivityStatWire{Count: 10}}, {Repo: "other/repo", Merges: ActivityStatWire{Count: 100}}},
		Evidence:     &HeartbeatEvidence{HiveState: &hivestate.Trends{Last7d: hivestate.Window{Samples: 2, CoverageHours: 2, AgentIdleRatio: .25}}},
	}
	in := quadrantInputsFor(e, now)
	if in.repoMerges14d == nil || *in.repoMerges14d != 10 || in.repoMergeDays != 7 || in.agentIdleRatio == nil || *in.agentIdleRatio != .25 {
		t.Fatalf("extraction: %+v", in)
	}
	e.RepoActivityCollectedAt = now.Add(-25 * time.Hour)
	if quadrantInputsFor(e, now).repoMerges14d != nil {
		t.Fatal("stale activity used")
	}
	e.Evidence.HiveState.Last7d.AgentIdleRatio = 0
	if quadrantInputsFor(e, now).agentIdleRatio != nil {
		t.Fatal("old spoke's zero without agent observations was scored")
	}
	e.Evidence.HiveState.Last7d.AgentSamples = 10
	if p := quadrantInputsFor(e, now).agentIdleRatio; p == nil || *p != 0 {
		t.Fatal("observed all-running agents must score real zero idle")
	}
	w := sanitizeHiveStateWindow(e.Evidence.HiveState.Last7d, 168)
	if w.AgentSamples != 10 {
		t.Fatal("heartbeat sanitizer dropped idle denominator")
	}
	e.Evidence.HiveState.Last7d.CoverageHours = .5
	if quadrantInputsFor(e, now).agentIdleRatio != nil {
		t.Fatal("under-covered idle ratio used")
	}
	for _, tc := range []struct {
		role        string
		admin, want bool
	}{{"", false, false}, {"member", false, true}, {"", true, true}} {
		rows := []MyHiveEntry{{RegistryEntry: e, Role: tc.role}}
		attachQuadrants(rows, tc.admin, now)
		if (rows[0].Quadrant != nil) != tc.want {
			t.Errorf("visibility: %+v", tc)
		}
	}
}

func TestQuadrantOrderAndInvalidInputs(t *testing.T) {
	want := []string{AxisTrust, AxisProductivity, AxisSatisfaction, AxisEfficiency}
	if !reflect.DeepEqual(QuadrantAxisOrder, want) {
		t.Fatal("axis order changed")
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), -1} {
		q := ScoreFleet([]quadrantInputs{{agentIdleRatio: &v}})[0]
		if q.ScoredAxes != 0 {
			t.Fatalf("invalid measurement scored: %v", v)
		}
	}
	if criteriaFor("unknown", quadrantInputs{}) != nil {
		t.Fatal("unknown axis")
	}
}
