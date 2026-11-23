package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const followUp19229Body = "A review perspective on #19227 ended with a question rather than a finding.\n\n" +
	"> A reviewer handed this to a person: security requires human: Not deeply reviewed: the diff touches Hive proxy policy / egress trust surfaces.\n\n" +
	"Source: hivecommons/hive#19227 (head `0c1450c`).\n"

func governorFollowUpLabels() []string {
	return []string{"origin/governor", reviewQuestionFollowUpLabel, "help wanted"}
}

func TestGovernorSecurityFollowUp(t *testing.T) {
	if !governorSecurityFollowUp(governorFollowUpLabels(), followUp19229Body) {
		t.Fatal("#19229's shape must read as a governor security follow-up")
	}
	if governorSecurityFollowUp([]string{reviewQuestionFollowUpLabel}, followUp19229Body) {
		t.Fatal("without origin/governor the issue is not the governor's follow-up")
	}
	if governorSecurityFollowUp(governorFollowUpLabels(), "> correctness requires human: unclear intent") {
		t.Fatal("another perspective's hand-off is not a security one")
	}
}

func TestIssueHumanOnlyAsk(t *testing.T) {
	if got := issueHumanOnlyAsk(nil, "hivecommons/hive", governorFollowUpLabels(), followUp19229Body); got != delegatedecision.HumanOnlySecurityReview {
		t.Fatalf("governor security follow-up = %q, want %q", got, delegatedecision.HumanOnlySecurityReview)
	}
	if got := issueHumanOnlyAsk(nil, "hivecommons/hive", nil, "please do this\n"+gfi.HumanOnlyMarker); got == "" {
		t.Fatal("the human-only marker must count")
	}
	if got := issueHumanOnlyAsk(nil, "hivecommons/hive", []string{"human-only"}, ""); got == "" {
		t.Fatal("the human-only label must count")
	}
	if got := issueHumanOnlyAsk(nil, "hivecommons/hive", []string{"needs-human"}, "agent stood down"); got != "" {
		t.Fatalf("needs-human alone is the ordinary park a decision lifts, got %q", got)
	}
}

func TestOwnerOnlyIssueDecisionDisablesDelegate(t *testing.T) {
	it := StageTrackerItem{Kind: "issue", Number: 19229, Stage: stagelabels.IssueNeedsDecision.Number(), OwnerOnly: humanOnlyWhySecurityFollowUp}
	d := stageTrackerDecisionNeededWith(it, nil, "", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), blockerOptionsContext{})
	if d == nil || d.BlockerReason != "owner-only:"+humanOnlyWhySecurityFollowUp {
		t.Fatalf("decision = %+v, want the owner-only issue decision", d)
	}
	if o := decisionOptionByAction(t, d, decisionActionDelegate); o.Enabled || o.ReasonIfDisabled == "" {
		t.Fatalf("Let hive decide must be off with a reason: %+v", o)
	}
	if o := decisionOptionByAction(t, d, decisionActionApprove); !o.Enabled {
		t.Fatalf("the person's own approve stays on: %+v", o)
	}

	it.OwnerOnly = "label:needs-human"
	d = stageTrackerDecisionNeededWith(it, nil, "", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), blockerOptionsContext{})
	if o := decisionOptionByAction(t, d, decisionActionDelegate); !o.Enabled {
		t.Fatalf("control: a needs-human park keeps Let hive decide: %+v", o)
	}
}
