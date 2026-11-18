package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestContributorLaneOutdatedChangeRequestAsksForReReview pins
// projectbluefin/chairlift#799: a change request left on an older commit the
// author has pushed past waits on its reviewer (re-review-requested), not on
// the author. A request on the head, or carried to it by a base merge, is
// still the author's turn, and a later approval or a dismissal clears both.
func TestContributorLaneOutdatedChangeRequestAsksForReReview(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*contributorLaneFixture)
		wantReason string
		wantMerges int
	}{
		{
			name: "request on an older commit asks its reviewer to re-review",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{humanReview(1, "Danathar", "CHANGES_REQUESTED", "sha6", "three small asks")}
			},
			wantReason: "re-review-requested:Danathar",
		},
		{
			name: "request on the head is the author's turn",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{humanReview(1, "Danathar", "CHANGES_REQUESTED", "sha7", "three small asks")}
			},
			wantReason: contributorLaneReasonChangesRequested,
		},
		{
			name: "one request on the head outranks an outdated one",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{
					humanReview(1, "alice", "CHANGES_REQUESTED", "sha6", "old ask"),
					humanReview(2, "bob", "CHANGES_REQUESTED", "sha7", "new ask"),
				}
			},
			wantReason: contributorLaneReasonChangesRequested,
		},
		{
			name: "every outdated reviewer is named, sorted",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{
					humanReview(1, "zed", "CHANGES_REQUESTED", "sha5", "old ask"),
					humanReview(2, "alice", "CHANGES_REQUESTED", "sha6", "old ask"),
				}
			},
			wantReason: "re-review-requested:alice,zed",
		},
		{
			name: "a later approval from the reviewer clears it",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{
					humanReview(1, "Danathar", "CHANGES_REQUESTED", "sha6", "three small asks"),
					humanReview(2, "Danathar", "APPROVED", "sha7", "thanks"),
				}
			},
			wantMerges: 1,
		},
		{
			name: "a dismissed request clears it",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{humanReview(1, "Danathar", "DISMISSED", "sha6", "three small asks")}
			},
			wantMerges: 1,
		},
		{
			name: "a request with no commit stays the author's turn",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{{"id": 1, "state": "CHANGES_REQUESTED", "user": map[string]string{"login": "maintainer"}}}
			},
			wantReason: contributorLaneReasonChangesRequested,
		},
		{
			// The head is a merge of the base into the reviewed commit: the
			// reviewed change is unchanged, so the author still owes it.
			name: "a base merge carries the request to the head",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{humanReview(1, "Danathar", "CHANGES_REQUESTED", "sha6", "three small asks")}
				fx.headCommit = map[string]any{"sha": "sha7", "parents": []map[string]string{{"sha": "sha6"}, {"sha": "base1"}}}
				files := []map[string]any{{"filename": "pkg/widget/widget.go", "status": "modified"}}
				fx.compareFiles = map[string][]map[string]any{"sha7": files, "sha6": files}
				fx.compareMergeBase = map[string]string{"sha7": "base1", "sha6": "base0"}
			},
			wantReason: contributorLaneReasonChangesRequested,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			tc.mutate(&fx)
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 11, 17, 16, 11, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if reason != tc.wantReason || fx.merges != tc.wantMerges {
				t.Fatalf("reason=%q merges=%d, want %q/%d", reason, fx.merges, tc.wantReason, tc.wantMerges)
			}
		})
	}
}

func TestChangeRequestStandingReason(t *testing.T) {
	if got := (changeRequestStanding{}).reason("changes-requested"); got != "" {
		t.Fatalf("nothing standing: %q, want empty", got)
	}
	if got := (changeRequestStanding{onHead: true, reReview: []string{"bob"}}).reason("changes-requested"); got != "changes-requested" {
		t.Fatalf("on head with an outdated one too: %q, want the author's reason", got)
	}
	if got := (changeRequestStanding{reReview: []string{"bob", "Alice"}}).reason("changes-requested"); got != "re-review-requested:Alice,bob" {
		t.Fatalf("outdated only: %q", got)
	}
}

// TestReReviewRequestedCauseNamesReviewerAndAuthor: the 085 note asks the
// reviewer to re-review and says the author pushed, never that the author
// owes anything or is offline.
func TestReReviewRequestedCauseNamesReviewerAndAuthor(t *testing.T) {
	const head = "948192f343aa"
	const reviewed = "298e8a5849bb"
	url := "https://github.com/projectbluefin/chairlift/pull/799#pullrequestreview-1"
	got := reReviewRequestedCause(head, "castrojo", []string{"Danathar"}, []prstage.ChangeRequestRef{
		{Login: "Danathar", CommitID: reviewed, SubmittedAt: time.Date(2026, 11, 17, 11, 30, 0, 0, time.UTC), URL: url},
	})
	for _, want := range []string{
		"@Danathar requested changes on 2026-11-17 at `" + stagelabels.ShortSHA(reviewed) + "`",
		"@castrojo has pushed since ([changes since that review](https://github.com/projectbluefin/chairlift/compare/" + reviewed + "..." + head + "))",
		"waits on the reviewer, not on @castrojo",
		"@Danathar, please re-review this head",
		"a maintainer can dismiss the review",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cause = %q\nwant it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "offline") || strings.Contains(got, "the hive has pushed") {
		t.Fatalf("cause blames the wrong party: %q", got)
	}
	// From the reason alone (no review refs): still names the reviewer.
	bare := decisionCause(prstage.ReReviewRequestedReason([]string{"Danathar"}))
	if !strings.Contains(bare, "@Danathar requested changes on an older commit, and the author has pushed since") || !strings.Contains(bare, "@Danathar, please re-review this head") {
		t.Fatalf("bare cause = %q", bare)
	}
}

func TestNativeQueueWithdrawsReReviewRequested(t *testing.T) {
	if !nativeQueueWithdrawReason(prstage.ReReviewRequestedReason([]string{"alice"})) {
		t.Fatal("a re-review-requested PR keeps its native queue entry; GitHub will not merge it")
	}
}
