package github

import (
	"context"
)

// RequiredCheckContextPresent reports whether any live branch protection or
// ruleset still requires the named context. Uninstall uses this read-only
// proof and never deletes or rewrites an entire protection policy: GitHub's
// branch-protection API offers no conditional exact-context mutation capable
// of preserving a concurrent administrator change.
func (c *Client) RequiredCheckContextPresent(ctx context.Context, repository, branch, contextName string) (bool, error) {
	protection, err := c.BranchProtection(ctx, repository, branch)
	if err != nil {
		return false, err
	}
	for _, check := range protection.RequiredCheckIdentities {
		if exactCheckName(check.Context) == exactCheckName(contextName) {
			return true, nil
		}
	}
	return false, nil
}
