package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/chairlift#799: @Danathar's change request was on an older
// commit and @castrojo had pushed the fix. The row asked "@castrojo has been
// offline … take the PR over now?". A re-review-requested 085 names the
// reviewer and the author's push, waits on the reviewer, and never offers
// the takeover or calls the author offline, even with the adoption grace
// run out on the row.
func TestReReviewRequestedRowWaitsOnTheReviewer(t *testing.T) {
	reason := prstage.ReReviewRequestedReason([]string{"Danathar"})
	it := StageTrackerItem{
		Repo: "projectbluefin/chairlift", Number: 799, Kind: "pr", Stage: stagelabels.NeedsDecision.Number(),
		Head: "948192f343abcdef", Author: "castrojo", Reason: reason,
		Adoption: &StageTrackerAdoption{Author: "castrojo", Due: true},
		Takeover: &StageTrackerTakeover{GraceElapsed: true},
	}
	d := stageTrackerDecisionNeededAt(it, nil, "", time.Date(2026, 11, 17, 16, 11, 0, 0, time.UTC))
	if d == nil {
		t.Fatal("no decision for a re-review-requested row")
	}
	want := "@Danathar requested changes on an older commit; @castrojo pushed `" + shortHead(it.Head) + "` since. Re-review it, or dismiss the review?"
	if d.Question != want {
		t.Fatalf("question = %q\nwant %q", d.Question, want)
	}
	if strings.Contains(d.Question+d.Blocker, "offline") {
		t.Fatalf("decision calls the author offline: %+v", d)
	}
	for _, o := range d.Options {
		if o.Action == decisionActionApprove && o.Enabled {
			t.Fatalf("approve option = %+v, want disabled: it does not clear the change request", o)
		}
		if o.Action == decisionActionAdopt {
			t.Fatalf("decision offers adoption: %+v", o)
		}
	}
	if kind, who := stageTrackerWaitKind(it); kind != waitKindReReview || who != "Danathar" {
		t.Fatalf("wait kind/who = %s/%s, want %s/Danathar", kind, who, waitKindReReview)
	}
	if got := humanizeStageReason(reason); got != "waiting on @Danathar to re-review: the change request is on an older commit" {
		t.Fatalf("caption clause = %q", got)
	}
	// Any other 085 keeps the operator's decision wait.
	it.Reason = prstage.ContributorLaneReasonReporterTrustHold
	if kind, who := stageTrackerWaitKind(it); kind != waitKindNeedsDecision || who != waitWhoOperator {
		t.Fatalf("other 085 wait kind/who = %s/%s", kind, who)
	}
}

func TestDecisionReReviewNames(t *testing.T) {
	for in, want := range map[string]string{"": "the reviewer", "a": "@a", "a,b": "@a and @b", "a,b,c": "@a, @b and @c"} {
		var reviewers []string
		if in != "" {
			reviewers = strings.Split(in, ",")
		}
		if got := decisionReReviewNames(reviewers); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
