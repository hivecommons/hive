package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestDeriveDelegatesToRegisteredDerivers(t *testing.T) {
	prevIssue, prevPR := Derivers()
	t.Cleanup(func() {
		deriversMu.Lock()
		issueDeriver, prDeriver = prevIssue, prevPR
		deriversMu.Unlock()
	})
	deriversMu.Lock()
	issueDeriver, prDeriver = nil, nil
	deriversMu.Unlock()

	issue := Facts{Ref: Ref{Kind: stagelabels.IssueItem}}
	pr := Facts{Ref: Ref{Kind: stagelabels.PRItem}}
	if d := DeriveIssue(issue); !d.Abstain || d.Reason != reasonNoDeriver {
		t.Fatalf("unregistered issue deriver: %+v", d)
	}
	if d := DerivePR(pr); !d.Abstain || d.Reason != reasonNoDeriver {
		t.Fatalf("unregistered pr deriver: %+v", d)
	}

	SetDerivers(
		DeriverFunc(stagelabels.IssueItem, func(Facts) Decision { return Decision{Stage: stagelabels.Claimed, Reason: "issue"} }),
		DeriverFunc(stagelabels.PRItem, func(Facts) Decision { return Decision{Stage: stagelabels.MergeQueue, Reason: "pr"} }),
	)
	if d := DeriveIssue(issue); d.Stage != stagelabels.Claimed || d.Reason != "issue" {
		t.Fatalf("issue: %+v", d)
	}
	if d := DerivePR(pr); d.Stage != stagelabels.MergeQueue || d.Reason != "pr" {
		t.Fatalf("pr: %+v", d)
	}
	// Each asserts the kind: issue facts to DerivePR abstain, and vice versa.
	if d := DerivePR(issue); !d.Abstain || d.Reason != reasonKindMismatch {
		t.Fatalf("pr deriver on issue facts: %+v", d)
	}
	if d := DeriveIssue(pr); !d.Abstain || d.Reason != reasonKindMismatch {
		t.Fatalf("issue deriver on pr facts: %+v", d)
	}
	if IssueDeriver().Kind() != stagelabels.IssueItem || PRDeriver().Kind() != stagelabels.PRItem {
		t.Fatal("deriver kinds")
	}
	// nil leaves a registration alone.
	SetDerivers(nil, nil)
	if d := DeriveIssue(issue); d.Stage != stagelabels.Claimed {
		t.Fatalf("SetDerivers(nil, nil) changed the issue deriver: %+v", d)
	}
}

func TestDecisionDesiredAndRef(t *testing.T) {
	d := Decision{Stage: stagelabels.Fixing, Role: stagelabels.RoleTrusted, Kind: "hive", Stalled: true, DCO: stagelabels.DCOYes}
	want := stagelabels.Desired{Stage: stagelabels.Fixing, Role: stagelabels.RoleTrusted, Kind: "hive", Stalled: true, DCO: stagelabels.DCOYes}
	if d.Desired() != want {
		t.Fatalf("Desired() = %+v", d.Desired())
	}
	r := Ref{Owner: "Acme", Repo: "Widgets", Number: 12, Kind: stagelabels.PRItem}
	if r.Key() != stagelabels.Key("Acme/Widgets", 12) || r.Key() != "acme/widgets#12" || r.String() != "Acme/Widgets#12" || r.FullRepo() != "Acme/Widgets" {
		t.Fatalf("key=%q string=%q", r.Key(), r.String())
	}
	// The default forge is implied: an explicit "github" keys identically.
	gh := r
	gh.Source = "GitHub"
	if gh.Key() != r.Key() || !gh.IsDefaultSource() || !r.IsDefaultSource() {
		t.Fatalf("github source key=%q", gh.Key())
	}
	other := r
	other.Source = "GitLab"
	if other.Key() != "gitlab:acme/widgets#12" || other.IsDefaultSource() || other.String() != r.String() {
		t.Fatalf("other source key=%q", other.Key())
	}
}
