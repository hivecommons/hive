package github

import (
	"encoding/json"
	"testing"
)

// hivecommons/hive#19325: the PR batch fed the review-state cache with only
// reviewDecision, so a PR whose MEMBER reviewer requested changes read as
// "no human review yet". The batch now carries latestOpinionatedReviews and
// derives the same state as the per-repo review query.
func TestGraphQLPRBatchReviewStateCarriesLatestHumanReview(t *testing.T) {
	c := humanCommentTestClient(t, "http://127.0.0.1:1")
	var n graphQLPRBatchNode
	if err := json.Unmarshal([]byte(`{"number":424,"reviewDecision":"CHANGES_REQUESTED",
		"latestOpinionatedReviews":{"nodes":[
			{"state":"CHANGES_REQUESTED","submittedAt":"2026-11-18T20:25:19Z","authorAssociation":"MEMBER","author":{"__typename":"User","login":"nate"}},
			{"state":"APPROVED","submittedAt":"2026-11-18T21:00:00Z","authorAssociation":"NONE","author":{"__typename":"User","login":"stranger"}},
			{"state":"APPROVED","submittedAt":"2026-11-18T22:00:00Z","authorAssociation":"NONE","author":{"__typename":"Bot","login":"hive[bot]"}}
		]},
		"comments":{"totalCount":3},"reviewThreads":{"totalCount":7}}`), &n); err != nil {
		t.Fatal(err)
	}
	c.storeGraphQLPRBatchReview("cncf/techdocs", n)
	states, ok := c.graphQLBatchReviewStates("cncf/techdocs")
	if !ok {
		t.Fatal("batch review states not cached")
	}
	st := states[424]
	if st.decision != ReviewDecisionChangesRequested || st.latestHumanState != ReviewDecisionChangesRequested || st.latestHumanBy != "nate" {
		t.Fatalf("state = %+v, want CHANGES_REQUESTED by nate", st)
	}
	if st.latestHumanAt.IsZero() || st.approvals != 2 || len(st.changesRequestedBy) != 1 || st.comments != 3 || st.reviewThreads != 7 {
		t.Fatalf("state = %+v", st)
	}
}
