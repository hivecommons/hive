package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14975 (adopted by #14980) and #14947 (by #14977): a
// contributor PR parked at 085 by the review swarm's requires_human was
// adopted in supersede mode, yet stayed at needs-decision because 085 did
// not yield to adoption. The owner's rule: a PR that needed adoption goes
// back to the CI/Review column as adopted; nothing a person must decide
// remains on it.
func supersededAdoptionFacts(reviewState stagelabels.VerdictState, verdict string) Facts {
	return Facts{
		Ref:    Ref{Owner: "o", Repo: "r", Number: 14975, Kind: stagelabels.PRItem},
		Open:   true,
		Labels: []string{"stage/085-needs-decision", "author/human"},
		PR: &PRFacts{
			HeadSHA: "de64cb1de64cb1de64cb1de64cb1de64cb1de64c",
			Review:  ReviewFacts{State: reviewState, Verdict: verdict, VerdictKnown: true, DecisionRequested: verdict == "requires_human"},
			Lanes: []LaneFact{
				{Lane: stageLaneSelf, Reason: "not-self-authored"},
				{Lane: stageLaneTrusted, Reason: "untrusted-author-role"},
				{Lane: stageLaneContributor, Reason: "reviewer-verdict:requires_human", Role: stagelabels.RoleContributor},
			},
		},
	}
}

func TestDerivePR_NeedsDecisionYieldsToSupersedeAdoption(t *testing.T) {
	f := supersededAdoptionFacts(stagelabels.VerdictRequiresHuman, "requires_human")
	f.PR.Pass = &PRPassFacts{Route: &stagelabels.Evidence{Head: f.PR.HeadSHA, HeadFresh: true, Verdict: stagelabels.VerdictRequiresHuman}}
	before := DerivePR(f)
	if before.Stage != stagelabels.NeedsDecision || before.Human == nil {
		t.Fatalf("without adoption want 085 with a route, got %+v", before)
	}

	f.PR.Pass.AdoptionLive = true
	after := DerivePR(f)
	if after.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("adopted: stage = %s, want %s (%+v)", after.Stage, stagelabels.AdoptedByHive, after)
	}
	if after.Human != nil {
		t.Fatalf("adopted: no decision is owed on the original, got route %+v", after.Human)
	}
	if after.Terminal || after.Abstain {
		t.Fatalf("adopted: %+v", after)
	}
}

func TestDerivePR_NeedsDecisionKeepsParkUnderHoldOrSentinel(t *testing.T) {
	for name, route := range map[string]*stagelabels.Evidence{
		"hold":     {HeadFresh: true, Verdict: stagelabels.VerdictRequiresHuman, Holds: []string{"hive/hold"}},
		"sentinel": {HeadFresh: true, Verdict: stagelabels.VerdictRequiresHuman, Sentinel: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := supersededAdoptionFacts(stagelabels.VerdictRequiresHuman, "requires_human")
			f.PR.Pass = &PRPassFacts{Route: route, AdoptionLive: true}
			d := DerivePR(f)
			if d.Stage != stagelabels.NeedsDecision {
				t.Fatalf("%s: stage = %s, want 085 kept for the person (%+v)", name, d.Stage, d)
			}
		})
	}
}

func TestDerivePR_EscalatedAdoptionStaysAtNeedsDecision(t *testing.T) {
	f := supersededAdoptionFacts(stagelabels.VerdictRequiresHuman, "requires_human")
	f.PR.Pass = &PRPassFacts{Route: &stagelabels.Evidence{HeadFresh: true, Verdict: stagelabels.VerdictRequiresHuman}, AdoptionLive: true, EscalationReason: "adoption-escalated:dco"}
	d := DerivePR(f)
	if d.Stage != stagelabels.NeedsDecision || d.Reason != "adoption-escalated:dco" {
		t.Fatalf("escalated adoption: %+v", d)
	}
}

// With no lane observation (the restage after adoptContributorPR, or a
// webhook-driven derivation), the adoption facts alone place the original
// at 074 with the superseding PR named.
func TestDerivePR_AdoptionFactsAloneYield074(t *testing.T) {
	f := Facts{
		Ref:      Ref{Owner: "o", Repo: "r", Number: 14975, Kind: stagelabels.PRItem},
		Open:     true,
		Labels:   []string{"stage/085-needs-decision"},
		Park:     ParkFacts{DecisionLabel: "stage/085-needs-decision"},
		Adoption: AdoptionFacts{Adopted: true, Live: true, Head: "de64cb1", SupersededBy: 14980},
		PR:       &PRFacts{HeadSHA: "de64cb1"},
	}
	d := DerivePR(f)
	if d.Stage != stagelabels.AdoptedByHive || d.Human != nil {
		t.Fatalf("adoption facts: %+v", d)
	}
	if d.Reason != "adopted by the hive: superseded by #14980" {
		t.Fatalf("reason = %q", d.Reason)
	}

	stale := f
	stale.Adoption.Head = "0ld0ld0"
	if d := DerivePR(stale); d.Stage == stagelabels.AdoptedByHive {
		t.Fatalf("stale adoption head must not claim 074: %+v", d)
	}
	yielded := f
	yielded.Adoption.Yielded = true
	if d := DerivePR(yielded); d.Stage == stagelabels.AdoptedByHive {
		t.Fatalf("yielded adoption must not claim 074: %+v", d)
	}
	held := f
	held.Park.Holds = []string{"hive/hold"}
	if d := DerivePR(held); d.Stage == stagelabels.AdoptedByHive {
		t.Fatalf("held PR must keep its park: %+v", d)
	}
	labelled := f
	labelled.Labels = append(labelled.Labels, NeedsHumanLabel)
	labelled.Park.NeedsHuman, labelled.Park.DecisionLabel = true, NeedsHumanLabel
	if d := DerivePR(labelled); d.Stage == stagelabels.AdoptedByHive {
		t.Fatalf("needs-human label must keep its park (#14471): %+v", d)
	}
}
