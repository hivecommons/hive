package review

import "testing"

// hivecommons/hive#19229: the security perspective's own requires_human is
// a structured "a person must read this" the aggregate carries, distinct
// from the requires_human the relay stamps on every perspective of a
// decision-requested review.
func TestAggregateSecurityRoutedToPerson(t *testing.T) {
	got := AggregateReports([]PerspectiveReport{
		baseReport(PerspectiveCorrectness, VerdictApprove),
		baseReport(PerspectiveSecurity, VerdictRequiresHuman),
	}, AggregateOptions{})
	if !got.SecurityRoutedToPerson || !got.SecurityRequiresPerson() {
		t.Fatalf("security requires_human must route to a person: %+v", got)
	}
	if len(got.HumanSummaries) == 0 || !SecurityRequiresHumanSummary("A reviewer handed this to a person: "+got.HumanSummaries[0]) {
		t.Fatalf("human summary %v must read as the security hand-off", got.HumanSummaries)
	}
}

func TestAggregateSecurityRoutedIgnoresStampedDecision(t *testing.T) {
	sec := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	sec.DecisionRequested = true
	sec.DecisionQuestion = "should this merge ahead of the docs PR?"
	got := AggregateReports([]PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictApprove), sec}, AggregateOptions{})
	if got.SecurityRoutedToPerson {
		t.Fatalf("a decision question stamped across perspectives is not a security hand-off: %+v", got)
	}
}

func TestAggregateSecurityRoutedNeedsRequiresHuman(t *testing.T) {
	a := Aggregate{SecurityRoutedToPerson: true}
	if a.SecurityRequiresPerson() {
		t.Fatal("an aggregate whose requires_human was cleared is not routed to a person")
	}
}

func TestSecurityRequiresHumanSummaryOtherPerspective(t *testing.T) {
	for _, text := range []string{"correctness requires human: unclear intent", "the security of the change looks fine", ""} {
		if SecurityRequiresHumanSummary(text) {
			t.Errorf("%q must not read as a security hand-off", text)
		}
	}
}

func TestClassifyDecisionQuestionSecurityRoutedIsSignOff(t *testing.T) {
	in := DecisionQuestionInput{
		Question:     "A reviewer handed this to a person: security requires human: Not deeply reviewed: the diff touches Hive proxy policy",
		HiveAuthored: true, CIGreen: true, SecurityRequiresPerson: true,
	}
	class, reason := ClassifyDecisionQuestion(in)
	if class != DecisionQuestionPolicySignOff || reason != ReasonSecurityRoutedToPerson {
		t.Fatalf("class = %s (%s), want policy sign-off for a security hand-off", class, reason)
	}
	in.SecurityRequiresPerson = false
	in.Question = "should this also cover the docs page?"
	if class, _ := ClassifyDecisionQuestion(in); class != DecisionQuestionScope {
		t.Fatalf("control: a scope question without the security hand-off stays %s, got %s", DecisionQuestionScope, class)
	}
}
