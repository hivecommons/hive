package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// branchNamesMaxPages bounds ListBranchNames: 20 pages of 100 covers any
// repository the contribute queue dispatches for, and a repository with more
// branches than that gets a truncated list rather than an unbounded walk.
const branchNamesMaxPages = 20

// ListBranchNames returns the names of repo's branches ("owner/name", or a
// bare name under the client's org), paginated to branchNamesMaxPages.
//
// The contribute dispatcher uses it to validate the target branch an issue
// declares (`[v6]` / `Target: v6`, hivecommons/hive#13146) before telling an
// agent to base its work there: a declared branch that does not exist falls
// back to the repository's default rather than failing at `gh pr create`. The
// list is uncached here; the dispatcher caches per repo per sweep.
func (c *Client) ListBranchNames(ctx context.Context, repo string) ([]string, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	if owner == "" || name == "" {
		return nil, errors.New("ListBranchNames: owner and repo are required")
	}
	opts := &gh.BranchListOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var names []string
	for page := 0; page < branchNamesMaxPages; page++ {
		branches, resp, err := c.client.Repositories.ListBranches(ctx, owner, name, opts)
		if err != nil {
			return nil, fmt.Errorf("listing branches for %s/%s: %w", owner, name, err)
		}
		for _, b := range branches {
			if n := strings.TrimSpace(b.GetName()); n != "" {
				names = append(names, n)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return names, nil
}
