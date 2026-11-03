package hub

import (
	"strings"
	"testing"
)

// The nudge strings on quadrant sub-criteria are the actionable half of the
// radar: they are what the hover tells an operator to DO. Every branch below
// was previously untested, so a wording regression (or a nudge silently
// disappearing) would ship unseen. These tests pin the conditions each nudge
// fires under, and — just as deliberately — the conditions it must stay
// silent under.

func bptr(v bool) *bool { return &v }

// findCriterion returns the MEASURED criterion by name; an unmeasured one is a
// test failure because every test here is about a value that was reported.
func findCriterion(t *testing.T, cs []subCriterion, name string) subCriterion {
	t.Helper()
	for _, c := range cs {
		if c.spec.name == name {
			if !c.measured {
				t.Fatalf("criterion %q is unmeasured (%s)", name, c.reason)
			}
			return c
		}
	}
	t.Fatalf("criterion %q not found", name)
	return subCriterion{}
}

// assertUnmeasured checks a criterion is listed but carries no measurement.
func assertUnmeasured(t *testing.T, cs []subCriterion, name string) {
	t.Helper()
	for _, c := range cs {
		if c.spec.name == name {
			if c.measured {
				t.Errorf("criterion %q must be unmeasured, got value %v", name, c.value)
			}
			if c.reason == "" {
				t.Errorf("unmeasured criterion %q must say why", name)
			}
			return
		}
	}
	t.Errorf("criterion %q is not listed at all — an unmeasured criterion must still appear", name)
}

// --- Trust: merge_acceptance nudge -------------------------------------------

func TestTrustMergeAcceptanceNudge(t *testing.T) {
	// Below the 0.7 threshold the nudge names the rate and points at the
	// mission, because a low acceptance rate is a mission-tuning problem.
	low := findCriterion(t, trustCriteria(quadrantInputs{mergeAcceptance: fptr(0.5)}), "merge_acceptance")
	if !strings.Contains(low.nudge, "50%") || !strings.Contains(low.nudge, "tune the mission") {
		t.Errorf("merge_acceptance nudge below threshold = %q, want the rate and a mission-tuning pointer", low.nudge)
	}

	// At or above the threshold the criterion still scores but stays silent.
	ok := findCriterion(t, trustCriteria(quadrantInputs{mergeAcceptance: fptr(0.7)}), "merge_acceptance")
	if ok.nudge != "" {
		t.Errorf("merge_acceptance at 0.7 should not nudge, got %q", ok.nudge)
	}
}

// --- Efficiency: output_per_agent (idle burn) --------------------------------

func TestEfficiencyOutputPerAgent(t *testing.T) {
	// Three agents, one merged PR: perAgent < 1 is the definition of idle
	// burn, and the nudge suggests pausing idle agents.
	in := quadrantInputs{agentCount: 3, prsMerged90d: iptr(1)}
	c := findCriterion(t, efficiencyCriteria(in), "output_per_agent")
	if c.value <= 0 || c.value >= 1 {
		t.Errorf("output_per_agent value = %v, want 1/3", c.value)
	}
	if c.spec.curve.lowerIsBetter() {
		t.Error("output_per_agent must be higher-is-better")
	}
	if !strings.Contains(c.nudge, "3 agents") || !strings.Contains(c.nudge, "pausing idle agents") {
		t.Errorf("idle-burn nudge = %q, want agent count and a pause suggestion", c.nudge)
	}

	// One PR per agent or better: scored, but no nudge.
	busy := findCriterion(t,
		efficiencyCriteria(quadrantInputs{agentCount: 2, prsMerged90d: iptr(4)}), "output_per_agent")
	if busy.nudge != "" {
		t.Errorf("output_per_agent at 2 PRs/agent should not nudge, got %q", busy.nudge)
	}
	if busy.value != 2 {
		t.Errorf("output_per_agent value = %v, want 2", busy.value)
	}

	// Zero agents: the criterion is unmeasured — a hive that is not running
	// is not inefficient.
	assertUnmeasured(t, efficiencyCriteria(quadrantInputs{agentCount: 0, prsMerged90d: iptr(4)}), "output_per_agent")
}

// --- Productivity: budget-exhausted zero vs idle zero -------------------------

func TestProductivityZeroMergedNudgeDistinguishesThrottledFromIdle(t *testing.T) {
	// A throttled hive is out of budget, not underperforming — the nudge must
	// say so instead of telling the owner to ship more.
	throttled := findCriterion(t, productivityCriteria(quadrantInputs{
		prsMerged90d:    iptr(0),
		budgetExhausted: bptr(true),
	}), critThroughput)
	if !strings.Contains(throttled.nudge, "Budget exhausted") {
		t.Errorf("throttled-hive nudge = %q, want the budget named as the cause", throttled.nudge)
	}

	// budgetExhausted=false is NOT exhaustion: the plain idle wording applies.
	idle := findCriterion(t, productivityCriteria(quadrantInputs{
		prsMerged90d:    iptr(0),
		budgetExhausted: bptr(false),
	}), critThroughput)
	if !strings.Contains(idle.nudge, "No agent merges") {
		t.Errorf("idle-hive (budget ok) nudge = %q, want the no-PRs wording", idle.nudge)
	}

	// Nil budget signal falls back to the idle wording too.
	unknown := findCriterion(t, productivityCriteria(quadrantInputs{
		prsMerged90d: iptr(0),
	}), critThroughput)
	if !strings.Contains(unknown.nudge, "No agent merges") {
		t.Errorf("idle-hive (budget unknown) nudge = %q, want the no-PRs wording", unknown.nudge)
	}

	// Nonzero throughput never nudges.
	shipping := findCriterion(t, productivityCriteria(quadrantInputs{
		prsMerged90d: iptr(5),
	}), critThroughput)
	if shipping.nudge != "" {
		t.Errorf("a shipping hive should not be nudged, got %q", shipping.nudge)
	}
}

// --- Productivity: contributor engagement -------------------------------------

func TestProductivityContributorEngagement(t *testing.T) {
	// Partial engagement names the split.
	partial := findCriterion(t, productivityCriteria(quadrantInputs{
		contributorCount:   4,
		activeContributors: 1,
	}), "contributor_engagement")
	if partial.value != 0.25 {
		t.Errorf("engagement ratio = %v, want 0.25", partial.value)
	}
	if !strings.Contains(partial.nudge, "1 of 4 contributors active") {
		t.Errorf("engagement nudge = %q, want the active/enrolled split", partial.nudge)
	}

	// Full engagement: scored at 1.0, silent.
	full := findCriterion(t, productivityCriteria(quadrantInputs{
		contributorCount:   3,
		activeContributors: 3,
	}), "contributor_engagement")
	if full.value != 1.0 || full.nudge != "" {
		t.Errorf("full engagement = (%v, %q), want (1.0, no nudge)", full.value, full.nudge)
	}
}

// --- Productivity: relay idleness ---------------------------------------------

func TestProductivityRelayIdleNudgeRequiresContributors(t *testing.T) {
	// An idle relay on a hive WITH contributors is actionable.
	withContribs := findCriterion(t, productivityCriteria(quadrantInputs{
		tasksCompleted7d: iptr(0),
		contributorCount: 2,
	}), "relay_7d")
	if withContribs.nudge != "Contributor relay is idle" {
		t.Errorf("relay nudge with contributors = %q, want the idle-relay wording", withContribs.nudge)
	}

	// The same zero on a solo hive is noise, not a nudge.
	solo := findCriterion(t, productivityCriteria(quadrantInputs{
		tasksCompleted7d: iptr(0),
	}), "relay_7d")
	if solo.nudge != "" {
		t.Errorf("solo hive with idle relay should not be nudged, got %q", solo.nudge)
	}
}

// --- Productivity: human_blocked combines both queues --------------------------

func TestProductivityHumanBlockedCombinesHoldAndReviewQueues(t *testing.T) {
	// Both signals present: summed into one criterion so the phenomenon is
	// not double-weighted.
	both := findCriterion(t, productivityCriteria(quadrantInputs{
		holdTotal:      iptr(2),
		awaitingReview: iptr(3),
	}), "human_blocked")
	if both.value != 5 {
		t.Errorf("human_blocked value = %v, want 5 (2 held + 3 awaiting)", both.value)
	}
	if !both.spec.curve.lowerIsBetter() {
		t.Error("human_blocked must be lower-is-better")
	}
	if !strings.Contains(both.nudge, "5 items waiting on a human decision") {
		t.Errorf("human_blocked nudge = %q, want the combined count", both.nudge)
	}

	// Either signal alone is enough to score the criterion.
	holdOnly := findCriterion(t, productivityCriteria(quadrantInputs{
		holdTotal: iptr(1),
	}), "human_blocked")
	if holdOnly.value != 1 {
		t.Errorf("human_blocked (hold only) = %v, want 1", holdOnly.value)
	}
	reviewOnly := findCriterion(t, productivityCriteria(quadrantInputs{
		awaitingReview: iptr(4),
	}), "human_blocked")
	if reviewOnly.value != 4 {
		t.Errorf("human_blocked (review only) = %v, want 4", reviewOnly.value)
	}

	// A measured zero scores but stays silent.
	emptyQueues := findCriterion(t, productivityCriteria(quadrantInputs{
		holdTotal:      iptr(0),
		awaitingReview: iptr(0),
	}), "human_blocked")
	if emptyQueues.value != 0 || emptyQueues.nudge != "" {
		t.Errorf("empty queues = (%v, %q), want (0, no nudge)", emptyQueues.value, emptyQueues.nudge)
	}

	// Neither signal present: the criterion is unmeasured rather than a fake
	// zero.
	assertUnmeasured(t, productivityCriteria(quadrantInputs{}), "human_blocked")
}

// --- Productivity: SLA violations ----------------------------------------------

func TestProductivitySLAViolations(t *testing.T) {
	hot := findCriterion(t, productivityCriteria(quadrantInputs{
		slaViolations: iptr(3),
	}), "sla_violations")
	if hot.value != 3 || !hot.spec.curve.lowerIsBetter() {
		t.Errorf("sla_violations = (%v, lowerIsBetter=%v), want (3, true)", hot.value, hot.spec.curve.lowerIsBetter())
	}
	if !strings.Contains(hot.nudge, "3 items past their SLA") {
		t.Errorf("sla nudge = %q, want the violation count", hot.nudge)
	}

	// A measured zero scores silently.
	ok := findCriterion(t, productivityCriteria(quadrantInputs{
		slaViolations: iptr(0),
	}), "sla_violations")
	if ok.value != 0 || ok.nudge != "" {
		t.Errorf("zero SLA violations = (%v, %q), want (0, no nudge)", ok.value, ok.nudge)
	}
}

// --- unscoredReason / criteriaFor dispatch -------------------------------------

func TestUnscoredReasonCoversEveryAxis(t *testing.T) {
	// Every real axis has a specific reason; only an unknown axis gets the
	// generic fallback. A new axis routed to the fallback is a wiring bug.
	reasons := map[string]string{}
	for _, axis := range QuadrantAxisOrder {
		r := unscoredReason(axis)
		if r == "" || r == "Not measured yet" {
			t.Errorf("axis %q has no specific unscored reason (got %q)", axis, r)
		}
		reasons[r] = axis
	}
	if len(reasons) != len(QuadrantAxisOrder) {
		t.Errorf("unscored reasons are not distinct per axis: %v", reasons)
	}
	if unscoredReason("no-such-axis") != "Not measured yet" {
		t.Errorf("unknown axis reason = %q, want the generic fallback", unscoredReason("no-such-axis"))
	}
}

func TestCriteriaForUnknownAxisReturnsNil(t *testing.T) {
	if got := criteriaFor("no-such-axis", quadrantInputs{acmmLevel: 3}); got != nil {
		t.Errorf("criteriaFor on an unknown axis = %v, want nil", got)
	}
}
