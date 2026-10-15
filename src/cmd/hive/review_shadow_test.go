package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestReviewShadowGate(t *testing.T) {
	yes := github.MergeableYes
	pr := github.PullRequest{Number: 8, Mergeable: yes, MergeableState: "clean", CIStatus: "success"}
	now := time.Now()
	tests := []struct {
		name       string
		gates      mergeGates
		wantBucket mergeBucket
		wantRecord bool
		wantHeld   bool
	}{
		{"shadow on, no approval: still eligible, would have held", mergeGates{shadowReview: true, reviewLoaded: true}, mergeBucketEligible, true, true},
		{"shadow on, artifact unavailable: still eligible, would have held", mergeGates{shadowReview: true}, mergeBucketEligible, true, true},
		{"shadow off: no record", mergeGates{reviewLoaded: true}, mergeBucketEligible, false, false},
		{"require_approval on: still blocked, no shadow record", mergeGates{requireReviewApproval: true, reviewLoaded: true}, mergeBucketSkip, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bucket, _, _ := classifyMergeEligibility(pr, false, "org/repo", tc.gates)
			if bucket != tc.wantBucket {
				t.Fatalf("bucket = %v, want %v", bucket, tc.wantBucket)
			}
			gate := reviewShadowGate(tc.gates, "org/repo", pr, now)
			if (gate != nil) != tc.wantRecord {
				t.Fatalf("recorded = %v, want %v", gate != nil, tc.wantRecord)
			}
			if gate != nil && *gate.WouldHaveHeld != tc.wantHeld {
				t.Fatalf("would_have_held = %v, want %v", *gate.WouldHaveHeld, tc.wantHeld)
			}
		})
	}
}
