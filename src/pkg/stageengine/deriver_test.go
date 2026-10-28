package stageengine

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The engine must derive PRs without importing automerge for init side effects.
func TestBuiltInDerivers(t *testing.T) {
	issue := Facts{Ref: Ref{Kind: stagelabels.IssueItem}, Open: true, Issue: &IssueFacts{}}
	pr := Facts{Ref: Ref{Kind: stagelabels.PRItem}, Open: true, PR: &PRFacts{Lanes: []LaneFact{{Lane: "self-authored", Reason: "conflicting"}}}}
	if d := DerivePR(pr); d.Abstain || d.Stage != stagelabels.Fixing {
		t.Fatalf("built-in PR: %+v", d)
	}
	if d := DeriveIssue(issue); d.Abstain {
		t.Fatalf("built-in issue: %+v", d)
	}
	if d := DerivePR(issue); !d.Abstain || d.Reason != reasonKindMismatch {
		t.Fatalf("PR on issue: %+v", d)
	}
	if d := DeriveIssue(pr); !d.Abstain || d.Reason != reasonKindMismatch {
		t.Fatalf("issue on PR: %+v", d)
	}
	if IssueDeriver().Kind() != stagelabels.IssueItem || PRDeriver().Kind() != stagelabels.PRItem {
		t.Fatal("deriver kinds")
	}
}

func TestDecisionDesiredAndRef(t *testing.T) {
	d := Decision{Stage: stagelabels.Fixing, Role: stagelabels.RoleTrusted, Kind: "hive", Stalled: true, DCO: stagelabels.DCOYes}
	want := stagelabels.Desired{Stage: stagelabels.Fixing, Role: stagelabels.RoleTrusted, Kind: "hive", Stalled: true, DCO: stagelabels.DCOYes}
	if !reflect.DeepEqual(d.Desired(), want) {
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
