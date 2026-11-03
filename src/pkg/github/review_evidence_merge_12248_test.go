package github

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
)

// The PR fetch that starts a bundle is a network call; it must not run while
// reviewEvidenceMu is held, or the merge-eligibility poll loop stalls on it.
func TestUpsertReviewEvidenceFetchesPROutsideMutex(t *testing.T) {
	withEvidenceRoot(t)
	c := evidenceTestClient(t, &evidencePRFixture{head: "h1"}, nil)

	lockFree := false
	fetches := 0
	fetchPR := func() *gh.PullRequest {
		fetches++
		if reviewEvidenceMu.TryLock() {
			lockFree = true
			reviewEvidenceMu.Unlock()
		}
		return &gh.PullRequest{
			User: &gh.User{Login: gh.Ptr("alice"), Type: gh.Ptr("User")},
			Base: &gh.PullRequestBranch{SHA: gh.Ptr("base000")},
		}
	}
	c.upsertReviewEvidence("o/r", 7, "h1", reviewEvidenceUpdate{}, evidence.Policy{}, nil, fetchPR, time.Now())

	if fetches == 0 {
		t.Fatal("fetchPR was never called for a new bundle")
	}
	if !lockFree {
		t.Fatal("fetchPR ran while reviewEvidenceMu was held")
	}
}
