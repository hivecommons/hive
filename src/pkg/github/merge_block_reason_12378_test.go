package github

import "testing"

// hivecommons/hive#12378: one predicate for "a person's review decision blocks
// the merge", read by the merge classifier and the your-turn decision.
func TestHumanReviewBlocksMerge12378(t *testing.T) {
	tests := []struct {
		name       string
		protection *ProtectionFacts
		want       string
		wantBlocks bool
	}{
		{name: "no facts", protection: nil},
		{name: "approved", protection: &ProtectionFacts{ReviewDecision: ReviewDecisionApproved}},
		{
			name:       "review required",
			protection: &ProtectionFacts{ReviewDecision: ReviewDecisionReviewRequired, ApprovalsGiven: 1},
			want:       "an approving review is required by branch protection (1 given)",
			wantBlocks: true,
		},
		{
			name:       "changes requested",
			protection: &ProtectionFacts{ReviewDecision: ReviewDecisionChangesRequested, ChangesRequestedBy: []string{"bob"}},
			want:       "changes requested by @bob",
			wantBlocks: true,
		},
		{
			name:       "review required behind a failing required check names the check",
			protection: &ProtectionFacts{ReviewDecision: ReviewDecisionReviewRequired, RequiredChecksKnown: true, FailingRequiredChecks: []string{"build"}},
			want:       `required check "build" is failing`,
			wantBlocks: true,
		},
		{
			name:       "failing check without a review decision is not a review block",
			protection: &ProtectionFacts{RequiredChecksKnown: true, FailingRequiredChecks: []string{"build"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, blocks := PullRequest{Protection: tc.protection}.HumanReviewBlocksMerge()
			if blocks != tc.wantBlocks || got != tc.want {
				t.Fatalf("HumanReviewBlocksMerge() = (%q, %v), want (%q, %v)", got, blocks, tc.want, tc.wantBlocks)
			}
		})
	}
}
