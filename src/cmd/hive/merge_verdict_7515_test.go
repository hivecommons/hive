package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

// hivecommons/hive#7515: hovering a non-green PR pill must say, in plain
// words, why the sweep will not merge it and what would unblock it. Before
// this, a PR GitHub reported as not mergeable got the bare GitHub enum
// ("not mergeable on GitHub (blocked)") even when the classifier had just
// been handed the exact gate — "CI failing: build, lint", "awaiting review
// approval", "held: …" — that made GitHub say blocked. These cases pin the
// wording for every row of the issue's table so the reason cannot be
// dropped again.
func TestClassifyMergeEligibility_BlockedKeepsSweepReason(t *testing.T) {
	no := github.MergeableNo
	blockedPR := func(n int, ci string, failing ...string) github.PullRequest {
		return github.PullRequest{Number: n, Mergeable: no, MergeableState: "blocked", BaseRef: "v4", CIStatus: ci, FailingChecks: failing}
	}
	cases := []struct {
		name       string
		pr         github.PullRequest
		held       bool
		gates      mergeGates
		wantBucket mergeBucket
		wantState  github.MergeVerdictState
		wantReason string // exact
	}{
		{
			name:       "required check red",
			pr:         blockedPR(1, "failure", "build", "lint"),
			gates:      mergeGates{requiredChecks: map[string]bool{"build": true, "lint": true}},
			wantBucket: mergeBucketFailing,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "blocked — CI failing: build, lint",
		},
		{
			// The review gate used to sit behind the GitHub-says-no return,
			// so a PR blocked for want of a review never reached it.
			name:       "review missing",
			pr:         blockedPR(2, "success"),
			gates:      mergeGates{requireReviewApproval: true, reviewLoaded: true},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "blocked — awaiting review approval",
		},
		{
			name:       "review artifact unavailable",
			pr:         blockedPR(3, "success"),
			gates:      mergeGates{requireReviewApproval: true, reviewLoaded: false},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "blocked — review approval required, but review-verdicts.json is unavailable",
		},
		{
			name:       "hold label",
			pr:         blockedPR(4, "success"),
			held:       true,
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "blocked — held: a hold label keeps it out of the sweep",
		},
		{
			name: "intent verdict",
			pr:   blockedPR(5, "success"),
			gates: mergeGates{enforceIntent: true, intentVerdicts: map[string]intent.Verdict{
				"org/repo/5": {AgentPR: true, Authorized: false, Reason: "no authorizing issue"},
			}},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "blocked — intent verification: no authorizing issue",
		},
		{
			// A required check still running is why GitHub says blocked.
			name:       "required check pending",
			pr:         blockedPR(6, "pending"),
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "blocked — CI pending",
		},
		{
			// Every sweep gate passes and GitHub still says blocked: a
			// branch-protection rule the sweep does not read yet. Say so.
			name:       "no sweep gate explains it",
			pr:         blockedPR(7, "success"),
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "blocked — all sweep gates pass; a branch-protection rule is unsatisfied",
		},
		{
			name:       "conflicts",
			pr:         github.PullRequest{Number: 8, Mergeable: no, MergeableState: "dirty", BaseRef: "v4", CIStatus: "success"},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "has merge conflicts with v4 — needs a rebase",
		},
		{
			name:       "conflicts with a sweep reason on top",
			pr:         github.PullRequest{Number: 9, Mergeable: no, MergeableState: "dirty", BaseRef: "v4", CIStatus: "failure", FailingChecks: []string{"build"}},
			wantBucket: mergeBucketFailing,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "has merge conflicts with v4 — needs a rebase; also CI failing: build",
		},
		{
			name:       "behind",
			pr:         github.PullRequest{Number: 10, Mergeable: no, MergeableState: "behind", BaseRef: "v4", CIStatus: "success"},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "behind v4 — needs an update from the base branch",
		},
		{
			// An abbreviated payload with no base ref still reads sensibly.
			name:       "behind with the base branch unknown",
			pr:         github.PullRequest{Number: 11, Mergeable: no, MergeableState: "behind", CIStatus: "success"},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "behind the base branch — needs an update from it",
		},
		{
			name:       "draft",
			pr:         github.PullRequest{Number: 12, Draft: true, Mergeable: github.MergeableYes, CIStatus: "success"},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "draft — mark ready for review to enter the sweep",
		},
		{
			name:       "unknown",
			pr:         github.PullRequest{Number: 13, CIStatus: "pending"},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictUnknown,
			wantReason: "mergeability not yet computed by GitHub — re-checked next tick; CI pending",
		},
		{
			// A GitHub state this function has no wording for still names
			// the state and keeps the sweep reason.
			name:       "an unrecognised GitHub state falls back to naming it",
			pr:         github.PullRequest{Number: 14, Mergeable: no, MergeableState: "draft", CIStatus: "success"},
			held:       true,
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: "not mergeable on GitHub (draft); also held: a hold label keeps it out of the sweep",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, verdict := mergeVerdictOf(t, tc.pr, tc.held, tc.gates)
			if bucket != tc.wantBucket {
				t.Errorf("bucket = %v, want %v", bucket, tc.wantBucket)
			}
			if verdict.State != tc.wantState {
				t.Errorf("state = %q, want %q", verdict.State, tc.wantState)
			}
			if verdict.Reason != tc.wantReason {
				t.Errorf("reason = %q\n   want %q", verdict.Reason, tc.wantReason)
			}
			// The bare enum is never the whole story any more.
			for _, enum := range []string{"(blocked)", "(dirty)", "(behind)"} {
				if strings.Contains(verdict.Reason, enum) {
					t.Errorf("reason %q still shows GitHub's raw state %s", verdict.Reason, enum)
				}
			}
		})
	}
}

// Moving the review gate ahead of the GitHub-says-no return must not change
// which bucket anything lands in: a MergeableNo PR skips either way, and a
// mergeable one never reached that return. The verdict is the only thing
// the order affects.
func TestClassifyMergeEligibility_ReviewGateOrderKeepsBuckets(t *testing.T) {
	gates := mergeGates{requireReviewApproval: true, reviewLoaded: true}
	for _, pr := range []github.PullRequest{
		{Number: 1, Mergeable: github.MergeableNo, MergeableState: "blocked", CIStatus: "success"},
		{Number: 2, Mergeable: github.MergeableNo, MergeableState: "dirty", CIStatus: "success"},
		{Number: 3, Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success"},
	} {
		bucket, verdict := mergeVerdictOf(t, pr, false, gates)
		if bucket != mergeBucketSkip {
			t.Errorf("#%d: bucket = %v, want skip (reason %q)", pr.Number, bucket, verdict.Reason)
		}
		if !strings.Contains(verdict.Reason, "awaiting review approval") {
			t.Errorf("#%d: reason %q does not name the missing review", pr.Number, verdict.Reason)
		}
	}
}
