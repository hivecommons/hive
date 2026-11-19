package workflowmodel

import (
	"slices"
	"testing"
)

func TestNormalizeReply(t *testing.T) {
	for in, want := range map[string]string{
		"approve analysis": "approve-analysis", " Approve_Analysis ": "approve-analysis", "approve--analysis": "approve-analysis",
		"decide:approve": "decide:approve", "": "", "  ": "",
	} {
		if got := NormalizeReply(in); got != want {
			t.Errorf("NormalizeReply(%q)=%q want %q", in, got, want)
		}
	}
}

func TestProfileRepliesResolveToTransitions(t *testing.T) {
	m := techdocs(t)
	m.Spec.Labels = LabelSet{Prefix: "docs", Entries: []LabelEntry{{Name: "docs/040-draft-under-way", State: "draft-under-way"}, {Name: "docs/090-approved", State: "approved"}}}
	p, _, err := Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.RepliesForNumber(45); !slices.Equal(got, []string{"approve analysis", "revise"}) {
		t.Fatalf("RepliesForNumber(45)=%v", got)
	}
	if got := p.RepliesForNumber(10); got != nil {
		t.Fatalf("RepliesForNumber(10)=%v", got)
	}
	if got := p.RepliesForNumber(999); got != nil {
		t.Fatalf("RepliesForNumber(999)=%v", got)
	}
	if got := p.ReplyVerbs(); !slices.Equal(got, []string{"approve", "approve analysis", "hand back", "request changes", "revise"}) {
		t.Fatalf("ReplyVerbs=%v", got)
	}

	// Gate reply wins: the gate analysis-approved releases the move to
	// draft-under-way, whose label entry becomes the label to apply.
	eff, ok := p.ReplyForNumber(45, "Approve Analysis")
	if !ok || eff.Verb != "approve analysis" || eff.From != "analysis-review" || eff.To != "draft-under-way" || eff.Event != "draft-opened" || eff.Label != "docs/040-draft-under-way" {
		t.Fatalf("approve analysis: %+v %v", eff, ok)
	}
	// Transition note: `revise` fires analysis-posted, whose fact observes
	// the techdocs/analysis label, so that label is applied.
	eff, ok = p.Reply("analysis-review", "revise")
	if !ok || eff.To != "analysing" || eff.Event != "analysis-posted" || eff.Label != "techdocs/analysis" {
		t.Fatalf("revise: %+v %v", eff, ok)
	}
	// A declared verb with no transition is recognised but moves nothing.
	eff, ok = p.Reply("analysing", "hand-back")
	if !ok || eff.Verb != "hand back" || eff.To != "" || eff.Label != "" {
		t.Fatalf("hand back: %+v %v", eff, ok)
	}
	eff, ok = p.ReplyForNumber(50, "approve")
	if !ok || eff.To != "approved" || eff.Label != "docs/090-approved" {
		t.Fatalf("approve: %+v %v", eff, ok)
	}
	// Not offered in this state, unknown state, unknown number, blank verb.
	for _, c := range []struct {
		state, verb string
	}{{"analysis-review", "approve"}, {"requested", "approve analysis"}, {"nope", "revise"}, {"analysis-review", "  "}} {
		if _, ok := p.Reply(c.state, c.verb); ok {
			t.Errorf("Reply(%q,%q) accepted", c.state, c.verb)
		}
	}
	if _, ok := p.ReplyForNumber(999, "approve"); ok {
		t.Error("ReplyForNumber(999) accepted")
	}
	var nilP *Profile
	if nilP.RepliesForNumber(45) != nil || nilP.ReplyVerbs() != nil {
		t.Error("nil profile must answer nil")
	}
	if _, ok := nilP.Reply("x", "y"); ok {
		t.Error("nil profile Reply accepted")
	}
	if _, ok := nilP.ReplyForNumber(45, "y"); ok {
		t.Error("nil profile ReplyForNumber accepted")
	}
}

// A verb matched only by its event's label or id still resolves.
func TestProfileReplyMatchesEventLabel(t *testing.T) {
	m := techdocs(t)
	for i := range m.Spec.States {
		if m.Spec.States[i].ID == "requested" {
			m.Spec.States[i].Lifecycle.Replies = []string{"Accepted"}
		}
	}
	p, _, err := Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	eff, ok := p.ReplyForNumber(10, "accepted")
	if !ok || eff.To != "analysing" || eff.Event != "accepted" || eff.Label != "techdocs/accepted" {
		t.Fatalf("accepted: %+v %v", eff, ok)
	}
}
