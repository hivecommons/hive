package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Each activity monitor row says who can finish it, from the status
// refresh's your-turn decision rather than the current step: a PR at review
// whose repo requires an approving review is the operator's from the start,
// an issue its PR covers waits elsewhere, and a row with no stored decision
// says nothing. A PR back with its author or a person's draft is neither the
// operator's nor Hive's. An issue's linked PR entry carries the PR's answer.
func TestStageTrackerSaysWhoFinishesEachRow(t *testing.T) {
	const approval = "Waiting on a person's review: an approving review is required by branch protection (0 given)"
	repo := FrontendRepo{Name: "x",
		ActionableIssues: []any{
			github.Issue{Repo: "x", Number: 1, Title: "parked"},
			github.Issue{Repo: "x", Number: 2, Title: "covered", LinkedPRs: []github.IssueLinkedPR{{Number: 9, State: "open"}}},
			github.Issue{Repo: "x", Number: 3, Title: "no decision"},
		},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 8, Title: "hive's"}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 9, Title: "needs approval"}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 10, Title: "returned", Author: "alice", Labels: []string{"stage/072-returned-to-contributor"}}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 11, Title: "person's draft", Author: "bob", Draft: true, Labels: []string{"author/human"}}},
		},
		Ownership: map[string]Ownership{
			ownershipKey(ownershipKindIssue, 1): {YourTurn: true, Verb: OwnershipVerbDecide, ReasonCode: OwnershipReasonParkedNeedsDecision, Reason: "Parked until a maintainer decides"},
			ownershipKey(ownershipKindIssue, 2): waitingElsewhere(),
			ownershipKey(ownershipKindPR, 8):    notYourTurn(),
			ownershipKey(ownershipKindPR, 9):    {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonReviewDecision, Reason: approval},
			ownershipKey(ownershipKindPR, 10):   notYourTurn(),
			ownershipKey(ownershipKindPR, 11):   notYourTurn(),
		},
	}
	tr := (&Server{}).buildStageTracker(&StatusPayload{Repos: []FrontendRepo{repo}}, nil, time.Now())

	type want struct{ finisher, verb, reason string }
	wants := map[string]want{
		"issue#1": {finisherPerson, OwnershipVerbDecide, "Parked until a maintainer decides"},
		"issue#2": {finisherElsewhere, "", ""},
		"issue#3": {"", "", ""},
		"pr#8":    {finisherHive, "", ""},
		"pr#9":    {finisherPerson, OwnershipVerbReview, approval},
		// Not the operator's turn, but not Hive's either: the row's own step
		// or a person's draft names the author.
		"pr#10": {finisherElsewhere, "", "Returned to @alice — @alice must act"},
		"pr#11": {finisherElsewhere, "", "Draft — its author must mark it ready for review"},
	}
	seen := 0
	for _, it := range tr.Items {
		key := ownershipKey(it.Kind, it.Number)
		w, ok := wants[key]
		if !ok {
			continue
		}
		seen++
		if got := (want{it.Finisher, it.FinisherVerb, it.FinisherReason}); got != w {
			t.Errorf("%s: finisher = %+v, want %+v", key, got, w)
		}
		if key == "issue#2" {
			if len(it.LinkedPRs) != 1 || it.LinkedPRs[0].Number != 9 {
				t.Fatalf("issue#2 linked PRs = %+v, want #9", it.LinkedPRs)
			}
			lp := it.LinkedPRs[0]
			if got := (want{lp.Finisher, lp.FinisherVerb, lp.FinisherReason}); got != wants["pr#9"] {
				t.Errorf("issue#2's linked PR #9: finisher = %+v, want the PR's own %+v", got, wants["pr#9"])
			}
		}
	}
	if seen != len(wants) {
		t.Fatalf("saw %d of %d rows: %+v", seen, len(wants), tr.Items)
	}
}
