package github

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// versionTestPRs is a small mixed queue: an agent coverage PR, a contributor
// fix and a contributor docs PR, all opened at the same time.
func versionTestPRs() []PullRequest {
	opened := reviewQueueTestNow.Add(-6 * time.Hour)
	return []PullRequest{
		{Repo: "acme/app", Number: 10, Title: "[quality] test: cover rescan", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"agent/quality"}, HeadSHA: "aaa", CIStatus: "success", CreatedAt: opened},
		{Repo: "acme/app", Number: 11, Title: "fix: nil deref on restart", Author: "alice", HeadSHA: "bbb", CIStatus: "pending", CreatedAt: opened},
		{Repo: "acme/app", Number: 12, Title: "docs: explain the queue", Author: "bob", HeadSHA: "ccc", CIStatus: "success", CreatedAt: opened},
	}
}

func versionTestOpts(items ...review.Aggregate) ReviewQueueOptions {
	return ReviewQueueOptions{Now: reviewQueueTestNow, Verdicts: review.Artifact{Items: items}}
}

func queueVersion(prs []PullRequest, opts ReviewQueueOptions) (string, []ReviewQueueEntry) {
	q := BuildReviewQueue(prs, opts)
	return ReviewQueueVersion(q), q
}

// The same inputs always give the same version, whatever order the PRs were
// enumerated in, and computing it leaves the queue untouched.
func TestReviewQueueVersion_StableForSameInputs(t *testing.T) {
	prs := versionTestPRs()
	opts := versionTestOpts(queueVerdict("acme/app", 10, "aaa", review.ConfidenceMax))
	v1, q1 := queueVersion(prs, opts)
	before := append([]ReviewQueueEntry(nil), q1...)
	if again := ReviewQueueVersion(q1); again != v1 {
		t.Fatalf("version not stable: %q then %q", v1, again)
	}
	if !reflect.DeepEqual(before, q1) {
		t.Fatal("ReviewQueueVersion modified the queue")
	}
	reversed := []PullRequest{prs[2], prs[1], prs[0]}
	if v2, _ := queueVersion(reversed, opts); v2 != v1 {
		t.Fatalf("enumeration order changed the version: %q vs %q", v1, v2)
	}
	if len(v1) != reviewQueueVersionHexLen {
		t.Fatalf("version %q has length %d, want %d", v1, len(v1), reviewQueueVersionHexLen)
	}
	if ReviewQueueVersion(nil) != ReviewQueueVersion([]ReviewQueueEntry{}) {
		t.Fatal("nil and empty queues must share a version")
	}
	if ReviewQueueVersion(nil) == v1 {
		t.Fatal("empty queue shares a version with a populated one")
	}
}

// Each rank input #9590 names ("re-rank when commits, CI results, reviews or
// conflicts change", and drop PRs on merge or close) must invalidate the
// version, so a consumer comparing versions never keeps a stale rank.
func TestReviewQueueVersion_InvalidatedByEachRankInput(t *testing.T) {
	baseOpts := versionTestOpts(queueVerdict("acme/app", 10, "aaa", review.ConfidenceMax))
	base, baseQ := queueVersion(versionTestPRs(), baseOpts)

	cases := []struct {
		name   string
		mutate func(prs []PullRequest, opts *ReviewQueueOptions) []PullRequest
		check  func(t *testing.T, q []ReviewQueueEntry)
	}{
		{
			name: "new commits (head SHA) drop the old-head verdict",
			mutate: func(prs []PullRequest, _ *ReviewQueueOptions) []PullRequest {
				prs[0].HeadSHA = "aaa2"
				return prs
			},
			check: func(t *testing.T, q []ReviewQueueEntry) {
				if e := findEntry(t, q, "acme/app", 10); e.Reviewed || e.HeadSHA != "aaa2" {
					t.Fatalf("pushed PR still reviewed or stale head: %+v", e)
				}
			},
		},
		{
			name: "CI state change",
			mutate: func(prs []PullRequest, _ *ReviewQueueOptions) []PullRequest {
				prs[1].CIStatus = "failure"
				return prs
			},
			check: func(t *testing.T, q []ReviewQueueEntry) {
				if e := findEntry(t, q, "acme/app", 11); e.CIState != ReviewQueueCIRed {
					t.Fatalf("CI state = %q, want red", e.CIState)
				}
			},
		},
		{
			name: "new review verdict for the current head",
			mutate: func(prs []PullRequest, opts *ReviewQueueOptions) []PullRequest {
				opts.Verdicts.Items = append(opts.Verdicts.Items, queueVerdict("acme/app", 11, "bbb", 0))
				return prs
			},
			check: func(t *testing.T, q []ReviewQueueEntry) {
				if e := findEntry(t, q, "acme/app", 11); !e.Reviewed || e.ConfidenceBand != ReviewQueueBandDoNotMerge {
					t.Fatalf("verdict not reflected: %+v", e)
				}
			},
		},
		{
			name: "merge conflict appears",
			mutate: func(prs []PullRequest, _ *ReviewQueueOptions) []PullRequest {
				prs[2].MergeableState = reviewQueueMergeableStateDirty
				return prs
			},
			check: func(t *testing.T, q []ReviewQueueEntry) {
				if e := findEntry(t, q, "acme/app", 12); !hasReason(e, "has merge conflicts") {
					t.Fatalf("conflict not reflected: %v", e.Reasons)
				}
			},
		},
		{
			name: "PR merged or closed (leaves the enumeration)",
			mutate: func(prs []PullRequest, _ *ReviewQueueOptions) []PullRequest {
				return prs[:2]
			},
			check: func(t *testing.T, q []ReviewQueueEntry) {
				if len(q) != 2 {
					t.Fatalf("queue length = %d, want 2", len(q))
				}
			},
		},
		{
			name: "PR put on hold",
			mutate: func(prs []PullRequest, opts *ReviewQueueOptions) []PullRequest {
				opts.Held = map[string]bool{ReviewQueueKey("acme/app", 12): true}
				return prs
			},
			check: func(t *testing.T, q []ReviewQueueEntry) {
				if e := findEntry(t, q, "acme/app", 12); !e.Held {
					t.Fatal("hold not reflected")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := baseOpts
			opts.Verdicts.Items = append([]review.Aggregate(nil), baseOpts.Verdicts.Items...)
			prs := tc.mutate(versionTestPRs(), &opts)
			got, q := queueVersion(prs, opts)
			tc.check(t, q)
			if got == base {
				t.Fatalf("version %q unchanged after %s", got, tc.name)
			}
		})
	}

	// The base queue itself was not disturbed by the mutations above.
	if again, _ := queueVersion(versionTestPRs(), baseOpts); again != base {
		t.Fatalf("base version drifted: %q vs %q", base, again)
	}
	if got := queueKeys(baseQ); !reflect.DeepEqual(got, []string{"acme/app#11", "acme/app#12", "acme/app#10"}) {
		t.Fatalf("base order = %v", got)
	}
}

// Inputs the rank does NOT depend on must not churn the version: a verdict
// recorded for a head the PR no longer has, or one for a PR outside the
// queue, leaves both the order and the version exactly as they were.
func TestReviewQueueVersion_UnchangedByIrrelevantInputs(t *testing.T) {
	baseOpts := versionTestOpts(queueVerdict("acme/app", 10, "aaa", review.ConfidenceMax))
	base, baseQ := queueVersion(versionTestPRs(), baseOpts)

	opts := baseOpts
	opts.Verdicts.Items = append(append([]review.Aggregate(nil), baseOpts.Verdicts.Items...),
		queueVerdict("acme/app", 11, "old-head", 0),
		queueVerdict("acme/other", 99, "zzz", review.ConfidenceMax),
	)
	got, q := queueVersion(versionTestPRs(), opts)
	if got != base {
		t.Fatalf("irrelevant verdicts changed the version: %q vs %q", base, got)
	}
	if !reflect.DeepEqual(queueKeys(q), queueKeys(baseQ)) {
		t.Fatalf("irrelevant verdicts changed the order: %v vs %v", queueKeys(q), queueKeys(baseQ))
	}
}
