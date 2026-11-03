package automerge

import (
	"context"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

// hivecommons/hive#12962: the sweep's review verdict gate and the review
// pipeline's human_hold, which the dashboard's your-turn decision reads
// through pipeline.ReviewHolds, agree on whether a person released a
// requires_human hold. The sweep records the release it found; the pipeline
// reads that record through the same hgithub.ReleasesReviewHold rule.
func TestReviewVerdictGateAgreesWithPipeline12962(t *testing.T) {
	after := reviewHoldVerdictAt.Add(90 * time.Minute)
	before := reviewHoldVerdictAt.Add(-time.Hour)
	verdict := review.Aggregate{
		Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Verdict: review.VerdictRequiresHuman, RequiresHuman: true, RecordedAt: reviewHoldVerdictAt,
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveSecurity: review.VerdictRequiresHuman, review.PerspectiveCorrectness: review.VerdictApprove},
	}
	for _, tc := range []struct {
		name     string
		reviews  []reviewHoldActivity
		comments []reviewHoldActivity
		wantHeld bool
	}{
		{name: "person approved after the verdict", reviews: []reviewHoldActivity{{login: "owner", state: "APPROVED", createdAt: after}}},
		{name: "person replied /hive approve after the verdict", comments: []reviewHoldActivity{{login: "owner", body: "/hive approve", createdAt: after}}},
		{name: "only a bot's approval, or a person's before the verdict", wantHeld: true,
			reviews: []reviewHoldActivity{
				{login: testHiveAppBotLogin, userType: "Bot", state: "APPROVED", createdAt: after},
				{login: "owner", state: "APPROVED", createdAt: before},
			},
			comments: []reviewHoldActivity{
				{login: "owner", body: "/hive approve", createdAt: before},
				{login: "renovate[bot]", userType: "Bot", body: "/hive approve", createdAt: after},
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{mergeApplied: true, mergeCalls: &merges})
			defer api.Close()
			srv := newReviewHoldFront(t, api.URL, &reviewHoldFront{reviews: tc.reviews, comments: tc.comments})
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.reviewVerdictHold = verdictHoldFor("acme/widget", requiresHumanHold("sha7"))
			var recorded *hgithub.ReviewHoldRelease
			c.recordReviewHoldRelease = func(repo string, number int, release hgithub.ReviewHoldRelease) {
				if repo == "acme/widget" && number == 7 {
					recorded = &release
				}
			}
			_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil {
				t.Fatalf("sweep err = %v", err)
			}
			sweepHeld := reason == autoMergeReasonReviewRequiresHuman
			if sweepHeld != tc.wantHeld || (sweepHeld && merges != 0) || (!sweepHeld && reason != "") {
				t.Fatalf("sweep reason = %q, merges = %d; want held = %v", reason, merges, tc.wantHeld)
			}
			if (recorded != nil) == tc.wantHeld {
				t.Fatalf("recorded release = %+v; want one only when released", recorded)
			}

			holds := pipeline.ReviewHolds(pipeline.Inputs{
				PR:          hgithub.ReviewQueueEntry{Repo: "acme/widget", Number: 7, HeadSHA: "sha7"},
				Verdicts:    []review.Aggregate{verdict},
				HoldRelease: recorded,
			})
			if boardHeld := len(holds) > 0; boardHeld != sweepHeld {
				t.Fatalf("pipeline holds = %+v but sweep held = %v", holds, sweepHeld)
			}
		})
	}
}
