package github

import "testing"

// TestBranchProtectionBlockReason is the pin for the ONE place that maps a
// blocked PR's branch-protection facts to the words an operator reads in the
// dashboard tooltip (hivecommons/hive#7515). Every row asserts the EXACT
// string, so a rewording anywhere has to come through here.
func TestBranchProtectionBlockReason(t *testing.T) {
	tests := []struct {
		name    string
		pr      PullRequest
		want    string
		wantOK  bool
		comment string
	}{
		{
			name: "no facts at all declines to guess",
			pr:   PullRequest{Repo: "org/repo", Number: 1},
			// Negative control: the function must NOT return a plausible
			// default. "unknown" is the honest answer and the caller prints
			// its own placeholder.
			wantOK: false,
		},
		{
			name: "facts present but empty declines to guess",
			pr:   PullRequest{Protection: &ProtectionFacts{}},
			// Second negative control: a non-nil Protection whose every
			// field is the zero value is still "we determined nothing".
			wantOK: false,
		},
		{
			name: "required set known and everything green declines to guess",
			pr: PullRequest{Protection: &ProtectionFacts{
				RequiredChecksKnown: true,
				ReviewDecision:      ReviewDecisionApproved,
			}},
			// Third negative control: approved + no red/absent required
			// check means the rule is one we do not read (a merge-queue
			// rule, a CODEOWNERS rule). Do not invent one.
			wantOK: false,
		},
		{
			name: "changes requested names the reviewer",
			pr: PullRequest{Protection: &ProtectionFacts{
				ReviewDecision:     ReviewDecisionChangesRequested,
				ChangesRequestedBy: []string{"reviewer"},
			}},
			want:   "changes requested by @reviewer",
			wantOK: true,
		},
		{
			name: "changes requested by several, deduped, @-stripped, sorted",
			pr: PullRequest{Protection: &ProtectionFacts{
				ReviewDecision:     ReviewDecisionChangesRequested,
				ChangesRequestedBy: []string{"zoe", "@alice", "zoe", ""},
			}},
			want:   "changes requested by @alice, @zoe",
			wantOK: true,
		},
		{
			name: "changes requested with no login still says so",
			pr: PullRequest{Protection: &ProtectionFacts{
				ReviewDecision:     ReviewDecisionChangesRequested,
				ChangesRequestedBy: nil,
			}},
			want:   "a reviewer requested changes",
			wantOK: true,
		},
		{
			name: "one failing required check",
			pr: PullRequest{Protection: &ProtectionFacts{
				RequiredChecksKnown:   true,
				FailingRequiredChecks: []string{"build"},
			}},
			want:   `required check "build" is failing`,
			wantOK: true,
		},
		{
			name: "two failing required checks pluralise",
			pr: PullRequest{Protection: &ProtectionFacts{
				RequiredChecksKnown:   true,
				FailingRequiredChecks: []string{"lint", "build"},
			}},
			want:   `required checks "build", "lint" are failing`,
			wantOK: true,
		},
		{
			name: "required check that never reported",
			pr: PullRequest{Protection: &ProtectionFacts{
				RequiredChecksKnown:   true,
				MissingRequiredChecks: []string{"validate"},
			}},
			want:   `required check "validate" has not reported`,
			wantOK: true,
		},
		{
			name: "two required checks that never reported pluralise",
			pr: PullRequest{Protection: &ProtectionFacts{
				RequiredChecksKnown:   true,
				MissingRequiredChecks: []string{"validate", "dco"},
			}},
			want:   `required checks "dco", "validate" have not reported`,
			wantOK: true,
		},
		{
			name: "missing required checks are ignored when the set is not known",
			pr: PullRequest{Protection: &ProtectionFacts{
				RequiredChecksKnown:   false,
				MissingRequiredChecks: []string{"validate"},
				ReviewDecision:        ReviewDecisionReviewRequired,
			}},
			// Fourth negative control: without a known required set the
			// check lists mean nothing, so the review rule is what is left.
			want:   "an approving review is required by branch protection (0 given)",
			wantOK: true,
		},
		{
			name: "review required names the shortfall",
			pr: PullRequest{Protection: &ProtectionFacts{
				ReviewDecision: ReviewDecisionReviewRequired,
				ApprovalsGiven: 1,
			}},
			want:   "an approving review is required by branch protection (1 given)",
			wantOK: true,
		},
		{
			name: "changes requested outranks a red required check",
			pr: PullRequest{Protection: &ProtectionFacts{
				ReviewDecision:        ReviewDecisionChangesRequested,
				ChangesRequestedBy:    []string{"alice"},
				RequiredChecksKnown:   true,
				FailingRequiredChecks: []string{"build"},
			}},
			want:   "changes requested by @alice",
			wantOK: true,
		},
		{
			name: "a red required check outranks one that never reported",
			pr: PullRequest{Protection: &ProtectionFacts{
				RequiredChecksKnown:   true,
				FailingRequiredChecks: []string{"build"},
				MissingRequiredChecks: []string{"validate"},
			}},
			want:   `required check "build" is failing`,
			wantOK: true,
		},
		{
			name: "a red required check outranks a missing approval",
			pr: PullRequest{Protection: &ProtectionFacts{
				ReviewDecision:        ReviewDecisionReviewRequired,
				RequiredChecksKnown:   true,
				FailingRequiredChecks: []string{"build"},
			}},
			want:   `required check "build" is failing`,
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.pr.BranchProtectionBlockReason()
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (reason %q)", ok, tt.wantOK, got)
			}
			if got != tt.want {
				t.Errorf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBranchProtectionBlockReason_NeverInventsAReason is the invariant behind
// the negative controls above, stated once: whenever the function claims a
// reason, some fact must support it. A build that returns a hardcoded string
// for everything passes each individual row's "want" only by accident; this
// one fails it outright.
func TestBranchProtectionBlockReason_NeverInventsAReason(t *testing.T) {
	silent := []PullRequest{
		{},
		{Protection: &ProtectionFacts{}},
		{Protection: &ProtectionFacts{ReviewDecision: ReviewDecisionApproved}},
		{Protection: &ProtectionFacts{RequiredChecksKnown: true}},
		{Protection: &ProtectionFacts{ReviewDecision: ReviewDecisionNone, RequiredChecksKnown: true}},
		{Protection: &ProtectionFacts{FailingRequiredChecks: []string{"build"}}},
		{Protection: &ProtectionFacts{MissingRequiredChecks: []string{"validate"}}},
	}
	for i, pr := range silent {
		if got, ok := pr.BranchProtectionBlockReason(); ok {
			t.Errorf("case %d: claimed %q with nothing to support it", i, got)
		}
	}

	// ...and the reasons it DOES give must differ from one another, so a
	// single hardcoded string cannot satisfy the table above.
	distinct := map[string]bool{}
	for _, pr := range []PullRequest{
		{Protection: &ProtectionFacts{ReviewDecision: ReviewDecisionChangesRequested, ChangesRequestedBy: []string{"alice"}}},
		{Protection: &ProtectionFacts{RequiredChecksKnown: true, FailingRequiredChecks: []string{"build"}}},
		{Protection: &ProtectionFacts{RequiredChecksKnown: true, MissingRequiredChecks: []string{"validate"}}},
		{Protection: &ProtectionFacts{ReviewDecision: ReviewDecisionReviewRequired}},
	} {
		got, ok := pr.BranchProtectionBlockReason()
		if !ok {
			t.Fatalf("expected a reason for %+v", pr.Protection)
		}
		if distinct[got] {
			t.Errorf("reason %q is not specific to its facts", got)
		}
		distinct[got] = true
	}
	if len(distinct) != 4 {
		t.Errorf("got %d distinct reasons, want 4", len(distinct))
	}
}
