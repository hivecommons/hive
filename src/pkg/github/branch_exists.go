package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// BranchExists reports whether repo has the named branch. A GitHub 404 is a
// definitive negative; authentication, rate-limit, and transport failures are
// returned so callers do not silently redirect work to another base.
func (c *Client) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	branch = strings.TrimSpace(branch)
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	if owner == "" || name == "" || branch == "" {
		return false, fmt.Errorf("repo and branch are required")
	}
	_, resp, err := c.client.Repositories.GetBranch(ctx, owner, name, branch, 3)
	if err == nil {
		return true, nil
	}
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("checking branch %s on %s/%s: %w", branch, owner, name, err)
}
