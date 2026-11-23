package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func changesRequestedPR(requestedBy []string, requested ...string) github.PullRequest {
	return github.PullRequest{
		Number:             7,
		RequestedReviewers: requested,
		Protection:         &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested, ChangesRequestedBy: requestedBy},
	}
}

// Only a PR whose every change requester was re-requested reads as
// "changes addressed" (hivecommons/hive#19538).
func TestStageTrackerChangesAddressedBy(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   github.PullRequest
		want string
	}{
		{name: "re-requested reviewer", pr: changesRequestedPR([]string{"Danathar"}, "danathar"), want: "Danathar"},
		{name: "not re-requested", pr: changesRequestedPR([]string{"Danathar"}, "someone-else")},
		{name: "no requested reviewers", pr: changesRequestedPR([]string{"Danathar"})},
		{name: "one of two re-requested", pr: changesRequestedPR([]string{"Danathar", "nate"}, "Danathar")},
		{name: "both re-requested", pr: changesRequestedPR([]string{"Danathar", "nate"}, "nate", "Danathar"), want: "Danathar,nate"},
		{name: "no change request", pr: github.PullRequest{RequestedReviewers: []string{"Danathar"}, Protection: &github.ProtectionFacts{}}},
		{name: "no protection facts", pr: github.PullRequest{RequestedReviewers: []string{"Danathar"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(stageTrackerChangesAddressedBy(tc.pr), ","); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The 085 row's next-move clause, the decision question and the review
// signal say "changes addressed, re-approve" instead of a bare decision.
func TestChangesAddressedWording(t *testing.T) {
	it := StageTrackerItem{Kind: "pr", Number: 7, Stage: stagelabels.NeedsDecision.Number(), Head: "abc1234def", ChangesAddressedBy: []string{"Danathar"}}
	if got := stageTrackerDecisionWhy(it); got != changesAddressedWord {
		t.Fatalf("why = %q, want %q", got, changesAddressedWord)
	}
	a := stageTrackerStageActor(it, nil, time.Now())
	if a.Kind != StageActorHuman || !strings.Contains(a.Why, changesAddressedWord) || a.HumanReason != changesAddressedWord {
		t.Fatalf("actor = %+v, want the changes-addressed reason", a)
	}
	d := decisionForPR(it, nil, "", false)
	if d == nil || !strings.HasPrefix(d.Question, "Changes addressed, re-approve") || !strings.Contains(d.Question, "@Danathar") {
		t.Fatalf("decision = %+v", d)
	}

	review := prGitHubReview(changesRequestedPR([]string{"Danathar"}, "Danathar"))
	if review == nil || !strings.HasPrefix(review.label, changesAddressedWord) {
		t.Fatalf("review signal = %+v, want changes addressed", review)
	}
	if review := prGitHubReview(changesRequestedPR([]string{"Danathar"})); review == nil || !strings.HasPrefix(review.label, "changes requested") {
		t.Fatalf("review signal without a re-request = %+v, want changes requested", review)
	}
	plain := it
	plain.ChangesAddressedBy = nil
	if got := stageTrackerDecisionWhy(plain); got == changesAddressedWord {
		t.Fatal("a PR without a re-requested change requester must not read changes addressed")
	}
}
