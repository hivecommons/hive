package prstage

import (
	"slices"
	"testing"
)

func TestReReviewRequestedReasonRoundTrip(t *testing.T) {
	reason := ReReviewRequestedReason([]string{"zed", "@Alice", "", "zed"})
	if reason != "re-review-requested:Alice,zed" {
		t.Fatalf("reason = %q, want sorted, deduped logins without @", reason)
	}
	got, ok := ParseReReviewRequested(reason)
	if !ok || !slices.Equal(got, []string{"Alice", "zed"}) {
		t.Fatalf("parse = %v %v", got, ok)
	}
	if _, ok := ParseReReviewRequested(ContributorLaneReasonChangesRequested); ok {
		t.Fatal("changes-requested parsed as a re-review")
	}
}
