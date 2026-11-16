package delegatedecision

import "testing"

// hivecommons/hive#18468: the delegate never applies `hold`; an owner-only
// item gets decision-needed, which the hold rail reads like `hold`.
func TestOwnerOnlyAppliesDecisionNeededNotHold(t *testing.T) {
	d := Run(Input{Kind: KindIssue, Number: 11, Title: "Workflow fails: GITHUB_TOKEN lacks packages:write"})
	if d.Verdict != VerdictOwnerOnly {
		t.Fatalf("verdict = %s, want owner-only", d.Verdict)
	}
	var added []string
	for _, a := range d.Actions {
		if a.Kind == ActionLabelsAdd {
			added = append(added, a.Labels...)
		}
	}
	if hasLabel(added, LabelHold) || !hasLabel(added, LabelDecisionNeeded) || !hasLabel(added, LabelNeedsOwner) {
		t.Fatalf("labels added = %v, want needs-owner + decision-needed and never hold", added)
	}
}

func TestHoldRailReadsHoldAndDecisionNeeded(t *testing.T) {
	for _, labels := range [][]string{{LabelHold}, {LabelDecisionNeeded}, {"Decision-Needed"}} {
		if !HeldByLabels(labels) {
			t.Errorf("HeldByLabels(%v) = false, want held", labels)
		}
		d := Run(Input{Kind: KindIssue, Number: 11, Title: "Workflow fails: GITHUB_TOKEN lacks packages:write", Labels: labels})
		for _, a := range d.Actions {
			if a.Kind == ActionLabelsAdd || a.Kind == ActionLabelsRemove {
				t.Errorf("held by %v: delegate wrote labels %+v", labels, a)
			}
		}
	}
	if HeldByLabels([]string{"needs-owner"}) {
		t.Error("needs-owner alone is not a hold")
	}
}

func TestDelegateNeverLiftsGateOrHold(t *testing.T) {
	for _, label := range []string{LabelHold, LabelDecisionNeeded} {
		d := ApplyRails(Decision{Verdict: VerdictDirection, Actions: []Action{{Kind: ActionLabelsRemove, Labels: []string{label}}}}, Rails{})
		for _, a := range d.Actions {
			if a.Kind == ActionLabelsRemove {
				t.Errorf("removing %q survived the rails: %+v", label, d.Actions)
			}
		}
	}
}

func TestLLMMayNotAddHold(t *testing.T) {
	if hasLabel(LLMLabels, LabelHold) {
		t.Fatalf("LLMLabels = %v; the model must never add hold", LLMLabels)
	}
	if hasLabel(LLMRemovableLabels, LabelDecisionNeeded) {
		t.Fatalf("LLMRemovableLabels = %v; the model must never lift decision-needed", LLMRemovableLabels)
	}
}
