package hub

import (
	"strings"
	"testing"
)

func TestQuadrantNudgesFollowAbsoluteWeakness(t *testing.T) {
	for _, tc := range []struct {
		rate float64
		want bool
	}{{0, false}, {.1, false}, {.2, true}, {.25, true}} {
		q := ScoreFleet([]quadrantInputs{{agentIdleRatio: fptr(tc.rate * 4)}})[0]
		a, _ := axisOf(q, AxisEfficiency)
		if (a.Nudge != "") != tc.want {
			t.Errorf("idle ratio %g: %+v", tc.rate*4, a)
		}
	}
	in := quadrantInputs{reposEnrolled: 1, prsMerged90d: iptr(0), budgetExhausted: bptr(true)}
	c := findCriterion(t, productivityCriteria(in), "merges_per_repo_week")
	if !strings.Contains(c.nudge, "Budget exhausted") {
		t.Fatal(c.nudge)
	}
	in.budgetExhausted = nil
	c = findCriterion(t, productivityCriteria(in), "merges_per_repo_week")
	if !strings.Contains(c.nudge, "No recorded merges") {
		t.Fatal(c.nudge)
	}
}

func TestQuadrantCriteriaBelongToOutcomeAxes(t *testing.T) {
	in := quadrantInputs{slaViolations: iptr(2), evidence: &HeartbeatEvidence{
		Merged90d: 20, Verdicts90d: 20, Reverted14d90d: 1, FalseApprove90d: 1, FalseBlock90d: 1,
	}}
	findCriterion(t, trustCriteria(in), "revert_rate")
	findCriterion(t, trustCriteria(in), "false_approve_rate")
	findCriterion(t, satisfactionCriteria(in), "human_override_rate")
	findCriterion(t, satisfactionCriteria(in), "sla_violations")
	if len(efficiencyCriteria(in)) != 0 || len(productivityCriteria(in)) != 0 {
		t.Fatal("outcome criteria leaked into cost/throughput")
	}
	for _, axis := range QuadrantAxisOrder {
		if unscoredReason(axis) == "" || unscoredReason(axis) == "Not enough data" {
			t.Fatalf("missing reason for %s", axis)
		}
	}
}
