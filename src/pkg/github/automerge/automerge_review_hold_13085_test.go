package automerge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

// hivecommons/hive#13085: on the contributor lane a trusted merger (here the
// hive owner "bob", who may also be the PR's author) releases a
// requires_human verdict with an approving review or a `/hive approve` reply
// posted after it, as on the self-authored lane. The release is recorded in
// the ledger the dashboard's your-turn decision reads, so the board stops
// holding the PR; anyone who is not a trusted merger cannot release it.
func TestContributorLaneReviewHoldRelease13085(t *testing.T) {
	after := reviewHoldVerdictAt.Add(90 * time.Minute)
	before := reviewHoldVerdictAt.Add(-time.Hour)
	requiresHuman := ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	for _, tc := range []struct {
		name         string
		verdict      ContributorLaneVerdict
		hold         ReviewVerdictHoldFunc
		reviews      []reviewHoldActivity
		comments     []reviewHoldActivity
		wantApprover string
		wantNoCalls  bool
	}{
		{name: "owner /hive approve after the verdict releases", verdict: requiresHuman, hold: verdictHoldFor("acme/widget", requiresHumanHold("sha7")),
			comments:     []reviewHoldActivity{{login: "bob", body: "/hive approve\nMerge once CI is green on the current head.", createdAt: after}},
			wantApprover: "bob"},
		{name: "trusted merger approving review after the verdict releases", verdict: requiresHuman, hold: verdictHoldFor("acme/widget", requiresHumanHold("sha7")),
			reviews:      []reviewHoldActivity{{login: "carol", state: "APPROVED", createdAt: after}},
			wantApprover: "carol"},
		{name: "owner /hive approve before the verdict does not release", verdict: requiresHuman, hold: verdictHoldFor("acme/widget", requiresHumanHold("sha7")),
			comments: []reviewHoldActivity{{login: "bob", body: "/hive approve", createdAt: before}}},
		{name: "contributor's own /hive approve does not release", verdict: requiresHuman, hold: verdictHoldFor("acme/widget", requiresHumanHold("sha7")),
			comments: []reviewHoldActivity{{login: "mallory", body: "/hive approve", createdAt: after}}},
		{name: "bot /hive approve does not release", verdict: requiresHuman, hold: verdictHoldFor("acme/widget", requiresHumanHold("sha7")),
			comments: []reviewHoldActivity{{login: "renovate[bot]", userType: "Bot", body: "/hive approve", createdAt: after}}},
		{name: "a verdict that does not need a person costs no API call", verdict: ContributorLaneVerdict{Configured: true, Verdict: "approve", Approved: true},
			hold: verdictHoldFor("acme/widget", requiresHumanHold("sha7")), wantNoCalls: true,
			comments: []reviewHoldActivity{{login: "bob", body: "/hive approve", createdAt: after}}},
		{name: "no recorded hold releases nothing", verdict: requiresHuman, hold: verdictHoldFor("acme/other", requiresHumanHold("sha7")), wantNoCalls: true,
			comments: []reviewHoldActivity{{login: "bob", body: "/hive approve", createdAt: after}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.NotFoundHandler())
			defer api.Close()
			front := &reviewHoldFront{reviews: tc.reviews, comments: tc.comments}
			srv := newReviewHoldFront(t, api.URL, front)
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.reviewVerdictHold = tc.hold
			var recorded *hgithub.ReviewHoldRelease
			c.recordReviewHoldRelease = func(repo string, number int, release hgithub.ReviewHoldRelease) {
				if repo == "acme/widget" && number == 7 {
					recorded = &release
				}
			}
			approver, released := c.contributorLaneReviewHoldRelease(context.Background(), "acme/widget", "acme", "widget", 7, "sha7", tc.verdict)
			if approver != tc.wantApprover || released != (tc.wantApprover != "") {
				t.Fatalf("release = (%q, %v), want approver %q", approver, released, tc.wantApprover)
			}
			if (recorded != nil) != released {
				t.Fatalf("recorded release = %+v, want one only when released", recorded)
			}
			if tc.wantNoCalls && front.hits != 0 {
				t.Fatalf("listed reviews/comments %d times, want none", front.hits)
			}

			// The dashboard's your-turn decision reads the recorded release
			// through pipeline.ReviewHolds; it must agree with the lane.
			holds := pipeline.ReviewHolds(pipeline.Inputs{
				PR: hgithub.ReviewQueueEntry{Repo: "acme/widget", Number: 7, HeadSHA: "sha7"},
				Verdicts: []review.Aggregate{{
					Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Verdict: review.VerdictRequiresHuman, RequiresHuman: true, RecordedAt: reviewHoldVerdictAt,
					Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveSecurity: review.VerdictRequiresHuman},
				}},
				HoldRelease: recorded,
			})
			if tc.verdict.Approved {
				return
			}
			if boardHeld := len(holds) > 0; boardHeld == released {
				t.Fatalf("pipeline holds = %+v but lane released = %v", holds, released)
			}
		})
	}
}
