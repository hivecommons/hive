package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// codeOwnerRule is whether a branch requires a code owner's review, and which
// rule says so (hivecommons/hive#12953).
type codeOwnerRule struct {
	required bool
	source   string
}

// codeOwnerReviewRule reads whether branch requires code-owner review: first
// the rulesets that apply to it (GET /repos/{o}/{r}/rules/branches/{branch},
// Metadata read), then classic branch protection's review enforcement. The
// classic endpoint needs Administration permission an App usually lacks, so a
// 403/404 from it is remembered in the shared forbidden cache and not asked
// again until the cache expires. Any failure reads as "not required": the PR
// then keeps today's classification, never a stronger one.
func (c *Client) codeOwnerReviewRule(ctx context.Context, owner, repo, branch string) codeOwnerRule {
	if c == nil || c.client == nil || owner == "" || repo == "" || strings.TrimSpace(branch) == "" {
		return codeOwnerRule{}
	}
	// go-github's GetRulesForBranch does not escape the branch, so a name with
	// a "/" (release/1.x) would 404; build the path as commit_ci.go does.
	path := fmt.Sprintf("repos/%s/%s/rules/branches/%s?per_page=100", owner, repo, url.PathEscape(branch))
	if req, err := c.client.NewRequest("GET", path, nil); err == nil {
		var rules *gh.BranchRules
		if _, err := c.client.Do(ctx, req, &rules); err == nil && rules != nil {
			for _, rule := range rules.PullRequest {
				if rule != nil && rule.Parameters.RequireCodeOwnerReview {
					return codeOwnerRule{required: true, source: c.rulesetLabel(ctx, owner, repo, rule.RulesetID)}
				}
			}
		}
	}

	key := "code-owner-reviews:" + requiredChecksForbiddenCacheKey(c.client, owner, repo, branch)
	if sharedRequiredChecksForbiddenCache.get(key) {
		return codeOwnerRule{}
	}
	enf, _, err := c.client.Repositories.GetPullRequestReviewEnforcement(ctx, owner, repo, branch)
	if err != nil {
		if isRequiredChecksForbiddenCacheable(err) {
			sharedRequiredChecksForbiddenCache.put(key)
		}
		return codeOwnerRule{}
	}
	if enf != nil && enf.RequireCodeOwnerReviews {
		return codeOwnerRule{required: true, source: "classic branch protection"}
	}
	return codeOwnerRule{}
}

// rulesetLabel names a ruleset for an operator: `ruleset "protect main"`, or
// `ruleset #42` when its name cannot be read.
func (c *Client) rulesetLabel(ctx context.Context, owner, repo string, id int64) string {
	if id <= 0 {
		return "a ruleset"
	}
	if rs, _, err := c.client.Repositories.GetRuleset(ctx, owner, repo, id, true); err == nil && rs != nil && strings.TrimSpace(rs.Name) != "" {
		return "ruleset " + strconv.Quote(strings.TrimSpace(rs.Name))
	}
	return "ruleset #" + strconv.FormatInt(id, 10)
}
