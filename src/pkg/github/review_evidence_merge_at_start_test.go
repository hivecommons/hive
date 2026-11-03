package github

import (
	"reflect"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
)

// A bundle started after the PR already merged (automerge ran before the
// first review wrote one, or someone else merged) records the merge GitHub
// reports instead of leaving the PR with no outcome forever.
func TestUpsertReviewEvidenceRecordsMergeAlreadyOnPR(t *testing.T) {
	withEvidenceRoot(t)
	c := evidenceTestClient(t, &evidencePRFixture{head: "h1"}, nil)

	opened := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	mergedAt := opened.Add(3 * time.Hour)
	fetchPR := func() *gh.PullRequest {
		return &gh.PullRequest{
			User:           &gh.User{Login: gh.Ptr("alice"), Type: gh.Ptr("User")},
			Base:           &gh.PullRequestBranch{SHA: gh.Ptr("base000")},
			Merged:         gh.Ptr(true),
			MergedBy:       &gh.User{Login: gh.Ptr("hive[bot]")},
			MergedAt:       &gh.Timestamp{Time: mergedAt},
			MergeCommitSHA: gh.Ptr("mergesha"),
			CreatedAt:      &gh.Timestamp{Time: opened},
		}
	}
	now := mergedAt.Add(2 * time.Minute)
	c.upsertReviewEvidence("o/r", 7, "h1", reviewEvidenceUpdate{}, evidence.Policy{}, nil, fetchPR, now)

	b, err := LoadReviewEvidence(ReviewEvidencePath("", "o/r", 7, "h1"))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	if b.Merge == nil {
		t.Fatal("merge not recorded on bundle started after the PR merged")
	}
	want := evidence.MergeEvent{Actor: "hive[bot]", Method: reviewEvidenceMergeObservedMethod, At: mergedAt, SHA: "mergesha", OpenedAt: opened}
	if got := *b.Merge; got.Actor != want.Actor || got.Method != want.Method || !got.At.Equal(want.At) || got.SHA != want.SHA || !got.OpenedAt.Equal(want.OpenedAt) || len(got.IssueAuthors) != 0 {
		t.Errorf("merge = %+v, want %+v", got, want)
	}
	if b.Validate() != nil {
		t.Errorf("bundle does not validate: %v", b.Validate())
	}
}

// An open PR starts a bundle with no merge, exactly as before.
func TestUpsertReviewEvidenceOpenPRHasNoMerge(t *testing.T) {
	withEvidenceRoot(t)
	c := evidenceTestClient(t, &evidencePRFixture{head: "h1"}, nil)
	fetchPR := func() *gh.PullRequest {
		return &gh.PullRequest{
			User: &gh.User{Login: gh.Ptr("alice"), Type: gh.Ptr("User")},
			Base: &gh.PullRequestBranch{SHA: gh.Ptr("base000")},
		}
	}
	c.upsertReviewEvidence("o/r", 7, "h1", reviewEvidenceUpdate{}, evidence.Policy{}, nil, fetchPR, time.Now())
	b, err := LoadReviewEvidence(ReviewEvidencePath("", "o/r", 7, "h1"))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	if b.Merge != nil {
		t.Errorf("open PR must not carry a merge event, got %+v", *b.Merge)
	}
}

// Sparse PR metadata still yields a merge event that validates.
func TestReviewEvidenceMergeFromPRFallbacks(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if m := reviewEvidenceMergeFromPR(nil, "h1", now); m != nil {
		t.Errorf("nil PR: got %+v", *m)
	}
	if m := reviewEvidenceMergeFromPR(&gh.PullRequest{}, "h1", now); m != nil {
		t.Errorf("unmerged PR: got %+v", *m)
	}
	m := reviewEvidenceMergeFromPR(&gh.PullRequest{Merged: gh.Ptr(true)}, "h1", now)
	want := evidence.MergeEvent{Actor: reviewEvidenceMergeObservedMethod, Method: reviewEvidenceMergeObservedMethod, At: now, SHA: "h1"}
	if m == nil || !reflect.DeepEqual(*m, want) {
		t.Errorf("sparse merged PR: got %+v, want %+v", m, want)
	}
	// opened_at after the merge (clock skew) is dropped, not recorded negative.
	m = reviewEvidenceMergeFromPR(&gh.PullRequest{Merged: gh.Ptr(true), MergedAt: &gh.Timestamp{Time: now}, CreatedAt: &gh.Timestamp{Time: now.Add(time.Hour)}}, "h1", now)
	if m == nil || !m.OpenedAt.IsZero() {
		t.Errorf("skewed opened_at must be dropped, got %+v", m)
	}
}
