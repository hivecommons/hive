package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A person returned a live adoption's head the hive cannot push (a fork it
// declined to re-home) to its author: every pass of the 072 dwell from the
// decision lands on the author's stage, whatever the lane read, so the PR
// neither reads as the hive's (074) nor flaps between the two.
func TestDerivePR_AuthorsTurnHoldsAuthorStage(t *testing.T) {
	facts := func(kind authorkind.Kind) Facts {
		return Facts{
			Ref:    Ref{Owner: "o", Repo: "r", Number: 1308, Kind: stagelabels.PRItem},
			Open:   true,
			Labels: []string{"stage/072-returned-to-contributor"},
			PR: &PRFacts{
				HeadSHA: "a25e8a5a25e8a5a25e8a5a25e8a5a25e8a5a25e8",
				Lanes:   []LaneFact{{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Role: stagelabels.RoleContributor}},
				Pass:    &PRPassFacts{AuthorKind: kind},
			},
		}
	}
	f := facts(authorkind.Clanker)
	if d := DerivePR(f); d.Stage != stagelabels.ReviewPending {
		t.Fatalf("lane alone: %+v, want 050", d)
	}
	f.PR.Pass.AdoptionLive = true
	if d := DerivePR(f); d.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("live adoption: %+v, want 074", d)
	}
	f.PR.Pass.AdoptionLive, f.PR.Pass.AuthorsTurn = false, true
	if d := DerivePR(f); d.Stage != stagelabels.ReturnedToContributor || d.Human != nil || d.Reason != autoMergeReasonHumanChangesRequested {
		t.Fatalf("author's turn on a clanker PR: %+v, want 072 on the decision's reason", d)
	}
	h := facts(authorkind.Human)
	h.PR.Pass.AuthorsTurn = true
	if d := DerivePR(h); d.Stage != stagelabels.NeedsAuthor {
		t.Fatalf("author's turn on a person's PR: %+v, want 071", d)
	}
}
