package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// ErrHumanDecisionLabelMissing means the optional label lookup returned 404.
// Callers can report this configuration mismatch without treating it as a
// runtime failure. The underlying GitHub error remains available via errors.As.
var ErrHumanDecisionLabelMissing = errors.New("human decision label missing")

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
		var apiErr *gh.ErrorResponse
		if errors.As(err, &apiErr) && apiErr.Response != nil && apiErr.Response.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %q not present in %s/%s: %w", ErrHumanDecisionLabelMissing, label, owner, name, err)
		}
		return fmt.Errorf("look up human decision label %q in %s/%s: %w", label, owner, name, err)
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, name, number, []string{label}); err != nil {
		return fmt.Errorf("apply label %q to %s/%s#%d: %w", label, owner, name, number, err)
	}
	c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
		WriteTarget{Repo: owner + "/" + name, Number: number}, "label", label)
	return nil
}
