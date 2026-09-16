package github

import (
	"context"
	"strings"
)

// maxRequestedByLookups bounds how many rationale issues resolveRequestedBy
// reads before giving up. A PR normally cites one issue; the cap keeps a
// body that references a dozen from spending a dozen API calls at the PR
// choke point.
const maxRequestedByLookups = 5

// resolveRequestedBy returns the login of the first HUMAN who opened one of the
// issues a PR request offers as its rationale (#7208): the same references the
// self-authorisation gate reads (rationaleIssues — title/body claims plus the
// declared IssueN list), in the same order, so "the issue this PR is for" means
// one thing across both.
//
// Empty when nothing is cited, when every cited issue was filed by the hive
// itself or another bot, or when the lookups fail. Failure is not an error
// here: attribution is a courtesy on top of the PR, and a PR opened without
// it is still correct. A hive-filed issue is deliberately not credited to the
// bot — there is nobody to thank.
func (c *Client) resolveRequestedBy(ctx context.Context, repo, title, body string, declared []int) string {
	if c == nil || c.client == nil {
		return ""
	}
	refs := rationaleIssues(repo, title, body, declared)
	if len(refs) > maxRequestedByLookups {
		refs = refs[:maxRequestedByLookups]
	}
	for _, ref := range refs {
		owner, name := splitRepoRef(ref.Repo, c.org)
		if owner == "" || name == "" {
			continue
		}
		issue, _, err := c.client.Issues.Get(ctx, owner, name, ref.Issue)
		if err != nil {
			c.logger.Debug("requested_by: could not read a cited issue, skipping it",
				"repo", ref.Repo, "issue", ref.Issue, "error", err.Error())
			continue
		}
		if c.isHumanAuthor(issue.GetUser()) {
			return strings.TrimSpace(issue.GetUser().GetLogin())
		}
	}
	return ""
}
