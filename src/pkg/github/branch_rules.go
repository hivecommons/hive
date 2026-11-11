package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/workflowmodel/ingest"
)

// FetchBranchRules reads branch's merge requirements for repo
// ("owner/name") for the workflow-model ingest (hivecommons/hive#17620
// phase 5): GET /repos/{o}/{r}/branches/{b}/protection first (needs
// administration:read; 404 for an unprotected branch or a token without
// that scope), then GET /repos/{o}/{r}/rules/branches/{b}, the rulesets
// view every reader may use. It returns nil, nil when neither endpoint
// reports anything; errors are only transport failures.
func (c *Client) FetchBranchRules(ctx context.Context, repo, branch string) (*ingest.BranchRules, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("FetchBranchRules: repo %q must be owner/name", repo)
	}
	prot, resp, err := c.client.Repositories.GetBranchProtection(ctx, owner, name, branch)
	switch {
	case err == nil && prot != nil:
		return branchRulesFromProtection(prot, repo, branch), nil
	case resp == nil || (resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden):
		return nil, fmt.Errorf("FetchBranchRules: %s@%s protection: %w", repo, branch, err)
	}
	rules, resp, err := c.client.Repositories.GetRulesForBranch(ctx, owner, name, branch, nil)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden) {
			return nil, nil
		}
		return nil, fmt.Errorf("FetchBranchRules: %s@%s rules: %w", repo, branch, err)
	}
	return branchRulesFromRulesets(rules, repo, branch), nil
}

// branchRulesFromProtection maps the legacy branch-protection object.
func branchRulesFromProtection(p *gh.Protection, repo, branch string) *ingest.BranchRules {
	r := &ingest.BranchRules{Branch: branch, Source: ingest.BranchRulesProtection, Endpoint: fmt.Sprintf("repos/%s/branches/%s/protection", repo, branch)}
	if s := p.RequiredStatusChecks; s != nil {
		if s.Contexts != nil {
			r.RequiredChecks = append(r.RequiredChecks, *s.Contexts...)
		}
		if s.Checks != nil {
			for _, ck := range *s.Checks {
				if ck != nil && ck.Context != "" {
					r.RequiredChecks = append(r.RequiredChecks, ck.Context)
				}
			}
		}
	}
	if pr := p.RequiredPullRequestReviews; pr != nil {
		r.RequiredApprovals = pr.RequiredApprovingReviewCount
		r.RequireCodeOwnerReview = pr.RequireCodeOwnerReviews
		r.DismissStaleReviews = pr.DismissStaleReviews
	}
	r.EnforceAdmins = p.EnforceAdmins != nil && p.EnforceAdmins.Enabled
	r.LinearHistory = p.RequireLinearHistory != nil && p.RequireLinearHistory.Enabled
	r.SignedCommits = p.RequiredSignatures != nil && p.RequiredSignatures.Enabled != nil && *p.RequiredSignatures.Enabled
	if r.Empty() {
		return nil
	}
	return r
}

// branchRulesFromRulesets maps the rules active on a branch across the
// repository and organisation rulesets.
func branchRulesFromRulesets(b *gh.BranchRules, repo, branch string) *ingest.BranchRules {
	if b == nil {
		return nil
	}
	r := &ingest.BranchRules{Branch: branch, Source: ingest.BranchRulesRulesets, Endpoint: fmt.Sprintf("repos/%s/rules/branches/%s", repo, branch)}
	for _, pr := range b.PullRequest {
		if pr == nil {
			continue
		}
		if pr.Parameters.RequiredApprovingReviewCount > r.RequiredApprovals {
			r.RequiredApprovals = pr.Parameters.RequiredApprovingReviewCount
		}
		r.RequireCodeOwnerReview = r.RequireCodeOwnerReview || pr.Parameters.RequireCodeOwnerReview
		r.DismissStaleReviews = r.DismissStaleReviews || pr.Parameters.DismissStaleReviewsOnPush
	}
	for _, sc := range b.RequiredStatusChecks {
		if sc == nil {
			continue
		}
		for _, ck := range sc.Parameters.RequiredStatusChecks {
			if ck != nil && ck.Context != "" {
				r.RequiredChecks = append(r.RequiredChecks, ck.Context)
			}
		}
	}
	r.LinearHistory = len(b.RequiredLinearHistory) > 0
	r.SignedCommits = len(b.RequiredSignatures) > 0
	if r.Empty() {
		return nil
	}
	return r
}
