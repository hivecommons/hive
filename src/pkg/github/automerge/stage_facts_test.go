package automerge

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestPRDeriverReproducesResolveStage asserts the stageengine adapter is a
// faithful wrapper: Facts rebuilt from the tracker's lane observations
// derive exactly what resolveStage decides today (stage, reason, role,
// terminal, hand-off), for every lane shape the legacy tests exercise.
func TestPRDeriverReproducesResolveStage(t *testing.T) {
	approve := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}}
	reject := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
	human := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}}
	humanApproved := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}, HumanApprover: "clubanderson", HumanApprovalHead: "sha", Role: stagelabels.RoleContributor}
	decision := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "approve", DecisionRequested: true, DecisionQuestion: "@clubanderson should decide."}}
	trusted := stageDetail{Role: stagelabels.RoleTrusted}
	base := stageObservation{Owner: "acme", Repo: "widgets", Number: 7, Author: "dev", HeadSHA: "sha", Labels: []string{"bug"}}
	obs := func(o stageObservation) stageObservation {
		o.Owner, o.Repo, o.Number = base.Owner, base.Repo, base.Number
		if o.Author == "" {
			o.Author = base.Author
		}
		if o.HeadSHA == "" {
			o.HeadSHA = base.HeadSHA
		}
		if o.Labels == nil {
			o.Labels = base.Labels
		}
		return o
	}
	cases := []struct {
		name string
		list []stageObservation
	}{
		{"merged", []stageObservation{obs(stageObservation{Lane: stageLaneSelf, Merged: true})}},
		{"closed", []stageObservation{obs(stageObservation{Lane: stageLaneSelf, Reason: "closed"})}},
		{"draft", []stageObservation{obs(stageObservation{Lane: stageLaneSelf, Reason: "draft"})}},
		{"handoff abstains", []stageObservation{obs(stageObservation{Lane: stageLaneSelf, Reason: "not-app-authored"})}},
		{"error abstains", []stageObservation{obs(stageObservation{Lane: stageLaneSelf, Reason: "fetch-pr", Err: true})}},
		{"review pending", []stageObservation{obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict})}},
		{"requires_human", []stageObservation{obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: human})}},
		{"human approval on current head", []stageObservation{obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSoaking + "(remaining=30m)", Detail: humanApproved})}},
		{"human approval on new head", []stageObservation{obs(stageObservation{Lane: stageLaneContributor, HeadSHA: "newsha", Reason: contributorLaneReasonSoaking + "(remaining=30m)", Detail: humanApproved})}},
		{"decision requested", []stageObservation{obs(stageObservation{Lane: stageLaneContributor, Reason: "required-check-pending:unit", Detail: decision})}},
		{"approved ci pending", []stageObservation{obs(stageObservation{Lane: stageLaneTrusted, Reason: "required-check-pending:build", Detail: approve})}},
		{"self ci red", []stageObservation{obs(stageObservation{Lane: stageLaneSelf, Reason: "check-failing:unit"})}},
		{"clanker ci red", []stageObservation{obs(stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, Reason: "required-check-failing:unit"})}},
		{"human conflicting", []stageObservation{obs(stageObservation{AuthorKind: authorkind.Human, Lane: stageLaneTrusted, Reason: "conflicting"})}},
		{"hive kind ci red", []stageObservation{obs(stageObservation{AuthorKind: authorkind.Hive, Lane: stageLaneContributor, Reason: "required-check-failing:unit"})}},
		{"changes requested", []stageObservation{obs(stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, Reason: "pending: head pushed 1m0s ago (< min_head_age)", Detail: reject})}},
		{"lane precedence: trusted weak, contributor refines", []stageObservation{
			obs(stageObservation{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}),
			obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{Role: stagelabels.RoleContributor}}),
		}},
		{"weak opinion only", []stageObservation{obs(stageObservation{Lane: stageLaneTrusted, Reason: "untrusted-author-role"})}},
		{"self outranks contributor handoff", []stageObservation{
			obs(stageObservation{Lane: stageLaneSelf, Reason: "check-failing:unit", Detail: trusted}),
			obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonAppAuthored}),
		}},
		{"registry-trusted with no contributor opinion abstains", []stageObservation{
			obs(stageObservation{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}),
			obs(stageObservation{Lane: stageLaneContributor, Reason: "something-new", Detail: trusted}),
		}},
		{"maintainer trusted error and contributor no-claim abstains", []stageObservation{
			obs(stageObservation{Lane: stageLaneTrusted, Reason: "fetch-pr", Err: true, Detail: trusted}),
			obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: stageDetail{Role: stagelabels.RoleUntrusted}}),
		}},
		{"maintainer ci pending beats no-claim", []stageObservation{
			obs(stageObservation{Lane: stageLaneTrusted, Reason: "required-check-pending:golangci-lint", Detail: trusted}),
			obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoClaim, Detail: trusted}),
		}},
		{"contributor tier needs-human", []stageObservation{
			obs(stageObservation{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}),
			obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonClaimTier, Detail: stageDetail{Role: stagelabels.RoleContributor}}),
		}},
		{"sentinel blocked needs-decision", []stageObservation{
			obs(stageObservation{Lane: stageLaneTrusted, Reason: "untrusted-author-role"}),
			obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonSentinelBlocked, Detail: trusted}),
		}},
		{"author/hive label lifts role", []stageObservation{obs(stageObservation{Lane: stageLaneContributor, Reason: contributorLaneReasonNoVerdict, Labels: []string{"author/hive"}})}},
	}
	deriver := PRDeriver()
	if deriver.Kind() != stagelabels.PRItem {
		t.Fatal("kind")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, wantRole, wantObs := resolveStage(tc.list)
			f := factsFrom(tc.list)
			if f.Ref.Owner != "acme" || f.Ref.Number != 7 || f.PR == nil || len(f.PR.Lanes) != len(tc.list) {
				t.Fatalf("facts %+v", f)
			}
			got := deriver.Derive(f)
			if want.Rank == stageRankNone {
				if !got.Abstain || got.Reason != wantObs.Reason {
					t.Fatalf("legacy has no opinion; got %+v", got)
				}
				return
			}
			wantReason := want.Reason
			if wantReason == "" {
				wantReason = wantObs.Reason
			}
			if got.Abstain || got.Stage != want.Stage || got.Reason != wantReason || got.Role != wantRole || got.Terminal != want.Terminal {
				t.Fatalf("got %+v; want stage=%v reason=%q role=%v terminal=%v", got, want.Stage, wantReason, wantRole, want.Terminal)
			}
			if (got.Human != nil) != (want.Human && !want.Terminal) || (got.Human != nil && got.Human.Stage != want.Stage) {
				t.Fatalf("human route %+v vs verdict human=%v", got.Human, want.Human)
			}
			// The rebuilt observations classify exactly as the originals.
			for i, o := range observationsFrom(f) {
				a, b := classifyStage(o), classifyStage(tc.list[i])
				if a != b {
					t.Fatalf("lane %d reclassifies as %+v, was %+v", i, a, b)
				}
			}
		})
	}
}

func TestPRDeriverAbstainsWithoutEvidence(t *testing.T) {
	d := PRDeriver()
	if got := d.Derive(factsFrom(nil)); !got.Abstain {
		t.Fatalf("empty list: %+v", got)
	}
	if got := d.Derive(stageengine.Facts{Ref: stageengine.Ref{Kind: stagelabels.IssueItem}}); !got.Abstain {
		t.Fatalf("issue facts: %+v", got)
	}
	if got := d.Derive(stageengine.Facts{Ref: stageengine.Ref{Kind: stagelabels.PRItem}, PR: &stageengine.PRFacts{}}); !got.Abstain {
		t.Fatalf("no lanes: %+v", got)
	}
}

// A PR no lane has observed but a person parked (an out-of-cycle Parked fact
// from the issue request relay, or a needs-decision / needs-human label on
// the fresh read) sits at 085 on the human route with the role it wears; an
// un-park or a closed PR still abstains until the lanes speak.
func TestPRDeriverParksUnobservedPR(t *testing.T) {
	d := PRDeriver()
	base := stageengine.Facts{Ref: stageengine.Ref{Kind: stagelabels.PRItem}, Open: true, PR: &stageengine.PRFacts{}}

	parked := base
	parked.Labels = []string{"needs-decision", stagelabels.RoleContributor.Label()}
	parked.Park = stageengine.ParkFacts{DecisionLabel: "needs-decision"}
	got := d.Derive(parked)
	if got.Abstain || got.Stage != stagelabels.NeedsDecision || got.Role != stagelabels.RoleContributor ||
		got.Human == nil || got.Human.Stage != stagelabels.NeedsDecision || got.Reason != "parked on a person: needs-decision" {
		t.Fatalf("decision park: %+v", got)
	}

	human := base
	human.Park = stageengine.ParkFacts{NeedsHuman: true}
	if got := d.Derive(human); got.Abstain || got.Stage != stagelabels.NeedsDecision || got.Reason != "parked on a person: "+stageengine.NeedsHumanLabel {
		t.Fatalf("needs-human park: %+v", got)
	}

	unparked := parked
	unparked.Park.ApprovedDirection = true
	if got := d.Derive(unparked); !got.Abstain {
		t.Fatalf("un-park must abstain without lanes: %+v", got)
	}
	closed := parked
	closed.Open = false
	if got := d.Derive(closed); !got.Abstain {
		t.Fatalf("closed PR must abstain: %+v", got)
	}
}
