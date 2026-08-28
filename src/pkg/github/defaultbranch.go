package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// FallbackDefaultBranch names the branch CreatePR would fall back to if it
// chose to guess instead of failing (see CreatePR's doc): it does not, by
// default, but the constant is kept so error messages that explain the
// refusal never carry a bare string literal.
//
// It is a LAST resort, not a starting assumption. Hive used to hardcode it as
// THE base for every PR it opened, which silently mis-targeted every
// repository whose default branch is not "main": the PR's diff then carried
// the entire divergence between the two branches, and merging it landed the
// change on the wrong branch (kubestellar/hive#4928).
const FallbackDefaultBranch = "main"

// DefaultBranch resolves the default branch of owner/repo from the
// repository's own metadata (the `default_branch` REST field — the API
// behind `gh repo view --json defaultBranchRef`), which the App's
// metadata:read permission already covers.
//
// The result is cached for the life of the process: a repo's default branch
// changes about as often as the repo is renamed, so caching keeps this off
// the hot path of every PR open. Only successful lookups are cached — a
// transient API error must not pin the repo to a wrong answer until the next
// restart.
//
// Unlike the original #4930 draft, this does NOT silently substitute
// FallbackDefaultBranch on error: a lookup failure is returned to the caller
// so it can choose to fail the PR request rather than open it against a base
// nobody confirmed is right (kubestellar/hive#4928 requirement: a wrong base
// is worse than a delayed PR). Callers that have no explicit base and cannot
// tolerate a failed request may still choose to fall back to
// FallbackDefaultBranch themselves, logging that decision.
func (c *Client) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	owner, repo = strings.TrimSpace(owner), strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return "", errors.New("DefaultBranch: owner and repo are required")
	}
	key := owner + "/" + repo
	if branch, ok := c.cachedDefaultBranch(key); ok {
		return branch, nil
	}

	r, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolving default branch for %s: %w", key, err)
	}
	branch := strings.TrimSpace(r.GetDefaultBranch())
	if branch == "" {
		// A repo with no default_branch is not a thing GitHub returns for a
		// live repo, but an empty string here would cache a base of "" and
		// make every later PR open fail — treat it as unresolved and do not
		// cache it.
		return "", fmt.Errorf("resolving default branch for %s: repository metadata reported an empty default branch", key)
	}
	c.storeDefaultBranch(key, branch)
	return branch, nil
}

func (c *Client) cachedDefaultBranch(key string) (string, bool) {
	c.defaultBranchMu.RLock()
	defer c.defaultBranchMu.RUnlock()
	branch, ok := c.defaultBranches[key]
	return branch, ok
}

func (c *Client) storeDefaultBranch(key, branch string) {
	c.defaultBranchMu.Lock()
	defer c.defaultBranchMu.Unlock()
	if c.defaultBranches == nil {
		c.defaultBranches = map[string]string{}
	}
	c.defaultBranches[key] = branch
}
