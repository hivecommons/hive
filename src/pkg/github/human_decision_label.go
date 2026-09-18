package github

import (
	"context"
	"fmt"
	"strings"
)

// ApplyHumanDecisionLabel marks a pull request with the operator's configured
// "a human needs to look at this" label.
//
// The label is never created. A hive reviews other people's repositories, and
// inventing a label there is a change to someone else's project taxonomy that
// nobody asked for. So this borrows a label the repo already maintains: if the
// label does not exist — a typo, or a repo that simply does not use one — the
// call is a no-op and the review comment's in-body marker remains the signal.
func (c *Client) ApplyHumanDecisionLabel(ctx context.Context, repo string, number int, label string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	label = strings.TrimSpace(label)
	if label == "" || number <= 0 {
		return nil
	}
	owner, name := splitRepoRef(repo, c.org)
	if owner == "" || name == "" {
		return fmt.Errorf("human decision label: cannot resolve repo %q", repo)
	}
	if _, _, err := c.client.Issues.GetLabel(ctx, owner, name, label); err != nil {
		return fmt.Errorf("human decision label %q not present in %s/%s: %w", label, owner, name, err)
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, name, number, []string{label}); err != nil {
		return fmt.Errorf("apply label %q to %s/%s#%d: %w", label, owner, name, number, err)
	}
	return nil
}
