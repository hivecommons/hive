package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#7515 step 2: when GitHub says "blocked", the tooltip must
// name the branch-protection rule that is unsatisfied, not the word
// "blocked". Step 1 (#7516) kept the SWEEP's own gate when it had one; these
// cases pin what happens with GitHub's own facts — a review decision, a red
// or absent required check — which the sweep now collects during enumeration.
//
// The wording itself lives in github.PullRequest.BranchProtectionBlockReason
// (pinned by TestBranchProtectionBlockReason); what is pinned here is that
// the classifier reaches for it, and that it still declines to guess.
func TestClassifyMergeEligibility_BlockedNamesTheProtectionRule(t *testing.T) {
	blocked := func(p *github.ProtectionFacts) github.PullRequest {
		return github.PullRequest{
			Number:         1,
			Mergeable:      github.MergeableNo,
			MergeableState: "blocked",
			BaseRef:        "v4",
			CIStatus:       "success",
			Protection:     p,
		}
	}

	cases := []struct {
		name       string
		pr         github.PullRequest
		gates      mergeGates
		wantReason string // exact
	}{
		{
			name: "changes requested is named where the placeholder used to be",
			pr: blocked(&github.ProtectionFacts{
				ReviewDecision:     github.ReviewDecisionChangesRequested,
				ChangesRequestedBy: []string{"reviewer"},
			}),
			wantReason: "blocked — changes requested by @reviewer",
		},
		{
			name: "a required check that never reported is named",
			pr: blocked(&github.ProtectionFacts{
				RequiredChecksKnown:   true,
				MissingRequiredChecks: []string{"validate"},
			}),
			wantReason: `blocked — required check "validate" has not reported`,
		},
		{
			name: "a review GitHub requires is named",
			pr: blocked(&github.ProtectionFacts{
				ReviewDecision: github.ReviewDecisionReviewRequired,
			}),
			wantReason: "blocked — an approving review is required by branch protection (0 given)",
		},
		{
			// NEGATIVE CONTROL. Facts were gathered and they explain
			// nothing: the honest placeholder must survive. A build that
			// always prints a derived rule fails here.
			name: "no rule derivable keeps the honest placeholder",
			pr: blocked(&github.ProtectionFacts{
				RequiredChecksKnown: true,
				ReviewDecision:      github.ReviewDecisionApproved,
			}),
			wantReason: "blocked — all sweep gates pass; a branch-protection rule is unsatisfied",
		},
		{
			// NEGATIVE CONTROL. No facts at all (an older governor, a repo
			// whose GraphQL query failed) must read exactly as it did after
			// step 1 — this is the #7516 regression guard.
			name:       "no facts at all keeps the step-1 placeholder",
			pr:         blocked(nil),
			wantReason: "blocked — all sweep gates pass; a branch-protection rule is unsatisfied",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, verdict, _ := classifyMergeEligibility(tc.pr, false, "org/repo", tc.gates)
			if bucket != mergeBucketSkip {
				t.Errorf("bucket = %v, want %v", bucket, mergeBucketSkip)
			}
			if verdict.State != github.MergeVerdictBlocked {
				t.Errorf("state = %q, want %q", verdict.State, github.MergeVerdictBlocked)
			}
			if verdict.Reason != tc.wantReason {
				t.Errorf("reason = %q\n    want %q", verdict.Reason, tc.wantReason)
			}
		})
	}
}

// When the sweep has its own gate AND GitHub names a different rule, the
// operator needs both: clearing the sweep's gate alone will not merge the PR.
// When the two say the same thing, it must not be said twice.
func TestClassifyMergeEligibility_BlockedKeepsBothReasons(t *testing.T) {
	base := func(p *github.ProtectionFacts) github.PullRequest {
		return github.PullRequest{
			Number:         2,
			Mergeable:      github.MergeableNo,
			MergeableState: "blocked",
			BaseRef:        "v4",
			CIStatus:       "failure",
			FailingChecks:  []string{"build"},
			Protection:     p,
		}
	}
	gates := mergeGates{requiredChecks: map[string]bool{"build": true}}

	t.Run("different rules are both reported", func(t *testing.T) {
		pr := base(&github.ProtectionFacts{
			ReviewDecision:     github.ReviewDecisionChangesRequested,
			ChangesRequestedBy: []string{"reviewer"},
		})
		_, verdict, _ := classifyMergeEligibility(pr, false, "org/repo", gates)
		want := "blocked — CI failing: build; GitHub also requires: changes requested by @reviewer"
		if verdict.Reason != want {
			t.Errorf("reason = %q\n    want %q", verdict.Reason, want)
		}
	})

	t.Run("the same rule is not said twice", func(t *testing.T) {
		// The sweep's reason already contains the derived rule verbatim.
		pr := github.PullRequest{
			Number:         3,
			Mergeable:      github.MergeableNo,
			MergeableState: "blocked",
			BaseRef:        "v4",
			CIStatus:       "success",
			Protection: &github.ProtectionFacts{
				ReviewDecision:     github.ReviewDecisionChangesRequested,
				ChangesRequestedBy: []string{"reviewer"},
			},
		}
		// Feed the intent gate a reason that literally contains the rule.
		_, verdict, _ := classifyMergeEligibility(pr, true, "org/repo", mergeGates{})
		if verdict.Reason != "blocked — held: a hold label keeps it out of the sweep; GitHub also requires: changes requested by @reviewer" {
			t.Errorf("held+changes-requested reason = %q", verdict.Reason)
		}
	})
}

// A state OTHER than "blocked" must not sprout a branch-protection rule:
// "dirty" is a conflict and "behind" is a stale branch, and neither is a
// protection rule the operator can clear by getting a review.
func TestClassifyMergeEligibility_ProtectionRuleOnlyAppliesToBlocked(t *testing.T) {
	facts := &github.ProtectionFacts{
		ReviewDecision:     github.ReviewDecisionChangesRequested,
		ChangesRequestedBy: []string{"reviewer"},
	}
	for _, tc := range []struct {
		state string
		want  string
	}{
		{"dirty", "has merge conflicts with v4 — needs a rebase"},
		{"behind", "behind v4 — needs an update from the base branch"},
	} {
		pr := github.PullRequest{
			Number: 4, Mergeable: github.MergeableNo, MergeableState: tc.state,
			BaseRef: "v4", CIStatus: "success", Protection: facts,
		}
		_, verdict, _ := classifyMergeEligibility(pr, false, "org/repo", mergeGates{})
		if verdict.Reason != tc.want {
			t.Errorf("%s reason = %q\n    want %q", tc.state, verdict.Reason, tc.want)
		}
	}
}
