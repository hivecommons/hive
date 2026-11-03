package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#12953: a PR GitHub holds for a code owner's review
// (reviewDecision null under a require_code_owner_review rule) is your turn
// exactly as a REVIEW_REQUIRED one is, and the reason names the rule.
func TestPRCodeOwnerReviewIsYourTurn12953(t *testing.T) {
	codeOwner := func(pr github.PullRequest, decision github.ReviewDecision) github.PullRequest {
		pr.MergeableState = "blocked"
		pr.CIStatus = "success"
		pr.RequestedReviewers = []string{"alice"}
		pr.Protection = &github.ProtectionFacts{
			ReviewDecision:          decision,
			CodeOwnerReviewRequired: true,
			CodeOwnerRuleSource:     `ruleset "protect main"`,
		}
		return pr
	}
	const reason = `code-owner review required by ruleset "protect main"`
	cases := []struct {
		name   string
		pr     github.PullRequest
		want   want11565
		reason string
	}{
		{"person's PR, code-owner review outstanding", codeOwner(personPR11565(), github.ReviewDecisionNone), reviewDecision12378, reason},
		{"hive PR, code-owner review outstanding", codeOwner(hivePR11565(), github.ReviewDecisionNone), reviewDecision12378, reason},
		{"code owner approved", codeOwner(personPR11565(), github.ReviewDecisionApproved), notYours11565, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decidePROwnershipGated(tc.pr, nil, false, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
			checkOwnership11565(t, got, tc.want)
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to contain %q", got.Reason, tc.reason)
			}
			_, skipped := tc.pr.HumanReviewBlocksMerge()
			if yours := got.YourTurn && got.ReasonCode == OwnershipReasonReviewDecision; yours != skipped {
				t.Fatalf("merge classifier skips = %v, ownership = %+v", skipped, got)
			}
		})
	}
}
