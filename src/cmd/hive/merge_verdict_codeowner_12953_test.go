package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#12953: a ruleset that requires a code owner's review with
// required_approving_review_count 0 leaves GitHub's reviewDecision null while
// the PR is "blocked". With green required checks the classifier used to call
// it eligible ("ignoring non-required GitHub blockers"), and the serialized
// lane held it at the front until the front timeout. It is skipped like a
// REVIEW_REQUIRED PR, naming the rule.
func TestClassifyMergeEligibility_CodeOwnerReviewIsNotEligible12953(t *testing.T) {
	pr := func(decision github.ReviewDecision) github.PullRequest {
		return github.PullRequest{
			Number:             108,
			Mergeable:          github.MergeableNo,
			MergeableState:     "blocked",
			BaseRef:            "main",
			RequestedReviewers: []string{"alice"},
			CIStatus:           "success",
			Protection: &github.ProtectionFacts{
				ReviewDecision:          decision,
				RequiredChecksKnown:     true,
				CodeOwnerReviewRequired: decision == github.ReviewDecisionNone,
				CodeOwnerRuleSource:     `ruleset "protect main"`,
			},
		}
	}

	t.Run("outstanding code-owner review", func(t *testing.T) {
		bucket, verdict, _ := classifyMergeEligibility(pr(github.ReviewDecisionNone), false, "org/repo", mergeGates{})
		if bucket != mergeBucketSkip {
			t.Errorf("bucket = %v, want %v", bucket, mergeBucketSkip)
		}
		if verdict.State != github.MergeVerdictBlocked {
			t.Errorf("state = %q, want %q", verdict.State, github.MergeVerdictBlocked)
		}
		want := `blocked — code-owner review required by ruleset "protect main"`
		if verdict.Reason != want {
			t.Errorf("reason = %q\n    want %q", verdict.Reason, want)
		}
	})

	t.Run("code owner approved", func(t *testing.T) {
		bucket, verdict, _ := classifyMergeEligibility(pr(github.ReviewDecisionApproved), false, "org/repo", mergeGates{})
		if bucket != mergeBucketEligible || verdict.State != github.MergeVerdictEligible {
			t.Errorf("bucket/state = %v/%q, want eligible", bucket, verdict.State)
		}
	})
}
