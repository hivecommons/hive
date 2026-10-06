package github

import (
	"context"
	"fmt"
)

// ViewerCanUpdateBranch asks GitHub whether the authenticated viewer (including
// an App installation token) can merge the base into this PR's branch. REST's
// maintainer_can_modify flag is not evidence that this particular token can
// write to a fork. A missing or unreadable capability fails closed.
//
// This is only a capability lookup, not authorization to update: callers must
// also require the owner's contributor_prs base-sync setting and use
// UpdateBranchExpectedHead with the evaluated head. It is intentionally opt-in
// so existing direct merge paths make no additional GitHub calls.
func (c *Client) ViewerCanUpdateBranch(ctx context.Context, repo string, number int) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	if number <= 0 {
		return false, fmt.Errorf("ViewerCanUpdateBranch: PR number is required")
	}
	owner, name := c.splitRepo(repo)
	if err := validateRepoRef(owner, name); err != nil {
		return false, fmt.Errorf("ViewerCanUpdateBranch: %w", err)
	}
	const query = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { viewerCanUpdateBranch }
  }
}`
	var data struct {
		Repository *struct {
			PullRequest *struct {
				ViewerCanUpdateBranch *bool `json:"viewerCanUpdateBranch"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphQL(ctx, query, map[string]any{
		"owner": owner, "name": name, "number": number,
	}, &data); err != nil {
		return false, fmt.Errorf("reading branch-update permission for %s/%s#%d: %w", owner, name, number, err)
	}
	if data.Repository == nil || data.Repository.PullRequest == nil || data.Repository.PullRequest.ViewerCanUpdateBranch == nil {
		return false, fmt.Errorf("reading branch-update permission for %s/%s#%d: GitHub omitted viewerCanUpdateBranch", owner, name, number)
	}
	return *data.Repository.PullRequest.ViewerCanUpdateBranch, nil
}
